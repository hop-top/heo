package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
	"hop.top/uma/testing/xrrtest"
)

// Cassettes of the path through uma's reference iMessage gateway, run from
// heo. heo cannot record relay.messages itself: the gateway is the
// operator's own. What it records is the scenario of uma's recording guide
// with heo's real binary in uma's place: heo posts a first message to the
// gateway's callback, the gateway sends it through an iMessage backend
// (Blooio, or BlueBubbles Server on a Mac), the same callback repeated and
// the lookup are answered from the gateway's key log without a second send,
// and the recipient's reply comes back through the backend's webhook into
// the same heo conversation. The reference gateway runs as the operator
// runs it, a process built from the uma checkout heo is built against (read
// only: nothing in it is edited or written); the requests it makes to the
// backend pass through a proxy of this test's, which is where they are
// recorded and, live, sandboxed (internal/xrrx: one backend, one handle,
// capped sends). The gateway and heo talk over loopback; nothing between
// them is recorded.
//
// In replay (the default) the proxy answers from the committed cassettes and
// the recipient's reply is a committed webhook the test posts to the
// gateway, so nothing reaches a backend and no credential is needed. None
// are recorded yet: with no cassette each test skips, and `make
// record-imessage` makes them (docs/DEVELOPING.md). TestIMessageRehearsal
// runs the whole path, record then replay, against canned answers in a
// temporary directory, so the harness is exercised on every run.

const (
	imCassettes = "testdata/cassettes"
	imFixtures  = "testdata/imessage"

	// The channel's credentials in the test, shared by heo and the gateway.
	imWebhookSecret  = "im-relay-webhook-secret-0123456789"  // #nosec G101 -- test value
	imCallbackSecret = "im-relay-callback-secret-0123456789" // #nosec G101 -- test value

	// The values a replay uses in place of the recorded ones: reserved
	// numbers and example.com only.
	imReplayRecipient = "+15550100001"
	imReplayLine      = "+15550100002"
	imReplayAccount   = "uma-test@example.com"
	imReplayKey       = "bl_replay_key"                          // #nosec G101 -- replay value
	imReplaySecret    = "whsec_replay0123456789abcdef0123456789" // #nosec G101 -- replay value
	imReplayPassword  = "replay-password"                        // #nosec G101 -- replay value
	imReplayToken     = "replay-webhook-token-0123456789"        // #nosec G101 -- replay value
	imReplayServer    = "http://bluebubbles.example.com:1234"

	// imReplyWait bounds the wait for the person's reply when recording.
	imReplyWait = 3 * time.Minute
)

// imBackend is one backend of the scenario, with the values a run uses for
// it: the recorded ones when recording, the replay ones otherwise.
type imBackend struct {
	name string
	// to is the recipient; line the address the backend reports as the
	// recipient of inbound messages, which the inbox binds.
	to, line string
	// base is the backend's real address; token the webhook's path token
	// (BlueBubbles).
	base, token string
	// apiKey, secret, password are the credentials the gateway is given.
	apiKey, secret, password string
}

func (b imBackend) placeholders() []xrrtest.Option {
	opts := []xrrtest.Option{
		xrrtest.WithPlaceholder(b.to, "RECIPIENT_HANDLE"), xrrtest.WithPlaceholder(strings.TrimPrefix(b.to, "+"), "RECIPIENTDIGITS"),
	}
	if b.name == xrrx.IMessageBackendBlooio {
		return append(opts, xrrtest.WithPlaceholder(b.line, "SENDER_NUMBER"), xrrtest.WithPlaceholder(strings.TrimPrefix(b.line, "+"), "SENDERDIGITS"))
	}
	u, _ := url.Parse(b.base)
	return append(opts, xrrtest.WithPlaceholder(b.password, "BLUEBUBBLES_PASSWORD"), xrrtest.WithPlaceholder(u.Host, "BLUEBUBBLES_HOST"),
		xrrtest.WithPlaceholder(b.line, "BLUEBUBBLES_ACCOUNT"),
		// heo mints a new operation ID on every run; BlueBubbles takes it as the send's tempGuid.
		xrrtest.WithNormalize(func(r *xrrtest.Request) {
			var body map[string]any
			if json.Unmarshal(r.Body, &body) == nil {
				if _, ok := body["tempGuid"]; ok {
					body["tempGuid"] = "OPERATION_KEY"
					r.Body, _ = json.Marshal(body)
				}
			}
		}))
}

// replayBackend is the backend as a replay sees it.
func replayBackend(name string) imBackend {
	b := imBackend{name: name, to: imReplayRecipient, line: imReplayLine, apiKey: imReplayKey, secret: imReplaySecret}
	if name == xrrx.IMessageBackendBlueBubbles {
		b.line, b.base, b.password, b.token = imReplayAccount, imReplayServer, imReplayPassword, imReplayToken
	} else {
		b.base = xrrx.IMessageBlooioBase
	}
	return b
}

// liveBackend is the backend of a recording.
func liveBackend(s *xrrx.IMessageSandbox) imBackend {
	return imBackend{
		name: s.Backend, to: s.To, line: s.Sender, base: s.Base, token: s.WebhookToken,
		apiKey: s.APIKey, secret: s.WebhookSecret, password: s.Password,
	}
}

// imGatewayBin builds the reference gateway, hop.top/uma/cmd/imessagegateway,
// from heo's own module: go.mod, replaces included, picks the uma version, so
// the gateway is the one heo is built against and nothing is fetched beyond
// the module cache. The binary is written beside heo's own test binary.
var imGatewayBin = sync.OnceValues(func() (string, error) {
	bin := filepath.Join(filepath.Dir(heoBin), "imessagegateway")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "hop.top/uma/cmd/imessagegateway") //nolint:gosec // output path is the test's own temp dir
	// Resolve from heo's go.mod alone, whatever workspace surrounds the checkout.
	build.Env = append(os.Environ(), "GOFLAGS=-mod=readonly", "GOWORK=off")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	return bin, build.Run()
})

// imProxy is where the gateway's requests to the backend are recorded: the
// gateway is told this loopback address as its backend, and each request is
// made again to the real one through the recording client.
type imProxy struct {
	srv    *http.Server
	url    string
	client *http.Client
	origin string
	n      atomic.Int32
}

func newIMProxy(t *testing.T, client *http.Client, backend imBackend) *imProxy {
	t.Helper()
	u, err := url.Parse(backend.base)
	if err != nil {
		t.Fatal(err)
	}
	p := &imProxy{client: client, origin: u.Scheme + "://" + u.Host}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.url = "http://" + ln.Addr().String()
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	t.Cleanup(func() { _ = p.srv.Close() })
	return p
}

func (p *imProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.n.Add(1)
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	out, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin+r.URL.RequestURI(), bytes.NewReader(body)) //nolint:gosec // G704: the backend the recording was told, through the sandbox
	if err != nil {
		http.Error(w, "proxy", http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Content-Type", "Accept", "Authorization", "Idempotency-Key"} {
		if v := r.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	resp, err := p.client.Do(out) //nolint:gosec // G704: as above
	if err != nil {
		// The gateway reads a refused or dropped request as a lost answer.
		http.Error(w, "proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	for _, h := range []string{"Content-Type", "Retry-After"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// imGateway is the reference gateway process.
type imGateway struct {
	cmd  *exec.Cmd
	addr string
	log  *bytes.Buffer
}

// startIMGateway runs the gateway on addr, in front of backend through the
// proxy, posting to heo's webhook hook. Its environment is its own: nothing
// of the recording's reaches it but the backend's credentials.
func startIMGateway(t *testing.T, addr string, backend imBackend, proxy, hook string) *imGateway {
	t.Helper()
	bin, err := imGatewayBin()
	if err != nil {
		t.Fatalf("the reference gateway: %v", err)
	}
	args := []string{"-addr", addr, "-backend", backend.name, "-data", t.TempDir(), "-uma-webhook-url", hook}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"IMESSAGE_GATEWAY_CALLBACK_SECRET=" + imCallbackSecret, "IMESSAGE_GATEWAY_WEBHOOK_SECRET=" + imWebhookSecret,
	}
	if backend.name == xrrx.IMessageBackendBlooio {
		args = append(args, "-blooio-from", backend.line, "-blooio-base-url", proxy+"/v4")
		env = append(env, "BLOOIO_API_KEY="+backend.apiKey, "BLOOIO_WEBHOOK_SECRET="+backend.secret)
	} else {
		args = append(args, "-bluebubbles-url", proxy, "-bluebubbles-account", backend.line)
		env = append(env, "BLUEBUBBLES_PASSWORD="+backend.password, "IMESSAGE_GATEWAY_WEBHOOK_TOKEN="+backend.token)
	}
	g := &imGateway{addr: addr, log: &bytes.Buffer{}}
	g.cmd = exec.CommandContext(t.Context(), bin, args...) //nolint:gosec // the gateway built above, with arguments of the test's own
	g.cmd.Env = env
	g.cmd.Stdout, g.cmd.Stderr = g.log, g.log
	if err := g.cmd.Start(); err != nil {
		t.Fatalf("start the gateway: %v", err)
	}
	t.Cleanup(func() {
		_ = g.cmd.Process.Kill()
		_ = g.cmd.Wait()
		if t.Failed() {
			t.Logf("gateway log:\n%s", g.log.String())
		}
		for _, lit := range []string{imCallbackSecret, imWebhookSecret, backend.apiKey, backend.password, backend.secret} {
			if lit != "" && strings.Contains(g.log.String(), lit) {
				t.Error("the gateway log carries a credential")
			}
		}
	})
	eventually(t, "the gateway listens", func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	})
	return g
}

// imCapture is the address the backend's webhook reaches while recording: a
// proxy to the gateway that keeps the body of the person's reply once the
// gateway took it, for the fixture a replay posts.
type imCapture struct {
	reply chan []byte
}

func newIMCapture(t *testing.T, listen, gateway string) *imCapture {
	t.Helper()
	target, _ := url.Parse("http://" + gateway)
	c := &imCapture{reply: make(chan []byte, 1)}
	rp := httputil.NewSingleHostReverseProxy(target)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(raw))
		sw := &imStatusWriter{ResponseWriter: w, code: http.StatusOK}
		rp.ServeHTTP(sw, r)
		if sw.code == http.StatusOK && strings.Contains(strings.ToLower(string(raw)), strings.ToLower(xrrx.IMessageReply)) {
			select {
			case c.reply <- raw:
			default:
			}
		}
	})
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("listen on %s: %v", listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return c
}

// imStatusWriter remembers the status the gateway answered.
type imStatusWriter struct {
	http.ResponseWriter
	code int
}

func (w *imStatusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// imScrub replaces the recorded identifiers of a captured webhook with
// their replay values, in any letter case (a backend may report an Apple ID
// as it was typed), blanks the names it carries and indents it for review.
func imScrub(raw []byte, values [][2]string) []byte {
	s := string(raw)
	for _, v := range values {
		if v[0] != "" && v[0] != v[1] {
			s = regexp.MustCompile("(?i)"+regexp.QuoteMeta(v[0])).ReplaceAllLiteralString(s, v[1])
		}
	}
	var doc any
	if json.Unmarshal([]byte(s), &doc) != nil {
		return []byte(s)
	}
	imBlank(doc)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return []byte(s)
	}
	return append(out, '\n')
}

func imBlank(v any) {
	names := map[string]string{"name": "", "displayName": "", "first_name": "", "last_name": "", "organization_id": "org_test"}
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if repl, ok := names[k]; ok {
				if _, isString := x.(string); isString {
					v[k] = repl
					continue
				}
			}
			imBlank(x)
		}
	case []any:
		for _, x := range v {
			imBlank(x)
		}
	}
}

// imSign is Blooio's signature of a webhook: "t=<unix>,v1=<hex HMAC-SHA-256
// of '<t>.<body>'>".
func imSign(secret string, body []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// imRun is how the scenario runs once.
type imRun struct {
	mode xrr.Mode
	// cassettes and fixture are where the exchanges and the captured reply
	// live; live is the recording's sandbox, nil in replay.
	cassettes, fixture string
	live               *xrrx.IMessageSandbox
	// backend is the values this run uses.
	backend imBackend
	// listen is the address the backend's webhook reaches while recording.
	listen string
}

// runIMessage runs the scenario.
func runIMessage(t *testing.T, run imRun) {
	t.Helper()
	b := run.backend
	opts := append([]xrrtest.Option{xrrtest.WithMode(run.mode)}, b.placeholders()...)
	if run.live != nil {
		opts = append(opts, xrrtest.WithTransport(run.live.Transport(nil)))
	}
	client := xrrtest.NewClient(t, run.cassettes, opts...)
	client.Timeout = 2 * time.Minute
	proxy := newIMProxy(t, client, b)

	gwAddr := freeAddr(t)
	c := relayCLISite(t, gwAddr, true)
	d := c.start()
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(model.TypeRelayMessages), "--key", "imessage", "--credential-ref", "gw",
		"--configuration", relayChannelJSON(relay.MediumIMessage, "http://"+gwAddr+"/uma/reply", "http://"+gwAddr+"/uma/lookup"))
	for purpose, value := range map[string]string{relay.PurposeWebhook: imWebhookSecret, relay.PurposeCallback: imCallbackSecret} {
		raw, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": value})
		c.s.write("secrets/uma/default/gw@"+purpose, string(raw))
	}
	c.heo(0, "inbox", "create", "--channel", "imessage", "--key", "imessages", "--destination", strings.ToLower(b.line),
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	hook := "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)

	gw := startIMGateway(t, gwAddr, b, proxy.url, hook)
	webhook := "http://" + gwAddr + "/webhook"
	if b.token != "" {
		webhook += "/" + b.token
	}
	var capture *imCapture
	if run.mode == xrr.ModeRecord {
		capture = newIMCapture(t, run.listen, gwAddr)
	}
	_ = gw

	// A preview sends nothing.
	send := []string{"message", "send", "--inbox", "imessages", "--to", "relay.imessage/phone:" + b.to, "--text", xrrx.IMessageText}
	c.heo(0, append(cloneArgs(send), "--dry-run")...)
	if n := proxy.n.Load(); n != 0 {
		t.Fatalf("a preview made %d backend requests", n)
	}

	// The first message: one send through the backend.
	var first sendJSON
	c.json(&first, append(cloneArgs(send), "--idempotency-key", "im-send-1")...)
	if got := c.waitDelivery(first.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected, model.Unknown); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Fatalf("the first message = %s %+v", got.State, got.Failure)
	}
	if n := proxy.n.Load(); n != 1 {
		t.Fatalf("%d backend requests for the first message, want 1", n)
	}
	var sent model.Message
	c.json(&sent, "message", "get", string(first.Message))
	backendID := providerRef(sent)
	if backendID == "" {
		t.Fatalf("the message names no backend ID: %+v", sent.External)
	}

	// The same callback again, and the lookup: answered from the gateway's
	// key log, with no second send.
	key := string(first.Operation.ID)
	dup, _ := json.Marshal(map[string]string{"medium": "imessage", "to": b.to, "body": xrrx.IMessageText, "operation_id": key, "attempt_id": "repeat"})
	if status, answer := imGatewayCall(t, http.MethodPost, "http://"+gwAddr+"/uma/reply", key, dup); status != http.StatusOK || answer["message_id"] != backendID {
		t.Fatalf("the repeated callback answered %d %v, want 200 with %s", status, answer, backendID)
	}
	if status, answer := imGatewayCall(t, http.MethodGet, "http://"+gwAddr+"/uma/lookup?operation_id="+key, "", nil); status != http.StatusOK ||
		answer["status"] != "sent" || answer["message_id"] != backendID {
		t.Fatalf("the lookup answered %d %v, want sent %s", status, answer, backendID)
	}
	if n := proxy.n.Load(); n != 1 {
		t.Fatalf("%d backend requests after the repeat and the lookup, want 1", n)
	}

	// The recipient answers.
	var posted []byte
	switch {
	case run.mode == xrr.ModeRecord && run.live.Rehearse:
		// The backend, as the rehearsal has it, posts the reply to the address a
		// real backend's webhook reaches, where the capture takes it.
		imRehearsalReply(t, b, "http://"+run.listen+strings.TrimPrefix(webhook, "http://"+gwAddr))
	case run.mode == xrr.ModeRecord:
		t.Logf("reply %q to the message from %s now; waiting up to %s for its webhook at %s%s", xrrx.IMessageReply, b.line, imReplyWait, run.listen, strings.TrimPrefix(webhook, "http://"+gwAddr))
	default:
		raw, err := os.ReadFile(run.fixture) //nolint:gosec // G304: the test's own fixture
		if err != nil {
			t.Fatalf("cassettes without the reply webhook %s: record again (%v)", run.fixture, err)
		}
		imPostWebhook(t, webhook, raw, b)
	}
	if capture != nil {
		select {
		case posted = <-capture.reply:
		case <-time.After(imReplyWait):
			t.Fatalf("no reply from %s within %s", b.to, imReplyWait)
		}
	}
	if posted != nil {
		values := [][2]string{{b.to, imReplayRecipient}, {strings.TrimPrefix(b.to, "+"), strings.TrimPrefix(imReplayRecipient, "+")}, {b.line, replayBackend(b.name).line}}
		if err := os.MkdirAll(filepath.Dir(run.fixture), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(run.fixture, imScrub(posted, values), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The reply is a message of the recipient in the conversation the first
	// message started.
	var got model.Message
	eventually(t, "the reply in the conversation", func() bool {
		for _, m := range c.messagesOf(first.Conversation) {
			if strings.EqualFold(strings.TrimSpace(textOf(m)), xrrx.IMessageReply) && m.Author.External.Namespace == relay.NamespaceIMessage {
				got = m
				return true
			}
		}
		return false
	})
	if !strings.EqualFold(got.Author.External.ID, strings.ToLower(b.to)) {
		t.Fatalf("the reply is from %q, want the recipient", got.Author.External.ID)
	}
	if convs := c.conversationsOf("imessages"); len(convs) != 1 {
		t.Fatalf("conversations = %+v, want the one the first message started", convs)
	}
	c.stop(d)
}

// imPostWebhook posts a backend webhook to the gateway, signed as the
// backend signs it.
func imPostWebhook(t *testing.T, webhook string, body []byte, b imBackend) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.name == xrrx.IMessageBackendBlooio {
		req.Header.Set("X-Blooio-Signature", imSign(b.secret, body))
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback gateway the test started
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the gateway's webhook answered %d %s", resp.StatusCode, data)
	}
}

// imGatewayCall calls the gateway as heo does.
func imGatewayCall(t *testing.T, method, rawURL, key string, body []byte) (int, map[string]string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+imCallbackSecret)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback gateway the test started
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]string
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

// providerRef is the backend's ID of m: the provider message reference.
func providerRef(m model.Message) string {
	for _, e := range m.External {
		if e.Namespace == model.ProviderMessageNamespace && e.Kind == model.RefKindMessage {
			return e.ID
		}
	}
	return ""
}

func textOf(m model.Message) string {
	var parts []string
	for _, p := range m.Content.Parts {
		if p.Kind == model.PartText {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// imCassetteCount counts the requests the test's cassette directory holds.
func imCassetteCount(dir string) int {
	n := 0
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".req.yaml") {
			n++
		}
	}
	return n
}

// openIMessage returns the live sandbox when this test is recording, nil in
// replay, and skips a replay with nothing recorded and a recording that was
// not asked to go live or names another backend.
func openIMessage(t *testing.T, backend string) *xrrx.IMessageSandbox {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("iMessage is replayed or recorded, never passed through to a backend")
	case xrr.ModeReplay:
		if imCassetteCount(filepath.Join(imCassettes, t.Name(), "gateway")) == 0 {
			t.Skip("no " + backend + " cassettes are recorded yet: `make record-imessage` with a backend you own (docs/DEVELOPING.md)")
		}
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording iMessage is live: set %s=1 and the backend (docs/DEVELOPING.md)", xrrx.EnvLive)
	}
	if os.Getenv(xrrx.EnvIMessageBackend) != backend {
		t.Skipf("recording the %s backend: %s is %q", backend, xrrx.EnvIMessageBackend, os.Getenv(xrrx.EnvIMessageBackend))
	}
	if err := xrrx.CheckLiveIMessage(os.Getenv); err != nil {
		t.Fatal(err)
	}
	s, err := xrrx.IMessageSandboxFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recording %s: at most %d sends (%d already made), rehearsal=%v", backend, s.Max, s.Sent(), s.Rehearse)
	t.Cleanup(func() {
		for _, dir := range []string{filepath.Join(imCassettes, t.Name()), imFixtures} {
			found, err := xrrx.AuditWith(dir, s.Deny()...)
			if err != nil {
				t.Errorf("audit: %v", err)
			}
			for _, f := range found {
				t.Errorf("recording is not clean: %s", f)
			}
		}
	})
	return s
}

// imScenario is a recorded test of backend: replayed from the committed
// cassettes, or recorded live.
func imScenario(t *testing.T, backend string) {
	t.Helper()
	withE2EBinary(t, func() {
		live := openIMessage(t, backend)
		run := imRun{
			mode: xrrModeOf(t), cassettes: filepath.Join(imCassettes, t.Name(), "gateway"),
			fixture: filepath.Join(imFixtures, backend+"-reply.json"), backend: replayBackend(backend),
		}
		if live != nil {
			run.live, run.backend, run.listen = live, liveBackend(live), live.Listen
		}
		runIMessage(t, run)
		if live != nil {
			t.Logf("sends made so far, across runs: %d of at most %d", live.Sent(), live.Max)
		}
	})
}

// TestXRRIMessageBlooio runs the scenario through the reference gateway over
// Blooio: one message to the maintainers' own handle, the repeated callback
// and the lookup, and the person's reply. It skips until cassettes exist.
func TestXRRIMessageBlooio(t *testing.T) { imScenario(t, xrrx.IMessageBackendBlooio) }

// TestXRRIMessageBlueBubbles is the scenario over BlueBubbles Server on the
// maintainers' Mac: an AppleScript send, no Private API. It skips until
// cassettes exist.
func TestXRRIMessageBlueBubbles(t *testing.T) { imScenario(t, xrrx.IMessageBackendBlueBubbles) }

// imRehearsalReply is the recipient's answer, as the backend would post it,
// for a rehearsal that has no person: the documented webhook shape of each
// backend. It returns the body it posted.
func imRehearsalReply(t *testing.T, b imBackend, webhook string) {
	t.Helper()
	var body any
	if b.name == xrrx.IMessageBackendBlooio {
		body = map[string]any{"type": "message.received", "data": map[string]any{
			"message_id": "msg_rehearsal_in", "direction": "inbound", "sender": b.to, "recipient": b.line, "text": xrrx.IMessageReply,
		}}
	} else {
		body = map[string]any{"type": "new-message", "data": map[string]any{
			"guid": "rehearsal-in", "text": xrrx.IMessageReply, "isFromMe": false,
			"handle": map[string]any{"address": b.to}, "chats": []any{map[string]any{"guid": "iMessage;-;" + b.to}},
		}}
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, webhook, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if b.name == xrrx.IMessageBackendBlooio {
		req.Header.Set("X-Blooio-Signature", imSign(b.secret, raw))
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback gateway the test started
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the gateway's webhook answered %d", resp.StatusCode)
	}
}

// TestIMessageRehearsal runs the whole path of a recording, record and then
// replay, for each backend, against canned answers in a temporary directory:
// the reference gateway as a process, the proxy that records its backend
// requests, the sandbox that gates them, the cassettes and the replay. It
// reaches no backend and commits nothing; it is the harness's own test, so
// a recording's path is exercised before anything is sent.
func TestIMessageRehearsal(t *testing.T) {
	if xrrModeOf(t) != xrr.ModeReplay {
		t.Skip("the rehearsal runs in the default mode")
	}
	withE2EBinary(t, func() {
		for _, name := range []string{xrrx.IMessageBackendBlooio, xrrx.IMessageBackendBlueBubbles} {
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				sb := &xrrx.IMessageSandbox{ //nolint:gosec // G101: made-up rehearsal values
					Backend: name, To: "recorder@example.com", Sender: "+15550109001", Base: xrrx.IMessageBlooioBase,
					APIKey: "bl_record_key_value", WebhookSecret: "whsec_record0123456789abcdef0123456789", Max: 2, Ledger: filepath.Join(dir, "ledger"), Rehearse: true,
				}
				if name == xrrx.IMessageBackendBlueBubbles {
					sb.Base, sb.Sender, sb.APIKey, sb.WebhookSecret = "http://192.0.2.10:1234", "mac-owner@example.com", "", ""
					sb.Password, sb.WebhookToken = "record-password-value", "record-webhook-token-0123456789"
				}
				cassettes := filepath.Join(dir, "cassettes")
				record := imRun{
					mode: xrr.ModeRecord, cassettes: cassettes, fixture: filepath.Join(dir, "fixtures", name+"-reply.json"),
					live: sb, backend: liveBackend(sb), listen: freeAddr(t),
				}
				runIMessage(t, record)
				if n := imCassetteCount(cassettes); n != 1 {
					t.Fatalf("the rehearsal recorded %d requests, want the one send", n)
				}
				if sb.Sent() != 0 {
					t.Errorf("a rehearsal took %d ledger slots", sb.Sent())
				}
				found, err := xrrx.AuditWith(dir, sb.Deny()...)
				if err != nil {
					t.Fatal(err)
				}
				for _, f := range found {
					t.Errorf("the rehearsal's recording is not clean: %s", f)
				}
				runIMessage(t, imRun{
					mode: xrr.ModeReplay, cassettes: cassettes, fixture: record.fixture, backend: replayBackend(name),
				})
			})
		}
	})
}
