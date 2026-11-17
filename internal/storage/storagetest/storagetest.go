// Package storagetest is the conformance suite every heo storage driver
// passes. A driver's own test supplies a Harness that provisions real
// stores for it; Run drives the driver through heo's orchestration
// (storage.Migrate, storage.Open) and checks the contract: open,
// idempotent migration, backup before migration, refusal of a newer
// schema, exclusive ownership, recovery from a failed migration and a
// backup/restore round trip.
package storagetest

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Harness provisions stores for one driver.
type Harness interface {
	// Driver is the driver under test.
	Driver() storage.Driver
	// NewStore provisions an empty data store no other test shares and
	// returns the configuration that reaches it, its BackupDir included.
	NewStore(t *testing.T) storage.Config
	// Downgrade moves an initialized store one schema version back,
	// keeping its data, as the previous heo release would have left it.
	Downgrade(t *testing.T, cfg storage.Config)
	// MarkNewer records the store as migrated by a newer heo: one schema
	// version past this binary's.
	MarkNewer(t *testing.T, cfg storage.Config)
	// BreakMigrations makes every schema change on the store fail, for
	// connections made after it returns, until repair runs; reads,
	// ownership and backups keep working.
	BreakMigrations(t *testing.T, cfg storage.Config) (repair func())
}

// Tenant is the uma tenant the suite writes as.
const Tenant model.TenantID = "conformance"

// Run runs every conformance case against h.
func Run(t *testing.T, h Harness) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"OpenFreshStore", testOpenFreshStore},
		{"ReportVersions", testReportVersions},
		{"MigrateIdempotent", testMigrateIdempotent},
		{"BackupBeforeMigrate", testBackupBeforeMigrate},
		{"OpenRefusesPendingMigration", testOpenRefusesPendingMigration},
		{"RefuseNewerSchema", testRefuseNewerSchema},
		{"ExclusiveOwnership", testExclusiveOwnership},
		{"FailedMigrationLeavesRecoverableStore", testFailedMigrationRecoverable},
		{"BackupRestoreRoundTrip", testBackupRestoreRoundTrip},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, h) })
	}
}

func ctxOf(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func connect(t *testing.T, h Harness, cfg storage.Config) storage.Conn {
	t.Helper()
	c, err := h.Driver().Connect(ctxOf(t), cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func acquire(t *testing.T, c storage.Conn) storage.Owner {
	t.Helper()
	o, err := c.Acquire(ctxOf(t))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = o.Release(context.WithoutCancel(t.Context())) })
	return o
}

func migrate(t *testing.T, o storage.Owner) storage.MigrateResult {
	t.Helper()
	res, err := storage.Migrate(ctxOf(t), o)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return res
}

func open(t *testing.T, o storage.Owner) store.Store {
	t.Helper()
	st, _, err := storage.Open(ctxOf(t), o)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

func inspect(t *testing.T, c storage.Conn) storage.Report {
	t.Helper()
	rep, err := storage.Inspect(ctxOf(t), c)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	return rep
}

// initialized provisions a store, migrates it and writes one contact.
func initialized(t *testing.T, h Harness) (storage.Config, model.Contact) {
	t.Helper()
	cfg := h.NewStore(t)
	c := connect(t, h, cfg)
	o := acquire(t, c)
	migrate(t, o)
	contact := putContact(t, open(t, o), "00000000-0000-4000-8000-000000000001", "Seeded")
	if err := o.Release(ctxOf(t)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return cfg, contact
}

func putContact(t *testing.T, st store.Store, id, name string) model.Contact {
	t.Helper()
	ct := model.Contact{
		Ref:         model.ContactRef{Tenant: Tenant, ID: model.ID(id)},
		DisplayName: name,
		Kind:        "person",
		Revision:    "r1",
	}
	err := st.Transact(ctxOf(t), Tenant, func(tx store.Tx) error {
		return tx.PutContact(ctxOf(t), ct, "")
	})
	if err != nil {
		t.Fatalf("PutContact: %v", err)
	}
	return ct
}

// contactExists reports whether ct is stored, failing on any error but
// not-found.
func contactExists(t *testing.T, st store.Store, ct model.Contact) bool {
	t.Helper()
	var got model.Contact
	err := st.View(ctxOf(t), Tenant, func(r store.Reader) error {
		var err error
		got, err = r.GetContact(ctxOf(t), ct.Ref)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return false
	case err != nil:
		t.Fatalf("GetContact: %v", err)
	}
	if got.DisplayName != ct.DisplayName {
		t.Fatalf("contact = %+v, want %+v", got, ct)
	}
	return true
}

func wantCode(t *testing.T, err error, code model.ErrorCode, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: got nil error, want %s", what, code)
	}
	if got := model.CodeOf(err); got != code {
		t.Fatalf("%s: error %v has code %q, want %q", what, err, got, code)
	}
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// testOpenFreshStore: an empty store is uninitialized, Migrate creates
// the schema without a backup (nothing to back up), and the opened store
// takes writes and reads them back.
func testOpenFreshStore(t *testing.T, h Harness) {
	cfg := h.NewStore(t)
	c := connect(t, h, cfg)
	if rep := inspect(t, c); rep.Compat != storage.CompatUninitialized || rep.Schema != 0 {
		t.Fatalf("fresh store report = %+v, want uninitialized at 0", rep)
	}
	o := acquire(t, c)
	res := migrate(t, o)
	if res.Backup != nil {
		t.Errorf("fresh store backed up: %+v", res.Backup)
	}
	if res.From != 0 || res.To == 0 {
		t.Errorf("migrate result = %+v", res)
	}
	st := open(t, o)
	ct := putContact(t, st, "00000000-0000-4000-8000-000000000010", "Fresh")
	if !contactExists(t, st, ct) {
		t.Fatal("written contact not found")
	}
	if rep := inspect(t, c); rep.Compat != storage.CompatOK {
		t.Fatalf("migrated store report = %+v, want compatible", rep)
	}
}

// testReportVersions: the report names the driver, the server, the
// store library and both schema versions — the service status hook.
func testReportVersions(t *testing.T, h Harness) {
	cfg, _ := initialized(t, h)
	c := connect(t, h, cfg)
	rep := inspect(t, c)
	if rep.Driver != h.Driver().Name() {
		t.Errorf("Driver = %q, want %q", rep.Driver, h.Driver().Name())
	}
	if rep.Server == "" || rep.Library == "" {
		t.Errorf("report misses server or library version: %+v", rep)
	}
	if rep.LibrarySchema <= 0 || rep.Schema != rep.LibrarySchema || rep.Compat != storage.CompatOK {
		t.Errorf("report = %+v, want schema = library schema > 0, compatible", rep)
	}
	// The owner reports the same.
	o := acquire(t, c)
	orep, err := storage.Inspect(ctxOf(t), o)
	if err != nil || orep != rep {
		t.Errorf("owner report = %+v, %v; want %+v", orep, err, rep)
	}
}

// testMigrateIdempotent: migrating a current store changes nothing and
// takes no backup.
func testMigrateIdempotent(t *testing.T, h Harness) {
	cfg, ct := initialized(t, h)
	c := connect(t, h, cfg)
	o := acquire(t, c)
	before := backupFiles(t, cfg.BackupDir)
	for i := range 2 {
		res := migrate(t, o)
		if res.Backup != nil || res.From != res.To {
			t.Fatalf("migrate #%d on a current store = %+v, want a no-op", i+1, res)
		}
	}
	if after := backupFiles(t, cfg.BackupDir); len(after) != len(before) {
		t.Errorf("no-op migrations wrote backups: %v", after)
	}
	if !contactExists(t, open(t, o), ct) {
		t.Fatal("data lost across no-op migrations")
	}
}

// testBackupBeforeMigrate: an older store is backed up before it is
// migrated, and the backup holds the pre-migration schema and data.
func testBackupBeforeMigrate(t *testing.T, h Harness) {
	cfg, ct := initialized(t, h)
	h.Downgrade(t, cfg)
	c := connect(t, h, cfg)
	old := inspect(t, c)
	if old.Compat != storage.CompatMigrate {
		t.Fatalf("downgraded store report = %+v, want migration_required", old)
	}
	o := acquire(t, c)
	res := migrate(t, o)
	if res.Backup == nil {
		t.Fatal("older store migrated without a backup")
	}
	if res.Backup.Schema != old.Schema || res.Backup.Reason != storage.ReasonPreMigrate || res.Backup.Driver != h.Driver().Name() {
		t.Errorf("backup = %+v, want schema %d, reason %s", res.Backup, old.Schema, storage.ReasonPreMigrate)
	}
	if !strings.HasPrefix(res.Backup.Path, cfg.BackupDir) {
		t.Errorf("backup %s outside %s", res.Backup.Path, cfg.BackupDir)
	}
	if fi, err := os.Stat(res.Backup.Path); err != nil {
		t.Fatalf("backup file: %v", err)
	} else if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("backup mode %v, want owner-only", fi.Mode().Perm())
	}
	if res.From != old.Schema || res.To != old.LibrarySchema {
		t.Errorf("migrate result = %+v", res)
	}
	if !contactExists(t, open(t, o), ct) {
		t.Fatal("data lost across migration")
	}
	// The backup is the pre-migration store: restoring it brings back
	// the old schema with the data.
	if err := o.Restore(ctxOf(t), res.Backup.Path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep := inspect(t, c); rep.Schema != old.Schema {
		t.Fatalf("restored schema = %d, want %d (pre-migration)", rep.Schema, old.Schema)
	}
	migrate(t, o)
	if !contactExists(t, open(t, o), ct) {
		t.Fatal("restored backup lost data")
	}
}

// testOpenRefusesPendingMigration: workers never start on an older
// schema.
func testOpenRefusesPendingMigration(t *testing.T, h Harness) {
	cfg, _ := initialized(t, h)
	h.Downgrade(t, cfg)
	c := connect(t, h, cfg)
	o := acquire(t, c)
	st, rep, err := storage.Open(ctxOf(t), o)
	wantCode(t, err, model.SchemaMismatch, "Open on an older schema")
	if st != nil || rep.Compat != storage.CompatMigrate {
		t.Errorf("Open returned store %v, report %+v", st, rep)
	}
}

// testRefuseNewerSchema: a store a newer heo migrated is neither
// migrated (no downgrade), backed up nor opened.
func testRefuseNewerSchema(t *testing.T, h Harness) {
	cfg, _ := initialized(t, h)
	h.MarkNewer(t, cfg)
	c := connect(t, h, cfg)
	rep := inspect(t, c)
	if rep.Compat != storage.CompatNewer || rep.Schema != rep.LibrarySchema+1 {
		t.Fatalf("report = %+v, want newer_schema at library+1", rep)
	}
	o := acquire(t, c)
	before := backupFiles(t, cfg.BackupDir)
	res, err := storage.Migrate(ctxOf(t), o)
	wantCode(t, err, model.SchemaMismatch, "Migrate on a newer schema")
	t.Logf("newer schema refused: %v", err)
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("refusal %q does not say the schema is newer", err)
	}
	if res.Backup != nil {
		t.Errorf("refused migration took a backup: %+v", res.Backup)
	}
	if after := backupFiles(t, cfg.BackupDir); len(after) != len(before) {
		t.Errorf("refused migration wrote backups: %v", after)
	}
	if got := inspect(t, c); got.Schema != rep.Schema {
		t.Fatalf("schema after refused migration = %d, want %d (unchanged)", got.Schema, rep.Schema)
	}
	_, orep, err := storage.Open(ctxOf(t), o)
	wantCode(t, err, model.SchemaMismatch, "Open on a newer schema")
	if orep.Compat != storage.CompatNewer {
		t.Errorf("Open report = %+v", orep)
	}
}

// testExclusiveOwnership: while one owner holds the store, a second is
// refused; once released, it can take it.
func testExclusiveOwnership(t *testing.T, h Harness) {
	cfg, _ := initialized(t, h)
	c1 := connect(t, h, cfg)
	c2 := connect(t, h, cfg)
	o1 := acquire(t, c1)
	if err := o1.Verify(ctxOf(t)); err != nil {
		t.Fatalf("Verify while held: %v", err)
	}
	o2, err := c2.Acquire(ctxOf(t))
	if err == nil {
		_ = o2.Release(ctxOf(t))
		t.Fatal("second owner admitted while the first holds the store")
	}
	wantCode(t, err, model.Conflict, "second Acquire")
	t.Logf("second owner refused: %v", err)
	if cfg.Owner != "" && !strings.Contains(err.Error(), cfg.Owner) {
		t.Errorf("refusal %q does not name the holder %q", err, cfg.Owner)
	}
	// Reads need no ownership.
	if rep := inspect(t, c2); rep.Compat != storage.CompatOK {
		t.Errorf("non-owner report = %+v", rep)
	}
	if err := o1.Release(ctxOf(t)); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := o1.Release(ctxOf(t)); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if err := o1.Verify(ctxOf(t)); err == nil {
		t.Error("Verify after Release succeeded")
	}
	o2 = acquire(t, c2)
	if err := o2.Verify(ctxOf(t)); err != nil {
		t.Fatalf("Verify of the new owner: %v", err)
	}
	// Closing the connection gives ownership up too.
	if err := c2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c3 := connect(t, h, cfg)
	acquire(t, c3)
}

// testFailedMigrationRecoverable: a migration that fails leaves the
// store at its old schema, not half-migrated, with the pre-migration
// backup on disk; once the cause is fixed the store restores from it and
// migrates.
func testFailedMigrationRecoverable(t *testing.T, h Harness) {
	cfg, ct := initialized(t, h)
	h.Downgrade(t, cfg)
	repair := h.BreakMigrations(t, cfg)
	repaired := false
	defer func() {
		if !repaired {
			repair()
		}
	}()

	c := connect(t, h, cfg)
	old := inspect(t, c)
	o := acquire(t, c)
	res, err := storage.Migrate(ctxOf(t), o)
	if err == nil {
		t.Fatal("migration succeeded on a store that refuses schema changes")
	}
	t.Logf("migration failed as arranged: %v", err)
	if res.Backup == nil {
		t.Fatal("failed migration has no pre-migration backup")
	}
	if !strings.Contains(err.Error(), res.Backup.Path) {
		t.Errorf("failure %q does not name the backup %s", err, res.Backup.Path)
	}
	if _, err := os.Stat(res.Backup.Path); err != nil {
		t.Fatalf("backup file: %v", err)
	}
	if rep := inspect(t, c); rep.Schema != old.Schema || rep.Compat != storage.CompatMigrate {
		t.Fatalf("after failed migration report = %+v, want schema %d unchanged", rep, old.Schema)
	}
	_, _, err = storage.Open(ctxOf(t), o)
	wantCode(t, err, model.SchemaMismatch, "Open after a failed migration")
	if err := o.Release(ctxOf(t)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	repair()
	repaired = true
	c = connect(t, h, cfg)
	o = acquire(t, c)
	if err := o.Restore(ctxOf(t), res.Backup.Path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rep := inspect(t, c); rep.Schema != old.Schema {
		t.Fatalf("restored schema = %d, want %d", rep.Schema, old.Schema)
	}
	migrate(t, o)
	if !contactExists(t, open(t, o), ct) {
		t.Fatal("data lost after recovery")
	}
}

// testBackupRestoreRoundTrip: a restore brings back exactly the backed-up
// data: later writes are gone, earlier ones back.
func testBackupRestoreRoundTrip(t *testing.T, h Harness) {
	cfg, seeded := initialized(t, h)
	c := connect(t, h, cfg)
	o := acquire(t, c)
	b, err := o.Backup(ctxOf(t), "manual")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if b.Reason != "manual" || b.Schema != inspect(t, c).Schema {
		t.Errorf("backup = %+v", b)
	}
	st := open(t, o)
	later := putContact(t, st, "00000000-0000-4000-8000-000000000020", "Later")
	if err := o.Restore(ctxOf(t), b.Path); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	st = open(t, o)
	if !contactExists(t, st, seeded) {
		t.Error("backed-up contact missing after restore")
	}
	if contactExists(t, st, later) {
		t.Error("contact written after the backup survived the restore")
	}
	// The restored store takes writes again.
	putContact(t, st, "00000000-0000-4000-8000-000000000021", "After restore")

	if err := o.Restore(ctxOf(t), b.Path+".missing"); err == nil {
		t.Error("restore of a missing backup succeeded")
	}
}
