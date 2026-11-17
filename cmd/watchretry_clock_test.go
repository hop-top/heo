//go:build unix

package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"hop.top/kit/go/console/output"
)

// fakeClock is time that only the helper's waits move: an outage of
// minutes runs in no time, and what the helper decides from it is exact.
type fakeClock struct {
	t     time.Time
	slept time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (f *fakeClock) clock() clock {
	return clock{
		now: func() time.Time { return f.t },
		after: func(d time.Duration) <-chan time.Time {
			f.t = f.t.Add(d)
			f.slept += d
			ch := make(chan time.Time, 1)
			ch <- f.t
			return ch
		},
	}
}

// outageRun is a retry helper over a step that fails while the fake
// clock is inside one of its outages, and counts its calls.
type outageRun struct {
	t       *testing.T
	clk     *fakeClock
	said    *bytes.Buffer
	calls   int
	call    func() error
	outages [][2]time.Duration // [from, to) since the clock's start
	down    error
}

func newOutageRun(t *testing.T, reconnect time.Duration, down error, outages ...[2]time.Duration) *outageRun {
	t.Helper()
	r := &outageRun{t: t, clk: newFakeClock(), said: &bytes.Buffer{}, outages: outages, down: down}
	start := r.clk.t
	run := retryOn(r.clk.clock(), func(context.Context, struct{}) (int, error) {
		r.calls++
		at := r.clk.t.Sub(start)
		for _, o := range r.outages {
			if at >= o[0] && at < o[1] {
				return 0, r.down
			}
		}
		return r.calls, nil
	}, reconnect, r.said)
	r.call = func() error {
		_, err := run(context.Background(), struct{}{})
		return err
	}
	return r
}

// idle lets the watch sit between calls, as it does between polls.
func (r *outageRun) idle(d time.Duration) { r.clk.t = r.clk.t.Add(d) }

// since is how far the fake clock has run from its start.
func (r *outageRun) since(start time.Time) time.Duration { return r.clk.t.Sub(start) }

// TestRetryOutageClockStartsAtItsFirstFailure: the bound counts from the
// first failure of the outage, not from when the watch started, attached
// or last heard from the daemon: a watch that was quietly attached for
// an hour waits its whole bound when the daemon goes away.
func TestRetryOutageClockStartsAtItsFirstFailure(t *testing.T) {
	const bound = 10 * time.Second
	// Attached at the start; the daemon is down from an hour in, for 6s.
	r := newOutageRun(t, bound, prerequisite(), [2]time.Duration{time.Hour, time.Hour + 6*time.Second})
	if err := r.call(); err != nil {
		t.Fatal(err)
	}
	r.idle(time.Hour)
	if err := r.call(); err != nil {
		t.Fatalf("err = %v: a 6s outage under a 10s bound, an hour after the watch attached, ended it", err)
	}
	if r.clk.slept < 6*time.Second || r.clk.slept > bound {
		t.Errorf("waited %s for a 6s outage", r.clk.slept)
	}
}

// TestRetryGivesUpAtTheBoundFromTheFirstFailure: a daemon that never
// returns is waited for exactly the bound, said once, and ended with
// PREREQUISITE (exit 70) whether it refused or dropped the call.
func TestRetryGivesUpAtTheBoundFromTheFirstFailure(t *testing.T) {
	const bound = 2 * time.Second
	for name, down := range map[string]error{
		"refused":         prerequisite(),
		"connection lost": lostByClient(t, closeAfterRequest),
	} {
		t.Run(name, func(t *testing.T) {
			r := newOutageRun(t, bound, down, [2]time.Duration{time.Minute, 24 * time.Hour})
			if err := r.call(); err != nil {
				t.Fatal(err)
			}
			r.idle(time.Minute)
			err := r.call()
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != output.CodePrerequisite || oe.ExitCode != 70 {
				t.Fatalf("err = %v, want PREREQUISITE (exit 70) once the bound ran out", err)
			}
			if r.clk.slept != bound {
				t.Errorf("waited %s, want the %s bound exactly", r.clk.slept, bound)
			}
			if r.calls < 4 {
				t.Errorf("%d calls, want retries inside the bound", r.calls)
			}
			if got, want := r.said.String(), "daemon unavailable, waiting up to 2s\n"; got != want {
				t.Errorf("said %q, want %q", got, want)
			}
		})
	}
}

// TestRetryOutageStartsOverWhenACallSucceeds: two outages, each inside
// the bound, are two outages: the clock of the first does not carry into
// the second, and each says it is waiting.
func TestRetryOutageStartsOverWhenACallSucceeds(t *testing.T) {
	const bound = 10 * time.Second
	for name, down := range map[string]error{
		"refused":         prerequisite(),
		"connection lost": lostByClient(t, closeAfterRequest),
	} {
		t.Run(name, func(t *testing.T) {
			r := newOutageRun(t, bound, down,
				[2]time.Duration{time.Second, 7 * time.Second},
				[2]time.Duration{9 * time.Second, 15 * time.Second})
			begin := r.clk.t
			if err := r.call(); err != nil {
				t.Fatal(err)
			}
			r.idle(time.Second)
			if err := r.call(); err != nil {
				t.Fatalf("first outage: %v", err)
			}
			if at := r.since(begin); at < 7*time.Second {
				t.Fatalf("first outage ended at %s, before the daemon returned", at)
			}
			r.idle(9*time.Second - r.since(begin)) // on to the second outage
			if err := r.call(); err != nil {
				t.Fatalf("second outage, %s into the run and 6s long: %v", r.since(begin), err)
			}
			if n := strings.Count(r.said.String(), "waiting"); n != 2 {
				t.Errorf("said it was waiting %d times for two outages: %q", n, r.said)
			}
		})
	}
}
