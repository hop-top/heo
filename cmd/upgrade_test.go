package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"hop.top/kit/go/core/netpolicy"
)

// upgradeSandbox isolates HOME and the XDG tree for in-process runs.
// These runs never reach an install: replacing the binary is exercised
// on a copy of the built heo in tests/e2e, never on the test binary.
func upgradeSandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	// Unix socket paths must stay short.
	run, err := os.MkdirTemp("/tmp", "heo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(run) })
	t.Setenv("XDG_RUNTIME_DIR", run)
	return run
}

func runHeo(t *testing.T, args ...string) runResult {
	t.Helper()
	a := newApp()
	var out, errb bytes.Buffer
	a.r.Cmd.SetArgs(args)
	a.r.Cmd.SetOut(&out)
	a.r.Cmd.SetErr(&errb)
	a.r.Cmd.SetIn(strings.NewReader(""))
	err := a.r.Execute(context.Background())
	r := runResult{stdout: out.String(), stderr: errb.String()}
	if err != nil {
		r.exit = ExitCode(err)
	}
	return r
}

// feed serves a custom release feed and counts asset downloads.
type feed struct {
	srv       *httptest.Server
	version   string
	status    int
	downloads atomic.Int64
}

func newFeed(t *testing.T, version string, status int) *feed {
	t.Helper()
	f := &feed{version: version, status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			if f.status != http.StatusOK {
				http.Error(w, "no release", f.status)
				return
			}
			_, _ = fmt.Fprintf(w, `{"version":%q,"url":%q,"notes":"test release"}`,
				f.version, f.srv.URL+"/heo_"+f.version+"_test.tar.gz")
		default:
			f.downloads.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *feed) args() []string {
	return []string{
		"-c", "upgrade.release_url=" + f.srv.URL + "/latest.json",
		"-c", "upgrade.checksum_url=" + f.srv.URL + "/checksums.txt",
	}
}

func upgradeArgs(f *feed, extra ...string) []string {
	return append(append([]string{"upgrade"}, extra...), f.args()...)
}

// TestUpgradeNoReleasePublished: a feed with no release (GitHub's 404
// before the first release) fails clearly with NOT_FOUND.
func TestUpgradeNoReleasePublished(t *testing.T) {
	upgradeSandbox(t)
	f := newFeed(t, "", http.StatusNotFound)
	r := runHeo(t, upgradeArgs(f, "--format", "json")...)
	if r.exit != 3 {
		t.Fatalf("exit %d, want 3\n%s", r.exit, r.stderr)
	}
	env := errEnvelope(t, r.stderr)
	if env.Code != "NOT_FOUND" || !strings.Contains(env.Message, "no heo release") {
		t.Errorf("envelope = %+v", env)
	}
	if r.stdout != "" {
		t.Errorf("stdout = %q", r.stdout)
	}
}

// TestUpgradeUpToDate: nothing newer, nothing downloaded, exit 0.
func TestUpgradeUpToDate(t *testing.T) {
	upgradeSandbox(t)
	f := newFeed(t, "0.0.0", http.StatusOK)
	r := runHeo(t, upgradeArgs(f, "--format", "json")...)
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.stderr)
	}
	res := decode[upgradeResult](t, r.stdout)
	if res.Upgraded || res.Latest != "0.0.0" || res.Current == "" {
		t.Errorf("result = %+v", res)
	}
	if n := f.downloads.Load(); n != 0 {
		t.Errorf("%d downloads", n)
	}
	if h := runHeo(t, upgradeArgs(f)...); h.exit != 0 || !strings.Contains(h.stdout, "up to date") {
		t.Errorf("human: exit %d, %q", h.exit, h.stdout)
	}
}

// TestUpgradeDryRunDownloadsNothing: an available update is planned,
// not fetched.
func TestUpgradeDryRunDownloadsNothing(t *testing.T) {
	upgradeSandbox(t)
	f := newFeed(t, "9.9.9", http.StatusOK)
	r := runHeo(t, upgradeArgs(f, "--dry-run", "--format", "json")...)
	if r.exit != 0 {
		t.Fatalf("exit %d\n%s", r.exit, r.stderr)
	}
	p := decode[planOut](t, r.stdout)
	if p.Command != "heo upgrade" || len(p.Effects) != 1 || p.Effects[0].Kind != "replace" ||
		!strings.Contains(p.Effects[0].Detail, "9.9.9") {
		t.Errorf("plan = %+v", p)
	}
	if n := f.downloads.Load(); n != 0 {
		t.Errorf("dry run downloaded %d times", n)
	}
}

// TestUpgradeUnreachableFeed: a feed that cannot be reached is
// UNAVAILABLE, exit 6.
func TestUpgradeUnreachableFeed(t *testing.T) {
	upgradeSandbox(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	r := runHeo(t, "upgrade", "--format", "json",
		"-c", "upgrade.release_url=http://"+addr+"/latest.json", "-c", "upgrade.checksum_url=http://"+addr+"/c")
	if r.exit != 6 {
		t.Fatalf("exit %d, want 6\n%s", r.exit, r.stderr)
	}
	if env := errEnvelope(t, r.stderr); env.Code != "UNAVAILABLE" {
		t.Errorf("envelope = %+v", env)
	}
}

// TestUpgradeOfflineRefused: --offline never reaches a remote feed.
func TestUpgradeOfflineRefused(t *testing.T) {
	upgradeSandbox(t)
	// TEST-NET-1: never routed; the offline guard refuses before any
	// dial.
	r := runHeo(t, "upgrade", "--offline", "--format", "json",
		"-c", "upgrade.release_url=http://192.0.2.1/latest.json", "-c", "upgrade.checksum_url=http://192.0.2.1/c")
	if r.exit != 2 {
		t.Fatalf("exit %d, want 2\n%s", r.exit, r.stderr)
	}
	if env := errEnvelope(t, r.stderr); !strings.Contains(env.Message, "--offline") {
		t.Errorf("envelope = %+v", env)
	}
}

// TestUpgradeRefusedWhileServiceRuns: a live control socket means a
// daemon runs from this binary: refused, told to stop it, nothing
// fetched.
func TestUpgradeRefusedWhileServiceRuns(t *testing.T) {
	run := upgradeSandbox(t)
	f := newFeed(t, "9.9.9", http.StatusOK)
	for _, sock := range []string{
		filepath.Join(run, "heo", "default", "heo.sock"),  // the selected profile
		filepath.Join(run, "heo", "aps-noor", "heo.sock"), // another profile on this binary
	} {
		t.Run(filepath.Base(filepath.Dir(sock)), func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			// Leave the file behind on close: a stale socket.
			ln.(*net.UnixListener).SetUnlinkOnClose(false)
			r := runHeo(t, upgradeArgs(f, "--format", "json")...)
			_ = ln.Close()
			if r.exit != 4 {
				t.Fatalf("exit %d, want 4\n%s", r.exit, r.stderr)
			}
			env := errEnvelope(t, r.stderr)
			if env.Code != "CONFLICT" || !strings.Contains(env.Message, sock) ||
				!strings.Contains(env.SuggestedFix, "service stop") {
				t.Errorf("envelope = %+v", env)
			}
			if n := f.downloads.Load(); n != 0 {
				t.Errorf("%d downloads", n)
			}
		})
	}
	// A stale socket file with no listener is not a daemon.
	r := runHeo(t, upgradeArgs(f, "--dry-run")...)
	if r.exit != 0 {
		t.Errorf("stale socket: exit %d\n%s", r.exit, r.stderr)
	}
}

// TestUpgradeCustomFeedNeedsChecksums: downloads are always verified.
func TestUpgradeCustomFeedNeedsChecksums(t *testing.T) {
	upgradeSandbox(t)
	f := newFeed(t, "9.9.9", http.StatusOK)
	r := runHeo(t, "upgrade", "--format", "json", "-c", "upgrade.release_url="+f.srv.URL+"/latest.json")
	if r.exit != 2 {
		t.Fatalf("exit %d, want 2\n%s", r.exit, r.stderr)
	}
	if env := errEnvelope(t, r.stderr); !strings.Contains(env.Message, "upgrade.checksum_url") {
		t.Errorf("envelope = %+v", env)
	}
}

func TestUpgradeErrorClasses(t *testing.T) {
	src := "github.com/hop-top/heo"
	for _, tc := range []struct {
		name string
		err  error
		code string
		exit int
	}{
		{"no github release", errors.New("upgrade: github API returned 404"), "NOT_FOUND", 3},
		{"no custom release", errors.New("upgrade: custom URL returned 404 Not Found"), "NOT_FOUND", 3},
		{"offline", &url.Error{Op: "Get", URL: "https://api.github.com", Err: fmt.Errorf("GET x: %w", netpolicy.ErrOffline)}, "USAGE", 2},
		{"network", &url.Error{Op: "Get", URL: "https://api.github.com", Err: errors.New("dial tcp: no route")}, "UNAVAILABLE", 6},
		{"server error", errors.New("upgrade: github API returned 502"), "UNAVAILABLE", 6},
		{"bad checksum", errors.New("upgrade: checksum mismatch: got a, want b"), "GENERIC", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := upgradeError(tc.err, src)
			if e.Code != tc.code || e.ExitCode != tc.exit {
				t.Errorf("%v → %s/%d, want %s/%d", tc.err, e.Code, e.ExitCode, tc.code, tc.exit)
			}
		})
	}
}

// TestUpgradeIsTheOnlyTopLevelVerb: heo upgrade sits at depth 1 by
// explicit exception, never on a served surface.
func TestUpgradeIsTheOnlyTopLevelVerb(t *testing.T) {
	a := newApp()
	up := child(a.r.Cmd, "upgrade")
	if up == nil || up.HasSubCommands() || up.Annotations["kit/top-level-verb"] != "true" {
		t.Fatal("upgrade missing or not a declared top-level verb")
	}
	if up.Annotations["kit/self-hosting"] != "true" {
		t.Error("upgrade is invocable on served surfaces")
	}
}
