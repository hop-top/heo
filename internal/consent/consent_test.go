package consent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/consent"
	"hop.top/heo/internal/consent/consenttest"
	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

const (
	refreshToken = consenttest.RefreshToken
	accessToken  = consenttest.AccessToken
)

type idp = consenttest.IDP

func newIDP(t *testing.T) *idp { return consenttest.New(t) }

func run(t *testing.T, i *idp, r consent.Request) (consent.Outcome, error) {
	t.Helper()
	r.Provider = i.Provider(r.Provider)
	if r.Timeout == 0 {
		r.Timeout = 10 * time.Second
	}
	return consent.Run(context.Background(), r, consenttest.Browse)
}

func gmail(t *testing.T, cfg string) consent.Provider {
	t.Helper()
	p, err := consent.ProviderFor(google.Type, json.RawMessage(cfg), consent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestProviderForGoogle: Gmail asks for offline access and a fresh
// consent, with the adapter's scopes; the mailbox address, when the
// configuration has one, is the login hint, and "me" is not an address.
func TestProviderForGoogle(t *testing.T) {
	p := gmail(t, `{"user":"me","address":"ops@example.com"}`)
	if p.Name != "google" || p.AuthURL != "https://accounts.google.com/o/oauth2/v2/auth" || p.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Fatalf("provider = %+v", p)
	}
	if strings.Join(p.Scopes, " ") != strings.Join(google.RequiredPermissions, " ") {
		t.Errorf("scopes = %v, want the adapter's %v", p.Scopes, google.RequiredPermissions)
	}
	if p.Params["access_type"] != "offline" || p.Params["prompt"] != "consent" || p.Params["login_hint"] != "ops@example.com" {
		t.Errorf("params = %v", p.Params)
	}
	if hint := gmail(t, `{"user":"me"}`).Params["login_hint"]; hint != "" {
		t.Errorf("login hint %q for the user \"me\"", hint)
	}
	if hint := gmail(t, `{"user":"boss@example.com"}`).Params["login_hint"]; hint != "boss@example.com" {
		t.Errorf("login hint %q from the user", hint)
	}
	q, err := consent.ProviderFor(google.Type, nil, consent.Options{Scopes: []string{google.ScopeModify}, LoginHint: "me@gmail.com"})
	if err != nil || strings.Join(q.Scopes, " ") != google.ScopeModify || q.Params["login_hint"] != "me@gmail.com" {
		t.Errorf("explicit scopes and hint = %+v, %v", q, err)
	}
}

// TestProviderForMicrosoft: Graph asks for its delegated permissions and
// offline_access in the named directory (common by default); the
// mailbox is the login hint; a national cloud and a directory that is not
// a path segment are refused.
func TestProviderForMicrosoft(t *testing.T) {
	p, err := consent.ProviderFor(microsoft.Type, json.RawMessage(`{"mailbox":"shared@example.com"}`), consent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "microsoft" || p.AuthURL != "https://login.microsoftonline.com/common/oauth2/v2.0/authorize" ||
		p.TokenURL != "https://login.microsoftonline.com/common/oauth2/v2.0/token" {
		t.Fatalf("provider = %+v", p)
	}
	if strings.Join(p.Scopes, " ") != "Mail.ReadWrite Mail.Send offline_access" {
		t.Errorf("scopes = %v", p.Scopes)
	}
	if p.Params["login_hint"] != "shared@example.com" || p.Params["access_type"] != "" {
		t.Errorf("params = %v", p.Params)
	}
	d, err := consent.ProviderFor(microsoft.Type, nil, consent.Options{Directory: "contoso.onmicrosoft.com", Scopes: []string{"Mail.ReadWrite.Shared"}})
	if err != nil || !strings.Contains(d.AuthURL, "/contoso.onmicrosoft.com/") || strings.Join(d.Scopes, " ") != "Mail.ReadWrite.Shared offline_access" {
		t.Errorf("directory and scopes = %+v, %v", d, err)
	}
	if _, err := consent.ProviderFor(microsoft.Type, json.RawMessage(`{"cloud":"usgov"}`), consent.Options{}); !model.IsCode(err, model.Unsupported) ||
		!strings.Contains(err.Error(), "global Microsoft cloud only") {
		t.Errorf("national cloud: %v", err)
	}
	for _, dir := range []string{"a/b", "..", ".x", "a b", "a?b", "a#b", "a\nb", "ü"} {
		if _, err := consent.ProviderFor(microsoft.Type, nil, consent.Options{Directory: dir}); !model.IsCode(err, model.Validation) {
			t.Errorf("directory %q: %v, want a validation error", dir, err)
		}
	}
	for dir, want := range map[string]string{
		"https://login.microsoftonline.com/organizations/oauth2/v2.0/token": "organizations",
		"https://login.microsoftonline.com/contoso.com/oauth2/v2.0/token":   "contoso.com",
		"https://oauth2.googleapis.com/token":                               "",
		"https://login.microsoftonline.com/x/oauth2/token":                  "",
	} {
		if got := consent.DirectoryOf(dir); got != want {
			t.Errorf("DirectoryOf(%q) = %q, want %q", dir, got, want)
		}
	}
}

// TestProviderForRefusesOtherTypes: only the two mail adapters that take a
// user's consent have a flow.
func TestProviderForRefusesOtherTypes(t *testing.T) {
	for _, typ := range []model.TypeKey{model.TypeSmtpMail, model.TypeImapMail, model.TypeGithubIssues, "nope"} {
		_, err := consent.ProviderFor(typ, nil, consent.Options{})
		if !model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), "google.mail and microsoft.mail") {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if got := consent.Types(); len(got) != 2 {
		t.Errorf("types = %v", got)
	}
}

// TestPreviewShowsTheConsentWithoutRunningIt: the URL carries the client,
// the scopes, the offline parameters and the PKCE method, with the values
// minted per run as placeholders; nothing is listened on or sent.
func TestPreviewShowsTheConsentWithoutRunningIt(t *testing.T) {
	i := newIDP(t)
	r := consent.Request{Provider: i.Provider(gmail(t, `{"address":"ops@example.com"}`)), ClientID: "client-1", SecretRef: "uma/default/gmail.refresh_token"} //nolint:gosec // fixture ids and secret refs, not credentials
	plan, err := consent.Preview(r)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(plan.ConsentURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if !strings.HasPrefix(plan.ConsentURL, i.URL()+"/authorize?") || q.Get("client_id") != "client-1" || q.Get("response_type") != "code" ||
		q.Get("scope") != strings.Join(google.RequiredPermissions, " ") || q.Get("code_challenge_method") != "S256" ||
		q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("login_hint") != "ops@example.com" ||
		q.Get("state") != "<state>" || q.Get("code_challenge") != "<challenge>" ||
		q.Get("redirect_uri") != "http://127.0.0.1:<port>/oauth/heo/callback" || plan.RedirectURL != q.Get("redirect_uri") {
		t.Errorf("consent URL = %s", plan.ConsentURL)
	}
	if n := len(i.Authorizations()) + len(i.Exchanges()); n != 0 {
		t.Errorf("a preview reached the provider %d times", n)
	}
	for name, bad := range map[string]consent.Request{
		"no client":      {Provider: r.Provider, SecretRef: "k"},
		"no refresh key": {Provider: r.Provider, ClientID: "c"},
		"secret no key":  {Provider: r.Provider, ClientID: "c", SecretRef: "k", ClientSecret: "s"},
		"key no secret":  {Provider: r.Provider, ClientID: "c", SecretRef: "k", ClientSecretRef: "uma/default/c"}, //nolint:gosec // fixture ids and secret refs, not credentials
		"no scopes":      {Provider: consent.Provider{AuthURL: "a", TokenURL: "t"}, ClientID: "c", SecretRef: "k"},
	} {
		if _, err := consent.Preview(bad); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
}

// TestRunSignsInAndReturnsTheGrant: the user's browser is sent to the
// provider with the PKCE challenge, the redirect lands on the loopback
// listener, the code is exchanged with the matching verifier (and the
// client's secret, for a confidential client), and the outcome is a
// refresh_token grant naming the keys it was given, plus the envelope
// that holds the refresh token.
func TestRunSignsInAndReturnsTheGrant(t *testing.T) {
	i := newIDP(t)
	p := gmail(t, `{"address":"ops@example.com"}`)
	out, err := run(t, i, consent.Request{ //nolint:gosec // fixture ids and secret refs, not credentials
		Provider: p, ClientID: "client-1", ClientSecret: "client-secret-value", ClientSecretRef: "uma/default/gmail-client",
		SecretRef: "uma/default/gmail.refresh_token",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Clear()
	g := out.Grant
	if g.Kind != oauth.RefreshToken || g.ClientID != "client-1" || g.TokenURL != i.URL()+"/token" ||
		g.SecretRef != "uma/default/gmail.refresh_token" || g.ClientSecretRef != "uma/default/gmail-client" ||
		strings.Join(g.Scopes, " ") != strings.Join(google.RequiredPermissions, " ") {
		t.Fatalf("grant = %+v", g)
	}
	var env struct {
		Kind, Value string
	}
	if err := json.Unmarshal(out.RefreshSecret, &env); err != nil || env.Kind != "secret" || env.Value != refreshToken {
		t.Fatalf("refresh envelope = %s, %v", out.RefreshSecret, err)
	}
	if out.ExpiresAt.Before(time.Now().Add(30 * time.Minute)) {
		t.Errorf("access token expires at %s", out.ExpiresAt)
	}
	if len(i.Authorizations()) != 1 || len(i.Exchanges()) != 1 {
		t.Fatalf("provider saw %d authorizations and %d exchanges", len(i.Authorizations()), len(i.Exchanges()))
	}
	a, f := i.Authorizations()[0], i.Exchanges()[0]
	if a.Get("client_id") != "client-1" || a.Get("access_type") != "offline" || a.Get("prompt") != "consent" ||
		a.Get("login_hint") != "ops@example.com" || !strings.HasPrefix(a.Get("redirect_uri"), "http://127.0.0.1:") {
		t.Errorf("authorization request = %v", a)
	}
	if f.Get("client_secret") != "client-secret-value" || f.Get("client_id") != "client-1" || f.Get("code_verifier") == "" ||
		f.Get("redirect_uri") != a.Get("redirect_uri") {
		t.Errorf("exchange = %v", f)
	}
	// Nothing prints the secret.
	for _, s := range []string{fmt.Sprintf("%v", out), fmt.Sprintf("%+v", out), fmt.Sprintf("%#v", out), fmt.Sprintf("%s", out)} {
		if strings.Contains(s, refreshToken) || strings.Contains(s, accessToken) {
			t.Errorf("outcome prints a token: %s", s)
		}
	}
	if b, _ := json.Marshal(out); strings.Contains(string(b), refreshToken) {
		t.Errorf("outcome marshals a token: %s", b)
	}
}

// TestRunPublicClient: a client with no secret sends none, and the grant
// names none.
func TestRunPublicClient(t *testing.T) {
	i := newIDP(t)
	ms, err := consent.ProviderFor(microsoft.Type, nil, consent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, i, consent.Request{Provider: ms, ClientID: "app-1", SecretRef: "uma/default/outlook.refresh_token"})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Clear()
	if out.Grant.ClientSecretRef != "" || i.Exchanges()[0].Get("client_secret") != "" ||
		strings.Join(out.Grant.Scopes, " ") != "Mail.ReadWrite Mail.Send offline_access" || i.Authorizations()[0].Get("access_type") != "" {
		t.Errorf("grant = %+v, exchange = %v, authorization = %v", out.Grant, i.Exchanges()[0], i.Authorizations()[0])
	}
}

// TestRunRefusals: a user who declines, a provider that issues no refresh
// token, a client the provider rejects and a login nobody finishes are each
// a fixed answer that names the way out and carries no code or token, and
// nothing is returned.
func TestRunRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		set      func(*idp)
		code     model.ErrorCode
		contains string
		timeout  time.Duration
	}{
		"declined":     {func(i *idp) { i.Deny() }, model.Unauthorized, "consent was declined", 0},
		"no refresh":   {func(i *idp) { i.NoRefreshToken() }, model.Unauthorized, "myaccount.google.com/permissions", 0},
		"bad client":   {func(i *idp) { i.RejectClient() }, model.Unauthorized, "check the client ID", 0},
		"never signed": {func(i *idp) {}, model.Unavailable, "not completed in time", 400 * time.Millisecond},
	} {
		i := newIDP(t)
		tc.set(i)
		r := consent.Request{Provider: i.Provider(gmail(t, `{}`)), ClientID: "c", SecretRef: "uma/default/g.refresh_token", Timeout: tc.timeout} //nolint:gosec // fixture ids and secret refs, not credentials
		var out consent.Outcome
		var err error
		if name == "never signed" {
			out, err = consent.Run(context.Background(), r, func(string) {})
		} else {
			out, err = run(t, i, r)
		}
		if !model.IsCode(err, tc.code) || !strings.Contains(err.Error(), tc.contains) {
			t.Errorf("%s: %v, want %s containing %q", name, err, tc.code, tc.contains)
		}
		if len(out.RefreshSecret) != 0 || out.Grant.Kind != "" {
			t.Errorf("%s: an outcome came with the failure: %+v", name, out.Grant)
		}
		if err != nil && (strings.Contains(err.Error(), "code-") || strings.Contains(err.Error(), refreshToken) || strings.Contains(err.Error(), accessToken)) {
			t.Errorf("%s: the error carries material: %v", name, err)
		}
	}
	// Microsoft names its own way out.
	i := newIDP(t)
	i.NoRefreshToken()
	ms, _ := consent.ProviderFor(microsoft.Type, nil, consent.Options{})
	_, err := run(t, i, consent.Request{Provider: ms, ClientID: "c", SecretRef: "uma/default/o.refresh_token"}) //nolint:gosec // fixture ids and secret refs, not credentials
	if !model.IsCode(err, model.Unauthorized) || !strings.Contains(err.Error(), "offline_access") {
		t.Errorf("microsoft with no refresh token: %v", err)
	}
}
