package storage_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// fakeDriver checks settings: "bad" must be empty.
type fakeDriver struct{ name string }

func (d fakeDriver) Name() string             { return d.name }
func (d fakeDriver) Defaults() map[string]any { return map[string]any{"bad": "", "level": 3} }
func (d fakeDriver) Validate(s storage.Settings) []storage.Problem {
	if s.GetString("bad") != "" {
		return []storage.Problem{{Key: "bad", Message: "must be empty"}}
	}
	return nil
}

func (d fakeDriver) Connect(context.Context, storage.Config) (storage.Conn, error) {
	return nil, errors.New("not connectable")
}

func TestRegistryExplicitRegistration(t *testing.T) {
	r := storage.NewRegistry()
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("new registry has drivers %v: registration must be explicit", names)
	}
	for _, d := range []storage.Driver{fakeDriver{"zeta"}, fakeDriver{"alpha"}} {
		if err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(fakeDriver{"alpha"}); err == nil {
		t.Error("duplicate name registered")
	}
	for _, bad := range []string{"", "Pg", "pg-x", "1pg"} {
		if err := r.Register(fakeDriver{bad}); err == nil {
			t.Errorf("malformed name %q registered", bad)
		}
	}
	if err := r.Register(nil); err == nil {
		t.Error("nil driver registered")
	}
	if got := r.Names(); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Errorf("Names = %v", got)
	}
	if _, err := r.Lookup("sqlite"); err == nil || !strings.Contains(err.Error(), `"sqlite"`) ||
		!strings.Contains(err.Error(), "alpha, zeta") {
		t.Errorf("Lookup unknown = %v, want the name and every registered driver", err)
	}
	if got := r.Defaults(); got["storage.alpha.level"] != 3 || got["storage.zeta.bad"] != "" || len(got) != 4 {
		t.Errorf("Defaults = %v", got)
	}
}

func TestRegistryCheck(t *testing.T) {
	r := storage.NewRegistry()
	if err := r.Register(fakeDriver{"alpha"}); err != nil {
		t.Fatal(err)
	}
	v := viper.New()
	v.Set(storage.KeyDriver, "sqlite")
	issues := r.Check(v)
	if len(issues) != 1 || issues[0].Key != storage.KeyDriver || issues[0].Warning ||
		!strings.Contains(issues[0].Message, "registered drivers: alpha") {
		t.Fatalf("unknown driver issues = %+v", issues)
	}
	v.Set(storage.KeyDriver, "alpha")
	if issues := r.Check(v); len(issues) != 0 {
		t.Fatalf("valid config issues = %+v", issues)
	}
	v.Set("storage.alpha.bad", "x")
	issues = r.Check(v)
	if len(issues) != 1 || issues[0].Key != "storage.alpha.bad" {
		t.Fatalf("driver issues = %+v, want keyed storage.alpha.bad", issues)
	}
	d, s, err := r.Select(v)
	if err != nil || d.Name() != "alpha" || s.GetString("bad") != "x" {
		t.Errorf("Select = %v, %v, %v", d, s, err)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		schema, lib int64
		want        storage.Compat
	}{
		{0, 12, storage.CompatUninitialized},
		{11, 12, storage.CompatMigrate},
		{1, 12, storage.CompatMigrate},
		{12, 12, storage.CompatOK},
		{13, 12, storage.CompatNewer},
	} {
		if got := storage.Classify(tc.schema, tc.lib); got != tc.want {
			t.Errorf("Classify(%d, %d) = %s, want %s", tc.schema, tc.lib, got, tc.want)
		}
	}
}

// fakeOwner records the calls the orchestrator makes.
type fakeOwner struct {
	schema, lib int64
	calls       []string
	backupErr   error
	migrateErr  error
}

func (o *fakeOwner) Versions(context.Context) (storage.Versions, error) {
	return storage.Versions{Driver: "fake", Schema: o.schema, LibrarySchema: o.lib, Library: "fake@v1"}, nil
}
func (o *fakeOwner) Verify(context.Context) error { return nil }
func (o *fakeOwner) Backup(_ context.Context, reason string) (storage.Backup, error) {
	o.calls = append(o.calls, "backup:"+reason)
	if o.backupErr != nil {
		return storage.Backup{}, o.backupErr
	}
	return storage.Backup{Path: "/backups/b1.dump", Schema: o.schema, Reason: reason}, nil
}
func (o *fakeOwner) Restore(context.Context, string) error {
	o.calls = append(o.calls, "restore")
	return nil
}
func (o *fakeOwner) Migrate(context.Context) error {
	o.calls = append(o.calls, "migrate")
	if o.migrateErr != nil {
		return o.migrateErr
	}
	o.schema = o.lib
	return nil
}
func (o *fakeOwner) Open(context.Context) (store.Store, error) {
	o.calls = append(o.calls, "open")
	return memory.New(memory.Options{}), nil
}
func (o *fakeOwner) Release(context.Context) error { return nil }

func TestMigrateOrchestration(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		owner       fakeOwner
		wantCalls   []string
		wantCode    model.ErrorCode
		wantBackup  bool
		wantFrom    int64
		wantTo      int64
		wantMessage string
	}{
		{name: "current store untouched", owner: fakeOwner{schema: 12, lib: 12}, wantFrom: 12, wantTo: 12},
		{name: "fresh store initialized without backup", owner: fakeOwner{schema: 0, lib: 12},
			wantCalls: []string{"migrate"}, wantFrom: 0, wantTo: 12},
		{name: "older store backed up first", owner: fakeOwner{schema: 11, lib: 12},
			wantCalls: []string{"backup:pre-migrate", "migrate"}, wantBackup: true, wantFrom: 11, wantTo: 12},
		{name: "newer store refused before any write", owner: fakeOwner{schema: 13, lib: 12},
			wantCode: model.SchemaMismatch, wantFrom: 13, wantTo: 13, wantMessage: "newer than 12"},
		{name: "failed backup stops the migration",
			owner:     fakeOwner{schema: 11, lib: 12, backupErr: model.NewError(model.Unavailable, "disk full")},
			wantCalls: []string{"backup:pre-migrate"}, wantCode: model.Unavailable, wantFrom: 11, wantTo: 11,
			wantMessage: "nothing was migrated"},
		{name: "failed migration names the backup",
			owner:     fakeOwner{schema: 11, lib: 12, migrateErr: model.NewError(model.Permanent, "boom")},
			wantCalls: []string{"backup:pre-migrate", "migrate"}, wantCode: model.Permanent, wantBackup: true,
			wantFrom: 11, wantTo: 11, wantMessage: "/backups/b1.dump"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := tc.owner
			res, err := storage.Migrate(ctx, &o)
			if !slices.Equal(o.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", o.calls, tc.wantCalls)
			}
			if got := model.CodeOf(err); got != tc.wantCode || (tc.wantCode != "" && err == nil) {
				t.Errorf("error = %v, want code %q", err, tc.wantCode)
			}
			if tc.wantMessage != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMessage)) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantMessage)
			}
			if (res.Backup != nil) != tc.wantBackup {
				t.Errorf("backup = %+v, want present %v", res.Backup, tc.wantBackup)
			}
			if res.From != tc.wantFrom || res.To != tc.wantTo {
				t.Errorf("result = %+v, want %d -> %d", res, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestMigrateDetectsShortMigration(t *testing.T) {
	o := &shortOwner{fakeOwner{schema: 10, lib: 12}}
	_, err := storage.Migrate(context.Background(), o)
	if model.CodeOf(err) != model.SchemaMismatch {
		t.Fatalf("migration that stopped short: error %v, want schema_mismatch", err)
	}
}

// shortOwner's migration stops one version short.
type shortOwner struct{ fakeOwner }

func (o *shortOwner) Migrate(context.Context) error { o.schema = o.lib - 1; return nil }

func TestOpenChecksCompatibilityFirst(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		schema int64
		code   model.ErrorCode
		compat storage.Compat
	}{
		{12, "", storage.CompatOK},
		{11, model.SchemaMismatch, storage.CompatMigrate},
		{0, model.SchemaMismatch, storage.CompatUninitialized},
		{13, model.SchemaMismatch, storage.CompatNewer},
	} {
		o := &fakeOwner{schema: tc.schema, lib: 12}
		st, rep, err := storage.Open(ctx, o)
		if model.CodeOf(err) != tc.code || rep.Compat != tc.compat {
			t.Errorf("schema %d: Open = %v, %+v; want code %q, compat %s", tc.schema, err, rep, tc.code, tc.compat)
		}
		opened := slices.Contains(o.calls, "open")
		if (tc.code == "") != opened || (st != nil) != opened {
			t.Errorf("schema %d: store opened = %v, want %v", tc.schema, opened, tc.code == "")
		}
	}
}

func TestStatusProvider(t *testing.T) {
	ctx := context.Background()
	rep := storage.Report{Versions: storage.Versions{Driver: "fake", Schema: 3, LibrarySchema: 3}, Compat: storage.CompatOK}
	sec, err := storage.StatusProvider(func(context.Context) (storage.Report, error) { return rep, nil })(ctx)
	if err != nil || sec.Title != storage.StatusTitle || sec.Status != "ok" || sec.Data != rep {
		t.Errorf("section = %+v, %v", sec, err)
	}
	sec, err = storage.StatusProvider(func(context.Context) (storage.Report, error) {
		return storage.Report{}, model.NewError(model.Unavailable, "down")
	})(ctx)
	if err != nil || sec.Status != "error" || !strings.Contains(sec.ErrorMessage, "down") {
		t.Errorf("error section = %+v, %v", sec, err)
	}
}

// TestPlanMigrate: the preview of Migrate makes no call but Versions,
// and refuses a newer schema with the same error Migrate returns.
func TestPlanMigrate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		schema     int64
		want       storage.MigratePlan
		wantRefuse bool
	}{
		{"current", 12, storage.MigratePlan{From: 12, To: 12, Compat: storage.CompatOK}, false},
		{"fresh", 0, storage.MigratePlan{From: 0, To: 12, Compat: storage.CompatUninitialized}, false},
		{"older", 11, storage.MigratePlan{From: 11, To: 12, Compat: storage.CompatMigrate, Backup: true}, false},
		{"newer", 13, storage.MigratePlan{From: 13, To: 13, Compat: storage.CompatNewer}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &fakeOwner{schema: tc.schema, lib: 12}
			p, err := storage.PlanMigrate(ctx, o)
			if len(o.calls) != 0 {
				t.Errorf("plan made calls %v", o.calls)
			}
			if p != tc.want {
				t.Errorf("plan = %+v, want %+v", p, tc.want)
			}
			if tc.wantRefuse {
				_, merr := storage.Migrate(ctx, &fakeOwner{schema: tc.schema, lib: 12})
				if model.CodeOf(err) != model.SchemaMismatch || err.Error() != merr.Error() {
					t.Errorf("plan error %v, want Migrate's %v", err, merr)
				}
			} else if err != nil {
				t.Errorf("plan error %v", err)
			}
		})
	}
}
