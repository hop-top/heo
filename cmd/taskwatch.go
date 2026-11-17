package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"hop.top/kit/go/core/id"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// The steps of a task watch, beyond event watch's: close ends the
// subscription once the task is final.
const stepClose = "close"

// taskStep is one step of a task watch, run in the daemon: open (or
// resume) the task's subscription, lease its next batch, acknowledge a
// batch once its lines are written, or close the subscription. Inbox and
// Conversation locate the task for the steps after open, so a poll reads
// it by reference and scans nothing.
type taskStep struct {
	Op           string   `json:"op"`
	Subscription model.ID `json:"subscription,omitempty"`
	Token        string   `json:"token,omitempty"`
	Inbox        model.ID `json:"inbox,omitempty"`
	Conversation model.ID `json:"conversation,omitempty"`
}

// taskStepOut is what one step answers: the subscription and its
// checkpoint; for a poll the events of the batch that concern the task,
// the acknowledgement token and the task's state; for an open also where
// the task is.
type taskStepOut struct {
	Subscription model.ID      `json:"subscription,omitempty"`
	Checkpoint   model.Cursor  `json:"checkpoint,omitempty"`
	Events       []model.Event `json:"events"`
	Token        string        `json:"token,omitempty"`
	Inbox        model.ID      `json:"inbox,omitempty"`
	Conversation model.ID      `json:"conversation,omitempty"`
	State        string        `json:"state"`
	Terminal     bool          `json:"terminal"`
}

// taskStepper runs one task watch step where the runtime lives.
type taskStepper func(ctx context.Context, st taskStep) (taskStepOut, error)

func (a *app) taskWatchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch <task>",
		Short: "Stream a task's events until it reaches a final state",
		Long: `Stream the events of the task <task> (record ID) as uma commits them:
its changes (task.updated: state, artifacts, status) and the status
messages it carries (message.created). One JSON object per line under
--format json (NDJSON), one row per line otherwise (recorded, kind,
task, what changed, event ID). The watch ends, exit 0, once the task is
in a final state (completed, failed, canceled, rejected) and its
events are written: read the state, which is on the last line and on
stderr, to tell how. A task that is final when the watch starts ends it
once the events the consumer has not seen are written: none for a new
consumer at tail, the task's history from beginning.

The stream is your durable subscription named --consumer (default
heo-task-<task>), narrowed by uma to task and message events of the
task's inbox; heo keeps the events of this task. Each batch is
acknowledged once its lines are written, so rerunning with the same
consumer resumes after the last line written: nothing is skipped, and an
event may repeat only if a watch stopped between writing a batch and
acknowledging it (deduplicate by event ID). A new consumer starts at
--from tail (default: events from now on) or beginning (the oldest
retained). A position behind event retention is refused (CURSOR_EXPIRED,
exit 4): start a new consumer and read the task. The subscription is
closed when the watch ends at a final state.

With no daemon running the watch fails at once, PREREQUISITE (exit 70).
Once attached it waits out a daemon that stops answering, a restart
say, for --reconnect-timeout (default 30s) of continuous unavailability,
saying so once on stderr, and resumes where it was; past that it fails
with exit 70. --reconnect-timeout 0 fails at once.

--timeout stops waiting after that long (exit 6, TRANSIENT: the task
was still being worked; follow it again with the same consumer).
Ctrl-C or SIGTERM acknowledges what was written, prints where it
stopped on stderr and exits 0.

Task events are internal: only desk actors may watch (a viewer reads
tasks with ` + "`task get`" + `). Not served over REST, SSE, gRPC or MCP: a stream
held open would hold the daemon.`,
		Example: `  heo task watch task_123
  heo task watch task_123 --timeout 5m --format json
  heo task watch task_123 --consumer ops --from beginning`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			timeout, _ := cmd.Flags().GetDuration(flagTimeout)
			if timeout < 0 {
				return output.UsageError("--" + flagTimeout + ": want a duration of zero (wait without limit) or more")
			}
			start, err := taskWatchStartOf(cmd, args[0])
			if err != nil {
				return err
			}
			if raw, _ := cmd.Flags().GetString(flagStep); raw != "" {
				return a.taskStepRun(cmd, args, start, raw)
			}
			if cli.IsDryRun(cmd) {
				return cli.RenderPlan(cmd, taskWatchPlan(args[0], start))
			}
			ctx := cmd.Context()
			route, _ := control.RouteOf(ctx, a.control.servesControl())
			switch route {
			case control.Local:
				// A served call reached the daemon without --step: the
				// loop would hold the daemon's one served call for as
				// long as the task is open.
				return output.UsageError("task watch streams until the task is final and does not run inside the daemon: " +
					"run `" + a.heoCommand("task watch <task>") + "` at the terminal, which drives it over the control socket one step at a time")
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
			return a.followTask(ctx, cmd, args[0], start, a.remoteTaskStep(cmd, args), timeout, reconnect)
		},
	}
	c.Flags().String(flagConsumer, "", "Name of your durable subscription; reuse it to resume (default heo-task-<task>)")
	c.Flags().String(flagFrom, desk.StartTail, "Where a new consumer starts: tail (from now on) or beginning (oldest retained)")
	c.Flags().Duration(flagTimeout, 0, "Stop waiting after this long (exit 6); 0 waits until the task is final")
	addReconnectFlag(c)
	c.Flags().String(flagStep, "", "One step of a watch driven over the control socket (heo's own client)")
	_ = c.Flags().MarkHidden(flagStep)
	daemonCommand(c)
	// A watch runs until its task is final, and kit serves one call at a
	// time: on REST, SSE, gRPC or MCP it would hold the daemon (an MCP
	// tool call never returns). Like event watch, it is withheld from all
	// four and stays on the control socket, where the terminal's watch
	// drives it as short --step calls.
	cli.SetWithholdFrom(c, cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &model.Event{}, Version: "1.0"})
	return c
}

// taskWatchStartOf reads --consumer and --from of a watch of task.
func taskWatchStartOf(cmd *cobra.Command, task string) (desk.WatchStart, error) {
	var w desk.WatchStart
	w.Consumer, _ = cmd.Flags().GetString(flagConsumer)
	if w.Consumer == "" {
		w.Consumer = desk.TaskConsumer(task)
	}
	w.From, _ = cmd.Flags().GetString(flagFrom)
	if w.From != desk.StartTail && w.From != desk.StartBeginning {
		return w, output.UsageError(fmt.Sprintf("--%s %q: want %s or %s", flagFrom, w.From, desk.StartTail, desk.StartBeginning))
	}
	return w, nil
}

// taskWatchPlan renders a task watch for --dry-run: it opens nothing.
func taskWatchPlan(task string, w desk.WatchStart) cli.Plan {
	return cli.Plan{
		Args: map[string]any{"task": task, "consumer": w.Consumer,
			"start": "resume at its checkpoint when open; otherwise start at " + w.From},
		Effects: []cli.Effect{planEffect("update", "subscription:"+w.Consumer, true,
			"open or resume the subscription; stream the task's events and acknowledge each batch once its lines are written; close it when the task is final")},
		PrerequisitesChecked: []string{"flags"},
	}
}

// taskStepRun runs one step for heo's own client and prints its answer.
func (a *app) taskStepRun(cmd *cobra.Command, args []string, start desk.WatchStart, raw string) error {
	var st taskStep
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return output.UsageError("--" + flagStep + ": " + err.Error())
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (taskStepOut, error) {
		return a.runTaskStep(ctx, c, args[0], start, st)
	})
	if err != nil {
		return err
	}
	return output.Dispatch(cmd, a.r.Viper, res)
}

// taskInternal is the audience of task events: reading them is for
// desk actors alone, so every step is authorized under it.
var taskInternal = model.Audience{Visibility: model.Internal}

// runTaskStep runs one task watch step in the daemon for c, authorized
// again by heo's policy under the events' internal audience, and by uma
// for the task and every event.
func (a *app) runTaskStep(ctx context.Context, c control.Caller, task string, start desk.WatchStart, st taskStep) (taskStepOut, error) {
	var out taskStepOut
	sub := model.SubscriptionRef{Tenant: c.Tenant, ID: st.Subscription}
	var action string
	switch st.Op {
	case stepOpen:
		action = model.ActionSubscriptionOpen
	case stepPoll:
		action = model.ActionSubscriptionPoll
	case stepAck:
		action = model.ActionSubscriptionAck
	case stepClose:
		action = model.ActionSubscriptionClose
	default:
		return out, output.UsageError(fmt.Sprintf("--%s: unknown step %q", flagStep, st.Op))
	}
	err := a.desk(ctx, c, action, taskInternal, func(s desk.Service) error {
		switch st.Op {
		case stepOpen:
			t, err := s.FindTask(ctx, task, "")
			if err != nil {
				return err
			}
			out.Inbox, out.Conversation = t.Ref.Conversation.Inbox.ID, t.Ref.Conversation.ID
			out.State, out.Terminal = t.State, model.IsTerminalTaskState(t.State)
			key, err := id.New(operationKeyPrefix)
			if err != nil {
				return err
			}
			start.Filter = model.Filter{Kinds: desk.TaskWatchKinds, Inboxes: []model.InboxRef{t.Ref.Conversation.Inbox}}
			got, err := s.OpenWatch(ctx, start, model.OperationID(key))
			if err != nil {
				return a.taskWatchFailure(err, task)
			}
			out.Subscription, out.Checkpoint = got.Ref.ID, got.Checkpoint
		case stepPoll:
			p, err := s.PollTask(ctx, s.TaskRefOf(st.Inbox, st.Conversation, model.ID(task)), sub)
			if err != nil {
				return a.taskWatchFailure(err, task)
			}
			out.Subscription, out.Events, out.Token = sub.ID, p.Events, p.Token
			out.State, out.Terminal = p.Task.State, model.IsTerminalTaskState(p.Task.State)
		case stepAck:
			got, err := s.AckWatch(ctx, sub, st.Token)
			if err != nil {
				return err
			}
			out.Subscription, out.Checkpoint = got.Ref.ID, got.Checkpoint
		case stepClose:
			key, err := id.New(operationKeyPrefix)
			if err != nil {
				return err
			}
			out.Subscription = sub.ID
			return s.CloseWatch(ctx, sub, model.OperationID(key))
		}
		return nil
	})
	if out.Events == nil {
		out.Events = []model.Event{}
	}
	return out, err
}

// taskWatchFailure explains a watch that cannot start or go on: a
// position behind event retention.
func (a *app) taskWatchFailure(err error, task string) error {
	if model.IsCode(err, model.CursorExpired) {
		e := errmap.Of(err)
		e.Message = "the events after this position were purged by event retention: " + e.Message
		e.SuggestedFix = "read the task with `" + a.heoCommand("task get "+task) + "`, then watch it again under a new consumer: `" +
			a.heoCommand("task watch "+task+" --consumer <new> --from beginning") + "`"
		return e
	}
	return err
}

// remoteTaskStep runs each step in the daemon over the control socket,
// as heo's own client: the watch's flags, the step, json.
func (a *app) remoteTaskStep(cmd *cobra.Command, args []string) taskStepper {
	return func(ctx context.Context, st taskStep) (taskStepOut, error) {
		raw, err := json.Marshal(st)
		if err != nil {
			return taskStepOut{}, err
		}
		req := control.RequestFor(cmd, args)
		req.Flags[flagStep] = string(raw)
		res, err := a.controlClient().Invoke(ctx, req)
		if err != nil {
			return taskStepOut{}, err
		}
		return control.Decode[taskStepOut](res)
	}
}

// followTask streams the events of task through step until the task is
// final and its events are written, ctx ends or timeout passes: open the
// subscription, lease each batch, write its lines and only then
// acknowledge it. A transient refusal (the daemon's rate limit, a busy
// store) backs off and goes on; a daemon that stops answering is waited
// for up to reconnect (see retryTransient).
func (a *app) followTask(ctx context.Context, cmd *cobra.Command, task string, w desk.WatchStart, step taskStepper, timeout, reconnect time.Duration) error {
	step = retryTransient(step, reconnect, cmd.ErrOrStderr())
	var deadline <-chan time.Time
	sctx := ctx // what open and poll run under: cut short by the timeout
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	opened, err := step(sctx, taskStep{Op: stepOpen})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "watching task %s (%s) as consumer %s\n", task, opened.State, w.Consumer); err != nil {
		return err
	}
	loc := taskStep{Subscription: opened.Subscription, Inbox: opened.Inbox, Conversation: opened.Conversation}
	asJSON := a.r.Viper.GetString("format") == output.JSON
	out := cmd.OutOrStdout()
	checkpoint, state := opened.Checkpoint, opened.State
	for {
		if ctx.Err() != nil {
			return a.taskWatchStopped(cmd, task, w, state, checkpoint)
		}
		select {
		case <-deadline:
			return a.taskWatchTimedOut(task, w, state)
		default:
		}
		poll := loc
		poll.Op = stepPoll
		b, err := step(sctx, poll)
		if err != nil {
			if ctx.Err() != nil {
				return a.taskWatchStopped(cmd, task, w, state, checkpoint)
			}
			if sctx.Err() != nil {
				return a.taskWatchTimedOut(task, w, state)
			}
			return err
		}
		state = b.State
		if err := writeTaskEvents(out, b.Events, asJSON); err != nil {
			return err
		}
		if b.Token != "" {
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), watchAckTime)
			ack := loc
			ack.Op, ack.Token = stepAck, b.Token
			acked, err := step(actx, ack)
			cancel()
			if err != nil {
				return err
			}
			checkpoint = acked.Checkpoint
		}
		switch {
		case b.Terminal && b.Token == "":
			return a.taskWatchEnded(cmd, task, state, step, loc)
		case b.Token != "":
			continue
		}
		select {
		case <-ctx.Done():
		case <-deadline:
			return a.taskWatchTimedOut(task, w, state)
		case <-time.After(watchIdle):
		}
	}
}

// taskWatchEnded ends a watch whose task is final and whose events are
// written: it closes the subscription, which has nothing left to resume,
// and says how the task ended.
func (a *app) taskWatchEnded(cmd *cobra.Command, task, state string, step taskStepper, loc taskStep) error {
	cctx, cancel := context.WithTimeout(context.Background(), watchAckTime)
	defer cancel()
	closeStep := loc
	closeStep.Op = stepClose
	var note string
	if _, err := step(cctx, closeStep); err != nil {
		note = fmt.Sprintf(" (the subscription stays open: %v)", err)
	}
	_, err := fmt.Fprintf(cmd.ErrOrStderr(), "task %s reached %s%s\n", task, state, note)
	return err
}

// taskWatchStopped ends a watch its caller interrupted: where it
// stopped, on stderr, and success.
func (a *app) taskWatchStopped(cmd *cobra.Command, task string, w desk.WatchStart, state string, checkpoint model.Cursor) error {
	_, err := fmt.Fprintf(cmd.ErrOrStderr(), "stopped: task %s was %s; consumer %s at checkpoint %s; rerun `%s` to resume\n",
		task, state, w.Consumer, orDash(string(checkpoint)), a.heoCommand("task watch "+task+" --"+flagConsumer+" "+w.Consumer))
	return err
}

// taskWatchTimedOut ends a watch whose timeout passed with the task
// still open.
func (a *app) taskWatchTimedOut(task string, w desk.WatchStart, state string) error {
	e := output.TransientError(fmt.Sprintf("task %s was still %s when --%s passed", task, state, flagTimeout))
	e.SuggestedFix = "follow it again: `" + a.heoCommand("task watch "+task+" --"+flagConsumer+" "+w.Consumer) + "` resumes where this stopped"
	return e
}

// writeTaskEvents writes one line per event: its JSON, or its row.
func writeTaskEvents(w io.Writer, evs []model.Event, asJSON bool) error {
	for _, ev := range evs {
		var line []byte
		if asJSON {
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			line = b
		} else {
			line = []byte(taskEventRow(ev))
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// taskEventRow is a task event's text row: recorded, kind, resource,
// what changed, event ID.
func taskEventRow(ev model.Event) string {
	return strings.Join([]string{
		stamp(ev.RecordedAt), ev.Kind, string(ev.Resource.Kind) + ":" + string(ev.Resource.ID), taskEventChange(ev), string(ev.ID),
	}, "\t")
}

// taskEventChange says what an event changed: a task.updated its state
// move or fields, a message.created the status message it carries.
func taskEventChange(ev model.Event) string {
	switch ev.Kind {
	case model.EventTaskUpdated:
		var p model.TaskUpdatedPayload
		if json.Unmarshal(ev.Payload.Data, &p) != nil {
			return "-"
		}
		if p.State != nil {
			from := p.State.From
			if from == "" {
				from = "(new)"
			}
			return "state " + from + " -> " + p.State.To
		}
		return orDash(strings.Join(p.Changes, ","))
	case model.EventMessageCreated:
		return "status message"
	}
	return "-"
}
