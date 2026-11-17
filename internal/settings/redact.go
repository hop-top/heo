package settings

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"hop.top/kit/go/core/redact"
)

// Redacted replaces a value that may be credential material.
const Redacted = "***REDACTED***"

// sensitiveWords mark a key whose value is a credential by name. A
// segment ending in one of refSuffixes names where a credential lives,
// not the credential, and stays visible.
var (
	sensitiveWords = map[string]bool{
		"token": true, "secret": true, "password": true, "passwd": true,
		"passphrase": true, "apikey": true, "privatekey": true,
	}
	sensitivePairs = []string{"api_key", "private_key", "secret_key", "access_key", "client_secret"}
	refSuffixes    = []string{"_ref", "_file", "_path", "_env", "_name"}
)

// isSensitiveKey reports whether key names credential material.
func isSensitiveKey(key string) bool {
	for _, seg := range strings.Split(strings.ToLower(key), ".") {
		if hasAnySuffix(seg, refSuffixes) {
			continue
		}
		for _, pair := range sensitivePairs {
			if strings.Contains(seg, pair) {
				return true
			}
		}
		for _, w := range strings.FieldsFunc(seg, func(r rune) bool { return r == '_' || r == '-' }) {
			if sensitiveWords[w] {
				return true
			}
		}
	}
	return false
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// secrets is kit's secret-pattern redactor (the vendored gitleaks
// corpus). kit's PII pack is left out on purpose: it treats every IP
// address as personal data and would hide listener addresses, which a
// config listing exists to show.
var secrets = sync.OnceValue(func() *redact.Redactor {
	rules, err := redact.LoadGitleaks(redact.DefaultGitleaksPath())
	if err != nil {
		panic(fmt.Sprintf("settings: load secret rules: %v", err))
	}
	r := redact.New().AddRules(rules...)
	if _, err := r.SetReplacement(redact.Mask); err != nil {
		panic(fmt.Sprintf("settings: redaction strategy: %v", err))
	}
	return r
})

// LooksLikeSecret reports whether s matches a known credential pattern.
// It asks for the regions redaction would replace, not for scanner
// findings: a distinctive pattern counts at any entropy and an allowlist
// never exempts a match, so it fails closed.
func LooksLikeSecret(s string) bool { return len(secrets().Spans(s)) > 0 }

// Display renders v for output, hiding credential material: the
// whole value when the key names a credential, any embedded match of a
// known credential pattern otherwise.
func Display(key string, v any) string {
	s := render(v)
	if s == "" {
		return s
	}
	if isSensitiveKey(key) {
		return Redacted
	}
	if LooksLikeSecret(s) || hasURLPassword(s) {
		return Redacted
	}
	return s
}

// hasURLPassword reports whether s is a URL carrying a password in its
// userinfo (scheme://user:password@host).
func hasURLPassword(s string) bool {
	if !strings.Contains(s, "://") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return false
	}
	_, ok := u.User.Password()
	return ok
}

// render prints a scalar as is and a list or map as JSON.
func render(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any, map[string]any, []string:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	default:
		return fmt.Sprint(t)
	}
}
