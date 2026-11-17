package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// shutdownSignals are the signals that stop the daemon. Kit's
// supervisor drains on SIGINT and SIGTERM; heo has no reload, so a
// SIGHUP, which a closed terminal sends, is a stop request too rather
// than the default disposition that ends the process at once.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// watchShutdown makes the shutdown signals heo's to interpret for one
// run of the daemon: the first begins a graceful stop — SIGHUP by
// canceling the returned context, which kit's supervisor runs under as
// it does on its own first SIGINT or SIGTERM — and a second of any of
// them calls abort, which gives up on the workers' drain. Kit's
// supervisor sees the second signal only between one service's stop
// and the next; a stop already waiting on claimed work never learns of
// it. The returned release restores the default handling.
func watchShutdown(parent context.Context, abort func()) (ctx context.Context, release func()) {
	ctx, cancel := context.WithCancel(parent)
	sigs := make(chan os.Signal, len(shutdownSignals))
	signal.Notify(sigs, shutdownSignals...)
	done := make(chan struct{})
	go func() {
		for n := 0; ; n++ {
			select {
			case <-sigs:
			case <-done:
				return
			}
			if n == 0 {
				cancel()
				continue
			}
			abort()
		}
	}()
	return ctx, func() {
		signal.Stop(sigs)
		close(done)
		cancel()
	}
}
