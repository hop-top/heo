//go:build !unix

package control

import (
	"fmt"
	"os"
)

// checkOwner refuses every socket: without Unix ownership and peer
// credentials there is no control channel to trust.
func checkOwner(path string, _ os.FileInfo, _ bool) error {
	return &ownerError{
		msg: fmt.Sprintf("cannot verify the owner of %s: the control socket needs a unix platform", path),
		fix: "run heo on linux or darwin",
	}
}
