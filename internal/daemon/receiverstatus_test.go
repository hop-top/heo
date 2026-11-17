package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// receiverScript drives a scripted receiver one step at a time: each
// step runs on the connection that is up, or on the next one that opens.
type receiverScript struct {
	t    *testing.T
	cmds chan func(adapterstest.Connection) error

	mu      sync.Mutex
	started map[int]time.Time
	failed  []time.Time
}

func scriptReceiver(t *testing.T, f *adapterstest.Factory) *receiverScript {
	s := &receiverScript{t: t, cmds: make(chan func(adapterstest.Connection) error), started: map[int]time.Time{}}
	f.ScriptReceive(s.run)
	return s
}

func (s *receiverScript) run(ctx context.Context, c adapterstest.Connection) error {
	s.mu.Lock()
	s.started[c.N] = time.Now()
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case step := <-s.cmds:
			if err := step(c); err != nil {
				s.mu.Lock()
				s.failed = append(s.failed, time.Now())
				s.mu.Unlock()
				return err
			}
		}
	}
}

// do hands step to the connection that is up or the next to open.
func (s *receiverScript) do(step func(adapterstest.Connection) error) {
	s.t.Helper()
	select {
	case s.cmds <- step:
	case <-time.After(10 * time.Second):
		s.t.Fatal("no receiver connection took the step")
	}
}

func (s *receiverScript) link(connected bool) {
	s.t.Helper()
	s.do(func(c adapterstest.Connection) error { c.Report(connected); return nil })
}

func (s *receiverScript) fail(err error) {
	s.t.Helper()
	s.do(func(c adapterstest.Connection) error { c.Report(false); return err })
}

// connectionStart is when connection n opened, zero before.
func (s *receiverScript) connectionStart(n int) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started[n]
}

// lastFailure is when the latest failing step ran.
func (s *receiverScript) lastFailure() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.failed) == 0 {
		return time.Time{}
	}
	return s.failed[len(s.failed)-1]
}

// withLinkReports makes the fake report its link, as the adapters that
// have a link to report do, and registers it so.
func withLinkReports() rigOption {
	return func(r *rig) {
		r.fake.ReportLink()
		r.registry = adapters.NewRegistry()
		if err := r.registry.Register(r.fake.Adapter()); err != nil {
			r.t.Fatal(err)
		}
	}
}

// withReceiverTiming polls each receiver's status every poll and has uma
// retry attempts connections, backing off base up to 4*base.
func withReceiverTiming(poll time.Duration, attempts int, base time.Duration) rigOption {
	return func(r *rig) {
		r.deps.ReceiverStatusPoll = poll
		r.deps.Retry = uma.RetryPolicy{MaxAttempts: attempts, BaseDelay: base, MaxDelay: 4 * base}
	}
}

func unavailable(wait time.Duration) error {
	return &model.Error{Code: model.Unavailable, SafeMessage: "provider unavailable", RetryAfter: wait}
}

// scopeLog samples a channel's scope and keeps each distinct state it
// reads, with its reason.
type scopeLog struct {
	mu   sync.Mutex
	seen []daemon.ScopeStatus
	stop func()
}

func (r *rig) logScope(svc string, ch model.ChannelRef) *scopeLog {
	l := &scopeLog{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l.stop = func() { cancel(); <-done }
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if sc, ok := r.scope(svc, ch); ok {
				l.mu.Lock()
				if n := len(l.seen); n == 0 || l.seen[n-1].State != sc.State || l.seen[n-1].Reason != sc.Reason {
					l.seen = append(l.seen, sc)
				}
				l.mu.Unlock()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	r.t.Cleanup(l.stop)
	return l
}

// mark returns how many states were seen so far.
func (l *scopeLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// since returns the states seen from mark on.
func (l *scopeLog) since(mark int) []daemon.ScopeStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]daemon.ScopeStatus(nil), l.seen[mark:]...)
}

func scopeIs(r *rig, ch model.ChannelRef, state daemon.ScopeState, reason string) func() bool {
	return func() bool {
		sc, _ := r.scope(daemon.NameReceivers, ch)
		return sc.State == state && strings.Contains(sc.Reason, reason)
	}
}

// TestReceiverScopeFollowsTheReportedLink: a receiver whose adapter
// reports its link reads starting until the link is up, ready while it
// is, and waiting when it drops, and the receivers service is ready only
// while every link is.
func TestReceiverScopeFollowsTheReportedLink(t *testing.T) {
	r := newRig(t, "acme", withLinkReports(), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	script := scriptReceiver(t, r.fake)
	ch := r.channel("link", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	svc := s.svcs[daemon.NameReceivers]

	waitFor(t, scopeIs(r, ch, daemon.ScopeStarting, "connecting"))
	if svc.Ready() {
		t.Error("receivers ready while the adapter reports no link")
	}
	if got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive); got.Phase != string(uma.ReceiverRunning) || got.Link != string(uma.ReceiverConnecting) {
		t.Errorf("receiving before the link = %+v, want running, connecting", got)
	}

	script.link(true)
	s.ready(daemon.NameReceivers)
	waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))
	if got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive); got.State != "ready" || got.Phase != string(uma.ReceiverRunning) ||
		got.Link != string(uma.ReceiverConnected) || got.Failures != 0 || !got.RetryAt.IsZero() || got.Owner == "" {
		t.Errorf("receiving with the link up = %+v", got)
	}

	script.link(false)
	waitFor(t, scopeIs(r, ch, daemon.ScopeWaiting, "connecting"))
	waitFor(t, func() bool { return service(r.host.Status(), daemon.NameReceivers).State == daemon.StateDegraded })

	script.link(true)
	waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))
	waitFor(t, func() bool { return service(r.host.Status(), daemon.NameReceivers).State == daemon.StateReady })
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
}

// TestReceiverIsNotReadyBeforeItsInstanceExists: an adapter that reports
// its link has no link to show while its instance is still being opened,
// and that is not a receiver that is up.
func TestReceiverIsNotReadyBeforeItsInstanceExists(t *testing.T) {
	r := newRig(t, "acme", withLinkReports(), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	script := scriptReceiver(t, r.fake)
	ch := r.channel("slow", adapterstest.Config{Account: "acct-1"})
	r.fake.SlowOpen(300 * time.Millisecond)
	log := r.logScope(daemon.NameReceivers, ch)
	s := r.run(5*time.Second, daemon.NameReceivers)
	// The plan opens the channel, then the receiver's connection does: the
	// instance exists once its Run has started.
	waitFor(t, func() bool { return r.fake.Runs(ch.ID) == 1 })
	before := log.since(0)
	if len(before) == 0 {
		t.Fatal("no scope seen while the instance was opened")
	}
	for _, sc := range before {
		if sc.State == daemon.ScopeReady {
			t.Errorf("ready before the instance existed: %+v", before)
		}
	}
	script.link(true)
	s.ready(daemon.NameReceivers)
	waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))
}

// TestReceiverWithoutAReporterIsReadyWhileItRuns: an adapter that
// reports no link is ready while its receive call runs, with no link
// state to show.
func TestReceiverWithoutAReporterIsReadyWhileItRuns(t *testing.T) {
	r := newRig(t, "acme", withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	scriptReceiver(t, r.fake)
	ch := r.channel("quiet", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))
	if got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive); got.State != "ready" || got.Phase != string(uma.ReceiverRunning) || got.Link != "" {
		t.Errorf("receiving = %+v, want ready, running, no link", got)
	}
}

// TestReceiverScopeStaysWaitingBetweenFailingRetries: while connections
// fail and uma retries them, the scope reads waiting with the failure's
// code, never ready, whether the adapter reports its link or not. A
// reporting adapter is ready again when its link is up; one that does not
// when a connection accepts ingress.
func TestReceiverScopeStaysWaitingBetweenFailingRetries(t *testing.T) {
	for _, reporting := range []bool{true, false} {
		name := "without a reporter"
		if reporting {
			name = "with a reporter"
		}
		t.Run(name, func(t *testing.T) {
			opts := []rigOption{withReceiverTiming(10*time.Millisecond, 8, 20*time.Millisecond)}
			if reporting {
				opts = append(opts, withLinkReports())
			}
			r := newRig(t, "acme", opts...)
			script := scriptReceiver(t, r.fake)
			ch := r.channel("flap", adapterstest.Config{Account: "acct-1"})
			r.inbox("in", ch, "dest")
			s := r.run(5*time.Second, daemon.NameReceivers)
			script.link(true)
			s.ready(daemon.NameReceivers)
			waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))

			log := r.logScope(daemon.NameReceivers, ch)
			waitFor(t, func() bool { return log.mark() > 0 })
			mark := log.mark()
			for range 3 {
				script.fail(unavailable(0))
				// The next connection opens and sits running after a failure
				// until it fails in turn: the window a naive reader calls ready.
				time.Sleep(60 * time.Millisecond)
			}
			waitFor(t, scopeIs(r, ch, daemon.ScopeWaiting, string(model.Unavailable)))
			got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive)
			if got.Failures != 3 || got.LastFailure != string(model.Unavailable) {
				t.Errorf("receiving while failing = %+v, want 3 failures, last unavailable", got)
			}

			if reporting {
				script.link(true)
			} else {
				script.do(func(c adapterstest.Connection) error {
					if err := c.Accept(t.Context(), adapterstest.Inject{Event: "e1", Message: "m1", From: "u1", Destination: "dest", Text: "hi"}); err != nil {
						t.Errorf("accept: %v", err)
					}
					return nil
				})
			}
			waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))
			waitFor(t, func() bool {
				states := log.since(mark)
				return len(states) > 0 && states[len(states)-1].State == daemon.ScopeReady
			})
			states := log.since(mark)
			named := false
			for _, sc := range states[:len(states)-1] {
				if sc.State != daemon.ScopeWaiting {
					t.Errorf("state while failing = %+v, want waiting, never ready: %+v", sc, states)
				}
				named = named || strings.Contains(sc.Reason, string(model.Unavailable))
			}
			if !named {
				t.Errorf("no state named the failure's code: %+v", states)
			}
			// uma counts the failures until a connection accepts ingress: a
			// link that is up proves the adapter's side only.
			got = r.host.Receiving(t.Context(), ch, model.CapabilityReceive)
			switch {
			case !reporting && (got.Failures != 0 || got.LastFailure != ""):
				t.Errorf("receiving after ingress = %+v, want no failures", got)
			case reporting && (got.Failures != 3 || got.Link != string(uma.ReceiverConnected)):
				t.Errorf("receiving after the link came up = %+v, want link connected and the failures kept", got)
			}
		})
	}
}

// TestReceiverStatedWaitIsSurfaced: a failure that states a wait parks
// the receiver, uma opens no connection before it ends, and the scope and
// the receiving report name when the next one opens.
func TestReceiverStatedWaitIsSurfaced(t *testing.T) {
	const wait = 600 * time.Millisecond
	r := newRig(t, "acme", withReceiverTiming(10*time.Millisecond, 5, 10*time.Millisecond))
	script := scriptReceiver(t, r.fake)
	ch := r.channel("limit", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)

	script.fail(unavailable(wait))
	failedAt := time.Now()
	waitFor(t, scopeIs(r, ch, daemon.ScopeWaiting, "retrying at"))
	sc, _ := r.scope(daemon.NameReceivers, ch)
	got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive)
	if got.Phase != string(uma.ReceiverRetrying) || got.Failures != 1 || got.LastFailure != string(model.Unavailable) ||
		got.RetryAt.Before(failedAt.Add(wait-100*time.Millisecond)) || got.RetryAt.After(failedAt.Add(wait+500*time.Millisecond)) {
		t.Errorf("receiving = %+v, want retrying after one failure, at about %v", got, failedAt.Add(wait))
	}
	if !strings.Contains(sc.Reason, got.RetryAt.UTC().Format(time.RFC3339)) {
		t.Errorf("scope reason %q does not name the retry time %s", sc.Reason, got.RetryAt.UTC().Format(time.RFC3339))
	}
	if n := r.fake.Runs(ch.ID); n != 1 {
		t.Errorf("connections = %d during the stated wait, want 1", n)
	}
	waitFor(t, func() bool { return !script.connectionStart(2).IsZero() })
	if gap := script.connectionStart(2).Sub(script.lastFailure()); gap < wait-50*time.Millisecond {
		t.Errorf("next connection after %v, want after the stated %v", gap, wait)
	}
}

// TestReceiverStatedWaitOutlastsAnExhaustedCall: when uma gives up after
// its attempts, the failure's stated wait still holds the next receive
// call off: heo does not reconnect before the provider's reset.
func TestReceiverStatedWaitOutlastsAnExhaustedCall(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a stated wait of two seconds")
	}
	const wait = 2500 * time.Millisecond
	r := newRig(t, "acme", withReceiverTiming(10*time.Millisecond, 1, 10*time.Millisecond))
	script := scriptReceiver(t, r.fake)
	ch := r.channel("reset", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)

	script.fail(unavailable(wait))
	waitFor(t, scopeIs(r, ch, daemon.ScopeWaiting, "retrying in"))
	if sc, _ := r.scope(daemon.NameReceivers, ch); !strings.Contains(sc.Reason, "provider unavailable") || !strings.Contains(sc.Reason, "2.5s") && !strings.Contains(sc.Reason, "3s") {
		t.Errorf("reason = %q, want the failure and the wait", sc.Reason)
	}
	deadline := time.Now().Add(10 * time.Second)
	for script.connectionStart(2).IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("receiver never reconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gap := script.connectionStart(2).Sub(script.lastFailure()); gap < wait-100*time.Millisecond {
		t.Errorf("reconnected after %v, want after the stated %v", gap, wait)
	}
}

// TestReceiverEndedFailsWithTheReceiveError: a failure uma does not
// retry ends the call, and the scope reads failed with the error.
func TestReceiverEndedFailsWithTheReceiveError(t *testing.T) {
	r := newRig(t, "acme", withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	script := scriptReceiver(t, r.fake)
	ch := r.channel("refused", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	script.fail(model.NewError(model.Permanent, "the provider refused the bot token"))
	waitFor(t, scopeIs(r, ch, daemon.ScopeFailed, "the provider refused the bot token"))
	time.Sleep(100 * time.Millisecond) // a few polls: the scope stays failed
	if sc, _ := r.scope(daemon.NameReceivers, ch); sc.State != daemon.ScopeFailed {
		t.Errorf("scope = %+v after the call ended, want failed", sc)
	}
	if got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive); got.State != "failed" || got.Phase != "" || got.Owner != "" {
		t.Errorf("receiving = %+v, want failed with no call and no lease", got)
	}
	if n := r.fake.Runs(ch.ID); n != 1 {
		t.Errorf("connections = %d, want 1: a failure for good is not retried", n)
	}
}

// TestReceiverConnectingSettlesAtTheTimeout: a channel whose adapter
// never reports a link holds the service's readiness until
// services.receivers.settle_timeout, then settles waiting.
func TestReceiverConnectingSettlesAtTheTimeout(t *testing.T) {
	const settle = 300 * time.Millisecond
	r := newRig(t, "acme", withLinkReports(), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond),
		withSet(daemon.KeyReceiversSettleTimeout, settle.String()))
	scriptReceiver(t, r.fake)
	ch := r.channel("slow", adapterstest.Config{Account: "acct-1"})
	start := time.Now()
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	if took := time.Since(start); took < settle-50*time.Millisecond || took > settle+700*time.Millisecond {
		t.Errorf("receivers ready after %v, want at the %v settle timeout: %+v", took, settle, service(r.host.Status(), daemon.NameReceivers))
	}
	rs := service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeWaiting || !strings.Contains(sc.Reason, "connecting") || rs.State != daemon.StateDegraded {
		t.Errorf("past the settle timeout: service %s, scope %+v", rs.State, sc)
	}
}

// TestReceiverLeaseHeldElsewhereShowsTheOwnerOnly: a process that does
// not run the receive call knows the lease's owner and expiry and no
// phase.
func TestReceiverLeaseHeldElsewhereShowsTheOwnerOnly(t *testing.T) {
	r := newRig(t, "acme", withLinkReports(), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	scriptReceiver(t, r.fake)
	ch := r.channel("shared", adapterstest.Config{Account: "acct-1"})

	other := uma.RuntimeCall{Principal: model.Principal{Tenant: r.tenant, Actor: "other"}}
	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan error, 1)
	go func() { held <- r.seed.Runtime().Receive(ctx, other, ch, uma.ReceiveOwner("other-daemon")) }()
	t.Cleanup(func() { cancel(); <-held })
	waitFor(t, func() bool { return r.fake.Receiving(ch.ID) == 1 })

	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	waitFor(t, scopeIs(r, ch, daemon.ScopeWaiting, "receiver lease held by other-daemon"))
	got := r.host.Receiving(t.Context(), ch, model.CapabilityReceive)
	if got.State != "waiting" || got.Owner != "other-daemon" || got.Until.IsZero() ||
		got.Phase != "" || got.Link != "" || got.Failures != 0 || got.LastFailure != "" || !got.RetryAt.IsZero() {
		t.Errorf("receiving = %+v, want the owner and expiry only", got)
	}
}

// storeReads counts the receiver lease reads the daemon makes.
type storeReads struct {
	store.Store
	n     atomic.Int64
	views atomic.Int64
}

func (s *storeReads) View(ctx context.Context, tenant model.TenantID, fn func(store.Reader) error) error {
	s.views.Add(1)
	return s.Store.View(ctx, tenant, func(rd store.Reader) error { return fn(readCounter{Reader: rd, n: &s.n}) })
}

type readCounter struct {
	store.Reader
	n *atomic.Int64
}

func (c readCounter) Lease(ctx context.Context, res model.Ref) (store.Lease, error) {
	c.n.Add(1)
	return c.Reader.Lease(ctx, res)
}

// TestReceiverStatusReadsAreBounded: each running receiver is read once
// per poll at most, and no more once it stops.
func TestReceiverStatusReadsAreBounded(t *testing.T) {
	const poll = 50 * time.Millisecond
	reads := &storeReads{}
	r := newRig(t, "acme", withReceiverTiming(poll, 5, 20*time.Millisecond))
	reads.Store = r.storage.st
	r.storage.view = reads
	scriptReceiver(t, r.fake)
	ch := r.channel("count", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))

	before := reads.n.Load()
	time.Sleep(20 * poll)
	if n := reads.n.Load() - before; n > 24 {
		t.Errorf("%d status reads in 20 polls of one receiver, want about 20", n)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
	stopped := reads.n.Load()
	time.Sleep(4 * poll)
	if n := reads.n.Load(); n != stopped {
		t.Errorf("%d status reads after the receivers stopped", n-stopped)
	}
}

// TestReceiverStatusPollsOpenNoStoreUnit: at steady state the status
// polls of N running receivers read memory alone: no store unit opens.
func TestReceiverStatusPollsOpenNoStoreUnit(t *testing.T) {
	const poll, channels, ticks = 20 * time.Millisecond, 5, 25
	reads := &storeReads{}
	r := newRig(t, "acme", withReceiverTiming(poll, 5, 20*time.Millisecond))
	reads.Store = r.storage.st
	r.storage.view = reads
	scriptReceiver(t, r.fake)
	var chs []model.ChannelRef
	for i := range channels {
		chs = append(chs, r.channel(fmt.Sprintf("steady-%d", i), adapterstest.Config{Account: fmt.Sprintf("acct-%d", i)}))
	}
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	for _, ch := range chs {
		waitFor(t, scopeIs(r, ch, daemon.ScopeReady, ""))
	}
	// Let the fast start-up reads settle into the steady interval.
	time.Sleep(10 * poll)

	views, leases := reads.views.Load(), reads.n.Load()
	time.Sleep(ticks * poll)
	if n := reads.views.Load() - views; n != 0 {
		t.Errorf("%d store units opened by %d receivers over %d polls, want 0", n, channels, ticks)
	}
	if n := reads.n.Load() - leases; n != 0 {
		t.Errorf("%d lease reads at steady state, want 0", n)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
}

// TestReceiverStopsWhileRetrying: stopping the daemon while a receiver
// waits out a stated wait ends it at once, clean.
func TestReceiverStopsWhileRetrying(t *testing.T) {
	r := newRig(t, "acme", withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	script := scriptReceiver(t, r.fake)
	ch := r.channel("parked", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	script.fail(unavailable(time.Hour))
	waitFor(t, scopeIs(r, ch, daemon.ScopeWaiting, "retrying at"))
	start := time.Now()
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("stop took %v with a receiver parked for an hour", took)
	}
}
