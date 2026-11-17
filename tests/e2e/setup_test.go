package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStdin is run with stdin fed from input: a file, not a terminal.
func (s *sandbox) runStdin(input string, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, heoBin, args...) //nolint:gosec // runs the binary TestMain built
	cmd.Dir = s.path("cwd")
	cmd.Stdin = strings.NewReader(input)
	for k, v := range s.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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

// useFileSecrets selects the file credential backend for every
// profile, so no keychain is touched.
func (s *sandbox) useFileSecrets() {
	s.write("cfg/heo/config.yaml", "credentials:\n  backend: file\n")
}

// tokenFile is where the file backend keeps a profile's surface token.
func (s *sandbox) tokenFile(pdir string) string {
	return s.path("data", "heo", pdir, "secrets", "heo", pdir, "surface-token")
}

// storedToken reads the token value from the file backend's record.
func (s *sandbox) storedToken(pdir string) string {
	s.t.Helper()
	b, err := os.ReadFile(s.tokenFile(pdir))
	if err != nil {
		s.t.Fatalf("surface token record: %v", err)
	}
	var rec struct {
		Token   string `json:"token"`
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(b, &rec); err != nil || len(rec.Token) != 43 || rec.Subject != me() {
		s.t.Fatalf("surface token record = %+v, %v", rec, err)
	}
	return rec.Token
}

// setupOut is `service setup --format json` decoded.
type setupOut struct {
	Changed  bool `json:"changed"`
	Surfaces []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
		Addr    string `json:"addr"`
		URL     string `json:"url"`
	} `json:"surfaces"`
	Token *struct {
		Ref     string `json:"ref"`
		Backend string `json:"backend"`
		Subject string `json:"subject"`
		Action  string `json:"action"`
		Value   string `json:"value"`
	} `json:"token"`
}

func decodeSetup(t *testing.T, raw string) setupOut {
	t.Helper()
	var out setupOut
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("setup output: %v\n%s", err, raw)
	}
	return out
}

func (o setupOut) addr(svc string) string {
	for _, s := range o.Surfaces {
		if s.Name == svc && s.Enabled {
			return s.Addr
		}
	}
	return ""
}

// noSetupWrites fails when setup left a profile file or a token.
func (s *sandbox) noSetupWrites(pdir string) {
	s.t.Helper()
	for _, p := range []string{s.path("cfg", "heo", "profiles"), s.path("data", "heo", pdir, "secrets")} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			s.t.Errorf("%s exists: %v", p, err)
		}
	}
}

// TestSetupNonTTYNeverPrompts: without a terminal, setup with nothing
// to do refuses with USAGE, naming the arguments, and reads nothing
// from stdin even when stdin holds answers; with its arguments it
// runs without a prompt.
func TestSetupNonTTYNeverPrompts(t *testing.T) {
	s := newSandbox(t)
	s.useFileSecrets()
	for _, input := range []string{"", "1,2,3\n\n\n\n"} {
		r := s.runStdin(input, "service", "setup")
		s.want(r, 2, "service", "setup")
		for _, want := range []string{"USAGE", "api, rpc, mcp", "--rotate-token", "heo service setup api"} {
			if !strings.Contains(r.stderr, want) {
				t.Errorf("stdin %q: stderr lacks %q:\n%s", input, want, r.stderr)
			}
		}
		if strings.Contains(r.stderr+r.stdout, "Surfaces to enable") || strings.Contains(r.stderr+r.stdout, "listen address") {
			t.Errorf("stdin %q: prompted:\n%s%s", input, r.stdout, r.stderr)
		}
		s.noSetupWrites("default")
	}
	r := s.runStdin("1\n", "service", "setup", "api", "--format", "json")
	s.want(r, 0, "service", "setup", "api")
	if strings.Contains(r.stderr, "listen address") || decodeSetup(t, r.stdout).addr("api") == "" {
		t.Errorf("setup api: stdout %s\nstderr %s", r.stdout, r.stderr)
	}
}

// TestSetupTokenInSecretStoreOnly: the token is minted into the
// credential backend and named in configuration by its store key
// only; no output, configuration file or listing carries it unless
// --show-token asks, and then stdout carries it once. A re-run keeps
// it and changes nothing; --rotate-token replaces it.
func TestSetupTokenInSecretStoreOnly(t *testing.T) {
	s := newSandbox(t)
	s.useFileSecrets()
	api := freeAddr(t)
	r := s.run(nil, "service", "setup", "api", "mcp", "--addr", "api="+api)
	s.want(r, 0, "service", "setup", "api", "mcp")
	tok := s.storedToken("default")
	var seen []string
	seen = append(seen, r.stdout, r.stderr)
	if !strings.Contains(r.stdout, "minted") || !strings.Contains(r.stdout, "$HEO_TOKEN") ||
		!strings.Contains(r.stdout, "http://"+api+"/v1/commands") || !strings.Contains(r.stdout, `"mcpServers"`) {
		t.Errorf("setup output:\n%s", r.stdout)
	}

	profileFile := s.path("cfg", "heo", "profiles", "default.yaml")
	body, err := os.ReadFile(profileFile) //nolint:gosec // sandbox path
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"surface_token_ref: heo/default/surface-token", "addr: " + api, "enabled: true"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("profile file lacks %q:\n%s", want, body)
		}
	}

	// Re-run: the same token, nothing written.
	r = s.run(nil, "service", "setup", "api", "mcp", "--format", "json")
	s.want(r, 0)
	seen = append(seen, r.stdout, r.stderr)
	out := decodeSetup(t, r.stdout)
	if out.Changed || out.Token == nil || out.Token.Action != "kept" || out.Token.Value != "" ||
		out.Token.Backend != "file" || out.Token.Subject != me() || out.addr("api") != api || s.storedToken("default") != tok {
		t.Errorf("re-run = %s", r.stdout)
	}
	if again, _ := os.ReadFile(profileFile); !bytes.Equal(again, body) { //nolint:gosec // sandbox path
		t.Errorf("re-run rewrote the profile file:\n%s", again)
	}

	for _, args := range [][]string{{"config", "list"}, {"config", "validate"}, {"config", "get", "credentials.surface_token_ref"}, {"status"}} {
		r := s.run(nil, args...)
		seen = append(seen, r.stdout, r.stderr)
	}
	files, _ := filepath.Glob(s.path("cfg", "heo", "*.yaml"))
	more, _ := filepath.Glob(s.path("cfg", "heo", "profiles", "*.yaml"))
	for _, f := range append(files, more...) {
		b, _ := os.ReadFile(f) //nolint:gosec // sandbox path
		seen = append(seen, string(b))
	}
	for i, text := range seen {
		if strings.Contains(text, tok) {
			t.Errorf("output/file %d carries the token:\n%s", i, text)
		}
	}

	// --show-token: once, on stdout.
	r = s.run(nil, "service", "setup", "--show-token")
	s.want(r, 0)
	if strings.Count(r.stdout, tok) != 1 || strings.Contains(r.stderr, tok) {
		t.Errorf("--show-token: stdout %q stderr %q", r.stdout, r.stderr)
	}
	r = s.run(nil, "service", "setup", "--show-token", "--format", "json")
	if out := decodeSetup(t, r.stdout); out.Token == nil || out.Token.Value != tok || strings.Count(r.stdout, tok) != 1 {
		t.Errorf("--show-token json: %s", r.stdout)
	}

	// --rotate-token: a new token, shown only when asked.
	r = s.run(nil, "service", "setup", "--rotate-token", "--format", "json")
	s.want(r, 0)
	out = decodeSetup(t, r.stdout)
	next := s.storedToken("default")
	if next == tok || out.Token == nil || out.Token.Action != "rotated" || out.Token.Value != "" || strings.Contains(r.stdout, next) {
		t.Errorf("rotate: %s", r.stdout)
	}
}

// TestSetupLoopbackOnly: an address that is not a loopback IP is
// refused before anything is written, and a non-loopback address the
// configuration already carries is replaced by a loopback one.
func TestSetupLoopbackOnly(t *testing.T) {
	s := newSandbox(t)
	s.useFileSecrets()
	for _, addr := range []string{"0.0.0.0:8080", "[::]:8080", ":8080", "192.168.1.10:8080", "localhost:8080", "example.com:80"} {
		r := s.run(nil, "service", "setup", "api", "--addr", "api="+addr)
		s.want(r, 2, "service", "setup", "api", "--addr", "api="+addr)
		if !strings.Contains(r.stderr, "loopback") {
			t.Errorf("%s: stderr = %s", addr, r.stderr)
		}
		s.noSetupWrites("default")
	}
	s.write("cfg/heo/config.yaml", "credentials:\n  backend: file\nservices:\n  rpc:\n    addr: 0.0.0.0:8082\n")
	r := s.run(nil, "service", "setup", "rpc", "--format", "json")
	s.want(r, 0)
	if got := decodeSetup(t, r.stdout).addr("rpc"); !strings.HasPrefix(got, "127.0.0.1:") {
		t.Errorf("rpc addr = %q, want loopback", got)
	}
}

// TestSetupDryRunWritesNothing: --dry-run renders the plan, keys and
// the token's fate but never a token, and writes nothing: no profile
// file, no token, no rotation.
func TestSetupDryRunWritesNothing(t *testing.T) {
	s := newSandbox(t)
	s.useFileSecrets()
	args := []string{"service", "setup", "api", "rpc", "mcp", "--show-token", "--dry-run", "--format", "json"}
	r := s.run(nil, args...)
	s.want(r, 0, args...)
	var plan struct {
		Effects []struct{ Kind, Target, Detail string }
	}
	if err := json.Unmarshal([]byte(r.stdout), &plan); err != nil {
		t.Fatal(err, r.stdout)
	}
	text := r.stdout
	for _, want := range []string{"secret:heo/default/surface-token", "services.api.enabled = true", "services.mcp.addr",
		"credentials.surface_token_ref = heo/default/surface-token"} {
		if !strings.Contains(text, want) {
			t.Errorf("plan lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, `"value"`) {
		t.Errorf("plan carries a token value:\n%s", text)
	}
	s.noSetupWrites("default")

	s.want(s.run(nil, "service", "setup", "api"), 0)
	tok := s.storedToken("default")
	profile, _ := os.ReadFile(s.path("cfg", "heo", "profiles", "default.yaml")) //nolint:gosec // sandbox path
	r = s.run(nil, "service", "setup", "rpc", "--disable", "api", "--rotate-token", "--show-token", "--dry-run")
	s.want(r, 0)
	if strings.Contains(r.stdout+r.stderr, tok) || !strings.Contains(r.stdout, "rotate") ||
		!strings.Contains(r.stdout, "services.api.enabled = false") {
		t.Errorf("dry-run rotate:\n%s%s", r.stdout, r.stderr)
	}
	after, _ := os.ReadFile(s.path("cfg", "heo", "profiles", "default.yaml")) //nolint:gosec // sandbox path
	if s.storedToken("default") != tok || !bytes.Equal(after, profile) {
		t.Errorf("dry-run changed the token or the profile file:\n%s", after)
	}
}

// TestSetupApsProfileTokenInHeoStore: an aps-bound profile's token goes
// to heo's own store; the aps secrets file is never written.
func TestSetupApsProfileTokenInHeoStore(t *testing.T) {
	s := newSandbox(t)
	s.useFileSecrets()
	s.apsProfile("noor")
	secrets := s.write("aps/profiles/noor/secrets.env", "A=1\n")
	r := s.run(nil, "--profile", "aps:noor", "service", "setup", "api", "--format", "json")
	s.want(r, 0, "--profile", "aps:noor", "service", "setup", "api")
	out := decodeSetup(t, r.stdout)
	if out.Token == nil || out.Token.Ref != "heo/aps-noor/surface-token" || out.Token.Backend != "file" {
		t.Errorf("aps setup = %s", r.stdout)
	}
	s.storedToken("aps-noor")
	if b, err := os.ReadFile(secrets); err != nil || string(b) != "A=1\n" { //nolint:gosec // sandbox path
		t.Errorf("aps secrets file = %q, %v", b, err)
	}
	if _, err := os.Stat(s.path("cfg", "heo", "profiles", "aps-noor.yaml")); err != nil {
		t.Errorf("aps profile file: %v", err)
	}
}

// TestStartRefusesMissingSurfaceToken: a surface configured for the
// surface token whose token is not stored is refused at start, naming
// setup.
func TestStartRefusesMissingSurfaceToken(t *testing.T) {
	s := newSandbox(t)
	s.write("cfg/heo/config.yaml", "credentials:\n  backend: file\n  surface_token_ref: heo/default/surface-token\n")
	args := []string{"service", "start", noRuntime, "--enable", "api", "--addr", freeAddr(t)}
	r := s.run(nil, args...)
	s.want(r, 2, args...)
	if !strings.Contains(r.stderr, "no surface token stored") || !strings.Contains(r.stderr, "heo service setup api") ||
		readyRe.MatchString(r.stderr) {
		t.Errorf("stderr = %s", r.stderr)
	}
}

// TestSetupSurfacesAuthenticate: after setup, the daemon, on its data
// store, serves REST, gRPC and MCP to a bearer of the profile's token
// as the user who minted it, and turns away a request without it or
// with another; a rotated token is refused from the next request on,
// with no restart.
func TestSetupSurfacesAuthenticate(t *testing.T) {
	withE2EBinary(t, func() { setupSurfacesAuthenticate(t) })
}

func setupSurfacesAuthenticate(t *testing.T) {
	db := newDB(t).migrate()
	s := newSandbox(t)
	s.useStore(db)
	api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
	r := s.run(nil, "service", "setup", "api", "rpc", "mcp",
		"--addr", "api="+api, "--addr", "rpc="+rpc, "--addr", "mcp="+mcp, "--show-token", "--format", "json")
	s.want(r, 0, "service", "setup")
	tok := decodeSetup(t, r.stdout).Token.Value
	if tok == "" || tok != s.storedToken("default") {
		t.Fatalf("setup token: %s", r.stdout)
	}

	d := s.start(nil, nil, []string{"socket", "api", "rpc", "mcp", "workers", "receivers"}, "service", "start")
	defer func() { s.want(result{exit: d.term()}, 0) }()
	x := &surfaces{d: d, api: api, rpc: rpc, mcp: mcp}
	path := []string{"e2e", "whoami"}

	refused := func(token string) {
		t.Helper()
		if code, body := x.rest(t, http.MethodGet, "/v1/commands", token, ""); code != http.StatusUnauthorized {
			t.Errorf("REST discovery with %q = %d %v, want 401", token, code, body)
		}
		if code, body := x.rest(t, http.MethodGet, "/v1/commands/e2e/whoami", token, ""); code != http.StatusUnauthorized {
			t.Errorf("REST whoami with %q = %d %v, want 401", token, code, body)
		}
		if code, body := x.invokeRPC(t, token, map[string]any{"path": path}); code != http.StatusUnauthorized ||
			!strings.Contains(asString(body), "unauthenticated") {
			t.Errorf("rpc whoami with %q = %d %v, want 401 unauthenticated", token, code, body)
		}
		resp, err := authed(http.MethodPost, "http://"+mcp+"/mcp", token,
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"0"}}}`)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("MCP initialize with %q = %d, want 401", token, resp.StatusCode)
		}
	}
	admitted := func(token string) {
		t.Helper()
		code, body := x.rest(t, http.MethodGet, "/v1/commands/e2e/whoami", token, "")
		data, _ := body["data"].(map[string]any)
		if code != http.StatusOK || data["actor"] != me() || data["surface"] != "rest" {
			t.Errorf("REST whoami = %d %v, want actor %s", code, body, me())
		}
		code, body = x.invokeRPC(t, token, map[string]any{"path": path})
		if code != http.StatusOK || !strings.Contains(asString(body), me()) {
			t.Errorf("rpc whoami = %d %v", code, body)
		}
		got := newMCPClient(t, "http://"+mcp+"/mcp", token).callTool(t, "e2e.whoami", map[string]any{})
		if got["actor"] != me() || got["surface"] != "mcp" {
			t.Errorf("MCP whoami = %v", got)
		}
	}

	refused("")
	refused("not-the-token")
	refused(tok + "x")
	admitted(tok)

	// Rotation: the old token is refused at once, the new one admitted.
	r = s.run(nil, "service", "setup", "--rotate-token", "--show-token", "--format", "json")
	s.want(r, 0)
	next := decodeSetup(t, r.stdout).Token.Value
	if next == "" || next == tok {
		t.Fatalf("rotate: %s", r.stdout)
	}
	refused(tok)
	admitted(next)

	if strings.Contains(d.log(), tok) || strings.Contains(d.log(), next) {
		t.Errorf("daemon log carries a token:\n%s", d.log())
	}
}
