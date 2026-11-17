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

// flagSet is `conversation tag`'s whole-list flag.
const flagSet = "set"

// conversationTagOut is what `conversation tag` reports: the
// conversation as uma holds it (the tags are the provider's, and uma
// records none), the provider message the change acts on, the effect
// run and what it asked, the operation and its key.
type conversationTagOut struct {
	Conversation model.Conversation `json:"conversation"     yaml:"conversation"`
	Message      string             `json:"message"          yaml:"message"`
	Effect       string             `json:"effect"           yaml:"effect"`
	Add          []string           `json:"add,omitempty"    yaml:"add,omitempty"`
	Remove       []string           `json:"remove,omitempty" yaml:"remove,omitempty"`
	Set          *[]string          `json:"set,omitempty"    yaml:"set,omitempty"`
	Operation    operationView      `json:"operation"        yaml:"operation"`
	OperationKey string             `json:"operation_key"    yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty"  yaml:"notes,omitempty"`
}

// mailTagOf reads the change the leaf was given: --set only when named,
// an empty value naming an empty list.
func mailTagOf(cmd *cobra.Command) desk.MailTag {
	var t desk.MailTag
	t.Add, _ = cmd.Flags().GetStringSlice(flagAdd)
	t.Remove, _ = cmd.Flags().GetStringSlice(flagRemove)
	if cmd.Flags().Changed(flagSet) {
		v, _ := cmd.Flags().GetStringSlice(flagSet)
		t.Set = &v
	}
	return t
}

func tagAsked(t desk.MailTag) string {
	if t.Set != nil {
		return "set categories to " + setText(t.Set)
	}
	var parts []string
	if len(t.Add) > 0 {
		parts = append(parts, "add "+strings.Join(t.Add, ", "))
	}
	if len(t.Remove) > 0 {
		parts = append(parts, "remove "+strings.Join(t.Remove, ", "))
	}
	return strings.Join(parts, "; ")
}

// conversationTagPlan renders a tag plan for --dry-run: the external
// change at the mail provider, and nothing in uma.
func conversationTagPlan(p desk.TagPlan, key string) *cli.Plan {
	cur := p.Conversation
	args := map[string]any{
		"conversation": cur.Ref.ID, "inbox": cur.Ref.Inbox.ID, "destination": cur.ReplyRoute,
		"visibility": p.Request.Audience.Visibility, "message": p.Message,
		"change":   p.Request.Action.Name + " " + string(p.Request.Action.Data),
		"current":  cur,
		"delivery": "queued, not applied: nothing changes in uma, whose conversation records no provider tags",
	}
	if key != "" {
		args["operation_key"] = key
	}
	if len(p.Resolved) > 0 {
		args["resolved"] = resolvedLines(p.Resolved)
	}
	checked := effectArgs(args, p.Declaration)
	effect := planEffect("update", "mail:message "+p.Message, true, fmt.Sprintf(
		"external: the provider message %s of conversation %s, sent as %s (%s); the mailbox shows it, and uma records none of it",
		p.Message, cur.Ref.ID, p.Request.Action.Name, tagAsked(p.Tag)))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: append(checked, "conversation", "message", "request")}
}

func conversationTagRows(o conversationTagOut) [][2]string {
	rows := [][2]string{
		{"conversation", string(o.Conversation.Ref.ID)},
		{"message", orDash(o.Message)},
		{"effect", o.Effect},
	}
	if len(o.Add) > 0 {
		rows = append(rows, [2]string{"add asked", strings.Join(o.Add, ", ")})
	}
	if len(o.Remove) > 0 {
		rows = append(rows, [2]string{"remove asked", strings.Join(o.Remove, ", ")})
	}
	if o.Set != nil {
		rows = append(rows, [2]string{"set asked", setText(o.Set)})
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) conversationTagCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "tag <conversation>",
		Short: "Change the mailbox's own tags on a mail conversation's message",
		Long: `Change the tags the mail provider keeps on the message the mail
conversation <conversation> (record ID) replies to, the latest the
peer wrote: Gmail's labels, by label ID or name, with --add and --remove
(repeat or comma-separate; IDs such as Label_12, STARRED or INBOX, or a
name such as Clients/Acme), or
Outlook's categories, by name, with --set, which replaces the whole list
(--set= or --set '' clears it). Name one way, not both. The change
acts on that one message, never on the rest of the thread. A value may
not hold a comma.

These are the provider's tags, not heo's labels: ` + "`conversation label`" + `
adds labels uma defines (` + "`label list`" + `), local to uma, and changes nothing
at the provider; this changes the provider's tags and records none in
uma. A Gmail label given by name is resolved through the account's
mailbox list (` + "`provider-label list`" + `) to its ID, which is what the change
sends: an ID the list holds is used as it is, and wins over a name
spelled like it; a name is matched exactly, then without regard to case, and two labels that
share it are refused (exit 2) with their IDs. A value the list does not hold
is sent as given, and whether it exists is the provider's answer, shown on
the operation. Outlook's category names are sent as given. --dry-run prints
what each name resolved to.

This is an external change: the provider applies it to the message and
the mailbox shows it, in every client. It is queued as a uma
operation: the command returns once uma accepted it durably, never
applied. The daemon's workers send it, and ` + "`delivery get <operation>`" + `
follows it.

Only a channel whose adapter declares extension_execute and ships the
effect the flags need can do this: Gmail's labels for --add and
--remove (google.mail), Outlook's categories for --set (microsoft.mail).
--dry-run refuses any other, exit 2 (UNSUPPORTED), as does the real run,
before anything is written; an adapter that ships the other effect
says which flags it takes. --dry-run also asks the adapter, offline, to
check the fields (VALIDATION, exit 2), and prints the exact change and
where it goes; it writes and sends nothing. A conversation the peer has
written no message in has nothing to tag (CONFLICT, exit 4). Committed
under --idempotency-key: rerunning with the same key and arguments
returns the same operation (replayed); without one, heo mints a key and
prints it. Desk role.`,
		Example: `  heo conversation tag conv_123 --add Label_12,STARRED --remove INBOX --dry-run
  heo conversation tag conv_123 --set Billing,Urgent
  heo conversation tag conv_123 --set= --idempotency-key untag-123`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t := mailTagOf(cmd)
			if err := desk.CheckMailTag(t); err != nil {
				return output.UsageError(strings.TrimPrefix(err.Error(), "validation: "))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[conversationTagOut], error) {
				var w written[conversationTagOut]
				err := a.desk(ctx, c, model.ActionConversationLabel, internalAudience, func(s desk.Service) error {
					p, err := s.PlanTag(ctx, args[0], t, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = conversationTagPlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionConversationLabel, Verb: "tag", Class: tagClass(p.Tag),
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Preview: conversationTagPlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.TagConversation(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = conversationTagOut{Conversation: conv, Message: p.Message, Effect: p.Request.Action.Name,
						Add: p.Tag.Add, Remove: p.Tag.Remove, Set: p.Tag.Set, Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = append(queuedNote(w.Out.Operation), fmt.Sprintf(
						"uma records no provider tags: `heo delivery get %s` follows the change, and the mailbox shows it once applied", acc.Operation.Ref.ID))
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, conversationTagRows, res.Out.Notes)
		},
	}
	c.Flags().StringSlice(flagAdd, nil, "Gmail labels to add, by ID or name (repeat or comma-separate)")
	c.Flags().StringSlice(flagRemove, nil, "Gmail labels to remove, by ID or name (repeat or comma-separate)")
	c.Flags().StringSlice(flagSet, nil, "Outlook category names that replace the whole list; empty clears it (repeat or comma-separate)")
	c.MarkFlagsOneRequired(flagAdd, flagRemove, flagSet)
	c.MarkFlagsMutuallyExclusive(flagAdd, flagSet)
	c.MarkFlagsMutuallyExclusive(flagRemove, flagSet)
	providerDependent(c, model.CapabilityExtensionExecute)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &conversationTagOut{}, Version: "1.0"})
	return c
}

// tagClass is the target class of a tag change: the tags it names.
func tagClass(t desk.MailTag) string {
	names := append(append([]string{}, t.Add...), t.Remove...)
	if t.Set != nil {
		names = append(names, *t.Set...)
	}
	return strings.Join(names, ",")
}

// setText says a set asked for, in a plan or a row.
func setText(s *[]string) string {
	if s == nil {
		return "unchanged"
	}
	if len(*s) == 0 {
		return "(none)"
	}
	return strings.Join(*s, ", ")
}
