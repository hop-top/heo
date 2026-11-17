package xrrx

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	xrr "hop.top/xrr"
)

// The "real" workspace of these tests. None of it is an actual workspace.
const (
	slToken   = "xoxb-" + "0123456789-abcdefghijklmnop" //nolint:gosec // G101: a made-up value, not a credential
	slTeam    = "T01ABCDEFGH"
	slBotUser = "U01BOTUSER1"
	slBotID   = "B01BOTBOT01"
	slChannel = "C01CHANNEL1"
	slPerson  = "U02SOMEONE9"
	slTS      = "1760000000.000100"
)

// fakeSlack is the network under the sandbox transport: it records every
// request it is sent and answers from a table.
type fakeSlack struct {
	mu     sync.Mutex
	seen   []string
	auth   []string
	bodies []string
	status int
	answer map[string]string
	header http.Header
}

func (f *fakeSlack) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Host+r.URL.Path)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	answer, ok := f.answer[method]
	if !ok {
		answer = slPosted
	}
	h := f.header
	if h == nil {
		h = http.Header{"Content-Type": {"application/json; charset=utf-8"}, "X-Slack-Req-Id": {"abc123"}, "Set-Cookie": {"b=1"}}
	}
	return &http.Response{StatusCode: status, Header: h.Clone(), Body: io.NopCloser(strings.NewReader(answer)), Request: r}, nil
}

func (f *fakeSlack) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.seen...)
}

// slPosted is what chat.postMessage answers, with what the answer must
// lose: the workspace, the app's profile, generated block IDs.
const slPosted = `{"ok":true,"channel":"` + slChannel + `","ts":"` + slTS + `","message":{"type":"message","user":"` + slBotUser +
	`","bot_id":"` + slBotID + `","app_id":"A01APPAPP01","team":"` + slTeam + `","text":"heo adapter test 1","ts":"` + slTS +
	`","bot_profile":{"id":"` + slBotID + `","app_id":"A01APPAPP01","name":"Real Bot","team_id":"` + slTeam + `"},` +
	`"blocks":[{"type":"rich_text","block_id":"Xy3k"}]}}`

func newSlack(t *testing.T, max int) *SlackSandbox {
	t.Helper()
	return &SlackSandbox{
		Token: slToken, TeamID: slTeam, BotUserID: slBotUser, BotID: slBotID, ChannelID: slChannel, Max: max,
		Ledger: filepath.Join(t.TempDir(), "ledger"),
	}
}

// slackDo calls method through rt as the recorded process would: the
// stand-in token, the placeholder channel.
func slackDo(t *testing.T, rt http.RoundTripper, method string, args map[string]any) (*http.Response, string, error) {
	t.Helper()
	return slackDoAs(t, rt, SlackStandInToken, "https://slack.com/api/"+method, args)
}

func slackDoAs(t *testing.T, rt http.RoundTripper, token, rawURL string, args map[string]any) (*http.Response, string, error) {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out), nil
}

func post(text string) map[string]any {
	return map[string]any{"channel": SlackChannelID, "text": text, "mrkdwn": false}
}

// TestSlackSandboxPostsUnderRealNamesAndKeepsNoneOfThem: a post that names
// the placeholder channel reaches Slack under the real one with the real
// token, and the answer comes back with the workspace, bot, channel and
// app replaced, the app's profile and block IDs dropped and the headers
// that churn removed.
func TestSlackSandboxPostsUnderRealNamesAndKeepsNoneOfThem(t *testing.T) {
	f := &fakeSlack{}
	s := newSlack(t, 3)
	resp, body, err := slackDo(t, s.Transport(f), "chat.postMessage", post("heo adapter test 1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "POST slack.com/api/chat.postMessage" {
		t.Fatalf("Slack was sent %v", got)
	}
	if f.auth[0] != "Bearer "+slToken {
		t.Errorf("Slack saw credential %q, want the real token", "<redacted>")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil || sent["channel"] != slChannel || sent["text"] != "heo adapter test 1" {
		t.Errorf("Slack was sent %v, want the real channel and the text", f.bodies[0])
	}
	all := body
	for k, vs := range resp.Header {
		all += "\n" + k + ": " + strings.Join(vs, ", ")
	}
	for _, real := range []string{slToken, slTeam, slBotUser, slBotID, slChannel, "A01APPAPP01", "Real Bot", "Xy3k", "abc123", "b=1"} {
		if strings.Contains(all, real) {
			t.Errorf("the answer still holds %q:\n%s", real, all)
		}
	}
	for _, want := range []string{
		`"channel":"` + SlackChannelID + `"`, `"user":"` + SlackBotUserID + `"`, `"bot_id":"` + SlackBotID + `"`,
		`"app_id":"` + SlackAppID + `"`, `"ts":"` + slTS + `"`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the answer lacks %q:\n%s", want, all)
		}
	}
	if resp.Header.Get("Content-Type") == "" || resp.Header.Get("X-Slack-Req-Id") != "" {
		t.Errorf("headers = %v, want only the content type", resp.Header)
	}
	if n := s.Posted(); n != 1 {
		t.Errorf("the ledger holds %d posts, want 1", n)
	}
}

// TestSlackSandboxRefusesWhatARecordingMayNotDo: every request outside the
// recording's narrow path fails before it is sent, takes no slot of the
// ledger and reaches nothing.
func TestSlackSandboxRefusesWhatARecordingMayNotDo(t *testing.T) {
	f := &fakeSlack{}
	s := newSlack(t, 3)
	s.noteTS(0, slTS)
	rt := s.Transport(f)
	good := func(mod func(map[string]any)) map[string]any {
		a := post("heo adapter test 1")
		if mod != nil {
			mod(a)
		}
		return a
	}
	meta := func(m any) func(map[string]any) { return func(a map[string]any) { a["metadata"] = m } }
	for name, tc := range map[string]struct {
		method string
		args   map[string]any
		url    string
		token  string
	}{
		"another channel":   {method: "chat.postMessage", args: good(func(a map[string]any) { a["channel"] = "C0000000009" })},
		"the real channel":  {method: "chat.postMessage", args: good(func(a map[string]any) { a["channel"] = slChannel })},
		"a direct message":  {method: "chat.postMessage", args: good(func(a map[string]any) { a["channel"] = "D0000000001" })},
		"no channel":        {method: "chat.postMessage", args: map[string]any{"text": "heo adapter test 1"}},
		"another text":      {method: "chat.postMessage", args: post("hello everyone")},
		"a link":            {method: "chat.postMessage", args: post("heo adapter test https://example.com")},
		"a newline":         {method: "chat.postMessage", args: post("heo adapter test\nhi")},
		"a long text":       {method: "chat.postMessage", args: post("heo adapter test " + strings.Repeat("x", 80))},
		"blocks":            {method: "chat.postMessage", args: good(func(a map[string]any) { a["blocks"] = []any{} })},
		"an attachment":     {method: "chat.postMessage", args: good(func(a map[string]any) { a["attachments"] = []any{} })},
		"a mention":         {method: "chat.postMessage", args: good(func(a map[string]any) { a["link_names"] = true })},
		"a stranger thread": {method: "chat.postMessage", args: good(func(a map[string]any) { a["thread_ts"] = "1700000000.000001" })},
		"foreign metadata":  {method: "chat.postMessage", args: good(meta(map[string]any{"event_type": "other", "event_payload": map[string]any{}}))},
		"metadata payload":  {method: "chat.postMessage", args: good(meta(map[string]any{"event_type": slackAttempt, "event_payload": map[string]any{"secret": "x"}}))},
		"an unknown ts":     {method: "chat.update", args: map[string]any{"channel": SlackChannelID, "ts": "1700000000.000001", "text": "heo adapter test 2"}},
		"a delete of one":   {method: "chat.delete", args: map[string]any{"channel": SlackChannelID, "ts": "1700000000.000001"}},
		"an update's text":  {method: "chat.update", args: map[string]any{"channel": SlackChannelID, "ts": slTS, "text": "rewritten"}},
		"a reaction name":   {method: "reactions.add", args: map[string]any{"channel": SlackChannelID, "timestamp": slTS, "name": "Thumbs Up"}},
		"a reaction on one": {method: "reactions.add", args: map[string]any{"channel": SlackChannelID, "timestamp": "1700000000.000001", "name": "eyes"}},
		"a long page":       {method: "conversations.history", args: map[string]any{"channel": SlackChannelID, "limit": 200}},
		"opening a dm":      {method: "conversations.open", args: map[string]any{"users": "U0000000002"}},
		"auth":              {method: "auth.test", args: map[string]any{}},
		"uploading":         {method: "files.upload", args: map[string]any{"channel": SlackChannelID}},
		"listing users":     {method: "users.list", args: map[string]any{}},
		"an admin method":   {method: "admin.conversations.delete", args: map[string]any{"channel_id": SlackChannelID}},
		"another host":      {url: "https://evil.example/api/chat.postMessage", args: post("heo adapter test 1")},
		"hooks":             {url: "https://hooks.slack.com/actions/T0000000001/1/abc", args: post("heo adapter test 1")},
		"a query":           {url: "https://slack.com/api/chat.postMessage?x=1", args: post("heo adapter test 1")},
		"plain http":        {url: "http://slack.com/api/chat.postMessage", args: post("heo adapter test 1")},
		"a traversal":       {url: "https://slack.com/api/../api/chat.postMessage", args: post("heo adapter test 1")},
		"a real token":      {method: "chat.postMessage", args: post("heo adapter test 1"), token: slToken},
		"no token":          {method: "chat.postMessage", args: post("heo adapter test 1"), token: "-"},
	} {
		u := tc.url
		if u == "" {
			u = "https://slack.com/api/" + tc.method
		}
		token := tc.token
		if token == "" {
			token = SlackStandInToken
		}
		if token == "-" {
			token = ""
		}
		if _, _, err := slackDoAs(t, rt, token, u, tc.args); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("refused requests reached Slack: %v", got)
	}
	if n := s.Posted(); n != 0 {
		t.Errorf("refused requests took %d slots", n)
	}
	// GET is refused too.
	req, _ := http.NewRequest(http.MethodGet, "https://slack.com/api/conversations.history", nil)
	req.Header.Set("Authorization", "Bearer "+SlackStandInToken)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Error("GET was sent")
	}
}

// TestSlackSandboxRefusesAFourthPost: the cap is the ledger's, and it
// outlives the process: a new sandbox on the same ledger has the posts
// already made counted. A post whose answer failed counts, and a refused
// post reaches nothing.
func TestSlackSandboxRefusesAFourthPost(t *testing.T) {
	f := &fakeSlack{}
	s := newSlack(t, 3)
	for i := 0; i < 2; i++ {
		if _, _, err := slackDo(t, s.Transport(f), "chat.postMessage", post("heo adapter test 1")); err != nil {
			t.Fatalf("post %d: %v", i+1, err)
		}
	}
	f.status = http.StatusInternalServerError
	if _, _, err := slackDo(t, s.Transport(f), "chat.postMessage", post("heo adapter test 3")); err != nil {
		t.Fatalf("a failed post: %v", err)
	}
	f.status = 0
	before := len(f.sent())
	again := newSlack(t, 3)
	again.Ledger = s.Ledger
	if _, _, err := slackDo(t, again.Transport(f), "chat.postMessage", post("heo adapter test 4")); err == nil ||
		!strings.Contains(err.Error(), "cap of 3 posts") {
		t.Fatalf("fourth post: %v, want the cap named", err)
	}
	if len(f.sent()) != before {
		t.Error("a post over the cap reached Slack")
	}
	if s.Posted() != 3 {
		t.Errorf("ledger holds %d posts, want 3", s.Posted())
	}
	// Reads and writes on what was posted are not posts.
	if _, _, err := slackDo(t, again.Transport(f), "chat.update", map[string]any{"channel": SlackChannelID, "ts": slTS, "text": "heo adapter test 1b"}); err != nil {
		t.Errorf("an edit of the recording's own message: %v", err)
	}
}

// TestSlackSandboxAnswerThatStillNamesTheWorkspaceIsNotHandedOn: what the
// scrub does not recognise must not reach a cassette or the adapter.
func TestSlackSandboxAnswerThatStillNamesTheWorkspaceIsNotHandedOn(t *testing.T) {
	for name, answer := range map[string]string{ // #nosec G101 -- made-up values, not credentials
		"a workspace host": `{"ok":true,"url":"https://acme-corp.slack.com/"}`,
		"a bot token":      `{"ok":true,"note":"xoxb-999999999-aaaaaaaaaaaa"}`,
		"the real token":   `{"ok":true,"note":"` + slToken + `"}`,
	} {
		f := &fakeSlack{answer: map[string]string{"chat.postMessage": answer}}
		s := newSlack(t, 3)
		if _, body, err := slackDo(t, s.Transport(f), "chat.postMessage", post("heo adapter test 1")); err == nil ||
			strings.Contains(err.Error(), "acme-corp") || strings.Contains(err.Error(), "999999999") || body != "" {
			t.Errorf("%s: handed on (%q, %v)", name, body, err)
		}
	}
	// A real person elsewhere in an answer becomes a placeholder, the same
	// one each time.
	f := &fakeSlack{answer: map[string]string{"chat.postMessage": `{"ok":true,"a":"` + slPerson + `","b":"<@` + slPerson + `>"}`}}
	s := newSlack(t, 3)
	_, body, err := slackDo(t, s.Transport(f), "chat.postMessage", post("heo adapter test 1"))
	if err != nil || strings.Contains(body, slPerson) || !strings.Contains(body, `"a":"U0000000010","b":"<@U0000000010>"`) {
		t.Errorf("other user = %q, %v; want a stable placeholder", body, err)
	}
	if !contains(s.Deny(), slPerson) {
		t.Errorf("Deny() = %v, want the replaced user among the literals", s.Deny())
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// TestSlackSandboxKeepsOnlyItsOwnMessages: a history read returns the
// recording's own texts and nothing of the channel's other people.
func TestSlackSandboxKeepsOnlyItsOwnMessages(t *testing.T) {
	history := `{"ok":true,"has_more":true,"response_metadata":{"next_cursor":"abc"},"messages":[` +
		`{"type":"message","user":"` + slPerson + `","text":"lunch?","ts":"1760000009.000100"},` +
		`{"type":"message","user":"` + slBotUser + `","text":"heo adapter test 2","ts":"1760000002.000100"},` +
		`{"type":"message","user":"` + slBotUser + `","text":"a message of the bot's, not the recording's","ts":"1760000001.000100"},` +
		`{"type":"message","user":"` + slBotUser + `","text":"heo adapter test 1","ts":"` + slTS + `","reactions":[{"name":"thumbsup","users":["` + slPerson + `"],"count":1}]}]}`
	f := &fakeSlack{answer: map[string]string{"conversations.history": history}}
	s := newSlack(t, 3)
	got, err := s.ReadBack(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["text"] != "heo adapter test 1" || got[1]["text"] != "heo adapter test 2" {
		t.Fatalf("read back %v, want the two test texts, oldest first", got)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), slPerson) || strings.Contains(string(b), "lunch") {
		t.Errorf("read back still names someone else: %s", b)
	}
}

// TestSlackSandboxRehearsal: with the rehearsal on, nothing is sent, the
// answers are Slack's shapes over a small workspace of what the recording
// posted, the cap still holds, and the scrub runs on the canned answers
// like any other.
func TestSlackSandboxRehearsal(t *testing.T) {
	f := &fakeSlack{}
	s := newSlack(t, 2)
	s.Rehearse = true
	rt := s.Transport(f)
	var posted struct{ TS string }
	call := func(method string, args map[string]any) map[string]any {
		t.Helper()
		_, body, err := slackDo(t, rt, method, args)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: %v\n%s", method, err, body)
		}
		return out
	}
	out := call("chat.postMessage", post("heo adapter test 1"))
	posted.TS, _ = out["ts"].(string)
	if out["ok"] != true || out["channel"] != SlackChannelID || posted.TS == "" {
		t.Fatalf("post = %v", out)
	}
	ch := map[string]any{"channel": SlackChannelID}
	with := func(kv ...any) map[string]any {
		m := map[string]any{"channel": SlackChannelID}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	_ = ch
	if out := call("chat.update", with("ts", posted.TS, "text", "heo adapter test 1 edited")); out["ok"] != true {
		t.Fatalf("update = %v", out)
	}
	if out := call("reactions.add", with("timestamp", posted.TS, "name", "thumbsup")); out["ok"] != true {
		t.Fatalf("react = %v", out)
	}
	if out := call("reactions.add", with("timestamp", posted.TS, "name", "thumbsup")); out["error"] != "already_reacted" {
		t.Fatalf("react twice = %v", out)
	}
	hist := call("conversations.history", with("limit", 10))
	msgs, _ := hist["messages"].([]any)
	if len(msgs) != 1 || !strings.Contains(toJSON(msgs[0]), "edited") || !strings.Contains(toJSON(msgs[0]), "thumbsup") {
		t.Fatalf("history = %v, want the edited message with its reaction", hist)
	}
	if out := call("reactions.remove", with("timestamp", posted.TS, "name", "thumbsup")); out["ok"] != true {
		t.Fatalf("unreact = %v", out)
	}
	if out := call("chat.delete", with("ts", posted.TS)); out["ok"] != true {
		t.Fatalf("delete = %v", out)
	}
	if out := call("chat.delete", with("ts", posted.TS)); out["error"] != "message_not_found" {
		t.Fatalf("delete twice = %v", out)
	}
	call("chat.postMessage", post("heo adapter test 2"))
	if _, _, err := slackDo(t, rt, "chat.postMessage", post("heo adapter test 3")); err == nil || !strings.Contains(err.Error(), "cap of 2 posts") {
		t.Fatalf("third rehearsed post: %v, want the cap", err)
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("a rehearsal reached the network: %v", got)
	}
	if s.Posted() != 0 {
		t.Errorf("a rehearsal wrote the ledger (%d)", s.Posted())
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestNormalizeSlackBody: the attempt metadata of a send names the run's
// operation and attempt; its recorded form does not, and is the same on
// every run.
func TestNormalizeSlackBody(t *testing.T) {
	run := func(op, attempt string) string {
		b := `{"channel":"C0000000001","text":"heo adapter test 1","mrkdwn":false,` +
			`"metadata":{"event_type":"uma_attempt","event_payload":{"operation":"` + op + `","attempt":"` + attempt + `"}}}`
		return string(normalizeSlackBody([]byte(b)))
	}
	one, two := run("op_1", "at_1"), run("op_2", "at_2")
	if one != two || strings.Contains(one, "op_1") || !strings.Contains(one, SlackOperation) || !strings.Contains(one, SlackAttempt) {
		t.Errorf("normalized forms differ or keep the run's IDs:\n%s\n%s", one, two)
	}
	if got := string(normalizeSlackBody([]byte("not json"))); got != "not json" {
		t.Errorf("a body that is not JSON = %q", got)
	}
	if got := string(normalizeSlackBody([]byte(`{"channel":"C"}`))); got != `{"channel":"C"}` {
		t.Errorf("a body with no metadata = %q", got)
	}
}

// TestCheckLiveSlack: a live recording needs record mode, the live flag,
// no CI, redaction on and a bot token, a workspace, a bot user and a
// channel; a direct message or a token that is not a bot's is refused.
func TestCheckLiveSlack(t *testing.T) {
	base := map[string]string{
		xrr.EnvMode: "record", EnvLive: "1", EnvSlackToken: slToken, EnvSlackTeamID: slTeam, EnvSlackBotUserID: slBotUser,
		EnvSlackChannelID: slChannel, EnvSlackLedger: "/tmp/ledger",
	}
	with := func(k, v string) func(string) string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return func(key string) string { return m[key] }
	}
	if err := CheckLiveSlack(with("x", "")); err != nil {
		t.Fatalf("a complete environment: %v", err)
	}
	for name, getenv := range map[string]func(string) string{
		"replay":           with(xrr.EnvMode, "replay"),
		"flag unset":       with(EnvLive, ""),
		"flag not 1":       with(EnvLive, "true"),
		"CI":               with("CI", "true"),
		"redaction off":    with(xrr.EnvRedactDisable, "1"),
		"no token":         with(EnvSlackToken, ""),
		"a user token":     with(EnvSlackToken, "xoxp-0123456789-abcdefghijklmnop"),
		"no workspace":     with(EnvSlackTeamID, ""),
		"workspace shaped": with(EnvSlackTeamID, "u01abcdefgh"),
		"no bot user":      with(EnvSlackBotUserID, ""),
		"no channel":       with(EnvSlackChannelID, ""),
		"a direct message": with(EnvSlackChannelID, "D01ABCDEFGH"),
		"a bad bot ID":     with(EnvSlackBotID, "X01ABCDEFGH"),
		"a bad cap":        with(EnvSlackMaxPosts, "many"),
	} {
		if err := CheckLiveSlack(getenv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestForwarderReachesSlackOnlyThroughALiveSandbox: slack.com's Web API is
// reached by a live recording alone, and hooks.slack.com by none: a
// response URL answers once, to the person who pressed the control.
func TestForwarderReachesSlackOnlyThroughALiveSandbox(t *testing.T) {
	spy := &fakeSlack{}
	for name, e := range map[string]*Env{
		"replay":          {Mode: xrr.ModeReplay},
		"record, no live": {Mode: xrr.ModeRecord},
		"passthrough":     {Mode: xrr.ModePassthrough},
	} {
		if _, _, err := slackDo(t, e.forwarder(spy), "chat.postMessage", post("heo adapter test 1")); err == nil {
			t.Errorf("%s: slack.com reached", name)
		}
	}
	live := &Env{Mode: xrr.ModeRecord, Slack: newSlack(t, 3)}
	if _, _, err := slackDoAs(t, live.forwarder(spy), SlackStandInToken, "https://hooks.slack.com/actions/T0000000001/1/abc", map[string]any{"text": "x"}); err == nil {
		t.Error("a live recording reached hooks.slack.com")
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("refused traffic reached the network: %v", got)
	}
	if _, _, err := slackDo(t, live.forwarder(spy), "chat.postMessage", post("heo adapter test 1")); err != nil {
		t.Errorf("a live recording's post: %v", err)
	}
	if got := spy.sent(); len(got) != 1 {
		t.Errorf("the live post reached the network %d times, want 1", len(got))
	}
}

// TestSlackSandboxReadBackFollowsThreads: a reply in a thread is not in the
// channel's history, so the read-back asks for each thread the history
// shows and merges the recording's own replies, oldest first.
func TestSlackSandboxReadBackFollowsThreads(t *testing.T) {
	own := func(text, ts string, extra string) string {
		return `{"type":"message","user":"` + slBotUser + `","text":"` + text + `","ts":"` + ts + `"` + extra + `}`
	}
	f := &fakeSlack{answer: map[string]string{
		"conversations.history": `{"ok":true,"messages":[` + own("heo adapter test 1", slTS, `,"reply_count":1`) + `]}`,
		"conversations.replies": `{"ok":true,"messages":[` + own("heo adapter test 1", slTS, `,"reply_count":1`) + `,` +
			own("heo adapter test 2", "1760000001.000100", `,"thread_ts":"`+slTS+`"`) + `,` +
			`{"type":"message","user":"` + slPerson + `","text":"+1","ts":"1760000002.000100"}]}`,
	}}
	s := newSlack(t, 3)
	s.noteTS(0, slTS)
	got, err := s.ReadBack(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0]["text"] != "heo adapter test 1" || got[1]["text"] != "heo adapter test 2" {
		t.Fatalf("read back %v, want the message and its reply", got)
	}
	if seen := f.sent(); len(seen) != 2 || seen[1] != "POST slack.com/api/conversations.replies" {
		t.Errorf("requests = %v, want the history and one thread", seen)
	}
}
