package desk

import (
	"context"
	"slices"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// ReconcilePlan is a reconcile that was checked before anything was
// asked of the provider: the delivery as it is now, and the adapter
// behind it.
type ReconcilePlan struct {
	Operation model.Operation
	// Reconciler is the adapter of the delivery's channel and whether it
	// declares reconcile; nil when that cannot be told.
	Reconciler *Reconciler
	// Refusal, set by a preview only, is the refusal uma would answer a
	// submission with because the channel cannot reconcile: the error
	// itself, for the caller to render as the submission's. Nil when
	// nothing is known to refuse it.
	Refusal error
}

// Runs reports whether reconciling asks the adapter: only an
// outcome_unknown delivery has an attempt to ask about. A delivery in
// any other state is left as it is: settled ones need nothing, and the
// others (queued, retry_pending, dead_lettered) belong to uma's workers
// or to a retry.
func (p ReconcilePlan) Runs() bool { return VerdictOf(p.Operation.State) == VerdictReconcile }

// PlanReconcile reads the delivery id as it is now, with the adapter it
// would ask. It asks the provider nothing.
func (s Service) PlanReconcile(ctx context.Context, id string, mode channels.Mode) (ReconcilePlan, error) {
	op, err := s.GetOperation(ctx, id)
	if err != nil {
		return ReconcilePlan{}, err
	}
	p := ReconcilePlan{Operation: op}
	if p.Runs() {
		p.Reconciler = s.ReconcilerOf(ctx, op)
		if mode == channels.Preview {
			p.Refusal = s.reconcileRefusal(ctx, op)
		}
	}
	return p, nil
}

// reconcileRefusal is the refusal uma answers a reconcile of op with
// when the channel the delivery left through cannot reconcile, read
// from the channel's live capability status as uma's attempt gate reads
// it: an adapter that declares none, one whose configuration leaves it
// off (a relay with no lookup URL) and one whose provider permission is
// withdrawn. It is nil when nothing is known to refuse the reconcile:
// no status to read (the delivery has no message, the caller reads no
// inbox of it, or the status cannot be read now), where a submission is
// left to uma as it always is. It asks the provider nothing of the
// lookup; the status read may probe the provider's permissions.
func (s Service) reconcileRefusal(ctx context.Context, op model.Operation) error {
	if op.ResultMessage == nil || s.Capabilities == nil {
		return nil
	}
	in, err := s.FindInbox(ctx, string(op.ResultMessage.Conversation.Inbox.ID))
	if err != nil {
		return nil
	}
	statuses, err := s.Capabilities(ctx, in.Channel)
	if err != nil {
		return nil
	}
	return reconcileGate(statuses)
}

// reconcileGate judges the channel's capability statuses as uma's
// attempt gate does for reconcile, with its words: Unsupported when the
// capability is not among them (not declared) or is off (disabled or
// unsupported), Unauthorized when the provider withdrew its permission,
// the status's own code, retryable, when it is only unavailable for
// now.
func reconcileGate(statuses []model.CapabilityStatus) error {
	i := slices.IndexFunc(statuses, func(st model.CapabilityStatus) bool { return st.Name == model.CapabilityReconcile })
	if i < 0 {
		return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter cannot reconcile", Retry: model.NoRetry}
	}
	st := statuses[i]
	switch {
	case st.Available:
		return nil
	case st.Reason == model.Unauthorized:
		return &model.Error{Code: model.Unauthorized, SafeMessage: "provider permission for reconcile withdrawn", Retry: model.NoRetry}
	case st.Reason.DefaultRetry() == model.RetrySameOperation:
		return model.NewError(st.Reason, "reconcile temporarily unavailable")
	}
	return &model.Error{Code: model.Unsupported, SafeMessage: "reconcile unavailable", Retry: model.NoRetry}
}

// Reconcile asks the adapter of the outcome_unknown delivery in p, now,
// what became of its unresolved attempt, and returns the delivery as uma
// holds it afterwards. uma records only the adapter's answer, never an
// inference:
//
//   - the effect applied: the delivery settles (provider_accepted or
//     delivered), with the provider's references;
//   - the effect failed for good: it settles failed or rejected;
//   - the provider holds no record, past the adapter's settle window: it
//     was not applied, and the delivery becomes retry_pending, retried by
//     uma at its NextAttemptAt;
//   - the answer proves nothing (no record inside the settle window): it
//     comes back unchanged, still outcome_unknown;
//   - the adapter cannot be asked: uma's error, and nothing is recorded.
//
// uma authorizes it as the caller, for reading the operation and then for
// reconciling it, and refuses an adapter that declares no reconcile
// (unsupported) and a delivery a worker is carrying out right now or that
// is no longer waiting for reconciliation (conflict). It is idempotent.
// A delivery that is not outcome_unknown is returned as it is, with no
// call.
func (s Service) Reconcile(ctx context.Context, p ReconcilePlan) (model.Operation, error) {
	if !p.Runs() {
		return p.Operation, nil
	}
	return s.Client.Operations().Reconcile(ctx, model.Lookup[model.OperationRef]{Call: call(), Ref: p.Operation.Ref})
}
