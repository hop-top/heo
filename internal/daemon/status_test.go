package daemon_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// TestStatusNamesStoreOwner: while a runtime service holds the store,
// Status names this daemon as its owner — owner name, pid, host, since
// when — and the last time ownership was verified; once the store is
// closed it names none.
func TestStatusNamesStoreOwner(t *testing.T) {
	r := newRig(t, "acme")
	if st := r.host.Status(); st.Owner != nil {
		t.Fatalf("owner before start = %+v", st.Owner)
	}
	before := time.Now()
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	o := r.host.Status().Owner
	host, _ := os.Hostname()
	if o == nil || o.Owner != "heo/acme" || o.PID != os.Getpid() || o.Host != host ||
		o.Since.Before(before.Add(-time.Second)) || o.Since.After(time.Now()) {
		t.Fatalf("owner = %+v", o)
	}
	// The rig verifies ownership every 20ms.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if o := r.host.Status().Owner; o != nil && !o.VerifiedAt.IsZero() && !o.VerifiedAt.Before(o.Since) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ownership never reported verified: %+v", r.host.Status().Owner)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if o := r.host.Status().Owner; o != nil {
		t.Errorf("owner after stop = %+v", o)
	}
}

// TestQueueCountsByState: Queue reads the operation queue's depth from
// the store the daemon owns — waiting, claimed and dead-lettered items,
// each counted apart — and refuses with ErrNotRunning while no runtime
// service holds the store.
func TestQueueCountsByState(t *testing.T) {
	r := newRig(t, "acme")
	if _, err := r.host.Queue(t.Context()); !errors.Is(err, daemon.ErrNotRunning) {
		t.Fatalf("queue before start: %v, want ErrNotRunning", err)
	}
	// Receivers alone: the store is open and no worker claims anything.
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	ctx := t.Context()
	// Six items: three wait, two are claimed, one is dead-lettered.
	var claimed []store.Work
	err := r.storage.st.Transact(ctx, r.tenant, func(tx store.Tx) error {
		for i := 1; i <= 6; i++ {
			res := model.ChannelRef{Tenant: r.tenant, ID: model.ID(uuidFor(1000 + i))}.Ref()
			if err := tx.EnqueueWork(ctx, store.Work{ID: model.ID(uuidFor(2000 + i)), Kind: uma.WorkKindOperation, Resource: res}); err != nil {
				return err
			}
		}
		// Another kind is not the operation queue.
		res := model.ChannelRef{Tenant: r.tenant, ID: model.ID(uuidFor(1100))}.Ref()
		if err := tx.EnqueueWork(ctx, store.Work{ID: model.ID(uuidFor(2100)), Kind: "other.kind", Resource: res}); err != nil {
			return err
		}
		var err error
		claimed, err = tx.ClaimWork(ctx, store.WorkClaim{
			Kinds: []string{uma.WorkKindOperation}, Limit: 3, Owner: "test", Until: time.Now().Add(time.Minute),
		})
		if err != nil {
			return err
		}
		return tx.QuarantineWork(ctx, claimed[0])
	})
	if err != nil || len(claimed) != 3 {
		t.Fatalf("seed queue: %v (%d claimed)", err, len(claimed))
	}
	q, err := r.host.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := daemon.QueueStatus{Kind: uma.WorkKindOperation, Waiting: 3, InFlight: 2, DeadLettered: 1}
	if q != want {
		t.Errorf("queue = %+v, want %+v", q, want)
	}
	s.stop()
	if _, err := r.host.Queue(context.Background()); !errors.Is(err, daemon.ErrNotRunning) {
		t.Errorf("queue after stop: %v, want ErrNotRunning", err)
	}
}
