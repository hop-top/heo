package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- Twilio signs webhooks with HMAC-SHA1
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
)

// Cassettes of twilio.sms through the real heo binary. In replay (the
// default) the daemon answers from committed cassettes and the incoming
// text and status callback come from committed, unsigned fixtures, so
// nothing reaches Twilio and no credential is needed. Recording is the one
// live step, and it is narrow: two texts to the owner's own phone from the
// account's own number, behind HEO_RECORD_LIVE=1 and XRR_MODE=record,
// never in CI; see docs/DEVELOPING.md. The daemon is told a placeholder
// account, placeholder numbers and a stand-in auth token only;
// internal/xrrx translates between those and the real ones, counts every
// send against a cap, and refuses everything else.

const (
	// twToken is the auth token of the test profile, in replay and in
	// record: a stand-in. The real token is the transport's, never heo's,
	// so no fixture is signed with it.
	twToken = "e2e-twilio-stand-in-token" // #nosec G101 -- test value
	// twWebhookURL is the URL the channel's signature check is made over.
	// It is a name Twilio never calls: no public URL exists.
	twWebhookURL = "https://heo.invalid/twilio/sms"
	twFixtures   = "testdata/twilio"
)

// twLive is the recording's side of the account: nil in replay.
type twLive struct {
	s *xrrx.TwilioSandbox
}

// openTwilio returns the live sandbox when this test is recording, nil in
// replay, and skips a recording that was not asked to go live.
func openTwilio(t *testing.T) *twLive {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("twilio.sms is replayed or recorded, never passed through to Twilio")
	case xrr.ModeReplay:
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording twilio.sms is live: set %s=1 and %s (docs/DEVELOPING.md)", xrrx.EnvLive, xrrx.EnvTwilioTo)
	}
	if err := xrrx.CheckLiveTwilio(os.Getenv); err != nil {
		t.Fatal(err)
	}
	s, err := xrrx.NewTwilioSandbox(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LearnSender(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	from := s.FromNumber()
	t.Logf("recording twilio.sms: sender ...%s, at most %d sends (%d already made), rehearsal=%v",
		from[len(from)-4:], s.Max, s.Sent(), s.Rehearse)
	t.Cleanup(func() {
		found, err := xrrx.AuditWith("testdata", s.Deny()...)
		if err != nil {
			t.Errorf("audit: %v", err)
		}
		for _, f := range found {
			t.Errorf("recording is not clean: %s", f)
		}
	})
	return &twLive{s: s}
}

// twForm reads a fixture: one field per line, already percent-encoded,
// blank lines and # comments ignored. It returns the form's body and its
// parameters.
func twForm(t *testing.T, name string) (string, url.Values) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(twFixtures, name)) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatalf("no fixture %s: %v", name, err)
	}
	var fields []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			fields = append(fields, l)
		}
	}
	body := strings.Join(fields, "&")
	v, err := url.ParseQuery(body)
	if err != nil {
		t.Fatal(err)
	}
	return body, v
}

// twSign is X-Twilio-Signature: the base64 HMAC-SHA1, under the auth
// token, of the URL Twilio calls followed by each POST parameter's name
// and value in name order.
func twSign(token, signedURL string, form url.Values) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	data := signedURL
	for _, k := range keys {
		data += k + form.Get(k)
	}
	m := hmac.New(sha1.New, []byte(token))
	m.Write([]byte(data))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// twSite is a daemon with a twilio.sms channel on the placeholder account,
// fed fixtures and recording or replaying its calls to Twilio.
type twSite struct {
	*cliSite
	d       *daemon
	hookURL string
	live    *twLive
	channel model.Channel
}

func newTWSite(t *testing.T, live *twLive) *twSite {
	t.Helper()
	c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
	var env []string
	if live != nil {
		env = live.s.ChildEnv()
	}
	// One entry serves both purposes (twilio.rest and twilio.webhook): the
	// stand-in token.
	b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": twToken})
	if err != nil {
		t.Fatal(err)
	}
	c.s.write("secrets/uma/default/twilio", string(b))
	s := &twSite{cliSite: c, live: live}
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook"}
	s.d = c.s.start(append(c.s.xrrEnv("daemon"), env...), nil, []string{"socket", "receivers", "webhook", "workers"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", s.d.log())
		}
		if strings.Contains(s.d.log(), twToken) {
			t.Error("the daemon log carries the channel's token")
		}
		if live != nil {
			for _, lit := range live.s.Deny() {
				if lit != "" && strings.Contains(s.d.log(), lit) {
					t.Error("the daemon log carries something of the real account")
					break
				}
			}
		}
	})
	cfg := sms.Config{AccountSID: xrrx.TwilioAccountSID, From: xrrx.TwilioSender, WebhookURL: twWebhookURL}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(model.TypeTwilioSms), "--key", "sms", "--configuration", string(data), "--credential-ref", "twilio")
	s.channel = ch.Channel
	c.heo(0, "inbox", "create", "--channel", "sms", "--key", "texts", "--destination", xrrx.TwilioSender,
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	s.hookURL = "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	return s
}

// deliver posts a fixture to the channel's webhook as Twilio would: signed
// under the channel's auth token over the URL configured on the channel,
// retried while the daemon has not mounted the channel's route yet. It
// returns the status of the last answer.
func (s *twSite) deliver(fixture, token string) int {
	s.t.Helper()
	body, form := twForm(s.t, fixture)
	sig := twSign(token, twWebhookURL, form)
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.hookURL, bytes.NewReader([]byte(body)))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Twilio-Signature", sig)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback webhook of the daemon the test started
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				return resp.StatusCode
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("webhook %s: the channel's route never answered (%v)", fixture, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// state waits for op to reach one of want.
func (s *twSite) state(op model.ID, want ...model.DeliveryState) deliveryJSON {
	s.t.Helper()
	var d deliveryJSON
	eventually(s.t, "delivery "+string(op)+" in "+fmt.Sprint(want), func() bool {
		s.json(&d, "delivery", "get", string(op))
		for _, w := range want {
			if d.State == w {
				return true
			}
		}
		return false
	})
	return d
}

// cassettes counts the requests the daemon's cassette directory holds.
func (s *twSite) cassettes() int {
	s.t.Helper()
	n := 0
	ents, _ := os.ReadDir(filepath.Join("testdata", "cassettes", s.t.Name(), "daemon"))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".req.yaml") {
			n++
		}
	}
	return n
}

// TestXRRTwilioSMSProviderVerbs: twilio.sms through the real heo binary.
// A text to a number too short to exist ends rejected by Twilio (error
// 21265: a 400 that creates and bills nothing); a send starts a conversation
// with the recipient, a signed incoming text from the same number joins
// it, a reply answers it, and a signed status callback is verified. Each send is a request the daemon's workers make to
// Twilio and the cassettes hold. The verbs the adapter does not declare
// are refused before anything is written, and a preview sends nothing. In
// record mode the two messages are read back from the account. In replay
// nothing reaches Twilio.
func TestXRRTwilioSMSProviderVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		live := openTwilio(t)
		s := newTWSite(t, live)

		// The verb table, offline: twilio.sms starts conversations and
		// declares only history of the provider verbs.
		table := s.heo(0, "channel", "get", "sms").stdout
		for _, want := range []string{
			"verb message edit         unsupported: not_declared (edit)", "verb message delete       unsupported: not_declared (delete)",
			"verb reaction add         unsupported: not_declared (react)", "verb conversation status unsupported: not_declared (work_item_act)",
			"verb history fetch        declared (history)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		// A preview sends nothing.
		held := s.cassettes()
		send := func(to, text string) []string {
			return []string{"message", "send", "--inbox", "texts", "--to", "phone:" + to, "--text", text}
		}
		s.heo(0, append(send(xrrx.TwilioRecipient, "heo adapter test 1"), "--dry-run")...)
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// A number too short to exist: Twilio refuses it, free of charge.
		var bad sendJSON
		s.json(&bad, append(send("+1555", "heo adapter test 0"), "--idempotency-key", "tw-invalid")...)
		d := s.state(bad.Operation.ID, model.Rejected, model.Failed, model.ProviderAccepted, model.Delivered)
		if d.State != model.Rejected || d.Failure == nil || !strings.Contains(d.Failure.Error(), "21265") {
			t.Fatalf("a send to an invalid number = %s %+v, want rejected with Twilio's error 21265", d.State, d.Failure)
		}

		// Send 1: starts a conversation with the recipient.
		var first sendJSON
		s.json(&first, append(send(xrrx.TwilioRecipient, "heo adapter test 1"), "--idempotency-key", "tw-send-1")...)
		if d := s.state(first.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
			t.Fatalf("send 1 = %s %+v", d.State, d.Failure)
		}
		conv := string(first.Conversation)

		// A status callback is verified like any form from Twilio: wrong
		// token 403, signed 200. One that names no attempt (no recorded send
		// can: the attempt's ID is new on every run) settles nothing.
		if got := s.deliver("status-delivered.form", "wrong-token"); got != http.StatusForbidden {
			t.Errorf("a status callback signed with another token = %d, want 403", got)
		}
		if got := s.deliver("status-delivered.form", twToken); got != http.StatusOK {
			t.Fatalf("status callback = %d, want 200", got)
		}
		if d := s.state(first.Operation.ID, model.ProviderAccepted, model.Delivered); d.State != model.ProviderAccepted {
			t.Errorf("a status callback that names no attempt settled the send: %s", d.State)
		}

		// An incoming text, signed, joins the conversation; a reply answers it.
		if got := s.deliver("inbound-sms.form", twToken); got != http.StatusOK {
			t.Fatalf("incoming text = %d, want 200", got)
		}
		var msgs pageJSON[model.Message]
		var inbound model.Message
		eventually(t, "the incoming text", func() bool {
			s.json(&msgs, "message", "list", "--conversation", conv)
			for _, m := range msgs.Items {
				if desk.Text(m.Content) == "heo adapter test inbound" {
					inbound = m
					return true
				}
			}
			return false
		})
		var reply replyJSON
		s.json(&reply, "message", "reply", conv, "--text", "heo adapter test 2", "--idempotency-key", "tw-reply")
		if d := s.state(reply.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
			t.Fatalf("the reply = %s %+v", d.State, d.Failure)
		}

		// What twilio.sms does not declare is refused before any write.
		held = s.cassettes()
		wantStderr(t, s.heo(2, "message", "edit", string(inbound.Ref.ID), "--revision", string(inbound.Revision), "--text", "x", "--dry-run"), "UNSUPPORTED", "does not declare edit")
		wantStderr(t, s.heo(2, "message", "delete", string(inbound.Ref.ID), "--revision", string(inbound.Revision), "--dry-run"), "UNSUPPORTED", "does not declare delete")
		wantStderr(t, s.heo(2, "reaction", "add", "--message", string(inbound.Ref.ID), "👍", "--dry-run"), "UNSUPPORTED", "does not declare react")
		if s.cassettes() != held {
			t.Errorf("a refused verb made a request")
		}

		if live != nil {
			t.Logf("sends made so far, across runs: %d of at most %d", live.s.Sent(), live.s.Max)
			live.expectTexts(t)
		}
	})
}

// expectTexts reads back, in record mode, what the daemon sent: exactly
// the two test texts, from the account's number to the recipient.
func (l *twLive) expectTexts(t *testing.T) {
	t.Helper()
	if l.s.Rehearse {
		return
	}
	got, err := l.s.ReadBack(t.Context(), nil)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Twilio holds %d messages sent by this recording, want 2", len(got))
	}
	for i, raw := range got {
		var m struct{ Body, From, To, Direction, Status string }
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("heo adapter test %d", i+1); m.Body != want || m.From != xrrx.TwilioSender || m.To != xrrx.TwilioRecipient || m.Direction != "outbound-api" {
			t.Errorf("message %d = %+v, want %q from the sender to the recipient, outbound", i+1, m, want)
		}
		t.Logf("message %d: status %s", i+1, m.Status)
	}
}
