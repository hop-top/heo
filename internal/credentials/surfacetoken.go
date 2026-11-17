package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/uma/model"
)

// The profile's surface token is the bearer credential REST, gRPC and
// MCP over HTTP accept: one per profile, shared by the three, minted by
// `heo service setup` into heo's own kit store (Backend.SurfaceTokens)
// under SurfaceTokenKey, and named in configuration by
// KeySurfaceTokenRef. The store holds a record, never configuration:
//
//	{"v":1,"token":"<base64url of 32 random bytes>","subject":"uid:<n>","minted_at":"<RFC 3339>"}
//
// subject is the actor a request bearing the token acts as: the uid of
// the user who minted it, the same actor the control socket names that
// user, so authorization grants and audit see one identity on every
// surface.

// surfaceTokenBytes is the token's entropy.
const surfaceTokenBytes = 32

// surfaceTokenVersion is the record's format.
const surfaceTokenVersion = 1

// ErrSurfaceTokenRefused is every refusal of a presented token: none
// presented, none stored, or a different one. It never says which.
var ErrSurfaceTokenRefused = errors.New("bearer token not accepted")

// errSurfaceTokenMalformed is a stored record heo cannot read. Fixed
// text: the record holds the token.
var errSurfaceTokenMalformed = errors.New("the stored surface token record is not readable; rotate it with `heo service setup --rotate-token`")

// SurfaceToken is what a stored token says about itself, without the
// token.
type SurfaceToken struct {
	// Subject is the actor a bearer of the token acts as.
	Subject model.ActorID
	// MintedAt is when the token was minted.
	MintedAt time.Time
}

type surfaceTokenRecord struct {
	V        int       `json:"v"`
	Token    string    `json:"token"`
	Subject  string    `json:"subject"`
	MintedAt time.Time `json:"minted_at"`
}

// surfaceRefRe bounds a store key: '/'-separated segments of the
// characters a credential reference allows.
var surfaceRefRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+-]*(/[A-Za-z0-9_][A-Za-z0-9._@+-]*)*$`)

// ValidSurfaceTokenRef checks a KeySurfaceTokenRef value: a store key
// of '/'-separated segments, none empty, "." or "..".
func ValidSurfaceTokenRef(ref string) error {
	if len(ref) > 256 || !surfaceRefRe.MatchString(ref) {
		return errors.New("not a secret-store key ('/'-separated segments of letters, digits and ._@+-)")
	}
	for _, seg := range strings.Split(ref, "/") {
		if seg == "." || seg == ".." {
			return errors.New(`not a secret-store key: no segment may be "." or ".."`)
		}
	}
	return nil
}

// MintSurfaceToken stores a new token for subject under key, replacing
// any earlier one, and returns it. A replaced token stops verifying at
// once.
func MintSurfaceToken(ctx context.Context, st kitsecret.MutableStore, key string, subject model.ActorID, now time.Time) (string, error) {
	if err := subject.Validate(); err != nil {
		return "", fmt.Errorf("surface token subject: %w", err)
	}
	raw := make([]byte, surfaceTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("surface token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	clear(raw)
	b, err := json.Marshal(surfaceTokenRecord{
		V: surfaceTokenVersion, Token: tok, Subject: string(subject), MintedAt: now.UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("surface token: %w", err)
	}
	if err := st.Set(ctx, key, b); err != nil {
		// The store's own error is dropped: it may quote the value.
		return "", fmt.Errorf("store the surface token under %s: the credential backend refused the write", key)
	}
	return tok, nil
}

// LookupSurfaceToken reports whether a token is stored under key, and
// what it says about itself.
func LookupSurfaceToken(ctx context.Context, st kitsecret.Store, key string) (SurfaceToken, bool, error) {
	rec, err := readSurfaceToken(ctx, st, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return SurfaceToken{}, false, nil
	case err != nil:
		return SurfaceToken{}, false, err
	}
	return SurfaceToken{Subject: model.ActorID(rec.Subject), MintedAt: rec.MintedAt}, true, nil
}

// RevealSurfaceToken returns the token stored under key. Only `service
// setup --show-token` asks for it.
func RevealSurfaceToken(ctx context.Context, st kitsecret.Store, key string) (string, error) {
	rec, err := readSurfaceToken(ctx, st, key)
	if err != nil {
		if errors.Is(err, kitsecret.ErrNotFound) {
			return "", fmt.Errorf("no surface token under %s: run `heo service setup`", key)
		}
		return "", err
	}
	return rec.Token, nil
}

// VerifySurfaceToken checks presented against the token stored under
// key, reading the store on every call so a rotation takes effect on
// the next request. Any mismatch, and no token stored, is
// ErrSurfaceTokenRefused; a store that cannot be read is its own error.
func VerifySurfaceToken(ctx context.Context, st kitsecret.Store, key, presented string) (SurfaceToken, error) {
	rec, err := readSurfaceToken(ctx, st, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return SurfaceToken{}, ErrSurfaceTokenRefused
	case err != nil:
		return SurfaceToken{}, err
	}
	// Compared as digests: equal length, so the comparison's time says
	// nothing about the token's length.
	want, got := sha256.Sum256([]byte(rec.Token)), sha256.Sum256([]byte(presented))
	if presented == "" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return SurfaceToken{}, ErrSurfaceTokenRefused
	}
	return SurfaceToken{Subject: model.ActorID(rec.Subject), MintedAt: rec.MintedAt}, nil
}

// readSurfaceToken reads and checks the record under key. Not found is
// kitsecret.ErrNotFound; any other store failure, and a record heo
// cannot read, has fixed text.
func readSurfaceToken(ctx context.Context, st kitsecret.Store, key string) (surfaceTokenRecord, error) {
	sec, err := st.Get(ctx, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return surfaceTokenRecord{}, kitsecret.ErrNotFound
	case err != nil:
		return surfaceTokenRecord{}, fmt.Errorf("read the surface token under %s: the credential backend is unavailable", key)
	case sec == nil:
		return surfaceTokenRecord{}, kitsecret.ErrNotFound
	}
	var rec surfaceTokenRecord
	if json.Unmarshal(sec.Value, &rec) != nil || rec.V != surfaceTokenVersion || rec.Token == "" ||
		model.ActorID(rec.Subject).Validate() != nil {
		return surfaceTokenRecord{}, errSurfaceTokenMalformed
	}
	return rec, nil
}
