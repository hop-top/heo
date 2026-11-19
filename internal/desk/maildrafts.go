package desk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

// Mail extension effects beyond the tags: a reply kept as a provider
// draft, and an unsubscribe. Both are effects of the mail adapters
// (google.mail, microsoft.mail), one per adapter, so the effect a verb
// runs is the one the channel's adapter ships.

// mailEffect is the name of a mail effect on the channel's adapter: the
// Gmail one for google.mail, the Graph one otherwise. The version is the
// same for both adapters, and the offline gate refuses an adapter that
// ships neither.
func mailEffect(adapter model.TypeKey, gmail, graph string) (string, uint32) {
	if adapter == model.TypeMicrosoftMail {
		return graph, microsoft.ExtensionVersion
	}
	return gmail, google.ExtensionVersion
}

// MailDraft is a change to a reply the mailbox keeps as a draft: create
// one (Text), replace one's content (Draft and Text) or delete one (Draft
// and Delete). The provider sends nothing; the person finishes the draft
// in their mail client.
type MailDraft struct {
	// Text is the reply's text. Required to create or update; absent on
	// a delete.
	Text string
	// Draft is the provider's draft ID, as an earlier create reported it.
	Draft string
	// Delete removes Draft. Permanent: the provider keeps no copy.
	Delete bool
	// Recipients address the draft beyond the reply route's address:
	// reply-all, cc and bcc. None on a delete. Where an adapter leaves a
	// person's changes to an existing draft alone (Outlook's update), it
	// applies them on create only.
	Recipients model.ReplyRecipients
}

// Verb names the change: create, update or delete.
func (d MailDraft) Verb() string {
	switch {
	case d.Delete:
		return "delete"
	case d.Draft != "":
		return "update"
	}
	return "create"
}

// CheckMailDraft checks the change asked for: the usage rules, which need
// no record. The adapter's own rules (blank text, a padded draft ID) are
// its own, checked by the preview.
func CheckMailDraft(d MailDraft) error {
	switch {
	case d.Delete && d.Draft == "":
		return model.NewError(model.Validation, "draft: --delete needs the draft ID to remove (--draft)")
	case d.Delete && d.Text != "":
		return model.NewError(model.Validation, "draft: --delete removes a draft and takes no --text")
	case !d.Delete && d.Text == "":
		return model.NewError(model.Validation, "draft: --text is required to create or replace a draft")
	case d.Delete && !d.Recipients.IsZero():
		return model.NewError(model.Validation, "draft: --delete removes a draft and takes no "+RecipientFlags)
	}
	_, err := NormalizeRecipients(d.Recipients)
	return err
}

// Data is the effect's action data, as the adapter reads it.
func (d MailDraft) Data() (json.RawMessage, error) {
	m := map[string]any{}
	if d.Text != "" {
		m["text"] = d.Text
	}
	if d.Draft != "" {
		m["draft"] = d.Draft
	}
	if d.Delete {
		m["delete"] = true
	}
	if r, err := NormalizeRecipients(d.Recipients); err == nil && !r.IsZero() {
		if r.ReplyAll {
			m["reply_all"] = true
		}
		if len(r.Cc) > 0 {
			m["cc"] = r.Cc
		}
		if len(r.Bcc) > 0 {
			m["bcc"] = r.Bcc
		}
	}
	return json.Marshal(m)
}

// DraftPlan is a validated reply draft: the mail adapter's effect on the
// conversation, and the provider message the reply answers.
type DraftPlan struct {
	Request      model.ExecuteExtension
	Conversation model.Conversation
	Draft        MailDraft
	// Message is the provider message the reply answers: the one the
	// conversation's reply route names, the latest the peer wrote. Empty
	// when the route names none: the provider then starts a new thread.
	Message string
	// Declaration is set by a preview only.
	Declaration *EffectDeclaration
	// Recipients is the declaration of reply_recipients and Limits the
	// limits the channel reports now, both set by a preview of a draft
	// with recipients.
	Recipients *Declaration
	Limits     *model.RecipientLimits
	// Facts are what the draft says about itself for the hold point:
	// AddedCC when it names recipients beyond the reply route's.
	Facts HoldFacts
}

// PlanReplyDraft builds and checks the draft of a reply in conversation
// id, under operation op. It writes nothing: the draft goes to the
// conversation's reply route address, as a reply does, and to the
// recipients it names (reply-all, cc, bcc) when its channel's adapter
// declares reply_recipients, as a reply with recipients needs.
// The channel's adapter must declare extension_execute and ship its
// reply_draft effect, in a preview and on submission. A preview also asks
// the adapter, offline, to check the fields (model.Validation); a
// submission leaves that to uma.
func (s Service) PlanReplyDraft(ctx context.Context, id string, d MailDraft, op model.OperationID, mode channels.Mode) (DraftPlan, error) {
	if err := CheckMailDraft(d); err != nil {
		return DraftPlan{}, err
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return DraftPlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return DraftPlan{}, err
	}
	decl, _, err := s.declared(ctx, in, "message reply-draft", model.CapabilityExtensionExecute)
	if err != nil {
		return DraftPlan{}, err
	}
	name, version := mailEffect(decl.Adapter, google.ExtensionReplyDraft, microsoft.ExtensionReplyDraft)
	eff, err := s.effectDeclared(ctx, in, "message reply-draft", name, "")
	if err != nil {
		return DraftPlan{}, err
	}
	data, err := d.Data()
	if err != nil {
		return DraftPlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: conv.Ref.Ref(), Audience: effectAudience,
		Action: model.Extension{Name: name, Version: version, Data: data},
	}
	if err := req.Validate(); err != nil {
		return DraftPlan{}, err
	}
	p := DraftPlan{Request: req, Conversation: conv, Draft: d, Message: conv.ReplyRoute.ParentID}
	if rec, _ := NormalizeRecipients(d.Recipients); !rec.IsZero() {
		p.Facts.AddedCC = true
		if p.Limits, p.Recipients, err = s.gateRecipients(ctx, in, conv, "message reply-draft", rec, mode == channels.Preview); err != nil {
			return DraftPlan{}, err
		}
	}
	if mode == channels.Preview {
		p.Declaration = &eff
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return DraftPlan{}, err
		}
	}
	return p, nil
}

// SubmitReplyDraft submits p: uma queues the effect, and the daemon's
// workers send it to the mail provider. It returns the conversation as
// uma holds it, which the draft does not touch: a draft is no message of
// uma's.
func (s Service) SubmitReplyDraft(ctx context.Context, p DraftPlan) (model.Conversation, model.Acceptance, error) {
	acc, err := s.Client.Extensions().Execute(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, err
	}
	c, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Conversation.Ref})
	return c, acc, err
}

// Unsubscribe methods: what the adapter may use, named by its data. The
// adapter decides from the message's own headers, never from the caller;
// a method narrows what it may do, it never widens it.
const (
	UnsubscribeAuto     = "auto"
	UnsubscribeOneClick = "one_click"
	UnsubscribeMailto   = "mailto"
)

// UnsubscribeMethods are the methods the verb takes.
var UnsubscribeMethods = []string{UnsubscribeAuto, UnsubscribeOneClick, UnsubscribeMailto}

// CheckUnsubscribeMethod checks a method name; empty means auto.
func CheckUnsubscribeMethod(m string) error {
	if m == "" {
		return nil
	}
	for _, x := range UnsubscribeMethods {
		if m == x {
			return nil
		}
	}
	return model.NewError(model.Validation, fmt.Sprintf("method %q: want one of %s", m, strings.Join(UnsubscribeMethods, ", ")))
}

// UnsubscribePlan is a validated unsubscribe: the mail adapter's effect on
// the conversation, aimed at the provider message the peer wrote.
type UnsubscribePlan struct {
	Request      model.ExecuteExtension
	Conversation model.Conversation
	// Method is the method the adapter may use: auto, one_click or mailto.
	Method string
	// Message is the provider message whose List-Unsubscribe headers the
	// adapter reads at the attempt.
	Message string
	// Declaration is set by a preview only.
	Declaration *EffectDeclaration
}

// PlanUnsubscribe builds and checks the unsubscribe from the sender of
// the message conversation id replies to (its first message), under
// operation op. It writes
// nothing, and neither reads nor sends a URL: the adapter reads the
// message's headers again from the provider when the effect runs, and
// follows only what they permit (an RFC 8058 one-click POST, or a mailto
// in the sender's own organization). A conversation whose reply route
// names no provider message has nothing to unsubscribe from
// (model.Conflict). The channel's adapter must declare extension_execute
// and ship its unsubscribe effect, in a preview and on submission.
func (s Service) PlanUnsubscribe(ctx context.Context, id, method string, op model.OperationID, mode channels.Mode) (UnsubscribePlan, error) {
	return s.PlanUnsubscribeMessage(ctx, id, method, "", op, mode)
}

// PlanUnsubscribeMessage is PlanUnsubscribe for the message of the
// conversation named by its record ID message (a message of the
// conversation the provider holds, else model.Validation); empty is the
// message its reply route names, the first the conversation held.
func (s Service) PlanUnsubscribeMessage(ctx context.Context, id, method, message string, op model.OperationID, mode channels.Mode) (UnsubscribePlan, error) {
	if err := CheckUnsubscribeMethod(method); err != nil {
		return UnsubscribePlan{}, err
	}
	if method == "" {
		method = UnsubscribeAuto
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return UnsubscribePlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return UnsubscribePlan{}, err
	}
	decl, _, err := s.declared(ctx, in, "conversation unsubscribe", model.CapabilityExtensionExecute)
	if err != nil {
		return UnsubscribePlan{}, err
	}
	name, version := mailEffect(decl.Adapter, google.ExtensionUnsubscribe, microsoft.ExtensionUnsubscribe)
	eff, err := s.effectDeclared(ctx, in, "conversation unsubscribe", name, "")
	if err != nil {
		return UnsubscribePlan{}, err
	}
	parent, target, err := s.effectMessage(ctx, conv, message, "unsubscribe from")
	if err != nil {
		return UnsubscribePlan{}, err
	}
	data, err := json.Marshal(map[string]string{"method": method})
	if err != nil {
		return UnsubscribePlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: conv.Ref.Ref(), Audience: effectAudience,
		Action: model.Extension{Name: name, Version: version, Data: data}, Message: target,
	}
	if err := req.Validate(); err != nil {
		return UnsubscribePlan{}, err
	}
	p := UnsubscribePlan{Request: req, Conversation: conv, Method: method, Message: parent}
	if mode == channels.Preview {
		p.Declaration = &eff
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return UnsubscribePlan{}, err
		}
	}
	return p, nil
}

// Unsubscribe submits p: uma queues the effect, and the daemon's workers
// run it against the provider. It is irreversible: a request has gone to
// a third party. It returns the conversation as uma holds it, untouched.
func (s Service) Unsubscribe(ctx context.Context, p UnsubscribePlan) (model.Conversation, model.Acceptance, error) {
	acc, err := s.Client.Extensions().Execute(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, err
	}
	c, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Conversation.Ref})
	return c, acc, err
}
