package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/model"
)

// Consumer names the bridge's subscription in uma. It is the bridge's
// alone: `event watch` opens its own under the caller's actor and name,
// the bridge reads as its own actor (authz.BridgeActor), and neither
// moves the other's checkpoint.
const Consumer = "heo-bridge"

// correlation tags the bridge's calls in uma.
const correlation model.CorrelationID = "heo-bridge"

// Health topics: [Source].[Category].[Object].[Action], apart from the
// domain events, for desk scoped subscribers only.
const (
	sourceHealth   = "heo"
	categoryHealth = "runtime"
	objectHealth   = "bridge"

	ActionStarted   = "started"
	ActionDegraded  = "degraded"
	ActionRecovered = "recovered"
	ActionStopped   = "stopped"
)

// HealthTopic is the topic of the bridge's health event action.
func HealthTopic(action string) bus.Topic {
	return bus.Topic(sourceHealth + "." + categoryHealth + "." + objectHealth + "." + action)
}

// emitter names the bridge on every event it publishes.
const emitter = "heo.bridge"

// Health is a health event's payload: the bridge's state and, when it
// is degraded, the code of uma's or the bus's error, never its text.
type Health struct {
	Consumer string `json:"consumer"`
	State    State  `json:"state"`
	Code     string `json:"code,omitempty"`
}

// State is where the bridge stands.
type State string

// States.
const (
	// StateStarting: the subscription is not open yet.
	StateStarting State = "starting"
	// StateReady: the bridge reads the log and publishes what it finds.
	StateReady State = "ready"
	// StateDegraded: the last attempt failed; the bridge retries with a
	// backoff, and acknowledges nothing it has not published.
	StateDegraded State = "degraded"
)

// Source is the part of uma's Events service the bridge reads with.
// uma.Events satisfies it.
type Source interface {
	Open(context.Context, model.OpenSubscription) (model.SubscriptionResult, error)
	Poll(context.Context, model.Poll) (model.EventBatch, error)
	Ack(context.Context, model.Acknowledge) (model.SubscriptionResult, error)
	Close(context.Context, model.Lookup[model.SubscriptionRef], model.Command) (model.Acceptance, error)
}

// Publisher is where the bridge publishes: the Hub.
type Publisher interface {
	Publish(context.Context, bus.Event) error
}

// Logger receives the bridge's reports; kit's console logger satisfies
// it.
type Logger interface {
	Info(msg any, keyvals ...any)
	Error(msg any, keyvals ...any)
}

// Defaults.
const (
	// DefaultPoll is how long one poll waits for events.
	DefaultPoll = 5 * time.Second
	// DefaultBatch is how many events one poll reads.
	DefaultBatch = 100
	// DefaultRetryMin and DefaultRetryMax bound the backoff between
	// failed attempts.
	DefaultRetryMin = time.Second
	DefaultRetryMax = time.Minute
	// workGrace bounds publishing and acknowledging a batch already
	// read when the bridge is told to stop.
	workGrace = 10 * time.Second
)

// Options configures a Bridge.
type Options struct {
	// Source is uma's event service, as the bridge's own actor.
	Source Source
	// Publisher receives every domain and health event.
	Publisher Publisher
	// Poll, Batch, RetryMin and RetryMax default as above.
	Poll, RetryMin, RetryMax time.Duration
	Batch                    int
	Logger                   Logger
	// OnChange, when set, is called after each change of State, on the
	// bridge's goroutine; it must not block.
	OnChange func(Status)
}

// Status is the bridge's state, for `service status`. How far behind
// the log head the bridge is cannot be read cheaply, since positions in
// uma's log are opaque; the last event published and when says whether
// it keeps up.
type Status struct {
	Consumer string `json:"consumer" yaml:"consumer"`
	State    State  `json:"state" yaml:"state"`
	// Code is the code of the last failure while degraded.
	Code string `json:"code,omitempty" yaml:"code,omitempty"`
	// Checkpoint is the subscription's last acknowledged position.
	Checkpoint model.Cursor `json:"checkpoint,omitempty" yaml:"checkpoint,omitempty"`
	// LastEvent is the ID of the last event published, LastEventAt when
	// uma recorded it, and LastPublishedAt when the bridge published it.
	LastEvent       model.ID  `json:"last_event,omitempty" yaml:"last_event,omitempty"`
	LastEventAt     time.Time `json:"last_event_at,omitzero" yaml:"last_event_at,omitempty"`
	LastPublishedAt time.Time `json:"last_published_at,omitzero" yaml:"last_published_at,omitempty"`
	// Published counts the events published since the bridge started.
	Published uint64 `json:"published" yaml:"published"`
}

// Bridge tails uma's event log as a durable subscription and republishes
// each event on the bus. It is at-least-once towards the bus and
// best-effort beyond it: a batch is acknowledged after all its events
// were published, so a crash in between redelivers it; nothing waits
// for, retries or remembers a subscriber.
type Bridge struct {
	src      Source
	pub      Publisher
	log      Logger
	onChange func(Status)
	poll     time.Duration
	batch    int
	retry    [2]time.Duration

	mu     sync.Mutex
	status Status
	opens  int

	// sub and unacked belong to the Run loop alone: the open
	// subscription, and the token of a batch published but not yet
	// acknowledged.
	sub     model.SubscriptionRef
	unacked string
}

// New returns a Bridge.
func New(o Options) *Bridge {
	b := &Bridge{src: o.Source, pub: o.Publisher, log: o.Logger, onChange: o.OnChange, poll: o.Poll, batch: o.Batch,
		retry: [2]time.Duration{o.RetryMin, o.RetryMax}}
	if b.poll <= 0 {
		b.poll = DefaultPoll
	}
	if b.batch <= 0 {
		b.batch = DefaultBatch
	}
	if b.retry[0] <= 0 {
		b.retry[0] = DefaultRetryMin
	}
	if b.retry[1] < b.retry[0] {
		b.retry[1] = max(DefaultRetryMax, b.retry[0])
	}
	b.status = Status{Consumer: Consumer, State: StateStarting}
	return b
}

// Status returns the bridge's state.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

// Start opens the subscription now, so that events committed after it
// returns are read even when the first poll comes later. A failure is
// not final: Run opens it again, with the same backoff as any other
// failure.
func (b *Bridge) Start(ctx context.Context) error { return b.open(ctx) }

// Run reads and republishes until ctx ends. A failure of uma or of the
// bus never ends it: the bridge reports itself degraded and retries
// after a growing delay. A batch it has read when ctx ends is still
// published and acknowledged, within a grace period.
func (b *Bridge) Run(ctx context.Context) {
	defer b.emit(context.WithoutCancel(ctx), ActionStopped, StateStarting, "")
	delay := b.retry[0]
	for ctx.Err() == nil {
		err := b.step(ctx)
		if err == nil {
			delay = b.retry[0]
			continue
		}
		if ctx.Err() != nil {
			return
		}
		b.degrade(ctx, err)
		if !sleep(ctx, delay) {
			return
		}
		delay = min(delay*2, b.retry[1])
	}
}

// step makes one attempt: open the subscription if it is not, finish
// acknowledging a batch already published, or poll one batch, publish
// it and acknowledge it.
func (b *Bridge) step(ctx context.Context) error {
	if b.sub == (model.SubscriptionRef{}) {
		if err := b.open(ctx); err != nil {
			return err
		}
	}
	if b.unacked != "" {
		return b.ack(ctx)
	}
	batch, err := b.src.Poll(ctx, model.Poll{
		Call: model.Call{Correlation: correlation}, Subscription: b.sub, Limit: b.batch, Wait: b.poll,
	})
	if err != nil {
		return b.pollFailed(ctx, err)
	}
	if batch.AckToken == "" {
		b.ready(ctx)
		return nil
	}
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), workGrace)
	defer cancel()
	for _, ev := range batch.Events {
		if err := b.publish(work, ev); err != nil {
			return err
		}
	}
	b.unacked = batch.AckToken
	return b.ack(work)
}

// open opens the bridge's subscription, at the tail when it is new: the
// bus is not a log, so history before the bridge existed is not
// replayed. An open subscription keeps its checkpoint, which is how a
// restart resumes.
func (b *Bridge) open(ctx context.Context) error {
	b.mu.Lock()
	b.opens++
	op := model.OperationID(fmt.Sprintf("bridge-open-%d-%d", time.Now().UnixNano(), b.opens))
	b.mu.Unlock()
	res, err := b.src.Open(ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: correlation}, Operation: op},
		Consumer: Consumer, Start: model.StartTail, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		return err
	}
	b.sub = res.Subscription.Ref
	b.unacked = ""
	b.mu.Lock()
	b.status.Checkpoint = res.Subscription.Checkpoint
	b.mu.Unlock()
	b.ready(ctx)
	return nil
}

// pollFailed handles a failed poll. A checkpoint behind event retention
// is closed and reopened at the tail: the events in the gap are gone,
// uma says, and the bridge is degraded until it reads again. A
// subscription closed behind the bridge's back is reopened.
func (b *Bridge) pollFailed(ctx context.Context, err error) error {
	switch model.CodeOf(err) {
	case model.CursorExpired:
		b.report("bridge: checkpoint behind event retention; reopening at the tail", "code", model.CursorExpired)
		b.degrade(ctx, err)
		_, cerr := b.src.Close(ctx, model.Lookup[model.SubscriptionRef]{Call: model.Call{Correlation: correlation}, Ref: b.sub},
			model.Command{Call: model.Call{Correlation: correlation}, Operation: b.closeOperation()})
		if cerr != nil && model.CodeOf(cerr) != model.Conflict {
			return cerr
		}
		b.sub = model.SubscriptionRef{}
		return nil
	case model.Conflict, model.NotFound:
		b.sub = model.SubscriptionRef{}
	}
	return err
}

func (b *Bridge) closeOperation() model.OperationID {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opens++
	return model.OperationID(fmt.Sprintf("bridge-close-%d-%d", time.Now().UnixNano(), b.opens))
}

// publish republishes one event.
func (b *Bridge) publish(ctx context.Context, ev model.Event) error {
	topic, payload := Map(ev)
	e := bus.NewEvent(topic, emitter, payload)
	if !ev.RecordedAt.IsZero() {
		e.Timestamp = ev.RecordedAt
	}
	if err := b.pub.Publish(ctx, e); err != nil {
		return err
	}
	b.mu.Lock()
	b.status.Published++
	b.status.LastEvent, b.status.LastEventAt, b.status.LastPublishedAt = ev.ID, ev.RecordedAt, time.Now().UTC()
	b.mu.Unlock()
	return nil
}

// ack acknowledges the published batch. An acknowledgement uma refuses
// as a conflict (the lease ended, another poller won) leaves the batch
// to be redelivered; any other failure is retried with the same token,
// without publishing the batch again.
func (b *Bridge) ack(ctx context.Context) error {
	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), workGrace)
	defer cancel()
	res, err := b.src.Ack(work, model.Acknowledge{
		Command:      model.Command{Call: model.Call{Correlation: correlation}, Operation: ackOperation(b.unacked)},
		Subscription: b.sub, Token: b.unacked,
	})
	switch {
	case err == nil:
		b.mu.Lock()
		b.status.Checkpoint = res.Subscription.Checkpoint
		b.mu.Unlock()
	case model.CodeOf(err) == model.Conflict:
		b.report("bridge: acknowledgement refused; the batch will be redelivered", "code", model.Conflict)
	default:
		return err
	}
	b.unacked = ""
	b.ready(ctx)
	return nil
}

// ackOperation is the operation acknowledging token, so an
// acknowledgement retried after a lost answer replays.
func ackOperation(token string) model.OperationID {
	sum := sha256.Sum256([]byte(token))
	return model.OperationID("bridge-ack-" + hex.EncodeToString(sum[:16]))
}

// ready records a successful attempt and reports the transition.
func (b *Bridge) ready(ctx context.Context) {
	b.mu.Lock()
	prev := b.status.State
	b.status.State, b.status.Code = StateReady, ""
	b.mu.Unlock()
	if prev != StateReady {
		b.changed()
	}
	switch prev {
	case StateStarting:
		b.emit(ctx, ActionStarted, StateReady, "")
	case StateDegraded:
		b.emit(ctx, ActionRecovered, StateReady, "")
	}
}

// degrade records a failed attempt and reports the transition, once per
// outage.
func (b *Bridge) degrade(ctx context.Context, err error) {
	code := string(model.CodeOf(err))
	if code == "" {
		code = "error"
	}
	b.mu.Lock()
	prev := b.status.State
	b.status.State, b.status.Code = StateDegraded, code
	b.mu.Unlock()
	if prev != StateDegraded {
		b.changed()
	}
	b.report("bridge: degraded; retrying", "code", code, "error", err)
	if prev != StateDegraded {
		b.emit(ctx, ActionDegraded, StateDegraded, code)
	}
}

// emit publishes a health event; a bus that refuses it is not the
// bridge's problem to report on the bus.
func (b *Bridge) emit(ctx context.Context, action string, state State, code string) {
	_ = b.pub.Publish(ctx, bus.NewEvent(HealthTopic(action), emitter, Health{Consumer: Consumer, State: state, Code: code}))
}

func (b *Bridge) changed() {
	if b.onChange != nil {
		b.onChange(b.Status())
	}
}

func (b *Bridge) report(msg string, kv ...any) {
	if b.log != nil {
		b.log.Error(msg, kv...)
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
