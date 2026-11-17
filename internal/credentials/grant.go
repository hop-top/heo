package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// OAuthOptions tune how a stored OAuth grant is exchanged for access
// tokens. The zero value is right for production.
type OAuthOptions struct {
	// HTTPClient sends the token requests; a client with a 30 second
	// timeout when nil. Redirects are never followed whatever it is.
	HTTPClient *http.Client
	// LockDir holds the advisory lock files that serialize a refresh
	// across processes; kit's default, under the user cache dir, when
	// empty.
	LockDir string
	// Skew is how long before expiry an access token counts as stale;
	// kit's default (five minutes) when zero.
	Skew time.Duration
}

// tokenTimeout bounds one token request when no client is supplied.
const tokenTimeout = 30 * time.Second

// grantSource is the token source of one stored grant. digest pins the
// grant it was built for: a grant stored anew gets a new source.
type grantSource struct {
	src    *oauth.Source
	grant  oauth.Grant
	digest string
}

// grantError is a failure of a stored OAuth grant: the uma error to
// return, and the diagnosis status it makes.
type grantError struct {
	me     *model.Error
	status Status
	fix    string
	kind   oauth.GrantKind
}

func (e *grantError) Error() string { return e.me.Error() }
func (e *grantError) Unwrap() error { return e.me }

// grantKindOf reports the grant kind of a stored value that is an OAuth
// grant envelope rather than a credential envelope. The two share no
// "kind": uma's are secret, password, oauth2, client_cert and kerberos.
func grantKindOf(raw []byte) (oauth.GrantKind, bool) {
	var head struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &head) != nil {
		return "", false
	}
	k := oauth.GrantKind(head.Kind)
	return k, k.Valid()
}

// peeked is the first entry of a reference's keys that holds a value.
type peeked struct {
	key string
	// raw is the value of an entry that is not a grant, to decode as a
	// credential envelope without reading the store again.
	raw []byte
	// grant and gerr are set for a grant envelope: its decoding, or
	// why it is not valid.
	grant   oauth.Grant
	gerr    *grantError
	isGrant bool
}

// peekGrant reads the first of keys holding a value and classifies it:
// an OAuth grant, or a credential envelope. Every read failure is "not
// a grant" with no entry: the credential path reads again and reports
// it.
func (b *Backend) peekGrant(ctx context.Context, keys []string) peeked {
	for _, k := range keys {
		sec, err := b.store.Get(ctx, k)
		if errors.Is(err, kitsecret.ErrNotFound) {
			continue
		}
		if err != nil || sec == nil {
			return peeked{}
		}
		kind, isGrant := grantKindOf(sec.Value)
		if !isGrant {
			return peeked{key: k, raw: sec.Value}
		}
		defer clear(sec.Value)
		g, err := oauth.DecodeGrant(sec.Value)
		if err != nil {
			return peeked{key: k, isGrant: true, gerr: b.grantFault(kind, err)}
		}
		return peeked{key: k, isGrant: true, grant: g}
	}
	return peeked{}
}

// writable reports whether a rotated refresh token and a cached access
// token can be written back to where credentials resolve. An aps
// profile's secrets and the agefile backend are read-only; the others
// declare it.
func (b *Backend) writable() bool {
	_, mutable := b.store.(kitsecret.MutableStore)
	return mutable && oauth.IsWritable(b.store)
}

// grantTokens returns the token source of the grant stored under key,
// built once per stored grant so concurrent callers share one refresh.
func (b *Backend) grantTokens(ctx context.Context, key string, g oauth.Grant) (*grantSource, *grantError) {
	digest := oauth.Digest(g)
	b.gmu.Lock()
	defer b.gmu.Unlock()
	if gs := b.grants[key]; gs != nil && gs.digest == digest {
		return gs, nil
	}
	hc := b.oauth.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: tokenTimeout}
	}
	opts := []oauth.Option{oauth.WithHTTPClient(hc), oauth.WithClock(b.now)}
	if b.oauth.LockDir != "" {
		opts = append(opts, oauth.WithLockDir(b.oauth.LockDir))
	}
	if b.oauth.Skew > 0 {
		opts = append(opts, oauth.WithSkew(b.oauth.Skew))
	}
	src, err := oauth.TokenSource(ctx, b.store, key, opts...)
	if err != nil {
		return nil, b.grantFault(g.Kind, err)
	}
	gs := &grantSource{src: src, grant: g, digest: digest}
	b.grants[key] = gs
	return gs, nil
}

// grantToken mints or reuses the access token of gs as the credential
// of req.
func (b *Backend) grantToken(ctx context.Context, gs *grantSource) (ports.Credential, *grantError) {
	tok, err := gs.src.TokenWithIdentity(ctx)
	if err != nil {
		return nil, b.grantFault(gs.grant.Kind, err)
	}
	// User and AuthzID are the SASL identity (IMAP XOAUTH2) a writer
	// stored with the cached token; empty for a token heo minted.
	return ports.OAuth2{User: tok.User, AuthzID: tok.AuthzID, AccessToken: tok.AccessToken, ExpiresAt: tok.ExpiresAt}, nil
}

// readGrant resolves the OAuth grant stored under key for req, nil
// grantError and a nil credential never both.
func (b *Backend) readGrant(ctx context.Context, req ports.CredentialRequest, key string, g oauth.Grant) (ports.Credential, *grantSource, *grantError) {
	if !req.Accepts(ports.KindOAuth2) {
		return nil, nil, &grantError{
			me:     model.NewError(model.Unsupported, "stored credential kind oauth2 not accepted"),
			status: StatusUnsupported, kind: g.Kind,
			fix: "store a credential of a kind the channel accepts in " + b.where([]string{key}),
		}
	}
	gs, gerr := b.grantTokens(ctx, key, g)
	if gerr != nil {
		return nil, nil, gerr
	}
	cred, gerr := b.grantToken(ctx, gs)
	if gerr != nil {
		return nil, nil, gerr
	}
	return cred, gs, nil
}

// grantFault maps a kit OAuth failure onto the uma error a caller sees,
// and the diagnosis status it makes. The text is fixed: kit's errors
// carry no material, and heo adds none.
func (b *Backend) grantFault(kind oauth.GrantKind, err error) *grantError {
	ge := &grantError{kind: kind}
	var ve *oauth.ValidationError
	switch {
	case errors.Is(err, oauth.ErrNotWritable):
		msg := b.notWritableMessage()
		ge.me, ge.status, ge.fix = model.NewError(model.Unsupported, msg), StatusUnsupported, msg
	case errors.Is(err, oauth.ErrNeedsReconsent):
		ge.me = model.NewError(model.Unauthorized, "the provider no longer accepts the OAuth grant: the user must consent again")
		ge.status = StatusRejected
		ge.fix = "run the consent flow again and store the new grant"
	case errors.Is(err, oauth.ErrCredentialRejected):
		ge.me = model.NewError(model.Unauthorized, "the provider rejected the OAuth grant's credentials")
		ge.status = StatusRejected
		ge.fix = "store a valid client secret, key or refresh token the grant refers to"
	case errors.Is(err, oauth.ErrTransient):
		ge.me = model.NewError(model.Transient, "the OAuth token endpoint could not be reached or failed; try again")
		ge.status = StatusUnavailable
		ge.fix = "check that the token endpoint is reachable"
	case errors.As(err, &ve):
		ge.me = model.NewError(model.Permanent, "stored OAuth grant is malformed: "+strings.TrimPrefix(ve.Error(), "oauth: invalid envelope: "))
		ge.status = StatusMalformed
		ge.fix = "replace the grant (or the secret it refers to) with a valid one"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		ge.me, ge.status = model.NewError(model.Transient, "credential check interrupted"), StatusUnavailable
	default:
		// Kit's remaining failures (a redirecting or untrusted token
		// endpoint, an unusable reply) carry fixed text.
		ge.me = model.NewError(model.Unavailable, strings.TrimPrefix(err.Error(), "oauth: "))
		ge.status = StatusUnavailable
		ge.fix = "check the grant's token_url"
	}
	return ge
}

// notWritableMessage names why the profile's credential backend cannot
// hold a refresh-token grant, and the backends that can.
func (b *Backend) notWritableMessage() string {
	var held string
	if b.name == BackendAps {
		held = "aps secrets are read-only to heo"
	} else {
		held = fmt.Sprintf("the %s backend is read-only", b.name)
	}
	var ok []string
	for _, n := range Backends() {
		if n != BackendAgefile {
			ok = append(ok, n)
		}
	}
	return fmt.Sprintf("a refresh_token grant needs a writable credential backend to keep a rotated refresh token, and %s: "+
		"use a native profile with credentials.backend set to one of %s", held, strings.Join(ok, ", "))
}

// GrantRefusal is a stored OAuth grant this profile's backend cannot
// serve.
type GrantRefusal struct {
	// Grant is the grant kind.
	Grant string
	// Message says why, and names the backends that can.
	Message string
}

// RefuseGrant reports whether the entry stored for req is an OAuth
// grant the backend cannot serve: a refresh_token grant on a backend
// that cannot be written, an aps profile's secrets or agefile. A
// provider may rotate the refresh token on any use, so heo cannot tell
// a rotating grant from a stable one before it is used, and refuses
// the kind. Nothing is sent to a provider; no stored value is changed.
// A reference with no grant, or one that cannot be read, is not
// refused.
func (b *Backend) RefuseGrant(ctx context.Context, req Request) *GrantRefusal {
	pr := b.portsRequest(req)
	keys, err := b.keysFor(pr)
	if err != nil {
		return nil
	}
	p := b.peekGrant(ctx, keys)
	if !p.isGrant || p.gerr != nil || p.grant.Kind != oauth.RefreshToken || b.writable() {
		return nil
	}
	return &GrantRefusal{Grant: string(p.grant.Kind), Message: b.notWritableMessage()}
}
