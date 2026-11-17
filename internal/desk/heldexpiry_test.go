package desk_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// decides lets the desk agent decide held operations: heo's policy grants
// no decision yet, so the test policy does.
type decides struct{ policy.Authorizer }

func (d decides) Authorize(ctx context.Context, a policy.Authorization) (policy.Decision, error) {
	switch a.Action {
	case model.ActionOperationApprove, model.ActionOperationReject, model.ActionOperationEdit, model.ActionTrustRead:
		if a.Principal.Actor == agent {
			return policy.Decision{Allow: true, PolicyVersion: "test", ReasonCode: "test.decides"}, nil
		}
	}
	return d.Authorizer.Authorize(ctx, a)
}

// heldWorld is a world whose desk agent may decide.
func heldWorld(t *testing.T) *world {
	t.Helper()
	w := newWorld(t)
	w.gate.inner = decides{w.gate.inner}
	return w
}

// proposeEdit has actor propose an edit of the first message as a held
// operation under key, through heo's hold point (expiry one hour). It
// returns the acceptance as Settle shaped it and the events heo announced.
func proposeEdit(t *testing.T, w *world, actor model.ActorID, key, text string) (model.Acceptance, *[]desk.HeldCreated, error) {
	t.Helper()
	ctx := t.Context()
	m := w.firstMessage()
	s, events := holding(w.as(actor), nil)
	ep, err := s.PlanEdit(ctx, string(m.Ref.ID), string(w.local.Ref.ID), m.Revision, text, model.OperationID(key), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Hold(ctx, &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
		Channel: string(w.local.Ref.Inbox.ID), Target: m.Ref.Ref(), Preview: map[string]any{"text": text}})
	if err != nil || h == nil {
		t.Fatalf("hold = %v, %v", h, err)
	}
	acc, err := s.Edit(ctx, ep)
	acc, err = h.Settle(ctx, acc, err)
	return acc, events, err
}

func mustHold(t *testing.T, w *world, key, text string) model.Operation {
	t.Helper()
	acc, _, err := proposeEdit(t, w, plugin, key, text)
	if err != nil || acc.Operation.State != model.Held {
		t.Fatalf("propose %s = %v, %v", key, acc.Operation.State, err)
	}
	return acc.Operation
}

func decideArgs(op model.Operation) model.DecideHeld {
	return model.DecideHeld{Call: call, Operation: op.Ref, ExpectedRevision: op.Revision, Note: "looked"}
}

// sweeper runs one worker whose sweep fires once the engine clock moves.
func sweeper(w *world, every time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(w.t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.e.Runtime().Run(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call},
			uma.WorkerOptions{Owner: "sweeper", Poll: 5 * time.Millisecond, Lease: 10 * time.Second, SweepEvery: every})
	}()
	return func() { cancel(); <-done }
}

func awaitState(t *testing.T, s desk.Service, id model.ID, want string) desk.HeldOperation {
	t.Helper()
	for range 400 {
		if v, err := s.GetHeld(t.Context(), string(id)); err == nil && v.Status == want {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("operation %s never reached %s", id, want)
	return desk.HeldOperation{}
}

func TestHeldExpiryEndToEnd(t *testing.T) {
	w := heldWorld(t)
	op := mustHold(t, w, "expire-me", "late")
	stop := sweeper(w, time.Minute)
	defer stop()

	// Before the deadline the operation stays held.
	w.clock.Advance(30 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if v, err := w.as(agent).GetHeld(t.Context(), string(op.Ref.ID)); err != nil || v.Status != "held" {
		t.Fatalf("before deadline = %+v, %v", v, err)
	}
	w.clock.Advance(time.Hour)
	v := awaitState(t, w.as(agent), op.Ref.ID, "expired")
	if v.Decision != "expired" || v.DecidedBy != "" {
		t.Fatalf("expired view = %+v", v)
	}

	// Nothing was decided: no approval of it, and no trust moved.
	exp, _ := w.as(agent).GetOperation(t.Context(), string(op.Ref.ID))
	_, err := w.client(agent).Operations().Approve(t.Context(), decideArgs(exp))
	wantCode(t, err, model.Conflict, "decided")
	tr, err := w.client(agent).Operations().Trust(t.Context(), model.TrustLookup{Call: call, Channel: w.ch, Key: v.TrustKey})
	if err != nil && !model.IsCode(err, model.NotFound) {
		t.Fatal(err)
	}
	if tr.Approvals != 0 || tr.Corrections != 0 || tr.Clean != 0 {
		t.Fatalf("trust moved by an expiry: %+v", tr)
	}

	// An expired proposal no longer guards: the same write is held anew.
	again, _, err := proposeEdit(t, w, plugin, "expire-me-again", "later")
	if err != nil || again.AlreadyHeld || again.Operation.State != model.Held || again.Operation.Ref == op.Ref {
		t.Fatalf("re-proposal = %+v, %v", again, err)
	}
}

func TestHeldDuplicateReturnsExistingHeldOperation(t *testing.T) {
	w := heldWorld(t)
	first, ev1, err := proposeEdit(t, w, plugin, "dup-1", "one")
	if err != nil || first.AlreadyHeld || first.Operation.State != model.Held || len(*ev1) != 1 {
		t.Fatalf("first = %+v, %v, events %d", first, err, len(*ev1))
	}
	second, ev2, err := proposeEdit(t, w, plugin, "dup-2", "two")
	if err != nil || !second.AlreadyHeld || second.Operation.State != model.Held || second.Operation.Ref != first.Operation.Ref || len(*ev2) != 0 {
		t.Fatalf("duplicate = %+v, %v, events %d", second, err, len(*ev2))
	}
	// Another plugin that may read the held operation gets it back too.
	other, _, err := proposeEdit(t, w, plugin2, "dup-3", "three")
	if err != nil || !other.AlreadyHeld || other.Operation.Ref != first.Operation.Ref {
		t.Fatalf("other proposer = %+v, %v", other, err)
	}
	// One held operation, one record trail.
	p, err := w.as(agent).ListHeld(t.Context(), desk.OperationFilter{}, model.PageRequest{Limit: 10})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("held = %+v, %v", p.Items, err)
	}
}

// readGate refuses the target read of one actor once armed.
type readGate struct {
	policy.Authorizer
	actor model.ActorID
	armed atomic.Bool
}

func (g *readGate) Authorize(ctx context.Context, a policy.Authorization) (policy.Decision, error) {
	if g.armed.Load() && a.Principal.Actor == g.actor && a.Action == model.ActionMessageRead {
		return policy.Decision{PolicyVersion: "test", ReasonCode: "test.unreadable"}, nil
	}
	return g.Authorizer.Authorize(ctx, a)
}

// TestHeldDuplicateOfUnreadableOperationIsTypedConflict: a proposer that
// may not read the held operation's target is refused with model.Conflict
// and told nothing of the held one.
func TestHeldDuplicateOfUnreadableOperationIsTypedConflict(t *testing.T) {
	w := heldWorld(t)
	g := &readGate{Authorizer: w.gate.inner, actor: plugin2}
	w.gate.inner = g
	first := mustHold(t, w, "dup-hidden", "one")

	m := w.firstMessage()
	s, _ := holding(w.as(plugin2), nil)
	ep, err := s.PlanEdit(t.Context(), string(m.Ref.ID), string(w.local.Ref.ID), m.Revision, "two", "dup-hidden-2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Hold(t.Context(), &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
		Channel: string(w.local.Ref.Inbox.ID), Target: m.Ref.Ref(), Preview: "p"})
	if err != nil {
		t.Fatal(err)
	}
	g.armed.Store(true)
	acc, err := s.Edit(t.Context(), ep)
	_, err = h.Settle(t.Context(), acc, err)
	wantCode(t, err, model.Conflict, "not shown to you")
	if strings.Contains(err.Error(), string(first.Ref.ID)) {
		t.Fatalf("error names the held operation: %v", err)
	}
}
