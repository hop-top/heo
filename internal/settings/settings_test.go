package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/profile"
	"hop.top/kit/go/console/output"
	kitconfig "hop.top/kit/go/core/config"
)

func exitOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ce *output.Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v (%T) carries no kit exit code", err, err)
	}
	return ce.ExitCode
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixture lays out system, user and profile files in a temp dir.
type fixture struct {
	dir   string
	in    Inputs
	viper *viper.Viper
}

func newFixture(t *testing.T, ref profile.Ref) *fixture {
	t.Helper()
	dir := t.TempDir()
	in := Inputs{
		Profile:     ref,
		SystemPath:  filepath.Join(dir, "etc", "heo", "config.yaml"),
		UserPath:    filepath.Join(dir, "user", "heo", "config.yaml"),
		ProfilePath: filepath.Join(dir, "user", "heo", "profiles", ref.Dir()+".yaml"),
		EnvPrefix:   "HEOTEST",
	}
	v := viper.New()
	if err := kitconfig.BindEnv(v, in.EnvPrefix); err != nil {
		t.Fatal(err)
	}
	return &fixture{dir: dir, in: in, viper: v}
}

func (f *fixture) load(t *testing.T) *State {
	t.Helper()
	s, err := Load(f.viper, f.in)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func entry(t *testing.T, s *State, key string) Entry {
	t.Helper()
	for _, e := range s.Entries() {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry %q in %v", key, s.Entries())
	return Entry{}
}

func TestPrecedenceAndOrigin(t *testing.T) {
	f := newFixture(t, profile.Ref{Scheme: profile.HeoScheme, ID: "work"})
	writeFile(t, f.in.SystemPath, "a: system\nb: system\nc: system\nd: system\ne: system\nf: system\n")
	writeFile(t, f.in.UserPath, "b: user\nc: user\nd: user\ne: user\nf: user\n")
	writeFile(t, f.in.ProfilePath, "c: profile\nd: profile\ne: profile\nf: profile\n")
	extra := writeFile(t, filepath.Join(f.dir, "extra.yaml"), "d: file\ne: file\nf: file\n")
	f.in.Files = []string{extra}
	t.Setenv("HEOTEST_E", "env")
	t.Setenv("HEOTEST_F", "env")
	f.in.Overrides = map[string]any{"f": "override"}

	s := f.load(t)
	want := map[string][3]string{
		"a": {"system", "system", f.in.SystemPath},
		"b": {"user", "user", f.in.UserPath},
		"c": {"profile", "profile", f.in.ProfilePath},
		"d": {"file", "file", extra},
		"e": {"env", "env", "HEOTEST_E"},
		"f": {"override", "override", "-c f"},
	}
	for key, w := range want {
		e := entry(t, s, key)
		if e.Value != w[0] || string(e.Origin) != w[1] || e.Detail != w[2] {
			t.Errorf("%s = {%q %q %q}, want %v", key, e.Value, e.Origin, e.Detail, w)
		}
		if got := f.viper.GetString(key); got != w[0] {
			t.Errorf("viper %s = %q, want %q", key, got, w[0])
		}
	}
	// Built-in defaults report as such.
	if e := entry(t, s, "services.socket.enabled"); e.Origin != OriginDefault || e.Value != "true" {
		t.Errorf("services.socket.enabled = %+v, want default true", e)
	}
	// The profile row reports the selected reference.
	if e := entry(t, s, KeyProfile); e.Value != "heo://profile/work" {
		t.Errorf("profile row = %+v", e)
	}
}

// TestOverrideKeepsSiblings pins that a -c override of one key leaves its
// siblings resolving from lower layers: services.api.addr=x must not hide
// services.socket.*.
func TestOverrideKeepsSiblings(t *testing.T) {
	f := newFixture(t, profile.Default())
	f.in.Overrides = map[string]any{"services": map[string]any{"api": map[string]any{"addr": "127.0.0.1:9999"}}}
	f.load(t)
	if got := f.viper.GetString("services.api.addr"); got != "127.0.0.1:9999" {
		t.Errorf("services.api.addr = %q", got)
	}
	if !f.viper.GetBool("services.socket.enabled") {
		t.Error("services.socket.enabled shadowed by an unrelated -c override")
	}
	if got := f.viper.GetString("services.socket.auth.mode"); got != "peer" {
		t.Errorf("services.socket.auth.mode = %q, want peer", got)
	}
}

func TestProfileKeyRefusedInFilesAndOverrides(t *testing.T) {
	f := newFixture(t, profile.Default())
	writeFile(t, f.in.UserPath, "profile: work\n")
	_, err := Load(f.viper, f.in)
	if got := exitOf(t, err); got != 2 {
		t.Fatalf("profile key in user file: exit %d (%v), want 2", got, err)
	}
	if !strings.Contains(err.Error(), f.in.UserPath) {
		t.Errorf("error %q does not name the file", err)
	}

	f = newFixture(t, profile.Default())
	f.in.Overrides = map[string]any{"profile": "work"}
	if got := exitOf(t, func() error { _, err := Load(f.viper, f.in); return err }()); got != 2 {
		t.Fatalf("-c profile=work: exit %d, want 2", got)
	}
}

func TestMissingProfileFile(t *testing.T) {
	// A named native profile must have its file.
	f := newFixture(t, profile.Ref{Scheme: profile.HeoScheme, ID: "work"})
	f.in.RequireProfileFile = true
	_, err := Load(f.viper, f.in)
	if got := exitOf(t, err); got != 3 {
		t.Fatalf("missing work.yaml: exit %d (%v), want 3", got, err)
	}
	// Unless the command creates it.
	f.in.RequireProfileFile = false
	if _, err := Load(f.viper, f.in); err != nil {
		t.Fatalf("missing work.yaml, not required: %v", err)
	}
	// default and aps-bound profiles need no file.
	for _, ref := range []profile.Ref{profile.Default(), {Scheme: profile.ApsScheme, ID: "noor"}} {
		f := newFixture(t, ref)
		f.in.RequireProfileFile = true
		if _, err := Load(f.viper, f.in); err != nil {
			t.Fatalf("%s without a file: %v", ref, err)
		}
	}
}

func TestDefaultsPerProfile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("XDG_DATA_HOME", "/home/u/.local/share")
	for _, tc := range []struct {
		ref         profile.Ref
		socket, sec string
		backups     string
	}{
		{profile.Default(), "/run/user/1000/heo/default/heo.sock", "/home/u/.local/share/heo/default/secrets", "/home/u/.local/share/heo/default/backups"},
		{profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}, "/run/user/1000/heo/aps-noor/heo.sock", "/home/u/.local/share/heo/aps-noor/secrets", "/home/u/.local/share/heo/aps-noor/backups"},
	} {
		d, err := Defaults(tc.ref)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"services.socket.enabled":                    true,
			"services.socket.auth.mode":                  "peer",
			"services.socket.auth.peer.require_same_uid": true,
			"services.socket.path":                       tc.socket,
			"services.api.enabled":                       false,
			"services.rpc.enabled":                       false,
			"services.mcp.enabled":                       false,
			"services.all.idempotency.enabled":           false,
			"runtime.tenant":                             tc.ref.Dir(),
			"credentials.backend":                        "keyring",
			"credentials.keyring.service":                "heo",
			"credentials.file.dir":                       tc.sec,
			"storage.driver":                             "postgres",
			"storage.backup.dir":                         tc.backups,
			"storage.blobs.dir":                          strings.TrimSuffix(tc.backups, "backups") + "blobs",
		}
		for k, w := range want {
			if d[k] != w {
				t.Errorf("%s: %s = %v, want %v", tc.ref, k, d[k], w)
			}
		}
	}
}

func TestEntriesRedactSecrets(t *testing.T) {
	f := newFixture(t, profile.Default())
	writeFile(t, f.in.UserPath, strings.Join([]string{
		"providers:",
		"  gh:",
		"    note: ghp_1234567890abcdefghijABCDEFGHIJ123456",
		"    password: hunter2",
		"    credential_ref: gh/imap",
		"    url: https://bot:hunter2@git.example.com/x",
		"services:",
		"  api:",
		"    addr: 127.0.0.1:8080",
		"    auth:",
		"      token: plain-looking",
		"",
	}, "\n"))
	s := f.load(t)
	for key, want := range map[string]string{
		"providers.gh.url":            Redacted,
		"providers.gh.note":           Redacted,
		"providers.gh.password":       Redacted,
		"services.api.auth.token":     Redacted,
		"providers.gh.credential_ref": "gh/imap",
		"services.api.addr":           "127.0.0.1:8080",
	} {
		if got := entry(t, s, key).Value; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestChainHighestFirst(t *testing.T) {
	f := newFixture(t, profile.Ref{Scheme: profile.HeoScheme, ID: "work"})
	writeFile(t, f.in.ProfilePath, "a: 1\n")
	extra := writeFile(t, filepath.Join(f.dir, "x.yaml"), "a: 2\n")
	f.in.Files = []string{extra}
	s := f.load(t)
	var got []string
	for _, l := range s.Chain() {
		got = append(got, string(l.Origin)+":"+l.Path)
	}
	want := []string{
		"file:" + extra,
		"profile:" + f.in.ProfilePath,
		"user:" + f.in.UserPath,
		"system:" + f.in.SystemPath,
		"default:<defaults>",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Chain() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !s.Chain()[1].Exists || s.Chain()[2].Exists {
		t.Errorf("Exists flags wrong: %+v", s.Chain())
	}
}

func TestValidate(t *testing.T) {
	f := newFixture(t, profile.Default())
	s := f.load(t)
	if issues := Validate(f.viper, s); len(issues) != 0 {
		t.Fatalf("defaults invalid: %+v", issues)
	}

	f = newFixture(t, profile.Default())
	writeFile(t, f.in.UserPath, strings.Join([]string{
		"runtime:",
		"  tenant: ' padded'",
		"services:",
		"  socket:",
		"    path: /" + strings.Repeat("x", 120) + "/heo.sock",
		"  api:",
		"    enabled: true",
		"",
	}, "\n"))
	s = f.load(t)
	issues := Validate(f.viper, s)
	keys := map[string]bool{}
	for _, i := range issues {
		if i.Severity == SeverityError {
			keys[i.Key] = true
		}
	}
	for _, k := range []string{"runtime.tenant", "services.socket.path", "services.api.auth.mode"} {
		if !keys[k] {
			t.Errorf("no error for %s in %+v", k, issues)
		}
	}
}

func TestValidateCredentials(t *testing.T) {
	f := newFixture(t, profile.Default())
	writeFile(t, f.in.ProfilePath, strings.Join([]string{
		"credentials:",
		"  backend: env",
		"  colour: blue",
		"  openbao:",
		"    token: hunter2",
		"",
	}, "\n"))
	s := f.load(t)
	issues := Validate(f.viper, s)
	got := map[string]Issue{}
	for _, i := range issues {
		if i.Severity == SeverityError {
			got[i.Key] = i
		}
		if strings.Contains(i.Message, "hunter2") {
			t.Errorf("issue echoes a credential: %+v", i)
		}
	}
	if i := got["credentials.backend"]; !strings.Contains(i.Message, "supported: agefile, file, keyring, openbao") ||
		i.Origin != OriginProfile {
		t.Errorf("credentials.backend issue = %+v", i)
	}
	if i := got["credentials.colour"]; !strings.Contains(i.Message, "unknown") {
		t.Errorf("credentials.colour issue = %+v", i)
	}
	if i := got["credentials.openbao.token"]; !strings.Contains(i.Message, "never") {
		t.Errorf("credentials.openbao.token issue = %+v", i)
	}

	f = newFixture(t, profile.Default())
	f.in.Overrides = map[string]any{"credentials": map[string]any{"backend": "file"}}
	issues = Validate(f.viper, f.load(t))
	if HasErrors(issues) || len(issues) != 1 || issues[0].Severity != SeverityWarning {
		t.Errorf("file backend: %+v", issues)
	}
}

func TestExtraDefaultsAndChecks(t *testing.T) {
	f := newFixture(t, profile.Default())
	f.in.ExtraDefaults = map[string]any{
		"storage.fake.level": 3,
		// heo's own default wins over a contributed one.
		"storage.driver": "fake",
	}
	writeFile(t, f.in.UserPath, "storage:\n  fake:\n    mode: odd\n")
	s := f.load(t)
	if got := f.viper.GetInt("storage.fake.level"); got != 3 {
		t.Errorf("contributed default = %d, want 3", got)
	}
	if got := f.viper.GetString("storage.driver"); got != DefaultStorageDriver {
		t.Errorf("storage.driver = %q, want heo's default %q", got, DefaultStorageDriver)
	}
	if got := s.Lookup("storage.fake.level"); len(got) != 1 || got[0].Origin != OriginDefault {
		t.Errorf("contributed default entry = %+v", got)
	}
	check := func(v *viper.Viper, report func(Severity, string, string)) {
		if v.GetString("storage.fake.mode") == "odd" {
			report(SeverityError, "storage.fake.mode", "odd is not a mode")
		}
	}
	issues := Validate(f.viper, s, check)
	if len(issues) != 1 || issues[0].Key != "storage.fake.mode" || issues[0].Origin != OriginUser ||
		issues[0].Severity != SeverityError {
		t.Fatalf("check issues = %+v, want one user-origin error", issues)
	}
}

// TestDefaultSocketPathsFitSunPath checks the per-profile socket
// default against the 103-byte unix socket bound for the longest
// profile directories on the runtime bases heo meets.
func TestDefaultSocketPathsFitSunPath(t *testing.T) {
	bases := []string{
		"/Users/" + strings.Repeat("u", 32) + "/Library/Application Support", // darwin, 32-byte user
		"/run/user/4294967294", // linux, widest uid
		"/tmp",                 // fallback
	}
	refs := []profile.Ref{
		profile.Default(),
		{Scheme: profile.HeoScheme, ID: "a" + strings.Repeat("b", 15)},
		{Scheme: profile.ApsScheme, ID: strings.Repeat("c", 16)},
	}
	for _, base := range bases {
		t.Setenv("XDG_RUNTIME_DIR", base)
		for _, ref := range refs {
			p, err := SocketPath(ref)
			if err != nil {
				t.Fatal(err)
			}
			if len(p) > maxSocketPath {
				t.Errorf("%s: %s is %d bytes, over %d", ref, p, len(p), maxSocketPath)
			}
			if !strings.HasSuffix(p, "/heo/"+ref.Dir()+"/heo.sock") {
				t.Errorf("%s: socket %s is not per profile", ref, p)
			}
		}
	}
}

// TestLooksLikeSecretFailsClosed: a value matching a distinctive
// credential pattern counts as one whatever its entropy, and a value
// matching none does not.
func TestLooksLikeSecretFailsClosed(t *testing.T) {
	for s, want := range map[string]bool{
		"ghp_1234567890abcdefghijABCDEFGHIJ123456": true,
		"ghp_" + strings.Repeat("a", 36):           true,
		"127.0.0.1:8080":                           false,
		"gh/imap":                                  false,
	} {
		if got := LooksLikeSecret(s); got != want {
			t.Errorf("LooksLikeSecret(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestValidateHeldOperations(t *testing.T) {
	f := newFixture(t, profile.Default())
	if issues := Validate(f.viper, f.load(t)); len(issues) != 0 {
		t.Fatalf("defaults: %+v", issues)
	}
	if f.viper.GetBool(KeyHeldEnabled) || f.viper.GetString(KeyHeldExpiry) != DefaultHeldExpiry.String() {
		t.Fatalf("defaults = %v, %q", f.viper.GetBool(KeyHeldEnabled), f.viper.GetString(KeyHeldExpiry))
	}
	f = newFixture(t, profile.Default())
	f.in.Overrides = map[string]any{"held_operations": map[string]any{"enabled": true, "expiry": "0s", "on_behalf_of": "x\x00y"}}
	keys := map[string]bool{}
	for _, i := range Validate(f.viper, f.load(t)) {
		if i.Severity == SeverityError {
			keys[i.Key] = true
		}
	}
	if !keys[KeyHeldExpiry] || !keys[KeyHeldOnBehalfOf] {
		t.Fatalf("errors = %v", keys)
	}
}

func TestValidateTrustLadder(t *testing.T) {
	f := newFixture(t, profile.Default())
	f.in.Overrides = map[string]any{"held_operations": map[string]any{"trust": map[string]any{
		"enabled": true, "seed": []string{"read", "archive"},
	}}}
	for _, i := range Validate(f.viper, f.load(t)) {
		if i.Severity == SeverityError {
			t.Fatalf("valid ladder: %+v", i)
		}
	}
	for name, trust := range map[string]map[string]any{
		"seed label":     {"seed": []string{"label"}},
		"seed trash":     {"seed": []string{"trash"}},
		"negative":       {"thresholds": map[string]any{"archive": -1}},
		"unsubscribe":    {"thresholds": map[string]any{"unsubscribe": 3}},
		"supervised bad": {"supervised": map[string]any{"days": -2}},
		"never unsub":    {"never_auto": []string{"unsubscribe"}},
		"never seeded":   {"never_auto": []string{"archive"}, "seed": []string{"archive"}},
		"never zero":     {"never_auto": []string{"attention"}},
		"first reply":    {"inboxes": map[string]any{"in1": map[string]any{"first_reply": "maybe"}}},
		"inbox negative": {"inboxes": map[string]any{"in1": map[string]any{"thresholds": map[string]any{"status": -1}}}},
	} {
		f = newFixture(t, profile.Default())
		f.in.Overrides = map[string]any{"held_operations": map[string]any{"trust": trust}}
		var got bool
		for _, i := range Validate(f.viper, f.load(t)) {
			got = got || i.Severity == SeverityError && i.Key == "held_operations.trust"
		}
		if !got {
			t.Fatalf("%s: no error", name)
		}
	}
}

func TestValidateApprovalChat(t *testing.T) {
	chat := func(actor string) map[string]any {
		return map[string]any{"held_operations": map[string]any{"chat": map[string]any{
			"enabled": true,
			"conversations": []map[string]any{{"inbox": "in1", "conversation": "cv1",
				"members": []map[string]any{{"identity": "idn1", "actor": actor}}}},
		}}}
	}
	f := newFixture(t, profile.Default())
	f.in.Overrides = chat("uid:1001")
	for _, i := range Validate(f.viper, f.load(t)) {
		if i.Severity == SeverityError {
			t.Fatalf("valid chat: %+v", i)
		}
	}
	f = newFixture(t, profile.Default())
	f.in.Overrides = chat("plugin:ops")
	var got bool
	for _, i := range Validate(f.viper, f.load(t)) {
		got = got || i.Severity == SeverityError && i.Key == "held_operations.chat"
	}
	if !got {
		t.Fatal("plugin member: no error")
	}
}
