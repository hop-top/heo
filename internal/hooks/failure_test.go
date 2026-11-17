package hooks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/hooks/hookstest"
	"hop.top/heo/internal/plugins"
)

// TestFailureClasses: each way a plugin can fail has its class, from
// the real process, in both modes.
func TestFailureClasses(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	l := hooks.Launch{MaxOutput: 1000}
	cases := []struct {
		mode      string
		class     hooks.Class
		permanent bool
		timeout   time.Duration
	}{
		{hookstest.ModeExit1, hooks.ClassRetryable, false, 0},
		{hookstest.ModeExit2, hooks.ClassPermanent, true, 0},
		{hookstest.ModeExit3, hooks.ClassCrash, false, 0},
		{hookstest.ModeKill, hooks.ClassCrash, false, 0},
		{hookstest.ModeSlow, hooks.ClassTimeout, false, 150 * time.Millisecond},
		{hookstest.ModeBadJSON, hooks.ClassMalformed, false, 0},
		{hookstest.ModeBig, hooks.ClassMalformed, false, 0},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			name := tc.mode
			args := []string{log, tc.mode}
			if stream {
				name += "/stream"
				args = []string{log, tc.mode + ":stream"}
			}
			t.Run(name, func(t *testing.T) {
				timeout := tc.timeout
				if timeout == 0 {
					timeout = 5 * time.Second
				}
				var ex hooks.Executor = &hooks.ExecProcess{Command: cmd, Args: args, Timeout: timeout, Launch: l}
				if stream {
					ex = &hooks.StreamProcess{Command: cmd, Args: args, Timeout: timeout, Launch: l}
				}
				defer func() { _ = ex.Close() }() // teardown
				_, err := ex.Run(context.Background(), env("e1"))
				if err == nil {
					t.Fatal("no error")
				}
				if got := hooks.Classify(err); got != tc.class {
					t.Errorf("class %q, want %q (%v)", got, tc.class, err)
				}
				if hooks.IsPermanent(err) != tc.permanent {
					t.Errorf("permanent %v", hooks.IsPermanent(err))
				}
			})
		}
	}
}

func TestClassify(t *testing.T) {
	op := func(err error) error { return &hooks.OpError{Index: 2, Verb: "message note", Applied: 2, Err: err} }
	for name, tc := range map[string]struct {
		err   error
		class hooks.Class
		index int
		has   bool
	}{
		"plain":                 {errors.New("store busy"), hooks.ClassError, 0, false},
		"operation":             {op(errors.New("store busy")), hooks.ClassOperation, 2, true},
		"gate refusal":          {op(hooks.Permanent(errors.New("denied"))), hooks.ClassPermanent, 2, true},
		"permanent":             {hooks.Permanent(errors.New("x")), hooks.ClassPermanent, 0, false},
		"malformed by applier":  {&hooks.Failure{Class: hooks.ClassMalformed, Reason: "result rejected"}, hooks.ClassMalformed, 0, false},
		"unclassified failure":  {&hooks.Failure{Reason: "x"}, hooks.ClassError, 0, false},
		"permanent beats class": {&hooks.Failure{Permanent: true, Class: hooks.ClassCrash}, hooks.ClassPermanent, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hooks.Classify(tc.err); got != tc.class {
				t.Errorf("class %q, want %q", got, tc.class)
			}
			if i, ok := hooks.FailureIndex(tc.err); i != tc.index || ok != tc.has {
				t.Errorf("index %d,%v want %d,%v", i, ok, tc.index, tc.has)
			}
		})
	}
}

// TestBackoffJitter: equal jitter. The wait is in [d/2, d] for the
// exponential delay d, capped at Max; the draw decides where.
func TestBackoffJitter(t *testing.T) {
	b := plugins.Backoff{Initial: 100 * time.Millisecond, Max: 450 * time.Millisecond}
	low := func(int64) int64 { return 0 }
	high := func(n int64) int64 { return n - 1 }
	for n, d := range map[int]time.Duration{1: 100, 2: 200, 3: 400, 4: 450, 9: 450} {
		d *= time.Millisecond
		if got := hooks.Backoff(b, n, low); got != d/2 {
			t.Errorf("attempt %d, lowest draw: %s, want %s", n, got, d/2)
		}
		if got := hooks.Backoff(b, n, high); got != d {
			t.Errorf("attempt %d, highest draw: %s, want %s", n, got, d)
		}
	}
	seen := map[time.Duration]bool{}
	for range 200 {
		got := hooks.Backoff(b, 3, nil)
		if got < 200*time.Millisecond || got > 400*time.Millisecond {
			t.Fatalf("out of [200ms, 400ms]: %s", got)
		}
		seen[got] = true
	}
	if len(seen) < 20 {
		t.Errorf("only %d distinct waits in 200 draws: no jitter", len(seen))
	}
}
