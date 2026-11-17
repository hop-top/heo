package e2e

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/blobs"
	"hop.top/uma/model"
)

// blobSnapshot is every entry under dir with its mode and bytes.
func blobSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, _ := d.Info()
		rel, _ := filepath.Rel(dir, p)
		line := rel + " " + fi.Mode().String()
		if fi.Mode().IsRegular() {
			line += " " + readFile(t, p)
		}
		out = append(out, line)
		return nil
	})
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// archiveEntry returns one entry's bytes from a backup archive.
func archiveEntry(t *testing.T, path, name string) []byte {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: a backup the test took
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("%s has no %s", path, name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == name {
			b, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
}

// TestStorageBackupRestoreAttachmentBytes drives the built binary on
// Postgres: attach files through a channel, back up, let the profile
// move on and lose its blob directory, restore, and download the very
// same bytes. A tampered blob and a manifest that does not match its
// archive are refused with nothing modified; a store that fails to
// restore leaves the old blob directory in place; a backup without
// blobs, as an older heo wrote, restores and leaves the directory alone.
func TestStorageBackupRestoreAttachmentBytes(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		blobDir := c.s.path("data", "heo", "default", "blobs")
		store, err := blobs.Open(blobDir)
		if err != nil {
			t.Fatal(err)
		}
		c.db.blobs = store
		pgDump, pgRestore := pgDumpInContainer(t, c.s), pgRestoreInContainer(t, c.s)
		backups := c.s.path("backups")
		tools := []string{"-c", "storage.postgres.pg_dump=" + pgDump, "-c", "storage.postgres.pg_restore=" + pgRestore,
			"-c", "storage.backup.dir=" + backups}
		storageCmd := func(exit int, args ...string) result {
			t.Helper()
			full := append(append([]string{}, args...), tools...)
			r := c.s.run(nil, full...)
			c.s.want(r, exit, full...)
			return r
		}

		d := c.start()
		ch := c.fakeChannel("fake", adapterstest.Config{Account: "acct-bk"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "files", "--destination", "files-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		invoice := marker + " invoice"
		binary := []byte{0x00, 0xff, 0xfe, 0x0a, 0x0d, 0x1b, '[', '3', '1', 'm', 0x80, 0x00}
		m1 := c.received(ch, "m1",
			adapterstest.InjectAttachment{Key: "k-invoice", Name: "invoice.txt", MediaType: "text/plain", Data: []byte(invoice)},
			adapterstest.InjectAttachment{Key: "k-blob", Name: "photo.bin", MediaType: "application/octet-stream", Data: binary})
		invoiceRef, photoRef := attachmentRef(m1, 0), attachmentRef(m1, 1)
		c.stop(d)

		// Status counts the attachments without naming them.
		var st struct {
			Blobs struct {
				Dir   string
				Files int
				Bytes int64
			}
		}
		r := storageCmd(0, "storage", "status", "--format", "json")
		if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
			t.Fatal(err)
		}
		if st.Blobs.Dir != blobDir || st.Blobs.Files != 2 || st.Blobs.Bytes != int64(len(invoice)+len(binary)) {
			t.Fatalf("status blobs = %+v", st.Blobs)
		}

		var bk struct {
			Path  string
			Blobs struct{ Files int }
		}
		r = storageCmd(0, "storage", "backup", "--format", "json")
		if err := json.Unmarshal([]byte(r.stdout), &bk); err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(bk.Path) != backups || bk.Blobs.Files != 2 {
			t.Fatalf("backup = %+v", bk)
		}
		if fi, err := os.Stat(bk.Path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("archive = %v, %v", fi, err)
		}
		if strings.Contains(r.stdout+r.stderr, marker) {
			t.Error("backup output carries attachment bytes")
		}
		wantTree := blobSnapshot(t, blobDir)

		// The profile moves on: another attachment arrives, then the
		// blob directory is lost.
		d = c.start()
		m2 := c.received(ch, "m2", adapterstest.InjectAttachment{Key: "k-late", Name: "late.txt", MediaType: "text/plain", Data: []byte("after the backup")})
		lateRef := attachmentRef(m2, 0)
		c.stop(d)
		if err := os.RemoveAll(blobDir); err != nil {
			t.Fatal(err)
		}

		// A broken backup is refused with nothing modified.
		before := len(entries(t, backups))
		raw, err := os.ReadFile(bk.Path) //nolint:gosec // G304: the backup this test took
		if err != nil {
			t.Fatal(err)
		}
		tampered := bytes.Replace(raw, []byte(invoice), []byte(strings.ToUpper(invoice)), 1)
		mismatched := regexp.MustCompile(`"count": 2`).ReplaceAll(raw, []byte(`"count": 3`))
		if bytes.Equal(tampered, raw) || bytes.Equal(mismatched, raw) {
			t.Fatal("could not break the archive: its layout changed")
		}
		for name, broken := range map[string][]byte{"tampered": tampered, "mismatched": mismatched} {
			p := c.s.path("broken-" + name + ".tar")
			if err := os.WriteFile(p, broken, 0o600); err != nil {
				t.Fatal(err)
			}
			r := storageCmd(2, "storage", "restore", p, "--confirm=yes", "--format", "json")
			if !strings.Contains(r.stderr, "VALIDATION") || strings.Contains(r.stderr, marker) {
				t.Errorf("%s: stderr = %s", name, r.stderr)
			}
			if _, err := os.Stat(blobDir); err == nil {
				t.Errorf("%s: a refused restore made the blob directory", name)
			}
			if got := len(entries(t, backups)); got != before {
				t.Errorf("%s: a refused restore wrote backups: %d, was %d", name, got, before)
			}
		}

		// A store that fails to restore leaves the blob directory as it
		// was (here: gone). The failing pg_restore stands in for any
		// cause.
		failing := c.s.write("bin/pg_restore_fails", "#!/bin/sh\necho 'pg_restore: simulated failure' >&2\nexit 1\n")
		if err := os.Chmod(failing, 0o700); err != nil { //nolint:gosec // G302: the test's own executable
			t.Fatal(err)
		}
		r = c.s.run(nil, "storage", "restore", bk.Path, "--confirm=yes", "-c", "storage.postgres.pg_dump="+pgDump,
			"-c", "storage.postgres.pg_restore="+failing, "-c", "storage.backup.dir="+backups)
		if r.exit == 0 {
			t.Fatal("restore succeeded with a failing pg_restore")
		}
		if _, err := os.Stat(blobDir); err == nil {
			t.Error("a failed restore left a blob directory where there was none")
		}
		for _, n := range entries(t, filepath.Dir(blobDir)) {
			if strings.HasPrefix(n, "blobs.") {
				t.Errorf("a failed restore left %s beside the blob directory", n)
			}
		}
		// The failed restore dropped the store's tables; restoring the
		// good backup again recovers all of it.
		r = storageCmd(0, "storage", "restore", bk.Path, "--confirm=yes", "--format", "json")
		var rs struct {
			Blobs          struct{ Files int }
			BlobsUntouched bool `json:"blobs_untouched"`
		}
		if err := json.Unmarshal([]byte(r.stdout), &rs); err != nil {
			t.Fatal(err)
		}
		if rs.Blobs.Files != 2 || rs.BlobsUntouched {
			t.Errorf("restore = %s", r.stdout)
		}
		if got := blobSnapshot(t, blobDir); got != wantTree {
			t.Errorf("restored tree:\n%s\nwant:\n%s", got, wantTree)
		}
		if fi, err := os.Stat(blobDir); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("blob directory = %v, %v; want 0700", fi, err)
		}

		// The same bytes come back through the daemon.
		d = c.start()
		if r := c.heo(0, "attachment", "download", invoiceRef, "-o", "-"); r.stdout != invoice {
			t.Fatalf("invoice after restore = %q", r.stdout)
		}
		if r := c.heo(0, "attachment", "download", photoRef, "-o", "-"); r.stdout != string(binary) {
			t.Fatalf("photo after restore = %q", r.stdout)
		}
		wantStderr(t, c.heo(3, "attachment", "get", lateRef), "NOT_FOUND")
		c.stop(d)

		// A backup an older heo wrote is the bare driver dump: it restores
		// the store and leaves the blob directory, and says so.
		old := c.s.path("backups", "older.dump")
		if err := os.WriteFile(old, archiveEntry(t, bk.Path, "store.dump"), 0o600); err != nil {
			t.Fatal(err)
		}
		stale := filepath.Join(blobDir, "keep-me")
		if err := os.WriteFile(stale, []byte("kept"), 0o600); err != nil {
			t.Fatal(err)
		}
		tree := blobSnapshot(t, blobDir)
		r = storageCmd(0, "storage", "restore", old, "--confirm=yes", "--format", "json")
		rs.BlobsUntouched, rs.Blobs.Files = false, 0
		if err := json.Unmarshal([]byte(r.stdout), &rs); err != nil {
			t.Fatal(err)
		}
		if !rs.BlobsUntouched || rs.Blobs.Files != 0 {
			t.Errorf("old-format restore = %s", r.stdout)
		}
		if got := blobSnapshot(t, blobDir); got != tree {
			t.Errorf("an old-format restore changed the blob directory:\n%s\nwant:\n%s", got, tree)
		}
		if r := storageCmd(0, "storage", "restore", old, "--confirm=yes"); !strings.Contains(r.stdout, "untouched") {
			t.Errorf("human output does not say the blob directory was left alone:\n%s", r.stdout)
		}
	})
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
