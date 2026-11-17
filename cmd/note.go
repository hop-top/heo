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

// noteOut is what a note add reports: the note uma recorded, its
// visibility (always internal), its placement, and the operation —
// committed for a local note, queued for one the provider receives.
type noteOut struct {
	Message      model.ID           `json:"message"               yaml:"message"`
	Conversation model.ID           `json:"conversation"          yaml:"conversation"`
	Inbox        model.ID           `json:"inbox"                 yaml:"inbox"`
	Visibility   model.Visibility   `json:"visibility"            yaml:"visibility"`
	Placement    desk.Placement     `json:"placement"             yaml:"placement"`
	Destination  *model.ExternalRef `json:"destination,omitempty" yaml:"destination,omitempty"`
	Operation    operationView      `json:"operation"             yaml:"operation"`
	OperationKey string             `json:"operation_key"         yaml:"operation_key"`
}

func (a *app) noteCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "note",
		Short: "Read and add internal notes",
		Long: `Read and add internal notes: messages in a conversation for desk actors
alone. A note is never public, and never a reply.

Where a note lives is its inbox's choice (` + "`inbox update --deliver-notes`" + `):
an inbox keeps notes in uma (placement local) or also delivers them to
the provider as private notes (placement provider). ` + "`note add`" + ` makes
you state it, and refuses a placement that is not the inbox's.

Every command runs in the profile's daemon (none running exits 70) and
is the desk role's: any other caller is refused (exit 5), on every
surface.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.noteListCmd(), a.noteGetCmd(), a.noteAddCmd())
	return grp
}

func (a *app) noteListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List internal notes across the inboxes you may read",
		Long: `List the internal notes you may read, across every inbox you may read,
in the order uma recorded them, a page at a time (--limit, --cursor with
the previous page's next). --inbox (key or ID) and --conversation
(record ID) narrow it. Desk role. Read-only.`,
		Example: `  heo note list --conversation conv_123
  heo note list --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.messageListRun(cmd, args, true)
		},
	}
	addMessageFilterFlags(c)
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &page[model.Message]{}, Version: "1.0"})
	return c
}

func (a *app) noteGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <note>",
		Short: "Show one internal note",
		Long: `Show the internal note <note>, by record ID. --conversation (record ID)
narrows the lookup. A message that is not a note exits 3: ` + "`message get`" + `
shows it. Desk role. Read-only.`,
		Example: `  heo note get msg_123
  heo note get msg_123 --conversation conv_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.messageGetRun(cmd, args, true)
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the note belongs to (record ID); narrows the lookup")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &model.Message{}, Version: "1.0"})
	return c
}

// notePlan renders a note plan for --dry-run: where the note goes and
// that it is internal.
func notePlan(p desk.NotePlan, key string) *cli.Plan {
	conv, in := p.Conversation, p.Inbox
	inbox := channels.Name(in.Key, in.Ref.ID)
	args := map[string]any{
		"conversation": conv.Ref.ID, "inbox": inbox, "channel": in.Channel.ID,
		"visibility": model.Internal, "placement": p.Placement, "content": p.Request.Content,
	}
	detail := fmt.Sprintf("internal note, kept in uma: inbox %s keeps notes local and nothing reaches the provider", inbox)
	if p.Placement == desk.PlacementProvider {
		args["destination"] = conv.ReplyRoute
		detail = fmt.Sprintf("internal note, committed in uma and queued to the provider as a private note on %s "+
			"through inbox %s (channel %s); never public", routeName(conv.ReplyRoute), inbox, in.Channel.ID)
	}
	if key != "" {
		args["operation_key"] = key
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("create", "conversation:"+string(conv.Ref.ID), false, detail)},
		PrerequisitesChecked: []string{"authorization", "conversation", "inbox", "placement", "request"}}
}

func noteRows(o noteOut) [][2]string {
	rows := [][2]string{
		{"message", string(o.Message)},
		{"conversation", string(o.Conversation)},
		{"inbox", string(o.Inbox)},
		{"visibility", string(o.Visibility)},
		{"placement", string(o.Placement)},
	}
	if o.Destination != nil {
		rows = append(rows, [2]string{"destination", routeName(*o.Destination)})
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) noteAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add <conversation>",
		Short: "Add an internal note to a conversation",
		Long: `Add an internal note with --text to the conversation <conversation>
(record ID). A note is always internal: desk actors alone read it, and
it never becomes a public reply.

--placement is required and must be where the conversation's inbox
places notes: local (kept in uma) or provider (also delivered to the
provider as a private note, through an inbox with --deliver-notes). Any
other is a conflict (exit 4) naming the inbox: heo never changes where a
note goes behind your back.

A local note commits at once. A provider note commits in uma and is
queued: the command returns with its operation, never delivered, and
the daemon's workers deliver it.

--dry-run prints the exact placement, destination and visibility and
writes nothing; running it for real checks everything again. Committed
as a uma operation under --idempotency-key: rerunning with the same key
and text returns the same operation (replayed); without one, heo mints
a key and prints it. Desk role.`,
		Example: `  heo note add conv_123 --text "Customer is on the legacy plan." --placement local
  heo note add conv_123 --text "Escalated to billing." --placement provider --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _ := cmd.Flags().GetString(flagPlacement)
			placement, err := desk.ParsePlacement(raw)
			if err != nil {
				return output.UsageError(fmt.Sprintf("--%s %q: want %s or %s", flagPlacement, raw, desk.PlacementLocal, desk.PlacementProvider))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			text, _ := cmd.Flags().GetString(flagText)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[noteOut], error) {
				var w written[noteOut]
				err := a.desk(ctx, c, model.ActionMessageNote, internalAudience, func(s desk.Service) error {
					p, err := s.PlanNote(ctx, args[0], text, placement, opID(key))
					if err != nil {
						return a.placementFailure(err)
					}
					if cli.IsDryRun(cmd) {
						w.Plan = notePlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionMessageNote, Verb: "note", Class: string(p.Placement),
						Channel: string(p.Inbox.Ref.ID), Target: p.Conversation.Ref.Ref(), Preview: notePlan(p, key)})
					if err != nil {
						return err
					}
					acc, err := s.AddNote(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = noteOut{
						Conversation: p.Conversation.Ref.ID, Inbox: p.Inbox.Ref.ID, Visibility: model.Internal,
						Placement: p.Placement, Operation: operationOf(acc), OperationKey: key,
					}
					if p.Placement == desk.PlacementProvider {
						dest := p.Conversation.ReplyRoute
						w.Out.Destination = &dest
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
			return renderDeskWrite(a, cmd, res, noteRows, queuedNote(res.Out.Operation))
		},
	}
	addTextFlag(c, "Text of the note")
	c.Flags().String(flagPlacement, "", "Where the note lives, as its inbox places notes: local or provider (required)")
	_ = c.MarkFlagRequired(flagPlacement)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &noteOut{}, Version: "1.0"})
	return c
}
