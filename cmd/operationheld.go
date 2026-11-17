package cmd

import (
	"context"
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

// flagDecisionNote is the note recorded with a decision.
const flagDecisionNote = "note"

// The selector of a batch approval and the arguments of an edit.
const (
	flagClass         = "class"
	flagProposer      = "proposer"
	flagMax           = "max"
	flagArgs          = "args"
	flagCorrectedKeys = "corrected-keys"
)

// heldRow is a held operation in a listing table.
type heldRow struct {
	Operation string `json:"operation" yaml:"operation" table:"OPERATION"`
	Kind      string `json:"kind"      yaml:"kind"      table:"KIND"`
	Target    string `json:"target"    yaml:"target"    table:"TARGET"`
	Proposer  string `json:"proposer"  yaml:"proposer"  table:"PROPOSER"`
	Status    string `json:"status"    yaml:"status"    table:"STATUS"`
	Expires   string `json:"expires"   yaml:"expires"   table:"EXPIRES"`
}

func heldRowOf(h desk.HeldOperation) heldRow {
	exp := "-"
	if h.ExpiresAt != nil {
		exp = stamp(*h.ExpiresAt)
	}
	return heldRow{Operation: h.ID, Kind: h.Kind, Target: h.Target, Proposer: h.Proposer, Status: h.Status, Expires: exp}
}

func heldRows(h desk.HeldOperation) [][2]string {
	rows := [][2]string{{"id", h.ID}, {"status", h.Status}, {"kind", h.Kind}, {"target", h.Target},
		{"proposer", h.Proposer}, {"reason", h.Reason}, {"trust key", h.TrustKey}}
	if h.OnBehalfOf != "" {
		rows = append(rows, [2]string{"on behalf of", h.OnBehalfOf})
	}
	if h.ExpiresAt != nil {
		rows = append(rows, [2]string{"expires", stamp(*h.ExpiresAt)})
	}
	if h.Decision != "" {
		rows = append(rows, [2]string{"decision", h.Decision}, [2]string{"decided by", h.DecidedBy})
	}
	if len(h.Preview) > 0 {
		rows = append(rows, [2]string{"preview", string(h.Preview)})
	}
	return append(rows, [2]string{"revision", string(h.Revision)})
}

func (a *app) operationHeldCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "held",
		Short: "Review the operations plugin actors proposed for a decision",
		Long: `A plugin actor's write may be held: uma stores it as a proposal and
applies nothing until a person decides. ` + "`held list`" + ` and ` + "`held show`" + `
read the proposals, ` + "`held approve`" + ` applies one as proposed (or
every one a selector matches), ` + "`held edit`" + ` applies one with
corrected arguments and ` + "`held reject`" + ` applies nothing, and
` + "`held history`" + ` shows the audit trail of each decision.

Deciding is for people: a plugin actor never approves or rejects, whatever
role it holds. Every command runs in the profile's daemon (none running
exits 70). Reading is the desk and operator roles'; deciding is theirs too.`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(grp)
	grp.AddCommand(a.heldListCmd(), a.heldShowCmd(), a.heldDecideCmd(desk.DecideApprove), a.heldDecideCmd(desk.DecideReject), a.heldEditCmd(), a.operationHeldHistoryCmd())
	return grp
}

func (a *app) heldListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the held operations you may read, newest change first",
		Long: `List the operations uma holds for a decision that you may read: the
operation, its kind, target, proposer, status and deadline. Newest change
first, a page at a time (--limit, --cursor with the previous page's next).
--state lists other states instead (held by default; approved proposals
are queued and later states, rejected, expired). --kind, --inbox,
--conversation, --since and --until narrow it as in ` + "`operation list`" + `.
Read-only.`,
		Example: `  heo operation held list
  heo operation held list --kind message.edit --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			var f desk.OperationFilter
			f.Kinds, _ = cmd.Flags().GetStringSlice(flagKind)
			f.States, _ = cmd.Flags().GetStringSlice(flagState)
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			f.Conversation, _ = cmd.Flags().GetString(flagConversation)
			if f.Since, err = timeFlag(cmd, flagSince); err != nil {
				return err
			}
			if f.Until, err = timeFlag(cmd, flagUntil); err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[desk.HeldOperation], error) {
				out := page[desk.HeldOperation]{Items: []desk.HeldOperation{}}
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListHeld(ctx, f, pg)
					if err != nil {
						return err
					}
					out.Next, out.Complete = p.Next, p.Complete
					out.Items = append(out.Items, p.Items...)
					return nil
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]heldRow, len(res.Items))
			for i, h := range res.Items {
				rows[i] = heldRowOf(h)
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().StringSlice(flagKind, nil, "Only operations of these kinds, such as message.edit")
	c.Flags().StringSlice(flagState, nil, "Only operations in these states (default held)")
	c.Flags().String(flagInbox, "", "Only operations on this inbox (key or ID)")
	c.Flags().String(flagConversation, "", "Only operations on this conversation (ID)")
	c.Flags().String(flagSince, "", "Only operations last changed at or after this time (RFC 3339 or a date)")
	c.Flags().String(flagUntil, "", "Only operations last changed before this time (RFC 3339 or a date)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[desk.HeldOperation]{}, "1.0")
	return c
}

// flagOperation names the held operation(s) where MCP cannot pass a
// positional argument.
const flagOperation = "operation"

// heldIDs resolves the operations a command names: the positional
// <operation> arguments or --operation, never both. Over MCP only the
// flag can be passed.
func heldIDs(cmd *cobra.Command, args []string) ([]string, error) {
	flagged, err := cmd.Flags().GetStringSlice(flagOperation)
	if err != nil { // show takes one: a string flag
		one, _ := cmd.Flags().GetString(flagOperation)
		if flagged = nil; one != "" {
			flagged = []string{one}
		}
	}
	switch {
	case len(flagged) == 0 && len(args) == 0:
		return nil, output.UsageError("name the operation: <operation>, or --" + flagOperation + " <operation>")
	case len(flagged) > 0 && len(args) > 0:
		return nil, output.UsageError("<operation> and --" + flagOperation + " both name the operation: pass one")
	case len(flagged) > 0:
		return flagged, nil
	}
	return args, nil
}

func (a *app) heldShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show [<operation>]",
		Short: "Show a held operation: proposer, reason, preview and revision",
		Long: `Show the held operation <operation>: who proposed it and why, what it
would do (the preview, the verb's --dry-run output), its trust key, its
deadline and its revision, which ` + "`held approve --revision`" + ` and
` + "`held reject --revision`" + ` compare against. An operation that is not
held, or that you may not read, exits 3. Name it by <operation> or, where
a positional argument cannot be passed (MCP), by --operation; not both.
Read-only.`,
		Example: `  heo operation held show op_123
  heo operation held show --operation op_123 --format json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := heldIDs(cmd, args)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (desk.HeldOperation, error) {
				var out desk.HeldOperation
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) (err error) {
					out, err = s.GetHeld(ctx, ids[0])
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
			return writeRows(cmd.OutOrStdout(), heldRows(res))
		},
	}
	c.Flags().String(flagOperation, "", "The held operation, in place of <operation> (for MCP, which cannot pass it)")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &desk.HeldOperation{}, "1.0")
	return c
}

// heldResult is one operation's outcome in a decision.
type heldResult struct {
	Operation  string `json:"operation"            yaml:"operation"`
	OK         bool   `json:"ok"                   yaml:"ok"`
	Status     string `json:"status,omitempty"     yaml:"status,omitempty"`
	Decision   string `json:"decision,omitempty"   yaml:"decision,omitempty"`
	Replayed   bool   `json:"replayed"             yaml:"replayed"`
	Correction bool   `json:"correction"           yaml:"correction"`
	Revision   string `json:"revision,omitempty"   yaml:"revision,omitempty"`
	Error      string `json:"error,omitempty"      yaml:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty" yaml:"error_code,omitempty"`
}

// heldDecideOut is what a decision reports: one result per operation,
// in the order named, and how many failed.
type heldDecideOut struct {
	Decision     string       `json:"decision"      yaml:"decision"`
	Results      []heldResult `json:"results"       yaml:"results"`
	Failed       int          `json:"failed"        yaml:"failed"`
	OperationKey string       `json:"operation_key" yaml:"operation_key"`
	// Batch is the batch id a selector run gave every approval: uma
	// counts them once per trust key. Empty for operations named.
	Batch string `json:"batch,omitempty" yaml:"batch,omitempty"`
	// Skipped are the operations a selector matched that always-held
	// rules exclude from a batch.
	Skipped []desk.Skipped `json:"skipped,omitempty" yaml:"skipped,omitempty"`
}

func heldDecideRows(o heldDecideOut) [][2]string {
	rows := [][2]string{{"decision", o.Decision}}
	for _, r := range o.Results {
		v := r.Status
		switch {
		case !r.OK:
			v = "failed: " + r.Error
		case r.Replayed:
			v += " (replayed)"
		}
		rows = append(rows, [2]string{r.Operation, v})
	}
	for _, k := range o.Skipped {
		rows = append(rows, [2]string{k.Operation, "skipped: " + k.Why})
	}
	if o.Batch != "" {
		rows = append(rows, [2]string{"batch", o.Batch})
	}
	return append(rows, [2]string{"key", o.OperationKey})
}

func (a *app) heldDecideCmd(d desk.Decision) *cobra.Command {
	verb, short, long, effect := "approve", "Approve held operations: apply them as proposed",
		`Approve the held operations <operation>...: uma queues each as proposed,
under the key its proposal claimed, and the daemon's workers apply it.

Instead of naming operations, a selector approves every held operation it
matches in one call: --class (a verb class, such as trash, move_to or
reply:send) and --inbox (the mailbox) together, --proposer to narrow to
one plugin. A selector with no class or no inbox is refused, never read as
"everything". Only the operations you may read match. --dry-run lists the
matches and writes nothing; review it before the real run. At most --max
operations are approved (default `+fmt.Sprint(desk.DefaultBatch)+`, at most `+fmt.Sprint(desk.MaxBatch)+`): more
matches are refused, not truncated, so narrow the selector or raise
--max. The always-held rules still exclude an operation from a batch: a
cc added, a Reply-To that differs, an unattended send, a mailbox marked
approval-always, a carry-over, the supervised week. They are listed as
skipped and left held for a decision on their own. Every approval of a
run shares one batch id (reported as batch), which uma counts once per
trust key: approving fifty trashes is one clean approval of their class,
not fifty.`, "queues the held operation as proposed"
	if d == desk.DecideReject {
		verb, short, effect = "reject", "Reject held operations: apply nothing",
			"rejects the held operation; nothing is applied"
		long = `Reject the held operations <operation>...: nothing is applied and no
work is queued. A rejection counts against the proposer's trust.`
	}
	c := &cobra.Command{
		Use:   verb + " [<operation>...]",
		Short: short,
		Long: long + `

Name one operation or several, as <operation>... or, where a positional
argument cannot be passed (MCP), --operation (repeatable); not both. Each is decided on its own: one that
fails (not held, expired, stale, a conflict) is reported and the rest are
still decided; the command then exits 1 and the output lists every
result. --revision compares against the operation revision you read with
` + "`held show`" + ` (a mismatch is a conflict, exit 4) and applies to one
operation; without it heo reads the current revision. A decision already
made is returned as it stands (replayed), writing nothing.

An approval after the target changed, a new inbound message on the
conversation say, is refused as stale (exit 4) unless --force. --force
skips only that check, never the revision compare or an authorization.
--note records why. --dry-run shows what each decision would do and
writes nothing. --idempotency-key tags the call; a decision is
idempotent whatever it is, so a rerun returns the stored result.

A plugin actor may not decide: it is refused, exit 5.`,
		Example: "  heo operation held " + verb + " op_123\n  heo operation held " + verb + " op_123 op_124 --note \"checked\"\n  heo operation held " + verb + " op_123 --dry-run\n  heo operation held " + verb + " --operation op_123 --operation op_124" + selectorExample(d),
		Args:    cobra.MinimumNArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			sel, selMax, picked, err := heldSelectorOf(cmd, args)
			if err != nil {
				return err
			}
			var ids []string
			if !picked {
				if ids, err = heldIDs(cmd, args); err != nil {
					return err
				}
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			if rev != "" && (len(ids) > 1 || picked) {
				return output.UsageError("--" + flagRevision + " names one operation's revision: pass one <operation>")
			}
			force, _ := cmd.Flags().GetBool(flagForce)
			note, _ := cmd.Flags().GetString(flagDecisionNote)
			action := model.ActionOperationApprove
			if d == desk.DecideReject {
				action = model.ActionOperationReject
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[heldDecideOut], error) {
				var w written[heldDecideOut]
				err := a.desk(ctx, c, action, model.Audience{}, func(s desk.Service) error {
					if picked {
						var err error
						w, err = a.decideSelected(ctx, s, selected{sel: sel, max: selMax, force: force, note: note, key: key, dry: cli.IsDryRun(cmd)})
						return err
					}
					if cli.IsDryRun(cmd) {
						p := &cli.Plan{Args: map[string]any{"operations": ids, "decision": verb, "force": force, "note": note},
							PrerequisitesChecked: []string{"operation", "held", "authorization"}}
						for _, id := range ids {
							h, err := s.GetHeld(ctx, id)
							if err != nil {
								return err
							}
							if h.Status != string(model.Held) {
								return model.NewError(model.Conflict, fmt.Sprintf("operation %s is %s, not held", id, h.Status))
							}
							p.Effects = append(p.Effects, planEffect("update", "operation:"+id, false, fmt.Sprintf(
								"%s %s on %s proposed by %s: %s", verb, h.Kind, h.Target, h.Proposer, effect)))
						}
						w.Plan = p
						return nil
					}
					out := heldDecideOut{Decision: verb, Results: []heldResult{}, OperationKey: key}
					for _, id := range ids {
						dr, err := s.DecideHeld(ctx, desk.DecideRequest{ID: id, Decision: d, Revision: rev, Force: force, Note: note, Correlation: key})
						r := heldResult{Operation: id, OK: err == nil}
						if err != nil {
							e := errmap.Of(err)
							r.Error, r.ErrorCode = e.Message, e.Code
							out.Failed++
						} else {
							r.Status, r.Decision, r.Replayed, r.Correction, r.Revision = dr.Status, dr.Decision, dr.Replayed, dr.Correction, dr.Revision
						}
						out.Results = append(out.Results, r)
					}
					w.Out = out
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			if err := renderDeskWrite(a, cmd, res, heldDecideRows, nil); err != nil || res.Plan != nil {
				return err
			}
			return heldFailure(res.Out)
		},
	}
	c.Flags().StringSlice(flagOperation, nil, "The held operations, in place of <operation>... (for MCP, which cannot pass them); repeatable")
	if d == desk.DecideApprove {
		addSelectorFlags(c)
	}
	c.Flags().String(flagRevision, "", "Revision of the held operation you read, from `held show`; one <operation> only")
	c.Flags().Bool(flagForce, false, "Decide although the target changed since the operation was held")
	c.Flags().String(flagDecisionNote, "", "Why: recorded with the decision")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &heldDecideOut{}, "1.0")
	return c
}

// heldFailure is the exit of a decision: nil when every operation was
// decided. A single operation fails as its own error would; several
// report how many failed, the results having been written.
func heldFailure(o heldDecideOut) error {
	if o.Failed == 0 {
		return nil
	}
	if len(o.Results) == 1 {
		r := o.Results[0]
		e := &output.Error{Code: r.ErrorCode, Message: r.Error}
		e.ExitCode = exitOfCode(r.ErrorCode)
		return e
	}
	names := []string{}
	for _, r := range o.Results {
		if !r.OK {
			names = append(names, r.Operation)
		}
	}
	return fmt.Errorf("%d of %d held operations not decided: %s", o.Failed, len(o.Results), strings.Join(names, ", "))
}

// exitOfCode is the exit code of an envelope code a decision reported.
func exitOfCode(code string) int {
	switch code {
	case output.CodeNotFound:
		return output.ExitNotFound
	case output.CodeConflict:
		return output.ExitConflict
	case output.CodeUnauthorized:
		return output.ExitUnauthorized
	case "VALIDATION", "UNSUPPORTED":
		return output.ExitUsage
	case output.CodeTransient, "UNAVAILABLE":
		return output.ExitTransient
	}
	return output.ExitGeneric
}
