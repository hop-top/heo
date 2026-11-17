// Package e2e drives the built heo binary: exit codes, streams and
// files are read from a real process, never from `go run`.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	// Linked only so `go test` reruns this package when heo's source
	// changes: the binary under test is built in TestMain, which the
	// test cache cannot see.
	_ "hop.top/heo/cmd"
)

var heoBin string

// runTimeout bounds every one-shot heo run.
const runTimeout = 60 * time.Second

// recordTimeout bounds a run while cassettes are being recorded: a model
// that has to load before it answers takes longer than any other peer.
const recordTimeout = 10 * time.Minute

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "heo-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	heoBin = filepath.Join(dir, "heo")
	if runtime.GOOS == "windows" {
		heoBin += ".exe"
	}
	build := exec.Command("go", "build", "-buildvcs=false", "-o", heoBin, "hop.top/heo") //nolint:gosec // output path is the test's own temp dir
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build heo:", err)
		os.Exit(1)
	}
	code := m.Run()
	stopPostgres()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// sandbox is an isolated HOME, XDG tree, aps data dir and working
// directory for one test.
type sandbox struct {
	t    *testing.T
	root string
	env  map[string]string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	// The control socket path must fit 103 bytes: keep the tree short
	// when the platform temp dir is long (darwin's /var/folders/...).
	base := os.TempDir()
	if len(base) > 32 && runtime.GOOS != "windows" {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "heo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	s := &sandbox{t: t, root: root, env: map[string]string{
		"PATH":            os.Getenv("PATH"),
		"HOME":            filepath.Join(root, "home"),
		"XDG_CONFIG_HOME": filepath.Join(root, "cfg"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"XDG_STATE_HOME":  filepath.Join(root, "state"),
		"XDG_CACHE_HOME":  filepath.Join(root, "cache"),
		"XDG_RUNTIME_DIR": filepath.Join(root, "run"),
		"APS_DATA_PATH":   filepath.Join(root, "aps"),
		"TMPDIR":          os.TempDir(),
	}}
	for _, d := range []string{"home", "cwd"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s *sandbox) path(rel ...string) string {
	return filepath.Join(append([]string{s.root}, rel...)...)
}

func (s *sandbox) write(rel, body string) string {
	s.t.Helper()
	p := s.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil { //nolint:gosec // path is confined to the test sandbox
		s.t.Fatal(err)
	}
	return p
}

type result struct {
	stdout, stderr string
	exit           int
}

// run executes heo with extra environment (KEY=value) and args; stdin
// is empty and neither stream is a terminal.
func (s *sandbox) run(env []string, args ...string) result {
	s.t.Helper()
	return s.runIn("", env, args...)
}

// runIn is run with stdin as the command's standard input.
func (s *sandbox) runIn(stdin string, env []string, args ...string) result {
	s.t.Helper()
	// A command that should return but serves instead fails here
	// rather than hanging the suite.
	limit := runTimeout
	if os.Getenv("XRR_MODE") == "record" {
		limit = recordTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, heoBin, args...) //nolint:gosec // runs the binary TestMain built
	cmd.Dir = s.path("cwd")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	for k, v := range s.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.exit = ee.ExitCode()
	default:
		s.t.Fatalf("run heo %v: %v", args, err)
	}
	return r
}

func (s *sandbox) want(r result, exit int, args ...string) {
	s.t.Helper()
	if r.exit != exit {
		s.t.Fatalf("heo %s: exit %d, want %d\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), r.exit, exit, r.stdout, r.stderr)
	}
}

// get runs `config get <key>` and returns the bare value.
func (s *sandbox) get(env []string, pre []string, key string) result {
	s.t.Helper()
	return s.run(env, append(append([]string{}, pre...), "config", "get", key)...)
}

func (s *sandbox) apsProfile(id string) {
	s.write(filepath.Join("aps", "profiles", id, "profile.yaml"), "id: "+id+"\n")
}

// TestProfileRefGrammarCLI runs the reference table through --profile
// (before and after the subcommand) and through HEO_PROFILE.
func TestProfileRefGrammarCLI(t *testing.T) {
	cases := []struct {
		ref, canonical string
		exit           int
	}{
		{"", "heo://profile/default", 0},
		{"work", "heo://profile/work", 0},
		{"heo://profile/work", "heo://profile/work", 0},
		{"aps:noor", "aps://profile/noor", 0},
		{"aps://profile/noor", "aps://profile/noor", 0},
		{"foo://profile/x", "", 2},
		{"heo://other/work", "", 2},
		{"aps://profile/noor/extra", "", 2},
		{"aps://profile/no%2Fr", "", 2},
		{"heo://profile/work?x=1", "", 2},
		{"aps-noor", "", 2},
		{"Work", "", 2},
	}
	s := newSandbox(t)
	s.apsProfile("noor")
	s.write("cfg/heo/profiles/work.yaml", "runtime:\n  tenant: work\n")
	for _, tc := range cases {
		forms := map[string]struct {
			env  []string
			args []string
		}{
			"flag-before": {nil, []string{"--profile", tc.ref, "config", "get", "profile"}},
			"flag-after":  {nil, []string{"config", "get", "profile", "--profile", tc.ref}},
			"env":         {[]string{"HEO_PROFILE=" + tc.ref}, []string{"config", "get", "profile"}},
		}
		for name, f := range forms {
			t.Run(tc.ref+"/"+name, func(t *testing.T) {
				if tc.ref == "" && name != "env" {
					f.args = []string{"config", "get", "profile"}
				}
				r := s.run(f.env, f.args...)
				s.want(r, tc.exit, f.args...)
				if tc.exit == 0 && strings.TrimSpace(r.stdout) != tc.canonical {
					t.Errorf("profile = %q, want %s", r.stdout, tc.canonical)
				}
				if tc.exit != 0 && r.stdout != "" {
					t.Errorf("failure wrote to stdout: %q", r.stdout)
				}
			})
		}
	}
}

func TestUnknownProfileSchemeListsSupported(t *testing.T) {
	s := newSandbox(t)
	for _, args := range [][]string{
		{"--profile", "foo://profile/x", "config", "list"},
		{"config", "list", "--profile", "foo://profile/x"},
		{"--profile", "foo://profile/x", "status"},
	} {
		r := s.run(nil, args...)
		s.want(r, 2, args...)
		if !strings.Contains(r.stderr, `unsupported profile scheme "foo"; supported: aps, heo`) {
			t.Errorf("heo %v stderr = %q", args, r.stderr)
		}
	}
	// The structured envelope carries the same class.
	r := s.run([]string{"HEO_PROFILE=foo://profile/x"}, "config", "list", "--format", "json")
	s.want(r, 2, "config", "list", "--format", "json")
	var env struct {
		Code     string `json:"code"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(lastJSON(r.stderr)), &env); err != nil || env.Code != "USAGE" || env.ExitCode != 2 {
		t.Errorf("envelope = %+v (%v) from %q", env, err, r.stderr)
	}
}

// lastJSON returns the last {...} block of s.
func lastJSON(s string) string {
	if i := strings.LastIndex(s, "\n{"); i >= 0 {
		return s[i+1:]
	}
	if i := strings.Index(s, "{"); i >= 0 {
		return s[i:]
	}
	return s
}

func TestProfileRefNoImplicitAps(t *testing.T) {
	s := newSandbox(t)
	s.apsProfile("noor")
	r := s.get([]string{"APS_PROFILE=noor"}, nil, "profile")
	s.want(r, 0, "config", "get", "profile")
	if got := strings.TrimSpace(r.stdout); got != "heo://profile/default" {
		t.Fatalf("APS_PROFILE leaked into heo's profile: %s", got)
	}
	// Explicit binding works under the same environment.
	r = s.get([]string{"APS_PROFILE=other", "HEO_PROFILE=aps:noor"}, nil, "profile")
	if got := strings.TrimSpace(r.stdout); got != "aps://profile/noor" {
		t.Fatalf("HEO_PROFILE=aps:noor resolved to %s", got)
	}
}

func TestProfileFlagBeatsEnv(t *testing.T) {
	s := newSandbox(t)
	s.write("cfg/heo/profiles/work.yaml", "")
	r := s.run([]string{"HEO_PROFILE=other"}, "--profile", "work", "config", "get", "profile", "--format", "json")
	s.want(r, 0)
	var e struct{ Value, Origin, Detail string }
	if err := json.Unmarshal([]byte(r.stdout), &e); err != nil {
		t.Fatal(err, r.stdout)
	}
	if e.Value != "heo://profile/work" || e.Origin != "flag" || e.Detail != "--profile" {
		t.Errorf("profile entry = %+v", e)
	}
}

func TestApsProfileMissingNotFound(t *testing.T) {
	s := newSandbox(t)
	for _, f := range []struct {
		env  []string
		args []string
	}{
		{nil, []string{"--profile", "aps:ghost", "config", "list"}},
		{[]string{"HEO_PROFILE=aps://profile/ghost"}, []string{"config", "list"}},
	} {
		r := s.run(f.env, f.args...)
		s.want(r, 3, f.args...)
		if !strings.Contains(r.stderr, "aps://profile/ghost") {
			t.Errorf("stderr does not name the reference: %q", r.stderr)
		}
	}
	// heo never creates aps state.
	if _, err := os.Stat(s.path("aps", "profiles", "ghost")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("aps profile dir created: %v", err)
	}

	// An existing aps profile gets heo state under aps-<id>.
	s.apsProfile("noor")
	r := s.get(nil, []string{"--profile", "aps:noor"}, "services.socket.path")
	s.want(r, 0)
	if want := filepath.Join(s.path("run"), "heo", "aps-noor", "heo.sock"); strings.TrimSpace(r.stdout) != want {
		t.Errorf("socket path = %q, want %s", r.stdout, want)
	}
	r = s.get(nil, []string{"--profile", "aps:noor"}, "runtime.tenant")
	if strings.TrimSpace(r.stdout) != "aps-noor" {
		t.Errorf("tenant = %q, want aps-noor", r.stdout)
	}
}

func TestNamedProfileNeedsItsFile(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "--profile", "work", "config", "list")
	s.want(r, 3, "--profile", "work", "config", "list")
	// set creates it, then the profile resolves.
	r = s.run(nil, "--profile", "work", "config", "set", "runtime.tenant", "acme")
	s.want(r, 0, "config", "set")
	if _, err := os.Stat(s.path("cfg", "heo", "profiles", "work.yaml")); err != nil {
		t.Fatalf("profile file not written: %v", err)
	}
	r = s.get(nil, []string{"--profile", "work"}, "runtime.tenant")
	s.want(r, 0)
	if strings.TrimSpace(r.stdout) != "acme" {
		t.Errorf("tenant = %q", r.stdout)
	}
	// The default profile writes its own file and leaves work alone.
	r = s.run(nil, "config", "set", "runtime.tenant", "home")
	s.want(r, 0)
	if r := s.get(nil, nil, "runtime.tenant"); strings.TrimSpace(r.stdout) != "home" {
		t.Errorf("default tenant = %q", r.stdout)
	}
	if r := s.get(nil, []string{"--profile", "work"}, "runtime.tenant"); strings.TrimSpace(r.stdout) != "acme" {
		t.Errorf("work tenant changed: %q", r.stdout)
	}
}

func TestKitYamlInCwdIgnored(t *testing.T) {
	s := newSandbox(t)
	body := "runtime:\n  tenant: from-kit\nservices:\n  api:\n    enabled: true\nmarker: kit\n"
	s.write("cwd/kit.yaml", body)
	s.write("cwd/.kit.yaml", body)
	s.write("cwd/.kit/config.yaml", body)
	if err := os.MkdirAll(s.path("cwd", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := s.get(nil, nil, "runtime.tenant")
	s.want(r, 0)
	if strings.TrimSpace(r.stdout) != "default" {
		t.Errorf("tenant = %q: a kit project file configured heo", r.stdout)
	}
	s.want(s.get(nil, nil, "marker"), 3)
	r = s.run(nil, "config", "list", "--format", "json")
	s.want(r, 0)
	if strings.Contains(r.stdout, "kit") && strings.Contains(r.stdout, "marker") {
		t.Errorf("config list shows kit project keys: %s", r.stdout)
	}
	r = s.run(nil, "config", "paths")
	if strings.Contains(r.stdout, "kit") {
		t.Errorf("config paths lists a kit project file: %s", r.stdout)
	}
}

func TestConfigListOriginsAndRedaction(t *testing.T) {
	s := newSandbox(t)
	s.write("cfg/heo/config.yaml", strings.Join([]string{
		"services:",
		"  api:",
		"    addr: 127.0.0.1:8080",
		"providers:",
		"  gh:",
		"    note: ghp_1234567890abcdefghijABCDEFGHIJ123456",
		"    password: hunter2",
		"    credential_ref: gh/imap",
		"",
	}, "\n"))
	s.write("cfg/heo/profiles/default.yaml", "runtime:\n  tenant: acme\n")
	extra := s.write("extra.yaml", "services:\n  all:\n    timeouts:\n      command: 1m\n")
	r := s.run([]string{"HEO_SERVICES_API_ENABLED=true"},
		"-c", extra, "-c", "services.rpc.enabled=true", "config", "list", "--format", "json")
	s.want(r, 0)
	var entries []struct{ Key, Value, Origin, Detail string }
	if err := json.Unmarshal([]byte(r.stdout), &entries); err != nil {
		t.Fatal(err, r.stdout)
	}
	got := map[string][3]string{}
	for _, e := range entries {
		got[e.Key] = [3]string{e.Value, e.Origin, e.Detail}
	}
	want := map[string][3]string{
		"profile":                       {"heo://profile/default", "default", "built-in"},
		"services.socket.enabled":       {"true", "default", "built-in"},
		"services.api.addr":             {"127.0.0.1:8080", "user", s.path("cfg", "heo", "config.yaml")},
		"runtime.tenant":                {"acme", "profile", s.path("cfg", "heo", "profiles", "default.yaml")},
		"services.all.timeouts.command": {"1m", "file", extra},
		"services.api.enabled":          {"true", "env", "HEO_SERVICES_API_ENABLED"},
		"services.rpc.enabled":          {"true", "override", "-c services.rpc.enabled"},
		"providers.gh.note":             {"***REDACTED***", "user", s.path("cfg", "heo", "config.yaml")},
		"providers.gh.password":         {"***REDACTED***", "user", s.path("cfg", "heo", "config.yaml")},
		"providers.gh.credential_ref":   {"gh/imap", "user", s.path("cfg", "heo", "config.yaml")},
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	for _, secret := range []string{"ghp_1234567890", "hunter2"} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Errorf("secret %q in output", secret)
		}
	}
	// The table form redacts too.
	r = s.run(nil, "config", "list")
	s.want(r, 0)
	if strings.Contains(r.stdout, "hunter2") || !strings.Contains(r.stdout, "ORIGIN") {
		t.Errorf("table output:\n%s", r.stdout)
	}
}

func TestConfigSetRefusals(t *testing.T) {
	s := newSandbox(t)
	for _, args := range [][]string{
		{"config", "set", "profile", "work"},
		{"config", "set", "Bad Key", "x"},
		{"config", "set", "providers.gh.note", "ghp_1234567890abcdefghijABCDEFGHIJ123456"},
		{"config", "set", "--scope", "system", "a", "b"},
	} {
		r := s.run(nil, args...)
		s.want(r, 2, args...)
		if strings.Contains(r.stderr, "ghp_1234567890") {
			t.Errorf("refusal echoed the secret: %s", r.stderr)
		}
	}
	if _, err := os.Stat(s.path("cfg", "heo", "profiles", "default.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused set wrote the profile file: %v", err)
	}
	// A profile key in a file is refused too.
	s.write("cfg/heo/config.yaml", "profile: work\n")
	s.want(s.run(nil, "config", "list"), 2)
	s.write("cfg/heo/config.yaml", "")
	s.want(s.run(nil, "-c", "profile=work", "config", "list"), 2)
}

func TestConfigSetScopesAndDryRun(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "config", "set", "services.socket.enabled", "false", "--dry-run")
	s.want(r, 0)
	if _, err := os.Stat(s.path("cfg", "heo", "profiles", "default.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--dry-run wrote the file: %v", err)
	}
	s.want(s.run(nil, "config", "set", "--scope", "user", "services.all.timeouts.command", "2m"), 0)
	s.want(s.run(nil, "config", "set", "services.socket.enabled", "false"), 0)
	b, err := os.ReadFile(s.path("cfg", "heo", "profiles", "default.yaml"))
	if err != nil || !strings.Contains(string(b), "enabled: false") {
		t.Fatalf("profile file = %q, %v (want a typed bool)", b, err)
	}
	r = s.run(nil, "config", "get", "services.all.timeouts.command", "--format", "json")
	if !strings.Contains(r.stdout, `"origin": "user"`) || !strings.Contains(r.stdout, `"2m"`) {
		t.Errorf("user-scope value: %s", r.stdout)
	}
	// A section key lists its leaves.
	r = s.run(nil, "config", "get", "services.socket", "--format", "json")
	s.want(r, 0)
	if !strings.Contains(r.stdout, "services.socket.auth.mode") || !strings.Contains(r.stdout, "services.socket.path") {
		t.Errorf("section get: %s", r.stdout)
	}
}

func TestConfigValidate(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "config", "validate")
	s.want(r, 0, "config", "validate")

	r = s.run([]string{"HEO_RUNTIME_TENANT= x"}, "-c", "services.api.enabled=true", "config", "validate", "--format", "json")
	s.want(r, 2)
	var res struct {
		Valid  bool
		Issues []struct{ Severity, Key, Origin string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatal(err, r.stdout)
	}
	keys := map[string]string{}
	for _, i := range res.Issues {
		keys[i.Key] = i.Severity + "/" + i.Origin
	}
	if res.Valid || keys["services.api.auth.mode"] != "error/override" || keys["runtime.tenant"] != "error/env" {
		t.Errorf("validate result = %+v", res)
	}
	if !strings.Contains(r.stderr, `"code": "VALIDATION"`) {
		t.Errorf("stderr envelope = %s", r.stderr)
	}
	// Validation is side-effect free: no file, no socket.
	for _, p := range []string{s.path("cfg", "heo"), s.path("run", "heo")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("validate created %s", p)
		}
	}
}

// TestCredentialBackendConfig: the backend is chosen in heo's own
// configuration and checked offline; kit's env backend is not one.
func TestCredentialBackendConfig(t *testing.T) {
	s := newSandbox(t)
	r := s.get(nil, nil, "credentials.backend")
	s.want(r, 0)
	if strings.TrimSpace(r.stdout) != "keyring" {
		t.Errorf("default backend = %q, want keyring", r.stdout)
	}

	r = s.run(nil, "-c", "credentials.backend=env", "config", "validate", "--format", "json")
	s.want(r, 2)
	var res struct {
		Valid  bool
		Issues []struct{ Severity, Key, Message, Origin string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatal(err, r.stdout)
	}
	var found bool
	for _, i := range res.Issues {
		if i.Key == "credentials.backend" && i.Severity == "error" && i.Origin == "override" &&
			strings.Contains(i.Message, "supported: agefile, file, keyring, openbao") {
			found = true
		}
	}
	if res.Valid || !found {
		t.Errorf("validate env backend = %+v", res)
	}

	// openbao's token never sits in configuration, and an address
	// carrying one is refused without being echoed.
	r = s.run(nil, "-c", "credentials.backend=openbao", "-c", "credentials.openbao.addr=https://x:hunter2@bao",
		"-c", "credentials.openbao.token=hunter2", "config", "validate", "--format", "json")
	s.want(r, 2)
	if strings.Contains(r.stdout+r.stderr, "hunter2") {
		t.Errorf("validate echoes a credential:\n%s\n%s", r.stdout, r.stderr)
	}
	r = s.run(nil, "-c", "credentials.openbao.addr=https://x:hunter2@bao", "config", "list")
	s.want(r, 0)
	if strings.Contains(r.stdout, "hunter2") {
		t.Errorf("config list shows a URL password:\n%s", r.stdout)
	}

	// An aps-bound profile keeps the kit store for its surface token;
	// its configuration validates the same way.
	s.apsProfile("noor")
	r = s.run(nil, "--profile", "aps:noor", "config", "validate")
	s.want(r, 0, "--profile", "aps:noor", "config", "validate")
}

func TestConfigValidateStorage(t *testing.T) {
	s := newSandbox(t)
	type issue struct{ Severity, Key, Message, Origin string }
	validate := func(env []string, args ...string) (result, []issue) {
		t.Helper()
		r := s.run(env, append(args, "config", "validate", "--format", "json")...)
		var res struct{ Issues []issue }
		if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
			t.Fatal(err, r.stdout)
		}
		return r, res.Issues
	}

	r, issues := validate([]string{"HEO_STORAGE_DRIVER=sqlite"})
	s.want(r, 2)
	if len(issues) != 1 || issues[0].Key != "storage.driver" || issues[0].Origin != "env" ||
		!strings.Contains(issues[0].Message, `"sqlite"`) || !strings.Contains(issues[0].Message, "registered drivers: postgres") {
		t.Errorf("unknown driver issues = %+v", issues)
	}

	r, issues = validate(nil, "-c", "storage.postgres.dsn=postgres://heo:hunter2@db/heo")
	s.want(r, 2)
	if len(issues) != 1 || issues[0].Key != "storage.postgres.dsn" || !strings.Contains(issues[0].Message, "password_ref") {
		t.Errorf("inline password issues = %+v", issues)
	}
	if strings.Contains(r.stdout+r.stderr, "hunter2") {
		t.Errorf("validate echoed the password: %s %s", r.stdout, r.stderr)
	}

	r, issues = validate(nil, "-c", "storage.postgres.dsn=host=db dbname=heo", "-c", "storage.postgres.password_ref=heo/pg")
	s.want(r, 0)
	if len(issues) != 0 {
		t.Errorf("valid storage config issues = %+v", issues)
	}
}

func TestServiceDefaultsSocketOnly(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "service", "list", "--format", "json")
	s.want(r, 0)
	var rows []struct {
		Service string `json:"service"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatal(err, r.stdout)
	}
	enabled := map[string]bool{}
	for _, row := range rows {
		enabled[row.Service] = row.Enabled
	}
	want := map[string]bool{
		"socket": true, "api": false, "rpc": false, "mcp": false,
		"workers": true, "receivers": true, "webhook": false,
	}
	for k, w := range want {
		if v, ok := enabled[k]; !ok || v != w {
			t.Errorf("service %s enabled = %v (present %v), want %v", k, v, ok, w)
		}
	}
}

func TestGroupsRefuseUnknownSubcommands(t *testing.T) {
	s := newSandbox(t)
	s.want(s.run(nil, "config", "bogus"), 2)
	s.want(s.run(nil, "bogus"), 2)
}

func TestSpecCoverageFull(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "spec", "coverage", "--min", "100")
	s.want(r, 0, "spec", "coverage", "--min", "100")
}
