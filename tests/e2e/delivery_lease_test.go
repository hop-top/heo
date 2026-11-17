package e2e

import (
	"errors"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// stopWorker stops a test-owned worker and waits for the leases it left
// behind to end. Cancelling a worker that has claimed an operation
// again, as uma does an outcome_unknown one when its next attempt falls
// due, abandons the claim; the lease stays until its term ends, and a
// reconcile before then is refused (CONFLICT, "operation is being
// processed"), as it should be: nothing in the fixture was settled yet.
func (c *cliSite) stopWorker(stop func()) {
	c.t.Helper()
	stop()
	pool := c.db.pool()
	eventually(c.t, "the stopped worker's leases ended", func() bool {
		var n int
		// A row outlives its lease, so only an unexpired owner counts.
		const q = `SELECT count(*) FROM uma_leases WHERE resource_kind = 'operation' AND owner IS NOT NULL AND until > now()`
		if err := pool.QueryRow(c.t.Context(), q).Scan(&n); err != nil {
			c.t.Fatalf("count leases: %v", err)
		}
		return n == 0
	})
}

// TestDeliveryReconcileWhileWorkerHoldsLease: `delivery reconcile` of an
// outcome_unknown delivery that a worker is working on is refused with
// CONFLICT (exit 4), not run beside the worker, and says to reconcile
// again in a moment. The worker is held mid-send by the fake adapter, so
// the lease is held exactly as long as the test says; once it is released
// the delivery settles and the same command answers.
func TestDeliveryReconcileWhileWorkerHoldsLease(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		c.s.env["HEO_E2E_FAKE_RECONCILE"] = "found"
		d := c.start("workers")
		c.fakeChannel("fake", adapterstest.Config{Account: "acct-dl-lease"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "outbox", "--destination", "out-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)

		// The test's own worker, on a fake whose lookups are held: when the
		// unknown outcome falls due the worker claims it and stops in the
		// adapter, so the lease is held exactly as long as the test says.
		fake := adapterstest.New()
		fake.EnableReconcile()
		fake.ReconcileFinds()
		fake.HoldReconciles()
		defer fake.ReleaseReconciles()
		e := c.db.engine([]adapter.Factory{fake}, func(cfg *uma.Config) {
			cfg.Work = &uma.WorkPolicy{MaxClaims: 10, MaxInFlight: 8}
			cfg.Retry = uma.RetryPolicy{MaxAttempts: 5, BaseDelay: 300 * time.Millisecond, MaxDelay: 300 * time.Millisecond}
		})
		fake.FailSends(errors.New("connection reset after write"))
		var s sendJSON
		c.json(&s, "message", "send", "--inbox", "outbox", "--to", "user:ada", "--text", "hi ada", "--idempotency-key", "d-lease")
		op := s.Operation.ID
		stop := worker(t, e, "dl-lease", 30*time.Second)
		defer stop()
		select {
		case <-fake.Reconciling():
		case <-time.After(20 * time.Second):
			t.Fatal("the worker never reconciled the unknown send")
		}

		if got := c.delivery(op); got.State != model.Unknown {
			t.Fatalf("delivery while the worker holds it = %+v", got)
		}
		r := c.heo(4, "delivery", "reconcile", string(op), "--format", "json")
		wantStderr(t, r, "CONFLICT", "operation is being processed", "reconcile again in a moment")
		if got := c.delivery(op); got.State != model.Unknown {
			t.Fatalf("a refused reconcile changed the delivery: %+v", got)
		}

		// The worker finishes; the delivery settles and the lease is gone.
		fake.ReleaseReconciles()
		c.waitDelivery(op, model.ProviderAccepted, model.Delivered)
		r = c.heo(0, "delivery", "reconcile", string(op), "--format", "json")
		var rec reconcileJSON
		mustJSON(t, r.stdout, &rec)
		if rec.Reconciliation != "not_needed" || strings.Contains(r.stderr, "CONFLICT") {
			t.Fatalf("reconcile after the worker finished = %+v\n%s", rec, r.stderr)
		}
		stop()
		c.stop(d)
	})
}
