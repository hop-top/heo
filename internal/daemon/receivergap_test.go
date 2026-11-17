package daemon_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// pushCreds resolves the "push" purpose once stored is set, and refuses
// it as a missing credential until then.
type pushCreds struct{ stored atomic.Bool }

func (c *pushCreds) Resolve(_ context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	if req.Purpose == "push" && !c.stored.Load() {
		return nil, &model.Error{Code: model.Unauthorized, SafeMessage: "no credential stored", Retry: model.NoRetry}
	}
	return ports.Secret{Bytes: []byte("s")}, nil
}

// TestReceiverNeedingACredentialItLacksIsNotReady: an adapter that goes on
// without the feature a missing credential serves reports nothing, yet
// its channel is not ready: the scope reads degraded with the credential
// and what stays off, the receivers service is degraded, and the channel
// is ready once the credential is stored. A channel whose configuration
// does not need it is ready from the start.
func TestReceiverNeedingACredentialItLacksIsNotReady(t *testing.T) {
	creds := &pushCreds{}
	r := newRig(t, "acme", withCredentials(creds), withReceiverTiming(10*time.Millisecond, 5, 20*time.Millisecond))
	scriptReceiver(t, r.fake)
	a := r.fake.Adapter()
	a.CredentialPurposes = []string{"push"}
	a.CredentialIf = map[string]adapters.CredentialCondition{"push": {
		Needed: func(ch model.Channel) bool { return ch.Key == "pushing" },
		Effect: "no push subscription; polling only",
	}}
	r.registry = adapters.NewRegistry()
	if err := r.registry.Register(a); err != nil {
		t.Fatal(err)
	}
	cfg := adapterstest.Config{Account: "acct-1"}
	pushing := r.channelOf(model.Channel{Key: "pushing", Type: adapterstest.Type, CredentialRef: "cred", Configuration: cfg.Configuration()})
	cfg.Account = "acct-2"
	polling := r.channelOf(model.Channel{Key: "polling", Type: adapterstest.Type, CredentialRef: "cred", Configuration: cfg.Configuration()})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)

	waitFor(t, scopeIs(r, polling, daemon.ScopeReady, ""))
	waitFor(t, scopeIs(r, pushing, daemon.ScopeDegraded, "credential push: no credential stored; no push subscription; polling only"))
	waitFor(t, func() bool { return service(r.host.Status(), daemon.NameReceivers).State == daemon.StateDegraded })

	creds.stored.Store(true)
	waitFor(t, scopeIs(r, pushing, daemon.ScopeReady, ""))
	waitFor(t, func() bool { return service(r.host.Status(), daemon.NameReceivers).State == daemon.StateReady })
	s.stop()
}
