package cmd

import (
	"context"
	"errors"
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

// deliveryOut is a delivery: the operation uma delivers, its state, its
// failure with uma's retry advice, and what heo advises doing next.
type deliveryOut struct {
	ID                 model.ID            `json:"id"                            yaml:"id"`
	State              model.DeliveryState `json:"state"                         yaml:"state"`
	Result             *model.Ref          `json:"result,omitempty"              yaml:"result,omitempty"`
	Message            *model.MessageRef   `json:"message,omitempty"             yaml:"message,omitempty"`
	ProviderReferences []model.ExternalRef `json:"provider_references,omitempty" yaml:"provider_references,omitempty"`
	Failure            *model.Error        `json:"failure,omitempty"             yaml:"failure,omitempty"`
	NextAttemptAt      *time.Time          `json:"next_attempt_at,omitempty"     yaml:"next_attempt_at,omitempty"`
	UpdatedAt          time.Time           `json:"updated_at"                    yaml:"updated_at"`
	Advice             deliveryAdvice      `json:"advice"                        yaml:"advice"`
	Revision           model.Revision      `json:"revision"                      yaml:"revision"`
}

// deliveryAdvice is what a delivery's state allows: whether `delivery
// retry` replays it, and what to do now.
type deliveryAdvice struct {
	Verdict desk.Verdict `json:"verdict" yaml:"verdict"`
	Retry   bool         `json:"retry"   yaml:"retry"`
	Next    string       `json:"next"    yaml:"next"`
}

// duplicateNote is what heo says wherever a delivery may go out again:
// delivery is at least once, and a provider may not deduplicate.
const duplicateNote = "if an earlier attempt reached the provider, it may receive the message twice"

// advice is what to do about op, spelled for this profile.
func (a *app) advice(op model.Operation) deliveryAdvice {
	id := string(op.Ref.ID)
	v := desk.VerdictOf(op.State)
	out := deliveryAdvice{Verdict: v, Retry: v == desk.VerdictRetry}
	switch v {
	case desk.VerdictRetry:
		out.Next = "its work was quarantined after attempts that never finished; once the cause is fixed, `" +
			a.heoCommand("delivery retry "+id) + "` queues it again (operator role); " + duplicateNote
		if op.Failure != nil && op.Failure.Code == model.OutcomeUnknown {
			out.Next += "; an unfinished attempt is reconciled before any new one"
		}
	case desk.VerdictReconcile:
		out.Next = "the provider may already have it, so it is never retried blindly: `" +
			a.heoCommand("delivery reconcile "+id) + "` asks its adapter now"
		if at := attemptAt(op); at != "" {
			out.Next += " (uma asks on its own at " + at + ")"
		}
		out.Next += "; if the adapter cannot, look for it at the provider" + references(op) +
			", and only if it never arrived send it again as a new operation under a new --idempotency-key"
	case desk.VerdictResubmit:
		out.Next = "final: uma never runs it again and `delivery retry` refuses it; fix the cause" + failureCause(op) +
			", then send it again as a new operation under a new --idempotency-key"
	case desk.VerdictDone:
		out.Next = "nothing to do: " + string(op.State)
	default:
		out.Next = "in progress: the daemon's workers run it"
		if op.State == model.RetryPending {
			out.Next = "an attempt failed without applying it; uma retries it on its own" + retryAfter(op)
			if at := attemptAt(op); at != "" {
				out.Next += ", next at " + at
			}
		}
		out.Next += "; nothing to retry by hand"
	}
	return out
}

func references(op model.Operation) string {
	if len(op.ProviderReferences) == 0 {
		return ""
	}
	refs := make([]string, 0, len(op.ProviderReferences))
	for _, r := range op.ProviderReferences {
		refs = append(refs, routeName(r))
	}
	return " (references: " + strings.Join(refs, ", ") + ")"
}

func failureCause(op model.Operation) string {
	if op.Failure == nil {
		return ""
	}
	return " (" + op.Failure.Error() + ")"
}

func retryAfter(op model.Operation) string {
	if op.Failure == nil || op.Failure.RetryAfter <= 0 {
		return ""
	}
	return ", not before " + op.Failure.RetryAfter.String()
}

func (a *app) deliveryOf(op model.Operation) deliveryOut {
	return deliveryOut{
		ID: op.Ref.ID, State: op.State, Result: op.Result, Message: op.ResultMessage,
		ProviderReferences: op.ProviderReferences, Failure: op.Failure, NextAttemptAt: op.NextAttemptAt, UpdatedAt: op.UpdatedAt,
		Advice: a.advice(op), Revision: op.Revision,
	}
}

func deliveryRows(d deliveryOut) [][2]string {
	rows := [][2]string{{"id", string(d.ID)}, {"state", string(d.State)}}
	if d.Message != nil {
		rows = append(rows, [2]string{"message", string(d.Message.ID)},
			[2]string{"conversation", string(d.Message.Conversation.ID)})
	} else if d.Result != nil {
		rows = append(rows, [2]string{"result", string(d.Result.Kind) + " " + string(d.Result.ID)})
	}
	refs := make([]string, 0, len(d.ProviderReferences))
	for _, r := range d.ProviderReferences {
		refs = append(refs, routeName(r))
	}
	rows = append(rows, [2]string{"provider refs", orDash(strings.Join(refs, ", "))})
	if f := d.Failure; f != nil {
		rows = append(rows, [2]string{"failure", f.Error()}, [2]string{"failure retry", string(f.Retry)})
		if f.RetryAfter > 0 {
			rows = append(rows, [2]string{"retry after", f.RetryAfter.Round(time.Millisecond).String()})
		}
	}
	if d.NextAttemptAt != nil {
		rows = append(rows, [2]string{"next attempt", stamp(*d.NextAttemptAt)})
	}
	return append(rows,
		[2]string{"retry", map[bool]string{true: "yes", false: "no"}[d.Advice.Retry]},
		[2]string{"next", d.Advice.Next},
		[2]string{"revision", string(d.Revision)},
	)
}

func (a *app) deliveryCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "delivery",
		Short: "List and inspect deliveries, reconcile unknown ones, retry dead-lettered ones",
		Long: `Inspect what happened to an outbound operation — a reply, a note the
provider receives, a conversation start — and retry one only when that
is safe. ` + "`delivery list`" + ` lists the ones you submitted by state, inbox,
conversation and time; ` + "`delivery reconcile`" + ` asks the provider now what
became of an outcome_unknown one.

A delivery is a uma operation, named by the operation ID a write
printed. Its state is one of queued, retry_pending, provider_accepted,
delivered, failed, rejected, outcome_unknown, dead_lettered or
committed; a failure carries uma's retry advice.

` + "`delivery retry`" + ` replays a dead-lettered delivery alone. An
outcome_unknown one is refused (exit 8): the provider may already have
it, so it is reconciled first (uma's workers do that on their own
schedule, shown as next attempt; ` + "`delivery reconcile`" + ` does it now). A failed one is final: send it again as
a new operation, under a new --idempotency-key. Delivery is at least
once: a delivery sent again may reach the provider twice.

Every command runs in the profile's daemon (none running exits 70).
Reading and reconciling are the desk and operator roles'; retrying is the
operator's.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.deliveryListCmd(), a.deliveryGetCmd(), a.deliveryRetryCmd(), a.deliveryReconcileCmd())
	return grp
}

func (a *app) deliveryGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <operation>",
		Short: "Show a delivery's state, failure and retry advice",
		Long: `Show the delivery of the operation <operation> (the operation ID a
write printed): its state, what it produced, the provider references
known so far, its failure with uma's retry advice, and what to do next —
whether ` + "`delivery retry`" + ` may replay it. No such operation, or one
you may not read, exits 3. Read-only.`,
		Example: `  heo delivery get op_123
  heo delivery get op_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (deliveryOut, error) {
				var out deliveryOut
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) error {
					op, err := s.GetOperation(ctx, args[0])
					out = a.deliveryOf(op)
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
			return writeRows(cmd.OutOrStdout(), deliveryRows(res))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &deliveryOut{}, Version: "1.0"})
	return c
}

// retryOut is what a retry reports: the delivery as it is after the
// replay — queued, never delivered, when the command returns — and the
// replay operation that queued it.
type retryOut struct {
	Delivery     deliveryOut   `json:"delivery"      yaml:"delivery"`
	Replay       operationView `json:"replay"        yaml:"replay"`
	OperationKey string        `json:"operation_key" yaml:"operation_key"`
}

func retryRows(o retryOut) [][2]string {
	return append(deliveryRows(o.Delivery), [][2]string{
		{"replay", fmt.Sprintf("%s (%s%s)", o.Replay.ID, o.Replay.State, map[bool]string{true: ", replayed"}[o.Replay.Replayed])},
		{"operation key", o.OperationKey},
	}...)
}

// retryPlan renders a retry plan for --dry-run.
func (a *app) retryPlan(p desk.RetryPlan, key string) *cli.Plan {
	op := p.Operation
	args := map[string]any{"operation": op.Ref.ID, "state": op.State}
	if op.Failure != nil {
		args["failure"] = op.Failure.Error()
	}
	if key != "" {
		args["operation_key"] = key
	}
	effect := planEffect("update", "operation:"+string(op.Ref.ID), false,
		"replay: queue the dead-lettered delivery again for the daemon's workers; "+duplicateNote)
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: []string{"authorization", "operation", "state dead_lettered"}}
}

// retryFailure is a retry the safety check refused, with what to do
// instead.
func (a *app) retryFailure(err error) error {
	var r *desk.RetryRefusal
	if !errors.As(err, &r) {
		return err
	}
	e := errmap.Of(err)
	e.SuggestedFix = a.advice(r.Operation).Next
	return e
}

func (a *app) deliveryRetryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "retry <operation>",
		Short: "Queue a dead-lettered delivery again, after a safety check",
		Long: `Queue the delivery of the operation <operation> again, once a safety
check passed: it must be dead_lettered — quarantined after attempts that
never finished. uma then runs it again as its originating actor,
reauthorizing it first. Delivery is at least once: if an earlier attempt
reached the provider, it may receive the message twice.

Any other state is refused before anything is submitted:
outcome_unknown exits 8 (OUTCOME_UNKNOWN), naming how to reconcile it,
since the provider may already have it; failed and rejected are final
(exit 4: send it again as a new operation, under a new
--idempotency-key); queued, retry_pending and done ones have nothing to
retry (exit 4).

--dry-run checks and shows the replay, and submits nothing. Committed
as a uma operation under --idempotency-key: rerunning with the same key
returns the same replay and queues nothing twice; without one, heo
mints a key and prints it. Operator role.`,
		Example: `  heo delivery retry op_123 --dry-run
  heo delivery retry op_123 --idempotency-key retry-42`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[retryOut], error) {
				var w written[retryOut]
				err := a.desk(ctx, c, model.ActionOperationReplay, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanRetry(ctx, args[0], opID(key))
					if err != nil {
						return a.retryFailure(err)
					}
					if cli.IsDryRun(cmd) {
						w.Plan = a.retryPlan(p, key)
						return nil
					}
					acc, cur, err := s.Retry(ctx, p)
					if err != nil {
						return err
					}
					w.Out = retryOut{Delivery: a.deliveryOf(cur), Replay: operationOf(acc), OperationKey: key}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			var notes []string
			if res.Plan == nil {
				notes = []string{fmt.Sprintf("queued again: the daemon's workers deliver it next (delivery %s); %s",
					res.Out.Delivery.ID, duplicateNote)}
			}
			return res.render(a, cmd, retryRows, notes)
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &retryOut{}, Version: "1.0"})
	return c
}
