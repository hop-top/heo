//go:build unix

package control

import (
	"fmt"
	"os"
	"syscall"
)

// checkOwner refuses fi unless the caller's effective uid owns it, and,
// for the socket's directory, unless no other user has any permission
// on it.
func checkOwner(path string, fi os.FileInfo, dir bool) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return &ownerError{msg: fmt.Sprintf("cannot read the owner of %s", path),
			fix: "set services.socket.path to a socket in a directory you own"}
	}
	me := os.Geteuid()
	if int(st.Uid) != me {
		what := "control socket"
		if dir {
			what = "control socket directory"
		}
		return &ownerError{
			msg: fmt.Sprintf("%s %s is owned by uid %d, not by you (uid %d): refusing to send commands through it",
				what, path, st.Uid, me),
			fix: "set services.socket.path to a socket in a directory you own, or remove the foreign one",
		}
	}
	if dir && fi.Mode().Perm()&0o077 != 0 {
		return &ownerError{
			msg: fmt.Sprintf("control socket directory %s is open to other users (mode %04o)",
				path, fi.Mode().Perm()),
			fix: "chmod 700 " + path,
		}
	}
	return nil
}
