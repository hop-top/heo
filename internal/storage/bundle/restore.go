package bundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"hop.top/heo/internal/storage"
	"hop.top/uma/model"
)

// RestoreOptions tunes RestoreArchive.
type RestoreOptions struct {
	// Covered says the blob directory about to be replaced is already in
	// a backup the caller took, so the replaced tree is deleted once the
	// restore succeeds. Otherwise it is kept beside the directory and
	// reported.
	Covered bool
}

// RestoreResult is what RestoreArchive did to the blob directory.
type RestoreResult struct {
	// Blobs counts the files now in the blob directory; nil when the
	// archive carried none and the directory was left alone.
	Blobs *storage.BlobStats
	// Replaced is where the previous blob tree was left, empty when it
	// was deleted or there was none.
	Replaced string
}

// Restore replaces the store, and the blob directory when the backup at
// path carries one, with the backup's. It examines the backup first,
// refuses a driver or schema it does not fit, and stages everything
// before changing anything.
func (w *Owner) Restore(ctx context.Context, path string) error {
	a, err := Examine(ctx, path)
	if err != nil {
		return err
	}
	v, err := w.Versions(ctx)
	if err != nil {
		return err
	}
	if err := w.Accept(a, v); err != nil {
		return err
	}
	_, err = w.RestoreArchive(ctx, a, RestoreOptions{})
	return err
}

// Accept checks that a can be restored onto the store described by v
// and into this owner's blob directory, before anything is changed.
func (w *Owner) Accept(a *Archive, v storage.Versions) error {
	if err := a.Compatible(v); err != nil {
		return err
	}
	if a.HasBlobs() && w.tree == nil {
		return errNoTree()
	}
	return nil
}

func errNoTree() error {
	return model.NewError(model.Unsupported, "the backup carries a blob directory, but this profile's blob "+
		"backend keeps its own bytes and has none to replace")
}

// RestoreArchive restores a, which Examine returned, replacing the
// store and, if a carries blobs, the blob directory.
//
// Everything is staged first: the store's dump in a file, the blob tree
// in a directory beside the live one, each checked against the manifest
// again as it is written. The blob directory is then swapped for the
// staged one with renames, and only then is the store replaced; a store
// that fails to restore puts the old blob directory back. The live blob
// directory is therefore the old tree or the new one, never a mix.
func (w *Owner) RestoreArchive(ctx context.Context, a *Archive, opts RestoreOptions) (RestoreResult, error) {
	if a.Legacy {
		return RestoreResult{}, w.Owner.Restore(ctx, a.Path)
	}
	withBlobs := a.HasBlobs()
	if withBlobs && w.tree == nil {
		return RestoreResult{}, errNoTree()
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return RestoreResult{}, model.NewError(model.Unavailable, "backup directory: "+errText(err))
	}
	dump, err := os.CreateTemp(w.dir, ".restore-*.dump")
	if err != nil {
		return RestoreResult{}, model.NewError(model.Unavailable, "staging the store dump: "+errText(err))
	}
	defer func() { _ = os.Remove(dump.Name()) }()
	x := &extractor{dump: dump}
	var target string
	if withBlobs {
		if target, err = w.stageDir(); err != nil {
			_ = dump.Close()
			return RestoreResult{}, err
		}
		if x.root, err = os.MkdirTemp(filepath.Dir(target), filepath.Base(target)+".restore-*"); err != nil {
			_ = dump.Close()
			return RestoreResult{}, model.NewError(model.Unavailable, "staging the blob directory beside "+target+": "+errText(err))
		}
		defer func() { _ = os.RemoveAll(x.root) }()
	}
	err = w.extract(ctx, a, x)
	if cerr := dump.Close(); err == nil && cerr != nil {
		err = model.NewError(model.Unavailable, "staging the store dump: "+errText(cerr))
	}
	if err != nil {
		return RestoreResult{}, err
	}

	var res RestoreResult
	var swapped swap
	if withBlobs {
		if swapped, err = exchange(target, x.root); err != nil {
			return RestoreResult{}, err
		}
	}
	if err := w.Owner.Restore(ctx, dump.Name()); err != nil {
		if withBlobs {
			if uerr := swapped.undo(x.root); uerr != nil {
				return RestoreResult{}, fmt.Errorf("%w (and the previous blob directory could not be put back: %s)", err, uerr)
			}
		}
		return RestoreResult{}, err
	}
	if withBlobs {
		res.Blobs = &storage.BlobStats{Files: a.Manifest.Blobs.Count, Bytes: a.Manifest.Blobs.Bytes}
		res.Replaced = swapped.old
		if swapped.old != "" && opts.Covered && os.RemoveAll(swapped.old) == nil {
			res.Replaced = ""
		}
	}
	return res, nil
}

// stageDir returns the blob directory to replace: the real path of a
// symlinked directory, and its parent made if the tree was never there.
func (w *Owner) stageDir() (string, error) {
	root, exists, err := resolve(w.tree.Dir())
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	if !exists {
		if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
			return "", model.NewError(model.Unavailable, "blob directory: "+errText(err))
		}
	}
	removeStale(root)
	return root, nil
}

// removeStale deletes staging directories an interrupted restore left
// beside target. They are named by this package alone and hold only
// copies from a backup.
func removeStale(target string) {
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(filepath.Base(target)) + `\.restore-[0-9]+$`)
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && re.MatchString(e.Name()) {
			_ = os.RemoveAll(filepath.Join(filepath.Dir(target), e.Name()))
		}
	}
}

// extract writes a's store dump and blob tree into x, checking them
// against the manifest once more.
func (w *Owner) extract(ctx context.Context, a *Archive, x *extractor) error {
	f, err := os.Open(a.Path) //nolint:gosec // G304: the operator names the backup to restore
	if err != nil {
		return model.NewError(model.NotFound, "backup "+a.Path+": "+errText(err))
	}
	defer func() { _ = f.Close() }()
	m, err := readEntries(ctx, f, a.Path, x)
	if err != nil {
		return err
	}
	if !sameManifest(m, a.Manifest) {
		return invalid(a.Path, "the backup changed while it was restored")
	}
	if x.root == "" {
		return nil
	}
	// Modes last: a directory may be read-only for the owner, and the
	// files below it had to be written first.
	sort.Slice(x.dirs, func(i, j int) bool { return len(x.dirs[i].Path) > len(x.dirs[j].Path) })
	for _, d := range x.dirs {
		if err := os.Chmod(filepath.Join(x.root, filepath.FromSlash(d.Path)), dirMode(d.Mode)); err != nil {
			return model.NewError(model.Unavailable, "staging the blob directory: "+errText(err))
		}
	}
	return nil
}

func sameManifest(a, b Manifest) bool {
	return a.Format == b.Format && a.Driver == b.Driver && a.Schema == b.Schema &&
		a.Store == b.Store && (a.Blobs == nil) == (b.Blobs == nil) &&
		(a.Blobs == nil || (a.Blobs.Count == b.Blobs.Count && a.Blobs.Bytes == b.Blobs.Bytes))
}

// fileMode is a file's recorded permissions with the world's removed:
// attachments are private to the daemon's user and group.
func fileMode(m uint32) fs.FileMode { return fs.FileMode(m).Perm() &^ 0o007 }

// dirMode is a directory's recorded permissions with the world's
// removed and the owner's guaranteed, so the tree stays usable.
func dirMode(m uint32) fs.FileMode { return fs.FileMode(m).Perm()&^0o007 | 0o700 }

// extractor is the sink that stages a backup: the dump into a file, the
// blob tree under root.
type extractor struct {
	dump *os.File
	root string
	dirs []Dir
}

func (x *extractor) store(r io.Reader, _ File) error {
	if _, err := io.Copy(x.dump, r); err != nil {
		return stageError(err, "staging the store dump")
	}
	return nil
}

func (x *extractor) dir(d Dir) error {
	if x.root == "" {
		return nil
	}
	x.dirs = append(x.dirs, d)
	if err := os.MkdirAll(filepath.Join(x.root, filepath.FromSlash(d.Path)), 0o700); err != nil {
		return model.NewError(model.Unavailable, "staging the blob directory: "+errText(err))
	}
	return nil
}

func (x *extractor) file(r io.Reader, f File) error {
	if x.root == "" {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	p := filepath.Join(x.root, filepath.FromSlash(f.Path))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return model.NewError(model.Unavailable, "staging the blob directory: "+errText(err))
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: below the staging directory; the path was validated against the manifest
	if err != nil {
		return model.NewError(model.Unavailable, "staging the blob directory: "+errText(err))
	}
	_, cerr := io.Copy(out, r)
	if err := errors.Join(cerr, out.Sync(), out.Close()); err != nil {
		return stageError(err, "staging the blob directory")
	}
	if err := os.Chmod(p, fileMode(f.Mode)); err != nil {
		return model.NewError(model.Unavailable, "staging the blob directory: "+errText(err))
	}
	if !f.ModTime.IsZero() {
		if err := os.Chtimes(p, time.Time{}, f.ModTime); err != nil {
			return model.NewError(model.Unavailable, "staging the blob directory: "+errText(err))
		}
	}
	return nil
}

// stageError passes on a damaged archive and a cancelled context, and
// calls any other failure by what was being staged.
func stageError(err error, what string) error {
	if errors.Is(err, errDamaged) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return model.NewError(model.Unavailable, what+": "+errText(err))
}

// swap is a blob directory exchanged for its staged replacement.
type swap struct {
	target string
	// old is where the previous tree was moved, empty if there was none.
	old string
}

// exchange makes the tree at staged the blob directory at target. The
// previous tree, if any, is renamed aside first and renamed back if the
// second rename fails.
func exchange(target, staged string) (swap, error) {
	s := swap{target: target}
	if _, err := os.Lstat(target); err == nil {
		s.old = fmt.Sprintf("%s.replaced-%d", target, time.Now().UnixNano())
		if err := os.Rename(target, s.old); err != nil {
			return swap{}, model.NewError(model.Unavailable, "replacing the blob directory: "+errText(err)+
				"; nothing was changed")
		}
	}
	if err := os.Rename(staged, target); err != nil {
		if s.old != "" {
			if rerr := os.Rename(s.old, target); rerr != nil {
				return swap{}, model.NewError(model.Unavailable, "replacing the blob directory failed ("+errText(err)+
					") and the previous one could not be put back; it is at "+s.old)
			}
		}
		return swap{}, model.NewError(model.Unavailable, "replacing the blob directory: "+errText(err)+"; nothing was changed")
	}
	syncDir(filepath.Dir(target))
	return s, nil
}

// undo reverses the exchange: the new tree goes back to staged, the
// previous one back to the blob directory.
func (s swap) undo(staged string) error {
	if err := os.Rename(s.target, staged); err != nil {
		return fmt.Errorf("moving the restored blob directory away: %s", errText(err))
	}
	if s.old != "" {
		if err := os.Rename(s.old, s.target); err != nil {
			return fmt.Errorf("the previous blob directory is at %s: %s", s.old, errText(err))
		}
	}
	return nil
}

// syncDir flushes a directory's entries; best effort, not every
// platform can.
func syncDir(dir string) {
	d, err := os.Open(dir) //nolint:gosec // G304: the blob directory's parent
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
