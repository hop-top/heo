package desk_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

func approve(id string) desk.DecideRequest {
	return desk.DecideRequest{ID: id, Decision: desk.DecideApprove}
}

func TestApproveHeldThenReplay(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "ap-1")
	s := w.as(agent)
	var recs []desk.DecisionRecord
	s.OnDecided = func(_ context.Context, r desk.DecisionRecord) { recs = append(recs, r) }

	r := approve(string(ref.ID))
	r.Note = "ok"
	got, err := s.DecideHeld(t.Context(), r)
	if err != nil || got.Status != string(model.Queued) || got.Decision != "approved" || got.DecidedBy != string(agent) || got.Replayed {
		t.Fatalf("approve = %+v, %v", got, err)
	}
	if len(recs) != 1 || recs[0].Decision != desk.DecideApprove || recs[0].By != agent || recs[0].Note != "ok" || recs[0].Operation != string(ref.ID) {
		t.Fatalf("records = %+v", recs)
	}
	// A second approve is the operation as it stands, replayed, unrecorded.
	again, err := s.DecideHeld(t.Context(), approve(string(ref.ID)))
	if err != nil || !again.Replayed || again.Status != string(model.Queued) || len(recs) != 1 {
		t.Fatalf("second approve = %+v, %v, records %d", again, err, len(recs))
	}
	// Approving what was rejected is a conflict.
	_, err = s.DecideHeld(t.Context(), desk.DecideRequest{ID: string(ref.ID), Decision: desk.DecideReject})
	if !model.IsCode(err, model.Conflict) {
		t.Fatalf("reject after approve = %v", err)
	}
}

func TestRejectAppliesNothing(t *testing.T) {
	w := newWorld(t)
	m := w.firstMessage()
	ref := holdEdit(t, w, "rj-1")
	s := w.as(agent)
	got, err := s.DecideHeld(t.Context(), desk.DecideRequest{ID: string(ref.ID), Decision: desk.DecideReject, Note: "no"})
	if err != nil || got.Status != string(model.Rejected) || got.Decision != "rejected" || !got.Correction {
		t.Fatalf("reject = %+v, %v", got, err)
	}
	if op, err := s.GetOperation(t.Context(), string(ref.ID)); err != nil || op.State != model.Rejected || !desk.Terminal(op.State) {
		t.Fatalf("operation after reject = %+v, %v", op.State, err)
	}
	after, err := s.FindMessage(t.Context(), string(m.Ref.ID), string(w.local.Ref.ID), false)
	if err != nil || desk.Text(after.Content) != desk.Text(m.Content) {
		t.Fatalf("message after reject = %q, %v; want %q", desk.Text(after.Content), err, desk.Text(m.Content))
	}
}

func TestDecideHeldRevisionCompare(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "rv-1")
	s := w.as(agent)
	_, err := s.DecideHeld(t.Context(), desk.DecideRequest{ID: string(ref.ID), Decision: desk.DecideApprove, Revision: "not-it"})
	if !model.IsCode(err, model.Conflict) || desk.IsStale(err) {
		t.Fatalf("revision mismatch = %v (stale %v)", err, desk.IsStale(err))
	}
	if v, err := s.GetHeld(t.Context(), string(ref.ID)); err != nil || v.Status != "held" {
		t.Fatalf("still held: %+v, %v", v, err)
	}
	// Not held: not found.
	_, err = s.DecideHeld(t.Context(), approve(string(replyOp(t, s, w, "plain").ID)))
	wantCode(t, err, model.NotFound, "")
}

func TestPluginCannotDecide(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "pl-1")
	// The plugin holds the desk role, which carries the decisions, and
	// still never decides: the policy denies it, and so does the verb.
	for _, act := range []string{model.ActionOperationApprove, model.ActionOperationReject, model.ActionOperationEdit} {
		if ok, _ := w.pol.Permits(model.Principal{Tenant: tenant, Actor: plugin}, act); ok {
			t.Errorf("policy lets a plugin %s", act)
		}
		if ok, _ := w.pol.Permits(model.Principal{Tenant: tenant, Actor: agent}, act); !ok {
			t.Errorf("policy denies a desk person %s", act)
		}
	}
	for _, d := range []desk.Decision{desk.DecideApprove, desk.DecideReject} {
		_, err := w.as(plugin).DecideHeld(t.Context(), desk.DecideRequest{ID: string(ref.ID), Decision: d})
		wantCode(t, err, model.Unauthorized, "plugin")
	}
	// Even a Service whose policy is bypassed, uma's own check aside:
	if v, err := w.as(agent).GetHeld(t.Context(), string(ref.ID)); err != nil || v.Status != "held" {
		t.Fatalf("held after plugin attempts: %+v, %v", v, err)
	}
}

// staleOps answers an approval as uma does when the target advanced:
// model.Conflict with the stale_target reason, unless Force is set.
type staleOps struct {
	uma.Operations
	forced *bool
}

func (o staleOps) Approve(ctx context.Context, d model.DecideHeld) (model.Decided, error) {
	if !d.Force {
		return model.Decided{}, model.NewReasonError(model.Conflict, model.StaleTarget, "target changed since the operation was held")
	}
	*o.forced = true
	return o.Operations.Approve(ctx, d)
}

type staleClient struct {
	uma.Client
	ops staleOps
}

func (c staleClient) Operations() uma.Operations { return c.ops }

func TestApproveStaleThenForce(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "st-1")
	s := w.as(agent)
	var forced bool
	s.Client = staleClient{Client: s.Client, ops: staleOps{Operations: s.Client.Operations(), forced: &forced}}

	_, err := s.DecideHeld(t.Context(), approve(string(ref.ID)))
	if !desk.IsStale(err) || !model.IsCode(err, model.Conflict) || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("stale approve = %v", err)
	}
	var recs []desk.DecisionRecord
	s.OnDecided = func(_ context.Context, r desk.DecisionRecord) { recs = append(recs, r) }
	r := approve(string(ref.ID))
	r.Force = true
	got, err := s.DecideHeld(t.Context(), r)
	if err != nil || !forced || got.Status != string(model.Queued) || len(recs) != 1 || !recs[0].Forced {
		t.Fatalf("forced approve = %+v, %v, forced %v, records %+v", got, err, forced, recs)
	}
}

// TestStaleIsUmasReason pins how heo tells a stale refusal from a
// revision mismatch, both model.Conflict: by uma's reason, never its text.
func TestStaleIsUmasReason(t *testing.T) {
	if !desk.StaleOf(model.NewReasonError(model.Conflict, model.StaleTarget, "any words at all")) {
		t.Error("uma's stale refusal not recognized")
	}
	for _, err := range []error{
		model.NewError(model.Conflict, "target changed since the operation was held"), // the old tell: text alone
		model.NewReasonError(model.Conflict, model.StaleRevision, "target changed since the operation was held"),
		model.NewReasonError(model.NotFound, model.StaleTarget, "x"),
		errors.New("target changed since the operation was held"),
	} {
		if desk.StaleOf(err) {
			t.Errorf("%v taken for stale", err)
		}
	}
}

// staleTarget holds a reply and moves its conversation on, as a new
// message or a transition would: uma now refuses an approval as stale.
func staleTarget(t *testing.T, w *world) model.Operation {
	t.Helper()
	op := holdClass(t, w, w.local, "stale-1", "state", "trash", training)
	cur := w.refetch(w.local)
	tp, err := w.as(agent).PlanTransition(t.Context(), string(w.local.Ref.ID), cur.Revision, model.ConversationResolved, "stale-t", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = w.as(agent).Transition(t.Context(), tp); err != nil {
		t.Fatal(err)
	}
	return op
}

// Against the real uma: a stale target is told from a revision mismatch
// by uma's own code, for an approval and for an edit.
func TestStaleDecisionOnRealUma(t *testing.T) {
	w := newWorld(t)
	op := staleTarget(t, w)
	s := w.as(agent)
	_, err := s.DecideHeld(t.Context(), desk.DecideRequest{ID: string(op.Ref.ID), Decision: desk.DecideApprove})
	if !desk.IsStale(err) {
		t.Fatalf("approve after the target moved = %v, want stale", err)
	}
	_, err = s.DecideHeld(t.Context(), desk.DecideRequest{ID: string(op.Ref.ID), Decision: desk.DecideApprove, Revision: "not-it"})
	if err == nil || desk.IsStale(err) || !model.IsCode(err, model.Conflict) {
		t.Fatalf("revision mismatch = %v, want a conflict that is not stale", err)
	}
	cur, _ := s.GetOperation(t.Context(), string(op.Ref.ID))
	_, err = s.EditHeld(t.Context(), desk.EditRequest{ID: string(op.Ref.ID), Args: cur.Held.Intent.Data})
	if !desk.IsStale(err) || strings.Contains(err.Error(), "--force") {
		t.Fatalf("edit after the target moved = %v, want stale without --force", err)
	}
	// Forced, the approval goes through: stale is the one thing --force skips.
	if got, err := s.DecideHeld(t.Context(), desk.DecideRequest{ID: string(op.Ref.ID), Decision: desk.DecideApprove, Force: true}); err != nil || got.Decision != "approved" {
		t.Fatalf("forced approve = %+v, %v", got, err)
	}
}
