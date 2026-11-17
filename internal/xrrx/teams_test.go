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

// The "real" bot, tenant, service and user of these tests. None of it is an
// actual registration.
const (
	tmToken   = "eyJhbGciOiJSUzI1NiJ9.eyJhdWQiOiJtYWRlLXVwIn0.c2lnbmF0dXJlLW1hZGUtdXA" //nolint:gosec // G101: a made-up value, not a credential
	tmApp     = "7a1b2c3d-1111-4222-8333-444455556666"
	tmTenant  = "8b2c3d4e-2222-4333-8444-555566667777"
	tmService = "https://smba.trafficmanager.net/emea/"
	tmUser    = "29:1AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	tmChat    = "a:1ZyXwVuTsRqPoNmLkJiHgFeDcBa9876543210"
	tmOther   = "29:1OtherPersonAccountIdMadeUp-0123456789"
)

// fakeTeams is the network under the sandbox transport: it records every
// request it is sent and answers from a table.
type fakeTeams struct {
	mu      sync.Mutex
	seen    []string
	urls    []string
	auths   []string
	bodies  []string
	status  int
	answer  map[string]string
	header  http.Header
	counter int
}

func (f *fakeTeams) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Host+r.URL.Path)
	f.urls = append(f.urls, r.URL.String())
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, string(b))
	f.counter++
	n := f.counter
	f.mu.Unlock()
	status := f.status
	if status == 0 {
		status = http.StatusOK
	}
	key := r.Method
	if strings.HasSuffix(r.URL.Path, "/activities") {
		key = "send"
	} else if r.Method == http.MethodPost {
		key = "create"
	}
	answer, ok := f.answer[key]
	if !ok {
		switch key {
		case "create":
			answer = tmCreated
		case "send":
			answer = `{"id":"` + strconv.Itoa(1760000000000+n) + `"}`
		default:
			answer = `{"id":"1760000000001"}`
		}
	}
	h := f.header
	if h == nil {
		h = http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"b=1"}, "Server": {"Microsoft-IIS/10.0"}, "X-Ms-Request-Id": {"abc"}}
	}
	return &http.Response{StatusCode: status, Header: h.Clone(), Body: io.NopCloser(strings.NewReader(answer)), Request: r}, nil
}

func (f *fakeTeams) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.seen...)
}

// tmCreated is what creating the personal chat answers, with what the
// answer must lose: the user's name and object ID, the real service.
var tmCreated = `{"id":"` + tmChat + `","activityId":null,"serviceUrl":"` + tmService + `",` +
	`"members":[{"id":"` + tmUser + `","name":"Alex Example","aadObjectId":"9c3d4e5f-3333-4444-8555-666677778888"}]}`

func newTeams(t *testing.T, max int) *TeamsSandbox {
	t.Helper()
	return &TeamsSandbox{
		Token: tmToken, AppID: tmApp, TenantID: tmTenant, ServiceURL: tmService, UserID: tmUser, ConversationID: tmChat,
		Max: max, Ledger: filepath.Join(t.TempDir(), "ledger"),
	}
}

// teamsDo calls the Connector through rt as the recorded process would: the
// stand-in token, the placeholder service and chat.
func teamsDo(t *testing.T, rt http.RoundTripper, method, tail string, body any) (*http.Response, string, error) {
	t.Helper()
	return teamsDoAs(t, rt, method, "https://"+TeamsServiceHost+"/teams/v3/conversations"+tail, TeamsStandInToken, body)
}

func teamsDoAs(t *testing.T, rt http.RoundTripper, method, rawURL, token string, body any) (*http.Response, string, error) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out), nil
}

func tmSend(text string) map[string]any {
	return map[string]any{"type": "message", "text": text, "textFormat": "plain"}
}

const tmChatPath = "/" + TeamsConversationID + "/activities"

func tmCreate() map[string]any {
	return map[string]any{
		"bot": map[string]any{"id": "28:" + TeamsAppID}, "members": []any{map[string]any{"id": TeamsUserID}},
		"isGroup": false, "tenantId": TeamsTenantID, "channelData": map[string]any{"tenant": map[string]any{"id": TeamsTenantID}},
	}
}

// TestTeamsSandboxSendsUnderRealNamesAndKeepsNoneOfThem: a send that names
// the placeholder chat reaches the Bot Connector under the real service
// and chat with the real token as the bearer, and the answer comes back
// with the bot, the tenant, the service, the user and the chat replaced,
// every name dropped and the headers that churn removed.
func TestTeamsSandboxSendsUnderRealNamesAndKeepsNoneOfThem(t *testing.T) {
	f := &fakeTeams{}
	s := newTeams(t, 3)
	rt := s.Transport(f)

	// The personal chat is created for the user the sandbox names.
	resp, body, err := teamsDo(t, rt, http.MethodPost, "", tmCreate())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "POST smba.trafficmanager.net/emea/v3/conversations" {
		t.Fatalf("the Bot Connector was sent %v", got)
	}
	var created map[string]any
	if err := json.Unmarshal([]byte(f.bodies[0]), &created); err != nil || created["tenantId"] != tmTenant ||
		created["bot"].(map[string]any)["id"] != "28:"+tmApp || created["members"].([]any)[0].(map[string]any)["id"] != tmUser ||
		created["channelData"].(map[string]any)["tenant"].(map[string]any)["id"] != tmTenant {
		t.Errorf("the Bot Connector was sent %s, want the real bot, tenant and user", f.bodies[0])
	}
	if f.auths[0] != "Bearer "+tmToken {
		t.Errorf("the Bot Connector saw %q, want the real token", "<redacted>")
	}
	all := body
	for k, vs := range resp.Header {
		all += "\n" + k + ": " + strings.Join(vs, ", ")
	}
	for _, real := range []string{tmToken, tmApp, tmTenant, "trafficmanager", "emea", tmUser, tmChat, "Alex Example", "9c3d4e5f", "aadObjectId", "Microsoft-IIS", "b=1", "abc"} {
		if strings.Contains(strings.ToLower(all), strings.ToLower(real)) {
			t.Errorf("the answer still holds %q:\n%s", real, all)
		}
	}
	for _, want := range []string{`"id":"` + TeamsConversationID + `"`, `"id":"` + TeamsUserID + `"`, `"serviceUrl":"` + TeamsServiceURL + `"`} {
		if !strings.Contains(all, want) {
			t.Errorf("the answer lacks %q:\n%s", want, all)
		}
	}
	if resp.Header.Get("Content-Type") == "" || resp.Header.Get("Server") != "" {
		t.Errorf("headers = %v, want only the content type", resp.Header)
	}

	// A send to the chat: real chat in the path, a slot of the ledger, the
	// activity remembered as the recording's own.
	_, body, err = teamsDo(t, rt, http.MethodPost, tmChatPath, tmSend("heo adapter test 1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.urls[1]; got != "https://smba.trafficmanager.net/emea/v3/conversations/"+tmChat+"/activities" {
		t.Errorf("the Bot Connector was sent %s, want the real service and chat", got)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.bodies[1]), &sent); err != nil || sent["text"] != "heo adapter test 1" || sent["textFormat"] != "plain" || sent["type"] != "message" {
		t.Errorf("the Bot Connector was sent %v", f.bodies[1])
	}
	var id struct{ ID string }
	_ = json.Unmarshal([]byte(body), &id)
	if n := s.Sent(); n != 1 || !s.knownActivity(id.ID) || id.ID == "" {
		t.Errorf("ledger holds %d sends and knows %q (%q), want 1 and the activity of the answer", n, id.ID, body)
	}
}

// TestTeamsSandboxRefusesWhatARecordingMayNotDo: every request outside the
// recording's narrow path fails before it is sent, takes no slot of the
// ledger and reaches nothing.
func TestTeamsSandboxRefusesWhatARecordingMayNotDo(t *testing.T) {
	f := &fakeTeams{}
	s := newTeams(t, 3)
	s.noteActivity(0, "1760000000042")
	rt := s.Transport(f)
	const act = "/1760000000042"
	with := func(m map[string]any, kv ...any) map[string]any {
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	create := func(kv ...any) map[string]any { return with(tmCreate(), kv...) }
	send := func(text string, kv ...any) map[string]any { return with(tmSend(text), kv...) }
	update := func(text string, kv ...any) map[string]any {
		return with(map[string]any{"type": "message", "id": "1760000000042", "text": text, "textFormat": "plain"}, kv...)
	}
	type call struct {
		method, tail string
		body         any
		url, token   string
	}
	good := "heo adapter test 1"
	for name, tc := range map[string]call{
		"another chat":          {http.MethodPost, "/a:1OtherChatMadeUp0123456789/activities", send(good), "", ""},
		"the real chat":         {http.MethodPost, "/" + tmChat + "/activities", send(good), "", ""},
		"a channel":             {http.MethodPost, "/19:abc@thread.tacv2/activities", send(good), "", ""},
		"another text":          {http.MethodPost, tmChatPath, send("hello everyone"), "", ""},
		"a link":                {http.MethodPost, tmChatPath, send("heo adapter test https://example.com"), "", ""},
		"a mention":             {http.MethodPost, tmChatPath, send("heo adapter test @everyone"), "", ""},
		"markup":                {http.MethodPost, tmChatPath, send("heo adapter test <at>All</at>"), "", ""},
		"a newline":             {http.MethodPost, tmChatPath, send("heo adapter test\nhi"), "", ""},
		"a long text":           {http.MethodPost, tmChatPath, send("heo adapter test " + strings.Repeat("x", 80)), "", ""},
		"a typing indicator":    {http.MethodPost, tmChatPath, map[string]any{"type": "typing"}, "", ""},
		"a card":                {http.MethodPost, tmChatPath, send(good, "attachments", []any{map[string]any{"contentType": "application/vnd.microsoft.card.adaptive"}}), "", ""},
		"an entity":             {http.MethodPost, tmChatPath, send(good, "entities", []any{}), "", ""},
		"a markdown format":     {http.MethodPost, tmChatPath, send(good, "textFormat", "markdown"), "", ""},
		"a recipient":           {http.MethodPost, tmChatPath, send(good, "recipient", map[string]any{"id": tmOther}), "", ""},
		"a stranger reply":      {http.MethodPost, tmChatPath, send(good, "replyToId", "1760000000007"), "", ""},
		"an unknown update":     {http.MethodPut, tmChatPath + "/1760000000007", map[string]any{"type": "message", "id": "1760000000007", "text": good + "b", "textFormat": "plain"}, "", ""},
		"an unknown delete":     {http.MethodDelete, tmChatPath + "/1760000000007", nil, "", ""},
		"an update's path":      {http.MethodPut, tmChatPath + act, update(good+"b", "id", "1760000000007"), "", ""},
		"an update's text":      {http.MethodPut, tmChatPath + act, update("rewritten"), "", ""},
		"an update's card":      {http.MethodPut, tmChatPath + act, update(good+"b", "attachments", []any{}), "", ""},
		"a reaction type":       {http.MethodPut, tmChatPath + act + "/reactions/eyes", nil, "", ""},
		"a reaction elsewhere":  {http.MethodPut, tmChatPath + "/1760000000007/reactions/like", nil, "", ""},
		"a body on a delete":    {http.MethodDelete, tmChatPath + act, map[string]any{"x": 1}, "", ""},
		"reading an activity":   {http.MethodGet, tmChatPath + act, nil, "", ""},
		"listing members":       {http.MethodGet, "/" + TeamsConversationID + "/members", nil, "", ""},
		"a group chat":          {http.MethodPost, "", create("isGroup", true), "", ""},
		"another user":          {http.MethodPost, "", with(tmCreate(), "members", []any{map[string]any{"id": tmOther}}), "", ""},
		"two users":             {http.MethodPost, "", with(tmCreate(), "members", []any{map[string]any{"id": TeamsUserID}, map[string]any{"id": tmOther}}), "", ""},
		"another tenant":        {http.MethodPost, "", create("tenantId", tmTenant), "", ""},
		"another bot":           {http.MethodPost, "", with(tmCreate(), "bot", map[string]any{"id": "28:" + tmApp}), "", ""},
		"a topic":               {http.MethodPost, "", create("topicName", "x"), "", ""},
		"channel data too":      {http.MethodPost, "", create("channelData", map[string]any{"tenant": map[string]any{"id": TeamsTenantID}, "channel": map[string]any{"id": "x"}}), "", ""},
		"channel data's tenant": {http.MethodPost, "", create("channelData", map[string]any{"tenant": map[string]any{"id": tmTenant}}), "", ""},
		"a real token":          {http.MethodPost, tmChatPath, send(good), "", tmToken},
		"no token":              {http.MethodPost, tmChatPath, send(good), "", "-"},
		"another host":          {http.MethodPost, "", tmCreate(), "https://smba.trafficmanager.net/teams/v3/conversations", ""},
		"the real service":      {http.MethodPost, tmChatPath, send(good), "https://smba.trafficmanager.net/emea/v3/conversations" + tmChatPath, ""},
		"the key set":           {http.MethodGet, "", nil, "https://login.botframework.com/v1/.well-known/keys", ""},
		"the token endpoint":    {http.MethodPost, "", nil, "https://login.microsoftonline.com/" + tmTenant + "/oauth2/v2.0/token", ""},
		"a query":               {http.MethodPost, tmChatPath + "?isTargetedActivity=true", send(good), "", ""},
		"plain http":            {http.MethodPost, tmChatPath, send(good), "http://" + TeamsServiceHost + "/teams/v3/conversations" + tmChatPath, ""},
		"another region":        {http.MethodPost, tmChatPath, send(good), "https://" + TeamsServiceHost + "/emea/v3/conversations" + tmChatPath, ""},
		"a traversal":           {http.MethodPost, "/../conversations" + tmChatPath, send(good), "", ""},
		"user info":             {http.MethodPost, tmChatPath, send(good), "https://u:p@" + TeamsServiceHost + "/teams/v3/conversations" + tmChatPath, ""},
		"the v3 root":           {http.MethodGet, "", nil, "https://" + TeamsServiceHost + "/teams/v3", ""},
	} {
		u := tc.url
		if u == "" {
			u = "https://" + TeamsServiceHost + "/teams/v3/conversations" + tc.tail
		}
		token := TeamsStandInToken
		switch tc.token {
		case "":
		case "-":
			token = ""
		default:
			token = tc.token
		}
		if _, _, err := teamsDoAs(t, rt, tc.method, u, token, tc.body); err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("refused requests reached the Bot Connector: %v", got)
	}
	if n := s.Sent(); n != 0 {
		t.Errorf("refused requests took %d slots", n)
	}
	// A body that is not JSON.
	form, _ := http.NewRequest(http.MethodPost, "https://"+TeamsServiceHost+"/teams/v3/conversations"+tmChatPath, strings.NewReader("text=heo+adapter+test"))
	form.Header.Set("Authorization", "Bearer "+TeamsStandInToken)
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := rt.RoundTrip(form); err == nil {
		t.Error("a form was sent")
	}
	// What is allowed goes through, on the message the recording sent.
	for name, tc := range map[string]struct {
		method, tail string
		body         any
	}{
		"an update":   {http.MethodPut, tmChatPath + act, update("heo adapter test 1b")},
		"a reaction":  {http.MethodPut, tmChatPath + act + "/reactions/like", nil},
		"a clearing":  {http.MethodDelete, tmChatPath + act + "/reactions/like", nil},
		"a deletion":  {http.MethodDelete, tmChatPath + act, nil},
		"a reply":     {http.MethodPost, tmChatPath, send(good, "replyToId", "1760000000042")},
		"a plain one": {http.MethodPost, tmChatPath, send("heo adapter test 2")},
		"the chat":    {http.MethodPost, "", tmCreate()},
	} {
		if _, _, err := teamsDo(t, rt, tc.method, tc.tail, tc.body); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s.Sent() != 2 {
		t.Errorf("ledger holds %d sends, want the two sends", s.Sent())
	}
	// The path of each is the real chat's.
	for _, u := range f.urls {
		if strings.Contains(u, TeamsServiceHost) || strings.Contains(u, "heo-adapter-test") {
			t.Errorf("a placeholder reached the Bot Connector: %s", u)
		}
	}
}

// TestTeamsSandboxRefusesAFourthSend: the cap is the ledger's, and it
// outlives the process: a new sandbox on the same ledger has the sends
// already made counted. A send whose answer failed counts, and a refused
// send reaches nothing.
func TestTeamsSandboxRefusesAFourthSend(t *testing.T) {
	f := &fakeTeams{}
	s := newTeams(t, 3)
	for i := 0; i < 2; i++ {
		if _, _, err := teamsDo(t, s.Transport(f), http.MethodPost, tmChatPath, tmSend("heo adapter test 1")); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	f.status = http.StatusInternalServerError
	f.answer = map[string]string{"send": `{"error":{"code":"ServiceError","message":"try again"}}`}
	if _, _, err := teamsDo(t, s.Transport(f), http.MethodPost, tmChatPath, tmSend("heo adapter test 3")); err != nil {
		t.Fatalf("a failed send: %v", err)
	}
	f.status, f.answer = 0, nil
	before := len(f.sent())
	again := newTeams(t, 3)
	again.Ledger = s.Ledger
	if _, _, err := teamsDo(t, again.Transport(f), http.MethodPost, tmChatPath, tmSend("heo adapter test 4")); err == nil ||
		!strings.Contains(err.Error(), "cap of 3 sends") {
		t.Fatalf("fourth send: %v, want the cap named", err)
	}
	if len(f.sent()) != before {
		t.Error("a send over the cap reached the Bot Connector")
	}
	if s.Sent() != 3 {
		t.Errorf("ledger holds %d sends, want 3", s.Sent())
	}
	// What was sent in an earlier run is the recording's still, from the
	// ledger; changing it is not a send. A message no send made is not.
	var first struct{ ID string }
	_ = json.Unmarshal([]byte(`{"id":"`+strconv.Itoa(1760000000001)+`"}`), &first)
	upd := func(id string) error {
		_, _, err := teamsDo(t, again.Transport(f), http.MethodPut, tmChatPath+"/"+id, map[string]any{"type": "message", "id": id, "text": "heo adapter test 1b", "textFormat": "plain"})
		return err
	}
	if err := upd(first.ID); err != nil {
		t.Errorf("an update of the recording's own message, from the ledger: %v", err)
	}
	if err := upd("1760000000999"); err == nil {
		t.Error("an update of a message no send made was allowed")
	}
}

// TestTeamsScrubNumbersPlaceholdersInKeyOrder: the placeholders of anyone
// else are numbered in the order the scrub meets them, and it meets an
// object's keys sorted, so the same answer scrubs to the same bytes in
// every sandbox and every run. Go ranges over a map in a random order; a
// scrub that followed it numbered the same chat differently each time.
func TestTeamsScrubNumbersPlaceholdersInKeyOrder(t *testing.T) {
	doc := []byte(`{"id":"9","from":{"id":"` + tmOther + `"},"recipient":{"id":"29:somebody-else-entirely"},` +
		`"conversation":{"id":"19:chan@thread.tacv2"},"members":[{"id":"29:third-person-here"}]}`)
	// Keys sorted: conversation, from, id, members, recipient.
	want := `{"conversation":{"id":"19:heo-adapter-test-other-conversation-1"},"from":{"id":"29:heo-adapter-test-other-2"},` +
		`"id":"9","members":[{"id":"29:heo-adapter-test-other-3"}],"recipient":{"id":"29:heo-adapter-test-other-4"}}`
	for i := range 60 {
		got, err := newTeams(t, 3).ScrubJSON(doc)
		if err != nil || string(got) != want {
			t.Fatalf("scrub %d = %s, %v\nwant   %s", i, got, err, want)
		}
	}
}

// TestTeamsSandboxAnswerThatStillNamesTheBotIsNotHandedOn: what the scrub
// does not recognise must not reach a cassette or the adapter.
func TestTeamsSandboxAnswerThatStillNamesTheBotIsNotHandedOn(t *testing.T) {
	for name, answer := range map[string]string{ // #nosec G101 -- made-up values, not credentials
		"a token":         `{"id":"1","note":"eyJhbGciOiJSUzI1NiJ9.eyJhdWQiOiJzb21lb25lIn0.c29tZW9uZXMtc2lnbmF0dXJl"}`,
		"the real token":  `{"id":"1","note":"` + tmToken + `"}`,
		"a person's name": `{"id":"1","from":{"name":"Alex Example"}}`,
		"an object ID":    `{"id":"1","from":{"aadObjectId":"9c3d4e5f-3333-4444-8555-666677778888"}}`,
		"an email":        `{"id":"1","from":{"email":"alex@example.com"}}`,
	} {
		f := &fakeTeams{answer: map[string]string{"send": answer}}
		s := newTeams(t, 3)
		_, body, err := teamsDo(t, s.Transport(f), http.MethodPost, tmChatPath, tmSend("heo adapter test 1"))
		switch name {
		case "the real token":
			// Replaced by the scrub, never handed on as it was.
			if err == nil && strings.Contains(body, tmToken) {
				t.Errorf("%s: the token reached the caller: %q", name, body)
			}
		case "a person's name", "an object ID", "an email":
			// The keys are dropped, not refused.
			if err != nil || strings.Contains(body, "Alex") || strings.Contains(body, "9c3d4e5f") || strings.Contains(body, "example.com") {
				t.Errorf("%s: %q, %v; want the key dropped", name, body, err)
			}
		default:
			if err == nil || body != "" || strings.Contains(err.Error(), "c29tZW9uZXMt") {
				t.Errorf("%s: handed on (%q, %v)", name, body, err)
			}
		}
	}
	// Anyone else elsewhere in an answer becomes a placeholder, the same one
	// each time, and a name of theirs is dropped.
	f := &fakeTeams{answer: map[string]string{"send": `{"id":"9","replyToId":"3","from":{"id":"` + tmOther + `","name":"Jo","aadObjectId":"1d2e3f4a-4444-4555-8666-777788889999"},` +
		`"recipient":{"id":"` + tmOther + `"},"conversation":{"id":"19:chan@thread.tacv2","tenantId":"` + tmTenant + `"}}`}}
	s := newTeams(t, 3)
	_, body, err := teamsDo(t, s.Transport(f), http.MethodPost, tmChatPath, tmSend("heo adapter test 1"))
	if err != nil || strings.Contains(body, "OtherPerson") || strings.Contains(body, "Jo") || strings.Contains(body, "chan@") || strings.Contains(body, tmTenant) ||
		strings.Count(body, `"id":"29:`+teamsOtherUserBase+`2"`) != 2 || !strings.Contains(body, `"tenantId":"`+TeamsTenantID+`"`) {
		t.Errorf("other user = %q, %v; want a stable placeholder, the tenant's and no name", body, err)
	}
	if !contains(s.Deny(), tmOther) {
		t.Errorf("Deny() = %v, want the replaced user among the literals", s.Deny())
	}
	// A throttling answer keeps its shape and its Retry-After.
	f = &fakeTeams{status: 429, header: http.Header{"Content-Type": {"application/json"}, "Retry-After": {"7"}, "X-Ms-Request-Id": {"x"}},
		answer: map[string]string{"send": `{"error":{"code":"Throttled","message":"too many requests for ` + tmChat + `"}}`}}
	resp, body, err := teamsDo(t, newTeams(t, 3).Transport(f), http.MethodPost, tmChatPath, tmSend("heo adapter test 1"))
	if err != nil || resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "7" || resp.Header.Get("X-Ms-Request-Id") != "" ||
		strings.Contains(body, tmChat) || !strings.Contains(body, `"code":"Throttled"`) || !strings.Contains(body, TeamsConversationID) {
		t.Errorf("throttled = %d %q %v, want the shape, the wait and the placeholder", resp.StatusCode, body, err)
	}
}

// TestTeamsSandboxScrubsTransportErrors: an error that spells out the
// request URL, which holds the real service and chat, is not handed on
// with them.
func TestTeamsSandboxScrubsTransportErrors(t *testing.T) {
	s := newTeams(t, 3)
	boom := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &urlError{url: r.URL.String() + " bearer " + tmToken}
	})
	_, _, err := teamsDo(t, s.Transport(boom), http.MethodPost, tmChatPath, tmSend("heo adapter test 1"))
	if err == nil || strings.Contains(err.Error(), tmChat) || strings.Contains(err.Error(), tmToken) || strings.Contains(err.Error(), "trafficmanager") ||
		!strings.Contains(err.Error(), "<token>") || !strings.Contains(err.Error(), TeamsServiceHost) {
		t.Fatalf("error = %v, want the real names replaced", err)
	}
}

// TestTeamsSandboxRehearsal: with the rehearsal on, nothing is sent, the
// answers are the Bot Connector's documented shapes over an in-memory chat
// that names the real bot, tenant, user and chat, and the same scrub and
// cap apply.
func TestTeamsSandboxRehearsal(t *testing.T) {
	f := &fakeTeams{}
	s := newTeams(t, 2)
	s.Rehearse = true
	rt := s.Transport(f)
	do := func(method, tail string, body any) string {
		t.Helper()
		_, out, err := teamsDo(t, rt, method, tail, body)
		if err != nil {
			t.Fatalf("%s %s: %v", method, tail, err)
		}
		return out
	}
	created := do(http.MethodPost, "", tmCreate())
	for _, real := range []string{tmChat, tmUser, tmApp, "trafficmanager", "Rehearsal", "11111111-2222"} {
		if strings.Contains(strings.ToLower(created), strings.ToLower(real)) {
			t.Errorf("a rehearsal's answer holds %q: %s", real, created)
		}
	}
	if !strings.Contains(created, `"id":"`+TeamsConversationID+`"`) {
		t.Errorf("create answered %s", created)
	}
	first := do(http.MethodPost, tmChatPath, tmSend("heo adapter test 1"))
	second := do(http.MethodPost, tmChatPath, map[string]any{"type": "message", "text": "heo adapter test 2", "textFormat": "plain", "replyToId": "1760000000001"})
	if !strings.Contains(first, `"id":"1760000000001"`) || !strings.Contains(second, `"id":"1760000000002"`) {
		t.Errorf("sends answered %s and %s", first, second)
	}
	act := tmChatPath + "/1760000000001"
	if !strings.Contains(do(http.MethodPut, act, map[string]any{"type": "message", "id": "1760000000001", "text": "heo adapter test 1b", "textFormat": "plain"}), `"id"`) {
		t.Error("an update was refused")
	}
	do(http.MethodPut, act+"/reactions/like", nil)
	do(http.MethodDelete, act+"/reactions/like", nil)
	do(http.MethodDelete, act, nil)
	if _, body, _ := teamsDo(t, rt, http.MethodDelete, act, nil); !strings.Contains(body, "ActivityNotFoundInConversation") {
		t.Errorf("a second deletion answered %q", body)
	}
	// The cap holds in a rehearsal too, and nothing was sent.
	if _, _, err := teamsDo(t, rt, http.MethodPost, tmChatPath, tmSend("heo adapter test 3")); err == nil || !strings.Contains(err.Error(), "cap of 2 sends") {
		t.Errorf("a third send: %v, want the cap named", err)
	}
	if got := f.sent(); len(got) != 0 {
		t.Errorf("a rehearsal reached the network: %v", got)
	}
}

// TestNormalizeTeamsBody: a request's recorded body has its keys sorted, so
// the same request fingerprints the same on every run.
func TestNormalizeTeamsBody(t *testing.T) {
	got := string(normalizeTeamsBody([]byte(`{"text":"heo adapter test 1","type":"message","replyToId":"7","textFormat":"plain"}`)))
	want := `{"replyToId":"7","text":"heo adapter test 1","textFormat":"plain","type":"message"}`
	if got != want {
		t.Errorf("normalized = %s, want %s", got, want)
	}
	if got := string(normalizeTeamsBody([]byte("not json"))); got != "not json" {
		t.Errorf("a body that is not JSON = %q", got)
	}
}

// TestCheckLiveTeams: a live recording needs record mode, the live flag, no
// CI, redaction on, a Bot Connector token (a JWT, never the app's client
// secret), the bot, the tenant, a Microsoft service, one user and their
// personal chat; a value that is none of those, a placeholder and an
// unknown service host are refused.
func TestCheckLiveTeams(t *testing.T) {
	base := map[string]string{
		xrr.EnvMode: "record", EnvLive: "1", EnvTeamsConnectorToken: tmToken, EnvTeamsAppID: tmApp, EnvTeamsTenantID: tmTenant,
		EnvTeamsServiceURL: tmService, EnvTeamsUserID: tmUser, EnvTeamsConversationID: tmChat, EnvTeamsLedger: "/tmp/ledger",
	}
	with := func(k, v string) func(string) string {
		m := map[string]string{}
		for kk, vv := range base {
			m[kk] = vv
		}
		m[k] = v
		return func(key string) string { return m[key] }
	}
	if err := CheckLiveTeams(with("x", "")); err != nil {
		t.Fatalf("a complete environment: %v", err)
	}
	for _, svc := range []string{"https://smba.infra.gcc.teams.microsoft.com/teams", "https://SMBA.trafficmanager.net/amer/", "https://smba.infra.gov.teams.microsoft.us/gcch/"} {
		if err := CheckLiveTeams(with(EnvTeamsServiceURL, svc)); err != nil {
			t.Errorf("service %s: %v", svc, err)
		}
	}
	for name, getenv := range map[string]func(string) string{
		"replay":               with(xrr.EnvMode, "replay"),
		"flag unset":           with(EnvLive, ""),
		"flag not 1":           with(EnvLive, "true"),
		"CI":                   with("CI", "true"),
		"redaction off":        with(xrr.EnvRedactDisable, "1"),
		"no token":             with(EnvTeamsConnectorToken, ""),
		"a client secret":      with(EnvTeamsConnectorToken, "Abc8Q~madeUpClientSecretValue0123456789abcd"),
		"the stand-in":         with(EnvTeamsConnectorToken, TeamsStandInToken),
		"no app":               with(EnvTeamsAppID, ""),
		"an app that is none":  with(EnvTeamsAppID, "my-bot"),
		"the placeholder app":  with(EnvTeamsAppID, TeamsAppID),
		"no tenant":            with(EnvTeamsTenantID, ""),
		"a tenant by name":     with(EnvTeamsTenantID, "contoso.onmicrosoft.com"),
		"one GUID for both":    with(EnvTeamsTenantID, strings.ToUpper(tmApp)),
		"no service":           with(EnvTeamsServiceURL, ""),
		"plain http":           with(EnvTeamsServiceURL, "http://smba.trafficmanager.net/teams/"),
		"another host":         with(EnvTeamsServiceURL, "https://evil.example/teams/"),
		"a lookalike host":     with(EnvTeamsServiceURL, "https://smba.trafficmanager.net.evil.example/teams/"),
		"the placeholder":      with(EnvTeamsServiceURL, TeamsServiceURL),
		"user info":            with(EnvTeamsServiceURL, "https://u:p@smba.trafficmanager.net/teams/"),
		"a port":               with(EnvTeamsServiceURL, "https://smba.trafficmanager.net:8443/teams/"),
		"a query":              with(EnvTeamsServiceURL, "https://smba.trafficmanager.net/teams/?x=1"),
		"no region":            with(EnvTeamsServiceURL, "https://smba.trafficmanager.net/"),
		"a deep path":          with(EnvTeamsServiceURL, "https://smba.trafficmanager.net/a/b/"),
		"no user":              with(EnvTeamsUserID, ""),
		"a bot's account":      with(EnvTeamsUserID, "28:"+tmApp),
		"an object ID":         with(EnvTeamsUserID, "9c3d4e5f-3333-4444-8555-666677778888"),
		"the placeholder user": with(EnvTeamsUserID, TeamsUserID),
		"no chat":              with(EnvTeamsConversationID, ""),
		"a channel":            with(EnvTeamsConversationID, "19:abc123456789@thread.tacv2"),
		"the placeholder chat": with(EnvTeamsConversationID, TeamsConversationID),
		"a bad cap":            with(EnvTeamsMaxSends, "many"),
		"a negative cap":       with(EnvTeamsMaxSends, "-1"),
	} {
		if err := CheckLiveTeams(getenv); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestForwarderReachesTeamsOnlyThroughALiveSandbox: the Bot Connector is
// reached by a live recording alone.
func TestForwarderReachesTeamsOnlyThroughALiveSandbox(t *testing.T) {
	spy := &fakeTeams{}
	for name, e := range map[string]*Env{
		"replay":          {Mode: xrr.ModeReplay},
		"record, no live": {Mode: xrr.ModeRecord},
		"passthrough":     {Mode: xrr.ModePassthrough},
	} {
		if _, _, err := teamsDo(t, e.forwarder(spy), http.MethodPost, tmChatPath, tmSend("heo adapter test 1")); err == nil {
			t.Errorf("%s: the Bot Connector was reached", name)
		}
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("refused traffic reached the network: %v", got)
	}
	live := &Env{Mode: xrr.ModeRecord, Teams: newTeams(t, 3)}
	if _, _, err := teamsDo(t, live.forwarder(spy), http.MethodPost, tmChatPath, tmSend("heo adapter test 1")); err != nil {
		t.Errorf("a live recording's send: %v", err)
	}
	if got := spy.sent(); len(got) != 1 {
		t.Errorf("the live send reached the network %d times, want 1", len(got))
	}
}
