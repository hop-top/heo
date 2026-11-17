package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/uma/model"
)

// OutboundKinds are the operation kinds a delivery list covers: the
// writes the daemon's workers deliver to a provider.
var OutboundKinds = []string{
	model.ActionMessageReply, model.ActionMessageNote, model.ActionConversationStart, model.ActionWorkItemAct,
}

// DeliveryStates are the states an operation or delivery list filters by.
var DeliveryStates = []model.DeliveryState{
	model.Queued, model.RetryPending, model.ProviderAccepted, model.Delivered,
	model.Failed, model.Unknown, model.Rejected, model.Committed, model.DeadLettered,
	model.Held, model.Expired,
}

// OperationFilter narrows an operation list. Inbox is a key or ID,
// Conversation an ID; Kinds are operation kinds and States delivery
// states, any of which matches; the window is [Since, Until) over the
// time of an operation's latest change, the time uma lists by. Actor,
// when set, keeps the operations that actor sent.
type OperationFilter struct {
	Inbox        string
	Conversation string
	Kinds        []string
	States       []string
	Since, Until *time.Time
	Actor        model.ActorID
}

// ListOperations returns one page of the operations uma holds that the
// caller may read, newest change first, narrowed by f. uma owns the
// order, the window, the paging and the authorization of each
// operation; a page may be short and still carry a cursor.
func (s Service) ListOperations(ctx context.Context, f OperationFilter, page model.PageRequest) (model.Page[model.Operation], error) {
	states := make([]string, 0, len(f.States))
	for _, st := range f.States {
		if !slices.Contains(DeliveryStates, model.DeliveryState(st)) {
			names := make([]string, len(DeliveryStates))
			for i, d := range DeliveryStates {
				names[i] = string(d)
			}
			return model.Page[model.Operation]{}, model.NewError(model.Validation, fmt.Sprintf(
				"state %q: want one of %s", st, strings.Join(names, ", ")))
		}
		states = append(states, st)
	}
	filter := model.Filter{Kinds: f.Kinds, Statuses: states, Since: f.Since, Before: f.Until}
	if f.Actor != "" {
		actor := f.Actor
		filter.Actor = &actor
	}
	var inbox model.Inbox
	if f.Inbox != "" {
		in, err := s.FindInbox(ctx, f.Inbox)
		if err != nil {
			return model.Page[model.Operation]{}, err
		}
		inbox = in
		filter.Inboxes = []model.InboxRef{in.Ref}
	}
	if f.Conversation != "" {
		// With the inbox named, the conversation is addressed without
		// reading it: a caller that may read operations but not
		// conversations (the operator) can still list one's.
		if err := checkID("conversation", f.Conversation); err != nil {
			return model.Page[model.Operation]{}, err
		}
		ref := model.ConversationRef{Inbox: inbox.Ref, ID: model.ID(f.Conversation)}
		if f.Inbox == "" {
			conv, err := s.FindConversation(ctx, f.Conversation)
			if err != nil {
				return model.Page[model.Operation]{}, err
			}
			ref = conv.Ref
		}
		filter.Conversation = &ref
	}
	return s.Client.Operations().List(ctx, model.ListRequest{Call: call(), Filter: filter, Page: page})
}

// ListDeliveries is ListOperations restricted to the outbound kinds a
// provider delivers (OutboundKinds).
func (s Service) ListDeliveries(ctx context.Context, f OperationFilter, page model.PageRequest) (model.Page[model.Operation], error) {
	f.Kinds = OutboundKinds
	return s.ListOperations(ctx, f, page)
}

// Reconciler is what heo can tell of how a delivery is reconciled: the
// adapter of the channel it left through, and whether that adapter
// declares reconcile. uma reconciles an outcome_unknown delivery inside
// its workers, with the adapter's own lookup (the sent copy, the audit
// trail, the echo); an adapter that declares none leaves it unknown
// for good.
type Reconciler struct {
	Adapter    model.TypeKey
	Reconciles bool
	// Settle is how long after an attempt the adapter's lookup may still
	// miss an applied effect: inside it, "not found" settles nothing.
	Settle time.Duration
}

// ReconcilerOf describes the adapter behind the channel op was
// delivered through: from its message, the conversation, the inbox and
// its channel, each read as the caller. It returns nil when that cannot
// be told — the operation produced no message, or the caller may not
// read the path to the adapter — and heo then says nothing of it.
func (s Service) ReconcilerOf(ctx context.Context, op model.Operation) *Reconciler {
	if op.ResultMessage == nil || s.Adapter == nil {
		return nil
	}
	conv, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: op.ResultMessage.Conversation})
	if err != nil {
		return nil
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return nil
	}
	d, err := s.Adapter(ctx, in.Channel)
	if err != nil {
		return nil
	}
	return &Reconciler{Adapter: d.Type, Reconciles: slices.Contains(d.Capabilities, model.CapabilityReconcile), Settle: d.ReconcileSettle}
}
