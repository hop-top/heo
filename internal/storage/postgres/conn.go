package postgres

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3/database"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
	"hop.top/uma/store"
	umapg "hop.top/uma/store/postgres"
)

// libraryModule is the store package whose version Versions reports.
const libraryModule = "hop.top/uma/store/postgres"

// ownerLockSalt namespaces heo's ownership lock among the advisory locks
// other software takes in the same database.
const ownerLockSalt = "heo.storage.owner/"

type conn struct {
	pool      *pgxpool.Pool
	dsn       string
	password  []byte
	runner    Runner
	pgDump    string
	pgRestore string
	backupDir string
	owner     string

	mu     sync.Mutex
	held   *owner
	closed bool
}

var _ storage.Conn = (*conn)(nil)

// Versions reads the server version and the schema version applied in
// the database from uma's migration table.
func (c *conn) Versions(ctx context.Context) (storage.Versions, error) {
	v := storage.Versions{Driver: Name, Library: libraryVersion(), LibrarySchema: umapg.SchemaVersion}
	if err := c.pool.QueryRow(ctx, "SHOW server_version").Scan(&v.Server); err != nil {
		return v, queryError(ctx, err)
	}
	schema, err := c.schemaVersion(ctx)
	if err != nil {
		return v, err
	}
	v.Schema = schema
	return v, nil
}

// schemaVersion returns the newest migration applied, 0 when uma's
// migration table does not exist.
func (c *conn) schemaVersion(ctx context.Context) (int64, error) {
	st, err := database.NewStore(database.DialectPostgres, umapg.VersionTable)
	if err != nil {
		return 0, model.NewError(model.Permanent, "migration table reader unavailable")
	}
	db := stdlib.OpenDBFromPool(c.pool)
	defer func() { _ = db.Close() }()
	if ext, ok := st.(database.StoreExtender); ok {
		exists, err := ext.TableExists(ctx, db)
		if err != nil {
			return 0, queryError(ctx, err)
		}
		if !exists {
			return 0, nil
		}
	}
	v, err := st.GetLatestVersion(ctx, db)
	if errors.Is(err, database.ErrVersionNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, queryError(ctx, err)
	}
	return v, nil
}

// libraryVersion names the store module and the version this binary was
// built with. Test binaries record no dependencies and report (devel).
func libraryVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return libraryModule
	}
	for _, m := range bi.Deps {
		if m.Path != libraryModule {
			continue
		}
		if m.Replace != nil {
			if v := m.Replace.Version; v != "" && v != "(devel)" {
				return libraryModule + "@" + v
			}
			return libraryModule + "@" + m.Version + " (local replace)"
		}
		return libraryModule + "@" + m.Version
	}
	return libraryModule + "@(devel)"
}

// Acquire takes the ownership lock on a dedicated connection, so pool
// churn never drops it.
func (c *conn) Acquire(ctx context.Context) (storage.Owner, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, model.NewError(model.Permanent, "storage connection closed")
	}
	if c.held != nil {
		return nil, model.NewError(model.Conflict, "this connection already owns the store")
	}
	lc, err := pgx.ConnectConfig(ctx, c.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, connError(ctx, err)
	}
	var key int64
	var ok bool
	err = lc.QueryRow(ctx, "SELECT k, pg_try_advisory_lock(k) FROM (SELECT hashtextextended($1 || current_schema(), 0) AS k) s",
		ownerLockSalt).Scan(&key, &ok)
	if err != nil {
		_ = lc.Close(context.WithoutCancel(ctx))
		return nil, queryError(ctx, err)
	}
	if !ok {
		holder := lockHolder(ctx, lc, key)
		_ = lc.Close(context.WithoutCancel(ctx))
		return nil, model.NewError(model.Conflict, "the data store is owned by another heo"+holder+
			"; stop it before starting this one")
	}
	o := &owner{c: c, lc: lc, key: key}
	c.held = o
	return o, nil
}

// lockHolder describes the session holding key, best effort.
func lockHolder(ctx context.Context, lc *pgx.Conn, key int64) string {
	var pid int32
	var app string
	var since time.Time
	err := lc.QueryRow(ctx, `
SELECT a.pid, a.application_name, a.backend_start
FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory' AND l.granted AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND l.classid = ((($1::bigint) >> 32) & 4294967295)::oid
  AND l.objid = (($1::bigint) & 4294967295)::oid
  AND l.objsubid = 1
LIMIT 1`, key).Scan(&pid, &app, &since)
	if err != nil {
		return ""
	}
	if app == "" {
		app = "unnamed client"
	}
	return fmt.Sprintf(" (%s, server pid %d, since %s)", app, pid, since.UTC().Format(time.RFC3339))
}

// Close releases ownership and closes the pool.
func (c *conn) Close() error {
	c.mu.Lock()
	o := c.held
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	if already {
		return nil
	}
	var err error
	if o != nil {
		err = o.Release(context.Background())
	}
	c.pool.Close()
	return err
}

type owner struct {
	c   *conn
	lc  *pgx.Conn
	key int64

	mu       sync.Mutex
	released bool
}

var _ storage.Owner = (*owner)(nil)

func (o *owner) Versions(ctx context.Context) (storage.Versions, error) { return o.c.Versions(ctx) }

// Verify checks the lock session is alive and still holds the lock.
func (o *owner) Verify(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.released {
		return model.NewError(model.Conflict, "store ownership was released")
	}
	var held bool
	err := o.lc.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM pg_locks
WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid()
  AND classid = ((($1::bigint) >> 32) & 4294967295)::oid
  AND objid = (($1::bigint) & 4294967295)::oid
  AND objsubid = 1)`, o.key).Scan(&held)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return model.NewError(model.Conflict, "store ownership lost: the session holding it is gone")
	}
	if !held {
		return model.NewError(model.Conflict, "store ownership lost")
	}
	return nil
}

func (o *owner) Migrate(ctx context.Context) error {
	if err := o.Verify(ctx); err != nil {
		return err
	}
	err := umapg.Migrate(ctx, o.c.pool)
	// Cached statements may name tables a migration changed.
	o.c.pool.Reset()
	return err
}

func (o *owner) Open(ctx context.Context) (store.Store, error) {
	if err := o.Verify(ctx); err != nil {
		return nil, err
	}
	st, err := umapg.Open(ctx, o.c.pool, umapg.Options{})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Release unlocks and closes the lock session.
func (o *owner) Release(ctx context.Context) error {
	o.mu.Lock()
	if o.released {
		o.mu.Unlock()
		return nil
	}
	o.released = true
	o.mu.Unlock()

	o.c.mu.Lock()
	if o.c.held == o {
		o.c.held = nil
	}
	o.c.mu.Unlock()

	ctx = context.WithoutCancel(ctx)
	_, uerr := o.lc.Exec(ctx, "SELECT pg_advisory_unlock($1)", o.key)
	// Closing the session drops the lock even when the unlock failed.
	if cerr := o.lc.Close(ctx); cerr != nil && uerr == nil {
		return queryError(ctx, cerr)
	}
	return nil
}

// queryError classifies a database error without its query text.
func queryError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var me *model.Error
	if errors.As(err, &me) {
		return me
	}
	return model.NewError(model.Unavailable, "postgres: "+safeParseError(err))
}
