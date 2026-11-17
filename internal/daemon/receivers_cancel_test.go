package daemon_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/runtime/bus"
)

// failures records the failed events the supervisor publishes.
type failures struct {
	mu sync.Mutex
	n  []string
}

func (f *failures) Publish(_ context.Context, e bus.Event) error {
	if strings.HasSuffix(string(e.Topic), ".failed") {
		f.mu.Lock()
		f.n = append(f.n, string(e.Topic))
		f.mu.Unlock()
	}
	return nil
}

func (f *failures) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.n...)
}

// TestReceiversCancelBeforeReadyIsACleanStop: a signal that lands while
// a receiver is still settling, before the service reported ready, ends
// the run as a clean stop. The service's Start returns on the canceled
// context without holding; kit must take that return as the
// cancellation, not as a failed start. kit once took the canceled
// context and the return at random, so the signal is raised from the
// receiver's own connection, which leaves Start's return and the
// supervisor's wake-up racing, and the run repeats.
func TestReceiversCancelBeforeReadyIsACleanStop(t *testing.T) {
	r := newRig(t, "acme", withLinkReports(), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond),
		withSet(daemon.KeyReceiversSettleTimeout, "5s"))
	// The connection never reports a link, so the service keeps
	// settling; it raises the signal the run is armed with.
	signal := make(chan context.CancelFunc, 1)
	r.fake.ScriptReceive(func(ctx context.Context, _ adapterstest.Connection) error {
		select {
		case cancel := <-signal:
			cancel()
		case <-ctx.Done():
		}
		<-ctx.Done()
		return ctx.Err()
	})
	r.channel("slow", adapterstest.Config{Account: "acct-1"})
	const runs = 300
	bad := 0
	for i := range runs {
		var f failures
		s := r.runWith(5*time.Second, []serve.Option{serve.WithPublisher(&f)}, daemon.NameReceivers)
		signal <- s.cancel
		res := s.result()
		if res.Outcome != serve.OutcomeCleanStop || len(f.all()) != 0 {
			bad++
			t.Errorf("run %d: outcome %s: %v; failed events %v", i, res.Outcome, res.Err, f.all())
			if bad > 3 {
				return
			}
		}
	}
}

// TestReceiversStartReturnsOnCancelBeforeReady: a Start canceled while
// its receivers settle returns at once, without reporting ready and
// without waiting for its own Stop. kit reads that return as the
// cancellation; a Start held open until Stop would only slow the stop.
func TestReceiversStartReturnsOnCancelBeforeReady(t *testing.T) {
	r := newRig(t, "acme", withLinkReports(), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond),
		withSet(daemon.KeyReceiversSettleTimeout, "5s"))
	connected := make(chan struct{}, 1)
	r.fake.ScriptReceive(func(ctx context.Context, _ adapterstest.Connection) error {
		select {
		case connected <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	})
	r.channel("slow", adapterstest.Config{Account: "acct-1"})
	var svc daemon.Service
	for _, s := range r.host.Services() {
		if s.Name() == daemon.NameReceivers {
			svc = s
		}
	}
	if err := svc.Validate(); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		ctx, cancel := context.WithCancel(context.Background())
		var reported atomic.Bool
		done := make(chan error, 1)
		go func() { done <- svc.Start(ctx, func() { reported.Store(true) }) }()
		select {
		case <-connected:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: no receiver connection", i)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil || reported.Load() {
				t.Errorf("run %d: Start = %v, ready reported %v, want nil and not ready", i, err, reported.Load())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("run %d: Start held open after cancel, before Stop", i)
		}
		if err := svc.Stop(context.Background()); err != nil {
			t.Fatalf("run %d: stop: %v", i, err)
		}
	}
}
