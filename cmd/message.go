package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// replyOut is what a reply reports: the message uma recorded for it,
// where it goes, under which visibility, and the operation that
// delivers it — queued, never delivered, when the command returns.
type replyOut struct {
	Message      model.ID          `json:"message"       yaml:"message"`
	Conversation model.ID          `json:"conversation"  yaml:"conversation"`
	Inbox        model.ID          `json:"inbox"         yaml:"inbox"`
	Visibility   model.Visibility  `json:"visibility"    yaml:"visibility"`
	Destination  model.ExternalRef `json:"destination"   yaml:"destination"`
	// Recipients are the recipients the reply adds beyond the
	// destination, as asked: absent for a reply to the destination alone.
	Recipients   *model.ReplyRecipients `json:"recipients,omitempty" yaml:"recipients,omitempty"`
	Operation    operationView          `json:"operation"            yaml:"operation"`
	OperationKey string                 `json:"operation_key"        yaml:"operation_key"`
}

func (a *app) messageCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "message",
		Short: "Read messages across inboxes, reply in a conversation, start one",
		Long: `Read the messages of every inbox you may read, reply publicly in a
conversation, and send the first message of a new exchange. Each message
shows its visibility (public, or internal for a note) and its origin:
inbound from the provider, or a uma operation and the actor who
submitted it.

Internal notes are for desk actors alone: for any other role they are
not there, listed or by ID. ` + "`heo note`" + ` handles notes.

Every command runs in the profile's daemon (none running exits 70).
Reading is open to every role that reads conversations; replying and
sending are the desk role's. A reply or a send is queued: the command
returns once uma accepted it durably, with its operation, and the
daemon's workers deliver it.

` + "`message edit`" + ` and ` + "`message delete`" + ` change a message on its platform; they work
only on channels whose adapter declares edit or delete (` + "`channel get`" + `
shows them).`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.messageListCmd(), a.messageGetCmd(), a.messageReplyCmd(), a.messageSendCmd())
	grp.AddCommand(a.messageEditCmd(), a.messageDeleteCmd(), a.messageDraftCmd(), a.messageReplyDraftCmd(), a.messageRawCmd())
	return grp
}

func (a *app) messageListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List or search messages across the inboxes you may read",
		Long: `List the messages you may read, across every inbox you may read, in
the order uma recorded them, a page at a time (--limit, --cursor with
the previous page's next), each with its conversation, visibility and
origin. --inbox (key or ID) and --conversation (record ID) narrow it.
Internal notes appear for desk actors only. Read-only.

Search by narrowing the list. --text keeps messages whose text holds
every word given, whole words in any order, ignoring case; --since and
--until bound the message's own time as [since, until), each an RFC 3339
time or a date (midnight UTC). Label filters take label names,
repeated or comma-separated: --label keeps messages of conversations
holding any of them, --label-all all of them, --label-none none of
them, as ` + "`conversation list`" + ` does. Filters combine by AND, and uma applies
them: it searches only what you may read, so an internal note never
matches for a caller who may not read notes. There is no ranking and no
index of heo's own. A search page can be short or empty and still
incomplete: follow the next cursor until the listing says it is
complete. Filtering by label needs the role to read labels: desk and
viewer.`,
		Example: `  heo message list --conversation conv_123
  heo message list --inbox web --format json
  heo message list --text "invoice refund" --since 2026-10-01
  heo message list --text refund --label billing --label-none spam`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.messageListRun(cmd, args, false)
		},
	}
	addMessageFilterFlags(c)
	addMessageSearchFlags(c)
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[model.Message]{}, "1.0")
	return c
}

func addMessageFilterFlags(c *cobra.Command) {
	c.Flags().String(flagInbox, "", "Only messages of this inbox (key or ID)")
	c.Flags().String(flagConversation, "", "Only messages of this conversation (record ID)")
}

// addMessageSearchFlags adds the search flags of message list: text, time
// bounds and label filters, all applied by uma.
func addMessageSearchFlags(c *cobra.Command) {
	c.Flags().String(flagText, "", "Only messages whose text holds all of these words")
	c.Flags().String(flagSince, "", "Only messages at or after this time (RFC 3339 or date)")
	c.Flags().String(flagUntil, "", "Only messages before this time (RFC 3339 or date)")
	addLabelFilterFlags(c, "messages of conversations")
}

// messageListRun lists messages, or internal notes alone.
func (a *app) messageListRun(cmd *cobra.Command, args []string, notes bool) error {
	pg, err := pageOf(cmd)
	if err != nil {
		return err
	}
	f, err := searchFilter(cmd, deskFilter(cmd))
	if err != nil {
		return err
	}
	aud, action := model.Audience{}, model.ActionMessageRead
	if notes {
		aud = internalAudience
	}
	if labelFiltered(f) {
		// As conversation list: filtering by label is label.read; uma
		// still authorizes each message and its conversation.
		action = model.ActionLabelRead
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Message], error) {
		var out page[model.Message]
		err := a.desk(ctx, c, action, aud, func(s desk.Service) error {
			list := s.ListMessages
			if notes {
				list = s.ListNotes
			}
			p, err := list(ctx, f, pg)
			out = pageFrom(p)
			return a.labelFailure(err)
		})
		return out, err
	})
	if err != nil {
		return err
	}
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, res)
	}
	rows := make([]messageRow, 0, len(res.Items))
	for _, m := range res.Items {
		rows = append(rows, messageRowOf(m))
	}
	return renderPage(a, cmd, rows, res.Next, res.Complete)
}

func (a *app) messageGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <message>",
		Short: "Show one message",
		Long: `Show the message <message>, by record ID: its conversation, inbox,
visibility, origin, timestamps and text. --conversation (record ID)
narrows the lookup. An internal note shows for desk actors only. No
such message, or one you may not read, exits 3. Read-only.`,
		Example: `  heo message get msg_123
  heo message get msg_123 --conversation conv_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.messageGetRun(cmd, args, false)
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &model.Message{}, "1.0")
	return c
}

// messageGetRun shows one message, or one internal note.
func (a *app) messageGetRun(cmd *cobra.Command, args []string, note bool) error {
	conv, _ := cmd.Flags().GetString(flagConversation)
	aud := model.Audience{}
	if note {
		aud = internalAudience
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (model.Message, error) {
		var out model.Message
		err := a.desk(ctx, c, model.ActionMessageRead, aud, func(s desk.Service) (err error) {
			out, err = s.FindMessage(ctx, args[0], conv, note)
			return err
		})
		return out, err
	})
	if err != nil {
		return err
	}
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, res)
	}
	return writeRows(cmd.OutOrStdout(), messageRows(res))
}

// replyPlan renders a reply plan for --dry-run: the exact destination
// and visibility.
func replyPlan(p desk.ReplyPlan, key string) *cli.Plan {
	conv, in, dest := p.Conversation, p.Inbox, p.Destination()
	inbox := channels.Name(in.Key, in.Ref.ID)
	args := map[string]any{
		"conversation": conv.Ref.ID, "inbox": inbox, "channel": in.Channel.ID,
		"destination": dest, "visibility": model.Public, "content": p.Request.Content,
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := []string{"authorization", "conversation", "inbox", "request"}
	to := routeName(dest)
	if p.Recipients != nil {
		recipientArgs(args, p.Recipients, p.Declaration, p.Limits)
		to += " and " + desk.RecipientsLine(*p.Recipients)
		checked = append(checked, "recipients")
		if p.Declaration != nil {
			checked = append(checked, "adapter capability (declared)")
		}
		if p.Limits != nil {
			checked = append(checked, "recipient limits (live)")
		}
	}
	effect := planEffect("send", "conversation:"+string(conv.Ref.ID), false, fmt.Sprintf(
		"public reply to %s via inbox %s; queued for the daemon's workers", to, inbox))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: checked}
}

func replyRows(o replyOut) [][2]string {
	rows := [][2]string{
		{"message", string(o.Message)},
		{"conversation", string(o.Conversation)},
		{"inbox", string(o.Inbox)},
		{"visibility", string(o.Visibility)},
		{"destination", routeName(o.Destination)},
	}
	if o.Recipients != nil {
		rows = append(rows, [2]string{"recipients", desk.RecipientsLine(*o.Recipients)})
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) messageReplyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reply <conversation>",
		Short: "Reply publicly in a conversation, through its channel",
		Long: `Reply publicly in the conversation <conversation> (record ID) with
--text. The reply leaves through the conversation's inbox, on the reply
route uma stored for it, and answers the latest inbound message you may
read. A reply is always public: ` + "`note add`" + ` adds an internal note.

The reply goes to that route's address alone, unless it names more:
--reply-all adds the other recipients (To and Cc) of the message it
answers, the mailbox itself dropped; --cc and --bcc add explicit bare
addresses (at most 50 in all), a bcc address seen by no recipient. They
need reply_recipients on the conversation's channel (smtp.mail,
imap.mail, google.mail, microsoft.mail declare it; ` + "`heo channel get`" + `
shows the channel's limits): a channel without it refuses, exit 2
(UNSUPPORTED), before anything is stored. A plugin actor's reply that
adds recipients is held for a decision, as one that adds a cc always is.

The command returns once uma accepted the reply durably: its state is
queued, with the operation that delivers it, never delivered. The
daemon's workers deliver it, checking your permission again first.

--dry-run prints the exact destination (inbox, channel, reply route),
the recipients it adds and visibility, checks them against the limits
the channel reports now, and writes nothing; running it for real checks
everything again. Committed as a uma operation under --idempotency-key:
rerunning with the same key and text returns the same operation
(replayed) and sends nothing twice; without one, heo mints a key and
prints it. Desk role.`,
		Example: `  heo message reply conv_123 --text "Thanks, we're on it." --dry-run
  heo message reply conv_123 --text "Thanks, we're on it." --idempotency-key reply-42
  heo message reply conv_123 --text "Looping you in." --reply-all --cc lead@example.com --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			text, _ := cmd.Flags().GetString(flagText)
			rec := recipientsOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[replyOut], error) {
				var w written[replyOut]
				err := a.desk(ctx, c, model.ActionMessageReply, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanReplyTo(ctx, args[0], text, rec, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = replyPlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionMessageReply, Verb: "reply", Class: "send",
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Facts: p.Facts, Preview: replyPlan(p, key)})
					if err != nil {
						return err
					}
					acc, err := s.Reply(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = replyOut{
						Conversation: p.Conversation.Ref.ID, Inbox: p.Inbox.Ref.ID, Visibility: model.Public,
						Destination: p.Destination(), Recipients: p.Recipients, Operation: operationOf(acc), OperationKey: key,
					}
					if m := acc.Operation.ResultMessage; m != nil {
						w.Out.Message = m.ID
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			notes := queuedNote(res.Out.Operation)
			if res.Out.Recipients != nil && res.Out.Recipients.ReplyAll {
				notes = append(notes, recipientNote)
			}
			return renderDeskWrite(a, cmd, res, replyRows, notes)
		},
	}
	addTextFlag(c, "Text of the reply")
	addRecipientFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &replyOut{}, "1.0")
	return c
}
