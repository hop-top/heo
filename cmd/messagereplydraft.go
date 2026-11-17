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

// Flags of `message reply-draft`.
const (
	flagDraft  = "draft"
	flagDelete = "delete"
)

// replyDraftOut is what `message reply-draft` reports: the conversation
// as uma holds it (a draft is no message of uma's), the provider message
// the reply answers, the change, the effect run, the operation and its
// key. The draft's ID is the provider's: it is on the operation once the
// provider has made the draft (`delivery get`).
type replyDraftOut struct {
	Conversation model.Conversation `json:"conversation"       yaml:"conversation"`
	Message      string             `json:"message,omitempty"  yaml:"message,omitempty"`
	Change       string             `json:"change"             yaml:"change"`
	Draft        string             `json:"draft,omitempty"    yaml:"draft,omitempty"`
	// Recipients are the recipients the draft adds beyond the reply
	// route's address, as asked; absent when it adds none.
	Recipients   *model.ReplyRecipients `json:"recipients,omitempty" yaml:"recipients,omitempty"`
	Effect       string                 `json:"effect"             yaml:"effect"`
	Operation    operationView          `json:"operation"          yaml:"operation"`
	OperationKey string                 `json:"operation_key"      yaml:"operation_key"`
	Notes        []string               `json:"notes,omitempty"    yaml:"notes,omitempty"`
}

// replyDraftPlan renders a reply draft plan for --dry-run: the external
// change at the mail provider's Drafts, and nothing in uma. It sends
// nothing, and names exactly one recipient: the reply route's.
func replyDraftPlan(p desk.DraftPlan, key string) *cli.Plan {
	cur := p.Conversation
	args := map[string]any{
		"conversation": cur.Ref.ID, "inbox": cur.Ref.Inbox.ID, "destination": cur.ReplyRoute,
		"visibility": p.Request.Audience.Visibility, "message": orDash(p.Message),
		"change":      p.Request.Action.Name + " " + string(p.Request.Action.Data),
		"change_kind": p.Draft.Verb(),
		"current":     cur,
		"delivery":    "queued, not applied: the provider keeps a draft for the person to finish and send in their mail client; nothing is sent, nothing changes in uma",
	}
	if p.Draft.Draft != "" {
		args["draft"] = p.Draft.Draft
	}
	if p.Draft.Text != "" {
		args["content"] = p.Draft.Text
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := effectArgs(args, p.Declaration)
	to := routeName(cur.ReplyRoute)
	if rec, _ := desk.NormalizeRecipients(p.Draft.Recipients); !rec.IsZero() {
		recipientArgs(args, &rec, p.Recipients, p.Limits)
		to += " and " + desk.RecipientsLine(rec)
		checked = append(checked, "recipients")
		if p.Recipients != nil {
			checked = append(checked, "adapter capability (declared)")
		}
		if p.Limits != nil {
			checked = append(checked, "recipient limits (live)")
		}
	}
	detail := fmt.Sprintf("external: %s a draft reply to %s in the mailbox's Drafts, answering message %s of conversation %s (%s); not sent",
		p.Draft.Verb(), to, orDash(p.Message), cur.Ref.ID, p.Request.Action.Name)
	reversible := p.Draft.Verb() != "delete"
	if !reversible {
		detail += "; a deleted draft is gone for good (the provider keeps no copy)"
	}
	effect := planEffect(map[string]string{"create": "create", "update": "update", "delete": "delete"}[p.Draft.Verb()],
		"mail:drafts conversation:"+string(cur.Ref.ID), reversible, detail)
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: append(checked, "conversation", "request")}
}

func replyDraftRows(o replyDraftOut) [][2]string {
	rows := [][2]string{
		{"conversation", string(o.Conversation.Ref.ID)},
		{"answers message", orDash(o.Message)},
		{"change", o.Change},
	}
	if o.Draft != "" {
		rows = append(rows, [2]string{"draft", o.Draft})
	}
	if o.Recipients != nil {
		rows = append(rows, [2]string{"recipients", desk.RecipientsLine(*o.Recipients)})
	}
	rows = append(rows, [2]string{"effect", o.Effect})
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) messageReplyDraftCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reply-draft <conversation>",
		Short: "Keep a reply as a draft in the mailbox, for a person to finish and send",
		Long: `Leave a reply to the mail conversation <conversation> (record ID) in
the mailbox's own Drafts, with --text, for a person to finish and send
in their mail client. Nothing is sent: ` + "`message reply`" + ` sends; this does
not. The draft is composed as a reply is: from the mailbox, to the
conversation's reply route address, with the subject, In-Reply-To and
References of the message the peer wrote, in that thread. It adds
recipients only when asked: --reply-all (the other recipients of the
message answered, the mailbox itself dropped), --cc and --bcc (explicit
bare addresses, at most 50 in all), which need reply_recipients on the
channel (exit 2, UNSUPPORTED, where it is not declared or the limits it
reports refuse them). They address a draft on create (google.mail also
on update; microsoft.mail leaves an existing draft's recipients as a
person changed them) and never a delete. It is the mailbox's draft, not a message of uma's:
` + "`message list`" + ` never shows it, and ` + "`message draft`" + ` (which only writes
text for you to read) is unrelated.

Three changes, named by flags:

  --text T                   create the draft
  --draft ID --text T        replace that draft's content (a person's
                             edit made since is overwritten)
  --draft ID --delete        delete that draft (permanent: the provider
                             keeps no copy)

The draft ID is the provider's. The create reports none up front: the
provider makes the draft when the daemon's workers run the operation,
and ` + "`delivery get <operation>`" + ` shows it among the provider references.
Use a new --idempotency-key for each update or delete. An update or a
delete touches only a draft this command made for this conversation; any
other draft of the mailbox, one a person wrote included, is refused.

It is an external change, queued as a uma operation: the command
returns once uma accepted it durably, never applied. Only a channel
whose adapter declares extension_execute and ships its reply_draft
effect can do this (google.mail, microsoft.mail); --dry-run refuses any
other, exit 2 (UNSUPPORTED), as does the real run, before anything is
written. --dry-run also asks the adapter, offline, to check the fields
(VALIDATION, exit 2) and prints the exact change and where it goes; it
writes and sends nothing. Committed under --idempotency-key: rerunning
with the same key and arguments returns the same operation (replayed);
without one, heo mints a key and prints it. Desk role.`,
		Example: `  heo message reply-draft conv_123 --text "Thanks, we'll look into it." --dry-run
  heo message reply-draft conv_123 --text "Thanks, we'll look into it."
  heo message reply-draft conv_123 --text "Looping you in." --reply-all --cc lead@example.com --dry-run
  heo message reply-draft conv_123 --draft r-77 --text "Updated wording." --idempotency-key rd-2
  heo message reply-draft conv_123 --draft r-77 --delete --idempotency-key rd-3`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var d desk.MailDraft
			d.Text, _ = cmd.Flags().GetString(flagText)
			d.Draft, _ = cmd.Flags().GetString(flagDraft)
			d.Delete, _ = cmd.Flags().GetBool(flagDelete)
			d.Recipients = recipientsOf(cmd)
			if err := desk.CheckMailDraft(d); err != nil {
				return output.UsageError(strings.TrimPrefix(err.Error(), "validation: "))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[replyDraftOut], error) {
				var w written[replyDraftOut]
				err := a.desk(ctx, c, model.ActionMessageReply, internalAudience, func(s desk.Service) error {
					p, err := s.PlanReplyDraft(ctx, args[0], d, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = replyDraftPlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionMessageReply, Verb: "reply", Class: "draft",
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Facts: p.Facts, Preview: replyDraftPlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.SubmitReplyDraft(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = replyDraftOut{Conversation: conv, Message: p.Message, Change: d.Verb(), Draft: d.Draft,
						Recipients: addedRecipients(d.Recipients), Effect: p.Request.Action.Name, Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = append(queuedNote(w.Out.Operation), fmt.Sprintf(
						"nothing is sent: `heo delivery get %s` follows the draft to the mailbox and shows its provider ID", acc.Operation.Ref.ID))
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, replyDraftRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagText, "", "Text of the draft reply (required unless --delete)")
	c.Flags().String(flagDraft, "", "The provider's draft ID, to replace or delete a draft this command made")
	c.Flags().Bool(flagDelete, false, "Delete the draft named by --draft (permanent)")
	addRecipientFlags(c)
	providerDependent(c, model.CapabilityExtensionExecute)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &replyDraftOut{}, Version: "1.0"})
	return c
}
