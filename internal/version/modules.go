package version

import (
	"runtime/debug"
	"strings"
)

// Number is heo's own version, as release builds stamp it ("1.2.3",
// "0.1.0-alpha.1"); "dev" for any other build.
func Number() string { return strings.TrimPrefix(ver, "v") }

// Module names the module at path and the version this binary was built
// with, for example "hop.top/kit@v0.5.0-alpha.24". A module replaced by
// a local directory reports its required version marked "(local
// replace)"; one missing from the build information (test binaries
// record none) reports "(devel)".
func Module(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return path + "@(devel)"
	}
	return moduleIn(bi.Deps, path)
}

func moduleIn(deps []*debug.Module, path string) string {
	for _, m := range deps {
		if m.Path != path {
			continue
		}
		if m.Replace != nil {
			if v := m.Replace.Version; v != "" && v != "(devel)" {
				return path + "@" + v
			}
			return path + "@" + m.Version + " (local replace)"
		}
		return path + "@" + m.Version
	}
	return path + "@(devel)"
}
