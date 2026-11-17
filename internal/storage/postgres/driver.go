// Package postgres is heo's postgres storage driver: uma's store/postgres
// on a pgx pool, with heo-side ownership, version reporting and
// pg_dump/pg_restore backups.
//
// Settings, under storage.postgres:
//
//	dsn           libpq connection string or URL, without a password;
//	              empty uses libpq defaults and PG* environment variables
//	password_ref  credential reference for the password, resolved through
//	              the profile's credential backend
//	max_conns     pool size; 0 keeps pgx's default
//	pg_dump       pg_dump executable for backups
//	pg_restore    pg_restore executable for restores
//
// Ownership is a session advisory lock held on a dedicated connection,
// keyed by the database and the schema the store lives in: a second
// daemon on the same store is refused, and a crashed owner's lock goes
// with its session.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Name is the driver's storage.driver value.
const Name = "postgres"

// Setting keys, relative to storage.postgres.
const (
	KeyDSN         = "dsn"
	KeyPasswordRef = "password_ref"
	KeyMaxConns    = "max_conns"
	KeyPgDump      = "pg_dump"
	KeyPgRestore   = "pg_restore"
)

// PasswordPurpose is the purpose the password reference is resolved
// for.
const PasswordPurpose = "storage.postgres"

// Options configures the driver.
type Options struct {
	// Runner runs pg_dump and pg_restore. Nil runs them as local
	// processes.
	Runner Runner
}

// Driver is the postgres storage driver.
type Driver struct {
	runner Runner
}

// New returns the driver.
func New(o Options) *Driver {
	r := o.Runner
	if r == nil {
		r = ExecRunner{}
	}
	return &Driver{runner: r}
}

var _ storage.Driver = (*Driver)(nil)

// Name returns "postgres".
func (*Driver) Name() string { return Name }

// Defaults returns the driver's built-in settings.
func (*Driver) Defaults() map[string]any {
	return map[string]any{
		KeyDSN:         "",
		KeyPasswordRef: "",
		KeyMaxConns:    0,
		KeyPgDump:      "pg_dump",
		KeyPgRestore:   "pg_restore",
	}
}

// Validate checks settings offline: the DSN parses and carries no
// password, the pool size is sane and the backup tools are named.
func (*Driver) Validate(s storage.Settings) []storage.Problem {
	var out []storage.Problem
	add := func(key, msg string) { out = append(out, storage.Problem{Key: key, Message: msg}) }
	dsn := s.GetString(KeyDSN)
	if inline, err := hasInlinePassword(dsn); err != nil {
		add(KeyDSN, err.Error())
	} else if inline {
		add(KeyDSN, "carries a password; remove it and set "+KeyPasswordRef+" to a credential reference")
	} else if _, err := pgxpool.ParseConfig(dsn); err != nil {
		add(KeyDSN, "does not parse: "+safeParseError(err))
	}
	if n := s.GetInt(KeyMaxConns); n < 0 {
		add(KeyMaxConns, fmt.Sprintf("%d: must be 0 (pgx default) or positive", n))
	}
	for _, k := range []string{KeyPgDump, KeyPgRestore} {
		if s.GetString(k) == "" {
			add(k, "empty: backups need it; set the executable name or path")
		}
	}
	return out
}

// Connect builds the pool from settings, resolving the password
// reference, and checks the server answers. It neither reads nor
// changes the schema.
func (d *Driver) Connect(ctx context.Context, cfg storage.Config) (storage.Conn, error) {
	if probs := d.Validate(cfg.Settings); len(probs) > 0 {
		p := probs[0]
		return nil, model.NewError(model.Validation, "storage.postgres."+p.Key+": "+p.Message)
	}
	dsn := cfg.Settings.GetString(KeyDSN)
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, model.NewError(model.Validation, "storage.postgres.dsn does not parse: "+safeParseError(err))
	}
	var password []byte
	if ref := cfg.Settings.GetString(KeyPasswordRef); ref != "" {
		user, secret, err := resolvePassword(ctx, cfg.Credentials, ref)
		if err != nil {
			return nil, err
		}
		if user != "" {
			pcfg.ConnConfig.User = user
		}
		password = secret
		pcfg.ConnConfig.Password = string(password)
	}
	if n := cfg.Settings.GetInt(KeyMaxConns); n > 0 {
		pcfg.MaxConns = int32(min(n, 1<<20)) //nolint:gosec // G115: bounded above
	}
	if cfg.Owner != "" {
		if _, set := pcfg.ConnConfig.RuntimeParams["application_name"]; !set {
			pcfg.ConnConfig.RuntimeParams["application_name"] = cfg.Owner
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, model.NewError(model.Unavailable, "postgres pool: "+safeParseError(err))
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, connError(ctx, err)
	}
	return &conn{
		pool:      pool,
		dsn:       dsn,
		password:  password,
		runner:    d.runner,
		pgDump:    cfg.Settings.GetString(KeyPgDump),
		pgRestore: cfg.Settings.GetString(KeyPgRestore),
		backupDir: cfg.BackupDir,
		owner:     cfg.Owner,
	}, nil
}

// resolvePassword resolves the password reference: a password
// credential supplies the login too, a secret only the password.
func resolvePassword(ctx context.Context, creds storage.Credentials, ref string) (user string, secret []byte, err error) {
	if creds == nil {
		return "", nil, model.NewError(model.Validation, "storage.postgres.password_ref is set but no credential backend is configured")
	}
	c, err := creds.Resolve(ctx, storage.CredentialRequest{
		Reference: ref,
		Purpose:   PasswordPurpose,
		Accept:    []ports.CredentialKind{ports.KindPassword, ports.KindSecret},
	})
	if err != nil {
		return "", nil, credentialError(err)
	}
	switch c := c.(type) {
	case ports.Password:
		return c.User, c.Secret, nil
	case *ports.Password:
		return c.User, c.Secret, nil
	case ports.Secret:
		return "", c.Bytes, nil
	case *ports.Secret:
		return "", c.Bytes, nil
	}
	return "", nil, model.NewError(model.Unsupported, "storage.postgres.password_ref resolved to an unsupported credential kind; want password or secret")
}

// credentialError keeps a uma-coded resolver error and classifies any
// other as unavailable, without its text: it may describe the secret.
func credentialError(err error) error {
	var me *model.Error
	if errors.As(err, &me) {
		return me
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return model.NewError(model.Unavailable, "storage.postgres.password_ref could not be resolved")
}

// connError classifies a failure to reach the server.
func connError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return model.NewError(model.Unavailable, "postgres unreachable: "+safeParseError(err))
}
