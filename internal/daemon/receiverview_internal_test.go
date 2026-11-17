package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// TestReceiverViewComposesTheScope pins how uma's receiver status and
// the adapter's listener make a channel's scope.
func TestReceiverViewComposesTheScope(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	running := uma.ReceiverStatus{Phase: uma.ReceiverRunning, Since: now}
	failing := func(st uma.ReceiverStatus) uma.ReceiverStatus {
		st.Failures, st.Reason = 2, model.Unavailable
		return st
	}
	with := func(st uma.ReceiverStatus, link uma.ReceiverLink) uma.ReceiverStatus { st.Link = link; return st }
	withCode := func(st uma.ReceiverStatus, code string) uma.ReceiverStatus {
		st.Link, st.Detail = uma.ReceiverDegraded, code
		return st
	}
	retrying := uma.ReceiverStatus{Phase: uma.ReceiverRetrying, Failures: 2, Reason: model.RateLimited, RetryAt: now.Add(time.Second)}
	farRetry := retrying
	farRetry.RetryAt = later

	for _, tc := range []struct {
		name     string
		st       uma.ReceiverStatus
		settled  bool
		reports  bool
		serving  bool
		bound    bool
		listen   error
		want     ScopeState
		contains string
		none     bool
	}{
		{name: "no status yet", none: true},
		{name: "ended is the caller's", st: uma.ReceiverStatus{Phase: uma.ReceiverEnded}, none: true},
		{name: "link up", st: with(running, uma.ReceiverConnected), want: ScopeReady},
		{name: "link up after failures", st: with(failing(running), uma.ReceiverConnected), want: ScopeReady},
		{name: "no reporter, no failure", st: running, want: ScopeReady},
		{name: "no reporter, failures since", st: failing(running), settled: true, want: ScopeWaiting, contains: "unavailable"},
		{name: "no reporter, failures, not settled", st: failing(running), want: ScopeStarting, contains: "unavailable"},
		{name: "link connecting, not settled", st: with(running, uma.ReceiverConnecting), want: ScopeStarting, contains: "connecting"},
		{name: "link connecting, settled", st: with(running, uma.ReceiverConnecting), settled: true, want: ScopeWaiting, contains: "connecting"},
		{name: "reporter before its instance", st: running, reports: true, settled: true, want: ScopeWaiting, contains: "connecting"},
		{name: "retrying, not settled", st: retrying, want: ScopeStarting, contains: "rate_limited"},
		{name: "retrying, settled", st: retrying, settled: true, want: ScopeWaiting, contains: "rate_limited"},
		{name: "retrying past the settle deadline", st: farRetry, want: ScopeWaiting, contains: later.UTC().Format(time.RFC3339)},
		{name: "link degraded", st: withCode(running, "google.mail.push_denied"), want: ScopeDegraded, contains: "google.mail.push_denied"},
		{name: "link degraded, settled", st: withCode(running, "google.mail.push_denied"), settled: true, want: ScopeDegraded, contains: "mail arrives by polling only"},
		{name: "link degraded, unknown code", st: withCode(running, "x.future_code"), want: ScopeDegraded, contains: "x.future_code"},
		{name: "link degraded, no code", st: with(running, uma.ReceiverDegraded), want: ScopeDegraded, contains: "degraded"},
		{name: "listener not bound", st: running, serving: true, want: ScopeStarting},
		{name: "listener bound", st: running, serving: true, bound: true, want: ScopeReady},
		{name: "listener failed", st: running, serving: true, listen: errors.New("address in use"), want: ScopeWaiting, contains: "listener: address in use"},
		{name: "retrying beats a bound listener", st: retrying, settled: true, serving: true, bound: true, want: ScopeWaiting, contains: "retrying at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var settled atomic.Bool
			settled.Store(tc.settled)
			v := &receiverView{serving: tc.serving, reports: tc.reports, settleBy: now.Add(10 * time.Minute), settled: &settled,
				status: tc.st, bound: tc.bound, listenErr: tc.listen}
			state, reason, ok := v.compose(now)
			if tc.none {
				if ok {
					t.Fatalf("composed %s %q, want nothing", state, reason)
				}
				return
			}
			if !ok || state != tc.want || !strings.Contains(reason, tc.contains) {
				t.Errorf("= %s %q (%v), want %s containing %q", state, reason, ok, tc.want, tc.contains)
			}
		})
	}
}

// TestReceiverViewLearnsAReportedLink: an adapter nobody said reports a
// link is taken for one once a link has been seen, so the moment before
// a connection's instance exists is not a ready receiver.
func TestReceiverViewLearnsAReportedLink(t *testing.T) {
	var settled atomic.Bool
	settled.Store(true)
	v := &receiverView{settleBy: time.Now(), settled: &settled}
	v.status = uma.ReceiverStatus{Phase: uma.ReceiverRunning, Link: uma.ReceiverConnected}
	if state, _, _ := v.compose(time.Now()); state != ScopeReady {
		t.Fatalf("connected = %s", state)
	}
	v.status = uma.ReceiverStatus{Phase: uma.ReceiverRunning}
	if state, _, _ := v.compose(time.Now()); state != ScopeWaiting {
		t.Errorf("running with no link after one was seen = %s, want waiting", state)
	}
}

// TestReceiverViewLeavesTheScopeToTheCallerBetweenCalls: while no
// receive call runs, a late listener report does not set the scope from
// the last call's status, which the caller's own wait has replaced.
func TestReceiverViewLeavesTheScopeToTheCallerBetweenCalls(t *testing.T) {
	var set []ScopeStatus
	var settled atomic.Bool
	v := &receiverView{scope: func(sc ScopeStatus) { set = append(set, sc) }, settle: func() { settled.Store(true) },
		serving: true, settleBy: time.Now(), settled: &settled}
	v.begin()
	v.mu.Lock()
	v.status = uma.ReceiverStatus{Phase: uma.ReceiverRunning, Link: uma.ReceiverConnected}
	v.mu.Unlock()
	v.listener(nil)
	if len(set) != 1 || set[0].State != ScopeReady {
		t.Fatalf("during the call: %+v, want ready", set)
	}
	v.end()
	v.listener(errors.New("address in use"))
	v.listener(nil)
	if len(set) != 1 {
		t.Errorf("a listener report after the call set %+v", set[1:])
	}
	// The next call starts from nothing: the listener has to bind again.
	v.begin()
	v.mu.Lock()
	v.status = uma.ReceiverStatus{Phase: uma.ReceiverRunning}
	changed := v.recompute(time.Now())
	v.mu.Unlock()
	if !changed || set[len(set)-1].State != ScopeStarting {
		t.Errorf("next call before its listener bound: %+v, want starting", set[len(set)-1])
	}
}

// TestReceiverViewGapIsNotReady: a receiver that runs without a
// credential its configuration needs (its adapter goes on without the
// feature) reads degraded with why, however well it connects, and ready
// again once the credential is there.
func TestReceiverViewGapIsNotReady(t *testing.T) {
	now := time.Now()
	var settled atomic.Bool
	settled.Store(true)
	var gap atomic.Value
	gap.Store("graph.notifications missing")
	var set []ScopeStatus
	v := &receiverView{settleBy: now, settled: &settled, scope: func(sc ScopeStatus) { set = append(set, sc) }, settle: func() {},
		gapCheck: func(context.Context) string { return gap.Load().(string) }, poll: time.Hour, fast: time.Millisecond,
		read: func(context.Context) (uma.ReceiverStatus, error) {
			return uma.ReceiverStatus{Phase: uma.ReceiverRunning, Link: uma.ReceiverConnected}, nil
		}}
	v.begin()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.watch(ctx)
	waitFor(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.last.State == ScopeDegraded })
	v.mu.Lock()
	if !strings.Contains(v.last.Reason, "graph.notifications missing") {
		t.Errorf("reason = %q", v.last.Reason)
	}
	v.mu.Unlock()
	gap.Store("")
	waitFor(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.last.State == ScopeReady })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("condition not met")
}

// TestReceiverViewDegradedLink: a receiver whose adapter reports a
// degraded link still receives, by polling; its scope reads degraded with
// the code and what it means, the code is the scope's own field, an
// unknown code is shown raw, and it is ready again once the link is.
func TestReceiverViewDegradedLink(t *testing.T) {
	var settled atomic.Bool
	settled.Store(true)
	var set []ScopeStatus
	v := &receiverView{settleBy: time.Now(), settled: &settled, scope: func(sc ScopeStatus) { set = append(set, sc) }, settle: func() {}}
	v.begin()
	step := func(st uma.ReceiverStatus) ScopeStatus {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.status = st
		v.recompute(time.Now())
		return set[len(set)-1]
	}
	run := uma.ReceiverStatus{Phase: uma.ReceiverRunning}
	deg := func(code string) uma.ReceiverStatus {
		st := run
		st.Link, st.Detail = uma.ReceiverDegraded, code
		return st
	}
	up := run
	up.Link = uma.ReceiverConnected

	sc := step(deg("google.mail.push_denied"))
	if sc.State != ScopeDegraded || sc.Code != "google.mail.push_denied" ||
		sc.Reason != "push registration refused (google.mail.push_denied): mail arrives by polling only" {
		t.Errorf("degraded = %+v", sc)
	}
	if sc = step(up); sc.State != ScopeReady || sc.Reason != "" || sc.Code != "" {
		t.Errorf("cleared = %+v, want ready", sc)
	}
	if sc = step(deg("x.future_code")); sc.State != ScopeDegraded || sc.Reason != "degraded (x.future_code): still receiving" || sc.Code != "x.future_code" {
		t.Errorf("unknown code = %+v", sc)
	}

	// With the credential gap of the same channel: one reason, not two.
	v.mu.Lock()
	v.gap = "credential graph.notifications: missing; no push subscription"
	v.mu.Unlock()
	if sc = step(deg("microsoft.mail.client_state_missing")); sc.State != ScopeDegraded || sc.Reason != v.gap || sc.Code != "microsoft.mail.client_state_missing" {
		t.Errorf("gap and the same cause = %+v, want the gap alone", sc)
	}
	sc = step(deg("microsoft.mail.subscription_rejected"))
	if !strings.HasPrefix(sc.Reason, v.gap) || !strings.Contains(sc.Reason, "microsoft.mail.subscription_rejected") {
		t.Errorf("gap and another cause = %q, want both", sc.Reason)
	}
	if sc = step(up); sc.State != ScopeDegraded || sc.Reason != v.gap || sc.Code != "" {
		t.Errorf("gap alone = %+v", sc)
	}
}

// TestGapAloneReadsDegradedWhileReceiving: a credential gap on a
// receiver whose call otherwise runs reads degraded (mail still arrives
// by polling); one that is not receiving yet keeps its wait.
func TestGapAloneReadsDegradedWhileReceiving(t *testing.T) {
	var settled atomic.Bool
	settled.Store(true)
	v := &receiverView{settleBy: time.Now(), settled: &settled, gap: "credential graph.notifications: missing"}
	v.status = uma.ReceiverStatus{Phase: uma.ReceiverRunning, Link: uma.ReceiverConnected}
	if state, reason, _ := v.compose(time.Now()); state != ScopeDegraded || reason != v.gap {
		t.Errorf("gap, receiving = %s %q, want degraded", state, reason)
	}
	v.status = uma.ReceiverStatus{Phase: uma.ReceiverRunning, Link: uma.ReceiverConnecting}
	if state, _, _ := v.compose(time.Now()); state != ScopeWaiting {
		t.Errorf("gap, link connecting = %s, want waiting", state)
	}
}
