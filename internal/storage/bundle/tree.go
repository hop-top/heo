package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"hop.top/kit/go/storage/blob"
	kitlocal "hop.top/kit/go/storage/blob/local"
	"hop.top/uma/model"
)

// Tree is the optional capability of a blob backend whose bytes live in
// one directory on this host: heo then carries that directory in its
// backups. A backend without it, an object store for example, is not a
// Tree: it keeps its bytes out of heo's archives and owns their backup.
type Tree interface {
	// Dir is the directory the backend's files live under.
	Dir() string
}

type localDir string

func (d localDir) Dir() string { return string(d) }

// LocalDir is the Tree of the local directory blob backend rooted at
// dir.
func LocalDir(dir string) Tree { return localDir(dir) }

// Stats counts the files under a tree without reading them.
type Stats struct {
	// Dir is the tree's directory.
	Dir string `json:"dir" yaml:"dir"`
	// Exists is false before the first attachment is staged.
	Exists bool `json:"exists" yaml:"exists"`
	// Files and Bytes count the blobs below Dir and their size. The
	// backend's own bookkeeping files, such as content-type sidecars,
	// are not blobs and are not counted.
	Files int   `json:"files" yaml:"files"`
	Bytes int64 `json:"bytes" yaml:"bytes"`
}

// Inspect counts t's blobs and their size. It reads no blob and
// changes nothing.
func Inspect(ctx context.Context, t Tree) (Stats, error) {
	st := Stats{Dir: t.Dir()}
	root, exists, err := resolve(t.Dir())
	if err != nil || !exists {
		return st, err
	}
	st.Exists = true
	keys, err := blobKeys(ctx, root)
	if err != nil {
		return st, err
	}
	err = walk(ctx, root, func(string, fs.FileInfo) error { return nil }, func(rel string, info fs.FileInfo) error {
		if keys[rel] {
			st.Files++
			st.Bytes += info.Size()
		}
		return nil
	})
	return st, err
}

// blobKeys is the set of blob keys the local backend holds under root,
// as the backend lists them. Every other file under root is the
// backend's own bookkeeping: carried by a backup, never counted. Keys
// are the files' slash-separated paths below root.
func blobKeys(ctx context.Context, root string) (map[string]bool, error) {
	store, err := kitlocal.New(root)
	if err != nil {
		return nil, model.NewError(model.Unavailable, "blob directory: "+errText(err))
	}
	var objs []blob.Object
	if objs, err = store.List(ctx, ""); err != nil {
		return nil, readError(ctx, err)
	}
	keys := make(map[string]bool, len(objs))
	for _, o := range objs {
		keys[o.Key] = true
	}
	return keys, nil
}

// resolve follows a symlinked directory to the directory itself, so a
// tree linked onto another volume is archived and swapped in place,
// never replaced by a plain directory. exists is false when dir is not
// there yet.
func resolve(dir string) (string, bool, error) {
	real, err := filepath.EvalSymlinks(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, false, nil
	case err != nil:
		return "", false, model.NewError(model.Unavailable, "blob directory: "+errText(err))
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", false, model.NewError(model.Unavailable, "blob directory: "+errText(err))
	}
	if !fi.IsDir() {
		return "", false, model.NewError(model.Validation, "storage.blobs.dir "+dir+" is not a directory")
	}
	return real, true, nil
}

// walk visits root's directories, then files, in name order, parents
// before children. Anything else than files and directories is refused:
// the backend writes nothing else, and an archive would not carry it.
// Errors never name an entry: blob names are not logged.
func walk(ctx context.Context, root string, dir func(rel string, info fs.FileInfo) error, file func(rel string, info fs.FileInfo) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return model.NewError(model.Unavailable, "blob directory: "+errText(err))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return model.NewError(model.Unavailable, "blob directory: "+errText(err))
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return model.NewError(model.Unavailable, "blob directory: "+errText(err))
		}
		rel = filepath.ToSlash(rel)
		switch {
		case info.IsDir():
			return dir(rel, info)
		case info.Mode().IsRegular():
			return file(rel, info)
		}
		return model.NewError(model.Unsupported, "the blob directory holds an entry that is neither a file nor a directory; "+
			"heo backs up files and directories only")
	})
}

// scan lists and checksums the tree at dir into a manifest section.
// A tree that does not exist yet is an empty one.
func scan(ctx context.Context, dir string) (*Blobs, error) {
	b := &Blobs{Dirs: []Dir{}, Files: []File{}}
	root, exists, err := resolve(dir)
	if err != nil || !exists {
		return b, err
	}
	keys, err := blobKeys(ctx, root)
	if err != nil {
		return nil, err
	}
	err = walk(ctx, root, func(rel string, info fs.FileInfo) error {
		b.Dirs = append(b.Dirs, Dir{Path: rel, Mode: perm(info.Mode())})
		return nil
	}, func(rel string, info fs.FileInfo) error {
		f := File{Path: rel, Size: info.Size(), Mode: perm(info.Mode()), ModTime: info.ModTime().UTC(), Sidecar: !keys[rel]}
		sum, n, err := hashFile(ctx, filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		if n != f.Size {
			return model.NewError(model.Conflict, "the blob directory changed while it was read")
		}
		f.SHA256 = sum
		b.Files = append(b.Files, f)
		if !f.Sidecar {
			b.Count++
			b.Bytes += f.Size
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(b.Dirs, func(i, j int) bool { return b.Dirs[i].Path < b.Dirs[j].Path })
	sort.Slice(b.Files, func(i, j int) bool { return b.Files[i].Path < b.Files[j].Path })
	return b, nil
}

func perm(m fs.FileMode) uint32 { return uint32(m.Perm()) }

// hashFile returns the SHA-256 (hex) and size of the file at path.
func hashFile(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file under the blob directory the walk listed
	if err != nil {
		return "", 0, model.NewError(model.Unavailable, "blob directory: "+errText(err))
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, ctxReader{ctx, f})
	if err != nil {
		return "", 0, readError(ctx, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ctxReader stops a long copy when its context ends.
type ctxReader struct {
	ctx context.Context //nolint:containedctx // a reader wrapper lives for one copy
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func readError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return model.NewError(model.Unavailable, "reading backup data: "+errText(err))
}

func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		// The operation and cause, not the path: blob names stay out of
		// messages.
		return fmt.Sprintf("%s: %v", pe.Op, pe.Err)
	}
	return err.Error()
}
