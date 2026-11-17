package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
)

// flagTo names a send's recipient.
const flagTo = "to"

// sendOut is what a send reports: the message uma recorded for it, the
// conversation the inbox's selection picked or created, where it goes,
// under which visibility, and the operation that delivers it — queued,
// never delivered, when the command returns.
type sendOut struct {
	Message      model.ID          `json:"message"       yaml:"message"`
	Conversation model.ID          `json:"conversation"  yaml:"conversation"`
	Inbox        model.ID          `json:"inbox"         yaml:"inbox"`
	Visibility   model.Visibility  `json:"visibility"    yaml:"visibility"`
	Destination  model.ExternalRef `json:"destination"   yaml:"destination"`
	Operation    operationView     `json:"operation"     yaml:"operation"`
	OperationKey string            `json:"operation_key" yaml:"operation_key"`
}

// sendSelection is what a send preview says of the conversation: the
// inbox's selection policy picks it when uma accepts the send.
const sendSelection = "selected or created by the inbox's selection policy"

// sendPlan renders a send plan for --dry-run: the exact destination and
// visibility, and that the conversation is selected or created.
func sendPlan(p desk.StartPlan, key string) *cli.Plan {
	in, dest := p.Inbox, p.Destination()
	inbox := channels.Name(in.Key, in.Ref.ID)
	args := map[string]any{
		"conversation": sendSelection, "inbox": inbox, "channel": in.Channel.ID, "adapter": p.Adapter,
		"destination": dest, "visibility": model.Public, "content": desk.Text(p.Request.Content),
	}
	// The destination is the route uma will send on, after the adapter's
	// normalization; what --to spelled shows only when it differs.
	if p.AsTyped != nil {
		args["as_typed"] = *p.AsTyped
	}
	if p.Identity != nil {
		args["identity"] = p.Identity.ID
	}
	if p.Contact != nil {
		args["contact"] = p.Contact.ID
	}
	if key != "" {
		args["operation_key"] = key
	}
	effect := planEffect("send", "inbox:"+string(in.Ref.ID), false, fmt.Sprintf(
		"public first message to %s via inbox %s, in the conversation its selection policy picks or creates; "+
			"queued for the daemon's workers", routeName(dest), inbox))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: []string{"authorization", "inbox", "adapter capability", "recipient", "request"}}
}

func sendRows(o sendOut) [][2]string {
	return append([][2]string{
		{"message", string(o.Message)},
		{"conversation", string(o.Conversation)},
		{"inbox", string(o.Inbox)},
		{"visibility", string(o.Visibility)},
		{"destination", routeName(o.Destination)},
	}, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) messageSendCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "send",
		Short: "Start an exchange: send a first message through an inbox",
		Long: `Send the first message of a new exchange through the inbox --inbox (key
or ID) to the recipient --to, with --text. ` + "`message reply`" + ` answers a
conversation that exists; send starts one, to someone who may never
have written in. The message is public.

--to names the recipient as the provider route the channel sends on:

  <kind>:<id>              a route on the inbox's channel, e.g.
                           address:ada@example.com or phone:+15550123,
                           in the adapter's identity namespace
  <namespace>/<kind>:<id>  the same, naming the namespace when the
                           adapter declares more than one
  identity:<id>            an identity of the inbox (record ID): the
                           route uma stored for its conversation
  contact:<id>             a contact (record ID) with one identity in
                           the inbox
  room:<id>                a provider room, in a per_room inbox
  thread:<id>              a provider thread, in a per_thread inbox

A per_room or per_thread inbox takes only its own kind of reference, not
a person: it files by the room or thread, whoever writes in it. A chat
adapter whose rooms are not in its identity namespace needs it named:
slack.chat/room:C0123ABCD (Slack), telegram.messages/room:-1001234567890
(Telegram), discord.chat/room:123456789012345680 (Discord),
microsoft.teams/room:19:abc123@thread.tacv2 (Teams). An adapter with several
namespaces needs the recipient's: relay.imessage/phone:+15550100003, or
relay.imessage/phone:alice@example.com for an Apple ID, in any letter case:
a start lower-cases it (relay.messages). A Slack or Discord thread, a Telegram forum topic or a Teams
channel thread lives in a channel or chat, which --to cannot name with it:
answer a thread with
` + "`message reply`" + `.

The inbox's selection policy picks the conversation when uma accepts
the send: built-in rules reuse the recipient's open or pending one and
never reopen one, else create one. Only a
channel whose adapter declares start_conversation can send first; any
other inbox is refused, exit 2 (UNSUPPORTED), before anything is
written. ` + "`adapter list`" + ` shows each adapter's capabilities.

The command returns once uma accepted the message durably: its state is
queued, with the operation that delivers it and the conversation it
went to, never delivered. The daemon's workers deliver it, checking
your permission again first.

--dry-run prints the exact destination (inbox, channel, route) and
visibility, and that the conversation will be selected or created, and
writes nothing; running it for real checks everything again. The route is
the one the send uses, after the adapter spells it as its provider does (an
iMessage address is lower-cased): what --to said shows as "as typed" only
when it differs, and a recipient the adapter refuses fails the preview as
it fails the send. Committed
as a uma operation under --idempotency-key: rerunning with the same key,
recipient and text returns the same operation (replayed) and sends
nothing twice; without one, heo mints a key and prints it. Desk role.`,
		Example: `  heo message send --inbox support --to address:ada@example.com --text "Your order shipped." --dry-run
  heo message send --inbox sms --to phone:+15550123 --text "See you at 10." --idempotency-key remind-42
  heo message send --inbox support --to identity:idn_123 --text "Following up."
  heo message send --inbox team --to room:general --text "Release is out."`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			inbox, _ := cmd.Flags().GetString(flagInbox)
			to, _ := cmd.Flags().GetString(flagTo)
			text, _ := cmd.Flags().GetString(flagText)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[sendOut], error) {
				var w written[sendOut]
				err := a.desk(ctx, c, model.ActionConversationStart, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanStart(ctx, inbox, to, text, opID(key))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = sendPlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionConversationStart, Verb: "start",
						Channel: string(p.Inbox.Ref.ID), Target: p.Inbox.Ref.Ref(), Preview: sendPlan(p, key)})
					if err != nil {
						return err
					}
					acc, err := s.Start(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = sendOut{
						Inbox: p.Inbox.Ref.ID, Visibility: model.Public, Destination: p.Destination(),
						Operation: operationOf(acc), OperationKey: key,
					}
					if m := acc.Operation.ResultMessage; m != nil {
						w.Out.Message, w.Out.Conversation = m.ID, m.Conversation.ID
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, sendRows, queuedNote(res.Out.Operation))
		},
	}
	c.Flags().String(flagInbox, "", "Inbox to send through (key or ID) (required)")
	c.Flags().String(flagTo, "", "Recipient: <kind>:<id>, <namespace>/<kind>:<id>, identity:<id> or contact:<id> (required)")
	_ = c.MarkFlagRequired(flagInbox)
	_ = c.MarkFlagRequired(flagTo)
	addTextFlag(c, "Text of the message")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &sendOut{}, Version: "1.0"})
	return c
}
