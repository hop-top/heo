package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/consent"
	"hop.top/heo/internal/consent/consenttest"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// authSite is one profile's `channel authorize`, with the daemon, the
// browser and the provider replaced: a fake authorization server, a
// credential backend on the file store, and a channel the daemon would
// answer with.
type authSite struct {
	t      *testing.T
	a      *app
	idp    *consenttest.IDP
	b      *credentials.Backend
	ch     channelView
	format string
	opened atomic.Int32
	out    bytes.Buffer
	errOut bytes.Buffer
}

func newAuthSite(t *testing.T) *authSite {
	t.Helper()
	ref := profile.Default()
	b, err := credentials.Open(t.Context(), credentials.Options{
		Profile: ref, Tenant: model.TenantID(ref.Dir()),
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"},
		OAuth:  credentials.OAuthOptions{LockDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &authSite{t: t, a: newApp(), idp: consenttest.New(t), b: b}
	s.ch = channelView{Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: model.TenantID(ref.Dir()), ID: "ch_1"}, Key: "gmail", Type: google.Type, CredentialRef: "gmail",
		Configuration: model.Extension{Name: google.ConfigSchema, Version: google.ConfigVersion, Data: json.RawMessage(`{"user":"me","address":"ops@example.com"}`)},
		Revision:      "rev_1",
	}}
	return s
}

func (s *authSite) deps() authorizeDeps {
	return authorizeDeps{
		channel:  func(context.Context, string) (channelView, error) { return s.ch, nil },
		purposes: func(model.TypeKey) []string { return []string{"google.mail.api"} },
		backend:  func(context.Context) (*credentials.Backend, error) { return s.b, nil },
		run:      consent.Run,
		open: func(url string) error {
			s.opened.Add(1)
			consenttest.Browse(url)
			return nil
		},
		http:     &http.Client{Timeout: 5 * time.Second},
		provider: s.idp.Provider,
		stderr:   &s.errOut,
	}
}

// cmd is a fresh `channel authorize` leaf with args parsed, as the
// terminal gives them: a command per run, so no flag value carries over.
// A --dry-run among them is honoured, and --format follows the site.
func (s *authSite) cmd(args ...string) (*cobra.Command, authorizeInput, error) {
	s.t.Helper()
	c := s.a.channelAuthorizeCmd()
	c.Flags().Bool("dry-run", false, "")
	c.PersistentFlags().String("format", s.format, "")
	if err := c.ParseFlags(args); err != nil {
		return nil, authorizeInput{}, output.UsageError(err.Error())
	}
	in := authorizeInputOf(c)
	in.Channel = "gmail"
	c.SetContext(s.t.Context())
	s.out.Reset()
	s.errOut.Reset()
	c.SetOut(&s.out)
	c.SetErr(&s.errOut)
	return c, in, nil
}

// run runs `channel authorize` with args.
func (s *authSite) run(deps authorizeDeps, args ...string) error {
	s.t.Helper()
	c, in, err := s.cmd(args...)
	if err != nil {
		return err
	}
	return s.a.authorize(c, in, deps)
}

// asJSON makes the site print what a script reads.
func (s *authSite) asJSON() {
	s.format = output.JSON
	s.a.r.Viper.Set("format", output.JSON)
}

// authorizeInputOf reads the flags of a parsed leaf.
func authorizeInputOf(c *cobra.Command) authorizeInput {
	var in authorizeInput
	f := c.Flags()
	in.ClientID, _ = f.GetString(flagClientID)
	in.ClientSecretRef, _ = f.GetString(flagClientSecretRef)
	in.Scopes, _ = f.GetStringSlice(flagScope)
	in.Directory, _ = f.GetString(flagDirectory)
	in.LoginHint, _ = f.GetString(flagLoginHint)
	in.RefreshRef, _ = f.GetString(flagRefreshRef)
	in.NoBrowser, _ = f.GetBool(flagNoBrowser)
	in.Timeout, _ = f.GetDuration(flagTimeout)
	in.Replace, _ = f.GetBool(flagReplace)
	return in
}

func (s *authSite) store(ref string, raw []byte) {
	s.t.Helper()
	key, err := s.b.Key(ref)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.b.SurfaceTokens().Set(s.t.Context(), key, raw); err != nil {
		s.t.Fatal(err)
	}
}

func secretEnvelope(v string) []byte {
	b, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
	return b
}

func (s *authSite) accessToken() ports.OAuth2 {
	s.t.Helper()
	c, err := s.b.Resolve(s.t.Context(), credentials.Request{Reference: "gmail", Purpose: "google.mail.api", Accept: []ports.CredentialKind{ports.KindOAuth2}})
	if err != nil {
		s.t.Fatalf("resolve after authorize: %v", err)
	}
	return c.(ports.OAuth2)
}

// apsAuthBackend is the credential backend of an aps-bound profile, whose
// secrets heo only reads: it cannot keep a refresh token.
func apsAuthBackend(t *testing.T) *credentials.Backend {
	t.Helper()
	data := t.TempDir()
	t.Setenv("APS_DATA_PATH", data)
	dir := filepath.Join(data, "profiles", "noor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"profile.yaml": "id: noor\n", "secrets.env": "UNRELATED='x'\n"} { //nolint:gosec // fixture file contents, not credentials
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ref := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	b, err := credentials.Open(t.Context(), credentials.Options{Profile: ref, Tenant: model.TenantID(ref.Dir()),
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func noToken(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, tok := range []string{consenttest.RefreshToken, consenttest.AccessToken, "client-secret-value", "code-1"} {
			if strings.Contains(s, tok) {
				t.Errorf("%s carries %q:\n%s", what, tok, s)
			}
		}
	}
}

// TestChannelAuthorizeKeepsTheGrantAndTheBackendMintsFromIt: a Gmail
// channel's first consent opens the consent URL, signs the user in against
// the provider, stores the grant under the channel's credential reference
// and the refresh token beside it, and the credential backend then mints an
// access token from them with the refresh token the provider issued. The
// output names the keys and the scopes and carries no token.
func TestChannelAuthorizeKeepsTheGrantAndTheBackendMintsFromIt(t *testing.T) {
	s := newAuthSite(t)
	s.store("gmail-client", secretEnvelope("client-secret-value"))
	s.asJSON()
	if err := s.run(s.deps(), "--client-id", "1234.apps.example.com", "--client-secret-ref", "gmail-client"); err != nil {
		t.Fatal(err)
	}
	var out authorizeOut
	if err := json.Unmarshal(s.out.Bytes(), &out); err != nil {
		t.Fatalf("%v\n%s", err, s.out.String())
	}
	if out.Channel != "gmail" || out.Type != "google.mail" || out.Credential != "gmail" || out.Provider != "google" ||
		out.ClientID != "1234.apps.example.com" || out.Grant != "refresh_token" || out.Backend != "file" ||
		out.GrantKey != "uma/default/gmail" || out.RefreshKey != "uma/default/gmail.refresh_token" ||
		strings.Join(out.Scopes, " ") != strings.Join(google.RequiredPermissions, " ") || out.AccessExpires.Before(time.Now().Add(30*time.Minute)) {
		t.Fatalf("out = %+v", out)
	}
	if s.opened.Load() != 1 || !strings.Contains(s.errOut.String(), s.idp.URL()+"/authorize?") {
		t.Errorf("browser opened %d times; prompt:\n%s", s.opened.Load(), s.errOut.String())
	}
	noToken(t, "the output", s.out.String(), s.errOut.String())

	e, err := s.b.Entry(t.Context(), "gmail")
	if err != nil || e.Grant == nil || e.Grant.Kind != oauth.RefreshToken || e.Grant.TokenURL != s.idp.URL()+"/token" ||
		e.Grant.ClientSecretRef != "uma/default/gmail-client" || e.Grant.SecretRef != "uma/default/gmail.refresh_token" {
		t.Fatalf("stored entry = %+v, %v", e, err)
	}
	if rt, err := s.b.SecretAt(t.Context(), e.Grant.SecretRef); err != nil || string(rt) != consenttest.RefreshToken {
		t.Errorf("refresh token stored = %q, %v", rt, err)
	}
	if tok := s.accessToken(); tok.AccessToken == "" {
		t.Error("no access token minted from the stored grant")
	}
	ex := s.idp.Exchanges()
	if len(ex) != 2 || ex[0].Get("client_secret") != "client-secret-value" || ex[1].Get("grant_type") != "refresh_token" ||
		ex[1].Get("refresh_token") != consenttest.RefreshToken {
		t.Errorf("token requests = %v", ex)
	}
	a := s.idp.Authorizations()[0]
	if a.Get("login_hint") != "ops@example.com" || a.Get("access_type") != "offline" || a.Get("client_id") != "1234.apps.example.com" {
		t.Errorf("authorization request = %v", a)
	}
}

// TestChannelAuthorizeTextOutput: the text form lists where things were
// kept, never a token, and says what to run next.
func TestChannelAuthorizeTextOutput(t *testing.T) {
	s := newAuthSite(t)
	if err := s.run(s.deps(), "--client-id", "c1"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"credential ref", "gmail", "grant key", "uma/default/gmail", "refresh token key", "uma/default/gmail.refresh_token",
		"refresh_token in the file backend", "access token expires", "heo channel check gmail --remote"} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("text output lacks %q:\n%s", want, s.out.String())
		}
	}
	noToken(t, "the output", s.out.String(), s.errOut.String())
}

// TestChannelAuthorizeNoBrowser: --no-browser prints the URL and opens
// nothing; a sign-in nobody completes ends at --timeout, writes nothing.
func TestChannelAuthorizeNoBrowser(t *testing.T) {
	s := newAuthSite(t)
	err := s.run(s.deps(), "--client-id", "c1", "--no-browser", "--timeout", "400ms")
	if !model.IsCode(err, model.Unavailable) || !strings.Contains(err.Error(), "not completed in time") {
		t.Fatalf("err = %v", err)
	}
	if s.opened.Load() != 0 || !strings.Contains(s.errOut.String(), "/authorize?") {
		t.Errorf("opened %d times; prompt:\n%s", s.opened.Load(), s.errOut.String())
	}
	if e, _ := s.b.Entry(t.Context(), "gmail"); e.Exists {
		t.Errorf("wrote %+v", e)
	}
}

// TestChannelAuthorizeDryRunWritesAndSendsNothing: the preview names the
// scopes, the client, the keys it would write and the consent URL with its
// per-run values left open; it opens no browser, reaches the provider
// never and leaves the store as it was.
func TestChannelAuthorizeDryRunWritesAndSendsNothing(t *testing.T) {
	s := newAuthSite(t)
	s.store("gmail-client", secretEnvelope("client-secret-value"))
	s.asJSON()
	if err := s.run(s.deps(), "--client-id", "c1", "--client-secret-ref", "gmail-client", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Args    map[string]json.RawMessage
		Effects []struct{ Kind, Target, Detail string }
	}
	if err := json.Unmarshal(s.out.Bytes(), &plan); err != nil {
		t.Fatalf("%v\n%s", err, s.out.String())
	}
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "credential:gmail" {
		t.Fatalf("effects = %+v", plan.Effects)
	}
	for _, want := range []string{"refresh_token grant", "uma/default/gmail", "uma/default/gmail.refresh_token", "file backend"} {
		if !strings.Contains(plan.Effects[0].Detail, want) {
			t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
		}
	}
	var scopes []string
	var url string
	_ = json.Unmarshal(plan.Args["scopes"], &scopes)
	_ = json.Unmarshal(plan.Args["consent_url"], &url)
	if strings.Join(scopes, " ") != strings.Join(google.RequiredPermissions, " ") ||
		!strings.Contains(url, "state=%3Cstate%3E") || !strings.Contains(url, "code_challenge=%3Cchallenge%3E") ||
		string(plan.Args["client_id"]) != `"c1"` || string(plan.Args["confidential_client"]) != "true" {
		t.Errorf("plan args = %v", plan.Args)
	}
	if n := len(s.idp.Authorizations()) + len(s.idp.Exchanges()); n != 0 || s.opened.Load() != 0 {
		t.Errorf("a preview reached the provider %d times and opened the browser %d times", n, s.opened.Load())
	}
	if e, _ := s.b.Entry(t.Context(), "gmail"); e.Exists {
		t.Errorf("a preview wrote %+v", e)
	}
	noToken(t, "the plan", s.out.String())
}

// TestChannelAuthorizeMicrosoft: a Graph mailbox signs in to the directory
// named, with the delegated scopes and offline_access, as a public client.
func TestChannelAuthorizeMicrosoft(t *testing.T) {
	s := newAuthSite(t)
	s.ch.Type, s.ch.CredentialRef, s.ch.Key = microsoft.Type, "outlook", "outlook"
	s.ch.Configuration = model.Extension{Name: microsoft.ConfigSchema, Version: microsoft.ConfigVersion, Data: json.RawMessage(`{"mailbox":"me@contoso.com"}`)}
	deps := s.deps()
	var asked consent.Provider
	deps.provider = func(p consent.Provider) consent.Provider { asked = p; return s.idp.Provider(p) }
	if err := s.run(deps, "--client-id", "app-1", "--directory", "contoso.onmicrosoft.com"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked.AuthURL, "/contoso.onmicrosoft.com/oauth2/v2.0/authorize") || asked.Params["login_hint"] != "me@contoso.com" ||
		strings.Join(asked.Scopes, " ") != "Mail.ReadWrite Mail.Send offline_access" {
		t.Errorf("provider asked = %+v", asked)
	}
	a, ex := s.idp.Authorizations()[0], s.idp.Exchanges()[0]
	if a.Get("client_id") != "app-1" || a.Get("login_hint") != "me@contoso.com" || a.Get("access_type") != "" || ex.Get("client_secret") != "" {
		t.Errorf("authorization %v, exchange %v", a, ex)
	}
	e, _ := s.b.Entry(t.Context(), "outlook")
	if e.Grant == nil || e.Grant.ClientSecretRef != "" || e.Grant.SecretRef != "uma/default/outlook.refresh_token" {
		t.Errorf("stored grant = %+v", e.Grant)
	}
	// A second consent reads the directory back from the stored token URL.
	deps.provider = func(p consent.Provider) consent.Provider { asked = p; return s.idp.Provider(p) }
	if err := s.run(deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked.AuthURL, "/oauth2/v2.0/authorize") {
		t.Errorf("provider asked = %+v", asked)
	}
}

// TestChannelAuthorizeReusesTheStoredGrant: a second consent, after the
// first one expired or was revoked, needs no flag: the client, its secret,
// the scopes and the directory come from the grant already stored, and the
// refresh token is replaced where it was.
func TestChannelAuthorizeReusesTheStoredGrant(t *testing.T) {
	s := newAuthSite(t)
	s.store("gmail-client", secretEnvelope("client-secret-value"))
	if err := s.run(s.deps(), "--client-id", "c1", "--client-secret-ref", "gmail-client", "--scope", google.ScopeModify); err != nil {
		t.Fatal(err)
	}
	if err := s.run(s.deps()); err != nil {
		t.Fatalf("second consent with no flags: %v", err)
	}
	e, _ := s.b.Entry(t.Context(), "gmail")
	if e.Grant == nil || e.Grant.ClientID != "c1" || strings.Join(e.Grant.Scopes, " ") != google.ScopeModify ||
		e.Grant.ClientSecretRef != "uma/default/gmail-client" {
		t.Fatalf("grant after the second consent = %+v", e.Grant)
	}
	if got := s.idp.Authorizations(); len(got) != 2 || got[1].Get("client_id") != "c1" || got[1].Get("scope") != google.ScopeModify {
		t.Errorf("second authorization request = %v", got)
	}
	if ex := s.idp.Exchanges(); len(ex) != 2 || ex[1].Get("client_secret") != "client-secret-value" {
		t.Errorf("second exchange = %v", ex)
	}
	if !strings.Contains(s.out.String(), "refresh_token in the file backend") {
		t.Errorf("output:\n%s", s.out.String())
	}
	// A different client is not given the stored client's secret.
	if err := s.run(s.deps(), "--client-id", "other-client"); err != nil {
		t.Fatal(err)
	}
	if ex := s.idp.Exchanges(); ex[len(ex)-1].Get("client_secret") != "" {
		t.Errorf("another client was sent the stored client's secret: %v", ex[len(ex)-1])
	}
}

// TestChannelAuthorizeRefusals: each refusal comes before the browser is
// opened or anything is written, so a person never signs in to be told no.
func TestChannelAuthorizeRefusals(t *testing.T) {
	type refusal struct {
		setup    func(*authSite, *authorizeDeps)
		args     []string
		code     model.ErrorCode
		contains string
		usage    bool
	}
	for name, tc := range map[string]refusal{
		"no client id": {args: nil, contains: "--client-id", usage: true},
		"no credential ref": {setup: func(s *authSite, _ *authorizeDeps) { s.ch.CredentialRef = "" },
			args: []string{"--client-id", "c"}, contains: "no credential reference"},
		"a channel with no consent": {setup: func(s *authSite, _ *authorizeDeps) { s.ch.Type = model.TypeSmtpMail },
			args: []string{"--client-id", "c"}, code: model.Unsupported, contains: "google.mail and microsoft.mail"},
		"national cloud": {setup: func(s *authSite, _ *authorizeDeps) {
			s.ch.Type = microsoft.Type
			s.ch.Configuration.Data = json.RawMessage(`{"cloud":"usgov"}`)
		}, args: []string{"--client-id", "c"}, code: model.Unsupported, contains: "global Microsoft cloud only"},
		"holds a password": {setup: func(s *authSite, _ *authorizeDeps) {
			s.store("gmail", []byte(`{"v":1,"kind":"password","user":"u","password":"pw-seed"}`))
		}, args: []string{"--client-id", "c"}, code: model.Conflict, contains: "already holds a password"},
		"holds another grant": {setup: func(s *authSite, _ *authorizeDeps) {
			g, _ := oauth.EncodeGrant(oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: s.idp.URL() + "/token", ClientID: "c", SecretRef: "uma/default/cs"}) //nolint:gosec // fixture file contents, not credentials
			s.store("gmail", g)
		}, args: []string{"--client-id", "c"}, code: model.Conflict, contains: "already holds a client_credentials"},
		"missing client secret": {args: []string{"--client-id", "c", "--client-secret-ref", "nowhere"}, code: model.NotFound, contains: "client secret"},
		"bad directory": {setup: func(s *authSite, _ *authorizeDeps) { s.ch.Type = microsoft.Type },
			args: []string{"--client-id", "c", "--directory", "a/b"}, code: model.Validation, contains: "directory"},
		"read-only backend": {setup: func(s *authSite, d *authorizeDeps) {
			d.backend = func(context.Context) (*credentials.Backend, error) { return apsAuthBackend(t), nil }
		}, args: []string{"--client-id", "c"}, code: model.Unsupported, contains: "writable credential backend"},
		"no daemon": {setup: func(s *authSite, d *authorizeDeps) {
			d.channel = func(context.Context, string) (channelView, error) {
				return channelView{}, errors.New("no daemon answers")
			}
		}, args: []string{"--client-id", "c"}, contains: "no daemon answers"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newAuthSite(t)
			deps := s.deps()
			if tc.setup != nil {
				tc.setup(s, &deps)
			}
			before, _ := s.b.SurfaceTokens().List(t.Context(), "")
			err := s.run(deps, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err = %v, want one containing %q", err, tc.contains)
			}
			var oe *output.Error
			switch {
			case tc.usage && (!errors.As(err, &oe) || oe.ExitCode != output.ExitUsage):
				t.Errorf("err = %v, want a usage error", err)
			case tc.code != "" && !model.IsCode(err, tc.code):
				t.Errorf("err = %v, want %s", err, tc.code)
			}
			if after, _ := s.b.SurfaceTokens().List(t.Context(), ""); len(after) != len(before) {
				t.Errorf("a refused consent wrote: %v -> %v", before, after)
			}
			if s.opened.Load() != 0 || len(s.idp.Authorizations()) != 0 {
				t.Errorf("a refused consent reached the provider: %d authorizations, browser opened %d times", len(s.idp.Authorizations()), s.opened.Load())
			}
			noToken(t, "the refusal", err.Error(), s.errOut.String())
			if strings.Contains(err.Error(), "pw-seed") {
				t.Errorf("the refusal carries the stored password: %v", err)
			}
		})
	}
}

// TestChannelAuthorizeReplacesACredentialOnRequest: --replace says the
// stored password may be lost, and the output says it was.
func TestChannelAuthorizeReplacesACredentialOnRequest(t *testing.T) {
	s := newAuthSite(t)
	s.store("gmail", []byte(`{"v":1,"kind":"password","user":"u","password":"pw-seed"}`))
	if err := s.run(s.deps(), "--client-id", "c", "--replace"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.out.String(), "a password entry") {
		t.Errorf("output:\n%s", s.out.String())
	}
	if e, _ := s.b.Entry(t.Context(), "gmail"); e.Grant == nil {
		t.Errorf("entry = %+v", e)
	}
}

// TestChannelAuthorizeWhenTheUserSaysNoOrTheProviderIssuesNoToken: the
// provider's refusals are fixed answers; nothing is stored, and the
// existing credential, when there is one, is untouched.
func TestChannelAuthorizeWhenTheUserSaysNoOrTheProviderIssuesNoToken(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(*consenttest.IDP)
		code model.ErrorCode
		want string
	}{
		"declined":   {func(i *consenttest.IDP) { i.Deny() }, model.Unauthorized, "declined"},
		"no refresh": {func(i *consenttest.IDP) { i.NoRefreshToken() }, model.Unauthorized, "no refresh token"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newAuthSite(t)
			tc.set(s.idp)
			s.store("gmail", []byte(`{"v":1,"kind":"password","user":"u","password":"pw-seed"}`))
			err := s.run(s.deps(), "--client-id", "c", "--replace")
			if !model.IsCode(err, tc.code) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s containing %q", err, tc.code, tc.want)
			}
			if e, _ := s.b.Entry(t.Context(), "gmail"); e.Kind != "password" || e.Grant != nil {
				t.Errorf("the stored credential changed: %+v", e)
			}
			if ok, _ := s.b.SurfaceTokens().Exists(t.Context(), "uma/default/gmail.refresh_token"); ok {
				t.Error("a refresh token was stored")
			}
			noToken(t, "the failure", err.Error(), s.errOut.String())
		})
	}
}

// TestChannelAuthorizeIsTerminalOnly: the verb is interactive, so no
// served surface runs it and a plugin may not; it is a write that is not
// idempotent, and carries its examples and next steps.
func TestChannelAuthorizeIsTerminalOnly(t *testing.T) {
	a := newApp()
	c := find(a.r.Cmd, "channel authorize")
	if c == nil {
		t.Fatal("channel authorize is not in the tree")
	}
	if c.Annotations[cmdmeta.KeySelfHosting] != "true" {
		t.Error("channel authorize is not marked self-hosting: a served surface would run an interactive login")
	}
	if control.IsDaemon(c) {
		t.Error("channel authorize runs in the daemon: its browser and its listener are the terminal's")
	}
	for _, flag := range []string{flagClientID, flagClientSecretRef, flagScope, flagDirectory, flagLoginHint, flagRefreshRef, flagNoBrowser, flagTimeout, flagReplace} {
		if c.Flags().Lookup(flag) == nil {
			t.Errorf("no --%s", flag)
		}
	}
	for _, secret := range []string{"client-secret", "token", "password", "secret"} {
		if c.Flags().Lookup(secret) != nil {
			t.Errorf("channel authorize takes a --%s: credentials are referenced, never passed", secret)
		}
	}
}
