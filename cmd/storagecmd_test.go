package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/postgres"
	"hop.top/heo/internal/storage/postgres/pgtest"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
	"hop.top/uma/store"
	umapg "hop.top/uma/store/postgres"
)

const storeTenant model.TenantID = "default"

func TestMain(m *testing.M) {
	code := m.Run()
	if err := pgtest.Terminate(); err != nil {
		log.Printf("terminate postgres container: %v", err)
	}
	os.Exit(code)
}

// storeEnv is one profile whose storage is a fresh database in the
// shared test server, and a direct line to that database for seeding
// and checking data behind the commands' back.
type storeEnv struct {
	t      *testing.T
	srv    *pgtest.Server
	dsn    string
	backup string
	blobs  string
	driver *postgres.Driver
	direct storage.Config
}

func newStoreEnv(t *testing.T) *storeEnv {
	t.Helper()
	srv := pgtest.Require(t)
	dsn := srv.NewDatabase(t)
	_, v, p := loadStorageProfile(t, map[string]any{"dsn": dsn, "password_ref": "heo/pg"})
	storeLogin(t, v, p, "heo/pg", pgtest.User, pgtest.Password)
	backup := filepath.Join(t.TempDir(), "backups")
	blobs := filepath.Join(t.TempDir(), "data", "blobs")
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "heo", "profiles")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil { //nolint:gosec // G703: the test's own temp config dir
		t.Fatal(err)
	}
	body := "credentials:\n  backend: file\nstorage:\n  backup:\n    dir: " + backup +
		"\n  blobs:\n    dir: " + blobs +
		"\n  postgres:\n    dsn: " + dsn + "\n    password_ref: heo/pg\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte(body), 0o600); err != nil { //nolint:gosec // G703: the test's own temp config dir
		t.Fatal(err)
	}
	// No terminal to ask on: the confirm gate behaves as in a script.
	t.Cleanup(cli.InstallGlobalPromptSource(func() *cli.PromptTTY { return nil }))
	e := &storeEnv{t: t, srv: srv, dsn: dsn, backup: backup, blobs: blobs}
	e.driver = postgres.New(postgres.Options{Runner: srv.Runner()})
	e.direct = storage.Config{
		Settings: storage.MapSettings{
			postgres.KeyDSN: dsn, postgres.KeyPasswordRef: pgtest.PasswordRef,
			postgres.KeyPgDump: "pg_dump", postgres.KeyPgRestore: "pg_restore",
		},
		Credentials: pgtest.Credentials,
		BackupDir:   filepath.Join(t.TempDir(), "direct-backups"),
		Owner:       "heo/test-daemon",
	}
	return e
}

// storeLogin writes a password envelope under ref through the profile's
// own credential backend.
func storeLogin(t *testing.T, v interface{ GetString(string) string }, p profile.Ref, ref, user, pass string) {
	t.Helper()
	ctx := context.Background()
	b, err := credentials.Open(ctx, credentials.Options{
		Profile: p,
		Tenant:  model.TenantID(v.GetString(settings.KeyTenant)),
		Config:  credentials.ConfigFrom(v.GetString),
	})
	if err != nil {
		t.Fatal(err)
	}
	key, err := b.Key(ref)
	if err != nil {
		t.Fatal(err)
	}
	env, err := json.Marshal(map[string]any{"v": 1, "kind": "password", "user": user, "password": pass})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SurfaceTokens().Set(ctx, key, env); err != nil {
		t.Fatal(err)
	}
}

type runResult struct {
	stdout, stderr string
	exit           int
}

// heo runs one command line in process on a fresh command tree whose
// postgres driver runs pg_dump and pg_restore in the test container.
func (e *storeEnv) heo(args ...string) runResult {
	e.t.Helper()
	a := newApp()
	reg := storage.NewRegistry()
	if err := reg.Register(postgres.New(postgres.Options{Runner: e.srv.Runner()})); err != nil {
		e.t.Fatal(err)
	}
	a.storage = reg
	var out, errb bytes.Buffer
	a.r.Cmd.SetArgs(args)
	a.r.Cmd.SetOut(&out)
	a.r.Cmd.SetErr(&errb)
	a.r.Cmd.SetIn(strings.NewReader(""))
	err := a.r.Execute(context.Background())
	r := runResult{stdout: out.String(), stderr: errb.String()}
	if err != nil {
		r.exit = ExitCode(err)
		if r.exit == 0 {
			r.exit = 1
		}
	}
	return r
}

func (e *storeEnv) want(r runResult, exit int, args ...string) {
	e.t.Helper()
	if r.exit != exit {
		e.t.Fatalf("heo %s: exit %d, want %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), r.exit, exit, r.stdout, r.stderr)
	}
}

func (e *storeEnv) run(exit int, args ...string) runResult {
	e.t.Helper()
	r := e.heo(args...)
	e.want(r, exit, args...)
	return r
}

// owner takes the store's ownership directly, the way a running daemon
// holds it; release gives it back.
func (e *storeEnv) owner() (storage.Owner, storage.Conn) {
	e.t.Helper()
	c, err := e.driver.Connect(context.Background(), e.direct)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	e.t.Cleanup(func() { _ = c.Close() })
	o, err := c.Acquire(context.Background())
	if err != nil {
		e.t.Fatalf("acquire: %v", err)
	}
	return o, c
}

// withStore runs fn on the opened store, holding ownership only for
// its duration.
func (e *storeEnv) withStore(fn func(st store.Store)) {
	e.t.Helper()
	o, c := e.owner()
	defer func() { _ = c.Close() }()
	st, _, err := storage.Open(context.Background(), o)
	if err != nil {
		e.t.Fatalf("open: %v", err)
	}
	fn(st)
	if err := o.Release(context.Background()); err != nil {
		e.t.Fatal(err)
	}
}

func (e *storeEnv) putContact(id, name string) model.Contact {
	e.t.Helper()
	ct := model.Contact{
		Ref:         model.ContactRef{Tenant: storeTenant, ID: model.ID(id)},
		DisplayName: name,
		Kind:        "person",
		Revision:    "r1",
	}
	e.withStore(func(st store.Store) {
		err := st.Transact(context.Background(), storeTenant, func(tx store.Tx) error {
			return tx.PutContact(context.Background(), ct, "")
		})
		if err != nil {
			e.t.Fatalf("PutContact: %v", err)
		}
	})
	return ct
}

func (e *storeEnv) hasContact(ct model.Contact) bool {
	e.t.Helper()
	found := false
	e.withStore(func(st store.Store) {
		err := st.View(context.Background(), storeTenant, func(r store.Reader) error {
			got, err := r.GetContact(context.Background(), ct.Ref)
			if err == nil && got.DisplayName == ct.DisplayName {
				found = true
			}
			if model.IsCode(err, model.NotFound) {
				return nil
			}
			return err
		})
		if err != nil {
			e.t.Fatalf("GetContact: %v", err)
		}
	})
	return found
}

func (e *storeEnv) schema() int64 {
	e.t.Helper()
	c, err := e.driver.Connect(context.Background(), e.direct)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	v, err := c.Versions(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return v.Schema
}

func (e *storeEnv) backups() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.backup)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, d := range entries {
		out = append(out, filepath.Join(e.backup, d.Name()))
	}
	return out
}

// envelope is the structured error kit renders under --format json.
type errEnv struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	SuggestedFix string `json:"suggested_fix"`
	ExitCode     int    `json:"exit_code"`
}

func errEnvelope(t *testing.T, stderr string) errEnv {
	t.Helper()
	i := strings.LastIndex(stderr, "\n{")
	body := stderr
	if i >= 0 {
		body = stderr[i+1:]
	} else if j := strings.Index(stderr, "{"); j >= 0 {
		body = stderr[j:]
	}
	var env errEnv
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("error envelope: %v in %q", err, stderr)
	}
	return env
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

type statusOut struct {
	Profile string `json:"profile"`
	Heo     string `json:"heo"`
	Kit     string `json:"kit"`
	Uma     string `json:"uma"`
	Storage struct {
		Driver        string `json:"driver"`
		Server        string `json:"server"`
		Library       string `json:"library"`
		Schema        int64  `json:"schema"`
		LibrarySchema int64  `json:"library_schema"`
		Compat        string `json:"compat"`
	} `json:"storage"`
	Action string `json:"action"`
}

type migrateOut struct {
	From   int64           `json:"from"`
	To     int64           `json:"to"`
	Backup *storage.Backup `json:"backup"`
}

type restoreOut struct {
	Restored       string             `json:"restored"`
	Safety         *storage.Backup    `json:"pre_restore_backup"`
	Blobs          *storage.BlobStats `json:"blobs"`
	BlobsUntouched bool               `json:"blobs_untouched"`
	ReplacedBlobs  string             `json:"replaced_blobs"`
	Schema         int64              `json:"schema"`
	Compat         string             `json:"compat"`
	NextSteps      string             `json:"action"`
}

type planOut struct {
	Command string `json:"command"`
	Effects []struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
		Detail string `json:"detail"`
	} `json:"effects"`
}

// TestStorageStatusReportsVersions: heo, kit, uma and store versions
// and compatibility, without ownership: it works while a daemon holds
// the store.
func TestStorageStatusReportsVersions(t *testing.T) {
	e := newStoreEnv(t)
	s := decode[statusOut](t, e.run(0, "storage", "status", "--format", "json").stdout)
	if s.Storage.Compat != string(storage.CompatUninitialized) || s.Storage.Schema != 0 ||
		!strings.Contains(s.Action, "heo storage migrate") {
		t.Errorf("fresh store status = %+v", s)
	}
	e.run(0, "storage", "migrate")
	_, _ = e.owner() // a daemon holds the store
	s = decode[statusOut](t, e.run(0, "storage", "status", "--format", "json").stdout)
	if s.Profile != "heo://profile/default" || s.Heo == "" ||
		!strings.HasPrefix(s.Kit, "hop.top/kit@") || !strings.HasPrefix(s.Uma, "hop.top/uma@") {
		t.Errorf("versions = %+v", s)
	}
	if s.Storage.Driver != postgres.Name || s.Storage.Server == "" || s.Storage.Library == "" ||
		s.Storage.Schema != umapg.SchemaVersion || s.Storage.LibrarySchema != umapg.SchemaVersion ||
		s.Storage.Compat != string(storage.CompatOK) || s.Action != "" {
		t.Errorf("storage = %+v, action %q", s.Storage, s.Action)
	}
	human := e.run(0, "storage", "status").stdout
	for _, want := range []string{"compatible", "schema", "hop.top/kit@"} {
		if !strings.Contains(human, want) {
			t.Errorf("human status lacks %q:\n%s", want, human)
		}
	}
}

// TestStorageBackupRestoreRoundTrip: backup → mutate → restore brings
// back exactly the backed-up data; the backup is an owner-only file
// whose path is reported.
func TestStorageBackupRestoreRoundTrip(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	seeded := e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")

	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	if !strings.HasPrefix(b.Path, e.backup) || b.Schema != umapg.SchemaVersion || b.Reason != "manual" {
		t.Fatalf("backup = %+v", b)
	}
	fi, err := os.Stat(b.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 0600", fi.Mode().Perm())
	}
	// Human output is the path alone, for scripts.
	if p := strings.TrimSpace(e.run(0, "storage", "backup").stdout); !strings.HasPrefix(p, e.backup) || strings.Contains(p, "\n") {
		t.Errorf("human backup output = %q", p)
	}

	later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
	r := decode[restoreOut](t, e.run(0, "storage", "restore", b.Path, "--confirm=yes", "--format", "json").stdout)
	if r.Restored != b.Path || r.Schema != umapg.SchemaVersion || r.Compat != string(storage.CompatOK) {
		t.Errorf("restore = %+v", r)
	}
	if r.Safety == nil || !strings.HasPrefix(r.Safety.Path, e.backup) || r.Safety.Reason != "pre-restore" {
		t.Fatalf("no pre-restore backup of the replaced data: %+v", r.Safety)
	}
	if !e.hasContact(seeded) {
		t.Error("backed-up contact missing after restore")
	}
	if e.hasContact(later) {
		t.Error("contact written after the backup survived the restore")
	}
	// The pre-restore backup holds what the restore replaced.
	e.run(0, "storage", "restore", r.Safety.Path, "--confirm=yes")
	if !e.hasContact(later) {
		t.Error("pre-restore backup lost the replaced data")
	}

	// A missing backup is not found, and nothing is touched.
	r2 := e.heo("storage", "restore", b.Path+".missing", "--confirm=yes", "--format", "json")
	e.want(r2, 3)
	if !e.hasContact(later) {
		t.Error("failed restore changed the store")
	}
}

// TestStorageRestoreNeedsConfirm: restore is destructive; without a
// terminal and without --confirm it is refused before anything runs.
func TestStorageRestoreNeedsConfirm(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
	before := len(e.backups())

	r := e.heo("storage", "restore", b.Path, "--format", "json")
	e.want(r, 5, "storage", "restore")
	if env := errEnvelope(t, r.stderr); env.Code != "UNAUTHORIZED" || !strings.Contains(env.Message, "confirm") {
		t.Errorf("refusal = %+v", env)
	}
	if !e.hasContact(later) {
		t.Error("unconfirmed restore replaced the data")
	}
	if n := len(e.backups()); n != before {
		t.Errorf("unconfirmed restore wrote %d backups", n-before)
	}
	e.want(e.heo("storage", "restore", b.Path, "--confirm=no"), 5)
	if !e.hasContact(later) {
		t.Error("--confirm=no restore replaced the data")
	}
}

// TestStorageMigrateBacksUpFirst: an older store is backed up, then
// migrated; the result reports from → to and the backup.
func TestStorageMigrateBacksUpFirst(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	ct := e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
	e.srv.Downgrade(t, e.dsn)
	old := e.schema()

	m := decode[migrateOut](t, e.run(0, "storage", "migrate", "--format", "json").stdout)
	if m.From != old || m.To != umapg.SchemaVersion {
		t.Errorf("migrate = %+v, want %d → %d", m, old, umapg.SchemaVersion)
	}
	if m.Backup == nil || m.Backup.Schema != old || m.Backup.Reason != storage.ReasonPreMigrate {
		t.Fatalf("older store migrated without a pre-migrate backup: %+v", m.Backup)
	}
	if _, err := os.Stat(m.Backup.Path); err != nil {
		t.Fatalf("backup file: %v", err)
	}
	if !e.hasContact(ct) {
		t.Error("data lost across migration")
	}
	// Current store: nothing to do, no backup.
	n := len(e.backups())
	m = decode[migrateOut](t, e.run(0, "storage", "migrate", "--format", "json").stdout)
	if m.From != m.To || m.Backup != nil || len(e.backups()) != n {
		t.Errorf("no-op migrate = %+v", m)
	}
	if out := e.run(0, "storage", "migrate").stdout; !strings.Contains(out, "current") {
		t.Errorf("human no-op migrate = %q", out)
	}
}

// TestStorageMigrateRefusesNewerSchema: a store a newer heo migrated is
// never downgraded: refused before any backup or change.
func TestStorageMigrateRefusesNewerSchema(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	e.srv.MarkNewer(t, e.dsn)
	newer := e.schema()

	r := e.heo("storage", "migrate", "--format", "json")
	e.want(r, 1, "storage", "migrate")
	env := errEnvelope(t, r.stderr)
	// heo's own refusal, in heo's terms, before the store library's.
	if env.Code != "SCHEMA_MISMATCH" || !strings.Contains(env.Message, "newer") || !strings.Contains(env.Message, "upgrade heo") ||
		!strings.Contains(env.SuggestedFix, "heo upgrade") {
		t.Errorf("refusal = %+v", env)
	}
	if got := e.schema(); got != newer {
		t.Errorf("schema = %d after refusal, want %d", got, newer)
	}
	if b := e.backups(); len(b) != 0 {
		t.Errorf("refused migration wrote backups: %v", b)
	}
	// The dry run refuses the same way.
	e.want(e.heo("storage", "migrate", "--dry-run"), 1)
	s := decode[statusOut](t, e.run(0, "storage", "status", "--format", "json").stdout)
	if s.Storage.Compat != string(storage.CompatNewer) || !strings.Contains(s.Action, "heo upgrade") {
		t.Errorf("status = %+v", s)
	}
}

// TestStorageFailedMigrationRecoverable: a failed migration names its
// backup and leaves the store at the old schema; once the cause is
// fixed, restore + migrate recover it.
func TestStorageFailedMigrationRecoverable(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	ct := e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
	e.srv.Downgrade(t, e.dsn)
	old := e.schema()
	repair := e.srv.BreakMigrations(t, e.dsn)

	r := e.heo("storage", "migrate", "--format", "json")
	if r.exit == 0 {
		repair()
		t.Fatal("migration succeeded on a store that refuses schema changes")
	}
	env := errEnvelope(t, r.stderr)
	b := e.backups()
	if len(b) != 1 || !strings.Contains(env.Message, b[0]) || !strings.Contains(env.SuggestedFix, "heo storage restore") {
		repair()
		t.Fatalf("failure %+v does not name the backup %v", env, b)
	}
	if got := e.schema(); got != old {
		t.Errorf("schema = %d after failed migration, want %d", got, old)
	}
	repair()
	e.run(0, "storage", "restore", b[0], "--confirm=yes")
	e.run(0, "storage", "migrate")
	if got := e.schema(); got != umapg.SchemaVersion {
		t.Errorf("schema = %d after recovery", got)
	}
	if !e.hasContact(ct) {
		t.Error("data lost after recovery")
	}
}

// TestStorageCommandsRefuseOwnedStore: while a daemon owns the store,
// every ownership-taking leaf is refused with CONFLICT and told to stop
// it; nothing is written.
func TestStorageCommandsRefuseOwnedStore(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	n := len(e.backups())
	_, _ = e.owner()
	for _, args := range [][]string{
		{"storage", "backup"},
		{"storage", "migrate"},
		{"storage", "restore", b.Path, "--confirm=yes"},
		{"storage", "backup", "--dry-run"},
		{"storage", "restore", b.Path, "--dry-run"},
	} {
		r := e.heo(append(args, "--format", "json")...)
		e.want(r, 4, args...)
		env := errEnvelope(t, r.stderr)
		if env.Code != "CONFLICT" || !strings.Contains(env.SuggestedFix, "heo service stop") ||
			!strings.Contains(env.Message, "heo/test-daemon") {
			t.Errorf("heo %v refusal = %+v", args, env)
		}
	}
	if got := len(e.backups()); got != n {
		t.Errorf("refused commands wrote %d backups", got-n)
	}
}

// TestStorageDryRunChangesNothing: every write previews a plan and
// changes neither the store nor the backup directory.
func TestStorageDryRunChangesNothing(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
	e.srv.Downgrade(t, e.dsn)
	old := e.schema()
	n := len(e.backups())

	p := decode[planOut](t, e.run(0, "storage", "migrate", "--dry-run", "--format", "json").stdout)
	if p.Command != "heo storage migrate" || len(p.Effects) != 2 ||
		p.Effects[0].Kind != "create" || p.Effects[1].Kind != "migrate" {
		t.Errorf("migrate plan = %+v", p)
	}
	p = decode[planOut](t, e.run(0, "storage", "backup", "--dry-run", "--format", "json").stdout)
	if len(p.Effects) != 1 || p.Effects[0].Kind != "create" || !strings.HasPrefix(p.Effects[0].Target, e.backup) {
		t.Errorf("backup plan = %+v", p)
	}
	// No confirmation is needed to preview a restore.
	p = decode[planOut](t, e.run(0, "storage", "restore", b.Path, "--dry-run", "--format", "json").stdout)
	if len(p.Effects) != 3 || p.Effects[1].Kind != "replace" || !strings.Contains(p.Effects[1].Detail, b.Path) ||
		p.Effects[2].Kind != "replace" || !strings.HasPrefix(p.Effects[2].Target, "blobs:") {
		t.Errorf("restore plan = %+v", p)
	}
	if got := e.schema(); got != old {
		t.Errorf("schema %d after dry runs, want %d", got, old)
	}
	if got := len(e.backups()); got != n {
		t.Errorf("dry runs wrote %d backups", got-n)
	}
	e.run(0, "storage", "migrate")
	if !e.hasContact(later) {
		t.Error("dry runs changed the data")
	}
}
