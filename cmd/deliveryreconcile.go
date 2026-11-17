package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// codeOutcomeUnknown is the error code of an outcome that is still
// unknown (errmap's code for uma's outcome_unknown).
const codeOutcomeUnknown = "OUTCOME_UNKNOWN"

// Reconciliation outcomes of `delivery reconcile`.
const (
	// reconcileNotNeeded: the delivery has no unknown outcome to
	// reconcile, so the provider was not asked.
	reconcileNotNeeded = "not_needed"
	// reconcileSettled: the provider's answer settled the delivery.
	reconcileSettled = "settled"
	// reconcileNotApplied: the provider holds no record of the message,
	// so it was not applied: uma sends it again on its own schedule.
	reconcileNotApplied = "not_applied"
	// reconcilePending: the provider's answer proved nothing, and the
	// outcome is still unknown.
	reconcilePending = "pending"
)

// reconcileOut is what `delivery reconcile` reports: whether the
// provider was asked and what became of the delivery. heo reports only
// what uma holds after the adapter's answer; it settles nothing itself.
type reconcileOut struct {
	ID                 model.ID            `json:"id"                            yaml:"id"`
	State              model.DeliveryState `json:"state"                         yaml:"state"`
	PreviousState      model.DeliveryState `json:"previous_state"                yaml:"previous_state"`
	Reconciliation     string              `json:"reconciliation"                yaml:"reconciliation"`
	Adapter            *reconcileAdapter   `json:"adapter,omitempty"             yaml:"adapter,omitempty"`
	ProviderReferences []model.ExternalRef `json:"provider_references,omitempty" yaml:"provider_references,omitempty"`
	Failure            *model.Error        `json:"failure,omitempty"             yaml:"failure,omitempty"`
	NextAttemptAt      *time.Time          `json:"next_attempt_at,omitempty"     yaml:"next_attempt_at,omitempty"`
	Next               string              `json:"next"                          yaml:"next"`
	Revision           model.Revision      `json:"revision"                      yaml:"revision"`
}

// reconcileAdapter is the adapter of the delivery's channel and whether
// it declares reconcile.
type reconcileAdapter struct {
	Type       model.TypeKey `json:"type"                  yaml:"type"`
	Reconciles bool          `json:"reconciles"            yaml:"reconciles"`
	Settle     string        `json:"settle,omitempty"      yaml:"settle,omitempty"`
}

func adapterOf(r *desk.Reconciler) *reconcileAdapter {
	if r == nil {
		return nil
	}
	a := &reconcileAdapter{Type: r.Adapter, Reconciles: r.Reconciles}
	if r.Reconciles && r.Settle > 0 {
		a.Settle = r.Settle.String()
	}
	return a
}

// attemptAt spells the time uma next works on a delivery.
func attemptAt(op model.Operation) string {
	if op.NextAttemptAt == nil {
		return ""
	}
	return stamp(*op.NextAttemptAt)
}

// reconcileOf reports a reconcile of the delivery in p, which left it as
// after.
func (a *app) reconcileOf(p desk.ReconcilePlan, after model.Operation) reconcileOut {
	out := reconcileOut{
		ID: after.Ref.ID, State: after.State, PreviousState: p.Operation.State, Adapter: adapterOf(p.Reconciler),
		ProviderReferences: after.ProviderReferences, Failure: after.Failure, NextAttemptAt: after.NextAttemptAt,
		Revision: after.Revision,
	}
	id := string(after.Ref.ID)
	switch {
	case !p.Runs():
		out.Reconciliation = reconcileNotNeeded
		out.Next = a.advice(after).Next
	case desk.VerdictOf(after.State) == desk.VerdictReconcile:
		out.Reconciliation = reconcilePending
		out.Next = "the provider's answer proved nothing, so uma changed nothing"
		if out.Adapter != nil && out.Adapter.Settle != "" {
			out.Next += ": within " + out.Adapter.Settle + " of an attempt, a message the lookup does not find may " +
				"still have been applied"
		}
		if at := attemptAt(after); at != "" {
			out.Next += "; uma asks again at " + at + ", and `" + a.heoCommand("delivery reconcile "+id) + "` asks now"
		} else {
			out.Next += "; uma has no attempt scheduled, so run `" + a.heoCommand("delivery reconcile "+id) + "` again later"
		}
		out.Next += ". Until it settles it stays outcome_unknown and `delivery retry` refuses it"
	case after.State == model.RetryPending:
		out.Reconciliation = reconcileNotApplied
		out.Next = "the provider holds no record of it, so it was not applied: uma sends it again"
		if at := attemptAt(after); at != "" {
			out.Next += " at " + at
		}
		out.Next += "; nothing to retry by hand (`" + a.heoCommand("delivery get "+id) + "` follows it)"
	default:
		out.Reconciliation = reconcileSettled
		out.Next = a.advice(after).Next
	}
	return out
}

// reconcilePlan renders a reconcile for --dry-run: what would be asked,
// and of whom. It asks the provider nothing.
func (a *app) reconcilePlan(p desk.ReconcilePlan) *cli.Plan {
	op := p.Operation
	plan := &cli.Plan{
		Args:                 map[string]any{"operation": op.Ref.ID, "state": op.State},
		Effects:              []cli.Effect{},
		PrerequisitesChecked: []string{"authorization", "operation"},
	}
	if at := attemptAt(op); at != "" {
		plan.Args["next_attempt_at"] = at
	}
	switch {
	case !p.Runs():
		plan.Warnings = []string{fmt.Sprintf("nothing to reconcile: the delivery is %s, so the provider is not asked "+
			"(%s)", op.State, a.advice(op).Next)}
	case p.Reconciler != nil && !p.Reconciler.Reconciles:
		plan.Warnings = []string{fmt.Sprintf("%s declares no reconcile: uma would refuse (UNSUPPORTED) and nothing would "+
			"be asked; look for the message at the provider%s", p.Reconciler.Adapter, references(op))}
	default:
		who := "the channel's adapter"
		if p.Reconciler != nil {
			who = string(p.Reconciler.Adapter)
			plan.Args["adapter"] = who
			if p.Reconciler.Settle > 0 {
				plan.Warnings = []string{fmt.Sprintf("within %s of an attempt, a message the lookup does not find settles nothing",
					p.Reconciler.Settle)}
			}
		}
		plan.PrerequisitesChecked = append(plan.PrerequisitesChecked, "state outcome_unknown")
		plan.Effects = append(plan.Effects, planEffect("update", "operation:"+string(op.Ref.ID), false,
			"ask "+who+" now, at its provider, what became of the unresolved attempt (its sent copy, audit trail "+
				"or echo): an applied message settles the delivery, one failed for good fails it, none found past "+
				"the settle window makes it retry_pending for uma to send again, and an answer that proves nothing "+
				"changes nothing; nothing is sent"))
	}
	return plan
}

func reconcileRows(o reconcileOut) [][2]string {
	rows := [][2]string{{"id", string(o.ID)}, {"state", string(o.State)}}
	if o.PreviousState != o.State {
		rows = append(rows, [2]string{"was", string(o.PreviousState)})
	}
	rows = append(rows, [2]string{"reconciliation", o.Reconciliation})
	if o.Adapter != nil {
		rows = append(rows, [2]string{"adapter", string(o.Adapter.Type) + map[bool]string{
			true: " (declares reconcile)", false: " (declares no reconcile)"}[o.Adapter.Reconciles]})
	}
	refs := make([]string, 0, len(o.ProviderReferences))
	for _, r := range o.ProviderReferences {
		refs = append(refs, routeName(r))
	}
	rows = append(rows, [2]string{"provider refs", orDash(strings.Join(refs, ", "))})
	if f := o.Failure; f != nil {
		rows = append(rows, [2]string{"failure", f.Error()})
	}
	if o.NextAttemptAt != nil {
		rows = append(rows, [2]string{"next attempt", stamp(*o.NextAttemptAt)})
	}
	return append(rows, [2]string{"next", o.Next}, [2]string{"revision", string(o.Revision)})
}

// reconcileFailure explains a reconcile uma refused or could not make,
// from uma's own error: an adapter that cannot reconcile, a delivery a
// worker holds, a provider that could not be asked.
func (a *app) reconcileFailure(err error, id string, p desk.ReconcilePlan) error {
	switch {
	case err == nil:
		return nil
	case model.IsCode(err, model.Unsupported):
		e := errmap.Of(err)
		e.SuggestedFix = "nothing will reconcile it"
		if p.Reconciler != nil {
			e.SuggestedFix += ": " + string(p.Reconciler.Adapter) + " declares no reconcile, or the channel's configuration leaves it off (a relay with no lookup URL)"
		}
		e.SuggestedFix += "; look for the message at the provider" + references(p.Operation) +
			", and only if it never arrived send it again as a new operation under a new --idempotency-key"
		return e
	case model.IsCode(err, model.Conflict):
		e := errmap.Of(err)
		e.SuggestedFix = "a worker is carrying it out, or it moved on: `" + a.heoCommand("delivery get "+id) +
			"` shows its state; reconcile again in a moment if it is still outcome_unknown"
		return e
	}
	if e := errmap.Of(err); e.Transience == output.TransienceTransient {
		e.SuggestedFix = "the provider could not be asked and nothing was recorded: the delivery is unchanged; " +
			"reconcile again later"
		return e
	}
	return err
}

func (a *app) deliveryReconcileCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reconcile <operation>",
		Short: "Ask the provider now what became of an unknown outcome",
		Long: `Reconcile the delivery of the operation <operation>: ask its channel's
adapter, now, what became of the attempt whose outcome is unknown, and
report the delivery as uma holds it afterwards. An outcome_unknown
delivery is one the provider may already have: a connection dropped
after the request went out, say. uma's workers ask on their own
schedule (next at the time shown as next attempt); this does not wait
for it. The adapter looks the message up at its provider — its sent
copy, audit trail or echo — and only its answer changes the delivery;
heo and uma never infer an outcome:

  applied          settled: provider_accepted or delivered, with the
                   provider's references; exit 0
  failed for good  settled: failed or rejected; exit 0. Send it again
                   as a new operation under a new --idempotency-key
  no record, past  not applied: retry_pending, which uma sends again at
  the adapter's    the next attempt shown; exit 0. Nothing is sent by
  settle window    this command
  proves nothing   such as no record inside the settle window: nothing
                   changes and the delivery stays outcome_unknown, with
                   the next attempt shown; exit 8 (OUTCOME_UNKNOWN):
                   the outcome is still unknown, never success
  cannot be asked  an unreachable provider: the error (exit 6 when
                   transient) and nothing is recorded

The provider is asked for outcome_unknown deliveries only. Any other
state is reported as it is (reconciliation not_needed, exit 0, with what
to do next): queued, retry_pending and dead_lettered belong to uma's
workers or to ` + "`delivery retry`" + `. An adapter that declares no reconcile is
refused by uma (UNSUPPORTED, exit 2): nothing will reconcile the
delivery, and the next step is the provider. A delivery a worker is
carrying out right now is CONFLICT (exit 4): reconcile again in a
moment. Reconciling is idempotent: repeating it, or running it after
uma settled the delivery, is safe and asks the provider nothing more.

--dry-run shows what would be asked and of which adapter, and asks
nothing. Desk and operator roles: reconciling spends a provider
call, so the viewer and external roles may not.`,
		Example: `  heo delivery reconcile op_123 --dry-run
  heo delivery reconcile op_123
  heo delivery reconcile op_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A still-unknown outcome renders the report and then fails as
			// OUTCOME_UNKNOWN: forwarded, the report comes back for this
			// process to render and judge.
			res, err := daemonRunReport(a, cmd, args, codeOutcomeUnknown, func(ctx context.Context, c control.Caller) (written[reconcileOut], error) {
				var w written[reconcileOut]
				err := a.desk(ctx, c, model.ActionOperationReconcile, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanReconcile(ctx, args[0], planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						// A channel that cannot reconcile is refused as a
						// submission is: the same error, wording and exit.
						if p.Refusal != nil {
							return a.reconcileFailure(p.Refusal, args[0], p)
						}
						w.Plan = a.reconcilePlan(p)
						return nil
					}
					after, err := s.Reconcile(ctx, p)
					if err != nil {
						return a.reconcileFailure(err, args[0], p)
					}
					w.Out = a.reconcileOf(p, after)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			if err := res.render(a, cmd, reconcileRows, nil); err != nil || res.Plan != nil || res.Out.Reconciliation != reconcilePending {
				return err
			}
			// The report is out; the outcome is still unknown, which is
			// never success.
			e := errmap.Of(model.NewError(model.OutcomeUnknown, fmt.Sprintf(
				"delivery %s is still outcome_unknown: the provider's answer proved nothing", res.Out.ID)))
			e.SuggestedFix = "see `next`: uma asks again on its schedule, or reconcile again later"
			return e
		},
	}
	daemonCommand(c)
	// It changes the delivery's state in the shared store, and asks a
	// provider: not a read. It claims no operation key (uma gives it
	// none), and repeating it is safe.
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &reconcileOut{}, "1.0")
	return c
}
