package xrrx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The "real" account of these tests. None of it is an actual account.
const (
	twAccount = "AC" + "0123456789abcdef0123456789abcdef"
	twToken   = "0f1e2d3c4b5a69788796a5b4c3d2e1f0" //nolint:gosec // G101: a made-up value, not a credential
	twFrom    = "+12025550123"
	twTo      = "+13035550188"
	twMsg     = "SM00112233445566778899aabbccddeeff"
)

// fakeTwilio is the network under the sandbox transport: it records every
// request it is sent and answers from a table.
type fakeTwilio struct {
	mu     sync.Mutex
	seen   []string
	auth   []string
	bodies []string
	status int
	answer string
	header http.Header
}

func (f *fakeTwilio) RoundTrip(r *http.Request) (*http.Response, error) {
	var b []byte
	if r.Body != nil {
		b, _ = io.ReadAll(r.Body)
	}
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Host+r.URL.Path+"?"+r.URL.RawQuery)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.bodies = append(f.bodies, string(b))
	f.mu.Unlock()
	status := f.status
	if status == 0 {
		status = http.StatusCreated
	}
	answer := f.answer
	if answer == "" {
		answer = twAnswer
	}
	h := f.header
	if h == nil {
		h = http.Header{"Content-Type": {"application/json"}, "Location": {"https://api.twilio.com/2010-04-01/Accounts/" + twAccount + "/Messages/" + twMsg + ".json"}, "X-Twilio-Request-Id": {"RQ1"}}
	}
	return &http.Response{StatusCode: status, Header: h.Clone(), Body: io.NopCloser(strings.NewReader(answer)), Request: r}, nil
}

func (f *fakeTwilio) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.seen...)
}

const twAnswer = `{"account_sid":"` + twAccount + `","api_version":"2010-04-01","body":"heo adapter test 1",` +
	`"date_created":"Mon, 12 Oct 2026 19:30:00 +0000","date_sent":null,"date_updated":"Mon, 12 Oct 2026 19:30:00 +0000",` +
	`"direction":"outbound-api","error_code":null,"error_message":null,"from":"` + twFrom + `","messaging_service_sid":null,` +
	`"num_media":"0","num_segments":"1","price":null,"price_unit":"USD","sid":"` + twMsg + `","status":"queued","to":"` + twTo + `",` +
	`"subresource_uris":{"media":"/2010-04-01/Accounts/` + twAccount + `/Messages/` + twMsg + `/Media.json"},` +
	`"uri":"/2010-04-01/Accounts/` + twAccount + `/Messages/` + twMsg + `.json"}`

func newTwilio(t *testing.T, max int) *TwilioSandbox {
	t.Helper()
	return &TwilioSandbox{AccountSID: twAccount, Token: twToken, From: twFrom, To: twTo, Max: max, Ledger: filepath.Join(t.TempDir(), "ledger")}
}

func twilioDo(t *testing.T, rt http.RoundTripper, method, rawURL, form string) (*http.Response, string, error) {
	t.Helper()
	var body io.Reader
	if form != "" {
		body = strings.NewReader(form)
	}
	req, err := http.NewRequest(method, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(TwilioAccountSID, "stand-in")
	if form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

const (
	messagesURL = "https://api.twilio.com/2010-04-01/Accounts/" + TwilioAccountSID + "/Messages.json"
	okForm      = "From=%2B15550100001&To=%2B15550100002&Body=heo+adapter+test+1"
)

// TestTwilioSandboxSendsRealNamesAndKeepsNoneOfThem: a send that names the
// placeholder account and numbers reaches Twilio under the real ones with
// the real credential and no status callback, and the answer comes back
// with the account, SID, numbers, dates and the headers that churn
// replaced or dropped.
func TestTwilioSandboxSendsRealNamesAndKeepsNoneOfThem(t *testing.T) {
	f := &fakeTwilio{}
	s := newTwilio(t, 2)
	resp, body, err := twilioDo(t, s.Transport(f), http.MethodPost, messagesURL,
		okForm+"&StatusCallback="+url.QueryEscape("https://hooks.invalid/twilio?operation=o&attempt=a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.sent(); len(got) != 1 || got[0] != "POST api.twilio.com/2010-04-01/Accounts/"+twAccount+"/Messages.json?" {
		t.Fatalf("Twilio was sent %v, want the real account's path", got)
	}
	user, pass, _ := (&http.Request{Header: http.Header{"Authorization": {f.auth[0]}}}).BasicAuth()
	if user != twAccount || pass != twToken {
		t.Errorf("Twilio saw credential %q:%q, want the real account's", user, "<token>")
	}
	sent, _ := url.ParseQuery(f.bodies[0])
	if sent.Get("To") != twTo || sent.Get("From") != twFrom || sent.Get("Body") != "heo adapter test 1" || len(sent) != 3 {
		t.Errorf("Twilio was sent the form %v, want the real numbers, the text and no status callback", sent)
	}
	all := body
	for k, vs := range resp.Header {
		all += "\n" + k + ": " + strings.Join(vs, ", ")
	}
	for _, real := range []string{twAccount, twFrom, twTo, "5550123", "5550188", twMsg, "2026", "RQ1"} {
		if strings.Contains(all, real) {
			t.Errorf("the answer still holds %q:\n%s", real, all)
		}
	}
	for _, want := range []string{
		`"account_sid":"` + TwilioAccountSID + `"`, `"from":"` + TwilioSender + `"`, `"to":"` + TwilioRecipient + `"`,
		`"sid":"SM00000000000000000000000000000001"`, `"date_created":"` + TwilioDate + `"`, `"messaging_service_sid":null`,
		"/Accounts/" + TwilioAccountSID + "/Messages/SM00000000000000000000000000000001.json",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the answer lacks %q:\n%s", want, all)
		}
	}
	if resp.Header.Get("X-Twilio-Request-Id") != "" {
		t.Error("a request-id header survived")
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, TwilioAccountSID) {
		t.Errorf("Location = %q, want it scrubbed, not dropped", loc)
	}
	if n := s.Sent(); n != 1 {
		t.Errorf("the ledger holds %d sends, want 1", n)
	}
}

// TestTwilioSandboxRefusesAThirdSend: the cap is the ledger's, and it
// outlives the process: a new sandbox on the same ledger has the sends
// already made counted. A refused send reaches nothing.
func TestTwilioSandboxRefusesAThirdSend(t *testing.T) {
	f := &fakeTwilio{}
	s := newTwilio(t, 2)
	for i := 0; i < 2; i++ {
		if _, _, err := twilioDo(t, s.Transport(f), http.MethodPost, messagesURL, okForm); err != nil {
			t.Fatalf("send %d: %v", i+1, err)
		}
	}
	again := newTwilio(t, 2)
	again.Ledger = s.Ledger
	if _, _, err := twilioDo(t, again.Transport(f), http.MethodPost, messagesURL, okForm); err == nil || !strings.Contains(err.Error(), "cap of 2 sends is spent") || strings.Contains(err.Error(), again.Ledger) {
		t.Fatalf("the third send: %v, want the cap's refusal", err)
	}
	if n := len(f.sent()); n != 2 {
		t.Errorf("Twilio was sent %d requests, want 2", n)
	}
	if n := again.Sent(); n != 2 {
		t.Errorf("the ledger holds %d sends, want 2", n)
	}
	if zero := newTwilio(t, 0); func() bool {
		_, _, err := twilioDo(t, zero.Transport(f), http.MethodPost, messagesURL, okForm)
		return err == nil
	}() {
		t.Error("a cap of zero allowed a send")
	}
}

// TestTwilioSandboxErrorsNameNoOne: a transport error spells out the
// request's URL, which holds the real account; the error the caller gets
// does not, and still unwraps to the cause.
func TestTwilioSandboxErrorsNameNoOne(t *testing.T) {
	s := newTwilio(t, 2)
	cause := io.ErrUnexpectedEOF
	broken := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: r.URL.String() + "?token=" + twToken + "&to=" + url.QueryEscape(twTo), Err: cause}
	})
	_, _, err := twilioDo(t, s.Transport(broken), http.MethodPost, messagesURL, okForm)
	if err == nil {
		t.Fatal("no error")
	}
	for _, real := range []string{twAccount, twToken, twTo, "3035550188"} {
		if strings.Contains(err.Error(), real) {
			t.Errorf("the error names %q: %v", real, err)
		}
	}
	if !errors.Is(err, cause) {
		t.Errorf("the error does not unwrap to its cause: %v", err)
	}
}

// TestTwilioSandboxAFailedSendStillCounts: a send whose answer is lost
// may have been sent, so its slot stays taken.
func TestTwilioSandboxAFailedSendStillCounts(t *testing.T) {
	s := newTwilio(t, 2)
	broken := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF })
	if _, _, err := twilioDo(t, s.Transport(broken), http.MethodPost, messagesURL, okForm); err == nil {
		t.Fatal("a lost answer was not an error")
	}
	if n := s.Sent(); n != 1 {
		t.Errorf("the ledger holds %d sends after a lost answer, want 1", n)
	}
}

// TestTwilioSandboxAllowsOnlyItsEndpoints: any other host, scheme,
// method, account or path fails before it is sent: no calls, numbers,
// keys, usage, balance, deletion or webhook configuration.
func TestTwilioSandboxAllowsOnlyItsEndpoints(t *testing.T) {
	acct := "https://api.twilio.com/2010-04-01/Accounts/" + TwilioAccountSID
	for name, tc := range map[string]struct{ method, url, form string }{
		"another host":         {"POST", "https://api.twilio.net/2010-04-01/Accounts/" + TwilioAccountSID + "/Messages.json", okForm},
		"a subdomain":          {"POST", "https://studio.twilio.com/v2/Flows", okForm},
		"plain http":           {"POST", "http://api.twilio.com/2010-04-01/Accounts/" + TwilioAccountSID + "/Messages.json", okForm},
		"the real account":     {"GET", "https://api.twilio.com/2010-04-01/Accounts/" + twAccount + "/Messages.json", ""},
		"the accounts list":    {"GET", "https://api.twilio.com/2010-04-01/Accounts.json", ""},
		"the account":          {"GET", acct + ".json", ""},
		"calls":                {"POST", acct + "/Calls.json", "To=%2B15550100002&From=%2B15550100001&Url=x"},
		"call list":            {"GET", acct + "/Calls.json", ""},
		"keys":                 {"POST", acct + "/Keys.json", "FriendlyName=x"},
		"balance":              {"GET", acct + "/Balance.json", ""},
		"usage":                {"GET", acct + "/Usage/Records.json", ""},
		"a number's webhook":   {"POST", acct + "/IncomingPhoneNumbers/PN00000000000000000000000000000001.json", "SmsUrl=https://x.invalid"},
		"buying a number":      {"POST", acct + "/IncomingPhoneNumbers.json", "PhoneNumber=%2B15550100003"},
		"deleting a message":   {"DELETE", acct + "/Messages/SM00000000000000000000000000000001.json", ""},
		"updating a message":   {"POST", acct + "/Messages/SM00000000000000000000000000000001.json", "Body="},
		"a message never sent": {"GET", acct + "/Messages/SM00000000000000000000000000000009.json", ""},
		"a non-canonical path": {"GET", "https://api.twilio.com/2010-04-01/Accounts/" + TwilioAccountSID + "/../Calls.json", ""},
		"an encoded path":      {"GET", acct + "/Messages%2F..%2FCalls.json", ""},
		"an odd query":         {"GET", acct + "/Messages.json?Anything=1", ""},
		"a query number":       {"GET", acct + "/Messages.json?To=%2B15550100009", ""},
		"credentials in a URL": {"GET", "https://u:p@api.twilio.com/2010-04-01/Accounts/" + TwilioAccountSID + "/Messages.json", ""},
		"a put":                {"PUT", messagesURL, okForm},
		"a patch":              {"PATCH", messagesURL, okForm},
	} {
		f := &fakeTwilio{}
		s := newTwilio(t, 2)
		// A message this sandbox has answered with: its SID is known, so
		// only the method keeps it from being changed or deleted.
		s.sids = map[string]string{twMsg: "SM00000000000000000000000000000001"}
		s.counters = map[string]int{"SM": 1, "AC": 1}
		if _, _, err := twilioDo(t, s.Transport(f), tc.method, tc.url, tc.form); err == nil {
			t.Errorf("%s was sent", name)
		}
		if got := f.sent(); len(got) != 0 {
			t.Errorf("%s reached Twilio: %v", name, got)
		}
		if n := s.Sent(); n != 0 {
			t.Errorf("%s took %d slots", name, n)
		}
	}
}

// TestTwilioSandboxTextsOnlyTheRecipient: a send to any other number, or
// with anything beyond a short test text, fails before it takes a slot or
// reaches Twilio. The recipient is the owner's number and no one else's.
func TestTwilioSandboxTextsOnlyTheRecipient(t *testing.T) {
	for name, form := range map[string]string{
		"another number":      "From=%2B15550100001&To=%2B15550100003&Body=heo+adapter+test+1",
		"a real-looking one":  "From=%2B15550100001&To=%2B14155550100&Body=heo+adapter+test+1",
		"the real recipient":  "From=%2B15550100001&To=" + url.QueryEscape(twTo) + "&Body=heo+adapter+test+1",
		"a long short code":   "From=%2B15550100001&To=%2B1555012&Body=heo+adapter+test+1",
		"a messaging service": "MessagingServiceSid=MG00000000000000000000000000000001&To=%2B15550100002&Body=heo+adapter+test+1",
		"media":               okForm + "&MediaUrl=https%3A%2F%2Fx.invalid%2Fa.png",
		"a content template":  "From=%2B15550100001&To=%2B15550100002&ContentSid=HX00000000000000000000000000000001",
		"another sender":      "From=%2B15550100009&To=%2B15550100002&Body=heo+adapter+test+1",
		"the real sender":     "From=" + url.QueryEscape(twFrom) + "&To=%2B15550100002&Body=heo+adapter+test+1",
		"no sender":           "To=%2B15550100002&Body=heo+adapter+test+1",
		"another text":        "From=%2B15550100001&To=%2B15550100002&Body=hello",
		"a link":              "From=%2B15550100001&To=%2B15550100002&Body=heo+adapter+test+https%3A%2F%2Fx.invalid",
		"a long text":         "From=%2B15550100001&To=%2B15550100002&Body=heo+adapter+test+" + strings.Repeat("x", 80),
		"a second recipient":  okForm + "&To=%2B15550100003",
		"no form":             "",
	} {
		f := &fakeTwilio{}
		s := newTwilio(t, 2)
		if _, _, err := twilioDo(t, s.Transport(f), http.MethodPost, messagesURL, form); err == nil {
			t.Errorf("%s was sent", name)
		}
		if got := f.sent(); len(got) != 0 {
			t.Errorf("%s reached Twilio: %v", name, got)
		}
		if n := s.Sent(); n != 0 {
			t.Errorf("%s took %d slots", name, n)
		}
	}
}

// TestTwilioSandboxAnInvalidRecipientIsFree: a number too short to exist
// goes to Twilio, which refuses it (error 21265) before creating or
// billing anything: no slot is taken. Were it accepted, it would count and
// the answer would not be handed on.
func TestTwilioSandboxAnInvalidRecipientIsFree(t *testing.T) {
	f := &fakeTwilio{status: http.StatusBadRequest, answer: `{"code":21265,"message":"'To' number cannot be a Short Code: +XXXX","more_info":"https://www.twilio.com/docs/errors/21265","status":400}`}
	s := newTwilio(t, 2)
	resp, body, err := twilioDo(t, s.Transport(f), http.MethodPost, messagesURL, "From=%2B15550100001&To=%2B1555&Body=heo+adapter+test+0")
	if err != nil || resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "21265") {
		t.Fatalf("invalid recipient: %v, %v %s", err, resp, body)
	}
	if sent, _ := url.ParseQuery(f.bodies[0]); sent.Get("To") != "+1555" || sent.Get("From") != twFrom {
		t.Errorf("Twilio was sent %v", sent)
	}
	if n := s.Sent(); n != 0 {
		t.Errorf("an invalid recipient took %d slots", n)
	}

	accepted := &fakeTwilio{}
	if _, _, err := twilioDo(t, s.Transport(accepted), http.MethodPost, messagesURL, "From=%2B15550100001&To=%2B1555&Body=heo+adapter+test+0"); err == nil {
		t.Error("an accepted invalid recipient was handed on")
	}
	if n := s.Sent(); n != 1 {
		t.Errorf("an accepted invalid recipient left %d slots taken, want 1", n)
	}
}

// TestTwilioSandboxReadsOnlyItsOwnTexts: a list of messages keeps the
// sandbox's own texts to the recipient and drops the rest of the
// account's history; one message is fetched by the placeholder SID the
// sandbox gave out, and by no other; the incoming numbers are read once.
func TestTwilioSandboxReadsOnlyItsOwnTexts(t *testing.T) {
	list := `{"messages":[` +
		`{"sid":"SMaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","body":"call me back","to":"+14155550199","from":"` + twFrom + `"},` +
		`{"sid":"` + twMsg + `","body":"heo adapter test 1","to":"` + twTo + `","from":"` + twFrom + `","status":"delivered"}],` +
		`"next_page_uri":"/2010-04-01/Accounts/` + twAccount + `/Messages.json?PageToken=x","page":0,"start":0,"end":57}`
	f := &fakeTwilio{status: 200, answer: list}
	s := newTwilio(t, 2)
	_, body, err := twilioDo(t, s.Transport(f), http.MethodGet, messagesURL+"?To=%2B15550100002&PageSize=20", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "call me back") || strings.Contains(body, "4155550199") || !strings.Contains(body, `"status":"delivered"`) || strings.Contains(body, "PageToken") || strings.Contains(body, `"end":57`) || !strings.Contains(body, `"end":0`) {
		t.Errorf("the list = %s", body)
	}
	if got := f.sent(); len(got) != 1 || !strings.Contains(got[0], "To=%2B13035550188") {
		t.Errorf("Twilio was asked %v, want the real recipient", got)
	}
	f.answer = twAnswer
	_, one, err := twilioDo(t, s.Transport(f), http.MethodGet, "https://api.twilio.com/2010-04-01/Accounts/"+TwilioAccountSID+"/Messages/SM00000000000000000000000000000001.json", "")
	if err != nil || !strings.Contains(one, `"status":"queued"`) {
		t.Fatalf("fetch by the placeholder SID: %v %s", err, one)
	}
	if got := f.sent(); !strings.Contains(got[len(got)-1], "/Messages/"+twMsg+".json") {
		t.Errorf("Twilio was asked %v, want the real SID", got)
	}

	n := &fakeTwilio{status: 200, answer: `{"incoming_phone_numbers":[{"phone_number":"` + twFrom + `","sid":"PN0123456789abcdef0123456789abcdef","friendly_name":"Acme Co","capabilities":{"sms":true}}]}`}
	ls := newTwilio(t, 2)
	ls.From, ls.LearnFrom = "", true
	if err := ls.LearnSender(context.Background(), n); err != nil || ls.FromNumber() != twFrom {
		t.Fatalf("learn the sender: %v, %q", err, ls.FromNumber())
	}
	if err := ls.LearnSender(context.Background(), n); err == nil {
		t.Error("the incoming numbers were read twice")
	}
	two := &fakeTwilio{status: 200, answer: `{"incoming_phone_numbers":[{"phone_number":"+12025550101","capabilities":{"sms":true}},{"phone_number":"+12025550102","capabilities":{"sms":true}}]}`}
	amb := newTwilio(t, 2)
	amb.From, amb.LearnFrom = "", true
	if err := amb.LearnSender(context.Background(), two); err == nil {
		t.Error("two sending numbers were not ambiguous")
	}
}

// TestTwilioSandboxHandsOnNothingThatNamesTheAccount: an answer that still
// holds a SID, a number, the account's digits or its token after the scrub
// is not handed on, whatever field it sits in.
func TestTwilioSandboxHandsOnNothingThatNamesTheAccount(t *testing.T) {
	for name, answer := range map[string]string{ //nolint:gosec // G101: made-up SIDs, not credentials
		"another number":   `{"body":"call +14155550199"}`,
		"a subaccount SID": `{"note":"SK` + `0123456789abcdef0123456789abcdef"}`,
		"the digits":       `{"note":"(303) 555-0188"}`,
		"the token":        `{"note":"` + twToken + `"}`,
	} {
		f := &fakeTwilio{answer: answer}
		s := newTwilio(t, 2)
		_, body, err := twilioDo(t, s.Transport(f), http.MethodPost, messagesURL, okForm)
		if name == "a subaccount SID" {
			// A new SID is replaced, not refused: it gets a placeholder.
			if err != nil || strings.Contains(body, "0123456789abcdef") || !strings.Contains(body, "SK0000000000000000000000000000000") {
				t.Errorf("%s: %v %s", name, err, body)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: handed on %s", name, body)
		}
	}
}

// TestTwilioRehearsalReachesNothing: a rehearsal answers from canned
// answers, applies the same refusals and the cap, and the answers go
// through the same scrub.
func TestTwilioRehearsalReachesNothing(t *testing.T) {
	boom := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("a rehearsal reached the network")
		return nil, nil
	})
	s := newTwilio(t, 2)
	s.Rehearse = true
	for i := 1; i <= 2; i++ {
		_, body, err := twilioDo(t, s.Transport(boom), http.MethodPost, messagesURL, "From=%2B15550100001&To=%2B15550100002&Body=heo+adapter+test+"+string(rune('0'+i)))
		if err != nil || !strings.Contains(body, `"status":"queued"`) || strings.Contains(body, twTo) || !strings.Contains(body, "SM0000000000000000000000000000000"+string(rune('0'+i))) {
			t.Fatalf("rehearsed send %d: %v %s", i, err, body)
		}
	}
	if _, _, err := twilioDo(t, s.Transport(boom), http.MethodPost, messagesURL, okForm); err == nil {
		t.Error("a rehearsal allowed a third send")
	}
	resp, body, err := twilioDo(t, s.Transport(boom), http.MethodPost, messagesURL, "From=%2B15550100001&To=%2B1555&Body=heo+adapter+test+0")
	if err != nil || resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "21265") {
		t.Errorf("rehearsed invalid recipient: %v %v %s", err, resp, body)
	}
	if _, _, err := twilioDo(t, s.Transport(boom), http.MethodDelete, messagesURL, ""); err == nil {
		t.Error("a rehearsal allowed a delete")
	}
	if n := s.Sent(); n != 0 {
		t.Errorf("a rehearsal wrote %d ledger slots", n)
	}
}

// TestTwilioSandboxNeedsThePlaceholderCredential: a caller that holds the
// real account SID is refused: the recorded process is told only the
// placeholder.
func TestTwilioSandboxNeedsThePlaceholderCredential(t *testing.T) {
	f := &fakeTwilio{}
	s := newTwilio(t, 2)
	req, _ := http.NewRequest(http.MethodPost, messagesURL, strings.NewReader(okForm))
	req.SetBasicAuth(twAccount, "x")
	if _, err := s.Transport(f).RoundTrip(req); err == nil || len(f.sent()) != 0 {
		t.Errorf("a caller with the real account SID was served: %v, %v", err, f.sent())
	}
	req, _ = http.NewRequest(http.MethodPost, messagesURL, strings.NewReader(okForm))
	if _, err := s.Transport(f).RoundTrip(req); err == nil || len(f.sent()) != 0 {
		t.Errorf("a caller with no credential was served: %v, %v", err, f.sent())
	}
}

func TestCheckLiveTwilio(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ok := map[string]string{"XRR_MODE": "record", EnvLive: "1", EnvTwilioTo: "+15550100002"}
	if err := CheckLiveTwilio(env(ok)); err != nil {
		t.Fatalf("a live recording: %v", err)
	}
	for name, mutate := range map[string]func(map[string]string){
		"replay":          func(m map[string]string) { m["XRR_MODE"] = "replay" },
		"passthrough":     func(m map[string]string) { m["XRR_MODE"] = "passthrough" },
		"no live flag":    func(m map[string]string) { delete(m, EnvLive) },
		"CI":              func(m map[string]string) { m["CI"] = "true" },
		"no recipient":    func(m map[string]string) { delete(m, EnvTwilioTo) },
		"a bad recipient": func(m map[string]string) { m[EnvTwilioTo] = "5550100002" },
		"redaction off":   func(m map[string]string) { m["XRR_REDACT_DISABLE"] = "1" },
	} {
		m := map[string]string{}
		for k, v := range ok {
			m[k] = v
		}
		mutate(m)
		if err := CheckLiveTwilio(env(m)); err == nil {
			t.Errorf("%s: a live recording was allowed", name)
		}
	}
}

func TestTwilioSandboxFromEnv(t *testing.T) {
	m := map[string]string{
		EnvTwilioAccountSID: twAccount, EnvTwilioToken: twToken, EnvTwilioFrom: twFrom, EnvTwilioTo: twTo,
		EnvTwilioLedger: filepath.Join(t.TempDir(), "l"),
	}
	get := func(k string) string { return m[k] }
	s, err := TwilioSandboxFromEnv(get)
	if err != nil || s.Max != 2 {
		t.Fatalf("sandbox: %+v, %v (the cap must default to 2)", s, err)
	}
	m[EnvTwilioMaxSends] = "1"
	if s, _ = TwilioSandboxFromEnv(get); s.Max != 1 {
		t.Errorf("cap = %d, want 1", s.Max)
	}
	round, err := TwilioSandboxFromEnv(func(k string) string {
		for _, kv := range s.ChildEnv() {
			if name, v, _ := strings.Cut(kv, "="); name == k {
				return v
			}
		}
		return ""
	})
	if err != nil || round.AccountSID != twAccount || round.To != twTo || round.From != twFrom || round.Max != 1 {
		t.Errorf("the child's sandbox = %+v, %v", round, err)
	}
	for k, bad := range map[string]string{EnvTwilioAccountSID: "ACnope", EnvTwilioTo: "x", EnvTwilioFrom: "x", EnvTwilioToken: "", EnvTwilioMaxSends: "many"} {
		old := m[k]
		m[k] = bad
		if _, err := TwilioSandboxFromEnv(get); err == nil {
			t.Errorf("%s=%q was accepted", k, bad)
		}
		m[k] = old
	}
	parent, err := NewTwilioSandbox(func(k string) string {
		if k == EnvTwilioFrom {
			return ""
		}
		return get(k)
	})
	if err != nil || !parent.LearnFrom {
		t.Errorf("the driving process without a sender must learn it: %+v, %v", parent, err)
	}
}

func TestDenyListsTheRecordingsLiterals(t *testing.T) {
	s := newTwilio(t, 2)
	if _, _, err := twilioDo(t, s.Transport(&fakeTwilio{}), http.MethodPost, messagesURL, okForm); err != nil {
		t.Fatal(err)
	}
	deny := strings.Join(s.Deny(), " ")
	for _, want := range []string{twAccount, twToken, twFrom, twTo, "3035550188", "2025550123", twMsg} {
		if !strings.Contains(deny, want) {
			t.Errorf("Deny lacks %q", want)
		}
	}
	d := t.TempDir()
	write(t, d, "x.json", `{"note":"text 3035550188"}`)
	if fs, _ := AuditWith(d, s.Deny()...); len(fs) == 0 {
		t.Error("the recording's own digits passed the audit")
	}
}

func TestNormalizeTwilioFormScrubsTheRunsIdentifiers(t *testing.T) {
	in := "Body=x&StatusCallback=" + url.QueryEscape("https://hooks.invalid/t?attempt=00000ad0-1&operation=00000ad0-2&keep=1")
	a := string(normalizeTwilioForm([]byte(in)))
	b := string(normalizeTwilioForm([]byte(strings.ReplaceAll(strings.ReplaceAll(in, "ad0-1", "bee-7"), "ad0-2", "bee-8"))))
	if a != b || strings.Contains(a, "ad0") || !strings.Contains(a, TwilioOperation) || !strings.Contains(a, "keep%3D1") {
		t.Errorf("normalized forms differ or keep the run's IDs:\n%s\n%s", a, b)
	}
	if got := string(normalizeTwilioForm([]byte("To=x"))); got != "To=x" {
		t.Errorf("a form with no callback changed: %q", got)
	}
}

// TestTwilioSandboxReadBackFindsWhatTheRecordedProcessSent: the process
// that drives a recording reads back, by SID, the messages another process
// sent: the ledger carries the SIDs, and the placeholders the reader
// gives them are the ones the sender saw.
func TestTwilioSandboxReadBackFindsWhatTheRecordedProcessSent(t *testing.T) {
	sender := newTwilio(t, 2)
	_, first, err := twilioDo(t, sender.Transport(&fakeTwilio{}), http.MethodPost, messagesURL, okForm)
	if err != nil {
		t.Fatal(err)
	}
	reader := newTwilio(t, 2)
	reader.Ledger = sender.Ledger
	f := &fakeTwilio{status: 200}
	got, err := reader.ReadBack(context.Background(), f)
	if err != nil || len(got) != 1 {
		t.Fatalf("read back: %v, %d messages", err, len(got))
	}
	if !strings.Contains(first, `"sid":"SM00000000000000000000000000000001"`) || !strings.Contains(string(got[0]), `"sid":"SM00000000000000000000000000000001"`) {
		t.Errorf("the sender saw %s, the reader %s: the placeholders must agree", first, got[0])
	}
	if l := f.sent(); len(l) != 1 || !strings.Contains(l[0], "GET api.twilio.com/2010-04-01/Accounts/"+twAccount+"/Messages/"+twMsg+".json") {
		t.Errorf("Twilio was asked %v, want the real SID", l)
	}
}
