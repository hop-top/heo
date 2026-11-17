// Package consent runs the interactive first consent of a user-consented
// mailbox: Gmail on a consumer account, a Microsoft Graph mailbox reached
// with delegated permissions. Both need a person to sign in once; the
// provider answers with a refresh token, and every access token after
// that is minted from it by the credential backend (see
// internal/credentials).
//
// The protocol is kit's: an authorization code flow with PKCE (S256)
// whose redirect lands on a 127.0.0.1 listener on an ephemeral port
// (cmdsurface.LoopbackPKCE). This package writes no PKCE client. It
// knows, per channel type, where the provider's endpoints are, which
// scopes the adapter needs and what the authorization request must ask
// for to get a refresh token, and it turns kit's result into the grant
// the credential backend keeps.
//
// Nothing here reads or writes a store, and no secret reaches an error
// or a plan: the refresh token leaves in Outcome only.
package consent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

// Microsoft Entra directories a delegated sign-in may name besides a
// tenant ID or a domain.
const (
	DirectoryCommon        = "common"
	DirectoryOrganizations = "organizations"
	DirectoryConsumers     = "consumers"
)

// Endpoints of the providers' global clouds.
const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token" //nolint:gosec // public token endpoint URL, not a credential
	msLogin        = "https://login.microsoftonline.com"
)

// DefaultTimeout bounds a sign-in when the caller sets none: kit's.
const DefaultTimeout = cmdsurface.DefaultLoopbackTimeout

// DirectoryOf is the Entra directory a Microsoft token URL signs in to,
// empty for a URL that is not one of Microsoft's: what a second consent
// reads back from the grant the first one stored.
func DirectoryOf(tokenURL string) string {
	rest, ok := strings.CutPrefix(tokenURL, msLogin+"/")
	if !ok {
		return ""
	}
	dir, tail, ok := strings.Cut(rest, "/")
	if !ok || tail != "oauth2/v2.0/token" {
		return ""
	}
	if d, err := url.PathUnescape(dir); err == nil {
		return d
	}
	return ""
}

// offlineAccess is the Microsoft scope that makes the token endpoint
// issue a refresh token.
const offlineAccess = "offline_access"

// Provider is the consent flow of one channel type: where to sign in, what
// to ask for and what the authorization request must carry for the
// provider to issue a refresh token.
type Provider struct {
	// Type is the channel's adapter type.
	Type model.TypeKey
	// Name is the provider's name in logs and plans: google or microsoft.
	Name string
	// AuthURL and TokenURL are the provider's authorization and token
	// endpoints.
	AuthURL, TokenURL string
	// Scopes are what the consent asks for.
	Scopes []string
	// Params are the extra authorization request parameters: the
	// offline access Google needs, the mailbox the user signs in as.
	Params map[string]string
}

// Options tune ProviderFor.
type Options struct {
	// Directory is the Microsoft Entra directory (tenant) to sign in to;
	// DirectoryCommon when empty. It means nothing to Gmail.
	Directory string
	// Scopes replace the adapter's own when set.
	Scopes []string
	// LoginHint is the mailbox the sign-in is for, when the channel's
	// configuration does not name one.
	LoginHint string
}

// Types are the channel types that have an interactive consent.
func Types() []model.TypeKey { return []model.TypeKey{google.Type, microsoft.Type} }

// ProviderFor is the consent flow of a channel of type typ whose adapter
// configuration is configuration. It is model.Unsupported for a type with
// none, and for a Microsoft national cloud, whose Graph scopes and
// sign-in endpoints differ and are not mapped here.
func ProviderFor(typ model.TypeKey, configuration json.RawMessage, o Options) (Provider, error) {
	scopes := slices.Clone(o.Scopes)
	switch typ {
	case google.Type:
		var c struct {
			User    string `json:"user"`
			Address string `json:"address"`
		}
		_ = json.Unmarshal(configuration, &c)
		if len(scopes) == 0 {
			scopes = slices.Clone(google.RequiredPermissions)
		}
		p := Provider{
			Type: typ, Name: "google", AuthURL: googleAuthURL, TokenURL: googleTokenURL, Scopes: scopes,
			// Google issues a refresh token only to a request for offline
			// access, and again only when the user is shown consent anew.
			Params: map[string]string{"access_type": "offline", "prompt": "consent"},
		}
		if hint := firstAddress(o.LoginHint, c.Address, c.User); hint != "" {
			p.Params["login_hint"] = hint
		}
		return p, nil
	case microsoft.Type:
		var c struct {
			Mailbox string `json:"mailbox"`
			Cloud   string `json:"cloud"`
		}
		_ = json.Unmarshal(configuration, &c)
		if c.Cloud != "" && c.Cloud != microsoft.CloudGlobal {
			return Provider{}, model.NewError(model.Unsupported, fmt.Sprintf(
				"cloud %q: heo runs the first consent for the global Microsoft cloud only; for a national cloud, store a refresh_token grant by hand (configuration.md, OAuth grants)", c.Cloud))
		}
		dir := o.Directory
		if dir == "" {
			dir = DirectoryCommon
		}
		if err := checkDirectory(dir); err != nil {
			return Provider{}, err
		}
		if len(scopes) == 0 {
			scopes = slices.Clone(microsoft.RequiredPermissions)
		}
		if !slices.Contains(scopes, offlineAccess) {
			scopes = append(scopes, offlineAccess)
		}
		p := Provider{
			Type: typ, Name: "microsoft", Scopes: scopes, Params: map[string]string{},
			AuthURL:  msLogin + "/" + url.PathEscape(dir) + "/oauth2/v2.0/authorize",
			TokenURL: msLogin + "/" + url.PathEscape(dir) + "/oauth2/v2.0/token",
		}
		if hint := firstAddress(o.LoginHint, c.Mailbox); hint != "" {
			p.Params["login_hint"] = hint
		}
		return p, nil
	}
	return Provider{}, model.NewError(model.Unsupported, fmt.Sprintf(
		"channel type %s has no interactive consent; heo runs it for %s", typ, joinTypes(Types())))
}

func joinTypes(ts []model.TypeKey) string {
	s := make([]string, len(ts))
	for i, t := range ts {
		s[i] = string(t)
	}
	return strings.Join(s, " and ")
}

// firstAddress is the first of candidates that is a mailbox address: a
// user principal name or a Gmail user ID that is not "me".
func firstAddress(candidates ...string) string {
	for _, c := range candidates {
		if strings.Contains(c, "@") && !strings.ContainsAny(c, " \t\r\n") {
			return c
		}
	}
	return ""
}

// checkDirectory refuses a directory that is not one of the three
// aliases, a tenant ID or a domain: it is a path segment of the sign-in
// URL, so it holds no separator or control character.
func checkDirectory(dir string) error {
	for _, r := range dir {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return model.NewError(model.Validation, fmt.Sprintf(
				"directory %q: want common, organizations, consumers, a tenant ID or a domain", dir))
		}
	}
	if dir == "" || dir[0] == '.' {
		return model.NewError(model.Validation, "directory: want common, organizations, consumers, a tenant ID or a domain")
	}
	return nil
}

// Request is one consent: whose client asks, and where the result is
// kept.
type Request struct {
	Provider Provider
	// ClientID is the OAuth client the user consents to.
	ClientID string
	// ClientSecret is the client's secret when it is a confidential one:
	// a Google "desktop" client has one. Empty for a public client.
	ClientSecret string
	// ClientSecretRef is the store key the grant names the client secret
	// by; required with ClientSecret, and only then.
	ClientSecretRef string
	// SecretRef is the store key the grant names the refresh token by.
	SecretRef string
	// Timeout bounds the whole sign-in; kit's default when zero.
	Timeout time.Duration
	// HTTPClient sends the token exchange; kit's default when nil. It
	// never follows a redirect.
	HTTPClient *http.Client
}

// Validate checks the request: a client, the keys a grant names and
// scopes the provider can be asked for.
func (r Request) Validate() error {
	switch {
	case r.ClientID == "":
		return model.NewError(model.Validation, "client ID: required")
	case r.SecretRef == "":
		return model.NewError(model.Validation, "refresh token key: required")
	case (r.ClientSecret != "") != (r.ClientSecretRef != ""):
		return model.NewError(model.Validation, "client secret and its store key come together")
	case len(r.Provider.Scopes) == 0:
		return model.NewError(model.Validation, "scopes: at least one")
	}
	return nil
}

func (r Request) config() oauth2.Config {
	return oauth2.Config{
		ClientID: r.ClientID, ClientSecret: r.ClientSecret, Scopes: slices.Clone(r.Provider.Scopes),
		Endpoint: oauth2.Endpoint{AuthURL: r.Provider.AuthURL, TokenURL: r.Provider.TokenURL},
	}
}

// authParams are the provider's extra request parameters, in a fixed
// order.
func (r Request) authParams() []oauth2.AuthCodeOption {
	keys := make([]string, 0, len(r.Provider.Params))
	for k := range r.Provider.Params {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]oauth2.AuthCodeOption, 0, len(keys))
	for _, k := range keys {
		out = append(out, oauth2.SetAuthURLParam(k, r.Provider.Params[k]))
	}
	return out
}

// Plan is what a consent would do, without running it: where it signs
// in, what it asks for and where the redirect lands.
type Plan struct {
	// ConsentURL is the authorization URL with the per-run values shown
	// as <port>, <state> and <challenge>: the real URL is minted when it
	// runs, and is only worth anything to the one login it was minted
	// for.
	ConsentURL string
	// RedirectURL is the loopback callback, its port chosen when it runs.
	RedirectURL string
}

// providerSlug is the callback route's name.
const providerSlug = "heo"

// Preview shows the consent URL and the redirect. It opens no listener
// and reaches no provider.
func Preview(r Request) (Plan, error) {
	if err := r.Validate(); err != nil {
		return Plan{}, err
	}
	cfg := r.config()
	cfg.RedirectURL = "http://127.0.0.1:<port>/oauth/" + providerSlug + "/callback"
	opts := append([]oauth2.AuthCodeOption{
		oauth2.SetAuthURLParam("code_challenge", "<challenge>"),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	}, r.authParams()...)
	return Plan{ConsentURL: cfg.AuthCodeURL("<state>", opts...), RedirectURL: cfg.RedirectURL}, nil
}

// Outcome is a completed consent: the grant to store, the refresh token
// envelope it names, and when the first access token expires. It holds a
// secret: Clear it once it is stored.
type Outcome struct {
	// Grant is the validated refresh_token grant.
	Grant oauth.Grant
	// RefreshSecret is the uma "secret" envelope holding the refresh
	// token, to store at Grant.SecretRef.
	RefreshSecret []byte
	// ExpiresAt is when the access token the exchange returned expires.
	ExpiresAt time.Time
}

// Clear zeroes the refresh token envelope.
func (o *Outcome) Clear() { clear(o.RefreshSecret) }

func (Outcome) String() string               { return "consent.Outcome{[redacted]}" }
func (o Outcome) GoString() string           { return o.String() }
func (Outcome) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (o Outcome) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(o.String())) }

// Run signs the user in. It listens on 127.0.0.1 only, hands ready the
// authorization URL once the listener is up (the CLI shows it and opens a
// browser), and returns when the redirect arrives and the code has been
// exchanged. A user who declines is model.Unauthorized; a login that
// outlasts the timeout, model.Unavailable; a provider that issues no
// refresh token, model.Unauthorized naming what to ask for.
func Run(ctx context.Context, r Request, ready func(authorizeURL string)) (Outcome, error) {
	if err := r.Validate(); err != nil {
		return Outcome{}, err
	}
	p := cmdsurface.PKCEProvider{
		Name: providerSlug, Config: r.config(), SecretRef: r.SecretRef, ClientSecretRef: r.ClientSecretRef,
		AuthParams: r.authParams(), HTTPClient: r.HTTPClient,
	}
	res, err := cmdsurface.LoopbackPKCE(ctx, p, cmdsurface.LoopbackOptions{Timeout: r.Timeout, OnReady: ready})
	if err != nil {
		return Outcome{}, fault(r.Provider, err)
	}
	return Outcome{Grant: res.Grant, RefreshSecret: res.RefreshSecret, ExpiresAt: res.Token.ExpiresAt}, nil
}

// fault maps kit's failure onto the uma error a caller sees. The text is
// fixed: neither kit's errors nor the provider's carry a code or a
// token, and none is added.
func fault(p Provider, err error) error {
	switch {
	case errors.Is(err, cmdsurface.ErrOAuthDenied):
		return model.NewError(model.Unauthorized, "the provider reported that consent was declined")
	case errors.Is(err, oauth.ErrNeedsReconsent):
		return model.NewError(model.Unauthorized, noRefreshToken(p))
	case errors.Is(err, context.DeadlineExceeded):
		return model.NewError(model.Unavailable, "the sign-in was not completed in time")
	case errors.Is(err, context.Canceled):
		return model.NewError(model.Transient, "the sign-in was interrupted")
	case errors.Is(err, oauth.ErrCredentialRejected):
		return model.NewError(model.Unauthorized, "the provider rejected the client: check the client ID and its secret")
	case errors.Is(err, oauth.ErrTransient):
		return model.NewError(model.Transient, "the provider's token endpoint could not be reached or failed; try again")
	}
	return model.NewError(model.Unavailable, "the sign-in failed: "+strings.TrimPrefix(err.Error(), "cmdsurface: "))
}

// noRefreshToken says why a provider issued no refresh token and what to
// do about it.
func noRefreshToken(p Provider) string {
	switch p.Name {
	case "google":
		return "Google issued no refresh token: revoke this app's access at https://myaccount.google.com/permissions and consent again"
	case "microsoft":
		return "Microsoft issued no refresh token: the consent must include the offline_access scope"
	}
	return "the provider issued no refresh token"
}
