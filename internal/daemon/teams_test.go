package daemon_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/profile"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/model"
)

// These tests run the microsoft.teams adapter in heo's runtime: the
// Bot Connector's signed activities reach the channel's webhook route on
// the real listener, and the keys the adapter verifies them with, and what
// it sends back, go through heo's HTTP seam to an in-process Bot Framework
// (teamsfake_test.go). No Azure Bot, tenant, token or network is involved.

const (
	teamsTenantID = "1111aaaa-1111-4111-8111-11111111bbbb"
	teamsAppID    = "2222cccc-2222-4222-8222-22222222dddd"
	teamsBotID    = "28:" + teamsAppID
	teamsPerson   = "29:alice-person"
	teamsChannel  = "19:channel123@thread.tacv2"
	teamsGroup    = "19:group456@thread.v2"
	teamsToken    = "connector-access-token-for-tests" // #nosec G101 -- test value
	teamsSecret   = "bot-client-secret-for-tests~Zx9"  // #nosec G101 -- test value
	teamsRef      = "teams"
)

// offsetClock is the system clock moved forward by what a test advanced
// it: time still passes, and a token's hour can be skipped.
type offsetClock struct {
	mu  sync.Mutex
	off time.Duration
}

func (c *offsetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.off)
}

func (c *offsetClock) advance(d time.Duration) {
	c.mu.Lock()
	c.off += d
	c.mu.Unlock()
}

func teamsConfig(mod ...func(*teams.Config)) teams.Config {
	cfg := teams.Config{AppID: teamsAppID, TenantID: teamsTenantID, ServiceURL: teamsServiceURL}
	for _, m := range mod {
		m(&cfg)
	}
	return cfg
}

// teamsCreds is heo's file credential backend holding the bot's client
// secret under reference teamsRef, as a secret credential under the purpose
// heo mints the Bot Connector token with; none when secret is empty.
func teamsCreds(t *testing.T, secret string) *recordingCreds {
	t.Helper()
	c, _ := teamsRotatingCreds(t, secret)
	return c
}

// teamsRotatingCreds is teamsCreds with the means to store another secret
// under the same reference while the daemon runs, as an operator who
// rotated the app's secret does.
func teamsRotatingCreds(t *testing.T, secret string) (*recordingCreds, func(string)) {
	t.Helper()
	b, err := credentials.Open(t.Context(), credentials.Options{
		Profile: profile.Default(), Tenant: "acme",
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := b.Keys(teamsRef, composition.PurposeTeamsClientSecret)
	if err != nil {
		t.Fatal(err)
	}
	store := func(secret string) {
		t.Helper()
		data, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": secret})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SurfaceTokens().Set(t.Context(), keys[0], data); err != nil {
			t.Fatal(err)
		}
	}
	if secret != "" {
		store(secret)
	}
	return &recordingCreds{next: b.Channels()}, store
}

// The activities the Bot Connector posts, as the Bot Framework prints them.

func tmActivity(kind, id, from, conv, convType string, mod ...func(map[string]any)) map[string]any {
	a := map[string]any{
		"type": kind, "id": id, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "serviceUrl": teamsServiceURL,
		"channelId": "msteams", "from": map[string]any{"id": from, "name": "Alice", "aadObjectId": "aad-alice"},
		"recipient":    map[string]any{"id": teamsBotID, "name": "heo"},
		"conversation": map[string]any{"id": conv, "conversationType": convType, "tenantId": teamsTenantID},
		"channelData":  map[string]any{"tenant": map[string]any{"id": teamsTenantID}},
	}
	for _, m := range mod {
		m(a)
	}
	return a
}

func tmMessage(id, from, conv, convType, text string, mod ...func(map[string]any)) map[string]any {
	return tmActivity("message", id, from, conv, convType, append([]func(map[string]any){func(a map[string]any) { a["text"] = text }}, mod...)...)
}

// tmPress is the invoke Teams posts for an Action.Execute on the card of
// message card.
func tmPress(id, from, conv, convType, card, verb string, data any, mod ...func(map[string]any)) map[string]any {
	return tmActivity("invoke", id, from, conv, convType, append([]func(map[string]any){func(a map[string]any) {
		a["name"], a["replyToId"] = "adaptiveCard/action", card
		a["value"] = map[string]any{"action": map[string]any{"type": "Action.Execute", "verb": verb, "data": data}, "trigger": "manual"}
	}}, mod...)...)
}

// teamsSite is a daemon with one microsoft.teams channel and an inbox
// bound to the tenant, its provider calls answered by the in-process Bot
// Framework.
type teamsSite struct {
	t     *testing.T
	r     *rig
	bot   *teamsBot
	creds *recordingCreds
	clock *offsetClock
	ch    model.ChannelRef
	inbox model.InboxRef
	route string
	s     *supervised
}

func newTeamsSite(t *testing.T, creds *recordingCreds, services ...string) *teamsSite {
	t.Helper()
	return newTeamsSiteMode(t, creds, model.ModePerRoom, newTeamsBot(t, teamsToken), services...)
}

// newTeamsSiteMode is newTeamsSite with an inbox of selection mode, over bot.
func newTeamsSiteMode(t *testing.T, creds *recordingCreds, mode string, bot *teamsBot, services ...string) *teamsSite {
	t.Helper()
	clock := &offsetClock{}
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withHTTP(bot), func(r *rig) { r.deps.Clock = clock })
	selection := model.Selection{Mode: mode}
	if mode == model.ModePerContact {
		lock := true
		selection.LockToSingle, selection.LifecycleRules = &lock, model.RuleSetContactLifecycleV1
	}
	site := &teamsSite{t: t, r: r, bot: bot, creds: creds, clock: clock}
	site.ch = r.channelOf(model.Channel{
		Key: "teams", Type: model.TypeMicrosoftTeams, CredentialRef: teamsRef, Configuration: teamsConfig().Extension(),
	})
	acc, err := r.client().Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-" + mode), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: site.ch, Name: mode, Destinations: []string{teamsTenantID},
		Selection: selection,
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

// post sends body to the channel's route as the Bot Connector would,
// with the Authorization header given (none when empty).
func (s *teamsSite) post(contentType, body, authorization string) (int, string) {
	s.t.Helper()
	var header []string
	if authorization != "" {
		header = []string{"Authorization", authorization}
	}
	return do(s.t, http.MethodPost, s.route, contentType, body, header...)
}

// activity posts v with a valid token.
func (s *teamsSite) activity(v map[string]any) (int, string) {
	s.t.Helper()
	return s.signed(v, s.bot.teamsToken(teamsAppID))
}

// signed posts v with the bearer token given.
func (s *teamsSite) signed(v map[string]any, token string) (int, string) {
	s.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.post("application/json", string(b), "Bearer "+token)
}

func (s *teamsSite) conversations() []model.Conversation {
	s.t.Helper()
	p, err := s.r.client().Conversations().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *teamsSite) messages(conv model.ConversationRef) []model.Message {
	s.t.Helper()
	p, err := s.r.client().Messages().List(s.t.Context(), model.ListRequest{
		Call:   model.Call{Correlation: "test"},
		Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *teamsSite) interactions() []model.Interaction {
	s.t.Helper()
	p, err := s.r.client().Interactions().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

// byText is the message of conv whose text is text.
func (s *teamsSite) byText(conv model.ConversationRef, text string) model.Message {
	s.t.Helper()
	for _, m := range s.messages(conv) {
		if desk.Text(m.Content) == text {
			return m
		}
	}
	s.t.Fatalf("no message %q in %v", text, conv)
	return model.Message{}
}

// settled waits until the operation is no longer queued or in flight.
func (s *teamsSite) settled(acc model.Acceptance) model.Operation {
	s.t.Helper()
	var op model.Operation
	waitFor(s.t, func() bool {
		op = s.r.operation(acc.Operation.Ref)
		switch op.State {
		case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected:
			return true
		}
		return false
	})
	return op
}

// roomConversation is the conversation of Teams conversation conv, in the
// per-room inbox.
func (s *teamsSite) roomConversation(conv string) model.Conversation {
	s.t.Helper()
	for _, cv := range s.conversations() {
		if cv.ReplyRoute.ID == conv {
			return cv
		}
	}
	s.t.Fatalf("no conversation for %s in %+v", conv, s.conversations())
	return model.Conversation{}
}

// TestTeamsWebhook: the channel's route is the bot's messaging endpoint.
// A request without a Bot Connector token, or whose token fails any one
// check (algorithm, signature, key, audience, issuer, expiry, service URL)
// is refused 401 and reaches nothing; one whose key the Bot Framework does
// not endorse for Teams is refused 403; one whose keys cannot be read gets
// 503, then succeeds once they can. A message in a channel opens its
// conversation once however often it is delivered, what the bot said
// itself and another tenant are acknowledged and dropped, another Bot
// Framework channel is refused 403 (the key is endorsed for Teams only), and an Action.Execute is recorded as an
// interaction and answered with the invoke response. The keys come through
// heo's HTTP client, and receiving makes no Bot Connector call.
func TestTeamsWebhook(t *testing.T) {
	creds := teamsCreds(t, teamsSecret)
	s := newTeamsSite(t, creds, daemon.NameWebhook)
	msg := tmMessage("1760000100000", teamsPerson, teamsChannel, "channel", "is anyone there?")
	body, _ := json.Marshal(msg)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claim := func(k string, v any) func(h, c map[string]any) { return func(_, c map[string]any) { c[k] = v } }
	header := func(k string, v any) func(h, c map[string]any) { return func(h, _ map[string]any) { h[k] = v } }
	for name, tc := range map[string]struct{ auth string }{
		"no authorization":   {},
		"not a bearer":       {auth: "Basic " + s.bot.teamsToken(teamsAppID)},
		"garbage":            {auth: "Bearer not.a.token"},
		"another audience":   {auth: "Bearer " + s.bot.teamsToken("33333333-3333-4333-8333-333333333333")},
		"another issuer":     {auth: "Bearer " + s.bot.teamsToken(teamsAppID, claim("iss", "https://sts.windows.net/"+teamsTenantID+"/"))},
		"expired":            {auth: "Bearer " + s.bot.teamsToken(teamsAppID, claim("exp", now.Add(-10*time.Minute).Unix()))},
		"no expiry":          {auth: "Bearer " + s.bot.teamsToken(teamsAppID, claim("exp", nil))},
		"not yet valid":      {auth: "Bearer " + s.bot.teamsToken(teamsAppID, claim("nbf", now.Add(10*time.Minute).Unix()), claim("exp", now.Add(time.Hour).Unix()))},
		"another service":    {auth: "Bearer " + s.bot.teamsToken(teamsAppID, claim("serviceurl", "https://smba.elsewhere.example/teams/"))},
		"no service":         {auth: "Bearer " + s.bot.teamsToken(teamsAppID, claim("serviceurl", ""))},
		"unknown key":        {auth: "Bearer " + s.bot.teamsToken(teamsAppID, header("kid", "kid-invented"))},
		"no key":             {auth: "Bearer " + s.bot.teamsToken(teamsAppID, header("kid", ""))},
		"unsigned (none)":    {auth: "Bearer " + s.bot.teamsToken(teamsAppID, header("alg", "none"))},
		"another algorithm":  {auth: "Bearer " + s.bot.teamsToken(teamsAppID, header("alg", "HS256"))},
		"another key signed": {auth: "Bearer " + teamsSign(other, teamsAppID, teamsKid)},
	} {
		if code, _ := s.post("application/json", string(body), tc.auth); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
	if code, _ := do(t, http.MethodGet, s.route, "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	if code, _ := s.post("application/json", "{not json", "Bearer "+s.bot.teamsToken(teamsAppID)); code != http.StatusBadRequest {
		t.Errorf("malformed activity = %d, want 400", code)
	}
	if n := len(s.conversations()); n != 0 {
		t.Fatalf("refused requests opened %d conversations", n)
	}
	if meta, keys := s.bot.keyFetchCount(); meta == 0 || keys == 0 {
		t.Errorf("the signing keys were never read through heo's HTTP client: %d metadata, %d key set reads", meta, keys)
	}

	// A message in a channel: the channel's conversation, once.
	for i := 0; i < 2; i++ {
		if code, body := s.activity(msg); code != http.StatusOK {
			t.Fatalf("message delivery %d = %d %q", i+1, code, body)
		}
	}
	convs := s.conversations()
	if len(convs) != 1 || convs[0].Mode != model.ModePerRoom || convs[0].ReplyRoute.Kind != model.RefKindRoom || convs[0].ReplyRoute.ID != teamsChannel {
		t.Fatalf("conversations after the message = %+v, want one for the channel", convs)
	}
	got := s.messages(convs[0].Ref)
	if len(got) != 1 || providerID(got[0]) != teamsChannel+"|1760000100000" {
		t.Fatalf("messages = %+v, a retried delivery is a duplicate and adds none; the message is named <conversation>|<activity>", got)
	}

	// Acknowledged, nothing recorded: the bot's own message, another
	// tenant, another Bot Framework channel.
	elsewhere := func(a map[string]any) {
		a["conversation"].(map[string]any)["tenantId"] = "99999999-9999-4999-8999-999999999999"
		a["channelData"].(map[string]any)["tenant"] = map[string]any{"id": "99999999-9999-4999-8999-999999999999"}
	}
	for name, a := range map[string]map[string]any{
		"the bot's own message": tmMessage("1760000100001", teamsBotID, teamsChannel, "channel", "echo"),
		"another tenant":        tmMessage("1760000100002", teamsPerson, teamsChannel, "channel", "elsewhere", elsewhere),
	} {
		if code, body := s.activity(a); code != http.StatusOK {
			t.Errorf("%s = %d %q, want 200", name, code, body)
		}
	}
	// Another Bot Framework channel: the key is endorsed for Teams only.
	if code, _ := s.activity(tmMessage("1760000100003", teamsPerson, teamsChannel, "channel", "skype", func(a map[string]any) { a["channelId"] = "skype" })); code != http.StatusForbidden {
		t.Errorf("an activity of another Bot Framework channel = %d, want 403", code)
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 || len(s.conversations()) != 1 {
		t.Errorf("dropped activities were recorded: %d messages, %d conversations", len(got), len(s.conversations()))
	}

	// An Action.Execute on a card: the invoke response, one interaction.
	press := tmPress("1760000100004", teamsPerson, teamsChannel, "channel", "1760000099000", "approve", map[string]any{"ticket": 42})
	for i := 0; i < 2; i++ {
		code, resp := s.activity(press)
		if code != http.StatusOK || !strings.Contains(resp, `"statusCode":200`) || !strings.Contains(resp, "application/vnd.microsoft.activity.message") {
			t.Fatalf("Action.Execute delivery %d = %d %q, want 200 and the invoke response", i+1, code, resp)
		}
	}
	ins := s.interactions()
	if len(ins) != 1 || ins[0].Kind != model.InteractionAction || ins[0].Action != "approve" || !strings.Contains(ins[0].Arguments, `"ticket":42`) {
		t.Fatalf("interactions = %+v, want one approve action, however often Teams retries it", ins)
	}
	if code, _ := s.activity(tmActivity("invoke", "1760000100005", teamsPerson, teamsChannel, "channel", func(a map[string]any) {
		a["name"], a["value"] = "task/fetch", map[string]any{}
	})); code != http.StatusNotImplemented {
		t.Errorf("a task/fetch invoke = %d, want 501", code)
	}
	if len(s.interactions()) != 1 {
		t.Error("a dialog invoke was recorded as an interaction")
	}
	if n := s.bot.total(); n != 0 {
		t.Errorf("receiving made %d Bot Connector calls", n)
	}
	if creds.asked(teamsRef, composition.PurposeTeamsClientSecret) || s.bot.mintCount() != 0 {
		t.Error("receiving resolved the bot's client secret or minted a token: inbound verification needs neither")
	}
}

// TestTeamsWebhookKeys: the Bot Framework's keys come through heo's HTTP
// client. A key set that cannot be read answers 503, so the Bot Connector
// retries, and the request succeeds once it can be; a key not endorsed
// for Teams is refused 403, with nothing recorded.
func TestTeamsWebhookKeys(t *testing.T) {
	s := newTeamsSite(t, teamsCreds(t, teamsSecret), daemon.NameWebhook)
	msg := tmMessage("1760000200000", teamsPerson, teamsChannel, "channel", "hello")
	s.bot.setKeysDown(true)
	if code, _ := s.activity(msg); code != http.StatusServiceUnavailable {
		t.Errorf("keys unreadable = %d, want 503", code)
	}
	s.bot.setKeysDown(false)
	if code, body := s.activity(msg); code != http.StatusOK {
		t.Errorf("keys readable again = %d %q, want 200", code, body)
	}
	if n := len(s.conversations()); n != 1 {
		t.Errorf("conversations = %d, want the one the retried delivery opened", n)
	}

	bot := newTeamsBot(t, teamsToken)
	bot.setEndorsements("skype")
	s = newTeamsSiteMode(t, teamsCreds(t, teamsSecret), model.ModePerRoom, bot, daemon.NameWebhook)
	if code, _ := s.activity(msg); code != http.StatusForbidden {
		t.Errorf("key endorsed for another channel = %d, want 403", code)
	}
	if n := len(s.conversations()); n != 0 {
		t.Errorf("an unendorsed key opened %d conversations", n)
	}
}
