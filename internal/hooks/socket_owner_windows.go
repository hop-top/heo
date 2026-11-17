//go:build windows

package hooks

import (
	"fmt"
	"os"
)

// checkSocketOwner refuses every socket on windows: its owner cannot be
// read the unix way, and an unverified owner could stand in for the plugin.
func checkSocketOwner(path string, _ os.FileInfo) error {
	return fmt.Errorf("%s: socket owner checks are not supported on windows", path)
}
