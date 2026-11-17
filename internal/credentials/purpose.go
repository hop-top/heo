package credentials

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"

	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/uma/model"
)

// A credential reference resolves per purpose. Each purpose may have
// its own entry; a purpose without one falls back to the reference's
// shared entry, so one envelope can still serve every purpose while a
// purpose that needs a different secret (a webhook signing secret next
// to an API token) gets its own, rotated on its own:
//
//	kit store            uma/<tenant>/<ref>/<purpose>, then uma/<tenant>/<ref>
//	aps secrets file     <REF>__<PURPOSE>, then <REF>
//
// kit's file backend keeps a purpose key in a file beside the shared
// one, <ref>@<purpose> (see fileStore); the keys stay the kit store's.
//
// In a kit store the purpose is one more path-escaped key segment. In
// an aps secrets file it is a suffix keeping the name a valid
// environment variable: the purpose upper-cased, every '.', '-', '@'
// and '+' turned into '_', joined to the reference by "__".

// purposeRe bounds a purpose: one segment of the characters a
// reference segment allows, so it can neither be "." or ".." nor hold a
// '/' that would move it into another segment.
var purposeRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@+-]*$`)

const maxPurpose = 100

// apsPurposeSep joins a reference and its purpose suffix in an aps
// secrets file.
const apsPurposeSep = "__"

func validPurpose(p string) error {
	if len(p) > maxPurpose || !purposeRe.MatchString(p) {
		return model.NewError(model.Validation, "credential purpose is not valid (letters, digits and ._@+-, starting with a letter, digit or '_')")
	}
	return nil
}

// apsPurposeName is the aps secret name of reference's entry for
// purpose: <reference>__<PURPOSE>.
func apsPurposeName(reference, purpose string) string {
	suffix := strings.Map(func(r rune) rune {
		switch r {
		case '.', '-', '@', '+':
			return '_'
		}
		return r
	}, strings.ToUpper(purpose))
	return reference + apsPurposeSep + suffix
}

// kitPurposeKey is the kit store key of a reference's entry for
// purpose: its shared key plus the path-escaped purpose segment.
func kitPurposeKey(base, purpose string) string { return base + "/" + url.PathEscape(purpose) }

// fallbackStore reads the first of keys that holds a secret, whatever
// key it is asked for: a purpose's own entry, then the reference's
// shared one. Only "not found" falls through; any other failure is
// returned as is, never answered from the next key.
type fallbackStore struct {
	kitsecret.Store
	keys []string
}

func (s fallbackStore) Get(ctx context.Context, _ string) (*kitsecret.Secret, error) {
	for _, k := range s.keys {
		sec, err := s.Store.Get(ctx, k)
		if !errors.Is(err, kitsecret.ErrNotFound) {
			return sec, err
		}
	}
	return nil, kitsecret.ErrNotFound
}

// locate returns the first of keys stored in the store, or
// kitsecret.ErrNotFound when none is.
func locate(ctx context.Context, store kitsecret.Store, keys []string) (string, error) {
	for _, k := range keys {
		ok, err := store.Exists(ctx, k)
		if err != nil {
			return "", err
		}
		if ok {
			return k, nil
		}
	}
	return "", kitsecret.ErrNotFound
}

// heldStore answers a Get with a value already read, from key.
type heldStore struct {
	kitsecret.Store
	key   string
	value []byte
}

func (s heldStore) Get(context.Context, string) (*kitsecret.Secret, error) {
	return &kitsecret.Secret{Key: s.key, Value: s.value}, nil
}
