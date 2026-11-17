package daemon_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/uma/model"
)

// withStagger marks the fake adapter Staggered and spreads first starts
// over spread.
func withStagger(spread time.Duration) rigOption {
	return func(r *rig) {
		r.fake.Stagger()
		r.registry = adapters.NewRegistry()
		if err := r.registry.Register(r.fake.Adapter()); err != nil {
			r.t.Fatal(err)
		}
		r.v.Set(daemon.KeyReceiversStartSpread, spread.String())
	}
}

// TestStaggeredStartSpreadsReceiversWithoutHoldingReadiness: with a
// start spread of an hour, uma holds each channel's first connection
// for its own offset in the window (at most one channel's offset is
// zero); the held channels read starting with the spread as the reason,
// and the service is ready anyway.
func TestStaggeredStartSpreadsReceiversWithoutHoldingReadiness(t *testing.T) {
	const n = 4
	r := newRig(t, "acme", withStagger(time.Hour))
	var refs []model.ChannelRef
	for i := range n {
		refs = append(refs, r.channel(fmt.Sprintf("ch%d", i), adapterstest.Config{Account: fmt.Sprintf("acct-%d", i)}))
	}
	start := time.Now()
	r.running() // fails unless receivers and webhook report ready
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("ready after %s: readiness waited for spread starts", d)
	}
	time.Sleep(200 * time.Millisecond)
	started := 0
	for _, a := range refs {
		if r.fake.Runs(a.ID) > 0 {
			started++
			continue
		}
		waitFor(t, func() bool {
			sc, ok := r.scope(daemon.NameReceivers, a)
			return ok && sc.State == daemon.ScopeStarting && strings.Contains(sc.Reason, "spread")
		})
	}
	if started > 1 {
		t.Errorf("%d of %d channels connected at once, want at most 1 under an hour's spread", started, n)
	}
}

// TestStartSpreadEndsAndChannelsServe: after a short spread every
// channel connects and reads ready: the starting reason does not outlast
// the window.
func TestStartSpreadEndsAndChannelsServe(t *testing.T) {
	const n = 3
	r := newRig(t, "acme", withStagger(2*time.Second))
	var refs []model.ChannelRef
	for i := range n {
		refs = append(refs, r.channel(fmt.Sprintf("ch%d", i), adapterstest.Config{Account: fmt.Sprintf("acct-%d", i)}))
	}
	r.running()
	for _, a := range refs {
		waitFor(t, func() bool {
			sc, ok := r.scope(daemon.NameReceivers, a)
			return r.fake.Receiving(a.ID) == 1 && ok && sc.State == daemon.ScopeReady
		})
	}
}

// TestChannelAddedLaterStartsAtOnce: the spread applies to the daemon's
// first start only; a channel a reconcile adds connects without it.
func TestChannelAddedLaterStartsAtOnce(t *testing.T) {
	r := newRig(t, "acme", withStagger(time.Hour))
	r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	r.running()
	b := r.channel("beta", adapterstest.Config{Account: "acct-b"})
	r.reconcile()
	waitFor(t, func() bool { return r.fake.Receiving(b.ID) == 1 })
}
