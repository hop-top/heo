package credentials

import (
	"context"
	"errors"
	"fmt"

	"hop.top/heo/internal/profile"
	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/agefile"
	"hop.top/kit/go/storage/secret/keyring"
	"hop.top/kit/go/storage/secret/openbao"
)

// newKeyring builds an OS keyring store. Tests replace it so that no
// test reaches a real keychain.
var newKeyring = func(service string) kitsecret.MutableStore { return keyring.New(service) }

// SurfaceTokenKey is the secret-store key of the profile's HTTP-surface
// bearer token: heo/<pdir>/surface-token.
func SurfaceTokenKey(ref profile.Ref) string { return "heo/" + ref.Dir() + "/surface-token" }

// OpenbaoTokenKey is the OS keyring key holding the profile's OpenBao
// token, in the configured keyring service: heo/<pdir>/openbao-token.
func OpenbaoTokenKey(ref profile.Ref) string { return "heo/" + ref.Dir() + "/openbao-token" }

// openKitStore constructs the configured kit backend directly: no kit
// registry lookup, so only the backends named here are linked.
func openKitStore(ctx context.Context, c Config, ref profile.Ref) (kitsecret.MutableStore, error) {
	switch c.Backend {
	case BackendKeyring:
		return newKeyring(c.KeyringService), nil
	case BackendFile:
		return newFileStore(c.FileDir), nil
	case BackendAgefile:
		return agefile.New(c.AgefilePath, c.AgefileIdentity), nil
	case BackendOpenbao:
		return openOpenbao(ctx, c, ref)
	}
	return nil, errors.New(unsupportedMessage(c.Backend))
}

// openOpenbao reads the OpenBao token, a bootstrap secret, from the OS
// keyring. Neither the token nor the backend's own errors reach the
// returned error.
func openOpenbao(ctx context.Context, c Config, ref profile.Ref) (kitsecret.MutableStore, error) {
	key := OpenbaoTokenKey(ref)
	tok, err := newKeyring(c.KeyringService).Get(ctx, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return nil, fmt.Errorf("openbao token not found: store it in the OS keyring, service %q, key %q", c.KeyringService, key)
	case err != nil:
		return nil, fmt.Errorf("openbao token: OS keyring service %q unavailable", c.KeyringService)
	case tok == nil || len(tok.Value) == 0:
		return nil, fmt.Errorf("openbao token in OS keyring service %q, key %q, is empty", c.KeyringService, key)
	}
	token := string(tok.Value)
	clear(tok.Value)
	s, err := openbao.New(c.OpenbaoAddr, token, c.OpenbaoMount)
	if err != nil {
		return nil, fmt.Errorf("openbao: cannot build a client for %s", KeyOpenbaoAddr)
	}
	return s, nil
}
