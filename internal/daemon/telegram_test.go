package daemon_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/model"
)

// These tests run the telegram.messages adapter in heo's runtime: the
// updates Telegram sends reach the channel's webhook route on the real
// listener, or are read by heo's receiver from an in-process Bot API
// (telegramfake_test.go), and what the adapter sends back goes through
// heo's HTTP seam to the same Bot API. No bot, token or network is
// involved.

const (
	telegramBotID    int64 = 7000000001
	telegramUsername       = "heo_test_bot"
	telegramPerson   int64 = 5000000002
	telegramRoom     int64 = -1001234567890
	telegramToken          = "7000000001:AAHtest-token-for-tests" // #nosec G101 -- test value
	telegramSecret         = "telegram-webhook-secret-0123456789" // #nosec G101 -- test value
	telegramRef            = "tg"
	telegramHeader         = "X-Telegram-Bot-Api-Secret-Token"
)

func telegramConfig(mod ...func(*telegram.Config)) telegram.Config {
	cfg := telegram.Config{BotID: telegramBotID, BotUsername: telegramUsername, PollTimeoutSeconds: 1}
	for _, m := range mod {
		m(&cfg)
	}
	return cfg
}

func fullTelegramCreds(t *testing.T) *recordingCreds {
	return slackCreds(t, "acme", telegramRef, map[string]string{
		telegram.CredentialPurposeAPI: telegramToken, telegram.CredentialPurposeWebhook: telegramSecret,
	})
}

// The updates Telegram sends, as the Bot API prints them.

func tgUser(id int64) map[string]any {
	return map[string]any{"id": id, "is_bot": id == telegramBotID, "first_name": "Test"}
}

func tgChat(id int64) map[string]any { return map[string]any{"id": id, "type": chatType(id)} }

func tgMessage(update, chat, id, from int64, text string, mod ...func(map[string]any)) map[string]any {
	m := map[string]any{"message_id": id, "from": tgUser(from), "chat": tgChat(chat), "date": time.Now().Unix(), "text": text}
	for _, f := range mod {
		f(m)
	}
	return map[string]any{"update_id": update, "message": m}
}

// tgCommand is a message that starts with a bot command for the bot.
func tgCommand(update, chat, id, from int64, command, args string) map[string]any {
	text := "/" + command + "@" + telegramUsername
	if args != "" {
		text += " " + args
	}
	return tgMessage(update, chat, id, from, text, func(m map[string]any) {
		m["entities"] = []any{map[string]any{"type": "bot_command", "offset": 0, "length": len(command) + len(telegramUsername) + 2}}
	})
}

func tgPress(update, chat, msg, from int64, id, data string) map[string]any {
	return map[string]any{"update_id": update, "callback_query": map[string]any{
		"id": id, "from": tgUser(from), "chat_instance": "ci-1", "data": data,
		"message": map[string]any{"message_id": msg, "chat": tgChat(chat), "date": time.Now().Unix(), "text": "choose", "from": tgUser(telegramBotID)},
	}}
}

// telegramSite is a daemon with one telegram.messages channel and a
// per-room inbox bound to the bot, its provider calls answered by the
// in-process Bot API.
type telegramSite struct {
	t     *testing.T
	r     *rig
	api   *telegramAPI
	creds *recordingCreds
	ch    model.ChannelRef
	inbox model.InboxRef
	route string
	s     *supervised
}

func newTelegramSite(t *testing.T, creds *recordingCreds, cfg telegram.Config, services ...string) *telegramSite {
	t.Helper()
	return newTelegramSiteMode(t, creds, cfg, model.ModePerRoom, services...)
}

// newTelegramSiteMode is newTelegramSite with an inbox of selection mode.
func newTelegramSiteMode(t *testing.T, creds *recordingCreds, cfg telegram.Config, mode string, services ...string) *telegramSite {
	t.Helper()
	return newTelegramSiteBefore(t, creds, cfg, mode, nil, services...)
}

// newTelegramSiteBefore is newTelegramSiteMode with the bot's API set up
// by before ahead of the daemon's start: a receiver connects as the
// services start, and reads once ready.
func newTelegramSiteBefore(t *testing.T, creds *recordingCreds, cfg telegram.Config, mode string, before func(*telegramAPI), services ...string) *telegramSite {
	t.Helper()
	api := newTelegramAPI(t, telegramToken, telegramBotID)
	if before != nil {
		before(api)
	}
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withHTTP(api))
	selection := model.Selection{Mode: mode}
	if mode == model.ModePerContact {
		lock := true
		selection.LockToSingle, selection.LifecycleRules = &lock, model.RuleSetContactLifecycleV1
	}
	site := &telegramSite{t: t, r: r, api: api, creds: creds}
	site.ch = r.channelOf(model.Channel{
		Key: "bot", Type: model.TypeTelegramMessages, CredentialRef: telegramRef, Configuration: cfg.Extension(),
	})
	acc, err := r.client().Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-rooms"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: site.ch, Name: "rooms", Destinations: []string{strconv.FormatInt(telegramBotID, 10)},
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

// post sends update to the channel's route as Telegram would, with the
// secret token header given (none when empty).
func (s *telegramSite) post(contentType string, body, secret string) (int, string) {
	s.t.Helper()
	var header []string
	if secret != "" {
		header = []string{telegramHeader, secret}
	}
	return do(s.t, http.MethodPost, s.route, contentType, body, header...)
}

// update posts v with the right secret token.
func (s *telegramSite) update(v map[string]any) (int, string) {
	s.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.post("application/json", string(b), telegramSecret)
}

func (s *telegramSite) conversations() []model.Conversation {
	s.t.Helper()
	p, err := s.r.client().Conversations().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *telegramSite) messages(conv model.ConversationRef) []model.Message {
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

func (s *telegramSite) interactions() []model.Interaction {
	s.t.Helper()
	p, err := s.r.client().Interactions().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

// byText is the message of conv whose text is text.
func (s *telegramSite) byText(conv model.ConversationRef, text string) model.Message {
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
func (s *telegramSite) settled(acc model.Acceptance) model.Operation {
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

// roomConversation is the conversation of chat, in the per-room inbox.
func (s *telegramSite) roomConversation(chat int64) model.Conversation {
	s.t.Helper()
	for _, cv := range s.conversations() {
		if cv.ReplyRoute.ID == strconv.FormatInt(chat, 10) {
			return cv
		}
	}
	s.t.Fatalf("no conversation for chat %d in %+v", chat, s.conversations())
	return model.Conversation{}
}

// TestTelegramWebhook: the channel's route is the bot's webhook URL.
// A request with no secret token or another one is
// refused 401 and reaches nothing; so is one whose stored secret is too
// short to prove anything. A message in a chat opens the chat's
// conversation once however often it is delivered, what the bot said
// itself is acknowledged and dropped, and a button press and a command
// are recorded as interactions and answered 200 with an empty body.
func TestTelegramWebhook(t *testing.T) {
	creds := fullTelegramCreds(t)
	s := newTelegramSite(t, creds, telegramConfig(), daemon.NameWebhook)
	good := func(n int64) string {
		b, _ := json.Marshal(tgMessage(n, telegramRoom, n, telegramPerson, "is anyone there?"))
		return string(b)
	}
	for name, c := range map[string]struct {
		secret string
	}{
		"another secret":    {secret: "not-the-webhook-secret-at-all"},
		"a prefix":          {secret: telegramSecret[:20]},
		"the secret + more": {secret: telegramSecret + "x"},
		"no secret":         {},
	} {
		code, _ := s.post("application/json", good(1), c.secret)
		if code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
	if code, _ := do(t, http.MethodGet, s.route, "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	if code, _ := s.post("text/plain", good(1), telegramSecret); code != http.StatusBadRequest {
		t.Errorf("text/plain = %d, want 400", code)
	}
	if code, _ := s.post("application/json", "{not json", telegramSecret); code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", code)
	}
	if !creds.asked(telegramRef, telegram.CredentialPurposeWebhook) {
		t.Error("the webhook secret token was never resolved")
	}
	if n := len(s.conversations()); n != 0 {
		t.Fatalf("refused requests opened %d conversations", n)
	}

	// A message in a chat: the chat's conversation, once.
	for i := 0; i < 2; i++ {
		if code, body := s.update(tgMessage(10, telegramRoom, 100, telegramPerson, "is anyone there?")); code != http.StatusOK {
			t.Fatalf("message delivery %d = %d %q", i+1, code, body)
		}
	}
	convs := s.conversations()
	if len(convs) != 1 || convs[0].Mode != model.ModePerRoom || convs[0].ReplyRoute.Kind != model.RefKindRoom ||
		convs[0].ReplyRoute.ID != strconv.FormatInt(telegramRoom, 10) {
		t.Fatalf("conversations after the message = %+v, want one for the chat", convs)
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 {
		t.Fatalf("messages = %d, a repeated delivery is a duplicate and adds none", len(got))
	}
	// Acknowledged, nothing recorded: what the bot said itself.
	if code, body := s.update(tgMessage(11, telegramRoom, 101, telegramBotID, "echo")); code != http.StatusOK {
		t.Errorf("the bot's own message = %d %q, want 200", code, body)
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 || len(s.conversations()) != 1 {
		t.Errorf("the bot's own message was recorded: %d messages, %d conversations", len(got), len(s.conversations()))
	}

	// A button press and a command: an empty 200, two interactions.
	if code, body := s.update(tgPress(12, telegramRoom, 100, telegramPerson, "cb-1", "approve:42")); code != http.StatusOK || body != "" {
		t.Errorf("button press = %d %q, want 200 and an empty body", code, body)
	}
	if code, body := s.update(tgCommand(13, telegramRoom, 102, telegramPerson, "triage", "ticket 7")); code != http.StatusOK || body != "" {
		t.Errorf("command = %d %q, want 200 and an empty body", code, body)
	}
	byAction := map[string]model.Interaction{}
	for _, in := range s.interactions() {
		byAction[in.Action+in.Command] = in
	}
	if len(byAction) != 2 || byAction["approve:42"].Kind != model.InteractionAction ||
		byAction["triage"].Arguments != "ticket 7" || byAction["triage"].Kind != model.InteractionCommand {
		t.Errorf("interactions = %+v, want the press and the command", byAction)
	}
	if got := s.messages(convs[0].Ref); len(got) != 1 {
		t.Errorf("a command became a message: %d messages", len(got))
	}
	if n := s.api.total(); n != 0 {
		t.Errorf("receiving by webhook made %d calls to Telegram", n)
	}
}

// TestTelegramWebhookCredentials: with no webhook secret token stored the
// route answers 503, so Telegram retries once it is; one shorter than the
// adapter takes as proof is refused whatever the request carries.
func TestTelegramWebhookCredentials(t *testing.T) {
	body, _ := json.Marshal(tgMessage(1, telegramRoom, 1, telegramPerson, "hi"))
	none := slackCreds(t, "acme", telegramRef, nil)
	s := newTelegramSite(t, none, telegramConfig(), daemon.NameWebhook)
	if code, _ := s.post("application/json", string(body), telegramSecret); code != http.StatusServiceUnavailable {
		t.Errorf("no secret stored = %d, want 503", code)
	}
	if !none.asked(telegramRef, telegram.CredentialPurposeWebhook) {
		t.Error("the secret token was never asked of heo's backend")
	}

	short := slackCreds(t, "acme", telegramRef, map[string]string{telegram.CredentialPurposeWebhook: "short"})
	s = newTelegramSite(t, short, telegramConfig(), daemon.NameWebhook)
	if code, _ := s.post("application/json", string(body), "short"); code == http.StatusOK {
		t.Errorf("a stored secret of 5 bytes = %d, want a refusal", code)
	}
	if n := len(s.conversations()); n != 0 {
		t.Errorf("a short secret admitted %d conversations", n)
	}
}
