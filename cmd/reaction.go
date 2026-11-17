package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// reactionListOut is the reactions on a message as uma stores them.
// Complete says whether the set came from an authoritative provider
// snapshot: when false, the provider may hold reactions uma never heard
// of (it learns of them from events, and some providers send none).
type reactionListOut struct {
	Message   model.ID                `json:"message"   yaml:"message"`
	Complete  bool                    `json:"complete"  yaml:"complete"`
	Reactions []model.MessageReaction `json:"reactions" yaml:"reactions"`
}

// reactionRow is a reaction in a listing table: who reacted (an
// identity of the provider or a desk actor), or none for a count the
// provider gave without names.
type reactionRow struct {
	Emoji string `json:"emoji" yaml:"emoji" table:"EMOJI"`
	Count int    `json:"count" yaml:"count" table:"COUNT"`
	By    string `json:"by"    yaml:"by"    table:"BY"`
	At    string `json:"at"    yaml:"at"    table:"AT"`
}

func reactionRowOf(r model.MessageReaction) reactionRow {
	by := "-"
	switch {
	case r.By != nil:
		by = "identity " + string(r.By.ID)
	case r.Actor != "":
		by = string(r.Actor)
	}
	return reactionRow{Emoji: r.Emoji, Count: r.Count, By: by, At: stamp(r.At)}
}

// reactOut is what adding or removing a reaction reports: the message,
// the reaction, and the operation that sends it — queued, never applied,
// when the command returns.
type reactOut struct {
	Message      model.ID      `json:"message"       yaml:"message"`
	Emoji        string        `json:"emoji"         yaml:"emoji"`
	Remove       bool          `json:"remove"        yaml:"remove"`
	Operation    operationView `json:"operation"     yaml:"operation"`
	OperationKey string        `json:"operation_key" yaml:"operation_key"`
}

func reactRows(o reactOut) [][2]string {
	act := "add"
	if o.Remove {
		act = "remove"
	}
	return append([][2]string{
		{"message", string(o.Message)},
		{"emoji", o.Emoji},
		{"reaction", act},
	}, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) reactionCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "reaction",
		Short: "List, add and remove the reactions on a message",
		Long: `List the reactions on a message, and add or remove your reaction to
it on the platform it lives on.

Every command runs in the profile's daemon (none running exits 70).
Listing is a read, open to every role that reads the message. Adding
and removing change the message at its provider, so they work only on
channels whose adapter declares react; they are the desk role's. A
reaction is queued: the command returns once uma accepted it durably,
and it shows in the list when the provider applied it.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.reactionListCmd(), a.reactionWriteCmd(false), a.reactionWriteCmd(true))
	return grp
}

func (a *app) reactionListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the reactions on a message",
		Long: `List the reactions on the message --message (record ID) that you may
read, as uma stores them: each with its emoji, who reacted when the
provider says, and when. --conversation (record ID) narrows the lookup.
Read-only, and open to every role that reads the message; it needs no
capability.

complete says how far to trust the list. true: uma holds the provider's
authoritative set. false: it was built from reaction events, or folded
past its size limit, so the provider may hold reactions uma has not
heard of; some providers (GitHub) send no reaction events at all, and a
later read of the message from the provider refreshes the set. A
count-only row is the provider's total for an emoji, with no names.`,
		Example: `  heo reaction list --message msg_123
  heo reaction list --message msg_123 --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, _ := cmd.Flags().GetString(flagMessage)
			conv, _ := cmd.Flags().GetString(flagConversation)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (reactionListOut, error) {
				var out reactionListOut
				err := a.desk(ctx, c, model.ActionMessageRead, model.Audience{}, func(s desk.Service) error {
					l, err := s.Reactions(ctx, id, conv)
					out = reactionListOut{Message: l.Message.Ref.ID, Complete: l.Complete, Reactions: l.Reactions}
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
			rows := make([]reactionRow, 0, len(res.Reactions))
			for _, r := range res.Reactions {
				rows = append(rows, reactionRowOf(r))
			}
			if err := output.Dispatch(cmd, a.r.Viper, rows); err != nil {
				return err
			}
			if res.Complete {
				return nil
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "complete: false; built from reaction events, the provider may hold others")
			return err
		},
	}
	c.Flags().String(flagMessage, "", "Message to list the reactions of (record ID) (required)")
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	_ = c.MarkFlagRequired(flagMessage)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &reactionListOut{}, Version: "1.0"})
	return c
}

// reactionWriteCmd builds `reaction add` and `reaction remove`.
func (a *app) reactionWriteCmd(remove bool) *cobra.Command {
	verb, what, example := "add", "Add your reaction to a message", "heo reaction add --message msg_123 👍"
	long := `Add your reaction <emoji> to the message --message (record ID) on the
platform it lives on. One token of 1 to 64 bytes, compared byte for
byte, never normalized: a variation selector or a skin tone is another
emoji. Which emoji a provider takes, and how many reactions one user may
leave on a message, is the provider's: GitHub accepts eight, and
WhatsApp keeps one per user, a new emoji replacing the old. An emoji
the provider refuses is the provider's answer, reported on the
operation.`
	if remove {
		verb, what, example = "remove", "Remove your reaction from a message", "heo reaction remove --message msg_123 👍"
		long = `Remove your reaction <emoji> from the message --message (record ID) on
the platform it lives on. Only your own reaction (the provider account's
reaction) can be removed.`
	}
	c := &cobra.Command{
		Use:   verb + " <emoji>",
		Short: what + ", at its provider",
		Long: long + `

--conversation (record ID) narrows the lookup. --revision, optional,
names the revision of the message you read; when set and the message
moved on, it is a conflict (exit 4) naming the current revision.

The reaction is queued: the command returns once uma accepted it
durably, with its operation, never applied. The daemon's workers send
it. Only a channel whose adapter declares react can do this; any other
is refused, exit 2 (UNSUPPORTED), before anything is written, and a
declared one the channel's configuration switched off (GitHub without
replies) fails the operation as unsupported or disabled.

--dry-run prints the platform effect and the audience, says which
adapter declares react and that live availability is not checked, and
writes nothing. Committed under --idempotency-key: rerunning with the
same key returns the same operation (replayed); without one, heo
mints a key and prints it. Desk role.`,
		Example: "  " + example + " --dry-run\n  " + example + " --idempotency-key react-42",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			id, _ := cmd.Flags().GetString(flagMessage)
			conv, _ := cmd.Flags().GetString(flagConversation)
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[reactOut], error) {
				var w written[reactOut]
				err := a.desk(ctx, c, model.ActionMessageReact, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanReact(ctx, id, conv, args[0], remove, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					m := p.Message
					extra := map[string]any{"emoji": args[0]}
					if rev != "" {
						extra["expected_revision"] = rev
					}
					plan := messageEffectPlan("update", true, m, p.Declaration, extra, key, fmt.Sprintf(
						"%s your reaction %s on message %s at its provider; queued for the daemon's workers, applied only when the provider confirms",
						verb, args[0], m.Ref.ID))
					if cli.IsDryRun(cmd) {
						w.Plan = plan
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, messageHold(model.ActionMessageReact, "react", m, plan))
					if err != nil {
						return err
					}
					acc, err := s.React(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = reactOut{Message: m.Ref.ID, Emoji: args[0], Remove: remove, Operation: operationOf(acc), OperationKey: key}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, reactRows, messageEffectNote("changed", res.Out.Operation))
		},
	}
	c.Flags().String(flagMessage, "", "Message to react to (record ID) (required)")
	c.Flags().String(flagConversation, "", "Conversation the message belongs to (record ID); narrows the lookup")
	c.Flags().String(flagRevision, "", "Revision of the message you read, from `message get`; compared when set")
	_ = c.MarkFlagRequired(flagMessage)
	providerDependent(c, model.CapabilityReact)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &reactOut{}, Version: "1.0"})
	return c
}
