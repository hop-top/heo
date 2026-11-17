package xrrx

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Environment of a live Discord recording. Every value is the recording
// process's alone: nothing here is a file or a flag.
const (
	// EnvDiscordToken is the bot's token. The recorded process is told a
	// stand-in instead; the transport sends this one in the Authorization
	// header and nowhere else, so it is in no file of heo's and no cassette.
	EnvDiscordToken = "HEO_RECORD_DISCORD_BOT_TOKEN" // #nosec G101 -- the name of a variable, not a credential
	// EnvDiscordApplicationID and EnvDiscordBotUserID name the application
	// and its bot user; EnvDiscordGuildID the one server, and
	// EnvDiscordChannelID the one channel of it, the maintainers' own, the
	// recording writes to.
	EnvDiscordApplicationID = "HEO_RECORD_DISCORD_APPLICATION_ID"
	EnvDiscordBotUserID     = "HEO_RECORD_DISCORD_BOT_USER_ID"
	EnvDiscordGuildID       = "HEO_RECORD_DISCORD_GUILD_ID"
	EnvDiscordChannelID     = "HEO_RECORD_DISCORD_CHANNEL_ID"
	// EnvDiscordMaxSends caps the Create Message requests ever made.
	EnvDiscordMaxSends = "HEO_RECORD_DISCORD_MAX_SENDS"
	// EnvDiscordLedger names the directory that remembers the sends already
	// made, across runs; a user cache directory when unset.
	EnvDiscordLedger = "HEO_RECORD_DISCORD_LEDGER"
	// EnvDiscordRehearse, "1", reaches nothing: the transport answers from a
	// small in-memory Discord, so a recording's whole path can be rehearsed
	// before it sends anything. Its cassettes are worthless and are cleared
	// by the next recording.
	EnvDiscordRehearse = "HEO_RECORD_DISCORD_REHEARSE"

	discordHost = "discord.com"
	// discordGatewaySuffix is the domain of the gateway: never reached by a
	// recording, which records REST requests only (a WebSocket session is
	// no HTTP exchange).
	discordGatewaySuffix = ".discord.gg"
	discordAPIPrefix     = "/api/v10/"
	// DefaultDiscordMaxSends is the cap when EnvDiscordMaxSends is unset:
	// the recording sends two messages, and one more is spare for a run
	// that failed midway.
	DefaultDiscordMaxSends = 3
)

// What a Discord recording leaves in place of the application, the bot, the
// server and the channel, and what the recorded process is told in their
// stead.
const (
	DiscordApplicationID = "900000000000000101"
	DiscordBotUserID     = "900000000000000102"
	DiscordGuildID       = "900000000000000103"
	DiscordChannelID     = "900000000000000104"
	// DiscordPerson is the user fixtures write as.
	DiscordPerson = "900000000000000105"
	// DiscordBotUsername replaces the bot's username.
	DiscordBotUsername = "heo-adapter-test-bot"
	// discordOtherBase is where the placeholders of any other ID start.
	discordOtherBase = 900000000000000200
	// DiscordStandInToken is the bot token heo's credential store holds in
	// every recording and replay; the real one is the transport's. Its first
	// segment is, as a real token's, the bot user's ID in base64.
	DiscordStandInToken = "OTAwMDAwMDAwMDAwMDAwMTAy.Gheoad.heo_adapter_test_stand_in_token_0" // #nosec G101 -- a stand-in, not a credential
	// DiscordTextPrefix starts every text a recording sends.
	DiscordTextPrefix = "heo adapter test"
	// discordMaxText bounds the text of a send.
	discordMaxText = 64
	// discordNonceMax is Discord's bound on a message's nonce.
	discordNonceMax = 25
)

var (
	// discordTokenRe is a bot token: the bot user ID in base64, a six
	// character timestamp and the HMAC.
	discordTokenRe = regexp.MustCompile(`^([A-Za-z0-9_-]{16,32})\.([A-Za-z0-9_-]{6})\.([A-Za-z0-9_-]{27,64})$`)
	// discordTokenAnyRe is a bot token anywhere in text.
	discordTokenAnyRe  = regexp.MustCompile(`\b[A-Za-z0-9_-]{16,32}\.[A-Za-z0-9_-]{6}\.[A-Za-z0-9_-]{27,64}\b`)
	discordSnowflakeRe = regexp.MustCompile(`^[1-9][0-9]{16,19}$`)
)

// CheckLiveDiscord says whether this environment may send recorded traffic
// to Discord: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI, redaction on, and
// the application, bot, server and one channel named. Replay and
// passthrough never may.
func CheckLiveDiscord(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "Discord"); err != nil {
		return err
	}
	_, err := discordFromEnv(getenv)
	return err
}

// DiscordSandbox is the one server channel a live recording reaches, and
// what the recording must never keep of it. The recorded process is told
// the placeholders (DiscordApplicationID, DiscordBotUserID, DiscordGuildID,
// DiscordChannelID) and a stand-in token; Transport translates between
// those and the real ones, counts every send against the cap, and refuses
// everything else.
type DiscordSandbox struct {
	// Token is the real bot token.
	Token string
	// ApplicationID, BotUserID, GuildID and ChannelID are the real ones.
	ApplicationID, BotUserID, GuildID, ChannelID string
	// Max caps the sends the ledger will ever allow.
	Max int
	// Ledger is the directory that remembers the sends made.
	Ledger string
	// Rehearse answers from an in-memory Discord and reaches nothing.
	Rehearse bool

	mu       sync.Mutex
	others   map[string]string // real ID of anyone else -> placeholder
	known    map[string]bool   // ID of the messages this process sent
	rehearse discordRehearsal
}

// DiscordSandboxFromEnv is the sandbox of a process that drives or is
// recorded: the values come from the environment and are checked.
func DiscordSandboxFromEnv(getenv func(string) string) (*DiscordSandbox, error) {
	return discordFromEnv(getenv)
}

func discordFromEnv(getenv func(string) string) (*DiscordSandbox, error) {
	s := &DiscordSandbox{
		Token: getenv(EnvDiscordToken), Max: DefaultDiscordMaxSends, Ledger: getenv(EnvDiscordLedger),
		Rehearse: getenv(EnvDiscordRehearse) == "1",
	}
	for _, f := range []struct {
		env string
		to  *string
	}{
		{EnvDiscordApplicationID, &s.ApplicationID}, {EnvDiscordBotUserID, &s.BotUserID},
		{EnvDiscordGuildID, &s.GuildID}, {EnvDiscordChannelID, &s.ChannelID},
	} {
		*f.to = getenv(f.env)
		if !discordSnowflakeRe.MatchString(*f.to) {
			return nil, fmt.Errorf("xrrx: %s is not a Discord ID (a snowflake: digits only)", f.env)
		}
	}
	m := discordTokenRe.FindStringSubmatch(s.Token)
	if m == nil || s.Token == DiscordStandInToken {
		return nil, fmt.Errorf("xrrx: %s is not a real bot token", EnvDiscordToken)
	}
	// A bot token begins with the bot user's ID: a token of another bot
	// would write as it.
	if id, err := base64.RawStdEncoding.DecodeString(m[1]); err != nil || string(id) != s.BotUserID {
		return nil, fmt.Errorf("xrrx: %s is not the token of the bot user %s names", EnvDiscordToken, EnvDiscordBotUserID)
	}
	seen := map[string]string{}
	for _, f := range []struct{ env, id string }{
		{EnvDiscordApplicationID, s.ApplicationID}, {EnvDiscordBotUserID, s.BotUserID},
		{EnvDiscordGuildID, s.GuildID}, {EnvDiscordChannelID, s.ChannelID},
	} {
		if prev, dup := seen[f.id]; dup && (prev != EnvDiscordApplicationID || f.env != EnvDiscordBotUserID) {
			return nil, fmt.Errorf("xrrx: %s and %s name one ID", prev, f.env)
		}
		seen[f.id] = f.env
	}
	if v := getenv(EnvDiscordMaxSends); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("xrrx: %s is not a number of sends", EnvDiscordMaxSends)
		}
		s.Max = n
	}
	if s.Ledger == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("xrrx: no ledger directory: %w (set %s)", err, EnvDiscordLedger)
		}
		s.Ledger = filepath.Join(dir, "heo", "discord-record-sends")
	}
	return s, nil
}

// ChildEnv is what the recorded process is told of the sandbox.
func (s *DiscordSandbox) ChildEnv() []string {
	env := []string{
		EnvLive + "=1", EnvDiscordToken + "=" + s.Token, EnvDiscordApplicationID + "=" + s.ApplicationID,
		EnvDiscordBotUserID + "=" + s.BotUserID, EnvDiscordGuildID + "=" + s.GuildID, EnvDiscordChannelID + "=" + s.ChannelID,
		EnvDiscordMaxSends + "=" + strconv.Itoa(s.Max), EnvDiscordLedger + "=" + s.Ledger,
	}
	if s.Rehearse {
		env = append(env, EnvDiscordRehearse+"=1")
	}
	return env
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the token and its parts, the application, the bot user, the
// server, the channel and every other ID the sandbox has replaced.
func (s *DiscordSandbox) Deny() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []string{s.Token, s.ApplicationID, s.BotUserID, s.GuildID, s.ChannelID}
	if m := discordTokenRe.FindStringSubmatch(s.Token); m != nil {
		out = append(out, m[1], m[3])
	}
	for real := range s.others {
		out = append(out, real)
	}
	return out
}

// Sent is how many sends the ledger holds.
func (s *DiscordSandbox) Sent() int {
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
func (s *DiscordSandbox) takeSlot() (int, error) {
	if err := os.MkdirAll(s.Ledger, 0o700); err != nil { //nolint:gosec // G703: the ledger directory the recording was given
		return 0, fmt.Errorf("xrrx: Discord sandbox: no ledger, send refused: %w", err)
	}
	for n := 1; n <= s.Max; n++ {
		f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
		if err == nil {
			_, _ = fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
			return n, f.Close()
		}
		if !os.IsExist(err) {
			return 0, fmt.Errorf("xrrx: Discord sandbox: ledger: %w", err)
		}
	}
	return 0, fmt.Errorf("xrrx: Discord sandbox: the cap of %d sends is spent, send refused", s.Max)
}

// noteMessage writes the ID Discord gave a send into its ledger slot, and
// remembers it for this process's own edits, deletes and reactions: the
// channel holds messages of people, and only the recording's own are
// touched.
func (s *DiscordSandbox) noteMessage(slot int, id string) {
	s.mu.Lock()
	if s.known == nil {
		s.known = map[string]bool{}
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
func (s *DiscordSandbox) knownMessage(id string) bool {
	s.mu.Lock()
	ok := s.known[id]
	s.mu.Unlock()
	if ok {
		return true
	}
	for n := 1; n <= s.Max; n++ {
		b, err := os.ReadFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n))) //nolint:gosec // G304: the ledger directory the recording was given
		if err != nil {
			continue
		}
		// The first field is the time of the claim.
		if fields := strings.Fields(string(b)); len(fields) > 1 && slices.Contains(fields[1:], id) {
			return true
		}
	}
	return false
}

// discordKind classifies an allowed request.
type discordKind int

const (
	discordSend discordKind = iota
	discordWrite
)

// discordRoute is one REST route a recording may call.
type discordRoute struct {
	method string
	kind   discordKind
	// fields are the JSON fields its body may carry.
	fields []string
}

// discordRoutes are the only requests a recording may make, by shape:
// Create Message, Edit Message, Delete Message, Create Reaction and Delete
// Own Reaction, in the sandbox's channel and on messages the recording
// sent. Everything else is refused before it is sent: no read (history,
// reconciliation, Get Channel, Get Gateway Bot), no direct message (Create
// DM), no typing indicator, no thread, no webhook or interaction callback
// (a token only a user's interaction makes), and no administration.
var discordRoutes = map[string]discordRoute{
	"send":    {http.MethodPost, discordSend, []string{"content", "allowed_mentions", "nonce", "enforce_nonce", "message_reference"}},
	"edit":    {http.MethodPatch, discordWrite, []string{"content", "allowed_mentions", "components"}},
	"delete":  {http.MethodDelete, discordWrite, nil},
	"react":   {http.MethodPut, discordWrite, nil},
	"unreact": {http.MethodDelete, discordWrite, nil},
}

// Transport returns the RoundTripper that sends requests to Discord
// through base (http.DefaultTransport when nil). A request must be to
// https://discord.com/api/v10/channels/<placeholder channel>/... for one of
// discordRoutes and, where it names a message, one this recording sent; a
// send or an edit must carry a short text starting "heo adapter test" with
// no link, no mention and no formatting, and a send takes one of the
// ledger's slots first. The caller must hold the stand-in token. Everything
// else fails before it is sent. The answer comes back with the
// application, the bot, the server, the channel and every other ID
// replaced, the names dropped and the churning headers removed; an answer
// that still names what it must not is not handed on.
func (s *DiscordSandbox) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out, route, kind, err := s.translate(req)
		if err != nil {
			return nil, err
		}
		var resp *http.Response
		if s.Rehearse {
			resp, err = s.canned(route, out)
		} else {
			resp, err = s.send(base, out, route, kind)
		}
		if err != nil {
			return nil, err
		}
		return s.scrubResponse(resp)
	})
}

func (s *DiscordSandbox) send(base http.RoundTripper, out *http.Request, route string, kind discordKind) (*http.Response, error) {
	slot := 0
	if kind == discordSend {
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
	if route == "send" && resp.StatusCode == http.StatusOK {
		resp = s.noteAnswer(slot, resp)
	}
	return resp, nil
}

// noteAnswer records the ID of a send's answer and hands the answer on.
func (s *DiscordSandbox) noteAnswer(slot int, resp *http.Response) *http.Response {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return resp
	}
	var m struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &m) == nil && discordSnowflakeRe.MatchString(m.ID) {
		s.noteMessage(slot, m.ID)
	}
	return resp
}

// scrubbedError is err with the token a transport error might spell out,
// and the IDs of the sandbox, replaced. The original stays reachable
// through Unwrap.
func (s *DiscordSandbox) scrubbedError(err error) error {
	return &scrubbedErr{msg: s.scrubString(err.Error()), err: err}
}

// translate is req aimed at the real channel, with the real token, or an
// error when req may not be sent. It returns the request, the name of the
// route it follows and its kind.
func (s *DiscordSandbox) translate(req *http.Request) (*http.Request, string, discordKind, error) {
	u := req.URL
	refuse := func(why string) (*http.Request, string, discordKind, error) {
		return nil, "", 0, fmt.Errorf("xrrx: Discord sandbox: %s %s refused: %s", req.Method, ScrubText(u.Redacted()), why)
	}
	if u.Scheme != "https" || u.Host != discordHost || u.User != nil || u.RawQuery != "" {
		return refuse("only https://" + discordHost + discordAPIPrefix + "channels/<channel>/..., with no query")
	}
	escaped := u.EscapedPath()
	if !strings.HasPrefix(escaped, discordAPIPrefix+"channels/") {
		return refuse("path is not a channel route of API v10")
	}
	segs := strings.Split(strings.TrimPrefix(escaped, discordAPIPrefix+"channels/"), "/")
	if slices.Contains(segs, "") || slices.Contains(segs, ".") || slices.Contains(segs, "..") {
		return refuse("path is not a channel route of API v10")
	}
	// segs: <channel>/messages[/<id>[/reactions/<emoji>/@me]]
	if len(segs) < 2 || segs[0] != DiscordChannelID || segs[1] != "messages" {
		return refuse("only the sandbox's channel and its messages")
	}
	name, msg, emoji := "", "", ""
	switch {
	case len(segs) == 2 && req.Method == http.MethodPost:
		name = "send"
	case len(segs) == 3 && req.Method == http.MethodPatch:
		name, msg = "edit", segs[2]
	case len(segs) == 3 && req.Method == http.MethodDelete:
		name, msg = "delete", segs[2]
	case len(segs) == 6 && segs[3] == "reactions" && segs[5] == "@me" && (req.Method == http.MethodPut || req.Method == http.MethodDelete):
		name, msg, emoji = "react", segs[2], segs[4]
		if req.Method == http.MethodDelete {
			name = "unreact"
		}
	default:
		return refuse("only Create, Edit and Delete Message and Create and Delete Own Reaction")
	}
	spec := discordRoutes[name]
	if msg != "" {
		if !discordSnowflakeRe.MatchString(msg) || !s.knownMessage(msg) {
			return refuse("the message must be one this recording sent")
		}
	}
	if emoji != "" {
		if err := checkDiscordEmoji(emoji); err != nil {
			return refuse(err.Error())
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bot "+DiscordStandInToken {
		return refuse("the caller must hold the stand-in token, not a real one")
	}
	var body []byte
	if spec.fields != nil {
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
		if err := s.checkArgs(name, spec.fields, args); err != nil {
			return refuse(err.Error())
		}
		if ref, ok := args["message_reference"].(map[string]any); ok {
			ref["channel_id"] = s.ChannelID
		}
		if body, err = json.Marshal(args); err != nil {
			return refuse("the body is not encodable")
		}
	} else if req.Body != nil && req.Body != http.NoBody {
		raw, _ := io.ReadAll(io.LimitReader(req.Body, 1))
		_ = req.Body.Close()
		if len(raw) > 0 {
			return refuse("this route takes no body")
		}
	}
	out := req.Clone(req.Context())
	out.Host = ""
	parts := []string{"channels", s.ChannelID, "messages"}
	if msg != "" {
		parts = append(parts, msg)
	}
	if emoji != "" {
		parts = append(parts, "reactions", emoji, "@me")
	}
	out.URL.RawPath = discordAPIPrefix + strings.Join(parts, "/")
	out.URL.Path, _ = url.PathUnescape(out.URL.RawPath)
	out.Header = http.Header{"Authorization": {"Bot " + s.Token}, "Accept": {"application/json"}, "User-Agent": {discordUserAgent(req)}}
	if body != nil {
		out.Header.Set("Content-Type", "application/json")
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		out.ContentLength = int64(len(body))
	} else {
		out.Body, out.GetBody, out.ContentLength = http.NoBody, nil, 0
	}
	return out, name, spec.kind, nil
}

// discordUserAgent keeps the form Discord requires, "DiscordBot (url,
// version)", from the caller; any other is replaced by a neutral one.
func discordUserAgent(req *http.Request) string {
	if ua := req.Header.Get("User-Agent"); strings.HasPrefix(ua, "DiscordBot (") && !strings.ContainsAny(ua, "\r\n") && len(ua) < 128 {
		return ua
	}
	return "DiscordBot (https://github.com/hop-top/heo, 1)"
}

// checkDiscordEmoji holds a reaction's emoji, as the URL carries it, to a
// short Unicode emoji: no custom emoji (name:id), no path or word.
func checkDiscordEmoji(escaped string) error {
	dec, err := url.PathUnescape(escaped)
	if err != nil || dec == "" || len(dec) > 16 || !utf8.ValidString(dec) {
		return errors.New("a reaction is one short Unicode emoji")
	}
	for _, r := range dec {
		if r < 0x80 && r != '‍' {
			return errors.New("a reaction is one short Unicode emoji")
		}
	}
	return nil
}

// checkArgs holds one request's fields to what a recording may do.
func (s *DiscordSandbox) checkArgs(route string, allowed []string, args map[string]any) error {
	for k := range args {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("field %q is not allowed on %s", k, route)
		}
	}
	text, ok := args["content"].(string)
	if !ok || !strings.HasPrefix(text, DiscordTextPrefix) || utf8.RuneCountInString(text) > discordMaxText ||
		strings.Contains(text, "://") || strings.ContainsAny(text, "\n\r@<>") {
		return fmt.Errorf("the text must start %q, be short and carry no link or mention", DiscordTextPrefix)
	}
	if v, present := args["allowed_mentions"]; present {
		am, ok := v.(map[string]any)
		parse, hasParse := am["parse"].([]any)
		if !ok || len(am) != 1 || !hasParse || len(parse) != 0 {
			return errors.New(`allowed_mentions must be {"parse":[]}: nothing is pinged`)
		}
	}
	if v, present := args["components"]; present {
		if list, ok := v.([]any); !ok || len(list) != 0 {
			return errors.New("no components")
		}
	}
	if v, present := args["nonce"]; present {
		if n, ok := v.(string); !ok || n == "" || len(n) > discordNonceMax {
			return errors.New("the nonce is a short string")
		}
	}
	if v, present := args["enforce_nonce"]; present {
		if _, ok := v.(bool); !ok {
			return errors.New("enforce_nonce is a flag")
		}
	}
	if v, present := args["message_reference"]; present {
		ref, ok := v.(map[string]any)
		if !ok || len(ref) == 0 {
			return errors.New("message_reference is a message of this recording")
		}
		for k, c := range ref {
			switch k {
			case "message_id":
				id, _ := c.(string)
				if !discordSnowflakeRe.MatchString(id) || !s.knownMessage(id) {
					return errors.New("message_reference must name a message this recording sent")
				}
			case "channel_id":
				if id, _ := c.(string); id != DiscordChannelID {
					return errors.New("message_reference names only the sandbox's channel")
				}
			case "fail_if_not_exists":
				if _, ok := c.(bool); !ok {
					return errors.New("message_reference.fail_if_not_exists is a flag")
				}
			default:
				return fmt.Errorf("message_reference.%s is not allowed", k)
			}
		}
		if _, ok := ref["message_id"]; !ok {
			return errors.New("message_reference names no message of this recording")
		}
	}
	return nil
}

// discordNoncePlaceholder replaces a message's nonce in a recorded request.
const discordNoncePlaceholder = "<nonce>"

// normalizeDiscordBody is a Discord request's recorded body: keys sorted,
// and the nonce, which uma derives from an operation ID that is new on
// every run, replaced, so the same request fingerprints the same in
// replay. Anything that is not a JSON object is returned as it is.
func normalizeDiscordBody(body []byte) []byte {
	args, err := decodeObject(body)
	if err != nil {
		return body
	}
	if _, ok := args["nonce"]; ok {
		args["nonce"] = discordNoncePlaceholder
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return body
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
