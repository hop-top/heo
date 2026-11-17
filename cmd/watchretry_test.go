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

// retrying runs the helper over a step scripted as a list of results,
// the last one repeating, and returns what it answered, the steps run
// and what it said on stderr.
func retrying(t *testing.T, reconnect time.Duration, script ...error) (call func() error, calls *int, said *bytes.Buffer) {
	t.Helper()
	old := retryBase
	retryBase = 5 * time.Millisecond
	t.Cleanup(func() { retryBase = old })
	n := 0
	said = &bytes.Buffer{}
	run := retryTransient(func(context.Context, struct{}) (int, error) {
		err := script[min(n, len(script)-1)]
		n++
		return n, err
	}, reconnect, said)
	// A helper that waits where it should not fails the test, not hangs it.
	return func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := run(ctx, struct{}{})
		return err
	}, &n, said
}

func prerequisite() error { return output.PrerequisiteError("no heo daemon is running") }

// TestRetryFailsFastBeforeAttached: with no daemon answering, a watch
// that has not attached yet gets the PREREQUISITE refusal at once, once
// tried, however long the reconnect bound.
func TestRetryFailsFastBeforeAttached(t *testing.T) {
	call, n, said := retrying(t, time.Minute, prerequisite())
	err := call()
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != output.CodePrerequisite || *n != 1 || said.Len() != 0 {
		t.Fatalf("err %v after %d tries, said %q; want PREREQUISITE after one try, silently", err, *n, said)
	}
}

// TestRetryBoundsAttachedOutage: once attached, a refusal as PREREQUISITE
// is waited out for the bound — one line on stderr, then the refusal —
// and a daemon that returns inside the bound is carried on with.
func TestRetryBoundsAttachedOutage(t *testing.T) {
	// Attached, then gone for good.
	call, n, said := retrying(t, 60*time.Millisecond, nil, prerequisite())
	if err := call(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := call()
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != output.CodePrerequisite {
		t.Fatalf("err = %v, want the PREREQUISITE refusal once the bound ran out", err)
	}
	if took := time.Since(start); took < 50*time.Millisecond || took > 2*time.Second || *n < 3 {
		t.Errorf("gave up after %s and %d tries, want about the 60ms bound with retries", took, *n)
	}
	if got := said.String(); got != "daemon unavailable, waiting up to 60ms\n" {
		t.Errorf("said %q, want the waiting line once", got)
	}

	// Attached, gone for a moment, back.
	call, n, said = retrying(t, time.Minute, nil, prerequisite(), prerequisite(), nil)
	if err := call(); err != nil {
		t.Fatal(err)
	}
	if err := call(); err != nil || *n != 4 {
		t.Fatalf("err %v after %d tries, want success on the fourth", err, *n)
	}
	if strings.Count(said.String(), "waiting") != 1 {
		t.Errorf("said %q, want the waiting line once for one outage", said)
	}

	// Bound 0: at once, silently.
	call, n, said = retrying(t, 0, nil, prerequisite())
	_ = call()
	if err := call(); err == nil || *n != 2 || said.Len() != 0 {
		t.Errorf("err %v after %d tries, said %q; want the refusal at once", err, *n, said)
	}
}

// TestRetryOtherTransientRefusalsKeepRetrying: a rate limit is retried
// whether or not the watch is attached, and anything else ends it.
func TestRetryOtherTransientRefusalsKeepRetrying(t *testing.T) {
	call, n, said := retrying(t, 0, output.TransientError("rate limited"), output.TransientError("rate limited"), nil)
	if err := call(); err != nil || *n != 3 || said.Len() != 0 {
		t.Errorf("err %v after %d tries, said %q", err, *n, said)
	}
	call, n, _ = retrying(t, time.Minute, output.NotFoundError("no such operation"))
	if err := call(); err == nil || *n != 1 {
		t.Errorf("err %v after %d tries, want the refusal at once", err, *n)
	}
}
