package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const fakeRelease = "9.9.9"

// releaseServer serves a custom release feed for fakeRelease: a tar.gz
// holding a stand-in heo that prints a marker, and a checksums file.
// badSum publishes a wrong checksum.
func releaseServer(t *testing.T, badSum bool) *httptest.Server {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	script := []byte("#!/bin/sh\necho heo " + fakeRelease + " installed\n")
	if err := tw.WriteHeader(&tar.Header{Name: "heo", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(script); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("heo_%s_%s_%s.tar.gz", fakeRelease, runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(archive.Bytes())
	hexSum := hex.EncodeToString(sum[:])
	if badSum {
		hexSum = strings.Repeat("0", 64)
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			_ = json.NewEncoder(w).Encode(map[string]string{"version": fakeRelease, "url": srv.URL + "/" + name})
		case "/" + name:
			_, _ = w.Write(archive.Bytes())
		case "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", hexSum, name)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// installedCopy copies the built heo into the sandbox, so an upgrade
// replaces the copy, never the binary other tests run.
func installedCopy(t *testing.T, s *sandbox) string {
	t.Helper()
	data, err := os.ReadFile(heoBin) //nolint:gosec // G304: the built test binary
	if err != nil {
		t.Fatal(err)
	}
	p := s.path("bin", "heo")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o755); err != nil { //nolint:gosec // G306: an executable
		t.Fatal(err)
	}
	return p
}

// runBin runs the binary at bin like sandbox.run runs heo.
func (s *sandbox) runBin(bin string, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the test's own copy of heo
	cmd.Dir = s.path("cwd")
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
		s.t.Fatalf("run %s %v: %v", bin, args, err)
	}
	return r
}

func feedArgs(srv *httptest.Server) []string {
	return []string{"-c", "upgrade.release_url=" + srv.URL + "/latest.json", "-c", "upgrade.checksum_url=" + srv.URL + "/checksums.txt"}
}

// TestUpgradeReplacesBinary: a newer verified release replaces the
// installed binary in place.
func TestUpgradeReplacesBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release archives are unix-only")
	}
	s := newSandbox(t)
	srv := releaseServer(t, false)
	bin := installedCopy(t, s)

	// The dry run plans and leaves the binary alone.
	r := s.runBin(bin, append([]string{"upgrade", "--dry-run", "--format", "json"}, feedArgs(srv)...)...)
	s.want(r, 0, "upgrade", "--dry-run")
	if !strings.Contains(r.stdout, fakeRelease) {
		t.Errorf("plan = %s", r.stdout)
	}
	if v := s.runBin(bin, "--version"); v.exit != 0 || strings.Contains(v.stdout, "installed") {
		t.Fatalf("dry run replaced the binary: %+v", v)
	}

	r = s.runBin(bin, append([]string{"upgrade", "--format", "json"}, feedArgs(srv)...)...)
	s.want(r, 0, "upgrade")
	var res struct {
		Current, Latest, Path string
		Upgraded              bool
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	want, _ := filepath.EvalSymlinks(bin)
	if !res.Upgraded || res.Latest != fakeRelease || res.Current != "dev" || res.Path != want {
		t.Errorf("result = %+v", res)
	}
	if out := s.runBin(bin); !strings.Contains(out.stdout, "heo "+fakeRelease+" installed") {
		t.Fatalf("binary not replaced: %+v", out)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("replaced binary not executable: %v %v", fi, err)
	}
}

// TestUpgradeBadChecksumKeepsBinary: a download that fails verification
// is never installed.
func TestUpgradeBadChecksumKeepsBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release archives are unix-only")
	}
	s := newSandbox(t)
	srv := releaseServer(t, true)
	bin := installedCopy(t, s)
	r := s.runBin(bin, append([]string{"upgrade", "--format", "json"}, feedArgs(srv)...)...)
	s.want(r, 1, "upgrade")
	if !strings.Contains(r.stderr, "checksum mismatch") || !strings.Contains(r.stderr, "unchanged") {
		t.Errorf("stderr = %s", r.stderr)
	}
	if v := s.runBin(bin, "--version"); v.exit != 0 || strings.Contains(v.stdout, "installed") {
		t.Fatalf("binary changed after a failed verification: %+v", v)
	}
	entries, err := os.ReadDir(filepath.Dir(bin))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("temp files left next to the binary: %v", entries)
	}
}

// TestUpgradeWithoutRelease: no release published yet is NOT_FOUND,
// exit 3, nothing on stdout.
func TestUpgradeWithoutRelease(t *testing.T) {
	s := newSandbox(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	r := s.run(nil, append([]string{"upgrade", "--format", "json"}, feedArgs(srv)...)...)
	s.want(r, 3, "upgrade")
	if r.stdout != "" || !strings.Contains(r.stderr, "no heo release is published") {
		t.Errorf("stdout %q stderr %q", r.stdout, r.stderr)
	}
}
