package daemon_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// teamsBot is the Bot Framework services the microsoft.teams adapter
// talks to, in process: an http.RoundTripper under heo's HTTP client that
// answers the documented shapes of the OpenID metadata and key set the
// adapter verifies inbound tokens with, and of the Bot Connector REST API
// it answers through. It also signs the activities the tests post to the
// webhook, as the Bot Connector would. It is a stand-in of our own, not a
// recording; nothing reaches the network.
type teamsBot struct {
	t *testing.T

	key *rsa.PrivateKey
	kid string

	mu sync.Mutex
	// token is the Connector access token the service accepts, and the
	// one the token endpoint issues; secret the client secret it accepts.
	token, secret string
	// mints are the requests the token endpoint saw; entraScript answers
	// the next ones before anything else.
	mints       []teamsMint
	entraScript []teamsFailure
	// certs are the certificates registered for the app: a client assertion
	// signed for one of them is accepted in place of the secret.
	certs []*x509.Certificate
	// endorsements are the channels the published key is endorsed for.
	endorsements []string
	// keysDown answers the metadata and key set 500.
	keysDown    bool
	metaFetches int
	keyFetches  int
	calls       []teamsCall
	seq         int
	msgs        map[string]*teamsPosted
	// installed are the users the app is installed for: a personal chat
	// can be created with them only.
	installed map[string]bool
	// fail, when it names an operation, answers it with the status and code.
	fail map[string]teamsFailure
}

// teamsCall is one Connector request the adapter made.
type teamsCall struct {
	Host string
	// Op is create (a personal chat), send, typing, update, delete, react
	// or unreact.
	Op           string
	Conversation string
	Activity     string
	Reaction     string
	Auth         string
	Body         map[string]any
}

// teamsPosted is a message the bot or a person posted: "<conversation>|<id>".
type teamsPosted struct {
	From      string
	Text      string
	Card      bool
	ReplyTo   string
	Reactions []string
	Deleted   bool
}

type teamsFailure struct {
	Status int
	Code   string
}

// teamsMint is one request the token endpoint saw.
type teamsMint struct {
	Path, Method, Grant, Scope, App, Secret, Authorization string
	// AssertionType and Assertion are the client assertion of a certificate
	// grant.
	AssertionType, Assertion string
}

const (
	teamsLoginHost     = "login.botframework.com"
	teamsEntraHost     = "login.microsoftonline.com"
	teamsConnectorHost = "smba.trafficmanager.net"
	teamsServiceURL    = "https://" + teamsConnectorHost + "/teams/"
	teamsKid           = "kid-heo-1"
)

func newTeamsBot(t *testing.T, token string) *teamsBot {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &teamsBot{
		t: t, key: key, kid: teamsKid, token: token, secret: teamsSecret, endorsements: []string{"msteams"},
		msgs: map[string]*teamsPosted{}, installed: map[string]bool{}, fail: map[string]teamsFailure{},
	}
}

// Do is ports.HTTPClient.
func (b *teamsBot) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host == teamsEntraHost {
		return b.entra(req)
	}
	var body map[string]any
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				b.t.Errorf("teams request %s is not JSON: %v", req.URL.Path, err)
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch req.URL.Host {
	case teamsLoginHost:
		return b.login(req)
	case teamsConnectorHost:
		return b.connector(req, body)
	}
	return nil, fmt.Errorf("teams fake: unexpected host %s", req.URL.Host)
}

// entra answers Microsoft Entra's token endpoint as documented for the
// client credentials grant: the Connector token for the right tenant, app,
// secret and scope, else invalid_client as Entra words it.
func (b *teamsBot) entra(req *http.Request) (*http.Response, error) {
	if err := req.ParseForm(); err != nil {
		return nil, err
	}
	m := teamsMint{
		Path: req.URL.Path, Method: req.Method, Grant: req.PostForm.Get("grant_type"), Scope: req.PostForm.Get("scope"),
		App: req.PostForm.Get("client_id"), Secret: req.PostForm.Get("client_secret"), Authorization: req.Header.Get("Authorization"),
		AssertionType: req.PostForm.Get("client_assertion_type"), Assertion: req.PostForm.Get("client_assertion"),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.mints = append(b.mints, m)
	if len(b.entraScript) > 0 {
		f := b.entraScript[0]
		b.entraScript = b.entraScript[1:]
		return teamsReply(f.Status, nil), nil
	}
	reply := func(status int, v any) (*http.Response, error) {
		raw, _ := json.Marshal(v)
		return teamsReply(status, raw), nil
	}
	if m.Path != "/"+teamsTenantID+"/oauth2/v2.0/token" || m.Method != http.MethodPost || m.Grant != "client_credentials" ||
		m.Scope != "https://api.botframework.com/.default" || m.App != teamsAppID {
		return reply(http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": "AADSTS900144: the request body is not what Entra documents"})
	}
	if m.AssertionType != "" {
		if m.Secret != "" || m.AssertionType != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" || !b.assertionHolds(m) {
			return reply(http.StatusUnauthorized, map[string]any{
				"error": "invalid_client", "error_codes": []int{700027},
				"error_description": "AADSTS700027: Client assertion contains an invalid signature. [Reason - The key was not found.]",
			})
		}
	} else if m.Secret != b.secret {
		return reply(http.StatusUnauthorized, map[string]any{
			"error": "invalid_client", "error_codes": []int{7000215},
			"error_description": "AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value, not the client secret ID.",
		})
	}
	return reply(http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599, "access_token": b.token})
}

// registerCerts replaces the certificates registered for the app.
func (b *teamsBot) registerCerts(certs ...*x509.Certificate) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.certs = certs
}

// assertionHolds says whether the mint's client assertion is a JWT signed
// RS256 for a registered certificate (found by its x5t#S256 header), for
// this tenant's endpoint, by and about the app. The window and the ID are
// the minter's own tests'. The caller holds b.mu.
func (b *teamsBot) assertionHolds(m teamsMint) bool {
	parts := strings.Split(m.Assertion, ".")
	if len(parts) != 3 {
		return false
	}
	var seg [3][]byte
	for i, p := range parts {
		raw, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			return false
		}
		seg[i] = raw
	}
	var h, c map[string]any
	if json.Unmarshal(seg[0], &h) != nil || json.Unmarshal(seg[1], &c) != nil {
		return false
	}
	if h["alg"] != "RS256" || c["iss"] != teamsAppID || c["sub"] != teamsAppID || c["jti"] == "" ||
		c["aud"] != "https://"+teamsEntraHost+"/"+teamsTenantID+"/oauth2/v2.0/token" {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, cert := range b.certs {
		thumb := sha256.Sum256(cert.Raw)
		if h["x5t#S256"] == base64.RawURLEncoding.EncodeToString(thumb[:]) {
			pub, ok := cert.PublicKey.(*rsa.PublicKey)
			return ok && rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], seg[2]) == nil
		}
	}
	return false
}

// login answers the OpenID metadata and the key set it names.
func (b *teamsBot) login(req *http.Request) (*http.Response, error) {
	if b.keysDown {
		return teamsReply(http.StatusInternalServerError, nil), nil
	}
	switch req.URL.Path {
	case "/v1/.well-known/openidconfiguration":
		b.metaFetches++
		return teamsJSON(map[string]any{
			"issuer": "https://api.botframework.com", "jwks_uri": "https://" + teamsLoginHost + "/v1/.well-known/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}), nil
	case "/v1/.well-known/keys":
		b.keyFetches++
		return teamsJSON(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": b.kid,
			"n": b64(b.key.N.Bytes()), "e": b64(big.NewInt(int64(b.key.E)).Bytes()), "endorsements": b.endorsements,
		}}}), nil
	}
	return teamsReply(http.StatusNotFound, nil), nil
}

// connector answers the Bot Connector REST API under the service URL.
func (b *teamsBot) connector(req *http.Request, body map[string]any) (*http.Response, error) {
	path, ok := strings.CutPrefix(req.URL.Path, "/teams")
	if !ok {
		return teamsReply(http.StatusNotFound, nil), nil
	}
	call := teamsCall{Host: req.URL.Host, Auth: req.Header.Get("Authorization"), Body: body}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// v3/conversations[/<id>/activities[/<activity>[/reactions/<type>]]]
	switch {
	case len(segs) == 2 && req.Method == http.MethodPost:
		call.Op = "create"
	case len(segs) >= 4 && segs[2] != "" && segs[3] == "activities":
		call.Conversation = segs[2]
		if len(segs) > 4 {
			call.Activity = segs[4]
		}
		switch {
		case len(segs) == 4 && req.Method == http.MethodPost && body["type"] == "typing":
			call.Op = "typing"
		case len(segs) == 4 && req.Method == http.MethodPost:
			call.Op = "send"
		case len(segs) == 5 && req.Method == http.MethodPut:
			call.Op = "update"
		case len(segs) == 5 && req.Method == http.MethodDelete:
			call.Op = "delete"
		case len(segs) == 7 && segs[5] == "reactions" && req.Method == http.MethodPut:
			call.Op, call.Reaction = "react", segs[6]
		case len(segs) == 7 && segs[5] == "reactions" && req.Method == http.MethodDelete:
			call.Op, call.Reaction = "unreact", segs[6]
		}
	}
	b.calls = append(b.calls, call)
	if call.Op == "" {
		return teamsReply(http.StatusNotFound, nil), nil
	}
	if call.Auth != "Bearer "+b.token {
		return teamsError(http.StatusUnauthorized, "BadArgument"), nil
	}
	if f, ok := b.fail[call.Op]; ok && f.Status != 0 {
		return teamsError(f.Status, f.Code), nil
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	switch call.Op {
	case "create":
		members, _ := body["members"].([]any)
		var user string
		if len(members) == 1 {
			m, _ := members[0].(map[string]any)
			user, _ = m["id"].(string)
		}
		if !b.installed[user] {
			return teamsError(http.StatusForbidden, "BotNotInConversationRoster"), nil
		}
		return teamsJSON(map[string]any{"id": "a:personal-" + user}), nil
	case "send":
		b.seq++
		id := fmt.Sprintf("%d", 1_760_000_000_000+b.seq)
		_, card := body["attachments"]
		b.msgs[call.Conversation+"|"+id] = &teamsPosted{From: "bot", Text: str("text"), Card: card, ReplyTo: str("replyToId")}
		return teamsJSON(map[string]any{"id": id}), nil
	case "typing":
		return teamsJSON(map[string]any{"id": "typing"}), nil
	}
	m := b.msgs[call.Conversation+"|"+call.Activity]
	switch {
	case m == nil || m.Deleted:
		return teamsError(http.StatusNotFound, "ActivityNotFoundInConversation"), nil
	case m.From != "bot" && (call.Op == "update" || call.Op == "delete"):
		return teamsError(http.StatusForbidden, "ForbiddenOperationException"), nil
	}
	switch call.Op {
	case "update":
		m.Text = str("text")
	case "delete":
		m.Deleted = true
	case "react":
		m.Reactions = append(m.Reactions, call.Reaction)
	case "unreact":
		kept := m.Reactions[:0]
		for _, r := range m.Reactions {
			if r != call.Reaction {
				kept = append(kept, r)
			}
		}
		m.Reactions = kept
	}
	return teamsJSON(map[string]any{"id": call.Activity}), nil
}

// person records a message a person wrote, so the bot may be asked to
// change it (and Teams refuses).
func (b *teamsBot) person(conversation, id, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs[conversation+"|"+id] = &teamsPosted{From: "person", Text: text}
}

// install marks the app as installed for user.
func (b *teamsBot) install(user string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.installed[user] = true
}

func (b *teamsBot) failWith(op string, status int, code string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if status == 0 {
		delete(b.fail, op)
		return
	}
	b.fail[op] = teamsFailure{Status: status, Code: code}
}

func (b *teamsBot) setKeysDown(down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keysDown = down
}

func (b *teamsBot) setEndorsements(channels ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.endorsements = channels
}

// called returns the Connector requests of operation op, in order.
func (b *teamsBot) called(op string) []teamsCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []teamsCall
	for _, c := range b.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// total is how many Connector requests were made.
func (b *teamsBot) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// keyFetchCount is how many times the metadata and the key set were read.
func (b *teamsBot) keyFetchCount() (meta, keys int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.metaFetches, b.keyFetches
}

// posted is the message "<conversation>|<id>", a copy; nil when none.
func (b *teamsBot) posted(id string) *teamsPosted {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := b.msgs[id]
	if m == nil {
		return nil
	}
	c := *m
	c.Reactions = append([]string(nil), m.Reactions...)
	return &c
}

// Tokens the Bot Connector sends the bot with each activity.

// teamsToken is the Bot Connector token of an activity for app, signed
// RS256 by the published key, with the documented claims; mod may alter
// the claims or the header before signing.
func (b *teamsBot) teamsToken(app string, mod ...func(header, claims map[string]any)) string {
	return teamsSign(b.key, app, b.kid, mod...)
}

func teamsSign(key *rsa.PrivateKey, app, kid string, mod ...func(header, claims map[string]any)) string {
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	claims := map[string]any{
		"iss": "https://api.botframework.com", "aud": app, "serviceurl": teamsServiceURL,
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for _, m := range mod {
		m(header, claims)
	}
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return b64(raw)
	}
	signing := enc(header) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + b64(sig)
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func teamsJSON(v any) *http.Response {
	raw, _ := json.Marshal(v)
	return teamsReply(http.StatusOK, raw)
}

func teamsError(status int, code string) *http.Response {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": code}})
	return teamsReply(status, raw)
}

func teamsReply(status int, body []byte) *http.Response {
	h := http.Header{}
	if body != nil {
		h.Set("Content-Type", "application/json; charset=utf-8")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body))}
}

// mintCount is how many requests the token endpoint saw.
func (b *teamsBot) mintCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.mints)
}

// minted is the token endpoint's requests, in order.
func (b *teamsBot) minted() []teamsMint {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]teamsMint(nil), b.mints...)
}

// setConnectorToken is the Connector token the service accepts, and the
// token endpoint issues, from now on.
func (b *teamsBot) setConnectorToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = token
}

// failEntra makes the token endpoint answer the next requests with status.
func (b *teamsBot) failEntra(status ...int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range status {
		b.entraScript = append(b.entraScript, teamsFailure{Status: s})
	}
}
