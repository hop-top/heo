package xrrx

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// deps lists the import paths heo's main package links, with tags.
func deps(t *testing.T, tags string) []string {
	t.Helper()
	args := []string{"list", "-buildvcs=false", "-deps", "-f", "{{.ImportPath}}"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	out, err := exec.Command("go", append(args, "hop.top/heo")...).Output() //nolint:gosec // fixed go list arguments
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list %v: %v\n%s", args, err, ee.Stderr)
		}
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Fields(string(out))
}

// recorders are what a release binary must not link: xrr itself, uma's
// xrrtest, this package, and the standard library's testing that
// xrrtest drags in.
func recorders(pkgs []string) []string {
	var out []string
	for _, p := range pkgs {
		if p == "hop.top/xrr" || strings.HasPrefix(p, "hop.top/xrr/") ||
			p == "hop.top/uma/testing/xrrtest" || p == "hop.top/heo/internal/xrrx" || p == "testing" {
			out = append(out, p)
		}
	}
	return out
}

// TestReleaseBuildGraphHasNoXRR: the release binary's import graph holds
// no recorder, and the e2e build's holds all of them, so the check
// cannot pass for want of looking.
func TestReleaseBuildGraphHasNoXRR(t *testing.T) {
	if got := recorders(deps(t, "")); len(got) != 0 {
		t.Errorf("the release build links %v; xrr belongs to the e2e build (tag heoe2e) only", got)
	}
	e2e := recorders(deps(t, "heoe2e"))
	for _, want := range []string{"hop.top/xrr", "hop.top/xrr/adapters/exec", "hop.top/xrr/adapters/http", "hop.top/uma/testing/xrrtest", "hop.top/heo/internal/xrrx"} {
		if !slices.Contains(e2e, want) {
			t.Errorf("the e2e build does not link %s (links %v): the seam is not wired", want, e2e)
		}
	}
}
