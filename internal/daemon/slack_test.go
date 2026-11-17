package daemon_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/profile"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

// These tests run the slack.chat adapter in heo's runtime: Slack's
// signed requests reach the channel's webhook route on the real
// listener, and what the adapter sends back goes through heo's HTTP
// seam to an in-process Slack (slackfake_test.go). No Slack workspace,
// token or network is involved.

const (
	slackTeam   = "T0000000001"
	slackBot    = "U0000000001"
	slackPerson = "U0000000002"
	slackRoom   = "C0000000001"
	slackSecret = "slack-signing-secret-for-tests" // #nosec G101 -- test value
	slackToken  = "xoxb-test-bot-token"            // #nosec G101 -- test value
	slackRef    = "slack"
)

// slackCreds is heo's file credential backend holding each purpose's own
// entry of reference ref: the bot token and the signing secret are two
// values, so two entries (see "One secret per purpose").
func slackCreds(t *testing.T, tenant model.TenantID, ref string, byPurpose map[string]string) *recordingCreds {
	t.Helper()
	b, err := credentials.Open(t.Context(), credentials.Options{
		Profile: profile.Default(), Tenant: tenant,
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for purpose, value := range byPurpose {
		keys, err := b.Keys(ref, purpose)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(secretEnvelope(value))
		if err != nil {
			t.Fatal(err)
		}
		if err := b.SurfaceTokens().Set(t.Context(), keys[0], data); err != nil {
			t.Fatal(err)
		}
	}
	return &recordingCreds{next: b.Channels()}
}

func fullSlackCreds(t *testing.T) *recordingCreds {
	return slackCreds(t, "acme", slackRef, map[string]string{
		slack.CredentialPurposeAPI: slackToken, slack.CredentialPurposeSigning: slackSecret,
	})
}

// slackSignature is X-Slack-Signature: "v0=" and the hex HMAC-SHA256,
// under the signing secret, of "v0:", the timestamp, ":" and the body.
func slackSignature(secret, stamp, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + stamp + ":" + body))
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

// slackSite is a daemon with one slack.chat channel and a per-room inbox
// bound to the workspace, its webhook up and its provider calls answered
// by the in-process Slack.
type slackSite struct {
	t     *testing.T
	r     *rig
	api   *slackAPI
	creds *recordingCreds
	ch    model.ChannelRef
	inbox model.InboxRef
	route string
	s     *supervised
}

func newSlackSite(t *testing.T, creds *recordingCreds, services ...string) *slackSite {
	t.Helper()
	return newSlackSiteTuned(t, creds, nil, services...)
}

// newSlackSiteTuned is newSlackSite with tune run on the rig before the
// daemon starts, to set the settings under test (the workers' poll).
func newSlackSiteTuned(t *testing.T, creds *recordingCreds, tune func(*rig), services ...string) *slackSite {
	t.Helper()
	api := newSlackAPI(t)
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withHTTP(api))
	if tune != nil {
		tune(r)
	}
	site := &slackSite{t: t, r: r, api: api, creds: creds}
	site.ch = r.channelOf(model.Channel{
		Key: "chat", Type: model.TypeSlackChat, CredentialRef: slackRef,
		Configuration: slack.Config{TeamID: slackTeam, BotUserID: slackBot}.Extension(),
	})
	acc, err := r.client().Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-rooms"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: site.ch, Name: "rooms", Destinations: []string{slackTeam},
		Selection: model.Selection{Mode: model.ModePerRoom},
	}})
	if err != nil {
		t.Fatalf("configure inbox: %v", err)
	}
	if site.inbox, err = acc.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	names := append([]string{daemon.NameWebhook}, services...)
	site.s = r.run(5*time.Second, names...)
	site.s.ready(names...)
	site.route = "http://" + site.s.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix + string(site.ch.ID)
	return site
}

// post sends body to the channel's route as Slack would: signed under
// secret at stamp, with the content type given.
func (s *slackSite) post(contentType, body, secret string, at time.Time) (int, string) {
	s.t.Helper()
	stamp := strconv.FormatInt(at.Unix(), 10)
	return do(s.t, http.MethodPost, s.route, contentType, body,
		"X-Slack-Request-Timestamp", stamp, "X-Slack-Signature", slackSignature(secret, stamp, body))
}

// event posts an Events API delivery signed correctly, now.
func (s *slackSite) event(v map[string]any) (int, string) {
	s.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.post("application/json", string(b), slackSecret, time.Now())
}

func slackMessage(eventID, user, text, ts string, mod ...func(map[string]any)) map[string]any {
	ev := map[string]any{"type": "message", "channel": slackRoom, "channel_type": "channel", "user": user, "text": text, "ts": ts}
	for _, m := range mod {
		m(ev)
	}
	return map[string]any{"type": "event_callback", "team_id": slackTeam, "api_app_id": "A0000000001", "event_id": eventID, "event": ev}
}

func (s *slackSite) conversations() []model.Conversation {
	s.t.Helper()
	p, err := s.r.client().Conversations().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *slackSite) messages(conv model.ConversationRef) []model.Message {
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

func (s *slackSite) interactions() []model.Interaction {
	s.t.Helper()
	p, err := s.r.client().Interactions().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

// TestSlackWebhook: the channel's route is the app's one request URL.
// Slack's url_verification is answered with its challenge only when
// signed, a request signed wrongly, with another secret or too long ago
// is refused 401 and reaches nothing, a message in a room opens the
// room's conversation once however often it is delivered, what the app's
// own bot said and what another workspace said is acknowledged and
// dropped, and a button press and a slash command, signed forms, are
// recorded as interactions and answered 200 with an empty body.
func TestSlackWebhook(t *testing.T) {
	creds := fullSlackCreds(t)
	s := newSlackSite(t, creds)
	challenge := `{"type":"url_verification","challenge":"3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"}`

	// Slack's handshake: the challenge, as plain text, to a signed request.
	if code, body := s.post("application/json", challenge, slackSecret, time.Now()); code != http.StatusOK ||
		strings.TrimSpace(body) != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
		t.Errorf("signed url_verification = %d %q, want 200 and the challenge", code, body)
	}
	now := time.Now()
	stamp := strconv.FormatInt(now.Unix(), 10)
	for _, tc := range []struct {
		name   string
		header []string
		body   string
		secret string
		at     time.Time
	}{
		{name: "another secret", body: challenge, secret: "not-the-signing-secret", at: now},
		{name: "stale", body: challenge, secret: slackSecret, at: now.Add(-10 * time.Minute)},
		{name: "from the future", body: challenge, secret: slackSecret, at: now.Add(10 * time.Minute)},
		{name: "unsigned", header: []string{}, body: challenge},
		{name: "malformed signature", header: []string{"X-Slack-Request-Timestamp", stamp, "X-Slack-Signature", "v0=zz"}, body: challenge},
		{name: "wrong version", header: []string{"X-Slack-Request-Timestamp", stamp, "X-Slack-Signature", "v1=" + strings.Repeat("0", 64)}, body: challenge},
	} {
		var code int
		var body string
		if tc.header != nil || tc.secret == "" {
			code, body = do(t, http.MethodPost, s.route, "application/json", tc.body, tc.header...)
		} else {
			code, body = s.post("application/json", tc.body, tc.secret, tc.at)
		}
		if code != http.StatusUnauthorized || strings.Contains(body, "3eZbrw1a") {
			t.Errorf("%s: %d %q, want 401 and no challenge", tc.name, code, body)
		}
	}
	// A body altered after signing fails too.
	if code, _ := do(t, http.MethodPost, s.route, "application/json", challenge+" ",
		"X-Slack-Request-Timestamp", stamp, "X-Slack-Signature", slackSignature(slackSecret, stamp, challenge)); code != http.StatusUnauthorized {
		t.Errorf("altered body = %d, want 401", code)
	}
	if code, _ := do(t, http.MethodGet, s.route, "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	if code, _ := s.post("text/plain", "hello", slackSecret, time.Now()); code != http.StatusBadRequest {
		t.Errorf("signed text/plain = %d, want 400", code)
	}
	if !creds.asked(slackRef, slack.CredentialPurposeSigning) {
		t.Errorf("the signing secret was never resolved")
	}
	if n := len(s.conversations()); n != 0 {
		t.Fatalf("refused requests opened %d conversations", n)
	}

	// A message in a room: the room's conversation, once.
	msg := slackMessage("Ev1", slackPerson, "is anyone there?", "1760000100.000100")
	for i := 0; i < 2; i++ {
		if code, body := s.event(msg); code != http.StatusOK {
			t.Fatalf("message delivery %d = %d %q", i+1, code, body)
		}
	}
	convs := s.conversations()
	if len(convs) != 1 || convs[0].Mode != model.ModePerRoom || convs[0].ReplyRoute.Kind != model.RefKindRoom || convs[0].ReplyRoute.ID != slackRoom {
		t.Fatalf("conversations after the message = %+v, want one for the room", convs)
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 {
		t.Fatalf("messages = %d, a retried delivery is a duplicate and adds none", len(got))
	}

	// Acknowledged, nothing recorded: the app's own bot, another workspace.
	other := slackMessage("Ev2", slackPerson, "elsewhere", "1760000101.000100")
	other["team_id"] = "T0000000099"
	for name, ev := range map[string]map[string]any{
		"the app's bot":     slackMessage("Ev3", slackBot, "echo", "1760000102.000100"),
		"another workspace": other,
	} {
		if code, body := s.event(ev); code != http.StatusOK {
			t.Errorf("%s = %d %q, want 200", name, code, body)
		}
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 || len(s.conversations()) != 1 {
		t.Errorf("a dropped event was recorded: %d messages, %d conversations", len(got), len(s.conversations()))
	}

	// A button press and a slash command: signed forms, an empty 200.
	press, _ := json.Marshal(map[string]any{
		"type": "block_actions", "api_app_id": "A0000000001", "trigger_id": "398738663015.47445629121.803a0bc887a1",
		"team": map[string]any{"id": slackTeam}, "user": map[string]any{"id": slackPerson, "team_id": slackTeam},
		"container": map[string]any{"type": "message", "message_ts": "1760000100.000100", "channel_id": slackRoom},
		"channel":   map[string]any{"id": slackRoom}, "message": map[string]any{"type": "message", "ts": "1760000100.000100", "user": slackBot},
		"response_url": "https://hooks.slack.com/actions/T0000000001/397700885554/96rGlfmibIGlgcZRskXaIFfN",
		"actions":      []any{map[string]any{"action_id": "approve", "type": "button", "value": "deploy-42", "action_ts": "1760000103.000200"}},
	})
	form := url.Values{"payload": {string(press)}}.Encode()
	if code, body := s.post("application/x-www-form-urlencoded", form, slackSecret, time.Now()); code != http.StatusOK || body != "" {
		t.Errorf("button press = %d %q, want 200 and an empty body", code, body)
	}
	cmd := url.Values{
		"team_id": {slackTeam}, "channel_id": {slackRoom}, "user_id": {slackPerson}, "command": {"/triage"}, "text": {"ticket 7"},
		"api_app_id": {"A0000000001"}, "trigger_id": {"13345224609.738474920.8088930838d88f008e0"},
		"response_url": {"https://hooks.slack.com/commands/T0000000001/1/abc"},
	}.Encode()
	if code, body := s.post("application/x-www-form-urlencoded", cmd, slackSecret, time.Now()); code != http.StatusOK || body != "" {
		t.Errorf("slash command = %d %q, want 200 and an empty body", code, body)
	}
	byAction := map[string]model.Interaction{}
	for _, in := range s.interactions() {
		byAction[in.Action+in.Command] = in
	}
	if len(byAction) != 2 || byAction["approve"].Arguments != "deploy-42" || byAction["approve"].Kind != model.InteractionAction ||
		byAction["triage"].Arguments != "ticket 7" || byAction["triage"].Kind != model.InteractionCommand {
		t.Errorf("interactions = %+v, want the press and the command", byAction)
	}
	// A form not signed is not read.
	if code, _ := do(t, http.MethodPost, s.route, "application/x-www-form-urlencoded", form); code != http.StatusUnauthorized {
		t.Errorf("unsigned form = %d, want 401", code)
	}
	if n := s.api.total(); n != 0 {
		t.Errorf("receiving made %d calls to Slack", n)
	}
}

// TestSlackWebhookWithoutCredentials: with no signing secret stored the
// route answers 503, so Slack retries once it is, and url_verification
// gets no challenge.
func TestSlackWebhookWithoutCredentials(t *testing.T) {
	creds := slackCreds(t, "acme", slackRef, nil)
	s := newSlackSite(t, creds)
	body := `{"type":"url_verification","challenge":"abc123"}`
	code, got := s.post("application/json", body, slackSecret, time.Now())
	if code != http.StatusServiceUnavailable || strings.Contains(got, "abc123") {
		t.Errorf("no credential = %d %q, want 503 and no challenge", code, got)
	}
	if !creds.asked(slackRef, slack.CredentialPurposeSigning) {
		t.Errorf("the signing secret was never asked of heo's backend")
	}
}

// waitOperation waits until the operation reaches one of states.
func (s *slackSite) waitOperation(ref model.OperationRef, states ...model.DeliveryState) model.Operation {
	s.t.Helper()
	var op model.Operation
	waitFor(s.t, func() bool {
		op = s.r.operation(ref)
		for _, st := range states {
			if string(op.State) == string(st) {
				return true
			}
		}
		return false
	})
	return op
}

// byText is the message of conv whose text is text.
func (s *slackSite) byText(conv model.ConversationRef, text string) model.Message {
	s.t.Helper()
	for _, m := range s.messages(conv) {
		if desk.Text(m.Content) == text {
			return m
		}
	}
	s.t.Fatalf("no message %q in %v", text, conv)
	return model.Message{}
}

// providerID is the Slack message ID ("<channel>:<ts>") the adapter
// reported for m, "" until the provider accepted it.
func providerID(m model.Message) string {
	for _, e := range m.External {
		if e.Namespace == model.ProviderMessageNamespace && e.Kind == model.RefKindMessage {
			return e.ID
		}
	}
	return ""
}

// settled waits until the operation is no longer queued or in flight.
func (s *slackSite) settled(acc model.Acceptance) model.Operation {
	s.t.Helper()
	return s.waitOperation(acc.Operation.Ref, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected)
}

// TestSlackProviderVerbs: through heo's workers, the real slack.chat
// adapter and heo's HTTP seam, against Slack in process: a first message
// to a room, a reply, an edit, a reaction added and removed and a
// deletion each reach the Web API as the documented method with the bot
// token as bearer; an edit of someone else's message is refused by Slack
// and ends rejected, never as a new message; interaction responses go to
// the response URL without the bot token, a follow-up too.
func TestSlackProviderVerbs(t *testing.T) {
	creds := fullSlackCreds(t)
	s := newSlackSite(t, creds, daemon.NameWorkers)
	ctx := t.Context()
	c := s.r.client()
	text := func(v string) model.Content { return desk.TextContent(v) }

	// A first message to a room that never wrote in.
	room2 := model.ExternalRef{Channel: s.ch, Namespace: slack.Namespace, Kind: model.RefKindRoom, ID: "C0000000002"}
	acc, err := c.Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start"), Inbox: s.inbox, Recipient: room2, Content: text("welcome to the room")})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if op := s.settled(acc); op.State != model.ProviderAccepted {
		t.Fatalf("start = %s %+v", op.State, op.Failure)
	}
	post := s.api.called("chat.postMessage")
	if len(post) != 1 || post[0].Body["channel"] != "C0000000002" || post[0].Body["text"] != "welcome to the room" ||
		post[0].Auth != "Bearer "+slackToken || post[0].Body["mrkdwn"] != false || post[0].Body["thread_ts"] != nil {
		t.Fatalf("start sent %+v, want one top-level post to the room with the bot token", post)
	}
	if !creds.asked(slackRef, slack.CredentialPurposeAPI) {
		t.Error("the bot token was never asked of heo's backend")
	}

	// Someone writes in the first room; a reply answers the room.
	if code, body := s.event(slackMessage("Ev10", slackPerson, "hello team", "1760000200.000100")); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	var room model.Conversation
	for _, cv := range s.conversations() {
		if cv.ReplyRoute.ID == slackRoom {
			room = cv
		}
	}
	if room.Ref.ID == "" {
		t.Fatalf("no conversation for %s in %+v", slackRoom, s.conversations())
	}
	reply, err := c.Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: room.Ref, Content: text("on it")})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if op := s.settled(reply); op.State != model.ProviderAccepted {
		t.Fatalf("reply = %s %+v", op.State, op.Failure)
	}
	mine := s.byText(room.Ref, "on it")
	id := providerID(mine)
	if id == "" || !strings.HasPrefix(id, slackRoom+":") {
		t.Fatalf("the reply names provider message %q, want <room>:<ts>", id)
	}
	ts := strings.TrimPrefix(id, slackRoom+":")

	// Edit, react, unreact, delete: each its own Web API method, on the
	// message the provider named.
	edited, err := c.Messages().Edit(ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "edit-1", ExpectedRevision: mine.Revision},
		Message: mine.Ref, Content: text("on it, fixed"),
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if op := s.settled(edited); op.State != model.ProviderAccepted {
		t.Fatalf("edit = %s %+v", op.State, op.Failure)
	}
	if up := s.api.called("chat.update"); len(up) != 1 || up[0].Body["channel"] != slackRoom || up[0].Body["ts"] != ts || up[0].Body["text"] != "on it, fixed" {
		t.Fatalf("edit sent %+v", up)
	}
	if got := s.api.posted(id); got == nil || got.Text != "on it, fixed" {
		t.Fatalf("Slack holds %+v after the edit", got)
	}
	react := func(op string, remove bool) {
		t.Helper()
		acc, err := c.Messages().React(ctx, model.Reaction{Command: s.r.cmd(op), Message: mine.Ref, Emoji: "👍", Remove: remove})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if got := s.settled(acc); got.State != model.ProviderAccepted {
			t.Fatalf("%s = %s %+v", op, got.State, got.Failure)
		}
	}
	react("react-add", false)
	if got := s.api.posted(id); len(got.Reactions) != 1 || got.Reactions[0] != "thumbsup" {
		t.Fatalf("reactions after add = %+v, want thumbsup", got)
	}
	react("react-remove", true)
	if got := s.api.posted(id); len(got.Reactions) != 0 {
		t.Fatalf("reactions after remove = %+v", got)
	}
	if add := s.api.called("reactions.add"); len(add) != 1 || add[0].Body["timestamp"] != ts || add[0].Body["name"] != "thumbsup" || add[0].Body["channel"] != slackRoom {
		t.Fatalf("reactions.add sent %+v", add)
	}
	mine = s.byText(room.Ref, "on it, fixed")
	del, err := c.Messages().Delete(ctx, model.DeleteMessage{Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "delete-1", ExpectedRevision: mine.Revision}, Message: mine.Ref})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if op := s.settled(del); op.State != model.ProviderAccepted {
		t.Fatalf("delete = %s %+v", op.State, op.Failure)
	}
	if got := s.api.posted(id); got == nil || !got.Deleted {
		t.Fatalf("Slack holds %+v after the deletion", got)
	}
	for _, m := range []string{"chat.postMessage", "chat.update", "reactions.add", "reactions.remove", "chat.delete"} {
		for _, call := range s.api.called(m) {
			if call.Host != "slack.com" || call.Auth != "Bearer "+slackToken {
				t.Errorf("%s went to %s with %q", m, call.Host, call.Auth)
			}
		}
	}

	// Someone else's message is not the bot's to change: Slack refuses and
	// the edit ends rejected; nothing is posted instead.
	theirs := s.byText(room.Ref, "hello team")
	s.api.failWith("chat.update", "cant_update_message")
	posts := len(s.api.called("chat.postMessage"))
	refused, err := c.Messages().Edit(ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "edit-2", ExpectedRevision: theirs.Revision},
		Message: theirs.Ref, Content: text("rewritten"),
	})
	if err != nil {
		t.Fatalf("edit of another's message: %v", err)
	}
	if op := s.settled(refused); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unauthorized ||
		!strings.Contains(op.Failure.Error(), "cant_update_message") {
		t.Fatalf("edit of another's message = %s %+v, want rejected as unauthorized: cant_update_message", op.State, op.Failure)
	}
	if len(s.api.called("chat.postMessage")) != posts {
		t.Error("a refused edit posted a new message")
	}
	s.api.failWith("chat.update", "")

	// Interactions: a button press is answered at its response URL, as a
	// message and then as a follow-up, and never with the bot token.
	press, _ := json.Marshal(map[string]any{
		"type": "block_actions", "api_app_id": "A0000000001", "trigger_id": "398738663015.47445629121.aaaa",
		"team": map[string]any{"id": slackTeam}, "user": map[string]any{"id": slackPerson, "team_id": slackTeam},
		"container": map[string]any{"type": "message", "message_ts": "1760000200.000100", "channel_id": slackRoom},
		"channel":   map[string]any{"id": slackRoom}, "message": map[string]any{"type": "message", "ts": "1760000200.000100"},
		"response_url": "https://hooks.slack.com/actions/T0000000001/397700885554/96rGlfmibIGlgcZRskXaIFfN",
		"actions":      []any{map[string]any{"action_id": "approve", "type": "button", "value": "go", "action_ts": "1760000201.000200"}},
	})
	if code, body := s.post("application/x-www-form-urlencoded", url.Values{"payload": {string(press)}}.Encode(), slackSecret, time.Now()); code != http.StatusOK || body != "" {
		t.Fatalf("press = %d %q", code, body)
	}
	ins := s.interactions()
	if len(ins) != 1 {
		t.Fatalf("interactions = %+v", ins)
	}
	respond := func(op string, followUp bool, mode string) model.Operation {
		t.Helper()
		aud := model.Audience{Visibility: model.Public}
		if mode == model.ResponseModeEphemeral {
			aud.Visibility = model.Restricted
		}
		acc, err := c.Interactions().Respond(ctx, model.Respond{
			Command: s.r.cmd(op), Interaction: ins[0].Ref, Mode: mode, Audience: aud,
			Content: text("approved " + op), FollowUp: followUp, ExpiresAt: time.Now().Add(10 * time.Minute),
		})
		if err != nil {
			t.Fatalf("respond %s: %v", op, err)
		}
		return s.settled(acc)
	}
	if op := respond("answer", false, model.ResponseModeMessage); op.State != model.ProviderAccepted {
		t.Fatalf("respond = %s %+v", op.State, op.Failure)
	}
	if op := respond("follow-up", true, model.ResponseModeEphemeral); op.State != model.ProviderAccepted {
		t.Fatalf("follow-up = %s %+v", op.State, op.Failure)
	}
	hooks := s.api.called("actions/T0000000001/397700885554/96rGlfmibIGlgcZRskXaIFfN")
	if len(hooks) != 2 || hooks[0].Host != "hooks.slack.com" || hooks[0].Auth != "" || hooks[1].Auth != "" ||
		hooks[0].Body["response_type"] != "in_channel" || hooks[0].Body["text"] != "approved answer" ||
		hooks[1].Body["response_type"] != "ephemeral" || hooks[1].Body["text"] != "approved follow-up" {
		t.Fatalf("response URL got %+v, want an in_channel answer then an ephemeral follow-up, no token", hooks)
	}
}

// TestSlackOneReceiverPerWorkspace: two slack.chat channels on one
// workspace and bot would both be handed what Slack posts to the app, so
// heo refuses to start them, within a profile and across the profiles of
// a host; another bot of the same workspace is another account.
func TestSlackOneReceiverPerWorkspace(t *testing.T) {
	channel := func(key, ref, team, bot string) model.Channel {
		return model.Channel{
			Key: key, Type: model.TypeSlackChat, CredentialRef: ref,
			Configuration: slack.Config{TeamID: team, BotUserID: bot}.Extension(),
		}
	}
	r := newRig(t, "acme", withComposition())
	r.channelOf(channel("one", "slack-1", slackTeam, slackBot))
	r.channelOf(channel("two", "slack-2", slackTeam, slackBot))
	res := r.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "receive the same provider account") ||
		!strings.Contains(res.Err.Message, `"one"`) || !strings.Contains(res.Err.Message, `"two"`) {
		t.Fatalf("one profile, two channels on one install: %s %v", res.Outcome, res.Err)
	}

	ok := newRig(t, "acme", withComposition())
	ok.channelOf(channel("one", "slack-1", slackTeam, slackBot))
	ok.channelOf(channel("two", "slack-2", slackTeam, "U0000000003"))
	ok.run(5*time.Second, daemon.NameWebhook).ready(daemon.NameWebhook)

	dir := t.TempDir()
	first := newRig(t, "first", withComposition(), withAccounts(dir))
	second := newRig(t, "second", withComposition(), withAccounts(dir))
	first.channelOf(channel("one", "slack-1", slackTeam, slackBot))
	second.channelOf(channel("two", "slack-1", slackTeam, slackBot))
	s1 := first.run(5*time.Second, daemon.NameWebhook)
	s1.ready(daemon.NameWebhook)
	res = second.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/first (pid ") {
		t.Fatalf("second profile on the first's install: %s %v", res.Outcome, res.Err)
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
}
