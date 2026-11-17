package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// DefaultReceiverStatusPoll is how often a receiver's status is read
// from uma while its receive call runs.
const DefaultReceiverStatusPoll = 2 * time.Second

// receiverView is one channel's receiver scope while a receive call
// runs: it reads what uma reports of the call (uma.ReceiverState: its
// phase, the adapter's link, the failures counted, the next connection's
// time) and what the adapter's listener reports, and composes the scope
// from both. A receive call that runs is not a receiver that serves: it
// may be waiting to reconnect, or connecting, and only the status says.
//
// uma keeps the status in the memory of the process running the call, so
// only this daemon sees it, and offers no event for a change: the view
// reads it on a timer and not at all while no call runs. The read is of
// memory alone (ReceiverState) while the call runs here; uma's store
// read (ReceiverStatus) answers only when it does not. The timer
// is fast (poll/20) while the channel has not settled and after a
// change, and backs off, doubling, to the poll interval (2s by default)
// while nothing changes: at most one read per channel per poll at
// steady state. Between reads the scope is the last read.
type receiverView struct {
	// scope sets the channel's scope; settle marks the channel settled.
	scope  func(ScopeStatus)
	settle func()
	// ch is the channel's scope, with no state.
	ch ScopeStatus
	// serving is true for a listener adapter: its scope waits for the
	// listener too. reports is true for an adapter that reports its link
	// (adapters.Adapter.ReportsLink), or once a link has been seen.
	serving, reports bool
	// settleBy is when a channel still connecting or retrying settles
	// waiting. settled is whether it has.
	settleBy time.Time
	settled  *atomic.Bool
	// poll is the steady interval of the status reads; fast is the
	// interval while the channel starts and after a change.
	poll, fast time.Duration
	// read reads uma's status of the channel: memory while the call runs
	// here, the store otherwise.
	read func(context.Context) (uma.ReceiverStatus, error)
	// log reports a scope change, nil to report none.
	log func(ScopeStatus)
	// gapCheck, when set, says what a credential the channel's
	// configuration needs is missing, or "" when nothing is. Its
	// adapter runs on without the feature that credential serves and
	// reports nothing, so a receiver that connects is still not ready:
	// the view reads degraded with the gap as the reason.
	gapCheck func(context.Context) string

	mu sync.Mutex
	// active is true while a receive call runs; the scope is the view's
	// only then.
	active bool
	// bound and listenErr are the listener's last report.
	bound     bool
	listenErr error
	// status is the last status read of the running call.
	status uma.ReceiverStatus
	// gap is the last gapCheck's answer.
	gap string
	// last is the last scope the view set.
	last ScopeStatus
}

// begin starts a receive call: nothing is known of it yet, and a
// listener adapter's listener has yet to bind.
func (v *receiverView) begin() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.active, v.bound, v.listenErr, v.status, v.gap = true, false, nil, uma.ReceiverStatus{}, ""
	v.last = ScopeStatus{}
}

// end stops the view: the receive call returned, and the scope is the
// caller's again.
func (v *receiverView) end() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.active = false
}

// listener records the adapter's listener report: err when it failed,
// nil when it is bound.
func (v *receiverView) listener(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bound, v.listenErr = err == nil, err
	v.recompute(time.Now())
}

// watch reads the status until ctx ends: at once, then after fast while
// the channel has not settled, the call shows no phase or the scope just
// changed, and otherwise after twice the last wait up to poll. A channel
// that has not settled is read again at its settle deadline at the
// latest.
func (v *receiverView) watch(ctx context.Context) {
	wait := v.fast
	for first := true; ; first = false {
		changed := false
		// A gap is looked at once and again while it lasts: the operator
		// stores the credential, and the receiver is ready without a
		// restart. A channel with none is not asked again.
		var gap string
		check := v.gapCheck != nil && (first || v.currentGap() != "")
		if check {
			gap = v.gapCheck(ctx)
		}
		if st, err := v.read(ctx); err == nil {
			v.mu.Lock()
			v.status = st
			if check {
				v.gap = gap
			}
			changed = v.recompute(time.Now())
			v.mu.Unlock()
			if st.Phase == "" {
				changed = true
			}
		}
		if changed || !v.settled.Load() {
			wait = v.fast
		} else {
			wait = min(wait*2, v.poll)
		}
		d := wait
		if !v.settled.Load() {
			d = min(d, max(time.Until(v.settleBy), time.Millisecond))
		}
		if !sleep(ctx, d) {
			return
		}
	}
}

// currentGap is the last gap found.
func (v *receiverView) currentGap() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gap
}

// recompute sets the scope from the listener and the status, and reports
// whether it changed. The caller holds v.mu.
func (v *receiverView) recompute(now time.Time) bool {
	if !v.active {
		return false
	}
	state, reason, ok := v.compose(now)
	if !ok {
		return false
	}
	sc := v.ch
	sc.State, sc.Reason, sc.Code = state, reason, v.degradedCode()
	v.scope(sc)
	if state != ScopeStarting {
		v.settle()
	}
	changed := v.last.State != state || v.last.Reason != reason || v.last.Code != sc.Code
	if changed && v.log != nil {
		v.log(sc)
	}
	v.last = sc
	return changed
}

// compose is the scope the listener and the status make, and false when
// they make none yet: no status read, or the call is ending and the
// caller sets the scope from what ends it.
//
//   - retrying: waiting, with the code of the last failure and when the
//     next connection opens;
//   - a listener that failed: waiting; one not bound yet: starting;
//   - running with the link up: ready;
//   - running with the link connecting, or, with no link reported,
//     after failures that accepted nothing since: waiting, as a
//     reconnect that has not proved itself is not a receiver that serves;
//   - running, no link reported, no failure: ready;
//   - running with the link degraded: degraded, with the adapter's code and
//     what it means; the receiver still receives, by polling, and is
//     ready again once the adapter reports its link up;
//   - ready, but a credential the configuration needs is missing:
//     degraded, as mail still arrives by polling; one that is not
//     receiving yet keeps its wait.
//
// A channel that has not settled reads starting rather than waiting until
// its settle deadline.
func (v *receiverView) compose(now time.Time) (ScopeState, string, bool) {
	state, reason, ok := v.composeLink(now)
	if !ok || v.gap == "" {
		return state, reason, ok
	}
	switch {
	case state == ScopeReady:
		// Mail still arrives by polling: degraded, not waiting.
		return ScopeDegraded, v.gap, true
	case v.degradedCode() != "":
		// The gap names the credential and what stays off; a degradation
		// the gap explains adds nothing, another is told after it.
		if meaningOf(v.degradedCode()).credential {
			return ScopeDegraded, v.gap, true
		}
		return ScopeDegraded, v.gap + "; " + reason, true
	}
	return state, reason, ok
}

// degradedCode is the adapter's code for what degrades the call, while
// the call runs and reports its link degraded; "" otherwise.
func (v *receiverView) degradedCode() string {
	if v.status.Phase == uma.ReceiverRunning && v.status.Link == uma.ReceiverDegraded {
		return v.status.Detail
	}
	return ""
}

// composeLink is compose before a gap in the credentials is weighed.
func (v *receiverView) composeLink(now time.Time) (ScopeState, string, bool) {
	st := v.status
	if st.Link != "" {
		v.reports = true
	}
	switch {
	case st.Phase == uma.ReceiverEnded:
		return "", "", false
	case st.Phase == uma.ReceiverRetrying:
		// A retry that opens after the settle deadline cannot settle the
		// channel by itself: it waits, at once.
		if st.RetryAt.After(v.settleBy) {
			return ScopeWaiting, retryingReason(st), true
		}
		return v.unsettled(now), retryingReason(st), true
	case v.listenErr != nil:
		return ScopeWaiting, "listener: " + v.listenErr.Error(), true
	case st.Phase == "":
		return "", "", false
	case v.serving && !v.bound:
		return ScopeStarting, "", true
	}
	link := st.Link
	if link == "" && v.reports {
		// The instance does not exist yet, or no longer: not connected.
		link = uma.ReceiverConnecting
	}
	switch {
	case link == uma.ReceiverConnected:
		return ScopeReady, "", true
	case link == uma.ReceiverDegraded:
		// Receiving, so not unsettled: the state is known.
		return ScopeDegraded, degradedReason(st.Detail), true
	case link == uma.ReceiverConnecting:
		return v.unsettled(now), connectingReason(st), true
	case st.Failures > 0:
		return v.unsettled(now), reconnectingReason(st), true
	}
	return ScopeReady, "", true
}

// unsettled is waiting once the channel has settled or its settle
// deadline passed, starting before.
func (v *receiverView) unsettled(now time.Time) ScopeState {
	if !v.settled.Load() && now.Before(v.settleBy) {
		return ScopeStarting
	}
	return ScopeWaiting
}

func failuresOf(n int) string {
	if n == 1 {
		return "1 failure"
	}
	return fmt.Sprintf("%d failures", n)
}

// retryingReason names the last failure's code, the failures counted and
// when the next connection opens.
func retryingReason(st uma.ReceiverStatus) string {
	return fmt.Sprintf("%s: retrying at %s after %s", codeName(st.Reason), st.RetryAt.UTC().Format(time.RFC3339), failuresOf(st.Failures))
}

// connectingReason is a link that is not up.
func connectingReason(st uma.ReceiverStatus) string {
	if st.Failures == 0 {
		return "connecting to the provider"
	}
	return fmt.Sprintf("connecting to the provider after %s, last %s", failuresOf(st.Failures), codeName(st.Reason))
}

// reconnectingReason is a call that opened a connection after failures
// and has accepted nothing yet.
func reconnectingReason(st uma.ReceiverStatus) string {
	return fmt.Sprintf("reconnected after %s, last %s; nothing received since", failuresOf(st.Failures), codeName(st.Reason))
}

func codeName(c model.ErrorCode) string {
	if c == "" {
		return "failed"
	}
	return string(c)
}

// degradedMeaning is what an adapter's degradation code means for an
// operator, and whether it is the same cause as a missing credential
// the channel's configuration needs (told once, by the gap).
type degradedMeaning struct {
	text       string
	credential bool
}

// degradedMeanings holds the codes the mail adapters report while they
// receive by polling alone. An adapter's code is stable; one not here
// is shown as it is.
var degradedMeanings = map[string]degradedMeaning{
	"google.mail.push_denied":                 {text: "push registration refused"},
	"google.mail.push_rejected":               {text: "push registration rejected"},
	"google.mail.push_unavailable":            {text: "push registration unavailable"},
	"microsoft.mail.subscription_rejected":    {text: "change-notification subscription rejected"},
	"microsoft.mail.client_state_missing":     {text: "change-notification secret missing", credential: true},
	"microsoft.mail.subscription_unavailable": {text: "change-notification subscription unavailable"},
}

func meaningOf(code string) degradedMeaning { return degradedMeanings[code] }

// degradedReason is the reason of a receiver that still receives while
// its adapter reports part of what it relies on failing: what it means
// and the code, and what the operator gets meanwhile.
func degradedReason(code string) string {
	m, known := degradedMeanings[code]
	switch {
	case known:
		return fmt.Sprintf("%s (%s): mail arrives by polling only", m.text, code)
	case code != "":
		return fmt.Sprintf("degraded (%s): still receiving", code)
	}
	return "degraded: still receiving"
}
