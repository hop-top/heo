package xrrx

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	xrr "hop.top/xrr"
)

// The "real" sandbox of these tests. None of it is an actual account.
const (
	realRepo    = "acme-person/box-of-issues"
	realLogin   = "acme-person"
	realAccount = 4242
)

// fakeGitHub is the network under the sandbox transport: it records every
// request it is sent and answers from a table.
type fakeGitHub struct {
	mu      sync.Mutex
	seen    []string
	private string // the "private" value the repository check answers
	status  int    // the repository check's status
	answer  func(*http.Request) (int, http.Header, string)
}

func (f *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	status, h, body := http.StatusOK, http.Header{"Content-Type": {"application/json; charset=utf-8"}}, ""
	switch {
	case r.URL.Path == "/repos/"+realRepo:
		status = f.status
		if status == 0 {
			status = http.StatusOK
		}
		body = `{"full_name":"` + realRepo + `","private":` + f.private + `,"visibility":"private"}`
	case f.answer != nil:
		status, h, body = f.answer(r)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (f *fakeGitHub) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var w []string
	for _, s := range f.seen {
		if !strings.HasPrefix(s, "GET ") {
			w = append(w, s)
		}
	}
	return w
}

func sandboxOf(f *fakeGitHub) (*GitHubSandbox, *http.Client) {
	g := &GitHubSandbox{Repo: realRepo, Login: realLogin, AccountID: realAccount}
	return g, &http.Client{Transport: g.Transport(f)}
}

func do(t *testing.T, c *http.Client, method, url, body string) (*http.Response, string, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

const comment = `{
 "url":"https://api.github.com/repos/acme-person/box-of-issues/issues/comments/77",
 "html_url":"https://github.com/acme-person/box-of-issues/issues/3#issuecomment-77",
 "issue_url":"https://api.github.com/repos/Acme-Person/Box-Of-Issues/issues/3",
 "id":77,"node_id":"IC_kwDOreal77",
 "user":{"login":"acme-person","id":4242,"node_id":"MDQ6VXNlcjQyNDI=","email":"acme@person.example",
   "avatar_url":"https://avatars.githubusercontent.com/u/4242?v=4","gravatar_id":"abcdef",
   "url":"https://api.github.com/users/acme-person","html_url":"https://github.com/acme-person","type":"User"},
 "created_at":"2026-10-12T10:00:00Z","updated_at":"2026-10-12T10:00:00Z",
 "body":"hi\n\n<!-- uma:00000ad0-0000-7000-8000-0000000001c1:00000ad0-0000-7000-8000-0000000001c2 -->",
 "reactions":{"url":"https://api.github.com/repos/acme-person/box-of-issues/issues/comments/77/reactions","total_count":0}}`

// TestSandboxTransportSendsRealNamesAndKeepsNoneOfThem: a request that
// names the placeholder repository reaches GitHub under the real one, and
// the answer comes back with the account, the repository, every user,
// node ID, avatar and email replaced, and the rate-limit headers that
// change on every call dropped.
func TestSandboxTransportSendsRealNamesAndKeepsNoneOfThem(t *testing.T) {
	f := &fakeGitHub{private: "true", answer: func(r *http.Request) (int, http.Header, string) {
		h := http.Header{
			"Content-Type":      {"application/json; charset=utf-8"},
			"X-Ratelimit-Used":  {"17"},
			"X-Ratelimit-Reset": {"1791800000"},
			"Link":              {`<https://api.github.com/repositories/9/issues/3/comments?page=2>; rel="next", <https://api.github.com/repos/acme-person/box-of-issues/issues/3/comments?page=2>; rel="last"`},
			"Location":          {"https://api.github.com/repos/acme-person/box-of-issues/issues/comments/77"},
		}
		return http.StatusCreated, h, comment
	}}
	_, c := sandboxOf(f)
	resp, body, err := do(t, c, http.MethodPost, "https://api.github.com/repos/owner/sandbox/issues/3/comments", `{"body":"hi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.writes(); len(got) != 1 || got[0] != "POST /repos/"+realRepo+"/issues/3/comments" {
		t.Fatalf("GitHub was sent %v, want the real repository path", got)
	}
	all := body
	for k, vs := range resp.Header {
		all += "\n" + k + ": " + strings.Join(vs, ", ")
	}
	for _, real := range []string{realLogin, realRepo, "box-of-issues", "avatars.githubusercontent", "gravatar_id\":\"abcdef", "MDQ6VXNl", "IC_kwDOreal", "acme@person", "4242"} {
		if strings.Contains(strings.ToLower(all), strings.ToLower(real)) {
			t.Errorf("the answer still holds %q:\n%s", real, all)
		}
	}
	for _, want := range []string{
		`"login":"` + SandboxLogin + `"`, `"id":9100001`, `"node_id":"NODE_ID"`, SandboxRepo + "/issues/comments/77",
		`"id":77`, "https://api.github.com/users/" + SandboxLogin, "uma:<operation>:<attempt>",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the answer lacks %q:\n%s", want, all)
		}
	}
	for k := range resp.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-ratelimit") {
			t.Errorf("header %s survived", k)
		}
	}
	if resp.Header.Get("Location") == "" || !strings.Contains(resp.Header.Get("Location"), SandboxRepo) {
		t.Errorf("Location = %q", resp.Header.Get("Location"))
	}
}

// TestSandboxTransportAssertsPrivateBeforeEveryWrite: the guard. A write
// is sent only after the repository answered private, asked again before
// each write; a repository that is public, unreadable, or answers
// something else gets no write at all.
func TestSandboxTransportAssertsPrivateBeforeEveryWrite(t *testing.T) {
	ok := func(*http.Request) (int, http.Header, string) {
		return 200, http.Header{"Content-Type": {"application/json"}}, `{}`
	}
	for name, f := range map[string]*fakeGitHub{
		"public":       {private: "false", answer: ok},
		"missing flag": {private: "null", answer: ok},
		"unreadable":   {private: "true", status: http.StatusNotFound, answer: ok},
		"forbidden":    {private: "true", status: http.StatusForbidden, answer: ok},
	} {
		_, c := sandboxOf(f)
		for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
			if _, _, err := do(t, c, m, "https://api.github.com/repos/owner/sandbox/issues/3/comments", `{}`); err == nil {
				t.Errorf("%s %s: the write was allowed", name, m)
			}
		}
		if w := f.writes(); len(w) != 0 {
			t.Errorf("%s: GitHub was sent %v", name, w)
		}
	}

	f := &fakeGitHub{private: "true", answer: ok}
	_, c := sandboxOf(f)
	for _, m := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		if _, _, err := do(t, c, m, "https://api.github.com/repos/owner/sandbox/issues/3/comments", `{}`); err != nil {
			t.Fatalf("%s on a private repository: %v", m, err)
		}
	}
	want := []string{"GET /repos/" + realRepo, "POST /repos/" + realRepo + "/issues/3/comments", "GET /repos/" + realRepo,
		"PATCH /repos/" + realRepo + "/issues/3/comments", "GET /repos/" + realRepo, "DELETE /repos/" + realRepo + "/issues/3/comments"}
	if strings.Join(f.seen, "|") != strings.Join(want, "|") {
		t.Errorf("GitHub saw\n%v\nwant a private check before each write\n%v", f.seen, want)
	}

	// The repository turns public between two writes: the second is refused.
	f2 := &fakeGitHub{private: "true", answer: ok}
	_, c2 := sandboxOf(f2)
	if _, _, err := do(t, c2, http.MethodPost, "https://api.github.com/repos/owner/sandbox/issues/3/comments", `{}`); err != nil {
		t.Fatal(err)
	}
	f2.mu.Lock()
	f2.private = "false"
	f2.mu.Unlock()
	if _, _, err := do(t, c2, http.MethodPost, "https://api.github.com/repos/owner/sandbox/issues/3/comments", `{}`); err == nil {
		t.Error("a write to a repository that turned public was allowed")
	}
}

// TestSandboxTransportReachesOnlyTheSandboxIssues: nothing but the
// sandbox repository's issue endpoints on api.github.com is reachable.
func TestSandboxTransportReachesOnlyTheSandboxIssues(t *testing.T) {
	f := &fakeGitHub{private: "true"}
	_, c := sandboxOf(f)
	for _, u := range []string{
		"https://api.github.com/user",
		"https://api.github.com/repos/owner/sandbox",
		"https://api.github.com/repos/owner/sandbox/hooks",
		"https://api.github.com/repos/owner/sandbox/actions/runs",
		"https://api.github.com/repos/owner/sandbox/issuesx",
		"https://api.github.com/repos/owner/sandbox-two/issues/1",
		"https://api.github.com/repos/" + realRepo + "/issues/1",
		"https://api.github.com/repos/owner/sandbox/issues/../hooks",
		"https://api.github.com/orgs/owner/repos",
		"https://api.github.com/gists",
		"https://example.com/repos/owner/sandbox/issues/1",
		"http://api.github.com/repos/owner/sandbox/issues/1",
		"https://api.github.com.evil.example/repos/owner/sandbox/issues/1",
	} {
		if _, _, err := do(t, c, http.MethodGet, u, ""); err == nil {
			t.Errorf("GET %s was sent", u)
		}
	}
	for _, m := range []string{http.MethodPut, http.MethodHead, http.MethodOptions} {
		if _, _, err := do(t, c, m, "https://api.github.com/repos/owner/sandbox/issues/1", ""); err == nil {
			t.Errorf("%s was sent", m)
		}
	}
	if len(f.seen) != 0 {
		t.Errorf("GitHub was sent %v", f.seen)
	}
	for _, ok := range []string{
		"https://api.github.com/repos/owner/sandbox/issues",
		"https://api.github.com/repos/owner/sandbox/issues/1",
		"https://api.github.com/repos/owner/sandbox/issues/comments/9/reactions?per_page=100",
	} {
		if _, _, err := do(t, c, http.MethodGet, ok, ""); err != nil {
			t.Errorf("GET %s: %v", ok, err)
		}
	}
}

// TestSandboxTransportRefusesAnAnswerThatStillNamesTheAccount: a body the
// scrub cannot make clean is not handed on to be recorded.
func TestSandboxTransportRefusesAnAnswerThatStillNamesTheAccount(t *testing.T) {
	for name, body := range map[string]string{ //nolint:gosec // G101: token-shaped test strings, not credentials
		"avatar in prose": `{"body":"see https://avatars.githubusercontent.com/u/4242"}`,
		"gravatar":        `{"body":"https://secure.gravatar.com/avatar/abc"}`,
		"a token":         `{"body":"ghp_0123456789abcdefghijABCDEFGHIJ012345"}`,
	} {
		f := &fakeGitHub{private: "true", answer: func(*http.Request) (int, http.Header, string) {
			return 200, http.Header{"Content-Type": {"application/json"}}, body
		}}
		_, c := sandboxOf(f)
		if _, _, err := do(t, c, http.MethodGet, "https://api.github.com/repos/owner/sandbox/issues/1", ""); err == nil {
			t.Errorf("%s: the answer was handed on", name)
		}
	}
}

// TestSandboxScrubJSON: shapes the transport meets: nested objects and
// arrays, other users, numbers that are not ids, escaping kept readable.
func TestSandboxScrubJSON(t *testing.T) {
	g := &GitHubSandbox{Repo: realRepo, Login: realLogin, AccountID: realAccount}
	out, err := g.ScrubJSON([]byte(`[{"user":{"login":"somebody-else","id":99,"node_id":"U_x","avatar_url":"u"},"n":4242,"title":"a <b> & c",
		"owner":{"login":"Acme-Person","id":4242}}]`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"login":"sandbox-user"`, `"id":9100002`, `"login":"sandbox-account"`, `"id":9100001`, `"n":4242`, `"title":"a <b> \u0026 c"`} {
		if want == `"title":"a <b> \u0026 c"` {
			want = `"title":"a <b> & c"`
		}
		if !strings.Contains(s, want) {
			t.Errorf("scrubbed JSON lacks %q:\n%s", want, s)
		}
	}
	// A user object keeps who and what kind, and nothing that names or
	// describes a person.
	out, err = g.ScrubJSON([]byte(`{"user":{"login":"somebody-else","id":99,"type":"User","site_admin":false,"name":"A Person","email":"a@b.example",
		"followers_url":"https://api.github.com/users/somebody-else/followers","company":"Acme","bio":"hi","url":"https://api.github.com/users/somebody-else"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"A Person", "a@b.example", "followers_url", "Acme", "bio", "somebody-else"} {
		if strings.Contains(string(out), gone) {
			t.Errorf("a user object still holds %q: %s", gone, out)
		}
	}
	if !strings.Contains(string(out), `"url":"https://api.github.com/users/sandbox-user"`) {
		t.Errorf("user url: %s", out)
	}
	if _, err := g.ScrubJSON([]byte(`{"a":`)); err == nil {
		t.Error("malformed JSON was scrubbed")
	}
}

// TestSandboxLearnsTheAccountFromTheFirstAnswer: a recording process that
// does not know the account yet takes it from the first user GitHub names.
func TestSandboxLearnsTheAccountFromTheFirstAnswer(t *testing.T) {
	f := &fakeGitHub{private: "true", answer: func(*http.Request) (int, http.Header, string) {
		return 201, http.Header{"Content-Type": {"application/json"}}, comment
	}}
	g := &GitHubSandbox{Repo: realRepo, Learn: true}
	c := &http.Client{Transport: g.Transport(f)}
	_, body, err := do(t, c, http.MethodPost, "https://api.github.com/repos/owner/sandbox/issues", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if g.Login != realLogin || g.AccountID != realAccount {
		t.Errorf("learned %q / %d", g.Login, g.AccountID)
	}
	if strings.Contains(body, realLogin) || !strings.Contains(body, SandboxLogin) {
		t.Errorf("answer: %s", body)
	}
}

// TestSandboxFromEnv: the child process of a recording is told the real
// names through its environment; incomplete or malformed settings are
// refused, and so is a repository not of the form owner/name.
func TestSandboxFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	g, err := GitHubSandboxFromEnv(env(map[string]string{EnvGitHubRepo: realRepo, EnvGitHubLogin: realLogin, EnvGitHubAccountID: "4242"}))
	if err != nil || g.Repo != realRepo || g.Login != realLogin || g.AccountID != 4242 {
		t.Fatalf("%+v, %v", g, err)
	}
	for name, m := range map[string]map[string]string{
		"no repo":      {EnvGitHubLogin: realLogin, EnvGitHubAccountID: "4242"},
		"bad repo":     {EnvGitHubRepo: "no-slash", EnvGitHubLogin: realLogin, EnvGitHubAccountID: "4242"},
		"extra slash":  {EnvGitHubRepo: "a/b/c", EnvGitHubLogin: realLogin, EnvGitHubAccountID: "4242"},
		"no login":     {EnvGitHubRepo: realRepo, EnvGitHubAccountID: "4242"},
		"bad account":  {EnvGitHubRepo: realRepo, EnvGitHubLogin: realLogin, EnvGitHubAccountID: "x"},
		"zero account": {EnvGitHubRepo: realRepo, EnvGitHubLogin: realLogin, EnvGitHubAccountID: "0"},
	} {
		if _, err := GitHubSandboxFromEnv(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The learning form (the recording's parent) needs the repository alone.
	if g, err := NewGitHubSandbox(env(map[string]string{EnvGitHubRepo: realRepo})); err != nil || !g.Learn {
		t.Errorf("NewGitHubSandbox: %+v, %v", g, err)
	}
}

// TestCheckLive: live GitHub traffic needs the record mode, the explicit
// flag, no CI, and the sandbox named; replay and passthrough never do.
func TestCheckLive(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	base := map[string]string{xrr.EnvMode: "record", EnvLive: "1", EnvGitHubRepo: realRepo}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range base {
			m[a] = b
		}
		m[k] = v
		return m
	}
	if err := CheckLive(env(base)); err != nil {
		t.Fatalf("record + live: %v", err)
	}
	for name, m := range map[string]map[string]string{
		"replay":       with(xrr.EnvMode, "replay"),
		"passthrough":  with(xrr.EnvMode, "passthrough"),
		"no mode":      with(xrr.EnvMode, ""),
		"flag unset":   with(EnvLive, ""),
		"flag not 1":   with(EnvLive, "true"),
		"CI":           with("CI", "true"),
		"no repo":      with(EnvGitHubRepo, ""),
		"bad repo":     with(EnvGitHubRepo, "x"),
		"redact allow": with(xrr.EnvRedactAllow, "token"),
	} {
		if err := CheckLive(env(m)); err == nil {
			t.Errorf("%s: live traffic allowed", name)
		}
	}
	if err := CheckLive(env(with("CI", "true"))); !errors.Is(err, ErrRecordInCI) {
		t.Errorf("CI: %v, want ErrRecordInCI", err)
	}
}

// TestGitHubIsReachedOnlyByALiveRecording: through the process's
// transport, api.github.com is refused in record mode without the live
// flag and in passthrough, and never forwarded in replay; with the sandbox
// it goes through the sandbox transport, where the placeholder repository
// is translated.
func TestGitHubIsReachedOnlyByALiveRecording(t *testing.T) {
	reached := false
	spy := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reached = true
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})
	for _, mode := range []xrr.Mode{xrr.ModeRecord, xrr.ModePassthrough} {
		e := &Env{Mode: mode, Dir: t.TempDir()}
		c := &http.Client{Transport: e.Transport(spy)}
		if _, _, err := do(t, c, http.MethodGet, "https://api.github.com/repos/owner/sandbox/issues/1", ""); err == nil || reached {
			t.Errorf("%s without the sandbox: err %v, reached %v", mode, err, reached)
		}
	}
	f := &fakeGitHub{private: "true"}
	e := &Env{Mode: xrr.ModeRecord, Dir: t.TempDir(), GitHub: &GitHubSandbox{Repo: realRepo, Login: realLogin, AccountID: realAccount}}
	c := &http.Client{Transport: e.Transport(f)}
	if _, _, err := do(t, c, http.MethodGet, "https://api.github.com/repos/owner/sandbox/issues/1", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 1 || f.seen[0] != "GET /repos/"+realRepo+"/issues/1" {
		t.Errorf("GitHub saw %v", f.seen)
	}
}

// TestOperationMarkerIsScrubbedFromRequests: the echo marker uma appends
// to a comment names the operation and attempt, new on every run; it must
// not change a request's fingerprint.
func TestOperationMarkerIsScrubbedFromRequests(t *testing.T) {
	a := ScrubRequestText(`{"body":"hi\n\n\u003c!-- uma:00000ad0-0000-7000-8000-0000000001c1:00000ad0-0000-7000-8000-0000000001c2 --\u003e"}`)
	b := ScrubRequestText(`{"body":"hi\n\n\u003c!-- uma:00000ad0-0000-7000-8000-0000000099aa:00000ad0-0000-7000-8000-0000000099bb --\u003e"}`)
	if a != b || !strings.Contains(a, "uma:<operation>:<attempt>") {
		t.Errorf("markers differ after the scrub:\n%s\n%s", a, b)
	}
}
