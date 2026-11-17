package hooks

import (
	"errors"
	"math/rand/v2"
	"time"

	"hop.top/heo/internal/plugins"
)

// Class is why one attempt to handle an event failed. It is recorded on
// the dead letter and named on the health events, never the failure's
// text, which may carry a plugin's stderr.
type Class string

// Failure classes.
const (
	// ClassTimeout: the plugin did not answer within its timeout.
	ClassTimeout Class = "timeout"
	// ClassCrash: the process could not start, was killed by a signal,
	// exited with a code other than 0, 1 and 2, or closed its output
	// without answering. Retried.
	ClassCrash Class = "crash"
	// ClassMalformed: the output was not a valid result (not JSON, off
	// the schema, a verb heo refuses) or outgrew the stdout cap. Retried:
	// a plugin may answer differently the next time.
	ClassMalformed Class = "malformed"
	// ClassRetryable: the plugin exited 1, its own word for "try
	// again". Retried.
	ClassRetryable Class = "retryable_exit"
	// ClassPermanent: the plugin exited 2, or a gate refused an
	// operation for good. Never retried.
	ClassPermanent Class = "permanent"
	// ClassOperation: an operation of a valid result failed to apply
	// for a reason retrying may change (the store was busy). Retried.
	ClassOperation Class = "operation"
	// ClassError: any other failure of an attempt. Retried.
	ClassError Class = "error"
)

// Classify returns the failure class of err.
func Classify(err error) Class {
	var f *Failure
	if errors.As(err, &f) {
		if f.Permanent {
			return ClassPermanent
		}
		if f.Class != "" {
			return f.Class
		}
	}
	var oe *OpError
	if errors.As(err, &oe) {
		return ClassOperation
	}
	return ClassError
}

// FailureIndex returns the index of the result's operation that stopped
// err's attempt, when err is an *OpError.
func FailureIndex(err error) (int, bool) {
	var oe *OpError
	if errors.As(err, &oe) {
		return oe.Index, true
	}
	return 0, false
}

// exitClass is the class of a process that exited with code: 2 is the
// plugin's permanent failure, 1 its retryable one, a signal (-1) or
// anything else a crash.
func exitClass(code int) Class {
	switch code {
	case exitPermanent:
		return ClassPermanent
	case exitRetryable:
		return ClassRetryable
	}
	return ClassCrash
}

// Backoff is the wait after the n-th failed attempt, with equal
// jitter: the exponential delay (Initial doubling, up to Max) is the
// ceiling, and the wait is drawn from its upper half, [d/2, d]. Equal
// jitter spreads plugins that fail together, so a shared outage is not
// retried in lockstep, while a floor keeps the first retry from
// following the failure at once, which full jitter ([0, d]) allows.
// jitter(n) returns a value in [0, n); nil takes math/rand.
func Backoff(b plugins.Backoff, n int, jitter func(int64) int64) time.Duration {
	d := b.Initial
	for i := 1; i < n && d < b.Max; i++ {
		d *= 2
	}
	d = min(d, b.Max)
	half := d / 2
	if half <= 0 {
		return d
	}
	if jitter == nil {
		jitter = rand.Int64N
	}
	return half + time.Duration(jitter(int64(d-half)+1))
}
