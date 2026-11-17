package daemon_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// These tests run the relay.messages adapter on its iMessage medium in
// heo's runtime: what the operator's gateway posts reaches the channel's
// webhook route on the real listener, authenticated by the channel's own
// signature scheme with the secret in heo's credential backend, and what
// heo sends back goes through heo's HTTP seam to the gateway's callback
// and lookup (relayfake_test.go). No gateway, backend or network is
// involved.

const (
	relayHost           = "gw.example.com:8787"
	relayCallback       = "callback-secret-0123456789abcdef0123456" // #nosec G101 -- test value
	relayWebhook        = "webhook-secret-0123456789abcdef01234567" // #nosec G101 -- test value
	relayRef            = "gw"
	relayLine           = "+15550100002"
	relayPerson         = "Alice@Example.com"
	relayPersonIdentity = "alice@example.com"
	relaySignature      = "X-Webhook-Signature"
	relayTimestamp      = "X-Webhook-Timestamp"
)

// relayConfig is the channel the reference gateway is configured for:
// its HMAC signature over a signed timestamp, replays refused, and its
// callback and lookup.
func relayConfig(mod ...func(*relay.Config)) relay.Config {
	cfg := relay.Config{
		Medium: relay.MediumIMessage,
		Auth: relay.Auth{
			Scheme: "hmac", Header: relaySignature,
			Timestamp: relay.AuthTimestamp{Header: relayTimestamp}, Replay: relay.AuthReplay{Source: "signature"},
		},
		Callback: &relay.Callback{URL: "https://" + relayHost + "/uma/reply", LookupURL: "https://" + relayHost + "/uma/lookup"},
	}
	for _, m := range mod {
		m(&cfg)
	}
	return cfg
}

func relayCreds(t *testing.T) *recordingCreds {
	return slackCreds(t, "acme", relayRef, map[string]string{relay.PurposeWebhook: relayWebhook, relay.PurposeCallback: relayCallback})
}

// relaySite is a daemon with one relay.messages channel and a
// per-contact inbox bound to the gateway's line.
type relaySite struct {
	t     *testing.T
	r     *rig
	gw    *relayGateway
	creds *recordingCreds
	ch    model.ChannelRef
	inbox model.InboxRef
	route string
	s     *supervised
}

// newRelaySite runs the daemon's services over one channel of cfg; the
// host lists the gateway's host as the only one a callback may name.
func newRelaySite(t *testing.T, creds *recordingCreds, cfg relay.Config, services ...string) *relaySite {
	t.Helper()
	gw := newRelayGateway(t, relayHost, relayCallback)
	r := newRig(t, "acme", withSet(composition.KeyRelayCallbackHosts, []string{relayHost}),
		withComposition(), withCredentials(creds), withHTTP(gw))
	return startRelaySite(t, r, gw, creds, cfg, services...)
}

func startRelaySite(t *testing.T, r *rig, gw *relayGateway, creds *recordingCreds, cfg relay.Config, services ...string) *relaySite {
	t.Helper()
	site := &relaySite{t: t, r: r, gw: gw, creds: creds}
	site.ch = r.channelOf(model.Channel{Key: "imessage", Type: model.TypeRelayMessages, CredentialRef: relayRef, Configuration: cfg.Extension()})
	lock := true
	acc, err := r.client().Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-imessage"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: site.ch, Name: "imessage", Destinations: []string{relayLine},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		t.Fatalf("configure inbox: %v", err)
	}
	if site.inbox, err = acc.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	site.s = r.run(5*time.Second, services...)
	site.s.ready(services...)
	if wh, ok := site.s.svcs[daemon.NameWebhook]; ok {
		if a, ok := wh.(serve.Addressed); ok {
			site.route = "http://" + a.Addr() + daemon.WebhookPrefix + string(site.ch.ID)
		}
	}
	return site
}

// relaySign is the signature the reference gateway puts on what it posts:
// hex HMAC-SHA-256 of the timestamp, a dot and the body.
func relaySign(secret, ts, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// postRaw posts body with the given headers.
func (s *relaySite) postRaw(body string, header ...string) (int, string) {
	s.t.Helper()
	return do(s.t, http.MethodPost, s.route, "application/json", body, header...)
}

// signed posts body as the gateway would at when, signed with secret.
func (s *relaySite) signed(body, secret string, when time.Time) (int, string) {
	s.t.Helper()
	ts := strconv.FormatInt(when.Unix(), 10)
	return s.postRaw(body, relayTimestamp, ts, relaySignature, relaySign(secret, ts, body))
}

// inbound is a message of the gateway's, in the neutral field names.
func relayInbound(from, id, text string) string {
	b, _ := json.Marshal(map[string]any{"from": from, "to": relayLine, "body": text, "message_id": id})
	return string(b)
}

// message delivers one inbound message, signed as the gateway signs it.
func (s *relaySite) message(from, id, text string) (int, string) {
	s.t.Helper()
	return s.signed(relayInbound(from, id, text), relayWebhook, time.Now())
}

func (s *relaySite) conversations() []model.Conversation {
	s.t.Helper()
	p, err := s.r.client().Conversations().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *relaySite) messages(conv model.ConversationRef) []model.Message {
	s.t.Helper()
	p, err := s.r.client().Messages().List(s.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "test"}, Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

// settled waits until the operation is no longer queued or in flight.
func (s *relaySite) settled(acc model.Acceptance) model.Operation {
	s.t.Helper()
	var op model.Operation
	waitFor(s.t, func() bool {
		op = s.r.operation(acc.Operation.Ref)
		switch op.State {
		case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected, model.Unknown:
			return true
		}
		return false
	})
	return op
}

// route is the route of a person's address on iMessage.
func (s *relaySite) route_(handle string) model.ExternalRef {
	return model.ExternalRef{Channel: s.ch, Namespace: relay.NamespaceIMessage, Kind: relay.RouteKind, ID: handle}
}

// TestRelayWebhook: the channel's route is the gateway's uma webhook URL.
// A request with no signature, another secret, a stale timestamp or a
// signature that does not parse is refused before its body is read, one
// that is not a message is 400, and the same signed delivery twice is
// refused 409. A message opens the sender's conversation once however
// often the gateway sends it, under the lowercased Apple ID of the sender
// in the iMessage namespace, and receiving by webhook calls nothing.
func TestRelayWebhook(t *testing.T) {
	creds := relayCreds(t)
	s := newRelaySite(t, creds, relayConfig(), daemon.NameWebhook)
	body := relayInbound(relayPerson, "m-1", "is anyone there?")
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	stale := strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	for name, c := range map[string]struct {
		header []string
		want   int
	}{
		"no signature":      {nil, http.StatusUnauthorized},
		"timestamp only":    {[]string{relayTimestamp, ts}, http.StatusUnauthorized},
		"another secret":    {[]string{relayTimestamp, ts, relaySignature, relaySign("not-the-webhook-secret-at-all-0123456", ts, body)}, http.StatusUnauthorized},
		"a stale timestamp": {[]string{relayTimestamp, stale, relaySignature, relaySign(relayWebhook, stale, body)}, http.StatusUnauthorized},
		"another body":      {[]string{relayTimestamp, ts, relaySignature, relaySign(relayWebhook, ts, body+" ")}, http.StatusUnauthorized},
		"an unreadable one": {[]string{relayTimestamp, ts, relaySignature, "not hex"}, http.StatusBadRequest},
	} {
		if code, _ := s.postRaw(body, c.header...); code != c.want {
			t.Errorf("%s = %d, want %d", name, code, c.want)
		}
	}
	if code, _ := do(t, http.MethodGet, s.route, "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	if !creds.asked(relayRef, relay.PurposeWebhook) {
		t.Error("the webhook secret was never resolved")
	}
	if n := len(s.conversations()); n != 0 {
		t.Fatalf("refused requests opened %d conversations", n)
	}
	// Authenticated, and not a message.
	for name, b := range map[string]string{
		"not JSON":               "{not json",
		"no message id":          `{"from":"+15550100001","to":"` + relayLine + `","body":"hi"}`,
		"no sender":              `{"to":"` + relayLine + `","body":"hi","message_id":"m-9"}`,
		"neither body nor media": `{"from":"+15550100001","to":"` + relayLine + `","message_id":"m-9"}`,
	} {
		if code, _ := s.signed(b, relayWebhook, now); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}

	// A message opens the sender's conversation, once.
	code, got := s.signed(body, relayWebhook, now)
	if code != http.StatusOK || got != `{"status":"accepted"}` {
		t.Fatalf("message = %d %q", code, got)
	}
	if code, _ := s.signed(body, relayWebhook, now); code != http.StatusConflict {
		t.Errorf("the same signed delivery again = %d, want 409", code)
	}
	if code, got := s.signed(body, relayWebhook, now.Add(time.Second)); code != http.StatusOK || got != `{"status":"accepted"}` {
		t.Errorf("the same message signed again = %d %q, want 200: a duplicate is accepted", code, got)
	}
	convs := s.conversations()
	if len(convs) != 1 || convs[0].Mode != model.ModePerContact || convs[0].ReplyRoute.Namespace != relay.NamespaceIMessage ||
		convs[0].ReplyRoute.Kind != relay.RouteKind || convs[0].ReplyRoute.ID != relayPersonIdentity {
		t.Fatalf("conversations = %+v, want one on relay.imessage/phone:%s", convs, relayPersonIdentity)
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 || desk.Text(got[0].Content) != "is anyone there?" {
		t.Fatalf("messages = %+v, a repeated delivery is a duplicate and adds none", got)
	}
	// A phone number is kept as sent.
	if code, _ := s.message("+15550100001", "m-2", "from a number"); code != http.StatusOK {
		t.Fatalf("a number's message = %d", code)
	}
	if got := s.conversations(); len(got) != 2 {
		t.Errorf("a second sender: %d conversations, want 2", len(got))
	}
	if n := s.gw.total(); n != 0 {
		t.Errorf("receiving made %d calls to the gateway", n)
	}
}

// TestRelayWebhookCredentials: with no webhook secret stored the route
// answers 503, so the gateway retries once it is; the secret is asked of
// heo's backend for the webhook purpose.
func TestRelayWebhookCredentials(t *testing.T) {
	none := slackCreds(t, "acme", relayRef, nil)
	s := newRelaySite(t, none, relayConfig(), daemon.NameWebhook)
	if code, _ := s.message(relayPerson, "m-1", "hi"); code != http.StatusServiceUnavailable {
		t.Errorf("no secret stored = %d, want 503", code)
	}
	if !none.asked(relayRef, relay.PurposeWebhook) {
		t.Error("the webhook secret was never asked of heo's backend")
	}
	if n := len(s.conversations()); n != 0 {
		t.Errorf("a refused message opened %d conversations", n)
	}
}

// TestRelayBearerWebhook: a gateway that authenticates with a bearer
// token is a channel configuration, not a heo setting.
func TestRelayBearerWebhook(t *testing.T) {
	cfg := relayConfig(func(c *relay.Config) { c.Auth = relay.Auth{Scheme: "bearer"} })
	s := newRelaySite(t, relayCreds(t), cfg, daemon.NameWebhook)
	body := relayInbound(relayPerson, "m-1", "hi")
	if code, _ := s.postRaw(body); code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	if code, _ := s.postRaw(body, "Authorization", "Bearer not-the-webhook-secret-at-all-0123456"); code != http.StatusUnauthorized {
		t.Errorf("another token = %d, want 401", code)
	}
	if code, _ := s.postRaw(body, "Authorization", "Bearer "+relayWebhook); code != http.StatusOK {
		t.Errorf("the token = %d, want 200", code)
	}
}

// settledOp is the operation of acc, settled.
func (s *relaySite) reply(conv model.ConversationRef, key, text string) model.Acceptance {
	s.t.Helper()
	acc, err := s.r.client().Messages().Reply(s.t.Context(), model.Reply{Command: s.r.cmd(key), Conversation: conv, Content: desk.TextContent(text)})
	if err != nil {
		s.t.Fatalf("reply: %v", err)
	}
	return acc
}

// TestRelaySend: through heo's workers, the real adapter and heo's HTTP
// seam, a reply goes to the gateway's callback with the callback secret
// as a bearer credential and the operation as the Idempotency-Key, to the
// sender's lowercased address, naming the message answered; a first
// message goes to any address the host names, under its namespace, and
// the gateway's own message ID is what heo keeps.
func TestRelaySend(t *testing.T) {
	creds := relayCreds(t)
	s := newRelaySite(t, creds, relayConfig(), daemon.NameWebhook, daemon.NameWorkers)
	if code, body := s.message(relayPerson, "m-1", "hello"); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	conv := s.conversations()[0]
	acc := s.reply(conv.Ref, "reply-1", "on it")
	op := s.settled(acc)
	if op.State != model.ProviderAccepted {
		t.Fatalf("reply = %s %+v", op.State, op.Failure)
	}
	sent := s.gw.called("/uma/reply")
	if len(sent) != 1 {
		t.Fatalf("the gateway took %d callbacks, want 1", len(sent))
	}
	c := sent[0]
	if c.Method != http.MethodPost || c.Host != relayHost || c.Authorization != "Bearer "+relayCallback || c.Key == "" ||
		c.Body["medium"] != "imessage" || c.Body["to"] != relayPersonIdentity || c.Body["body"] != "on it" || c.Body["in_reply_to"] != "m-1" ||
		c.Body["operation_id"] != c.Key {
		t.Fatalf("reply sent %+v, want a bearer-authenticated post to %s naming the sender and the message answered", c, relayPersonIdentity)
	}
	if !creds.asked(relayRef, relay.PurposeCallback) {
		t.Error("the callback secret was never asked of heo's backend")
	}
	mine := s.messages(conv.Ref)
	if id := providerID(mine[len(mine)-1]); id != "gw-1" {
		t.Errorf("the reply names provider message %q, want the gateway's gw-1", id)
	}

	// A first message to someone who never wrote in: a number, and an
	// address (a mixed-case one is lowercased by uma's start; see
	// TestRelayStartNormalizesAddress).
	for i, to := range []string{"+15550100003", "bob@example.com"} {
		start, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + strconv.Itoa(i)), Inbox: s.inbox, Recipient: s.route_(to), Content: desk.TextContent("welcome"),
		})
		if err != nil {
			t.Fatalf("start %s: %v", to, err)
		}
		if op := s.settled(start); op.State != model.ProviderAccepted {
			t.Fatalf("start %s = %s %+v", to, op.State, op.Failure)
		}
	}
	sent = s.gw.called("/uma/reply")
	if len(sent) != 3 || sent[1].Body["to"] != "+15550100003" || sent[2].Body["to"] != "bob@example.com" || sent[1].Body["in_reply_to"] != nil {
		t.Fatalf("callbacks = %+v, want the number and the address, answering nothing", sent)
	}
	if got := s.gw.called("/uma/lookup"); len(got) != 0 {
		t.Errorf("settled sends asked the lookup %d times", len(got))
	}
	if stray := s.gw.strays(); len(stray) > 0 {
		t.Errorf("the callback credential reached %v", stray)
	}
}

// TestRelayStartNormalizesAddress: a first message to an Apple ID written
// in mixed case goes to the gateway, and is filed, under the lowercased
// address, so the person's own reply from that address lands in the
// conversation the start opened: one identity, one conversation.
func TestRelayStartNormalizesAddress(t *testing.T) {
	s := newRelaySite(t, relayCreds(t), relayConfig(), daemon.NameWebhook, daemon.NameWorkers)
	start, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
		Command: s.r.cmd("start-mixed"), Inbox: s.inbox, Recipient: s.route_("Bob@Example.com"), Content: desk.TextContent("welcome"),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if op := s.settled(start); op.State != model.ProviderAccepted {
		t.Fatalf("start = %s %+v", op.State, op.Failure)
	}
	if sent := s.gw.called("/uma/reply"); len(sent) != 1 || sent[0].Body["to"] != "bob@example.com" {
		t.Fatalf("callbacks = %+v, want one to the lowercased address", sent)
	}
	if code, body := s.message("bob@example.com", "m-1", "hi, it is Bob"); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	convs := s.conversations()
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "bob@example.com" {
		t.Fatalf("conversations = %+v, want the start's one, on the lowercased address", convs)
	}
	if got := s.messages(convs[0].Ref); len(got) != 2 {
		t.Errorf("the conversation holds %d messages, want the start and the reply", len(got))
	}
}

// TestRelayLostAnswerIsLookedUp: a callback whose answer is lost is
// outcome_unknown, and with a lookup URL in the channel's callback heo
// asks the gateway what became of it by the same key before anything is
// sent again: the gateway holds the key, so the operation settles
// provider_accepted and no second message goes out.
func TestRelayLostAnswerIsLookedUp(t *testing.T) {
	s := newRelaySite(t, relayCreds(t), relayConfig(), daemon.NameWebhook, daemon.NameWorkers)
	if code, body := s.message(relayPerson, "m-1", "hello"); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	conv := s.conversations()[0]
	s.gw.setLose(1)
	acc := s.reply(conv.Ref, "reply-1", "on it")
	waitFor(t, func() bool { return len(s.gw.called("/uma/reply")) == 1 })
	waitFor(t, func() bool { return s.r.operation(acc.Operation.Ref).State == model.ProviderAccepted })
	op := s.r.operation(acc.Operation.Ref)
	if op.State != model.ProviderAccepted {
		t.Fatalf("after the lookup the operation is %s %+v, want provider_accepted", op.State, op.Failure)
	}
	if got := s.gw.called("/uma/reply"); len(got) != 1 {
		t.Errorf("the gateway took %d callbacks, want 1: the lookup settled it", len(got))
	}
	look := s.gw.called("/uma/lookup")
	if len(look) == 0 || look[0].Authorization != "Bearer "+relayCallback || look[0].Query.Get("operation_id") != s.gw.called("/uma/reply")[0].Key {
		t.Fatalf("lookups = %+v, want the callback's key under the callback secret", look)
	}
}

// TestRelayWithoutLookupStaysUnknown: a channel whose callback names no
// lookup URL reports reconcile disabled, and a send whose answer was lost
// stays outcome_unknown and is never sent again; the gateway is not asked.
func TestRelayWithoutLookupStaysUnknown(t *testing.T) {
	cfg := relayConfig(func(c *relay.Config) { c.Callback.LookupURL = "" })
	s := newRelaySite(t, relayCreds(t), cfg, daemon.NameWebhook, daemon.NameWorkers)
	if code, body := s.message(relayPerson, "m-1", "hello"); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	conv := s.conversations()[0]
	s.gw.setLose(1)
	acc := s.reply(conv.Ref, "reply-1", "on it")
	if op := s.settled(acc); op.State != model.Unknown {
		t.Fatalf("a lost answer = %s %+v, want outcome_unknown", op.State, op.Failure)
	}
	time.Sleep(200 * time.Millisecond)
	if got := s.gw.called("/uma/reply"); len(got) != 1 {
		t.Errorf("the gateway took %d callbacks, want 1: an unknown send is never sent again", len(got))
	}
	if got := s.gw.called("/uma/lookup"); len(got) != 0 {
		t.Errorf("the gateway was asked %d times with no lookup URL", len(got))
	}
}

// TestRelayCallbackAnswers: the gateway's answer to the callback is the
// operation's outcome: a refusal and a redirect are final, and a redirect
// is never followed, so the credential reaches no other host.
func TestRelayCallbackAnswers(t *testing.T) {
	s := newRelaySite(t, relayCreds(t), relayConfig(), daemon.NameWebhook, daemon.NameWorkers)
	if code, body := s.message(relayPerson, "m-1", "hello"); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	conv := s.conversations()[0]
	for _, tc := range []struct {
		status int
		want   model.DeliveryState
	}{
		{http.StatusUnprocessableEntity, model.Rejected},
		{http.StatusFound, model.Rejected},
	} {
		s.gw.setStatus("/uma/reply", tc.status)
		acc := s.reply(conv.Ref, "reply-"+strconv.Itoa(tc.status), "on it")
		if op := s.settled(acc); op.State != tc.want {
			t.Errorf("the gateway answered %d: operation %s %+v, want %s", tc.status, op.State, op.Failure, tc.want)
		}
	}
	if stray := s.gw.strays(); len(stray) > 0 {
		t.Errorf("the callback credential reached %v", stray)
	}
}

// TestRelayCallbackHostIsTheHosts: the host lists the hosts a callback may
// name, and a channel naming another is refused when it is configured, so
// nothing is stored and the callback credential reaches no other host;
// with none listed no channel can name a callback at all.
func TestRelayCallbackHostIsTheHosts(t *testing.T) {
	for name, hosts := range map[string][]string{"another host listed": {"other.example.com"}, "none listed": nil} {
		gw := newRelayGateway(t, relayHost, relayCallback)
		r := newRig(t, "acme", withSet(composition.KeyRelayCallbackHosts, hosts), withComposition(), withCredentials(relayCreds(t)), withHTTP(gw))
		_, err := r.client().Configuration().ConfigureChannel(t.Context(), model.ConfigureChannel{
			Command: r.cmd("channel-imessage"),
			Channel: model.Channel{Ref: model.ChannelRef{Tenant: r.tenant}, Key: "imessage", Type: model.TypeRelayMessages,
				CredentialRef: relayRef, Configuration: relayConfig().Extension()},
		})
		if !model.IsCode(err, model.Validation) {
			t.Errorf("%s: configuring a callback on %s = %v, want a validation refusal", name, relayHost, err)
		}
		if got := gw.total(); got != 0 {
			t.Errorf("%s: %d requests reached the gateway", name, got)
		}
	}
}

// TestRelayReceiveOnlyChannel: a channel with no callback receives and
// sends nothing: a reply fails as unsupported, with no request made.
func TestRelayReceiveOnlyChannel(t *testing.T) {
	cfg := relayConfig(func(c *relay.Config) { c.Callback = nil })
	s := newRelaySite(t, relayCreds(t), cfg, daemon.NameWebhook, daemon.NameWorkers)
	if code, body := s.message(relayPerson, "m-1", "hello"); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	conv := s.conversations()[0]
	acc, err := s.r.client().Messages().Reply(t.Context(), model.Reply{Command: s.r.cmd("reply-1"), Conversation: conv.Ref, Content: desk.TextContent("on it")})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if op := s.settled(acc); op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Unsupported {
		t.Fatalf("a receive-only channel's reply = %s %+v, want failed as unsupported", op.State, op.Failure)
	}
	if n := s.gw.total(); n != 0 {
		t.Errorf("a receive-only channel made %d requests", n)
	}
}

// TestRelayOneReceiverPerGateway: two channels of one medium on one
// gateway would be handed one stream, so the second refuses to start, in
// one profile and across profiles; another medium on the same gateway is
// another account.
func TestRelayOneReceiverPerGateway(t *testing.T) {
	channel := func(key, medium string) model.Channel {
		return model.Channel{Key: key, Type: model.TypeRelayMessages, CredentialRef: relayRef,
			Configuration: relayConfig(func(c *relay.Config) { c.Medium = medium }).Extension()}
	}
	setup := func(tenant model.TenantID, opts ...rigOption) *rig {
		gw := newRelayGateway(t, relayHost, relayCallback)
		return newRig(t, tenant, append([]rigOption{
			withSet(composition.KeyRelayCallbackHosts, []string{relayHost}), withComposition(), withCredentials(relayCreds(t)), withHTTP(gw),
		}, opts...)...)
	}
	r := setup("acme")
	r.channelOf(channel("one", relay.MediumIMessage))
	r.channelOf(channel("two", relay.MediumIMessage))
	res := r.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "receive the same provider account") ||
		!strings.Contains(res.Err.Message, `"one"`) || !strings.Contains(res.Err.Message, `"two"`) {
		t.Fatalf("one profile, two channels on one gateway: %s %v", res.Outcome, res.Err)
	}

	r = setup("acme")
	r.channelOf(channel("imessage", relay.MediumIMessage))
	r.channelOf(channel("sms", relay.MediumSMS))
	s := r.run(5*time.Second, daemon.NameWebhook)
	s.ready(daemon.NameWebhook)
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("two mediums on one gateway: %s %v", res.Outcome, res.Err)
	}

	dir := t.TempDir()
	first, second := setup("first", withAccounts(dir)), setup("second", withAccounts(dir))
	first.channelOf(channel("one", relay.MediumIMessage))
	second.channelOf(channel("two", relay.MediumIMessage))
	s1 := first.run(5*time.Second, daemon.NameWebhook)
	s1.ready(daemon.NameWebhook)
	res = second.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/first (pid ") {
		t.Fatalf("second profile on the first's gateway: %s %v", res.Outcome, res.Err)
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
}

// TestRelayOtherMediums: the medium is the channel's configuration, not
// something heo fixes: SMS and WhatsApp channels receive under their own
// identity namespace, a number kept as sent (an address is not lowercased
// there), and reply and start through the same callback naming the
// medium. Only the iMessage medium has a reference gateway and a recording
// guide.
func TestRelayOtherMediums(t *testing.T) {
	for medium, namespace := range map[string]string{relay.MediumSMS: relay.NamespaceSMS, relay.MediumWhatsApp: relay.NamespaceWhatsApp} {
		s := newRelaySite(t, relayCreds(t), relayConfig(func(c *relay.Config) { c.Medium = medium }), daemon.NameWebhook, daemon.NameWorkers)
		if code, body := s.message("+15550100001", "m-1", "hello"); code != http.StatusOK {
			t.Fatalf("%s inbound = %d %q", medium, code, body)
		}
		convs := s.conversations()
		if len(convs) != 1 || convs[0].ReplyRoute.Namespace != namespace || convs[0].ReplyRoute.ID != "+15550100001" {
			t.Fatalf("%s conversations = %+v, want one on %s", medium, convs, namespace)
		}
		if op := s.settled(s.reply(convs[0].Ref, "reply-"+medium, "on it")); op.State != model.ProviderAccepted {
			t.Fatalf("%s reply = %s %+v", medium, op.State, op.Failure)
		}
		start, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + medium), Inbox: s.inbox,
			Recipient: model.ExternalRef{Channel: s.ch, Namespace: namespace, Kind: relay.RouteKind, ID: "+15550100003"}, Content: desk.TextContent("welcome"),
		})
		if err != nil {
			t.Fatalf("%s start: %v", medium, err)
		}
		if op := s.settled(start); op.State != model.ProviderAccepted {
			t.Fatalf("%s start = %s %+v", medium, op.State, op.Failure)
		}
		sent := s.gw.called("/uma/reply")
		if len(sent) != 2 || sent[0].Body["medium"] != medium || sent[0].Body["to"] != "+15550100001" || sent[1].Body["medium"] != medium || sent[1].Body["to"] != "+15550100003" {
			t.Fatalf("%s callbacks = %+v", medium, sent)
		}
	}
}
