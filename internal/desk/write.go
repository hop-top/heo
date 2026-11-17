package desk

import (
	"context"
	"fmt"
	"strings"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// TransitionPlan is a validated conversation transition and the
// conversation it moves, as it is now.
type TransitionPlan struct {
	Request  model.Transition
	Current  model.Conversation
	Warnings []string
	// deferred is a stale revision left to uma on submission.
	deferred error
}

// PlanTransition builds and checks the request moving the conversation
// id, read at revision rev, to state under operation op. It checks the
// revision as a channel plan does (see channels.Mode) and the request's
// shape; whether the move is allowed is uma's alone to decide, by the
// conversation's lifecycle, when the request is submitted.
func (s Service) PlanTransition(ctx context.Context, id string, rev model.Revision, state string, op model.OperationID, m channels.Mode) (TransitionPlan, error) {
	cur, err := s.FindConversation(ctx, id)
	if err != nil {
		return TransitionPlan{}, err
	}
	deferred, err := channels.CheckRevision("conversation", id, cur.Revision, rev, m)
	if err != nil {
		return TransitionPlan{}, err
	}
	req := model.Transition{Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev}, Conversation: cur.Ref, State: state}
	if err := req.Validate(); err != nil {
		return TransitionPlan{}, err
	}
	p := TransitionPlan{Request: req, Current: cur, deferred: deferred}
	if cur.Status == state {
		p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s is already %s: committing changes nothing", id, state))
	}
	return p, nil
}

// Transition submits p and returns the conversation as it is after it.
func (s Service) Transition(ctx context.Context, p TransitionPlan) (model.Conversation, model.Acceptance, error) {
	acc, err := s.Client.Conversations().Transition(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, channels.Explain(err, p.deferred)
	}
	conv, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Request.Conversation})
	return conv, acc, err
}

// ReplyPlan is a validated public reply, the conversation it answers
// and the inbox it leaves through.
type ReplyPlan struct {
	Request      model.Reply
	Conversation model.Conversation
	Inbox        model.Inbox
	// Facts are what the reply says about itself for the hold point:
	// AddedCC when it names recipients beyond the reply route's; and,
	// read only while holding is on, ReplyToDiffers from the last
	// inbound message's headers and FirstReplyUnknown from heo's own
	// records of what was sent to the contact.
	Facts HoldFacts
	// Recipients are the recipients the reply adds beyond the reply
	// route's address, normalized; nil when it adds none.
	Recipients *model.ReplyRecipients
	// Declaration and Limits are set by a preview of a reply with
	// recipients: what was checked of the adapter, and the limits the
	// channel reports now.
	Declaration *Declaration
	Limits      *model.RecipientLimits
}

// Destination is where the reply goes: the conversation's stored reply
// route, which uma reads again when a worker delivers it.
func (p ReplyPlan) Destination() model.ExternalRef { return p.Conversation.ReplyRoute }

// PlanReply builds and checks the public reply text to the conversation
// id under operation op, to the reply route's address alone.
func (s Service) PlanReply(ctx context.Context, id, text string, op model.OperationID) (ReplyPlan, error) {
	return s.PlanReplyTo(ctx, id, text, model.ReplyRecipients{}, op, channels.Submit)
}

// PlanReplyTo is PlanReply with recipients added beyond the reply
// route's address: reply-all, cc and bcc. A reply that names any needs
// its channel's adapter to declare reply_recipients, in a preview and on
// submission alike (model.Unsupported otherwise, before anything is
// stored); a preview also reads the limits the channel reports now and
// refuses what they do not allow. A reply with recipients is held, for a
// plugin actor, under the facts of an added cc.
func (s Service) PlanReplyTo(ctx context.Context, id, text string, rec model.ReplyRecipients, op model.OperationID, mode channels.Mode) (ReplyPlan, error) {
	if strings.TrimSpace(text) == "" {
		return ReplyPlan{}, model.NewError(model.Validation, "text: required")
	}
	rec, err := NormalizeRecipients(rec)
	if err != nil {
		return ReplyPlan{}, err
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return ReplyPlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return ReplyPlan{}, err
	}
	req := model.Reply{Command: model.Command{Call: call(), Operation: op}, Conversation: conv.Ref, Content: TextContent(text)}
	p := ReplyPlan{Conversation: conv, Inbox: in}
	if !rec.IsZero() {
		req.Recipients = &rec
		p.Recipients = &rec
		p.Facts.AddedCC = true
		if p.Limits, p.Declaration, err = s.gateRecipients(ctx, in, conv, "message reply", rec, mode == channels.Preview); err != nil {
			return ReplyPlan{}, err
		}
	}
	if err := req.Validate(); err != nil {
		return ReplyPlan{}, err
	}
	p.Request = req
	if s.Holding() {
		if p.Facts.ReplyToDiffers, err = s.replyToDiffers(ctx, conv.Ref); err != nil {
			return ReplyPlan{}, err
		}
		if p.Facts.FirstReplyUnknown, err = s.firstReplyUnknown(ctx, conv); err != nil {
			return ReplyPlan{}, err
		}
	}
	return p, nil
}

// Reply submits p: uma queues it durably, and the daemon's workers
// deliver it.
func (s Service) Reply(ctx context.Context, p ReplyPlan) (model.Acceptance, error) {
	return s.Client.Messages().Reply(ctx, p.Request)
}

// Placement is where an internal note lives.
type Placement string

// The placements.
const (
	// PlacementLocal keeps the note in uma.
	PlacementLocal Placement = "local"
	// PlacementProvider also delivers it to the provider as a private
	// note, through an inbox that opted in.
	PlacementProvider Placement = "provider"
)

// ParsePlacement reads a placement.
func ParsePlacement(s string) (Placement, error) {
	switch p := Placement(s); p {
	case PlacementLocal, PlacementProvider:
		return p, nil
	}
	return "", model.NewError(model.Validation, fmt.Sprintf("placement %q: want %s or %s", s, PlacementLocal, PlacementProvider))
}

// InboxPlacement is where in places its notes: at the provider when it
// delivers notes, in uma otherwise.
func InboxPlacement(in model.Inbox) Placement {
	if in.DeliverNotes {
		return PlacementProvider
	}
	return PlacementLocal
}

// PlacementError refuses a note whose stated placement is not its
// inbox's: a conflict with the inbox's configuration.
type PlacementError struct {
	Inbox model.Inbox
	Want  Placement
}

func (e *PlacementError) Error() string {
	return fmt.Sprintf("note placement %s does not match inbox %s, which places notes %s",
		e.Want, channels.Name(e.Inbox.Key, e.Inbox.Ref.ID), InboxPlacement(e.Inbox))
}

// Unwrap is the uma conflict the error is.
func (e *PlacementError) Unwrap() error { return model.NewError(model.Conflict, e.Error()) }

// NotePlan is a validated internal note, the conversation it is added
// to, its inbox and its placement.
type NotePlan struct {
	Request      model.InternalNote
	Conversation model.Conversation
	Inbox        model.Inbox
	Placement    Placement
}

// PlanNote builds and checks the internal note text on the conversation
// id under operation op. placement must be the conversation's inbox's
// (InboxPlacement), else *PlacementError: the caller states where the
// note goes and heo checks it against what the inbox does.
func (s Service) PlanNote(ctx context.Context, id, text string, placement Placement, op model.OperationID) (NotePlan, error) {
	if strings.TrimSpace(text) == "" {
		return NotePlan{}, model.NewError(model.Validation, "text: required")
	}
	if _, err := ParsePlacement(string(placement)); err != nil {
		return NotePlan{}, err
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return NotePlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return NotePlan{}, err
	}
	if InboxPlacement(in) != placement {
		return NotePlan{}, &PlacementError{Inbox: in, Want: placement}
	}
	req := model.InternalNote{Command: model.Command{Call: call(), Operation: op}, Conversation: conv.Ref, Content: TextContent(text)}
	if err := req.Validate(); err != nil {
		return NotePlan{}, err
	}
	return NotePlan{Request: req, Conversation: conv, Inbox: in, Placement: placement}, nil
}

// AddNote submits p: committed in uma, and queued for the provider when
// the inbox delivers notes.
func (s Service) AddNote(ctx context.Context, p NotePlan) (model.Acceptance, error) {
	return s.Client.Messages().AddInternalNote(ctx, p.Request)
}
