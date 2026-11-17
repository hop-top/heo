package desk_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

// oneClaim quarantines a work item at its second unfinished claim.
func oneClaim(c *uma.Config) { c.Work = &uma.WorkPolicy{MaxClaims: 1, MaxInFlight: 4} }

// reply queues a reply from the agent in the local conversation.
func (w *world) reply(text string) model.OperationRef {
	w.t.Helper()
	s := w.as(agent)
	p, err := s.PlanReply(w.t.Context(), string(w.local.Ref.ID), text, w.cmd().Operation)
	if err != nil {
		w.t.Fatal(err)
	}
	acc, err := s.Reply(w.t.Context(), p)
	if err != nil {
		w.t.Fatal(err)
	}
	return acc.Operation.Ref
}

// worker runs one worker until the returned stop is called.
func (w *world) worker(owner string) (stop func()) {
	ctx, cancel := context.WithCancel(w.t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.e.Runtime().Run(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call},
			uma.WorkerOptions{Owner: owner, Poll: 5 * time.Millisecond, Lease: 10 * time.Second})
	}()
	return func() { cancel(); wg.Wait() }
}

// deadLetter has op's work claimed twice without finishing: the first
// worker stalls before any attempt, its lease runs out, and the second
// claim quarantines it.
func (w *world) deadLetter(op model.OperationRef) model.Operation {
	w.t.Helper()
	w.fake.Stall(true)
	stopA := w.worker("a")
	select {
	case <-w.fake.Stalled():
	case <-time.After(5 * time.Second):
		w.t.Fatal("no worker claimed the operation")
	}
	w.clock.Advance(11 * time.Second)
	stopB := w.worker("b")
	defer func() { stopB(); stopA(); w.fake.Stall(false) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := w.client(operator).Operations().Get(w.t.Context(), model.Lookup[model.OperationRef]{Call: call, Ref: op})
		if err != nil {
			w.t.Fatal(err)
		}
		if got.State == model.DeadLettered {
			return got
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("operation %s, want dead_lettered", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantRefusal(t *testing.T, err error, code model.ErrorCode, state model.DeliveryState, contains string) {
	t.Helper()
	var r *desk.RetryRefusal
	if !errors.As(err, &r) || r.Operation.State != state {
		t.Fatalf("err = %v, want a retry refusal of a %s delivery", err, state)
	}
	wantCode(t, err, code, contains)
}

// TestRetryOnlyDeadLettered: the safety check lets a retry replay a
// dead-lettered delivery alone. A queued,
// failed, delivered or outcome_unknown delivery is refused before uma
// is asked, outcome_unknown as uma's outcome_unknown with the reconcile
// path; a retry rerun under its key replays the stored retry.
func TestRetryOnlyDeadLettered(t *testing.T) {
	w := newWorld(t, oneClaim)
	ctx := t.Context()
	ops := w.as(operator)

	op := w.reply("held")
	queued, err := ops.GetOperation(ctx, string(op.ID))
	if err != nil || queued.State != model.Queued || desk.VerdictOf(queued.State) != desk.VerdictWait {
		t.Fatalf("queued = %+v, %v", queued, err)
	}
	_, err = ops.PlanRetry(ctx, string(op.ID), "retry-0")
	wantRefusal(t, err, model.Conflict, model.Queued, "still working on it")

	dead := w.deadLetter(op)
	if dead.Failure == nil || dead.Failure.Code != model.Permanent || desk.VerdictOf(dead.State) != desk.VerdictRetry {
		t.Fatalf("dead-lettered = %+v", dead)
	}
	if _, err := w.as(viewer).GetOperation(ctx, string(op.ID)); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer get = %v, want unauthorized", err)
	}
	p, err := ops.PlanRetry(ctx, string(op.ID), "retry-1")
	if err != nil || p.Operation.Revision != dead.Revision || p.Lookup.Ref != op {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	// uma decides who may replay: not the desk.
	if _, _, err := w.as(agent).Retry(ctx, p); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("desk retry = %v, want unauthorized", err)
	}
	acc, cur, err := ops.Retry(ctx, p)
	if err != nil || acc.Replayed || cur.State != model.Queued || cur.Failure != nil {
		t.Fatalf("retry = %+v %+v, %v", acc, cur, err)
	}
	// Rerun under its key: the stored retry, nothing queued twice.
	p, err = ops.PlanRetry(ctx, string(op.ID), "retry-1")
	if err != nil {
		t.Fatalf("plan rerun under the key: %v", err)
	}
	again, _, err := ops.Retry(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("rerun = %+v, %v", again, err)
	}
	_, err = ops.PlanRetry(ctx, string(op.ID), "retry-2")
	wantRefusal(t, err, model.Conflict, model.Queued, "still working on it")
	if got := w.settle(op); got.State != model.ProviderAccepted {
		t.Fatalf("retried = %s %+v", got.State, got.Failure)
	}
	_, err = ops.PlanRetry(ctx, string(op.ID), "retry-3")
	wantRefusal(t, err, model.Conflict, model.ProviderAccepted, "nothing to retry")

	// outcome_unknown: refused as uma's outcome_unknown.
	w.fake.FailSends(errors.New("connection reset after write"))
	unknown := w.settle(w.reply("lost"))
	if unknown.State != model.Unknown || desk.VerdictOf(unknown.State) != desk.VerdictReconcile {
		t.Fatalf("unknown = %+v", unknown)
	}
	_, err = ops.PlanRetry(ctx, string(unknown.Ref.ID), "retry-4")
	wantRefusal(t, err, model.OutcomeUnknown, model.Unknown, "reconcile it first")

	// failed: final, refused.
	w.fake.FailSends(model.NewError(model.Permanent, "mailbox gone"))
	failed := w.settle(w.reply("bounced"))
	if failed.State != model.Failed || desk.VerdictOf(failed.State) != desk.VerdictResubmit {
		t.Fatalf("failed = %+v", failed)
	}
	_, err = ops.PlanRetry(ctx, string(failed.Ref.ID), "retry-5")
	wantRefusal(t, err, model.Conflict, model.Failed, "failed finally")

	_, err = ops.PlanRetry(ctx, "op_404", "retry-6")
	wantCode(t, err, model.NotFound, "")
	_, err = ops.GetOperation(ctx, "a b")
	wantCode(t, err, model.NotFound, "operation a b")
}
