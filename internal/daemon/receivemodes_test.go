package daemon_test

import (
	"net/http"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/uma/model"
)

// These tests hold the receivers and the webhook to each channel's
// capability status, not to its adapter's declaration: the fake adapter
// declares both ways of receiving, and a channel's configuration
// switches one off (reason disabled), as telegram.messages' receive mode
// or a Slack app on Socket Mode does.

// inboundReady fails unless receivers and webhook read ready, not
// degraded, within a few seconds: a receiver reads ready once uma's
// status of its call says so, which the daemon reads within a poll.
func (r *rig) inboundReady() {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := r.host.Status()
		bad := ""
		for _, name := range []string{daemon.NameReceivers, daemon.NameWebhook} {
			if got := service(st, name).State; got != daemon.StateReady {
				bad = name + " = " + string(got)
			}
		}
		if bad == "" {
			return
		}
		if time.Now().After(deadline) {
			r.t.Errorf("%s, want ready", bad)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDisabledReceiveStartsNoReceiver: a channel whose configuration
// switches receive off is routed and never received persistently; the
// receivers service has no scope for it and is not degraded.
func TestDisabledReceiveStartsNoReceiver(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a", Disabled: []model.Capability{model.CapabilityReceive}})
	_, hooks := r.running()
	if code, _ := post(t, hooks+string(a.ID)); code != http.StatusNoContent {
		t.Errorf("route = %d, want 204", code)
	}
	if sc, ok := r.scope(daemon.NameWebhook, a); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("webhook scope = %+v, %v, want ready", sc, ok)
	}
	time.Sleep(200 * time.Millisecond)
	if n := r.fake.Runs(a.ID); n != 0 {
		t.Errorf("receiver started %d times for a channel with receive disabled", n)
	}
	if sc, ok := r.scope(daemon.NameReceivers, a); ok {
		t.Errorf("receivers scope for a channel with receive disabled: %+v", sc)
	}
	r.inboundReady()
}

// TestDisabledWebhookMountsNoRoute: a channel whose configuration
// switches receive_http off is received persistently and has no route;
// the webhook service has no scope for it and is not degraded.
func TestDisabledWebhookMountsNoRoute(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a", Disabled: []model.Capability{model.CapabilityReceiveHTTP}})
	_, hooks := r.running()
	waitFor(t, func() bool { return r.fake.Receiving(a.ID) == 1 })
	if code, _ := post(t, hooks+string(a.ID)); code != http.StatusNotFound {
		t.Errorf("route = %d, want 404: nothing is mounted", code)
	}
	if sc, ok := r.scope(daemon.NameWebhook, a); ok {
		t.Errorf("webhook scope for a channel with receive_http disabled: %+v", sc)
	}
	if sc, ok := r.scope(daemon.NameReceivers, a); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("receivers scope = %+v, %v, want ready", sc, ok)
	}
	r.inboundReady()
}

// TestDisabledBothClaimsNoAccount: a channel that receives neither way
// holds no claim on its provider account, so another channel on it
// receives.
func TestDisabledBothClaimsNoAccount(t *testing.T) {
	r := newRig(t, "acme")
	off := r.channel("off", adapterstest.Config{Account: "acct-1",
		Disabled: []model.Capability{model.CapabilityReceive, model.CapabilityReceiveHTTP}})
	on := r.channel("on", adapterstest.Config{Account: "acct-1"})
	r.running()
	waitFor(t, func() bool { return r.fake.Receiving(on.ID) == 1 })
	if r.fake.Runs(off.ID) != 0 {
		t.Error("a channel receiving neither way was received")
	}
	r.inboundReady()
}

// TestWithdrawnReceiveStillFails: receive unavailable for another reason
// than the channel's configuration, here a permission withdrawn, is a
// fault: the receiver is tried, its channel reads failed and the
// receivers service degraded, as before capability statuses were read.
func TestWithdrawnReceiveStillFails(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a", Withdrawn: []model.Capability{model.CapabilityReceive}})
	r.running()
	waitFor(t, func() bool { sc, ok := r.scope(daemon.NameReceivers, a); return ok && sc.State == daemon.ScopeFailed })
	if st := service(r.host.Status(), daemon.NameReceivers).State; st != daemon.StateDegraded {
		t.Errorf("receivers = %s, want degraded", st)
	}
}

// TestReconcileFollowsDisabledReceive: a channel updated to switch
// receive off loses its receiver and its receivers scope and keeps its
// route; switched back on, it is received again.
func TestReconcileFollowsDisabledReceive(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	_, hooks := r.running()
	waitFor(t, func() bool { return r.fake.Receiving(a.ID) == 1 })

	r.update(a, adapterstest.Config{Account: "acct-a", Disabled: []model.Capability{model.CapabilityReceive}})
	r.reconcile()
	waitFor(t, func() bool { return r.fake.Receiving(a.ID) == 0 })
	if sc, ok := r.scope(daemon.NameReceivers, a); ok {
		t.Errorf("receivers scope kept for a channel with receive disabled: %+v", sc)
	}
	if code, _ := post(t, hooks+string(a.ID)); code != http.StatusNoContent {
		t.Errorf("route = %d, want 204", code)
	}
	time.Sleep(100 * time.Millisecond)
	if n := r.fake.Runs(a.ID); n != 1 {
		t.Errorf("receiver started %d times, want only the first", n)
	}
	r.inboundReady()

	r.update(a, adapterstest.Config{Account: "acct-a"})
	r.reconcile()
	waitFor(t, func() bool { return r.fake.Runs(a.ID) == 2 && r.fake.Receiving(a.ID) == 1 })
	r.inboundReady()
}
