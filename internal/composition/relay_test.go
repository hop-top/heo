package composition_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// relayChannel is a relay.messages channel of medium on the callback,
// receiving only when callback is empty.
func relayChannel(t *testing.T, medium, callback, credentialRef string) model.Channel {
	t.Helper()
	cfg := relay.Config{Medium: medium, Auth: relay.Auth{Scheme: "bearer"}}
	if callback != "" {
		cfg.Callback = &relay.Callback{URL: callback}
	}
	return model.Channel{
		Ref:  model.ChannelRef{Tenant: "acme", ID: "00000000-0000-7000-8000-000000000001"},
		Type: model.TypeRelayMessages, CredentialRef: credentialRef, Configuration: cfg.Extension(),
	}
}

// TestRelaySettings: the callback allowlist is a host setting, empty by
// default, so no channel can send until the operator names a gateway; a
// YAML list or a comma list names it; http callbacks are off.
func TestRelaySettings(t *testing.T) {
	cfg, probs := composition.ReadRelay(settings())
	if len(cfg.CallbackHosts) != 0 || cfg.AllowHTTP || cfg.Timeout != relay.DefaultTimeout || len(probs) > 0 {
		t.Fatalf("defaults = %+v, %+v; want no hosts, no http, uma's timeout", cfg, probs)
	}
	for name, raw := range map[string]any{
		"yaml list":   []any{"gw.example.com:8787", " Other.Example.com "},
		"string list": []string{"gw.example.com:8787", "Other.Example.com"},
		"comma list":  "gw.example.com:8787, Other.Example.com,",
	} {
		cfg, probs := composition.ReadRelay(settings(composition.KeyRelayCallbackHosts, raw))
		if want := []string{"gw.example.com:8787", "other.example.com"}; !slices.Equal(cfg.CallbackHosts, want) || len(probs) > 0 {
			t.Errorf("%s = %+v, %+v; want %v", name, cfg, probs, want)
		}
	}
	for _, host := range []any{
		"https://gw.example.com", "gw.example.com/uma", "user@gw.example.com", "*.example.com", "gw.example.com:0",
		"gw.example.com:99999", "gw.example.com:http", "gw example.com", "gw.example.com?x=1", ":8787", 7,
	} {
		_, probs := composition.ReadRelay(settings(composition.KeyRelayCallbackHosts, []any{host}))
		if len(probs) != 1 || probs[0].Key != composition.KeyRelayCallbackHosts || probs[0].Warning {
			t.Errorf("host %v: %+v; want one blocking problem on %s", host, probs, composition.KeyRelayCallbackHosts)
		}
	}
	for _, d := range []any{"0s", "-1m", "forever", "11m"} {
		_, probs := composition.ReadRelay(settings(composition.KeyRelayTimeout, d))
		if len(probs) != 1 || probs[0].Key != composition.KeyRelayTimeout || probs[0].Warning {
			t.Errorf("timeout %v: %+v", d, probs)
		}
	}
	if cfg, probs := composition.ReadRelay(settings(composition.KeyRelayTimeout, "")); cfg.Timeout != relay.DefaultTimeout || len(probs) > 0 {
		t.Errorf("an empty timeout = %+v, %+v; want uma's default", cfg, probs)
	}
	if cfg, probs := composition.ReadRelay(settings(composition.KeyRelayTimeout, "1m")); cfg.Timeout != time.Minute || len(probs) > 0 {
		t.Errorf("timeout 1m = %+v, %+v", cfg, probs)
	}
}

// TestRelayAllowHTTPWarnsOffLoopback: clear-text callbacks are the
// operator's call, and said so when a host is not on this machine; the
// listed hosts alone are never a problem.
func TestRelayAllowHTTPWarnsOffLoopback(t *testing.T) {
	for _, tc := range []struct {
		hosts []any
		http  bool
		warn  bool
	}{
		{hosts: []any{"gw.example.com"}},
		{hosts: []any{"127.0.0.1:8787", "localhost:8787", "[::1]:8787"}, http: true},
		{hosts: []any{"127.0.0.1:8787", "gw.example.com:8787"}, http: true, warn: true},
		{http: true},
	} {
		probs := composition.Check(context.Background(), settings(
			composition.KeyRelayCallbackHosts, tc.hosts, composition.KeyRelayAllowHTTP, tc.http), nil)
		switch {
		case !tc.warn && len(probs) > 0:
			t.Errorf("%v http=%v: %+v", tc.hosts, tc.http, probs)
		case tc.warn && (len(probs) != 1 || probs[0].Key != composition.KeyRelayAllowHTTP || !probs[0].Warning ||
			!strings.Contains(probs[0].Message, "clear text")):
			t.Errorf("%v http=%v: %+v; want a warning on %s", tc.hosts, tc.http, probs, composition.KeyRelayAllowHTTP)
		}
	}
}

// TestRelayCallbackHostsAreTheHosts: a channel's callback is accepted
// only on a host the host setting lists, whatever the channel says; the
// configuration has no field that widens it, so a channel never picks
// where its callback credential is sent. A receive-only channel needs no
// host.
func TestRelayCallbackHostsAreTheHosts(t *testing.T) {
	const gw = "https://gw.example.com:8787/uma/reply"
	open := registry(t, settings(composition.KeyRelayCallbackHosts, []string{"gw.example.com:8787"}))
	none := registry(t, settings())
	for _, tc := range []struct {
		name     string
		callback string
		wantOK   bool
	}{
		{"listed host", gw, true},
		{"another host", "https://evil.example.com/uma/reply", false},
		{"another port", "https://gw.example.com:9999/uma/reply", false},
		{"http on a listed host", "http://gw.example.com:8787/uma/reply", false},
		{"host with credentials", "https://user:pw@gw.example.com:8787/uma/reply", false},
		{"host with a query", "https://gw.example.com:8787/uma/reply?to=x", false},
	} {
		a, _ := open.Lookup(model.TypeRelayMessages)
		issues := a.Factory.Validate(relayChannel(t, relay.MediumIMessage, tc.callback, "gw").Configuration)
		if ok := len(issues) == 0; ok != tc.wantOK {
			t.Errorf("%s: issues %+v, accepted %v, want %v", tc.name, issues, ok, tc.wantOK)
		}
	}
	a, _ := none.Lookup(model.TypeRelayMessages)
	if issues := a.Factory.Validate(relayChannel(t, relay.MediumIMessage, gw, "gw").Configuration); len(issues) == 0 {
		t.Error("no host listed: a callback was accepted")
	}
	if issues := a.Factory.Validate(relayChannel(t, relay.MediumIMessage, "", "gw").Configuration); len(issues) > 0 {
		t.Errorf("receive-only channel refused: %+v", issues)
	}
	// http is accepted only when the host allows it, and only on a listed host.
	plain := registry(t, settings(composition.KeyRelayCallbackHosts, []string{"127.0.0.1:8787"}, composition.KeyRelayAllowHTTP, true))
	a, _ = plain.Lookup(model.TypeRelayMessages)
	if issues := a.Factory.Validate(relayChannel(t, relay.MediumIMessage, "http://127.0.0.1:8787/uma/reply", "gw").Configuration); len(issues) > 0 {
		t.Errorf("allowed http callback refused: %+v", issues)
	}
	if issues := a.Factory.Validate(relayChannel(t, relay.MediumIMessage, "http://gw.example.com:8787/uma/reply", "gw").Configuration); len(issues) == 0 {
		t.Error("http callback on an unlisted host accepted")
	}
}

// TestRelayAccount: a relay.messages channel's account is its gateway
// and medium, so two channels of one medium reached through one callback
// would be handed the same traffic; another medium or another gateway is
// another account, case and the options that do not name the gateway are
// not, and a receive-only channel is keyed on its credential reference.
func TestRelayAccount(t *testing.T) {
	r := registry(t, settings())
	a, _ := r.Lookup(model.TypeRelayMessages)
	account := func(ch model.Channel) (string, error) { return a.AccountOf(ch) }
	const gw = "https://GW.example.com:8787/uma/reply"
	base, err := account(relayChannel(t, relay.MediumIMessage, gw, "one"))
	if err != nil {
		t.Fatal(err)
	}
	if want := string(model.TypeRelayMessages) + "\x00imessage/gw.example.com:8787/uma/reply"; base != want {
		t.Errorf("account = %q, want %q", base, want)
	}
	for name, ch := range map[string]model.Channel{
		"another medium":  relayChannel(t, relay.MediumSMS, gw, "one"),
		"another gateway": relayChannel(t, relay.MediumIMessage, "https://gw2.example.com:8787/uma/reply", "one"),
		"another path":    relayChannel(t, relay.MediumIMessage, "https://gw.example.com:8787/other", "one"),
		"receive only":    relayChannel(t, relay.MediumIMessage, "", "one"),
	} {
		if got, err := account(ch); err != nil || got == base {
			t.Errorf("%s = %q, %v; want an account other than %q", name, got, err, base)
		}
	}
	same := relayChannel(t, relay.MediumIMessage, "https://gw.example.com:8787/uma/reply/", "two")
	cfg := relay.Config{Medium: relay.MediumIMessage, Auth: relay.Auth{Scheme: "hmac", Header: "X-Sig"},
		Callback: &relay.Callback{URL: "https://gw.example.com:8787/uma/reply", Auth: "header", Header: "X-Key", LookupURL: "https://gw.example.com:8787/uma/lookup"}}
	same.Configuration = cfg.Extension()
	if got, err := account(same); err != nil || got != base {
		t.Errorf("same gateway with other options = %q, %v; want %q", got, err, base)
	}
	ro, _ := account(relayChannel(t, relay.MediumIMessage, "", "one"))
	if want := string(model.TypeRelayMessages) + "\x00imessage/credential:one"; ro != want {
		t.Errorf("receive-only account = %q, want %q", ro, want)
	}
	if got, err := account(relayChannel(t, relay.MediumIMessage, "", "")); err == nil {
		t.Errorf("channel naming no gateway and no credential got account %q", got)
	}
	if got, err := account(model.Channel{Type: model.TypeRelayMessages, CredentialRef: "one", Configuration: ext(t, "other.config", map[string]any{})}); err == nil {
		t.Errorf("foreign configuration got account %q", got)
	}
}

// TestRelayMalformedSettingsStopComposition: an allowlist that does not
// read is never half used; composing the adapters refuses what the check
// refuses, naming the key.
func TestRelayMalformedSettingsStopComposition(t *testing.T) {
	for key, bad := range map[string]any{
		composition.KeyRelayCallbackHosts: []any{"https://gw.example.com"},
		composition.KeyRelayTimeout:       "soon",
	} {
		v := settings(key, bad)
		_, err := composition.Adapters(composition.Options{Viper: func() *viper.Viper { return v }})
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s = %v: %v; want an error naming the key", key, bad, err)
		}
	}
}

// TestRelayTimeoutReachesTheAdapter: adapters.relay.timeout bounds the
// adapter's requests; the adapter declares it through the settle window it
// asks a lost answer to wait, twice the timeout once that is above two
// minutes.
func TestRelayTimeoutReachesTheAdapter(t *testing.T) {
	settle := func(v *viper.Viper) time.Duration {
		a, _ := registry(t, v).Lookup(model.TypeRelayMessages)
		return a.Factory.Describe().ReconcileSettle
	}
	if got := settle(settings()); got != relay.ReconcileSettle {
		t.Errorf("default settle window = %s, want %s", got, relay.ReconcileSettle)
	}
	if got := settle(settings(composition.KeyRelayTimeout, "3m")); got != 6*time.Minute {
		t.Errorf("settle window with a 3m timeout = %s, want 6m0s", got)
	}
}
