package desk

import (
	"context"
	"fmt"
	"strings"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Provider verbs. A message edit, a message delete, a reaction and a
// ticket status change are effects at the provider, so they work only
// where the channel's adapter declares the capability that carries them
// (edit, delete, react, extension_execute). heo checks that once, in a
// preview: --dry-run resolves the target as the caller, reads the
// adapter's declarations offline, and refuses what uma would refuse at
// submission before rendering any plan. It checks nothing live — whether
// the provider account may do it right now, whether the channel's
// configuration switched it off — which a desk actor could not read
// anyway (that is a channel read) and which uma checks itself, at
// submission and again at every attempt. A submission is left to uma
// whole: a second heo check would only race the first.

// Declaration is what a preview checked of a provider verb: the adapter
// declares the capability. Live availability is not part of it.
type Declaration struct {
	Capability model.Capability
	Adapter    model.TypeKey
	Channel    model.ChannelRef
}

// Line is the plan's `capability` line: what was checked, and what was
// not.
func (d Declaration) Line() string {
	return fmt.Sprintf("%s declared by %s; live availability not checked (operator: heo channel check %s --remote)",
		d.Capability, d.Adapter, d.Channel.ID)
}

// Declared checks that the adapter of inbox in's channel declares c for
// verb, offline: model.Unsupported otherwise, naming the verb, the
// capability and where to look.
func (s Service) Declared(ctx context.Context, in model.Inbox, verb string, c model.Capability) (Declaration, error) {
	d, _, err := s.declared(ctx, in, verb, c)
	return d, err
}

// declared is Declared that also returns the adapter's descriptor, for
// a verb that needs more of it than one capability.
func (s Service) declared(ctx context.Context, in model.Inbox, verb string, c model.Capability) (Declaration, adapter.Descriptor, error) {
	name := channels.Name(in.Key, in.Ref.ID)
	if s.Adapter == nil {
		return Declaration{}, adapter.Descriptor{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: its channel's adapter is unknown here", name))
	}
	d, err := s.Adapter(ctx, in.Channel)
	if err != nil {
		return Declaration{}, adapter.Descriptor{}, err
	}
	if !adapters.Declares(d, c) {
		return Declaration{}, d, model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: adapter %s does not declare %s, which `%s` needs; `heo channel get %s` lists the verbs the channel supports",
			name, d.Type, c, verb, in.Channel.ID))
	}
	return Declaration{Capability: c, Adapter: d.Type, Channel: in.Channel}, d, nil
}

// messageInbox is the inbox m was recorded in.
func (s Service) messageInbox(ctx context.Context, m model.Message) (model.Inbox, error) {
	return s.FindInbox(ctx, string(m.Ref.Conversation.Inbox.ID))
}

// EditPlan is a validated message edit: the request, the message as it
// is now and, from a preview, what was checked of the adapter.
type EditPlan struct {
	Request model.EditMessage
	Message model.Message
	// Declaration is set by a preview only.
	Declaration *Declaration
	deferred    error
}

// PlanEdit builds and checks the edit of message id (in conversation
// conv, record ID, when set) to the public text, at revision rev,
// under operation op. In a preview it also refuses an adapter that does
// not declare edit (model.Unsupported) and a deleted message; a
// submission leaves both, and a stale revision, to uma.
func (s Service) PlanEdit(ctx context.Context, id, conv string, rev model.Revision, text string, op model.OperationID, mode channels.Mode) (EditPlan, error) {
	if strings.TrimSpace(text) == "" {
		return EditPlan{}, model.NewError(model.Validation, "text: required")
	}
	m, err := s.FindMessage(ctx, id, conv, false)
	if err != nil {
		return EditPlan{}, err
	}
	p := EditPlan{Message: m}
	if p.Declaration, err = s.previewDeclared(ctx, m, mode, "message edit", model.CapabilityEdit); err != nil {
		return EditPlan{}, err
	}
	if mode == channels.Preview {
		if err := settledMessage(m, "edited"); err != nil {
			return EditPlan{}, err
		}
	}
	if p.deferred, err = channels.CheckRevision("message", id, m.Revision, rev, mode); err != nil {
		return EditPlan{}, err
	}
	p.Request = model.EditMessage{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Message: m.Ref, Content: TextContent(text),
	}
	if err := p.Request.Validate(); err != nil {
		return EditPlan{}, err
	}
	return p, nil
}

// Edit submits p: uma queues the edit durably, and the daemon's workers
// send it to the provider. The message is untouched until the provider
// applied it.
func (s Service) Edit(ctx context.Context, p EditPlan) (model.Acceptance, error) {
	acc, err := s.Client.Messages().Edit(ctx, p.Request)
	return acc, channels.Explain(err, p.deferred)
}

// DeletePlan is a validated message deletion.
type DeletePlan struct {
	Request     model.DeleteMessage
	Message     model.Message
	Declaration *Declaration
	deferred    error
}

// PlanDelete builds and checks the deletion of message id (in
// conversation conv, record ID, when set) at revision rev, under
// operation op, as PlanEdit does. The deletion is a tombstone, never a
// privacy redaction.
func (s Service) PlanDelete(ctx context.Context, id, conv string, rev model.Revision, op model.OperationID, mode channels.Mode) (DeletePlan, error) {
	m, err := s.FindMessage(ctx, id, conv, false)
	if err != nil {
		return DeletePlan{}, err
	}
	p := DeletePlan{Message: m}
	if p.Declaration, err = s.previewDeclared(ctx, m, mode, "message delete", model.CapabilityDelete); err != nil {
		return DeletePlan{}, err
	}
	if mode == channels.Preview {
		if err := settledMessage(m, "deleted"); err != nil {
			return DeletePlan{}, err
		}
	}
	if p.deferred, err = channels.CheckRevision("message", id, m.Revision, rev, mode); err != nil {
		return DeletePlan{}, err
	}
	p.Request = model.DeleteMessage{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Message: m.Ref,
	}
	if err := p.Request.Validate(); err != nil {
		return DeletePlan{}, err
	}
	return p, nil
}

// Delete submits p.
func (s Service) Delete(ctx context.Context, p DeletePlan) (model.Acceptance, error) {
	acc, err := s.Client.Messages().Delete(ctx, p.Request)
	return acc, channels.Explain(err, p.deferred)
}

// ReactPlan is a validated reaction, added or removed.
type ReactPlan struct {
	Request     model.Reaction
	Message     model.Message
	Declaration *Declaration
	deferred    error
}

// PlanReact builds and checks the caller's reaction emoji on message id
// (in conversation conv, record ID, when set), removed when remove is
// set, under operation op. rev is optional: uma compares it only when
// set. As PlanEdit, a preview refuses an adapter that does not declare
// react; whether the provider accepts this emoji is the provider's
// (github.issues takes eight), answered when the reaction is applied.
func (s Service) PlanReact(ctx context.Context, id, conv, emoji string, remove bool, rev model.Revision, op model.OperationID, mode channels.Mode) (ReactPlan, error) {
	if err := model.ValidateEmoji(emoji); err != nil {
		return ReactPlan{}, model.NewError(model.Validation, "emoji: "+err.Error())
	}
	m, err := s.FindMessage(ctx, id, conv, false)
	if err != nil {
		return ReactPlan{}, err
	}
	verb := "reaction add"
	if remove {
		verb = "reaction remove"
	}
	p := ReactPlan{Message: m}
	if p.Declaration, err = s.previewDeclared(ctx, m, mode, verb, model.CapabilityReact); err != nil {
		return ReactPlan{}, err
	}
	if rev != "" {
		if p.deferred, err = channels.CheckRevision("message", id, m.Revision, rev, mode); err != nil {
			return ReactPlan{}, err
		}
	}
	p.Request = model.Reaction{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Message: m.Ref, Emoji: emoji, Remove: remove,
	}
	if err := p.Request.Validate(); err != nil {
		return ReactPlan{}, err
	}
	return p, nil
}

// React submits p.
func (s Service) React(ctx context.Context, p ReactPlan) (model.Acceptance, error) {
	acc, err := s.Client.Messages().React(ctx, p.Request)
	return acc, channels.Explain(err, p.deferred)
}

// previewDeclared is the declaration check of a message verb, run in a
// preview only.
func (s Service) previewDeclared(ctx context.Context, m model.Message, mode channels.Mode, verb string, c model.Capability) (*Declaration, error) {
	if mode != channels.Preview {
		return nil, nil
	}
	in, err := s.messageInbox(ctx, m)
	if err != nil {
		return nil, err
	}
	d, err := s.Declared(ctx, in, verb, c)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// settledMessage refuses a message the provider verb cannot reach: one
// already deleted or redacted. uma refuses it too (a conflict); a
// preview says so before drawing a plan.
func settledMessage(m model.Message, what string) error {
	switch {
	case m.Deleted:
		return model.NewError(model.Conflict, fmt.Sprintf("message %s is deleted: it cannot be %s", m.Ref.ID, what))
	case m.Redacted:
		return model.NewError(model.Conflict, fmt.Sprintf("message %s is redacted: it cannot be %s", m.Ref.ID, what))
	}
	return nil
}

// ReactionList is the reactions on one message, as uma stores them.
type ReactionList struct {
	Message   model.Message
	Reactions []model.MessageReaction
	// Complete reports that uma's reaction state came from an
	// authoritative provider snapshot. State folded from reaction events
	// is not: the provider may hold reactions uma never heard of.
	Complete bool
}

// Reactions returns the reactions on message id (in conversation conv,
// record ID, when set) that the caller may read. A read: no capability
// gate, and what it shows is as complete as uma's record says.
func (s Service) Reactions(ctx context.Context, id, conv string) (ReactionList, error) {
	m, err := s.FindMessage(ctx, id, conv, false)
	if err != nil {
		return ReactionList{}, err
	}
	rs := m.Reactions
	if rs == nil {
		rs = []model.MessageReaction{}
	}
	return ReactionList{Message: m, Reactions: rs, Complete: m.ReactionsComplete}, nil
}

// Participant is one participant of a conversation: the identity, its
// role there and what uma knows of how sure the identity is.
type Participant struct {
	Identity model.IdentityRef
	Role     string
	// Assurance is the identity's own assurance level, empty when the
	// caller may not read identities.
	Assurance string
	// Contact is the contact the identity points at, empty likewise.
	Contact model.ID
}

// Participants returns the participants of conversation id with, where
// the caller may read the inbox's identities, their assurance. A read:
// availability is whatever uma recorded from ingress.
func (s Service) Participants(ctx context.Context, id string) (model.Conversation, []Participant, error) {
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return model.Conversation{}, nil, err
	}
	out := make([]Participant, 0, len(conv.Participants))
	if len(conv.Participants) == 0 {
		return conv, out, nil
	}
	known := map[model.IdentityRef]model.Identity{}
	req := model.ListRequest{Call: call(), Filter: model.Filter{Inboxes: []model.InboxRef{conv.Ref.Inbox}}, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Contacts().Identities(ctx, req)
		if err != nil {
			return model.Conversation{}, nil, err
		}
		for _, i := range p.Items {
			known[i.Ref] = i
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.Page.After = p.Next
	}
	for _, pt := range conv.Participants {
		row := Participant{Identity: pt.Identity, Role: pt.Role}
		if i, ok := known[pt.Identity]; ok {
			row.Assurance, row.Contact = i.Assurance, i.Contact.ID
		}
		out = append(out, row)
	}
	return conv, out, nil
}

// validateAction runs the channel adapter's offline action check, as
// uma's Extensions.Execute does before it stores an effect: the first
// issue is the error, with uma's code rules.
func (s Service) validateAction(ctx context.Context, ch model.ChannelRef, aud model.Audience, action model.Extension) error {
	if s.ValidateAction == nil {
		return nil
	}
	issues, err := s.ValidateAction(ctx, ch, aud, action)
	if err != nil {
		return err
	}
	if len(issues) == 0 {
		return nil
	}
	i := issues[0]
	code := model.Validation
	switch model.ErrorCode(i.Code) {
	case model.Unsupported, model.SchemaMismatch:
		code = model.ErrorCode(i.Code)
	}
	msg := "action." + i.Field
	if i.Message != "" {
		msg += ": " + i.Message
	}
	return model.NewError(code, msg)
}
