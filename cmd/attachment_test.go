package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"hop.top/kit/go/console/output"
)

// A provider's attachment name never leaves the directory it is
// written to, and never names nothing.
func TestFileNameIsLastElement(t *testing.T) {
	for in, want := range map[string]string{
		"invoice.pdf":          "invoice.pdf",
		"../../etc/passwd":     "passwd",
		`..\..\win\system.ini`: "system.ini",
		"/abs/path/photo.png":  "photo.png",
		"a\x00b\nc.txt":        "abc.txt",
		"":                     "attachment",
		"..":                   "attachment",
		".":                    "attachment",
		"dir/":                 "attachment",
		"  spaced.txt  ":       "spaced.txt",
	} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTarget(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ dest, name, want string }{
		{"", "a.txt", "a.txt"},
		{dir, "../a.txt", filepath.Join(dir, "a.txt")},
		{filepath.Join(dir, "out.bin"), "a.txt", filepath.Join(dir, "out.bin")},
	} {
		if got, err := target(tc.dest, tc.name); err != nil || got != tc.want {
			t.Errorf("target(%q, %q) = %q, %v; want %q", tc.dest, tc.name, got, err, tc.want)
		}
	}
}

// slurp reads a file the test made.
func slurp(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: the test's own temp file
}

func wantConflict(t *testing.T, err error) {
	t.Helper()
	oe, ok := err.(*output.Error)
	if !ok || oe.Code != output.CodeConflict || oe.ExitCode != output.ExitConflict {
		t.Fatalf("err = %#v, want a conflict", err)
	}
}

// An existing file is never overwritten without force, before the bytes
// move and again when they are written.
func TestWriteDownloadNeverOverwritesWithoutForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantConflict(t, checkDestination(path, false))
	wantConflict(t, writeDownload(path, []byte("new"), false))
	if b, _ := slurp(path); string(b) != "old" {
		t.Fatalf("file = %q after a refused overwrite", b)
	}
	if err := checkDestination(path, true); err != nil {
		t.Fatalf("check with force: %v", err)
	}
	if err := writeDownload(path, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if b, _ := slurp(path); string(b) != "new" {
		t.Fatalf("file = %q after force", b)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".heo-download-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestWriteDownloadCreatesOwnerOnly(t *testing.T) {
	for _, force := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "out.bin")
		if err := writeDownload(path, []byte("bytes"), force); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("force=%v: %v, %v; want 0600", force, fi, err)
		}
	}
}

// Nothing is refused where nothing would be overwritten.
func TestCheckDestinationAllows(t *testing.T) {
	dir := t.TempDir()
	for _, dest := range []string{"", "-", dir, filepath.Join(dir, "new.bin")} {
		if err := checkDestination(dest, false); err != nil {
			t.Errorf("checkDestination(%q) = %v", dest, err)
		}
	}
}

// A symlink in the way is not followed and not replaced without force.
func TestWriteDownloadDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Skip(err)
	}
	wantConflict(t, writeDownload(link, []byte("new"), false))
	wantConflict(t, checkDestination(link, false))
	if b, _ := slurp(victim); string(b) != "keep" {
		t.Fatalf("victim = %q", b)
	}
}
