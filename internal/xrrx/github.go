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
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"

	xrr "hop.top/xrr"
)

// Environment of a live GitHub recording.
const (
	// EnvLive must be "1" for a recording to reach GitHub at all, besides
	// XRR_MODE=record. Replay never needs it and never reaches anything.
	EnvLive = "HEO_RECORD_LIVE"
	// EnvGitHubRepo names the sandbox repository, owner/name. It must be
	// private; the recording checks before every write.
	EnvGitHubRepo = "HEO_RECORD_GITHUB_REPO"
	// EnvGitHubLogin and EnvGitHubAccountID tell the recorded process
	// who the credential acts as, so the scrub can name no one. The
	// recording's parent learns them from the first answer GitHub gives and
	// passes them on (GitHubSandbox.ChildEnv).
	EnvGitHubLogin     = "HEO_RECORD_GITHUB_LOGIN"
	EnvGitHubAccountID = "HEO_RECORD_GITHUB_ACCOUNT_ID"
	// EnvGitHubToken is the sandbox credential, read by the recording's
	// parent only. It is never passed to the recorded process: heo reads
	// its credential from the credential store.
	EnvGitHubToken = "HEO_RECORD_GITHUB_TOKEN" // #nosec G101 -- the name of a variable, not a credential

	githubHost = "api.github.com"
)

// markerRe is the echo marker uma appends to a comment it posts: the
// operation and attempt, new on every run.
var markerRe = regexp.MustCompile(`uma:[0-9a-fA-F-]{36}:[0-9a-fA-F-]{36}`)

// Marker replaces the identifiers in an echo marker.
const Marker = "uma:<operation>:<attempt>"

// CheckLive says whether this environment may send recorded traffic to
// the real GitHub sandbox: XRR_MODE=record, HEO_RECORD_LIVE=1, no CI, the
// redaction left on, and a sandbox repository named owner/name. Replay and
// passthrough never may.
func CheckLive(getenv func(string) string) error {
	if err := checkLiveBase(getenv, "GitHub"); err != nil {
		return err
	}
	if _, err := splitRepo(getenv(EnvGitHubRepo)); err != nil {
		return fmt.Errorf("xrrx: %s: %w", EnvGitHubRepo, err)
	}
	return nil
}

// checkLiveBase is what any live recording needs, whoever the provider:
// record mode, the live flag, no CI and the redaction left on.
func checkLiveBase(getenv func(string) string, provider string) error {
	if getenv(xrr.EnvMode) != string(xrr.ModeRecord) {
		return fmt.Errorf("xrrx: live %s traffic only in %s=%s", provider, xrr.EnvMode, xrr.ModeRecord)
	}
	if err := CheckRecord(getenv); err != nil {
		return err
	}
	if getenv(EnvLive) != "1" {
		return fmt.Errorf("xrrx: live %s traffic needs %s=1", provider, EnvLive)
	}
	return nil
}

func splitRepo(s string) ([2]string, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(s, " ?#%") {
		return [2]string{}, errors.New(`want "owner/name"`)
	}
	return [2]string{owner, name}, nil
}

// GitHubSandbox is the one private repository a live recording writes to,
// and what the recording must never keep of it. The recorded process
// names the placeholder repository (SandboxRepo) and sees the placeholder
// account; Transport translates between those and the real ones, and
// refuses everything else.
type GitHubSandbox struct {
	// Repo is the real sandbox repository, owner/name.
	Repo string
	// Login and AccountID are the real account the credential acts as.
	Login     string
	AccountID int64
	// Learn takes Login and AccountID from the first user GitHub names,
	// for the process that creates the sandbox's first issue.
	Learn bool

	mu sync.Mutex
}

// NewGitHubSandbox is the sandbox of the process that drives a recording
// (it creates the issue the recorded process works on): only the
// repository is known, and the account is learned from GitHub's first
// answer.
func NewGitHubSandbox(getenv func(string) string) (*GitHubSandbox, error) {
	if _, err := splitRepo(getenv(EnvGitHubRepo)); err != nil {
		return nil, fmt.Errorf("xrrx: %s: %w", EnvGitHubRepo, err)
	}
	return &GitHubSandbox{Repo: getenv(EnvGitHubRepo), Learn: true}, nil
}

// GitHubSandboxFromEnv is the sandbox of the recorded process: the
// repository and the account, all given.
func GitHubSandboxFromEnv(getenv func(string) string) (*GitHubSandbox, error) {
	if _, err := splitRepo(getenv(EnvGitHubRepo)); err != nil {
		return nil, fmt.Errorf("xrrx: %s: %w", EnvGitHubRepo, err)
	}
	id, err := strconv.ParseInt(getenv(EnvGitHubAccountID), 10, 64)
	if err != nil || id <= 0 || getenv(EnvGitHubLogin) == "" {
		return nil, fmt.Errorf("xrrx: %s and %s name the account the credential acts as", EnvGitHubLogin, EnvGitHubAccountID)
	}
	return &GitHubSandbox{Repo: getenv(EnvGitHubRepo), Login: getenv(EnvGitHubLogin), AccountID: id}, nil
}

// ChildEnv is what the recorded process is told of the sandbox.
func (g *GitHubSandbox) ChildEnv() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return []string{EnvLive + "=1", EnvGitHubRepo + "=" + g.Repo, EnvGitHubLogin + "=" + g.Login, EnvGitHubAccountID + "=" + strconv.FormatInt(g.AccountID, 10)}
}

// Deny lists the literals a recording of this sandbox must not hold, for
// AuditWith: the real repository, its name and the account's login.
func (g *GitHubSandbox) Deny() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	name := g.Repo
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return []string{g.Repo, name, g.Login}
}

func (g *GitHubSandbox) identity() (string, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Login, g.AccountID
}

// Transport returns the RoundTripper that sends requests to the sandbox
// repository through base (http.DefaultTransport when nil). A request must
// be to https://api.github.com, name the placeholder repository and an
// issues endpoint (issues, comments and reactions), and use GET, POST,
// PATCH or DELETE; anything else fails before it is sent. A write is sent
// only after the real repository answered private, asked again before
// each write. The answer comes back with the account, the repository,
// users, node IDs, avatars and emails replaced, and the rate-limit
// headers, which change on every call, dropped; an answer that still names
// what it must not is not handed on.
func (g *GitHubSandbox) Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		out, err := g.translate(req)
		if err != nil {
			return nil, err
		}
		if req.Method != http.MethodGet {
			if err := g.requirePrivate(req.Context(), base, req.Header.Get("Authorization")); err != nil {
				return nil, err
			}
		}
		resp, err := base.RoundTrip(out)
		if err != nil {
			return nil, err
		}
		return g.scrubResponse(resp)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// translate is req aimed at the real repository, or an error when req may
// not be sent.
func (g *GitHubSandbox) translate(req *http.Request) (*http.Request, error) {
	u := req.URL
	refuse := func(why string) (*http.Request, error) {
		return nil, fmt.Errorf("xrrx: GitHub sandbox: %s %s refused: %s", req.Method, u.Redacted(), why)
	}
	switch req.Method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete:
	default:
		return refuse("only GET, POST, PATCH and DELETE")
	}
	if u.Scheme != "https" || u.Host != githubHost || u.User != nil {
		return refuse("only https://" + githubHost)
	}
	p := u.EscapedPath()
	if p != path.Clean(p) || strings.Contains(p, "%") {
		return refuse("path is not canonical")
	}
	prefix := "/repos/" + SandboxRepo + "/issues"
	if p != prefix && !strings.HasPrefix(p, prefix+"/") {
		return refuse("only the sandbox repository's issue endpoints")
	}
	out := req.Clone(req.Context())
	out.URL.Path = "/repos/" + g.Repo + strings.TrimPrefix(p, "/repos/"+SandboxRepo)
	out.URL.RawPath = ""
	out.Host = ""
	return out, nil
}

// requirePrivate fails unless the real sandbox repository answers private
// right now.
func (g *GitHubSandbox) requirePrivate(ctx context.Context, base http.RoundTripper, auth string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+githubHost+"/repos/"+g.Repo, nil) //nolint:gosec // G704: a fixed host; the repository was validated as owner/name
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("xrrx: GitHub sandbox: cannot confirm the repository is private, write refused: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var r struct {
		Private  *bool  `json:"private"`
		FullName string `json:"full_name"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &r) != nil || r.Private == nil || !*r.Private || !strings.EqualFold(r.FullName, g.Repo) {
		return errors.New("xrrx: GitHub sandbox: the repository is not confirmed private, write refused")
	}
	return nil
}

var (
	rateLimitRe = regexp.MustCompile(`(?i)^x-ratelimit-`)
	// residueRe finds what must not survive the scrub, whatever field it
	// is in: an avatar or gravatar URL and a GitHub token.
	residueRe = regexp.MustCompile(`(?i)avatars\.githubusercontent\.com|gravatar\.com|\bgh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`)
)

func (g *GitHubSandbox) scrubResponse(resp *http.Response) (*http.Response, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if g.Learn {
		g.learn(raw)
	}
	login, _ := g.identity()
	if login == "" {
		return nil, errors.New("xrrx: GitHub sandbox: the account is not known, answer refused")
	}
	body := raw
	if len(bytes.TrimSpace(raw)) > 0 {
		if body, err = g.ScrubJSON(raw); err != nil {
			// Not JSON: scrub it as text.
			body = []byte(g.scrubString(string(raw)))
		}
	}
	for k, vs := range resp.Header {
		if rateLimitRe.MatchString(k) {
			delete(resp.Header, k)
			continue
		}
		for i, v := range vs {
			vs[i] = g.scrubString(v)
		}
		resp.Header[k] = vs
	}
	all := string(body)
	for _, vs := range resp.Header {
		all += "\n" + strings.Join(vs, "\n")
	}
	if m := residueRe.FindString(all); m != "" {
		return nil, fmt.Errorf("xrrx: GitHub sandbox: the answer still holds %q after the scrub, not handed on", m)
	}
	for _, d := range g.Deny() {
		if d != "" && strings.Contains(strings.ToLower(all), strings.ToLower(d)) {
			return nil, errors.New("xrrx: GitHub sandbox: the answer still names the sandbox after the scrub, not handed on")
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Encoding")
	return resp, nil
}

// learn takes the account from the first user object in raw.
func (g *GitHubSandbox) learn(raw []byte) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.Login != "" {
		return
	}
	var find func(any) bool
	find = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if u, ok := t["user"].(map[string]any); ok {
				login, _ := u["login"].(string)
				id, _ := u["id"].(float64)
				if login != "" && id > 0 {
					g.Login, g.AccountID = login, int64(id)
					return true
				}
			}
			for _, c := range t {
				if find(c) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if find(c) {
					return true
				}
			}
		}
		return false
	}
	find(v)
}

// scrubString replaces the real repository and login, in any case, and
// the operation marker.
func (g *GitHubSandbox) scrubString(s string) string {
	login, _ := g.identity()
	s = replaceFold(s, g.Repo, SandboxRepo)
	if login != "" {
		s = replaceFold(s, login, SandboxLogin)
	}
	return markerRe.ReplaceAllString(s, Marker)
}

// replaceFold is strings.ReplaceAll that ignores case in old.
func replaceFold(s, old, repl string) string {
	if old == "" {
		return s
	}
	return regexp.MustCompile(`(?i)`+regexp.QuoteMeta(old)).ReplaceAllLiteralString(s, repl)
}

// ScrubJSON rewrites a GitHub JSON document so it names no one: every
// user becomes the placeholder account or the placeholder user, with no
// email, name, avatar or gravatar; every node_id is NODE_ID; the
// repository, the login and the operation marker are replaced wherever
// they appear in a string. Other numbers, such as issue and comment IDs,
// stay: the recorded process reads them from one answer and sends them in
// the next. Keys come out sorted, so a recording does not depend on the
// order GitHub chose.
func (g *GitHubSandbox) ScrubJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("xrrx: more than one JSON document")
	}
	login, account := g.identity()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(g.scrubValue(v, login, account)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// userKept are the fields of a user object that stay, scrubbed: who, what
// kind, and the two links. Everything else (emails, names, the dozen
// per-user collection URLs) names a person or only adds noise.
var userKept = map[string]bool{
	"login": true, "id": true, "node_id": true, "type": true, "site_admin": true,
	"avatar_url": true, "url": true, "html_url": true,
}

func (g *GitHubSandbox) scrubValue(v any, login string, account int64) any {
	switch t := v.(type) {
	case string:
		return g.scrubString(t)
	case []any:
		for i := range t {
			t[i] = g.scrubValue(t[i], login, account)
		}
		return t
	case map[string]any:
		if l, ok := t["login"].(string); ok {
			isAccount := strings.EqualFold(l, login) || strings.EqualFold(l, SandboxLogin)
			id, placeholderLogin := SandboxUserID, SandboxUser
			if isAccount {
				id, placeholderLogin = SandboxAccountID, SandboxLogin
			}
			for k := range t {
				if !userKept[k] {
					delete(t, k)
				}
			}
			for _, k := range []string{"url", "html_url"} {
				if u, ok := t[k].(string); ok {
					t[k] = replaceFold(u, l, placeholderLogin)
				}
			}
			t["login"] = placeholderLogin
			if _, has := t["id"]; has {
				t["id"] = json.Number(strconv.FormatInt(id, 10))
			}
			if _, has := t["avatar_url"]; has {
				t["avatar_url"] = "https://avatars.invalid/u/" + strconv.FormatInt(id, 10)
			}
		}
		for k, c := range t {
			switch k {
			case "node_id":
				t[k] = NodeIDPlaceholder
			case "login", "avatar_url":
			default:
				t[k] = g.scrubValue(c, login, account)
			}
		}
		return t
	}
	return v
}

// Client is a GitHub sandbox client for the process that drives a
// recording: it seeds the issue the recorded process works on and cleans
// up. It goes through Transport, so it can reach nothing else.
type Client struct {
	g     *GitHubSandbox
	token string
	hc    *http.Client
}

// Client returns a client that signs with token over base (the default
// transport when nil).
func (g *GitHubSandbox) Client(token string, base http.RoundTripper) *Client {
	return &Client{g: g, token: token, hc: &http.Client{Transport: g.Transport(base)}}
}

// Do sends one request to path, which names the placeholder repository
// (/repos/owner/sandbox/issues...), with body encoded as JSON when not
// nil. It returns the scrubbed answer; a status of 300 or more is an
// error that carries it.
func (c *Client) Do(ctx context.Context, method, p string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := url.URL{Scheme: "https", Host: githubHost, Path: p}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
		return b, fmt.Errorf("xrrx: GitHub sandbox: %s %s: %s", method, p, resp.Status)
	}
	return b, nil
}
