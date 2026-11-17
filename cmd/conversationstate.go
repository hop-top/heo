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

// flagRole is `conversation move`'s special-use destination.
const flagRole = "role"

// conversationStateOut is what a provider state verb reports: the
// conversation as uma holds it (its provider state moves only once the
// provider applied the change), what was asked, the scope the channel
// reports, the operation and its key.
type conversationStateOut struct {
	Conversation model.Conversation `json:"conversation"     yaml:"conversation"`
	Verb         string             `json:"verb"             yaml:"verb"`
	Change       string             `json:"change"           yaml:"change"`
	Scope        string             `json:"scope,omitempty"  yaml:"scope,omitempty"`
	Operation    operationView      `json:"operation"        yaml:"operation"`
	OperationKey string             `json:"operation_key"    yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty"  yaml:"notes,omitempty"`
}

// stateDescription is the help each provider state verb carries.
type stateDescription struct {
	short string
	// what is the verb's effect at the provider, in the Long text.
	what string
	// example lines.
	example string
}

var stateDescriptions = map[model.StateVerb]stateDescription{
	model.StateVerbArchive: {
		short: "Archive a conversation at its provider",
		what: `Archive the conversation <conversation> (record ID) at its provider: a
mailbox takes it out of the inbox (Gmail drops the INBOX label, Graph and
IMAP file it in the Archive folder), a chat workspace archives the room or
thread.`,
		example: "  heo conversation archive conv_123 --dry-run\n  heo conversation archive conv_123 --idempotency-key arch-123",
	},
	model.StateVerbUnarchive: {
		short:   "Unarchive a conversation at its provider",
		what:    "Undo `conversation archive` at the provider: the conversation returns to where an archived one comes back to.",
		example: "  heo conversation unarchive conv_123",
	},
	model.StateVerbMove: {
		short: "Move a conversation at its provider (folder, role or labels)",
		what: `Move the conversation <conversation> (record ID) at its provider, by what
the channel's provider holds: to a folder or label named by its provider ID or name
(--to), to a special-use place the adapter resolves (--role inbox, archive,
trash or junk), or, on a provider that keeps labels, by adding and removing
provider labels (--add, --remove; repeat or comma-separate). Name one way:
a destination or label changes, never both. A provider that files a
conversation in one folder (Graph, IMAP) takes a destination only. These are
the provider's labels and folders, not heo's labels (` + "`conversation label`" + `, local to
uma) and not the message tags of ` + "`conversation tag`" + `.

--to, --add and --remove take a provider mailbox by ID or by name
(` + "`provider-label list`" + ` shows both). A name is resolved through the
channel's mailbox list to its ID, which is what the change sends; an ID that the list
holds is used as it is, and an ID wins over a name spelled like it. The
name is matched exactly, then without regard to case; two mailboxes
that share it are refused (exit 2) with their IDs. A value the list does not hold is sent as
given, and whether it exists is the provider's answer, shown on the
operation; so is any value on a channel that cannot list its mailboxes. --dry-run
prints what each name resolved to. A move to the junk role is how a
mailbox marks spam.`,
		example: "  heo conversation move conv_123 --role junk --dry-run\n  heo conversation move conv_123 --to Label_12\n  heo conversation move conv_123 --to Clients/Acme\n  heo conversation move conv_123 --add Label_12 --remove INBOX",
	},
	model.StateVerbRead: {
		short: "Mark a conversation read at its provider",
		what: `Mark the conversation <conversation> (record ID) read at its provider: the
mailbox shows it read in every client. It is not ` + "`conversation mark-read`" + `, your own
read marker in uma, which sends nothing to the provider and which this never
changes.`,
		example: "  heo conversation read conv_123 --dry-run\n  heo conversation read conv_123",
	},
	model.StateVerbUnread: {
		short: "Mark a conversation unread at its provider",
		what: `Mark the conversation <conversation> (record ID) unread at its provider. It
is not ` + "`conversation mark-unread`" + `, your own read marker in uma, which this never
changes.`,
		example: "  heo conversation unread conv_123",
	},
	model.StateVerbFlag: {
		short: "Star a conversation or mark it important at its provider",
		what: `Set a flag on the conversation <conversation> (record ID) at its provider:
--kind star (the default; Gmail's star, Graph's and IMAP's flag) or important
(where the provider has one). A kind the channel's provider does not have is
refused (UNSUPPORTED, exit 2).`,
		example: "  heo conversation flag conv_123 --dry-run\n  heo conversation flag conv_123 --kind important",
	},
	model.StateVerbUnflag: {
		short:   "Clear a flag of a conversation at its provider",
		what:    "Clear the flag --kind star (the default) or important on the conversation <conversation> (record ID) at its provider.",
		example: "  heo conversation unflag conv_123\n  heo conversation unflag conv_123 --kind important",
	},
	model.StateVerbTrash: {
		short: "Move a conversation to its provider's trash",
		what: `Move the account's copy of the conversation <conversation> (record ID) to its
provider's trash. It is not ` + "`message delete`" + `: nothing is deleted at the peer, and
the provider empties its trash on its own schedule. ` + "`conversation untrash`" + `
restores it.`,
		example: "  heo conversation trash conv_123 --dry-run\n  heo conversation trash conv_123",
	},
	model.StateVerbUntrash: {
		short:   "Restore a conversation from its provider's trash",
		what:    "Undo `conversation trash` at the provider: the conversation is restored from its trash.",
		example: "  heo conversation untrash conv_123",
	},
}

// stateCapability is the capability that carries a state verb.
func stateCapability(v model.StateVerb) model.Capability {
	c, _ := v.Capability()
	return c
}

// conversationStatePlan renders a state change plan for --dry-run: the
// external change at the provider, and nothing in uma until the
// provider applied it.
func conversationStatePlan(p desk.StatePlan, key string) *cli.Plan {
	cur := p.Conversation
	args := map[string]any{
		"conversation": cur.Ref.ID, "inbox": cur.Ref.Inbox.ID,
		"change":   p.Change.Asked(),
		"current":  cur,
		"delivery": "queued, not applied: uma's provider state for the conversation changes only once the provider applied it (shown as the operation's pending state until then); its status, labels and your read marker are never touched",
	}
	if p.Command.ExpectedRevision != "" {
		args["expected_revision"] = p.Command.ExpectedRevision
	}
	if key != "" {
		args["operation_key"] = key
	}
	if len(p.Resolved) > 0 {
		args["resolved"] = resolvedLines(p.Resolved)
	}
	checked := declarationArgs(args, p.Declaration)
	detail := fmt.Sprintf("external: the provider's own state of conversation %s (%s); the provider shows it in its own clients, uma records it once applied",
		cur.Ref.ID, p.Change.Asked())
	if l := p.Limits; l != nil {
		args["scope"] = string(l.Scope)
		args["limits"] = l
		checked = append(checked, "channel's live limits (scope, kinds, shape, roles)")
		if l.Scope == model.ScopeShared {
			detail += "; the scope is shared: everyone who uses the provider sees it"
		}
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "provider_state:"+string(cur.Ref.ID), true, detail)},
		PrerequisitesChecked: append(checked, "conversation", "request")}
}

func conversationStateRows(o conversationStateOut) [][2]string {
	rows := [][2]string{
		{"conversation", string(o.Conversation.Ref.ID)},
		{"change", o.Change},
	}
	if o.Scope != "" {
		rows = append(rows, [2]string{"scope", o.Scope})
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

// resolvedLines says what a verb resolved provider mailbox names to, for
// a plan: each name given and the ID it files by, as "name = ID".
func resolvedLines(rs []desk.Resolved) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Given + " = " + r.ID
	}
	return strings.Join(out, ", ")
}

// stateChangeOf reads the change the leaf was given.
func stateChangeOf(cmd *cobra.Command, v model.StateVerb) (desk.StateChange, error) {
	ch := desk.StateChange{Verb: v}
	switch v {
	case model.StateVerbFlag, model.StateVerbUnflag:
		ch.Kind, _ = cmd.Flags().GetString(flagKind)
	case model.StateVerbMove:
		ch.Destination.ID, _ = cmd.Flags().GetString(flagTo)
		ch.Destination.Role, _ = cmd.Flags().GetString(flagRole)
		ch.Add, _ = cmd.Flags().GetStringSlice(flagAdd)
		ch.Remove, _ = cmd.Flags().GetStringSlice(flagRemove)
	}
	if err := desk.CheckStateChange(ch); err != nil {
		return ch, output.UsageError(strings.TrimPrefix(err.Error(), "validation: "))
	}
	return ch, nil
}

// conversationStateCmd builds one provider state verb: `conversation
// archive`, `unarchive`, `move`, `read`, `unread`, `flag`, `unflag`,
// `trash` or `untrash`.
func (a *app) conversationStateCmd(v model.StateVerb) *cobra.Command {
	d := stateDescriptions[v]
	c := &cobra.Command{
		Use:   string(v) + " <conversation>",
		Short: d.short,
		Long: d.what + `

This changes what the provider holds, not uma's own state: the
conversation's status, its labels and your read marker are never touched
(` + "`conversation resolve`, `conversation label` and `conversation mark-read`" + ` do those),
and uma's provider state for it (` + "`conversation get`" + `) follows only once the
provider applied the change. The change is queued as a uma operation: the
command returns once uma accepted it durably, never applied. The daemon's
workers send it, and ` + "`delivery get <operation>`" + ` follows it. A change the provider
reports itself, in its own client, reaches uma the same way.

Only a channel whose adapter declares ` + string(stateCapability(v)) + ` can do this (` + "`heo channel get`" + `
lists them): --dry-run refuses any other, exit 2 (UNSUPPORTED), as does the
real run, before anything is written. --dry-run also reads what the channel
reports now of the verb, its scope (account: only the connected account's
own view changes, as a mailbox; shared: everyone sees it, as a chat room),
and refuses a flag kind, role or label move the provider does not have. It
prints the exact change and where it goes, and writes and sends nothing.
--revision applies the change only if the conversation is still at that
revision (conflict, exit 4). Committed under --idempotency-key: rerunning
with the same key and arguments returns the same operation (replayed); without
one, heo mints a key and prints it. A plugin actor's change is held for a
decision while held_operations is on, under the trust class the approvals
guide lists (` + "`operation held`" + `). Desk role.`,
		Example: d.example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ch, err := stateChangeOf(cmd, v)
			if err != nil {
				return err
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[conversationStateOut], error) {
				var w written[conversationStateOut]
				err := a.deskHeld(ctx, c, model.ActionConversationStateAccount, internalAudience, func(s desk.Service) error {
					p, err := s.PlanState(ctx, args[0], ch, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = conversationStatePlan(p, key)
						return nil
					}
					verb, class := p.Change.HoldClass()
					h, err := s.Hold(ctx, &p.Command, desk.HoldInput{
						Action: p.Action, Verb: verb, Class: class,
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Preview: conversationStatePlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.SubmitState(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = conversationStateOut{Conversation: conv, Verb: string(v), Change: p.Change.Asked(),
						Operation: operationOf(acc), OperationKey: key}
					if p.Limits != nil {
						w.Out.Scope = string(p.Limits.Scope)
					}
					w.Out.Notes = append(queuedNote(w.Out.Operation), fmt.Sprintf(
						"not yet applied at the provider: `heo delivery get %s` follows it, and `heo conversation get` shows the provider state once it did", acc.Operation.Ref.ID))
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, conversationStateRows, res.Out.Notes)
		},
	}
	switch v {
	case model.StateVerbFlag, model.StateVerbUnflag:
		c.Flags().String(flagKind, model.FlagStar, "Flag kind: star or important")
	case model.StateVerbMove:
		c.Flags().String(flagTo, "", "Destination: a provider folder or label, by ID or name")
		c.Flags().String(flagRole, "", "Destination by special-use role: inbox, archive, trash or junk")
		c.Flags().StringSlice(flagAdd, nil, "Provider labels to add, by ID or name (repeat or comma-separate)")
		c.Flags().StringSlice(flagRemove, nil, "Provider labels to remove, by ID or name (repeat or comma-separate)")
		c.MarkFlagsMutuallyExclusive(flagTo, flagRole)
		c.MarkFlagsOneRequired(flagTo, flagRole, flagAdd, flagRemove)
	}
	addOptionalRevisionFlag(c)
	providerDependent(c, stateCapability(v))
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &conversationStateOut{}, "1.0")
	return c
}
