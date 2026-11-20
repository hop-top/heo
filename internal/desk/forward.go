package desk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"

	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/model"
)

// MaxForwardNote bounds the note of a forward, as the adapter does.
const MaxForwardNote = 16 << 10

// MailForward is a forward of one message: the address it goes to and
// an optional note above the attached original.
type MailForward struct {
	// To is one bare address.
	To string
	// Text is the note; empty sends a fixed line instead.
	Text string
}

// CheckMailForward checks the forward asked for: the usage rules, which
// need no record. The adapter's own rules are its own, checked by the
// preview.
func CheckMailForward(f MailForward) error {
	a, err := mail.ParseAddress(f.To)
	switch {
	case f.To == "":
		return model.NewError(model.Validation, "forward: --to is required: the address to forward to")
	case err != nil || a.Name != "" || a.Address != f.To:
		return model.NewError(model.Validation, "forward: --to takes one bare address (name@host)")
	case len(f.Text) > MaxForwardNote:
		return model.NewError(model.Validation, fmt.Sprintf("forward: --text is longer than %d bytes", MaxForwardNote))
	}
	return nil
}

// Data is the effect's action data, as the adapter reads it.
func (f MailForward) Data() (json.RawMessage, error) {
	m := map[string]string{"to": f.To}
	if f.Text != "" {
		m["text"] = f.Text
	}
	return json.Marshal(m)
}

// ForwardPlan is a validated forward: the mail adapter's effect on the
// message's conversation, aimed at that message.
type ForwardPlan struct {
	Request      model.ExecuteExtension
	Conversation model.Conversation
	Forward      MailForward
	// Message is the record ID of the message forwarded and Provider the
	// provider's ID for it, which the adapter reads again at the attempt.
	Message  string
	Provider string
	// OriginalID is the original's own Message-ID header, the one the copy
	// carries in its References and a receiver finds it by; empty when
	// uma holds none.
	OriginalID string
	// Declaration and Size are set by a preview only. Size is the byte
	// count of the original as the provider serves it now, -1 when the
	// channel cannot say (no raw_read, or the read failed); the adapter
	// reads it again at the attempt.
	Declaration *EffectDeclaration
	Size        int64
}

// forwardVerb names the verb in refusals.
const forwardVerb = "message forward"

// PlanForward builds and checks the forward of the message with record ID
// id, to f.To, under operation op. It writes nothing and sends nothing.
// The message is a public message the caller may read that its provider
// holds (an internal note, a deleted or redacted message, or one with no
// provider message is refused: model.Validation or model.Conflict). The
// channel's adapter must declare extension_execute and ship its forward
// effect, in a preview and on submission: the one that attaches the
// original whole, as message/rfc822. An adapter that cannot is refused
// (model.Unsupported) rather than sent a lossy inline copy. A preview
// also asks the adapter, offline, to check the fields (model.Validation)
// and reads the original's size when the channel can serve it.
func (s Service) PlanForward(ctx context.Context, id string, f MailForward, op model.OperationID, mode channels.Mode) (ForwardPlan, error) {
	if err := CheckMailForward(f); err != nil {
		return ForwardPlan{}, err
	}
	m, err := s.FindMessage(ctx, id, "", false)
	if err != nil {
		return ForwardPlan{}, err
	}
	switch {
	case IsNote(m):
		return ForwardPlan{}, model.NewError(model.Validation, fmt.Sprintf("message %s: an internal note is not forwarded", id))
	case m.Deleted || m.Redacted:
		return ForwardPlan{}, model.NewError(model.Conflict, fmt.Sprintf("message %s: deleted or redacted; there is nothing to forward", id))
	}
	conv, err := s.FindConversation(ctx, string(m.Ref.Conversation.ID))
	if err != nil {
		return ForwardPlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return ForwardPlan{}, err
	}
	eff, err := s.effectDeclared(ctx, in, forwardVerb, google.ExtensionForward,
		"forwarding attaches the original whole as message/rfc822, which this adapter cannot do; nothing is sent")
	if err != nil {
		return ForwardPlan{}, err
	}
	provider := providerMessageID(m, conv.ReplyRoute.Channel)
	if provider == "" {
		return ForwardPlan{}, model.NewError(model.Conflict, fmt.Sprintf(
			"message %s: the provider holds no copy of it (no provider message), so there is no original to attach", id))
	}
	data, err := f.Data()
	if err != nil {
		return ForwardPlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: conv.Ref.Ref(), Audience: effectAudience,
		Action:  model.Extension{Name: google.ExtensionForward, Version: google.ExtensionVersion, Data: data},
		Message: &m.Ref,
	}
	if err := req.Validate(); err != nil {
		return ForwardPlan{}, err
	}
	p := ForwardPlan{Request: req, Conversation: conv, Forward: f, Message: id, Provider: provider, Size: -1}
	for _, x := range m.External {
		if x.Namespace == model.EmailHeaderNamespace && x.Kind == model.RefKindEmailMessageID {
			p.OriginalID = x.ID
		}
	}
	if mode == channels.Preview {
		p.Declaration = &eff
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return ForwardPlan{}, err
		}
		p.Size = s.originalSize(ctx, in, m)
	}
	return p, nil
}

// originalSize is the size of m's provider copy, -1 when the channel
// cannot serve it. A failed read is no reason to refuse a preview: the
// adapter reads the original again when the operation runs.
func (s Service) originalSize(ctx context.Context, in model.Inbox, m model.Message) int64 {
	if _, err := s.Declared(ctx, in, forwardVerb, model.CapabilityRawRead); err != nil {
		return -1
	}
	raw, err := s.Client.Messages().Raw(ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: m.Ref})
	if err != nil {
		return -1
	}
	return raw.Size
}

// ForwardMessage submits p: uma queues the effect, and the daemon's
// workers send it through the mail provider. It returns the conversation
// as uma holds it, which the forward does not touch: the copy is the
// provider's, and arrives, if at all, in the destination's own mailbox.
func (s Service) ForwardMessage(ctx context.Context, p ForwardPlan) (model.Conversation, model.Acceptance, error) {
	acc, err := s.Client.Extensions().Execute(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, err
	}
	c, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Conversation.Ref})
	return c, acc, err
}

// Note is the note as it is shown: the text, or the fixed line the adapter
// sends when there is none.
func (f MailForward) Note() string {
	if strings.TrimSpace(f.Text) == "" {
		return "Forwarded message attached."
	}
	return f.Text
}
