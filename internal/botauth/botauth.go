// Package botauth mints and refreshes the Bot Connector access token of
// one Microsoft Teams bot.
//
// A bot registration is a Microsoft Entra application in one tenant. The
// Bot Connector accepts a bearer token that application obtains from its
// tenant's token endpoint with the client credentials grant, for the
// scope https://api.botframework.com/.default
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication).
// A Minter does that for one channel: the operator supplies the app's
// client secret, or a certificate and its private key, and the Minter
// keeps the token it gets in memory, mints the next one before the cached
// one is out, and reads the credential from the credential store at every
// mint, so a secret or certificate rotated there is used by the next mint
// with no restart.
//
// With a secret, the request carries it in its body. With a certificate it
// carries a client assertion instead
// (https://learn.microsoft.com/en-us/entra/identity-platform/certificate-credentials,
// RFC 7523): a JWT signed RS256 with the private key, whose x5t#S256
// header names the certificate, for the tenant's token endpoint as
// audience and the app as issuer and subject, valid for ten minutes and
// good once.
//
// The protocol (request shape, error parsing) is golang.org/x/oauth2's
// clientcredentials package, which takes the assertion as extra form
// fields; golang-jwt/jwt/v5 signs it. The cache, the single flight, the
// backoff and the clock are the Minter's: oauth2's own token source reads
// the wall clock, refreshes ten seconds before expiry and has no backoff,
// and Microsoft's MSAL for Go would add endpoint discovery requests and
// a clock of its own.
//
// Nothing here logs or returns the secret, the key, an assertion or a
// token: errors carry the provider's own error code (invalid_client) and
// its AADSTS code, both checked to be plain tokens, and the HTTP status.
package botauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"golang.org/x/sync/singleflight"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const (
	// Scope is what the Bot Connector's token is requested for.
	Scope = "https://api.botframework.com/.default"
	// DefaultAuthority is the public cloud's Microsoft Entra endpoint.
	DefaultAuthority = "https://login.microsoftonline.com"

	// DefaultSkew is how long before its expiry a token is replaced.
	DefaultSkew = 5 * time.Minute
	// DefaultBackoffMin and DefaultBackoffMax bound the wait between
	// attempts after a failed mint: the minimum doubles per failure up
	// to the maximum.
	DefaultBackoffMin = 2 * time.Second
	DefaultBackoffMax = 5 * time.Minute

	// freshWindow is how long a token just minted answers a request for a
	// fresh one: the Bot Connector refusing a token minted seconds ago is
	// not cured by minting again, and callers that were all refused the
	// same old token share the one new one.
	freshWindow = 30 * time.Second
	// mintTimeout bounds one request to the token endpoint.
	mintTimeout = 30 * time.Second
	// maxRetryAfter caps a wait the endpoint states.
	maxRetryAfter = 10 * time.Minute
	// assertionTTL is how long a client assertion is valid: one mint uses
	// it at once, and Entra refuses a long-lived one.
	assertionTTL = 10 * time.Minute

	// assertionType is the client_assertion_type of a JWT bearer assertion
	// (RFC 7523 section 2.2).
	assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// Kind is what an app proves itself with.
type Kind string

// Kinds.
const (
	KindSecret      Kind = "secret"
	KindCertificate Kind = "certificate"
)

// Credential is what the app proves itself with at the token endpoint:
// its client secret, or a certificate registered for it with the private
// key that goes with it. Exactly one of the two. It renders as
// "[redacted]" under every fmt verb, in JSON and in slog.
type Credential struct {
	// Secret is the app's client secret (the value, not its ID).
	Secret string
	// Certificate is the app's certificate, the leaf; Key is its RSA
	// private key. Entra's assertions are RS256.
	Certificate *x509.Certificate
	Key         *rsa.PrivateKey
}

// Kind says which of the two the credential holds.
func (c Credential) Kind() Kind {
	if c.Secret == "" && (c.Certificate != nil || c.Key != nil) {
		return KindCertificate
	}
	return KindSecret
}

const redacted = "[redacted]"

func (Credential) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (Credential) String() string               { return redacted }
func (Credential) GoString() string             { return redacted }
func (Credential) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Credential) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Credential) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// check refuses a credential no token endpoint could accept, without
// naming any of its material.
func (c Credential) check(now time.Time) error {
	switch {
	case c.Secret != "" && (c.Certificate != nil || c.Key != nil):
		return model.NewError(model.Validation, "the app's credential is a client secret or a certificate, not both")
	case c.Secret != "":
		return nil
	case c.Certificate == nil && c.Key == nil:
		return model.NewError(model.Validation, "the app's credential is empty: a client secret or a certificate is needed")
	case c.Certificate == nil || c.Key == nil:
		return model.NewError(model.Validation, "the app's certificate needs its private key")
	}
	pub, ok := c.Certificate.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&c.Key.PublicKey) {
		return model.NewError(model.Unauthorized, "the private key does not belong to the app's certificate (an RSA key pair)")
	}
	if !now.Before(c.Certificate.NotAfter) {
		return model.NewError(model.Unauthorized, "the app's certificate expired on "+c.Certificate.NotAfter.UTC().Format(time.DateOnly))
	}
	return nil
}

// Logger is the structured logger a Minter reports to; kit's console
// logger satisfies it.
type Logger interface {
	Info(msg any, keyvals ...any)
	Error(msg any, keyvals ...any)
}

// Options configure a Minter.
type Options struct {
	// Tenant and AppID are the bot registration's Microsoft Entra tenant
	// and application (client) ID. Required.
	Tenant, AppID string
	// Authority is the token endpoint's origin; empty is DefaultAuthority.
	// It is a host setting: a channel never chooses where its secret goes.
	Authority string
	// HTTP sends the token requests. Required.
	HTTP ports.HTTPClient
	// Credential reads the app's current client secret or certificate, at
	// every mint, so the one stored now is the one used. Required. Its
	// errors are returned as they are.
	Credential func(context.Context) (Credential, error)
	// Rejected, when set, is told that the token endpoint refused the
	// credential of that kind Credential returned (invalid_client): the
	// credential store can remember that it is not to be trusted until
	// rotated.
	Rejected func(context.Context, Kind)
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Skew is how long before expiry a token is replaced; DefaultSkew
	// when zero. A token living under twice the skew is replaced half
	// way.
	Skew time.Duration
	// BackoffMin and BackoffMax; the defaults when zero.
	BackoffMin, BackoffMax time.Duration
	// Log reports mint failures and recoveries; nil reports nothing.
	Log Logger
}

// Token is a minted Bot Connector access token.
type Token struct {
	Value  string
	Expiry time.Time
}

// Minter mints, caches and refreshes one bot's Bot Connector token. It is
// safe for concurrent use: callers needing a token while one is being
// minted share that mint.
type Minter struct {
	o    Options
	url  string
	now  func() time.Time
	key  []byte
	http *http.Client
	sf   singleflight.Group

	mu        sync.Mutex
	have      bool
	tok       Token
	minted    time.Time
	refreshAt time.Time
	fail      *failure
	// notAfter is the certificate's not_after at the last mint that used
	// one; certUsed is false when the last mint used a secret.
	notAfter time.Time
	certUsed bool
}

// failure is the last failed mint and the wait it asks for.
type failure struct {
	err      *MintError
	credFP   [sha256.Size]byte
	attempts int
	until    time.Time
}

// New returns a Minter that has minted nothing yet.
func New(o Options) (*Minter, error) {
	switch {
	case o.Tenant == "" || o.AppID == "":
		return nil, errors.New("botauth: tenant and app ID are required")
	case o.HTTP == nil:
		return nil, errors.New("botauth: no HTTP client")
	case o.Credential == nil:
		return nil, errors.New("botauth: no credential source")
	}
	authority, err := ParseAuthority(o.Authority)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Skew <= 0 {
		o.Skew = DefaultSkew
	}
	if o.BackoffMin <= 0 {
		o.BackoffMin = DefaultBackoffMin
	}
	if o.BackoffMax < o.BackoffMin {
		o.BackoffMax = max(DefaultBackoffMax, o.BackoffMin)
	}
	m := &Minter{
		o:    o,
		url:  authority + "/" + url.PathEscape(o.Tenant) + "/oauth2/v2.0/token",
		now:  o.Now,
		key:  make([]byte, 32),
		http: &http.Client{Transport: doer{o.HTTP}},
	}
	if _, err := rand.Read(m.key); err != nil {
		return nil, fmt.Errorf("botauth: %w", err)
	}
	return m, nil
}

// ParseAuthority checks a token endpoint origin and returns it without a
// trailing slash; empty is DefaultAuthority. The client secret is sent
// there, so it must be https, with no user info, query or fragment; only a
// loopback host may be plain http (a stand-in endpoint of one's own).
func ParseAuthority(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultAuthority, nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery ||
		(u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%q: want an origin such as %s", s, DefaultAuthority)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return "", fmt.Errorf("%q: the client secret is sent there, so it must be https (http only for a loopback host)", s)
	}
	return u.Scheme + "://" + u.Host, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// doer makes a ports.HTTPClient the http.RoundTripper an http.Client
// wants.
type doer struct{ c ports.HTTPClient }

func (d doer) RoundTrip(r *http.Request) (*http.Response, error) { return d.c.Do(r) }

// Token returns a token that is valid and not within the skew of its
// expiry, minting one when there is none. fresh says the Bot Connector
// refused the token last returned: the cached one is not used, unless it
// was minted moments ago. While the token endpoint is failing, a cached
// token that is still valid is returned instead, and a call that has none
// fails at once with the last failure until the backoff is over or the
// credential changes. Errors are *model.Error.
func (m *Minter) Token(ctx context.Context, fresh bool) (Token, error) {
	now := m.now()
	m.mu.Lock()
	if m.have {
		switch {
		case fresh && now.Sub(m.minted) < freshWindow && now.Before(m.tok.Expiry):
			defer m.mu.Unlock()
			return m.tok, nil
		case !fresh && now.Before(m.refreshAt):
			defer m.mu.Unlock()
			return m.tok, nil
		}
	}
	cached, usable := m.tok, m.have && !fresh && now.Before(m.tok.Expiry)
	f := m.fail
	m.mu.Unlock()

	if f != nil && now.Before(f.until) {
		cred, err := m.o.Credential(ctx)
		if err != nil {
			return Token{}, err
		}
		fp := m.fingerprint(cred)
		if fp == f.credFP {
			if usable {
				return cached, nil
			}
			return Token{}, f.err.Model()
		}
		// A credential rotated since the failure is tried at once.
	}
	tok, err := m.mintShared(ctx)
	if err != nil {
		if usable {
			return cached, nil
		}
		return Token{}, toModel(err)
	}
	return tok, nil
}

// Probe mints a token now, whatever the cache and the backoff say, and
// reports why it could not: the diagnosis `channel check --remote` gives
// of a bot's credentials. A mint another caller has under way is shared.
// It returns nil when a token was minted.
func (m *Minter) Probe(ctx context.Context) error {
	if _, err := m.mintShared(ctx); err != nil {
		return err
	}
	return nil
}

// mintShared runs one mint for every caller waiting on one.
func (m *Minter) mintShared(ctx context.Context) (Token, error) {
	ch := m.sf.DoChan("mint", func() (any, error) {
		// The mint outlives the caller that started it: others share it.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mintTimeout)
		defer cancel()
		return m.mint(ctx)
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			return Token{}, r.Err
		}
		return r.Val.(Token), nil
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}
}

func (m *Minter) mint(ctx context.Context) (Token, error) {
	cred, err := m.o.Credential(ctx)
	if err != nil {
		return Token{}, err
	}
	if err := cred.check(m.now()); err != nil {
		return Token{}, err
	}
	fp := m.fingerprint(cred)
	var params url.Values
	if cred.Kind() == KindCertificate {
		signed, err := m.assertion(cred)
		if err != nil {
			return Token{}, err
		}
		params = url.Values{"client_assertion_type": {assertionType}, "client_assertion": {signed}}
	}
	tok, merr := m.fetch(ctx, cred, params)
	now := m.now()
	if merr != nil {
		m.record(merr, fp, now)
		if merr.Rejects() && m.o.Rejected != nil {
			m.o.Rejected(ctx, merr.Kind)
		}
		return Token{}, merr
	}
	m.mu.Lock()
	recovered := m.fail != nil
	ttl := tok.Expiry.Sub(now)
	m.tok, m.have, m.minted, m.fail = tok, true, now, nil
	m.refreshAt = now.Add(max(ttl-m.o.Skew, ttl/2))
	m.certUsed = cred.Kind() == KindCertificate
	m.notAfter = time.Time{}
	if m.certUsed {
		m.notAfter = cred.Certificate.NotAfter
	}
	m.mu.Unlock()
	if recovered && m.o.Log != nil {
		m.o.Log.Info("microsoft.teams: connector token minted again", "app_id", m.o.AppID)
	}
	return tok, nil
}

// CertificateNotAfter is the not_after of the certificate the last
// successful mint signed with; false when that mint used a secret, or
// none has succeeded.
func (m *Minter) CertificateNotAfter() (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notAfter, m.certUsed
}

// record keeps a failed mint and the wait before the next attempt.
func (m *Minter) record(err *MintError, fp [sha256.Size]byte, now time.Time) {
	m.mu.Lock()
	attempts := 1
	if m.fail != nil && m.fail.credFP == fp {
		attempts = m.fail.attempts + 1
	}
	wait := min(m.o.BackoffMin<<min(attempts-1, 30), m.o.BackoffMax)
	wait = max(wait, err.RetryAfter)
	m.fail = &failure{err: err, credFP: fp, attempts: attempts, until: now.Add(wait)}
	m.mu.Unlock()
	if m.o.Log != nil {
		m.o.Log.Error("microsoft.teams: connector token mint failed",
			"app_id", m.o.AppID, "error", err.Error(), "attempt", attempts, "retry_in", wait.String())
	}
}

// fingerprint is a keyed hash of a credential: equal for the same secret,
// or the same certificate and key, in this process, useless outside it.
func (m *Minter) fingerprint(c Credential) [sha256.Size]byte {
	h := hmac.New(sha256.New, m.key)
	h.Write([]byte(c.Kind()))
	h.Write([]byte{0})
	h.Write([]byte(c.Secret))
	if c.Certificate != nil {
		h.Write(c.Certificate.Raw)
	}
	if c.Key != nil {
		h.Write(c.Key.N.Bytes())
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// assertion signs the client assertion of one mint.
func (m *Minter) assertion(c Credential) (string, error) {
	now := m.now()
	sum := sha256.Sum256(c.Certificate.Raw)
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"aud": m.url, "iss": m.o.AppID, "sub": m.o.AppID, "jti": uuid.NewString(),
		"nbf": now.Unix(), "exp": now.Add(assertionTTL).Unix(),
	})
	t.Header["x5t#S256"] = base64.RawURLEncoding.EncodeToString(sum[:])
	signed, err := t.SignedString(c.Key)
	if err != nil {
		// The library's error names nothing of the key, and neither does ours.
		return "", model.NewError(model.Unsupported, "the app's private key cannot sign a client assertion (RS256)")
	}
	return signed, nil
}

// fetch asks the token endpoint for a token.
func (m *Minter) fetch(ctx context.Context, c Credential, params url.Values) (Token, *MintError) {
	cfg := clientcredentials.Config{
		ClientID: m.o.AppID, TokenURL: m.url, Scopes: []string{Scope},
		// Entra takes the credential in the body; oauth2's default probes the
		// Authorization header first and sends the request twice.
		AuthStyle: oauth2.AuthStyleInParams,
	}
	kind := c.Kind()
	cfg.ClientSecret, cfg.EndpointParams = c.Secret, params
	tok, err := cfg.Token(context.WithValue(ctx, oauth2.HTTPClient, m.http))
	if err != nil {
		me := mintError(err)
		me.Kind = kind
		return Token{}, me
	}
	// The token type is not checked: Entra issues bearer tokens only, and a
	// cassette's answer has it redacted.
	ttl, ok := expiresIn(tok)
	if !ok {
		return Token{}, &MintError{Code: "invalid_response"}
	}
	// The expiry is the Minter's clock plus what the endpoint said:
	// oauth2's own is the wall clock's.
	return Token{Value: tok.AccessToken, Expiry: m.now().Add(ttl)}, nil
}

// expiresIn is the token's lifetime as the endpoint stated it.
func expiresIn(t *oauth2.Token) (time.Duration, bool) {
	var secs float64
	switch v := t.Extra("expires_in").(type) {
	case float64:
		secs = v
	case string:
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		secs = n
	default:
		return 0, false
	}
	if secs <= 0 || secs > 24*3600*7 {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

var (
	codeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	stsRe  = regexp.MustCompile(`AADSTS[0-9]{3,7}`)
)

// MintError is a failed request to the token endpoint. It holds what is
// safe to show: the status, the provider's own error code and its AADSTS
// code, never the endpoint's free text, which could echo a request.
type MintError struct {
	// Kind is what the app proved itself with in the refused request.
	Kind Kind
	// Status is the HTTP status of the answer; 0 when there was none.
	Status int
	// Code is the OAuth error of the answer (invalid_client), when it is a
	// plain token; "invalid_response" for an answer that is no token;
	// "unreachable" for none.
	Code string
	// STS is the AADSTS code the answer's description starts with, such as
	// AADSTS7000215 (invalid client secret).
	STS string
	// Temporary reports a failure a later attempt can cure on its own: a
	// rate limit, a server error, no answer.
	Temporary bool
	// RetryAfter is the wait the answer asks for.
	RetryAfter time.Duration
}

func (e *MintError) Error() string {
	var b strings.Builder
	b.WriteString("Microsoft Entra token endpoint")
	switch {
	case e.Status != 0:
		b.WriteString(": HTTP " + strconv.Itoa(e.Status))
	case e.Code == "unreachable":
		b.WriteString(": unreachable")
	}
	if e.Code != "" && e.Code != "unreachable" {
		b.WriteString(": " + e.Code)
	}
	if e.STS != "" {
		b.WriteString(" (" + e.STS + ")")
	}
	return b.String()
}

// Rejects reports whether the endpoint refused the client secret or
// certificate itself.
func (e *MintError) Rejects() bool {
	return e.Code == "invalid_client" || e.Code == "unauthorized_client"
}

// Model is the error a caller of Token gets: a rate limit or a transient
// failure to retry, or an unauthorized one that only a fix of the secret
// or the registration cures.
func (e *MintError) Model() *model.Error {
	msg := e.Error()
	switch {
	case e.Temporary && e.Status == http.StatusTooManyRequests:
		return &model.Error{Code: model.RateLimited, SafeMessage: msg, Retry: model.RetrySameOperation, RetryAfter: e.RetryAfter}
	case e.Temporary:
		return &model.Error{Code: model.Transient, SafeMessage: msg, Retry: model.RetrySameOperation, RetryAfter: e.RetryAfter}
	case e.Rejects() && e.Kind == KindCertificate:
		return &model.Error{Code: model.Unauthorized, SafeMessage: msg + ": the app's client certificate is refused", Retry: model.NoRetry}
	case e.Rejects():
		return &model.Error{Code: model.Unauthorized, SafeMessage: msg + ": the app's client secret is refused", Retry: model.NoRetry}
	}
	return &model.Error{Code: model.Permanent, SafeMessage: msg, Retry: model.NoRetry}
}

func toModel(err error) error {
	if me, ok := errors.AsType[*MintError](err); ok {
		return me.Model()
	}
	return err
}

// mintError classifies what the oauth2 library returned.
func mintError(err error) *MintError {
	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		e := &MintError{}
		if re.Response != nil {
			e.Status = re.Response.StatusCode
			e.RetryAfter = retryAfter(re.Response.Header)
		}
		if codeRe.MatchString(re.ErrorCode) {
			e.Code = re.ErrorCode
		}
		e.STS = stsRe.FindString(re.ErrorDescription)
		e.Temporary = e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout ||
			e.Status >= 500 || e.Code == "temporarily_unavailable" || e.Code == "server_error"
		return e
	}
	if errors.Is(err, context.Canceled) {
		return &MintError{Code: "unreachable", Temporary: true}
	}
	// A transport failure, or an answer that does not parse. Neither
	// message is shown: they spell out the request.
	var ue *url.Error
	if errors.As(err, &ue) || errors.Is(err, context.DeadlineExceeded) {
		return &MintError{Code: "unreachable", Temporary: true}
	}
	return &MintError{Code: "invalid_response"}
}

func retryAfter(h http.Header) time.Duration {
	s, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64)
	if err != nil || s < 0 {
		return 0
	}
	return min(time.Duration(s)*time.Second, maxRetryAfter)
}
