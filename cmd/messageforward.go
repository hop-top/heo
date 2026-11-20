package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// forwardOut is what `message forward` reports: the conversation as uma
// holds it (untouched), the message forwarded and the provider's ID for
// it, where the copy goes, the effect run, the operation and its key.
type forwardOut struct {
	Conversation model.Conversation `json:"conversation"        yaml:"conversation"`
	Message      string             `json:"message"             yaml:"message"`
	Provider     string             `json:"provider_message"    yaml:"provider_message"`
	To           string             `json:"to"                  yaml:"to"`
	Effect       string             `json:"effect"              yaml:"effect"`
	Operation    operationView      `json:"operation"           yaml:"operation"`
	OperationKey string             `json:"operation_key"       yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty"     yaml:"notes,omitempty"`
}

// forwardCarries is what the preview, the help and the docs say the copy
// carries: the original whole, never a rewritten body.
const forwardCarries = "the original attached whole as message/rfc822 (its headers, body and attachments, byte for byte), " +
	"under a note; no inline rewrite of it"

// forwardFinds says how the copy is found in the destination's mailbox.
const forwardFinds = "the copy's References names the original's Message-ID and it has no In-Reply-To, so it is no reply and starts its own thread; " +
	"`heo message list --references <original Message-ID>` finds it where the destination is a heo inbox, " +
	"and the copy's own Message-ID, reported on the operation (`delivery get`), finds it with `--message-id`"

// forwardPlan renders a forward plan for --dry-run: the recipient, that
// the original travels attached, its size, and where it goes. It sends
// and writes nothing.
func forwardPlan(p desk.ForwardPlan, key string) *cli.Plan {
	cur := p.Conversation
	short := "size unknown"
	size := "unknown (the channel cannot serve the original's bytes now); the adapter reads it again when the operation runs"
	if p.Size >= 0 {
		short = fmt.Sprintf("%d bytes", p.Size)
		size = short + " as the provider serves it now (the attachment, before the note and transfer encoding); the adapter reads it again when the operation runs"
	}
	args := map[string]any{
		"message": p.Message, "provider_message": p.Provider, "conversation": cur.Ref.ID, "inbox": cur.Ref.Inbox.ID,
		"to": p.Forward.To, "note": p.Forward.Note(), "subject": "Fwd: <the original's subject>",
		"attachment": "the original, as message/rfc822", "carries": forwardCarries, "size": size,
		"finds": forwardFinds, "visibility": p.Request.Audience.Visibility,
		"change":   p.Request.Action.Name + " " + string(p.Request.Action.Data),
		"delivery": "queued, not applied: the adapter reads the original again from the provider when the daemon's workers run it; nothing changes in uma",
	}
	if p.OriginalID != "" {
		args["original_message_id"] = p.OriginalID
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := effectArgs(args, p.Declaration)
	effect := planEffect("send", "mail:message "+p.Provider, false, fmt.Sprintf(
		"external and irreversible: send a copy of provider message %s of conversation %s to %s, the original attached whole as message/rfc822 (%s, %s); "+
			"a mail leaves for a third party and cannot be recalled",
		p.Provider, cur.Ref.ID, p.Forward.To, p.Request.Action.Name, short))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: append(checked, "message", "request")}
}

func forwardRows(o forwardOut) [][2]string {
	return append([][2]string{
		{"message", o.Message},
		{"provider message", orDash(o.Provider)},
		{"to", o.To},
		{"conversation", string(o.Conversation.Ref.ID)},
		{"effect", o.Effect},
	}, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) messageForwardCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "forward <message>",
		Short: "Forward a message to another address with the original attached",
		Long: `Send a copy of the message <message> (record ID) from its own mailbox to
the address --to, with the original attached whole as message/rfc822
(RFC 2046): its headers, body and attachments, byte for byte, under the
note --text (a fixed line when there is none). The adapter reads the
original again from the provider when the operation runs; heo composes
none of it. The copy is Fwd: and the original's subject, from the
mailbox.

The copy is findable. Its References names the original's Message-ID and
it carries no In-Reply-To, so it is no reply and starts its own thread. In
a heo mailbox that receives it, ` + "`message list --references <original Message-ID>`" + `
finds it, and the copy's own Message-ID, which the operation reports once
the provider accepted it (` + "`delivery get <operation>`" + `), finds it with
` + "`message list --message-id`" + `. Forwarding does not change the original:
trash or archive it with the state verbs, after ` + "`delivery get`" + ` says the
copy left.

Only a channel whose adapter declares extension_execute and ships its
forward effect can do this (google.mail). An adapter that cannot attach
the original whole (microsoft.mail, imap.mail, smtp.mail) is refused,
exit 2 (UNSUPPORTED), in --dry-run and for real, before anything is
written: nothing is sent as an inline, lossy copy instead. An internal
note, a deleted or redacted message, and a message the provider holds no
copy of are refused (exit 2 or 4).

This is external and irreversible: a mail leaves for a third party and
cannot be recalled. It is a plugin actor's write that heo holds for a
decision while held_operations is on (see ` + "`operation held`" + `), and approving
it is the yes. --dry-run previews exactly this, with the recipient, the
attachment of the original and its size, and sends nothing.

The command returns once uma accepted it durably: queued, never applied.
The daemon's workers run it, and ` + "`delivery get <operation>`" + ` follows it. Committed
under --idempotency-key: rerunning with the same key and arguments
returns the same operation (replayed) and sends once; without one, heo
mints a key and prints it. Desk role.`,
		Example: `  heo message forward msg_123 --to desk@example.com --dry-run
  heo message forward msg_123 --to desk@example.com --text "Yours to handle." --idempotency-key fwd-123`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var f desk.MailForward
			f.To, _ = cmd.Flags().GetString(flagTo)
			f.Text, _ = cmd.Flags().GetString(flagText)
			if err := desk.CheckMailForward(f); err != nil {
				return output.UsageError(strings.TrimPrefix(err.Error(), "validation: "))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[forwardOut], error) {
				var w written[forwardOut]
				err := a.deskHeld(ctx, c, model.ActionExtensionExecute, internalAudience, func(s desk.Service) error {
					p, err := s.PlanForward(ctx, args[0], f, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = forwardPlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionExtensionExecute, Verb: "forward",
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Preview: forwardPlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.ForwardMessage(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = forwardOut{Conversation: conv, Message: p.Message, Provider: p.Provider, To: f.To,
						Effect: p.Request.Action.Name, Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = append(queuedNote(w.Out.Operation), fmt.Sprintf(
						"not yet sent: `heo delivery get %s` shows whether the provider accepted it and the copy's Message-ID", acc.Operation.Ref.ID))
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, forwardRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagTo, "", "Address to forward to: one bare address (required)")
	_ = c.MarkFlagRequired(flagTo)
	c.Flags().String(flagText, "", "Note written above the attached original (default: a fixed line)")
	providerDependent(c, model.CapabilityExtensionExecute)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &forwardOut{}, "1.0")
	return c
}
