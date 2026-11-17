package daemon_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/runtime/bus"
)

// lifecycle records the service started and stopped events, in the
// order the supervisor reports them.
type lifecycle struct {
	mu      sync.Mutex
	started []string
	stopped []string

	// hold, when set, blocks the supervisor's aggregate readiness
	// event, the last thing it does before it waits on its services.
	hold chan struct{}
}

func (l *lifecycle) Publish(_ context.Context, e bus.Event) error {
	if strings.HasSuffix(string(e.Topic), ".supervisor.ready_reported") && l.hold != nil {
		<-l.hold
	}
	p, ok := e.Payload.(serve.EventPayload)
	if !ok || p.Service == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case strings.HasSuffix(string(e.Topic), ".service.started"):
		l.started = append(l.started, p.Service)
	case strings.HasSuffix(string(e.Topic), ".service.stopped"):
		l.stopped = append(l.stopped, p.Service)
	}
	return nil
}

func (l *lifecycle) snapshot() (started, stopped []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.started), slices.Clone(l.stopped)
}

// TestStoppedEventsFollowTheOrderedStop: a signal cancels every
// service's context at once, and each service's Start returns on it.
// The stopped events must still come in the reverse of the start order:
// kit reports a service stopped when its Stop returns, not when its
// Start does. The test guards that behaviour of kit's supervisor
// through heo's services; kit at one time took whichever came first
// of the canceled context and the returned Start, at random.
func TestStoppedEventsFollowTheOrderedStop(t *testing.T) {
	names := []string{daemon.NameBridge, daemon.NameApprovalChat, daemon.NameWorkers,
		daemon.NameReceivers, daemon.NameWebhook, daemon.NameHooks}
	r := newRig(t, "acme")
	for i := range 10 {
		lc := lifecycle{hold: make(chan struct{})}
		s := r.runWith(5*time.Second, []serve.Option{serve.WithPublisher(&lc)}, names...)
		s.ready(names...)
		// The signal lands while the supervisor is not yet waiting, and
		// each Start has the time to return if it is going to. When
		// the supervisor does wait it finds the cancellation and every
		// return ready at once, and takes them in no set order.
		s.cancel()
		time.Sleep(50 * time.Millisecond)
		close(lc.hold)
		if res := s.result(); res.Outcome != serve.OutcomeCleanStop {
			t.Fatalf("run %d: outcome %s: %v", i, res.Outcome, res.Err)
		}
		started, stopped := lc.snapshot()
		want := slices.Clone(started)
		slices.Reverse(want)
		if len(started) != len(names) || !slices.Equal(stopped, want) {
			t.Fatalf("run %d: started %v, stopped %v, want the reverse of the start order", i, started, stopped)
		}
	}
}
