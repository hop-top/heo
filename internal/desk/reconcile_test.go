package desk_test

import (
	"errors"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// reconciling has the world's fake adapter declare reconcile, with the
// given settle window.
func reconciling(settle time.Duration) func(*uma.Config) {
	return func(c *uma.Config) {
		for _, f := range c.Adapters {
			if fake, ok := f.(interface {
				EnableReconcile()
				ReconcileSettle(time.Duration)
			}); ok {
				fake.EnableReconcile()
				fake.ReconcileSettle(settle)
			}
		}
	}
}

// unknownReply queues a reply whose send leaves the outcome unknown and
// returns it settled so.
func (w *world) unknownReply(text string) model.Operation {
	w.t.Helper()
	w.fake.FailSends(errors.New("connection reset after write"))
	defer w.fake.FailSends(nil)
	op := w.settle(w.reply(text))
	if op.State != model.Unknown {
		w.t.Fatalf("reply = %s %+v, want outcome_unknown", op.State, op.Failure)
	}
	return op
}

func (w *world) reconcile(who desk.Service, op model.Operation) (model.Operation, error) {
	w.t.Helper()
	p, err := who.PlanReconcile(w.t.Context(), string(op.Ref.ID), channels.Submit)
	if err != nil {
		w.t.Fatal(err)
	}
	return who.Reconcile(w.t.Context(), p)
}

// TestReconcileAppliedSettles: the provider holds the message, so the
// delivery settles with the provider's references; reconciling again is
// safe and asks the provider nothing more.
func TestReconcileAppliedSettles(t *testing.T) {
	w := newWorld(t, reconciling(0))
	unknown := w.unknownReply("lost")
	ops := w.as(operator)

	// The desk reads the path to the adapter, which the operator does not.
	p, err := w.as(agent).PlanReconcile(t.Context(), string(unknown.Ref.ID), channels.Submit)
	if err != nil || !p.Runs() || p.Reconciler == nil || !p.Reconciler.Reconciles {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	if w.fake.Reconciles() != 0 {
		t.Fatal("planning asked the provider")
	}
	w.fake.ReconcileAs(adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(w.ch, "fake-found"),
	}}, nil)
	got, err := ops.Reconcile(t.Context(), p)
	if err != nil || got.State != model.ProviderAccepted || len(got.ProviderReferences) == 0 || got.Ref != unknown.Ref {
		t.Fatalf("reconcile = %+v, %v", got, err)
	}
	if w.fake.Reconciles() != 1 {
		t.Fatalf("%d provider lookups, want 1", w.fake.Reconciles())
	}
	again, err := w.reconcile(ops, got)
	if err != nil || again.State != model.ProviderAccepted || w.fake.Reconciles() != 1 {
		t.Fatalf("reconcile again = %+v, %v, %d lookups", again, err, w.fake.Reconciles())
	}
}

// TestReconcileNotFound: a provider with no record of the message, past
// the settle window, means it was not applied: the delivery is
// retry_pending with the time uma retries it. Inside the window it
// proves nothing and the delivery stays outcome_unknown.
func TestReconcileNotFound(t *testing.T) {
	w := newWorld(t, reconciling(time.Hour))
	unknown := w.unknownReply("lost")
	ops := w.as(operator)
	w.fake.ReconcileAs(adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no such message")}, nil)

	got, err := w.reconcile(ops, unknown)
	if err != nil || got.State != model.Unknown || w.fake.Reconciles() != 1 {
		t.Fatalf("inside the settle window = %+v, %v (%d lookups)", got, err, w.fake.Reconciles())
	}
	w.clock.Advance(2 * time.Hour)
	got, err = w.reconcile(ops, unknown)
	if err != nil || got.State != model.RetryPending || got.NextAttemptAt == nil {
		t.Fatalf("past the settle window = %+v, %v", got, err)
	}
}

// TestReconcileProviderUnavailable: an adapter that cannot be asked is
// uma's error, and the delivery is unchanged.
func TestReconcileProviderUnavailable(t *testing.T) {
	w := newWorld(t, reconciling(0))
	unknown := w.unknownReply("lost")
	ops := w.as(operator)
	p, err := ops.PlanReconcile(t.Context(), string(unknown.Ref.ID), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ops.Reconcile(t.Context(), p) // the fake answers "unavailable" until told otherwise
	wantCode(t, err, model.Unavailable, "")
	cur, err := ops.GetOperation(t.Context(), string(unknown.Ref.ID))
	if err != nil || cur.State != model.Unknown || cur.Revision != unknown.Revision {
		t.Fatalf("after a failed lookup = %+v, %v", cur, err)
	}
}

// TestReconcileWithoutTheCapability: an adapter that declares no
// reconcile is refused by uma, not by an inference of heo's.
func TestReconcileWithoutTheCapability(t *testing.T) {
	w := newWorld(t)
	unknown := w.unknownReply("lost")
	ops := w.as(agent)
	p, err := ops.PlanReconcile(t.Context(), string(unknown.Ref.ID), channels.Submit)
	if err != nil || !p.Runs() || p.Reconciler == nil || p.Reconciler.Reconciles {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	_, err = ops.Reconcile(t.Context(), p)
	wantCode(t, err, model.Unsupported, "")
	if w.fake.Reconciles() != 0 {
		t.Fatal("the provider was asked")
	}
}

// TestReconcileLeavesOtherStatesAlone: only outcome_unknown asks the
// provider; every other state is returned as it is.
func TestReconcileLeavesOtherStatesAlone(t *testing.T) {
	w := newWorld(t, reconciling(0))
	ops := w.as(operator)
	queued := w.reply("held")
	p, err := ops.PlanReconcile(t.Context(), string(queued.ID), channels.Submit)
	if err != nil || p.Runs() || p.Operation.State != model.Queued {
		t.Fatalf("queued plan = %+v, %v", p, err)
	}
	got, err := ops.Reconcile(t.Context(), p)
	if err != nil || got.State != model.Queued || w.fake.Reconciles() != 0 {
		t.Fatalf("queued = %+v, %v (%d lookups)", got, err, w.fake.Reconciles())
	}
	done := w.settle(queued)
	if got, err := w.reconcile(ops, done); err != nil || got.State != model.ProviderAccepted || w.fake.Reconciles() != 0 {
		t.Fatalf("settled = %+v, %v (%d lookups)", got, err, w.fake.Reconciles())
	}
	_, err = ops.PlanReconcile(t.Context(), "op_404", channels.Submit)
	wantCode(t, err, model.NotFound, "")
}

// TestReconcileIsAuthorizedByUma: uma refuses the callers heo's policy
// gives no reconcile, before the provider is asked; the desk and the
// operator hold it.
func TestReconcileIsAuthorizedByUma(t *testing.T) {
	w := newWorld(t, reconciling(0))
	unknown := w.unknownReply("lost")
	w.fake.ReconcileAs(adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(w.ch, "fake-found"),
	}}, nil)
	// A viewer may not even read the operation.
	_, err := w.as(viewer).PlanReconcile(t.Context(), string(unknown.Ref.ID), channels.Submit)
	wantCode(t, err, model.Unauthorized, "")
	// Reading is not reconciling: uma asks for operation.reconcile besides.
	p, err := w.as(operator).PlanReconcile(t.Context(), string(unknown.Ref.ID), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	w.gate.refuse = map[string]bool{model.ActionOperationReconcile: true}
	_, err = w.as(operator).Reconcile(t.Context(), p)
	wantCode(t, err, model.Unauthorized, "")
	if w.fake.Reconciles() != 0 {
		t.Fatal("a refused reconcile reached the provider")
	}
	w.gate.refuse = nil
	if got, err := w.as(agent).Reconcile(t.Context(), p); err != nil || got.State != model.ProviderAccepted {
		t.Fatalf("desk reconcile = %+v, %v", got, err)
	}
}

// reconfigure replaces the world channel's configuration, as an
// operator does.
func (w *world) reconfigure(cfg adapterstest.Config) {
	w.t.Helper()
	page, err := w.client(operator).Configuration().Channels(w.t.Context(), model.ListRequest{Call: call, Page: model.PageRequest{Limit: 10}})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, ch := range page.Items {
		if ch.Ref == w.ch {
			ch.Configuration = cfg.Configuration()
			cmd := w.cmd()
			cmd.ExpectedRevision = ch.Revision
			if _, err := w.client(operator).Configuration().ConfigureChannel(w.t.Context(), model.ConfigureChannel{Command: cmd, Channel: ch}); err != nil {
				w.t.Fatalf("reconfigure: %v", err)
			}
			return
		}
	}
	w.t.Fatal("the world channel is not listed")
}

// previewAndSubmission reconciles op as who twice: as a preview, whose
// refusal is what the plan carries, and as a submission, which uma
// answers or refuses. It returns both errors.
func (w *world) previewAndSubmission(who desk.Service, op model.Operation) (preview, submission error) {
	w.t.Helper()
	id := string(op.Ref.ID)
	pp, err := who.PlanReconcile(w.t.Context(), id, channels.Preview)
	if err != nil {
		w.t.Fatalf("preview plan: %v", err)
	}
	sp, err := who.PlanReconcile(w.t.Context(), id, channels.Submit)
	if err != nil {
		w.t.Fatalf("submission plan: %v", err)
	}
	if sp.Refusal != nil {
		w.t.Fatalf("a submission's plan refused it itself, not uma: %v", sp.Refusal)
	}
	_, submission = who.Reconcile(w.t.Context(), sp)
	return pp.Refusal, submission
}

// TestReconcilePreviewRefusesAsSubmissionDoes: where the channel cannot
// reconcile, the preview refuses with the error uma answers a submission
// with (same code, same message), for the desk and the operator alike,
// and a preview of one that can asks nothing and refuses nothing. The
// capability is off by the channel's configuration (a relay with no
// lookup URL reports it so), or its permission is withdrawn.
func TestReconcilePreviewRefusesAsSubmissionDoes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cfg    adapterstest.Config
		code   model.ErrorCode
		reason string
	}{
		{"off by the channel's configuration", adapterstest.Config{Account: "acct", Disabled: []model.Capability{model.CapabilityReconcile}},
			model.Unsupported, "reconcile unavailable"},
		{"permission withdrawn", adapterstest.Config{Account: "acct", Withdrawn: []model.Capability{model.CapabilityReconcile}},
			model.Unauthorized, "provider permission for reconcile withdrawn"},
	} {
		for _, who := range []struct {
			name  string
			actor model.ActorID
		}{{"desk", agent}, {"operator", operator}} {
			t.Run(tc.name+"/"+who.name, func(t *testing.T) {
				w := newWorld(t, reconciling(0))
				unknown := w.unknownReply("lost")
				w.reconfigure(tc.cfg)
				preview, submission := w.previewAndSubmission(w.as(who.actor), unknown)
				wantCode(t, submission, tc.code, tc.reason)
				wantCode(t, preview, tc.code, tc.reason)
				if preview.Error() != submission.Error() {
					t.Errorf("preview refusal %q, submission %q: want the same", preview, submission)
				}
				if w.fake.Reconciles() != 0 {
					t.Fatal("the provider was asked")
				}
			})
		}
	}
}

// TestReconcilePreviewRefusesUndeclared: an adapter that declares no
// reconcile is refused in a preview as in a submission, whoever asks.
func TestReconcilePreviewRefusesUndeclared(t *testing.T) {
	for _, who := range []model.ActorID{agent, operator} {
		w := newWorld(t)
		unknown := w.unknownReply("lost")
		preview, submission := w.previewAndSubmission(w.as(who), unknown)
		wantCode(t, submission, model.Unsupported, "cannot reconcile")
		wantCode(t, preview, model.Unsupported, "cannot reconcile")
		if preview.Error() != submission.Error() {
			t.Errorf("preview refusal %q, submission %q: want the same", preview, submission)
		}
	}
}

// TestReconcilePreviewAllowsWhatCanBeAsked: an adapter that declares
// reconcile and reports it available is not refused in a preview, which
// asks the provider nothing; nor is a delivery that needs none.
func TestReconcilePreviewAllowsWhatCanBeAsked(t *testing.T) {
	w := newWorld(t, reconciling(0))
	unknown := w.unknownReply("lost")
	for _, who := range []model.ActorID{agent, operator} {
		p, err := w.as(who).PlanReconcile(t.Context(), string(unknown.Ref.ID), channels.Preview)
		if err != nil || p.Refusal != nil || !p.Runs() {
			t.Fatalf("%s preview = %+v, %v", who, p, err)
		}
	}
	if w.fake.Reconciles() != 0 {
		t.Fatal("a preview asked the provider")
	}
	// Whatever the channel reports, a delivery that is not unknown is not
	// asked about, so there is nothing to refuse.
	w.reconfigure(adapterstest.Config{Account: "acct", Disabled: []model.Capability{model.CapabilityReconcile}})
	done := w.settle(w.reply("fine"))
	p, err := w.as(agent).PlanReconcile(t.Context(), string(done.Ref.ID), channels.Preview)
	if err != nil || p.Refusal != nil || p.Runs() {
		t.Fatalf("settled delivery preview = %+v, %v", p, err)
	}
}
