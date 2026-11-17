package credentials

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"hop.top/heo/internal/profile"
	"hop.top/kit/go/core/xdg"
)

// Configuration keys, all under credentials.*. They name where
// credentials live, never a credential.
const (
	// KeyBackend selects the profile's kit secret store.
	KeyBackend = "credentials.backend"
	// KeyKeyringService is the OS keyring service the keyring backend
	// uses; openbao reads its token from it too.
	KeyKeyringService = "credentials.keyring.service"
	// KeyFileDir is the file backend's root directory.
	KeyFileDir = "credentials.file.dir"
	// KeyAgefilePath is the agefile backend's encrypted YAML file.
	KeyAgefilePath = "credentials.agefile.path"
	// KeyAgefileIdentity is the age identity file that decrypts it.
	KeyAgefileIdentity = "credentials.agefile.identity_file"
	// KeyOpenbaoAddr is the OpenBao server URL.
	KeyOpenbaoAddr = "credentials.openbao.addr"
	// KeyOpenbaoMount is the OpenBao KV v2 mount.
	KeyOpenbaoMount = "credentials.openbao.mount"
	// KeySurfaceTokenRef names the store key of the profile's surface
	// token, the bearer credential of REST, gRPC and MCP over HTTP
	// (see SurfaceTokenKey). `heo service setup` writes it; unset, no
	// HTTP surface authenticates with a surface token.
	KeySurfaceTokenRef = "credentials.surface_token_ref"
)

// Backend names.
const (
	// BackendKeyring is the OS keychain (macOS Keychain, Secret
	// Service, Windows Credential Manager). The default.
	BackendKeyring = "keyring"
	// BackendFile keeps one 0600 file per secret under a 0700
	// directory, unencrypted at rest.
	BackendFile = "file"
	// BackendAgefile reads an age-encrypted YAML file; read-only.
	BackendAgefile = "agefile"
	// BackendOpenbao is an OpenBao (Vault-compatible) KV v2 mount.
	BackendOpenbao = "openbao"
	// BackendAps is the aps profile's secrets. It is never configured:
	// an aps-bound profile (aps://profile/<id>) implies it for every
	// credential reference.
	BackendAps = "aps"
)

// DefaultKeyringService is the keyring service heo's secrets live in.
const DefaultKeyringService = "heo"

// Backends returns the configurable backends in sorted order. kit's
// env backend is not one of them, by design: a credential comes from
// the store the profile names, never from the inherited environment.
func Backends() []string {
	return []string{BackendAgefile, BackendFile, BackendKeyring, BackendOpenbao}
}

// Keys returns every configuration key this package reads.
func Keys() []string {
	return []string{
		KeyAgefileIdentity, KeyAgefilePath, KeyBackend, KeyFileDir,
		KeyKeyringService, KeyOpenbaoAddr, KeyOpenbaoMount, KeySurfaceTokenRef,
	}
}

// Config selects and configures the profile's kit secret store.
type Config struct {
	Backend         string
	KeyringService  string
	FileDir         string
	AgefilePath     string
	AgefileIdentity string
	OpenbaoAddr     string
	OpenbaoMount    string
	SurfaceTokenRef string
}

// ConfigFrom reads Config through get, typically viper's GetString.
func ConfigFrom(get func(key string) string) Config {
	return Config{
		Backend:         get(KeyBackend),
		KeyringService:  get(KeyKeyringService),
		FileDir:         get(KeyFileDir),
		AgefilePath:     get(KeyAgefilePath),
		AgefileIdentity: get(KeyAgefileIdentity),
		OpenbaoAddr:     get(KeyOpenbaoAddr),
		OpenbaoMount:    get(KeyOpenbaoMount),
		SurfaceTokenRef: get(KeySurfaceTokenRef),
	}
}

// Defaults returns the built-in credentials.* values for ref: the OS
// keyring, service "heo"; the file backend, when chosen, under
// <data>/heo/<pdir>/secrets.
func Defaults(ref profile.Ref) (map[string]any, error) {
	data, err := xdg.RawDataDir("heo")
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	return map[string]any{
		KeyBackend:        BackendKeyring,
		KeyKeyringService: DefaultKeyringService,
		KeyFileDir:        filepath.Join(data, ref.Dir(), "secrets"),
		KeyOpenbaoMount:   "secret",
	}, nil
}

// Problem is one finding of Validate.
type Problem struct {
	Key     string
	Warning bool
	Message string
}

// Validate checks c offline: nothing is opened or contacted.
func (c Config) Validate() []Problem {
	var out []Problem
	fail := func(key, msg string) { out = append(out, Problem{Key: key, Message: msg}) }
	warn := func(key, msg string) { out = append(out, Problem{Key: key, Warning: true, Message: msg}) }
	need := func(key, val, what string) bool {
		if strings.TrimSpace(val) == "" {
			fail(key, fmt.Sprintf("%s backend needs %s (%s)", c.Backend, what, key))
			return false
		}
		return true
	}

	switch c.Backend {
	case BackendKeyring:
		need(KeyKeyringService, c.KeyringService, "a keyring service")
	case BackendFile:
		if need(KeyFileDir, c.FileDir, "a directory") && !filepath.IsAbs(c.FileDir) {
			fail(KeyFileDir, fmt.Sprintf("%q is not an absolute path", c.FileDir))
		}
		warn(KeyBackend, "file: credentials are stored unencrypted at rest (one 0600 file each)")
	case BackendAgefile:
		need(KeyAgefilePath, c.AgefilePath, "the encrypted file")
		need(KeyAgefileIdentity, c.AgefileIdentity, "an age identity file")
		warn(KeyBackend, "agefile: read-only; heo cannot store its surface token there")
	case BackendOpenbao:
		need(KeyKeyringService, c.KeyringService, "a keyring service holding its token")
		if need(KeyOpenbaoAddr, c.OpenbaoAddr, "a server address") {
			if msg := checkAddr(c.OpenbaoAddr); msg != "" {
				fail(KeyOpenbaoAddr, msg)
			}
		}
		need(KeyOpenbaoMount, c.OpenbaoMount, "a KV v2 mount")
	default:
		fail(KeyBackend, unsupportedMessage(c.Backend))
	}
	if ref := c.SurfaceTokenRef; ref != "" {
		if err := ValidSurfaceTokenRef(ref); err != nil {
			fail(KeySurfaceTokenRef, err.Error())
		}
	}
	return out
}

// checkAddr validates an OpenBao address without echoing it: a URL may
// carry a password in its userinfo.
func checkAddr(addr string) string {
	u, err := url.Parse(addr)
	switch {
	case err != nil:
		return "not a valid URL"
	case u.User != nil:
		return "the address must not carry credentials; the token is read from the OS keyring"
	case u.Scheme != "http" && u.Scheme != "https":
		return "the address must be an http or https URL"
	case u.Host == "":
		return "the address has no host"
	}
	return ""
}

func unsupportedMessage(name string) string {
	msg := fmt.Sprintf("unsupported credential backend %q; supported: %s", name, strings.Join(Backends(), ", "))
	if strings.EqualFold(name, BackendAps) {
		msg += " (aps secrets are implied by an aps-bound profile, --profile aps:<id>)"
	}
	return msg
}
