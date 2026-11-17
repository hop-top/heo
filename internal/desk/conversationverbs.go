package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/uma"
	"hop.top/uma/model"
)

// The verbs that act on a conversation's provider side: adding and
// removing a participant (capability membership), telling the sender a
// message was delivered or read (receipt) and importing the provider's
// history (history). Like edit, delete and react they work only where
// the channel's adapter declares the capability, and a preview checks
// that offline (see providerverbs.go).

// ParticipantPlan is a validated participant change: add or remove.
type ParticipantPlan struct {
	Request      model.ParticipantChange
	Conversation model.Conversation
	// Identity is the identity the change names, resolved from the
	// identity or contact the caller gave.
	Identity model.Identity
	Remove   bool
	// Declaration is set by a preview only.
	Declaration *Declaration
	deferred    error
}

// PlanParticipant builds and checks adding who to, or removing it from,
// the participants of conversation conv (record ID) at revision rev,
// under operation op. who is an identity of the conversation's inbox or
// a contact with exactly one identity there (record IDs). The revision
// is always sent. In a preview it also refuses an adapter that does not
// declare membership (model.Unsupported) and a change uma would refuse
// as pointless or unsafe (model.Conflict): adding a participant,
// removing a non-participant or the conversation's owner. A submission
// leaves all of it, and a stale revision, to uma.
func (s Service) PlanParticipant(ctx context.Context, conv, who string, remove bool, rev model.Revision, op model.OperationID, mode channels.Mode) (ParticipantPlan, error) {
	if strings.TrimSpace(who) == "" {
		return ParticipantPlan{}, model.NewError(model.Validation, "identity or contact: required")
	}
	c, err := s.FindConversation(ctx, conv)
	if err != nil {
		return ParticipantPlan{}, err
	}
	id, err := s.participantIdentity(ctx, c, who)
	if err != nil {
		return ParticipantPlan{}, err
	}
	p := ParticipantPlan{Conversation: c, Identity: id, Remove: remove}
	if mode == channels.Preview {
		verb := "participant add"
		if remove {
			verb = "participant remove"
		}
		in, err := s.FindInbox(ctx, string(c.Ref.Inbox.ID))
		if err != nil {
			return ParticipantPlan{}, err
		}
		d, err := s.Declared(ctx, in, verb, model.CapabilityMembership)
		if err != nil {
			return ParticipantPlan{}, err
		}
		p.Declaration = &d
		if err := membershipConflict(c, id, remove); err != nil {
			return ParticipantPlan{}, err
		}
	}
	if p.deferred, err = channels.CheckRevision("conversation", conv, c.Revision, rev, mode); err != nil {
		return ParticipantPlan{}, err
	}
	p.Request = model.ParticipantChange{
		Command:      model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Conversation: c.Ref, Identity: id.Ref,
	}
	if err := p.Request.Validate(); err != nil {
		return ParticipantPlan{}, err
	}
	return p, nil
}

// AddParticipant submits p, an addition: uma queues it durably, and the
// daemon's workers send it to the provider. The participants are
// untouched until the provider applied it.
func (s Service) AddParticipant(ctx context.Context, p ParticipantPlan) (model.Acceptance, error) {
	acc, err := s.Client.Conversations().AddParticipant(ctx, p.Request)
	return acc, channels.Explain(err, p.deferred)
}

// RemoveParticipant submits p, a removal.
func (s Service) RemoveParticipant(ctx context.Context, p ParticipantPlan) (model.Acceptance, error) {
	acc, err := s.Client.Conversations().RemoveParticipant(ctx, p.Request)
	return acc, channels.Explain(err, p.deferred)
}

// membershipConflict is uma's refusal of a change the conversation's
// stored participants make pointless or unsafe, said in a preview.
func membershipConflict(c model.Conversation, id model.Identity, remove bool) error {
	held := slices.ContainsFunc(c.Participants, func(p model.Participant) bool { return p.Identity == id.Ref })
	switch {
	case !remove && held:
		return model.NewError(model.Conflict, fmt.Sprintf("identity %s is already a participant of conversation %s", id.Ref.ID, c.Ref.ID))
	case remove && !held:
		return model.NewError(model.Conflict, fmt.Sprintf("identity %s is not a participant of conversation %s", id.Ref.ID, c.Ref.ID))
	case remove && c.Owner != nil && *c.Owner == id.Ref:
		return model.NewError(model.Conflict, fmt.Sprintf("identity %s is the owner of conversation %s: the owner cannot be removed", id.Ref.ID, c.Ref.ID))
	}
	return nil
}

// participantIdentity resolves who among the identities of c's inbox
// that the caller may read: an identity by record ID, else a contact by
// record ID, which must hold exactly one identity there.
func (s Service) participantIdentity(ctx context.Context, c model.Conversation, who string) (model.Identity, error) {
	req := model.ListRequest{Call: call(), Filter: model.Filter{Inboxes: []model.InboxRef{c.Ref.Inbox}}, Page: model.PageRequest{Limit: scanPage}}
	var all []model.Identity
	for {
		p, err := s.Client.Contacts().Identities(ctx, req)
		if err != nil {
			return model.Identity{}, err
		}
		all = append(all, p.Items...)
		if p.Complete || p.Next == "" {
			break
		}
		req.Page.After = p.Next
	}
	for _, i := range all {
		if string(i.Ref.ID) == who {
			return i, nil
		}
	}
	var of []model.Identity
	for _, i := range all {
		if string(i.Contact.ID) == who {
			of = append(of, i)
		}
	}
	switch len(of) {
	case 1:
		return of[0], nil
	case 0:
		return model.Identity{}, model.NewError(model.NotFound, fmt.Sprintf(
			"%s: no identity or contact with that ID in inbox %s", who, c.Ref.Inbox.ID))
	}
	ids := make([]string, len(of))
	for i, x := range of {
		ids[i] = string(x.Ref.ID)
	}
	return model.Identity{}, model.NewError(model.Validation, fmt.Sprintf(
		"contact %s has %d identities in inbox %s: name one of them (%s)", who, len(of), c.Ref.Inbox.ID, strings.Join(ids, ", ")))
}

// ReceiptPlan is a validated receipt.
type ReceiptPlan struct {
	Request     model.Receipt
	Message     model.Message
	Declaration *Declaration
}

// PlanReceipt builds and checks a delivery or read receipt, kind
// (model.ReceiptDelivered or model.ReceiptRead), for message id (in
// conversation conv, record ID, when set), under operation op. The
// message must have arrived from the provider. In a preview it also
// refuses an adapter that does not declare receipt (model.Unsupported),
// a message sent through uma (model.Validation) and a deleted or
// redacted one (model.Conflict); uma refuses the same on submission. A
// receipt takes no revision.
func (s Service) PlanReceipt(ctx context.Context, id, conv, kind string, op model.OperationID, mode channels.Mode) (ReceiptPlan, error) {
	if !model.IsReceiptKind(kind) {
		return ReceiptPlan{}, model.NewError(model.Validation, fmt.Sprintf("kind %q: want %s or %s", kind, model.ReceiptRead, model.ReceiptDelivered))
	}
	m, err := s.FindMessage(ctx, id, conv, false)
	if err != nil {
		return ReceiptPlan{}, err
	}
	p := ReceiptPlan{Message: m}
	if p.Declaration, err = s.previewDeclared(ctx, m, mode, "receipt send", model.CapabilityReceipt); err != nil {
		return ReceiptPlan{}, err
	}
	if mode == channels.Preview {
		if err := settledMessage(m, "receipted"); err != nil {
			return ReceiptPlan{}, err
		}
		if m.Origin != nil || m.EchoOf != nil {
			return ReceiptPlan{}, model.NewError(model.Validation, fmt.Sprintf(
				"message %s did not arrive from the provider (it was sent through heo, or is the provider's echo of one): only a received message takes a receipt", m.Ref.ID))
		}
	}
	p.Request = model.Receipt{Command: model.Command{Call: call(), Operation: op}, Message: m.Ref, Kind: kind}
	if err := p.Request.Validate(); err != nil {
		return ReceiptPlan{}, err
	}
	return p, nil
}

// SendReceipt submits p: uma queues the receipt durably, and the
// daemon's workers send it to the provider. uma records nothing else:
// the operation is the record that a receipt was sent.
func (s Service) SendReceipt(ctx context.Context, p ReceiptPlan) (model.Acceptance, error) {
	return s.Client.Messages().Receipt(ctx, p.Request)
}

// HistoryScope is what an import reads: one channel or one conversation
// (record IDs).
type HistoryScope struct {
	Channel      string
	Conversation string
}

// HistoryPlan is a validated bounded history import.
type HistoryPlan struct {
	Request uma.ImportRequest
	// Channel is the channel the provider is read through.
	Channel model.ChannelRef
	// Conversation is set for a conversation import.
	Conversation *model.Conversation
	// Declaration is set by a preview only.
	Declaration *Declaration
}

// DefaultHistorySince is where an import starts when the caller names
// no time: the beginning of whatever the provider holds. The import is
// bounded by its limit, never by this.
var DefaultHistorySince = time.Unix(0, 0).UTC()

// PlanHistory builds and checks a bounded import, for requester, of
// scope's provider history from since (the start of the provider's window when zero)
// up to until (no upper bound when zero; the window is since <= t < until),
// after the cursor a previous report named, at most limit envelopes
// (uma's default when 0). The conversation or channel is resolved as
// the caller, so one it may not read is not found. In a preview it also
// refuses an adapter that does not declare history (model.Unsupported),
// offline; uma checks the same, and the live availability, before it
// reads anything.
func (s Service) PlanHistory(ctx context.Context, scope HistoryScope, requester model.ActorID, since, until time.Time, after string, limit int, mode channels.Mode) (HistoryPlan, error) {
	if (scope.Channel == "") == (scope.Conversation == "") {
		return HistoryPlan{}, model.NewError(model.Validation, "exactly one of a channel and a conversation is required")
	}
	if limit < 0 || limit > uma.MaxImportLimit {
		return HistoryPlan{}, model.NewError(model.Validation, fmt.Sprintf("limit: want 1 to %d", uma.MaxImportLimit))
	}
	if since.IsZero() {
		since = DefaultHistorySince
	}
	if !until.IsZero() && !until.After(since) {
		return HistoryPlan{}, model.NewError(model.Validation, "until: want a time after since")
	}
	p := HistoryPlan{Request: uma.ImportRequest{Requester: requester, Since: since.UTC(), After: after, Limit: limit}}
	if !until.IsZero() {
		p.Request.Until = until.UTC()
	}
	var in *model.Inbox
	if scope.Conversation != "" {
		c, err := s.FindConversation(ctx, scope.Conversation)
		if err != nil {
			return HistoryPlan{}, err
		}
		inbox, err := s.FindInbox(ctx, string(c.Ref.Inbox.ID))
		if err != nil {
			return HistoryPlan{}, err
		}
		p.Conversation, p.Channel, in = &c, inbox.Channel, &inbox
		p.Request.Conversation = &c.Ref
	} else {
		ch, err := s.inboxes().FindChannel(ctx, scope.Channel)
		if err != nil {
			return HistoryPlan{}, err
		}
		p.Channel = ch.Ref
		p.Request.Channel = &ch.Ref
	}
	if mode == channels.Preview {
		d, err := s.declaredFor(ctx, p.Channel, in, scope, model.CapabilityHistory)
		if err != nil {
			return HistoryPlan{}, err
		}
		p.Declaration = &d
	}
	return p, nil
}

// declaredFor is the declaration check of a history import: through
// the inbox when the scope is a conversation, else through the channel
// alone.
func (s Service) declaredFor(ctx context.Context, ch model.ChannelRef, in *model.Inbox, scope HistoryScope, c model.Capability) (Declaration, error) {
	if in != nil {
		return s.Declared(ctx, *in, "history fetch", c)
	}
	if s.Adapter == nil {
		return Declaration{}, model.NewError(model.Unsupported, fmt.Sprintf("channel %s: its adapter is unknown here", ch.ID))
	}
	d, err := s.Adapter(ctx, ch)
	if err != nil {
		return Declaration{}, err
	}
	if !adapters.Declares(d, c) {
		return Declaration{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"channel %s: adapter %s does not declare %s, which `history fetch` needs; `heo channel get %s` lists the verbs the channel supports",
			scope.Channel, d.Type, c, ch.ID))
	}
	return Declaration{Capability: c, Adapter: d.Type, Channel: ch}, nil
}

// Verbs reads the live status of the provider verbs on conversation c's
// channel as the caller: uma's Conversations.Capabilities, authorized as
// a read of the conversation's internal content (conversation.read),
// never as a channel read, so a desk actor sees it. It returns statuses
// only: no channel configuration, no credential. It opens the channel's
// adapter and may reach the provider.
func (s Service) Verbs(ctx context.Context, c model.Conversation) ([]channels.VerbLive, error) {
	st, err := s.Client.Conversations().Capabilities(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: c.Ref})
	if err != nil {
		return nil, err
	}
	return channels.VerbsLive(st), nil
}
