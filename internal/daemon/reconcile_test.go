package daemon_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/model"
)

// update replaces the configuration of the channel ref at its current
// revision, as `channel update` would, and returns the channel stored.
func (r *rig) update(ref model.ChannelRef, cfg adapterstest.Config) model.Channel {
	r.t.Helper()
	ch := r.stored(ref)
	ch.Configuration = cfg.Configuration()
	cmd := r.cmd("update-" + string(ref.ID) + "-" + string(ch.Revision))
	cmd.ExpectedRevision = ch.Revision
	if _, err := r.client().Configuration().ConfigureChannel(r.t.Context(), model.ConfigureChannel{Command: cmd, Channel: ch}); err != nil {
		r.t.Fatalf("update channel %s: %v", ch.Key, err)
	}
	return r.stored(ref)
}

// stored reads the channel ref as stored.
func (r *rig) stored(ref model.ChannelRef) model.Channel {
	r.t.Helper()
	page, err := r.client().Configuration().Channels(r.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 100},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, ch := range page.Items {
		if ch.Ref == ref {
			return ch
		}
	}
	r.t.Fatalf("channel %s not stored", ref.ID)
	return model.Channel{}
}

// reconcile applies the committed configuration to the running daemon,
// as a channel or inbox command does after its write.
func (r *rig) reconcile() {
	r.t.Helper()
	if err := r.host.Reconcile(r.t.Context()); err != nil {
		r.t.Fatalf("reconcile: %v", err)
	}
}

// scope returns svc's scope for channel ref, and whether it has one.
func (r *rig) scope(svc string, ref model.ChannelRef) (daemon.ScopeStatus, bool) {
	for _, sc := range service(r.host.Status(), svc).Scopes {
		if sc.Scope == "channel:"+string(ref.ID) {
			return sc, true
		}
	}
	return daemon.ScopeStatus{}, false
}

// stream is a held-open webhook request on a channel's StreamRoute.
type stream struct{ ended chan struct{} }

func openStream(t *testing.T, url string) *stream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+adapterstest.StreamRoute, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); resp.StatusCode != http.StatusOK || err != nil || line != "open\n" {
		t.Fatalf("stream = %d %q %v", resp.StatusCode, line, err)
	}
	s := &stream{ended: make(chan struct{})}
	go func() {
		_, _ = io.Copy(io.Discard, br)
		close(s.ended)
	}()
	return s
}

func (s *stream) open() bool {
	select {
	case <-s.ended:
		return false
	case <-time.After(200 * time.Millisecond):
		return true
	}
}

func (s *stream) endsSoon() bool {
	select {
	case <-s.ended:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// post posts to url and returns the status and the revision the fake
// adapter's instance answered at.
func post(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader("{}")) //nolint:gosec,noctx // loopback test URL
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get(adapterstest.RevisionHeader)
}

// running starts receivers and webhook and returns the webhook's base
// route for channels.
func (r *rig) running() (*supervised, string) {
	r.t.Helper()
	s := r.run(5*time.Second, daemon.NameReceivers, daemon.NameWebhook)
	s.ready(daemon.NameReceivers, daemon.NameWebhook)
	return s, "http://" + s.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix
}

// TestReconcileAddsChannelLeavingOthersRunning: a channel committed
// while the daemon runs receives and is routed without a restart; the
// channel already running keeps its receiver and its open request.
func TestReconcileAddsChannelLeavingOthersRunning(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	_, hooks := r.running()
	waitFor(t, func() bool { return r.fake.Receiving(a.ID) == 1 })
	held := openStream(t, hooks+string(a.ID))

	b := r.channel("beta", adapterstest.Config{Account: "acct-b"})
	if code, _ := post(t, hooks+string(b.ID)); code != http.StatusNotFound {
		t.Fatalf("uncommitted-to-runtime channel routed: %d", code)
	}
	r.reconcile()
	waitFor(t, func() bool { return r.fake.Receiving(b.ID) == 1 })
	if code, rev := post(t, hooks+string(b.ID)); code != http.StatusNoContent || rev != string(r.stored(b).Revision) {
		t.Errorf("new channel's webhook = %d at %q", code, rev)
	}
	for _, svc := range []string{daemon.NameReceivers, daemon.NameWebhook} {
		waitFor(t, func() bool { sc, ok := r.scope(svc, b); return ok && sc.State == daemon.ScopeReady })
		if sc, ok := r.scope(svc, b); !ok || sc.State != daemon.ScopeReady || sc.Key != "beta" {
			t.Errorf("%s scope for the new channel = %+v, %v", svc, sc, ok)
		}
	}
	if n := r.fake.Runs(a.ID); n != 1 {
		t.Errorf("unaffected channel's receiver started %d times, want once", n)
	}
	if !held.open() {
		t.Error("unaffected channel's open request ended")
	}
	// Nothing changed: nothing restarts.
	r.reconcile()
	if r.fake.Runs(a.ID) != 1 || r.fake.Runs(b.ID) != 1 {
		t.Errorf("a reconcile with no change restarted receivers: %d, %d", r.fake.Runs(a.ID), r.fake.Runs(b.ID))
	}
}

// TestReconcileReplacesChangedChannel: an updated channel's receiver
// and route are replaced: the route answers at the new revision, the
// request open on the old one ends, and the other channel is untouched.
func TestReconcileReplacesChangedChannel(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	b := r.channel("beta", adapterstest.Config{Account: "acct-b"})
	_, hooks := r.running()
	waitFor(t, func() bool { return r.fake.Receiving(a.ID) == 1 && r.fake.Receiving(b.ID) == 1 })
	old := openStream(t, hooks+string(a.ID))
	other := openStream(t, hooks+string(b.ID))

	updated := r.update(a, adapterstest.Config{Account: "acct-a2"})
	r.reconcile()
	waitFor(t, func() bool { return r.fake.Runs(a.ID) == 2 && r.fake.Receiving(a.ID) == 1 })
	if code, rev := post(t, hooks+string(a.ID)); code != http.StatusNoContent || rev != string(updated.Revision) {
		t.Errorf("replaced route = %d at %q, want the new revision %s", code, rev, updated.Revision)
	}
	if !old.endsSoon() {
		t.Error("the request open on the replaced route never ended")
	}
	if n := r.fake.Runs(b.ID); n != 1 {
		t.Errorf("unaffected channel's receiver started %d times, want once", n)
	}
	if !other.open() {
		t.Error("unaffected channel's open request ended")
	}
}

// TestReconcileUnmountsWithdrawnWebhook: a channel updated so that it
// no longer offers receive_http loses its route and its webhook scope;
// its receiver keeps running.
func TestReconcileUnmountsWithdrawnWebhook(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	_, hooks := r.running()
	if code, _ := post(t, hooks+string(a.ID)); code != http.StatusNoContent {
		t.Fatalf("route = %d", code)
	}
	r.update(a, adapterstest.Config{Account: "acct-a", NoWebhook: true})
	r.reconcile()
	if code, _ := post(t, hooks+string(a.ID)); code != http.StatusNotFound {
		t.Errorf("withdrawn webhook = %d, want 404: the old route is still mounted", code)
	}
	if sc, ok := r.scope(daemon.NameWebhook, a); ok {
		t.Errorf("webhook scope kept for an unmounted channel: %+v", sc)
	}
	waitFor(t, func() bool { return r.fake.Runs(a.ID) == 2 && r.fake.Receiving(a.ID) == 1 })
}

// TestReconcileRefusesDuplicateAccountByScope: a channel committed on
// an account another channel of the profile receives fails on its own
// scope; the incumbent keeps receiving. Once the incumbent moves to
// another account, the next change gives the account to the waiting
// channel.
func TestReconcileRefusesDuplicateAccountByScope(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-1"})
	s, hooks := r.running()
	waitFor(t, func() bool { return r.fake.Receiving(a.ID) == 1 })

	dup := r.channel("dup", adapterstest.Config{Account: "acct-1"})
	r.reconcile()
	for _, svc := range []string{daemon.NameReceivers, daemon.NameWebhook} {
		sc, ok := r.scope(svc, dup)
		if !ok || sc.State != daemon.ScopeFailed || !strings.Contains(sc.Reason, `channel "alpha"`) ||
			!strings.Contains(sc.Reason, "already receives this provider account") {
			t.Errorf("%s scope for the duplicate = %+v, %v", svc, sc, ok)
		}
	}
	if code, _ := post(t, hooks+string(dup.ID)); code != http.StatusServiceUnavailable {
		t.Errorf("duplicate's route = %d, want 503", code)
	}
	time.Sleep(50 * time.Millisecond)
	if r.fake.Runs(dup.ID) != 0 {
		t.Error("the duplicate account was received twice")
	}
	if r.fake.Runs(a.ID) != 1 || r.fake.Receiving(a.ID) != 1 {
		t.Errorf("incumbent restarted or stopped: runs %d, receiving %d", r.fake.Runs(a.ID), r.fake.Receiving(a.ID))
	}
	if st := r.host.Status(); !st.Ready || service(st, daemon.NameReceivers).State != daemon.StateDegraded {
		t.Errorf("status = %+v, want ready and degraded", st)
	}
	if !s.svcs[daemon.NameReceivers].Ready() {
		t.Error("a duplicate made receivers unready")
	}

	r.update(a, adapterstest.Config{Account: "acct-2"})
	r.reconcile()
	waitFor(t, func() bool { return r.fake.Receiving(dup.ID) == 1 && r.fake.Receiving(a.ID) == 1 })
	waitFor(t, func() bool { sc, ok := r.scope(daemon.NameReceivers, dup); return ok && sc.State == daemon.ScopeReady })
	if sc, ok := r.scope(daemon.NameReceivers, dup); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("duplicate after the account freed = %+v, %v", sc, ok)
	}
}

// TestReconcileClaimsAcrossProfiles: a live channel on an account
// another profile's daemon receives fails on its scope; an incumbent
// moving off the account releases its claim, and the claim of the new
// account is taken.
func TestReconcileClaimsAcrossProfiles(t *testing.T) {
	dir := t.TempDir()
	one := newRig(t, "one", withAccounts(dir))
	two := newRig(t, "two", withAccounts(dir))
	a := one.channel("alpha", adapterstest.Config{Account: "shared"})
	own := two.channel("own", adapterstest.Config{Account: "own"})
	one.running()
	two.running()
	waitFor(t, func() bool { return one.fake.Receiving(a.ID) == 1 && two.fake.Receiving(own.ID) == 1 })

	b := two.channel("beta", adapterstest.Config{Account: "shared"})
	two.reconcile()
	sc, ok := two.scope(daemon.NameReceivers, b)
	if !ok || sc.State != daemon.ScopeFailed || !strings.Contains(sc.Reason, "already received by heo/one (pid ") {
		t.Errorf("scope for the claimed account = %+v, %v", sc, ok)
	}
	if two.fake.Runs(own.ID) != 1 {
		t.Error("the other channel restarted")
	}

	one.update(a, adapterstest.Config{Account: "moved"})
	one.reconcile()
	waitFor(t, func() bool { return one.fake.Runs(a.ID) == 2 && one.fake.Receiving(a.ID) == 1 })
	two.reconcile()
	waitFor(t, func() bool { return two.fake.Receiving(b.ID) == 1 })

	c := two.channel("gamma", adapterstest.Config{Account: "moved"})
	two.reconcile()
	if sc, ok := two.scope(daemon.NameReceivers, c); !ok || !strings.Contains(sc.Reason, "already received by heo/one") {
		t.Errorf("the moved-to account is not claimed: %+v, %v", sc, ok)
	}
}

// TestReconcileWithoutInboundServices: with no runtime, or only
// workers, a reconcile has nothing to apply.
func TestReconcileWithoutInboundServices(t *testing.T) {
	r := newRig(t, "acme")
	r.reconcile()
	s := r.run(5*time.Second, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	ch := r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	r.reconcile()
	time.Sleep(50 * time.Millisecond)
	if r.fake.Runs(ch.ID) != 0 {
		t.Error("a receiver started without the receivers service")
	}
}

// TestReconcileStoppedReceiversTouchNothing: with the receivers service
// not running, a reconcile starts no receiver and reports no receivers
// scope.
func TestReconcileStoppedReceiversTouchNothing(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-a"})
	w := r.run(5*time.Second, daemon.NameWorkers, daemon.NameWebhook)
	w.ready(daemon.NameWorkers, daemon.NameWebhook)
	r.update(a, adapterstest.Config{Account: "acct-b"})
	r.reconcile()
	time.Sleep(50 * time.Millisecond)
	if r.fake.Runs(a.ID) != 0 {
		t.Error("a receiver started without the receivers service")
	}
	if _, ok := r.scope(daemon.NameReceivers, a); ok {
		t.Error("receivers scope set while receivers do not run")
	}
}
