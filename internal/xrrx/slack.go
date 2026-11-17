package xrrx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Environment of a live Slack recording. Every value is the recording
// process's alone: nothing here is a file or a flag.
const (
	// EnvSlackToken is the app's bot token (xoxb-...). The recorded process
	// is told a stand-in instead; the transport puts this one in the
	// Authorization header and nowhere else, so it is in no file of heo's
	// and no cassette.
	EnvSlackToken = "HEO_RECORD_SLACK_BOT_TOKEN" // #nosec G101 -- the name of a variable, not a credential
	// EnvSlackTeamID, EnvSlackBotUserID and EnvSlackChannelID are the
	// workspace (T...), the app's bot user (U...) and the throwaway channel
	// (C... or G...) the recording writes to and reads. EnvSlackBotID (B...)
	// is optional.
	EnvSlackTeamID    = "HEO_RECORD_SLACK_TEAM_ID"
	EnvSlackBotUserID = "HEO_RECORD_SLACK_BOT_USER_ID"
	EnvSlackBotID     = "HEO_RECORD_SLACK_BOT_ID"
	EnvSlackChannelID = "HEO_RECORD_SLACK_CHANNEL_ID"
	// EnvSlackMaxPosts caps the chat.postMessage requests ever made.
	EnvSlackMaxPosts = "HEO_RECORD_SLACK_MAX_POSTS"
	// EnvSlackLedger names the directory that remembers the posts already
	// made, across runs; a user cache directory when unset.
	EnvSlackLedger = "HEO_RECORD_SLACK_LEDGER"
	// EnvSlackRehearse, "1", reaches nothing: the transport answers from a
	// small in-memory Slack, so a recording's whole path can be rehearsed
	// before it posts anything. Its cassettes are worthless and are cleared
	// by the next recording.
	EnvSlackRehearse = "HEO_RECORD_SLACK_REHEARSE"

	slackHost = "slack.com"
	// slackHooksHost serves the response URLs of interactions.
	slackHooksHost = "hooks.slack.com"
	// DefaultSlackMaxPosts is the cap when EnvSlackMaxPosts is unset: the
	// recording posts two messages, and Slack refusing the attempt metadata
	// makes the adapter post the first one again without it.
	DefaultSlackMaxPosts = 3
)

// What a Slack recording leaves in place of the workspace, the bot and the
// channel, and what the recorded process is told in their stead.
const (
	SlackTeamID    = "T0000000001"
	SlackBotUserID = "U0000000001"
	SlackBotID     = "B0000000001"
	SlackChannelID = "C0000000001"
	SlackAppID     = "A0000000001"
	// SlackPerson is the user fixtures write in the channel as.
	SlackPerson = "U0000000002"
	// SlackStandInToken is the bot token heo's credential store holds in
	// every recording and replay; the real one is the transport's.
	SlackStandInToken = "xoxb-heo-adapter-test" // #nosec G101 -- a stand-in, not a credential
	// SlackStandInSigningSecret signs the fixtures the test posts.
	SlackStandInSigningSecret = "heo-adapter-test-signing-secret" // #nosec G101 -- a stand-in, not a credential
	// SlackTextPrefix starts every text a recording posts.
	SlackTextPrefix = "heo adapter test"
	// slackMaxText bounds the text of a post.
	slackMaxText = 64
	// slackAttempt is the metadata event type the adapter marks its sends
	// with.
	slackAttempt = "uma_attempt"
)

var (
	slackTokenRe = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{8,}$`)
	slackIDRe    = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,31}$`)
	// slackIDInTextRe is an ID-shaped token of text: a letter that opens
	// Slack IDs and eight to eleven upper-case letters or digits (longer
	// tokens, such as a Twilio SID, are not Slack IDs).
	slackIDInTextRe = regexp.MustCompile(`\b[TUWCGDBA][A-Z0-9]{8,11}\b`)
	// slackPlaceholderRe is what an ID is replaced with.
	slackPlaceholderRe = regexp.MustCompile(`^[A-Z]0{8,}[0-9]+$`)
	slackTSRe          = regexp.MustCompile(`^[0-9]{10}\.[0-9]{6}$`)
	slackReactionRe    = regexp.MustCompile(`^[a-z0-9_+-]{1,32}$`)
	slackBoundRe       = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,6})?$`)
)

// CheckLiveSlack says whether this environment may send recorded traffic
// to Slack: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI, redaction on, and
// the workspace, bot and one throwaway channel named. Replay and
// passthrough never may.
func CheckLiveSlack(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "Slack"); err != nil {
		return err
	}
	_, err := slackFromEnv(getenv)
	return err
}

// SlackSandbox is the one workspace, bot and channel a live recording
// reaches, and what the recording must never keep of them. The recorded
// process is told the placeholders (SlackTeamID, SlackBotUserID,
// SlackChannelID) and a stand-in token; Transport translates between
// those and the real ones, counts every post against the cap, and refuses
// everything else.
type SlackSandbox struct {
	// Token is the real bot token.
	Token string
	// TeamID, BotUserID, BotID and ChannelID are the real ones; BotID may
	// be empty.
	TeamID, BotUserID, BotID, ChannelID string
	// Max caps the posts the ledger will ever allow.
	Max int
	// Ledger is the directory that remembers the posts made.
	Ledger string
	// Rehearse answers from an in-memory Slack and reaches nothing.
	Rehearse bool

	mu       sync.Mutex
	others   map[string]string // real ID of anyone else -> placeholder
	counters map[string]int
	known    map[string]bool // ts of the messages this process posted
	rehearse rehearsal
}

// SlackSandboxFromEnv is the sandbox of a process that drives or is
// recorded: the values come from the environment and are checked.
func SlackSandboxFromEnv(getenv func(string) string) (*SlackSandbox, error) {
	return slackFromEnv(getenv)
}

func slackFromEnv(getenv func(string) string) (*SlackSandbox, error) {
	s := &SlackSandbox{
		Token: getenv(EnvSlackToken), TeamID: getenv(EnvSlackTeamID), BotUserID: getenv(EnvSlackBotUserID),
		BotID: getenv(EnvSlackBotID), ChannelID: getenv(EnvSlackChannelID), Max: DefaultSlackMaxPosts,
		Ledger: getenv(EnvSlackLedger), Rehearse: getenv(EnvSlackRehearse) == "1",
	}
	if !slackTokenRe.MatchString(s.Token) {
		return nil, fmt.Errorf("xrrx: %s is not a bot token (xoxb-...)", EnvSlackToken)
	}
	for _, c := range []struct{ env, v, prefix string }{
		{EnvSlackTeamID, s.TeamID, "T"}, {EnvSlackBotUserID, s.BotUserID, "U"}, {EnvSlackChannelID, s.ChannelID, "CG"},
	} {
		if !slackIDRe.MatchString(c.v) || !strings.ContainsAny(c.v[:1], c.prefix) {
			return nil, fmt.Errorf("xrrx: %s is not a Slack ID of the right kind (a channel is C... or G..., never a direct message)", c.env)
		}
	}
	if s.BotID != "" && (!slackIDRe.MatchString(s.BotID) || s.BotID[0] != 'B') {
		return nil, fmt.Errorf("xrrx: %s is not a Slack bot ID (B...)", EnvSlackBotID)
	}
	if v := getenv(EnvSlackMaxPosts); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("xrrx: %s is not a number of posts", EnvSlackMaxPosts)
		}
		s.Max = n
	}
	if s.Ledger == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("xrrx: no ledger directory: %w (set %s)", err, EnvSlackLedger)
		}
		s.Ledger = filepath.Join(dir, "heo", "slack-record-posts")
	}
	return s, nil
}

// ChildEnv is what the recorded process is told of the sandbox.
func (s *SlackSandbox) ChildEnv() []string {
	env := []string{
		EnvLive + "=1", EnvSlackToken + "=" + s.Token, EnvSlackTeamID + "=" + s.TeamID, EnvSlackBotUserID + "=" + s.BotUserID,
		EnvSlackBotID + "=" + s.BotID, EnvSlackChannelID + "=" + s.ChannelID,
		EnvSlackMaxPosts + "=" + strconv.Itoa(s.Max), EnvSlackLedger + "=" + s.Ledger,
	}
	if s.Rehearse {
		env = append(env, EnvSlackRehearse+"=1")
	}
	return env
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the token, the workspace, the bot, the channel and every
// other ID the sandbox has replaced.
func (s *SlackSandbox) Deny() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{s.Token, s.TeamID, s.BotUserID, s.BotID, s.ChannelID}
	for real := range s.others {
		out = append(out, real)
	}
	return out
}

// Posted is how many posts the ledger holds.
func (s *SlackSandbox) Posted() int {
	ents, _ := os.ReadDir(s.Ledger)
	n := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "post-") {
			n++
		}
	}
	return n
}

// takeSlot claims one of the posts the cap allows, before the request
// leaves: a post whose answer is lost still counts. The claim is a file
// created exclusively, so concurrent processes cannot both take the last.
func (s *SlackSandbox) takeSlot() (int, error) {
	if err := os.MkdirAll(s.Ledger, 0o700); err != nil { //nolint:gosec // G703: the ledger directory the recording was given
		return 0, fmt.Errorf("xrrx: Slack sandbox: no ledger, post refused: %w", err)
	}
	for n := 1; n <= s.Max; n++ {
		f, err := os.OpenFile(filepath.Join(s.Ledger, "post-"+strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
		if err == nil {
			_, _ = fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
			return n, f.Close()
		}
		if !os.IsExist(err) {
			return 0, fmt.Errorf("xrrx: Slack sandbox: ledger: %w", err)
		}
	}
	return 0, fmt.Errorf("xrrx: Slack sandbox: the cap of %d posts is spent, post refused", s.Max)
}

// noteTS writes the ts Slack gave a post into its ledger slot, so the
// process that drives the recording can read the message back, and
// remembers it for this process's own edits and deletes.
func (s *SlackSandbox) noteTS(slot int, ts string) {
	s.mu.Lock()
	if s.known == nil {
		s.known = map[string]bool{}
	}
	s.known[ts] = true
	s.mu.Unlock()
	if slot == 0 {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.Ledger, "post-"+strconv.Itoa(slot)), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
	if err == nil {
		_, _ = fmt.Fprintln(f, ts)
		_ = f.Close()
	}
}

// knownTS reports whether ts names a message this recording posted, in
// this process or, through the ledger, in an earlier one.
func (s *SlackSandbox) knownTS(ts string) bool {
	if !slackTSRe.MatchString(ts) {
		return false
	}
	s.mu.Lock()
	ok := s.known[ts]
	s.mu.Unlock()
	if ok {
		return true
	}
	for n := 1; n <= s.Max; n++ {
		b, err := os.ReadFile(filepath.Join(s.Ledger, "post-"+strconv.Itoa(n))) //nolint:gosec // G304: the ledger directory the recording was given
		if err != nil {
			continue
		}
		for _, l := range strings.Fields(string(b)) {
			if l == ts {
				return true
			}
		}
	}
	return false
}

// slackKind classifies an allowed request.
type slackKind int

const (
	slackPost slackKind = iota
	slackWrite
	slackRead
)

// slackMethods are the only Web API methods a recording may call, with the
// fields each may carry and what kind of request it is. Everything else,
// including conversations.open, files, users, auth and every admin method,
// is refused before it is sent.
var slackMethods = map[string]struct {
	kind   slackKind
	fields []string
}{
	"chat.postMessage":      {slackPost, []string{"channel", "text", "thread_ts", "mrkdwn", "metadata"}},
	"chat.update":           {slackWrite, []string{"channel", "ts", "text", "metadata"}},
	"chat.delete":           {slackWrite, []string{"channel", "ts"}},
	"reactions.add":         {slackWrite, []string{"channel", "timestamp", "name"}},
	"reactions.remove":      {slackWrite, []string{"channel", "timestamp", "name"}},
	"reactions.get":         {slackRead, []string{"channel", "timestamp", "full"}},
	"conversations.history": {slackRead, []string{"channel", "oldest", "latest", "limit", "inclusive", "include_all_metadata", "cursor"}},
	"conversations.replies": {slackRead, []string{"channel", "ts", "oldest", "latest", "limit", "inclusive", "include_all_metadata", "cursor"}},
}

// Transport returns the RoundTripper that sends requests to the workspace
// through base (http.DefaultTransport when nil). A request must be to
// https://slack.com/api/<method> for one of slackMethods, carry the
// stand-in token, name the placeholder channel and, where it names a
// message, one this recording posted; a post must also be a short text
// starting "heo adapter test" with no link, blocks or attachment, and
// takes one of the ledger's slots first. Everything else fails before it
// is sent. The answer comes back with the workspace, the bot, the channel
// and every other ID replaced, other people's messages dropped and the
// churning headers removed; an answer that still names what it must not is
// not handed on.
func (s *SlackSandbox) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out, method, kind, err := s.translate(req)
		if err != nil {
			return nil, err
		}
		var resp *http.Response
		if s.Rehearse {
			resp, err = s.canned(method, out)
		} else {
			resp, err = s.send(base, out, method, kind)
		}
		if err != nil {
			return nil, err
		}
		return s.scrubResponse(resp)
	})
}

func (s *SlackSandbox) send(base http.RoundTripper, out *http.Request, method string, kind slackKind) (*http.Response, error) {
	slot := 0
	if kind == slackPost {
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
	if method == "chat.postMessage" && resp.StatusCode == http.StatusOK {
		resp = s.noteAnswer(slot, resp)
	}
	return resp, nil
}

// noteAnswer records the ts of a post's answer and hands the answer on.
func (s *SlackSandbox) noteAnswer(slot int, resp *http.Response) *http.Response {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return resp
	}
	var m struct {
		OK bool   `json:"ok"`
		TS string `json:"ts"`
	}
	if json.Unmarshal(raw, &m) == nil && m.OK && slackTSRe.MatchString(m.TS) {
		s.noteTS(slot, m.TS)
	}
	return resp
}

// scrubbedError is err with the token, the workspace and the channel a
// transport error spells out replaced. The original stays reachable
// through Unwrap.
func (s *SlackSandbox) scrubbedError(err error) error {
	msg := s.scrubString(err.Error())
	if s.Token != "" {
		msg = strings.ReplaceAll(msg, s.Token, "<token>")
	}
	return &scrubbedErr{msg: msg, err: err}
}

// translate is req aimed at the real workspace, with the real token, or an
// error when req may not be sent. It returns the request, the Web API
// method it calls and its kind.
func (s *SlackSandbox) translate(req *http.Request) (*http.Request, string, slackKind, error) {
	u := req.URL
	refuse := func(why string) (*http.Request, string, slackKind, error) {
		return nil, "", 0, fmt.Errorf("xrrx: Slack sandbox: %s %s refused: %s", req.Method, u.Redacted(), why)
	}
	if u.Scheme != "https" || u.Host != slackHost || u.User != nil || u.RawQuery != "" {
		return refuse("only https://" + slackHost + "/api/, with no query")
	}
	p := u.EscapedPath()
	if p != path.Clean(p) || strings.Contains(p, "%") || !strings.HasPrefix(p, "/api/") {
		return refuse("path is not a Web API method")
	}
	method := strings.TrimPrefix(p, "/api/")
	spec, ok := slackMethods[method]
	if !ok || req.Method != http.MethodPost {
		return refuse("only POST of chat.postMessage, chat.update, chat.delete, reactions.add, reactions.remove, reactions.get, conversations.history and conversations.replies")
	}
	if req.Header.Get("Authorization") != "Bearer "+SlackStandInToken {
		return refuse("the caller must hold the stand-in token, not a real one")
	}
	if mt := req.Header.Get("Content-Type"); !strings.HasPrefix(mt, "application/json") {
		return refuse("the body must be JSON")
	}
	if req.Body == nil {
		return refuse("no body")
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<16))
	_ = req.Body.Close()
	if err != nil {
		return refuse("unreadable body")
	}
	args, err := decodeObject(raw)
	if err != nil {
		return refuse("the body is not one JSON object")
	}
	if err := s.checkArgs(method, spec.fields, args); err != nil {
		return refuse(err.Error())
	}
	real, err := json.Marshal(s.toReal(args))
	if err != nil {
		return refuse("the body is not encodable")
	}
	out := req.Clone(req.Context())
	out.Host = ""
	out.Body = io.NopCloser(bytes.NewReader(real))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(real)), nil }
	out.ContentLength = int64(len(real))
	out.Header = http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Accept": {"application/json"}}
	out.Header.Set("Authorization", "Bearer "+s.Token)
	return out, method, spec.kind, nil
}

func decodeObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil || v == nil || dec.More() {
		return nil, errors.New("not one object")
	}
	return v, nil
}

// checkArgs holds one request's fields to what a recording may do.
func (s *SlackSandbox) checkArgs(method string, allowed []string, args map[string]any) error {
	for k := range args {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return fmt.Errorf("field %q is not allowed on %s", k, method)
		}
	}
	str := func(k string) (string, bool) { v, ok := args[k].(string); return v, ok }
	if ch, ok := str("channel"); !ok || ch != SlackChannelID {
		return errors.New("only the sandbox's channel")
	}
	for _, k := range []string{"ts", "timestamp", "thread_ts"} {
		if v, present := args[k]; present {
			ts, ok := v.(string)
			if !ok || !s.knownTS(ts) {
				return fmt.Errorf("%s must name a message this recording posted", k)
			}
		}
	}
	if method == "chat.postMessage" || method == "chat.update" {
		text, ok := str("text")
		if !ok || !strings.HasPrefix(text, SlackTextPrefix) || utf8.RuneCountInString(text) > slackMaxText ||
			strings.Contains(text, "://") || strings.ContainsAny(text, "\n\r") {
			return fmt.Errorf("the text must start %q, be short and carry no link", SlackTextPrefix)
		}
	}
	if name, present := args["name"]; present {
		if n, ok := name.(string); !ok || !slackReactionRe.MatchString(n) {
			return errors.New("not a reaction name")
		}
	}
	if m, present := args["metadata"]; present {
		if err := checkSlackMetadata(m); err != nil {
			return err
		}
	}
	if l, present := args["limit"]; present {
		n, ok := l.(json.Number)
		if v, err := n.Int64(); !ok || err != nil || v < 1 || v > 20 {
			return errors.New("limit is 1 to 20")
		}
	}
	for _, k := range []string{"oldest", "latest"} {
		if v, present := args[k]; present {
			if t, ok := v.(string); !ok || !slackBoundRe.MatchString(t) {
				return fmt.Errorf("%s is not a timestamp", k)
			}
		}
	}
	return nil
}

// checkSlackMetadata allows only the attempt marker the adapter sends.
func checkSlackMetadata(v any) error {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 || m["event_type"] != slackAttempt {
		return errors.New("metadata is the attempt marker only")
	}
	payload, ok := m["event_payload"].(map[string]any)
	if !ok || len(payload) > 2 {
		return errors.New("metadata is the attempt marker only")
	}
	for k, v := range payload {
		if _, ok := v.(string); !ok || (k != "operation" && k != "attempt") {
			return errors.New("metadata is the attempt marker only")
		}
	}
	return nil
}

// toReal replaces the placeholders in args' string values with the real
// workspace, bot and channel.
func (s *SlackSandbox) toReal(v any) any {
	switch t := v.(type) {
	case string:
		for p, real := range map[string]string{SlackChannelID: s.ChannelID, SlackTeamID: s.TeamID, SlackBotUserID: s.BotUserID} {
			t = strings.ReplaceAll(t, p, real)
		}
		return t
	case []any:
		for i := range t {
			t[i] = s.toReal(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = s.toReal(t[k])
		}
	}
	return v
}

// Slack's attempt metadata carries the operation and attempt heo tagged
// the send with: new on every run, so the recorded request names
// placeholders, as for the echo marker.
const (
	// SlackOperation and SlackAttempt replace them in a recorded request.
	SlackOperation = "operation-id"
	SlackAttempt   = "attempt-id"
)

// normalizeSlackBody is a Slack request's recorded body: the attempt
// metadata without the run's operation and attempt, keys sorted. Anything
// that is not a JSON object is returned as it is.
func normalizeSlackBody(body []byte) []byte {
	args, err := decodeObject(body)
	if err != nil {
		return body
	}
	if m, ok := args["metadata"].(map[string]any); ok {
		if p, ok := m["event_payload"].(map[string]any); ok {
			for k, v := range map[string]string{"operation": SlackOperation, "attempt": SlackAttempt} {
				if _, has := p[k]; has {
					p[k] = v
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// SlackClient is a Slack sandbox client for the process that drives a
// recording: it reads back what the recorded process posted. It goes
// through Transport, so it can reach nothing else.
type SlackClient struct{ hc *http.Client }

// Client returns a client over base (the default transport when nil).
func (s *SlackSandbox) Client(base http.RoundTripper) *SlackClient {
	return &SlackClient{hc: &http.Client{Transport: s.Transport(base)}}
}

// Call posts args to method, which names the placeholder channel, and
// returns the scrubbed answer.
func (c *SlackClient) Call(ctx context.Context, method string, args map[string]any) (json.RawMessage, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+slackHost+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+SlackStandInToken)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return b, fmt.Errorf("xrrx: Slack sandbox: %s: %s", method, resp.Status)
	}
	return b, nil
}

// ReadBack returns, oldest first, the messages of the sandbox channel, and of
// the threads in it, that are the recording's own, scrubbed as the recorded process saw them.
func (s *SlackSandbox) ReadBack(ctx context.Context, base http.RoundTripper) ([]map[string]any, error) {
	raw, err := s.Client(base).Call(ctx, "conversations.history", map[string]any{"channel": SlackChannelID, "limit": 20, "include_all_metadata": true})
	if err != nil {
		return nil, err
	}
	var v struct {
		OK       bool             `json:"ok"`
		Error    string           `json:"error"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if !v.OK {
		return nil, fmt.Errorf("xrrx: Slack sandbox: conversations.history: %s", v.Error)
	}
	// A thread's replies are not in the channel's history: read each thread
	// the history shows.
	seen := map[string]bool{}
	for _, m := range v.Messages {
		ts, _ := m["ts"].(string)
		seen[ts] = true
	}
	for _, m := range slices.Clone(v.Messages) {
		if n, _ := m["reply_count"].(float64); n == 0 {
			continue
		}
		ts, _ := m["ts"].(string)
		replies, err := s.thread(ctx, base, ts)
		if err != nil {
			return nil, err
		}
		for _, r := range replies {
			if rts, _ := r["ts"].(string); !seen[rts] {
				seen[rts] = true
				v.Messages = append(v.Messages, r)
			}
		}
	}
	// Oldest first.
	sort.SliceStable(v.Messages, func(i, j int) bool {
		a, _ := v.Messages[i]["ts"].(string)
		b, _ := v.Messages[j]["ts"].(string)
		return a < b
	})
	return v.Messages, nil
}

// thread is the recording's own messages of the thread rooted at ts.
func (s *SlackSandbox) thread(ctx context.Context, base http.RoundTripper, ts string) ([]map[string]any, error) {
	raw, err := s.Client(base).Call(ctx, "conversations.replies", map[string]any{"channel": SlackChannelID, "ts": ts, "limit": 20, "include_all_metadata": true})
	if err != nil {
		return nil, err
	}
	var v struct {
		OK       bool             `json:"ok"`
		Error    string           `json:"error"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if !v.OK {
		return nil, fmt.Errorf("xrrx: Slack sandbox: conversations.replies: %s", v.Error)
	}
	return v.Messages, nil
}
