package cmd

import (
	"context"
	"encoding/json"
	"errors"
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

// Flags of event watch.
const (
	flagConsumer = "consumer"
	flagFrom     = "from"
	// flagStep carries one step of a watch the terminal drives over the
	// control socket (see watchStep). Hidden: heo's own client sends it.
	flagStep = "step"
)

// Watch pacing: how long an idle watch sleeps between polls, how long
// it backs off after a transient refusal at most, and how long
// acknowledging a written batch may take once the watch stops. Every
// step is one call on the daemon's control socket, rate limited per
// caller like any write: an idle watch polls once a second.
const (
	watchIdle       = time.Second
	watchMaxBackoff = 5 * time.Second
	watchAckTime    = 10 * time.Second
)

// The steps of a watch.
const (
	stepOpen = "open"
	stepPoll = "poll"
	stepAck  = "ack"
)

// watchStep is one step of a watch, run in the daemon: open (or
// resume) the subscription, lease its next batch, or acknowledge a
// batch once its lines are written.
//
// A watch at the terminal drives these steps one control socket call
// at a time, writing each batch's lines here before it acknowledges
// the batch: the socket answers each call once, so the stream itself
// is never held by the daemon. The watch is a client-side loop of
// short calls; no served surface runs it (see eventWatchCmd).
type watchStep struct {
	Op           string   `json:"op"`
	Subscription model.ID `json:"subscription,omitempty"`
	Token        string   `json:"token,omitempty"`
}

// watchStepOut is what one step answers: the subscription and its
// checkpoint, and for a poll the batch's events the caller may see and
// its acknowledgement token.
type watchStepOut struct {
	Subscription model.ID      `json:"subscription"`
	Checkpoint   model.Cursor  `json:"checkpoint"`
	Events       []model.Event `json:"events"`
	Token        string        `json:"token,omitempty"`
}

// stepper runs one watch step where the runtime lives.
type stepper func(ctx context.Context, st watchStep) (watchStepOut, error)

func (a *app) eventCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "event",
		Short: "List, show or watch committed events",
		Long: `List the events uma committed, a page at a time, show one by its ID, or
watch them as they happen — messages and interactions received, messages
created, conversation changes, delivery updates. A list or a get reads
and remembers nothing; a watch is a durable subscription that remembers
where you stopped.

Each event is shown only to a caller who may read it: content that is
not public (internal notes, restricted delivery updates) reaches desk
actors alone, checked again for every event.

Every command runs in the profile's daemon (none running exits 70).
Listing and getting are served on every surface. Watching is for the terminal and
the control socket: REST, gRPC, MCP and SSE do not serve it, since a
stream held open would hold the daemon.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.eventListCmd(), a.eventGetCmd(), a.eventWatchCmd())
	return grp
}

func (a *app) eventWatchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "watch",
		Short: "Stream events as they are committed, resuming where you stopped",
		Long: `Stream the events you may read as uma commits them, until interrupted:
one JSON object per line under --format json (NDJSON), one row per line
otherwise (recorded, kind, resource, inbox, visibility, event ID).

The stream is your durable subscription named --consumer (default
heo-cli). Each batch is acknowledged once its lines are written, which
moves the subscription's checkpoint past it, so rerunning with the same
consumer resumes after the last line written: nothing is skipped,
and an event may repeat only if a watch stopped between writing a batch
and acknowledging it (consumers deduplicate by event ID). Ctrl-C or
SIGTERM acknowledges what was written, prints the checkpoint on stderr
and exits 0.

A new consumer starts at --from tail (default: events from now on) or
beginning (the oldest retained), or at --cursor, a checkpoint printed
by an earlier watch. An open consumer always resumes at its own
checkpoint: a --cursor it is not at would skip or repeat events, and is
refused (CONFLICT, exit 4). A position behind event retention is
refused (CURSOR_EXPIRED, exit 4): start a new consumer and reconcile
from current state.

With no daemon running the watch fails at once, PREREQUISITE (exit 70).
Once attached it waits out a daemon that stops answering, a restart
say, for --reconnect-timeout (default 30s) of continuous unavailability,
saying so once on stderr, and resumes where it was; past that it fails
with exit 70. --reconnect-timeout 0 fails at once.

Every event is authorized again, one by one, for the caller: content
that is not public reaches desk actors alone. Reading roles: desk,
viewer, external. Not served over REST, SSE, gRPC or MCP.`,
		Example: `  heo event watch
  heo event watch --consumer audit --from beginning --format json
  heo event watch --consumer replay --cursor pg.42`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := watchStartOf(cmd)
			if err != nil {
				return err
			}
			if raw, _ := cmd.Flags().GetString(flagStep); raw != "" {
				return a.watchStepRun(cmd, args, start, raw)
			}
			if cli.IsDryRun(cmd) {
				return cli.RenderPlan(cmd, watchPlan(start))
			}
			ctx := cmd.Context()
			route, _ := control.RouteOf(ctx, a.control.servesControl())
			var (
				step      stepper
				reconnect time.Duration
			)
			switch route {
			case control.Local:
				// A served call reached the daemon without --step: the
				// loop would hold the daemon's one served call for as
				// long as the caller follows.
				return output.UsageError("event watch streams until interrupted and does not run inside the daemon: " +
					"run `" + a.heoCommand("event watch") + "` at the terminal, which drives it over the control socket one step at a time")
			case control.Forward:
				var err error
				if reconnect, err = reconnectOf(cmd); err != nil {
					return err
				}
				var stop context.CancelFunc
				ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
				step = a.remoteStep(cmd, args)
			default:
				return a.notDaemon()
			}
			return a.watch(ctx, cmd, step, start, reconnect)
		},
	}
	c.Flags().String(flagConsumer, desk.DefaultConsumer, "Name of your durable subscription; reuse it to resume")
	c.Flags().String(flagCursor, "", "Checkpoint a new consumer starts at, printed by an earlier watch")
	c.Flags().String(flagFrom, desk.StartTail, "Where a new consumer starts: tail (from now on) or beginning (oldest retained)")
	addReconnectFlag(c)
	c.Flags().String(flagStep, "", "One step of a watch driven over the control socket (heo's own client)")
	_ = c.Flags().MarkHidden(flagStep)
	daemonCommand(c)
	// A watch runs until its caller stops following, and kit serves one
	// call at a time: on REST, SSE, gRPC or MCP it would hold the
	// daemon, and an MCP tool call never returns. It is withheld from
	// all four and stays on the control socket, where the terminal's
	// watch drives it as short --step calls.
	cli.SetWithholdFrom(c, cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &model.Event{}, "1.0")
	return c
}

// watchStartOf reads --consumer, --cursor and --from.
func watchStartOf(cmd *cobra.Command) (desk.WatchStart, error) {
	var w desk.WatchStart
	w.Consumer, _ = cmd.Flags().GetString(flagConsumer)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	w.Cursor = model.Cursor(cursor)
	w.From, _ = cmd.Flags().GetString(flagFrom)
	if w.From != desk.StartTail && w.From != desk.StartBeginning {
		return w, output.UsageError(fmt.Sprintf("--%s %q: want %s or %s", flagFrom, w.From, desk.StartTail, desk.StartBeginning))
	}
	if cursor != "" && cmd.Flags().Changed(flagFrom) {
		return w, output.UsageError(fmt.Sprintf("--%s and --%s both say where to start: pass one", flagCursor, flagFrom))
	}
	if cursor != "" {
		w.From = ""
	}
	return w, nil
}

// watchPlan renders a watch for --dry-run: it opens nothing.
func watchPlan(w desk.WatchStart) cli.Plan {
	start := "resume at its checkpoint when open; otherwise start at " + w.From
	if w.Cursor != "" {
		start = "start at checkpoint " + string(w.Cursor) + " (refused if open elsewhere)"
	}
	return cli.Plan{
		Args: map[string]any{"consumer": w.Consumer, "start": start},
		Effects: []cli.Effect{planEffect("update", "subscription:"+w.Consumer, true,
			"open or resume the subscription; stream events and acknowledge each batch once its lines are written")},
		PrerequisitesChecked: []string{"flags"},
	}
}

// watchStepRun runs one step for heo's own client and prints its answer.
func (a *app) watchStepRun(cmd *cobra.Command, args []string, start desk.WatchStart, raw string) error {
	var st watchStep
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return output.UsageError("--" + flagStep + ": " + err.Error())
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (watchStepOut, error) {
		return a.runStep(ctx, c, start, st)
	})
	if err != nil {
		return err
	}
	return output.Dispatch(cmd, a.r.Viper, res)
}

// runStep runs one watch step in the daemon for c, authorized again by
// heo's policy, and by uma for every event.
func (a *app) runStep(ctx context.Context, c control.Caller, start desk.WatchStart, st watchStep) (watchStepOut, error) {
	var out watchStepOut
	sub := model.SubscriptionRef{Tenant: c.Tenant, ID: st.Subscription}
	var action string
	switch st.Op {
	case stepOpen:
		action = model.ActionSubscriptionOpen
	case stepPoll:
		action = model.ActionSubscriptionPoll
	case stepAck:
		action = model.ActionSubscriptionAck
	default:
		return out, output.UsageError(fmt.Sprintf("--%s: unknown step %q", flagStep, st.Op))
	}
	err := a.desk(ctx, c, action, model.Audience{}, func(s desk.Service) error {
		switch st.Op {
		case stepOpen:
			op, err := id.New(operationKeyPrefix)
			if err != nil {
				return err
			}
			got, err := s.OpenWatch(ctx, start, model.OperationID(op))
			if err != nil {
				return a.watchFailure(err, start)
			}
			out.Subscription, out.Checkpoint = got.Ref.ID, got.Checkpoint
		case stepPoll:
			b, err := s.PollWatch(ctx, sub, 0)
			if err != nil {
				return a.watchFailure(err, start)
			}
			out.Subscription, out.Events, out.Token = sub.ID, b.Events, b.Token
		case stepAck:
			got, err := s.AckWatch(ctx, sub, st.Token)
			if err != nil {
				return err
			}
			out.Subscription, out.Checkpoint = got.Ref.ID, got.Checkpoint
		}
		return nil
	})
	if out.Events == nil {
		out.Events = []model.Event{}
	}
	return out, err
}

// watchFailure explains a watch that cannot start or go on: a gap, or a
// position behind event retention.
func (a *app) watchFailure(err error, w desk.WatchStart) error {
	var gap *desk.GapError
	switch {
	case errors.As(err, &gap):
		e := errmap.Of(err)
		e.SuggestedFix = fmt.Sprintf("drop --%s to resume %s at %s, or pass a new --%s to start at %s",
			flagCursor, gap.Consumer, gap.At, flagConsumer, gap.Asked)
		return e
	case model.IsCode(err, model.CursorExpired):
		return a.expiredCursor(err, "start a new consumer: `"+a.heoCommand("event watch --consumer <new> --from beginning")+
			"` (oldest retained) or --from tail")
	}
	return err
}

// expiredCursor explains a position behind event retention, for a watch
// and a list alike: the events after it were purged, so nothing resumes
// there. start is how to start over; the reconcile path is the same.
func (a *app) expiredCursor(err error, start string) error {
	e := errmap.Of(err)
	e.Message = "the events after this position were purged by event retention: " + e.Message
	e.SuggestedFix = start + ", then reconcile from current state with `" +
		a.heoCommand("conversation list") + "` and `" + a.heoCommand("delivery get <operation>") + "`"
	return e
}

// remoteStep runs each step in the daemon over the control socket, as
// heo's own client: the watch's flags, the step, json.
func (a *app) remoteStep(cmd *cobra.Command, args []string) stepper {
	return func(ctx context.Context, st watchStep) (watchStepOut, error) {
		raw, err := json.Marshal(st)
		if err != nil {
			return watchStepOut{}, err
		}
		req := control.RequestFor(cmd, args)
		req.Flags[flagStep] = string(raw)
		res, err := a.controlClient().Invoke(ctx, req)
		if err != nil {
			return watchStepOut{}, err
		}
		return control.Decode[watchStepOut](res)
	}
}

// watch streams the subscription w names through step until ctx ends:
// open it, then lease each batch, write its lines and only then
// acknowledge it. A watch stopped by its context acknowledges the batch
// it wrote, prints where it stopped and succeeds.
func (a *app) watch(ctx context.Context, cmd *cobra.Command, step stepper, w desk.WatchStart, reconnect time.Duration) error {
	step = retryTransient(step, reconnect, cmd.ErrOrStderr())
	opened, err := step(ctx, watchStep{Op: stepOpen})
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	checkpoint := opened.Checkpoint
	asJSON := a.r.Viper.GetString("format") == output.JSON
	out := cmd.OutOrStdout()
	for ctx.Err() == nil {
		b, err := step(ctx, watchStep{Op: stepPoll, Subscription: opened.Subscription})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		if err := writeEvents(out, b.Events, asJSON); err != nil {
			return err
		}
		if b.Token == "" {
			select {
			case <-ctx.Done():
			case <-time.After(watchIdle):
			}
			continue
		}
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), watchAckTime)
		acked, err := step(actx, watchStep{Op: stepAck, Subscription: opened.Subscription, Token: b.Token})
		cancel()
		if err != nil {
			return err
		}
		checkpoint = acked.Checkpoint
	}
	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "stopped: consumer %s at checkpoint %s; rerun with --%s %s to resume\n",
		w.Consumer, orDash(string(checkpoint)), flagConsumer, w.Consumer)
	return err
}

// writeEvents writes one line per event: its JSON, or its row.
func writeEvents(w io.Writer, evs []model.Event, asJSON bool) error {
	for _, ev := range evs {
		var line []byte
		if asJSON {
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			line = b
		} else {
			line = []byte(eventRow(ev))
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// eventRow is an event's text row: recorded, kind, resource, inbox,
// visibility, event ID.
func eventRow(ev model.Event) string {
	return strings.Join([]string{
		stamp(ev.RecordedAt), ev.Kind, string(ev.Resource.Kind) + ":" + string(ev.Resource.ID),
		orDash(string(ev.Inbox)), orDash(string(ev.Audience.Visibility)), string(ev.ID),
	}, "\t")
}
