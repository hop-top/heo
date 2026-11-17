package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// Flags that address a reply beyond the conversation's reply route.
const (
	flagReplyAll = "reply-all"
	flagCc       = "cc"
	flagBcc      = "bcc"
)

// addRecipientFlags adds --reply-all, --cc and --bcc to a reply verb.
func addRecipientFlags(c *cobra.Command) {
	c.Flags().Bool(flagReplyAll, false, "Also reply to the other recipients of the message answered (the mailbox itself dropped); needs reply_recipients")
	c.Flags().StringSlice(flagCc, nil, "Add this address to the reply's cc; repeat or comma-separate; needs reply_recipients")
	c.Flags().StringSlice(flagBcc, nil, "Add this address to the reply's bcc, which no recipient sees; repeat or comma-separate; needs reply_recipients")
}

// recipientsOf reads the recipient flags of a reply verb.
func recipientsOf(cmd *cobra.Command) model.ReplyRecipients {
	var r model.ReplyRecipients
	r.ReplyAll, _ = cmd.Flags().GetBool(flagReplyAll)
	r.Cc, _ = cmd.Flags().GetStringSlice(flagCc)
	r.Bcc, _ = cmd.Flags().GetStringSlice(flagBcc)
	return r
}

// recipientArgs adds the recipients a reply names to a plan's
// arguments, and what a preview checked of the channel, when it did.
// Nothing is added for a reply that names none.
func recipientArgs(args map[string]any, r *model.ReplyRecipients, decl *desk.Declaration, l *model.RecipientLimits) {
	if r == nil {
		return
	}
	args["recipients"] = desk.RecipientsLine(*r)
	if r.ReplyAll {
		args["reply_all"] = true
	}
	if len(r.Cc) > 0 {
		args["cc"] = r.Cc
	}
	if len(r.Bcc) > 0 {
		args["bcc"] = r.Bcc
	}
	if decl != nil {
		args["recipients_capability"] = decl.Line()
	}
	if l != nil {
		args["recipient_limits"] = fmt.Sprintf("reply_all=%t cc=%t bcc=%t max_recipients=%d", l.ReplyAll, l.Cc, l.Bcc, l.MaxRecipients)
	}
}

// recipientNote says what a reply-all adds, which only the adapter
// knows: it reads the answered message's own recipients when it sends.
const recipientNote = "reply-all adds the other recipients (To and Cc) of the message answered, minus the mailbox itself, as the adapter reads them when it sends"

// addedRecipients is r as uma carries it, nil when it adds none or is
// unreadable (a plan refused it earlier).
func addedRecipients(r model.ReplyRecipients) *model.ReplyRecipients {
	n, err := desk.NormalizeRecipients(r)
	if err != nil || n.IsZero() {
		return nil
	}
	return &n
}
