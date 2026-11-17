package daemon_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// withServeDelay makes the fake a listener-serving adapter whose
// receivers serve d into their run.
func withServeDelay(d time.Duration) rigOption {
	return func(r *rig) {
		r.fake.ServeAfter(d)
		r.registry = adapters.NewRegistry()
		if err := r.registry.Register(r.fake.Adapter()); err != nil {
			r.t.Fatal(err)
		}
	}
}

// TestReceiversReadyWaitsForListener: a channel whose receiver serves on
// a listener reads starting until the listener binds, and the receivers
// service is not ready before then.
func TestReceiversReadyWaitsForListener(t *testing.T) {
	r := newRig(t, "acme", withServeDelay(300*time.Millisecond))
	ch := r.channel("mx", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	svc := s.svcs[daemon.NameReceivers]

	waitFor(t, func() bool { return r.fake.Receiving(ch.ID) == 1 })
	if !r.fake.Served(ch.ID) {
		if svc.Ready() {
			t.Error("receivers ready before the listener bound")
		}
		rs := service(r.host.Status(), daemon.NameReceivers)
		if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeStarting || rs.State != daemon.StateStarting {
			t.Errorf("before the listener bound: service %s, scope %+v", rs.State, sc)
		}
	}
	s.ready(daemon.NameReceivers)
	if !r.fake.Served(ch.ID) {
		t.Error("receivers ready before the listener bound")
	}
	rs := service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeReady || rs.State != daemon.StateReady {
		t.Errorf("after the listener bound: service %s, scope %+v", rs.State, sc)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
}

// TestReceiversReadyWithListenerLeaseHeld: a listener channel whose
// receiver lease another holder keeps settles waiting; it does not hold
// the receivers service's readiness.
func TestReceiversReadyWithListenerLeaseHeld(t *testing.T) {
	r := newRig(t, "acme", withServeDelay(0))
	ch := r.channel("mx", adapterstest.Config{Account: "acct-1"})

	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan error, 1)
	go func() {
		held <- r.seed.Runtime().Receive(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: r.tenant, Actor: "other"}},
			ch, uma.ReceiveOwner("other-daemon"))
	}()
	t.Cleanup(func() { cancel(); <-held })
	waitFor(t, func() bool { return r.fake.Served(ch.ID) })

	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	rs := service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeWaiting || rs.State != daemon.StateDegraded {
		t.Errorf("lease held elsewhere: service %s, scope %+v", rs.State, sc)
	}
}

// leaseFault fails every receiver lease the daemon takes with its error
// while one is set, as a store whose concurrent units could not be
// serialized does.
type leaseFault struct {
	mu    sync.Mutex
	err   error
	tries int
}

func (f *leaseFault) set(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *leaseFault) take() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	return f.err
}

func (f *leaseFault) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tries
}

// faultStore is the daemon's view of the store, failing its lease
// takes through f.
type faultStore struct {
	store.Store
	f *leaseFault
}

func (s faultStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, tenant, func(tx store.Tx) error { return fn(faultTx{Tx: tx, f: s.f}) })
}

type faultTx struct {
	store.Tx
	f *leaseFault
}

func (t faultTx) AcquireLease(ctx context.Context, r store.LeaseRequest) (store.Lease, error) {
	if err := t.f.take(); err != nil {
		return store.Lease{}, err
	}
	return t.Tx.AcquireLease(ctx, r)
}

// withLeaseFault fails the daemon's lease takes through f.
func withLeaseFault(f *leaseFault) rigOption {
	return func(r *rig) { r.storage.view = faultStore{Store: r.storage.st, f: f} }
}

// errSerialization is the store's answer to a unit that lost a
// serialization race.
var errSerialization = model.NewError(model.Transient, "unit could not be serialized; retry the unit")

// TestReceiversReadyWaitsOutTransientStart: a listener channel whose
// receiver fails transiently taking its lease, and is retried, reads
// starting and holds the receivers service's readiness until the retry
// binds its listener.
func TestReceiversReadyWaitsOutTransientStart(t *testing.T) {
	f := &leaseFault{err: errSerialization}
	r := newRig(t, "acme", withServeDelay(0), withLeaseFault(f))
	ch := r.channel("mx", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	svc := s.svcs[daemon.NameReceivers]

	waitFor(t, func() bool {
		sc, _ := scopeOf(service(r.host.Status(), daemon.NameReceivers), ch)
		return strings.Contains(sc.Reason, "serialized")
	})
	rs := service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeStarting || rs.State != daemon.StateStarting {
		t.Errorf("retrying a transient start: service %s, scope %+v", rs.State, sc)
	}
	if svc.Ready() {
		t.Error("receivers ready while a channel retries a transient start")
	}
	f.set(nil)
	s.ready(daemon.NameReceivers)
	if !r.fake.Served(ch.ID) {
		t.Error("receivers ready before the retried listener bound")
	}
	rs = service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeReady || rs.State != daemon.StateReady {
		t.Errorf("after the retry bound: service %s, scope %+v", rs.State, sc)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
}

// TestReceiversReadyAfterTransientStartDeadline: a channel still
// retrying a transient start at services.receivers.settle_timeout
// settles waiting: the service reports ready, degraded, and the channel
// keeps retrying until it serves.
func TestReceiversReadyAfterTransientStartDeadline(t *testing.T) {
	const settle = 200 * time.Millisecond
	f := &leaseFault{err: errSerialization}
	r := newRig(t, "acme", withServeDelay(0), withLeaseFault(f),
		withSet(daemon.KeyReceiversSettleTimeout, settle.String()))
	ch := r.channel("mx", adapterstest.Config{Account: "acct-1"})
	start := time.Now()
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	// The first retry comes 1s in: readiness at the settle timeout, not
	// at the next failure.
	if took := time.Since(start); took < settle || took > settle+500*time.Millisecond {
		t.Errorf("receivers ready after %v, want at the %v settle timeout", took, settle)
	}
	rs := service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeWaiting || !strings.Contains(sc.Reason, "serialized") ||
		rs.State != daemon.StateDegraded {
		t.Errorf("past the settle timeout: service %s, scope %+v", rs.State, sc)
	}
	f.set(nil)
	waitFor(t, func() bool { return r.fake.Served(ch.ID) })
	waitFor(t, func() bool { return service(r.host.Status(), daemon.NameReceivers).State == daemon.StateReady })
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
}

// TestReceiversReadyOnPermanentStartFailure: a channel whose receiver
// fails for good settles failed at once; the settle timeout does not
// hold readiness for it.
func TestReceiversReadyOnPermanentStartFailure(t *testing.T) {
	f := &leaseFault{err: model.NewError(model.Permanent, "lease refused")}
	r := newRig(t, "acme", withServeDelay(0), withLeaseFault(f),
		withSet(daemon.KeyReceiversSettleTimeout, "20s"))
	ch := r.channel("mx", adapterstest.Config{Account: "acct-1"})
	start := time.Now()
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("receivers ready after %v; a permanent failure settles at once", took)
	}
	rs := service(r.host.Status(), daemon.NameReceivers)
	if sc, _ := scopeOf(rs, ch); sc.State != daemon.ScopeFailed || rs.State != daemon.StateDegraded {
		t.Errorf("permanent start failure: service %s, scope %+v", rs.State, sc)
	}
	if n := f.attempts(); n != 1 {
		t.Errorf("lease taken %d times after a permanent failure, want once", n)
	}
}
