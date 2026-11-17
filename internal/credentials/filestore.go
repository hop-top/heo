package credentials

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/file"
)

// fileStore is kit's file backend under heo's file layout. Kit keeps
// one file per key at the key's path, so a purpose key, one segment
// below its reference's shared key, would need the shared file to be a
// directory. heo puts it beside the shared file instead, so a
// reference holds both:
//
//	uma/<tenant>/<ref>            <dir>/uma/<tenant>/<ref>
//	uma/<tenant>/<ref>/<purpose>  <dir>/uma/<tenant>/<ref>@<purpose>
//
// '@' separates the two: within <ref> and <purpose> (both already
// path-escaped segments, so they hold no '%' of their own) every '@'
// is written %40. A file name holds at most one literal '@', and no
// two keys share a file. Any other key, such as a surface token, is
// kit's own path. Callers see only the keys above: the mapping is
// this store's.
type fileStore struct {
	dir string
	kit *file.Store
}

var _ kitsecret.MutableStore = fileStore{}

// Writable declares that Set overwrites in place and Get reads it
// back, as kit's file backend does: an OAuth grant's cached token and
// rotated refresh token are written here.
func (fileStore) Writable() bool { return true }

func newFileStore(dir string) fileStore {
	return fileStore{dir: filepath.Clean(dir), kit: file.New(dir, nil)}
}

// fileKey returns the kit key of a store key under the file layout and,
// for a credential key, the reference's shared key: the directory an
// earlier build kept the reference's purpose files in.
func fileKey(key string) (fk, shared string) {
	seg := strings.Split(key, "/")
	if seg[0] != "uma" || len(seg) < 3 || len(seg) > 4 || slices.Contains(seg, "") {
		return key, ""
	}
	shared = strings.Join(seg[:3], "/")
	name := escapeAt(seg[2])
	if len(seg) == 4 {
		name += "@" + escapeAt(seg[3])
	}
	return seg[0] + "/" + seg[1] + "/" + name, shared
}

// logicalKey inverts fileKey for a kit key listed by the store.
func logicalKey(fk string) string {
	seg := strings.Split(fk, "/")
	if seg[0] != "uma" || len(seg) != 3 || slices.Contains(seg, "") {
		return fk
	}
	ref, purpose, isPurpose := strings.Cut(seg[2], "@")
	k := seg[0] + "/" + seg[1] + "/" + unescapeAt(ref)
	if isPurpose {
		k += "/" + unescapeAt(purpose)
	}
	return k
}

func escapeAt(s string) string   { return strings.ReplaceAll(s, "@", "%40") }
func unescapeAt(s string) string { return strings.ReplaceAll(s, "%40", "@") }

// path is the file holding key.
func (s fileStore) path(key string) string {
	fk, _ := fileKey(key)
	return filepath.Join(s.dir, filepath.FromSlash(fk))
}

// nestedAbsent reports a kit key below a file, or naming a directory:
// it holds no secret.
func nestedAbsent(err error) bool {
	return errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.EISDIR)
}

func (s fileStore) Get(ctx context.Context, key string) (*kitsecret.Secret, error) {
	fk, shared := fileKey(key)
	sec, err := s.kit.Get(ctx, fk)
	switch {
	case err == nil:
		sec.Key = key
		return sec, nil
	case !errors.Is(err, kitsecret.ErrNotFound) && !nestedAbsent(err):
		return nil, err
	}
	if shared != "" {
		if dir := filepath.Join(s.dir, filepath.FromSlash(shared)); isDir(dir) {
			return nil, &earlierLayout{dir: dir, base: s.path(shared)}
		}
	}
	return nil, kitsecret.ErrNotFound
}

func isDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// Exists reads the key: kit's file backend reports a directory as
// existing.
func (s fileStore) Exists(ctx context.Context, key string) (bool, error) {
	sec, err := s.Get(ctx, key)
	switch {
	case errors.Is(err, kitsecret.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	clear(sec.Value)
	return true, nil
}

func (s fileStore) Set(ctx context.Context, key string, value []byte) error {
	fk, _ := fileKey(key)
	return s.kit.Set(ctx, fk, value)
}

func (s fileStore) Delete(ctx context.Context, key string) error {
	fk, _ := fileKey(key)
	return s.kit.Delete(ctx, fk)
}

// List returns the keys under prefix, matched as kit's file backend
// matches them, on the keys callers use rather than the file names.
func (s fileStore) List(ctx context.Context, prefix string) ([]string, error) {
	all, err := s.kit.List(ctx, "")
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, fk := range all {
		if k := logicalKey(fk); matchesPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// matchesPrefix is kit's file backend prefix rule: a prefix ending in
// '/' or holding none matches as a string prefix; any other matches
// itself and the keys below it.
func matchesPrefix(key, prefix string) bool {
	if prefix == "" || strings.HasSuffix(prefix, "/") || !strings.Contains(prefix, "/") {
		return strings.HasPrefix(key, prefix)
	}
	return key == prefix || strings.HasPrefix(key, prefix+"/")
}

// earlierLayout reports a reference whose purpose files an earlier
// build kept in a directory named after it (<ref>/<purpose>). They are
// not read: the directory sits where the shared file goes, and one
// entry in two places could serve a stale secret after the other is
// rotated or deleted.
type earlierLayout struct {
	dir  string // the directory of purpose files
	base string // the reference's shared file now
}

func (e *earlierLayout) Error() string {
	return "credential files in an earlier layout: " + e.dir + " is a directory of purpose files"
}

func (e *earlierLayout) fix() string {
	return fmt.Sprintf("move each %s to %s (an '@' in <purpose> written %%40), then remove the directory %s",
		filepath.Join(e.dir, "<purpose>"), e.base+"@<purpose>", e.dir)
}
