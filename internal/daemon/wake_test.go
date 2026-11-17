package daemon

import (
	"context"
	"testing"
	"time"
)

// waitOf runs w.Wait(d) in the background and reports how long it took.
func waitOf(w *wakeWaiter, ctx context.Context, d time.Duration) <-chan time.Duration {
	out := make(chan time.Duration, 1)
	started := make(chan struct{})
	go func() {
		start := time.Now()
		close(started)
		_ = w.Wait(ctx, d)
		out <- time.Since(start)
	}()
	<-started
	time.Sleep(20 * time.Millisecond) // inside Wait
	return out
}

// TestWakeWaiterEndsAnIdleWait: a nudge ends the wait of every worker
// idle now, at once; a wait of any other length (a receiver's backoff, a
// lease renewal) is a plain timer the nudge leaves alone.
func TestWakeWaiterEndsAnIdleWait(t *testing.T) {
	w := newWakeWaiter()
	w.setPoll(2 * time.Second)
	idle1 := waitOf(w, t.Context(), 2*time.Second)
	idle2 := waitOf(w, t.Context(), 2*time.Second)
	other := waitOf(w, t.Context(), 400*time.Millisecond)
	w.nudge()
	for i, c := range []<-chan time.Duration{idle1, idle2} {
		if d := <-c; d > time.Second {
			t.Errorf("idle wait %d ended after %s, want it woken", i, d)
		}
	}
	if d := <-other; d < 380*time.Millisecond {
		t.Errorf("a wait that is no worker's ended after %s, want it to run its length", d)
	}
}

// TestWakeWaiterRemembersANudgeOnce: a nudge that lands between a
// worker's empty claim round and its wait ends that wait at once, once.
func TestWakeWaiterRemembersANudgeOnce(t *testing.T) {
	w := newWakeWaiter()
	w.setPoll(300 * time.Millisecond)
	w.nudge()
	start := time.Now()
	if err := w.Wait(t.Context(), 300*time.Millisecond); err != nil || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("the wait after a nudge: %v after %s, want it to end at once", err, time.Since(start))
	}
	start = time.Now()
	if err := w.Wait(t.Context(), 300*time.Millisecond); err != nil || time.Since(start) < 280*time.Millisecond {
		t.Fatalf("the next wait: %v after %s, want it to run its length", err, time.Since(start))
	}
}

// TestWakeWaiterWithoutAPollWakesNothing: until the workers say what
// their idle wait is, a nudge wakes no wait, and a cancelled context
// ends one with its error.
func TestWakeWaiterWithoutAPollWakesNothing(t *testing.T) {
	w := newWakeWaiter()
	w.nudge()
	start := time.Now()
	if err := w.Wait(t.Context(), 200*time.Millisecond); err != nil || time.Since(start) < 180*time.Millisecond {
		t.Fatalf("wait = %v after %s, want a plain timer", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := w.Wait(ctx, time.Minute); err == nil {
		t.Error("a cancelled wait returned no error")
	}
}
