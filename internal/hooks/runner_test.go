package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/plugins"
	"hop.top/uma/model"
)

// event builds a public message.created event in conversation conv.
func event(id, conv string) model.Event {
	data, _ := json.Marshal(model.MessageCreatedPayload{Message: model.MessageRef{
		Conversation: model.ConversationRef{ID: model.ID(conv)}, ID: model.ID("m-" + id)}})
	return model.Event{
		Version: 1, ID: model.ID(id), Kind: model.EventMessageCreated, Tenant: "acme", Inbox: "in1",
		Resource:   model.Ref{Kind: model.KindMessage, ID: model.ID("m-" + id)},
		Audience:   model.Audience{Visibility: model.Public},
		RecordedAt: time.Now().Add(-time.Minute),
		Payload:    model.Extension{Name: "uma." + model.EventMessageCreated, Version: model.PayloadVersion1, Data: data},
	}
}

// log is an in-memory event log read as a durable subscription.
type log struct {
	mu     sync.Mutex
	events []model.Event
	cursor int          // acknowledged position
	next   int          // scanned position of the batch leased
	opens  int          // Open calls
	acks   []int        // checkpoint after each Ack
	filter model.Filter // filter of the last Open
	name   string       // consumer of the last Open
	wake   chan struct{}
}

func newLog(evs ...model.Event) *log { return &log{events: evs, wake: make(chan struct{}, 1)} }

func (l *log) Open(_ context.Context, r model.OpenSubscription) (model.SubscriptionResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opens++
	l.filter, l.name = r.Filter, r.Consumer
	return model.SubscriptionResult{Subscription: model.Subscription{
		Ref: model.SubscriptionRef{ID: "s1"}, Checkpoint: model.Cursor(strconv.Itoa(l.cursor))}}, nil
}

func (l *log) Poll(ctx context.Context, p model.Poll) (model.EventBatch, error) {
	l.mu.Lock()
	n := min(len(l.events)-l.cursor, p.Limit)
	if n > 0 {
		l.next = l.cursor + n
		b := model.EventBatch{Events: append([]model.Event(nil), l.events[l.cursor:l.next]...), AckToken: "t" + strconv.Itoa(l.next)}
		l.mu.Unlock()
		return b, nil
	}
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return model.EventBatch{}, ctx.Err()
	case <-time.After(5 * time.Millisecond):
	}
	return model.EventBatch{}, nil
}

func (l *log) Ack(_ context.Context, a model.Acknowledge) (model.SubscriptionResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, _ := strconv.Atoi(a.Token[1:])
	l.cursor = n
	l.acks = append(l.acks, n)
	return model.SubscriptionResult{Subscription: model.Subscription{Checkpoint: model.Cursor(a.Token[1:])}}, nil
}

func (l *log) Close(context.Context, model.Lookup[model.SubscriptionRef], model.Command) (model.Acceptance, error) {
	return model.Acceptance{}, nil
}

func (l *log) acked() int { l.mu.Lock(); defer l.mu.Unlock(); return l.cursor }

// fn adapts functions to Executor, Applier and DeadLetterer.
type fnExec func(context.Context, hooks.Envelope) (hooks.Result, error)

func (f fnExec) Run(ctx context.Context, e hooks.Envelope) (hooks.Result, error) { return f(ctx, e) }
func (fnExec) Close() error                                                      { return nil }

type fnApply func(context.Context, hooks.Delivery, hooks.Result) error

func (f fnApply) Apply(ctx context.Context, d hooks.Delivery, r hooks.Result) error {
	return f(ctx, d, r)
}

type fnDead func(context.Context, hooks.DeadLetter) error

func (f fnDead) Record(ctx context.Context, d hooks.DeadLetter) error { return f(ctx, d) }

func plugin(name string, concurrency, attempts int) plugins.Plugin {
	return plugins.Plugin{Name: name, Command: "x", Mode: plugins.ModeExec, Timeout: time.Second, Concurrency: concurrency,
		Retry: plugins.Retry{MaxAttempts: attempts, Backoff: plugins.Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond}}}
}

func okExec(hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil }

// run starts a runner and stops it with the test.
func run(t *testing.T, o hooks.Options) *hooks.Runner {
	t.Helper()
	if o.Poll == 0 {
		o.Poll = 5 * time.Millisecond
	}
	if o.RetryMin == 0 {
		o.RetryMin = time.Millisecond
	}
	r := hooks.New(o)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("never: %s", what)
}

func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("happened: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestCursorWaitsForApply: the batch is acknowledged only once its result
// was applied, and again for the next.
func TestCursorWaitsForApply(t *testing.T) {
	src := newLog(event("e1", "c1"), event("e2", "c1"))
	release := make(chan struct{})
	var applied atomic.Int32
	run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: src,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) { return okExec(e) }),
		Apply: fnApply(func(ctx context.Context, d hooks.Delivery, _ hooks.Result) error {
			applied.Add(1)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}),
	})
	eventually(t, "the first result is being applied", func() bool { return applied.Load() == 1 })
	never(t, "the cursor moved before the result was applied", 80*time.Millisecond, func() bool { return src.acked() != 0 })
	close(release)
	eventually(t, "the batch acknowledged", func() bool { return src.acked() == 2 })
	if applied.Load() != 2 {
		t.Errorf("applied %d, want 2", applied.Load())
	}
}

// TestFailedApplyIsRetriedBeforeTheCursorMoves: an apply that fails once
// is attempted again, and the cursor moves after the second.
func TestFailedApplyIsRetriedBeforeTheCursorMoves(t *testing.T) {
	src := newLog(event("e1", "c1"))
	var n atomic.Int32
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: src,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil }),
		Apply: fnApply(func(_ context.Context, d hooks.Delivery, _ hooks.Result) error {
			if n.Add(1) == 1 {
				return errors.New("store busy")
			}
			if d.Attempt != 2 {
				return fmt.Errorf("attempt %d", d.Attempt)
			}
			return nil
		}),
	})
	eventually(t, "acknowledged", func() bool { return src.acked() == 1 })
	st := r.Status()
	if st.Delivered != 1 || st.DeadLettered != 0 || st.LastError == nil || st.LastError.Message != "store busy" {
		t.Errorf("status %+v", st)
	}
}

// TestDeadLetterBeforeCursor: exit-2 style failures and exhausted
// attempts are recorded, and only then does the cursor pass the event.
func TestDeadLetterBeforeCursor(t *testing.T) {
	for name, tc := range map[string]struct {
		err      error
		attempts int
		want     int
	}{
		"permanent":        {hooks.Permanent(errors.New("exit 2")), 5, 1},
		"attempts run out": {errors.New("exit 1"), 3, 3},
		"no retry (1 max)": {errors.New("exit 1"), 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			src := newLog(event("e1", "c1"), event("e2", "c1"))
			var mu sync.Mutex
			var recorded []hooks.DeadLetter
			var ackedAtRecord []int
			var calls atomic.Int32
			r := run(t, hooks.Options{
				Plugin: plugin("p", 1, tc.attempts), Source: src,
				Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
					if e.EventID == "e1" {
						calls.Add(1)
						return hooks.Result{}, tc.err
					}
					return hooks.Result{}, nil
				}),
				Dead: fnDead(func(_ context.Context, d hooks.DeadLetter) error {
					mu.Lock()
					defer mu.Unlock()
					recorded = append(recorded, d)
					ackedAtRecord = append(ackedAtRecord, src.acked())
					return nil
				}),
			})
			eventually(t, "both events passed", func() bool { return src.acked() == 2 })
			mu.Lock()
			defer mu.Unlock()
			if len(recorded) != 1 || recorded[0].Event.ID != "e1" || recorded[0].Attempts != tc.want ||
				recorded[0].Permanent != hooks.IsPermanent(tc.err) || recorded[0].Consumer != plugin("p", 1, 1).Consumer() {
				t.Errorf("recorded %+v", recorded)
			}
			if ackedAtRecord[0] != 0 {
				t.Errorf("cursor at %d when the dead letter was recorded, want 0", ackedAtRecord[0])
			}
			if int(calls.Load()) != tc.want {
				t.Errorf("plugin called %d times, want %d", calls.Load(), tc.want)
			}
			if st := r.Status(); st.DeadLettered != 1 || st.Delivered != 1 {
				t.Errorf("status %+v", st)
			}
		})
	}
}

// TestUnrecordedDeadLetterHoldsTheCursor: while the record cannot be
// written the batch is not acknowledged; once it can, it is.
func TestUnrecordedDeadLetterHoldsTheCursor(t *testing.T) {
	src := newLog(event("e1", "c1"))
	var healthy atomic.Bool
	var recorded atomic.Int32
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 1), Source: src,
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) {
			return hooks.Result{}, hooks.Permanent(errors.New("exit 2"))
		}),
		Dead: fnDead(func(context.Context, hooks.DeadLetter) error {
			if !healthy.Load() {
				return errors.New("disk full")
			}
			recorded.Add(1)
			return nil
		}),
	})
	eventually(t, "degraded", func() bool { return r.Status().State == hooks.StateDegraded })
	never(t, "acknowledged an event nothing recorded", 60*time.Millisecond, func() bool { return src.acked() != 0 })
	healthy.Store(true)
	eventually(t, "acknowledged once recorded", func() bool { return src.acked() == 1 })
	if recorded.Load() != 1 {
		t.Errorf("recorded %d, want 1", recorded.Load())
	}
}

// TestConversationOrder: one conversation's events reach the plugin one
// at a time in log order, while different conversations overlap.
func TestConversationOrder(t *testing.T) {
	var evs []model.Event
	for i := range 6 {
		for _, c := range []string{"A", "B", "C"} {
			evs = append(evs, event(fmt.Sprintf("%s%d", c, i), c))
		}
	}
	src := newLog(evs...)
	var mu sync.Mutex
	order := map[string][]string{}
	active := map[string]int{}
	var overlap bool
	var running, peak int
	run(t, hooks.Options{
		Plugin: plugin("p", 3, 1), Source: src, Batch: len(evs),
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			c := e.Event.Conversation
			mu.Lock()
			if active[c]++; active[c] > 1 {
				overlap = true
			}
			order[c] = append(order[c], e.EventID)
			if running++; running > peak {
				peak = running
			}
			mu.Unlock()
			time.Sleep(3 * time.Millisecond)
			mu.Lock()
			active[c]--
			running--
			mu.Unlock()
			return hooks.Result{}, nil
		}),
	})
	eventually(t, "all acknowledged", func() bool { return src.acked() == len(evs) })
	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Error("two events of one conversation ran at once")
	}
	for _, c := range []string{"A", "B", "C"} {
		for i, id := range order[c] {
			if want := fmt.Sprintf("%s%d", c, i); id != want {
				t.Errorf("conversation %s order = %v", c, order[c])
				break
			}
		}
	}
	if peak < 2 {
		t.Errorf("conversations never overlapped (peak %d) with concurrency 3", peak)
	}
}

// TestPluginsAreIsolated: a plugin that never answers does not slow
// another's runner, and neither moves the other's cursor.
func TestPluginsAreIsolated(t *testing.T) {
	slow, fast := newLog(event("s1", "c1")), newLog(event("f1", "c1"), event("f2", "c2"))
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	rs := run(t, hooks.Options{Plugin: plugin("slow", 1, 1), Source: slow,
		Exec: fnExec(func(ctx context.Context, _ hooks.Envelope) (hooks.Result, error) {
			select {
			case <-stuck:
			case <-ctx.Done():
			}
			return hooks.Result{}, nil
		})})
	run(t, hooks.Options{Plugin: plugin("fast", 1, 1), Source: fast,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) { return okExec(e) })})
	eventually(t, "the fast plugin is done", func() bool { return fast.acked() == 2 })
	if slow.acked() != 0 {
		t.Errorf("slow plugin's cursor at %d", slow.acked())
	}
	eventually(t, "the slow plugin is in flight", func() bool { return rs.Status().InFlight == 1 })
}

// TestAFailingPluginIsContained: a panic in a plugin's executor is a
// failed attempt, not a crash.
func TestAFailingPluginIsContained(t *testing.T) {
	src := newLog(event("e1", "c1"))
	var n atomic.Int32
	r := run(t, hooks.Options{Plugin: plugin("p", 1, 3), Source: src,
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) {
			if n.Add(1) == 1 {
				panic("boom")
			}
			return hooks.Result{}, nil
		})})
	eventually(t, "recovered and acknowledged", func() bool { return src.acked() == 1 })
	if r.Status().State != hooks.StateReady {
		t.Errorf("state %s", r.Status().State)
	}
}

// TestAudienceAndFilter: events the plugin's actor may not read, and
// those its filter refuses, are not delivered, and the cursor passes
// them.
func TestAudienceAndFilter(t *testing.T) {
	note := event("note", "c1")
	note.Audience = model.Audience{Visibility: model.Internal}
	other := event("other", "c2")
	other.Inbox = "in2"
	src := newLog(note, other, event("ok", "c1"))
	var mu sync.Mutex
	var got []string
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 1), Source: src,
		Match: func(_ context.Context, ev model.Event) (bool, error) { return ev.Inbox == "in1", nil },
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			mu.Lock()
			got = append(got, e.EventID)
			mu.Unlock()
			return hooks.Result{}, nil
		}),
	})
	eventually(t, "acknowledged", func() bool { return src.acked() == 3 })
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("delivered %v, want [ok]", got)
	}
	if st := r.Status(); st.Skipped != 2 || st.Delivered != 1 {
		t.Errorf("status %+v", st)
	}
}

// TestDeskAudienceNeedsDeskRole: with a Sees that admits desk content, a
// note is delivered.
func TestDeskAudienceNeedsDeskRole(t *testing.T) {
	note := event("note", "c1")
	note.Audience = model.Audience{Visibility: model.Internal}
	src := newLog(note)
	var n atomic.Int32
	run(t, hooks.Options{Plugin: plugin("p", 1, 1), Source: src,
		Sees: func(model.Audience) bool { return true },
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { n.Add(1); return hooks.Result{}, nil })})
	eventually(t, "delivered", func() bool { return n.Load() == 1 && src.acked() == 1 })
}

// TestStatus: the open uses the plugin's consumer name and resolved
// filter; the status shows the cursor, the lag of a batch in hand and the
// last error.
func TestStatusAndOpen(t *testing.T) {
	src := newLog(event("e1", "c1"))
	p := plugin("triage", 1, 1)
	p.Filter = plugins.Filter{Kinds: []string{model.EventMessageCreated}, Inboxes: []string{"support"}}
	gate := make(chan struct{})
	r := run(t, hooks.Options{
		Plugin: p, Source: src,
		Resolve: func(context.Context) (model.Filter, error) {
			return model.Filter{Kinds: p.Filter.Kinds, Inboxes: []model.InboxRef{{ID: "in1"}}}, nil
		},
		Exec: fnExec(func(ctx context.Context, _ hooks.Envelope) (hooks.Result, error) {
			select {
			case <-gate:
			case <-ctx.Done():
			}
			return hooks.Result{}, errors.New("exit 1: no")
		}),
	})
	eventually(t, "the event in flight", func() bool { return r.Status().InFlight == 1 })
	st := r.Status()
	if st.Plugin != "triage" || st.Consumer != p.Consumer() || st.State != hooks.StateReady || st.Lag < 30*time.Second {
		t.Errorf("status %+v", st)
	}
	src.mu.Lock()
	if src.name != p.Consumer() || len(src.filter.Inboxes) != 1 || src.filter.Inboxes[0].ID != "in1" {
		t.Errorf("opened %q with %+v", src.name, src.filter)
	}
	src.mu.Unlock()
	close(gate)
	eventually(t, "dead-lettered and acknowledged", func() bool { return src.acked() == 1 })
	st = r.Status()
	if st.Cursor != "1" || st.Lag != 0 || st.LastError == nil || st.LastError.Event != "e1" || st.DeadLettered != 1 {
		t.Errorf("status %+v", st)
	}
}

// TestStopLeavesTheBatchUnacknowledged: stopping mid-delivery neither
// dead-letters nor acknowledges; the next start reads the batch again.
func TestStopLeavesTheBatchUnacknowledged(t *testing.T) {
	src := newLog(event("e1", "c1"))
	started := make(chan struct{}, 1)
	var dead atomic.Int32
	r := hooks.New(hooks.Options{Plugin: plugin("p", 1, 3), Source: src, Poll: 5 * time.Millisecond,
		Exec: fnExec(func(ctx context.Context, _ hooks.Envelope) (hooks.Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			return hooks.Result{}, ctx.Err()
		}),
		Dead: fnDead(func(context.Context, hooks.DeadLetter) error { dead.Add(1); return nil })})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	<-started
	cancel()
	<-done
	if src.acked() != 0 || dead.Load() != 0 {
		t.Errorf("acked %d, dead-lettered %d", src.acked(), dead.Load())
	}
}

// TestDeliveryCarriesTheApplier: the plugin's dry_run reaches the
// applier with the plugin's name, and a permanent failure of the
// applier, an authorization refusal, dead-letters at once.
func TestDeliveryCarriesTheApplier(t *testing.T) {
	src := newLog(event("e1", "c1"))
	got := make(chan hooks.Delivery, 4)
	p := plugin("p", 1, 5)
	p.DryRun = true
	var dead atomic.Int32
	run(t, hooks.Options{
		Plugin: p, Source: src, Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) { return okExec(e) }),
		Apply: fnApply(func(_ context.Context, d hooks.Delivery, _ hooks.Result) error {
			got <- d
			return hooks.Permanent(errors.New("UNAUTHORIZED"))
		}),
		Dead: fnDead(func(_ context.Context, d hooks.DeadLetter) error {
			dead.Add(1)
			if !d.Permanent || d.Attempts != 1 {
				t.Errorf("dead letter %+v, want permanent on the first attempt", d)
			}
			return nil
		}),
	})
	eventually(t, "dead-lettered", func() bool { return dead.Load() == 1 })
	d := <-got
	if !d.DryRun || d.Plugin != "p" || d.Event.ID != "e1" {
		t.Errorf("delivery %+v, want dry run for p's e1", d)
	}
}

// heldEvent builds an internal operation.<kind> event for an operation
// proposed by proposer.
func heldEvent(id, kind string, proposer model.ActorID) model.Event {
	data, _ := json.Marshal(model.HeldOperationPayload{
		Operation: model.OperationRef{Tenant: "acme", ID: model.ID("op-" + id)},
		Key:       model.OperationKey{Target: model.Ref{Kind: model.KindMessage, ID: "m1"}},
		Proposer:  proposer,
	})
	return model.Event{
		Version: 1, ID: model.ID(id), Kind: kind, Tenant: "acme", Inbox: "in1",
		Resource:   model.Ref{Kind: model.KindOperation, ID: model.ID("op-" + id)},
		Audience:   model.Audience{Visibility: model.Internal},
		RecordedAt: time.Now().Add(-time.Minute),
		Payload:    model.Extension{Name: "uma." + kind, Version: model.PayloadVersion1, Data: data},
	}
}

// TestOwnHeldOperationIsNotDelivered: a plugin is not told that its own
// operation was held, or it could answer by proposing again and loop
// without a human in between. The same operation's decision and expiry,
// and operations held for other proposers, are delivered.
func TestOwnHeldOperationIsNotDelivered(t *testing.T) {
	src := newLog(
		heldEvent("own-held", model.EventOperationHeld, "plugin:p"),
		heldEvent("other-held", model.EventOperationHeld, "plugin:q"),
		heldEvent("own-decided", model.EventOperationDecided, "plugin:p"),
		heldEvent("own-expired", model.EventOperationExpired, "plugin:p"),
	)
	var mu sync.Mutex
	var got []string
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 1), Source: src,
		Sees: func(model.Audience) bool { return true },
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			mu.Lock()
			got = append(got, e.EventID)
			mu.Unlock()
			return hooks.Result{}, nil
		}),
	})
	eventually(t, "acknowledged", func() bool { return src.acked() == 4 })
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != "[other-held own-decided own-expired]" {
		t.Errorf("delivered %v, want the others' held and the decision and expiry", got)
	}
	if st := r.Status(); st.Skipped != 1 {
		t.Errorf("skipped %d, want 1", st.Skipped)
	}
}

// TestHeldOperationNeedsTheDeskAudience: operation events are internal,
// so a plugin that may not read desk content never receives them.
func TestHeldOperationNeedsTheDeskAudience(t *testing.T) {
	src := newLog(heldEvent("h", model.EventOperationHeld, "plugin:q"))
	var n atomic.Int32
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 1), Source: src,
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { n.Add(1); return hooks.Result{}, nil }),
	})
	eventually(t, "acknowledged", func() bool { return src.acked() == 1 })
	if n.Load() != 0 || r.Status().Skipped != 1 {
		t.Errorf("delivered %d, status %+v", n.Load(), r.Status())
	}
}
