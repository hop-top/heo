package daemon

import (
	"context"
	"sync"
	"time"
)

// wakeWaiter is the Waiter uma paces its workers with. It waits as a
// timer does, and a Nudge ends the wait of an idle worker at once, so
// work that must reach its provider within seconds (a Slack modal's
// trigger_id lives three) is not left to sit out the workers' poll,
// which is a second by default.
//
// uma's Run waits opts.Poll, through the Waiter, whenever a claim round
// finds nothing; the same Waiter paces a receiver's reconnect backoff
// and its lease renewal. Only a wait of exactly the workers' poll is a
// worker's idle wait, so a nudge wakes nothing else.
type wakeWaiter struct {
	mu sync.Mutex
	// poll is the workers' idle wait; zero, none is known and no wait is
	// woken.
	poll time.Duration
	// woken is closed, and replaced, by each nudge.
	woken chan struct{}
	// pending is a nudge no idle wait has entered since: a worker whose
	// claim round found nothing just before the nudge committed would
	// otherwise sleep through it. The next idle wait ends at once, once.
	pending bool
}

func newWakeWaiter() *wakeWaiter { return &wakeWaiter{woken: make(chan struct{})} }

// setPoll sets the duration of a worker's idle wait.
func (w *wakeWaiter) setPoll(d time.Duration) {
	w.mu.Lock()
	w.poll = d
	w.mu.Unlock()
}

// nudge ends the idle wait of every worker waiting now, and of the next
// one to start waiting.
func (w *wakeWaiter) nudge() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = true
	close(w.woken)
	w.woken = make(chan struct{})
}

// Wait is ports.Waiter: it waits d, or ctx's end, or, for a worker's idle
// wait, a nudge.
func (w *wakeWaiter) Wait(ctx context.Context, d time.Duration) error {
	var woken chan struct{}
	w.mu.Lock()
	if w.poll > 0 && d == w.poll {
		if w.pending {
			w.pending = false
			w.mu.Unlock()
			return nil
		}
		woken = w.woken
	}
	w.mu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	case <-woken: // nil, and never ready, for a wait that is no worker's
		return nil
	}
}
