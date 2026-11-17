package hooks_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/hooks/hookstest"
)

func env(id string) hooks.Envelope {
	return hooks.NewEnvelope("p", event(id, "c1"), 1, time.Now())
}

func TestExecProcess(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	run := func(mode string) (hooks.Result, error) {
		e := &hooks.ExecProcess{Command: cmd, Args: []string{log, mode}, Timeout: 5 * time.Second}
		return e.Run(context.Background(), env("e1"))
	}
	res, err := run(hookstest.ModeOK)
	if err != nil || len(res.Operations) != 1 || res.Operations[0].Verb != "message note" {
		t.Fatalf("ok: %+v %v", res, err)
	}
	if got := hookstest.Seen(log); !slices.Equal(got, []string{"e1"}) {
		t.Errorf("plugin saw %v", got)
	}
	if res, err = run(hookstest.ModeEmpty); err != nil || len(res.Operations) != 0 {
		t.Errorf("empty output is no operations: %+v %v", res, err)
	}
	for mode, perm := range map[string]bool{hookstest.ModeExit1: false, hookstest.ModeExit2: true, hookstest.ModeBadJSON: false} {
		_, err := run(mode)
		if err == nil || hooks.IsPermanent(err) != perm {
			t.Errorf("%s: permanent=%v err=%v", mode, hooks.IsPermanent(err), err)
		}
	}
}

func TestExecProcessTimeout(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	e := &hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeSlow}, Timeout: 100 * time.Millisecond}
	start := time.Now()
	_, err := e.Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestExecProcessStopIsNotAFailure(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e := &hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeSlow}, Timeout: time.Minute}
	if _, err := e.Run(ctx, env("e1")); err != context.DeadlineExceeded {
		t.Errorf("err %v, want the context's", err)
	}
}

// TestPluginEnvironmentIsMinimal: the daemon's environment does not reach
// a plugin.
func TestPluginEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("HEO_SECRET_TOKEN", "hunter2")
	cmd, log := hookstest.Plugin(t)
	e := &hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeEnv}, Timeout: 5 * time.Second}
	if _, err := e.Run(context.Background(), env("e1")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Clean(log + ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "HEO_SECRET_TOKEN") {
		t.Errorf("plugin environment leaks: %s", b)
	}
	if !strings.Contains(string(b), "PATH=") {
		t.Errorf("no PATH: %s", b)
	}
}

func TestStreamProcess(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	s := &hooks.StreamProcess{Command: cmd, Args: []string{log, hookstest.ModeOK + ":stream"}, Timeout: 5 * time.Second}
	defer func() { _ = s.Close() }() // teardown
	for _, id := range []string{"e1", "e2", "e3"} {
		res, err := s.Run(context.Background(), env(id))
		if err != nil || res.EventID != id || len(res.Operations) != 1 {
			t.Fatalf("%s: %+v %v", id, res, err)
		}
	}
	if got := hookstest.Seen(log); !slices.Equal(got, []string{"e1", "e2", "e3"}) {
		t.Errorf("plugin saw %v", got)
	}
}

// TestStreamProcessRestarts: a stream plugin that exits under an
// envelope fails that event, with its exit code's verdict, and the next
// envelope starts a fresh process.
func TestStreamProcessRestarts(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	s := &hooks.StreamProcess{Command: cmd, Args: []string{log, hookstest.ModeExit2 + ":stream"}, Timeout: 5 * time.Second}
	defer func() { _ = s.Close() }() // teardown
	for range 2 {
		if _, err := s.Run(context.Background(), env("e1")); err == nil || !hooks.IsPermanent(err) {
			t.Errorf("err %v, want a permanent failure", err)
		}
	}
	if got := hookstest.Seen(log); len(got) != 2 {
		t.Errorf("plugin started %d times, want 2: %v", len(got), got)
	}
}

func TestStreamProcessTimeout(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	s := &hooks.StreamProcess{Command: cmd, Args: []string{log, hookstest.ModeSlow + ":stream"}, Timeout: 100 * time.Millisecond}
	defer func() { _ = s.Close() }() // teardown
	if _, err := s.Run(context.Background(), env("e1")); err == nil || hooks.IsPermanent(err) {
		t.Errorf("err %v", err)
	}
}

func TestParseResult(t *testing.T) {
	for in, ok := range map[string]bool{
		"": true, "  \n": true, `{"operations":[]}`: true,
		`{"heo_version":1,"operations":[{"verb":"message note","args":["x"]}]}`: true,
		`{"operations":[{"args":["x"]}]}`:                                       false, `{"heo_version":2,"operations":[]}`: false, "nope": false,
	} {
		if _, err := hooks.ParseResult([]byte(in)); (err == nil) != ok {
			t.Errorf("%q: err %v", in, err)
		}
	}
}

type logLine struct{ msg, plugin, line string }

type recLog struct {
	mu    sync.Mutex
	lines []logLine
}

func (r *recLog) Info(msg any, kv ...any) {
	l := logLine{msg: fmt.Sprint(msg)}
	for i := 0; i+1 < len(kv); i += 2 {
		switch kv[i] {
		case "plugin":
			l.plugin = fmt.Sprint(kv[i+1])
		case "line":
			l.line = fmt.Sprint(kv[i+1])
		}
	}
	r.mu.Lock()
	r.lines = append(r.lines, l)
	r.mu.Unlock()
}
func (r *recLog) Error(any, ...any) {}
func (r *recLog) got() []logLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.lines)
}

func envOf(t *testing.T, log string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(log + ".env"))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			m[k] = v
		}
	}
	return m
}

// TestPluginEnvironmentContract: a plugin has PATH, its name, its own
// token and the surface address, and nothing else: not the daemon's
// variables, which hold credential references and the profile's
// surface token. Both modes.
func TestPluginEnvironmentContract(t *testing.T) {
	for k, v := range map[string]string{
		"HEO_TOKEN": "profile-surface-token", "HEO_CREDENTIALS_SURFACE_TOKEN_REF": "heo/default/surface-token",
		"AWS_SECRET_ACCESS_KEY": "aws-secret", "HOME": "/home/daemon",
	} {
		t.Setenv(k, v)
	}
	for _, mode := range []string{"exec", "stream"} {
		t.Run(mode, func(t *testing.T) {
			cmd, log := hookstest.Plugin(t)
			l := hooks.Launch{Name: "triage", URL: "http://127.0.0.1:9", Dir: filepath.Join(t.TempDir(), "w"),
				Token: func(context.Context) (string, error) { return "triage.s3cret", nil }}
			var err error
			if mode == "exec" {
				_, err = (&hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeEnv}, Timeout: 5 * time.Second, Launch: l}).Run(context.Background(), env("e1"))
			} else {
				s := &hooks.StreamProcess{Command: cmd, Args: []string{log, hookstest.ModeEnv + ":stream"}, Timeout: 5 * time.Second, Launch: l}
				defer func() { _ = s.Close() }() // teardown
				_, err = s.Run(context.Background(), env("e1"))
			}
			if err != nil {
				t.Fatal(err)
			}
			got := envOf(t, log)
			for k := range got {
				switch k {
				case "PATH", hooks.EnvName, hooks.EnvToken, hooks.EnvURL, "PWD", "SHLVL", "_", "OLDPWD":
				default:
					t.Errorf("unexpected variable %s", k)
				}
			}
			if got[hooks.EnvName] != "triage" || got[hooks.EnvToken] != "triage.s3cret" || got[hooks.EnvURL] != "http://127.0.0.1:9" {
				t.Errorf("contract: %v", got)
			}
			raw, _ := os.ReadFile(filepath.Clean(log + ".env"))
			for _, bad := range []string{"profile-surface-token", "surface-token", "aws-secret", "/home/daemon"} {
				if strings.Contains(string(raw), bad) {
					t.Errorf("environment leaks %q", bad)
				}
			}
		})
	}
}

// TestPluginWorkingDirectory: the plugin runs in its own directory,
// created owner-only, an existing looser one tightened.
func TestPluginWorkingDirectory(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	dir := filepath.Join(t.TempDir(), "state", "plugins", "triage")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	e := &hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeEnv}, Timeout: 5 * time.Second, Launch: hooks.Launch{Name: "triage", Dir: dir}}
	if _, err := e.Run(context.Background(), env("e1")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Clean(log + ".pwd"))
	if want, _ := filepath.EvalSymlinks(dir); strings.TrimSpace(string(b)) != want {
		t.Errorf("cwd %q, want %q", b, want)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v %v, want 0700", fi.Mode().Perm(), err)
	}
}

// TestPluginTokenFailureFailsTheAttempt: a token that cannot be read
// is a retryable failure, and the plugin is not started.
func TestPluginTokenFailureFailsTheAttempt(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	l := hooks.Launch{Name: "triage", Token: func(context.Context) (string, error) { return "", errors.New("backend down") }}
	_, err := (&hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeOK}, Timeout: 5 * time.Second, Launch: l}).Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || !strings.Contains(err.Error(), "backend down") {
		t.Errorf("err %v", err)
	}
	if len(hookstest.Seen(log)) != 0 {
		t.Error("plugin started without its token")
	}
}

// TestPluginStdoutCap: a result past the cap is malformed output, a
// failed attempt, in both modes; the stream plugin is not kept.
func TestPluginStdoutCap(t *testing.T) {
	cmd, log := hookstest.Plugin(t)
	l := hooks.Launch{Name: "triage", MaxOutput: 1000}
	_, err := (&hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeBig}, Timeout: 5 * time.Second, Launch: l}).Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || !strings.Contains(err.Error(), "larger than 1000") {
		t.Errorf("exec: %v", err)
	}
	s := &hooks.StreamProcess{Command: cmd, Args: []string{log, hookstest.ModeBig + ":stream"}, Timeout: 5 * time.Second, Launch: l}
	defer func() { _ = s.Close() }() // teardown
	start := time.Now()
	_, err = s.Run(context.Background(), env("e1"))
	if err == nil || hooks.IsPermanent(err) || !strings.Contains(err.Error(), "larger than 1000") {
		t.Errorf("stream: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("stream took %s", time.Since(start))
	}
}

// TestPluginStderrRouting: stderr goes to the log under the plugin's
// name, a line at a time, an unterminated last line included, in both
// modes.
func TestPluginStderrRouting(t *testing.T) {
	for _, mode := range []string{"exec", "stream"} {
		t.Run(mode, func(t *testing.T) {
			cmd, log := hookstest.Plugin(t)
			rec := &recLog{}
			l := hooks.Launch{Name: "triage", Log: rec}
			if mode == "exec" {
				if _, err := (&hooks.ExecProcess{Command: cmd, Args: []string{log, hookstest.ModeStderr}, Timeout: 5 * time.Second, Launch: l}).Run(context.Background(), env("e1")); err != nil {
					t.Fatal(err)
				}
			} else {
				s := &hooks.StreamProcess{Command: cmd, Args: []string{log, hookstest.ModeStderr + ":stream"}, Timeout: 5 * time.Second, Launch: l}
				if _, err := s.Run(context.Background(), env("e1")); err != nil {
					t.Fatal(err)
				}
				_ = s.Close() // the unterminated line is flushed as the process exits
			}
			var lines []string
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				lines = nil
				for _, g := range rec.got() {
					if g.plugin != "triage" {
						t.Errorf("logged under %q", g.plugin)
					}
					lines = append(lines, g.line)
				}
				if len(lines) == 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !slices.Equal(lines, []string{"first line", "second"}) {
				t.Errorf("logged %q", lines)
			}
		})
	}
}

// TestPluginStderrLineCap: one line is logged up to a bound.
func TestPluginStderrLineCap(t *testing.T) {
	rec := &recLog{}
	l := hooks.Launch{Name: "p", Log: rec}
	cmd, log := hookstest.Plugin(t)
	_ = cmd
	_ = log
	w := l.StderrForTest()
	long := strings.Repeat("x", 10000)
	_, _ = w.Write([]byte(long + "\nok\n"))
	got := rec.got()
	if len(got) != 2 || len(got[0].line) > 4096+len(" [truncated]") || !strings.HasSuffix(got[0].line, "[truncated]") || got[1].line != "ok" {
		t.Errorf("logged %d lines: %v", len(got), got)
	}
}
