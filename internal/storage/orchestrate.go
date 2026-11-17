package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Compat is how a store's schema relates to this binary's.
type Compat string

const (
	// CompatOK: the schemas match; workers may start.
	CompatOK Compat = "compatible"
	// CompatUninitialized: the store holds no schema yet; Migrate creates
	// it.
	CompatUninitialized Compat = "uninitialized"
	// CompatMigrate: the store is older; Migrate upgrades it after a
	// backup.
	CompatMigrate Compat = "migration_required"
	// CompatNewer: a newer heo migrated the store. This binary refuses to
	// migrate or open it: upgrade heo, or restore a backup taken before
	// that migration.
	CompatNewer Compat = "newer_schema"
)

// Classify compares a store's schema version with the library's.
func Classify(schema, library int64) Compat {
	switch {
	case schema > library:
		return CompatNewer
	case schema == library:
		return CompatOK
	case schema <= 0:
		return CompatUninitialized
	default:
		return CompatMigrate
	}
}

// Report is a store's versions and their compatibility with this
// binary: what `service status` shows for storage.
type Report struct {
	Versions `yaml:",inline"`
	Compat   Compat `json:"compat" yaml:"compat"`
}

// versionsSource is a Conn or an Owner.
type versionsSource interface {
	Versions(ctx context.Context) (Versions, error)
}

// Inspect reports src's versions and compatibility. It changes nothing
// and needs no ownership.
func Inspect(ctx context.Context, src versionsSource) (Report, error) {
	v, err := src.Versions(ctx)
	if err != nil {
		return Report{}, err
	}
	return Report{Versions: v, Compat: Classify(v.Schema, v.LibrarySchema)}, nil
}

// MigrateResult is what Migrate did.
type MigrateResult struct {
	// From and To are the schema versions before and after.
	From int64 `json:"from" yaml:"from"`
	To   int64 `json:"to"   yaml:"to"`
	// Backup is the backup taken before migrating, nil when the store
	// held no schema or nothing was pending. It is set even when the
	// migration then failed: it is the way back.
	Backup *Backup `json:"backup,omitempty" yaml:"backup,omitempty"`
}

// ReasonPreMigrate names the backup Migrate takes.
const ReasonPreMigrate = "pre-migrate"

// Migrate brings the owned store to this binary's schema:
//
//   - a newer schema is refused before anything is written: no backup,
//     no migration, never a downgrade;
//   - a matching schema is left alone;
//   - an older schema is backed up first, and a failed backup stops the
//     migration;
//   - an empty store is initialized; it has nothing to back up.
//
// The migrated store must then report this binary's schema.
func Migrate(ctx context.Context, o Owner) (MigrateResult, error) {
	v, err := o.Versions(ctx)
	if err != nil {
		return MigrateResult{}, err
	}
	res := MigrateResult{From: v.Schema, To: v.Schema}
	switch Classify(v.Schema, v.LibrarySchema) {
	case CompatOK:
		return res, nil
	case CompatNewer:
		return res, newerSchema(v)
	case CompatMigrate:
		b, err := o.Backup(ctx, ReasonPreMigrate)
		if err != nil {
			return res, withMessage(err, "backup before migration failed; nothing was migrated: ")
		}
		res.Backup = &b
	case CompatUninitialized:
	}
	if err := o.Migrate(ctx); err != nil {
		if res.Backup != nil {
			return res, withMessage(err, "migration failed; restore "+res.Backup.Path+" if the store is unusable: ")
		}
		return res, err
	}
	after, err := o.Versions(ctx)
	if err != nil {
		return res, err
	}
	res.To = after.Schema
	if after.Schema != after.LibrarySchema {
		return res, model.NewError(model.SchemaMismatch, "migration ended at schema version "+
			itoa(after.Schema)+", not "+itoa(after.LibrarySchema))
	}
	return res, nil
}

// MigratePlan is what Migrate would do to a store.
type MigratePlan struct {
	// From and To are the schema versions before and after.
	From int64 `json:"from" yaml:"from"`
	To   int64 `json:"to"   yaml:"to"`
	// Compat is the store's compatibility now.
	Compat Compat `json:"compat" yaml:"compat"`
	// Backup reports that Migrate would back the store up first.
	Backup bool `json:"backup" yaml:"backup"`
}

// PlanMigrate reports what Migrate would do, reading versions only. A
// newer schema is refused with the error Migrate returns.
func PlanMigrate(ctx context.Context, src versionsSource) (MigratePlan, error) {
	v, err := src.Versions(ctx)
	if err != nil {
		return MigratePlan{}, err
	}
	p := MigratePlan{From: v.Schema, To: v.LibrarySchema, Compat: Classify(v.Schema, v.LibrarySchema)}
	switch p.Compat {
	case CompatOK:
	case CompatNewer:
		p.To = v.Schema
		return p, newerSchema(v)
	case CompatMigrate:
		p.Backup = true
	case CompatUninitialized:
	}
	return p, nil
}

// Open checks that the owned store's schema is this binary's, then
// opens it: the compatibility check that runs before any worker starts.
// A store that needs migrating, or that a newer heo migrated, is refused
// with model.SchemaMismatch.
func Open(ctx context.Context, o Owner) (store.Store, Report, error) {
	rep, err := Inspect(ctx, o)
	if err != nil {
		return nil, Report{}, err
	}
	switch rep.Compat {
	case CompatOK:
	case CompatNewer:
		return nil, rep, newerSchema(rep.Versions)
	default:
		return nil, rep, model.NewError(model.SchemaMismatch, "store schema version "+itoa(rep.Schema)+
			" is older than "+itoa(rep.LibrarySchema)+"; migrate it before starting heo")
	}
	st, err := o.Open(ctx)
	if err != nil {
		return nil, rep, err
	}
	return st, rep, nil
}

func newerSchema(v Versions) error {
	return model.NewError(model.SchemaMismatch, "store schema version "+itoa(v.Schema)+
		" is newer than "+itoa(v.LibrarySchema)+" ("+v.Library+"); upgrade heo, or restore a backup taken before that migration")
}

// withMessage prefixes err's safe message, keeping its uma code.
func withMessage(err error, prefix string) error {
	var me *model.Error
	if errors.As(err, &me) {
		out := *me
		out.SafeMessage = prefix + me.SafeMessage
		return &out
	}
	return fmt.Errorf("%s%w", prefix, err)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
