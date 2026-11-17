package credentials

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// fileBackend is a native profile on the file backend, with its root
// directory.
func fileBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	cfg := fileConfig(t)
	return open(t, native, cfg), cfg.FileDir
}

// regularFile fails unless path is a regular file, and returns its
// bytes.
func regularFile(t *testing.T, path string) []byte {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s is %v, want a regular file", path, fi.Mode())
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file under the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFilePurposeSiblings: on the file backend a purpose entry is a
// file beside the shared one, <ref>@<purpose>, so a reference holds
// both; each resolves, and a purpose without a file of its own falls
// back to the shared file.
func TestFilePurposeSiblings(t *testing.T) {
	ctx := context.Background()
	b, dir := fileBackend(t)
	store(t, b, "support", envelope(t, "shared"))
	storePurpose(t, b, "support", "imap", envelope(t, "imap-own"))

	tenant := filepath.Join(dir, "uma", "default")
	shared := regularFile(t, filepath.Join(tenant, "support"))
	regularFile(t, filepath.Join(tenant, "support@imap"))

	if got := resolved(t, b, preq("support", "imap", false)); got != "imap-own" {
		t.Errorf("imap resolved %q, want its own file", got)
	}
	if got := resolved(t, b, preq("support", "smtp.submission", false)); got != "shared" {
		t.Errorf("smtp.submission resolved %q, want the shared file", got)
	}
	if d := b.Check(ctx, preq("support", "imap", false)); d.Status != StatusOK || d.Key != "uma/default/support/imap" {
		t.Errorf("Check imap = %+v", d)
	}
	if d := b.Check(ctx, preq("support", "smtp.submission", false)); d.Status != StatusOK || d.Key != "uma/default/support" {
		t.Errorf("Check smtp.submission = %+v", d)
	}

	// Rotating the purpose file leaves the shared file alone, and the
	// other way round.
	storePurpose(t, b, "support", "imap", envelope(t, "imap-2"))
	if got := regularFile(t, filepath.Join(tenant, "support")); !bytes.Equal(got, shared) {
		t.Errorf("rotating imap rewrote the shared file")
	}
	if got := resolved(t, b, preq("support", "smtp.submission", false)); got != "shared" {
		t.Errorf("smtp.submission after imap rotated = %q", got)
	}
	store(t, b, "support", envelope(t, "shared-2"))
	if got := resolved(t, b, preq("support", "imap", false)); got != "imap-2" {
		t.Errorf("imap after the shared file rotated = %q", got)
	}
	if got := resolved(t, b, preq("support", "smtp.submission", false)); got != "shared-2" {
		t.Errorf("smtp.submission after the shared file rotated = %q", got)
	}

	// Deleting the purpose file falls back to the shared one.
	keys, _ := b.Keys("support", "imap")
	if err := b.SurfaceTokens().Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, b, preq("support", "imap", false)); got != "shared-2" {
		t.Errorf("imap after its file was deleted = %q", got)
	}
	regularFile(t, filepath.Join(tenant, "support"))
}

// TestFileKeysStayLogical: the file layout is the file backend's own;
// keys, diagnoses and listings use the same uma/<tenant>/<ref>/<purpose>
// keys as every other backend.
func TestFileKeysStayLogical(t *testing.T) {
	ctx := context.Background()
	b, dir := fileBackend(t)
	want := []string{"uma/default/wa%2Fmain/meta.graph", "uma/default/wa%2Fmain"}
	if got, err := b.Keys("wa/main", "meta.graph"); err != nil || !slices.Equal(got, want) {
		t.Fatalf("Keys = %v, %v; want %v", got, err, want)
	}
	store(t, b, "wa/main", envelope(t, "shared"))
	storePurpose(t, b, "wa/main", "meta.graph", envelope(t, "graph"))
	regularFile(t, filepath.Join(dir, "uma", "default", "wa%2Fmain@meta.graph"))

	got, err := b.SurfaceTokens().List(ctx, "uma/default/")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"uma/default/wa%2Fmain", "uma/default/wa%2Fmain/meta.graph"}) {
		t.Errorf("List = %v", got)
	}
	if got, _ := b.SurfaceTokens().List(ctx, "uma/default/wa%2Fmain/"); !slices.Equal(got, []string{"uma/default/wa%2Fmain/meta.graph"}) {
		t.Errorf("List of the purpose entries = %v", got)
	}
	sec, err := b.SurfaceTokens().Get(ctx, want[0])
	if err != nil || sec.Key != want[0] {
		t.Errorf("Get = %+v, %v; want key %s", sec, err, want[0])
	}
}

// TestFileAtCannotCollide: '@' separates a reference from its purpose
// in a file name, so an '@' inside either is written %40: a reference
// "a@b" and reference "a" for purpose "b" are two files.
func TestFileAtCannotCollide(t *testing.T) {
	b, dir := fileBackend(t)
	store(t, b, "a@b", envelope(t, "ref a@b"))
	storePurpose(t, b, "a", "b", envelope(t, "ref a, purpose b"))
	storePurpose(t, b, "a", "x@y", envelope(t, "ref a, purpose x@y"))
	storePurpose(t, b, "a@x", "y", envelope(t, "ref a@x, purpose y"))

	tenant := filepath.Join(dir, "uma", "default")
	for _, name := range []string{"a%40b", "a@b", "a@x%40y", "a%40x@y"} {
		regularFile(t, filepath.Join(tenant, name))
	}
	for _, tc := range []struct{ ref, purpose, want string }{
		{"a@b", "imap", "ref a@b"},
		{"a", "b", "ref a, purpose b"},
		{"a", "x@y", "ref a, purpose x@y"},
		{"a@x", "y", "ref a@x, purpose y"},
	} {
		if got := resolved(t, b, preq(tc.ref, tc.purpose, false)); got != tc.want {
			t.Errorf("%s for %s resolved %q, want %q", tc.ref, tc.purpose, got, tc.want)
		}
	}
	if _, err := b.Resolve(context.Background(), preq("a", "imap", false)); model.CodeOf(err) != model.NotFound {
		t.Errorf("ref a has no shared file: %v, want not_found", err)
	}
}

// TestFileMissingNamesFiles: a missing credential's fix names the files
// to create, not the logical keys.
func TestFileMissingNamesFiles(t *testing.T) {
	b, dir := fileBackend(t)
	d := b.Check(context.Background(), preq("support", "imap", false))
	if d.Status != StatusMissing {
		t.Fatalf("Check = %+v, want missing", d)
	}
	for _, f := range []string{"support@imap", "support"} {
		if p := filepath.Join(dir, "uma", "default", f); !strings.Contains(d.SuggestedFix, p) {
			t.Errorf("fix %q does not name %s", d.SuggestedFix, p)
		}
	}
	if strings.Contains(d.SuggestedFix, filepath.Join("support", "imap")) {
		t.Errorf("fix %q names the nested layout", d.SuggestedFix)
	}
}

// TestFilePreviousLayout: purpose files an earlier build kept in a
// directory named after the reference (<ref>/<purpose>) are not read:
// every lookup of that reference fails, naming the move, and resolves
// once the files are moved.
func TestFilePreviousLayout(t *testing.T) {
	ctx := context.Background()
	b, dir := fileBackend(t)
	old := filepath.Join(dir, "uma", "default", "split")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "imap"), envelope(t, seed), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"imap", "smtp.submission"} {
		_, err := b.Resolve(ctx, preq("split", p, false))
		if err == nil || model.CodeOf(err) == model.NotFound {
			t.Errorf("resolve %s in the earlier layout: %v, want a failure that is not not_found", p, err)
		}
		noSeed(t, "earlier layout error", err)
		d := b.Check(ctx, preq("split", p, false))
		if d.Status != StatusMalformed {
			t.Errorf("Check %s = %+v, want malformed", p, d)
		}
		for _, s := range []string{old + string(filepath.Separator) + "<purpose>", old + "@<purpose>"} {
			if !strings.Contains(d.SuggestedFix, s) {
				t.Errorf("fix %q does not name %s", d.SuggestedFix, s)
			}
		}
		noSeed(t, "earlier layout diagnosis", d)
	}
	// Another reference is unaffected.
	store(t, b, "other", envelope(t, "other"))
	if got := resolved(t, b, preq("other", "imap", false)); got != "other" {
		t.Errorf("other reference = %q", got)
	}

	if err := os.Rename(filepath.Join(old, "imap"), old+"@imap"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	if got := resolved(t, b, preq("split", "imap", false)); got != seed {
		t.Errorf("after the move imap resolved %q", got)
	}
}

// TestOtherBackendsKeepNestedKeys: only the file backend maps a purpose
// key to a sibling; a kit store such as the keyring holds the key as
// is.
func TestOtherBackendsKeepNestedKeys(t *testing.T) {
	ctx := context.Background()
	keyrings := stubKeyring(t)
	b := open(t, native, Config{Backend: BackendKeyring, KeyringService: "heo-test"})
	storePurpose(t, b, "a@b", "imap", envelope(t, "own"))
	for _, k := range []string{"uma/default/a@b/imap"} {
		if ok, err := keyrings["heo-test"].Exists(ctx, k); err != nil || !ok {
			t.Errorf("keyring has no %s: %v", k, err)
		}
	}
	if d := b.Check(ctx, preq("a@b", "imap", false)); d.Status != StatusOK || d.Key != "uma/default/a@b/imap" {
		t.Errorf("Check = %+v", d)
	}
}
