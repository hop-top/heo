package daemon

import (
	"context"
	"errors"
	"fmt"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// DriverStorage opens the profile's data store through its storage
// driver: connect, take exclusive ownership, then open the uma store,
// which refuses a schema other than this binary's. Every refusal names
// what to do about it.
func DriverStorage(d storage.Driver, cfg storage.Config) StorageOpener {
	return func(ctx context.Context) (Storage, error) {
		conn, err := d.Connect(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("storage %s: connect: %w", d.Name(), err)
		}
		owner, err := conn.Acquire(ctx)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("storage %s: %w", d.Name(), err)
		}
		st, rep, err := storage.Open(ctx, owner)
		if err != nil {
			_ = conn.Close()
			return nil, schemaError(d.Name(), rep, err)
		}
		return &driverStorage{conn: conn, owner: owner, st: st, rep: rep}, nil
	}
}

// schemaError adds the way forward to a refused open.
func schemaError(driver string, rep storage.Report, err error) error {
	if !model.IsCode(err, model.SchemaMismatch) {
		return fmt.Errorf("storage %s: open: %w", driver, err)
	}
	var fix string
	switch rep.Compat {
	case storage.CompatNewer:
		fix = "install the heo release that migrated it, or restore a backup taken before that migration with `heo storage restore <path>`"
	case storage.CompatUninitialized:
		fix = "initialize it with `heo storage migrate`, then start again"
	default:
		fix = "run `heo storage migrate` (it backs the store up first), then start again"
	}
	var me *model.Error
	if errors.As(err, &me) {
		return fmt.Errorf("storage %s: %s; %s", driver, me.SafeMessage, fix)
	}
	return fmt.Errorf("storage %s: %w; %s", driver, err, fix)
}

type driverStorage struct {
	conn  storage.Conn
	owner storage.Owner
	st    store.Store
	rep   storage.Report
}

func (s *driverStorage) Store() store.Store               { return s.st }
func (s *driverStorage) Report() storage.Report           { return s.rep }
func (s *driverStorage) Verify(ctx context.Context) error { return s.owner.Verify(ctx) }

func (s *driverStorage) Close(ctx context.Context) error {
	err := s.owner.Release(ctx)
	return errors.Join(err, s.conn.Close())
}
