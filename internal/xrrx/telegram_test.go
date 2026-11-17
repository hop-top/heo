package xrrx

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	xrr "hop.top/xrr"
)

// The "real" bot and chat of these tests. None of it is an actual bot.
const (
	tgToken    = "123456789:AAEhBP0av28hBzfzuhw2Xv6Y1Gj3vZ_made_up" //nolint:gosec // G101: a made-up value, not a credential
	tgSecret   = "AAEhBP0av28hBzfzuhw2Xv6Y1Gj3vZ_made_up"           //nolint:gosec // G101: half of the made-up token
	tgBot      = int64(123456789)
	tgUsername = "Acme_Support_Bot"
	tgChat     = int64(-1001234567890)
	tgPerson   = int64(987654321)
)

// fakeTelegram is the network under the sandbox transport: it records
// every request it is sent and answers from a table.
type fakeTelegram struct {
	mu     sync.Mutex
	seen   []string
	paths  []string
	bodies []string
	status int
	answer map[string]string
	header http.Header
}

func (f *fakeTelegram) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Host+"/"+method)
	f.paths = append(f.paths, r.URL.Path)
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	answer, ok := f.answer[method]
	if !ok {
		answer = tgSent
	}
	h := f.header
	if h == nil {
		h = http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"b=1"}, "Server": {"nginx"}}
	}
	return &http.Response{StatusCode: status, Header: h.Clone(), Body: io.NopCloser(strings.NewReader(answer)), Request: r}, nil
}

func (f *fakeTelegram) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.seen...)
}

// tgSent is what sendMessage answers, with what the answer must lose: the
// bot's name and username, the group's title.
const tgSent = `{"ok":true,"result":{"message_id":42,"from":{"id":123456789,"is_bot":true,"first_name":"Acme Support","username":"Acme_Support_Bot"},` +
	`"chat":{"id":-1001234567890,"title":"Acme staff room","type":"supergroup"},"date":1760000000,"text":"heo adapter test 1"}}`

func newTelegram(t *testing.T, max int) *TelegramSandbox {
	t.Helper()
	return &TelegramSandbox{
		Token: tgToken, BotID: tgBot, BotUsername: tgUsername, ChatID: tgChat, Max: max,
		Ledger: filepath.Join(t.TempDir(), "ledger"),
	}
}

// telegramDo calls method through rt as the recorded process would: the
// stand-in token, the placeholder chat.
func telegramDo(t *testing.T, rt http.RoundTripper, method string, args map[string]any) (*http.Response, string, error) {
	t.Helper()
	return telegramDoAs(t, rt, "https://"+telegramHost+"/bot"+TelegramStandInToken+"/"+method, args)
}

func telegramDoAs(t *testing.T, rt http.RoundTripper, rawURL string, args map[string]any) (*http.Response, string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out), nil
}

func tgSend(text string) map[string]any {
	return map[string]any{"chat_id": TelegramChatID, "text": text}
}

// TestTelegramSandboxSendsUnderRealNamesAndKeepsNoneOfThem: a send that
// names the placeholder chat reaches Telegram under the real one with the
// real token in the path, and the answer comes back with the bot and the
// chat replaced, every name dropped and the headers that churn removed.
func TestTelegramSandboxSendsUnderRealNamesAndKeepsNoneOfThem(t *testing.T) {
	f := &fakeTelegram{}
	s := newTelegram(t, 3)
	resp, body, err := telegramDo(t, s.Transport(f), "sendMessage", tgSend("heo adapter test 1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "POST api.telegram.org/sendMessage" {
		t.Fatalf("Telegram was sent %v", got)
	}
	if f.paths[0] != "/bot"+tgToken+"/sendMessage" {
		t.Errorf("Telegram saw path %q, want the real token", "<redacted>")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil || sent["chat_id"] != float64(tgChat) || sent["text"] != "heo adapter test 1" {
		t.Errorf("Telegram was sent %v, want the real chat and the text", f.bodies[0])
	}
	all := body
	for k, vs := range resp.Header {
		all += "\n" + k + ": " + strings.Join(vs, ", ")
	}
	for _, real := range []string{tgToken, tgSecret, "123456789", "1001234567890", "Acme", "acme_support", "staff room", "nginx", "b=1"} {
		if strings.Contains(strings.ToLower(all), strings.ToLower(real)) {
			t.Errorf("the answer still holds %q:\n%s", real, all)
		}
	}
	for _, want := range []string{
		`"id":` + strconv.FormatInt(TelegramChatID, 10), `"id":` + strconv.FormatInt(TelegramBotID, 10),
		`"username":"` + TelegramBotUsername + `"`, `"message_id":42`, `"date":1760000000`, `"text":"heo adapter test 1"`, `"type":"supergroup"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the answer lacks %q:\n%s", want, all)
		}
	}
	if resp.Header.Get("Content-Type") == "" || resp.Header.Get("Server") != "" {
		t.Errorf("headers = %v, want only the content type", resp.Header)
	}
	if n := s.Sent(); n != 1 {
		t.Errorf("the ledger holds %d sends, want 1", n)
	}
	if !s.knownMessage(42) {
		t.Error("the message the answer named is not remembered as the recording's own")
	}
}

// TestTelegramSandboxRefusesWhatARecordingMayNotDo: every request outside
// the recording's narrow path fails before it is sent, takes no slot of
// the ledger and reaches nothing.
func TestTelegramSandboxRefusesWhatARecordingMayNotDo(t *testing.T) {
	f := &fakeTelegram{}
	s := newTelegram(t, 3)
	s.noteMessage(0, 42)
	rt := s.Transport(f)
	good := func(mod func(map[string]any)) map[string]any {
		a := tgSend("heo adapter test 1")
		if mod != nil {
			mod(a)
		}
		return a
	}
	msg := func(extra map[string]any) map[string]any {
		a := map[string]any{"chat_id": TelegramChatID, "message_id": 42}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	for name, tc := range map[string]struct {
		method string
		args   map[string]any
		url    string
	}{
		"another chat":        {method: "sendMessage", args: good(func(a map[string]any) { a["chat_id"] = int64(-1009999999999) })},
		"the real chat":       {method: "sendMessage", args: good(func(a map[string]any) { a["chat_id"] = tgChat })},
		"a user's chat":       {method: "sendMessage", args: good(func(a map[string]any) { a["chat_id"] = tgPerson })},
		"a username":          {method: "sendMessage", args: good(func(a map[string]any) { a["chat_id"] = "@acme_channel" })},
		"no chat":             {method: "sendMessage", args: map[string]any{"text": "heo adapter test 1"}},
		"another text":        {method: "sendMessage", args: tgSend("hello everyone")},
		"a link":              {method: "sendMessage", args: tgSend("heo adapter test https://example.com")},
		"a mention":           {method: "sendMessage", args: tgSend("heo adapter test @everyone")},
		"a newline":           {method: "sendMessage", args: tgSend("heo adapter test\nhi")},
		"a long text":         {method: "sendMessage", args: tgSend("heo adapter test " + strings.Repeat("x", 80))},
		"a parse mode":        {method: "sendMessage", args: good(func(a map[string]any) { a["parse_mode"] = "HTML" })},
		"a keyboard":          {method: "sendMessage", args: good(func(a map[string]any) { a["reply_markup"] = map[string]any{} })},
		"a topic":             {method: "sendMessage", args: good(func(a map[string]any) { a["message_thread_id"] = 7 })},
		"a stranger reply":    {method: "sendMessage", args: good(func(a map[string]any) { a["reply_parameters"] = map[string]any{"message_id": 7} })},
		"a reply to none":     {method: "sendMessage", args: good(func(a map[string]any) { a["reply_parameters"] = map[string]any{"allow_sending_without_reply": true} })},
		"a reply's chat":      {method: "sendMessage", args: good(func(a map[string]any) { a["reply_parameters"] = map[string]any{"message_id": 42, "chat_id": 5} })},
		"an unknown message":  {method: "editMessageText", args: msg(map[string]any{"message_id": 7, "text": "heo adapter test 2"})},
		"a delete of one":     {method: "deleteMessage", args: msg(map[string]any{"message_id": 7})},
		"a message of zero":   {method: "deleteMessage", args: msg(map[string]any{"message_id": 0})},
		"an edit's text":      {method: "editMessageText", args: msg(map[string]any{"text": "rewritten"})},
		"an edit's keyboard":  {method: "editMessageText", args: msg(map[string]any{"text": "heo adapter test 2", "reply_markup": map[string]any{}})},
		"two reactions":       {method: "setMessageReaction", args: msg(map[string]any{"reaction": []any{map[string]any{"type": "emoji", "emoji": "👍"}, map[string]any{"type": "emoji", "emoji": "🔥"}}})},
		"a custom reaction":   {method: "setMessageReaction", args: msg(map[string]any{"reaction": []any{map[string]any{"type": "custom_emoji", "custom_emoji_id": "1"}}})},
		"a big reaction":      {method: "setMessageReaction", args: msg(map[string]any{"reaction": []any{}, "is_big": true})},
		"reading the queue":   {method: "getUpdates", args: map[string]any{"limit": 1}},
		"the bot":             {method: "getMe", args: map[string]any{}},
		"a webhook":           {method: "setWebhook", args: map[string]any{"url": "https://example.com/x"}},
		"deleting the hook":   {method: "deleteWebhook", args: map[string]any{}},
		"answering a query":   {method: "answerCallbackQuery", args: map[string]any{"callback_query_id": "1"}},
		"a document":          {method: "sendDocument", args: map[string]any{"chat_id": TelegramChatID}},
		"leaving the chat":    {method: "leaveChat", args: map[string]any{"chat_id": TelegramChatID}},
		"banning someone":     {method: "banChatMember", args: map[string]any{"chat_id": TelegramChatID, "user_id": 5}},
		"another host":        {url: "https://evil.example/bot" + TelegramStandInToken + "/sendMessage", args: tgSend("heo adapter test 1")},
		"a query":             {url: "https://api.telegram.org/bot" + TelegramStandInToken + "/sendMessage?x=1", args: tgSend("heo adapter test 1")},
		"plain http":          {url: "http://api.telegram.org/bot" + TelegramStandInToken + "/sendMessage", args: tgSend("heo adapter test 1")},
		"a traversal":         {url: "https://api.telegram.org/bot" + TelegramStandInToken + "/../sendMessage", args: tgSend("heo adapter test 1")},
		"a file path":         {url: "https://api.telegram.org/file/bot" + TelegramStandInToken + "/x", args: map[string]any{}},
		"a real token":        {url: "https://api.telegram.org/bot" + tgToken + "/sendMessage", args: tgSend("heo adapter test 1")},
		"no token":            {url: "https://api.telegram.org/sendMessage", args: tgSend("heo adapter test 1")},
		"a nested method":     {url: "https://api.telegram.org/bot" + TelegramStandInToken + "/a/sendMessage", args: tgSend("heo adapter test 1")},
		"a percent-encoding":  {url: "https://api.telegram.org/bot" + TelegramStandInToken + "/send%4Dessage", args: tgSend("heo adapter test 1")},
		"a different bot":     {url: "https://api.telegram.org/bot7000000002:AAHheo_adapter_test_stand_in_token/sendMessage", args: tgSend("heo adapter test 1")},
		"userinfo in the URL": {url: "https://u:p@api.telegram.org/bot" + TelegramStandInToken + "/sendMessage", args: tgSend("heo adapter test 1")},
	} {
		u := tc.url
		if u == "" {
			u = "https://" + telegramHost + "/bot" + TelegramStandInToken + "/" + tc.method
		}
		if _, _, err := telegramDoAs(t, rt, u, tc.args); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("refused requests reached Telegram: %v", got)
	}
	if n := s.Sent(); n != 0 {
		t.Errorf("refused requests took %d slots", n)
	}
	// GET is refused too, and a body that is not JSON.
	req, _ := http.NewRequest(http.MethodGet, "https://"+telegramHost+"/bot"+TelegramStandInToken+"/sendMessage", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Error("GET was sent")
	}
	form, _ := http.NewRequest(http.MethodPost, "https://"+telegramHost+"/bot"+TelegramStandInToken+"/sendMessage", strings.NewReader("chat_id=1&text=heo+adapter+test"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := rt.RoundTrip(form); err == nil {
		t.Error("a form was sent")
	}
	// What is allowed goes through, on the message the recording sent.
	for name, tc := range map[string]struct {
		method string
		args   map[string]any
	}{
		"an edit":    {"editMessageText", msg(map[string]any{"text": "heo adapter test 1b"})},
		"a reaction": {"setMessageReaction", msg(map[string]any{"reaction": []any{map[string]any{"type": "emoji", "emoji": "👍"}}})},
		"a clearing": {"setMessageReaction", msg(map[string]any{"reaction": []any{}})},
		"a deletion": {"deleteMessage", msg(nil)},
		"a reply": {"sendMessage", good(func(a map[string]any) {
			a["reply_parameters"] = map[string]any{"message_id": 42, "allow_sending_without_reply": true}
		})},
		"a plain one": {"sendMessage", tgSend("heo adapter test 2")},
	} {
		if _, _, err := telegramDo(t, rt, tc.method, tc.args); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s.Sent() != 2 {
		t.Errorf("ledger holds %d sends, want the two sends", s.Sent())
	}
}

// TestTelegramSandboxRefusesAFourthSend: the cap is the ledger's, and it
// outlives the process: a new sandbox on the same ledger has the sends
// already made counted. A send whose answer failed counts, and a refused
// send reaches nothing.
func TestTelegramSandboxRefusesAFourthSend(t *testing.T) {
	f := &fakeTelegram{}
	s := newTelegram(t, 3)
	for i := 0; i < 2; i++ {
		if _, _, err := telegramDo(t, s.Transport(f), "sendMessage", tgSend("heo adapter test 1")); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	f.status = http.StatusInternalServerError
	f.answer = map[string]string{"sendMessage": `{"ok":false,"error_code":500,"description":"Internal Server Error"}`}
	if _, _, err := telegramDo(t, s.Transport(f), "sendMessage", tgSend("heo adapter test 3")); err != nil {
		t.Fatalf("a failed send: %v", err)
	}
	f.status, f.answer = 0, nil
	before := len(f.sent())
	again := newTelegram(t, 3)
	again.Ledger = s.Ledger
	if _, _, err := telegramDo(t, again.Transport(f), "sendMessage", tgSend("heo adapter test 4")); err == nil ||
		!strings.Contains(err.Error(), "cap of 3 sends") {
		t.Fatalf("fourth send: %v, want the cap named", err)
	}
	if len(f.sent()) != before {
		t.Error("a send over the cap reached Telegram")
	}
	if s.Sent() != 3 {
		t.Errorf("ledger holds %d sends, want 3", s.Sent())
	}
	// What was sent in an earlier run is the recording's still; changing it
	// is not a send.
	if _, _, err := telegramDo(t, again.Transport(f), "editMessageText", map[string]any{"chat_id": TelegramChatID, "message_id": 42, "text": "heo adapter test 1b"}); err != nil {
		t.Errorf("an edit of the recording's own message, from the ledger: %v", err)
	}
	if _, _, err := telegramDo(t, again.Transport(f), "editMessageText", map[string]any{"chat_id": TelegramChatID, "message_id": 43, "text": "heo adapter test 1b"}); err == nil {
		t.Error("an edit of a message no send made was allowed")
	}
}

// TestTelegramSandboxAnswerThatStillNamesTheBotIsNotHandedOn: what the
// scrub does not recognise must not reach a cassette or the adapter.
func TestTelegramSandboxAnswerThatStillNamesTheBotIsNotHandedOn(t *testing.T) {
	for name, answer := range map[string]string{ // #nosec G101 -- made-up values, not credentials
		"a token":          `{"ok":true,"note":"999999999:AAHzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`,
		"the real token":   `{"ok":true,"note":"` + tgToken + `"}`,
		"the secret half":  `{"ok":true,"note":"` + tgSecret + `"}`,
		"the bot's name":   `{"ok":true,"result":{"first_name":"Acme Support"}}`,
		"a person's name":  `{"ok":true,"result":{"last_name":"Doe"}}`,
		"a title":          `{"ok":true,"result":{"title":"Acme staff room"}}`,
		"a language":       `{"ok":true,"result":{"language_code":"fr"}}`,
		"the bot's number": `{"ok":true,"description":"user 123456789 blocked"}`, // replaced by the scrub: handed on
	} {
		f := &fakeTelegram{answer: map[string]string{"sendMessage": answer}}
		s := newTelegram(t, 3)
		_, body, err := telegramDo(t, s.Transport(f), "sendMessage", tgSend("heo adapter test 1"))
		if name == "the bot's number" {
			if err != nil || strings.Contains(body, "123456789") || !strings.Contains(body, strconv.FormatInt(TelegramBotID, 10)) {
				t.Errorf("%s: %q, %v; want the number replaced", name, body, err)
			}
			continue
		}
		if name == "the secret half" || name == "the real token" {
			// Replaced by the scrub, never handed on as it was.
			if err == nil && (strings.Contains(body, tgSecret) || strings.Contains(body, tgToken)) {
				t.Errorf("%s: the token reached the caller: %q", name, body)
			}
			continue
		}
		if err == nil || body != "" || strings.Contains(err.Error(), "Doe") || strings.Contains(err.Error(), "999999999") {
			t.Errorf("%s: handed on (%q, %v)", name, body, err)
		}
	}
	// A person elsewhere in an answer becomes a placeholder, the same one
	// each time, and a name of theirs is dropped.
	f := &fakeTelegram{answer: map[string]string{"sendMessage": `{"ok":true,"result":{"message_id":9,"reply_to_message":{"message_id":3,"from":{"id":987654321,"is_bot":false,"first_name":"Jo","username":"jo_doe"},` +
		`"chat":{"id":-1001234567890,"type":"supergroup","title":"Acme"}},"chat":{"id":-1001234567890,"type":"supergroup"},"from":{"id":987654321,"is_bot":false}}}`}}
	s := newTelegram(t, 3)
	_, body, err := telegramDo(t, s.Transport(f), "sendMessage", tgSend("heo adapter test 1"))
	if err != nil || strings.Contains(body, "987654321") || strings.Contains(body, "Jo") || strings.Contains(body, "jo_doe") ||
		strings.Count(body, `"id":`+strconv.FormatInt(telegramOtherBase, 10)) != 2 {
		t.Errorf("other user = %q, %v; want a stable placeholder and no name", body, err)
	}
	if !contains(s.Deny(), "987654321") {
		t.Errorf("Deny() = %v, want the replaced user among the literals", s.Deny())
	}
	// A refusal that names a migrated chat keeps the shape and loses the ID.
	f = &fakeTelegram{status: 400, answer: map[string]string{"sendMessage": `{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1007777777777}}`}}
	_, body, err = telegramDo(t, newTelegram(t, 3).Transport(f), "sendMessage", tgSend("heo adapter test 1"))
	if err != nil || strings.Contains(body, "7777777777") || !strings.Contains(body, `"migrate_to_chat_id":-1000000000100`) {
		t.Errorf("migrated chat = %q, %v", body, err)
	}
}

// TestTelegramSandboxScrubsTransportErrors: an error that spells out the
// request URL, which holds the bot token, is not handed on with it.
func TestTelegramSandboxScrubsTransportErrors(t *testing.T) {
	s := newTelegram(t, 3)
	boom := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &urlError{url: r.URL.String()}
	})
	_, _, err := telegramDo(t, s.Transport(boom), "sendMessage", tgSend("heo adapter test 1"))
	if err == nil || strings.Contains(err.Error(), tgToken) || strings.Contains(err.Error(), tgSecret) || !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("error = %v, want the token replaced", err)
	}
}

type urlError struct{ url string }

func (e *urlError) Error() string {
	return "Post \"" + e.url + "\": dial tcp: lookup api.telegram.org: no such host"
}

// TestTelegramSandboxRehearsal: with the rehearsal on, nothing is sent, the
// answers are Telegram's documented shapes over an in-memory chat that
// names the real bot and chat, and the same scrub and cap apply.
func TestTelegramSandboxRehearsal(t *testing.T) {
	f := &fakeTelegram{}
	s := newTelegram(t, 2)
	s.Rehearse = true
	rt := s.Transport(f)
	do := func(method string, args map[string]any) string {
		t.Helper()
		_, body, err := telegramDo(t, rt, method, args)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return body
	}
	first := do("sendMessage", tgSend("heo adapter test 1"))
	for _, real := range []string{"123456789", "1001234567890", "Acme", "Rehearsal", "acme_support"} {
		if strings.Contains(strings.ToLower(first), strings.ToLower(real)) {
			t.Errorf("a rehearsal's answer holds %q: %s", real, first)
		}
	}
	if !strings.Contains(first, `"message_id":101`) || !strings.Contains(first, `"id":`+strconv.FormatInt(TelegramChatID, 10)) {
		t.Errorf("first send answered %s", first)
	}
	second := do("sendMessage", map[string]any{"chat_id": TelegramChatID, "text": "heo adapter test 2", "reply_parameters": map[string]any{"message_id": 101}})
	if !strings.Contains(second, `"message_id":102`) {
		t.Errorf("second send answered %s", second)
	}
	if !strings.Contains(do("editMessageText", map[string]any{"chat_id": TelegramChatID, "message_id": 101, "text": "heo adapter test 1 edited"}), `"ok":true`) {
		t.Error("an edit was refused")
	}
	if !strings.Contains(do("editMessageText", map[string]any{"chat_id": TelegramChatID, "message_id": 101, "text": "heo adapter test 1 edited"}), "message is not modified") {
		t.Error("an edit to the same text was accepted")
	}
	if !strings.Contains(do("setMessageReaction", map[string]any{"chat_id": TelegramChatID, "message_id": 101, "reaction": []any{map[string]any{"type": "emoji", "emoji": "👍"}}}), `"ok":true`) {
		t.Error("a reaction was refused")
	}
	if !strings.Contains(do("deleteMessage", map[string]any{"chat_id": TelegramChatID, "message_id": 101}), `"ok":true`) {
		t.Error("a deletion was refused")
	}
	if !strings.Contains(do("deleteMessage", map[string]any{"chat_id": TelegramChatID, "message_id": 101}), "message to delete not found") {
		t.Error("a second deletion was accepted")
	}
	if !strings.Contains(do("editMessageText", map[string]any{"chat_id": TelegramChatID, "message_id": 101, "text": "heo adapter test 3"}), "message to edit not found") {
		t.Error("an edit of a deleted message was accepted")
	}
	// The cap holds in a rehearsal too, and nothing was sent.
	if _, _, err := telegramDo(t, rt, "sendMessage", tgSend("heo adapter test 3")); err == nil || !strings.Contains(err.Error(), "cap of 2 sends") {
		t.Errorf("a third send: %v, want the cap named", err)
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("a rehearsal reached the network: %v", got)
	}
}

// TestNormalizeTelegramBody: a request's recorded body has its keys
// sorted, so the same request fingerprints the same on every run.
func TestNormalizeTelegramBody(t *testing.T) {
	got := string(normalizeTelegramBody([]byte(`{"text":"heo adapter test 1","chat_id":-1000000000001,"reply_parameters":{"message_id":3,"allow_sending_without_reply":true}}`)))
	want := `{"chat_id":-1000000000001,"reply_parameters":{"allow_sending_without_reply":true,"message_id":3},"text":"heo adapter test 1"}`
	if got != want {
		t.Errorf("normalized = %s, want %s", got, want)
	}
	if got := string(normalizeTelegramBody([]byte("not json"))); got != "not json" {
		t.Errorf("a body that is not JSON = %q", got)
	}
}

// TestCheckLiveTelegram: a live recording needs record mode, the live
// flag, no CI, redaction on and a bot token, its username and one chat; a
// token that is not a bot's, the stand-in, a username of a channel and a
// chat that is the bot itself are refused.
func TestCheckLiveTelegram(t *testing.T) {
	base := map[string]string{
		xrr.EnvMode: "record", EnvLive: "1", EnvTelegramToken: tgToken, EnvTelegramBotUsername: tgUsername,
		EnvTelegramChatID: strconv.FormatInt(tgChat, 10), EnvTelegramLedger: "/tmp/ledger",
	}
	with := func(k, v string) func(string) string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return func(key string) string { return m[key] }
	}
	if err := CheckLiveTelegram(with("x", "")); err != nil {
		t.Fatalf("a complete environment: %v", err)
	}
	if err := CheckLiveTelegram(with(EnvTelegramChatID, "5000000001")); err != nil {
		t.Errorf("the private chat with the maintainer: %v", err)
	}
	for name, getenv := range map[string]func(string) string{
		"replay":          with(xrr.EnvMode, "replay"),
		"flag unset":      with(EnvLive, ""),
		"flag not 1":      with(EnvLive, "true"),
		"CI":              with("CI", "true"),
		"redaction off":   with(xrr.EnvRedactDisable, "1"),
		"no token":        with(EnvTelegramToken, ""),
		"not a token":     with(EnvTelegramToken, "xoxb-"+"0123456789-abcdefghijklmnop"),
		"a short secret":  with(EnvTelegramToken, "123456789:short"),
		"the stand-in":    with(EnvTelegramToken, TelegramStandInToken),
		"no username":     with(EnvTelegramBotUsername, ""),
		"a person's name": with(EnvTelegramBotUsername, "alex"),
		"an @ in a name":  with(EnvTelegramBotUsername, "@"+tgUsername),
		"no chat":         with(EnvTelegramChatID, ""),
		"a channel name":  with(EnvTelegramChatID, "@acme_channel"),
		"chat zero":       with(EnvTelegramChatID, "0"),
		"a padded chat":   with(EnvTelegramChatID, "-01001"),
		"the bot itself":  with(EnvTelegramChatID, "123456789"),
		"a bad cap":       with(EnvTelegramMaxSends, "many"),
		"a negative cap":  with(EnvTelegramMaxSends, "-1"),
	} {
		if err := CheckLiveTelegram(getenv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestForwarderReachesTelegramOnlyThroughALiveSandbox: api.telegram.org is
// reached by a live recording alone.
func TestForwarderReachesTelegramOnlyThroughALiveSandbox(t *testing.T) {
	spy := &fakeTelegram{}
	for name, e := range map[string]*Env{
		"replay":          {Mode: xrr.ModeReplay},
		"record, no live": {Mode: xrr.ModeRecord},
		"passthrough":     {Mode: xrr.ModePassthrough},
	} {
		if _, _, err := telegramDo(t, e.forwarder(spy), "sendMessage", tgSend("heo adapter test 1")); err == nil {
			t.Errorf("%s: api.telegram.org reached", name)
		}
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("refused traffic reached the network: %v", got)
	}
	live := &Env{Mode: xrr.ModeRecord, Telegram: newTelegram(t, 3)}
	if _, _, err := telegramDo(t, live.forwarder(spy), "sendMessage", tgSend("heo adapter test 1")); err != nil {
		t.Errorf("a live recording's send: %v", err)
	}
	if got := spy.sent(); len(got) != 1 {
		t.Errorf("the live send reached the network %d times, want 1", len(got))
	}
}
