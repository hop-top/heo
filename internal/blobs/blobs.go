// Package blobs is the attachment byte store of one heo profile: uma's
// blob port over a directory on the host, which only the daemon's user
// can enter.
//
// The directory is created, and kept, owner-only (0700). The backend
// writes its files group-readable (0640) and its subdirectories 0750,
// so the root's mode is what keeps private attachments from other
// users: Open sets it on every start, not only when it creates the
// directory.
package blobs

import (
	"errors"
	"fmt"
	"os"

	kitlocal "hop.top/kit/go/storage/blob/local"
	umablob "hop.top/uma/contrib/kit/blob"
	"hop.top/uma/ports"
)

// Open returns the blob store rooted at dir, creating the directory
// owner-only when it does not exist and restoring that mode when it
// does.
func Open(dir string) (ports.BlobStore, error) {
	if dir == "" {
		return nil, errors.New("blobs: no directory configured (storage.blobs.dir)")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("blobs: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory, which needs the search bit
		return nil, fmt.Errorf("blobs: restrict %s: %w", dir, err)
	}
	local, err := kitlocal.New(dir)
	if err != nil {
		return nil, fmt.Errorf("blobs: open %s: %w", dir, err)
	}
	return umablob.New(local)
}
