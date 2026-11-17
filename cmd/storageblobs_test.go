package cmd

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"hop.top/heo/internal/blobs"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/storage/bundle"
	kitblob "hop.top/kit/go/storage/blob"
	kitlocal "hop.top/kit/go/storage/blob/local"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func blobKey(id string) ports.BlobKey {
	return ports.BlobKey{Tenant: storeTenant, ID: model.ID(id)}
}

// stage keeps bytes in the profile's blob directory the way the daemon
// does.
func (e *storeEnv) stage(id, body string) {
	e.t.Helper()
	s, err := blobs.Open(e.blobs)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := s.Stage(e.t.Context(), blobKey(id), strings.NewReader(body), int64(len(body))); err != nil {
		e.t.Fatalf("stage %s: %v", id, err)
	}
}

// blob reads attachment id back, "" and false when it is not there.
func (e *storeEnv) blob(id string) (string, bool) {
	e.t.Helper()
	s, err := blobs.Open(e.blobs)
	if err != nil {
		e.t.Fatal(err)
	}
	rc, err := s.Open(e.t.Context(), blobKey(id))
	if err != nil {
		return "", false
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b), true
}

// blobTree is every file under the blob directory with its mode and
// bytes, to compare whole trees.
func (e *storeEnv) blobTree() []string {
	e.t.Helper()
	var out []string
	_ = filepath.WalkDir(e.blobs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(e.blobs, p)
		line := rel + " " + fi.Mode().String()
		if fi.Mode().IsRegular() {
			b, _ := os.ReadFile(p) //nolint:gosec // G304: a file the test made
			line += " " + string(b)
		}
		out = append(out, line)
		return nil
	})
	sort.Strings(out)
	return out
}

// contentTypes is the content type the backend reports for every blob
// under the blob directory, by key.
func (e *storeEnv) contentTypes() map[string]string {
	e.t.Helper()
	st, err := kitlocal.New(e.blobs)
	if err != nil {
		e.t.Fatal(err)
	}
	var objs []kitblob.Object
	if objs, err = st.List(e.t.Context(), ""); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, o := range objs {
		out[o.Key] = o.ContentType
	}
	return out
}

// beside lists what sits next to the blob directory: staging and kept
// copies would show.
func (e *storeEnv) beside() []string {
	e.t.Helper()
	des, err := os.ReadDir(filepath.Dir(e.blobs))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

type archiveEntry struct {
	hdr  tar.Header
	body []byte
}

// rewriteArchive edits the archive at path: fix changes the manifest
// (a map, so no field is lost), then entries.
func rewriteArchive(t *testing.T, path string, fix func(m map[string]any), mut func([]archiveEntry) []archiveEntry) {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: a backup the test made
	if err != nil {
		t.Fatal(err)
	}
	var all []archiveEntry
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		all = append(all, archiveEntry{hdr: *h, body: b})
	}
	_ = f.Close()
	if fix != nil {
		var m map[string]any
		if err := json.Unmarshal(all[0].body, &m); err != nil {
			t.Fatal(err)
		}
		fix(m)
		if all[0].body, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
	}
	if mut != nil {
		all = mut(all)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, a := range all {
		h := a.hdr
		h.Size = int64(len(a.body))
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(a.body)
	}
	_ = tw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	attA = "0190c1b2-7d3e-7a10-8c11-2f4e5a6b7c01"
	attB = "0190c1b2-7d3e-7a10-8c11-2f4e5a6b7c02"
	attC = "0190c1b2-7d3e-7a10-8c11-2f4e5a6b7c03"
)

// seedWithBlobs migrates the store and stages two attachments with a contact and two attachments.
func (e *storeEnv) seedWithBlobs() {
	e.t.Helper()
	e.run(0, "storage", "migrate")
	e.stage(attA, "attachment A bytes")
	e.stage(attB, strings.Repeat("\x00\xffB", 1000))
}

// TestStorageBackupRestoreCarriesBlobs: one archive holds the store and
// the attachments; after the data moves on, a restore brings back the
// contact and the attachment bytes, exactly, and drops what came later.
// Its pre-restore backup holds the attachments it replaced.
func TestStorageBackupRestoreCarriesBlobs(t *testing.T) {
	e := newStoreEnv(t)
	e.seedWithBlobs()
	seeded := e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
	treeAtBackup := e.blobTree()
	typesAtBackup := e.contentTypes()
	if len(typesAtBackup) != 2 {
		t.Fatalf("blobs at backup = %v, want 2", typesAtBackup)
	}
	for k, ct := range typesAtBackup {
		if ct == "" {
			t.Fatalf("blob %s has no content type before the backup", k)
		}
	}

	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	if filepath.Ext(b.Path) != bundle.Ext {
		t.Errorf("backup = %s, want one %s archive", b.Path, bundle.Ext)
	}
	if b.Blobs == nil || b.Blobs.Files != 2 || b.Blobs.Bytes != int64(len("attachment A bytes")+3000) {
		t.Fatalf("backup blobs = %+v", b.Blobs)
	}
	if n := len(e.backups()); n != 1 {
		t.Errorf("backup directory holds %d files, want the archive alone: %v", n, e.backups())
	}

	later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
	e.stage(attC, "attachment C, after the backup")
	if err := os.WriteFile(blobFileOf(t, e.blobs, "attachment A"), []byte("A overwritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	treeAfter := e.blobTree()

	r := decode[restoreOut](t, e.run(0, "storage", "restore", b.Path, "--confirm=yes", "--format", "json").stdout)
	if !e.hasContact(seeded) || e.hasContact(later) {
		t.Error("store not restored to the backup")
	}
	if got, ok := e.blob(attA); !ok || got != "attachment A bytes" {
		t.Errorf("attachment A = %q, %v", got, ok)
	}
	if got, ok := e.blob(attB); !ok || got != strings.Repeat("\x00\xffB", 1000) {
		t.Errorf("attachment B restored wrong (%d bytes, %v)", len(got), ok)
	}
	if _, ok := e.blob(attC); ok {
		t.Error("an attachment staged after the backup survived the restore")
	}
	if got := e.contentTypes(); !maps.Equal(got, typesAtBackup) {
		t.Errorf("content types after restore = %v, want %v", got, typesAtBackup)
	}
	if got := e.blobTree(); strings.Join(got, "\n") != strings.Join(treeAtBackup, "\n") {
		t.Errorf("tree after restore:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(treeAtBackup, "\n"))
	}
	if fi, err := os.Stat(e.blobs); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("blob directory = %v, %v; want 0700", fi, err)
	}
	if got := e.beside(); len(got) != 1 {
		t.Errorf("beside the blob directory: %v, want no staging and no kept copy", got)
	}
	if r.Blobs == nil || r.Blobs.Files != 2 || r.BlobsUntouched || r.ReplacedBlobs != "" {
		t.Errorf("restore result = %+v", r)
	}

	// The pre-restore backup holds the attachments the restore replaced.
	if r.Safety == nil || r.Safety.Blobs == nil || r.Safety.Blobs.Files != 3 {
		t.Fatalf("pre-restore backup = %+v", r.Safety)
	}
	e.run(0, "storage", "restore", r.Safety.Path, "--confirm=yes")
	if !e.hasContact(later) {
		t.Error("pre-restore backup lost the store's data")
	}
	if got, ok := e.blob(attC); !ok || got != "attachment C, after the backup" {
		t.Errorf("pre-restore backup lost attachment C: %q, %v", got, ok)
	}
	if got := e.blobTree(); strings.Join(got, "\n") != strings.Join(treeAfter, "\n") {
		t.Errorf("tree after undoing the restore:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(treeAfter, "\n"))
	}
}

// blobFileOf finds the one file under dir holding the bytes that start
// with prefix.
func blobFileOf(t *testing.T, dir, prefix string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.HasPrefix(string(b), prefix) { //nolint:gosec // G304: a file the test made
				found = p
			}
		}
		return nil
	})
	if found == "" {
		t.Fatalf("no blob starts with %q", prefix)
	}
	return found
}

// untouched asserts a refused restore changed nothing: the store, the
// blob directory, the backups.
type untouched struct {
	e       *storeEnv
	tree    string
	backups int
}

func (e *storeEnv) hold() untouched {
	e.t.Helper()
	return untouched{e: e, tree: strings.Join(e.blobTree(), "\n"), backups: len(e.backups())}
}

func (u untouched) verify(t *testing.T, ct func() bool) {
	t.Helper()
	if !ct() {
		t.Error("the store changed")
	}
	if got := strings.Join(u.e.blobTree(), "\n"); got != u.tree {
		t.Errorf("the blob directory changed:\n%s\nwas:\n%s", got, u.tree)
	}
	if n := len(u.e.backups()); n != u.backups {
		t.Errorf("a refused restore wrote %d backups", n-u.backups)
	}
	if got := u.e.beside(); len(got) != 1 {
		t.Errorf("beside the blob directory: %v", got)
	}
}

// TestStorageRestoreRefusesBrokenBackups: a tampered blob, a manifest
// that disagrees with its archive, and a schema newer than the binary's
// are each refused with nothing modified: not the store, the blob
// directory, or the backup directory.
func TestStorageRestoreRefusesBrokenBackups(t *testing.T) {
	cases := []struct {
		name string
		exit int
		code string
		fix  func(m map[string]any)
		mut  func([]archiveEntry) []archiveEntry
	}{
		{name: "tampered blob", exit: 2, code: "VALIDATION", mut: func(es []archiveEntry) []archiveEntry {
			for i := range es {
				if strings.HasPrefix(es[i].hdr.Name, "blobs/") && len(es[i].body) > 0 {
					es[i].body[0] ^= 0xff
				}
			}
			return es
		}},
		{name: "tampered dump", exit: 2, code: "VALIDATION", mut: func(es []archiveEntry) []archiveEntry {
			es[1].body[len(es[1].body)/2] ^= 0xff
			return es
		}},
		{name: "blob count", exit: 2, code: "VALIDATION", fix: func(m map[string]any) {
			b := m["blobs"].(map[string]any)
			b["count"] = b["count"].(float64) + 1
		}},
		{name: "blob total", exit: 2, code: "VALIDATION", fix: func(m map[string]any) {
			b := m["blobs"].(map[string]any)
			b["bytes"] = b["bytes"].(float64) - 1
		}},
		{name: "blob missing from archive", exit: 2, code: "VALIDATION", mut: func(es []archiveEntry) []archiveEntry {
			return es[:len(es)-1]
		}},
		{name: "foreign driver", exit: 2, code: "VALIDATION", fix: func(m map[string]any) { m["driver"] = "sqlite" }},
		{name: "newer schema", exit: 1, code: "SCHEMA_MISMATCH", fix: func(m map[string]any) { m["schema"] = m["schema"].(float64) + 1 }},
		{name: "newer manifest format", exit: 2, code: "VALIDATION", fix: func(m map[string]any) { m["format"] = 99 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newStoreEnv(t)
			e.seedWithBlobs()
			seeded := e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
			b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
			rewriteArchive(t, b.Path, c.fix, c.mut)
			// The profile moves on, so a restore that went through would show.
			e.stage(attC, "attachment C")
			later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
			held := e.hold()

			r := e.heo("storage", "restore", b.Path, "--confirm=yes", "--format", "json")
			e.want(r, c.exit, "storage", "restore")
			if env := errEnvelope(t, r.stderr); env.Code != c.code {
				t.Errorf("refusal = %+v, want %s", env, c.code)
			}
			if strings.Contains(r.stderr+r.stdout, attA) || strings.Contains(r.stderr+r.stdout, attB) {
				t.Errorf("the refusal names a blob:\n%s", r.stderr)
			}
			held.verify(t, func() bool { return e.hasContact(seeded) && e.hasContact(later) })
			// The dry run refuses the same way.
			e.want(e.heo("storage", "restore", b.Path, "--dry-run", "--format", "json"), c.exit)
		})
	}
}

// TestStorageRestoreFailedStoreKeepsBlobs: the archive is whole but its
// dump is not a database dump pg_restore accepts. The store fails to
// restore; the blob directory is the old one again, with no staging
// left, and the pre-restore backup puts the store back.
func TestStorageRestoreFailedStoreKeepsBlobs(t *testing.T) {
	e := newStoreEnv(t)
	e.seedWithBlobs()
	e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")
	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	// A dump that starts like one but is not: consistent checksums.
	rewriteArchive(t, b.Path, func(m map[string]any) {
		sum := sha256.Sum256([]byte("PGDMP-broken"))
		st := m["store"].(map[string]any)
		st["size"], st["sha256"] = len("PGDMP-broken"), hex.EncodeToString(sum[:])
	}, func(es []archiveEntry) []archiveEntry {
		es[1].body = []byte("PGDMP-broken")
		return es
	})
	e.stage(attC, "attachment C")
	later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
	tree := strings.Join(e.blobTree(), "\n")

	r := e.heo("storage", "restore", b.Path, "--confirm=yes", "--format", "json")
	if r.exit == 0 {
		t.Fatal("restore of a broken dump succeeded")
	}
	env := errEnvelope(t, r.stderr)
	if !strings.Contains(env.SuggestedFix, "pre-restore") && !strings.Contains(env.SuggestedFix, "restore the data it replaced") {
		t.Errorf("no way back offered: %+v", env)
	}
	if got := strings.Join(e.blobTree(), "\n"); got != tree {
		t.Errorf("the blob directory changed by a failed restore:\n%s\nwas:\n%s", got, tree)
	}
	if got := e.beside(); len(got) != 1 {
		t.Errorf("beside the blob directory: %v", got)
	}
	// The pre-restore backup, the newest archive, holds everything.
	var pre string
	for _, p := range e.backups() {
		if strings.Contains(p, "pre-restore") {
			pre = p
		}
	}
	if pre == "" {
		t.Fatalf("no pre-restore backup in %v", e.backups())
	}
	e.run(0, "storage", "restore", pre, "--confirm=yes")
	if !e.hasContact(later) {
		t.Error("the store was not recovered from the pre-restore backup")
	}
	if got, ok := e.blob(attC); !ok || got != "attachment C" {
		t.Errorf("attachment C = %q, %v", got, ok)
	}
}

// TestStorageRestoreOldFormatBackup: a backup an earlier heo wrote is a
// bare pg_dump file with no manifest and no attachments. It restores the
// store, leaves the blob directory as it is, and says so.
func TestStorageRestoreOldFormatBackup(t *testing.T) {
	e := newStoreEnv(t)
	e.seedWithBlobs()
	seeded := e.putContact("00000000-0000-4000-8000-000000000001", "Seeded")

	o, _ := e.owner()
	old, err := o.Backup(t.Context(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Ext(old.Path) != ".dump" {
		t.Fatalf("driver backup = %s", old.Path)
	}
	if err := o.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	later := e.putContact("00000000-0000-4000-8000-000000000002", "Later")
	e.stage(attC, "attachment C")
	tree := strings.Join(e.blobTree(), "\n")

	r := decode[restoreOut](t, e.run(0, "storage", "restore", old.Path, "--confirm=yes", "--format", "json").stdout)
	if !e.hasContact(seeded) || e.hasContact(later) {
		t.Error("store not restored from the old-format backup")
	}
	if got := strings.Join(e.blobTree(), "\n"); got != tree {
		t.Errorf("the blob directory changed by a backup that carries none:\n%s\nwas:\n%s", got, tree)
	}
	if r.Blobs != nil || !r.BlobsUntouched {
		t.Errorf("result = %+v, want blobs untouched", r)
	}
	if r.Safety != nil && r.Safety.Blobs != nil {
		t.Errorf("the pre-restore backup copied attachments a restore would not replace: %+v", r.Safety.Blobs)
	}
	human := e.run(0, "storage", "restore", old.Path, "--confirm=yes").stdout
	if !strings.Contains(human, "untouched") {
		t.Errorf("human output does not say the blob directory was left alone:\n%s", human)
	}
}

// TestStorageStatusReportsBlobs: status counts the attachment files and
// their size, in both renderings, whatever state the directory is in.
func TestStorageStatusReportsBlobs(t *testing.T) {
	e := newStoreEnv(t)
	e.run(0, "storage", "migrate")
	type blobsOut struct {
		Blobs struct {
			Backend string `json:"backend"`
			Dir     string `json:"dir"`
			Exists  bool   `json:"exists"`
			Files   int    `json:"files"`
			Bytes   int64  `json:"bytes"`
		} `json:"blobs"`
	}
	s := decode[blobsOut](t, e.run(0, "storage", "status", "--format", "json").stdout)
	if s.Blobs.Backend != "local" || s.Blobs.Dir != e.blobs || s.Blobs.Exists || s.Blobs.Files != 0 {
		t.Errorf("before any attachment: %+v", s.Blobs)
	}
	if out := e.run(0, "storage", "status").stdout; !strings.Contains(out, "not created yet") {
		t.Errorf("human status:\n%s", out)
	}
	e.stage(attA, "attachment A bytes")
	e.stage(attB, strings.Repeat("b", 2048))
	s = decode[blobsOut](t, e.run(0, "storage", "status", "--format", "json").stdout)
	if !s.Blobs.Exists || s.Blobs.Files != 2 || s.Blobs.Bytes != int64(len("attachment A bytes")+2048) {
		t.Errorf("status = %+v", s.Blobs)
	}
	out := e.run(0, "storage", "status").stdout
	for _, want := range []string{"blobs", e.blobs, "2 files", "KiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("human status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, attA) {
		t.Errorf("status names a blob:\n%s", out)
	}
}

// TestStorageDryRunPlansBlobs: backup and restore previews name the
// attachments and change nothing.
func TestStorageDryRunPlansBlobs(t *testing.T) {
	e := newStoreEnv(t)
	e.seedWithBlobs()
	b := decode[storage.Backup](t, e.run(0, "storage", "backup", "--format", "json").stdout)
	e.stage(attC, "attachment C")
	tree := strings.Join(e.blobTree(), "\n")
	n := len(e.backups())

	p := decode[planOut](t, e.run(0, "storage", "backup", "--dry-run", "--format", "json").stdout)
	if len(p.Effects) != 1 || !strings.Contains(p.Effects[0].Detail, "3 attachment files") {
		t.Errorf("backup plan = %+v", p)
	}
	p = decode[planOut](t, e.run(0, "storage", "restore", b.Path, "--dry-run", "--format", "json").stdout)
	if len(p.Effects) != 3 || p.Effects[2].Kind != "replace" || !strings.HasPrefix(p.Effects[2].Target, "blobs:") ||
		!strings.Contains(p.Effects[2].Detail, "2 attachment files") {
		t.Errorf("restore plan = %+v", p)
	}
	if got := strings.Join(e.blobTree(), "\n"); got != tree || len(e.backups()) != n {
		t.Error("a dry run changed the profile")
	}
}

// TestStorageRestoreKeepsUnbackedBlobs: onto a store with no schema
// there is no pre-restore backup, so the blob directory a restore
// replaces is the only copy: it is kept beside the directory and
// reported, not deleted. Another profile's archive restores here whole.
func TestStorageRestoreKeepsUnbackedBlobs(t *testing.T) {
	src := newStoreEnv(t)
	src.seedWithBlobs()
	b := decode[storage.Backup](t, src.run(0, "storage", "backup", "--format", "json").stdout)
	want := strings.Join(src.blobTree(), "\n")

	dst := newStoreEnv(t)
	dst.stage(attC, "bytes only this profile has")
	r := decode[restoreOut](t, dst.run(0, "storage", "restore", b.Path, "--confirm=yes", "--format", "json").stdout)
	if r.Safety != nil {
		t.Errorf("a store with no schema was backed up: %+v", r.Safety)
	}
	if got := strings.Join(dst.blobTree(), "\n"); got != want {
		t.Errorf("restored tree:\n%s\nwant:\n%s", got, want)
	}
	if r.ReplacedBlobs == "" {
		t.Fatalf("the replaced blob directory was deleted with no backup of it: %+v", r)
	}
	kept, err := os.ReadFile(blobFileOf(t, r.ReplacedBlobs, "bytes only"))
	if err != nil || string(kept) != "bytes only this profile has" {
		t.Errorf("kept copy = %q, %v", kept, err)
	}
	if human := dst.run(0, "storage", "restore", b.Path, "--confirm=yes").stdout; !strings.Contains(human, "attachment files restored") {
		t.Errorf("human output:\n%s", human)
	}
}
