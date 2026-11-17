package xrrx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment of a live Twilio recording. Every value is the recording
// process's alone: nothing here is a file or a flag.
const (
	// EnvTwilioAccountSID is the real account SID (AC...). The recorded
	// process is told the placeholder instead; the transport swaps them.
	EnvTwilioAccountSID = "HEO_RECORD_TWILIO_ACCOUNT_SID"
	// EnvTwilioToken is the account's auth token. It reaches only the
	// transport's Authorization header: heo's own credential store holds a
	// stand-in, so the token is in no file and signs no webhook fixture.
	EnvTwilioToken = "HEO_RECORD_TWILIO_AUTH_TOKEN" // #nosec G101 -- the name of a variable, not a credential
	// EnvTwilioFrom is the account's sending number, E.164. When unset the
	// recording's parent reads it once from the account's incoming numbers
	// and passes it on (TwilioSandbox.ChildEnv).
	EnvTwilioFrom = "HEO_RECORD_TWILIO_FROM"
	// EnvTwilioTo is the one number a recording may text: the owner's own.
	EnvTwilioTo = "HEO_RECORD_TWILIO_TO"
	// EnvTwilioMaxSends caps the messages ever sent to that number; two
	// when unset.
	EnvTwilioMaxSends = "HEO_RECORD_TWILIO_MAX_SENDS"
	// EnvTwilioLedger names the directory that remembers the sends already
	// made, across runs; a user cache directory when unset.
	EnvTwilioLedger = "HEO_RECORD_TWILIO_LEDGER"
	// EnvTwilioRehearse, "1", sends nothing anywhere: the transport answers
	// from canned answers shaped like Twilio's, so a recording's whole path
	// can be rehearsed before it spends a send. Its cassettes are
	// worthless and are cleared by the next recording.
	EnvTwilioRehearse = "HEO_RECORD_TWILIO_REHEARSE"

	twilioHost = "api.twilio.com"
	// DefaultTwilioMaxSends is the cap when EnvTwilioMaxSends is unset.
	DefaultTwilioMaxSends = 2
)

// What a Twilio recording leaves in place of the account and the numbers.
const (
	// TwilioAccountSID replaces the account SID.
	TwilioAccountSID = "AC00000000000000000000000000000001"
	// TwilioSender replaces the account's sending number.
	TwilioSender = "+15550100001"
	// TwilioRecipient replaces the number the recording texts.
	TwilioRecipient = "+15550100002"
	// TwilioDate replaces every date Twilio reports.
	TwilioDate = "Mon, 01 Jan 2024 00:00:00 +0000"
	// TwilioBodyPrefix starts every text a recording sends.
	TwilioBodyPrefix = "heo adapter test"
	// twilioMaxBody bounds the text of a send.
	twilioMaxBody = 64
)

var (
	// twilioSIDRe is a Twilio SID of any resource: two capitals and 32 hex.
	twilioSIDRe = regexp.MustCompile(`\b([A-Z]{2})([0-9a-fA-F]{32})\b`)
	// twilioE164Re is a phone number as the API writes it, or as a form
	// body encodes its plus.
	twilioE164Re = regexp.MustCompile(`(?:\+|%2[bB])[1-9][0-9]{6,14}\b`)
	// placeholderSIDRe is what a SID is replaced with.
	placeholderSIDRe = regexp.MustCompile(`^[A-Z]{2}0{30}[0-9]{2}$`)
	// invalidToRe is a recipient no one can have: too short to be a number.
	// Twilio refuses it (a 400, error 21265) before anything is created or billed.
	invalidToRe = regexp.MustCompile(`^\+[0-9]{1,6}$`)
	e164Re      = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

// IsTwilioPlaceholder reports whether n is one of the numbers a recording
// leaves in place of a real one.
func IsTwilioPlaceholder(n string) bool { return n == TwilioSender || n == TwilioRecipient }

// CheckLiveTwilio says whether this environment may send recorded traffic
// to Twilio: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI, redaction on, and
// the recipient named as a number. Replay and passthrough never may.
func CheckLiveTwilio(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "Twilio"); err != nil {
		return err
	}
	if !e164Re.MatchString(getenv(EnvTwilioTo)) {
		return fmt.Errorf("xrrx: %s names the one number a recording may text, in E.164", EnvTwilioTo)
	}
	return nil
}

// TwilioSandbox is the one Twilio account and the one recipient a live
// recording reaches, and what the recording must never keep of them. The
// recorded process is told the placeholder account and numbers
// (TwilioAccountSID, TwilioSender, TwilioRecipient) and a stand-in auth
// token; Transport translates between those and the real ones, counts
// every send against the cap, and refuses everything else.
type TwilioSandbox struct {
	// AccountSID and Token are the real account's.
	AccountSID, Token string
	// From is the account's sending number and To the recipient, E.164.
	From, To string
	// Max caps the sends the ledger will ever allow.
	Max int
	// Ledger is the directory that remembers the sends made.
	Ledger string
	// Rehearse answers from canned answers and reaches nothing.
	Rehearse bool
	// LearnFrom reads From from the account's incoming numbers, once.
	LearnFrom bool

	mu        sync.Mutex
	sids      map[string]string
	counters  map[string]int
	read      bool
	rehearsed int
}

// NewTwilioSandbox is the sandbox of the process that drives a recording:
// the sending number is learned from the account when not given.
func NewTwilioSandbox(getenv func(string) string) (*TwilioSandbox, error) {
	s, err := twilioFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	s.LearnFrom = s.From == ""
	return s, nil
}

// TwilioSandboxFromEnv is the sandbox of the recorded process: the sending
// number is given.
func TwilioSandboxFromEnv(getenv func(string) string) (*TwilioSandbox, error) {
	s, err := twilioFromEnv(getenv)
	if err != nil {
		return nil, err
	}
	if !e164Re.MatchString(s.From) {
		return nil, fmt.Errorf("xrrx: %s names the account's sending number, in E.164", EnvTwilioFrom)
	}
	return s, nil
}

func twilioFromEnv(getenv func(string) string) (*TwilioSandbox, error) {
	s := &TwilioSandbox{
		AccountSID: getenv(EnvTwilioAccountSID), Token: getenv(EnvTwilioToken),
		From: getenv(EnvTwilioFrom), To: getenv(EnvTwilioTo),
		Max: DefaultTwilioMaxSends, Ledger: getenv(EnvTwilioLedger), Rehearse: getenv(EnvTwilioRehearse) == "1",
	}
	if !twilioSIDRe.MatchString(s.AccountSID) || !strings.HasPrefix(s.AccountSID, "AC") {
		return nil, fmt.Errorf("xrrx: %s is not an account SID", EnvTwilioAccountSID)
	}
	if s.Token == "" {
		return nil, fmt.Errorf("xrrx: %s is not set", EnvTwilioToken)
	}
	if !e164Re.MatchString(s.To) {
		return nil, fmt.Errorf("xrrx: %s names the one number a recording may text, in E.164", EnvTwilioTo)
	}
	if v := getenv(EnvTwilioMaxSends); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("xrrx: %s is not a number of sends", EnvTwilioMaxSends)
		}
		s.Max = n
	}
	if s.Ledger == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("xrrx: no ledger directory: %w (set %s)", err, EnvTwilioLedger)
		}
		s.Ledger = filepath.Join(dir, "heo", "twilio-record-sends")
	}
	if s.From != "" && s.From == s.To {
		return nil, errors.New("xrrx: the recipient is the sending number")
	}
	return s, nil
}

// ChildEnv is what the recorded process is told of the sandbox.
func (s *TwilioSandbox) ChildEnv() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := []string{
		EnvLive + "=1", EnvTwilioAccountSID + "=" + s.AccountSID, EnvTwilioToken + "=" + s.Token,
		EnvTwilioFrom + "=" + s.From, EnvTwilioTo + "=" + s.To,
		EnvTwilioMaxSends + "=" + strconv.Itoa(s.Max), EnvTwilioLedger + "=" + s.Ledger,
	}
	if s.Rehearse {
		env = append(env, EnvTwilioRehearse+"=1")
	}
	return env
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the account SID, the numbers (as written and as digits), the
// auth token and every SID the sandbox has replaced.
func (s *TwilioSandbox) Deny() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{s.AccountSID, s.Token, s.From, s.To, digitsOf(s.From), digitsOf(s.To), nationalOf(s.From), nationalOf(s.To)}
	for real := range s.sids {
		out = append(out, real)
	}
	return out
}

func digitsOf(n string) string { return strings.TrimPrefix(n, "+") }

// nationalOf is the last ten digits of n, the form a person writes.
func nationalOf(n string) string {
	d := digitsOf(n)
	if len(d) > 10 {
		d = d[len(d)-10:]
	}
	return d
}

// Sent is how many sends the ledger holds.
func (s *TwilioSandbox) Sent() int {
	ents, _ := os.ReadDir(s.Ledger)
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "send-") {
			n++
		}
	}
	return n
}

// takeSlot claims one of the sends the cap allows, before the request
// leaves: a send whose answer is lost still counts. The claim is a file
// created exclusively, so concurrent processes cannot both take the last.
func (s *TwilioSandbox) takeSlot() (int, error) {
	if err := os.MkdirAll(s.Ledger, 0o700); err != nil { //nolint:gosec // G703: the ledger directory the recording was given
		return 0, fmt.Errorf("xrrx: Twilio sandbox: no ledger, send refused: %w", err)
	}
	for n := 1; n <= s.Max; n++ {
		f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
		if err == nil {
			_, _ = fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
			return n, f.Close()
		}
		if !os.IsExist(err) {
			return 0, fmt.Errorf("xrrx: Twilio sandbox: ledger: %w", err)
		}
	}
	return 0, fmt.Errorf("xrrx: Twilio sandbox: the cap of %d sends is spent, send refused", s.Max)
}

// Transport returns the RoundTripper that sends requests to the account
// through base (http.DefaultTransport when nil). A request must be to
// https://api.twilio.com and be one of: POST Messages.json for the
// placeholder account, from the placeholder sender to the placeholder
// recipient (the owner's number) or to a number too short to exist, with a
// short text starting "heo adapter test" and no media, link or status
// callback; GET of Messages.json or one message; and, once, GET of the
// account's incoming numbers. Everything else fails before it is sent. A
// send to the recipient takes one of the ledger's slots first. The
// answer comes back with the account, SIDs, numbers, dates and billing
// replaced and the churning headers dropped; an answer that still names
// what it must not is not handed on.
func (s *TwilioSandbox) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out, kind, err := s.translate(req)
		if err != nil {
			return nil, err
		}
		var resp *http.Response
		switch {
		case s.Rehearse:
			resp, err = s.canned(req, kind)
		default:
			resp, err = s.send(base, out, kind)
		}
		if err != nil {
			return nil, err
		}
		return s.scrubResponse(resp)
	})
}

type twilioKind int

const (
	kindSend twilioKind = iota
	kindSendInvalid
	kindList
	kindFetch
	kindNumbers
)

func (s *TwilioSandbox) send(base http.RoundTripper, out *http.Request, kind twilioKind) (*http.Response, error) {
	slot := 0
	if kind == kindSend {
		n, err := s.takeSlot()
		if err != nil {
			return nil, err
		}
		slot = n
	}
	resp, err := base.RoundTrip(out)
	if err != nil {
		return nil, s.scrubbedError(err)
	}
	if kind == kindSendInvalid && resp.StatusCode/100 == 2 {
		// Twilio accepted what should be refused: it counts as a send.
		_, _ = s.takeSlot()
		_ = resp.Body.Close()
		return nil, errors.New("xrrx: Twilio sandbox: an invalid recipient was accepted; the answer is not handed on")
	}
	if slot > 0 && resp.StatusCode/100 == 2 {
		resp = s.noteSID(slot, resp)
	}
	return resp, nil
}

// scrubbedError is err with the account, SIDs and numbers a transport
// error spells out (it names the request's URL) replaced, and the auth
// token removed. The original stays reachable through Unwrap.
func (s *TwilioSandbox) scrubbedError(err error) error {
	msg := s.scrubString(err.Error())
	if s.Token != "" {
		msg = strings.ReplaceAll(msg, s.Token, "<token>")
	}
	return &scrubbedErr{msg: msg, err: err}
}

type scrubbedErr struct {
	msg string
	err error
}

func (e *scrubbedErr) Error() string { return e.msg }
func (e *scrubbedErr) Unwrap() error { return e.err }

// noteSID writes the SID Twilio gave a send into its ledger slot, so the
// process that drives the recording can read the message back, and hands
// the answer on unread.
func (s *TwilioSandbox) noteSID(slot int, resp *http.Response) *http.Response {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return resp
	}
	var m struct {
		SID string `json:"sid"`
	}
	if json.Unmarshal(raw, &m) != nil || !twilioSIDRe.MatchString(m.SID) {
		return resp
	}
	f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(slot)), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
	if err == nil {
		_, _ = fmt.Fprintln(f, m.SID)
		_ = f.Close()
	}
	return resp
}

// ReadBack fetches, through the sandbox, each message the ledger holds a
// SID for, in the order they were sent, and returns the scrubbed
// resources. The placeholders it gives them are those the recorded
// process saw.
func (s *TwilioSandbox) ReadBack(ctx context.Context, base http.RoundTripper) ([]json.RawMessage, error) {
	var out []json.RawMessage
	c := s.Client(base)
	for n := 1; n <= s.Max; n++ {
		b, err := os.ReadFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n))) //nolint:gosec // G304: the ledger directory the recording was given
		if err != nil {
			break
		}
		lines := strings.Fields(string(b))
		if len(lines) < 2 {
			continue
		}
		p := s.scrubString(lines[1])
		raw, err := c.Get(ctx, "/2010-04-01/Accounts/"+TwilioAccountSID+"/Messages/"+p+".json", "")
		if err != nil {
			return out, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// translate is req aimed at the real account, with the real credential, or
// an error when req may not be sent.
func (s *TwilioSandbox) translate(req *http.Request) (*http.Request, twilioKind, error) {
	u := req.URL
	refuse := func(why string) (*http.Request, twilioKind, error) {
		return nil, 0, fmt.Errorf("xrrx: Twilio sandbox: %s %s refused: %s", req.Method, u.Redacted(), why)
	}
	if u.Scheme != "https" || u.Host != twilioHost || u.User != nil {
		return refuse("only https://" + twilioHost)
	}
	if user, _, ok := req.BasicAuth(); !ok || user != TwilioAccountSID {
		return refuse("the caller must hold the placeholder account, not a real one")
	}
	p := u.EscapedPath()
	if p != path.Clean(p) || strings.Contains(p, "%") {
		return refuse("path is not canonical")
	}
	prefix := "/2010-04-01/Accounts/" + TwilioAccountSID
	if !strings.HasPrefix(p, prefix+"/") {
		return refuse("only the sandbox account's endpoints")
	}
	rest := strings.TrimPrefix(p, prefix)
	out := req.Clone(req.Context())
	out.Host = ""
	out.URL.RawPath = ""
	out.URL.Path = "/2010-04-01/Accounts/" + s.AccountSID + rest
	out.Header = http.Header{"Accept": {"application/json"}}
	out.SetBasicAuth(s.AccountSID, s.Token)

	switch {
	case req.Method == http.MethodPost && rest == "/Messages.json":
		return s.translateSend(req, out)
	case req.Method == http.MethodGet && rest == "/Messages.json":
		if err := s.translateQuery(out, "To", "From"); err != nil {
			return refuse(err.Error())
		}
		return out, kindList, nil
	case req.Method == http.MethodGet && strings.HasPrefix(rest, "/Messages/") && strings.HasSuffix(rest, ".json"):
		sid := strings.TrimSuffix(strings.TrimPrefix(rest, "/Messages/"), ".json")
		real, ok := s.realSID(sid)
		if !ok || (!strings.HasPrefix(sid, "SM") && !strings.HasPrefix(sid, "MM")) {
			return refuse("only a message this sandbox has answered with")
		}
		out.URL.Path = "/2010-04-01/Accounts/" + s.AccountSID + "/Messages/" + real + ".json"
		out.URL.RawQuery = ""
		return out, kindFetch, nil
	case req.Method == http.MethodGet && rest == "/IncomingPhoneNumbers.json":
		s.mu.Lock()
		again := s.read
		s.read = true
		s.mu.Unlock()
		if again {
			return refuse("the incoming numbers are read once")
		}
		out.URL.RawQuery = "PageSize=20"
		return out, kindNumbers, nil
	}
	return refuse("only POST Messages.json, GET Messages.json, GET one message and GET the incoming numbers")
}

func (s *TwilioSandbox) translateQuery(out *http.Request, numbers ...string) error {
	q := out.URL.Query()
	allowed := map[string]bool{"PageSize": true, "Page": true, "PageToken": true, "DateSent": true, "DateSent<": true, "DateSent>": true}
	for _, n := range numbers {
		allowed[n] = true
	}
	for k := range q {
		if !allowed[k] {
			return fmt.Errorf("query %q is not allowed", k)
		}
	}
	for _, k := range numbers {
		if v := q.Get(k); v != "" {
			real, ok := s.realNumber(v)
			if !ok {
				return fmt.Errorf("%s is not the sandbox's sender or recipient", k)
			}
			q.Set(k, real)
		}
	}
	out.URL.RawQuery = q.Encode()
	return nil
}

func (s *TwilioSandbox) translateSend(req, out *http.Request) (*http.Request, twilioKind, error) {
	refuse := func(why string) (*http.Request, twilioKind, error) {
		return nil, 0, fmt.Errorf("xrrx: Twilio sandbox: %s %s refused: %s", req.Method, req.URL.Redacted(), why)
	}
	if req.Body == nil {
		return refuse("no form")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<16))
	_ = req.Body.Close()
	if err != nil {
		return refuse("unreadable form")
	}
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		return refuse("not a form")
	}
	for k, vs := range form {
		switch k {
		case "To", "From", "Body", "StatusCallback":
		default:
			return refuse("field " + strconv.Quote(k) + " is not allowed (text only: To, From, Body)")
		}
		if len(vs) != 1 {
			return refuse("field " + strconv.Quote(k) + " is repeated")
		}
	}
	body := form.Get("Body")
	if !strings.HasPrefix(body, TwilioBodyPrefix) || len(body) > twilioMaxBody || strings.Contains(body, "://") || strings.ContainsAny(body, "\n\r") {
		return refuse("the text must start " + strconv.Quote(TwilioBodyPrefix) + ", be short and carry no link")
	}
	if form.Get("From") != TwilioSender {
		return refuse("only from the sandbox's sender")
	}
	kind := kindSend
	to := form.Get("To")
	switch {
	case to == TwilioRecipient:
	case invalidToRe.MatchString(to):
		kind = kindSendInvalid
	default:
		return refuse("only to the sandbox's recipient")
	}
	real := url.Values{"From": {s.From}, "To": {to}, "Body": {body}}
	if to == TwilioRecipient {
		real.Set("To", s.To)
	}
	enc := real.Encode()
	out.Body = io.NopCloser(strings.NewReader(enc))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(enc)), nil }
	out.ContentLength = int64(len(enc))
	out.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return out, kind, nil
}

// realNumber maps a placeholder number to the real one.
func (s *TwilioSandbox) realNumber(n string) (string, bool) {
	switch n {
	case TwilioSender:
		return s.From, true
	case TwilioRecipient:
		return s.To, true
	}
	return "", false
}

// realSID maps a placeholder SID the sandbox gave out to the real one.
func (s *TwilioSandbox) realSID(placeholder string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for real, p := range s.sids {
		if p == placeholder {
			return real, true
		}
	}
	return "", false
}

var twilioDropHeaderRe = regexp.MustCompile(`(?i)^(x-twilio-|x-request-id|x-home-region|x-shenanigans|twilio-|x-ratelimit-|x-cache|x-amz|via$|server$|date$|strict-transport|access-control|etag|connection|vary)`)

func (s *TwilioSandbox) scrubResponse(resp *http.Response) (*http.Response, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	s.learn(raw)
	body := raw
	if len(bytes.TrimSpace(raw)) > 0 {
		if body, err = s.ScrubJSON(raw); err != nil {
			body = []byte(s.scrubString(string(raw)))
		}
	}
	for k, vs := range resp.Header {
		if twilioDropHeaderRe.MatchString(k) {
			delete(resp.Header, k)
			continue
		}
		for i, v := range vs {
			vs[i] = s.scrubString(v)
		}
		resp.Header[k] = vs
	}
	all := string(body)
	for _, vs := range resp.Header {
		all += "\n" + strings.Join(vs, "\n")
	}
	if err := s.residue(all); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
	return resp, nil
}

// residue fails when text still names what the sandbox exists to keep out.
func (s *TwilioSandbox) residue(text string) error {
	from, to := s.FromNumber(), s.To
	fail := func(what string) error {
		return fmt.Errorf("xrrx: Twilio sandbox: the answer still holds %s after the scrub, not handed on", what)
	}
	for _, m := range twilioSIDRe.FindAllString(text, -1) {
		if !placeholderSIDRe.MatchString(m) && m != TwilioAccountSID {
			return fail("a SID")
		}
	}
	for _, m := range twilioE164Re.FindAllString(text, -1) {
		if !IsTwilioPlaceholder(strings.Replace(strings.Replace(m, "%2B", "+", 1), "%2b", "+", 1)) {
			return fail("a phone number")
		}
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, text)
	for _, n := range []string{nationalOf(from), nationalOf(to)} {
		if n != "" && len(n) >= 7 && strings.Contains(digits, n) {
			return fail("a number's digits")
		}
	}
	low := strings.ToLower(text)
	for _, lit := range []string{s.AccountSID, s.Token} {
		if lit != "" && strings.Contains(low, strings.ToLower(lit)) {
			return fail("the account")
		}
	}
	return nil
}

// learn reads the sending number from the account's incoming numbers: the
// one that can send SMS.
func (s *TwilioSandbox) learn(raw []byte) {
	if !s.LearnFrom {
		return
	}
	var v struct {
		Numbers []struct {
			PhoneNumber  string `json:"phone_number"`
			Capabilities struct {
				SMS bool `json:"sms"`
			} `json:"capabilities"`
		} `json:"incoming_phone_numbers"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	var sms []string
	for _, n := range v.Numbers {
		if n.Capabilities.SMS && e164Re.MatchString(n.PhoneNumber) {
			sms = append(sms, n.PhoneNumber)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(sms) == 1 && s.From == "" {
		s.From = sms[0]
	}
}

// FromNumber is the sending number, once known.
func (s *TwilioSandbox) FromNumber() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.From
}

// scrubString replaces the account, every SID and the two numbers.
func (s *TwilioSandbox) scrubString(in string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrubLocked(in)
}

func (s *TwilioSandbox) scrubLocked(in string) string {
	if s.sids == nil {
		s.sids, s.counters = map[string]string{}, map[string]int{}
	}
	// The account's own placeholder is AC...01: others start after it.
	if s.counters["AC"] == 0 {
		s.counters["AC"] = 1
	}
	in = twilioSIDRe.ReplaceAllStringFunc(in, func(sid string) string {
		if sid == s.AccountSID || strings.EqualFold(sid, s.AccountSID) {
			return TwilioAccountSID
		}
		if p, ok := s.sids[sid]; ok {
			return p
		}
		prefix := sid[:2]
		s.counters[prefix]++
		p := fmt.Sprintf("%s%030d%02d", prefix, 0, s.counters[prefix])
		s.sids[sid] = p
		return p
	})
	for real, p := range map[string]string{s.From: TwilioSender, s.To: TwilioRecipient} {
		if real != "" {
			in = strings.ReplaceAll(in, real, p)
			in = strings.ReplaceAll(in, url.QueryEscape(real), url.QueryEscape(p))
		}
	}
	return in
}

// ScrubJSON rewrites a Twilio JSON document so it names no one: the
// account, every SID and the two numbers are replaced wherever they
// appear in a string, dates and billing are fixed, and a list of messages
// keeps only the sandbox's own texts. Keys come out sorted.
func (s *TwilioSandbox) ScrubJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("xrrx: more than one JSON document")
	}
	s.mu.Lock()
	v = s.scrubValue(v)
	s.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (s *TwilioSandbox) scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		return s.scrubLocked(t)
	case []any:
		for i := range t {
			t[i] = s.scrubValue(t[i])
		}
		return t
	case map[string]any:
		if msgs, ok := t["messages"].([]any); ok {
			kept := s.ownTexts(msgs)
			t["messages"] = kept
			// The page's own bounds, not the account's history behind it.
			for k, v := range map[string]int{"start": 0, "end": max(len(kept)-1, 0)} {
				if _, has := t[k]; has {
					t[k] = json.Number(strconv.Itoa(v))
				}
			}
			for _, k := range []string{"next_page_uri", "previous_page_uri"} {
				if _, has := t[k]; has {
					t[k] = nil
				}
			}
		}
		for k, c := range t {
			switch k {
			case "date_created", "date_updated", "date_sent":
				if _, ok := c.(string); ok {
					t[k] = TwilioDate
					continue
				}
			case "price":
				if c != nil {
					t[k] = "-0.00000"
					continue
				}
			case "price_unit":
				if c != nil {
					t[k] = "USD"
					continue
				}
			case "num_segments":
				if _, ok := c.(string); ok {
					t[k] = "1"
					continue
				}
			case "messaging_service_sid":
				t[k] = nil
				continue
			case "friendly_name":
				t[k] = "sandbox-account"
				continue
			}
			t[k] = s.scrubValue(c)
		}
		return t
	}
	return v
}

// ownTexts keeps the messages of a list that are the sandbox's own: a
// test text to the recipient. The rest of an account's history is not the
// recording's to keep.
func (s *TwilioSandbox) ownTexts(msgs []any) []any {
	out := []any{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		body, _ := mm["body"].(string)
		to, _ := mm["to"].(string)
		if strings.HasPrefix(body, TwilioBodyPrefix) && to == s.To {
			out = append(out, s.scrubValue(m))
		}
	}
	return out
}

// canned is the answer a rehearsal gives instead of Twilio's.
func (s *TwilioSandbox) canned(req *http.Request, kind twilioKind) (*http.Response, error) {
	status, body := http.StatusOK, ""
	switch kind {
	case kindSendInvalid:
		status = http.StatusBadRequest
		body = `{"code":21265,"message":"'To' number cannot be a Short Code: +XXXX","more_info":"https://www.twilio.com/docs/errors/21265","status":400}`
	case kindSend:
		if err := s.takeRehearsal(); err != nil {
			return nil, err
		}
		var raw []byte
		if req.GetBody != nil {
			rc, _ := req.GetBody()
			raw, _ = io.ReadAll(rc)
		}
		form, _ := url.ParseQuery(string(raw))
		s.mu.Lock()
		s.rehearsed++
		n := s.rehearsed
		s.mu.Unlock()
		status = http.StatusCreated
		b, _ := json.Marshal(map[string]any{
			"account_sid": s.AccountSID, "api_version": "2010-04-01", "body": form.Get("Body"),
			"date_created": "Fri, 01 Jan 2027 10:00:00 +0000", "date_sent": nil, "date_updated": "Fri, 01 Jan 2027 10:00:00 +0000",
			"direction": "outbound-api", "error_code": nil, "error_message": nil, "from": s.From, "messaging_service_sid": nil,
			"num_media": "0", "num_segments": "1", "price": nil, "price_unit": "USD",
			"sid": fmt.Sprintf("SM%032x", n+0xfeed), "status": "queued", "to": s.To,
			"uri": fmt.Sprintf("/2010-04-01/Accounts/%s/Messages/SM%032x.json", s.AccountSID, n+0xfeed),
		})
		body = string(b)
	case kindNumbers:
		b, _ := json.Marshal(map[string]any{"incoming_phone_numbers": []any{map[string]any{"phone_number": s.From, "capabilities": map[string]any{"sms": true}}}})
		body = string(b)
	default:
		body = `{"messages":[]}`
	}
	h := http.Header{"Content-Type": {"application/json"}}
	if kind == kindSend {
		h.Set("Location", "https://api.twilio.com/2010-04-01/Accounts/"+s.AccountSID+"/Messages/SM"+fmt.Sprintf("%032x", s.rehearsed+0xfeed)+".json")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// takeRehearsal counts a rehearsed send in memory only: a rehearsal has no
// ledger, and spends nothing.
func (s *TwilioSandbox) takeRehearsal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rehearsed >= s.Max {
		return fmt.Errorf("xrrx: Twilio sandbox: the cap of %d sends is spent, send refused", s.Max)
	}
	return nil
}

// TwilioClient is a Twilio sandbox client for the process that drives a
// recording: it learns the sending number and reads back what the recorded
// process sent. It goes through Transport, so it can reach nothing else.
type TwilioClient struct {
	hc *http.Client
}

// Client returns a client over base (the default transport when nil).
func (s *TwilioSandbox) Client(base http.RoundTripper) *TwilioClient {
	return &TwilioClient{hc: &http.Client{Transport: s.Transport(base)}}
}

// Get requests p, which names the placeholder account
// (/2010-04-01/Accounts/AC.../Messages.json), and returns the scrubbed answer.
func (c *TwilioClient) Get(ctx context.Context, p, query string) (json.RawMessage, error) {
	u := url.URL{Scheme: "https", Host: twilioHost, Path: p, RawQuery: query}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(TwilioAccountSID, "stand-in")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return b, fmt.Errorf("xrrx: Twilio sandbox: GET %s: %s", p, resp.Status)
	}
	return b, nil
}

// LearnSender reads the sending number from the account's incoming
// numbers, once, when it was not given. It fails unless exactly one number
// can send SMS.
func (s *TwilioSandbox) LearnSender(ctx context.Context, base http.RoundTripper) error {
	if !s.LearnFrom {
		return nil
	}
	if _, err := s.Client(base).Get(ctx, "/2010-04-01/Accounts/"+TwilioAccountSID+"/IncomingPhoneNumbers.json", ""); err != nil {
		return err
	}
	if s.FromNumber() == "" {
		return fmt.Errorf("xrrx: Twilio sandbox: not exactly one number can send SMS; set %s", EnvTwilioFrom)
	}
	return nil
}

// Twilio's status callback URL carries the operation and attempt heo
// tagged the send with: new on every run, so the recorded request names
// placeholders, as for the echo marker.
const (
	// TwilioOperation and TwilioAttempt replace them in a recorded send.
	TwilioOperation = "operation-id"
	TwilioAttempt   = "attempt-id"
)

// normalizeTwilioForm is a Twilio send's recorded form: the status
// callback URL without the run's operation and attempt. Anything that is
// not a form is returned as it is.
func normalizeTwilioForm(body []byte) []byte {
	form, err := url.ParseQuery(string(body))
	if err != nil || form.Get("StatusCallback") == "" {
		return body
	}
	cb, err := url.Parse(form.Get("StatusCallback"))
	if err != nil {
		return body
	}
	q := cb.Query()
	for k, v := range map[string]string{"operation": TwilioOperation, "attempt": TwilioAttempt} {
		if q.Has(k) {
			q.Set(k, v)
		}
	}
	cb.RawQuery = q.Encode()
	form.Set("StatusCallback", cb.String())
	return []byte(form.Encode())
}
