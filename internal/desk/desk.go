// Package desk is what heo's conversation, message, note, delivery and
// event commands do in the daemon, over the uma engine the daemon owns:
// find conversations and messages by record ID, list them across the
// inboxes the caller may read, plan and submit the desk's writes — a
// conversation transition, a public reply, an internal note — exactly
// as they would be submitted, inspect and retry deliveries, and watch
// the event log through a durable subscription.
//
// Every call runs as the caller's uma client, so uma authorizes each
// one for the caller and applies its own rules: the conversation
// lifecycle, the reply route, note delivery. heo adds no second
// interpretation of them. It adds three things of its own: every
// message and event it returns passes the caller's audience rule again
// (Service.Sees), so content that is not public never reaches a caller
// who is not a desk actor whatever uma returned; a note's placement,
// which the caller states and heo checks against the inbox before
// submitting; and a retry's safety check, which replays a
// dead-lettered delivery alone.
//
// IDs are record IDs. uma addresses conversations and messages by
// their full reference (tenant, inbox, conversation), so a lookup by ID
// scans what the caller may read.
package desk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

// Correlation tags the calls heo's desk commands make.
const Correlation model.CorrelationID = "heo-cli"

// scanPage is the page size of a scan for one record by ID.
const scanPage = 100

func call() model.Call { return model.Call{Correlation: Correlation} }

// Service is the desk one caller reaches through the daemon's runtime.
type Service struct {
	// Client is the caller's uma client: every read and write is
	// authorized for the caller.
	Client uma.Client
	// Tenant is the profile's tenant.
	Tenant model.TenantID
	// Sees reports whether the caller may read content under an
	// audience. Nil sees public content only.
	Sees func(model.Audience) bool
	// Adapter describes the adapter of a channel, which a desk actor may
	// not read itself: a start checks its declarations before submitting,
	// as uma does on accepting one. Nil refuses every start.
	Adapter func(context.Context, model.ChannelRef) (adapter.Descriptor, error)
	// Capabilities reads the live per-capability status of a channel, as
	// the runtime reads it: a desk actor may not read the channel, and
	// the status is all that leaves it (no configuration, no
	// credential). A preview of a verb that uma refuses at submission
	// by that status, `delivery reconcile`, asks it. Nil judges
	// nothing.
	Capabilities func(context.Context, model.ChannelRef) ([]model.CapabilityStatus, error)
	// Channels lists every channel of the tenant, read as the runtime
	// reads it: a desk actor may not read channels, and the provider
	// label verbs name one by its key or ID among those the caller's
	// inboxes reach. Nil refuses them.
	Channels func(context.Context) ([]model.Channel, error)
	// Effects returns the extension effects the adapter of a channel
	// ships (the definitions heo registered beside it), which a desk
	// actor may not read from the channel either: an extension effect's
	// verb checks that the adapter ships the one it runs, besides
	// declaring extension_execute. Nil refuses every such verb.
	Effects func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error)
	// ValidateAction asks the adapter factory of a channel, when it
	// implements adapter.ActionValidator, whether it accepts an
	// extension action's fields: offline, no provider call. Nil judges
	// nothing; a factory without the hook returns no issues.
	ValidateAction func(context.Context, model.ChannelRef, model.Audience, model.Extension) ([]model.ValidationIssue, error)
	// RecipientNormalizer returns the adapter factory of a channel as
	// an adapter.RecipientNormalizer, offline: a start plans its
	// recipient under the spelling uma will key the start on. It
	// returns nil for a factory without the interface. Nil judges
	// nothing: every recipient is planned as given.
	RecipientNormalizer func(context.Context, model.ChannelRef) (adapter.RecipientNormalizer, error)
	// Trigger reads the single-use trigger_id an interaction carries and
	// the instant it stops working: what a modal is opened with. Nil is
	// slack.TriggerOf, the adapter's own reader.
	Trigger func(model.Interaction) (slack.Trigger, bool)
	// Kick wakes the daemon's idle workers, so an effect that must reach
	// its provider within seconds is not left to their poll. Nil does
	// nothing.
	Kick func()
	// Actor is the authenticated caller, as the daemon verified it: what
	// `--to self` and `--assignee self` mean. It is never read from a
	// request.
	Actor model.ActorID
	// Permits reports whether heo's policy lets actor perform action at
	// all: the caller's own right to read workflow state, and whether an
	// assignee is eligible. Nil permits nothing.
	Permits func(actor model.ActorID, action string) bool
	// Now is heo's clock, which resolves `snooze --for`. Nil is the
	// system clock.
	Now func() time.Time
	// Holds is the hold point's configuration: when set, a plugin
	// actor's write is offered to its policy before submission (see
	// Hold). Nil holds nothing.
	Holds *Holds
	// OnDecided receives each decision uma wrote through DecideHeld,
	// never a replay: the point an audit record attaches to. Nil
	// records nothing.
	OnDecided func(context.Context, DecisionRecord)
}

// sees reports whether the caller may read m.
func (s Service) sees(m model.Message) bool { return s.seesAudience(m.Audience) }

func (s Service) inboxes() channels.Service {
	return channels.Service{Client: s.Client, Tenant: s.Tenant}
}

// Filter narrows a listing. Inbox and Conversation name records by key
// or ID; Statuses are conversation statuses. LabelAny, LabelAll and
// LabelNone name labels a conversation holds any of, all of and none of;
// set parts combine by AND, and uma applies them. Unread, Assignee,
// Snoozed and Linked are the workflow filters of a conversation listing,
// uma's too: see Workflow. Text, Since and Until
// search messages: Text holds the words every match has, and Since and
// Until bound the message's own time as [Since, Until). uma applies
// them, and the caller's audience and read rights to each message;
// heo adds no index and no filter of its own.
type Filter struct {
	Inbox        string
	Conversation string
	Statuses     []string
	LabelAny     []string
	LabelAll     []string
	LabelNone    []string
	Workflow     WorkflowFilter
	Text         string
	Since        *time.Time
	Until        *time.Time
	// Sort orders a conversation listing: SortLastActivity or empty for
	// uma's own order. UpdatedSince keeps the conversations whose last
	// activity is after it. Only ListConversationItems honours either.
	Sort         string
	UpdatedSince *time.Time
}

// FindInbox returns the inbox whose key or ID is ref.
func (s Service) FindInbox(ctx context.Context, ref string) (model.Inbox, error) {
	return s.inboxes().FindInbox(ctx, ref)
}

// inboxFilter resolves f's inbox into uma's filter.
func (s Service) inboxFilter(ctx context.Context, f Filter) (model.Filter, error) {
	var out model.Filter
	if f.Inbox != "" {
		in, err := s.FindInbox(ctx, f.Inbox)
		if err != nil {
			return out, err
		}
		out.Inboxes = []model.InboxRef{in.Ref}
	}
	return out, nil
}

// ListConversations returns one page of the conversations the caller
// may read, in every inbox it may read, narrowed by f's inbox, statuses,
// labels and workflow filters. Snoozed conversations are left out unless
// f says otherwise (WorkflowFilter.Snoozed).
func (s Service) ListConversations(ctx context.Context, f Filter, page model.PageRequest) (model.Page[model.Conversation], error) {
	for _, st := range f.Statuses {
		if !model.IsConversationStatus(st) {
			return model.Page[model.Conversation]{}, model.NewError(model.Validation, fmt.Sprintf(
				"status %q: want one of %s", st, strings.Join(Statuses(), ", ")))
		}
	}
	filter, err := s.inboxFilter(ctx, f)
	if err != nil {
		return model.Page[model.Conversation]{}, err
	}
	filter.Statuses = f.Statuses
	if filter.Labels, err = s.labelSelector(ctx, f); err != nil {
		return model.Page[model.Conversation]{}, err
	}
	if err := s.workflowFilter(f.Workflow, &filter); err != nil {
		return model.Page[model.Conversation]{}, err
	}
	return s.Client.Conversations().List(ctx, model.ListRequest{Call: call(), Filter: filter, Page: page})
}

// Statuses lists the conversation statuses.
func Statuses() []string {
	return []string{model.ConversationOpen, model.ConversationPending, model.ConversationResolved, model.ConversationClosed}
}

// FindConversation returns the conversation with record ID id among
// those the caller may read.
func (s Service) FindConversation(ctx context.Context, id string) (model.Conversation, error) {
	if err := checkID("conversation", id); err != nil {
		return model.Conversation{}, err
	}
	req := model.ListRequest{Call: call(), Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Conversations().List(ctx, req)
		if err != nil {
			return model.Conversation{}, err
		}
		for _, c := range p.Items {
			if string(c.Ref.ID) == id {
				return c, nil
			}
		}
		if p.Complete || p.Next == "" {
			return model.Conversation{}, model.NewError(model.NotFound, fmt.Sprintf("conversation %s: not found", id))
		}
		req.Page.After = p.Next
	}
}

func checkID(kind, id string) error {
	if id == "" {
		return model.NewError(model.Validation, kind+": a record ID is required")
	}
	if model.ID(id).Validate() != nil {
		return model.NewError(model.NotFound, fmt.Sprintf("%s %s: not found", kind, id))
	}
	return nil
}

// messageFilter resolves f's inbox, conversation, labels and search
// fields into uma's filter.
func (s Service) messageFilter(ctx context.Context, f Filter) (model.Filter, error) {
	filter, err := s.inboxFilter(ctx, f)
	if err != nil {
		return filter, err
	}
	filter.Text, filter.Since, filter.Before = f.Text, f.Since, f.Until
	if filter.Labels, err = s.labelSelector(ctx, f); err != nil {
		return filter, err
	}
	if f.Conversation == "" {
		return filter, nil
	}
	conv, err := s.FindConversation(ctx, f.Conversation)
	if err != nil {
		return filter, err
	}
	filter.Conversation = &conv.Ref
	return filter, nil
}

// ListMessages returns one page of the messages the caller may read,
// across the inboxes it may read, narrowed by f's inbox, conversation,
// labels and search fields: public messages, and internal notes for a
// desk actor. A search page can be short or empty and still incomplete:
// uma scans a bounded number of stored matches per call, so a caller
// follows Next until Complete.
func (s Service) ListMessages(ctx context.Context, f Filter, page model.PageRequest) (model.Page[model.Message], error) {
	filter, err := s.messageFilter(ctx, f)
	if err != nil {
		return model.Page[model.Message]{}, err
	}
	p, err := s.Client.Messages().List(ctx, model.ListRequest{Call: call(), Filter: filter, Page: page})
	if err != nil {
		return model.Page[model.Message]{}, err
	}
	p.Items = s.visible(p.Items, false)
	return p, nil
}

// ListNotes returns one page of the internal notes the caller may read,
// narrowed as ListMessages. uma has no visibility filter, so it scans
// messages until the page holds page.Limit notes or the listing ends;
// the next page starts where the scan stopped.
func (s Service) ListNotes(ctx context.Context, f Filter, page model.PageRequest) (model.Page[model.Message], error) {
	filter, err := s.messageFilter(ctx, f)
	if err != nil {
		return model.Page[model.Message]{}, err
	}
	out := model.Page[model.Message]{Items: []model.Message{}}
	req := model.ListRequest{Call: call(), Filter: filter, Page: page}
	for {
		// Asking for no more than the notes still wanted keeps the
		// page within its limit: no note is passed over.
		req.Page.Limit = page.Limit - len(out.Items)
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return model.Page[model.Message]{}, err
		}
		out.Items = append(out.Items, s.visible(p.Items, true)...)
		out.Next, out.Complete = p.Next, p.Complete
		if p.Complete || p.Next == "" || len(out.Items) >= page.Limit {
			return out, nil
		}
		req.Page.After = p.Next
	}
}

// visible keeps the messages the caller may read, internal notes only
// when notes is set.
func (s Service) visible(ms []model.Message, notes bool) []model.Message {
	out := make([]model.Message, 0, len(ms))
	for _, m := range ms {
		if !s.sees(m) || notes && !IsNote(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// IsNote reports whether m is an internal note.
func IsNote(m model.Message) bool { return m.Audience.Visibility == model.Internal }

// FindMessage returns the message with record ID id among those the
// caller may read, in the conversation conv (record ID) when it is set,
// an internal note only when note is set. A message the caller may not
// read is not found.
func (s Service) FindMessage(ctx context.Context, id, conv string, note bool) (model.Message, error) {
	kind := "message"
	if note {
		kind = "note"
	}
	if err := checkID(kind, id); err != nil {
		return model.Message{}, err
	}
	filter, err := s.messageFilter(ctx, Filter{Conversation: conv})
	if err != nil {
		return model.Message{}, err
	}
	req := model.ListRequest{Call: call(), Filter: filter, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Messages().List(ctx, req)
		if err != nil {
			return model.Message{}, err
		}
		for _, m := range p.Items {
			if string(m.Ref.ID) != id {
				continue
			}
			if !s.sees(m) {
				return model.Message{}, model.NewError(model.NotFound, fmt.Sprintf("%s %s: not found", kind, id))
			}
			if note && !IsNote(m) {
				return model.Message{}, model.NewError(model.NotFound, fmt.Sprintf(
					"note %s: not an internal note; `message get` shows it", id))
			}
			return m, nil
		}
		if p.Complete || p.Next == "" {
			return model.Message{}, model.NewError(model.NotFound, fmt.Sprintf("%s %s: not found", kind, id))
		}
		req.Page.After = p.Next
	}
}

// Text is the text of c's text parts, one per line.
func Text(c model.Content) string {
	var parts []string
	for _, p := range c.Parts {
		if p.Kind == model.PartText && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// TextContent is content of one text part.
func TextContent(text string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
}
