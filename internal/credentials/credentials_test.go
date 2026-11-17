package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/profile"
	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/memory"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// seed marks credential material in tests: it must never show up in an
// error, a diagnosis or anything else heo renders.
const seed = "SEEKRIT-7f3a9c"

func envelope(t *testing.T, password string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": 1, "kind": "password", "user": "support@example.com", "password": password})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var native = profile.Default()

func fileConfig(t *testing.T) Config {
	t.Helper()
	return Config{Backend: BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"}
}

func open(t *testing.T, ref profile.Ref, cfg Config) *Backend {
	t.Helper()
	b, err := Open(context.Background(), Options{Profile: ref, Tenant: model.TenantID(ref.Dir()), Config: cfg})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return b
}

// store writes value under the kit key heo resolves ref to.
func store(t *testing.T, b *Backend, ref string, value []byte) {
	t.Helper()
	key, err := b.Key(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SurfaceTokens().Set(context.Background(), key, value); err != nil {
		t.Fatal(err)
	}
}

func password(t *testing.T, c ports.Credential) string {
	t.Helper()
	p, ok := c.(ports.Password)
	if !ok {
		t.Fatalf("credential is %T, want ports.Password", c)
	}
	return string(p.Secret)
}

var acceptPassword = []ports.CredentialKind{ports.KindPassword}

func req(ref string, fresh bool) Request {
	return Request{Reference: ref, Purpose: "imap", Accept: acceptPassword, Fresh: fresh}
}

// noSeed fails when any rendering of v carries the seed.
func noSeed(t *testing.T, what string, v any) {
	t.Helper()
	for _, s := range []string{fmt.Sprint(v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
		if strings.Contains(s, seed) {
			t.Errorf("%s leaks credential material: %s", what, s)
		}
	}
	if b, err := json.Marshal(v); err == nil && strings.Contains(string(b), seed) {
		t.Errorf("%s leaks credential material as JSON: %s", what, b)
	}
}

func TestUnknownBackendListsSupported(t *testing.T) {
	for _, name := range []string{"env", "memory", "ghsecrets", "aps", "Keyring", ""} {
		cfg := Config{Backend: name, KeyringService: "heo", FileDir: "/x", OpenbaoMount: "secret"}
		var got *Problem
		for _, p := range cfg.Validate() {
			if p.Key == KeyBackend && !p.Warning {
				got = &p
			}
		}
		if got == nil {
			t.Errorf("backend %q: no error on %s", name, KeyBackend)
			continue
		}
		if !strings.Contains(got.Message, "supported: agefile, file, keyring, openbao") {
			t.Errorf("backend %q: message %q does not list the supported backends", name, got.Message)
		}
		if _, err := Open(context.Background(), Options{Profile: native, Tenant: "default", Config: cfg}); err == nil {
			t.Errorf("backend %q: Open succeeded", name)
		}
	}
}

func TestValidateBackendSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		key  string
	}{
		{"keyring service", Config{Backend: BackendKeyring}, KeyKeyringService},
		{"file dir relative", Config{Backend: BackendFile, FileDir: "secrets"}, KeyFileDir},
		{"agefile path", Config{Backend: BackendAgefile, AgefileIdentity: "/id"}, KeyAgefilePath},
		{"agefile identity", Config{Backend: BackendAgefile, AgefilePath: "/s.age"}, KeyAgefileIdentity},
		{"openbao addr", Config{Backend: BackendOpenbao, KeyringService: "heo", OpenbaoMount: "secret"}, KeyOpenbaoAddr},
		{"openbao addr scheme", Config{Backend: BackendOpenbao, KeyringService: "heo", OpenbaoAddr: "ftp://bao", OpenbaoMount: "secret"}, KeyOpenbaoAddr},
		{"openbao userinfo", Config{Backend: BackendOpenbao, KeyringService: "heo", OpenbaoAddr: "https://u:" + seed + "@bao", OpenbaoMount: "secret"}, KeyOpenbaoAddr},
		{"openbao mount", Config{Backend: BackendOpenbao, KeyringService: "heo", OpenbaoAddr: "https://bao"}, KeyOpenbaoMount},
	} {
		var hit bool
		for _, p := range tc.cfg.Validate() {
			noSeed(t, tc.name, p)
			if p.Key == tc.key && !p.Warning {
				hit = true
			}
		}
		if !hit {
			t.Errorf("%s: no error on %s in %+v", tc.name, tc.key, tc.cfg.Validate())
		}
	}
	ok := Config{Backend: BackendOpenbao, KeyringService: "heo", OpenbaoAddr: "https://bao.internal:8200", OpenbaoMount: "secret"}
	if p := ok.Validate(); len(p) != 0 {
		t.Errorf("valid openbao config: %+v", p)
	}
}

// TestNoEnvSecretBackendLinked keeps kit's environment-variable secret
// backends out of the binary: a credential must come from the store
// the profile configures, never from whatever the process inherited.
func TestNoEnvSecretBackendLinked(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list")
	}
	out, err := exec.Command("go", "list", "-buildvcs=false", "-deps", "hop.top/heo").CombinedOutput() //nolint:gosec // fixed arguments
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	deps := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		deps[strings.TrimSpace(l)] = true
	}
	if !deps["hop.top/uma/contrib/kit/secret"] {
		t.Fatal("heo does not link uma's kit secret bridge; the dependency list is not the one expected")
	}
	for _, banned := range []string{
		"hop.top/kit/go/storage/secret/env",
		"hop.top/kit/go/storage/secret/ghsecrets", // falls back to os.Getenv
	} {
		if deps[banned] {
			t.Errorf("heo links %s", banned)
		}
	}
}

func TestCredentialRotationAndRevocation(t *testing.T) {
	ctx := context.Background()
	b := open(t, native, fileConfig(t))
	store(t, b, "support", envelope(t, "one"))

	c, err := b.Resolve(ctx, req("support", false))
	if err != nil {
		t.Fatal(err)
	}
	if got := password(t, c); got != "one" {
		t.Fatalf("first resolve = %q", got)
	}

	// Rotated in the store: the next attempt sees it, no cache.
	store(t, b, "support", envelope(t, "two"))
	if c, err = b.Resolve(ctx, req("support", false)); err != nil || password(t, c) != "two" {
		t.Fatalf("after rotation: %v, %v", c, err)
	}
	// Rejected by the provider, then rotated: Fresh returns the new one.
	store(t, b, "support", envelope(t, "three"))
	if c, err = b.Resolve(ctx, req("support", true)); err != nil || password(t, c) != "three" {
		t.Fatalf("fresh after rotation: %v, %v", c, err)
	}

	// Revoked: deleted from the store.
	key, _ := b.Key("support")
	if err := b.SurfaceTokens().Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	_, err = b.Resolve(ctx, req("support", false))
	if model.CodeOf(err) != model.NotFound {
		t.Fatalf("revoked credential: %v, want not_found", err)
	}
	d := b.Check(ctx, req("support", false))
	if d.Status != StatusMissing || d.Reference != "support" || d.Backend != BackendFile {
		t.Errorf("Check after revocation = %+v", d)
	}
	if !strings.Contains(d.SuggestedFix, key) {
		t.Errorf("fix %q does not name the key %s", d.SuggestedFix, key)
	}
}

// TestFreshWithoutRotationIsUnauthorized: a credential the provider
// rejected is not handed back as if it were new.
func TestFreshWithoutRotationIsUnauthorized(t *testing.T) {
	ctx := context.Background()
	b := open(t, native, fileConfig(t))
	store(t, b, "support", envelope(t, seed))

	if _, err := b.Resolve(ctx, req("support", false)); err != nil {
		t.Fatal(err)
	}
	c, err := b.Resolve(ctx, req("support", true))
	if model.CodeOf(err) != model.Unauthorized || c != nil {
		t.Fatalf("fresh without rotation = %v, %v; want unauthorized", c, err)
	}
	noSeed(t, "fresh error", err)

	d := b.Check(ctx, req("support", false))
	if d.Status != StatusRejected || d.RejectedAt == nil {
		t.Errorf("Check after rejection = %+v", d)
	}
	noSeed(t, "rejected diagnosis", d)

	// Another purpose on the same reference is still served: the
	// provider decides on each new connection.
	if _, err := b.Resolve(ctx, Request{Reference: "support", Purpose: "smtp.submission", Accept: acceptPassword}); err != nil {
		t.Errorf("non-fresh resolve after rejection: %v", err)
	}

	store(t, b, "support", envelope(t, "rotated"))
	if c, err = b.Resolve(ctx, req("support", true)); err != nil || password(t, c) != "rotated" {
		t.Fatalf("fresh after rotation: %v, %v", c, err)
	}
	if d := b.Check(ctx, req("support", false)); d.Status != StatusOK || d.Kind != ports.KindPassword {
		t.Errorf("Check after rotation = %+v", d)
	}
}

func TestCheckReportsWithoutMaterial(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	cfg := fileConfig(t)
	b, err := Open(ctx, Options{Profile: native, Tenant: "default", Config: cfg, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		value  string
		accept []ports.CredentialKind
		want   Status
	}{
		"ok":        {`{"v":1,"kind":"password","user":"u","password":"` + seed + `"}`, acceptPassword, StatusOK},
		"expired":   {`{"v":1,"kind":"oauth2","access_token":"` + seed + `","expires_at":"2026-10-11T11:00:00Z"}`, nil, StatusExpired},
		"malformed": {`{"v":1,"kind":"password","user":"u","password":"` + seed + `","` + seed + `":"x"}`, nil, StatusMalformed},
		"notjson":   {seed, nil, StatusMalformed},
		"kind":      {`{"v":1,"kind":"secret","value":"` + seed + `"}`, acceptPassword, StatusUnsupported},
	}
	for name, tc := range cases {
		store(t, b, name, []byte(tc.value))
		d := b.Check(ctx, Request{Reference: name, Purpose: "imap", Accept: tc.accept})
		if d.Status != tc.want {
			t.Errorf("%s: status %s (%s), want %s", name, d.Status, d.Message, tc.want)
		}
		noSeed(t, name+" diagnosis", d)
		if _, err := b.Resolve(ctx, Request{Reference: name, Purpose: "imap", Accept: tc.accept}); err != nil {
			noSeed(t, name+" error", err)
		}
	}
	if d := b.Check(ctx, req("../escape", false)); d.Status != StatusInvalid {
		t.Errorf("bad reference: %+v", d)
	}
	if d := b.Check(ctx, req("nothing", false)); d.Status != StatusMissing {
		t.Errorf("missing reference: %+v", d)
	}
}

func TestChannelRequestForAnotherTenantRefused(t *testing.T) {
	ctx := context.Background()
	b := open(t, native, fileConfig(t))
	store(t, b, "support", envelope(t, "one"))
	creds := b.Channels()
	r := ports.CredentialRequest{Channel: model.ChannelRef{Tenant: "default", ID: "c1"}, Reference: "support", Purpose: "imap", Accept: acceptPassword}
	if _, err := creds.Resolve(ctx, r); err != nil {
		t.Fatalf("own tenant: %v", err)
	}
	r.Channel.Tenant = "other"
	if _, err := creds.Resolve(ctx, r); model.CodeOf(err) != model.Unauthorized {
		t.Fatalf("other tenant: %v, want unauthorized", err)
	}
}

func apsProfile(t *testing.T, id, secretsEnv string) string {
	t.Helper()
	data := t.TempDir()
	t.Setenv("APS_DATA_PATH", data)
	dir := filepath.Join(data, "profiles", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.yaml"), []byte("id: "+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secrets.env")
	if err := os.WriteFile(path, []byte(secretsEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApsProfileResolvesThroughApsSecrets(t *testing.T) {
	ctx := context.Background()
	ref := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	path := apsProfile(t, "noor", "OTHER=x\nSUPPORT_MAILBOX='"+string(envelope(t, "from-aps"))+"'\n")
	cfg := fileConfig(t)
	b := open(t, ref, cfg)
	if b.Name() != BackendAps {
		t.Fatalf("backend = %s, want aps", b.Name())
	}

	// The kit store holds a different value under the same reference:
	// an aps-bound profile never reads it.
	kitKey := "uma/aps-noor/SUPPORT_MAILBOX"
	if err := b.SurfaceTokens().Set(ctx, kitKey, envelope(t, "from-kit")); err != nil {
		t.Fatal(err)
	}
	c, err := b.Resolve(ctx, req("SUPPORT_MAILBOX", false))
	if err != nil {
		t.Fatal(err)
	}
	if got := password(t, c); got != "from-aps" {
		t.Fatalf("resolved %q, want the aps secret", got)
	}

	// aps rotates the file: the next attempt reads it.
	if err := os.WriteFile(path, []byte("SUPPORT_MAILBOX='"+string(envelope(t, "rotated"))+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = b.Resolve(ctx, req("SUPPORT_MAILBOX", true)); err != nil || password(t, c) != "rotated" {
		t.Fatalf("after aps rotation: %v, %v", c, err)
	}

	// References are aps secret names.
	if d := b.Check(ctx, req("support/imap", false)); d.Status != StatusInvalid {
		t.Errorf("non-name reference: %+v", d)
	}
	d := b.Check(ctx, req("MISSING", false))
	if d.Status != StatusMissing || !strings.Contains(d.SuggestedFix, path) {
		t.Errorf("missing aps secret: %+v", d)
	}

	// A broken secrets file: godotenv quotes the offending text, heo
	// never does.
	if err := os.WriteFile(path, []byte("BAD LINE "+seed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = b.Resolve(ctx, req("SUPPORT_MAILBOX", false))
	if err == nil {
		t.Fatal("resolve from a malformed secrets file succeeded")
	}
	noSeed(t, "aps parse error", err)
	d = b.Check(ctx, req("SUPPORT_MAILBOX", false))
	if d.Status != StatusMalformed {
		t.Errorf("malformed secrets file: %+v", d)
	}
	noSeed(t, "aps parse diagnosis", d)
}

func TestApsSecretsNeverWritten(t *testing.T) {
	ctx := context.Background()
	ref := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	path := apsProfile(t, "noor", "A=1\n")
	cfg := fileConfig(t)
	b := open(t, ref, cfg)
	if err := b.SurfaceTokens().Set(ctx, SurfaceTokenKey(ref), []byte("tok")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // the test's own temp file
	if err != nil || string(got) != "A=1\n" {
		t.Errorf("aps secrets file changed: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.FileDir, "heo", "aps-noor", "surface-token")); err != nil {
		t.Errorf("surface token not in heo's store: %v", err)
	}
}

func TestSurfaceTokenKey(t *testing.T) {
	for ref, want := range map[profile.Ref]string{
		profile.Default():                       "heo/default/surface-token",
		{Scheme: profile.HeoScheme, ID: "work"}: "heo/work/surface-token",
		{Scheme: profile.ApsScheme, ID: "noor"}: "heo/aps-noor/surface-token",
	} {
		if got := SurfaceTokenKey(ref); got != want {
			t.Errorf("SurfaceTokenKey(%s) = %s, want %s", ref, got, want)
		}
	}
}

// stubKeyring replaces the OS keyring so no test touches a real
// keychain.
func stubKeyring(t *testing.T) map[string]*memory.Store {
	t.Helper()
	services := map[string]*memory.Store{}
	prev := newKeyring
	newKeyring = func(service string) kitsecret.MutableStore {
		if services[service] == nil {
			services[service] = memory.New()
		}
		return services[service]
	}
	t.Cleanup(func() { newKeyring = prev })
	return services
}

func TestKitBackendsConstructedDirectly(t *testing.T) {
	ctx := context.Background()
	keyrings := stubKeyring(t)

	b := open(t, native, Config{Backend: BackendKeyring, KeyringService: "heo-test"})
	store(t, b, "support", envelope(t, "kr"))
	if keyrings["heo-test"] == nil {
		t.Fatal("keyring backend did not use the configured service")
	}
	if c, err := b.Resolve(ctx, req("support", false)); err != nil || password(t, c) != "kr" {
		t.Fatalf("keyring resolve: %v, %v", c, err)
	}

	// openbao's token is a bootstrap secret: read from the keyring,
	// never from configuration.
	bao := Config{Backend: BackendOpenbao, KeyringService: "heo-test", OpenbaoAddr: "http://127.0.0.1:1", OpenbaoMount: "secret"}
	_, err := Open(ctx, Options{Profile: native, Tenant: "default", Config: bao})
	if err == nil || !strings.Contains(err.Error(), OpenbaoTokenKey(native)) {
		t.Fatalf("openbao without token: %v; want an error naming %s", err, OpenbaoTokenKey(native))
	}
	noSeed(t, "openbao open error", err)
	if err := keyrings["heo-test"].Set(ctx, OpenbaoTokenKey(native), []byte(seed)); err != nil {
		t.Fatal(err)
	}
	ob := open(t, native, bao)
	if ob.Name() != BackendOpenbao {
		t.Errorf("backend = %s", ob.Name())
	}
	// Unreachable server: unavailable, fixed text.
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	d := ob.Check(short, req("support", false))
	if d.Status != StatusUnavailable {
		t.Errorf("unreachable openbao: %+v", d)
	}
	noSeed(t, "openbao diagnosis", d)

	age := open(t, native, Config{Backend: BackendAgefile, AgefilePath: "/nonexistent/s.age", AgefileIdentity: "/nonexistent/id"})
	if d := age.Check(ctx, req("support", false)); d.Status != StatusUnavailable {
		t.Errorf("agefile without files: %+v", d)
	}
}
