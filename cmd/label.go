package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags the label leaves and the label filters use.
const (
	flagLabel       = "label"
	flagLabelAll    = "label-all"
	flagLabelNone   = "label-none"
	flagAdd         = "add"
	flagRemove      = "remove"
	flagDescription = "description"
	flagColor       = "color"
)

// labelOut is what `label create` reports: the label as stored, the
// operation and its key.
type labelOut struct {
	Label        model.Label   `json:"label"         yaml:"label"`
	Operation    operationView `json:"operation"     yaml:"operation"`
	OperationKey string        `json:"operation_key" yaml:"operation_key"`
}

// labelRow is a label in a listing table.
type labelRow struct {
	ID          string `json:"id"          yaml:"id"          table:"ID,id"`
	Name        string `json:"name"        yaml:"name"        table:"NAME"`
	Description string `json:"description" yaml:"description" table:"DESCRIPTION"`
	Color       string `json:"color"       yaml:"color"       table:"COLOR"`
}

func labelRows(l model.Label) [][2]string {
	return [][2]string{
		{"id", string(l.Ref.ID)},
		{"name", l.Name},
		{"description", orDash(l.Description)},
		{"color", orDash(l.Color)},
		{"revision", string(l.Revision)},
	}
}

// shellWord quotes a label name for a suggested command line when the
// shell would split or interpret it.
func shellWord(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\n'\"\\$`!*?;&|<>()[]{}#~") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// labelFailure is a label name no label holds, with the way to fix it:
// create it before adding it, or pick an existing one. heo never
// creates a label implicitly.
func (a *app) labelFailure(err error) error {
	var ue *desk.UnknownLabelError
	if !errors.As(err, &ue) {
		return err
	}
	e := errmap.Of(err)
	list := "`" + a.heoCommand("label list") + "`"
	if ue.Use == desk.LabelAdd {
		e.SuggestedFix = "create it first: `" + a.heoCommand("label create "+shellWord(ue.Name)) + "`; " + list + " shows the labels"
	} else {
		e.SuggestedFix = list + " shows the labels"
	}
	return e
}

func (a *app) labelCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "label",
		Short: "Define the tenant's conversation labels",
		Long: `Define the labels conversations can hold. A label is uma's record,
tenant-wide: one definition reusable across every inbox and every
channel, stored locally whatever the channel — a provider's own labels
are never matched to these by name.

` + "`conversation label`" + ` adds and removes labels on a conversation;
` + "`conversation list --label`" + ` (any of), ` + "`--label-all`" + ` and
` + "`--label-none`" + ` filter by them.

Every command runs in the profile's daemon (none running exits 70).
Creating labels is the desk role's; desk and viewer list them.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.labelListCmd(), a.labelCreateCmd())
	return grp
}

func (a *app) labelListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the tenant's labels",
		Long: `List the tenant's labels a page at a time (--limit, --cursor with the
previous page's next), in the order they were created: ID, name,
description and color. Read-only.`,
		Example: `  heo label list
  heo label list --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Label], error) {
				var out page[model.Label]
				err := a.desk(ctx, c, model.ActionLabelRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListLabels(ctx, pg)
					out = pageFrom(p)
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
			rows := make([]labelRow, 0, len(res.Items))
			for _, l := range res.Items {
				rows = append(rows, labelRow{ID: string(l.Ref.ID), Name: l.Name, Description: orDash(l.Description), Color: orDash(l.Color)})
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[model.Label]{}, "1.0")
	return c
}

func labelCreatePlan(p desk.LabelPlan, key string) *cli.Plan {
	l := p.Request.Label
	args := map[string]any{"name": l.Name, "description": l.Description, "color": l.Color}
	if key != "" {
		args["operation_key"] = key
	}
	effect := planEffect("create", "label:"+l.Name, false,
		"tenant-wide label, reusable across every inbox; no conversation holds it yet")
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: []string{"authorization", "name", "request"}}
}

func (a *app) labelCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a label",
		Long: `Create the label <name> for the whole tenant, with an optional
--description and --color (#rrggbb, lowercase hex). Names are unique
under case folding: "Billing" and "billing" are one name, and creating
one that exists is a conflict (exit 4). A name may not hold a comma:
the commands that take label names take comma lists.

Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed); without
one, heo mints a key and prints it. --dry-run prints the label and
writes nothing. Desk role.`,
		Example: `  heo label create billing
  heo label create urgent --description "Answer within the hour" --color "#d00000"
  heo label create billing --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			if strings.Contains(args[0], ",") {
				return output.UsageError(fmt.Sprintf("label name %q holds a comma: label names are given as comma lists, so it could never be named", args[0]))
			}
			var change desk.LabelChange
			change.Name = args[0]
			change.Description, _ = cmd.Flags().GetString(flagDescription)
			change.Color, _ = cmd.Flags().GetString(flagColor)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[labelOut], error) {
				var w written[labelOut]
				err := a.desk(ctx, c, model.ActionLabelConfigure, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanLabelCreate(ctx, change, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = labelCreatePlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionLabelConfigure, Verb: "label-create", Class: change.Name,
						Target: model.Ref{Tenant: s.Tenant, Kind: model.KindLabel}, Preview: labelCreatePlan(p, key)})
					if err != nil {
						return err
					}
					l, acc, err := s.CreateLabel(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = labelOut{Label: l, Operation: operationOf(acc), OperationKey: key}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return res.render(a, cmd, func(o labelOut) [][2]string {
				return append(labelRows(o.Label), operationRows(o.Operation, o.OperationKey)...)
			}, nil)
		},
	}
	c.Flags().String(flagDescription, "", "What the label means")
	c.Flags().String(flagColor, "", "Display color, #rrggbb in lowercase hex")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &labelOut{}, "1.0")
	return c
}

// conversationLabelsOut is what `conversation label` reports: the
// conversation after the write, the names of the labels it holds, those
// the write changed, the operation and its key.
type conversationLabelsOut struct {
	Conversation model.Conversation `json:"conversation"      yaml:"conversation"`
	Labels       []string           `json:"labels"            yaml:"labels"`
	Added        []string           `json:"added,omitempty"   yaml:"added,omitempty"`
	Removed      []string           `json:"removed,omitempty" yaml:"removed,omitempty"`
	Operation    operationView      `json:"operation"         yaml:"operation"`
	OperationKey string             `json:"operation_key"     yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty"   yaml:"notes,omitempty"`
}

func names(ls []model.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func labelingPlan(p desk.LabelingPlan, key string) *cli.Plan {
	cur := p.Conversation
	verb, event := "add", model.EventConversationLabelAdded
	if p.Remove {
		verb, event = "remove", model.EventConversationLabelRemoved
	}
	args := map[string]any{
		"conversation": cur.Ref.ID, "inbox": cur.Ref.Inbox.ID, "labels": strings.Join(names(p.Labels), ", "),
		"change": verb, "current": cur,
	}
	if key != "" {
		args["operation_key"] = key
	}
	effects := []cli.Effect{}
	for _, l := range p.Changes {
		effects = append(effects, planEffect("update", "conversation:"+string(cur.Ref.ID), true,
			fmt.Sprintf("%s label %s, local to uma (nothing changes at the provider); uma emits %s", verb, l.Name, event)))
	}
	return &cli.Plan{Args: args, Effects: effects,
		PrerequisitesChecked: []string{"authorization", "labels", "request"}, Warnings: p.Warnings}
}

func (a *app) conversationLabelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "label <conversation>",
		Short: "Add labels to a conversation, or remove them",
		Long: `Add the labels --add names to the conversation <conversation> (record
ID), or remove the labels --remove names; one of the two, repeated or
comma-separated for several labels. Labels are uma's and local: nothing
changes at the provider.

Every name must be a label of the tenant (` + "`label list`" + `); an
unknown one is not found (exit 3) and nothing changes — heo never
creates a label behind your back (` + "`label create`" + `). Adding a label the
conversation already holds, or removing one it does not, changes
nothing and succeeds; uma emits one label-added or label-removed event
per label it actually changed.

Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed); without
one, heo mints a key and prints it. --dry-run prints what would change
and writes nothing. Desk role.`,
		Example: `  heo conversation label conv_123 --add billing
  heo conversation label conv_123 --add billing,urgent --dry-run
  heo conversation label conv_123 --remove billing`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			add, _ := cmd.Flags().GetStringSlice(flagAdd)
			remove, _ := cmd.Flags().GetStringSlice(flagRemove)
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			want, removing := add, false
			if len(remove) > 0 {
				want, removing = remove, true
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[conversationLabelsOut], error) {
				var w written[conversationLabelsOut]
				err := a.desk(ctx, c, model.ActionConversationLabel, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanLabeling(ctx, args[0], want, removing, opID(key))
					if err != nil {
						return a.labelFailure(err)
					}
					if cli.IsDryRun(cmd) {
						w.Plan = labelingPlan(p, key)
						return nil
					}
					verb := "label"
					if removing {
						verb = "unlabel"
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionConversationLabel, Verb: verb, Class: strings.Join(names(p.Changes), ","),
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Preview: labelingPlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.ApplyLabeling(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					held, err := s.LabelNames(ctx, conv.Labels)
					if err != nil {
						return err
					}
					w.Out = conversationLabelsOut{Conversation: conv, Labels: held, Operation: operationOf(acc),
						OperationKey: key, Notes: p.Warnings}
					if removing {
						w.Out.Removed = names(p.Changes)
					} else {
						w.Out.Added = names(p.Changes)
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, func(o conversationLabelsOut) [][2]string {
				rows := [][2]string{
					{"conversation", string(o.Conversation.Ref.ID)},
					{"labels", orDash(strings.Join(o.Labels, ", "))},
				}
				if o.Added != nil {
					rows = append(rows, [2]string{"added", strings.Join(o.Added, ", ")})
				}
				if o.Removed != nil {
					rows = append(rows, [2]string{"removed", strings.Join(o.Removed, ", ")})
				}
				rows = append(rows, [2]string{"revision", string(o.Conversation.Revision)})
				return append(rows, operationRows(o.Operation, o.OperationKey)...)
			}, res.Out.Notes)
		},
	}
	c.Flags().StringSlice(flagAdd, nil, "Labels to add, by name (repeat or comma-separate)")
	c.Flags().StringSlice(flagRemove, nil, "Labels to remove, by name (repeat or comma-separate)")
	c.MarkFlagsOneRequired(flagAdd, flagRemove)
	c.MarkFlagsMutuallyExclusive(flagAdd, flagRemove)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &conversationLabelsOut{}, "1.0")
	return c
}

// addLabelFilterFlags adds the label filters to a listing of conversations
// ("conversations") or of messages by their conversation's labels
// ("messages of conversations").
func addLabelFilterFlags(c *cobra.Command, what string) {
	c.Flags().StringSlice(flagLabel, nil, "Only "+what+" holding any of these labels (repeat or comma-separate)")
	c.Flags().StringSlice(flagLabelAll, nil, "Only "+what+" holding all of these labels (repeat or comma-separate)")
	c.Flags().StringSlice(flagLabelNone, nil, "Only "+what+" holding none of these labels (repeat or comma-separate)")
}

// labelFiltered reports whether f filters by labels.
func labelFiltered(f desk.Filter) bool {
	return len(f.LabelAny)+len(f.LabelAll)+len(f.LabelNone) > 0
}
