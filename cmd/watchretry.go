package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/output"
)

// flagReconnect is the flag of every watch that bounds how long an
// attached watch waits for a daemon that stopped answering.
const flagReconnect = "reconnect-timeout"

// defaultReconnect is how long an attached watch waits, by default, for
// a daemon that stopped answering: long enough to ride through a
// `service restart` or a service manager's restart, short enough that a
// daemon which is gone for good is noticed.
const defaultReconnect = 30 * time.Second

// addReconnectFlag adds --reconnect-timeout to a watch command.
func addReconnectFlag(c *cobra.Command) {
	c.Flags().Duration(flagReconnect, defaultReconnect,
		"How long an attached watch waits for a daemon that stopped answering before it fails (exit 70); 0 fails at once")
}

// reconnectOf reads --reconnect-timeout.
func reconnectOf(cmd *cobra.Command) (time.Duration, error) {
	d, _ := cmd.Flags().GetDuration(flagReconnect)
	if d < 0 {
		return 0, output.UsageError("--" + flagReconnect + ": want a duration of zero (fail at once) or more")
	}
	return d, nil
}

// retryBase is the first pause between tries; it doubles up to
// watchMaxBackoff.
var retryBase = watchIdle / 2

// clock is the time a retry helper reads and waits on, apart so a test
// can run an outage in no time.
type clock struct {
	now   func() time.Time
	after func(time.Duration) <-chan time.Time
}

var wallClock = clock{now: time.Now, after: time.After}

// retryTransient runs each step of a watch through step, riding out
// what is only a moment's refusal.
//
//   - A refusal that is transient and not PREREQUISITE — the daemon's
//     rate limit, a busy store — backs off and tries again, up to the
//     step's context.
//   - The daemon not answering is PREREQUISITE (no daemon, or no runtime
//     in it) or a call it had accepted and dropped (its connection
//     closed or reset, or cancelled as it stopped: control.Gone). A
//     watch that has not attached yet fails with the refusal at once, as
//     every other daemon command does. One that has attached waits for
//     the daemon to return, at most reconnect of continuous
//     unavailability counted from the first failure of the outage and
//     started over by the next call that succeeds: it says so once on
//     log when the wait begins, and fails with PREREQUISITE (exit 70)
//     when the bound runs out. reconnect 0 fails at once.
//
// Any other failure ends the watch.
func retryTransient[S, O any](step func(context.Context, S) (O, error), reconnect time.Duration, log io.Writer) func(context.Context, S) (O, error) {
	return retryOn(wallClock, step, reconnect, log)
}

func retryOn[S, O any](clk clock, step func(context.Context, S) (O, error), reconnect time.Duration, log io.Writer) func(context.Context, S) (O, error) {
	attached := false
	return func(ctx context.Context, st S) (O, error) {
		backoff := retryBase
		var down time.Time // when this outage began: its first failure
		for {
			out, err := step(ctx, st)
			if err == nil {
				attached = true
				return out, nil
			}
			var oe *output.Error
			if ctx.Err() != nil || !errors.As(err, &oe) {
				return out, err
			}
			gone := control.Gone(err)
			unavailable := gone || (oe.Transience == output.TransienceTransient && oe.Code == output.CodePrerequisite)
			if !unavailable && oe.Transience != output.TransienceTransient {
				return out, err
			}
			wait := backoff
			if unavailable {
				if !attached {
					return out, err
				}
				if down.IsZero() {
					down = clk.now()
					if reconnect > 0 {
						_, _ = fmt.Fprintf(log, "daemon unavailable, waiting up to %s\n", reconnect)
					}
				}
				left := reconnect - clk.now().Sub(down)
				if left <= 0 {
					if gone {
						return out, control.Unavailable(err)
					}
					return out, err
				}
				wait = min(wait, left)
			}
			select {
			case <-ctx.Done():
				return out, err
			case <-clk.after(wait):
			}
			backoff = min(2*backoff, watchMaxBackoff)
		}
	}
}
