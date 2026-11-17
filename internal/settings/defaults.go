package settings

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/trustcfg"
	"hop.top/kit/go/core/xdg"
)

// Key names heo reads itself.
const (
	// KeyProfile is the synthetic key `config list` and `config get`
	// report the selected profile under. It is never read from a file.
	KeyProfile = "profile"
	// KeyTenant is the uma tenant the profile's daemon acts for, set
	// server-side and never taken from a request.
	KeyTenant = "runtime.tenant"
	// KeySocketPath is the control socket path kit's socket service
	// binds.
	KeySocketPath = "services.socket.path"
	// KeyStorageDriver selects the storage driver; its settings live
	// under storage.<driver>.
	KeyStorageDriver = "storage.driver"
	// KeyStorageBackupDir is where storage backups are written.
	KeyStorageBackupDir = "storage.backup.dir"
	// KeyStorageBlobsDir is where attachment bytes are kept.
	KeyStorageBlobsDir = "storage.blobs.dir"
	// KeyRetentionOperations is how long uma keeps an operation, and
	// with it the operation key that finds it, after its last update.
	KeyRetentionOperations = "runtime.retention.operations"
)

// Keys of the hold point: whether a plugin actor's write is held for a
// human decision, how long it waits, and the actor the plugins act for.
const (
	KeyHeldEnabled    = "held_operations.enabled"
	KeyHeldExpiry     = "held_operations.expiry"
	KeyHeldOnBehalfOf = "held_operations.on_behalf_of"
	// KeyHeldTrustEnabled replaces holding every plugin write with the
	// trust ladder; the ladder's other settings live under
	// held_operations.trust (internal/trustcfg).
	KeyHeldTrustEnabled    = "held_operations.trust.enabled"
	KeyHeldTrustUnattended = "held_operations.trust.unattended"
	KeyHeldTrustSupervised = "held_operations.trust.supervised.enabled"
	KeyHeldTrustSupDays    = "held_operations.trust.supervised.days"
)

// DefaultHeldExpiry is KeyHeldExpiry unless configured otherwise: 72
// hours.
const DefaultHeldExpiry = 72 * time.Hour

// DefaultRetentionOperations is KeyRetentionOperations unless
// configured otherwise: 30 days.
const DefaultRetentionOperations = 720 * time.Hour

// DefaultStorageDriver is the driver a profile runs on unless
// configured otherwise.
const DefaultStorageDriver = "postgres"

// BackupDir returns the profile's default backup directory,
// <data dir>/heo/<pdir>/backups. It only builds the path.
func BackupDir(ref profile.Ref) (string, error) {
	dir, err := xdg.RawDataDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	return filepath.Join(dir, ref.Dir(), "backups"), nil
}

// BlobsDir returns the profile's default attachment directory,
// <data dir>/heo/<pdir>/blobs. It only builds the path.
func BlobsDir(ref profile.Ref) (string, error) {
	dir, err := xdg.RawDataDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	return filepath.Join(dir, ref.Dir(), "blobs"), nil
}

// PluginsDir returns the directory holding the plugins' working
// directories, <state dir>/heo/<pdir>/plugins; each plugin's is
// <name> under it. It only builds the path.
func PluginsDir(ref profile.Ref) (string, error) {
	dir, err := xdg.RawStateDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(dir, ref.Dir(), "plugins"), nil
}

// SocketPath returns the profile's default control socket path,
// <runtime dir>/heo/<pdir>/heo.sock. It only builds the path; nothing
// is created.
func SocketPath(ref profile.Ref) (string, error) {
	dir, err := xdg.RuntimeDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve runtime dir: %w", err)
	}
	return filepath.Join(dir, ref.Dir(), "heo.sock"), nil
}

// PIDPath returns the profile's daemon pidfile,
// <runtime dir>/heo/<pdir>/heo.pid. It only builds the path.
func PIDPath(ref profile.Ref) (string, error) {
	dir, err := xdg.RuntimeDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve runtime dir: %w", err)
	}
	return filepath.Join(dir, ref.Dir(), "heo.pid"), nil
}

// ControlDir creates heo's runtime directory, <runtime dir>/heo,
// owner-only, and returns it. Kit puts each profile's control socket
// there, control-<pdir>.sock, beside the profile directories; kit
// resolves the runtime directory only when it exists and falls back to
// the system temp dir otherwise, so the daemon and every client must
// find it present to agree on the path.
func ControlDir() (string, error) {
	dir, err := xdg.RuntimeDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve runtime dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create runtime dir: %w", err)
	}
	return dir, nil
}

// AccountClaimsDir returns where daemons of every profile claim
// provider accounts, <runtime dir>/heo/_accounts. No profile directory
// can take the name: profile names start with a letter.
func AccountClaimsDir() (string, error) {
	dir, err := xdg.RuntimeDir(Tool)
	if err != nil {
		return "", fmt.Errorf("resolve runtime dir: %w", err)
	}
	return filepath.Join(dir, "_accounts"), nil
}

// Defaults returns heo's built-in values for ref, flat dotted keys to
// values. They sit below every config file, environment variable and
// -c override; a key set under one service (services.api.rate_limit.
// enabled) also wins over the shared services.all one.
func Defaults(ref profile.Ref) (map[string]any, error) {
	socket, err := SocketPath(ref)
	if err != nil {
		return nil, err
	}
	backups, err := BackupDir(ref)
	if err != nil {
		return nil, err
	}
	blobs, err := BlobsDir(ref)
	if err != nil {
		return nil, err
	}
	d := map[string]any{
		// The control socket is the only surface on by default: an
		// owner-only Unix socket that admits same-uid peers, verified by
		// the kernel. Each profile has its own.
		"services.socket.enabled":                    true,
		"services.socket.auth.mode":                  "peer",
		"services.socket.auth.peer.require_same_uid": true,
		KeySocketPath:                                socket,
		// REST, gRPC and MCP over HTTP stay off until enabled with an
		// authenticator.
		"services.api.enabled": false,
		"services.rpc.enabled": false,
		"services.mcp.enabled": false,
		// uma's operation ledger is the only replay owner: kit's served
		// replay layer stays off on every surface.
		"services.all.idempotency.enabled": false,
		// Refuse request bodies over the cap with 413 body_too_large:
		// 1 MiB on the api and mcp listeners, 4 MiB on rpc.
		"services.all.body_limit.enabled": true,
		// Give a client 5s to send its request and 10s to read a
		// request/reply answer; stream responses are exempt.
		"services.all.timeouts.read_header": "5s",
		"services.all.timeouts.read":        "5s",
		"services.all.timeouts.write":       "10s",
		// Cancel a served command after 5 minutes unless it declares
		// its own kit/timeout; the caller gets deadline_exceeded.
		"services.all.timeouts.command": "5m",
		// Rate-limit each caller per side-effect tier, on loopback too;
		// the excess gets 429 rate_limited with Retry-After.
		"services.all.rate_limit.enabled": true,
		// Answer /healthz and /readyz without credentials.
		"services.all.health.enabled": true,
		// One tenant per daemon, named after the profile directory.
		KeyTenant: ref.Dir(),
		// Each profile owns one data store; heo backs it up under the
		// profile's data directory before every migration.
		KeyStorageDriver:    DefaultStorageDriver,
		KeyStorageBackupDir: backups,
		// Attachment bytes live beside the store, owner-only.
		KeyStorageBlobsDir: blobs,
		// An operation key finds its operation for 30 days after the
		// operation's last update.
		KeyRetentionOperations: DefaultRetentionOperations.String(),
		// Plugin writes are applied as they come until held operations
		// are switched on; then each waits 72 hours for a decision.
		KeyHeldEnabled:    false,
		KeyHeldExpiry:     DefaultHeldExpiry.String(),
		KeyHeldOnBehalfOf: "",
		// The ladder is opt-in, runs as unattended, and the supervised
		// first week is off.
		KeyHeldTrustEnabled:    false,
		KeyHeldTrustUnattended: true,
		KeyHeldTrustSupervised: false,
		KeyHeldTrustSupDays:    trustcfg.DefaultSupervisedDays,
	}
	// Credentials resolve through the OS keyring unless configured
	// otherwise; an aps-bound profile resolves its credential
	// references through the aps profile's secrets instead.
	cred, err := credentials.Defaults(ref)
	if err != nil {
		return nil, err
	}
	for k, v := range cred {
		d[k] = v
	}
	return d, nil
}
