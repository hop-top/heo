package desk

import (
	"context"

	"hop.top/uma/model"
)

// Terminal reports whether state ends a watch: the operation is
// delivered, final, dead-lettered, expired unheld, or its outcome is
// unknown. A held operation waits for a person's decision, and a queued
// or retry_pending operation is still being worked, and a state this
// version does not know is waited on: heo never calls what it cannot
// classify finished.
//
// An outcome_unknown operation can still be settled by uma's workers,
// which reconcile it with the provider; a watch ends on it because that
// can take as long as the provider's settle window, and the caller
// needs to know the answer is not in.
func Terminal(state model.DeliveryState) bool {
	switch state {
	case model.ProviderAccepted, model.Delivered, model.Committed,
		model.Failed, model.Rejected, model.Unknown, model.DeadLettered, model.Expired:
		return true
	}
	return false
}

// LookupOperation finds the operation the caller's write claimed under
// key, of kind and aimed at target, after a lost response: the caller's
// own operations only, authorized as a read of the operation found, as
// uma's lookup is. Nothing found — never claimed, claimed by another
// caller, or purged — is model.NotFound.
func (s Service) LookupOperation(ctx context.Context, kind string, target model.Ref, key string) (model.Operation, error) {
	return s.Client.Operations().Lookup(ctx, model.OperationLookup{
		Call: call(), Kind: kind, Target: target, ID: model.OperationID(key),
	})
}

// FindOperations finds the operations actor's writes claimed under key,
// after a lost response, when the kind and target are not known: uma's
// lookup takes both, and has no read by key alone, so this pages
// through the actor's own operations, newest change first, and keeps
// those whose key is key (of kind, when it is not empty). A key is
// reused across kinds and targets only by mistake, so more than one
// match is possible, and every operation inside uma's retention is
// read to say so. Nothing found — never claimed, past retention, or
// another caller's — is an empty result.
//
// An operation is listed where it was last written, so one a worker
// settles while the pages are being read can be missed; the caller
// reads again.
func (s Service) FindOperations(ctx context.Context, actor model.ActorID, key, kind string) ([]model.Operation, error) {
	f := OperationFilter{Actor: actor}
	if kind != "" {
		f.Kinds = []string{kind}
	}
	var found []model.Operation
	page := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListOperations(ctx, f, page)
		if err != nil {
			return nil, err
		}
		for _, op := range p.Items {
			if op.Key != nil && string(op.Key.ID) == key {
				found = append(found, op)
			}
		}
		if p.Complete || p.Next == "" {
			return found, nil
		}
		page.After = p.Next
	}
}
