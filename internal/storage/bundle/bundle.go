// Package bundle makes a heo backup one file: a tar archive holding the
// storage driver's dump of the data store, the attachment files of the
// blob directory, and a manifest that lists them all with their sizes
// and SHA-256 checksums.
//
//	manifest.json   authoritative: driver, schema, every entry, count, size
//	store.dump      the driver's own backup, opaque to bundle
//	blobs/<path>    the blob directory's tree, directories and files
//
// A restore reads the manifest first and checks every entry against it
// before anything is changed: an unlisted, missing, resized or altered
// entry refuses the whole archive. The blob directory is then staged
// next to the live one and swapped in with renames, so it is either the
// old tree or the new one, never half of each.
//
// A backup written before archives existed is a bare driver dump with no
// manifest; it restores as it always did and leaves the blob directory
// alone. So does an archive written without blobs, as the backup before
// a migration is: a migration does not touch attachment bytes.
//
// The blob directory is an optional capability (Tree). A profile whose
// blob backend is not a local directory has no Tree, and its archives
// hold the store alone.
package bundle

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
)

const (
	// FormatVersion is the manifest format this package writes and the
	// newest it reads.
	FormatVersion = 1
	// Ext is the file extension of an archive.
	Ext = ".tar"

	manifestName = "manifest.json"
	storeName    = "store.dump"
	blobsPrefix  = "blobs/"
	// maxManifest bounds the manifest read before it is trusted.
	maxManifest = 256 << 20
)

// Manifest is the archive's authoritative description of itself.
type Manifest struct {
	// Format is the manifest format version.
	Format int `json:"format"`
	// Driver wrote Store.
	Driver string `json:"driver"`
	// Schema is the store's schema version when the archive was taken.
	Schema    int64     `json:"schema"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	// Store describes the driver's dump.
	Store File `json:"store"`
	// Blobs describes the blob directory; nil when the archive carries
	// none, in which case a restore leaves the live directory alone.
	Blobs *Blobs `json:"blobs,omitempty"`
}

// File is one regular file of the archive.
type File struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	SHA256  string    `json:"sha256"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"mod_time,omitzero"`
	// Sidecar marks a file the backend keeps beside its blobs, such as
	// a content-type record: carried and restored with them, but not
	// a blob, so not counted.
	Sidecar bool `json:"sidecar,omitempty"`
}

// Dir is one directory of the blob tree, below its root.
type Dir struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

// Blobs is the blob tree: its directories, files and totals.
type Blobs struct {
	// Count and Bytes total the blobs among Files, the ones that are not
	// sidecars; a restore checks them against it.
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
	Dirs  []Dir  `json:"dirs"`
	Files []File `json:"files"`
}

// Owner is a storage.Owner whose backups are archives and whose restore
// understands them. Everything else is the wrapped owner's.
type Owner struct {
	storage.Owner
	dir  string
	tree Tree
}

// Wrap returns o with backup and restore done through archives written
// to and staged in backupDir. tree may be nil: the archives then hold
// the store alone.
func Wrap(o storage.Owner, backupDir string, tree Tree) *Owner {
	return &Owner{Owner: o, dir: backupDir, tree: tree}
}

// Tree is the blob tree this owner carries; nil if none.
func (w *Owner) Tree() Tree { return w.tree }

// Backup writes an archive of the store, and of the blob directory
// unless reason is the backup before a migration, which cannot change
// it. The archive is a new owner-only file in the backup directory.
func (w *Owner) Backup(ctx context.Context, reason string) (storage.Backup, error) {
	return w.Snapshot(ctx, reason, reason != storage.ReasonPreMigrate)
}

// Snapshot is Backup with the choice of carrying the blob directory
// made by the caller. A blob directory the owner does not have is never
// carried.
func (w *Owner) Snapshot(ctx context.Context, reason string, blobs bool) (storage.Backup, error) {
	b, err := w.Owner.Backup(ctx, reason)
	if err != nil {
		return b, err
	}
	dump := b.Path
	defer func() { _ = os.Remove(dump) }()
	treeDir := ""
	if w.tree != nil && blobs {
		treeDir = w.tree.Dir()
	}
	final := strings.TrimSuffix(dump, filepath.Ext(dump)) + Ext
	stats, err := writeArchive(ctx, final, dump, b, treeDir)
	if err != nil {
		return storage.Backup{}, err
	}
	b.Path, b.Blobs = final, stats
	return b, nil
}

// writeArchive writes the archive at final from the driver's dump and,
// when treeDir is set, the tree there.
func writeArchive(ctx context.Context, final, dump string, b storage.Backup, treeDir string) (stats *storage.BlobStats, err error) {
	sum, size, err := hashFile(ctx, dump)
	if err != nil {
		return nil, err
	}
	m := Manifest{
		Format: FormatVersion, Driver: b.Driver, Schema: b.Schema, Reason: b.Reason, CreatedAt: b.CreatedAt,
		Store: File{Path: storeName, Size: size, SHA256: sum, Mode: 0o600},
	}
	var root string
	if treeDir != "" {
		if m.Blobs, err = scan(ctx, treeDir); err != nil {
			return nil, err
		}
		if r, exists, err := resolve(treeDir); err != nil {
			return nil, err
		} else if exists {
			root = r
		}
		stats = &storage.BlobStats{Files: m.Blobs.Count, Bytes: m.Blobs.Bytes}
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(final, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: a new file named from the backup directory
	if err != nil {
		return nil, model.NewError(model.Unavailable, "backup file: "+errText(err))
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(final)
		}
	}()
	tw := tar.NewWriter(f)
	if err := putBytes(tw, manifestName, body, 0o600); err != nil {
		return nil, err
	}
	if err := putFile(ctx, tw, dump, m.Store, storeName); err != nil {
		return nil, err
	}
	if m.Blobs != nil {
		for _, d := range m.Blobs.Dirs {
			h := &tar.Header{Typeflag: tar.TypeDir, Name: blobsPrefix + d.Path + "/", Mode: int64(d.Mode)}
			if err := tw.WriteHeader(h); err != nil {
				return nil, writeError(err)
			}
		}
		for _, bf := range m.Blobs.Files {
			if err := putFile(ctx, tw, filepath.Join(root, filepath.FromSlash(bf.Path)), bf, blobsPrefix+bf.Path); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, writeError(err)
	}
	if err := f.Sync(); err != nil {
		return nil, writeError(err)
	}
	if err := f.Close(); err != nil {
		return nil, writeError(err)
	}
	keep = true
	return stats, nil
}

func putBytes(tw *tar.Writer, name string, body []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(body)), Mode: mode}); err != nil {
		return writeError(err)
	}
	if _, err := tw.Write(body); err != nil {
		return writeError(err)
	}
	return nil
}

// putFile copies the file at path into the archive as name, checking it
// still is what the manifest says it is.
func putFile(ctx context.Context, tw *tar.Writer, path string, want File, name string) error {
	src, err := os.Open(path) //nolint:gosec // G304: the driver's dump, or a file the walk listed
	if err != nil {
		return model.NewError(model.Unavailable, "reading backup data: "+errText(err))
	}
	defer func() { _ = src.Close() }()
	h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: want.Size, Mode: int64(want.Mode), ModTime: want.ModTime}
	if err := tw.WriteHeader(h); err != nil {
		return writeError(err)
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tw, sum), ctxReader{ctx, src}); err != nil {
		var se *fs.PathError
		if ctx.Err() != nil || errors.As(err, &se) {
			return readError(ctx, err)
		}
		return model.NewError(model.Conflict, "the blob directory changed while it was read")
	}
	if hex.EncodeToString(sum.Sum(nil)) != want.SHA256 {
		return model.NewError(model.Conflict, "the blob directory changed while it was read")
	}
	return nil
}

func writeError(err error) error {
	return model.NewError(model.Unavailable, "backup file: "+errText(err))
}
