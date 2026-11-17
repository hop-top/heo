package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// Flags of the operation commands.
const (
	flagTarget  = "target"
	flagTimeout = "timeout"
)

// operationOut is an operation as uma holds it, with the key it was
// submitted under, and that key's kind and target. It leaves out
// Operation.Pending, the content of a change still to apply.
type operationOut struct {
	ID                 model.ID                 `json:"id"                            yaml:"id"`
	State              model.DeliveryState      `json:"state"                         yaml:"state"`
	Terminal           bool                     `json:"terminal"                      yaml:"terminal"`
	Result             *model.Ref               `json:"result,omitempty"              yaml:"result,omitempty"`
	Message            *model.MessageRef        `json:"message,omitempty"             yaml:"message,omitempty"`
	Stream             *model.StreamRef         `json:"stream,omitempty"              yaml:"stream,omitempty"`
	Task               *model.TaskRef           `json:"task,omitempty"                yaml:"task,omitempty"`
	ProviderReferences []model.ExternalRef      `json:"provider_references,omitempty" yaml:"provider_references,omitempty"`
	ReadConfirmedAt    *time.Time               `json:"read_confirmed_at,omitempty"   yaml:"read_confirmed_at,omitempty"`
	Failure            *model.Error             `json:"failure,omitempty"             yaml:"failure,omitempty"`
	Progress           *model.OperationProgress `json:"progress,omitempty"            yaml:"progress,omitempty"`
	NextAttemptAt      *time.Time               `json:"next_attempt_at,omitempty"     yaml:"next_attempt_at,omitempty"`
	UpdatedAt          time.Time                `json:"updated_at"                    yaml:"updated_at"`
	Revision           model.Revision           `json:"revision"                      yaml:"revision"`
	Key                string                   `json:"key,omitempty"                 yaml:"key,omitempty"`
	Kind               string                   `json:"kind,omitempty"                yaml:"kind,omitempty"`
	Target             string                   `json:"target,omitempty"              yaml:"target,omitempty"`
}

func operationDetail(op model.Operation) operationOut {
	o := operationOut{
		ID: op.Ref.ID, State: op.State, Terminal: desk.Terminal(op.State), Result: op.Result, Message: op.ResultMessage,
		Stream: op.ResultStream, Task: op.ResultTask, ProviderReferences: op.ProviderReferences,
		ReadConfirmedAt: op.ReadConfirmedAt, Failure: op.Failure, Progress: op.Progress, Revision: op.Revision,
		NextAttemptAt: op.NextAttemptAt, UpdatedAt: op.UpdatedAt,
	}
	if k := op.Key; k != nil {
		o.Key, o.Kind, o.Target = string(k.ID), k.Kind, targetName(k.Target)
	}
	return o
}

// targetName spells a target as --target takes it: <kind>:<id>, or
// <kind>:<inbox>/<id> for a record of an inbox.
func targetName(r model.Ref) string {
	if r.Inbox != "" {
		return string(r.Kind) + ":" + string(r.Inbox) + "/" + string(r.ID)
	}
	return string(r.Kind) + ":" + string(r.ID)
}

// parseTarget reads --target: <kind>:<id> for a tenant-scoped record,
// <kind>:<inbox>/<id> for one that lives in an inbox, in tenant.
func parseTarget(tenant model.TenantID, s string) (model.Ref, error) {
	kind, rest, ok := strings.Cut(s, ":")
	inbox, id, nested := strings.Cut(rest, "/")
	if !nested {
		inbox, id = "", rest
	}
	if !ok || kind == "" || id == "" || (nested && inbox == "") {
		return model.Ref{}, output.UsageError(fmt.Sprintf("--%s %q: want <kind>:<id> or <kind>:<inbox>/<id>, "+
			"such as conversation:in_1/conv_9 or inbox:in_1", flagTarget, s))
	}
	return model.Ref{Tenant: tenant, Kind: model.ResourceKind(kind), Inbox: model.ID(inbox), ID: model.ID(id)}, nil
}

func operationDetailRows(o operationOut) [][2]string {
	rows := [][2]string{{"id", string(o.ID)}, {"state", string(o.State)}, {"terminal", yesNo(o.Terminal)}}
	if o.Key != "" {
		rows = append(rows, [2]string{"key", o.Key}, [2]string{"kind", o.Kind}, [2]string{"target", o.Target})
	}
	switch {
	case o.Message != nil:
		rows = append(rows, [2]string{"message", string(o.Message.ID)}, [2]string{"conversation", string(o.Message.Conversation.ID)})
	case o.Stream != nil:
		rows = append(rows, [2]string{"stream", string(o.Stream.ID)})
	case o.Task != nil:
		rows = append(rows, [2]string{"task", string(o.Task.ID)})
	case o.Result != nil:
		rows = append(rows, [2]string{"result", string(o.Result.Kind) + " " + string(o.Result.ID)})
	}
	if len(o.ProviderReferences) > 0 {
		refs := make([]string, len(o.ProviderReferences))
		for i, r := range o.ProviderReferences {
			refs[i] = routeName(r)
		}
		rows = append(rows, [2]string{"provider refs", strings.Join(refs, ", ")})
	}
	if o.Progress != nil {
		rows = append(rows, [2]string{"progress", fmt.Sprintf("%d done", o.Progress.Done)})
	}
	if f := o.Failure; f != nil {
		rows = append(rows, [2]string{"failure", f.Error()}, [2]string{"failure retry", string(f.Retry)})
	}
	if o.NextAttemptAt != nil {
		rows = append(rows, [2]string{"next attempt", stamp(*o.NextAttemptAt)})
	}
	return append(rows, [2]string{"updated", stamp(o.UpdatedAt)}, [2]string{"revision", string(o.Revision)})
}

func yesNo(b bool) string { return map[bool]string{true: "yes", false: "no"}[b] }

// terminalError is the exit a terminal state maps to, per the error
// map: a delivered, accepted or committed operation succeeds; any other
// terminal state fails as its failure's uma code does (a final failure
// 1, an unknown outcome 8 — never 0 — a rejection what uma says), and
// as a generic failure when uma recorded none.
func terminalError(op model.Operation) error {
	switch op.State {
	case model.ProviderAccepted, model.Delivered, model.Committed:
		return nil
	}
	var cause error
	switch {
	case op.Failure != nil:
		cause = op.Failure
	case op.State == model.Unknown:
		cause = model.NewError(model.OutcomeUnknown, "provider outcome unknown")
	default:
		cause = model.NewError(model.Permanent, "operation "+string(op.State))
	}
	e := errmap.Of(cause)
	e.Message = fmt.Sprintf("operation %s ended %s: %s", op.Ref.ID, op.State, e.Message)
	return e
}

func (a *app) operationCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "operation",
		Short: "Find and follow the operations your commands submitted",
		Long: `Every command that writes returns an operation: uma's record that it
accepted the command, which the daemon's workers then carry out. These
commands find one again — by its ID, or by the key you submitted it
under, which recovers a command whose answer never reached you — and
follow it until it ends.

` + "`operation list`" + ` shows the operations you submitted, as uma's ledger holds
them (each with its key, kind and target), ` + "`operation get`" + ` one operation's
state, and ` + "`operation watch`" + ` follows one until it ends. The list and a
key are yours: another caller's operations are not listed and its keys are
not found.

Every command runs in the profile's daemon (none running exits 70).
Reading is the desk and operator roles'.`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(grp)
	grp.AddCommand(a.operationListCmd(), a.operationGetCmd(), a.operationWatchCmd(), a.operationHeldCmd(), a.operationTrustCmd())
	return grp
}

// operationRow is an operation in a listing table, as uma holds it.
type operationRow struct {
	Key       string `json:"key"       yaml:"key"       table:"KEY"`
	Kind      string `json:"kind"      yaml:"kind"      table:"KIND"`
	State     string `json:"state"     yaml:"state"     table:"STATE"`
	Updated   string `json:"updated"   yaml:"updated"   table:"UPDATED"`
	Operation string `json:"operation" yaml:"operation" table:"OPERATION"`
	Target    string `json:"target"    yaml:"target"    table:"TARGET"`
}

func operationRowOf(op model.Operation) operationRow {
	o := operationDetail(op)
	return operationRow{
		Key: o.Key, Kind: o.Kind, Target: o.Target, State: string(op.State), Operation: string(op.Ref.ID), Updated: stamp(op.UpdatedAt),
	}
}

func (a *app) operationListCmd() *cobra.Command {
	states := make([]string, len(desk.DeliveryStates))
	for i, s := range desk.DeliveryStates {
		states[i] = string(s)
	}
	c := &cobra.Command{
		Use:   "list",
		Short: "List the operations you submitted, newest change first",
		Long: `List the operations you submitted through this profile, as uma's
ledger holds them: the key each was submitted under (the
--idempotency-key you passed, or the one heo minted and printed), its
kind, its target, its state, the operation ID and when it last changed.
Newest change first, so an operation that settled moves to the top; a
page at a time (--limit, --cursor with the previous page's next). A page
can be short and still have more: follow next until it is complete.

--kind keeps operations of these kinds (such as message.reply or
conversation.start; repeat or comma-separate), --state those in these
states (` + strings.Join(states, ", ") + `), --inbox (key or ID) and
--conversation (ID) those on one inbox or conversation, and --since and
--until bound the time of the last change, [since, until). Pass a key to
` + "`operation get --key`" + ` for the operation behind it.

Only operations you sent are listed, whatever else your role may read by
ID. Operations are kept for uma's retention (runtime.retention.operations);
a write uma refused before accepting it is not here. Read-only.`,
		Example: `  heo operation list
  heo operation list --kind message.reply --since 2026-10-01
  heo operation list --state outcome_unknown --format json --limit 10`,
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
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[operationRow], error) {
				out := page[operationRow]{Items: []operationRow{}}
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) error {
					f.Actor = c.Actor
					p, err := s.ListOperations(ctx, f, pg)
					if err != nil {
						return err
					}
					out.Next, out.Complete = p.Next, p.Complete
					for _, op := range p.Items {
						out.Items = append(out.Items, operationRowOf(op))
					}
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
			return renderPage(a, cmd, res.Items, res.Next, res.Complete)
		},
	}
	c.Flags().StringSlice(flagKind, nil, "Only operations of these kinds, such as message.reply or conversation.start")
	c.Flags().StringSlice(flagState, nil, "Only operations in these states: "+strings.Join(states, ", "))
	c.Flags().String(flagInbox, "", "Only operations on this inbox (key or ID)")
	c.Flags().String(flagConversation, "", "Only operations on this conversation (ID)")
	c.Flags().String(flagSince, "", "Only operations last changed at or after this time (RFC 3339 or a date)")
	c.Flags().String(flagUntil, "", "Only operations last changed before this time (RFC 3339 or a date)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[operationRow]{}, "1.0")
	return c
}

func (a *app) operationGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get [<operation>]",
		Short: "Show an operation's state, by ID or by the key it was submitted under",
		Long: `Show an operation: its state, what it produced, the provider references
known so far and its failure with uma's retry advice. Name it by
<operation>, the operation ID a write printed, or by --key, the
--idempotency-key it was submitted under — which recovers a command whose
answer never reached you (a dropped connection, a timeout), since the
key is the one thing you chose.

--key finds the operation uma holds for the key you submitted, among
your own operations. uma looks an operation up by its kind and target as
well as its key; when you name neither, heo reads your operations from
newest to oldest for the key, so a key names an operation whatever it
was. Name --kind (such as message.reply) and --target (<kind>:<id>, or
<kind>:<inbox>/<id> for a record in an inbox, such as
conversation:in_1/conv_9) to narrow the search; with both, uma answers
directly and nothing is read through. A key used for more than one
operation names them all (exit 4: narrow it). A key another caller used,
or one uma never accepted, is not found (exit 3); so is one past uma's
operation retention, which does not mean the command was never accepted.

A state is an answer: a failed or outcome_unknown operation exits 0 with
its state in the output (` + "`operation watch`" + ` exits by the state it ends in).
An operation you may not read exits 3. Read-only.`,
		Example: `  heo operation get op_123
  heo operation get --key send-42
  heo operation get --key send-42 --kind conversation.start --target inbox:in_1 --format json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, _ := cmd.Flags().GetString(flagKey)
			kind, _ := cmd.Flags().GetString(flagKind)
			target, _ := cmd.Flags().GetString(flagTarget)
			switch {
			case key == "" && len(args) == 0:
				return output.UsageError("name the operation: <operation>, or --" + flagKey + " <idempotency-key>")
			case key != "" && len(args) > 0:
				return output.UsageError("<operation> and --" + flagKey + " both name the operation: pass one")
			case key == "" && (kind != "" || target != ""):
				return output.UsageError("--" + flagKind + " and --" + flagTarget + " narrow --" + flagKey)
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (operationOut, error) {
				var out operationOut
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) (err error) {
					if key == "" {
						var op model.Operation
						if op, err = s.GetOperation(ctx, args[0]); err == nil {
							out = operationDetail(op)
						}
						return err
					}
					out, err = a.operationByKey(ctx, s, c, key, kind, target)
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
			return writeRows(cmd.OutOrStdout(), operationDetailRows(res))
		},
	}
	c.Flags().String(flagKey, "", "Find the operation by the --idempotency-key it was submitted under")
	c.Flags().String(flagKind, "", "With --key: its kind, such as message.reply")
	c.Flags().String(flagTarget, "", "With --key: its target, <kind>:<id> or <kind>:<inbox>/<id>")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &operationOut{}, "1.0")
	return c
}

// operationByKey finds the operation the caller's write claimed under
// key: through the kind and target the caller names, or, when it names
// fewer, among the caller's own operations. uma answers for the caller
// alone.
func (a *app) operationByKey(ctx context.Context, s desk.Service, c control.Caller, key, kind, target string) (operationOut, error) {
	var ref model.Ref
	if target != "" {
		var err error
		if ref, err = parseTarget(c.Tenant, target); err != nil {
			return operationOut{}, err
		}
	}
	if kind != "" && target != "" {
		op, err := s.LookupOperation(ctx, kind, ref, key)
		if err != nil {
			return operationOut{}, a.keyNotFound(err, key)
		}
		return operationDetail(op), nil
	}
	ops, err := s.FindOperations(ctx, c.Actor, key, kind)
	if err != nil {
		return operationOut{}, err
	}
	var found []operationOut
	for _, op := range ops {
		if o := operationDetail(op); target == "" || o.Target == targetName(ref) {
			found = append(found, o)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return operationOut{}, a.keyNotFound(model.NewError(model.NotFound, "no operation"), key)
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = f.Kind + " " + f.Target + " (" + string(f.ID) + ")"
	}
	e := errmap.Of(model.NewError(model.Conflict, fmt.Sprintf("key %q names %d operations: %s", key, len(found), strings.Join(names, "; "))))
	e.SuggestedFix = "narrow it with --" + flagKind + " and --" + flagTarget
	return operationOut{}, e
}

// keyNotFound explains a key uma holds no operation for.
func (a *app) keyNotFound(err error, key string) error {
	if !model.IsCode(err, model.NotFound) {
		return err
	}
	e := errmap.Of(model.NewError(model.NotFound, fmt.Sprintf(
		"no operation of yours was submitted under key %q: it was never accepted (a write uma refused before "+
			"claiming the key, or never sent), another caller's, or past uma's operation retention", key)))
	e.SuggestedFix = "check --" + flagKind + " and --" + flagTarget + " if you passed them; or rerun the identical " +
		"command with the same --idempotency-key: uma returns the stored operation, or accepts it once, never twice"
	return e
}

// opStep is one step of an operation watch the terminal drives over the
// control socket: read the operation once.
type opStep struct {
	Op string `json:"op"`
}

// stepGet is the one step of an operation watch.
const stepGet = "get"

func (a *app) operationWatchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch <operation>",
		Short: "Follow an operation until it ends, exiting by the state it ends in",
		Long: `Follow the operation <operation> until it ends: each change of state is
written as it happens — one JSON object per line under --format json
(NDJSON), one row per line otherwise (time, state, operation ID, what
went wrong) — and the command exits by the state it ends in.

An operation ends delivered, provider_accepted or committed (exit 0), or
failed, rejected or dead_lettered (the exit of its failure: a final one
1), or outcome_unknown (exit 8: the provider may have it, so it is never
0 and never 6; ` + "`delivery get`" + ` says how to reconcile it). queued and
retry_pending operations are being worked: the watch waits.

With no daemon running the watch fails at once, PREREQUISITE (exit 70).
Once attached it waits out a daemon that stops answering, a restart
say, for --reconnect-timeout (default 30s) of continuous unavailability,
saying so once on stderr, and follows on; past that it fails with exit
70. --reconnect-timeout 0 fails at once.

--timeout stops waiting after that long (exit 6, TRANSIENT: the
operation was still being worked; follow it again). Ctrl-C or SIGTERM
stops it and exits 0. Not served over REST, SSE, gRPC or MCP: a watch
held open would hold the daemon. Reading roles: desk and operator.`,
		Example: `  heo operation watch op_123
  heo operation watch op_123 --timeout 2m --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			timeout, _ := cmd.Flags().GetDuration(flagTimeout)
			if timeout < 0 {
				return output.UsageError("--" + flagTimeout + ": want a duration of zero (wait without limit) or more")
			}
			if raw, _ := cmd.Flags().GetString(flagStep); raw != "" {
				return a.opStepRun(cmd, args, raw)
			}
			ctx := cmd.Context()
			route, _ := control.RouteOf(ctx, a.control.servesControl())
			switch route {
			case control.Local:
				return output.UsageError("operation watch streams until the operation ends and does not run inside the daemon: " +
					"run `" + a.heoCommand("operation watch <operation>") + "` at the terminal, which drives it over the control socket one step at a time")
			case control.Forward:
			default:
				return a.notDaemon()
			}
			reconnect, err := reconnectOf(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			return a.followOperation(ctx, cmd, args[0], a.remoteOpStep(cmd, args), timeout, reconnect)
		},
	}
	c.Flags().Duration(flagTimeout, 0, "Stop waiting after this long (exit 6); 0 waits until the operation ends")
	addReconnectFlag(c)
	c.Flags().String(flagStep, "", "One step of a watch driven over the control socket (heo's own client)")
	_ = c.Flags().MarkHidden(flagStep)
	daemonCommand(c)
	// A watch runs until its operation ends, and kit serves one call at
	// a time: on REST, SSE, gRPC or MCP it would hold the daemon (an MCP
	// tool call never returns). Like event watch, it is withheld from
	// all four and stays on the control socket, where the terminal's
	// watch drives it as short --step calls.
	cli.SetWithholdFrom(c, cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &operationOut{}, "1.0")
	return c
}

// opStepRun runs one step for heo's own client and prints its answer.
func (a *app) opStepRun(cmd *cobra.Command, args []string, raw string) error {
	var st opStep
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return output.UsageError("--" + flagStep + ": " + err.Error())
	}
	if st.Op != stepGet {
		return output.UsageError(fmt.Sprintf("--%s: unknown step %q", flagStep, st.Op))
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (operationOut, error) {
		var out operationOut
		err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) error {
			op, err := s.GetOperation(ctx, args[0])
			if err == nil {
				out = operationDetail(op)
			}
			return err
		})
		return out, err
	})
	if err != nil {
		return err
	}
	return output.Dispatch(cmd, a.r.Viper, res)
}

// opPoll is how long an operation watch sleeps between reads: a read
// is one call on the daemon's control socket, rate limited per caller
// like any other.
var opPoll = watchIdle

// opStepper reads the operation once, where the runtime lives.
type opStepper func(ctx context.Context) (operationOut, error)

// remoteOpStep reads the operation in the daemon over the control
// socket, as heo's own client: the watch's flags, the step, json.
func (a *app) remoteOpStep(cmd *cobra.Command, args []string) opStepper {
	return func(ctx context.Context) (operationOut, error) {
		raw, err := json.Marshal(opStep{Op: stepGet})
		if err != nil {
			return operationOut{}, err
		}
		req := control.RequestFor(cmd, args)
		req.Flags[flagStep] = string(raw)
		res, err := a.controlClient().Invoke(ctx, req)
		if err != nil {
			return operationOut{}, err
		}
		return control.Decode[operationOut](res)
	}
}

// followOperation reads the operation id through step until it ends,
// ctx ends or timeout passes, writing each change of state. A transient
// refusal (the daemon's rate limit, a busy store) backs off and goes on;
// a daemon that stops answering is waited for up to reconnect (see
// retryTransient), as in event watch and task watch.
func (a *app) followOperation(ctx context.Context, cmd *cobra.Command, id string, step opStepper, timeout, reconnect time.Duration) error {
	var deadline <-chan time.Time
	rctx := ctx // what a read runs under: cut short by the timeout
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
		var cancel context.CancelFunc
		rctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	read := retryTransient(func(ctx context.Context, _ struct{}) (operationOut, error) { return step(ctx) },
		reconnect, cmd.ErrOrStderr())
	asJSON := a.r.Viper.GetString("format") == output.JSON
	out := cmd.OutOrStdout()
	var last operationOut
	first := true
	for {
		cur, err := read(rctx, struct{}{})
		switch {
		case ctx.Err() != nil:
			return a.followStopped(cmd, id, last, first)
		case err != nil && rctx.Err() != nil:
			return a.followTimedOut(id, last, first)
		case err != nil:
			return err
		}
		if first || cur.State != last.State || cur.Revision != last.Revision {
			if err := writeOperationLine(out, cur, asJSON); err != nil {
				return err
			}
		}
		last, first = cur, false
		if cur.Terminal {
			return terminalError(model.Operation{
				Ref: model.OperationRef{ID: cur.ID}, State: cur.State, Failure: cur.Failure,
			})
		}
		select {
		case <-ctx.Done():
			return a.followStopped(cmd, id, last, first)
		case <-deadline:
			return a.followTimedOut(id, last, first)
		case <-time.After(opPoll):
		}
	}
}

// followStopped ends a watch its caller interrupted: where it stopped,
// on stderr, and success.
func (a *app) followStopped(cmd *cobra.Command, id string, last operationOut, first bool) error {
	state := "not read"
	if !first {
		state = string(last.State)
	}
	_, err := fmt.Fprintf(cmd.ErrOrStderr(), "stopped: operation %s was %s; rerun `%s` to follow it\n",
		id, state, a.heoCommand("operation watch "+id))
	return err
}

// followTimedOut ends a watch whose timeout passed with the operation
// still being worked.
func (a *app) followTimedOut(id string, last operationOut, first bool) error {
	state := "not read"
	if !first {
		state = string(last.State)
	}
	e := output.TransientError(fmt.Sprintf("operation %s was still %s when --%s passed", id, state, flagTimeout))
	e.SuggestedFix = "follow it again: `" + a.heoCommand("operation watch "+id) + "`"
	return e
}

// writeOperationLine writes one change of an operation: its JSON, or a
// row of time, state, operation ID and what went wrong.
func writeOperationLine(w interface{ Write([]byte) (int, error) }, o operationOut, asJSON bool) error {
	var line []byte
	if asJSON {
		b, err := json.Marshal(o)
		if err != nil {
			return err
		}
		line = b
	} else {
		line = []byte(strings.Join([]string{stamp(time.Now()), string(o.State), string(o.ID), failureText(o.Failure)}, "\t"))
	}
	_, err := w.Write(append(line, '\n'))
	return err
}

func failureText(f *model.Error) string {
	if f == nil {
		return "-"
	}
	return f.Error()
}
