package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/postgres"
	kitconfig "hop.top/kit/go/core/config"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const storageSeed = "pg-seed-91c4" //nolint:gosec // G101: test credential

// loadStorageProfile loads the default profile's configuration in a
// temp HOME with the file credential backend and the given
// storage.postgres overrides, the way the command tree does.
func loadStorageProfile(t *testing.T, pg map[string]any) (*storage.Registry, *viper.Viper, profile.Ref) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(home, "run"))
	ref := profile.Default()
	in := settings.Inputs{Profile: ref, EnvPrefix: "HEOSTORAGETEST"}
	var err error
	if in.SystemPath, in.UserPath, in.ProfilePath, err = settings.Paths(ref); err != nil {
		t.Fatal(err)
	}
	in.SystemPath = filepath.Join(home, "etc", "heo", "config.yaml")
	reg := newStorageRegistry()
	in.ExtraDefaults = reg.Defaults()
	in.Overrides = map[string]any{
		"credentials": map[string]any{"backend": credentials.BackendFile},
		"storage":     map[string]any{"postgres": pg},
	}
	v := viper.New()
	if err := kitconfig.BindEnv(v, in.EnvPrefix); err != nil {
		t.Fatal(err)
	}
	if _, err := settings.Load(v, in); err != nil {
		t.Fatalf("load: %v", err)
	}
	return reg, v, ref
}

// storeCredential writes a password envelope under ref through the
// profile's own credential backend, and returns that backend.
func storeCredential(t *testing.T, v *viper.Viper, p profile.Ref, ref string) *credentials.Backend {
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
	env, err := json.Marshal(map[string]any{"v": 1, "kind": "password", "user": "heo_app", "password": storageSeed})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SurfaceTokens().Set(ctx, key, env); err != nil {
		t.Fatal(err)
	}
	return b
}

// unreachable is a DSN without a password whose server refuses at
// once: Connect gets past credential resolution and stops there.
const unreachable = "host=127.0.0.1 port=1 dbname=heo connect_timeout=2"

// TestStoragePasswordRefResolvesThroughProfileBackend: the postgres
// driver's password_ref resolves through the profile's credential
// backend, the one channels use, for purpose storage.postgres.
func TestStoragePasswordRefResolvesThroughProfileBackend(t *testing.T) {
	ctx := context.Background()
	reg, v, p := loadStorageProfile(t, map[string]any{"dsn": unreachable, "password_ref": "heo/pg"})
	storeCredential(t, v, p, "heo/pg")

	d, cfg, err := storageConfig(ctx, reg, v, p)
	if err != nil {
		t.Fatalf("storageConfig: %v", err)
	}
	if d.Name() != postgres.Name || cfg.BackupDir != v.GetString(settings.KeyStorageBackupDir) || cfg.Owner == "" {
		t.Errorf("driver %q, config %+v", d.Name(), cfg)
	}
	if cfg.Credentials == nil {
		t.Fatal("no credential backend passed to the storage driver")
	}
	c, err := cfg.Credentials.Resolve(ctx, storage.CredentialRequest{
		Reference: "heo/pg",
		Purpose:   postgres.PasswordPurpose,
		Accept:    []ports.CredentialKind{ports.KindPassword, ports.KindSecret},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if pw, ok := c.(ports.Password); !ok || string(pw.Secret) != storageSeed || pw.User != "heo_app" {
		t.Fatalf("resolved %T, want the stored password", c)
	}

	// Connect resolves the reference, then fails only on the network.
	_, err = d.Connect(ctx, cfg)
	if model.CodeOf(err) != model.Unavailable || !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("connect = %v, want unavailable past credential resolution", err)
	}
	if err != nil && strings.Contains(err.Error(), storageSeed) {
		t.Errorf("connect error leaks the password: %v", err)
	}
}

// TestStorageMissingPasswordRefDiagnosed: a reference with nothing
// stored fails Connect with the credential backend's diagnosis (status,
// store key, fix) and no material.
func TestStorageMissingPasswordRefDiagnosed(t *testing.T) {
	ctx := context.Background()
	reg, v, p := loadStorageProfile(t, map[string]any{"dsn": unreachable, "password_ref": "heo/absent"})
	b := storeCredential(t, v, p, "heo/pg")
	key, err := b.Key("heo/absent")
	if err != nil {
		t.Fatal(err)
	}

	d, cfg, err := storageConfig(ctx, reg, v, p)
	if err != nil {
		t.Fatalf("storageConfig: %v", err)
	}
	_, err = d.Connect(ctx, cfg)
	if model.CodeOf(err) != model.NotFound {
		t.Fatalf("connect = %v, want not_found", err)
	}
	msg := err.Error()
	for _, want := range []string{
		`"heo/absent"`, postgres.PasswordPurpose, string(credentials.StatusMissing),
		"no credential stored", "store a new credential in the file ",
		filepath.FromSlash(key) + "@" + postgres.PasswordPurpose + " (this purpose only)",
		filepath.FromSlash(key) + " (every purpose)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, storageSeed) {
		t.Errorf("diagnostic leaks material: %s", msg)
	}
}
