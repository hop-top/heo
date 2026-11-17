package e2e

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// relayRateLimitOff is the services block that lifts the daemon's write rate
// limit, for tests that make many refused writes.
const relayRateLimitOff = "  all:\n    rate_limit:\n      enabled: false\n"

const (
	relayCallbackSecret = "callback-secret-0123456789abcdef0123456" // #nosec G101 -- test value
	relayWebhookSecret  = "webhook-secret-0123456789abcdef01234567" // #nosec G101 -- test value
	relayLine           = "+15550100002"
)

// relayHosts is the host setting that lets a channel name host: the
// allowlist the operator owns.
func relayHosts(host string, allowHTTP bool) string {
	return fmt.Sprintf("adapters:\n  relay:\n    callback_hosts: [%q]\n    allow_http: %v\n", host, allowHTTP)
}

// relayCLISite is a CLI site whose host setting lists host as the only one a
// callback may name; the daemon's write rate limit is off, as the tests make
// many refused writes.
func relayCLISite(t *testing.T, host string, allowHTTP bool) *cliSite {
	t.Helper()
	c := newCLISite(t, relayRateLimitOff)
	c.s.write("cfg/heo/profiles/default.yaml", relayHosts(host, allowHTTP))
	return c
}

func relayChannelJSON(medium, callback, lookup string, extra ...string) string {
	cfg := map[string]any{
		"medium": medium,
		"auth": map[string]any{
			"scheme": "hmac", "header": "X-Webhook-Signature",
			"timestamp": map[string]any{"header": "X-Webhook-Timestamp"}, "replay": map[string]any{"source": "signature"},
		},
	}
	if callback != "" {
		cb := map[string]any{"url": callback}
		if lookup != "" {
			cb["lookup_url"] = lookup
		}
		cfg["callback"] = cb
	}
	for i := 0; i+1 < len(extra); i += 2 {
		cfg[extra[i]] = extra[i+1]
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// TestRelayChannelConfigurationChecked: on the release build, a
// relay.messages channel needs a medium and a webhook authentication, and
// refuses what the adapter refuses (a medium it does not know, no
// authentication, a field it does not know such as a secret) and a
// callback on any host the host's setting does not list, each exit 2
// with nothing written and the URL not echoed; the host owns the list,
// so with none set no channel can name a callback, and a channel that
// only receives needs none. A second channel of one medium on one gateway
// is refused exit 4, another medium is not. The preview runs the same
// checks and writes nothing, with the flag in either position.
func TestRelayChannelConfigurationChecked(t *testing.T) {
	withE2EBinary(t, func() {
		c := relayCLISite(t, "gw.example.com:8787", false)
		d := c.start()
		create := func(key, ref, cfg string, extra ...string) []string {
			return append([]string{"channel", "create", "--type", string(model.TypeRelayMessages), "--key", key, "--credential-ref", ref, "--configuration", cfg}, extra...)
		}
		const reply, look = "https://gw.example.com:8787/uma/reply", "https://gw.example.com:8787/uma/lookup"
		good := relayChannelJSON("imessage", reply, look)
		for name, cfg := range map[string]string{ //nolint:gosec // G101: made-up configurations, one with a URL naming credentials
			"no medium":          `{"auth":{"scheme":"bearer"}}`,
			"an unknown medium":  `{"medium":"rcs","auth":{"scheme":"bearer"}}`,
			"no authentication":  `{"medium":"imessage"}`,
			"an unknown scheme":  `{"medium":"imessage","auth":{"scheme":"none"}}`,
			"a secret in it":     `{"medium":"imessage","auth":{"scheme":"bearer"},"secret":"x"}`,
			"another host":       relayChannelJSON("imessage", "https://evil.example.com/uma/reply", ""),
			"another port":       relayChannelJSON("imessage", "https://gw.example.com:9999/uma/reply", ""),
			"a lookup elsewhere": relayChannelJSON("imessage", reply, "https://evil.example.com/uma/lookup"),
			"http":               relayChannelJSON("imessage", "http://gw.example.com:8787/uma/reply", ""),
			"credentials in it":  relayChannelJSON("imessage", "https://user:pw@gw.example.com:8787/uma/reply", ""),
			"a query":            relayChannelJSON("imessage", reply+"?to=x", ""),
		} {
			r := c.heo(2, create("bad", "gw", cfg)...)
			wantStderr(t, r, "VALIDATION")
			if strings.Contains(r.stderr, "evil.example.com") || strings.Contains(r.stderr, "pw@") {
				t.Errorf("%s: the refusal echoes the URL:\n%s", name, r.stderr)
			}
			wantStderr(t, c.heo(2, create("bad", "gw", cfg, "--dry-run")...), "VALIDATION")
			if got := c.channels(); len(got) != 0 {
				t.Fatalf("%s: wrote channels %+v", name, got)
			}
		}
		// The flag before the command's own.
		wantStderr(t, c.heo(2, append([]string{"--dry-run"}, create("bad", "gw", `{}`)...)...), "VALIDATION")

		c.heo(0, create("one", "gw-1", good, "--dry-run")...)
		if got := c.channels(); len(got) != 0 {
			t.Fatalf("a preview wrote channels %+v", got)
		}
		c.heo(0, create("one", "gw-1", good)...)
		wantStderr(t, c.heo(4, create("two", "gw-2", good)...), "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "gw-2", good, "--dry-run")...), "CONFLICT", "provider account")
		c.heo(0, create("sms", "gw-1", relayChannelJSON("sms", reply, look))...)
		// A channel that only receives needs no host, and is keyed on its
		// credential reference.
		c.heo(0, create("in-a", "gw-in", relayChannelJSON("whatsapp", "", ""))...)
		wantStderr(t, c.heo(4, create("in-b", "gw-in", relayChannelJSON("whatsapp", "", ""))...), "CONFLICT", "provider account")
		if got := c.channels(); len(got) != 3 {
			t.Fatalf("channels = %d, want the three that were accepted", len(got))
		}
		if out := c.heo(0, "channel", "check", "one").stdout; strings.Contains(out, "error") {
			t.Errorf("channel check reports an error:\n%s", out)
		}
		c.stop(d)

		// No host listed: nothing may name a callback.
		c = newCLISite(t, relayRateLimitOff)
		d = c.start()
		wantStderr(t, c.heo(2, create("one", "gw-1", good)...), "VALIDATION")
		c.heo(0, create("in", "gw-in", relayChannelJSON("imessage", "", ""))...)
		c.stop(d)
	})
}

// TestRelaySettingsChecked: a malformed callback host or timeout is
// reported by `config validate` and stops the daemon from composing its
// adapters, naming the key, so an allowlist is never half read.
func TestRelaySettingsChecked(t *testing.T) {
	for key, bad := range map[string]string{
		"adapters.relay.callback_hosts": "https://gw.example.com",
		"adapters.relay.timeout":        "soon",
	} {
		s := newSandbox(t)
		r := s.run(nil, "-c", key+"="+bad, "config", "validate")
		s.want(r, 2, "config", "validate")
		if !strings.Contains(r.stdout+r.stderr, key) {
			t.Errorf("%s = %q: config validate does not name the key:\n%s%s", key, bad, r.stdout, r.stderr)
		}
	}
	s := newSandbox(t)
	r := s.run(nil, "-c", "adapters.relay.callback_hosts=gw.example.com:8787, other.example.com", "-c", "adapters.relay.allow_http=true", "config", "validate")
	s.want(r, 0, "config", "validate")
	if !strings.Contains(r.stdout+r.stderr, "clear text") {
		t.Errorf("an http callback to a host off this machine is not warned about:\n%s%s", r.stdout, r.stderr)
	}
}

// relayGatewayServer is an operator's gateway of the test's own, a real
// server on loopback: it takes the callback and the lookup under the
// callback secret, remembers each Idempotency-Key and records what it got.
type relayGatewayServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	got   []map[string]any
	keys  map[string]string
	other int
	// lose makes the next callbacks act (the key is recorded) and lose their
	// answer: the connection drops.
	lose int
	// lookupDown makes the lookup drop the connection, as a gateway that
	// cannot be reached does; lookups counts the lookups it was asked.
	lookupDown bool
	lookups    int
}

func newRelayGatewayServer(t *testing.T) *relayGatewayServer {
	t.Helper()
	g := &relayGatewayServer{keys: map[string]string{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+relayCallbackSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/uma/reply":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			body["idempotency_key"] = r.Header.Get("Idempotency-Key")
			g.got = append(g.got, body)
			id := fmt.Sprintf("gw-%d", len(g.got))
			g.keys[r.Header.Get("Idempotency-Key")] = id
			if g.lose > 0 {
				g.lose--
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			_, _ = fmt.Fprintf(w, `{"message_id":%q}`, id)
		case r.Method == http.MethodGet && r.URL.Path == "/uma/lookup":
			g.lookups++
			if g.lookupDown {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			if id, ok := g.keys[r.URL.Query().Get("operation_id")]; ok {
				_, _ = fmt.Fprintf(w, `{"status":"sent","message_id":%q}`, id)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"status":"not_found"}`)
		default:
			g.other++
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *relayGatewayServer) host() string {
	u, _ := url.Parse(g.srv.URL)
	return u.Host
}

func (g *relayGatewayServer) callbacks() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.got...)
}

// post delivers body to the daemon's route for ch as the gateway would.
func relayPost(t *testing.T, route, body, secret string, when time.Time) int {
	t.Helper()
	ts := strconv.FormatInt(when.Unix(), 10)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "." + body))
		req.Header.Set("X-Webhook-Timestamp", ts)
		req.Header.Set("X-Webhook-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback test URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestRelayIMessageThroughTheBinary: the release build's relay.messages
// channel, its daemon and workers, against a gateway of the test's own on
// loopback (the host lists it and allows http, as an operator's gateway on
// this machine). The gateway's signed webhook is refused without a
// signature and answers 503 while no webhook secret is stored; with one, a
// message opens the sender's conversation under relay.imessage; a reply
// reaches the gateway's callback under the callback secret, as a bearer
// credential, with the operation as the Idempotency-Key, and settles; a
// first message names its namespace (a number is relay.imessage/phone:...,
// the bare phone: is ambiguous among the three mediums and refused);
// `channel check --remote` says reconcile is available and diagnoses both
// credentials.
func TestRelayIMessageThroughTheBinary(t *testing.T) {
	withE2EBinary(t, func() {
		gw := newRelayGatewayServer(t)
		c := relayCLISite(t, gw.host(), true)
		d := c.start()
		var ch channelOutJSON
		c.json(&ch, "channel", "create", "--type", string(model.TypeRelayMessages), "--key", "imessage", "--credential-ref", "gw",
			"--configuration", relayChannelJSON("imessage", gw.srv.URL+"/uma/reply", gw.srv.URL+"/uma/lookup"))
		c.heo(0, "inbox", "create", "--channel", "imessage", "--key", "imessages", "--destination", relayLine,
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		route := "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
		body := `{"from":"Alice@Example.com","to":"` + relayLine + `","body":"is anyone there?","message_id":"m-1"}`

		// Refused without a signature; 503 while no secret is stored.
		if code := relayPost(t, route, body, "", time.Now()); code != http.StatusUnauthorized {
			t.Errorf("unsigned = %d, want 401", code)
		}
		if code := relayPost(t, route, body, relayWebhookSecret, time.Now()); code != http.StatusServiceUnavailable {
			t.Errorf("no webhook secret stored = %d, want 503", code)
		}
		if got := c.conversationsOf("imessages"); len(got) != 0 {
			t.Fatalf("refused deliveries opened %+v", got)
		}
		secret := func(v string) string {
			b, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
			return string(b)
		}
		c.s.write("secrets/uma/default/gw@relay.webhook", secret(relayWebhookSecret))
		c.s.write("secrets/uma/default/gw@relay.callback", secret(relayCallbackSecret))
		if code := relayPost(t, route, body, "another-secret-0123456789abcdef012345", time.Now()); code != http.StatusUnauthorized {
			t.Errorf("another secret = %d, want 401", code)
		}
		if code := relayPost(t, route, body, relayWebhookSecret, time.Now()); code != http.StatusOK {
			t.Fatalf("signed = %d, want 200", code)
		}
		convs := c.conversationsOf("imessages")
		if len(convs) != 1 || convs[0].ReplyRoute.Namespace != relay.NamespaceIMessage || convs[0].ReplyRoute.ID != "alice@example.com" {
			t.Fatalf("conversations = %+v, want one on relay.imessage as alice@example.com", convs)
		}

		// A reply through the callback.
		var reply sendJSON
		c.json(&reply, "message", "reply", string(convs[0].Ref.ID), "--text", "on it", "--idempotency-key", "reply-1")
		got := c.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected)
		if got.State != model.ProviderAccepted && got.State != model.Delivered {
			t.Fatalf("reply = %s %+v", got.State, got.Failure)
		}
		cbs := gw.callbacks()
		if len(cbs) != 1 || cbs[0]["medium"] != "imessage" || cbs[0]["to"] != "alice@example.com" || cbs[0]["body"] != "on it" ||
			cbs[0]["in_reply_to"] != "m-1" || cbs[0]["idempotency_key"] != string(reply.Operation.ID) {
			t.Fatalf("the gateway took %+v", cbs)
		}

		// A first message: the namespace is named; a bare route is ambiguous.
		const to = "relay.imessage/phone:+15550100003"
		send := []string{"message", "send", "--inbox", "imessages", "--text", "welcome"}
		out := c.heo(0, append(send, "--to", to, "--dry-run")...).stdout
		if !strings.Contains(out, "destination   phone +15550100003") {
			t.Errorf("the preview does not name the number:\n%s", out)
		}
		out = c.heo(0, append([]string{"--dry-run"}, append(send, "--to", to)...)...).stdout
		if !strings.Contains(out, "destination   phone +15550100003") {
			t.Errorf("the preview, flag first, does not name the number:\n%s", out)
		}
		// An Apple ID is written the same way, in the phone kind.
		out = c.heo(0, append(send, "--to", "relay.imessage/phone:bob@example.com", "--dry-run")...).stdout
		if !strings.Contains(out, "destination   phone bob@example.com") {
			t.Errorf("the preview does not name the Apple ID:\n%s", out)
		}
		for _, bad := range []string{"phone:+15550100003", "room:x"} {
			wantStderr(t, c.heo(2, append(send, "--to", bad, "--dry-run")...), "VALIDATION")
			wantStderr(t, c.heo(2, append([]string{"--dry-run"}, append(send, "--to", bad)...)...), "VALIDATION")
		}
		wantStderr(t, c.heo(2, append(send, "--to", "phone:+15550100003")...), "VALIDATION", "relay.imessage", "relay.sms", "relay.whatsapp")
		if len(gw.callbacks()) != 1 {
			t.Fatalf("a preview or a refusal reached the gateway: %+v", gw.callbacks())
		}
		// Another medium's namespace passes the preview, which only knows the
		// adapter's three, and the adapter refuses it when the send runs.
		var wrong sendJSON
		c.json(&wrong, append(send, "--to", "relay.sms/phone:+15550100003")...)
		got = c.waitDelivery(wrong.Operation.ID, model.Failed, model.Rejected, model.ProviderAccepted, model.Delivered)
		if got.State == model.ProviderAccepted || got.State == model.Delivered || got.Failure == nil || got.Failure.Code != model.Validation {
			t.Fatalf("a send in another medium's namespace = %s %+v, want a validation failure", got.State, got.Failure)
		}
		if len(gw.callbacks()) != 1 {
			t.Fatalf("a refused route reached the gateway: %+v", gw.callbacks())
		}
		var first sendJSON
		c.json(&first, append(send, "--to", to)...)
		got = c.waitDelivery(first.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected)
		if got.State != model.ProviderAccepted && got.State != model.Delivered {
			t.Fatalf("first message = %s %+v", got.State, got.Failure)
		}
		if cbs = gw.callbacks(); len(cbs) != 2 || cbs[1]["to"] != "+15550100003" || cbs[1]["in_reply_to"] != nil {
			t.Fatalf("the gateway took %+v", cbs)
		}

		// What the provider allows now: reconcile is on (the callback names a
		// lookup), and both credentials are stored.
		var chk struct {
			Checks []struct {
				Findings []struct{ Severity, Scope, Message string }
				Remote   struct {
					Capabilities []struct {
						Name      string
						Available bool
						Reason    string
					}
					Credentials []struct{ Purpose, Status string }
				}
			}
		}
		c.json(&chk, "channel", "check", "imessage", "--remote")
		if len(chk.Checks) != 1 {
			t.Fatalf("check = %+v", chk)
		}
		avail := map[string]bool{}
		for _, cp := range chk.Checks[0].Remote.Capabilities {
			avail[cp.Name] = cp.Available
		}
		for _, name := range []string{"receive_http", "send", "reconcile", "start_conversation"} {
			if !avail[name] {
				t.Errorf("capability %s = %v in %+v", name, avail[name], chk.Checks[0].Remote.Capabilities)
			}
		}
		purposes := map[string]string{}
		for _, cr := range chk.Checks[0].Remote.Credentials {
			purposes[cr.Purpose] = cr.Status
		}
		if len(purposes) != 2 || purposes["relay.webhook"] != "ok" || purposes["relay.callback"] != "ok" {
			t.Errorf("credentials = %+v, want relay.webhook and relay.callback ok", chk.Checks[0].Remote.Credentials)
		}
		c.stop(d)
	})
}

// TestRelayReceiveOnlyChecks: a relay.messages channel with no callback
// only receives: `channel check --remote` reports send unavailable (what
// an inbox's replies need), reconcile disabled, and diagnoses the webhook
// credential but never the callback's, which nothing presents.
func TestRelayReceiveOnlyChecks(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, relayRateLimitOff)
		d := c.start()
		c.heo(0, "channel", "create", "--type", string(model.TypeRelayMessages), "--key", "in", "--credential-ref", "gw",
			"--configuration", relayChannelJSON("imessage", "", ""))
		c.heo(0, "inbox", "create", "--channel", "in", "--key", "imessages", "--destination", relayLine,
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		var chk struct {
			Checks []struct {
				Findings []struct{ Severity, Scope, Message string }
				Remote   struct {
					Capabilities []struct {
						Name      string
						Available bool
						Reason    string
					}
					Credentials []struct{ Purpose, Status string }
				}
			}
		}
		r := c.heo(2, "channel", "check", "in", "--remote", "--format", "json")
		if err := json.Unmarshal([]byte(r.stdout), &chk); err != nil || len(chk.Checks) != 1 {
			t.Fatalf("check: %v\n%s", err, r.stdout)
		}
		reasons := map[string]string{}
		for _, cp := range chk.Checks[0].Remote.Capabilities {
			if !cp.Available {
				reasons[cp.Name] = cp.Reason
			}
		}
		if reasons["send"] != "disabled" || reasons["reconcile"] != "disabled" || reasons["start_conversation"] != "disabled" || len(reasons) != 3 {
			t.Errorf("unavailable capabilities = %v, want send, reconcile and start_conversation disabled", reasons)
		}
		for _, cr := range chk.Checks[0].Remote.Credentials {
			if cr.Purpose == "relay.callback" {
				t.Errorf("the callback credential was diagnosed on a channel with no callback: %+v", chk.Checks[0].Remote.Credentials)
			}
		}
		var scopes []string
		for _, f := range chk.Checks[0].Findings {
			scopes = append(scopes, f.Scope)
		}
		if !strings.Contains(strings.Join(scopes, " "), "capability:send") || strings.Contains(strings.Join(scopes, " "), "credential:relay.callback") {
			t.Errorf("findings = %+v, want send unavailable and no callback credential", chk.Checks[0].Findings)
		}
		c.stop(d)
	})
}

// lookupsAsked is how many lookups the gateway took.
func (g *relayGatewayServer) lookupsAsked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lookups
}

// setLookupDown makes the gateway's lookup unreachable, or reachable.
func (g *relayGatewayServer) setLookupDown(down bool) {
	g.mu.Lock()
	g.lookupDown = down
	g.mu.Unlock()
}

func (g *relayGatewayServer) loseNext(n int) {
	g.mu.Lock()
	g.lose = n
	g.mu.Unlock()
}

// TestRelayLostAnswerOverTheBinary: a callback whose answer is lost is
// `outcome_unknown`. With a lookup URL the daemon asks the gateway, which
// holds the key, and the delivery settles with no second message; with
// none, the delivery stays unknown, `delivery reconcile` is refused and
// nothing is sent again.
func TestRelayLostAnswerOverTheBinary(t *testing.T) {
	withE2EBinary(t, func() {
		for _, withLookup := range []bool{true, false} {
			gw := newRelayGatewayServer(t)
			c := relayCLISite(t, gw.host(), true)
			secret := func(v string) string {
				b, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
				return string(b)
			}
			c.s.write("secrets/uma/default/gw@relay.webhook", secret(relayWebhookSecret))
			c.s.write("secrets/uma/default/gw@relay.callback", secret(relayCallbackSecret))
			d := c.start()
			lookup := ""
			if withLookup {
				lookup = gw.srv.URL + "/uma/lookup"
			}
			c.heo(0, "channel", "create", "--type", string(model.TypeRelayMessages), "--key", "imessage", "--credential-ref", "gw",
				"--configuration", relayChannelJSON("imessage", gw.srv.URL+"/uma/reply", lookup))
			c.heo(0, "inbox", "create", "--channel", "imessage", "--key", "imessages", "--destination", relayLine,
				"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
			gw.loseNext(1)
			var sent sendJSON
			c.json(&sent, "message", "send", "--inbox", "imessages", "--to", "relay.imessage/phone:+15550100003", "--text", "hello")
			eventually(t, "the gateway took the callback", func() bool { return len(gw.callbacks()) == 1 })
			if withLookup {
				got := c.waitDelivery(sent.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed)
				if got.State != model.ProviderAccepted && got.State != model.Delivered {
					t.Fatalf("with a lookup: %s %+v", got.State, got.Failure)
				}
			} else {
				got := c.waitDelivery(sent.Operation.ID, model.Unknown)
				if got.State != model.Unknown {
					t.Fatalf("without a lookup: %s %+v", got.State, got.Failure)
				}
				wantStderr(t, c.heo(2, "delivery", "reconcile", string(sent.Operation.ID)), "UNSUPPORTED", "nothing will reconcile it", "leaves it off")
			}
			time.Sleep(300 * time.Millisecond)
			if n := len(gw.callbacks()); n != 1 {
				t.Errorf("withLookup=%v: the gateway took %d callbacks, want 1", withLookup, n)
			}
			c.stop(d)
		}
	})
}

// TestRelayReconcilePreviewOverTheBinary: a reply to a relay.messages
// channel whose answer was lost stays outcome_unknown. With no lookup URL
// the channel reports reconcile disabled and `delivery reconcile` is
// refused (UNSUPPORTED, exit 2); `--dry-run` is refused the same way, in
// both formats and with the flag in either position, with the same
// message. With a lookup URL the preview plans the ask of the gateway
// (exit 0), and the submission, with the gateway down, is no refusal.
func TestRelayReconcilePreviewOverTheBinary(t *testing.T) {
	withE2EBinary(t, func() {
		for _, withLookup := range []bool{false, true} {
			gw := newRelayGatewayServer(t)
			// The lookup cannot be reached, so the daemon's own workers
			// leave the delivery unknown while the test looks at it.
			gw.setLookupDown(true)
			c := relayCLISite(t, gw.host(), true)
			secret := func(v string) string {
				b, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
				return string(b)
			}
			c.s.write("secrets/uma/default/gw@relay.webhook", secret(relayWebhookSecret))
			c.s.write("secrets/uma/default/gw@relay.callback", secret(relayCallbackSecret))
			d := c.start()
			lookup := ""
			if withLookup {
				lookup = gw.srv.URL + "/uma/lookup"
			}
			c.heo(0, "channel", "create", "--type", string(model.TypeRelayMessages), "--key", "imessage", "--credential-ref", "gw",
				"--configuration", relayChannelJSON("imessage", gw.srv.URL+"/uma/reply", lookup))
			c.heo(0, "inbox", "create", "--channel", "imessage", "--key", "imessages", "--destination", relayLine,
				"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
			gw.loseNext(1)
			var sent sendJSON
			c.json(&sent, "message", "send", "--inbox", "imessages", "--to", "relay.imessage/phone:+15550100003", "--text", "hello")
			eventually(t, "the gateway took the callback", func() bool { return len(gw.callbacks()) == 1 })
			c.waitDelivery(sent.Operation.ID, model.Unknown)
			op := sent.Operation.ID

			if !withLookup {
				for _, format := range []string{"", "json"} {
					real := c.heo(2, reconcileArgs(op, false, format, false)...)
					wantStderr(t, real, "UNSUPPORTED", "reconcile unavailable", "nothing will reconcile it", "leaves it off")
					for _, first := range []bool{false, true} {
						args := reconcileArgs(op, true, format, first)
						got := c.heo(2, args...)
						if refusal(got) != refusal(real) {
							t.Errorf("preview %v said\n%s\nthe submission said\n%s", args, refusal(got), refusal(real))
						}
						if strings.Contains(got.stdout, "operation:") || strings.Contains(got.stdout, `"kind"`) {
							t.Errorf("preview %v planned an effect it was refused:\n%s", args, got.stdout)
						}
					}
				}
				if n := gw.lookupsAsked(); n != 0 {
					t.Errorf("the gateway was asked %d lookups on a channel with no lookup URL", n)
				}
			} else {
				for _, format := range []string{"", "json"} {
					for _, first := range []bool{false, true} {
						args := reconcileArgs(op, true, format, first)
						r := c.heo(0, args...)
						if !strings.Contains(r.stdout, "operation:"+string(op)) || strings.Contains(r.stderr, "UNSUPPORTED") {
							t.Errorf("preview %v of a channel with a lookup URL:\n%s%s", args, r.stdout, r.stderr)
						}
					}
				}
				// The gateway being down is the submission's error, not a
				// refusal.
				r := c.s.run(nil, reconcileArgs(op, false, "", false)...)
				if strings.Contains(r.stderr, "UNSUPPORTED") {
					t.Errorf("a channel with a lookup URL was refused as one without:\n%s", r.stderr)
				}
			}
			time.Sleep(300 * time.Millisecond)
			if n := len(gw.callbacks()); n != 1 {
				t.Errorf("withLookup=%v: the gateway took %d callbacks, want 1", withLookup, n)
			}
			c.stop(d)
		}
	})
}
