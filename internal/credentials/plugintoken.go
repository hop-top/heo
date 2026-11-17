package credentials

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"hop.top/heo/internal/profile"
	"hop.top/kit/go/storage/secret"
	"hop.top/uma/model"
)

// A plugin's token is the bearer credential an HTTP surface or the CLI
// accepts for the actor plugin:<name>, and for no other. It is one per
// plugin, minted by `heo plugin token mint <name>` into the same kit
// store as the profile's surface token, under PluginTokenKey, in the
// same record format (surfacetoken.go) with the subject plugin:<name>.
//
// The token a plugin presents is "<name>.<secret>": the name selects
// the record, the secret is checked against it, and the record's
// subject must be the actor that name makes. base64url has no '.', so
// no surface token can be read as a plugin's.

// PluginActorPrefix starts the actor of every plugin.
const PluginActorPrefix = "plugin:"

var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidPluginName checks a plugin's name: a lowercase letter, then
// lowercase letters, digits and '-', at most 63 in all.
func ValidPluginName(name string) error {
	if !pluginNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a plugin name: a lowercase letter, then lowercase letters, digits and '-', at most 63 in all", name)
	}
	return nil
}

// PluginActor is the actor of the plugin called name.
func PluginActor(name string) model.ActorID { return model.ActorID(PluginActorPrefix + name) }

// PluginTokenKey is the secret-store key of a plugin's token:
// heo/<pdir>/plugin-token/<name>.
func PluginTokenKey(ref profile.Ref, name string) string {
	return "heo/" + ref.Dir() + "/plugin-token/" + name
}

// MintPluginToken stores a new token for the plugin called name,
// replacing any earlier one, and returns what the plugin presents. The
// replaced token stops verifying at once.
func MintPluginToken(ctx context.Context, st secret.MutableStore, ref profile.Ref, name string, now time.Time) (string, error) {
	if err := ValidPluginName(name); err != nil {
		return "", err
	}
	tok, err := MintSurfaceToken(ctx, st, PluginTokenKey(ref, name), PluginActor(name), now)
	if err != nil {
		return "", err
	}
	return name + "." + tok, nil
}

// RevokePluginToken deletes the token of the plugin called name. A
// plugin with no token is revoked already.
func RevokePluginToken(ctx context.Context, st secret.MutableStore, ref profile.Ref, name string) error {
	if err := ValidPluginName(name); err != nil {
		return err
	}
	if err := st.Delete(ctx, PluginTokenKey(ref, name)); err != nil && !errors.Is(err, secret.ErrNotFound) {
		return fmt.Errorf("revoke the plugin token under %s: the credential backend refused the delete", PluginTokenKey(ref, name))
	}
	return nil
}

// LookupPluginToken returns what the plugin called name presents, its
// token as `heo plugin token mint` printed it, and whether one is
// stored. Only the daemon asks, to hand a plugin process its own token.
func LookupPluginToken(ctx context.Context, st secret.Store, ref profile.Ref, name string) (string, bool, error) {
	if err := ValidPluginName(name); err != nil {
		return "", false, err
	}
	rec, err := readSurfaceToken(ctx, st, PluginTokenKey(ref, name))
	switch {
	case err == nil:
		return name + "." + rec.Token, true, nil
	case errors.Is(err, secret.ErrNotFound):
		return "", false, nil
	}
	return "", false, err
}

// VerifyPluginToken checks presented, "<name>.<secret>", against the
// token stored for that plugin, reading the store on every call so a
// revocation takes effect on the next request. The subject it returns
// is always plugin:<name>. Anything else, a revoked token included, is
// ErrSurfaceTokenRefused.
func VerifyPluginToken(ctx context.Context, st secret.Store, ref profile.Ref, presented string) (SurfaceToken, error) {
	name, sec, ok := strings.Cut(presented, ".")
	if !ok || ValidPluginName(name) != nil {
		return SurfaceToken{}, ErrSurfaceTokenRefused
	}
	tok, err := VerifySurfaceToken(ctx, st, PluginTokenKey(ref, name), sec)
	if err != nil {
		return SurfaceToken{}, err
	}
	if tok.Subject != PluginActor(name) {
		return SurfaceToken{}, ErrSurfaceTokenRefused
	}
	return tok, nil
}

// IsPluginToken reports whether presented has a plugin token's shape
// rather than a surface token's.
func IsPluginToken(presented string) bool { return strings.Contains(presented, ".") }
