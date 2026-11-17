package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"hop.top/uma/model"
)

// Reply recipients: who a reply or a reply draft is addressed to beyond
// the conversation's stored reply route. The route address stays the
// first recipient; reply-all adds the recipients of the message the
// reply answers (the adapter reads them, minus the mailbox itself), and
// cc and bcc are explicit bare addresses. A channel carries them only
// where its adapter declares reply_recipients, within the limits it
// reports.

// NormalizeRecipients checks the recipients asked for and returns them
// as uma will carry them: addresses normalized and deduplicated, an
// address of cc dropped from bcc, at most model.MaxReplyRecipients in
// cc and bcc together. Anything else is model.Validation. It needs no
// record and no adapter.
func NormalizeRecipients(r model.ReplyRecipients) (model.ReplyRecipients, error) {
	n, err := r.Normalize()
	if err != nil {
		return model.ReplyRecipients{}, model.NewError(model.Validation, fmt.Sprintf(
			"recipients: --cc and --bcc take bare addresses (name@host), at most %d in all", model.MaxReplyRecipients))
	}
	return n, nil
}

// RecipientFlags names the flags that carry recipients, as a verb's
// declaration line names them.
const RecipientFlags = "--reply-all, --cc, --bcc"

// gateRecipients checks that the channel of conversation conv can carry
// r for verb: its adapter declares reply_recipients (offline, always),
// and, in a preview, that the live limits it reports allow what r asks.
// It returns the limits it read, nil when it read none. r must be
// non-zero; the declaration and the limits are returned from a preview
// only, as for every verb. A preview that cannot read limits is refused, as a state
// verb's is: uma's gate would refuse the submission just the same.
func (s Service) gateRecipients(ctx context.Context, in model.Inbox, conv model.Conversation, verb string, r model.ReplyRecipients, live bool) (*model.RecipientLimits, *Declaration, error) {
	d, err := s.Declared(ctx, in, verb+" with "+RecipientFlags, model.CapabilityReplyRecipients)
	if err != nil {
		return nil, nil, err
	}
	if !live {
		return nil, nil, nil
	}
	l, err := s.recipientLimits(ctx, conv, verb)
	if err != nil {
		return nil, nil, err
	}
	if err := checkRecipientLimits(verb, r, l); err != nil {
		return nil, nil, err
	}
	return &l, &d, nil
}

// recipientLimits reads the channel's live status of reply_recipients
// for the conversation, as the caller (uma's Conversations.Capabilities,
// authorized as a read of the conversation). It opens the channel's
// adapter and may reach the provider.
func (s Service) recipientLimits(ctx context.Context, conv model.Conversation, verb string) (model.RecipientLimits, error) {
	c := model.CapabilityReplyRecipients
	statuses, err := s.Client.Conversations().Capabilities(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: conv.Ref})
	if err != nil {
		return model.RecipientLimits{}, err
	}
	i := slices.IndexFunc(statuses, func(st model.CapabilityStatus) bool { return st.Name == c })
	if i < 0 {
		return model.RecipientLimits{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"conversation %s: its channel's adapter does not report %s, which `%s` needs for %s", conv.Ref.ID, c, verb, RecipientFlags))
	}
	st := statuses[i]
	if !st.Available {
		code := st.Reason
		if code == "" {
			code = model.Unavailable
		}
		return model.RecipientLimits{}, model.NewError(code, fmt.Sprintf(
			"conversation %s: %s is %s on its channel now, so %s is refused", conv.Ref.ID, c, code, RecipientFlags))
	}
	l, err := model.ParseRecipientLimits(st.Limits)
	if err != nil {
		return model.RecipientLimits{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"conversation %s: its channel reports no readable limits for %s, so %s is refused", conv.Ref.ID, c, RecipientFlags))
	}
	return l, nil
}

// checkRecipientLimits refuses recipients the reported limits do not
// allow: reply-all, cc or bcc the channel does not send, or more
// addresses than it carries (the route address counts as one; what
// reply-all adds is known only to the adapter, which reads it).
func checkRecipientLimits(verb string, r model.ReplyRecipients, l model.RecipientLimits) error {
	refuse := func(what string) error {
		return model.NewError(model.Unsupported, fmt.Sprintf("%s: this channel does not send %s", verb, what))
	}
	switch {
	case r.ReplyAll && !l.ReplyAll:
		return refuse("reply-all (--reply-all)")
	case len(r.Cc) > 0 && !l.Cc:
		return refuse("cc (--cc)")
	case len(r.Bcc) > 0 && !l.Bcc:
		return refuse("bcc (--bcc)")
	}
	if n := 1 + len(r.Cc) + len(r.Bcc); l.MaxRecipients > 0 && n > l.MaxRecipients {
		return model.NewError(model.Unsupported, fmt.Sprintf(
			"%s: %d recipients (the reply route's address, %d cc, %d bcc), this channel carries at most %d",
			verb, n, len(r.Cc), len(r.Bcc), l.MaxRecipients))
	}
	return nil
}

// RecipientsLine describes r for a plan or a row: "reply-all, cc a, b;
// bcc c", or "-" when it adds none.
func RecipientsLine(r model.ReplyRecipients) string {
	var parts []string
	if r.ReplyAll {
		parts = append(parts, "reply-all")
	}
	if len(r.Cc) > 0 {
		parts = append(parts, "cc "+strings.Join(r.Cc, ", "))
	}
	if len(r.Bcc) > 0 {
		parts = append(parts, "bcc "+strings.Join(r.Bcc, ", "))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, "; ")
}
