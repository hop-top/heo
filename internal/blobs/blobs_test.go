package blobs_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hop.top/heo/internal/blobs"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func key() ports.BlobKey {
	return ports.BlobKey{Tenant: "acme", ID: "0190c1b2-7d3e-7a10-8c11-2f4e5a6b7c8d"}
}

// The store keeps what was staged, byte for byte, and gives it back.
func TestStageThenOpen(t *testing.T) {
	s, err := blobs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stage(t.Context(), key(), strings.NewReader("attached bytes"), 14); err != nil {
		t.Fatal(err)
	}
	rc, err := s.Open(t.Context(), key())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); string(b) != "attached bytes" {
		t.Fatalf("bytes = %q", b)
	}
}

// The root is owner-only whether it was created or already there, so
// the group-readable files below it are unreachable for other users.
func TestRootIsOwnerOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	for name, prepare := range map[string]func(){
		"created":  func() {},
		"existing": func() { _ = os.MkdirAll(root, 0o755); _ = os.Chmod(root, 0o755) }, //nolint:gosec // G301: a lax directory to be tightened
	} {
		t.Run(name, func(t *testing.T) {
			prepare()
			s, err := blobs.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Stage(t.Context(), key(), strings.NewReader("x"), 1); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Stat(root); err != nil || fi.Mode().Perm() != 0o700 {
				t.Fatalf("root = %v, %v; want 0700", fi, err)
			}
		})
	}
}

// A missing blob is not found, not a failure.
func TestMissingBlobNotFound(t *testing.T) {
	s, err := blobs.Open(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(t.Context(), key()); !model.IsCode(err, model.NotFound) {
		t.Fatalf("Open = %v, want not found", err)
	}
}

func TestOpenNeedsDirectory(t *testing.T) {
	if _, err := blobs.Open(""); err == nil {
		t.Fatal("Open(\"\") succeeded")
	}
}
