package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/heo/internal/trustcfg"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// selectorExample is the selector examples of approve's help.
func selectorExample(d desk.Decision) string {
	if d != desk.DecideApprove {
		return ""
	}
	return "\n  heo operation held approve --class trash --inbox support --dry-run" +
		"\n  heo operation held approve --class move_to --inbox support --proposer plugin:triage --note \"sorted\""
}

// addSelectorFlags adds the selector of a batch approval.
func addSelectorFlags(c *cobra.Command) {
	c.Flags().String(flagClass, "", "Selector: approve the held operations of this verb class, such as trash or move_to; needs --"+flagInbox)
	c.Flags().String(flagInbox, "", "Selector: only operations on this mailbox (inbox key or ID); needs --"+flagClass)
	c.Flags().String(flagProposer, "", "Selector: only operations this plugin actor proposed, such as plugin:triage")
	c.Flags().Int(flagMax, desk.DefaultBatch, fmt.Sprintf("Selector: approve at most this many; more matches are refused, not truncated (at most %d)", desk.MaxBatch))
}

// heldSelectorOf reads the selector of an approval. picked is false when
// none of its flags is set: the operations are named. A selector is
// never mixed with named operations or an --operation list, and needs
// both a class and a mailbox: an empty selector is refused here, before
// the daemon, and again by desk.
func heldSelectorOf(cmd *cobra.Command, args []string) (sel desk.HeldSelector, max int, picked bool, err error) {
	if cmd.Flags().Lookup(flagClass) == nil {
		return sel, 0, false, nil
	}
	sel.Class, _ = cmd.Flags().GetString(flagClass)
	sel.Inbox, _ = cmd.Flags().GetString(flagInbox)
	sel.Proposer, _ = cmd.Flags().GetString(flagProposer)
	max, _ = cmd.Flags().GetInt(flagMax)
	ops, _ := cmd.Flags().GetStringSlice(flagOperation)
	picked = sel != desk.HeldSelector{} || cmd.Flags().Changed(flagMax)
	switch {
	case !picked:
		return sel, max, false, nil
	case len(args) > 0 || len(ops) > 0:
		return sel, max, true, output.UsageError("<operation> and a selector (--" + flagClass + ", --" + flagInbox + ", --" + flagProposer + ") both name the operations: pass one")
	}
	if verr := sel.Validate(); verr != nil {
		return sel, max, true, output.UsageError("--" + flagClass + " and --" + flagInbox + " are both required: " + errmap.Of(verr).Message)
	}
	return sel, max, true, nil
}

// selected is a selector run of an approval.
type selected struct {
	sel   desk.HeldSelector
	max   int
	force bool
	note  string
	key   string
	dry   bool
}

// decideSelected approves the operations the selector matches, all under
// one batch id. A match past the cap is refused, nothing approved.
func (a *app) decideSelected(ctx context.Context, s desk.Service, r selected) (written[heldDecideOut], error) {
	var w written[heldDecideOut]
	cfg, err := trustcfg.Load(a.r.Viper)
	if err != nil {
		return w, err
	}
	rules := desk.BatchRules{ApprovalAlways: cfg.ApprovalAlways, Unattended: cfg.Unattended}
	got, err := s.SelectHeld(ctx, r.sel, rules, r.max)
	if err != nil {
		return w, err
	}
	skipped := make([]string, len(got.Skipped))
	for i, k := range got.Skipped {
		skipped[i] = k.Operation + ": " + k.Why
	}
	if r.dry {
		ids := make([]string, len(got.Items))
		for i, h := range got.Items {
			ids[i] = h.ID
		}
		args := map[string]any{"operations": ids, "decision": "approve", "class": r.sel.Class, "inbox": r.sel.Inbox,
			"max": r.max, "force": r.force, "note": r.note, "skipped": skipped, "more": got.More}
		if r.sel.Proposer != "" {
			args["proposer"] = r.sel.Proposer
		}
		p := &cli.Plan{Args: args, PrerequisitesChecked: []string{"selector", "held", "authorization", "always-held rules"}}
		for _, h := range got.Items {
			p.Effects = append(p.Effects, planEffect("update", "operation:"+h.ID, false, fmt.Sprintf(
				"approve %s on %s proposed by %s: queues the held operation as proposed", h.Kind, h.Target, h.Proposer)))
		}
		w.Plan = p
		return w, nil
	}
	if got.More {
		return w, model.NewError(model.Validation, fmt.Sprintf(
			"more than --%s %d held operations match: nothing approved. Narrow the selector (--%s) or raise --%s (at most %d); --dry-run lists the matches",
			flagMax, r.max, flagProposer, flagMax, desk.MaxBatch))
	}
	out := heldDecideOut{Decision: "approve", Results: []heldResult{}, OperationKey: r.key, Skipped: got.Skipped}
	if len(got.Items) > 0 {
		out.Batch = desk.NewBatchID()
	}
	for _, h := range got.Items {
		// The revision the selector listed: an operation that changed
		// since is a conflict, reported, never approved as it is now.
		dr, err := s.DecideHeld(ctx, desk.DecideRequest{ID: h.ID, Decision: desk.DecideApprove, Revision: model.Revision(h.Revision),
			Force: r.force, Note: r.note, Correlation: r.key, Batch: out.Batch})
		res := heldResult{Operation: h.ID, OK: err == nil}
		if err != nil {
			e := errmap.Of(err)
			res.Error, res.ErrorCode = e.Message, e.Code
			out.Failed++
		} else {
			res.Status, res.Decision, res.Replayed, res.Correction, res.Revision = dr.Status, dr.Decision, dr.Replayed, dr.Correction, dr.Revision
		}
		out.Results = append(out.Results, res)
	}
	w.Out = out
	return w, nil
}
