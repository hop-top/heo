package xrrx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const cleanReq = `xrr: "1"
adapter: http
fingerprint: aaaaaaaa
recorded_at: "2026-10-12T17:09:43Z"
payload:
    method: POST
    url: http://loopback.invalid/v1/messages
    headers:
        Content-Type: application/json
    body: '{"text":"hi","api_key":"<redacted:API_KEY>"}'
`

// TestAuditFlagsEachLeak: every kind of leak the scrub rules exist for is
// found by name in a cassette, and a clean cassette passes, its own
// recorded_at aside.
func TestAuditFlagsEachLeak(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "http-aaaaaaaa.req.yaml", cleanReq)
	if fs, err := Audit(dir); err != nil || len(fs) != 0 {
		t.Fatalf("clean cassette: %v, %v", fs, err)
	}
	for rule, mutate := range map[string]func(string) string{
		"credential header": func(s string) string {
			return strings.Replace(s, "Content-Type: application/json", "Authorization: x", 1)
		},
		"bearer or basic credential": func(s string) string { return strings.Replace(s, `"text":"hi"`, `"text":"Bearer abcdefghijklmnop"`, 1) },
		"loopback host:port":         func(s string) string { return strings.Replace(s, "loopback.invalid", "127.0.0.1:41234", 1) },
		"temp path":                  func(s string) string { return strings.Replace(s, `"text":"hi"`, `"text":"/tmp/heo123/x"`, 1) },
		"process id":                 func(s string) string { return strings.Replace(s, `"text":"hi"`, `"text":"pid=4242"`, 1) },
		"timestamp in a request":     func(s string) string { return strings.Replace(s, `"text":"hi"`, `"text":"2026-10-12T10:11:12Z"`, 1) },
		"unredacted secret field": func(s string) string {
			return strings.Replace(s, "headers:", "env:\n        PGPASSWORD: hunter2\n    headers:", 1)
		},
		"not yaml": func(string) string { return "payload: [unclosed" },
	} {
		d := t.TempDir()
		write(t, d, "http-aaaaaaaa.req.yaml", mutate(cleanReq))
		fs, err := Audit(d)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range fs {
			found = found || f.Rule == rule
		}
		if !found {
			t.Errorf("%s not found: %v", rule, fs)
		}
	}
	// A response may carry a timestamp: the adapter parses it.
	d := t.TempDir()
	write(t, d, "http-aaaaaaaa.resp.yaml", strings.Replace(cleanReq, `"text":"hi"`, `"text":"2026-10-12T10:11:12Z"`, 1))
	if fs, _ := Audit(d); len(fs) != 0 {
		t.Errorf("a response timestamp was flagged: %v", fs)
	}
}

// ghBody is a request body carrying one GitHub-shaped leak at a time.
const ghReq = `xrr: "1"
adapter: http
fingerprint: bbbbbbbb
recorded_at: "2026-10-12T17:09:43Z"
payload:
    method: GET
    url: https://api.github.com/repos/owner/sandbox/issues/1
    headers:
        Content-Type: application/json
    body: '%s'
`

// TestAuditFlagsGitHubIdentity: a GitHub token of every shape, the
// sandbox account and repository names, an avatar or gravatar URL, a
// node ID, a user login that is not a placeholder and an email address
// each fail the audit by name; the placeholders the scrub leaves pass.
func TestAuditFlagsGitHubIdentity(t *testing.T) {
	body := func(s string) string { return strings.Replace(ghReq, "%s", s, 1) }
	clean := body(`{"user":{"login":"sandbox-account","id":9100001,"node_id":"NODE_ID","avatar_url":"https://avatars.invalid/u/9100001"},"repo":"owner/sandbox","by":"a@example.invalid"}`)
	d := t.TempDir()
	write(t, d, "http-bbbbbbbb.resp.yaml", clean)
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("the placeholders were flagged: %v, %v", fs, err)
	}
	for _, c := range []struct{ rule, leak string }{
		{"github token", `ghp_0123456789abcdefghijABCDEFGHIJ012345`},
		{"github token", `gho_0123456789abcdefghijABCDEFGHIJ012345`},
		{"github token", `ghs_0123456789abcdefghijABCDEFGHIJ012345`},
		{"github token", `ghu_0123456789abcdefghijABCDEFGHIJ012345`},
		{"github token", `ghr_0123456789abcdefghijABCDEFGHIJ012345`},
		{"github token", `github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ012345`},
		{"sandbox identity", `jadb`},
		{"sandbox identity", `https://api.github.com/repos/x/heo-adapter-sandbox`},
		{"sandbox identity", `JADB`},
		{"avatar or gravatar URL", `https://avatars.githubusercontent.com/u/1?v=4`},
		{"avatar or gravatar URL", `https://www.gravatar.com/avatar/abc`},
		{"node id", `{"node_id":"I_kwDOABCDEF"}`},
		{"user login", `{"login":"someone"}`},
		{"email address", `person@company.org`},
	} {
		rule, leak := c.rule, c.leak
		name := rule
		for _, in := range []string{body(leak), strings.ReplaceAll(body(leak), `"`, `\"`)} {
			d := t.TempDir()
			write(t, d, "http-bbbbbbbb.resp.yaml", in)
			fs, err := Audit(d)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range fs {
				found = found || f.Rule == name
			}
			if !found {
				t.Errorf("%s not found in %q: %v", name, leak, fs)
			}
		}
	}
}

// TestAuditReadsFixturesToo: a JSON fixture beside the cassettes is held
// to the same rules, since it is committed with them.
func TestAuditReadsFixturesToo(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "github"), "issue.json", `{"user":{"login":"jadb"}}`)
	fs, err := Audit(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) == 0 {
		t.Fatal("a JSON fixture with the sandbox account passed the audit")
	}
	write(t, filepath.Join(d, "github"), "issue.json", `{"user":{"login":"sandbox-account"}}`)
	if fs, _ := Audit(d); len(fs) != 0 {
		t.Fatalf("a placeholder fixture was flagged: %v", fs)
	}
}

// TestAuditWithDeniesTheRecordedIdentity: values only a recording knows
// (the account's real login, the real repository) are denied by name, in
// any case, in cassettes and fixtures alike.
func TestAuditWithDeniesTheRecordedIdentity(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-bbbbbbbb.resp.yaml", strings.Replace(ghReq, "%s", `Zed-Real-Person/repo-1`, 1))
	write(t, d, "fixture.json", `{"who":"zed-real-person"}`)
	if fs, _ := Audit(d); len(fs) != 0 {
		t.Fatalf("plain Audit knows no recorded identity: %v", fs)
	}
	fs, err := AuditWith(d, "zed-real-person", "", "repo-1")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]bool{}
	for _, f := range fs {
		if f.Rule == "recorded identity" {
			files[f.File] = true
		}
	}
	if !files["http-bbbbbbbb.resp.yaml"] || !files["fixture.json"] {
		t.Errorf("recorded identity not found in both files: %v", fs)
	}
}
