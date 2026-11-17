package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// receivers runs the persistent receiver of every channel that
// receives: its adapter declares receive and the channel's capability
// status does not report it disabled. One owner per provider account.
type receivers struct {
	h *Host
	s *serviceState

	ready atomic.Bool

	mu     sync.Mutex
	run    *running
	ctx    context.Context
	cancel context.CancelFunc
	// settleTimeout is services.receivers.settle_timeout.
	settleTimeout time.Duration
	// startSpread is services.receivers.start_spread.
	startSpread time.Duration
	// runs is each running receiver, by channel.
	runs map[model.ID]*receiverRun
	// starting collects the first start's receivers, whose settling
	// the service waits for before it reports ready.
	starting bool
	settled  []chan struct{}
	loops    sync.WaitGroup
}

// receiverRun is one channel's receiver loop.
type receiverRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Receivers returns the receivers service.
func (h *Host) Receivers() Service { return &receivers{h: h, s: h.svcs[NameReceivers]} }

func (rc *receivers) Name() string { return NameReceivers }

func (rc *receivers) Validate() error {
	_, probs := ReadReceivers(rc.h.viper())
	return problemsError(probs)
}

// Ready reports whether receivers run on an owned store and every
// channel has settled: its receiver serves (for a listener adapter, its
// listener is bound; and for any, uma reports its call running with the
// adapter's link up, or with no link to report and no failure since it
// last accepted anything), or it waits or failed. A channel still
// connecting or retrying has not settled until it serves or
// services.receivers.settle_timeout passes; then it waits. A channel that cannot receive degrades the service; it
// does not make it unready.
func (rc *receivers) Ready() bool {
	rc.mu.Lock()
	r := rc.run
	rc.mu.Unlock()
	return rc.ready.Load() && r != nil && r.storageHealthy()
}

// Start reads the profile's channels and starts a receiver for each
// one that receives; a channel whose configuration switches receive off
// gets none, and no scope. Two channels of the profile on one provider
// account, or an account another daemon on this host already
// receives, refuse the start: nothing is received twice. A channel
// that cannot receive for any other reason is reported by scope and
// skipped. The service reports ready once every receiver has settled
// (see Ready); with no channel at all, at once. While it runs, a
// reconcile starts, replaces and stops receivers as channels change.
func (rc *receivers) Start(ctx context.Context, ready func()) error {
	cfg, probs := ReadReceivers(rc.h.viper())
	if err := problemsError(probs); err != nil {
		return err
	}
	rc.s.reset()
	r, err := rc.h.acquire(ctx)
	if err != nil {
		rc.s.set(StateFailed, err)
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	rc.mu.Lock()
	rc.run, rc.ctx, rc.cancel, rc.settleTimeout, rc.startSpread = r, runCtx, cancel, cfg.SettleTimeout, cfg.StartSpread
	rc.runs = map[model.ID]*receiverRun{}
	rc.starting, rc.settled = true, nil
	rc.mu.Unlock()
	if err := r.attach(ctx, rc); err != nil {
		cancel()
		rc.mu.Lock()
		rc.run, rc.ctx, rc.cancel, rc.runs, rc.starting = nil, nil, nil, nil, false
		rc.mu.Unlock()
		_ = rc.h.release(context.WithoutCancel(ctx))
		rc.s.set(StateFailed, err)
		return err
	}
	rc.mu.Lock()
	settled, n := rc.settled, len(rc.runs)
	rc.starting, rc.settled = false, nil
	rc.mu.Unlock()
	for _, done := range settled {
		select {
		case <-done:
		case <-ctx.Done():
			return nil
		case <-r.Lost():
			return rc.lost(r)
		}
	}
	// A receiver that returns on the cancellation settles too, so the
	// loop above can take a settle over the canceled context when both
	// are ready: a canceled start is not ready.
	if ctx.Err() != nil {
		return nil
	}
	rc.ready.Store(true)
	rc.s.set(StateReady, nil)
	r.logInfo("runtime: receivers running", "channels", n)
	ready()
	select {
	case <-ctx.Done():
		return nil
	case <-r.Lost():
		return rc.lost(r)
	}
}

// lost marks the service failed with the store ownership r lost.
func (rc *receivers) lost(r *running) error {
	rc.ready.Store(false)
	err := r.lostError()
	rc.s.set(StateFailed, err)
	return err
}

// startChannels starts a receiver for each channel that receives,
// reporting those that cannot by scope. A channel whose configuration
// switches receive off is skipped without a scope: it is not a fault.
func (rc *receivers) startChannels(r *running, chans []inbound) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.ctx == nil {
		return
	}
	for _, in := range chans {
		ch := in.ch
		switch {
		case !in.known:
			rc.s.scope(channelScope(ch, ScopeFailed, fmt.Sprintf("adapter %s is not in this heo build", ch.Type)))
			r.logError("runtime: channel skipped", "scope", scopeName(ch), "reason", "adapter not in this build", "type", ch.Type)
		case in.disabled[model.CapabilityReceive]:
			r.logInfo("runtime: receiver not started", "scope", scopeName(ch), "reason", "receive disabled by the channel's configuration")
		case !in.receives(model.CapabilityReceive):
		case in.unusable() != nil:
			err := in.unusable()
			rc.s.scope(channelScope(ch, ScopeFailed, err.Error()))
			r.logError("runtime: channel skipped", "scope", scopeName(ch), "reason", err)
		default:
			ctx, cancel := context.WithCancel(rc.ctx) //nolint:gosec // G118: stored in the run and cancelled when the run stops
			run := &receiverRun{cancel: cancel, done: make(chan struct{})}
			settled := make(chan struct{})
			rc.runs[ch.Ref.ID] = run
			if rc.starting {
				rc.settled = append(rc.settled, settled)
			}
			rc.loops.Add(1)
			// Only the daemon's first start spreads: a channel added or
			// changed while it runs starts at once.
			var spread time.Duration
			if rc.starting && in.ad.Staggered {
				spread = rc.startSpread
			}
			go rc.receive(ctx, r, in, run, settled, time.Now().Add(rc.settleTimeout), spread)
		}
	}
}

// stopChannels stops each channel's receiver, waiting within ctx for
// it to end and give up its receiver lease, and drops its scope. A
// stopped receiver reports nothing more.
func (rc *receivers) stopChannels(ctx context.Context, chans []inbound) {
	var stopped []*receiverRun
	rc.mu.Lock()
	for _, in := range chans {
		if run := rc.runs[in.ch.Ref.ID]; run != nil {
			delete(rc.runs, in.ch.Ref.ID)
			run.cancel()
			stopped = append(stopped, run)
		}
		rc.s.drop(scopeName(in.ch))
	}
	rc.mu.Unlock()
	for _, run := range stopped {
		select {
		case <-run.done:
		case <-ctx.Done():
			return
		}
	}
}

// report sets channel id's scope while run is still its receiver.
func (rc *receivers) report(id model.ID, run *receiverRun, sc ScopeStatus) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.runs[id] == run {
		rc.s.scope(sc)
	}
}

// receive runs one channel's receiver until ctx ends or the channel
// fails for good. A receiver lease a previous daemon still holds is
// waited out; what uma's receive call retries itself, it retries inside
// the call, and its scope follows uma's status of the call (see
// receiverView): ready once the adapter reports its link up, or, for
// an adapter that reports none, while the call runs and has not just
// failed. A call that returns is restarted with backoff when its failure
// is retryable. settled is closed once the channel first settles: its
// receiver serves, waits or failed. A listener adapter's receiver serves
// once the adapter reports its listener bound; until then the channel
// reads starting. A channel still connecting or retrying before it first
// settles keeps reading starting until settleBy: a transient start
// failure, such as a lease take that lost a serialization race, is not a
// settled channel. At settleBy it settles waiting.
func (rc *receivers) receive(ctx context.Context, r *running, in inbound, run *receiverRun, settled chan struct{}, settleBy time.Time, spread time.Duration) {
	defer rc.loops.Done()
	defer close(run.done)
	// spreadEnd is when uma's start spread, which holds the first
	// connection by a per-channel offset inside the window, is over. uma
	// reports no state for the hold: the call reads running, with no link
	// yet, so until the window ends the channel reads starting. The first
	// call only: a restart is not held again.
	var spreadEnd atomic.Int64
	if spread > 0 {
		spreadEnd.Store(time.Now().Add(spread).UnixNano())
	}
	report := func(sc ScopeStatus) { rc.report(in.ch.Ref.ID, run, sc) }
	scope := func(sc ScopeStatus) {
		if end := spreadEnd.Load(); end != 0 && time.Now().UnixNano() < end && sc.State != ScopeFailed {
			sc.State, sc.Reason = ScopeStarting, fmt.Sprintf("first connection spread over %s", spread.Round(time.Second))
		}
		report(sc)
	}
	var once sync.Once
	var isSettled atomic.Bool
	settle := func() { once.Do(func() { isSettled.Store(true); close(settled) }) }
	defer settle()
	ch, serving := in.ch, in.ad.Serving
	rt := r.engine.Runtime()
	poll := cmp.Or(r.deps.ReceiverStatusPoll, DefaultReceiverStatusPoll)
	view := &receiverView{
		scope: scope, settle: settle, ch: channelScope(ch, "", ""), serving: serving != nil, reports: in.ad.ReportsLink,
		settleBy: settleBy, settled: &isSettled, poll: poll, fast: min(poll, max(poll/20, 5*time.Millisecond)),
		read: func(ctx context.Context) (uma.ReceiverStatus, error) {
			// Memory first: a receive call this process runs answers
			// without a store unit. Only a call not running here, not
			// yet registered or already returned falls to the store.
			if st, ok := rt.ReceiverState(r.call(), ch.Ref); ok {
				return st, nil
			}
			return rt.ReceiverStatus(ctx, r.call(), ch.Ref)
		},
		log: func(sc ScopeStatus) {
			if sc.State == ScopeReady {
				r.logInfo("runtime: receiver ready", "scope", scopeName(ch))
			} else {
				r.logError("runtime: receiver not ready", "scope", scopeName(ch), "state", sc.State, "reason", sc.Reason)
			}
		},
	}
	view.gapCheck = rc.gapCheck(r, in)
	if serving != nil {
		stop := serving.Watch(ch.Ref, func(err error) { view.listener(err) })
		defer stop()
	}
	owner := r.deps.Owner + "/receiver"
	delay := minReceiveBackoff
	scope(channelScope(ch, ScopeStarting, ""))
	if spread > 0 {
		// Readiness never waits for a spread start.
		settle()
	}
	for first := true; ; first = false {
		started := time.Now()
		var opts []uma.ReceiveOption
		if first && spread > 0 {
			opts = append(opts, uma.ReceiveStartSpread(spread))
		} else {
			spreadEnd.Store(0)
		}
		err := rc.receiveCall(ctx, r, ch, owner, view, opts...)
		if err == nil || ctx.Err() != nil {
			return
		}
		delay = receiveBackoff(delay, time.Since(started))
		if model.IsCode(err, model.Conflict) {
			st, serr := rt.ReceiverStatus(ctx, r.call(), ch.Ref)
			wait := delay
			reason := "receiver lease held elsewhere"
			switch {
			case serr == nil && st.Owner != "":
				wait = max(time.Until(st.Until), 0) + time.Second
				reason = fmt.Sprintf("receiver lease held by %s until %s", st.Owner, st.Until.UTC().Format(time.RFC3339))
			case serr == nil:
				// Nobody holds the lease, so the conflict is the provider's
				// own, such as a Telegram bot whose webhook is set refusing
				// getUpdates: it ends when the operator changes the
				// provider, not when a lease expires, so ask again ever
				// less often rather than at the minimum interval.
				reason = "provider conflict: " + safeMessage(err)
				delay = min(delay*2, maxReceiveBackoff)
			}
			scope(channelScope(ch, ScopeWaiting, reason))
			settle()
			r.logInfo("runtime: receiver waiting", "scope", scopeName(ch), "reason", reason)
			if !sleep(ctx, wait) {
				return
			}
			continue
		}
		if !isRetryable(err) {
			scope(channelScope(ch, ScopeFailed, err.Error()))
			settle()
			r.logError("runtime: receiver failed", "scope", scopeName(ch), "error", err)
			return
		}
		// uma's call gave up after its attempts, each of which waited its
		// backoff or the longer wait its failure stated; the failure that
		// ended it states one too, which nothing has waited yet. The wait
		// is the backoff, or that longer one (a rate limit's reset, such
		// as Discord's identify budget): asking sooner only fails again.
		wait := delay
		if ra := retryAfter(err); ra > delay {
			wait = ra
		} else {
			wait = jitter(delay, phaseOf(r.deps.Phase)())
		}
		reason := err.Error()
		if wait > delay*11/10 {
			reason += fmt.Sprintf(" (retrying in %s)", wait.Round(time.Second))
		}
		if left := time.Until(settleBy); !isSettled.Load() && left > 0 {
			// Still starting: settle only if the settle timeout passes
			// before the retry.
			scope(channelScope(ch, ScopeStarting, "retrying: "+reason))
			r.logError("runtime: receiver failed starting; retrying", "scope", scopeName(ch), "error", err, "retry_in", wait)
			if left < wait {
				if !sleep(ctx, left) {
					return
				}
				wait -= left
				scope(channelScope(ch, ScopeWaiting, reason))
				settle()
				r.logError("runtime: receiver not started within the settle timeout; retrying", "scope", scopeName(ch), "error", err, "retry_in", wait)
			}
		} else {
			scope(channelScope(ch, ScopeWaiting, reason))
			settle()
			r.logError("runtime: receiver failed; retrying", "scope", scopeName(ch), "error", err, "retry_in", wait)
		}
		if !sleep(ctx, wait) {
			return
		}
		delay = min(delay*2, maxReceiveBackoff)
	}
}

// gapCheck is how the receiver of in finds a credential its channel's
// configuration needs (adapters.Adapter.CredentialIf) missing: the
// adapter runs without the feature the credential serves and says
// nothing, so the channel reads waiting and not ready, with the
// credential and what stays off. Nil for a channel that needs none.
func (rc *receivers) gapCheck(r *running, in inbound) func(context.Context) string {
	purposes := in.ad.ConditionalPurposes(in.ch)
	if len(purposes) == 0 || r.deps.Credentials == nil {
		return nil
	}
	return func(ctx context.Context) string {
		for _, p := range purposes {
			_, err := r.deps.Credentials.Resolve(ctx, ports.CredentialRequest{
				Channel: in.ch.Ref, Reference: in.ch.CredentialRef, Purpose: p, Accept: in.ad.CredentialKinds[p],
			})
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return ""
			}
			msg := fmt.Sprintf("credential %s: %s", p, safeMessage(err))
			if e := in.ad.CredentialIf[p].Effect; e != "" {
				msg += "; " + e
			}
			return msg
		}
		return ""
	}
}

// receiveCall runs one uma receive call, reading its status into view
// while it runs; the view is the channel's scope until the call returns.
func (rc *receivers) receiveCall(ctx context.Context, r *running, ch model.Channel, owner string, view *receiverView, opts ...uma.ReceiveOption) error {
	wctx, stop := context.WithCancel(ctx)
	watched := make(chan struct{})
	view.begin()
	go func() { defer close(watched); view.watch(wctx) }()
	err := r.engine.Runtime().Receive(ctx, r.call(), ch.Ref, append([]uma.ReceiveOption{uma.ReceiveOwner(owner)}, opts...)...)
	stop()
	<-watched
	view.end()
	return err
}

// safeMessage is err's message without its code prefix, as an adapter
// worded it for an operator.
func safeMessage(err error) string {
	if me, ok := errors.AsType[*model.Error](err); ok && me.SafeMessage != "" {
		return me.SafeMessage
	}
	return err.Error()
}

// heo's receiver restart backoff: around uma's own reconnects, which a
// Receive call exhausts before it returns.
const (
	minReceiveBackoff = time.Second
	maxReceiveBackoff = 5 * time.Minute
	// maxReceiveRetryAfter bounds a wait the provider states.
	maxReceiveRetryAfter = 24 * time.Hour
)

// retryAfter is the wait a failure states, as a rate limit does: zero
// when it states none, and never past maxReceiveRetryAfter.
func retryAfter(err error) time.Duration {
	if me, ok := errors.AsType[*model.Error](err); ok {
		return min(max(me.RetryAfter, 0), maxReceiveRetryAfter)
	}
	return 0
}

// jitter spreads d by up to +-10%, by phase in [0,1), so channels that
// failed together do not retry together.
func jitter(d time.Duration, phase float64) time.Duration {
	return d + time.Duration((phase*2-1)*0.1*float64(d))
}

// phaseOf is f, or the process's random source when nil.
func phaseOf(f func() float64) func() float64 {
	if f != nil {
		return f
	}
	return rand.Float64
}

// receiveBackoff is the wait before restarting a receiver that ran for
// ran and failed, after waiting delay before that run: a receiver that
// ran longer than the longest wait was healthy, so its failures count
// afresh.
func receiveBackoff(delay, ran time.Duration) time.Duration {
	if ran >= maxReceiveBackoff {
		return minReceiveBackoff
	}
	return delay
}

// Stop stops every receiver and releases the runtime; the last service
// to stop gives up the account claims.
func (rc *receivers) Stop(ctx context.Context) error {
	rc.ready.Store(false)
	rc.mu.Lock()
	r := rc.run
	rc.mu.Unlock()
	if r == nil {
		return nil
	}
	// No reconcile reaches the service from here on.
	r.detach(rc)
	rc.mu.Lock()
	cancel := rc.cancel
	rc.run, rc.ctx, rc.cancel, rc.runs = nil, nil, nil, nil
	rc.mu.Unlock()
	rc.s.set(StateStopping, nil)
	cancel()
	var errs []error
	if !waitGroup(ctx, &rc.loops) {
		errs = append(errs, errors.New("receivers did not stop before the stop deadline"))
	}
	if err := rc.h.release(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		rc.s.set(StateFailed, err)
		return err
	}
	rc.s.set(StateStopped, nil)
	return nil
}

// scopeName is a channel's scope: channel:<id>.
func scopeName(ch model.Channel) string { return "channel:" + string(ch.Ref.ID) }

// channelName names a channel for an operator: its key, else its ID.
func channelName(ch model.Channel) string {
	if ch.Key != "" {
		return fmt.Sprintf("%q (%s)", ch.Key, ch.Ref.ID)
	}
	return string(ch.Ref.ID)
}

func channelScope(ch model.Channel, st ScopeState, reason string) ScopeStatus {
	return ScopeStatus{Scope: scopeName(ch), Key: ch.Key, Type: string(ch.Type), State: st, Reason: reason}
}
