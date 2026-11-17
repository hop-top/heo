package xrrx

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	xrr "hop.top/xrr"
)

// Finding is one thing in a cassette that must not be there.
type Finding struct {
	// File is the cassette file, relative to the audited directory.
	File string
	// Rule names what was found.
	Rule string
	// Excerpt is the offending text, shortened.
	Excerpt string
}

func (f Finding) String() string { return fmt.Sprintf("%s: %s: %s", f.File, f.Rule, f.Excerpt) }

var (
	credentialHeaderRe = regexp.MustCompile(`(?im)^\s*(?:authorization|proxy-authorization|cookie|set-cookie|x-api-key|x-auth-token):`)
	bearerRe           = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9+/._~=-]{8,}`)
	recordedAtRe       = regexp.MustCompile(`(?m)^recorded_at:.*$`)

	// githubTokenRe is a GitHub credential of any shape: classic, OAuth,
	// user-to-server, server-to-server and refresh tokens, and the
	// fine-grained personal access token.
	githubTokenRe = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)
	// sandboxIdentityRe is the account the GitHub recordings are made with
	// and the sandbox repository: a cassette names the placeholders instead.
	sandboxIdentityRe = regexp.MustCompile(`(?i)jadb|heo-adapter-sandbox`)
	// avatarRe is an avatar or gravatar URL, which names an account.
	avatarRe = regexp.MustCompile(`(?i)avatars\.githubusercontent\.com|gravatar\.com`)
	// nodeIDRe and loginRe capture the value of a node_id or login field,
	// in JSON as written or escaped inside a YAML string.
	nodeIDRe = regexp.MustCompile(`node_id\\?"\s*:\s*\\?"([^"\\]*)`)
	loginRe  = regexp.MustCompile(`\blogin\\?"\s*:\s*\\?"([^"\\]*)`)
	// emailRe is an email address; its first group is the domain.
	emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,})`)
)

// What a GitHub recording leaves in place of the account, the sandbox and
// anyone else (see GitHubSandbox).
const (
	// NodeIDPlaceholder replaces every GitHub node_id.
	NodeIDPlaceholder = "NODE_ID"
	// SandboxRepo replaces the sandbox repository's full name.
	SandboxRepo = "owner/sandbox"
	// SandboxOwner is the owner half of SandboxRepo.
	SandboxOwner = "owner"
	// SandboxLogin replaces the recording account's login.
	SandboxLogin = "sandbox-account"
	// SandboxUser replaces any other user's login.
	SandboxUser = "sandbox-user"
	// SandboxAccountID replaces the recording account's user ID.
	SandboxAccountID int64 = 9100001
	// SandboxUserID replaces any other user's ID.
	SandboxUserID int64 = 9100002
)

// For Twilio it reports an SID, a 32-hex token or a phone number that is
// not a placeholder, the account's names, a Basic credential and a
// webhook signature.
//
// Audit reads every cassette under dir and reports what a cassette must
// not carry: a credential header, a bearer or basic credential, a field
// xrr's redactor takes for a secret that was left unredacted, a loopback
// host:port, a temp path, a process id, and, in a request, a timestamp
// (xrr's own recorded_at aside), which would change its fingerprint on
// the next run. For GitHub it also reports a token of any shape, the
// sandbox account and repository names, an avatar or gravatar URL, a
// node ID or user login that is not a placeholder, and an email address.
// It is the check that holds after the scrub rules have run, and the one
// a committed cassette must pass.
func Audit(dir string) ([]Finding, error) { return AuditWith(dir) }

// AuditWith is Audit that also denies the named literals, in any case:
// what only a recording knows, such as the account's real login or the
// real repository. Empty literals are ignored. Besides cassettes (.yaml)
// it reads the .json fixtures committed beside them.
func AuditWith(dir string, deny ...string) ([]Finding, error) {
	var out []Finding
	red := xrr.NewRedactor(xrr.RedactConfig{})
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, ".form")) {
			return err
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304: walking the cassette directory under audit
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, auditFile(rel, string(b), red, deny)...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Rule < out[j].Rule
	})
	return out, err
}

func auditFile(rel, text string, red *xrr.Redactor, deny []string) []Finding {
	var out []Finding
	add := func(rule, excerpt string) {
		if len(excerpt) > 80 {
			excerpt = excerpt[:80] + "..."
		}
		out = append(out, Finding{File: rel, Rule: rule, Excerpt: excerpt})
	}
	for _, c := range []struct {
		rule string
		re   *regexp.Regexp
	}{
		{"credential header", credentialHeaderRe},
		{"bearer or basic credential", bearerRe},
		{"loopback host:port", loopbackRe},
		{"temp path", tempRe},
		{"process id", pidRe},
		{"github token", githubTokenRe},
		{"sandbox identity", sandboxIdentityRe},
		{"avatar or gravatar URL", avatarRe},
	} {
		if m := c.re.FindString(text); m != "" {
			add(c.rule, m)
		}
	}
	for _, c := range []struct {
		rule string
		re   *regexp.Regexp
		ok   func(string) bool
	}{
		{"node id", nodeIDRe, func(v string) bool { return v == NodeIDPlaceholder }},
		{"user login", loginRe, func(v string) bool { return v == SandboxLogin || v == SandboxUser || v == SandboxOwner }},
		{"email address", emailRe, placeholderDomain},
	} {
		for _, m := range c.re.FindAllStringSubmatch(text, -1) {
			if !c.ok(m[1]) {
				add(c.rule, m[0])
				break
			}
		}
	}
	for _, f := range twilioFindings(text) {
		add(f[0], f[1])
	}
	for _, f := range slackFindings(text) {
		add(f[0], f[1])
	}
	for _, f := range telegramFindings(text) {
		add(f[0], f[1])
	}
	for _, f := range discordFindings(text) {
		add(f[0], f[1])
	}
	for _, f := range teamsFindings(text) {
		add(f[0], f[1])
	}
	low := strings.ToLower(text)
	for _, lit := range deny {
		if lit != "" && strings.Contains(low, strings.ToLower(lit)) {
			add("recorded identity", lit)
		}
	}
	if !strings.HasSuffix(rel, ".yaml") {
		return out
	}
	if strings.Contains(rel, ".req.") {
		if m := timeRe.FindString(recordedAtRe.ReplaceAllString(text, "")); m != "" {
			add("timestamp in a request", m)
		}
	}
	var doc any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		add("not yaml", err.Error())
		return out
	}
	walkScalars(doc, "", func(key, val string) {
		if strings.HasPrefix(val, "<redacted:") {
			return
		}
		if _, hit := red.RedactField(key, val); hit || secretNamed(key) {
			add("unredacted secret field", key)
		}
	})
	return out
}

// placeholderDomain reports whether an email domain names nobody: the
// reserved example domains and the .invalid TLD.
func placeholderDomain(d string) bool {
	d = strings.ToLower(d)
	return strings.HasSuffix(d, ".invalid") || d == "example.com" || d == "example.org" || d == "example.net"
}

// walkScalars calls f for every string scalar of doc with the key it
// sits under (a list's elements take the list's key).
func walkScalars(v any, key string, f func(key, val string)) {
	switch t := v.(type) {
	case map[string]any:
		for k, c := range t {
			if k == "recorded_at" {
				continue
			}
			walkScalars(c, k, f)
		}
	case []any:
		for _, c := range t {
			walkScalars(c, key, f)
		}
	case string:
		f(key, t)
	}
}

// secretNamed is a stricter reading of a field name than xrr's, which
// matches whole words: PGPASSWORD is a password, and xrr records it.
func secretNamed(key string) bool {
	k := strings.ToUpper(key)
	for _, w := range []string{"PASSWORD", "PASSWD", "SECRET", "TOKEN", "APIKEY", "API_KEY", "CREDENTIAL"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}
