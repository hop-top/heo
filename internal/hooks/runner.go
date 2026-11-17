package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/plugins"
	"hop.top/uma/model"
)

// Defaults.
const (
	// DefaultPoll is how long one poll waits for events.
	DefaultPoll = 5 * time.Second
	// DefaultBatch is how many events one poll reads. A batch is
	// acknowledged as a whole, so a small one bounds how long a slow
	// plugin holds its lease.
	DefaultBatch = 20
	// DefaultRetryMin and DefaultRetryMax bound the wait between failed
	// attempts to talk to uma.
	DefaultRetryMin = time.Second
	DefaultRetryMax = time.Minute
	// ackGrace bounds acknowledging a batch finished as the runner is
	// told to stop.
	ackGrace = 10 * time.Second
)

// State is where a runner stands.
type State string

// States.
const (
	// StateStarting: the subscription is not open yet.
	StateStarting State = "starting"
	// StateReady: the runner reads the log and delivers what it finds.
	StateReady State = "ready"
	// StateDegraded: the last attempt to read or acknowledge failed; the
	// runner retries with a backoff, and acknowledges nothing it has not
	// handled.
	StateDegraded State = "degraded"
)

// Resolve turns the plugin's filter into uma's, at each open: inbox
// keys become IDs. A failure is retried like any other.
type Resolve func(ctx context.Context) (model.Filter, error)

// Match is the part of a plugin's filter uma does not honour, checked
// on each event the plugin's actor may read. An event it refuses is
// acknowledged with its batch, undelivered.
type Match func(ctx context.Context, ev model.Event) (bool, error)

// Options configures a Runner.
type Options struct {
	Plugin plugins.Plugin
	// Source is uma's event service as the plugin's own actor.
	Source Source
	// Resolve gives the filter uma fixes at open; nil is the plugin's
	// kinds alone.
	Resolve Resolve
	// Match checks the rest of the filter; nil admits every event.
	Match Match
	// Sees reports whether the plugin's actor may read content under an
	// audience, checked again on each event; nil admits public events
	// only.
	Sees func(model.Audience) bool
	Exec Executor
	// Apply defaults to DiscardApplier and Dead to a recorder that
	// refuses nothing and keeps nothing.
	Apply Applier
	Dead  DeadLetterer
	// Store, when set, is the durable dead-letter list and attempt
	// count: it is the Dead letterer unless Dead is set, and what a
	// restart reads attempts back from. Nil keeps attempts in memory.
	Store Store
	// Lineage, when set, tracks the plugin's consumers so that a changed
	// filter opens the new consumer at the old one's checkpoint and then
	// closes the old; nil opens at the tail.
	Lineage LineageStore
	// Fetch reads an event for Replay and Test; nil refuses them.
	Fetch Fetch
	// List reads uma's event log as the plugin's actor, from a log
	// position, for ReplayFrom; nil refuses it. uma.Events.List
	// satisfies it.
	List ListEvents
	// Publisher receives the plugin's health events
	// (heo.runtime.hooks.<action>); nil publishes none.
	Publisher bridge.Publisher
	// Jitter draws the backoff's jitter: a value in [0, n). Nil takes
	// math/rand.
	Jitter func(n int64) int64
	// Poll, Batch, RetryMin and RetryMax default as above.
	Poll, RetryMin, RetryMax time.Duration
	Batch                    int
	Logger                   Logger
	// OnChange, when set, is called after a change of State, on the
	// runner's goroutine; it must not block.
	OnChange func(Status)
	// Now defaults to time.Now.
	Now func() time.Time
}

// LastError is the last failure of a delivery: attempts that were
// retried count, as do dead letters.
type LastError struct {
	Event   string    `json:"event" yaml:"event"`
	At      time.Time `json:"at" yaml:"at"`
	Message string    `json:"message" yaml:"message"`
}

// Status is one plugin's state, for `service status`. Positions in
// uma's log are opaque, so Lag is time: how long the oldest event of
// the batch in hand has waited since uma recorded it, zero while the
// plugin has caught up.
type Status struct {
	Plugin   string `json:"plugin" yaml:"plugin"`
	Consumer string `json:"consumer" yaml:"consumer"`
	Mode     string `json:"mode" yaml:"mode"`
	State    State  `json:"state" yaml:"state"`
	// Code is the code of the last failure to reach uma while degraded.
	Code string `json:"code,omitempty" yaml:"code,omitempty"`
	// Cursor is the subscription's last acknowledged position.
	Cursor model.Cursor `json:"cursor,omitempty" yaml:"cursor,omitempty"`
	// Lag is how far behind the log the plugin is; InFlight the events
	// of the batch in hand not yet handled.
	Lag      time.Duration `json:"lag" yaml:"lag"`
	InFlight int           `json:"in_flight" yaml:"in_flight"`
	// Delivered counts events applied, DeadLettered those recorded as
	// failed, Skipped those the plugin's filter or audience passed over,
	// since the runner started.
	Delivered    uint64     `json:"delivered" yaml:"delivered"`
	DeadLettered uint64     `json:"dead_lettered" yaml:"dead_lettered"`
	Skipped      uint64     `json:"skipped" yaml:"skipped"`
	LastEvent    string     `json:"last_event,omitempty" yaml:"last_event,omitempty"`
	LastEventAt  time.Time  `json:"last_event_at,omitzero" yaml:"last_event_at,omitempty"`
	LastError    *LastError `json:"last_error,omitempty" yaml:"last_error,omitempty"`
}

// Runner reads one plugin's subscription and delivers its events. It is
// at-least-once towards the plugin.
type Runner struct {
	o   Options
	now func() time.Time

	mu     sync.Mutex
	status Status
	opens  int

	// sub, unacked and acked belong to the Run goroutine alone: the open
	// subscription, and the token of a batch handled but not yet
	// acknowledged.
	sub     model.SubscriptionRef
	unacked string
}

// New returns a Runner.
func New(o Options) *Runner {
	r := &Runner{o: o, now: o.Now}
	if r.now == nil {
		r.now = time.Now
	}
	if r.o.Poll <= 0 {
		r.o.Poll = DefaultPoll
	}
	if r.o.Batch <= 0 {
		r.o.Batch = DefaultBatch
	}
	if r.o.RetryMin <= 0 {
		r.o.RetryMin = DefaultRetryMin
	}
	if r.o.RetryMax < r.o.RetryMin {
		r.o.RetryMax = max(DefaultRetryMax, r.o.RetryMin)
	}
	if r.o.Apply == nil {
		r.o.Apply = DiscardApplier{}
	}
	if r.o.Dead == nil && r.o.Store != nil {
		r.o.Dead = r.o.Store
	}
	if r.o.Dead == nil {
		r.o.Dead = discardDead{}
	}
	r.status = Status{Plugin: o.Plugin.Name, Consumer: o.Plugin.Consumer(), Mode: o.Plugin.Mode, State: StateStarting}
	return r
}

type discardDead struct{}

func (discardDead) Record(context.Context, DeadLetter) error { return nil }

// Status returns the runner's state.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Runner) correlation() model.CorrelationID {
	return model.CorrelationID("heo-hook-" + r.o.Plugin.Name)
}

// Start opens the subscription now, at the tail when it is new, so that
// events committed after it returns are read even when the first poll
// comes later. A failure is not final: Run opens it again.
func (r *Runner) Start(ctx context.Context) error { return r.open(ctx) }

// Run reads and delivers until ctx ends. A failure of uma never ends
// it: the runner reports itself degraded and retries after a growing
// delay. When ctx ends, a batch in hand is left unacknowledged and is
// read again on the next start.
func (r *Runner) Run(ctx context.Context) {
	// Run has no error return; a close failure is not actionable here.
	defer func() { _ = r.exec().Close() }()
	delay := r.o.RetryMin
	for ctx.Err() == nil {
		err := r.step(ctx)
		if err == nil {
			delay = r.o.RetryMin
			continue
		}
		if ctx.Err() != nil {
			return
		}
		r.degrade(err)
		if !sleep(ctx, delay) {
			return
		}
		delay = min(delay*2, r.o.RetryMax)
	}
}

func (r *Runner) exec() Executor { return r.o.Exec }

// step makes one attempt: open the subscription if it is not, finish
// acknowledging a batch already handled, or poll one batch, handle it
// and acknowledge it.
func (r *Runner) step(ctx context.Context) (err error) {
	defer func() {
		// A plugin's trouble is the plugin's: a panic anywhere in a
		// delivery is a failed attempt, never the daemon's.
		if p := recover(); p != nil {
			err = fmt.Errorf("hook runner panic: %v", p)
		}
	}()
	if r.sub == (model.SubscriptionRef{}) {
		if err := r.open(ctx); err != nil {
			return err
		}
	}
	if r.unacked != "" {
		return r.ack(ctx)
	}
	batch, err := r.o.Source.Poll(ctx, model.Poll{
		Call: model.Call{Correlation: r.correlation()}, Subscription: r.sub, Limit: r.o.Batch, Wait: r.o.Poll,
	})
	if err != nil {
		return r.pollFailed(ctx, err)
	}
	if batch.AckToken == "" {
		r.idle()
		return nil
	}
	if err := r.handle(ctx, batch.Events); err != nil {
		return err
	}
	r.unacked = batch.AckToken
	return r.ack(ctx)
}

func (r *Runner) open(ctx context.Context) error {
	filter := model.Filter{Kinds: r.o.Plugin.Filter.Kinds}
	if r.o.Resolve != nil {
		f, err := r.o.Resolve(ctx)
		if err != nil {
			return err
		}
		filter = f
	}
	var (
		res     model.SubscriptionResult
		retired []LineageEntry
		err     error
	)
	if r.o.Lineage != nil {
		res, retired, err = r.handover(ctx, filter)
	} else {
		res, err = r.openConsumer(ctx, r.o.Plugin.Consumer(), filter, model.StartTail, "")
	}
	if err != nil {
		return err
	}
	r.sub = res.Subscription.Ref
	r.unacked = ""
	r.mu.Lock()
	r.status.Cursor = res.Subscription.Checkpoint
	r.mu.Unlock()
	r.retire(ctx, retired)
	r.ready()
	return nil
}

func (r *Runner) openConsumer(ctx context.Context, name string, filter model.Filter, start string, after model.Cursor) (model.SubscriptionResult, error) {
	r.mu.Lock()
	r.opens++
	op := model.OperationID(fmt.Sprintf("hook-open-%s-%d-%d", r.o.Plugin.Name, r.now().UnixNano(), r.opens))
	r.mu.Unlock()
	return r.o.Source.Open(ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: r.correlation()}, Operation: op},
		Consumer: name, Filter: filter, Start: start, After: after, PayloadMajor: model.PayloadVersion1,
	})
}

// handover opens the plugin's consumer, at the checkpoint of the one it
// replaces when the filter changed. The consumers are tracked in the
// plugin's Lineage, written before the step it guards, so that a crash
// at any point leaves every consumer either tracked or not yet
// created, and the next start finishes the handover:
//
//  1. the new consumer is recorded as active;
//  2. the previous one is reopened (an open consumer keeps its
//     position) for its checkpoint, and the new one is opened there;
//  3. the previous ones are recorded as retiring;
//  4. the caller closes them, then clears the record.
//
// A crash after 2 reopens the new consumer, which keeps its position,
// and the old one still holds the events it has not passed. Events
// between the two checkpoints may be delivered twice, never skipped.
func (r *Runner) handover(ctx context.Context, filter model.Filter) (model.SubscriptionResult, []LineageEntry, error) {
	name, plugin := r.o.Plugin.Consumer(), r.o.Plugin.Name
	st, err := r.o.Lineage.LoadLineage(ctx, plugin)
	if err != nil {
		return model.SubscriptionResult{}, nil, err
	}
	var others []LineageEntry
	for _, e := range st.Active {
		if e.Consumer != name {
			others = append(others, e)
		}
	}
	cur := LineageEntry{Consumer: name, Filter: filter}
	want := Lineage{Active: append(slices.Clone(others), cur), Retiring: st.Retiring}
	if !lineageEqual(st, want) {
		if err := r.o.Lineage.SaveLineage(ctx, plugin, want); err != nil {
			return model.SubscriptionResult{}, nil, err
		}
	}
	start, after := model.StartTail, model.Cursor("")
	retired := slices.DeleteFunc(slices.Clone(st.Retiring), func(e LineageEntry) bool { return e.Consumer == name })
	for i, o := range others {
		prev, err := r.openConsumer(ctx, o.Consumer, o.Filter, model.StartTail, "")
		if err != nil {
			return model.SubscriptionResult{}, nil, fmt.Errorf("hooks: reopen %s for handover: %w", o.Consumer, err)
		}
		o.Ref = prev.Subscription.Ref
		retired = append(retired, o)
		if i == len(others)-1 && prev.Subscription.Checkpoint != "" {
			start, after = model.StartCheckpoint, prev.Subscription.Checkpoint
		}
	}
	res, err := r.openConsumer(ctx, name, filter, start, after)
	if err != nil {
		return model.SubscriptionResult{}, nil, err
	}
	if len(others) > 0 {
		done := Lineage{Active: []LineageEntry{cur}, Retiring: retired}
		if err := r.o.Lineage.SaveLineage(ctx, plugin, done); err != nil {
			return model.SubscriptionResult{}, nil, err
		}
	}
	return res, retired, nil
}

func lineageEqual(a, b Lineage) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// retire closes consumers a handover superseded, then clears the
// record. A consumer uma has closed or does not know is done. A failure
// leaves the record, and the close is tried again at the next open.
func (r *Runner) retire(ctx context.Context, list []LineageEntry) {
	if len(list) == 0 {
		return
	}
	for _, e := range list {
		_, err := r.o.Source.Close(ctx, model.Lookup[model.SubscriptionRef]{Call: model.Call{Correlation: r.correlation()}, Ref: e.Ref},
			model.Command{Call: model.Call{Correlation: r.correlation()}, Operation: r.closeOperation()})
		if err != nil && model.CodeOf(err) != model.Conflict && model.CodeOf(err) != model.NotFound {
			r.report("hooks: closing a superseded consumer failed; retrying at the next open", "plugin", r.o.Plugin.Name, "consumer", e.Consumer, "err", err)
			return
		}
	}
	st, err := r.o.Lineage.LoadLineage(ctx, r.o.Plugin.Name)
	if err == nil {
		st.Retiring = nil
		err = r.o.Lineage.SaveLineage(ctx, r.o.Plugin.Name, st)
	}
	if err != nil {
		r.report("hooks: recording the handover failed", "plugin", r.o.Plugin.Name, "err", err)
	}
}

// pollFailed handles a failed poll. A checkpoint behind event retention
// is closed and reopened at the tail: the events in the gap are gone,
// uma says. A subscription closed behind the runner's back is reopened.
func (r *Runner) pollFailed(ctx context.Context, err error) error {
	switch model.CodeOf(err) {
	case model.CursorExpired:
		r.report("hooks: checkpoint behind event retention; reopening at the tail", "plugin", r.o.Plugin.Name)
		r.degrade(err)
		_, cerr := r.o.Source.Close(ctx, model.Lookup[model.SubscriptionRef]{Call: model.Call{Correlation: r.correlation()}, Ref: r.sub},
			model.Command{Call: model.Call{Correlation: r.correlation()}, Operation: r.closeOperation()})
		if cerr != nil && model.CodeOf(cerr) != model.Conflict {
			return cerr
		}
		r.sub = model.SubscriptionRef{}
		return nil
	case model.Conflict, model.NotFound:
		r.sub = model.SubscriptionRef{}
	}
	return err
}

func (r *Runner) closeOperation() model.OperationID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens++
	return model.OperationID(fmt.Sprintf("hook-close-%s-%d-%d", r.o.Plugin.Name, r.now().UnixNano(), r.opens))
}

// ack acknowledges the handled batch. An acknowledgement uma refuses as
// a conflict (the lease ended) leaves the batch to be read again, its
// applied events replaying as no-ops; any other failure is retried with
// the same token without handling the batch again.
func (r *Runner) ack(ctx context.Context) error {
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackGrace)
	defer cancel()
	res, err := r.o.Source.Ack(work, model.Acknowledge{
		Command:      model.Command{Call: model.Call{Correlation: r.correlation()}, Operation: ackOperation(r.o.Plugin.Name, r.unacked)},
		Subscription: r.sub, Token: r.unacked,
	})
	switch {
	case err == nil:
		r.mu.Lock()
		r.status.Cursor = res.Subscription.Checkpoint
		r.mu.Unlock()
	case model.CodeOf(err) == model.Conflict:
		r.report("hooks: acknowledgement refused; the batch will be read again", "plugin", r.o.Plugin.Name)
	default:
		return err
	}
	r.unacked = ""
	r.idle()
	return nil
}

func ackOperation(plugin, token string) model.OperationID {
	sum := sha256.Sum256([]byte(plugin + "\x00" + token))
	return model.OperationID("hook-ack-" + hex.EncodeToString(sum[:16]))
}

// handle delivers a batch: each event the plugin's filter and audience
// admit, one conversation's events in order, several conversations at
// once up to the plugin's concurrency. It returns nil only when every
// event was applied or dead-lettered.
func (r *Runner) handle(ctx context.Context, events []model.Event) error {
	var lanes [][]model.Event
	index := map[string]int{}
	for _, ev := range events {
		ok, err := r.admits(ctx, ev)
		if err != nil {
			return err
		}
		if !ok {
			r.count(func(s *Status) { s.Skipped++ })
			continue
		}
		k := laneKey(ev)
		i, seen := index[k]
		if !seen {
			i = len(lanes)
			index[k] = i
			lanes = append(lanes, nil)
		}
		lanes[i] = append(lanes[i], ev)
	}
	if len(lanes) == 0 {
		return nil
	}
	pending := 0
	for _, l := range lanes {
		pending += len(l)
	}
	r.count(func(s *Status) {
		s.InFlight = pending
		if len(events) > 0 && !events[0].RecordedAt.IsZero() {
			s.Lag = r.now().Sub(events[0].RecordedAt)
		}
	})
	defer r.count(func(s *Status) { s.InFlight = 0 })

	work := make(chan []model.Event)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var first error
	fail := func(err error) {
		errMu.Lock()
		if first == nil {
			first = err
		}
		errMu.Unlock()
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workers := min(max(r.o.Plugin.Concurrency, 1), len(lanes))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lane := range work {
				for _, ev := range lane {
					if cctx.Err() != nil {
						return
					}
					if err := r.deliver(cctx, ev); err != nil {
						fail(err)
						cancel()
						return
					}
					r.count(func(s *Status) { s.InFlight-- })
				}
			}
		}()
	}
feed:
	for _, l := range lanes {
		select {
		case work <- l:
		case <-cctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

// laneKey names what an event is ordered with: its conversation, else
// its resource, else itself.
func laneKey(ev model.Event) string {
	if c := conversationOf(ev); c != "" {
		return "c:" + c
	}
	if ev.Resource.ID != "" {
		return "r:" + string(ev.Resource.Kind) + ":" + string(ev.Resource.ID)
	}
	return "e:" + string(ev.ID)
}

func (r *Runner) admits(ctx context.Context, ev model.Event) (bool, error) {
	sees := r.o.Sees
	if sees == nil {
		sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	}
	if !sees(ev.Audience) {
		return false, nil
	}
	if r.ownHeld(ev) {
		return false, nil
	}
	if r.o.Match == nil {
		return true, nil
	}
	return r.o.Match(ctx, ev)
}

// ownHeld reports whether ev says the plugin's own operation was held. A
// plugin that answers events by proposing operations would otherwise be
// handed the event of each proposal it makes and loop, with no human
// decision in between. Its operation's decision and expiry are
// delivered: a person acted, or the deadline passed, once each.
func (r *Runner) ownHeld(ev model.Event) bool {
	if ev.Kind != model.EventOperationHeld {
		return false
	}
	var p model.HeldOperationPayload
	if json.Unmarshal(ev.Payload.Data, &p) != nil {
		return false
	}
	return p.Proposer == r.o.Plugin.Actor()
}

// deliver hands one event to the plugin until it is applied, the plugin
// fails for good or the attempts run out; the last two are recorded as
// a dead letter. It returns an error only when the event was neither
// handled nor recorded, which keeps its batch unacknowledged.
//
// Attempts are counted in the Store, so an event redelivered after a
// restart continues its count instead of starting over; a plugin that
// crashes the daemon cannot retry one event forever.
func (r *Runner) deliver(ctx context.Context, ev model.Event) error {
	p := r.o.Plugin
	prior := r.loadAttempts(ctx, ev)
	first := prior.FirstAt
	// A count already at the budget (the daemon stopped before the
	// dead letter was recorded) still gets one last attempt.
	for attempt := min(prior.Attempts+1, max(p.Retry.MaxAttempts, 1)); ; attempt++ {
		started := r.now().UTC()
		if first.IsZero() {
			first = started
		}
		if attempt > 1 {
			// Written ahead: an attempt that takes the daemon down with
			// it still counts. The first is counted once it has failed.
			r.saveAttempts(ctx, ev, AttemptState{Attempts: attempt, FirstAt: first})
		}
		err := r.attempt(ctx, ev, attempt)
		if err == nil {
			r.clearAttempts(ctx, ev)
			if attempt > 1 {
				r.health(ctx, ActionRecovered, ev, Health{Attempt: attempt})
			}
			r.count(func(s *Status) {
				s.Delivered++
				s.LastEvent, s.LastEventAt = string(ev.ID), ev.RecordedAt
			})
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.failed(ev, err)
		class, perm := Classify(err), IsPermanent(err)
		idx, hasIdx := FailureIndex(err)
		var index *int
		if hasIdx {
			index = &idx
		}
		if !perm && attempt < p.Retry.MaxAttempts {
			wait := Backoff(p.Retry.Backoff, attempt, r.o.Jitter)
			r.saveAttempts(ctx, ev, AttemptState{Attempts: attempt, FirstAt: first})
			r.health(ctx, ActionRetry, ev, Health{Attempt: attempt, Class: class, FailureIndex: index, RetryInMs: wait.Milliseconds()})
			if !sleep(ctx, wait) {
				return ctx.Err()
			}
			continue
		}
		dl := DeadLetter{
			Plugin: p.Name, Consumer: p.Consumer(), Event: ev, Attempts: attempt, Reason: err.Error(),
			Class: class, FailureIndex: index, Permanent: perm, FirstAt: first, At: r.now().UTC(),
		}
		if derr := r.o.Dead.Record(ctx, dl); derr != nil {
			return fmt.Errorf("record dead letter for %s: %w", ev.ID, derr)
		}
		r.clearAttempts(ctx, ev)
		r.report("hooks: event dead-lettered", "plugin", p.Name, "event", string(ev.ID), "attempts", attempt, "class", string(class), "error", err)
		r.health(ctx, ActionDeadLettered, ev, Health{Attempt: attempt, Class: class, FailureIndex: index})
		r.count(func(s *Status) {
			s.DeadLettered++
			s.LastEvent, s.LastEventAt = string(ev.ID), ev.RecordedAt
		})
		return nil
	}
}

// Replay runs a dead-lettered event through the plugin once more, read
// again from uma as the plugin's actor. The operations carry the keys
// of the first run, so those already applied replay as no-ops. A replay
// that is handled resolves the record; one that fails leaves it dead,
// with the new attempt, class and message, and returns the failure.
func (r *Runner) Replay(ctx context.Context, id string) (DeadRecord, error) {
	p := r.o.Plugin
	if r.o.Store == nil || r.o.Fetch == nil {
		return DeadRecord{}, errors.New("dead letters cannot be replayed: no store or event reader configured")
	}
	rec, err := r.o.Store.Get(ctx, p.Name, id)
	if err != nil {
		return DeadRecord{}, err
	}
	if rec.State != StateDead {
		return rec, notDead(rec)
	}
	ev, err := r.o.Fetch(ctx, model.ID(id))
	if err != nil {
		return rec, fmt.Errorf("read event %s: %w", id, err)
	}
	attempt := rec.Attempts + 1
	if err := r.attempt(ctx, ev, attempt); err != nil {
		if ctx.Err() != nil {
			return rec, ctx.Err()
		}
		r.failed(ev, err)
		idx, hasIdx := FailureIndex(err)
		var index *int
		if hasIdx {
			index = &idx
		}
		dl := DeadLetter{
			Plugin: p.Name, Consumer: p.Consumer(), Event: ev, Attempts: attempt, Reason: err.Error(),
			Class: Classify(err), FailureIndex: index, Permanent: IsPermanent(err), FirstAt: rec.FirstAt, At: r.now().UTC(), Replay: true,
		}
		if derr := r.o.Store.Record(ctx, dl); derr != nil {
			return rec, fmt.Errorf("record dead letter for %s: %w", id, derr)
		}
		r.health(ctx, ActionDeadLettered, ev, Health{Attempt: attempt, Class: dl.Class, FailureIndex: index})
		rec, _ = r.o.Store.Get(ctx, p.Name, id)
		return rec, err
	}
	rec, err = r.o.Store.Resolve(ctx, p.Name, id, StateReplayed, r.now())
	if err != nil {
		return rec, err
	}
	r.health(ctx, ActionReplayed, ev, Health{Attempt: attempt})
	return rec, nil
}

func (r *Runner) health(ctx context.Context, action string, ev model.Event, h Health) {
	h.Plugin, h.Consumer, h.Event, h.MaxAttempts = r.o.Plugin.Name, r.o.Plugin.Consumer(), string(ev.ID), r.o.Plugin.Retry.MaxAttempts
	emit(ctx, r.o.Publisher, action, h)
}

// The attempt count is best effort: a store that cannot be read or
// written costs a restart its memory of the count, never the delivery.
func (r *Runner) loadAttempts(ctx context.Context, ev model.Event) AttemptState {
	if r.o.Store == nil {
		return AttemptState{}
	}
	s, err := r.o.Store.Load(ctx, r.o.Plugin.Name, string(ev.ID))
	if err != nil {
		r.report("hooks: attempt count unreadable; starting over", "plugin", r.o.Plugin.Name, "event", string(ev.ID), "error", err)
		return AttemptState{}
	}
	return s
}

func (r *Runner) saveAttempts(ctx context.Context, ev model.Event, s AttemptState) {
	if r.o.Store == nil {
		return
	}
	if err := r.o.Store.Save(ctx, r.o.Plugin.Name, string(ev.ID), s); err != nil {
		r.report("hooks: attempt count not saved", "plugin", r.o.Plugin.Name, "event", string(ev.ID), "error", err)
	}
}

func (r *Runner) clearAttempts(ctx context.Context, ev model.Event) {
	if r.o.Store == nil {
		return
	}
	if err := r.o.Store.Clear(ctx, r.o.Plugin.Name, string(ev.ID)); err != nil {
		r.report("hooks: attempt count not cleared", "plugin", r.o.Plugin.Name, "event", string(ev.ID), "error", err)
	}
}

// attempt runs the plugin once and applies its result. A panic in
// either is the attempt's failure, never the daemon's.
func (r *Runner) attempt(ctx context.Context, ev model.Event, attempt int) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	p := r.o.Plugin
	res, err := r.o.Exec.Run(ctx, NewEnvelope(p.Name, ev, attempt, r.now()))
	if err != nil {
		return err
	}
	return r.o.Apply.Apply(ctx, Delivery{Plugin: p.Name, Event: ev, Attempt: attempt, DryRun: p.DryRun}, res)
}

func (r *Runner) failed(ev model.Event, err error) {
	at := r.now().UTC()
	r.count(func(s *Status) { s.LastError = &LastError{Event: string(ev.ID), At: at, Message: err.Error()} })
}

func (r *Runner) count(f func(*Status)) {
	r.mu.Lock()
	f(&r.status)
	r.mu.Unlock()
}

// idle records a successful attempt that left nothing in hand.
func (r *Runner) idle() {
	r.count(func(s *Status) { s.Lag = 0 })
	r.ready()
}

func (r *Runner) ready() {
	r.mu.Lock()
	prev := r.status.State
	r.status.State, r.status.Code = StateReady, ""
	r.mu.Unlock()
	if prev != StateReady {
		r.changed()
	}
}

func (r *Runner) degrade(err error) {
	code := string(model.CodeOf(err))
	if code == "" {
		code = "error"
	}
	r.mu.Lock()
	prev := r.status.State
	r.status.State, r.status.Code = StateDegraded, code
	r.mu.Unlock()
	if prev != StateDegraded {
		r.changed()
	}
	r.failed(model.Event{}, err)
	r.report("hooks: degraded; retrying", "plugin", r.o.Plugin.Name, "code", code, "error", err)
}

func (r *Runner) changed() {
	if r.o.OnChange != nil {
		r.o.OnChange(r.Status())
	}
}

func (r *Runner) report(msg string, kv ...any) {
	if r.o.Logger != nil {
		r.o.Logger.Error(msg, kv...)
	}
}

// sleep waits d or until ctx ends, reporting whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
