package bundle_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/bundle"
	kitlocal "hop.top/kit/go/storage/blob/local"
	"hop.top/uma/model"
)

// fakeOwner is a storage.Owner whose backup is a small file and whose
// restore records what it was given.
type fakeOwner struct {
	storage.Owner
	dir     string
	payload []byte
	schema  int64
	library int64

	restoreErr   error
	restoreCalls int
	restored     []byte
	// seen is the blob directory as the store restore found it.
	seen func()
}

func newOwner(t *testing.T) *fakeOwner {
	t.Helper()
	return &fakeOwner{dir: filepath.Join(t.TempDir(), "backups"), payload: []byte("PGDMP-store-bytes"), schema: 7, library: 7}
}

func (f *fakeOwner) Versions(context.Context) (storage.Versions, error) {
	return storage.Versions{Driver: "fake", Schema: f.schema, LibrarySchema: f.library, Library: "fake@v1"}, nil
}

func (f *fakeOwner) Backup(_ context.Context, reason string) (storage.Backup, error) {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return storage.Backup{}, err
	}
	p := filepath.Join(f.dir, "fake-"+time.Now().UTC().Format("150405.000000000")+"-"+reason+".dump")
	if err := os.WriteFile(p, f.payload, 0o600); err != nil {
		return storage.Backup{}, err
	}
	return storage.Backup{Path: p, Driver: "fake", Schema: f.schema, Reason: reason, CreatedAt: time.Now().UTC()}, nil
}

func (f *fakeOwner) Restore(_ context.Context, path string) error {
	f.restoreCalls++
	if f.seen != nil {
		f.seen()
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the staged dump
	if err != nil {
		return err
	}
	f.restored = b
	return f.restoreErr
}

// mkTree writes a blob tree whose modes are what the backend writes,
// and a few it would not, so a restore that only inherits the umask
// shows.
func mkTree(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, body string, mode fs.FileMode) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write("acme/ab/0190-first", "first attachment bytes", 0o640)
	write("acme/ab/0190-second", strings.Repeat("\x00\xff", 4096), 0o600)
	write("acme/cd/0190-third", "third", 0o660)
	if err := os.MkdirAll(filepath.Join(dir, "acme", "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	for rel, m := range map[string]fs.FileMode{"acme": 0o750, "acme/ab": 0o750, "acme/cd": 0o770, "acme/empty": 0o750} {
		if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(rel)), m); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory
		t.Fatal(err)
	}
}

// snapshot is every entry under dir with its mode and, for files, its
// bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		v := fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(p) //nolint:gosec // G304: a file the test made
			if err != nil {
				return err
			}
			v += " " + string(b)
		}
		out[rel] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t *testing.T, got, want map[string]string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d entries, want %d", what, len(got), len(want))
	}
	for k, w := range want {
		if g, ok := got[k]; !ok || g != w {
			t.Errorf("%s: %q = %q, want %q", what, k, truncate(g), truncate(w))
		}
	}
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

// site is a profile: a store owner, the blob directory, and the wrapped
// owner over both.
type site struct {
	o    *fakeOwner
	tree string
	w    *bundle.Owner
}

func newSite(t *testing.T) *site {
	t.Helper()
	o := newOwner(t)
	tree := filepath.Join(t.TempDir(), "data", "blobs")
	mkTree(t, tree)
	return &site{o: o, tree: tree, w: bundle.Wrap(o, o.dir, bundle.LocalDir(tree))}
}

func (s *site) backup(t *testing.T, reason string) storage.Backup {
	t.Helper()
	b, err := s.w.Backup(t.Context(), reason)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return b
}

// A backup is one owner-only archive; restoring it onto a changed
// profile brings back the store dump and every attachment, byte for
// byte, with the modes the backend wrote.
func TestRoundTrip(t *testing.T) {
	s := newSite(t)
	want := snapshot(t, s.tree)
	b := s.backup(t, "manual")

	if filepath.Ext(b.Path) != bundle.Ext || filepath.Dir(b.Path) != s.o.dir {
		t.Errorf("archive = %s", b.Path)
	}
	if fi, err := os.Stat(b.Path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v, %v; want 0600", fi, err)
	}
	if got := entries(t, s.o.dir); len(got) != 1 {
		t.Errorf("backup directory = %v, want the archive alone", got)
	}
	if b.Blobs == nil || b.Blobs.Files != 3 || b.Blobs.Bytes != int64(len("first attachment bytes")+8192+len("third")) {
		t.Errorf("blobs = %+v", b.Blobs)
	}

	// The profile moves on: bytes change, files come and go.
	if err := os.WriteFile(filepath.Join(s.tree, "acme/ab/0190-first"), []byte("overwritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.tree, "acme", "cd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.tree, "acme", "later"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.w.Restore(t.Context(), b.Path); err != nil {
		t.Fatalf("restore: %v", err)
	}
	sameSnapshot(t, snapshot(t, s.tree), want, "restored tree")
	if !bytes.Equal(s.o.restored, s.o.payload) {
		t.Errorf("store restored from %q, want %q", s.o.restored, s.o.payload)
	}
	// The tree sits alone beside its parent: no staging, no old copy.
	if got := entries(t, filepath.Dir(s.tree)); len(got) != 2 {
		t.Errorf("beside the blob directory: %v, want blobs and its kept copy", got)
	}
}

// The directory is owner-only whatever the archive says, and modes the
// backend wrote survive where the umask would change them.
func TestRestoreModes(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, "manual")
	if err := os.RemoveAll(s.tree); err != nil {
		t.Fatal(err)
	}
	if err := s.w.Restore(t.Context(), b.Path); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fs.FileMode{
		".": 0o700, "acme/cd": 0o770 | fs.ModeDir, "acme/ab": 0o750 | fs.ModeDir,
		"acme/ab/0190-first": 0o640, "acme/ab/0190-second": 0o600, "acme/cd/0190-third": 0o660,
	} {
		fi, err := os.Stat(filepath.Join(s.tree, rel))
		if err != nil {
			t.Fatal(err)
		}
		if rel == "." {
			want |= fs.ModeDir
		}
		if fi.Mode() != want {
			t.Errorf("%s mode = %v, want %v", rel, fi.Mode(), want)
		}
	}
}

// Restoring over a directory that has more than the backup replaces it
// whole. The old tree is deleted when a backup already holds it, and
// otherwise kept beside the directory and reported.
func TestRestoreReplacedTree(t *testing.T) {
	for _, covered := range []bool{true, false} {
		s := newSite(t)
		want := snapshot(t, s.tree)
		b := s.backup(t, "manual")
		if err := os.WriteFile(filepath.Join(s.tree, "stale"), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := bundle.Examine(t.Context(), b.Path)
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.w.RestoreArchive(t.Context(), a, bundle.RestoreOptions{Covered: covered})
		if err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, snapshot(t, s.tree), want, "restored tree")
		if res.Blobs == nil || res.Blobs.Files != 3 {
			t.Errorf("result = %+v", res)
		}
		if covered {
			if res.Replaced != "" || len(entries(t, filepath.Dir(s.tree))) != 1 {
				t.Errorf("covered: replaced %q, beside: %v", res.Replaced, entries(t, filepath.Dir(s.tree)))
			}
			continue
		}
		if res.Replaced == "" {
			t.Fatal("the replaced tree was deleted without a backup of it")
		}
		if b, err := os.ReadFile(filepath.Join(res.Replaced, "stale")); err != nil || string(b) != "stale" {
			t.Errorf("kept tree: %q, %v", b, err)
		}
	}
}

// The backup before a migration carries no blobs, and restoring it
// leaves the blob directory as it is.
func TestPreMigrateBackupLeavesBlobsAlone(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, storage.ReasonPreMigrate)
	if b.Blobs != nil {
		t.Errorf("pre-migrate backup carries blobs: %+v", b.Blobs)
	}
	if err := os.WriteFile(filepath.Join(s.tree, "acme", "later"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s.tree)
	a, err := bundle.Examine(t.Context(), b.Path)
	if err != nil || a.HasBlobs() {
		t.Fatalf("examine: %+v, %v", a, err)
	}
	res, err := s.w.RestoreArchive(t.Context(), a, bundle.RestoreOptions{})
	if err != nil || res.Blobs != nil {
		t.Fatalf("restore: %+v, %v", res, err)
	}
	sameSnapshot(t, snapshot(t, s.tree), before, "blob directory")
	if !bytes.Equal(s.o.restored, s.o.payload) {
		t.Errorf("store not restored: %q", s.o.restored)
	}
}

// A backup an older heo wrote is a bare driver dump: it restores the
// store as before and leaves the blob directory alone.
func TestLegacyDumpRestores(t *testing.T) {
	s := newSite(t)
	legacy := filepath.Join(t.TempDir(), "old.dump")
	if err := os.WriteFile(legacy, []byte("PGDMP-an-older-backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s.tree)
	a, err := bundle.Examine(t.Context(), legacy)
	if err != nil || !a.Legacy || a.HasBlobs() {
		t.Fatalf("examine: %+v, %v", a, err)
	}
	if err := a.Compatible(storage.Versions{Driver: "anything", LibrarySchema: 1}); err != nil {
		t.Errorf("a legacy dump has no manifest to mismatch: %v", err)
	}
	if err := s.w.Restore(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	sameSnapshot(t, snapshot(t, s.tree), before, "blob directory")
	if string(s.o.restored) != "PGDMP-an-older-backup" {
		t.Errorf("store restored from %q", s.o.restored)
	}
}

func TestExamineRefusesWhatIsNotABackup(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk")
	if err := os.WriteFile(junk, []byte("not a backup at all, nor a tar archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Examine(t.Context(), junk); !model.IsCode(err, model.Validation) {
		t.Errorf("junk: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Examine(t.Context(), empty); !model.IsCode(err, model.Validation) {
		t.Errorf("empty: %v", err)
	}
	if _, err := bundle.Examine(t.Context(), filepath.Join(dir, "missing")); !model.IsCode(err, model.NotFound) {
		t.Errorf("missing: %v", err)
	}
	// A tar archive that does not start with the manifest.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "store.dump", Size: 1, Mode: 0o600})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	other := filepath.Join(dir, "other.tar")
	if err := os.WriteFile(other, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.Examine(t.Context(), other); !model.IsCode(err, model.Validation) {
		t.Errorf("manifest-less tar: %v", err)
	}
}

// entry is one tar entry with its body, for rewriting an archive.
type entry struct {
	hdr  tar.Header
	body []byte
}

func readArchive(t *testing.T, path string) []entry {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: the archive the test made
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []entry
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry{hdr: *h, body: b})
	}
}

func writeArchive(t *testing.T, path string, es []entry) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := e.hdr
		h.Size = int64(len(e.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// edit rewrites the archive at path: mut changes the manifest, then
// the entries.
func edit(t *testing.T, path string, mut func(m *bundle.Manifest), es func([]entry) []entry) {
	t.Helper()
	all := readArchive(t, path)
	if mut != nil {
		var m bundle.Manifest
		if err := json.Unmarshal(all[0].body, &m); err != nil {
			t.Fatal(err)
		}
		mut(&m)
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		all[0].body = b
	}
	if es != nil {
		all = es(all)
	}
	writeArchive(t, path, all)
}

// pristine says a refused restore left every part of the profile as it
// was: the blob directory, the store, and the backup directory.
type pristine struct {
	s      *site
	tree   map[string]string
	backup []string
}

func hold(t *testing.T, s *site) pristine {
	t.Helper()
	return pristine{s: s, tree: snapshot(t, s.tree), backup: entries(t, s.o.dir)}
}

func (p pristine) check(t *testing.T) {
	t.Helper()
	sameSnapshot(t, snapshot(t, p.s.tree), p.tree, "blob directory")
	if p.s.o.restoreCalls != 0 {
		t.Errorf("the store was restored %d times", p.s.o.restoreCalls)
	}
	if got := entries(t, p.s.o.dir); len(got) != len(p.backup) {
		t.Errorf("backup directory %v, was %v", got, p.backup)
	}
	if got := entries(t, filepath.Dir(p.s.tree)); len(got) != 1 {
		t.Errorf("beside the blob directory: %v", got)
	}
}

// An altered blob refuses the backup before anything changes, and the
// message does not name the blob.
func TestTamperedBlobRefused(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, "manual")
	edit(t, b.Path, nil, func(es []entry) []entry {
		for i := range es {
			if strings.HasSuffix(es[i].hdr.Name, "0190-first") {
				es[i].body = []byte("first attachment byteX")
			}
		}
		return es
	})
	p := hold(t, s)
	_, err := bundle.Examine(t.Context(), b.Path)
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("examine = %v, want a validation refusal", err)
	}
	if strings.Contains(err.Error(), "0190") {
		t.Errorf("the refusal names a blob: %v", err)
	}
	if err := s.w.Restore(t.Context(), b.Path); !model.IsCode(err, model.Validation) {
		t.Errorf("restore = %v", err)
	}
	p.check(t)
}

// A backup altered after it was examined is caught when its entries
// are staged, again before the live directory is touched.
func TestBackupChangedAfterExamineRefused(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, "manual")
	a, err := bundle.Examine(t.Context(), b.Path)
	if err != nil {
		t.Fatal(err)
	}
	edit(t, b.Path, nil, func(es []entry) []entry {
		es[len(es)-1].body = bytes.ToUpper(es[len(es)-1].body)
		return es
	})
	p := hold(t, s)
	if _, err := s.w.RestoreArchive(t.Context(), a, bundle.RestoreOptions{}); !model.IsCode(err, model.Validation) {
		t.Fatalf("restore = %v, want a validation refusal", err)
	}
	p.check(t)
}

// A manifest that disagrees with its archive, or with itself, refuses
// the backup: nothing is restored from an archive that cannot be
// trusted whole.
func TestManifestMismatchRefused(t *testing.T) {
	last := func(es []entry) int { return len(es) - 1 }
	cases := map[string]struct {
		mut func(*bundle.Manifest)
		es  func([]entry) []entry
	}{
		"count too high":  {mut: func(m *bundle.Manifest) { m.Blobs.Count++ }},
		"total too low":   {mut: func(m *bundle.Manifest) { m.Blobs.Bytes-- }},
		"newer format":    {mut: func(m *bundle.Manifest) { m.Format = bundle.FormatVersion + 1 }},
		"no format":       {mut: func(m *bundle.Manifest) { m.Format = 0 }},
		"no driver":       {mut: func(m *bundle.Manifest) { m.Driver = "" }},
		"store checksum":  {mut: func(m *bundle.Manifest) { m.Store.SHA256 = strings.Repeat("0", 64) }},
		"store size":      {mut: func(m *bundle.Manifest) { m.Store.Size++ }},
		"blob checksum":   {mut: func(m *bundle.Manifest) { m.Blobs.Files[0].SHA256 = strings.Repeat("0", 64) }},
		"blob size":       {mut: func(m *bundle.Manifest) { m.Blobs.Files[0].Size++; m.Blobs.Bytes++ }},
		"bad checksum":    {mut: func(m *bundle.Manifest) { m.Blobs.Files[0].SHA256 = "zz" }},
		"escaping path":   {mut: func(m *bundle.Manifest) { m.Blobs.Files[0].Path = "../escape" }},
		"absolute path":   {mut: func(m *bundle.Manifest) { m.Blobs.Files[0].Path = "/etc/passwd" }},
		"duplicate entry": {mut: func(m *bundle.Manifest) { m.Blobs.Files[1].Path = m.Blobs.Files[0].Path }},
		"missing entry":   {es: func(es []entry) []entry { return es[:last(es)] }},
		"unlisted entry": {es: func(es []entry) []entry {
			return append(es, entry{hdr: tar.Header{Name: "blobs/extra", Mode: 0o600}, body: []byte("x")})
		}},
		"repeated entry": {es: func(es []entry) []entry { return append(es, es[last(es)]) }},
		"entry kind": {es: func(es []entry) []entry {
			es[last(es)].hdr.Typeflag = tar.TypeSymlink
			es[last(es)].hdr.Linkname = "x"
			es[last(es)].body = nil
			return es
		}},
		"truncated archive": {es: nil},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSite(t)
			b := s.backup(t, "manual")
			if name == "truncated archive" {
				raw, err := os.ReadFile(b.Path) //nolint:gosec // G304: the archive the test made
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(b.Path, raw[:len(raw)/2], 0o600); err != nil { //nolint:gosec // G703: a backup the test made
					t.Fatal(err)
				}
			} else {
				edit(t, b.Path, c.mut, c.es)
			}
			p := hold(t, s)
			if err := s.w.Restore(t.Context(), b.Path); !model.IsCode(err, model.Validation) {
				t.Fatalf("restore = %v, want a validation refusal", err)
			}
			p.check(t)
		})
	}
}

// An archive from another driver, or from a newer schema than this
// binary runs, is refused against the store it would replace.
func TestCompatible(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, "manual")
	a, err := bundle.Examine(t.Context(), b.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Compatible(storage.Versions{Driver: "fake", Schema: 3, LibrarySchema: 7}); err != nil {
		t.Errorf("same schema: %v", err)
	}
	if err := a.Compatible(storage.Versions{Driver: "fake", LibrarySchema: 9}); err != nil {
		t.Errorf("older schema: %v", err)
	}
	if err := a.Compatible(storage.Versions{Driver: "other", LibrarySchema: 7}); !model.IsCode(err, model.Validation) {
		t.Errorf("other driver: %v", err)
	}
	if err := a.Compatible(storage.Versions{Driver: "fake", LibrarySchema: 6, Library: "fake@v0"}); !model.IsCode(err, model.SchemaMismatch) {
		t.Errorf("newer schema: %v", err)
	}
	// Restore applies the same checks.
	s.o.library = 6
	p := hold(t, s)
	if err := s.w.Restore(t.Context(), b.Path); !model.IsCode(err, model.SchemaMismatch) {
		t.Errorf("restore of a newer schema: %v", err)
	}
	p.check(t)
}

// A store that fails to restore leaves the blob directory as it was,
// with no staging left behind: the failed restore can be retried.
func TestFailedStoreRestoreKeepsOldTree(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, "manual")
	if err := os.WriteFile(filepath.Join(s.tree, "acme", "later"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, s.tree)
	s.o.restoreErr = errors.New("pg_restore failed")
	// By the time the store is restored the new tree is in place.
	s.o.seen = func() {
		if _, err := os.Stat(filepath.Join(s.tree, "acme", "later")); err == nil {
			t.Error("the store was restored before the blob directory was swapped")
		}
	}
	err := s.w.Restore(t.Context(), b.Path)
	if err == nil || !strings.Contains(err.Error(), "pg_restore failed") {
		t.Fatalf("restore = %v", err)
	}
	sameSnapshot(t, snapshot(t, s.tree), before, "blob directory")
	if got := entries(t, filepath.Dir(s.tree)); len(got) != 1 {
		t.Errorf("beside the blob directory: %v", got)
	}
	for _, n := range entries(t, s.o.dir) {
		if strings.HasPrefix(n, ".restore-") {
			t.Errorf("staged dump left behind: %s", n)
		}
	}
	// Retried once the cause is fixed.
	s.o.restoreErr, s.o.seen = nil, nil
	if err := s.w.Restore(t.Context(), b.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.tree, "acme", "later")); err == nil {
		t.Error("retry kept the later file")
	}
}

// When the staging area cannot be made, nothing is touched and the
// store is not restored.
func TestStagingUnavailableChangesNothing(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("permissions do not bind this user")
	}
	s := newSite(t)
	b := s.backup(t, "manual")
	parent := filepath.Dir(s.tree)
	if err := os.Chmod(parent, 0o500); err != nil { //nolint:gosec // G302: a directory the test makes read-only
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) }) //nolint:gosec // G302: a directory
	p := hold(t, s)
	if err := s.w.Restore(t.Context(), b.Path); !model.IsCode(err, model.Unavailable) {
		t.Fatalf("restore = %v, want unavailable", err)
	}
	_ = os.Chmod(parent, 0o700) //nolint:gosec // G302: a directory
	p.check(t)
}

// A blob directory that was never made is an empty tree: restoring
// makes it, owner-only.
func TestMissingBlobDirectory(t *testing.T) {
	o := newOwner(t)
	tree := filepath.Join(t.TempDir(), "never", "blobs")
	w := bundle.Wrap(o, o.dir, bundle.LocalDir(tree))
	b, err := w.Backup(t.Context(), "manual")
	if err != nil || b.Blobs == nil || b.Blobs.Files != 0 {
		t.Fatalf("backup = %+v, %v", b, err)
	}
	if _, err := os.Stat(tree); err == nil {
		t.Fatal("a backup created the blob directory")
	}
	if err := w.Restore(t.Context(), b.Path); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(tree); err != nil || fi.Mode().Perm() != 0o700 || !fi.IsDir() {
		t.Errorf("blob directory = %v, %v", fi, err)
	}
}

// A blob directory that is a symlink to another volume stays a link:
// the tree behind it is what is replaced.
func TestSymlinkedBlobDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	o := newOwner(t)
	base := t.TempDir()
	real := filepath.Join(base, "volume", "blobs")
	mkTree(t, real)
	link := filepath.Join(base, "blobs")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	w := bundle.Wrap(o, o.dir, bundle.LocalDir(link))
	want := snapshot(t, real)
	b, err := w.Backup(t.Context(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "extra"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := bundle.Examine(t.Context(), b.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.RestoreArchive(t.Context(), a, bundle.RestoreOptions{Covered: true}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v, %v", fi, err)
	}
	sameSnapshot(t, snapshot(t, real), want, "linked tree")
}

// Anything but files and directories in the blob tree stops the backup,
// and leaves nothing behind.
func TestBackupRefusesOtherEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	s := newSite(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(s.tree, "acme", "link")); err != nil {
		t.Fatal(err)
	}
	_, err := s.w.Backup(t.Context(), "manual")
	if !model.IsCode(err, model.Unsupported) {
		t.Fatalf("backup = %v", err)
	}
	if strings.Contains(err.Error(), "link") {
		t.Errorf("the refusal names the entry: %v", err)
	}
	if got := entries(t, s.o.dir); len(got) != 0 {
		t.Errorf("a refused backup left %v", got)
	}
}

// A backend that is not a local directory has no tree: archives hold
// the store alone, and an archive with blobs has nowhere to put them.
func TestNoTree(t *testing.T) {
	s := newSite(t)
	withBlobs := s.backup(t, "manual")

	o := newOwner(t)
	w := bundle.Wrap(o, o.dir, nil)
	b, err := w.Backup(t.Context(), "manual")
	if err != nil || b.Blobs != nil {
		t.Fatalf("backup = %+v, %v", b, err)
	}
	if err := w.Restore(t.Context(), b.Path); err != nil {
		t.Errorf("store-only archive: %v", err)
	}
	o.restoreCalls = 0
	if err := w.Restore(t.Context(), withBlobs.Path); !model.IsCode(err, model.Unsupported) || o.restoreCalls != 0 {
		t.Errorf("archive with blobs = %v (store restored %d times)", err, o.restoreCalls)
	}
}

// Staging an interrupted restore left is cleared; nothing else is.
func TestStaleStagingRemoved(t *testing.T) {
	s := newSite(t)
	b := s.backup(t, "manual")
	parent := filepath.Dir(s.tree)
	stale := filepath.Join(parent, "blobs.restore-123456")
	keep := filepath.Join(parent, "blobs.restore-notes")
	for _, d := range []string{stale, keep} {
		if err := os.MkdirAll(filepath.Join(d, "x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.w.Restore(t.Context(), b.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale staging kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("an unrelated directory was removed: %v", err)
	}
}

func TestInspect(t *testing.T) {
	s := newSite(t)
	st, err := bundle.Inspect(t.Context(), bundle.LocalDir(s.tree))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Exists || st.Dir != s.tree || st.Files != 3 || st.Bytes != int64(len("first attachment bytes")+8192+len("third")) {
		t.Errorf("stats = %+v", st)
	}
	st, err = bundle.Inspect(t.Context(), bundle.LocalDir(filepath.Join(s.tree, "nope")))
	if err != nil || st.Exists || st.Files != 0 {
		t.Errorf("missing tree = %+v, %v", st, err)
	}
}

// A cancelled context stops a backup and leaves no archive.
func TestBackupCancelled(t *testing.T) {
	s := newSite(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.w.Backup(ctx, "manual"); err == nil {
		t.Fatal("backup under a cancelled context succeeded")
	}
	if got := entries(t, s.o.dir); len(got) != 0 {
		t.Errorf("left %v", got)
	}
}

// TestInspectCountsBlobsNotSidecars: the backend's content-type sidecars
// are files in the tree but not blobs.
func TestInspectCountsBlobsNotSidecars(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "blobs")
	st, err := kitlocal.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for k, body := range map[string]string{"t/a": "aaa", "t/sub/b": "bbbbb"} {
		if err := st.Put(ctx, k, strings.NewReader(body), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := bundle.Inspect(ctx, bundle.LocalDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got.Files != 2 || got.Bytes != 8 {
		t.Fatalf("Inspect = %+v, want 2 blobs, 8 bytes", got)
	}
}
