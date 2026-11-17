package composition

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/botauth"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// KeyTeamsAuthority is the Microsoft Entra endpoint microsoft.teams
// channels mint their Bot Connector token at, as an origin. The bot's
// client secret is sent there, so it is a host setting and never a field
// of a channel: it must be https, or http on a loopback host (a
// stand-in endpoint of one's own). Empty is the public cloud's
// https://login.microsoftonline.com.
const KeyTeamsAuthority = "adapters.teams.authority"

// The purposes of the credential a microsoft.teams channel's reference
// holds, of which the operator stores exactly one: the bot's client secret,
// a "secret" credential, or the certificate registered for its app with the
// private key that goes with it, a "client_cert" credential (PEM, an RSA
// key). heo mints the Bot Connector token with it, and serves the token to
// the adapter under its own purpose, teams.CredentialPurposeConnector.
const (
	PurposeTeamsClientSecret      = "microsoft.teams.client_secret"      // #nosec G101 -- purpose name, not a credential
	PurposeTeamsClientCertificate = "microsoft.teams.client_certificate" // #nosec G101 -- purpose name, not a credential
)

// CertificateWarnBefore is how long before its not_after a certificate that
// still works is reported by `channel check --remote`: long enough to
// register a new one with the app and store it, whoever does it.
const CertificateWarnBefore = 30 * 24 * time.Hour

// TeamsConfig is the microsoft.teams host settings.
type TeamsConfig struct {
	// Authority is the token endpoint's origin, without a trailing slash.
	Authority string
}

func teamsDefaults() map[string]any {
	return map[string]any{KeyTeamsAuthority: botauth.DefaultAuthority}
}

// ReadTeams reads and checks the microsoft.teams host settings offline.
func ReadTeams(v *viper.Viper) (TeamsConfig, []Problem) {
	a, err := botauth.ParseAuthority(v.GetString(KeyTeamsAuthority))
	if err != nil {
		return TeamsConfig{Authority: botauth.DefaultAuthority}, []Problem{{Key: KeyTeamsAuthority, Message: err.Error()}}
	}
	return TeamsConfig{Authority: a}, nil
}

// teamsFactory opens microsoft.teams instances whose Bot Connector token
// is heo's to mint. The adapter asks its host for an OAuth2 credential
// under teams.CredentialPurposeConnector; the instance it is given
// answers that from a botauth.Minter of the channel, which reads the
// bot's client secret, or its certificate and key, from the profile's
// credential backend. Every other request goes to the backend as it came. Describe and Validate are
// uma's.
//
// A channel's Minter outlives the instance: the daemon opens an instance
// again whenever the channel changes, and the cached token and the
// backoff carry over while the bot, the tenant, the reference and the
// endpoint stay the same.
type teamsFactory struct {
	*teams.Factory
	o Options

	mu      sync.Mutex
	minters map[model.ChannelRef]*teamsMinter
}

type teamsMinter struct {
	key string
	m   *botauth.Minter
	now func() time.Time

	mu sync.Mutex
	// purpose is the purpose of the credential the last mint read.
	purpose string
}

func (tm *teamsMinter) used(purpose string) {
	tm.mu.Lock()
	tm.purpose = purpose
	tm.mu.Unlock()
}

func (tm *teamsMinter) lastPurpose() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.purpose
}

func newTeamsFactory(o Options) *teamsFactory {
	return &teamsFactory{Factory: teams.NewFactory(teams.Options{}), o: o, minters: map[model.ChannelRef]*teamsMinter{}}
}

// Open opens the instance on a credentials port that mints the Connector
// token. Without an HTTP client or a credentials port there is nothing to
// mint with: the instance opens as uma's and fails its calls as
// unavailable.
func (f *teamsFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	var cfg teams.Config
	if deps.HTTP == nil || deps.Credentials == nil || decode(ch, teams.ConfigSchema, &cfg) != nil {
		return f.Factory.Open(ctx, ch, deps)
	}
	host, probs := ReadTeams(f.o.viper())
	if err := problemsError(probs); err != nil {
		return nil, model.NewError(model.Permanent, err.Error())
	}
	m, err := f.minter(ch, cfg, host, deps)
	if err != nil {
		return nil, model.NewError(model.Permanent, "microsoft.teams: "+err.Error())
	}
	deps.Credentials = teamsCredentials{inner: deps.Credentials, m: m}
	return f.Factory.Open(ctx, ch, deps)
}

func (f *teamsFactory) minter(ch model.Channel, cfg teams.Config, host TeamsConfig, deps adapter.Dependencies) (*botauth.Minter, error) {
	key := strings.Join([]string{cfg.TenantID, cfg.AppID, ch.CredentialRef, host.Authority}, "\x00")
	f.mu.Lock()
	defer f.mu.Unlock()
	if tm := f.minters[ch.Ref]; tm != nil && tm.key == key {
		return tm.m, nil
	}
	now := time.Now
	if deps.Clock != nil {
		now = deps.Clock.Now
	}
	secret := ports.CredentialRequest{
		Channel: ch.Ref, Reference: ch.CredentialRef, Purpose: PurposeTeamsClientSecret,
		Accept: []ports.CredentialKind{ports.KindSecret},
	}
	cert := secret
	cert.Purpose, cert.Accept = PurposeTeamsClientCertificate, []ports.CredentialKind{ports.KindClientCert}
	tm := &teamsMinter{key: key, now: now}
	m, err := botauth.New(botauth.Options{
		Tenant: cfg.TenantID, AppID: cfg.AppID, Authority: host.Authority, HTTP: deps.HTTP, Now: now, Log: f.o.Logger,
		Credential: func(ctx context.Context) (botauth.Credential, error) {
			c, purpose, err := botCredential(ctx, deps.Credentials, secret, cert)
			tm.used(purpose)
			return c, err
		},
		Rejected: func(ctx context.Context, k botauth.Kind) {
			// The store remembers the credential it just served was refused,
			// so `channel check` reports it rejected until it is rotated.
			req := secret
			if k == botauth.KindCertificate {
				req = cert
			}
			req.Fresh = true
			_, _ = deps.Credentials.Resolve(ctx, req)
		},
	})
	if err != nil {
		return nil, err
	}
	tm.m = m
	f.minters[ch.Ref] = tm
	return m, nil
}

// botCredential reads what the bot proves itself with: its client secret,
// or its certificate and private key, whichever of the two is stored, and
// the purpose it came from. Neither, or both, is refused: with both the
// host would be choosing for the operator, and the one it did not choose
// would be the one that lapsed unnoticed. A credential that cannot be read
// is its own failure, not "not stored".
func botCredential(ctx context.Context, creds ports.Credentials, secretReq, certReq ports.CredentialRequest) (botauth.Credential, string, error) {
	secret, serr := clientSecret(ctx, creds, secretReq)
	cert, cerr := clientCertificate(ctx, creds, certReq)
	absent := func(err error) bool { return model.IsCode(err, model.NotFound) || model.IsCode(err, model.Unsupported) }
	switch {
	case serr == nil && cerr == nil:
		return botauth.Credential{}, "", model.NewError(model.Conflict, fmt.Sprintf(
			"both the app's client secret and its certificate are stored; keep exactly one of purposes %s and %s",
			PurposeTeamsClientSecret, PurposeTeamsClientCertificate))
	case serr != nil && !absent(serr):
		return botauth.Credential{}, PurposeTeamsClientSecret, serr
	case cerr != nil && !absent(cerr):
		return botauth.Credential{}, PurposeTeamsClientCertificate, cerr
	case serr == nil:
		return botauth.Credential{Secret: secret}, PurposeTeamsClientSecret, nil
	case cerr == nil:
		return cert, PurposeTeamsClientCertificate, nil
	}
	return botauth.Credential{}, "", model.NewError(model.NotFound, fmt.Sprintf(
		"no credential is stored for the bot: store its client secret under purpose %s, or its certificate and private key under purpose %s",
		PurposeTeamsClientSecret, PurposeTeamsClientCertificate))
}

// clientCertificate reads the bot's certificate and its private key from
// the backend. Entra's client assertions are signed RS256, so the key must
// be RSA.
func clientCertificate(ctx context.Context, creds ports.Credentials, req ports.CredentialRequest) (botauth.Credential, error) {
	c, err := creds.Resolve(ctx, req)
	if err != nil {
		return botauth.Credential{}, err
	}
	cc, ok := c.(ports.ClientCert)
	if !ok {
		return botauth.Credential{}, model.NewError(model.Unsupported, "the bot's certificate must be a client_cert credential")
	}
	leaf := cc.Certificate.Leaf
	if leaf == nil && len(cc.Certificate.Certificate) > 0 {
		if leaf, err = x509.ParseCertificate(cc.Certificate.Certificate[0]); err != nil {
			return botauth.Credential{}, model.NewError(model.Permanent, "the bot's certificate cannot be read")
		}
	}
	key, isRSA := cc.Certificate.PrivateKey.(*rsa.PrivateKey)
	switch {
	case leaf == nil:
		return botauth.Credential{}, model.NewError(model.Permanent, "the bot's certificate holds no certificate")
	case !isRSA:
		return botauth.Credential{}, model.NewError(model.Validation, "the bot's certificate key must be RSA: the token endpoint takes assertions signed RS256")
	}
	return botauth.Credential{Certificate: leaf, Key: key}, nil
}

// clientSecret reads the bot's client secret from the backend.
func clientSecret(ctx context.Context, creds ports.Credentials, req ports.CredentialRequest) (string, error) {
	c, err := creds.Resolve(ctx, req)
	if err != nil {
		return "", err
	}
	s, ok := c.(ports.Secret)
	if !ok {
		return "", model.NewError(model.Unsupported, "the bot's client secret must be a secret credential")
	}
	defer clear(s.Bytes)
	if exp := s.Expiry(); !exp.IsZero() && !time.Now().Before(exp) {
		return "", model.NewError(model.Unauthorized, "the bot's client secret is past its expiry")
	}
	if len(s.Bytes) == 0 {
		return "", model.NewError(model.Unauthorized, "the bot's client secret is empty")
	}
	return string(s.Bytes), nil
}

// Probe mints a token for ch now, bypassing the cache and the backoff,
// and reports why it failed, or, when it worked, what is worth knowing
// (a certificate close to its not_after). It reads what Open built: a
// channel that has no instance has nothing to probe, and a credential that
// is not stored or cannot be read is the credential diagnosis's to report.
// What the diagnosis cannot see (a key that is not RSA, a certificate past
// its not_after on this host's clock, two credentials stored) is reported
// here.
func (f *teamsFactory) Probe(ctx context.Context, ch model.Channel) *adapters.CredentialProblem {
	f.mu.Lock()
	tm := f.minters[ch.Ref]
	f.mu.Unlock()
	if tm == nil {
		return nil
	}
	err := tm.m.Probe(ctx)
	if err == nil {
		if na, ok := tm.m.CertificateNotAfter(); ok && na.Sub(tm.now()) < CertificateWarnBefore {
			return &adapters.CredentialProblem{
				Purpose: PurposeTeamsClientCertificate, Code: "expires_soon", Warning: true, Message: fmt.Sprintf(
					"the bot's certificate expires on %s (in %d days): register a new one with the app and replace the credential under purpose %s before then",
					na.UTC().Format(time.DateOnly), int(math.Ceil(na.Sub(tm.now()).Hours()/24)), PurposeTeamsClientCertificate),
			}
		}
		return nil
	}
	if me, ok := asMint(err); ok {
		purpose := PurposeTeamsClientSecret
		if me.Kind == botauth.KindCertificate {
			purpose = PurposeTeamsClientCertificate
		}
		return &adapters.CredentialProblem{Purpose: purpose, Code: me.Code, Message: probeMessage(me)}
	}
	switch code := model.CodeOf(err); code {
	case model.Validation, model.Unauthorized, model.Unsupported, model.Conflict, model.Permanent:
		purpose := tm.lastPurpose()
		if purpose == "" {
			purpose = PurposeTeamsClientSecret
		}
		msg := "the bot's credential cannot be used: "
		if me, ok := errors.AsType[*model.Error](err); ok {
			msg += me.SafeMessage
		}
		return &adapters.CredentialProblem{Purpose: purpose, Code: string(code), Message: msg}
	}
	return nil
}

func asMint(err error) (*botauth.MintError, bool) { return errors.AsType[*botauth.MintError](err) }

// probeMessage is what a failed mint says to the operator: the provider's
// own answer, and what cures it.
func probeMessage(e *botauth.MintError) string {
	msg := fmt.Sprintf("the bot's Connector token cannot be minted: %s", e)
	switch {
	case e.Rejects() && e.Kind == botauth.KindCertificate:
		return msg + "; store the certificate registered for the app, with its private key, under purpose " + PurposeTeamsClientCertificate +
			" (the app's Certificates & secrets lists its thumbprint)"
	case e.Rejects():
		return msg + "; store the app's current client secret (the secret's value, not its ID) under purpose " + PurposeTeamsClientSecret
	case e.Temporary && e.Code == "unreachable":
		return msg + "; check that this host reaches the token endpoint (" + KeyTeamsAuthority + ") and the channel's tenant_id"
	case e.Temporary:
		return msg + "; it is retried with backoff"
	}
	return msg + "; check the channel's app_id and tenant_id"
}

// teamsCredentials answers the Bot Connector purpose with the minted
// token and every other purpose from the backend.
type teamsCredentials struct {
	inner ports.Credentials
	m     *botauth.Minter
}

func (c teamsCredentials) Resolve(ctx context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	if req.Purpose != teams.CredentialPurposeConnector {
		return c.inner.Resolve(ctx, req)
	}
	if !req.Accepts(ports.KindOAuth2) {
		return nil, model.NewError(model.Unsupported, "the Bot Connector token is an oauth2 credential")
	}
	tok, err := c.m.Token(ctx, req.Fresh)
	if err != nil {
		return nil, err
	}
	return ports.OAuth2{AccessToken: tok.Value, ExpiresAt: tok.Expiry}, nil
}
