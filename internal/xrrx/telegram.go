package xrrx

import (
	"bytes"
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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Environment of a live Telegram recording. Every value is the recording
// process's alone: nothing here is a file or a flag.
const (
	// EnvTelegramToken is the bot's token ("<bot ID>:<secret>", from
	// BotFather). The recorded process is told a stand-in instead; the
	// transport puts this one in the request path and nowhere else, so it
	// is in no file of heo's and no cassette.
	EnvTelegramToken = "HEO_RECORD_TELEGRAM_BOT_TOKEN" // #nosec G101 -- the name of a variable, not a credential
	// EnvTelegramBotUsername is the bot's username without "@";
	// EnvTelegramChatID is the one chat the recording writes to, the
	// maintainers' own: a throwaway group the bot is in, or the private chat
	// with the maintainer, by its number.
	EnvTelegramBotUsername = "HEO_RECORD_TELEGRAM_BOT_USERNAME"
	EnvTelegramChatID      = "HEO_RECORD_TELEGRAM_CHAT_ID"
	// EnvTelegramMaxSends caps the sendMessage requests ever made.
	EnvTelegramMaxSends = "HEO_RECORD_TELEGRAM_MAX_SENDS"
	// EnvTelegramLedger names the directory that remembers the sends
	// already made, across runs; a user cache directory when unset.
	EnvTelegramLedger = "HEO_RECORD_TELEGRAM_LEDGER"
	// EnvTelegramRehearse, "1", reaches nothing: the transport answers from
	// a small in-memory Bot API, so a recording's whole path can be
	// rehearsed before it sends anything. Its cassettes are worthless and
	// are cleared by the next recording.
	EnvTelegramRehearse = "HEO_RECORD_TELEGRAM_REHEARSE"

	telegramHost = "api.telegram.org"
	// DefaultTelegramMaxSends is the cap when EnvTelegramMaxSends is unset:
	// the recording sends two messages, and one more is spare for a run
	// that failed midway.
	DefaultTelegramMaxSends = 3
)

// What a Telegram recording leaves in place of the bot and the chat, and
// what the recorded process is told in their stead.
const (
	TelegramBotID       int64 = 7000000001
	TelegramChatID      int64 = -1000000000001
	TelegramBotUsername       = "heo_adapter_test_bot"
	// TelegramPerson is the user fixtures write in the chat as.
	TelegramPerson int64 = 5000000002
	// telegramOtherBase is where the placeholders of any other ID start.
	telegramOtherBase int64 = 5000000010
	// TelegramStandInToken is the bot token heo's credential store holds in
	// every recording and replay; the real one is the transport's.
	TelegramStandInToken = "7000000001:AAHheo_adapter_test_stand_in_token" // #nosec G101 -- a stand-in, not a credential
	// TelegramStandInSecret is the webhook secret token of the fixtures the
	// test posts.
	TelegramStandInSecret = "heo-adapter-test-webhook-secret" // #nosec G101 -- a stand-in, not a credential
	// TelegramSecretHeader is the header Telegram carries the secret in.
	TelegramSecretHeader = "X-Telegram-Bot-Api-Secret-Token" // #nosec G101 -- a header name, not a credential
	// TelegramTextPrefix starts every text a recording sends.
	TelegramTextPrefix = "heo adapter test"
	// telegramMaxText bounds the text of a send.
	telegramMaxText = 64
)

var (
	telegramTokenRe = regexp.MustCompile(`^([0-9]{5,16}):[A-Za-z0-9_-]{30,64}$`)
	// telegramTokenAnyRe is a bot token anywhere in text, such as in a
	// request path.
	telegramTokenAnyRe = regexp.MustCompile(`\b[0-9]{5,16}:[A-Za-z0-9_-]{30,64}\b`)
	telegramUserRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,28}[Bb][Oo][Tt]$`)
	telegramMessageRe  = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
)

// CheckLiveTelegram says whether this environment may send recorded
// traffic to Telegram: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI,
// redaction on, and the bot and one chat named. Replay and passthrough
// never may.
func CheckLiveTelegram(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "Telegram"); err != nil {
		return err
	}
	_, err := telegramFromEnv(getenv)
	return err
}

// TelegramSandbox is the one bot and chat a live recording reaches, and
// what the recording must never keep of them. The recorded process is told
// the placeholders (TelegramBotID, TelegramChatID, TelegramBotUsername)
// and a stand-in token; Transport translates between those and the real
// ones, counts every send against the cap, and refuses everything else.
type TelegramSandbox struct {
	// Token is the real bot token; BotID is the number before its colon.
	Token string
	BotID int64
	// BotUsername is the real username, without "@".
	BotUsername string
	// ChatID is the real chat.
	ChatID int64
	// Max caps the sends the ledger will ever allow.
	Max int
	// Ledger is the directory that remembers the sends made.
	Ledger string
	// Rehearse answers from an in-memory Bot API and reaches nothing.
	Rehearse bool

	mu       sync.Mutex
	others   map[int64]int64 // real ID of anyone else -> placeholder
	known    map[int64]bool  // message_id of the messages this process sent
	rehearse telegramRehearsal
}

// TelegramSandboxFromEnv is the sandbox of a process that drives or is
// recorded: the values come from the environment and are checked.
func TelegramSandboxFromEnv(getenv func(string) string) (*TelegramSandbox, error) {
	return telegramFromEnv(getenv)
}

func telegramFromEnv(getenv func(string) string) (*TelegramSandbox, error) {
	s := &TelegramSandbox{
		Token: getenv(EnvTelegramToken), BotUsername: getenv(EnvTelegramBotUsername), Max: DefaultTelegramMaxSends,
		Ledger: getenv(EnvTelegramLedger), Rehearse: getenv(EnvTelegramRehearse) == "1",
	}
	m := telegramTokenRe.FindStringSubmatch(s.Token)
	if m == nil {
		return nil, fmt.Errorf("xrrx: %s is not a bot token (<bot ID>:<secret>)", EnvTelegramToken)
	}
	s.BotID, _ = strconv.ParseInt(m[1], 10, 64)
	if s.BotID <= 0 || s.Token == TelegramStandInToken {
		return nil, fmt.Errorf("xrrx: %s is not a real bot token", EnvTelegramToken)
	}
	if !telegramUserRe.MatchString(s.BotUsername) {
		return nil, fmt.Errorf("xrrx: %s is not a bot username (without @, ending in bot)", EnvTelegramBotUsername)
	}
	chat, err := strconv.ParseInt(getenv(EnvTelegramChatID), 10, 64)
	if err != nil || chat == 0 || strconv.FormatInt(chat, 10) != getenv(EnvTelegramChatID) {
		return nil, fmt.Errorf("xrrx: %s is not a chat ID (a number: a channel's @username is never a route)", EnvTelegramChatID)
	}
	if chat == s.BotID {
		return nil, fmt.Errorf("xrrx: %s names the bot itself", EnvTelegramChatID)
	}
	s.ChatID = chat
	if v := getenv(EnvTelegramMaxSends); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("xrrx: %s is not a number of sends", EnvTelegramMaxSends)
		}
		s.Max = n
	}
	if s.Ledger == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("xrrx: no ledger directory: %w (set %s)", err, EnvTelegramLedger)
		}
		s.Ledger = filepath.Join(dir, "heo", "telegram-record-sends")
	}
	return s, nil
}

// ChildEnv is what the recorded process is told of the sandbox.
func (s *TelegramSandbox) ChildEnv() []string {
	env := []string{
		EnvLive + "=1", EnvTelegramToken + "=" + s.Token, EnvTelegramBotUsername + "=" + s.BotUsername,
		EnvTelegramChatID + "=" + strconv.FormatInt(s.ChatID, 10),
		EnvTelegramMaxSends + "=" + strconv.Itoa(s.Max), EnvTelegramLedger + "=" + s.Ledger,
	}
	if s.Rehearse {
		env = append(env, EnvTelegramRehearse+"=1")
	}
	return env
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the token and its secret half, the bot, its username, the
// chat and every other ID the sandbox has replaced.
func (s *TelegramSandbox) Deny() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, secret, _ := strings.Cut(s.Token, ":")
	out := []string{s.Token, secret, strconv.FormatInt(s.BotID, 10), s.BotUsername, strconv.FormatInt(s.ChatID, 10)}
	for real := range s.others {
		out = append(out, strconv.FormatInt(real, 10))
	}
	return out
}

// Sent is how many sends the ledger holds.
func (s *TelegramSandbox) Sent() int {
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
func (s *TelegramSandbox) takeSlot() (int, error) {
	if err := os.MkdirAll(s.Ledger, 0o700); err != nil { //nolint:gosec // G703: the ledger directory the recording was given
		return 0, fmt.Errorf("xrrx: Telegram sandbox: no ledger, send refused: %w", err)
	}
	for n := 1; n <= s.Max; n++ {
		f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
		if err == nil {
			_, _ = fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
			return n, f.Close()
		}
		if !os.IsExist(err) {
			return 0, fmt.Errorf("xrrx: Telegram sandbox: ledger: %w", err)
		}
	}
	return 0, fmt.Errorf("xrrx: Telegram sandbox: the cap of %d sends is spent, send refused", s.Max)
}

// noteMessage writes the message_id Telegram gave a send into its ledger
// slot, and remembers it for this process's own edits, deletes and
// reactions: the chat holds messages of people, and only the recording's
// own are touched.
func (s *TelegramSandbox) noteMessage(slot int, id int64) {
	s.mu.Lock()
	if s.known == nil {
		s.known = map[int64]bool{}
	}
	s.known[id] = true
	s.mu.Unlock()
	if slot == 0 {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(slot)), os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
	if err == nil {
		_, _ = fmt.Fprintln(f, id)
		_ = f.Close()
	}
}

// knownMessage reports whether id names a message this recording sent, in
// this process or, through the ledger, in an earlier one.
func (s *TelegramSandbox) knownMessage(id int64) bool {
	s.mu.Lock()
	ok := s.known[id]
	s.mu.Unlock()
	if ok {
		return true
	}
	want := strconv.FormatInt(id, 10)
	for n := 1; n <= s.Max; n++ {
		b, err := os.ReadFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n))) //nolint:gosec // G304: the ledger directory the recording was given
		if err != nil {
			continue
		}
		// The first field is the time of the claim.
		if fields := strings.Fields(string(b)); len(fields) > 1 && slices.Contains(fields[1:], want) {
			return true
		}
	}
	return false
}

// telegramKind classifies an allowed request.
type telegramKind int

const (
	telegramSend telegramKind = iota
	telegramWrite
)

// telegramMethods are the only Bot API methods a recording may call, with
// the fields each may carry and what kind of request it is. Everything
// else is refused before it is sent: no read (the Bot API has none of past
// messages, and getUpdates would take the bot's queue), no getMe, no
// webhook method, no file or chat administration, and no answer to a
// callback query, which only a person pressing a button can make.
var telegramMethods = map[string]struct {
	kind   telegramKind
	fields []string
}{
	"sendMessage":        {telegramSend, []string{"chat_id", "text", "reply_parameters"}},
	"editMessageText":    {telegramWrite, []string{"chat_id", "message_id", "text"}},
	"deleteMessage":      {telegramWrite, []string{"chat_id", "message_id"}},
	"setMessageReaction": {telegramWrite, []string{"chat_id", "message_id", "reaction"}},
}

// Transport returns the RoundTripper that sends requests to Telegram
// through base (http.DefaultTransport when nil). A request must be to
// https://api.telegram.org/bot<stand-in token>/<method> for one of
// telegramMethods, name the placeholder chat and, where it names a
// message, one this recording sent; a send must also be a short text
// starting "heo adapter test" with no link and no formatting, and takes
// one of the ledger's slots first. Everything else fails before it is
// sent. The answer comes back with the bot, the chat and every other ID
// replaced, the names dropped and the churning headers removed; an answer
// that still names what it must not is not handed on.
func (s *TelegramSandbox) Transport(base http.RoundTripper) http.RoundTripper {
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

func (s *TelegramSandbox) send(base http.RoundTripper, out *http.Request, method string, kind telegramKind) (*http.Response, error) {
	slot := 0
	if kind == telegramSend {
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
	if method == "sendMessage" && resp.StatusCode == http.StatusOK {
		resp = s.noteAnswer(slot, resp)
	}
	return resp, nil
}

// noteAnswer records the message_id of a send's answer and hands the
// answer on.
func (s *TelegramSandbox) noteAnswer(slot int, resp *http.Response) *http.Response {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return resp
	}
	var m struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &m) == nil && m.OK && m.Result.MessageID > 0 {
		s.noteMessage(slot, m.Result.MessageID)
	}
	return resp
}

// scrubbedError is err with the token a transport error spells out in the
// request URL replaced, and the bot and chat with it. The original stays
// reachable through Unwrap.
func (s *TelegramSandbox) scrubbedError(err error) error {
	msg := err.Error()
	_, secret, _ := strings.Cut(s.Token, ":")
	for _, lit := range []string{s.Token, secret} {
		if lit != "" {
			msg = strings.ReplaceAll(msg, lit, "<token>")
		}
	}
	return &scrubbedErr{msg: s.scrubString(msg), err: err}
}

// translate is req aimed at the real bot, with the real token, or an error
// when req may not be sent. It returns the request, the Bot API method it
// calls and its kind.
func (s *TelegramSandbox) translate(req *http.Request) (*http.Request, string, telegramKind, error) {
	u := req.URL
	refuse := func(why string) (*http.Request, string, telegramKind, error) {
		return nil, "", 0, fmt.Errorf("xrrx: Telegram sandbox: %s %s refused: %s", req.Method, ScrubText(u.Redacted()), why)
	}
	if u.Scheme != "https" || u.Host != telegramHost || u.User != nil || u.RawQuery != "" {
		return refuse("only https://" + telegramHost + "/bot<token>/<method>, with no query")
	}
	p := u.EscapedPath()
	if p != path.Clean(p) || strings.Contains(p, "%") || !strings.HasPrefix(p, "/bot") {
		return refuse("path is not a Bot API method")
	}
	token, method, ok := strings.Cut(strings.TrimPrefix(p, "/bot"), "/")
	if !ok || strings.Contains(method, "/") {
		return refuse("path is not a Bot API method")
	}
	if token != TelegramStandInToken {
		return refuse("the caller must hold the stand-in token, not a real one")
	}
	spec, ok := telegramMethods[method]
	if !ok || req.Method != http.MethodPost {
		return refuse("only POST of sendMessage, editMessageText, deleteMessage and setMessageReaction")
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
	args["chat_id"] = json.Number(strconv.FormatInt(s.ChatID, 10))
	real, err := json.Marshal(args)
	if err != nil {
		return refuse("the body is not encodable")
	}
	out := req.Clone(req.Context())
	out.Host = ""
	out.URL.Path = "/bot" + s.Token + "/" + method
	out.URL.RawPath = ""
	out.Body = io.NopCloser(bytes.NewReader(real))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(real)), nil }
	out.ContentLength = int64(len(real))
	out.Header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	return out, method, spec.kind, nil
}

// checkArgs holds one request's fields to what a recording may do.
func (s *TelegramSandbox) checkArgs(method string, allowed []string, args map[string]any) error {
	for k := range args {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return fmt.Errorf("field %q is not allowed on %s", k, method)
		}
	}
	if n, ok := args["chat_id"].(json.Number); !ok || n.String() != strconv.FormatInt(TelegramChatID, 10) {
		return errors.New("only the sandbox's chat")
	}
	messageID := func(v any) error {
		n, ok := v.(json.Number)
		if !ok || !telegramMessageRe.MatchString(n.String()) {
			return errors.New("message_id is not a message ID")
		}
		id, _ := n.Int64()
		if !s.knownMessage(id) {
			return errors.New("message_id must name a message this recording sent")
		}
		return nil
	}
	if v, present := args["message_id"]; present {
		if err := messageID(v); err != nil {
			return err
		}
	}
	if v, present := args["reply_parameters"]; present {
		rp, ok := v.(map[string]any)
		if !ok || len(rp) == 0 {
			return errors.New("reply_parameters is a message of this recording")
		}
		for k, c := range rp {
			switch k {
			case "message_id":
				if err := messageID(c); err != nil {
					return err
				}
			case "allow_sending_without_reply":
				if _, ok := c.(bool); !ok {
					return errors.New("reply_parameters.allow_sending_without_reply is a flag")
				}
			default:
				return fmt.Errorf("reply_parameters.%s is not allowed", k)
			}
		}
		if _, ok := rp["message_id"]; !ok {
			return errors.New("reply_parameters names no message of this recording")
		}
	}
	if method == "sendMessage" || method == "editMessageText" {
		text, ok := args["text"].(string)
		if !ok || !strings.HasPrefix(text, TelegramTextPrefix) || utf8.RuneCountInString(text) > telegramMaxText ||
			strings.Contains(text, "://") || strings.ContainsAny(text, "\n\r@") {
			return fmt.Errorf("the text must start %q, be short and carry no link or mention", TelegramTextPrefix)
		}
	}
	if v, present := args["reaction"]; present {
		list, ok := v.([]any)
		if !ok || len(list) > 1 {
			return errors.New("one reaction at most")
		}
		for _, r := range list {
			e, ok := r.(map[string]any)
			emoji, _ := e["emoji"].(string)
			if !ok || len(e) != 2 || e["type"] != "emoji" || emoji == "" || len(emoji) > 16 {
				return errors.New("a reaction is one emoji")
			}
		}
	}
	return nil
}

// normalizeTelegramBody is a Bot API request's recorded body: keys
// sorted. Anything that is not a JSON object is returned as it is.
func normalizeTelegramBody(body []byte) []byte {
	args, err := decodeObject(body)
	if err != nil {
		return body
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
