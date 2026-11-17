package xrrx

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	xrr "hop.top/xrr"
)

// The "real" application, bot, server and channel of these tests. None of
// it is an actual one.
const (
	dcToken       = "MTIzNDU2Nzg5MDEyMzQ1Njc4.GabcDE.made_up_hmac_of_twenty_seven_chars" //nolint:gosec // G101: a made-up value, not a credential
	dcTokenID     = "MTIzNDU2Nzg5MDEyMzQ1Njc4"                                           //nolint:gosec // G101: a made-up value, not a credential
	dcTokenHMAC   = "made_up_hmac_of_twenty_seven_chars"                                 //nolint:gosec // G101: a made-up value, not a credential
	dcApplication = "223456789012345678"
	dcBot         = "123456789012345678"
	dcGuild       = "323456789012345678"
	dcChannel     = "423456789012345678"
	dcPerson      = "523456789012345678"
)

// fakeDiscord is the network under the sandbox transport: it records every
// request it is sent and answers from a table.
type fakeDiscord struct {
	mu     sync.Mutex
	seen   []string
	paths  []string
	bodies []string
	auths  []string
	status int
	answer map[string]string
}

func (f *fakeDiscord) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Host+r.URL.Path)
	f.paths = append(f.paths, r.URL.EscapedPath())
	f.bodies = append(f.bodies, string(b))
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	key := r.Method
	status := f.status
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	answer, ok := f.answer[key]
	if !ok {
		switch r.Method {
		case http.MethodPost:
			answer = dcSent
		case http.MethodPatch:
			answer = dcSent
		default:
			status, answer = http.StatusNoContent, ""
		}
	}
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"b=1"}, "Server": {"cloudflare"}, "X-RateLimit-Bucket": {"abc"}},
		Body: io.NopCloser(strings.NewReader(answer)), Request: r,
	}, nil
}

func (f *fakeDiscord) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.seen...)
}

// dcSent is what Create Message answers, with what the answer must lose:
// the bot's names and avatar, its member, the server and channel IDs, a
// mentioned person.
const dcSent = `{"id":"1700000000000000042","channel_id":"` + dcChannel + `","content":"heo adapter test 1","type":0,"flags":0,` +
	`"timestamp":"2026-10-14T10:00:00.000000+00:00","edited_timestamp":null,"mention_everyone":false,` +
	`"mentions":[{"id":"` + dcPerson + `","username":"jo_doe","global_name":"Jo Doe","avatar":"abc","discriminator":"0","bot":false}],` +
	`"author":{"id":"` + dcBot + `","bot":true,"username":"Acme Support","global_name":"Acme Support","avatar":"def","discriminator":"0","public_flags":0},` +
	`"member":{"nick":"Acme","roles":[],"joined_at":"2026-01-01T00:00:00.000000+00:00"},` +
	`"message_reference":{"message_id":"1700000000000000041","channel_id":"` + dcChannel + `","guild_id":"` + dcGuild + `"},"application_id":"` + dcApplication + `"}`

func newDiscord(t *testing.T, max int) *DiscordSandbox {
	t.Helper()
	return &DiscordSandbox{
		Token: dcToken, ApplicationID: dcApplication, BotUserID: dcBot, GuildID: dcGuild, ChannelID: dcChannel, Max: max,
		Ledger: filepath.Join(t.TempDir(), "ledger"),
	}
}

// discordDo calls the sandbox's transport as the recorded process would:
// the stand-in token, the placeholder channel.
func discordDo(t *testing.T, rt http.RoundTripper, method, route string, args map[string]any) (*http.Response, string, error) {
	t.Helper()
	return discordDoAs(t, rt, method, "https://"+discordHost+discordAPIPrefix+route, "Bot "+DiscordStandInToken, args)
}

func discordDoAs(t *testing.T, rt http.RoundTripper, method, rawURL, auth string, args map[string]any) (*http.Response, string, error) {
	t.Helper()
	var body io.Reader
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	if args != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/hop-top/uma, 1)")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out), nil
}

const dcSendRoute = "channels/" + DiscordChannelID + "/messages"

func dcSend(text string) map[string]any {
	return map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []any{}}, "nonce": "uabc", "enforce_nonce": true}
}

// TestDiscordSandboxSendsUnderRealNamesAndKeepsNoneOfThem: a send that
// names the placeholder channel reaches Discord under the real one with the
// real token in the Authorization header, and the answer comes back with
// the application, the bot, the server and the channel replaced, every
// name, avatar and member dropped and the headers that churn removed.
func TestDiscordSandboxSendsUnderRealNamesAndKeepsNoneOfThem(t *testing.T) {
	f := &fakeDiscord{}
	s := newDiscord(t, 3)
	resp, body, err := discordDo(t, s.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "POST discord.com/api/v10/channels/"+dcChannel+"/messages" {
		t.Fatalf("Discord was sent %v", got)
	}
	if f.auths[0] != "Bot "+dcToken {
		t.Error("Discord saw a token other than the real one")
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &sent); err != nil || sent["content"] != "heo adapter test 1" || sent["nonce"] != "uabc" {
		t.Errorf("Discord was sent %v", f.bodies[0])
	}
	all := body
	for k, vs := range resp.Header {
		all += "\n" + k + ": " + strings.Join(vs, ", ")
	}
	for _, real := range []string{dcToken, dcTokenID, dcTokenHMAC, dcApplication, dcBot, dcGuild, dcChannel, dcPerson, "Acme", "Jo Doe", "jo_doe", "global_name",
		"avatar", "joined_at", "nick", "cloudflare", "b=1", "abc"} {
		if strings.Contains(all, real) {
			t.Errorf("the answer still holds %q:\n%s", real, all)
		}
	}
	for _, want := range []string{
		`"channel_id":"` + DiscordChannelID + `"`, `"guild_id":"` + DiscordGuildID + `"`, `"application_id":"` + DiscordApplicationID + `"`,
		`"author":{"bot":true,"id":"` + DiscordBotUserID + `","username":"` + DiscordBotUsername + `"}`, `"id":"1700000000000000042"`,
		`"message_id":"1700000000000000041"`, `"content":"heo adapter test 1"`, `"timestamp":"2026-10-14T10:00:00.000000+00:00"`,
		`"mentions":[{"bot":false,"id":"900000000000000200"}]`,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the answer lacks %q:\n%s", want, all)
		}
	}
	if resp.Header.Get("Content-Type") == "" || resp.Header.Get("Server") != "" || resp.Header.Get("X-RateLimit-Bucket") != "" {
		t.Errorf("headers = %v, want only the content type", resp.Header)
	}
	if n := s.Sent(); n != 1 {
		t.Errorf("the ledger holds %d sends, want 1", n)
	}
	if !s.knownMessage("1700000000000000042") {
		t.Error("the message the answer named is not remembered as the recording's own")
	}
	if !contains(s.Deny(), dcPerson) || !contains(s.Deny(), dcToken) || !contains(s.Deny(), dcTokenID) {
		t.Errorf("Deny() = %v, want the token, its parts and the replaced person", s.Deny())
	}
}

// TestDiscordSandboxRefusesWhatARecordingMayNotDo: every request outside
// the recording's narrow path fails before it is sent, takes no slot of
// the ledger and reaches nothing.
func TestDiscordSandboxRefusesWhatARecordingMayNotDo(t *testing.T) {
	f := &fakeDiscord{}
	s := newDiscord(t, 3)
	const known = "1700000000000000042"
	s.noteMessage(0, known)
	rt := s.Transport(f)
	chan_ := "channels/" + DiscordChannelID + "/messages"
	good := func(mod func(map[string]any)) map[string]any {
		a := dcSend("heo adapter test 1")
		if mod != nil {
			mod(a)
		}
		return a
	}
	for name, tc := range map[string]struct {
		method, route, url, auth string
		args                     map[string]any
	}{
		"another channel":  {http.MethodPost, "channels/999999999999999999/messages", "", "", good(nil)},
		"the real channel": {http.MethodPost, "channels/" + dcChannel + "/messages", "", "", good(nil)},
		"a user's channel": {http.MethodPost, "channels/" + dcPerson + "/messages", "", "", good(nil)},
		"another text":     {http.MethodPost, chan_, "", "", dcSend("hello everyone")},
		"a link":           {http.MethodPost, chan_, "", "", dcSend("heo adapter test https://example.com")},
		"a mention":        {http.MethodPost, chan_, "", "", dcSend("heo adapter test <@123456789012345678>")},
		"an at":            {http.MethodPost, chan_, "", "", dcSend("heo adapter test @everyone")},
		"a newline":        {http.MethodPost, chan_, "", "", dcSend("heo adapter test\nhi")},
		"a long text":      {http.MethodPost, chan_, "", "", dcSend("heo adapter test " + strings.Repeat("x", 80))},
		"no text":          {http.MethodPost, chan_, "", "", map[string]any{"allowed_mentions": map[string]any{"parse": []any{}}}},
		"mentions parsed":  {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["allowed_mentions"] = map[string]any{"parse": []any{"everyone"}} })},
		"mentions unset":   {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["allowed_mentions"] = map[string]any{} })},
		"components":       {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["components"] = []any{map[string]any{"type": 1}} })},
		"an embed":         {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["embeds"] = []any{} })},
		"tts":              {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["tts"] = true })},
		"a long nonce":     {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["nonce"] = strings.Repeat("n", 40) })},
		"a stranger reply": {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["message_reference"] = map[string]any{"message_id": "1700000000000000099"} })},
		"a reply to none":  {http.MethodPost, chan_, "", "", good(func(a map[string]any) { a["message_reference"] = map[string]any{"fail_if_not_exists": false} })},
		"a reply's channel": {http.MethodPost, chan_, "", "", good(func(a map[string]any) {
			a["message_reference"] = map[string]any{"message_id": known, "channel_id": dcChannel}
		})},
		"a reply's guild": {http.MethodPost, chan_, "", "", good(func(a map[string]any) {
			a["message_reference"] = map[string]any{"message_id": known, "guild_id": dcGuild}
		})},
		"an unknown message":   {http.MethodPatch, chan_ + "/1700000000000000099", "", "", map[string]any{"content": "heo adapter test 2"}},
		"a delete of one":      {http.MethodDelete, chan_ + "/1700000000000000099", "", "", nil},
		"a message of zero":    {http.MethodDelete, chan_ + "/0", "", "", nil},
		"an edit's text":       {http.MethodPatch, chan_ + "/" + known, "", "", map[string]any{"content": "rewritten"}},
		"an edit's components": {http.MethodPatch, chan_ + "/" + known, "", "", map[string]any{"content": "heo adapter test 2", "components": []any{map[string]any{}}}},
		"an edit's flags":      {http.MethodPatch, chan_ + "/" + known, "", "", map[string]any{"content": "heo adapter test 2", "flags": 4}},
		"a delete's body":      {http.MethodDelete, chan_ + "/" + known, "", "", map[string]any{"x": 1}},
		"a custom emoji":       {http.MethodPut, chan_ + "/" + known + "/reactions/party%3A123/@me", "", "", nil},
		"a word as emoji":      {http.MethodPut, chan_ + "/" + known + "/reactions/hello/@me", "", "", nil},
		"another's reaction":   {http.MethodDelete, chan_ + "/" + known + "/reactions/%F0%9F%91%8D/" + dcPerson, "", "", nil},
		"all reactions":        {http.MethodDelete, chan_ + "/" + known + "/reactions", "", "", nil},
		"a long emoji":         {http.MethodPut, chan_ + "/" + known + "/reactions/%F0%9F%91%8D%F0%9F%91%8D%F0%9F%91%8D%F0%9F%91%8D%F0%9F%91%8D/@me", "", "", nil},
		"reading messages":     {http.MethodGet, chan_, "", "", nil},
		"reading one":          {http.MethodGet, chan_ + "/" + known, "", "", nil},
		"the channel":          {http.MethodGet, "channels/" + DiscordChannelID, "", "", nil},
		"the gateway":          {http.MethodGet, "gateway/bot", "", "", nil},
		"a direct message":     {http.MethodPost, "users/@me/channels", "", "", map[string]any{"recipient_id": dcPerson}},
		"typing":               {http.MethodPost, "channels/" + DiscordChannelID + "/typing", "", "", nil},
		"a thread member":      {http.MethodPut, "channels/" + DiscordChannelID + "/thread-members/@me", "", "", nil},
		"a callback":           {http.MethodPost, "interactions/1/tok/callback", "", "", map[string]any{"type": 5}},
		"a webhook":            {http.MethodPost, "webhooks/" + DiscordApplicationID + "/tok", "", "", map[string]any{"content": "heo adapter test 1"}},
		"a guild":              {http.MethodDelete, "guilds/" + DiscordGuildID, "", "", nil},
		"another host":         {http.MethodPost, "", "https://evil.example/api/v10/" + chan_, "", good(nil)},
		"the gateway host":     {http.MethodPost, "", "https://gateway.discord.gg/api/v10/" + chan_, "", good(nil)},
		"a query":              {http.MethodPost, chan_ + "?wait=1", "", "", good(nil)},
		"plain http":           {http.MethodPost, "", "http://discord.com/api/v10/" + chan_, "", good(nil)},
		"api v9":               {http.MethodPost, "", "https://discord.com/api/v9/" + chan_, "", good(nil)},
		"a traversal":          {http.MethodPost, "channels/" + DiscordChannelID + "/messages/../messages/..", "", "", good(nil)},
		"a doubled slash":      {http.MethodPost, "channels//messages", "", "", good(nil)},
		"a real token":         {http.MethodPost, chan_, "", "Bot " + dcToken, good(nil)},
		"no token":             {http.MethodPost, chan_, "", "-", good(nil)},
		"a bearer":             {http.MethodPost, chan_, "", "Bearer " + DiscordStandInToken, good(nil)},
		"userinfo in the URL":  {http.MethodPost, "", "https://u:p@discord.com/api/v10/" + chan_, "", good(nil)},
	} {
		u := tc.url
		if u == "" {
			u = "https://" + discordHost + discordAPIPrefix + tc.route
		}
		auth := tc.auth
		switch auth {
		case "":
			auth = "Bot " + DiscordStandInToken
		case "-":
			auth = ""
		}
		if _, _, err := discordDoAs(t, rt, tc.method, u, auth, tc.args); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("refused requests reached Discord: %v", got)
	}
	if n := s.Sent(); n != 0 {
		t.Errorf("refused requests took %d slots", n)
	}
	// A body that is not JSON.
	form, _ := http.NewRequest(http.MethodPost, "https://"+discordHost+discordAPIPrefix+chan_, strings.NewReader("content=heo+adapter+test"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.Header.Set("Authorization", "Bot "+DiscordStandInToken)
	if _, err := rt.RoundTrip(form); err == nil {
		t.Error("a form was sent")
	}
	// What is allowed goes through, on the message the recording sent.
	msg := chan_ + "/" + known
	for name, tc := range map[string]struct {
		method, route string
		args          map[string]any
	}{
		"an edit":    {http.MethodPatch, msg, map[string]any{"content": "heo adapter test 1b", "allowed_mentions": map[string]any{"parse": []any{}}, "components": []any{}}},
		"a reaction": {http.MethodPut, msg + "/reactions/%F0%9F%91%8D/@me", nil},
		"a clearing": {http.MethodDelete, msg + "/reactions/%F0%9F%91%8D/@me", nil},
		"a deletion": {http.MethodDelete, msg, nil},
		"a reply": {http.MethodPost, chan_, good(func(a map[string]any) {
			a["message_reference"] = map[string]any{"message_id": known, "channel_id": DiscordChannelID, "fail_if_not_exists": false}
		})},
		"a plain one": {http.MethodPost, chan_, dcSend("heo adapter test 2")},
	} {
		if _, _, err := discordDo(t, rt, tc.method, tc.route, tc.args); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s.Sent() != 2 {
		t.Errorf("ledger holds %d sends, want the two sends", s.Sent())
	}
	// The reaction reached Discord with its emoji intact, on the real
	// channel; the reply named the real channel.
	var sawReaction, sawReply bool
	for i, p := range f.paths {
		sawReaction = sawReaction || (f.seen[i][:3] == "PUT" && p == "/api/v10/channels/"+dcChannel+"/messages/"+known+"/reactions/%F0%9F%91%8D/@me")
		sawReply = sawReply || strings.Contains(f.bodies[i], `"channel_id":"`+dcChannel+`"`)
	}
	if !sawReaction || !sawReply {
		t.Errorf("reaction reached Discord = %v, reply under the real channel = %v; paths %v", sawReaction, sawReply, f.paths)
	}
}

// TestDiscordSandboxRefusesAFourthSend: the cap is the ledger's, and it
// outlives the process: a new sandbox on the same ledger has the sends
// already made counted. A send whose answer failed counts, and a refused
// send reaches nothing.
func TestDiscordSandboxRefusesAFourthSend(t *testing.T) {
	f := &fakeDiscord{}
	s := newDiscord(t, 3)
	for i := 0; i < 2; i++ {
		if _, _, err := discordDo(t, s.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1")); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	f.status = http.StatusInternalServerError
	f.answer = map[string]string{http.MethodPost: `{"code":0,"message":"500: Internal Server Error"}`}
	if _, _, err := discordDo(t, s.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 3")); err != nil {
		t.Fatalf("a failed send: %v", err)
	}
	f.status, f.answer = 0, nil
	before := len(f.sent())
	again := newDiscord(t, 3)
	again.Ledger = s.Ledger
	if _, _, err := discordDo(t, again.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 4")); err == nil ||
		!strings.Contains(err.Error(), "cap of 3 sends") {
		t.Fatalf("fourth send: %v, want the cap named", err)
	}
	if len(f.sent()) != before {
		t.Error("a send over the cap reached Discord")
	}
	if s.Sent() != 3 {
		t.Errorf("ledger holds %d sends, want 3", s.Sent())
	}
	// What was sent in an earlier run is the recording's still; changing it
	// is not a send.
	edit := map[string]any{"content": "heo adapter test 1b"}
	if _, _, err := discordDo(t, again.Transport(f), http.MethodPatch, dcSendRoute+"/1700000000000000042", edit); err != nil {
		t.Errorf("an edit of the recording's own message, from the ledger: %v", err)
	}
	if _, _, err := discordDo(t, again.Transport(f), http.MethodPatch, dcSendRoute+"/1700000000000000043", edit); err == nil {
		t.Error("an edit of a message no send made was allowed")
	}
}

// TestDiscordSandboxAnswerThatStillNamesTheBotIsNotHandedOn: what the
// scrub does not recognise must not reach a cassette or the adapter.
func TestDiscordSandboxAnswerThatStillNamesTheBotIsNotHandedOn(t *testing.T) {
	for name, answer := range map[string]string{ // #nosec G101 -- made-up values, not credentials
		"a token":         `{"id":"1700000000000000042","note":"MTIzNDU2Nzg5MDEyMzQ1Njc5` + `.GzzzzZ.zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`,
		"a nick":          `{"id":"1700000000000000042","x":{"nick":"Jo"}}`,
		"a global name":   `{"id":"1700000000000000042","x":{"global_name":"Jo"}}`,
		"an avatar":       `{"id":"1700000000000000042","x":{"avatar":"abcdef"}}`,
		"a discriminator": `{"id":"1700000000000000042","x":{"discriminator":"1234"}}`,
		"a number":        `{"id":"1700000000000000042","n":` + dcChannel + `}`,
		"a quoted number": `{"id":"1700000000000000042","n":[` + dcGuild + `]}`,
	} {
		f := &fakeDiscord{answer: map[string]string{http.MethodPost: answer}}
		s := newDiscord(t, 3)
		_, body, err := discordDo(t, s.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
		switch name {
		case "a nick", "a global name", "an avatar", "a discriminator":
			// Dropped by the scrub wherever they appear: handed on without.
			if err != nil || strings.Contains(body, "Jo") || strings.Contains(body, "abcdef") || strings.Contains(body, "1234") {
				t.Errorf("%s: %q, %v; want the name dropped", name, body, err)
			}
		default:
			if err == nil || body != "" || strings.Contains(err.Error(), "zzzz") {
				t.Errorf("%s: handed on (%q, %v)", name, body, err)
			}
		}
	}
	// The real IDs in free text are replaced, and a token never survives.
	f := &fakeDiscord{answer: map[string]string{http.MethodPost: `{"id":"1700000000000000042","content":"` + dcToken + ` in ` + dcGuild + ` and ` + dcBot + `"}`}}
	_, body, err := discordDo(t, newDiscord(t, 3).Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
	if err != nil || strings.Contains(body, dcToken) || strings.Contains(body, dcGuild) || strings.Contains(body, dcBot) ||
		!strings.Contains(body, DiscordGuildID) || !strings.Contains(body, DiscordBotUserID) {
		t.Errorf("free text = %q, %v; want the token gone and the IDs replaced", body, err)
	}
	// A person elsewhere in an answer becomes a placeholder, the same one
	// each time, and a name of theirs is dropped.
	f = &fakeDiscord{answer: map[string]string{http.MethodPost: `{"id":"1700000000000000042","author":{"id":"` + dcPerson + `","bot":false,"username":"jo_doe","global_name":"Jo"},` +
		`"referenced_message":{"id":"1700000000000000041","author":{"id":"` + dcPerson + `","username":"jo_doe"}}}`}}
	s := newDiscord(t, 3)
	_, body, err = discordDo(t, s.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
	if err != nil || strings.Contains(body, dcPerson) || strings.Contains(body, "Jo") || strings.Contains(body, "jo_doe") ||
		strings.Count(body, `"id":"900000000000000200"`) != 2 {
		t.Errorf("other user = %q, %v; want a stable placeholder and no name", body, err)
	}
	if !contains(s.Deny(), dcPerson) {
		t.Errorf("Deny() = %v, want the replaced user among the literals", s.Deny())
	}
	// An application that is its own bot user reads as each where it is
	// named so.
	same := newDiscord(t, 3)
	same.ApplicationID = same.BotUserID
	f = &fakeDiscord{answer: map[string]string{http.MethodPost: `{"id":"1700000000000000042","application_id":"` + dcBot + `","author":{"id":"` + dcBot + `","bot":true}}`}}
	_, body, err = discordDo(t, same.Transport(f), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
	if err != nil || !strings.Contains(body, `"application_id":"`+DiscordApplicationID+`"`) || !strings.Contains(body, `"id":"`+DiscordBotUserID+`"`) {
		t.Errorf("application that is the bot = %q, %v", body, err)
	}
}

// TestDiscordSandboxScrubsTransportErrors: an error that spells out the
// token or an ID is not handed on with it.
func TestDiscordSandboxScrubsTransportErrors(t *testing.T) {
	s := newDiscord(t, 3)
	boom := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &urlError{url: r.URL.String() + " " + dcToken}
	})
	_, _, err := discordDo(t, s.Transport(boom), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
	if err == nil || strings.Contains(err.Error(), dcToken) || strings.Contains(err.Error(), dcChannel) || !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("error = %v, want the token and the channel replaced", err)
	}
}

// TestDiscordSandboxRehearsal: with the rehearsal on, nothing is sent, the
// answers are Discord's documented shapes over an in-memory channel that
// names the real bot, server and channel, and the same scrub and cap
// apply.
func TestDiscordSandboxRehearsal(t *testing.T) {
	f := &fakeDiscord{}
	s := newDiscord(t, 2)
	s.Rehearse = true
	rt := s.Transport(f)
	do := func(method, route string, args map[string]any) (int, string) {
		t.Helper()
		resp, body, err := discordDo(t, rt, method, route, args)
		if err != nil {
			t.Fatalf("%s %s: %v", method, route, err)
		}
		return resp.StatusCode, body
	}
	_, first := do(http.MethodPost, dcSendRoute, dcSend("heo adapter test 1"))
	for _, real := range []string{dcBot, dcChannel, dcGuild, "Rehearsal", "avatar", "nick"} {
		if strings.Contains(first, real) {
			t.Errorf("a rehearsal's answer holds %q: %s", real, first)
		}
	}
	if !strings.Contains(first, `"id":"1700000000000000001"`) || !strings.Contains(first, `"channel_id":"`+DiscordChannelID+`"`) {
		t.Errorf("first send answered %s", first)
	}
	reply := dcSend("heo adapter test 2")
	reply["message_reference"] = map[string]any{"message_id": "1700000000000000001", "channel_id": DiscordChannelID}
	if _, second := do(http.MethodPost, dcSendRoute, reply); !strings.Contains(second, `"id":"1700000000000000002"`) || !strings.Contains(second, `"guild_id":"`+DiscordGuildID+`"`) {
		t.Errorf("second send answered %s", second)
	}
	msg := dcSendRoute + "/1700000000000000001"
	edit := map[string]any{"content": "heo adapter test 1 edited", "allowed_mentions": map[string]any{"parse": []any{}}, "components": []any{}}
	if code, body := do(http.MethodPatch, msg, edit); code != http.StatusOK || !strings.Contains(body, "heo adapter test 1 edited") {
		t.Errorf("an edit = %d %s", code, body)
	}
	if code, _ := do(http.MethodPut, msg+"/reactions/%F0%9F%91%8D/@me", nil); code != http.StatusNoContent {
		t.Errorf("a reaction = %d", code)
	}
	if code, _ := do(http.MethodDelete, msg+"/reactions/%F0%9F%91%8D/@me", nil); code != http.StatusNoContent {
		t.Errorf("a cleared reaction = %d", code)
	}
	if code, _ := do(http.MethodDelete, msg, nil); code != http.StatusNoContent {
		t.Errorf("a deletion = %d", code)
	}
	if code, body := do(http.MethodDelete, msg, nil); code != http.StatusNotFound || !strings.Contains(body, "10008") {
		t.Errorf("a second deletion = %d %s", code, body)
	}
	if code, _ := do(http.MethodPatch, msg, edit); code != http.StatusNotFound {
		t.Errorf("an edit of a deleted message = %d", code)
	}
	// The cap holds in a rehearsal too, and nothing was sent.
	if _, _, err := discordDo(t, rt, http.MethodPost, dcSendRoute, dcSend("heo adapter test 3")); err == nil || !strings.Contains(err.Error(), "cap of 2 sends") {
		t.Errorf("a third send: %v, want the cap named", err)
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("a rehearsal reached the network: %v", got)
	}
}

// TestNormalizeDiscordBody: a request's recorded body has its keys sorted
// and its nonce, new on every run, replaced, so the same request
// fingerprints the same on every run.
func TestNormalizeDiscordBody(t *testing.T) {
	got := string(normalizeDiscordBody([]byte(`{"nonce":"u1","content":"heo adapter test 1","allowed_mentions":{"parse":[]},"enforce_nonce":true}`)))
	want := `{"allowed_mentions":{"parse":[]},"content":"heo adapter test 1","enforce_nonce":true,"nonce":"<nonce>"}`
	if got != want {
		t.Errorf("normalized = %s, want %s", got, want)
	}
	if got := string(normalizeDiscordBody([]byte("not json"))); got != "not json" {
		t.Errorf("a body that is not JSON = %q", got)
	}
}

// TestCheckLiveDiscord: a live recording needs record mode, the live flag,
// no CI, redaction on and the bot token, the application, the bot, the
// server and one channel; a token that is not the named bot's, the
// stand-in, an ID that is not a snowflake and two things named by one ID
// are refused.
func TestCheckLiveDiscord(t *testing.T) {
	base := map[string]string{
		xrr.EnvMode: "record", EnvLive: "1", EnvDiscordToken: dcToken, EnvDiscordApplicationID: dcApplication, EnvDiscordBotUserID: dcBot,
		EnvDiscordGuildID: dcGuild, EnvDiscordChannelID: dcChannel, EnvDiscordLedger: "/tmp/ledger",
	}
	with := func(k, v string) func(string) string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return func(key string) string { return m[key] }
	}
	if err := CheckLiveDiscord(with("x", "")); err != nil {
		t.Fatalf("a complete environment: %v", err)
	}
	if err := CheckLiveDiscord(with(EnvDiscordApplicationID, dcBot)); err != nil {
		t.Errorf("an application that is its own bot user: %v", err)
	}
	for name, getenv := range map[string]func(string) string{
		"replay":           with(xrr.EnvMode, "replay"),
		"flag unset":       with(EnvLive, ""),
		"flag not 1":       with(EnvLive, "true"),
		"CI":               with("CI", "true"),
		"redaction off":    with(xrr.EnvRedactDisable, "1"),
		"no token":         with(EnvDiscordToken, ""),
		"not a token":      with(EnvDiscordToken, "xoxb-"+"0123456789-abcdefghijklmnop"),
		"a short secret":   with(EnvDiscordToken, dcTokenID+".GabcDE.short"),
		"the stand-in":     with(EnvDiscordToken, DiscordStandInToken),
		"another bot":      with(EnvDiscordBotUserID, "123456789012345679"),
		"no application":   with(EnvDiscordApplicationID, ""),
		"no guild":         with(EnvDiscordGuildID, ""),
		"no channel":       with(EnvDiscordChannelID, ""),
		"a channel's name": with(EnvDiscordChannelID, "#general"),
		"a padded channel": with(EnvDiscordChannelID, "0"+dcChannel),
		"the guild twice":  with(EnvDiscordChannelID, dcGuild),
		"a bad cap":        with(EnvDiscordMaxSends, "many"),
		"a negative cap":   with(EnvDiscordMaxSends, "-1"),
	} {
		if err := CheckLiveDiscord(getenv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestForwarderReachesDiscordOnlyThroughALiveSandbox: discord.com is reached
// by a live recording alone, and the gateway by none.
func TestForwarderReachesDiscordOnlyThroughALiveSandbox(t *testing.T) {
	spy := &fakeDiscord{}
	for name, e := range map[string]*Env{
		"replay":          {Mode: xrr.ModeReplay},
		"record, no live": {Mode: xrr.ModeRecord},
		"passthrough":     {Mode: xrr.ModePassthrough},
	} {
		if _, _, err := discordDo(t, e.forwarder(spy), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1")); err == nil {
			t.Errorf("%s: discord.com reached", name)
		}
	}
	live := &Env{Mode: xrr.ModeRecord, Discord: newDiscord(t, 3)}
	for _, host := range []string{"gateway.discord.gg", "gateway-us-east1-b.discord.gg"} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/?v=10&encoding=json", nil)
		if _, err := live.forwarder(spy).RoundTrip(req); err == nil || !strings.Contains(err.Error(), "no recording reaches") {
			t.Errorf("%s: %v, want the gateway refused by name", host, err)
		}
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("refused traffic reached the network: %v", got)
	}
	if _, _, err := discordDo(t, live.forwarder(spy), http.MethodPost, dcSendRoute, dcSend("heo adapter test 1")); err != nil {
		t.Errorf("a live recording's send: %v", err)
	}
	if got := spy.sent(); len(got) != 1 {
		t.Errorf("the live send reached the network %d times, want 1", len(got))
	}
}
