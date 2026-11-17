package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/errmap"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/plugins"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

const (
	flagEvent  = "event"
	flagPlugin = "plugin"
	// stateAll lists dead letters of every state.
	stateAll = "all"
)

// hookCmd builds the `hook` group: the hook runner's plugins, their
// dead letters, and the verbs that test and replay them. Every verb runs
// in the daemon, where the plugins' processes, tokens and actors are.
func (a *app) hookCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "hook",
		Short: "List, test and replay hook plugins and manage their dead letters",
		Long: `Inspect and operate the hook runner (services.hooks, plugins.*). The
verbs run in the daemon, where the plugins' processes, tokens and actors
are, and need the operator role (hook list, hook test and
hook dead-letter list and hook job get read; replay, retry and drop change state).

  hook list                      each plugin's state, cursor and dead letters
  hook test <plugin> --event ID  run a plugin on one stored event, apply nothing
  hook dead-letter list          events a plugin did not handle
  hook replay <plugin> --from C  queue a replay of the log from a position
  hook dead-letter retry|drop    queue a retry of one, or give it up
  hook job get <id>              the state and outcome of a queued job

See the hooks manual page for the runner, the result a plugin returns and
how failures are retried and dead-lettered.`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(grp)
	dl := &cobra.Command{
		Use:   "dead-letter",
		Short: "List, retry and drop the events a plugin did not handle",
		Long: `A dead letter is an event a plugin failed for good (exit 2) or failed
until its attempts ran out; the plugin's subscription moved on. The record
holds references only. list shows them, retry queues running the event through the
plugin again, drop gives one up.`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(dl)
	dl.AddCommand(a.hookDeadListCmd(), a.hookDeadRetryCmd(), a.hookDeadDropCmd())
	grp.AddCommand(a.hookListCmd(), a.hookTestCmd(), a.hookReplayCmd(), a.hookJobCmd(), dl)
	return grp
}

// hookLedger returns the daemon's dead-letter ledger once the policy
// permits the caller action, or the reason it cannot be reached.
func (a *app) hookLedger(c control.Caller, action string) (*hooks.Ledger, error) {
	pol, err := a.authorization()
	if err != nil {
		return nil, errmap.Of(err)
	}
	if err := permitted(pol, c, action, model.Audience{}); err != nil {
		return nil, err
	}
	l := a.runtime.Ledger()
	if l == nil {
		e := output.PrerequisiteError("the hooks service is not running: there is no dead-letter store to act on")
		e.SuggestedFix = "enable services.hooks and start the daemon: `" + a.heoCommand("service start") + "`"
		return nil, e
	}
	return l, nil
}

// hookFailure maps the errors of the ledger onto heo's.
func (a *app) hookFailure(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hooks.ErrNoDeadLetter):
		e := output.NotFoundError(what + ": no such dead letter")
		e.SuggestedFix = "`" + a.heoCommand("hook dead-letter list --state all") + "` shows them"
		return e
	case model.IsCode(err, model.NotFound):
		return errmap.Of(err)
	}
	return output.GenericError(err.Error())
}

// --- hook list ---------------------------------------------------------

// hookRow is one configured plugin: its settings, where its
// subscription stands and how many dead letters it holds.
type hookRow struct {
	Plugin string `json:"plugin" yaml:"plugin"`
	Mode   string `json:"mode" yaml:"mode"`
	// State is the runner's: starting, ready or degraded; not_running
	// when the hooks service holds no runner for the plugin.
	State  string   `json:"state" yaml:"state"`
	DryRun bool     `json:"dry_run" yaml:"dry_run"`
	Kinds  []string `json:"kinds" yaml:"kinds"`
	// Cursor is the subscription's last acknowledged position: a --from
	// of a replay would continue the log there.
	Cursor       string `json:"cursor,omitempty" yaml:"cursor,omitempty"`
	Lag          string `json:"lag,omitempty" yaml:"lag,omitempty"`
	Delivered    uint64 `json:"delivered" yaml:"delivered"`
	DeadLettered uint64 `json:"dead_lettered" yaml:"dead_lettered"`
	// Dead is the dead letters waiting, from the store; unset while the
	// store cannot be read.
	Dead      *int             `json:"dead,omitempty" yaml:"dead,omitempty"`
	LastError *hooks.LastError `json:"last_error,omitempty" yaml:"last_error,omitempty"`
}

const stateNotRunning = "not_running"

func (a *app) hookListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the hook plugins with their state, cursor and dead letters",
		Long: `List each configured plugin (plugins.*) beside what the hooks service
holds for it: the runner's state, its mode, whether it only records what
it would apply (dry_run), the event kinds it subscribes to, the cursor of
its subscription and how far behind the log it is, the events it
delivered and dead-lettered since the service started, and the dead
letters waiting in the store (` + "`hook dead-letter list`" + `). A plugin the service
holds no runner for (it failed to start: no token minted, say, see
` + "`service status`" + `) is not_running.

Operator role.`,
		Example: `  heo hook list
  heo hook list --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) ([]hookRow, error) {
				return a.hookRows(ctx, c)
			})
			if err != nil {
				return err
			}
			return output.Dispatch(cmd, a.r.Viper, rows)
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &[]hookRow{}, Version: "1.0"})
	return c
}

func (a *app) hookRows(ctx context.Context, c control.Caller) ([]hookRow, error) {
	pol, err := a.authorization()
	if err != nil {
		return nil, errmap.Of(err)
	}
	if err := permitted(pol, c, authz.ActionHookRead, model.Audience{}); err != nil {
		return nil, err
	}
	cfg, _ := plugins.Read(a.viper())
	live := map[string]hooks.Status{}
	for _, s := range a.runtime.Status().Hooks {
		live[s.Plugin] = s
	}
	var dead map[string]int
	if l := a.runtime.Ledger(); l != nil {
		if recs, err := l.List(ctx, "", hooks.StateDead); err == nil {
			dead = map[string]int{}
			for _, r := range recs {
				dead[r.Plugin]++
			}
		}
	}
	rows := make([]hookRow, 0, len(cfg))
	for _, p := range cfg {
		r := hookRow{Plugin: p.Name, Mode: p.Mode, State: stateNotRunning, DryRun: p.DryRun, Kinds: p.Filter.Kinds}
		if r.Kinds == nil {
			r.Kinds = []string{}
		}
		if s, ok := live[p.Name]; ok {
			r.State, r.Cursor = string(s.State), string(s.Cursor)
			r.Delivered, r.DeadLettered, r.LastError = s.Delivered, s.DeadLettered, s.LastError
			if s.Lag > 0 {
				r.Lag = s.Lag.Round(1e9).String()
			}
		}
		if dead != nil {
			n := dead[p.Name]
			r.Dead = &n
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// --- hook test ---------------------------------------------------------

func (a *app) hookTestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "test <plugin>",
		Short: "Run a plugin on one stored event and show its result, applying nothing",
		Long: `Read the event --event from uma as the plugin's own actor, run the
running plugin <plugin> on it once (its process, in its own directory, as
for a delivery) and print what it answered, without applying anything:
no operation runs, no dead letter and no attempt count is written, and
the plugin's subscription does not move.

The report lists each operation as it would be applied: its verb,
arguments, flags, the idempotency key it would run under, and whether the
check the applier makes before running anything would refuse it (an
unknown verb, one that runs at the terminal only or hosts itself, one
that cannot be replayed, a flag the verb lacks). One refused operation
rejects the whole result, as in a delivery (would_apply false).
Authorization is decided only when an operation runs, so a passing check
is not a promise the plugin's actor may do it.

An event the plugin would not be delivered (its actor may not read the
audience, or it is outside its filter) is reported skipped and the
plugin is not run. The exit code is 0 only when delivering the event
would succeed: the plugin answered and its result would be applied.

The plugin's process runs with the access it always has: a plugin that
acts on its own, outside heo, acts here too. Operator role.`,
		Example: `  heo hook test triage --event ev_123
  heo hook test triage --event ev_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ev, _ := cmd.Flags().GetString(flagEvent)
			// A test that would not succeed renders its report and then
			// fails: forwarded, the report comes back for this process to
			// render and judge.
			rep, err := daemonRunReport(a, cmd, args, output.CodeGeneric, func(ctx context.Context, c control.Caller) (hooks.TestReport, error) {
				l, err := a.hookLedger(c, authz.ActionHookRead)
				if err != nil {
					return hooks.TestReport{}, err
				}
				rep, err := l.Test(ctx, args[0], ev)
				return rep, a.hookFailure(err, args[0])
			})
			if err != nil {
				return err
			}
			if err := a.renderHookTest(cmd, rep); err != nil {
				return err
			}
			if why := testVerdict(rep); why != "" {
				return output.GenericError(why)
			}
			return nil
		},
	}
	c.Flags().String(flagEvent, "", "ID of the stored event to run the plugin on (required)")
	_ = c.MarkFlagRequired(flagEvent)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &hooks.TestReport{}, Version: "1.0"})
	return c
}

// testVerdict says why delivering the tested event would not succeed,
// empty when it would.
func testVerdict(r hooks.TestReport) string {
	switch {
	case !r.Admitted:
		return fmt.Sprintf("%s would not be delivered event %s: %s", r.Plugin, r.Event, r.Skipped)
	case !r.Ok:
		return fmt.Sprintf("%s failed on event %s (%s): %s", r.Plugin, r.Event, r.Failure.Class, r.Failure.Message)
	case !r.WouldApply:
		return fmt.Sprintf("%s's result for event %s would be rejected: %s", r.Plugin, r.Event, r.Rejected)
	}
	return ""
}

func (a *app) renderHookTest(cmd *cobra.Command, r hooks.TestReport) error {
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, r)
	}
	rows := [][2]string{{"plugin", r.Plugin}, {"event", r.Event + " (" + r.Kind + ")"}, {"admitted", strconv.FormatBool(r.Admitted)}}
	if r.Skipped != "" {
		rows = append(rows, [2]string{"skipped", r.Skipped})
	}
	if r.Failure != nil {
		rows = append(rows, [2]string{"failure", string(r.Failure.Class) + ": " + r.Failure.Message})
	}
	if r.Message != "" {
		rows = append(rows, [2]string{"message", r.Message})
	}
	if r.Admitted && r.Ok {
		rows = append(rows, [2]string{"would apply", strconv.FormatBool(r.WouldApply)}, [2]string{"applied", "false"})
	}
	if r.Rejected != "" {
		rows = append(rows, [2]string{"rejected", r.Rejected})
	}
	for _, n := range r.Notes {
		rows = append(rows, [2]string{"note", n})
	}
	for _, o := range r.Operations {
		v := o.Verb + " " + strings.Join(o.Args, " ")
		if !o.Allowed {
			v += "  REFUSED: " + o.Refusal
		}
		rows = append(rows, [2]string{"operation " + strconv.Itoa(o.Index), strings.TrimSpace(v) + "  key " + o.Key})
	}
	return writeRows(cmd.OutOrStdout(), rows)
}

// --- hook dead-letter --------------------------------------------------

// deadRow is a dead letter in a table; --format json carries the whole
// record.
type deadRow struct {
	Plugin   string `json:"plugin" yaml:"plugin"`
	Event    string `json:"event" yaml:"event"`
	Kind     string `json:"kind" yaml:"kind"`
	State    string `json:"state" yaml:"state"`
	Attempts int    `json:"attempts" yaml:"attempts"`
	Class    string `json:"class" yaml:"class"`
	Last     string `json:"last_attempt" yaml:"last_attempt"`
	Message  string `json:"message" yaml:"message"`
}

func deadRowOf(r hooks.DeadRecord) deadRow {
	return deadRow{Plugin: r.Plugin, Event: r.Event, Kind: r.Kind, State: string(r.State), Attempts: r.Attempts,
		Class: string(r.Class), Last: stamp(r.LastAt), Message: firstLine(r.Message)}
}

func deadRows(recs []hooks.DeadRecord) []deadRow {
	rows := make([]deadRow, 0, len(recs))
	for _, r := range recs {
		rows = append(rows, deadRowOf(r))
	}
	return rows
}

func (a *app) hookDeadListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the events plugins did not handle",
		Long: `List dead letters, oldest first: the event, its kind and resource, the
failure's class and message, the attempts made (across restarts and
replays), when it was first and last attempted and, for a failed
operation, its index. A record holds references only, never an event's
content: a replay reads the event again from uma as the plugin.

--plugin narrows to one plugin; --state to dead (the default: waiting
for drop), replayed or discarded (kept as history), or all.
Operator role.`,
		Example: `  heo hook dead-letter list
  heo hook dead-letter list --plugin triage --state all --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			plugin, _ := cmd.Flags().GetString(flagPlugin)
			state, _ := cmd.Flags().GetString(flagState)
			st, err := deadStateOf(state)
			if err != nil {
				return err
			}
			recs, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) ([]hooks.DeadRecord, error) {
				l, err := a.hookLedger(c, authz.ActionHookRead)
				if err != nil {
					return nil, err
				}
				recs, err := l.List(ctx, plugin, st)
				if recs == nil {
					recs = []hooks.DeadRecord{}
				}
				return recs, a.hookFailure(err, plugin)
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, recs)
			}
			return output.Dispatch(cmd, a.r.Viper, deadRows(recs))
		},
	}
	c.Flags().String(flagPlugin, "", "Only this plugin's dead letters")
	c.Flags().String(flagState, string(hooks.StateDead), "Only letters in this state: dead, replayed, discarded or all")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &[]hooks.DeadRecord{}, Version: "1.0"})
	return c
}

func deadStateOf(s string) (hooks.DeadState, error) {
	switch hooks.DeadState(s) {
	case hooks.StateDead, hooks.StateReplayed, hooks.StateDiscarded:
		return hooks.DeadState(s), nil
	}
	if s == stateAll {
		return "", nil
	}
	return "", output.UsageError(fmt.Sprintf("--%s %q: want dead, replayed, discarded or all", flagState, s))
}

// deadOut is a dead letter after a drop.
type deadOut struct {
	hooks.DeadRecord
	// Outcome is what the verb did: discarded.
	Outcome string `json:"outcome" yaml:"outcome"`
}

func deadRowsOf(o deadOut) [][2]string {
	r := o.DeadRecord
	rows := [][2]string{{"plugin", r.Plugin}, {"event", r.Event + " (" + r.Kind + ")"}, {"outcome", o.Outcome},
		{"state", string(r.State)}, {"attempts", strconv.Itoa(r.Attempts)}}
	if !r.ResolvedAt.IsZero() {
		rows = append(rows, [2]string{"resolved", stamp(r.ResolvedAt)})
	}
	return rows
}

// dropPlan renders a drop for --dry-run: what would change.
func dropPlan(rec hooks.DeadRecord) *cli.Plan {
	plan := &cli.Plan{
		Args:                 map[string]any{"plugin": rec.Plugin, "event": rec.Event, "state": rec.State, "attempts": rec.Attempts},
		Effects:              []cli.Effect{},
		PrerequisitesChecked: []string{"authorization", "dead letter"},
	}
	if rec.State != hooks.StateDead {
		plan.Warnings = []string{fmt.Sprintf("the letter is %s, not dead: drop would be refused and nothing would change", rec.State)}
		return plan
	}
	plan.Effects = append(plan.Effects, planEffect("update", "dead-letter:"+rec.Plugin+"/"+rec.Event, false,
		"discarded: the event is never delivered to the plugin again (the record stays listed as history); nothing is run"))
	return plan
}

func (a *app) hookDeadDropCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "drop <plugin> <event>",
		Short: "Give up on a dead letter: its event is never delivered again",
		Long: `Discard the dead letter of <event> for <plugin>. The record stays
listed as discarded, with when, and the event is never delivered to the
plugin again. Nothing is run and uma's event is untouched. Only a
dead letter is dropped: a replayed or discarded one is refused
(CONFLICT).

Destructive: it asks for confirmation on a terminal; without one it is
refused (exit 5) unless --confirm=yes is passed. REST, gRPC and MCP do
not serve it until the operator names the surface; the control socket
does. --dry-run shows what would change. Operator role.`,
		Example: `  heo hook dead-letter drop triage ev_123 --dry-run
  heo hook dead-letter drop triage ev_123 --confirm=yes`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[deadOut], error) {
				var w written[deadOut]
				l, err := a.hookLedger(c, authz.ActionHookManage)
				if err != nil {
					return w, err
				}
				if cli.IsDryRun(cmd) {
					rec, err := l.Get(ctx, args[0], args[1])
					if err != nil {
						return w, a.hookFailure(err, args[0]+"/"+args[1])
					}
					w.Plan = dropPlan(rec)
					return w, nil
				}
				rec, err := l.Discard(ctx, args[0], args[1])
				if err != nil {
					if errors.Is(err, hooks.ErrNoDeadLetter) {
						return w, a.hookFailure(err, args[0]+"/"+args[1])
					}
					e := output.ConflictError(err.Error())
					e.SuggestedFix = "only a dead letter is dropped; `" + a.heoCommand("hook dead-letter list --state all --plugin "+args[0]) + "` shows them"
					return w, e
				}
				w.Out = deadOut{DeadRecord: rec, Outcome: "discarded"}
				return w, nil
			})
			if err != nil {
				return err
			}
			return res.render(a, cmd, deadRowsOf, nil)
		},
	}
	daemonCommand(c)
	// The store is files on the daemon's machine, and a discarded event
	// is not offered again: not for a plugin, and not served by default.
	cli.SetSideEffect(c, cli.SideEffectDestructiveLocal)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &deadOut{}, Version: "1.0"})
	return c
}

// --- hook dead-letter retry, hook replay, hook job ---------------------

// Retries and replays are queued, not run in the command: the operations
// a plugin returns run through the command tree, which the command
// itself holds, so running them here would wait on itself. The daemon
// runs the job on its own goroutine, and the verb answers at once with a
// handle: hook job get <id>, the dead letter's state and the hooks
// section of service status show the outcome. Jobs live in memory.
const jobNote = "queued, not run: the daemon runs it on its own goroutine; `hook job get <id>` shows its outcome. " +
	"Jobs are kept in memory and end with the daemon"

func jobRows(j hooks.Job) [][2]string {
	rows := [][2]string{{"job", j.ID}, {"plugin", j.Plugin}, {"kind", j.Kind}, {"state", string(j.State)}}
	if j.Event != "" {
		rows = append(rows, [2]string{"event", j.Event})
	}
	if j.From != "" {
		rows = append(rows, [2]string{"from", j.From}, [2]string{"limit", strconv.Itoa(j.Limit)})
	}
	if j.Error != "" {
		rows = append(rows, [2]string{"error", j.Error})
	}
	if j.Retry != nil {
		rows = append(rows, [2]string{"letter", string(j.Retry.State) + ", attempts " + strconv.Itoa(j.Retry.Attempts)})
	}
	if r := j.Replay; r != nil {
		rows = append(rows, [2]string{"scanned", strconv.Itoa(r.Scanned)}, [2]string{"delivered", strconv.Itoa(r.Delivered)},
			[2]string{"skipped", strconv.Itoa(r.Skipped)}, [2]string{"failed", strconv.Itoa(r.Failed)},
			[2]string{"complete", strconv.FormatBool(r.Complete)}, [2]string{"next", string(r.Next)})
	}
	return rows
}

// jobPlan renders a queued job's preview: nothing is queued.
func jobPlan(args map[string]any, effects []cli.Effect, warnings ...string) *cli.Plan {
	return &cli.Plan{Args: args, Effects: effects, Warnings: warnings, PrerequisitesChecked: []string{"authorization", "plugin running"}}
}

func (a *app) enqueueFailure(err error, args []string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hooks.ErrNoDeadLetter):
		return a.hookFailure(err, strings.Join(args, "/"))
	case model.IsCode(err, model.CursorExpired):
		return a.expiredCursor(err, "start over with --from beginning (the oldest retained event)")
	case strings.Contains(err.Error(), "is not running"):
		e := output.PrerequisiteError(err.Error())
		e.SuggestedFix = "see `" + a.heoCommand("service status") + "`: fix why the plugin did not start, then restart the hooks service"
		return e
	case strings.Contains(err.Error(), "not dead"):
		e := output.ConflictError(err.Error())
		e.SuggestedFix = "only a dead letter is retried; `" + a.heoCommand("hook dead-letter list --state all") + "` shows them"
		return e
	case strings.Contains(err.Error(), "needs --from"):
		return output.UsageError(err.Error())
	}
	return errmap.Of(err)
}

func (a *app) hookDeadRetryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "retry <plugin> <event>",
		Short: "Queue running a dead-lettered event through its plugin again",
		Long: `Queue a retry of the dead letter of <event> for the running plugin
<plugin> and answer at once with a job (state queued). The daemon then
reads the event again from uma as the plugin's actor (its audience rule
applies as to any delivery) and runs the plugin on it, once, with the
attempt after the last. The result's operations are applied under the
keys of the first run: those already applied are no-ops, so a retry never
does a step twice. Handled, the record becomes replayed. Failed again,
it stays dead with the new attempt, class and message counted and the
job is failed.

The job is queued, not run in the command: ` + "`hook job get <id>`" + ` shows its
outcome, and so do ` + "`hook dead-letter list --state all`" + ` and the hooks
section of ` + "`service status`" + `. Jobs live in memory and end with the daemon;
a retry lost that way is queued again. A plugin's jobs run one at a time.
Only a dead letter is retried: a replayed or discarded one is refused
(CONFLICT), as is one whose plugin is not running.

--dry-run shows what would run and queues nothing. Operator role.`,
		Example: `  heo hook dead-letter retry triage ev_123 --dry-run
  heo hook dead-letter retry triage ev_123`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[hooks.Job], error) {
				var w written[hooks.Job]
				l, err := a.hookLedger(c, authz.ActionHookManage)
				if err != nil {
					return w, err
				}
				if cli.IsDryRun(cmd) {
					rec, err := l.Get(ctx, args[0], args[1])
					if err != nil {
						return w, a.hookFailure(err, args[0]+"/"+args[1])
					}
					w.Plan = retryPlan(rec, l)
					return w, nil
				}
				w.Out, err = l.EnqueueRetry(ctx, args[0], args[1])
				return w, a.enqueueFailure(err, args)
			})
			if err != nil {
				return err
			}
			return res.render(a, cmd, jobRows, []string{jobNote})
		},
	}
	daemonCommand(c)
	// Runs the plugin's process and applies its operations on the
	// daemon's machine, under keys that make a repeat a no-op.
	cli.SetSideEffect(c, cli.SideEffectWriteLocal)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &hooks.Job{}, Version: "1.0"})
	return c
}

func retryPlan(rec hooks.DeadRecord, l *hooks.Ledger) *cli.Plan {
	args := map[string]any{"plugin": rec.Plugin, "event": rec.Event, "state": rec.State, "attempts": rec.Attempts}
	if rec.State != hooks.StateDead {
		return jobPlan(args, []cli.Effect{}, fmt.Sprintf("the letter is %s, not dead: retry would be refused and nothing queued", rec.State))
	}
	if _, running := l.Runner(rec.Plugin); !running {
		return jobPlan(args, []cli.Effect{}, "plugin "+rec.Plugin+" is not running: retry would be refused")
	}
	return jobPlan(args, []cli.Effect{
		planEffect("create", "job:retry", false, "queue a job that reads the event again from uma as the plugin and runs the plugin on it, as attempt "+
			strconv.Itoa(rec.Attempts+1)+"; its operations are applied under the keys of the first run, so those already applied are no-ops"),
		planEffect("update", "dead-letter:"+rec.Plugin+"/"+rec.Event, false, "replayed when the plugin's result is applied; a failure leaves it dead with the new attempt counted"),
	})
}

func (a *app) hookReplayCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "replay <plugin>",
		Short: "Queue delivering the event log to a plugin again from a position",
		Long: `Queue a replay of the events of uma's log from --from to the running
plugin <plugin>, oldest first, at most --limit of them, and answer at
once with a job (state queued). --from is a log position (the next of
` + "`event list`" + `, or a plugin's cursor in ` + "`hook list`" + `) or "beginning", the oldest
retained event. The events are read as the plugin's actor with its
filter, so it is delivered only what it could have read the first time;
events outside its filter or audience are skipped.

This is a bounded walk of the log. The plugin's durable subscription is
not moved: uma cannot seek one, so it stays where it is and keeps
delivering new events, and a replay runs beside it, without ordering
against the events delivered now. Each event is delivered once, as the
runner delivers one: its operations carry the keys of the first delivery,
so those already applied are no-ops, and an event the plugin never saw is
handled for the first time. A failure is recorded as a dead letter (see
` + "`hook dead-letter retry`" + `) and the walk goes on.

The job is queued, not run in the command: ` + "`hook job get <id>`" + ` reports
scanned, delivered, skipped and failed events and next, the position
after the last event handled, to pass as the next --from when complete
is false. Jobs live in memory and end with the daemon; a replay lost
that way is queued again from the same position. A plugin's jobs run
one at a time.

--dry-run reads the log and lists the events that would be delivered; it
runs nothing and queues nothing. Operator role.`,
		Example: `  heo hook replay triage --from beginning --dry-run
  heo hook replay triage --from pg.42 --limit 50`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, _ := cmd.Flags().GetString(flagFrom)
			limit, _ := cmd.Flags().GetInt(flagLimit)
			if from == "" {
				return output.UsageError("--" + flagFrom + " is required: a log position (the next of `event list`) or beginning")
			}
			if limit < 1 {
				return output.UsageError(fmt.Sprintf("--%s %d: want 1 or more", flagLimit, limit))
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[hooks.Job], error) {
				var w written[hooks.Job]
				l, err := a.hookLedger(c, authz.ActionHookManage)
				if err != nil {
					return w, err
				}
				if cli.IsDryRun(cmd) {
					rep, err := l.ReplayFrom(ctx, args[0], from, limit, true)
					if err != nil {
						return w, a.enqueueFailure(err, args)
					}
					w.Plan = replayPlan(rep, limit)
					return w, nil
				}
				w.Out, err = l.EnqueueReplay(ctx, args[0], from, limit)
				return w, a.enqueueFailure(err, args)
			})
			if err != nil {
				return err
			}
			return res.render(a, cmd, jobRows, []string{jobNote})
		},
	}
	c.Flags().String(flagFrom, "", "Log position to start at (the next of `event list`, or a cursor of `hook list`), or beginning (required)")
	c.Flags().Int(flagLimit, defaultReplayLimit, "Most events to deliver")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteLocal)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &hooks.Job{}, Version: "1.0"})
	return c
}

// defaultReplayLimit bounds the events one replay delivers.
const defaultReplayLimit = 100

// replayPlan renders a replay for --dry-run: each event that would be
// delivered, and where the next replay would continue.
func replayPlan(r hooks.ReplayReport, limit int) *cli.Plan {
	plan := jobPlan(map[string]any{"plugin": r.Plugin, "from": r.From, "limit": limit, "next": r.Next, "complete": r.Complete}, []cli.Effect{})
	plan.PrerequisitesChecked = append(plan.PrerequisitesChecked, "log position")
	skipped := 0
	for _, e := range r.Events {
		if e.Outcome != hooks.ReplayWould {
			skipped++
			continue
		}
		plan.Effects = append(plan.Effects, planEffect("run", "event:"+e.Event, false,
			"deliver the "+e.Kind+" event to "+r.Plugin+" and apply its operations under the keys of any earlier delivery"))
	}
	if skipped > 0 {
		plan.Warnings = []string{fmt.Sprintf("%d scanned events are outside the plugin's filter or audience and would be skipped", skipped)}
	}
	return plan
}

func (a *app) hookJobCmd() *cobra.Command {
	grp := &cobra.Command{Use: "job", Short: "Inspect the retries and replays the daemon runs", Args: cobra.NoArgs}
	cli.SetHierarchical(grp)
	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Show the state and outcome of a queued retry or replay",
		Long: `Show the job <id> that ` + "`hook dead-letter retry`" + ` or ` + "`hook replay`" + ` answered with: queued,
running, done or failed, and when it did, a retry's dead letter or a
replay's counts and next position. Jobs are kept in memory, a few
hundred of them, and end with the daemon: an unknown id is NOT_FOUND,
and ` + "`hook dead-letter list --state all`" + ` is the durable record of a retry.
Operator role.`,
		Example: `  heo hook job get hj-abc-1
  heo hook job get hj-abc-1 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			j, err := daemonRun(a, cmd, args, func(_ context.Context, c control.Caller) (hooks.Job, error) {
				l, err := a.hookLedger(c, authz.ActionHookRead)
				if err != nil {
					return hooks.Job{}, err
				}
				j, ok := l.Job(args[0])
				if !ok {
					e := output.NotFoundError("job " + args[0] + ": not found")
					e.SuggestedFix = "jobs end with the daemon; `" + a.heoCommand("hook dead-letter list --state all") + "` shows what became of a retry"
					return j, e
				}
				return j, nil
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, j)
			}
			return writeRows(cmd.OutOrStdout(), jobRows(j))
		},
	}
	daemonCommand(get)
	cli.SetSideEffect(get, cli.SideEffectRead)
	cli.SetIdempotency(get, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(get, cli.OutputSchema{Type: &hooks.Job{}, Version: "1.0"})
	grp.AddCommand(get)
	return grp
}
