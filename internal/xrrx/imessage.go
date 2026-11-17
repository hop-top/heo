package xrrx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Environment of a live iMessage recording. heo cannot record the
// relay.messages adapter itself, since the gateway is the operator's own;
// what it records is the path through uma's reference gateway
// (cmd/imessagegateway in the uma repository): heo posts a first
// message to the gateway's callback, the gateway sends it through an
// iMessage backend (Blooio, or BlueBubbles Server on a Mac), and the
// recipient's reply comes back through the backend's webhook into the same
// heo conversation. The requests the gateway makes to the backend are what
// the cassettes hold. Every value is the recording process's alone.
const (
	// EnvIMessageBackend is "blooio" or "bluebubbles".
	EnvIMessageBackend = "HEO_RECORD_IMESSAGE_BACKEND"
	// EnvIMessageTo is the one handle the recording writes to, the
	// maintainers' own: a phone number in E.164 or an Apple ID's email
	// address, on a device they hold, to reply from.
	EnvIMessageTo = "HEO_RECORD_IMESSAGE_TO"
	// EnvIMessageListen is the address the recording listens on for the
	// backend's webhook, reachable by the backend (behind a tunnel for a
	// hosted one, a LAN or tailnet address for BlueBubbles).
	EnvIMessageListen = "HEO_RECORD_IMESSAGE_LISTEN"
	// EnvIMessageMaxSends caps the sends ever made.
	EnvIMessageMaxSends = "HEO_RECORD_IMESSAGE_MAX_SENDS"
	// EnvIMessageLedger names the directory that remembers the sends already
	// made, across runs; a user cache directory when unset.
	EnvIMessageLedger = "HEO_RECORD_IMESSAGE_LEDGER"
	// EnvIMessageRehearse, "1", reaches nothing: the transport answers from
	// canned shapes of the backend's send answer, so a recording's whole
	// path can be rehearsed before it sends anything.
	EnvIMessageRehearse = "HEO_RECORD_IMESSAGE_REHEARSE"

	// Blooio: the hosted relay's key, the line it sends from (E.164) and the
	// signing secret of its webhook (whsec_...).
	EnvBlooioAPIKey        = "HEO_RECORD_IMESSAGE_BLOOIO_API_KEY"
	EnvBlooioFrom          = "HEO_RECORD_IMESSAGE_BLOOIO_FROM"
	EnvBlooioWebhookSecret = "HEO_RECORD_IMESSAGE_BLOOIO_WEBHOOK_SECRET"
	// BlueBubbles Server on a Mac: its address, its password, the Apple ID
	// the Mac sends from (the "to" of inbound messages) and the random token
	// in the path of its unsigned webhook.
	EnvBlueBubblesURL          = "HEO_RECORD_IMESSAGE_BLUEBUBBLES_URL"
	EnvBlueBubblesPassword     = "HEO_RECORD_IMESSAGE_BLUEBUBBLES_PASSWORD" // #nosec G101 -- the name of a variable, not a credential
	EnvBlueBubblesAccount      = "HEO_RECORD_IMESSAGE_BLUEBUBBLES_ACCOUNT"
	EnvBlueBubblesWebhookToken = "HEO_RECORD_IMESSAGE_BLUEBUBBLES_WEBHOOK_TOKEN" // #nosec G101 -- the name of a variable, not a credential

	// DefaultIMessageMaxSends is the cap when EnvIMessageMaxSends is unset:
	// the recording sends one message, and one more is spare for a run that
	// failed midway.
	DefaultIMessageMaxSends = 2
)

// The backends a recording goes through, and what a recording leaves.
const (
	IMessageBackendBlooio      = "blooio"
	IMessageBackendBlueBubbles = "bluebubbles"

	// IMessageBlooioBase is the Blooio v4 API, which a recording reaches.
	IMessageBlooioBase = "https://api.blooio.com/v4"
	// IMessageTextPrefix starts every text a recording sends.
	IMessageTextPrefix = "heo adapter test"
	// IMessageText is the one message the recording sends; IMessageReply
	// what the person holding the recipient's device answers, exactly.
	IMessageText  = IMessageTextPrefix + ": please reply with the words heo adapter test reply"
	IMessageReply = "heo adapter test reply"
	// imessageMaxText bounds the text of a send.
	imessageMaxText = 96

	bluebubblesSendPath = "/api/v1/message/text"
	bluebubblesChat     = "iMessage;-;"
)

var (
	imessagePhoneRe = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	imessageEmailRe = regexp.MustCompile(`^[^@\s/]+@[^@\s/]+\.[^@\s/]+$`)
	imessageTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,}$`)
)

// CheckLiveIMessage says whether this environment may send recorded traffic
// to an iMessage backend: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI,
// redaction on, and the backend, its credentials, the one recipient and the
// webhook listener named. Replay and passthrough never may.
func CheckLiveIMessage(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "iMessage"); err != nil {
		return err
	}
	_, err := imessageFromEnv(getenv)
	return err
}

// IMessageSandbox is the one backend and the one handle a live recording
// reaches, and what the recording must never keep of them. The requests the
// reference gateway makes to the backend go through Transport, which allows
// the backend's send call to the one recipient and nothing else, and counts
// every send against the cap.
type IMessageSandbox struct {
	// Backend is IMessageBackendBlooio or IMessageBackendBlueBubbles.
	Backend string
	// To is the recipient, an email address lower-cased.
	To string
	// Sender is the line the backend sends from: Blooio's number, or the
	// Apple ID of the Mac.
	Sender string
	// Base is the real backend's address, without a trailing slash: the
	// Blooio v4 API, or the BlueBubbles server.
	Base string
	// APIKey (Blooio), Password and WebhookToken (BlueBubbles) and
	// WebhookSecret (Blooio) are the backend's credentials, the recording
	// process's alone.
	APIKey, Password, WebhookSecret, WebhookToken string
	// Listen is the address the webhook is received on.
	Listen string
	// Max caps the sends the ledger will ever allow.
	Max int
	// Ledger is the directory that remembers the sends made.
	Ledger string
	// Rehearse answers from canned shapes and reaches nothing.
	Rehearse bool
}

// IMessageSandboxFromEnv is the sandbox of a process that records: the
// values come from the environment and are checked.
func IMessageSandboxFromEnv(getenv func(string) string) (*IMessageSandbox, error) {
	return imessageFromEnv(getenv)
}

func imessageFromEnv(getenv func(string) string) (*IMessageSandbox, error) {
	s := &IMessageSandbox{
		Backend: getenv(EnvIMessageBackend), Listen: getenv(EnvIMessageListen), Ledger: getenv(EnvIMessageLedger),
		Max: DefaultIMessageMaxSends, Rehearse: getenv(EnvIMessageRehearse) == "1",
	}
	to := strings.TrimSpace(getenv(EnvIMessageTo))
	switch {
	case imessagePhoneRe.MatchString(to):
	case imessageEmailRe.MatchString(to):
		to = strings.ToLower(to)
	default:
		return nil, fmt.Errorf("xrrx: %s is not a handle: want a phone number in E.164 or an email address", EnvIMessageTo)
	}
	s.To = to
	need := func(name string) (string, error) {
		if v := getenv(name); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("xrrx: iMessage recording needs %s", name)
	}
	var err error
	switch s.Backend {
	case IMessageBackendBlooio:
		s.Base = IMessageBlooioBase
		for _, f := range []struct {
			name string
			into *string
		}{{EnvBlooioAPIKey, &s.APIKey}, {EnvBlooioFrom, &s.Sender}, {EnvBlooioWebhookSecret, &s.WebhookSecret}} {
			if *f.into, err = need(f.name); err != nil {
				return nil, err
			}
		}
		if !imessagePhoneRe.MatchString(s.Sender) {
			return nil, fmt.Errorf("xrrx: %s is not a number in E.164", EnvBlooioFrom)
		}
	case IMessageBackendBlueBubbles:
		var raw string
		for _, f := range []struct {
			name string
			into *string
		}{{EnvBlueBubblesURL, &raw}, {EnvBlueBubblesPassword, &s.Password}, {EnvBlueBubblesAccount, &s.Sender}, {EnvBlueBubblesWebhookToken, &s.WebhookToken}} {
			if *f.into, err = need(f.name); err != nil {
				return nil, err
			}
		}
		u, perr := url.Parse(raw)
		if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.User != nil || strings.Trim(u.Path, "/") != "" {
			return nil, fmt.Errorf("xrrx: %s is not the server's address (http or https, a host, no path, query or credentials)", EnvBlueBubblesURL)
		}
		s.Base = strings.TrimRight(raw, "/")
		if !imessageTokenRe.MatchString(s.WebhookToken) {
			return nil, fmt.Errorf("xrrx: %s is not a token of 16 or more letters, digits, '-' or '_'", EnvBlueBubblesWebhookToken)
		}
	default:
		return nil, fmt.Errorf("xrrx: %s=%q is not a backend (want %s or %s)", EnvIMessageBackend, s.Backend, IMessageBackendBlooio, IMessageBackendBlueBubbles)
	}
	if strings.EqualFold(s.Sender, s.To) {
		return nil, fmt.Errorf("xrrx: %s names the sending line itself", EnvIMessageTo)
	}
	if s.Listen == "" {
		return nil, fmt.Errorf("xrrx: iMessage recording needs %s, the address the backend's webhook reaches", EnvIMessageListen)
	}
	if v := getenv(EnvIMessageMaxSends); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("xrrx: %s is not a number of sends", EnvIMessageMaxSends)
		}
		s.Max = n
	}
	if s.Ledger == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("xrrx: no ledger directory: %w (set %s)", err, EnvIMessageLedger)
		}
		s.Ledger = filepath.Join(dir, "heo", "imessage-record-sends")
	}
	return s, nil
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the recipient (and its digits), the line, the credentials, the
// token and the server's address.
func (s *IMessageSandbox) Deny() []string {
	out := []string{s.To, strings.TrimPrefix(s.To, "+"), s.Sender, strings.TrimPrefix(s.Sender, "+"),
		s.APIKey, s.Password, s.WebhookSecret, s.WebhookToken}
	if u, err := url.Parse(s.Base); err == nil && s.Backend == IMessageBackendBlueBubbles {
		out = append(out, u.Host)
	}
	kept := out[:0]
	for _, v := range out {
		if v != "" {
			kept = append(kept, v)
		}
	}
	return kept
}

// Sent is how many sends the ledger holds.
func (s *IMessageSandbox) Sent() int {
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
func (s *IMessageSandbox) takeSlot() error {
	if err := os.MkdirAll(s.Ledger, 0o700); err != nil { //nolint:gosec // G703: the ledger directory the recording was given
		return fmt.Errorf("xrrx: iMessage sandbox: no ledger, send refused: %w", err)
	}
	for n := 1; n <= s.Max; n++ {
		f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
		if err == nil {
			_, _ = fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
			return f.Close()
		}
		if !os.IsExist(err) {
			return fmt.Errorf("xrrx: iMessage sandbox: ledger: %w", err)
		}
	}
	return fmt.Errorf("xrrx: iMessage sandbox: the cap of %d sends is spent, send refused", s.Max)
}

// Transport returns the RoundTripper that sends the gateway's requests to
// the backend through base (http.DefaultTransport when nil). The only
// request it allows is the backend's send call, to the backend's own
// address, naming exactly the recipient and the line, carrying a short plain
// text starting "heo adapter test"; it takes one of the ledger's slots
// first. Everything else (a read, another path, another host, another
// recipient, another body field, a link) fails before it is sent.
func (s *IMessageSandbox) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := s.check(req); err != nil {
			return nil, err
		}
		if s.Rehearse {
			return s.canned(), nil
		}
		if err := s.takeSlot(); err != nil {
			return nil, err
		}
		return base.RoundTrip(req)
	})
}

// check refuses what is not the one allowed send, and leaves the body in
// place for the send.
func (s *IMessageSandbox) check(req *http.Request) error {
	refuse := func(why string) error {
		return fmt.Errorf("xrrx: iMessage sandbox: %s %s refused: %s", req.Method, req.URL.Redacted(), why)
	}
	base, err := url.Parse(s.Base)
	if err != nil {
		return refuse("the sandbox has no backend address")
	}
	want := base.Path + "/messages"
	if s.Backend == IMessageBackendBlueBubbles {
		want = bluebubblesSendPath
	}
	switch {
	case req.Method != http.MethodPost:
		return refuse("only the backend's send is allowed")
	case !strings.EqualFold(req.URL.Host, base.Host) || req.URL.Scheme != base.Scheme:
		return refuse("not the backend's address")
	case req.URL.Path != want:
		return refuse("only the backend's send is allowed")
	}
	if req.Body == nil {
		return refuse("no body")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 8<<10))
	_ = req.Body.Close()
	if err != nil {
		return refuse("unreadable body")
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&body); err != nil {
		return refuse("the body is not a JSON object")
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	var text string
	switch s.Backend {
	case IMessageBackendBlooio:
		if len(body) != 3 || str("from") != s.Sender || !strings.EqualFold(str("to"), s.To) {
			return refuse("a send names the line and the recipient, and nothing else")
		}
		text = str("text")
	default:
		if len(body) != 4 || !strings.EqualFold(str("chatGuid"), bluebubblesChat+s.To) || str("method") != "apple-script" ||
			str("tempGuid") == "" || len(str("tempGuid")) > 255 {
			return refuse("a send is an AppleScript send to the recipient's one-to-one chat, and nothing else")
		}
		text = str("message")
	}
	switch {
	case !strings.HasPrefix(text, IMessageTextPrefix):
		return refuse("the text must start " + strconv.Quote(IMessageTextPrefix))
	case len(text) > imessageMaxText || strings.ContainsAny(text, "\r\n") || strings.Contains(strings.ToLower(text), "http"):
		return refuse("the text must be one short line with no link")
	}
	return nil
}

// canned is the answer a rehearsal gives instead of the backend's: the
// documented shape of a send's answer. It is a stand-in for debugging a
// recording's path before it sends anything; nothing it says is a
// backend's.
func (s *IMessageSandbox) canned() *http.Response {
	body := `{"id":"msg_rehearsal"}`
	if s.Backend == IMessageBackendBlueBubbles {
		body = `{"status":200,"message":"Message sent!","data":{"guid":"rehearsal-guid"}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)),
	}
}
