//go:build !windows

package hooks

import (
	"fmt"
	"os"
	"syscall"
)

// checkSocketOwner refuses a socket another user owns.
func checkSocketOwner(path string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read its owner", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the daemon's uid %d", path, st.Uid, os.Geteuid())
	}
	return nil
}
