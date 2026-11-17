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

// editOut is what an edit reports: the message as it is now — the old
// text, until the provider applies the edit — and the operation that
// sends the edit, queued, never applied, when the command returns.
type editOut struct {
	Message      model.ID         `json:"message"       yaml:"message"`
	Conversation model.ID         `json:"conversation"  yaml:"conversation"`
	Inbox        model.ID         `json:"inbox"         yaml:"inbox"`
	Visibility   model.Visibility `json:"visibility"    yaml:"visibility"`
	Revision     model.Revision   `json:"revision"      yaml:"revision"`
	Operation    operationView    `json:"operation"     yaml:"operation"`
	OperationKey string           `json:"operation_key" yaml:"operation_key"`
}

func editRows(o editOut) [][2]string {
	return append([][2]string{
		{"message", string(o.Message)},
		{"conversation", string(o.Conversation)},
		{"inbox", string(o.Inbox)},
		{"visibility", string(o.Visibility)},
		{"revision", string(o.Revision) + " (as read; the provider's change bumps it)"},
	}, operationRows(o.Operation, o.OperationKey)...)
}

// messageEffectNote is what a queued edit or delete says of delivery:
// the message changes when the provider applied it, not before.
func messageEffectNote(what string, op operationView) []string {
	notes := queuedNote(op)
	if len(notes) == 0 {
		return nil
	}
	return append(notes, fmt.Sprintf("the message is %s only once the provider applies it: `heo delivery get %s` shows the operation", what, op.ID))
}

// messageEffectPlan renders the plan of an edit or delete: the platform
// effect, the audience, what the preview checked of the adapter, and
// that acceptance is not application.
func messageEffectPlan(kind string, reversible bool, m model.Message, d *desk.Declaration, extra map[string]any, key, effect string) *cli.Plan {
	args := map[string]any{
		"message": m.Ref.ID, "conversation": m.Ref.Conversation.ID, "inbox": m.Ref.Conversation.Inbox.ID,
		"visibility": m.Audience.Visibility, "audience": m.Audience.Visibility,
	}
	for k, v := range extra {
		args[k] = v
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := declarationArgs(args, d)
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect(kind, "message:"+string(m.Ref.ID), reversible, effect)},
		PrerequisitesChecked: append(checked, "message", "revision", "request")}
}

func (a *app) messageEditCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "edit <message>",
		Short: "Edit a message at its provider",
		Long: `Replace the text of the message <message> (record ID) with --text, on
the platform it lives on: the provider edits it, and its readers see
the new text. The audience never changes. --conversation (record ID)
narrows the lookup.

--revision names the revision being edited, as ` + "`message get`" + ` shows it,
and is always sent: if the message changed since, it is a conflict
(exit 4) naming the current revision, and nothing is written. So is a
retry of an edit after the message's audience narrowed: read the
message again and use a new --idempotency-key.

The edit is queued: the command returns once uma accepted it durably,
with its operation, never applied. The daemon's workers send it, and
the message shows the new text only when the provider applied it. An
edit the provider applied after a newer edit of the message keeps the
newer text. Only a channel whose adapter declares edit can do this;
any other is refused, exit 2 (UNSUPPORTED), before anything is written.

--dry-run prints the platform effect, the audience and the operation,
says which adapter declares edit and that live availability (the
provider account's rights, the channel's configuration) is not checked,
and writes nothing; running it for real checks everything again.
Committed as a uma operation under --idempotency-key: rerunning with
the same key, revision and text returns the same operation (replayed);
without one, heo mints a key and prints it. Desk role.`,
		Example: `  heo message edit msg_123 --revision rev_9 --text "Corrected: 10:30." --dry-run
  heo message edit msg_123 --revision rev_9 --text "Corrected: 10:30." --idempotency-key fix-42`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			conv, _ := cmd.Flags().GetString(flagConversation)
			text, _ := cmd.Flags().GetString(flagText)
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[editOut], error) {
				var w written[editOut]
				err := a.desk(ctx, c, model.ActionMessageEdit, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanEdit(ctx, args[0], conv, rev, text, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					m := p.Message
					plan := messageEffectPlan("update", false, m, p.Declaration,
						map[string]any{"text": text, "expected_revision": rev}, key, fmt.Sprintf(
							"replace the text of message %s at its provider; queued for the daemon's workers, applied only when the provider confirms",
							m.Ref.ID))
					if cli.IsDryRun(cmd) {
						w.Plan = plan
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, messageHold(model.ActionMessageEdit, "edit", m, plan))
					if err != nil {
						return err
					}
					acc, err := s.Edit(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = editOut{
						Message: m.Ref.ID, Conversation: m.Ref.Conversation.ID, Inbox: m.Ref.Conversation.Inbox.ID,
						Visibility: m.Audience.Visibility, Revision: m.Revision, Operation: operationOf(acc), OperationKey: key,
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, editRows, messageEffectNote("changed", res.Out.Operation))
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	addRevisionFlag(c, "message")
	addTextFlag(c, "New text of the message")
	providerDependent(c, model.CapabilityEdit)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &editOut{}, "1.0")
	return c
}

func (a *app) messageDeleteCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "delete <message>",
		Short: "Delete a message at its provider (a tombstone)",
		Long: `Delete the message <message> (record ID) on the platform it lives on:
the provider removes it, and uma keeps a tombstone (deleted, its text
cleared). This is no privacy erasure: redaction is a separate act,
the operator's. --conversation (record ID) narrows the lookup.

--revision names the revision being deleted, as ` + "`message get`" + ` shows it,
and is always sent: a message that changed since is a conflict (exit 4)
naming the current revision. A message already deleted is a conflict.

The delete is queued: the command returns once uma accepted it
durably, with its operation, never applied. The daemon's workers send
it, and the message is a tombstone only when the provider applied it.
Only a channel whose adapter declares delete can do this; any other is
refused, exit 2 (UNSUPPORTED), before anything is written.

This is destructive and reaches the provider, so it is withheld from
the REST, gRPC and MCP surfaces until the operator names them; the
control socket allows it, and the terminal asks for --confirm=yes.
--dry-run prints the platform effect and the audience, says which
adapter declares delete and that live availability is not checked, and
writes nothing. Committed under --idempotency-key: rerunning with the
same key and revision returns the same operation (replayed); without
one, heo mints a key and prints it. Desk role.`,
		Example: `  heo message delete msg_123 --revision rev_9 --dry-run
  heo message delete msg_123 --revision rev_9 --confirm=yes --idempotency-key retract-42`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			conv, _ := cmd.Flags().GetString(flagConversation)
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[editOut], error) {
				var w written[editOut]
				err := a.desk(ctx, c, model.ActionMessageDelete, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanDelete(ctx, args[0], conv, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					m := p.Message
					plan := messageEffectPlan("delete", false, m, p.Declaration,
						map[string]any{"expected_revision": rev}, key, fmt.Sprintf(
							"delete message %s at its provider, leaving a tombstone in uma (not a privacy redaction); queued for the daemon's workers, applied only when the provider confirms",
							m.Ref.ID))
					if cli.IsDryRun(cmd) {
						w.Plan = plan
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, messageHold(model.ActionMessageDelete, "delete", m, plan))
					if err != nil {
						return err
					}
					acc, err := s.Delete(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = editOut{
						Message: m.Ref.ID, Conversation: m.Ref.Conversation.ID, Inbox: m.Ref.Conversation.Inbox.ID,
						Visibility: m.Audience.Visibility, Revision: m.Revision, Operation: operationOf(acc), OperationKey: key,
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, editRows, messageEffectNote("a tombstone", res.Out.Operation))
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	addRevisionFlag(c, "message")
	providerDependent(c, model.CapabilityDelete)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectDestructiveShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &editOut{}, "1.0")
	return c
}

// messageHold is what a write on message m offers the hold point: the
// verb's own plan as the preview, the message's inbox as the channel.
func messageHold(action, verb string, m model.Message, plan *cli.Plan) desk.HoldInput {
	return desk.HoldInput{Action: action, Verb: verb, Class: "message",
		Channel: string(m.Ref.Conversation.Inbox.ID), Target: m.Ref.Ref(), Preview: plan}
}
