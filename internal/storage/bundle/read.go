package bundle

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
)

// dumpMagic starts a pg_dump custom-format file: a backup written
// before archives existed.
var dumpMagic = []byte("PGDMP")

// Archive is a backup file that has been read through and found whole.
type Archive struct {
	// Path is the backup file.
	Path string
	// Legacy marks a bare driver dump without a manifest: it holds the
	// store alone and restores as the driver restores it.
	Legacy bool
	// Manifest is the archive's, zero for a legacy dump.
	Manifest Manifest
}

// HasBlobs reports whether restoring the archive replaces the blob
// directory.
func (a *Archive) HasBlobs() bool { return a.Manifest.Blobs != nil }

// Examine reads the backup at path through: the manifest first, then
// every entry against it. It changes nothing, and any entry missing,
// unlisted, resized or altered refuses the whole archive. A file that
// is not a heo archive is taken for a legacy driver dump when it
// starts like one, and the driver judges it.
func Examine(ctx context.Context, path string) (*Archive, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator names the backup to restore
	if err != nil {
		return nil, model.NewError(model.NotFound, "backup "+path+": "+errText(err))
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, len(dumpMagic))
	if n, _ := io.ReadFull(f, head); n == len(head) && bytes.Equal(head, dumpMagic) {
		return &Archive{Path: path, Legacy: true}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, model.NewError(model.Unavailable, "backup "+path+": "+errText(err))
	}
	m, err := readEntries(ctx, f, path, nil)
	if err != nil {
		return nil, err
	}
	return &Archive{Path: path, Manifest: m}, nil
}

// Compatible checks the archive against the store it would replace: the
// same driver, and a schema this binary can run. A schema newer than the
// binary's would leave a store no worker can start on.
func (a *Archive) Compatible(v storage.Versions) error {
	if a.Legacy {
		return nil
	}
	if a.Manifest.Driver != v.Driver {
		return model.NewError(model.Validation, "backup was written by the "+strconv.Quote(a.Manifest.Driver)+
			" storage driver; this profile uses "+strconv.Quote(v.Driver))
	}
	if a.Manifest.Schema > v.LibrarySchema {
		return model.NewError(model.SchemaMismatch, "backup holds schema version "+strconv.FormatInt(a.Manifest.Schema, 10)+
			", newer than "+strconv.FormatInt(v.LibrarySchema, 10)+" ("+v.Library+"); upgrade heo to restore it")
	}
	return nil
}

// sink receives what readEntries finds, after checking each against the
// manifest; nil discards it. A file's reader yields its bytes, which
// the sink must read to the end.
type sink interface {
	store(r io.Reader, f File) error
	dir(d Dir) error
	file(r io.Reader, f File) error
}

// invalid is the error for a backup that does not hold together. It
// never names a blob.
func invalid(path, why string) error {
	return model.NewError(model.Validation, "backup "+path+" is not whole: "+why)
}

// readEntries reads the archive in f, checks it against its manifest
// and feeds s. The archive is trusted no further than the manifest, and
// the manifest no further than its own consistency.
func readEntries(ctx context.Context, f io.Reader, path string, s sink) (Manifest, error) {
	tr := tar.NewReader(ctxReader{ctx, f})
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, notArchive(ctx, path, err)
	}
	if hdr.Name != manifestName || hdr.Typeflag != tar.TypeReg || hdr.Size > maxManifest {
		return Manifest{}, model.NewError(model.Validation, "backup "+path+" is not a heo backup: it does not start with its manifest")
	}
	body, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
	if err != nil {
		return Manifest{}, notArchive(ctx, path, err)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, invalid(path, "the manifest does not parse")
	}
	exp, err := expectations(&m)
	if err != nil {
		return Manifest{}, invalid(path, err.Error())
	}
	seen := make(map[string]bool, len(exp))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, invalid(path, "the archive is truncated or damaged")
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		e, ok := exp[name]
		if !ok || seen[name] {
			return Manifest{}, invalid(path, "an entry is not in the manifest, or appears twice")
		}
		seen[name] = true
		switch e.kind {
		case kindDir:
			if hdr.Typeflag != tar.TypeDir {
				return Manifest{}, invalid(path, "an entry is not the kind the manifest lists")
			}
			if s != nil {
				if err := s.dir(e.dir); err != nil {
					return Manifest{}, err
				}
			}
		default:
			if hdr.Typeflag != tar.TypeReg || hdr.Size != e.file.Size {
				return Manifest{}, invalid(path, "an entry differs in kind or size from the manifest")
			}
			if err := checkEntry(ctx, tr, e, s); err != nil {
				return Manifest{}, entryError(ctx, err, path)
			}
		}
	}
	if len(seen) != len(exp) {
		return Manifest{}, invalid(path, fmt.Sprintf("%d of the manifest's entries are missing", len(exp)-len(seen)))
	}
	return m, nil
}

// checkEntry streams one file through its checksum into the sink.
func checkEntry(ctx context.Context, tr *tar.Reader, e expectation, s sink) error {
	h := sha256.New()
	r := io.TeeReader(io.LimitReader(damageReader{tr}, e.file.Size), h)
	switch {
	case s == nil:
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
	case e.kind == kindStore:
		if err := s.store(r, e.file); err != nil {
			return err
		}
	default:
		if err := s.file(r, e.file); err != nil {
			return err
		}
	}
	// A sink that stopped early leaves bytes unhashed.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.file.SHA256 {
		return errChecksum
	}
	return nil
}

var (
	// errChecksum marks an entry whose bytes are not the manifest's.
	errChecksum = errors.New("checksum mismatch")
	// errDamaged marks an archive that ends or breaks inside an entry.
	errDamaged = errors.New("archive damaged")
)

// damageReader tags the errors of the archive's own bytes, so they are
// told from a sink's.
type damageReader struct{ r io.Reader }

func (d damageReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("%w: %w", errDamaged, err)
	}
	return n, err
}

// entryError renders a failure while reading one entry: a cancelled
// context as itself, a mismatch or damage as a refusal of path, a
// sink's failure as it is.
func entryError(ctx context.Context, err error, path string) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errChecksum):
		return invalid(path, "an entry's checksum does not match the manifest")
	case errors.Is(err, errDamaged):
		return invalid(path, "the archive is truncated or damaged")
	}
	return err
}

func notArchive(ctx context.Context, path string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return model.NewError(model.Validation, "backup "+path+" is neither a heo backup nor a "+
		"database dump")
}

type kind int

const (
	kindStore kind = iota
	kindDir
	kindBlob
)

type expectation struct {
	kind kind
	file File
	dir  Dir
}

// expectations validates the manifest and indexes the entries an
// archive must hold, by name.
func expectations(m *Manifest) (map[string]expectation, error) {
	switch {
	case m.Format < 1:
		return nil, errors.New("the manifest has no format version")
	case m.Format > FormatVersion:
		return nil, fmt.Errorf("the manifest is format %d, written by a newer heo; this one reads up to %d", m.Format, FormatVersion)
	case m.Driver == "":
		return nil, errors.New("the manifest names no storage driver")
	}
	exp := map[string]expectation{}
	if m.Store.Path != storeName {
		return nil, errors.New("the manifest's store entry is not store.dump")
	}
	if err := checkFile(m.Store); err != nil {
		return nil, err
	}
	exp[storeName] = expectation{kind: kindStore, file: m.Store}
	if m.Blobs == nil {
		return exp, nil
	}
	b := m.Blobs
	var total int64
	var blobs int
	for _, d := range b.Dirs {
		if !validRel(d.Path) {
			return nil, errors.New("a directory has an unusable path")
		}
		name := blobsPrefix + d.Path
		if _, dup := exp[name]; dup {
			return nil, errors.New("an entry is listed twice")
		}
		exp[name] = expectation{kind: kindDir, dir: d}
	}
	for _, f := range b.Files {
		if !validRel(f.Path) {
			return nil, errors.New("a blob has an unusable path")
		}
		if err := checkFile(f); err != nil {
			return nil, err
		}
		name := blobsPrefix + f.Path
		if _, dup := exp[name]; dup {
			return nil, errors.New("an entry is listed twice")
		}
		exp[name] = expectation{kind: kindBlob, file: f}
		if !f.Sidecar {
			blobs++
			total += f.Size
		}
	}
	if b.Count != blobs {
		return nil, fmt.Errorf("the manifest counts %d blobs and lists %d", b.Count, blobs)
	}
	if total != b.Bytes {
		return nil, fmt.Errorf("the manifest totals %d bytes and its blobs add up to %d", b.Bytes, total)
	}
	return exp, nil
}

func checkFile(f File) error {
	if f.Size < 0 {
		return errors.New("an entry has a negative size")
	}
	if len(f.SHA256) != hex.EncodedLen(sha256.Size) {
		return errors.New("an entry has no usable checksum")
	}
	if _, err := hex.DecodeString(f.SHA256); err != nil {
		return errors.New("an entry has no usable checksum")
	}
	return nil
}

// validRel accepts a clean, relative, slash-separated path that stays
// inside the tree.
func validRel(p string) bool {
	return p != "." && fs.ValidPath(p) && !strings.ContainsAny(p, "\\\x00")
}
