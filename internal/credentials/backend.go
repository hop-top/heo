package credentials

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"hop.top/heo/internal/profile"
	kitsecret "hop.top/kit/go/storage/secret"
	umasecret "hop.top/uma/contrib/kit/secret"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Request asks for the credential behind one reference, for one
// purpose and one connection attempt.
type Request struct {
	// Reference is the credential reference from configuration.
	Reference string
	// Purpose names what the credential is for ("imap",
	// "smtp.submission", "storage.postgres", ...).
	Purpose string
	// Accept lists the credential kinds the caller can use, preferred
	// first.
	Accept []ports.CredentialKind
	// Fresh reports that the provider rejected the credential last
	// returned for this reference: a credential that has not been
	// rotated since is refused with model.Unauthorized instead of being
	// handed back.
	Fresh bool
}

// Resolver resolves credential references through the profile's one
// credential backend. A heo component that needs a credential (a
// storage driver's connection secret, ...) depends on this, never on a
// concrete store. The returned credential is resolved for one
// connection attempt: callers do not keep it past that attempt or its
// Expiry. Errors are *model.Error and never carry material.
type Resolver interface {
	Resolve(ctx context.Context, req Request) (ports.Credential, error)
}

// Options configure Open.
type Options struct {
	// Profile is the selected profile.
	Profile profile.Ref
	// Tenant is the profile's uma tenant (runtime.tenant).
	Tenant model.TenantID
	// Config selects the kit secret store.
	Config Config
	// Now is the clock Check uses for expiry and the OAuth token
	// sources use for freshness; time.Now when nil.
	Now func() time.Time
	// OAuth tunes the exchange of a stored OAuth grant for access
	// tokens (see Backend).
	OAuth OAuthOptions
}

// Backend is a profile's credential backend: the kit secret store the
// configuration selects, or, for an aps-bound profile, the aps
// profile's secrets. It resolves every request against the store,
// with no cache, so a credential rotated in the store is used on the
// next attempt.
//
// An entry that holds an OAuth grant rather than a credential (kit's
// grant envelope, see hop.top/kit/go/storage/secret/oauth) resolves to
// a fresh access token: the grant stays in the store, the backend
// mints the token and caches it. A writable backend keeps the cached
// token and any rotated refresh token in the store; an aps profile's
// secrets and the agefile backend cannot be written, so the token is
// cached in memory only and a refresh_token grant is refused.
type Backend struct {
	name    string
	ref     profile.Ref
	tenant  model.TenantID
	kit     kitsecret.MutableStore // heo's own store: surface token, and credentials of native profiles
	store   kitsecret.Store        // where credential references resolve
	now     func() time.Time
	apsPath string
	oauth   OAuthOptions

	gmu    sync.Mutex // guards grants while a source is built
	grants map[string]*grantSource

	mu       sync.Mutex
	fpKey    []byte
	served   map[slot][sha256.Size]byte // fingerprint last served
	rejected map[slot]rejection         // credential the provider rejected
}

// slot is what Fresh and rejection are tracked per: one reference for
// one purpose, whichever entry serves it.
type slot struct{ reference, purpose string }

type rejection struct {
	fp [sha256.Size]byte
	at time.Time
}

var _ Resolver = (*Backend)(nil)

// Open constructs the profile's backend. It validates the
// configuration, constructs the kit store directly and, for an
// aps-bound profile, binds the aps profile's secrets file. Nothing is
// read from any store except openbao's token, read from the OS
// keyring.
func Open(ctx context.Context, o Options) (*Backend, error) {
	for _, p := range o.Config.Validate() {
		if !p.Warning {
			return nil, fmt.Errorf("%s: %s", p.Key, p.Message)
		}
	}
	if err := o.Tenant.Validate(); err != nil {
		return nil, fmt.Errorf("tenant: %w", err)
	}
	kit, err := openKitStore(ctx, o.Config, o.Profile)
	if err != nil {
		return nil, err
	}
	b := &Backend{
		name: o.Config.Backend, ref: o.Profile, tenant: o.Tenant, kit: kit, store: kit,
		now: o.Now, fpKey: make([]byte, 32), oauth: o.OAuth, grants: map[string]*grantSource{},
		served: map[slot][sha256.Size]byte{}, rejected: map[slot]rejection{},
	}
	if b.now == nil {
		b.now = time.Now
	}
	if o.Profile.IsAps() {
		if b.apsPath, err = profile.ApsSecretsPath(o.Profile.ID); err != nil {
			return nil, fmt.Errorf("locate aps secrets of %s: %w", o.Profile, err)
		}
		b.name, b.store = BackendAps, apsStore{path: b.apsPath}
	}
	if _, err := rand.Read(b.fpKey); err != nil {
		return nil, fmt.Errorf("credential fingerprint key: %w", err)
	}
	return b, nil
}

// Name returns the backend name: a configured kit backend, or "aps".
func (b *Backend) Name() string { return b.name }

// SurfaceTokens returns the store holding the profile's surface token
// under SurfaceTokenKey: always heo's kit store, because an aps
// profile's secrets are read-only to heo.
func (b *Backend) SurfaceTokens() kitsecret.MutableStore { return b.kit }

// Channels returns the backend as uma's ports.Credentials for the
// profile's channels. A request for another tenant than the profile's
// is refused with model.Unauthorized.
func (b *Backend) Channels() ports.Credentials { return channelCredentials{b} }

type channelCredentials struct{ b *Backend }

func (c channelCredentials) Resolve(ctx context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	return c.b.resolve(ctx, req)
}

// Resolve implements Resolver for heo's own references, scoped to the
// profile's tenant.
func (b *Backend) Resolve(ctx context.Context, req Request) (ports.Credential, error) {
	return b.resolve(ctx, b.portsRequest(req))
}

func (b *Backend) portsRequest(req Request) ports.CredentialRequest {
	return ports.CredentialRequest{
		Channel:   model.ChannelRef{Tenant: b.tenant},
		Reference: req.Reference,
		Purpose:   req.Purpose,
		Accept:    req.Accept,
		Fresh:     req.Fresh,
	}
}

func (b *Backend) resolve(ctx context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	keys, err := b.keysFor(req)
	if err != nil {
		return nil, err
	}
	cred, gs, err := b.readEntry(ctx, req, keys)
	if err != nil {
		var ge *grantError
		if errors.As(err, &ge) {
			return nil, ge.me
		}
		return nil, err
	}
	at := slot{req.Reference, req.Purpose}
	fp := b.fingerprint(cred)
	if gs != nil && req.Fresh {
		// The provider rejected the token last served. A minted
		// token is dropped once, so the next one is a new one; a
		// token this process already replaced is not dropped again.
		// Callers asking together share the new token: the one
		// served last is only refused when the drop gave it back.
		b.mu.Lock()
		last, ok := b.served[at]
		b.mu.Unlock()
		if ok && hmac.Equal(last[:], fp[:]) {
			gs.src.Invalidate()
			zero(cred)
			var gerr *grantError
			if cred, gerr = b.grantToken(ctx, gs); gerr != nil {
				return nil, gerr.me
			}
			fp = b.fingerprint(cred)
			if hmac.Equal(last[:], fp[:]) {
				b.mu.Lock()
				b.rejected[at] = rejection{fp: fp, at: b.now()}
				b.mu.Unlock()
				return nil, model.NewError(model.Unauthorized, "credential was rejected by the provider and has not been rotated")
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if last, ok := b.served[at]; req.Fresh && gs == nil && ok && hmac.Equal(last[:], fp[:]) {
		b.rejected[at] = rejection{fp: fp, at: b.now()}
		zero(cred)
		return nil, model.NewError(model.Unauthorized, "credential was rejected by the provider and has not been rotated")
	}
	b.served[at] = fp
	if r, ok := b.rejected[at]; ok && !hmac.Equal(r.fp[:], fp[:]) {
		delete(b.rejected, at)
	}
	return cred, nil
}

// readEntry resolves the entry stored under the first of keys holding
// one: an OAuth grant mints an access token (the returned source
// drops it again); anything else decodes as a credential envelope. A
// failure of a grant is a *grantError.
func (b *Backend) readEntry(ctx context.Context, req ports.CredentialRequest, keys []string) (ports.Credential, *grantSource, error) {
	p := b.peekGrant(ctx, keys)
	switch {
	case p.isGrant && p.gerr != nil:
		return nil, nil, p.gerr
	case p.isGrant:
		cred, gs, gerr := b.readGrant(ctx, req, p.key, p.grant)
		if gerr != nil {
			return nil, nil, gerr
		}
		return cred, gs, nil
	case p.raw != nil:
		// Decode what was just read, not the store again.
		cred, err := b.decode(ctx, req, heldStore{Store: b.store, key: p.key, value: p.raw})
		return cred, nil, err
	}
	cred, err := b.decode(ctx, req, fallbackStore{Store: b.store, keys: keys})
	return cred, nil, err
}

// decode reads the one credential envelope store holds for req.
func (b *Backend) decode(ctx context.Context, req ports.CredentialRequest, store kitsecret.Store) (ports.Credential, error) {
	creds, err := umasecret.New(store,
		umasecret.WithKeyFunc(func(ports.CredentialRequest) (string, error) { return "-", nil }))
	if err != nil {
		return nil, model.NewError(model.Unavailable, "credential store unavailable")
	}
	return creds.Resolve(ctx, req)
}

// Key returns the store key of a reference's shared entry, the one
// every purpose without its own entry resolves: uma/<tenant>/
// <reference> (segments path-escaped) in heo's kit store, the
// reference itself in an aps profile's secrets file.
func (b *Backend) Key(reference string) (string, error) {
	keys, err := b.Keys(reference, "")
	if err != nil {
		return "", err
	}
	return keys[0], nil
}

// Keys returns the store keys a reference resolves for purpose, in the
// order tried: the purpose's own entry, then the shared entry (see
// Key). An empty purpose has the shared entry only.
func (b *Backend) Keys(reference, purpose string) ([]string, error) {
	return b.keysFor(b.portsRequest(Request{Reference: reference, Purpose: purpose}))
}

var (
	// refRe bounds a reference into a kit store: '/'-separated
	// segments that start with a letter, digit or '_', so neither "."
	// nor ".." nor an empty segment can occur.
	refRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+-]*(/[A-Za-z0-9_][A-Za-z0-9._@+-]*)*$`)
	// apsRefRe is an aps secret name: an environment variable name.
	apsRefRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

const maxReference = 200

// keysFor returns the store keys of req in resolution order: the
// purpose's own entry (when req names a purpose), then the shared one.
func (b *Backend) keysFor(req ports.CredentialRequest) ([]string, error) {
	if req.Channel.Tenant != b.tenant {
		return nil, model.NewError(model.Unauthorized, "credential request for another tenant")
	}
	r, p := req.Reference, req.Purpose
	if p != "" {
		if err := validPurpose(p); err != nil {
			return nil, err
		}
	}
	if b.name == BackendAps {
		if len(r) > maxReference || !apsRefRe.MatchString(r) {
			return nil, model.NewError(model.Validation, "credential reference is not an aps secret name (letters, digits and '_', not starting with a digit)")
		}
		if p == "" {
			return []string{r}, nil
		}
		return []string{apsPurposeName(r, p), r}, nil
	}
	if len(r) > maxReference || !refRe.MatchString(r) {
		return nil, model.NewError(model.Validation, "credential reference is not valid ('/'-separated segments of letters, digits and ._@+-, each starting with a letter, digit or '_')")
	}
	base, err := umasecret.DefaultKey(req)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return []string{base}, nil
	}
	return []string{kitPurposeKey(base, p), base}, nil
}

// fingerprint is a keyed hash of a credential's material: equal for
// the same credential within this process, useless outside it.
func (b *Backend) fingerprint(c ports.Credential) [sha256.Size]byte {
	h := hmac.New(sha256.New, b.fpKey)
	part := func(p []byte) {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write(p)
	}
	str := func(s string) {
		p := []byte(s)
		part(p)
		clear(p)
	}
	str(string(c.Kind()))
	switch c := c.(type) {
	case ports.Secret:
		part(c.Bytes)
	case ports.Password:
		str(c.User)
		part(c.Secret)
		str(c.AuthzID)
	case ports.OAuth2:
		str(c.User)
		str(c.AccessToken)
		str(c.AuthzID)
	case ports.ClientCert:
		for _, der := range c.Certificate.Certificate {
			part(der)
		}
		str(c.AuthzID)
	case ports.Kerberos:
		str(c.Principal)
		str(c.Realm)
		part(c.Keytab)
		part(c.CCache)
		str(c.AuthzID)
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// zero clears the secret bytes of a credential that is not returned.
func zero(c ports.Credential) {
	switch c := c.(type) {
	case ports.Secret:
		clear(c.Bytes)
	case ports.Password:
		clear(c.Secret)
	case ports.Kerberos:
		clear(c.Keytab)
		clear(c.CCache)
	}
}

// storeProblem classifies a store failure without passing its text on.
func storeProblem(err error) (Status, string) {
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return StatusMissing, "no credential stored"
	case errors.As(err, new(*earlierLayout)):
		return StatusMalformed, err.Error()
	case errors.Is(err, errApsMalformed):
		return StatusMalformed, errApsMalformed.Error()
	case errors.Is(err, errApsUnreadable):
		return StatusUnavailable, errApsUnreadable.Error()
	case errors.Is(err, kitsecret.ErrNotSupported):
		return StatusUnsupported, "credential store cannot read secrets"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return StatusUnavailable, "credential check interrupted"
	}
	return StatusUnavailable, "credential store unavailable"
}

// where names the place a credential goes: one of keys, the first
// for a purpose's own entry, the last for the entry every purpose
// shares.
func (b *Backend) where(keys []string) string {
	if b.name == BackendAps {
		return fmt.Sprintf("%s (aps profile %s) as %s", b.apsPath, b.ref.ID, oneOf(keys))
	}
	if fs, ok := b.store.(fileStore); ok {
		files := make([]string, len(keys))
		for i, k := range keys {
			files[i] = fs.path(k)
		}
		return "the file " + oneOf(files)
	}
	return fmt.Sprintf("the %s store under key %s", b.name, oneOf(keys))
}

func oneOf(keys []string) string {
	if len(keys) == 2 {
		return keys[0] + " (this purpose only) or " + keys[1] + " (every purpose)"
	}
	return strings.Join(keys, ", ")
}
