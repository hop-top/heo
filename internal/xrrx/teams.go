package xrrx

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
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
	"unicode/utf8"
)

// Environment of a live Microsoft Teams recording. Every value is the
// recording process's alone: nothing here is a file or a flag.
const (
	// EnvTeamsConnectorToken is the Bot Connector access token, a JWT the
	// recording's operator minted from the bot's app registration (it lasts
	// an hour). The recorded daemon mints from a stand-in client secret and
	// is answered a stand-in token (TokenTransport); the transport puts
	// this one in the Authorization header and nowhere else, so it is in no
	// file of heo's and no cassette.
	EnvTeamsConnectorToken = "HEO_RECORD_TEAMS_CONNECTOR_TOKEN" // #nosec G101 -- the name of a variable, not a credential
	// EnvTeamsAppID and EnvTeamsTenantID name the bot registration and its
	// Microsoft Entra tenant, GUIDs.
	EnvTeamsAppID    = "HEO_RECORD_TEAMS_APP_ID"
	EnvTeamsTenantID = "HEO_RECORD_TEAMS_TENANT_ID"
	// EnvTeamsServiceURL is the Bot Connector service the bot's tenant is
	// on, such as https://smba.trafficmanager.net/teams/ (the public cloud's).
	EnvTeamsServiceURL = "HEO_RECORD_TEAMS_SERVICE_URL"
	// EnvTeamsUserID is the one user the recording writes to, the
	// maintainers' own: the Teams account ID ("29:...") the app is installed
	// for; EnvTeamsConversationID is the bot's personal chat with that
	// user ("a:..."), which the Bot Connector created.
	EnvTeamsUserID         = "HEO_RECORD_TEAMS_USER_ID"
	EnvTeamsConversationID = "HEO_RECORD_TEAMS_CONVERSATION_ID"
	// EnvTeamsMaxSends caps the message activities ever sent.
	EnvTeamsMaxSends = "HEO_RECORD_TEAMS_MAX_SENDS"
	// EnvTeamsLedger names the directory that remembers the sends already
	// made, across runs; a user cache directory when unset.
	EnvTeamsLedger = "HEO_RECORD_TEAMS_LEDGER"
	// EnvTeamsCredential, "certificate", has the recording's test prove the
	// placeholder bot with a stand-in certificate instead of the stand-in
	// secret (the default): a live recording or its rehearsal only, as the
	// cassettes replayed hold the secret's token request. Read by the test,
	// not by the sandbox.
	EnvTeamsCredential = "HEO_RECORD_TEAMS_CREDENTIAL"
	// EnvTeamsStandInCertificate is the stand-in certificate of the placeholder
	// bot (DER, base64): the public part the sandbox checks a certificate
	// grant's assertion against. The key is the test's, held in heo's
	// credential store, never in an environment.
	EnvTeamsStandInCertificate = "HEO_RECORD_TEAMS_STAND_IN_CERTIFICATE"
	// EnvTeamsRehearse, "1", reaches nothing: the transport answers from a
	// small in-memory Bot Connector, so a recording's whole path can be
	// rehearsed before it sends anything. Its cassettes are worthless and
	// are cleared by the next recording.
	EnvTeamsRehearse = "HEO_RECORD_TEAMS_REHEARSE"

	// DefaultTeamsMaxSends is the cap when EnvTeamsMaxSends is unset: the
	// recording sends two messages, and one more is spare for a run that
	// failed midway.
	DefaultTeamsMaxSends = 3
)

// What a Teams recording leaves in place of the bot, the tenant, the
// service and the user, and what the recorded process is told in their
// stead.
const (
	TeamsAppID    = "00000000-0000-4000-8000-0000000000a1"
	TeamsTenantID = "00000000-0000-4000-8000-0000000000a2"
	// TeamsServiceHost is the Bot Connector service the recorded process is
	// told; it resolves nowhere, and the transport sends what it is asked
	// to the real service. TeamsServiceURL is the channel's service_url.
	TeamsServiceHost = "smba.heo-adapter-test.invalid"
	TeamsServiceURL  = "https://" + TeamsServiceHost + "/teams/"
	// TeamsUserID and TeamsConversationID are the one user and the
	// personal chat with them; TeamsOtherUserBase starts the placeholders
	// of any other account.
	TeamsUserID         = "29:heo-adapter-test-user"
	TeamsConversationID = "a:heo-adapter-test-chat"
	teamsOtherUserBase  = "heo-adapter-test-other-"
	// TeamsStandInToken is the Bot Connector token the daemon is minted in
	// every recording and replay (from TeamsStandInSecret); the real one is
	// the transport's.
	TeamsStandInToken = "heo-adapter-test-stand-in-connector-token" // #nosec G101 -- a stand-in, not a credential
	// teamsRedactedToken is the stand-in token as xrr hands it to the
	// daemon: the token endpoint's answer is recorded through xrr, which
	// redacts an access_token field, and the daemon is given the redacted
	// copy it recorded.
	teamsRedactedToken = "<redacted:ACCESS_TOKEN>" // #nosec G101 -- a placeholder, not a credential
	// TeamsTextPrefix starts every text a recording sends.
	TeamsTextPrefix = "heo adapter test"
	// teamsMaxText bounds the text of a send.
	teamsMaxText = 64

	teamsServicePath = "/teams"
)

var (
	teamsGUIDRe     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	teamsUserRe     = regexp.MustCompile(`^29:[A-Za-z0-9_-]{8,}$`)
	teamsChatRe     = regexp.MustCompile(`^a:[A-Za-z0-9_-]{8,}$`)
	teamsActivityRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	// teamsJWTRe is a bearer token as the Bot Framework mints it: three
	// base64url segments. A client secret is none, which is the point of
	// asking for one: the app's secret is never the recording's to hold.
	teamsJWTRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}$`)
	// teamsJWTAnyRe is a token anywhere in text.
	teamsJWTAnyRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	// teamsServiceHostRe is where a Bot Connector service lives: Microsoft's
	// own hosts of the public, GCC, GCC High and DoD clouds. The token is
	// never sent anywhere else.
	teamsServiceHostRe = regexp.MustCompile(`(?i)^[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:trafficmanager\.net|teams\.microsoft\.com|teams\.microsoft\.us)$`)
)

// teamsReactions are the reaction types Teams names.
var teamsReactions = map[string]bool{"like": true, "heart": true, "laugh": true, "surprised": true, "sad": true, "angry": true}

// CheckLiveTeams says whether this environment may send recorded traffic
// to Microsoft Teams: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI, redaction
// on, and the bot, the tenant, the service, the user and the personal chat
// named. Replay and passthrough never may.
func CheckLiveTeams(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "Microsoft Teams"); err != nil {
		return err
	}
	_, err := teamsFromEnv(getenv)
	return err
}

// TeamsSandbox is the one bot, tenant and personal chat a live recording
// reaches, and what the recording must never keep of them. The recorded
// process is told the placeholders (TeamsAppID, TeamsTenantID,
// TeamsServiceURL, TeamsUserID, TeamsConversationID) and a stand-in token;
// Transport translates between those and the real ones, counts every send
// against the cap, and refuses everything else.
type TeamsSandbox struct {
	// Token is the real Bot Connector access token.
	Token string
	// AppID and TenantID are the real bot registration and tenant.
	AppID, TenantID string
	// ServiceURL is the real Bot Connector service, with a trailing "/".
	ServiceURL string
	// UserID and ConversationID are the real user and the personal chat.
	UserID, ConversationID string
	// Max caps the sends the ledger will ever allow.
	Max int
	// Ledger is the directory that remembers the sends made.
	Ledger string
	// Rehearse answers from an in-memory Bot Connector and reaches nothing.
	Rehearse bool

	// standIn is the stand-in certificate of the placeholder bot, when the
	// recorded daemon proves the app with a certificate instead of a secret.
	standIn *x509.Certificate

	mu       sync.Mutex
	others   map[string]string // real account or tenant ID of anyone else -> placeholder
	known    map[string]bool   // activity IDs of the messages this process sent
	rehearse teamsRehearsal
}

// TeamsSandboxFromEnv is the sandbox of a process that drives or is
// recorded: the values come from the environment and are checked.
func TeamsSandboxFromEnv(getenv func(string) string) (*TeamsSandbox, error) {
	return teamsFromEnv(getenv)
}

func teamsFromEnv(getenv func(string) string) (*TeamsSandbox, error) {
	s := &TeamsSandbox{
		Token: getenv(EnvTeamsConnectorToken), AppID: getenv(EnvTeamsAppID), TenantID: getenv(EnvTeamsTenantID),
		UserID: getenv(EnvTeamsUserID), ConversationID: getenv(EnvTeamsConversationID), Max: DefaultTeamsMaxSends,
		Ledger: getenv(EnvTeamsLedger), Rehearse: getenv(EnvTeamsRehearse) == "1",
	}
	if !teamsJWTRe.MatchString(s.Token) || s.Token == TeamsStandInToken {
		return nil, fmt.Errorf("xrrx: %s is not a Bot Connector access token (a JWT: mint one from the app's client credentials, never put the secret here)", EnvTeamsConnectorToken)
	}
	for env, v := range map[string]string{EnvTeamsAppID: s.AppID, EnvTeamsTenantID: s.TenantID} {
		if !teamsGUIDRe.MatchString(v) || strings.EqualFold(v, TeamsAppID) || strings.EqualFold(v, TeamsTenantID) {
			return nil, fmt.Errorf("xrrx: %s is not a real GUID", env)
		}
	}
	if strings.EqualFold(s.AppID, s.TenantID) {
		return nil, fmt.Errorf("xrrx: %s and %s name one GUID: the bot's app and its tenant are two", EnvTeamsAppID, EnvTeamsTenantID)
	}
	u, err := url.Parse(getenv(EnvTeamsServiceURL))
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery ||
		!teamsServiceHostRe.MatchString(u.Hostname()) || u.Port() != "" || strings.EqualFold(u.Hostname(), TeamsServiceHost) {
		return nil, fmt.Errorf("xrrx: %s is not a Bot Connector service URL (https, a Microsoft host such as smba.trafficmanager.net, no port, user info or query)", EnvTeamsServiceURL)
	}
	if p := strings.Trim(u.Path, "/"); p == "" || strings.Contains(p, "/") || strings.ContainsAny(p, "%.") {
		return nil, fmt.Errorf("xrrx: %s names no region: want https://<host>/<region>/, such as /teams/", EnvTeamsServiceURL)
	}
	if v := getenv(EnvTeamsStandInCertificate); v != "" {
		der, err := base64.StdEncoding.DecodeString(v)
		if err == nil {
			err = s.useStandIn(der)
		}
		if err != nil {
			return nil, fmt.Errorf("xrrx: %s is not the stand-in certificate (DER, base64): %w", EnvTeamsStandInCertificate, err)
		}
	}
	s.ServiceURL = "https://" + strings.ToLower(u.Host) + "/" + strings.Trim(u.Path, "/") + "/"
	if !teamsUserRe.MatchString(s.UserID) || s.UserID == TeamsUserID {
		return nil, fmt.Errorf("xrrx: %s is not a Teams user ID (29:..., the one the app is installed for)", EnvTeamsUserID)
	}
	if !teamsChatRe.MatchString(s.ConversationID) || s.ConversationID == TeamsConversationID {
		return nil, fmt.Errorf("xrrx: %s is not a personal chat's ID (a:..., the bot's chat with that user)", EnvTeamsConversationID)
	}
	if v := getenv(EnvTeamsMaxSends); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("xrrx: %s is not a number of sends", EnvTeamsMaxSends)
		}
		s.Max = n
	}
	if s.Ledger == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("xrrx: no ledger directory: %w (set %s)", err, EnvTeamsLedger)
		}
		s.Ledger = filepath.Join(dir, "heo", "teams-record-sends")
	}
	return s, nil
}

// ChildEnv is what the recorded process is told of the sandbox.
func (s *TeamsSandbox) ChildEnv() []string {
	env := []string{
		EnvLive + "=1", EnvTeamsConnectorToken + "=" + s.Token, EnvTeamsAppID + "=" + s.AppID, EnvTeamsTenantID + "=" + s.TenantID,
		EnvTeamsServiceURL + "=" + s.ServiceURL, EnvTeamsUserID + "=" + s.UserID, EnvTeamsConversationID + "=" + s.ConversationID,
		EnvTeamsMaxSends + "=" + strconv.Itoa(s.Max), EnvTeamsLedger + "=" + s.Ledger,
	}
	if s.Rehearse {
		env = append(env, EnvTeamsRehearse+"=1")
	}
	if s.standIn != nil {
		env = append(env, EnvTeamsStandInCertificate+"="+base64.StdEncoding.EncodeToString(s.standIn.Raw))
	}
	return env
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the token and its segments, the bot, the tenant, the service
// host, the user, the chat and every other ID the sandbox has replaced.
func (s *TeamsSandbox) Deny() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, _ := url.Parse(s.ServiceURL)
	out := []string{s.Token, s.AppID, s.TenantID, u.Hostname(), s.UserID, s.ConversationID}
	for real := range s.others {
		out = append(out, real)
	}
	return out
}

// Sent is how many sends the ledger holds.
func (s *TeamsSandbox) Sent() int {
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
func (s *TeamsSandbox) takeSlot() (int, error) {
	if err := os.MkdirAll(s.Ledger, 0o700); err != nil { //nolint:gosec // G703: the ledger directory the recording was given
		return 0, fmt.Errorf("xrrx: Teams sandbox: no ledger, send refused: %w", err)
	}
	for n := 1; n <= s.Max; n++ {
		f, err := os.OpenFile(filepath.Join(s.Ledger, "send-"+strconv.Itoa(n)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: the ledger directory the recording was given
		if err == nil {
			_, _ = fmt.Fprintln(f, time.Now().UTC().Format(time.RFC3339))
			return n, f.Close()
		}
		if !os.IsExist(err) {
			return 0, fmt.Errorf("xrrx: Teams sandbox: ledger: %w", err)
		}
	}
	return 0, fmt.Errorf("xrrx: Teams sandbox: the cap of %d sends is spent, send refused", s.Max)
}

// noteActivity writes the activity ID the Bot Connector gave a send into
// its ledger slot, and remembers it for this process's own edits, deletes
// and reactions: the chat holds messages of people, and only the
// recording's own are touched.
func (s *TeamsSandbox) noteActivity(slot int, id string) {
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

// knownActivity reports whether id names a message this recording sent, in
// this process or, through the ledger, in an earlier one.
func (s *TeamsSandbox) knownActivity(id string) bool {
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
		if fields := strings.Fields(string(b)); len(fields) > 1 {
			for _, f := range fields[1:] {
				if f == id {
					return true
				}
			}
		}
	}
	return false
}

// teamsOp classifies an allowed request.
type teamsOp string

const (
	teamsCreate  teamsOp = "create"
	teamsSend    teamsOp = "send"
	teamsUpdate  teamsOp = "update"
	teamsDelete  teamsOp = "delete"
	teamsReact   teamsOp = "react"
	teamsUnreact teamsOp = "unreact"
)

// teamsFields are the fields each request may carry. Everything else is
// refused before it is sent: no attachment or card, no typing indicator,
// no mention entity, no read (the Bot Connector has none of past
// messages), no roster, no other conversation, and no key or token
// endpoint of the Bot Framework or of Microsoft Entra.
var teamsFields = map[teamsOp][]string{
	teamsCreate: {"bot", "members", "isGroup", "tenantId", "channelData"},
	teamsSend:   {"type", "text", "textFormat", "replyToId"},
	teamsUpdate: {"type", "id", "text", "textFormat"},
}

// Transport returns the RoundTripper that sends requests to the Bot
// Connector through base (http.DefaultTransport when nil). A request must
// be to the placeholder service (TeamsServiceURL, which the transport
// rewrites to the real one), carry the stand-in token and be
// one of the calls of teamsFields on the placeholder personal chat, and,
// where it names a message, one this recording sent; a send must also be a
// short plain text starting "heo adapter test" with no link and no
// mention, and takes one of the ledger's slots first. Everything else
// fails before it is sent. The answer comes back with the bot, the tenant,
// the service, the user, the chat and every other ID replaced, the names
// dropped and the churning headers removed; an answer that still names
// what it must not is not handed on.
func (s *TeamsSandbox) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out, op, err := s.translate(req)
		if err != nil {
			return nil, err
		}
		var resp *http.Response
		if s.Rehearse {
			resp, err = s.canned(op, out)
		} else {
			resp, err = s.send(base, out, op)
		}
		if err != nil {
			return nil, err
		}
		return s.scrubResponse(resp)
	})
}

func (s *TeamsSandbox) send(base http.RoundTripper, out *http.Request, op teamsOp) (*http.Response, error) {
	slot := 0
	if op == teamsSend {
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
	if op == teamsSend && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp = s.noteAnswer(slot, resp)
	}
	return resp, nil
}

// noteAnswer records the activity ID of a send's answer and hands the
// answer on.
func (s *TeamsSandbox) noteAnswer(slot int, resp *http.Response) *http.Response {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return resp
	}
	var m struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &m) == nil && teamsActivityRe.MatchString(m.ID) {
		s.noteActivity(slot, m.ID)
	}
	return resp
}

// scrubbedError is err with what a transport error spells out of the
// request replaced: the real service host and chat.
func (s *TeamsSandbox) scrubbedError(err error) error {
	msg := err.Error()
	if s.Token != "" {
		msg = strings.ReplaceAll(msg, s.Token, "<token>")
	}
	return &scrubbedErr{msg: s.scrubString(msg), err: err}
}

// translate is req aimed at the real service, with the real token, or an
// error when req may not be sent. It returns the request and what kind of
// call it is.
func (s *TeamsSandbox) translate(req *http.Request) (*http.Request, teamsOp, error) {
	u := req.URL
	refuse := func(why string) (*http.Request, teamsOp, error) {
		return nil, "", fmt.Errorf("xrrx: Teams sandbox: %s %s refused: %s", req.Method, ScrubText(u.Redacted()), why)
	}
	if u.Scheme != "https" || u.Host != TeamsServiceHost || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return refuse("only the sandbox's Bot Connector service (" + TeamsServiceURL + "), with no query")
	}
	p := u.Path
	if p != path.Clean(p) || strings.Contains(u.EscapedPath(), "%2F") || !strings.HasPrefix(p, teamsServicePath+"/v3/conversations") {
		return refuse("path is not a Bot Connector conversation call")
	}
	if a := req.Header.Get("Authorization"); a != "Bearer "+TeamsStandInToken && a != "Bearer "+teamsRedactedToken {
		return refuse("the caller must hold the stand-in token, not a real one")
	}
	segs := strings.Split(strings.TrimPrefix(p, teamsServicePath+"/v3/conversations"), "/")
	// "" | "", <conversation>, "activities"[, <activity>[, "reactions", <type>]]
	op, activity, reaction, err := teamsClassify(req.Method, segs)
	if err != nil {
		return refuse(err.Error())
	}
	if op != teamsCreate && segs[1] != TeamsConversationID {
		return refuse("only the sandbox's personal chat")
	}
	if activity != "" && !teamsActivityRe.MatchString(activity) {
		return refuse("not an activity ID")
	}
	if (op == teamsUpdate || op == teamsDelete || op == teamsReact || op == teamsUnreact) && !s.knownActivity(activity) {
		return refuse("the activity must be a message this recording sent")
	}
	if reaction != "" && !teamsReactions[reaction] {
		return refuse("not one of Teams' six reactions")
	}
	var real []byte
	if op == teamsCreate || op == teamsSend || op == teamsUpdate {
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
		if err := s.checkArgs(op, activity, args); err != nil {
			return refuse(err.Error())
		}
		if real, err = s.realBody(op, args); err != nil {
			return refuse(err.Error())
		}
	} else if req.Body != nil {
		if b, _ := io.ReadAll(io.LimitReader(req.Body, 2)); len(b) > 0 {
			return refuse("this call takes no body")
		}
		_ = req.Body.Close()
	}
	out := req.Clone(req.Context())
	svc, _ := url.Parse(s.ServiceURL)
	out.URL = &url.URL{Scheme: "https", Host: svc.Host, Path: svc.Path + "v3/conversations"}
	if op != teamsCreate {
		out.URL.Path += "/" + s.ConversationID + "/activities"
		if activity != "" {
			out.URL.Path += "/" + activity
		}
		if reaction != "" {
			out.URL.Path += "/reactions/" + reaction
		}
	}
	out.Host = ""
	out.Header = http.Header{"Authorization": {"Bearer " + s.Token}, "Accept": {"application/json"}}
	out.Body, out.GetBody, out.ContentLength = http.NoBody, nil, 0
	if real != nil {
		out.Header.Set("Content-Type", "application/json; charset=utf-8")
		out.Body = io.NopCloser(bytes.NewReader(real))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(real)), nil }
		out.ContentLength = int64(len(real))
	}
	return out, op, nil
}

// teamsClassify says which call a method and path segments are.
func teamsClassify(method string, segs []string) (op teamsOp, activity, reaction string, err error) {
	bad := errors.New("only creating the personal chat, and sending, updating and deleting a message and its reactions")
	switch {
	case len(segs) == 1 && segs[0] == "" && method == http.MethodPost:
		return teamsCreate, "", "", nil
	case len(segs) >= 3 && segs[0] == "" && segs[1] != "" && segs[2] == "activities":
		switch {
		case len(segs) == 3 && method == http.MethodPost:
			return teamsSend, "", "", nil
		case len(segs) == 4 && method == http.MethodPut:
			return teamsUpdate, segs[3], "", nil
		case len(segs) == 4 && method == http.MethodDelete:
			return teamsDelete, segs[3], "", nil
		case len(segs) == 6 && segs[4] == "reactions" && method == http.MethodPut:
			return teamsReact, segs[3], segs[5], nil
		case len(segs) == 6 && segs[4] == "reactions" && method == http.MethodDelete:
			return teamsUnreact, segs[3], segs[5], nil
		}
	}
	return "", "", "", bad
}

// checkArgs holds one request's fields to what a recording may do.
func (s *TeamsSandbox) checkArgs(op teamsOp, activity string, args map[string]any) error {
	for k := range args {
		found := false
		for _, a := range teamsFields[op] {
			found = found || a == k
		}
		if !found {
			return fmt.Errorf("field %q is not allowed on %s", k, op)
		}
	}
	str := func(k string) (string, bool) { v, ok := args[k].(string); return v, ok }
	switch op {
	case teamsCreate:
		bot, _ := args["bot"].(map[string]any)
		members, _ := args["members"].([]any)
		if len(bot) != 1 || bot["id"] != "28:"+TeamsAppID {
			return errors.New("the bot must be the sandbox's")
		}
		if len(members) != 1 {
			return errors.New("one member: the sandbox's user")
		}
		if m, _ := members[0].(map[string]any); len(m) != 1 || m["id"] != TeamsUserID {
			return errors.New("only the sandbox's user")
		}
		if g, ok := args["isGroup"].(bool); !ok || g {
			return errors.New("a personal chat, not a group")
		}
		if t, _ := str("tenantId"); t != TeamsTenantID {
			return errors.New("only the sandbox's tenant")
		}
		if cd, _ := args["channelData"].(map[string]any); len(cd) != 1 {
			return errors.New("channelData names the tenant and nothing else")
		} else if tn, _ := cd["tenant"].(map[string]any); len(tn) != 1 || tn["id"] != TeamsTenantID {
			return errors.New("only the sandbox's tenant")
		}
		return nil
	case teamsSend, teamsUpdate:
		if t, _ := str("type"); t != "message" {
			return errors.New("a message activity only: no typing indicator, card or event")
		}
		if f, present := str("textFormat"); present && f != "plain" {
			return errors.New("textFormat is plain")
		}
		text, ok := str("text")
		if !ok || !strings.HasPrefix(text, TeamsTextPrefix) || utf8.RuneCountInString(text) > teamsMaxText ||
			strings.Contains(text, "://") || strings.ContainsAny(text, "\n\r@<>&") {
			return fmt.Errorf("the text must start %q, be short and carry no link, mention or markup", TeamsTextPrefix)
		}
		if op == teamsUpdate {
			if id, _ := str("id"); id != activity {
				return errors.New("the update names the activity of its path")
			}
		}
		if r, present := args["replyToId"]; present {
			id, ok := r.(string)
			if !ok || !teamsActivityRe.MatchString(id) || !s.knownActivity(id) {
				return errors.New("replyToId must name a message this recording sent")
			}
		}
	}
	return nil
}

// realBody is a request's body aimed at the real bot, tenant and user.
func (s *TeamsSandbox) realBody(op teamsOp, args map[string]any) ([]byte, error) {
	if op == teamsCreate {
		args["bot"] = map[string]any{"id": "28:" + s.AppID}
		args["members"] = []any{map[string]any{"id": s.UserID}}
		args["tenantId"] = s.TenantID
		args["channelData"] = map[string]any{"tenant": map[string]any{"id": s.TenantID}}
	}
	return json.Marshal(args)
}

// normalizeTeamsBody is a Connector request's recorded body: keys sorted.
// Anything that is not a JSON object is returned as it is.
func normalizeTeamsBody(body []byte) []byte {
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
