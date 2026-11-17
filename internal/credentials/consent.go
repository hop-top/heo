package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/uma/model"
)

// The first consent of a user-consented mailbox (internal/consent) ends
// here: the refresh token it obtained is kept as the grant of the
// channel's credential reference, where the backend already mints and
// refreshes access tokens from it. These methods inspect what the
// reference holds before anything is written, and write the two entries
// a refresh_token grant is made of, the secret before the grant that
// names it.

// Entry is what a credential reference's shared entry holds now.
type Entry struct {
	// Key is where it lives.
	Key string
	// Exists reports whether the store holds anything there.
	Exists bool
	// Kind is the stored envelope's kind: a grant kind
	// (refresh_token, client_credentials, ...) or a credential kind
	// (secret, password, oauth2, ...); empty when it is not an envelope
	// heo can read.
	Kind string
	// Grant is the OAuth grant stored there, when it is one.
	Grant *oauth.Grant
}

// Entry reads the shared entry of reference. It never returns the
// stored value, only its kind and, for a grant, the grant: a grant names
// secrets by key and holds none.
func (b *Backend) Entry(ctx context.Context, reference string) (Entry, error) {
	key, err := b.Key(reference)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Key: key}
	sec, err := b.store.Get(ctx, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return e, nil
	case err != nil || sec == nil:
		return Entry{}, model.NewError(model.Unavailable, "credential store unavailable")
	}
	defer clear(sec.Value)
	e.Exists = true
	var head struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(sec.Value, &head) == nil {
		e.Kind = head.Kind
	}
	if _, isGrant := grantKindOf(sec.Value); isGrant {
		if g, err := oauth.DecodeGrant(sec.Value); err == nil {
			e.Grant = &g
		}
	}
	return e, nil
}

// CanKeepGrant is nil when the profile's backend can hold a
// refresh_token grant: one a provider may rotate on any use needs a
// store that can be written. Otherwise it is model.Unsupported, naming
// the backends that can. Nothing is read or written.
func (b *Backend) CanKeepGrant() error {
	if b.writable() {
		return nil
	}
	return model.NewError(model.Unsupported, b.notWritableMessage())
}

// SecretAt reads the secret a grant names by key: the value of the uma
// "secret" envelope stored there. The key must be one heo's own
// references map to (uma/<tenant>/...), so a grant cannot name another
// entry of the store; its absence is model.NotFound. The caller clears
// the value.
func (b *Backend) SecretAt(ctx context.Context, key string) ([]byte, error) {
	if b.name != BackendAps && !strings.HasPrefix(key, "uma/"+string(b.tenant)+"/") {
		return nil, model.NewError(model.Validation, "store key is not a credential reference of this profile's tenant")
	}
	sec, err := b.store.Get(ctx, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return nil, model.NewError(model.NotFound, "no secret stored there")
	case err != nil || sec == nil:
		return nil, model.NewError(model.Unavailable, "credential store unavailable")
	}
	defer clear(sec.Value)
	var env struct {
		V     int    `json:"v"`
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if json.Unmarshal(sec.Value, &env) != nil || env.Kind != "secret" || env.Value == "" {
		return nil, model.NewError(model.Validation, `the entry is not a uma "secret" envelope with a value`)
	}
	return []byte(env.Value), nil
}

// GrantStore is a refresh_token grant to keep for a credential
// reference.
type GrantStore struct {
	// Reference is the channel's credential reference; the grant is its
	// shared entry.
	Reference string
	// Grant is the validated refresh_token grant; its SecretRef is the
	// store key RefreshSecret is written under.
	Grant oauth.Grant
	// RefreshSecret is the uma "secret" envelope holding the refresh
	// token.
	RefreshSecret []byte
	// Replace allows overwriting a shared entry that is not a
	// refresh_token grant (a static token, a password). A refresh_token
	// grant is always replaced: that is a new consent.
	Replace bool
	// Purposes are the purposes the channel's adapter resolves; one with
	// an entry of its own keeps it and does not use the grant.
	Purposes []string
}

// GrantStored says what a store did.
type GrantStored struct {
	// GrantKey and RefreshKey are the store keys written.
	GrantKey, RefreshKey string
	// Replaced is the kind of entry the grant replaced, empty for none.
	Replaced string
	// Shadowed are the purposes whose own entry takes precedence over
	// the grant.
	Shadowed []string
}

// StoreRefreshGrant keeps a refresh_token grant and its refresh token for
// a reference. It refuses a backend that cannot be written
// (model.Unsupported), a grant that is not a valid refresh_token grant or
// names the entry it would overwrite (model.Validation), and a shared
// entry that holds something else unless Replace is set (model.Conflict):
// a credential is never destroyed unseen. The refresh token is written
// first, so a grant never names a secret that is not there.
func (b *Backend) StoreRefreshGrant(ctx context.Context, in GrantStore) (GrantStored, error) {
	if err := b.CanKeepGrant(); err != nil {
		return GrantStored{}, err
	}
	if in.Grant.Kind != oauth.RefreshToken {
		return GrantStored{}, model.NewError(model.Validation, "only a refresh_token grant is kept by a consent")
	}
	if _, err := oauth.EncodeGrant(in.Grant); err != nil {
		return GrantStored{}, model.NewError(model.Validation, "the grant is not valid: "+strings.TrimPrefix(err.Error(), "oauth: invalid envelope: "))
	}
	if len(in.RefreshSecret) == 0 {
		return GrantStored{}, model.NewError(model.Validation, "no refresh token to keep")
	}
	cur, err := b.Entry(ctx, in.Reference)
	if err != nil {
		return GrantStored{}, err
	}
	own := "uma/" + string(b.tenant) + "/"
	for _, k := range []string{in.Grant.SecretRef, in.Grant.ClientSecretRef} {
		if k != "" && !strings.HasPrefix(k, own) {
			return GrantStored{}, model.NewError(model.Validation, "a grant names secrets by the store keys of this profile's credential references only")
		}
	}
	if in.Grant.SecretRef == cur.Key || in.Grant.ClientSecretRef == cur.Key {
		return GrantStored{}, model.NewError(model.Validation, "the grant names its own entry as a secret")
	}
	out := GrantStored{GrantKey: cur.Key, RefreshKey: in.Grant.SecretRef}
	switch {
	case !cur.Exists:
	case cur.Grant != nil && cur.Grant.Kind == oauth.RefreshToken:
		out.Replaced = string(oauth.RefreshToken)
	case in.Replace:
		out.Replaced = orUnreadable(cur.Kind)
	default:
		return GrantStored{}, model.NewError(model.Conflict, fmt.Sprintf(
			"reference %s already holds a %s credential: it would be lost; remove it or pass --replace",
			in.Reference, orUnreadable(cur.Kind)))
	}
	for _, p := range in.Purposes {
		keys, err := b.Keys(in.Reference, p)
		if err != nil || len(keys) < 2 {
			continue
		}
		if sec, err := b.store.Get(ctx, keys[0]); err == nil && sec != nil {
			clear(sec.Value)
			out.Shadowed = append(out.Shadowed, p)
		}
	}
	slices.Sort(out.Shadowed)
	grant, err := oauth.EncodeGrant(in.Grant)
	if err != nil {
		return GrantStored{}, model.NewError(model.Validation, "the grant is not valid")
	}
	if err := b.kit.Set(ctx, out.RefreshKey, in.RefreshSecret); err != nil {
		return GrantStored{}, model.NewError(model.Unavailable, "the refresh token could not be stored: credential store unavailable")
	}
	if err := b.kit.Set(ctx, out.GrantKey, grant); err != nil {
		return GrantStored{}, model.NewError(model.Unavailable, "the grant could not be stored: credential store unavailable")
	}
	b.mu.Lock()
	for s := range b.rejected {
		if s.reference == in.Reference {
			delete(b.rejected, s)
		}
	}
	b.mu.Unlock()
	return out, nil
}

func orUnreadable(kind string) string {
	if kind == "" {
		return "unreadable"
	}
	return kind
}
