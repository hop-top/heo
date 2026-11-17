package hooks

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"hop.top/uma/model"
)

// ListEvents reads uma's event log page by page, as a plugin's actor.
type ListEvents func(context.Context, model.ListEvents) (model.Page[model.Event], error)

// FromBeginning is the --from of a replay that starts at the retained
// beginning of the log rather than at a position.
const FromBeginning = "beginning"

// TestFailure is why a test run's plugin did not answer.
type TestFailure struct {
	Class     Class  `json:"class" yaml:"class"`
	Permanent bool   `json:"permanent" yaml:"permanent"`
	Message   string `json:"message" yaml:"message"`
}

// TestReport is what running a plugin on one stored event showed. Nothing
// in it was applied.
type TestReport struct {
	Plugin string `json:"plugin" yaml:"plugin"`
	Event  string `json:"event" yaml:"event"`
	Kind   string `json:"kind" yaml:"kind"`
	// Admitted is whether the plugin would be delivered the event: its
	// actor may read the audience and its filter passes. Skipped says why
	// not, and the plugin was not run.
	Admitted bool   `json:"admitted" yaml:"admitted"`
	Skipped  string `json:"skipped,omitempty" yaml:"skipped,omitempty"`
	// Notes are things worth knowing that did not stop the run, such as
	// an event kind the plugin's filter would not subscribe to.
	Notes []string `json:"notes,omitempty" yaml:"notes,omitempty"`
	// Ok is whether the plugin answered with a result.
	Ok      bool         `json:"ok" yaml:"ok"`
	Failure *TestFailure `json:"failure,omitempty" yaml:"failure,omitempty"`
	Message string       `json:"message,omitempty" yaml:"message,omitempty"`
	// Operations are the result's operations as they would be applied.
	Operations []PlannedOp `json:"operations" yaml:"operations"`
	// Rejected is why applying the result would reject it whole.
	Rejected string `json:"rejected,omitempty" yaml:"rejected,omitempty"`
	// WouldApply is whether delivering the event would apply the result:
	// it is valid and every operation passes Check. Authorization and
	// the verb's own refusals are decided only when it runs.
	WouldApply bool `json:"would_apply" yaml:"would_apply"`
	// Applied is always false: a test applies nothing.
	Applied bool `json:"applied" yaml:"applied"`
}

// Test runs the plugin on one stored event, read as the plugin's actor,
// and reports its result without applying it: no operation is
// dispatched, no dead letter or attempt count is written. A plugin that
// fails is the report's Failure, not an error; an event the plugin could
// not be delivered is reported Skipped and the plugin is not run.
func (r *Runner) Test(ctx context.Context, id string) (rep TestReport, err error) {
	p := r.o.Plugin
	rep = TestReport{Plugin: p.Name, Event: id, Operations: []PlannedOp{}}
	if r.o.Fetch == nil {
		return rep, errors.New("an event cannot be tested: no event reader configured")
	}
	ev, err := r.o.Fetch(ctx, model.ID(id))
	if err != nil {
		return rep, fmt.Errorf("read event %s: %w", id, err)
	}
	rep.Kind = ev.Kind
	if len(p.Filter.Kinds) > 0 && !slices.Contains(p.Filter.Kinds, ev.Kind) {
		rep.Notes = append(rep.Notes, fmt.Sprintf("%s is not among the kinds %s subscribes to: it would never be delivered this event", ev.Kind, p.Name))
	}
	rep.Admitted, rep.Skipped, err = r.admitReason(ctx, ev)
	if err != nil || !rep.Admitted {
		return rep, err
	}
	defer func() {
		if p := recover(); p != nil {
			rep.Ok, rep.Failure = false, &TestFailure{Class: ClassError, Message: fmt.Sprintf("panic: %v", p)}
			err = nil
		}
	}()
	res, runErr := r.o.Exec.Run(ctx, NewEnvelope(p.Name, ev, 1, r.now()))
	if runErr != nil {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.Failure = &TestFailure{Class: Classify(runErr), Permanent: IsPermanent(runErr), Message: runErr.Error()}
		return rep, nil
	}
	rep.Ok, rep.Message = true, res.Message
	d := Delivery{Plugin: p.Name, Event: ev, Attempt: 1, DryRun: true}
	if pl, ok := r.o.Apply.(Planner); ok {
		rep.Operations, rep.Rejected = pl.Plan(d, res)
	} else {
		for i, op := range res.Operations {
			rep.Operations = append(rep.Operations, PlannedOp{Index: i, Verb: op.Verb, Args: op.Args, Flags: op.Flags,
				Key: OperationKey(p.Name, ev.ID, op), Allowed: true})
		}
	}
	rep.WouldApply = rep.Rejected == ""
	return rep, nil
}

// admitReason is admits with the reason an event is passed over.
func (r *Runner) admitReason(ctx context.Context, ev model.Event) (bool, string, error) {
	sees := r.o.Sees
	if sees == nil {
		sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	}
	if !sees(ev.Audience) {
		return false, "the plugin's actor may not read the event's audience", nil
	}
	if r.o.Match != nil {
		ok, err := r.o.Match(ctx, ev)
		if err != nil || !ok {
			return false, "the event is outside the plugin's filter", err
		}
	}
	return true, "", nil
}

// Replay outcomes of one event.
const (
	ReplayDelivered = "delivered"
	ReplaySkipped   = "skipped"
	ReplayFailed    = "failed"
	// ReplayWould is an event a dry run would deliver.
	ReplayWould = "would_deliver"
)

// ReplayEvent is what a replay did with one event.
type ReplayEvent struct {
	Event   string `json:"event" yaml:"event"`
	Kind    string `json:"kind" yaml:"kind"`
	Outcome string `json:"outcome" yaml:"outcome"`
	Class   Class  `json:"class,omitempty" yaml:"class,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
}

// ReplayReport is what ReplayFrom did.
type ReplayReport struct {
	Plugin string `json:"plugin" yaml:"plugin"`
	From   string `json:"from" yaml:"from"`
	// Next is the log position after the last event handled: pass it as
	// the next --from to continue. Complete is set when the log held no
	// more events at that position.
	Next      model.Cursor  `json:"next" yaml:"next"`
	Complete  bool          `json:"complete" yaml:"complete"`
	DryRun    bool          `json:"dry_run" yaml:"dry_run"`
	Scanned   int           `json:"scanned" yaml:"scanned"`
	Delivered int           `json:"delivered" yaml:"delivered"`
	Skipped   int           `json:"skipped" yaml:"skipped"`
	Failed    int           `json:"failed" yaml:"failed"`
	Events    []ReplayEvent `json:"events" yaml:"events"`
}

// ReplayFrom delivers the events of uma's log from a position to the
// plugin again, read as the plugin's actor with its filter, at most
// limit of them, oldest first. It is a bounded walk of the log, never a
// move of the plugin's durable subscription: that stays where it is, and
// uma offers no way to seek it. from is a log position (the next of
// `event list`, a hooks status cursor) or FromBeginning.
//
// Each event is delivered as the runner delivers one, once: its
// operations carry the keys of the first delivery, so those already
// applied replay as no-ops. A failure is recorded as a dead letter, so
// `dead-letter retry` and `drop` act on it, and the walk goes on. Events
// the plugin's audience rule or filter passes over are skipped. A dry
// run reads the log and runs nothing. The walk runs beside the live
// subscription: ordering with events delivered now is not kept.
func (r *Runner) ReplayFrom(ctx context.Context, from string, limit int, dry bool) (ReplayReport, error) {
	p := r.o.Plugin
	rep := ReplayReport{Plugin: p.Name, From: from, DryRun: dry, Events: []ReplayEvent{}}
	if r.o.List == nil {
		return rep, errors.New("events cannot be replayed from a position: no event lister configured")
	}
	if from == "" {
		return rep, errors.New("a replay starts from a log position or \"" + FromBeginning + "\"")
	}
	if limit < 1 {
		return rep, fmt.Errorf("limit %d: want 1 or more", limit)
	}
	filter := model.Filter{Kinds: p.Filter.Kinds}
	if r.o.Resolve != nil {
		f, err := r.o.Resolve(ctx)
		if err != nil {
			return rep, err
		}
		filter = f
	}
	req := model.ListEvents{Call: model.Call{Correlation: r.correlation()}, Filter: filter, Start: model.StartCheckpoint, After: model.Cursor(from)}
	if from == FromBeginning {
		req.Start, req.After = model.StartBeginning, ""
	}
	rep.Next = model.Cursor(from)
	for handled := 0; handled < limit; {
		req.Limit = limit - handled
		pg, err := r.o.List(ctx, req)
		if err != nil {
			return rep, err
		}
		for _, ev := range pg.Items {
			rep.Scanned++
			handled++
			rep.Events = append(rep.Events, r.replayOne(ctx, ev, dry, &rep))
		}
		rep.Next, rep.Complete = pg.Next, pg.Complete
		if pg.Complete || pg.Next == "" {
			rep.Complete = true
			break
		}
		req.Start, req.After = model.StartCheckpoint, pg.Next
	}
	return rep, nil
}

func (r *Runner) replayOne(ctx context.Context, ev model.Event, dry bool, rep *ReplayReport) ReplayEvent {
	out := ReplayEvent{Event: string(ev.ID), Kind: ev.Kind}
	ok, why, err := r.admitReason(ctx, ev)
	switch {
	case err != nil:
		rep.Failed++
		out.Outcome, out.Message = ReplayFailed, err.Error()
		return out
	case !ok:
		rep.Skipped++
		out.Outcome, out.Message = ReplaySkipped, why
		return out
	case dry:
		out.Outcome = ReplayWould
		return out
	}
	now := r.now().UTC()
	if err := r.attempt(ctx, ev, 1); err != nil {
		rep.Failed++
		out.Outcome, out.Message, out.Class = ReplayFailed, err.Error(), Classify(err)
		r.failed(ev, err)
		if r.o.Dead != nil {
			idx, has := FailureIndex(err)
			var index *int
			if has {
				index = &idx
			}
			dl := DeadLetter{Plugin: r.o.Plugin.Name, Consumer: r.o.Plugin.Consumer(), Event: ev, Attempts: 1, Reason: err.Error(),
				Class: out.Class, FailureIndex: index, Permanent: IsPermanent(err), FirstAt: now, At: r.now().UTC()}
			if derr := r.o.Dead.Record(ctx, dl); derr != nil {
				out.Message += "; not recorded as a dead letter: " + derr.Error()
			}
		}
		return out
	}
	rep.Delivered++
	out.Outcome = ReplayDelivered
	return out
}
