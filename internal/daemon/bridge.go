package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/bridge"
	"hop.top/uma/model"
)

// BridgeScope is the bridge's scope in its service status: its
// subscription.
const BridgeScope = "consumer:" + bridge.Consumer

// bridgeOpenTimeout bounds opening the bridge's subscription before the
// service reports ready; a store that does not answer in time leaves the
// bridge degraded and retrying, not the daemon waiting.
const bridgeOpenTimeout = 10 * time.Second

// BridgeStatus is the event bridge's state: the subscription it reads,
// where it stands, and what it published last.
type BridgeStatus struct {
	bridge.Status
	// Dropped counts the events dropped for bus subscribers that fell
	// behind, since the hub was made.
	Dropped uint64 `json:"dropped" yaml:"dropped"`
}

// bridgeSvc republishes uma's committed domain events on the bus. It
// reads the event log as a durable subscription of its own, as its own
// reading actor (authz.BridgeActor), and never writes.
type bridgeSvc struct {
	h *Host
	s *serviceState

	ready atomic.Bool

	mu     sync.Mutex
	run    *running
	b      *bridge.Bridge
	cancel context.CancelFunc
	done   chan struct{}
}

// Bridge returns the event bridge service.
func (h *Host) Bridge() Service { return h.bridge }

func (br *bridgeSvc) Name() string { return NameBridge }

func (br *bridgeSvc) Validate() error {
	if br.h.opts.Hub == nil {
		return errors.New("no event bus configured")
	}
	return nil
}

// Ready reports whether the bridge runs on an owned store. A bridge
// that is retrying is ready, and degraded: it never gates the daemon.
func (br *bridgeSvc) Ready() bool {
	br.mu.Lock()
	r := br.run
	br.mu.Unlock()
	return br.ready.Load() && r != nil && r.storageHealthy()
}

// Start opens the store as its owner, if no runtime service has, opens
// the bridge's subscription (at the tail when it is new, at its
// checkpoint when it exists) and starts reading. Failing to open it is
// not failing to start: the bridge is degraded and retries.
func (br *bridgeSvc) Start(ctx context.Context, ready func()) error {
	if err := br.Validate(); err != nil {
		return err
	}
	br.s.reset()
	r, err := br.h.acquire(ctx)
	if err != nil {
		br.s.set(StateFailed, err)
		return err
	}
	client, err := r.engine.For(model.Principal{Tenant: r.deps.Tenant, Actor: authz.BridgeActor})
	if err != nil {
		_ = br.h.release(context.WithoutCancel(ctx))
		br.s.set(StateFailed, err)
		return err
	}
	b := bridge.New(bridge.Options{
		Source: client.Events(), Publisher: br.h.opts.Hub, Logger: r.deps.Logger,
		OnChange: br.report,
	})
	br.s.scope(ScopeStatus{Scope: BridgeScope, State: ScopeStarting})
	octx, ocancel := context.WithTimeout(ctx, bridgeOpenTimeout)
	if err := b.Start(octx); err != nil {
		r.logError("runtime: event bridge could not open its subscription yet; retrying", "error", err)
	}
	ocancel()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	br.mu.Lock()
	br.run, br.b, br.cancel, br.done = r, b, cancel, done
	br.mu.Unlock()
	go func() {
		defer close(done)
		b.Run(runCtx)
	}()
	br.report(b.Status())
	br.ready.Store(true)
	br.s.set(StateReady, nil)
	r.logInfo("runtime: event bridge running", "consumer", bridge.Consumer)
	ready()
	select {
	case <-ctx.Done():
		return nil
	case <-r.Lost():
		br.ready.Store(false)
		err := r.lostError()
		br.s.set(StateFailed, err)
		return err
	}
}

// report records the bridge's state as its scope's: ready while it reads
// and publishes, waiting while it retries.
func (br *bridgeSvc) report(st bridge.Status) {
	sc := ScopeStatus{Scope: BridgeScope, State: ScopeReady}
	switch st.State {
	case bridge.StateStarting:
		sc.State = ScopeStarting
	case bridge.StateDegraded:
		sc.State, sc.Reason = ScopeWaiting, "retrying after "+st.Code
	}
	br.s.scope(sc)
}

// Stop ends the read: a batch in hand is published and acknowledged,
// within ctx; the subscription is left open at its checkpoint, so the
// next start resumes where this one stopped. The store is released as
// the last runtime service stops.
func (br *bridgeSvc) Stop(ctx context.Context) error {
	br.ready.Store(false)
	br.mu.Lock()
	r, cancel, done := br.run, br.cancel, br.done
	br.run, br.cancel, br.done = nil, nil, nil
	br.mu.Unlock()
	if r == nil {
		return nil
	}
	br.s.set(StateStopping, nil)
	cancel()
	var errs []error
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, errors.New("event bridge did not return before the stop deadline; "+
			"its batch in hand is redelivered on the next start"))
	}
	if err := br.h.release(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		br.s.set(StateFailed, err)
		r.logError("runtime: event bridge stopped uncleanly", "error", err)
		return err
	}
	br.s.set(StateStopped, nil)
	r.logInfo("runtime: event bridge stopped")
	return nil
}

// status returns the bridge's state, nil while it is not running.
func (br *bridgeSvc) status() *BridgeStatus {
	br.mu.Lock()
	b := br.b
	run := br.run
	br.mu.Unlock()
	if b == nil || run == nil {
		return nil
	}
	st := BridgeStatus{Status: b.Status()}
	if br.h.opts.Hub != nil {
		st.Dropped = br.h.opts.Hub.Dropped()
	}
	return &st
}
