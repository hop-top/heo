package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/kit/go/console/serve"
	"hop.top/uma"
	"hop.top/uma/model"
)

// Service is a runtime service on kit's supervisor.
type Service interface {
	serve.Service
	serve.Validator
}

// workers runs queued uma operations: services.workers.count
// Runtime.Run loops, each claiming one item at a time.
type workers struct {
	h *Host
	s *serviceState

	ready    atomic.Bool
	stopping atomic.Bool

	mu     sync.Mutex
	run    *running
	cfg    WorkersConfig
	cancel context.CancelFunc
	loops  sync.WaitGroup
}

// Workers returns the workers service.
func (h *Host) Workers() Service { return &workers{h: h, s: h.svcs[NameWorkers]} }

func (w *workers) Name() string { return NameWorkers }

func (w *workers) Validate() error {
	_, probs := ReadWorkers(w.h.viper())
	return problemsError(probs)
}

// Ready reports whether the workers run on an owned store.
func (w *workers) Ready() bool {
	w.mu.Lock()
	r := w.run
	w.mu.Unlock()
	return w.ready.Load() && r != nil && r.storageHealthy()
}

// Start opens the store as its owner, if no runtime service has, and
// starts the worker loops. The loops run on a context of their own:
// cancelling ctx, as a signal does, stops nothing until Stop drains.
func (w *workers) Start(ctx context.Context, ready func()) error {
	cfg, probs := ReadWorkers(w.h.viper())
	if err := problemsError(probs); err != nil {
		return err
	}
	w.s.reset()
	w.stopping.Store(false)
	r, err := w.h.acquire(ctx)
	if err != nil {
		w.s.set(StateFailed, err)
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	w.mu.Lock()
	w.run, w.cfg, w.cancel = r, cfg, cancel
	w.mu.Unlock()
	r.wake.setPoll(cfg.Poll)
	for i := range cfg.Concurrency {
		w.loops.Add(1)
		go w.loop(runCtx, r, cfg, i)
	}
	w.ready.Store(true)
	w.s.set(StateReady, nil)
	r.logInfo("runtime: workers running", "concurrency", cfg.Concurrency)
	ready()
	select {
	case <-ctx.Done():
		return nil
	case <-r.Lost():
		w.ready.Store(false)
		err := r.lostError()
		w.s.set(StateFailed, err)
		return err
	}
}

// workerOptions is the options one worker runs under.
func workerOptions(owner string, cfg WorkersConfig) uma.WorkerOptions {
	return uma.WorkerOptions{
		Owner: owner, Limit: 1, Lease: cfg.Lease, Poll: cfg.Poll,
		SweepEvery: cfg.HeldSweepEvery,
	}
}

// loop runs one worker until a drain or close ends it. A run that fails
// is reported on the worker's scope and retried after a backoff.
func (w *workers) loop(ctx context.Context, r *running, cfg WorkersConfig, i int) {
	defer w.loops.Done()
	scope := "worker:" + strconv.Itoa(i)
	opts := workerOptions(r.deps.Owner+"/worker/"+strconv.Itoa(i), cfg)
	delay := time.Second
	for {
		w.s.scope(ScopeStatus{Scope: scope, State: ScopeReady})
		err := r.engine.Runtime().Run(ctx, r.call(), opts)
		if err == nil || ctx.Err() != nil || w.stopping.Load() {
			return
		}
		w.s.scope(ScopeStatus{Scope: scope, State: ScopeWaiting, Reason: err.Error()})
		r.logError("runtime: worker failed; retrying", "scope", scope, "error", err, "retry_in", delay)
		if !sleep(ctx, delay) {
			return
		}
		delay = min(delay*2, time.Minute)
	}
}

// Stop drains: no new claims, claimed work finishes, within
// services.workers.drain_timeout and inside ctx. Then, as the last
// runtime service to stop, it closes the engine and releases the store.
// Work still running at the deadline is abandoned to its lease, and
// Stop reports it.
func (w *workers) Stop(ctx context.Context) error {
	w.ready.Store(false)
	w.stopping.Store(true)
	w.mu.Lock()
	r, cfg, cancel := w.run, w.cfg, w.cancel
	w.run, w.cancel = nil, nil
	w.mu.Unlock()
	if r == nil {
		return nil
	}
	w.s.set(StateStopping, nil)
	limit := drainLimit(ctx, cfg.DrainTimeout)
	dctx, dcancel := context.WithTimeout(ctx, limit)
	// A second shutdown signal ends the wait at once.
	unwatch := context.AfterFunc(w.h.abort, dcancel)
	derr := r.engine.Runtime().Drain(dctx, r.call())
	unwatch()
	dcancel()
	var errs []error
	if derr != nil {
		// Abandon what is still running: its claims keep their leases
		// and the work resumes on the next start.
		if w.h.abort.Err() != nil {
			errs = append(errs, errors.New(
				"drain abandoned on a second signal; claimed work stays leased and resumes on the next start"))
		} else {
			errs = append(errs, fmt.Errorf(
				"drain did not finish within %s; claimed work stays leased and resumes on the next start", limit))
		}
	}
	cancel()
	if !waitGroup(ctx, &w.loops) {
		errs = append(errs, errors.New("workers did not return before the stop deadline"))
	}
	if err := w.h.release(ctx); err != nil {
		errs = append(errs, err)
	}
	err := errors.Join(errs...)
	if err != nil {
		w.s.set(StateFailed, err)
		r.logError("runtime: workers stopped uncleanly", "error", err)
		return err
	}
	w.s.set(StateStopped, nil)
	r.logInfo("runtime: workers drained")
	return nil
}

// drainLimit is the drain deadline: the configured timeout, cut short
// so the engine close and store release still fit in ctx.
func drainLimit(ctx context.Context, configured time.Duration) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return configured
	}
	left := time.Until(dl)
	reserve := min(left/5, 5*time.Second)
	return max(min(configured, left-reserve), 0)
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

// waitGroup waits for wg until ctx ends, reporting whether it finished.
func waitGroup(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// isRetryable reports whether err is a uma failure worth retrying as is.
func isRetryable(err error) bool {
	var me *model.Error
	if !errors.As(err, &me) {
		return true
	}
	return me.Code.DefaultRetry() == model.RetrySameOperation &&
		(me.Retry == "" || me.Retry == model.RetrySameOperation)
}
