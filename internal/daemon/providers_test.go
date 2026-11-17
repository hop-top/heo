package daemon_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/profile"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/adapter/mail/smtp"
	metawhatsapp "hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/adapter/messaging/twilio/sms"
	twiliowhatsapp "hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
	"hop.top/uma/testing/xrrtest"
)

// These tests run heo's direct-message adapters (internal/composition)
// in the runtime with heo's own credential backend and HTTP client. No
// provider is faked: a test reaches only what heo itself serves (the
// webhook listener, the smtp.mail listener, the widget) and the
// adapters' refusals of unauthenticated requests, which they decide
// before any provider call.

const twilioAccount = "AC00000000000000000000000000000001"

// recordingCreds resolves through heo's backend and records every
// request's reference and purpose.
type recordingCreds struct {
	next ports.Credentials
	mu   sync.Mutex
	reqs []ports.CredentialRequest
}

func (c *recordingCreds) Resolve(ctx context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	return c.next.Resolve(ctx, req)
}

// asked reports whether reference was resolved for purpose.
func (c *recordingCreds) asked(reference, purpose string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.reqs, func(r ports.CredentialRequest) bool {
		return r.Reference == reference && r.Purpose == purpose
	})
}

// askedFresh reports whether reference was resolved for purpose as one the
// provider rejected (Fresh).
func (c *recordingCreds) askedFresh(reference, purpose string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.ContainsFunc(c.reqs, func(r ports.CredentialRequest) bool {
		return r.Reference == reference && r.Purpose == purpose && r.Fresh
	})
}

// heoCreds is the file credential backend of a profile, holding
// stored, keyed by reference, as uma credential envelopes.
func heoCreds(t *testing.T, tenant model.TenantID, stored map[string]map[string]any) *recordingCreds {
	t.Helper()
	b, err := credentials.Open(t.Context(), credentials.Options{
		Profile: profile.Default(), Tenant: tenant,
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for ref, env := range stored {
		key, err := b.Key(ref)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SurfaceTokens().Set(t.Context(), key, data); err != nil {
			t.Fatal(err)
		}
	}
	return &recordingCreds{next: b.Channels()}
}

func secretEnvelope(v string) map[string]any {
	return map[string]any{"v": 1, "kind": "secret", "value": v}
}

// withComposition runs heo's direct-message adapters, composed over the
// store the daemon opens and reading the rig's configuration.
func withComposition() rigOption { return withCompositionListen(nil) }

// withCompositionListen is withComposition with the smtp.mail listener
// bound through listen.
func withCompositionListen(listen composition.ListenFunc) rigOption {
	return func(r *rig) {
		for k, val := range composition.Defaults() {
			r.v.SetDefault(k, val)
		}
		viperOf := func() *viper.Viper { return r.v }
		list, err := composition.Adapters(composition.Options{Viper: viperOf, Listen: listen})
		if err != nil {
			r.t.Fatal(err)
		}
		r.extra = append(r.extra, list...)
		r.compose = func(st store.Store) ([]adapters.Adapter, error) {
			return composition.Adapters(composition.Options{Viper: viperOf, Store: st, Listen: listen})
		}
	}
}

func (r *rig) inbox(name string, ch model.ChannelRef, destinations ...string) model.InboxRef {
	r.t.Helper()
	lock := true
	acc, err := r.client().Configuration().ConfigureInbox(r.t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-" + name), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: ch, Name: name, Destinations: destinations,
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		r.t.Fatalf("configure inbox %s: %v", name, err)
	}
	ref, err := acc.Operation.Result.AsInbox()
	if err != nil {
		r.t.Fatal(err)
	}
	return ref
}

func ext(t *testing.T, schema string, v any) model.Extension {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: schema, Version: 1, Data: data}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func do(t *testing.T, method, rawURL, contentType, body string, header ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	_, _ = bufio.NewReader(resp.Body).WriteTo(&b)
	return resp.StatusCode, b.String()
}

func scopeOf(st daemon.ServiceStatus, ch model.ChannelRef) (daemon.ScopeStatus, bool) {
	for _, sc := range st.Scopes {
		if sc.Scope == "channel:"+string(ch.ID) {
			return sc, true
		}
	}
	return daemon.ScopeStatus{}, false
}

// TestDirectMessageWebhooksMounted: every adapter that receives over
// HTTP is mounted at its channel's route, with the provider-facing URL
// from public_base_url, and answers there itself: each refuses an
// unauthenticated request after resolving the credential purpose it
// verifies with, through heo's backend.
func TestDirectMessageWebhooksMounted(t *testing.T) {
	creds := heoCreds(t, "acme", map[string]map[string]any{
		"widget-key": secretEnvelope(strings.Repeat("k", 32)),
		"twilio":     secretEnvelope("twilio-auth-token"),
		"meta":       secretEnvelope("meta-verify-token"),
		"graph":      secretEnvelope("graph-notification-secret"),
	})
	r := newRig(t, "acme", withComposition(), withCredentials(creds),
		withHTTP(composition.NewHTTPClient(nil)),
		withSet(daemon.KeyWebhookPublicBaseURL, "https://hooks.example.com/heo"))
	hook := "https://hooks.example.com/heo/webhooks/"
	chans := map[string]model.ChannelRef{
		"widget": r.channelOf(model.Channel{Key: "widget", Type: widget.Type,
			Configuration: widget.Configuration("site-1"), CredentialRef: "widget-key"}),
		"sms": r.channelOf(model.Channel{Key: "sms", Type: model.TypeTwilioSms, CredentialRef: "twilio",
			Configuration: sms.Config{AccountSID: twilioAccount, From: "+15005550006", WebhookURL: hook + "sms"}.Extension()}),
		"whatsapp": r.channelOf(model.Channel{Key: "whatsapp", Type: model.TypeTwilioWhatsapp, CredentialRef: "twilio",
			Configuration: twiliowhatsapp.Config{AccountSID: twilioAccount, From: "+15005550006", WebhookURL: hook + "whatsapp"}.Extension()}),
		"meta": r.channelOf(model.Channel{Key: "meta", Type: model.TypeMetaWhatsapp, CredentialRef: "meta",
			Configuration: metawhatsapp.Config{PhoneNumberID: "100000000000001"}.Extension()}),
		"gmail": r.channelOf(model.Channel{Key: "gmail", Type: google.Type, CredentialRef: "gmail",
			Configuration: ext(t, google.ConfigSchema, google.Config{User: "support@example.com",
				Topic: "projects/heo-test/topics/gmail-push", PushAudience: hook + "gmail", PushServiceAccount: "push@p.iam.gserviceaccount.com"})}),
		"outlook": r.channelOf(model.Channel{Key: "outlook", Type: microsoft.Type, CredentialRef: "graph",
			Configuration: ext(t, microsoft.ConfigSchema, microsoft.Config{Mailbox: "support@example.com", NotificationURL: hook + "outlook"})}),
	}
	s := r.run(5*time.Second, daemon.NameWebhook)
	s.ready(daemon.NameWebhook)
	base := "http://" + s.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix
	route := func(key string) string { return base + string(chans[key].ID) }

	ws := service(r.host.Status(), daemon.NameWebhook)
	for key, ch := range chans {
		sc, ok := scopeOf(ws, ch)
		if !ok || sc.State != daemon.ScopeReady || sc.URL != hook+string(ch.ID) {
			t.Errorf("%s scope = %+v (mounted %v)", key, sc, ok)
		}
	}

	// The widget is heo-hosted: a session is issued under the channel's
	// session key, and its event stream answers beside the receiver.
	code, body := do(t, http.MethodPost, route("widget")+"/session?"+composition.WidgetSiteParam+"=site-1", "application/json", "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil || sess.Token == "" || sess.Assurance != widget.AssuranceAnonymous {
		t.Errorf("widget session = %d %s", code, body)
	}
	if code, _ := do(t, http.MethodPost, route("widget")+"/session?"+composition.WidgetSiteParam+"=elsewhere", "application/json", "{}"); code != http.StatusUnauthorized {
		t.Errorf("widget session for an unlisted site = %d, want 401", code)
	}
	if code, _ := do(t, http.MethodGet, route("widget")+composition.WidgetEventsRoute, "", ""); code != http.StatusUnauthorized {
		t.Errorf("widget events without a token = %d, want 401", code)
	}

	for _, tc := range []struct {
		key, method, path, contentType, body string
		want                                 int
	}{
		{"sms", http.MethodPost, "", "application/x-www-form-urlencoded", "AccountSid=" + twilioAccount + "&From=%2B15558675310&Body=hi", http.StatusForbidden},
		{"whatsapp", http.MethodPost, "", "application/x-www-form-urlencoded", "AccountSid=" + twilioAccount + "&Body=hi", http.StatusForbidden},
		{"meta", http.MethodGet, "?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=1234", "", "", http.StatusForbidden},
		{"meta", http.MethodPost, "", "application/json", `{"object":"whatsapp_business_account","entry":[]}`, http.StatusUnauthorized},
		{"gmail", http.MethodPost, "", "application/json", `{"message":{"data":""}}`, http.StatusUnauthorized},
		{"outlook", http.MethodPost, "?validationToken=check-1", "text/plain", "", http.StatusOK},
		{"outlook", http.MethodPost, "", "application/json", `{"value":[{"clientState":"forged","changeType":"created","resource":"x"}]}`, http.StatusForbidden},
	} {
		if code, body := do(t, tc.method, route(tc.key)+tc.path, tc.contentType, tc.body); code != tc.want {
			t.Errorf("%s %s %s = %d %q, want %d", tc.method, tc.key, tc.path, code, body, tc.want)
		}
	}
	for _, want := range []struct{ ref, purpose string }{
		{"widget-key", widget.CredentialPurpose},
		{"twilio", sms.PurposeWebhook},
		{"meta", metawhatsapp.PurposeVerifyToken},
		{"meta", metawhatsapp.PurposeAppSecret},
		{"graph", microsoft.PurposeNotifications},
	} {
		if !creds.asked(want.ref, want.purpose) {
			t.Errorf("credential %q never resolved for purpose %q", want.ref, want.purpose)
		}
	}
}

// TestWidgetVisitorReachesInbox: a visitor message posted to the
// widget lands in the channel's inbox, and an open event stream ends
// with the stop instead of holding it to its deadline.
func TestWidgetVisitorReachesInbox(t *testing.T) {
	creds := heoCreds(t, "acme", map[string]map[string]any{"widget-key": secretEnvelope(strings.Repeat("k", 32))})
	r := newRig(t, "acme", withComposition(), withCredentials(creds))
	ch := r.channelOf(model.Channel{Key: "widget", Type: widget.Type, Configuration: widget.Configuration("site-1"), CredentialRef: "widget-key"})
	r.inbox("web", ch, "site-1")
	s := r.run(2*time.Second, daemon.NameWebhook)
	s.ready(daemon.NameWebhook)
	route := "http://" + s.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix + string(ch.ID)

	code, body := do(t, http.MethodPost, route+"/session", "application/json", "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("session = %d %s", code, body)
	}
	code, body = do(t, http.MethodPost, route+"/messages", "application/json", `{"id":"m-1","text":"hello from the site"}`,
		"Authorization", "Bearer "+sess.Token)
	if code != http.StatusAccepted {
		t.Fatalf("message = %d %s", code, body)
	}
	page, err := r.client().Conversations().List(t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("conversations = %+v, %v; want the visitor's", page.Items, err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, route+composition.WidgetEventsRoute+"?token="+url.QueryEscape(sess.Token), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("events = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	start := time.Now()
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop with an open event stream: %s %v", res.Outcome, res.Err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("stop took %v with an open event stream", d)
	}
}

// sseEvent is one event of a widget event stream.
type sseEvent struct{ id, name, data string }

// openEvents opens a widget event stream with token, resuming after
// cursor when it is not empty, and returns its events as they arrive.
func openEvents(t *testing.T, route, token, cursor string) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, route+composition.WidgetEventsRoute+"?token="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("events = %d", resp.StatusCode)
	}
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	out := make(chan sseEvent, 16)
	go func() {
		defer close(out)
		var ev sseEvent
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					out <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// nextEvent waits for the stream's next event.
func nextEvent(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("event stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
		return sseEvent{}
	}
}

// TestWidgetEventsSurviveRestart: what workers deliver to a widget
// visitor is kept in the profile's store, so after the daemon stops and
// starts again a stream resumes from its cursor; a cursor behind the
// retained entries (adapters.widget.retain) gets a reset first.
func TestWidgetEventsSurviveRestart(t *testing.T) {
	creds := heoCreds(t, "acme", map[string]map[string]any{"widget-key": secretEnvelope(strings.Repeat("k", 32))})
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withSet(composition.KeyWidgetRetain, 2))
	ch := r.channelOf(model.Channel{Key: "widget", Type: widget.Type, Configuration: widget.Configuration("site-1"), CredentialRef: "widget-key"})
	r.inbox("web", ch, "site-1")
	names := []string{daemon.NameWorkers, daemon.NameWebhook}
	s := r.run(2*time.Second, names...)
	s.ready(names...)
	route := "http://" + s.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix + string(ch.ID)

	code, body := do(t, http.MethodPost, route+"/session", "application/json", "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("session = %d %s", code, body)
	}
	if code, body := do(t, http.MethodPost, route+"/messages", "application/json", `{"id":"m-1","text":"hello"}`,
		"Authorization", "Bearer "+sess.Token); code != http.StatusAccepted {
		t.Fatalf("message = %d %s", code, body)
	}
	page, err := r.client().Conversations().List(t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("conversations = %+v, %v", page.Items, err)
	}
	reply := func(text string) {
		t.Helper()
		if _, err := r.client().Messages().Reply(t.Context(), model.Reply{Command: r.cmd("reply-" + text), Conversation: page.Items[0].Ref,
			Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}}); err != nil {
			t.Fatalf("reply %s: %v", text, err)
		}
	}
	live := openEvents(t, route, sess.Token, "")
	reply("one")
	first := nextEvent(t, live)
	reply("two")
	second := nextEvent(t, live)
	if first.name != widget.EventMessage || !strings.Contains(first.data, `"one"`) || second.name != widget.EventMessage || !strings.Contains(second.data, `"two"`) {
		t.Fatalf("live events = %+v, %+v; want both replies", first, second)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}

	s = r.run(2*time.Second, names...)
	s.ready(names...)
	route = "http://" + s.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix + string(ch.ID)
	resumed := nextEvent(t, openEvents(t, route, sess.Token, first.id))
	if resumed.id != second.id || !strings.Contains(resumed.data, `"two"`) {
		t.Fatalf("resumed after %s with %+v, want the reply delivered before the restart (%s)", first.id, resumed, second.id)
	}
	// A third reply leaves two entries retained: a stream from the start
	// is behind them and resets before the retained ones.
	reply("three")
	if ev := nextEvent(t, openEvents(t, route, sess.Token, second.id)); !strings.Contains(ev.data, `"three"`) {
		t.Fatalf("after %s = %+v, want the third reply", second.id, ev)
	}
	fromStart := openEvents(t, route, sess.Token, "")
	if ev := nextEvent(t, fromStart); ev.name != widget.EventReset {
		t.Fatalf("stream from the start = %+v, want a reset (retain 2)", ev)
	}
	if ev := nextEvent(t, fromStart); ev.id != second.id {
		t.Errorf("after the reset = %+v, want the oldest retained entry %s", ev, second.id)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
}

// TestReceiveHTTPAccountsChecked: channels that receive only over HTTP
// are held to one receiver per provider account too, within a profile
// and across profiles.
func TestReceiveHTTPAccountsChecked(t *testing.T) {
	smsChannel := func(key, ref string) model.Channel {
		return model.Channel{Key: key, Type: model.TypeTwilioSms, CredentialRef: ref,
			Configuration: sms.Config{AccountSID: twilioAccount, From: "+15005550006", WebhookURL: "https://hooks.example.com/" + key}.Extension()}
	}
	r := newRig(t, "acme", withComposition())
	r.channelOf(smsChannel("one", "twilio"))
	r.channelOf(smsChannel("two", "twilio-2"))
	res := r.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "receive the same provider account") ||
		!strings.Contains(res.Err.Message, `"one"`) || !strings.Contains(res.Err.Message, `"two"`) {
		t.Fatalf("one profile, two channels on one number: %s %v", res.Outcome, res.Err)
	}

	dir := t.TempDir()
	first := newRig(t, "first", withComposition(), withAccounts(dir))
	second := newRig(t, "second", withComposition(), withAccounts(dir))
	first.channelOf(smsChannel("one", "twilio"))
	second.channelOf(smsChannel("two", "twilio"))
	s1 := first.run(5*time.Second, daemon.NameWebhook)
	s1.ready(daemon.NameWebhook)
	res = second.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/first (pid ") {
		t.Fatalf("second profile on the first's number: %s %v", res.Outcome, res.Err)
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
}

// TestMailboxWithoutCredentialDegradesByScope: a mailbox whose
// credential is absent is reported by scope while the services stay
// ready, after asking heo's backend for its purpose; a mailbox with push
// disabled is not mounted on the webhook and fails nothing there.
func TestMailboxWithoutCredentialDegradesByScope(t *testing.T) {
	creds := heoCreds(t, "acme", nil)
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withHTTP(composition.NewHTTPClient(nil)))
	gmail := r.channelOf(model.Channel{Key: "gmail", Type: google.Type, CredentialRef: "gmail",
		Configuration: ext(t, google.ConfigSchema, google.Config{User: "support@example.com"})})
	mailbox := r.channelOf(model.Channel{Key: "mailbox", Type: imap.Type, CredentialRef: "mailbox",
		Configuration: ext(t, imap.ConfigSchema, imap.Config{Address: "support@example.com",
			IMAP: imap.Server{Host: "imap.example.com", TLSPort: 993}, Submission: imap.Server{Host: "smtp.example.com", TLSPort: 465}})})
	s := r.run(5*time.Second, daemon.NameReceivers, daemon.NameWebhook)
	s.ready(daemon.NameReceivers, daemon.NameWebhook)
	for _, ch := range []model.ChannelRef{gmail, mailbox} {
		waitFor(t, func() bool {
			sc, ok := scopeOf(service(r.host.Status(), daemon.NameReceivers), ch)
			return ok && sc.State == daemon.ScopeFailed
		})
	}
	st := r.host.Status()
	if rs := service(st, daemon.NameReceivers); rs.State != daemon.StateDegraded || !st.Ready {
		t.Errorf("receivers = %+v, ready %v", rs, st.Ready)
	}
	for ref, purpose := range map[string]string{"gmail": google.PurposeGmail, "mailbox": imap.PurposeIMAP} {
		if !creds.asked(ref, purpose) {
			t.Errorf("credential %q never resolved for purpose %q", ref, purpose)
		}
	}
	ws := service(st, daemon.NameWebhook)
	if _, mounted := scopeOf(ws, gmail); mounted || ws.State != daemon.StateReady {
		t.Errorf("webhook with a push-disabled mailbox = %+v", ws)
	}
	addr := s.svcs[daemon.NameWebhook].(serve.Addressed).Addr()
	if code, _ := do(t, http.MethodPost, "http://"+addr+daemon.WebhookPrefix+string(gmail.ID), "application/json", "{}"); code != http.StatusNotFound {
		t.Errorf("push-disabled mailbox route = %d, want 404", code)
	}
}

// TestSMTPReceiverListensOnConfiguredAddress: an smtp.mail channel's MX
// receiver serves on adapters.smtp.addr; without it the channel is
// reported by scope and sends only.
func TestSMTPReceiverListensOnConfiguredAddress(t *testing.T) {
	mx := func(r *rig) model.ChannelRef {
		return r.channelOf(model.Channel{Key: "mx", Type: model.TypeSmtpMail, CredentialRef: "relay",
			Configuration: ext(t, smtp.ConfigSchema, smtp.Config{Hostname: "mx.example.com",
				Recipients: []smtp.Recipient{{Address: "support@example.com", Destination: "support"}},
				From:       "support@example.com", Relay: smtp.Relay{Host: "relay.example.com", TLSPort: 465}})})
	}
	// The listener binds slowly, as on a loaded host: readiness waits
	// for it, so the MX answers the first dial once the service is ready.
	slow := func(ctx context.Context, network, address string) (net.Listener, error) {
		time.Sleep(300 * time.Millisecond)
		var lc net.ListenConfig
		return lc.Listen(ctx, network, address)
	}
	addr := freeAddr(t)
	r := newRig(t, "acme", withCompositionListen(slow), withSet(composition.KeySMTPAddr, addr))
	ch := mx(r)
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	if sc, _ := scopeOf(service(r.host.Status(), daemon.NameReceivers), ch); sc.State != daemon.ScopeReady {
		t.Errorf("mx ready: scope %+v", sc)
	}
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("receivers ready, mx listener not: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	greeting, _ := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if !strings.HasPrefix(greeting, "220 ") || !strings.Contains(greeting, "mx.example.com") {
		t.Errorf("greeting = %q", greeting)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}

	r = newRig(t, "acme", withComposition())
	ch = mx(r)
	s = r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	sc, ok := scopeOf(service(r.host.Status(), daemon.NameReceivers), ch)
	if !ok || sc.State != daemon.ScopeFailed || !strings.Contains(sc.Reason, composition.KeySMTPAddr+" is not set") {
		t.Errorf("mx without a listener: scope %+v", sc)
	}
}

// TestTwilioSMSRecordedRefusalThroughHeo replays Twilio's recorded
// answer to a refused create (error 21266, To equal to From; uma's
// twilio.sms cassette "rejected") through heo's runtime: the message is
// queued as an operator would, a heo worker sends it through the
// registered adapter, heo's HTTP client and heo's credential backend,
// and the operation settles rejected for good.
func TestTwilioSMSRecordedRefusalThroughHeo(t *testing.T) {
	const (
		accountSID = "AC00000000000000000000000000000000"
		number     = "+15005550006"
	)
	dir := filepath.Join(moduleDir(t, "hop.top/uma/adapter/messaging/twilio"), "sms", "testdata", "cassettes", "twilio", "rejected")
	recorded := xrrtest.NewClient(t, dir,
		xrrtest.WithMode(xrrtest.ModeReplay),
		xrrtest.WithPlaceholder(accountSID, "ACCOUNT_SID"),
		xrrtest.WithPlaceholder(number, "FROM_NUMBER"),
		xrrtest.WithPlaceholder(url.QueryEscape(number), "FROM_NUMBER_QUERY"),
	)
	creds := heoCreds(t, "acme", map[string]map[string]any{"twilio": secretEnvelope("replay-token")})
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withHTTP(composition.NewHTTPClient(recorded.Transport)))
	ch := r.channelOf(model.Channel{Key: "sms", Type: model.TypeTwilioSms, CredentialRef: "twilio",
		Configuration: sms.Config{AccountSID: accountSID, From: number}.Extension()})
	inbox := r.inbox("sms", ch, number)
	acc, err := r.client().Conversations().Start(t.Context(), model.StartConversation{
		Command: r.cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: ch, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: number},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "recorded test message"}}},
	})
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	s := r.run(5*time.Second, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	var op model.Operation
	waitFor(t, func() bool {
		op = r.operation(acc.Operation.Ref)
		return op.State != model.Queued && op.State != model.RetryPending
	})
	if op.State != model.Rejected || op.Failure == nil || op.Failure.Retry != model.NoRetry ||
		!strings.Contains(op.Failure.SafeMessage, "error 21266") {
		t.Fatalf("operation = %s (%+v), want rejected for good with Twilio's error 21266", op.State, op.Failure)
	}
	if !creds.asked("twilio", sms.PurposeREST) {
		t.Errorf("credential never resolved for purpose %q", sms.PurposeREST)
	}
}

// moduleDir is the directory of module path in this build.
func moduleDir(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", path).Output() //nolint:gosec // fixed module path
	if err != nil {
		t.Fatalf("go list %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}
