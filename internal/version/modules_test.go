package version

import (
	"runtime/debug"
	"testing"
)

func TestModuleIn(t *testing.T) {
	deps := []*debug.Module{
		{Path: "hop.top/kit", Version: "v0.5.0-alpha.24"},
		{Path: "hop.top/uma", Version: "v0.0.0-20261011033403-a029f52034df", Replace: &debug.Module{Path: "../uma"}},
		{Path: "hop.top/cite", Version: "v0.1.0", Replace: &debug.Module{Path: "hop.top/cite", Version: "v0.1.1"}},
	}
	for path, want := range map[string]string{
		"hop.top/kit":   "hop.top/kit@v0.5.0-alpha.24",
		"hop.top/uma":   "hop.top/uma@v0.0.0-20261011033403-a029f52034df (local replace)",
		"hop.top/cite":  "hop.top/cite@v0.1.1",
		"hop.top/other": "hop.top/other@(devel)",
	} {
		if got := moduleIn(deps, path); got != want {
			t.Errorf("moduleIn(%s) = %q, want %q", path, got, want)
		}
	}
}

func TestNumberStripsV(t *testing.T) {
	old := ver
	t.Cleanup(func() { ver = old })
	ver = "v1.2.3"
	if got := Number(); got != "1.2.3" {
		t.Errorf("Number() = %q", got)
	}
}
