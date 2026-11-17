package bridge_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/model"
)

// fakeSource is uma's event subscription, scripted.
type fakeSource struct {
	mu    sync.Mutex
	log   *callLog
	opens []model.OpenSubscription
	// batches are issued in order; a batch is re-issued, same token,
	// until it is acknowledged, as uma does while its lease lives.
	batches []model.EventBatch
	next    int
	acked   int
	// failures, popped one per call.
	openErrs, pollErrs, ackErrs, closeErrs []error
	closes                                 int
	head                                   model.Cursor
}

// callLog orders what the source and the publisher saw.
type callLog struct {
	mu    sync.Mutex
	items []string
}

func (l *callLog) add(s string) {
	l.mu.Lock()
	l.items = append(l.items, s)
	l.mu.Unlock()
}

func (l *callLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.items...)
}

func pop(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	*errs = (*errs)[1:]
	return err
}

var subRef = model.SubscriptionRef{Tenant: "acme", ID: "sub-1"}

func (f *fakeSource) Open(_ context.Context, req model.OpenSubscription) (model.SubscriptionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, req)
	f.log.add("open:" + req.Start)
	if err := pop(&f.openErrs); err != nil {
		return model.SubscriptionResult{}, err
	}
	return model.SubscriptionResult{Subscription: model.Subscription{Ref: subRef, Checkpoint: f.head}}, nil
}

func (f *fakeSource) Poll(ctx context.Context, req model.Poll) (model.EventBatch, error) {
	f.mu.Lock()
	if err := pop(&f.pollErrs); err != nil {
		f.mu.Unlock()
		return model.EventBatch{}, err
	}
	if f.next < len(f.batches) {
		b := f.batches[f.next]
		f.mu.Unlock()
		return b, nil
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return model.EventBatch{}, ctx.Err()
	case <-time.After(5 * time.Millisecond):
		return model.EventBatch{}, nil
	}
}

func (f *fakeSource) Ack(_ context.Context, req model.Acknowledge) (model.SubscriptionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log.add("ack:" + req.Token)
	if err := pop(&f.ackErrs); err != nil {
		return model.SubscriptionResult{}, err
	}
	if f.next < len(f.batches) && f.batches[f.next].AckToken == req.Token {
		f.head = f.batches[f.next].ScannedThrough
		f.next++
		f.acked++
	}
	return model.SubscriptionResult{Subscription: model.Subscription{Ref: subRef, Checkpoint: f.head}}, nil
}

func (f *fakeSource) Close(context.Context, model.Lookup[model.SubscriptionRef], model.Command) (model.Acceptance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.log.add("close")
	return model.Acceptance{}, pop(&f.closeErrs)
}

func (f *fakeSource) nOpens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opens)
}

func (f *fakeSource) open(i int) model.OpenSubscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[i]
}

func (f *fakeSource) nCloses() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func (f *fakeSource) ackedBatches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acked
}

// failingPublisher is a bus that refuses events while fail is set, and
// logs what it carried.
type failingPublisher struct {
	mu   sync.Mutex
	log  *callLog
	fail bool
	seen []bus.Event
}

func (p *failingPublisher) Publish(_ context.Context, e bus.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return errors.New("bus refused")
	}
	p.seen = append(p.seen, e)
	if pl, ok := e.Payload.(bridge.Payload); ok {
		p.log.add("publish:" + pl.Event)
	} else {
		p.log.add("health:" + string(e.Topic))
	}
	return nil
}

func (p *failingPublisher) set(fail bool) {
	p.mu.Lock()
	p.fail = fail
	p.mu.Unlock()
}

func (p *failingPublisher) events() []bus.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]bus.Event(nil), p.seen...)
}

func ev(id string, vis model.Visibility) model.Event {
	e := umaEvent(model.EventMessageCreated, vis, msgRes, model.Extension{})
	e.ID = model.ID(id)
	return e
}

type rig struct {
	t   *testing.T
	log *callLog
	src *fakeSource
	pub *failingPublisher
	b   *bridge.Bridge
}

func newRig(t *testing.T, batches ...model.EventBatch) *rig {
	t.Helper()
	log := &callLog{}
	r := &rig{t: t, log: log, src: &fakeSource{log: log, batches: batches}, pub: &failingPublisher{log: log}}
	r.b = bridge.New(bridge.Options{
		Source: r.src, Publisher: r.pub, Poll: 5 * time.Millisecond, RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond,
	})
	return r
}

// run runs the bridge until the returned stop is called.
func (r *rig) run() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.b.Run(ctx)
		close(done)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				r.t.Error("the bridge did not stop")
			}
		})
	}
	r.t.Cleanup(stop)
	return stop
}

func (r *rig) until(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	r.t.Fatalf("never: %s\n%v", what, r.log.get())
}

func batch(token string, through model.Cursor, events ...model.Event) model.EventBatch {
	return model.EventBatch{Events: events, AckToken: token, ScannedThrough: through}
}

func index(items []string, want string) int {
	for i, s := range items {
		if s == want {
			return i
		}
	}
	return -1
}

// TestBridgeOpensItsOwnDurableSubscription: the bridge reads uma's log
// as a subscription of its own, named for it, from the tail, with no
// filter: not the default consumer `event watch` opens, so a watch and
// the bridge never move each other's checkpoint.
func TestBridgeOpensItsOwnDurableSubscription(t *testing.T) {
	r := newRig(t)
	stop := r.run()
	r.until("open", func() bool { return r.src.nOpens() > 0 })
	stop()
	o := r.src.open(0)
	if o.Consumer != bridge.Consumer || o.Consumer == "heo-cli" || o.Consumer == "" {
		t.Errorf("consumer = %q", o.Consumer)
	}
	if o.Start != model.StartTail || o.PayloadMajor != model.PayloadVersion1 || o.After != "" {
		t.Errorf("open = %+v", o)
	}
	if len(o.Filter.Kinds) != 0 || len(o.Filter.Inboxes) != 0 {
		t.Errorf("filter = %+v", o.Filter)
	}
	if o.Operation == "" || o.Correlation == "" {
		t.Errorf("open carries no operation or correlation: %+v", o.Command)
	}
}

// TestBridgeAcksOnlyAfterPublishing: a batch is acknowledged after every
// one of its events was published, never before, so a crash between the
// two redelivers rather than loses.
func TestBridgeAcksOnlyAfterPublishing(t *testing.T) {
	r := newRig(t, batch("tok-1", "c1", ev("e1", model.Public), ev("e2", model.Internal)))
	r.run()
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	log := r.log.get()
	ack := index(log, "ack:tok-1")
	for _, id := range []string{"e1", "e2"} {
		if p := index(log, "publish:"+id); p < 0 || p > ack {
			t.Errorf("%s published at %d, acked at %d: %v", id, p, ack, log)
		}
	}
}

// TestBridgeDoesNotAckWhatItCouldNotPublish: while the bus refuses, the
// batch stays unacknowledged; when it takes events again the batch is
// published and acknowledged, once.
func TestBridgeDoesNotAckWhatItCouldNotPublish(t *testing.T) {
	r := newRig(t, batch("tok-1", "c1", ev("e1", model.Public)))
	r.pub.set(true)
	r.run()
	r.until("a refused publish to be seen degraded", func() bool { return r.b.Status().State == bridge.StateDegraded })
	time.Sleep(30 * time.Millisecond)
	if got := r.src.ackedBatches(); got != 0 || index(r.log.get(), "ack:tok-1") >= 0 {
		t.Fatalf("acknowledged a batch the bus refused: %v", r.log.get())
	}
	r.pub.set(false)
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	if r.b.Status().State != bridge.StateReady {
		t.Errorf("state = %s after recovering", r.b.Status().State)
	}
}

// TestBridgeRetriesAckWithoutRepublishing: an acknowledgement that
// fails is retried with the same token; its batch is not published
// again.
func TestBridgeRetriesAckWithoutRepublishing(t *testing.T) {
	r := newRig(t, batch("tok-1", "c1", ev("e1", model.Public)))
	r.src.ackErrs = []error{model.NewError(model.Unavailable, "store down"), model.NewError(model.Unavailable, "store down")}
	r.run()
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	published := 0
	acks := 0
	for _, s := range r.log.get() {
		switch s {
		case "publish:e1":
			published++
		case "ack:tok-1":
			acks++
		}
	}
	if published != 1 || acks != 3 {
		t.Errorf("published %d times, acked %d times: %v", published, acks, r.log.get())
	}
}

// TestBridgeStaleAckIsRedelivery: an acknowledgement uma refuses as
// conflicting (the lease ended; another poller won) is not an outage:
// the next poll redelivers, and the bridge goes on.
func TestBridgeStaleAckIsRedelivery(t *testing.T) {
	r := newRig(t, batch("tok-1", "c1", ev("e1", model.Public)))
	r.src.ackErrs = []error{model.NewError(model.Conflict, "batch lease expired")}
	r.run()
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	if st := r.b.Status(); st.State != bridge.StateReady {
		t.Errorf("state = %s", st.State)
	}
	published := 0
	for _, s := range r.log.get() {
		if s == "publish:e1" {
			published++
		}
	}
	if published != 2 {
		t.Errorf("event published %d times, want the original and the redelivery", published)
	}
}

// TestBridgeAcksBatchesWithNothingToPublish: events uma skipped as
// unreadable leave a token with no events; acknowledging it moves the
// checkpoint past them.
func TestBridgeAcksBatchesWithNothingToPublish(t *testing.T) {
	r := newRig(t, batch("tok-empty", "c9"))
	r.run()
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	if cp := r.b.Status().Checkpoint; cp != "c9" {
		t.Errorf("checkpoint = %q", cp)
	}
}

// TestBridgeStatus: the last event published, its time, the count and
// the checkpoint.
func TestBridgeStatus(t *testing.T) {
	r := newRig(t, batch("tok-1", "c1", ev("e1", model.Public), ev("e2", model.Internal)))
	if st := r.b.Status(); st.State != bridge.StateStarting || st.Consumer != bridge.Consumer || st.Published != 0 {
		t.Errorf("before run = %+v", st)
	}
	r.run()
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	st := r.b.Status()
	if st.State != bridge.StateReady || st.Published != 2 || st.LastEvent != "e2" || st.Checkpoint != "c1" {
		t.Errorf("status = %+v", st)
	}
	if st.LastEventAt.IsZero() || st.LastPublishedAt.IsZero() {
		t.Errorf("status times = %+v", st)
	}
}

// TestBridgeReopensAfterCursorExpired: a checkpoint behind retention is
// closed and reopened at the tail, the bridge reports itself degraded
// and then ready again; events in the gap are gone, as uma says.
func TestBridgeReopensAfterCursorExpired(t *testing.T) {
	r := newRig(t)
	r.src.pollErrs = []error{model.NewError(model.CursorExpired, "behind retention")}
	r.run()
	r.until("reopen", func() bool { return r.src.nOpens() == 2 })
	if got := r.src.open(1).Start; got != model.StartTail {
		t.Errorf("reopened at %q", got)
	}
	if r.src.nCloses() != 1 {
		t.Errorf("closes = %d", r.src.nCloses())
	}
	r.until("recovered", func() bool { return r.b.Status().State == bridge.StateReady })
	log := r.log.get()
	degraded, recovered := index(log, "health:heo.runtime.bridge.degraded"), index(log, "health:heo.runtime.bridge.recovered")
	if index(log, "close") < 0 || degraded < 0 || recovered < degraded {
		t.Errorf("close, degraded, recovered = %v", log)
	}
}

// TestBridgeKeepsTryingToOpen: uma refusing the subscription (a store
// restarting, a policy that does not yet allow it) leaves the bridge
// degraded and retrying, not stopped.
func TestBridgeKeepsTryingToOpen(t *testing.T) {
	r := newRig(t)
	r.src.openErrs = []error{model.NewError(model.Unavailable, "down"), model.NewError(model.Unauthorized, "denied")}
	r.run()
	r.until("opened", func() bool { return r.src.nOpens() == 3 && r.b.Status().State == bridge.StateReady })
}

// TestBridgeReopensAClosedSubscription: a subscription closed behind
// the bridge's back (uma's conflict) is reopened, not polled forever.
func TestBridgeReopensAClosedSubscription(t *testing.T) {
	r := newRig(t)
	r.src.pollErrs = []error{model.NewError(model.Conflict, "subscription closed; reopen it to resume")}
	r.run()
	r.until("reopen", func() bool { return r.src.nOpens() == 2 })
}

// TestBridgeHealthEventsAreDeskOnly: the bridge reports starting,
// degrading, recovering and stopping as heo.runtime.bridge.* events,
// apart from the domain events, with a code and no text; a public
// subscriber sees none of them.
func TestBridgeHealthEventsAreDeskOnly(t *testing.T) {
	hub := newHub(t)
	deskSeen, pubSeen := &collector{}, &collector{}
	defer hub.Subscribe(bridge.ScopeDesk, "heo.#", deskSeen.add)()
	defer hub.Subscribe(bridge.ScopePublic, "#", pubSeen.add)()
	log := &callLog{}
	src := &fakeSource{log: log, pollErrs: []error{model.NewError(model.Unavailable, "ada@example.com store down")}}
	b := bridge.New(bridge.Options{Source: src, Publisher: hub, Poll: 5 * time.Millisecond, RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	want := []bus.Topic{"heo.runtime.bridge.started", "heo.runtime.bridge.degraded", "heo.runtime.bridge.recovered"}
	deskSeen.waitFor(t, len(want))
	cancel()
	<-done
	got := deskSeen.waitFor(t, len(want)+1)
	want = append(want, "heo.runtime.bridge.stopped")
	for i, topic := range want {
		if got[i] != topic {
			t.Fatalf("health topics = %v, want %v", got, want)
		}
	}
	settle()
	if seen := pubSeen.got(); len(seen) != 0 {
		t.Errorf("a public subscriber received health events: %v", seen)
	}
}

// TestHealthPayloadCarriesNoText: a degraded event names the error's
// code, never its text.
func TestHealthPayloadCarriesNoText(t *testing.T) {
	r := newRig(t)
	r.src.pollErrs = []error{model.NewError(model.Unavailable, "ada@example.com BODY-SECRET")}
	r.run()
	r.until("recovered", func() bool {
		for _, e := range r.pub.events() {
			if e.Topic == "heo.runtime.bridge.recovered" {
				return true
			}
		}
		return false
	})
	for _, e := range r.pub.events() {
		if e.Topic != "heo.runtime.bridge.degraded" {
			continue
		}
		h, ok := e.Payload.(bridge.Health)
		if !ok || h.Code != string(model.Unavailable) || h.Consumer != bridge.Consumer {
			t.Errorf("degraded payload = %+v", e.Payload)
		}
		if s := fmt.Sprintf("%+v", e.Payload); containsAny(s, "ada@example.com", "BODY-SECRET") {
			t.Errorf("degraded payload carries the error text: %s", s)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// TestBridgePublishesDomainTopics: a batch goes out as the topics and
// payloads Map makes, stamped with the time uma recorded the event.
func TestBridgePublishesDomainTopics(t *testing.T) {
	r := newRig(t, batch("tok-1", "c1", ev("e1", model.Public), ev("e2", model.Internal)))
	r.run()
	r.until("ack", func() bool { return r.src.ackedBatches() == 1 })
	var topics []bus.Topic
	for _, e := range r.pub.events() {
		if _, ok := e.Payload.(bridge.Payload); ok {
			topics = append(topics, e.Topic)
			if !e.Timestamp.Equal(time.Unix(101, 0)) {
				t.Errorf("timestamp = %s", e.Timestamp)
			}
		}
	}
	if len(topics) != 2 || topics[0] != "uma.public.message.created" || topics[1] != "uma.desk.message.created" {
		t.Errorf("topics = %v", topics)
	}
}

// TestBridgeStopsPromptly: cancelling the context ends the run even
// while a poll waits.
func TestBridgeStopsPromptly(t *testing.T) {
	r := newRig(t)
	stop := r.run()
	r.until("open", func() bool { return r.src.nOpens() > 0 })
	start := time.Now()
	stop()
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s to stop", d)
	}
}
