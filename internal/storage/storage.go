// Package storage is heo's storage driver contract: how a profile's data
// store is selected, reached, owned, versioned, backed up and migrated.
//
// The runtime setting storage.driver names a Driver in a Registry the
// composition root builds by explicit registration; nothing registers
// itself at init. A driver connects to its backing database (Conn), takes
// exclusive ownership of it (Owner, a lease no second daemon can take
// while the first holds it), and through that owner backs up, restores,
// migrates and opens the uma store.Store heo runs on.
//
// heo, not the driver, decides when a migration runs: Migrate refuses a
// schema newer than the binary's (no silent downgrade), takes a backup
// before changing an existing schema, and verifies the result. Open
// refuses any schema but the binary's own, so workers never start on an
// incompatible store.
//
// Every driver passes the shared conformance suite in storagetest.
package storage

import (
	"context"
	"time"

	"hop.top/heo/internal/credentials"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Settings reads one driver's settings: keys are relative to
// storage.<driver>, so "dsn" reads storage.postgres.dsn.
type Settings interface {
	GetString(key string) string
	GetInt(key string) int
}

// CredentialRequest asks for the credential behind one reference from
// configuration, for one purpose ("storage.<driver>") and one connection
// attempt. It is the profile credential backend's request, so the
// backend resolves storage references the way it resolves channels'.
type CredentialRequest = credentials.Request

// Credentials resolves a storage credential reference through the
// profile's credential backend. Errors are *model.Error and never carry
// material; the returned credential is used for one connection attempt
// and never logged.
type Credentials interface {
	Resolve(ctx context.Context, req CredentialRequest) (ports.Credential, error)
}

// The profile's credential resolver is a storage Credentials as is.
var _ Credentials = credentials.Resolver(nil)

// CredentialsFunc adapts a function to Credentials.
type CredentialsFunc func(ctx context.Context, req CredentialRequest) (ports.Credential, error)

// Resolve calls f.
func (f CredentialsFunc) Resolve(ctx context.Context, req CredentialRequest) (ports.Credential, error) {
	return f(ctx, req)
}

// Problem is one finding of a driver's offline settings check. Key is
// relative to storage.<driver>.
type Problem struct {
	Key     string
	Message string
	// Warning marks a weaker-than-default setting; otherwise the settings
	// are unusable.
	Warning bool
}

// Config is what a driver needs to reach one profile's data store.
type Config struct {
	// Settings are the driver's storage.<driver>.* settings.
	Settings Settings
	// Credentials resolves the credential references among Settings.
	// Nil means no reference may be set.
	Credentials Credentials
	// BackupDir is where Backup writes; created owner-only on first use.
	BackupDir string
	// Owner names this process in ownership diagnostics, for example
	// "heo/work": a refused second owner is told who holds the store.
	Owner string
}

// Driver is one storage backend heo can run on.
type Driver interface {
	// Name is the value of storage.driver that selects this driver.
	Name() string
	// Defaults are the driver's built-in settings, keys relative to
	// storage.<driver>.
	Defaults() map[string]any
	// Validate checks settings without side effects: no connection, no
	// credential lookup, no file written.
	Validate(Settings) []Problem
	// Connect reaches the backing database without reading or changing
	// its schema and without taking ownership.
	Connect(ctx context.Context, cfg Config) (Conn, error)
}

// Versions describes a connected store: what the database holds and
// what this binary's store package implements.
type Versions struct {
	// Driver is the driver's name.
	Driver string `json:"driver"     yaml:"driver"`
	// Server is the database server's version, as it reports it.
	Server string `json:"server"     yaml:"server"`
	// Library is the store package and its module version.
	Library string `json:"library"    yaml:"library"`
	// Schema is the schema version applied in the database; 0 means the
	// store was never initialized.
	Schema int64 `json:"schema"     yaml:"schema"`
	// LibrarySchema is the schema version this binary's store package
	// implements.
	LibrarySchema int64 `json:"library_schema" yaml:"library_schema"`
}

// Conn is a connection to one data store. Reading its versions needs no
// ownership; everything that writes goes through an Owner.
type Conn interface {
	// Versions reports the store's versions.
	Versions(ctx context.Context) (Versions, error)
	// Acquire takes exclusive ownership of the store. While an Owner
	// holds it, Acquire on any other connection, in any process, fails
	// with a model.Conflict error naming the holder where the backend
	// can tell.
	Acquire(ctx context.Context) (Owner, error)
	// Close releases any ownership still held and the connection. A
	// store.Store opened through it must not be used afterwards.
	Close() error
}

// Owner is held exclusive ownership of a data store.
type Owner interface {
	// Versions reports the store's versions.
	Versions(ctx context.Context) (Versions, error)
	// Verify fails when ownership was lost, for example because the
	// backend dropped the session holding it.
	Verify(ctx context.Context) error
	// Backup writes a backup of the store's data and schema under
	// Config.BackupDir. reason is recorded in the backup's name.
	Backup(ctx context.Context, reason string) (Backup, error)
	// Restore replaces the store's data and schema with the backup at
	// path. A failed restore can be retried with the same backup.
	Restore(ctx context.Context, path string) error
	// Migrate applies the store package's pending schema migrations. It
	// is the raw step: heo calls it through Migrate, which refuses a
	// newer schema and backs up first.
	Migrate(ctx context.Context) error
	// Open returns the uma store. It refuses a schema other than the
	// store package's own; heo calls it through Open.
	Open(ctx context.Context) (store.Store, error)
	// Release gives up ownership. It is idempotent.
	Release(ctx context.Context) error
}

// Backup describes one backup a driver wrote.
type Backup struct {
	// Path is the backup file.
	Path string `json:"path"       yaml:"path"`
	// Driver wrote it.
	Driver string `json:"driver"     yaml:"driver"`
	// Schema is the store's schema version when it was taken.
	Schema int64 `json:"schema"     yaml:"schema"`
	// Reason is why it was taken, for example "pre-migrate".
	Reason    string    `json:"reason"     yaml:"reason"`
	CreatedAt time.Time `json:"created_at" yaml:"created_at"`
	// Blobs counts the attachment files the backup carries; nil when it
	// carries none (the store alone).
	Blobs *BlobStats `json:"blobs,omitempty" yaml:"blobs,omitempty"`
}

// BlobStats counts attachment files: what a backup carries or a blob
// directory holds.
type BlobStats struct {
	Files int   `json:"files" yaml:"files"`
	Bytes int64 `json:"bytes" yaml:"bytes"`
}
