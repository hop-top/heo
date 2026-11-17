package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags of inbox create and update.
const (
	flagChannel        = "channel"
	flagName           = "name"
	flagDestination    = "destination"
	flagMode           = "mode"
	flagPolicy         = "selection-policy"
	flagLockToSingle   = "lock-to-single"
	flagLifecycleRules = "lifecycle-rules"
	flagDeliverNotes   = "deliver-notes"
)

// inboxRow is an inbox in a listing table.
type inboxRow struct {
	ID           string `json:"id"           yaml:"id"           table:"ID,id"`
	Key          string `json:"key"          yaml:"key"          table:"KEY"`
	Name         string `json:"name"         yaml:"name"         table:"NAME"`
	Channel      string `json:"channel"      yaml:"channel"      table:"CHANNEL"`
	Mode         string `json:"mode"         yaml:"mode"         table:"MODE"`
	Destinations string `json:"destinations" yaml:"destinations" table:"DESTINATIONS"`
	State        string `json:"state"        yaml:"state"        table:"STATE"`
	Revision     string `json:"revision"     yaml:"revision"     table:"REVISION"`
}

// inboxOut is what an inbox write reports: the inbox as stored after
// it, the operation that wrote it and the key it ran under.
type inboxOut struct {
	Inbox        model.Inbox   `json:"inbox"           yaml:"inbox"`
	Operation    operationView `json:"operation"       yaml:"operation"`
	OperationKey string        `json:"operation_key"   yaml:"operation_key"`
	Notes        []string      `json:"notes,omitempty" yaml:"notes,omitempty"`
}

func inboxState(in model.Inbox) string {
	if in.Disabled {
		return "disabled"
	}
	return "enabled"
}

func (a *app) inboxCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "inbox",
		Short: "Configure the profile's inboxes and switch them on and off",
		Long: `Configure inboxes: each binds destinations of one channel (a site,
an address, a number) to a conversation selection policy. An inbox is
where inbound lands and what desk commands filter by.

An inbox is also the availability switch: ` + "`inbox disable`" + ` rejects
inbound to it, while its channel keeps its connection and queued sends
still deliver; ` + "`inbox enable`" + ` accepts inbound again.

Every command runs in the profile's daemon (none running exits 70).
Configuring inboxes is the operator role's; every role that reads
conversations reads inboxes. Writes name the revision they replace
(--revision, from ` + "`inbox get`" + `): a stale one is a conflict, exit 4.
--dry-run previews the exact request and writes nothing.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.inboxListCmd(), a.inboxGetCmd(), a.inboxCreateCmd(), a.inboxUpdateCmd(),
		a.inboxSwitchCmd(false), a.inboxSwitchCmd(true))
	return grp
}

func (a *app) inboxListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the profile's inboxes",
		Long: `List the inboxes you may read, a page at a time (--limit, --cursor
with the previous page's next), with their channel, selection mode,
destinations and state. Read-only.`,
		Example: `  heo inbox list
  heo inbox list --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Inbox], error) {
				var out page[model.Inbox]
				err := a.configure(ctx, c, model.ActionInboxRead, func(s channels.Service) error {
					p, err := s.ListInboxes(ctx, pg)
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
			rows := make([]inboxRow, 0, len(res.Items))
			for _, in := range res.Items {
				rows = append(rows, inboxRow{
					ID: string(in.Ref.ID), Key: orDash(in.Key), Name: orDash(in.Name), Channel: string(in.Channel.ID),
					Mode: in.Selection.Mode, Destinations: orDash(strings.Join(in.Destinations, ",")),
					State: inboxState(in), Revision: string(in.Revision),
				})
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[model.Inbox]{}, "1.0")
	return c
}

func (a *app) inboxGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <inbox>",
		Short: "Show one inbox",
		Long: `Show the inbox <inbox>, by key or ID: its channel, destinations,
selection, note delivery, state and revision (what ` + "`--revision`" + `
takes). Read-only. No such inbox, or one you may not read, exits 3.`,
		Example: `  heo inbox get web
  heo inbox get web --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (model.Inbox, error) {
				var out model.Inbox
				err := a.configure(ctx, c, model.ActionInboxRead, func(s channels.Service) (err error) {
					out, err = s.FindInbox(ctx, args[0])
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
			return writeRows(cmd.OutOrStdout(), inboxRows(res))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &model.Inbox{}, "1.0")
	return c
}

func inboxRows(in model.Inbox) [][2]string {
	sel := in.Selection.Mode
	if in.Selection.LockToSingle != nil {
		sel += fmt.Sprintf(", lock to single %v", *in.Selection.LockToSingle)
	}
	if in.Selection.LifecycleRules != "" {
		sel += ", lifecycle " + in.Selection.LifecycleRules
	}
	if in.Selection.Policy != "" {
		sel += ", policy " + in.Selection.Policy
	}
	return [][2]string{
		{"id", string(in.Ref.ID)},
		{"key", orDash(in.Key)},
		{"name", orDash(in.Name)},
		{"channel", string(in.Channel.ID)},
		{"destinations", orDash(strings.Join(in.Destinations, ", "))},
		{"selection", sel},
		{"deliver notes", fmt.Sprint(in.DeliverNotes)},
		{"state", inboxState(in)},
		{"revision", string(in.Revision)},
	}
}

// addInboxFlags adds the fields inbox create and update set.
func addInboxFlags(c *cobra.Command) {
	c.Flags().String(flagKey, "", "Name for the inbox, unique in the profile (what commands take in place of its ID)")
	c.Flags().String(flagName, "", "Display name")
	c.Flags().StringSlice(flagDestination, nil,
		"Destination of the channel the inbox receives (a site, an address, a number); repeat or comma-separate. Replaces the list")
	c.Flags().String(flagMode, "", "Conversation selection mode: "+strings.Join(selectionModes(), ", "))
	c.Flags().String(flagPolicy, "", "Named selection policy")
	c.Flags().Bool(flagLockToSingle, false, "Per-contact mode: keep one open conversation per contact (true or false; required in that mode)")
	c.Flags().String(flagLifecycleRules, "", "Per-contact mode: lifecycle rule set ("+model.RuleSetContactLifecycleV1+"; required in that mode)")
	c.Flags().Bool(flagDeliverNotes, false, "Deliver internal notes to the provider as private notes (the adapter must declare internal_note)")
}

func selectionModes() []string {
	return []string{model.ModePerContact, model.ModePerRoom, model.ModePerThread, model.ModePerWorkItem}
}

// inboxChange reads the inbox fields set on the command line.
func inboxChange(cmd *cobra.Command) channels.InboxChange {
	var c channels.InboxChange
	changed := func(name string) bool {
		f := cmd.Flags().Lookup(name)
		return f != nil && f.Changed
	}
	str := func(name string) *string {
		if !changed(name) {
			return nil
		}
		v, _ := cmd.Flags().GetString(name)
		return &v
	}
	boolean := func(name string) *bool {
		if !changed(name) {
			return nil
		}
		v, _ := cmd.Flags().GetBool(name)
		return &v
	}
	if v := str(flagChannel); v != nil {
		c.Channel = *v
	}
	c.Key, c.Name, c.Mode, c.Policy, c.LifecycleRules = str(flagKey), str(flagName), str(flagMode), str(flagPolicy), str(flagLifecycleRules)
	c.LockToSingle, c.DeliverNotes = boolean(flagLockToSingle), boolean(flagDeliverNotes)
	if changed(flagDestination) {
		d, _ := cmd.Flags().GetStringSlice(flagDestination)
		c.Destinations = &d
	}
	return c
}

// inboxPlan renders an inbox plan for --dry-run.
func inboxPlan(p channels.InboxPlan, key, verb string) *cli.Plan {
	in := p.Request.Inbox
	args := map[string]any{"inbox": in}
	effect := planEffect("create", "inbox:"+orNew(in.Key, in.Ref.ID), false,
		fmt.Sprintf("inbox on channel %s; ID and revision assigned at commit", in.Channel.ID))
	checked := []string{"authorization", "request", "selection", "destinations", "key"}
	if p.Current != nil {
		args["expected_revision"] = p.Request.ExpectedRevision
		args["current"] = *p.Current
		detail := fmt.Sprintf("replace revision %s; new revision assigned at commit", p.Request.ExpectedRevision)
		switch verb {
		case "disable":
			detail = "reject inbound to the inbox; its channel stays connected and queued sends still deliver; " + detail
		case "enable":
			detail = "accept inbound to the inbox again; " + detail
		}
		effect = planEffect("update", "inbox:"+channels.Name(in.Key, in.Ref.ID), true, detail)
		checked = append([]string{"revision"}, checked...)
	}
	if key != "" {
		args["operation_key"] = key
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: checked, Warnings: p.Warnings}
}

func (a *app) inboxWriteRun(cmd *cobra.Command, args []string, verb string,
	plan func(ctx context.Context, s channels.Service, op model.OperationID) (channels.InboxPlan, error),
) error {
	key, err := operationKey(cmd)
	if err != nil {
		return err
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[inboxOut], error) {
		var w written[inboxOut]
		err := a.configure(ctx, c, model.ActionInboxConfigure, func(s channels.Service) error {
			p, err := plan(ctx, s, opID(key))
			if err != nil {
				return err
			}
			if cli.IsDryRun(cmd) {
				w.Plan = inboxPlan(p, key, verb)
				return nil
			}
			in, acc, err := s.ConfigureInbox(ctx, p)
			if err != nil {
				return err
			}
			w.Out = inboxOut{Inbox: in, Operation: operationOf(acc), OperationKey: key, Notes: p.Warnings}
			return nil
		})
		return w, err
	})
	if err != nil {
		return err
	}
	return res.render(a, cmd, func(o inboxOut) [][2]string {
		return append(inboxRows(o.Inbox), operationRows(o.Operation, o.OperationKey)...)
	}, res.Out.Notes)
}

func (a *app) inboxCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create an inbox on a channel",
		Long: `Create an inbox on the channel --channel (key or ID), receiving its
--destination values, filing conversations by --mode:
per_contact (one conversation per contact; also needs --lock-to-single
and --lifecycle-rules ` + model.RuleSetContactLifecycleV1 + `), per_room,
per_thread or per_work_item. uma validates the selection.

Refused before anything is written: a destination another inbox of the
channel already receives (exit 4) — a destination has one inbox, so
receivers never have to choose — a key another inbox holds (exit 4),
note delivery on an adapter without internal_note (exit 2).

Committed as a uma operation under --idempotency-key (minted and printed
when absent). --dry-run prints the exact inbox that would be written and
writes nothing. Operator role.`,
		Example: `  heo inbox create --channel web --key web --name "Website" --destination site-1 --mode per_contact --lock-to-single=true --lifecycle-rules ` + model.RuleSetContactLifecycleV1 + `
  heo inbox create --channel support-mail --key support --destination support@example.com --mode per_thread --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.inboxWriteRun(cmd, args, "create", func(ctx context.Context, s channels.Service, op model.OperationID) (channels.InboxPlan, error) {
				return s.PlanInboxCreate(ctx, inboxChange(cmd), op, planMode(cmd))
			})
		},
	}
	c.Flags().String(flagChannel, "", "Channel the inbox receives on, by key or ID (required)")
	_ = c.MarkFlagRequired(flagChannel)
	addInboxFlags(c)
	_ = c.MarkFlagRequired(flagMode)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &inboxOut{}, "1.0")
	return c
}

func (a *app) inboxUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update <inbox>",
		Short: "Change an inbox at the revision you read",
		Long: `Replace fields of the inbox <inbox> (key or ID). Fields not given keep
their value; --destination replaces the whole list; the channel never
changes. Leaving per_contact mode drops --lock-to-single and
--lifecycle-rules, which hold only there. Existing conversations stay
as they are.

--revision names the revision being replaced, as ` + "`inbox get`" + ` shows it:
if the inbox changed since, the update is a conflict (exit 4) and
nothing is written. The same checks as create run first. Committed as a
uma operation under --idempotency-key (minted and printed when absent).
--dry-run prints the current inbox and the exact replacement. Operator
role.`,
		Example: `  heo inbox update web --revision rev_123 --destination site-1,site-2
  heo inbox update web --revision rev_123 --name "Website chat" --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.inboxWriteRun(cmd, args, "update", func(ctx context.Context, s channels.Service, op model.OperationID) (channels.InboxPlan, error) {
				return s.PlanInboxUpdate(ctx, args[0], revisionOf(cmd), inboxChange(cmd), op, planMode(cmd))
			})
		},
	}
	addRevisionFlag(c, "inbox")
	addInboxFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &inboxOut{}, "1.0")
	return c
}

// inboxSwitchCmd builds `inbox disable` (disable true) or `inbox enable`.
func (a *app) inboxSwitchCmd(disable bool) *cobra.Command {
	verb, short, long := "enable", "Accept inbound to an inbox again",
		`Enable the inbox <inbox> (key or ID): inbound to its destinations is
accepted again.`
	if disable {
		verb, short, long = "disable", "Reject inbound to an inbox; its channel and queued sends carry on",
			`Disable the inbox <inbox> (key or ID): inbound to its destinations is
rejected from now on. Nothing else stops: the channel keeps its
connection, receivers keep running for its other inboxes, and sends
already queued — replies included — still deliver. Existing
conversations stay as they are.`
	}
	c := &cobra.Command{
		Use:   verb + " <inbox>",
		Short: short,
		Long: long + `

--revision names the revision being replaced, as ` + "`inbox get`" + ` shows
it: if the inbox changed since, it is a conflict (exit 4) and nothing is
written. Committed as a uma operation under --idempotency-key (minted
and printed when absent). --dry-run prints the change and writes
nothing. Operator role.`,
		Example: fmt.Sprintf("  heo inbox %s web --revision rev_123\n  heo inbox %s web --revision rev_123 --dry-run", verb, verb),
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.inboxWriteRun(cmd, args, verb, func(ctx context.Context, s channels.Service, op model.OperationID) (channels.InboxPlan, error) {
				return s.PlanInboxUpdate(ctx, args[0], revisionOf(cmd), channels.InboxChange{Disabled: &disable}, op, planMode(cmd))
			})
		},
	}
	addRevisionFlag(c, "inbox")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &inboxOut{}, "1.0")
	return c
}
