package desk

import (
	"context"
	"fmt"

	"hop.top/uma/model"
)

// Verdict is what a delivery's state allows a retry to do.
type Verdict string

// The verdicts, one per family of delivery states.
const (
	// VerdictRetry: dead-lettered. Its work was quarantined after claims
	// that never finished; an administrative replay queues it again.
	// The only state a retry replays.
	VerdictRetry Verdict = "retry"
	// VerdictWait: queued or retry_pending. uma is still working on it,
	// and retries a failed attempt that applied nothing on its own.
	VerdictWait Verdict = "wait"
	// VerdictDone: provider_accepted, delivered or committed. Nothing to
	// retry.
	VerdictDone Verdict = "done"
	// VerdictResubmit: failed or rejected. Final: uma never runs it
	// again; only a new operation, under a new key, sends it again.
	VerdictResubmit Verdict = "resubmit"
	// VerdictReconcile: outcome_unknown. The provider may have applied
	// it; retrying blindly may deliver it twice, so its outcome is
	// reconciled first.
	VerdictReconcile Verdict = "reconcile"
)

// VerdictOf classifies a delivery state. A state this version does not
// know waits: heo never retries what it cannot classify.
func VerdictOf(s model.DeliveryState) Verdict {
	switch s {
	case model.DeadLettered:
		return VerdictRetry
	case model.ProviderAccepted, model.Delivered, model.Committed:
		return VerdictDone
	case model.Failed, model.Rejected:
		return VerdictResubmit
	case model.Unknown:
		return VerdictReconcile
	}
	return VerdictWait
}

// GetOperation returns the operation with record ID id, as uma shows it
// to the caller.
func (s Service) GetOperation(ctx context.Context, id string) (model.Operation, error) {
	if err := checkID("operation", id); err != nil {
		return model.Operation{}, err
	}
	return s.Client.Operations().Get(ctx, model.Lookup[model.OperationRef]{
		Call: call(), Ref: model.OperationRef{Tenant: s.Tenant, ID: model.ID(id)},
	})
}

// RetryRefusal is a retry the safety check refused: the delivery's
// state is not dead_lettered. An outcome_unknown delivery is refused as
// uma's outcome_unknown, any other as a conflict with its state.
type RetryRefusal struct {
	Operation model.Operation
}

func (e *RetryRefusal) Error() string {
	op := e.Operation
	id := op.Ref.ID
	switch VerdictOf(op.State) {
	case VerdictReconcile:
		return fmt.Sprintf("delivery %s is outcome_unknown: the provider may already have it, "+
			"so a retry could deliver it twice; reconcile it first", id)
	case VerdictResubmit:
		return fmt.Sprintf("delivery %s %s finally: uma never runs it again, and a retry replays only "+
			"dead-lettered deliveries", id, op.State)
	case VerdictDone:
		return fmt.Sprintf("delivery %s is %s: nothing to retry", id, op.State)
	}
	return fmt.Sprintf("delivery %s is %s: uma is still working on it, and retries a failed attempt "+
		"that applied nothing on its own", id, op.State)
}

// Unwrap is the uma error the refusal is: outcome_unknown for an
// unknown outcome, a conflict with the delivery's state otherwise.
func (e *RetryRefusal) Unwrap() error {
	if VerdictOf(e.Operation.State) == VerdictReconcile {
		return model.NewError(model.OutcomeUnknown, e.Error())
	}
	return model.NewError(model.Conflict, e.Error())
}

// RetryPlan is a retry that passed the safety check: the dead-lettered
// operation as it is now, and the replay that queues it again.
type RetryPlan struct {
	Operation model.Operation
	Lookup    model.Lookup[model.OperationRef]
	Command   model.Command
}

// PlanRetry checks that the delivery id may be retried under operation
// op, before anything is submitted: only a dead-lettered delivery is
// (*RetryRefusal otherwise). uma checks the state again when it
// replays, so a delivery that moved on meanwhile is a conflict. The
// replay names no revision: its key's payload is the delivery alone, so
// the same key replays the same retry whatever the delivery did since.
//
// A retry already made under op is its own answer: when the delivery
// is no longer dead-lettered, but uma holds a replay of it under op,
// the plan replays that stored retry rather than refusing it, so
// rerunning a retry with its key is safe.
func (s Service) PlanRetry(ctx context.Context, id string, op model.OperationID) (RetryPlan, error) {
	cur, err := s.GetOperation(ctx, id)
	if err != nil {
		return RetryPlan{}, err
	}
	p := RetryPlan{
		Operation: cur,
		Lookup:    model.Lookup[model.OperationRef]{Call: call(), Ref: cur.Ref},
		Command:   model.Command{Call: call(), Operation: op},
	}
	if VerdictOf(cur.State) == VerdictRetry {
		return p, nil
	}
	if s.replayed(ctx, cur.Ref, op) {
		return p, nil
	}
	return RetryPlan{}, &RetryRefusal{Operation: cur}
}

// replayed reports whether uma holds a replay of ref under op: the
// caller's own retry, made before.
func (s Service) replayed(ctx context.Context, ref model.OperationRef, op model.OperationID) bool {
	if op == "" {
		return false
	}
	_, err := s.Client.Operations().Lookup(ctx, model.OperationLookup{
		Call: call(), Kind: model.ActionOperationReplay, Target: ref.Ref(), ID: op,
	})
	return err == nil
}

// Retry submits p: uma queues the operation again, as an operation of
// its own, and the daemon's workers run it, reauthorizing its
// originating actor. It returns the replay and the delivery after it.
func (s Service) Retry(ctx context.Context, p RetryPlan) (model.Acceptance, model.Operation, error) {
	acc, err := s.Client.Operations().Replay(ctx, p.Lookup, p.Command)
	if err != nil {
		return acc, model.Operation{}, err
	}
	cur, err := s.Client.Operations().Get(ctx, p.Lookup)
	return acc, cur, err
}
