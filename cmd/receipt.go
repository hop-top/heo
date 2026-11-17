package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
)

// receiptOut is what sending a receipt reports: the message, the kind,
// and the operation that sends it — queued, never sent, when the
// command returns. It says nothing of what the provider showed.
type receiptOut struct {
	Message      model.ID      `json:"message"       yaml:"message"`
	Conversation model.ID      `json:"conversation"  yaml:"conversation"`
	Kind         string        `json:"kind"          yaml:"kind"`
	Operation    operationView `json:"operation"     yaml:"operation"`
	OperationKey string        `json:"operation_key" yaml:"operation_key"`
}

func receiptRows(o receiptOut) [][2]string {
	return append([][2]string{
		{"message", string(o.Message)},
		{"conversation", string(o.Conversation)},
		{"receipt", o.Kind},
	}, operationRows(o.Operation, o.OperationKey)...)
}

// receiptNote is what a queued receipt says: sent, at most, when the
// provider accepts it — never that anyone saw it.
func receiptNote(op operationView) []string {
	notes := queuedNote(op)
	if len(notes) == 0 {
		return nil
	}
	return append(notes, fmt.Sprintf(
		"a receipt tells the provider the message was delivered or read; whether the provider shows it to the sender is the provider's, and heo cannot tell (`heo delivery get %s` shows whether it was sent)", op.ID))
}

// receiptPlan renders the plan of a receipt: the platform effect, what
// the preview checked of the adapter, and that sending is not showing.
// A receipt takes no revision, so none is checked.
func receiptPlan(m model.Message, d *desk.Declaration, kind, key string) *cli.Plan {
	args := map[string]any{
		"message": m.Ref.ID, "conversation": m.Ref.Conversation.ID, "inbox": m.Ref.Conversation.Inbox.ID,
		"kind": kind, "audience": m.Audience.Visibility,
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := declarationArgs(args, d)
	effect := planEffect("send", "message:"+string(m.Ref.ID), false, fmt.Sprintf(
		"send the provider a %s receipt for message %s; queued for the daemon's workers, sent only when the provider accepts it, and what the sender is shown is the provider's",
		kind, m.Ref.ID))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: append(checked, "message", "request")}
}

func (a *app) receiptCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "receipt",
		Short: "Send a delivery or read receipt for a received message",
		Long: `Tell the provider that a message it delivered to you was delivered or
read, so its sender may see it. It reaches the provider, so it works
only on channels whose adapter declares receipt; it is the desk
role's, and runs in the profile's daemon (none running exits 70).

A receipt is not the local read state: it marks nothing read for you in
heo, and heo keeps no read state of its own. It is a message sent to
the provider, and the provider decides whether to show it.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.receiptSendCmd())
	return grp
}

func (a *app) receiptSendCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "send",
		Short: "Send a delivery or read receipt for a message, to its provider",
		Long: `Send the provider a receipt of --kind read or delivered for the message
--message (record ID) that arrived from it. --conversation (record ID)
narrows the lookup. Only a message that arrived from the provider takes
one: a message sent through heo, or the provider's echo of one, is a
usage error (exit 2); a deleted or redacted one is a conflict (exit 4).
A receipt names no revision.

The receipt is queued: the command returns once uma accepted it
durably, with its operation, never sent. The daemon's workers send it,
and the operation is the only record that it was: no message changes in
heo. Sending only means the provider accepted it; whether the sender is
shown a receipt at all is the provider's, and heo cannot tell. Only a
channel whose adapter declares receipt can do this; any other is
refused, exit 2 (UNSUPPORTED), before anything is written, and a
provider that has no receipt of this kind rejects it, reported on the
operation.

This is not the local read state; ` + "`heo conversation mark-read`" + ` does not
exist yet.

--dry-run prints the platform effect, says which adapter declares
receipt and that live availability is not checked, and writes nothing.
Committed under --idempotency-key: rerunning with the same key and kind
returns the same operation (replayed), and another kind under that key
is a conflict; without a key, heo mints one and prints it. Desk role.`,
		Example: `  heo receipt send --message msg_123 --kind read --dry-run
  heo receipt send --message msg_123 --kind delivered --idempotency-key receipt-42`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			id, _ := cmd.Flags().GetString(flagMessage)
			conv, _ := cmd.Flags().GetString(flagConversation)
			kind, _ := cmd.Flags().GetString(flagKind)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[receiptOut], error) {
				var w written[receiptOut]
				err := a.desk(ctx, c, model.ActionMessageReceipt, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanReceipt(ctx, id, conv, kind, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					m := p.Message
					if cli.IsDryRun(cmd) {
						w.Plan = receiptPlan(m, p.Declaration, kind, key)
						return nil
					}
					acc, err := s.SendReceipt(ctx, p)
					if err != nil {
						return err
					}
					w.Out = receiptOut{Message: m.Ref.ID, Conversation: m.Ref.Conversation.ID, Kind: kind, Operation: operationOf(acc), OperationKey: key}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, receiptRows, receiptNote(res.Out.Operation))
		},
	}
	c.Flags().String(flagMessage, "", "Message to send a receipt for (record ID) (required)")
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	c.Flags().String(flagKind, "", "What to tell the provider: read or delivered (required)")
	_ = c.MarkFlagRequired(flagMessage)
	_ = c.MarkFlagRequired(flagKind)
	providerDependent(c, model.CapabilityReceipt)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &receiptOut{}, "1.0")
	return c
}
