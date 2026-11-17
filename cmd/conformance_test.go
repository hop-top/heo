package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	kitconformance "hop.top/kit/go/conformance"
	"hop.top/kit/go/conformance/harness"
	"hop.top/kit/go/console/cli"
)

// TestAssertCLI runs kit's Layer-A conformance contract over the whole
// command tree: every leaf, every group, the kit-owned verbs heo mounts.
// It includes kit's guidance tier (EnforceGuidance): every runnable
// leaf declares kit/examples, and every leaf that is not a read
// declares kit/next-steps. guidance.go fills both; a new leaf fails
// here until it has them.
func TestAssertCLI(t *testing.T) {
	_ = kitconformance.AssertCLIWithOptions(t, newApp().r, // failures are reported on t
		kitconformance.Options{EnforceGuidance: true})
}

// TestRootSignature runs kit's signature walk on its own. AssertCLI
// runs it too, but a change to AssertCLI must not drop this check
// unnoticed: local globals, reserved names and depth are heo's
// grammar guard (SignatureStrictness reject).
func TestRootSignature(t *testing.T) {
	r := newApp().r
	if r.Config.SignatureStrictness != cli.SignatureStrictnessReject {
		t.Fatalf("SignatureStrictness = %q, want reject", r.Config.SignatureStrictness)
	}
	if rep := r.ValidateSignature(); rep.HasViolations() {
		for _, v := range rep.Violations {
			t.Errorf("%s [%s/%s] %s", v.Path, v.Check, v.Severity, v.Detail)
		}
	}
}

// destructiveGateArgs holds, per destructive leaf, a command line that
// satisfies cobra's own argument and required-flag checks, so a run
// reaches the confirmation gate. The gate runs before the handler:
// nothing here needs a daemon or a store.
var destructiveGateArgs = map[string][]string{
	"heo hook dead-letter drop": {"hook", "dead-letter", "drop", "triage", "ev_1"},
	"heo message delete":        {"message", "delete", "msg_1", "--revision", "r1"},
	"heo contact merge": {"contact", "merge", "ct_1", "ct_2", "--reason", "same_person",
		"--revision", "r1", "--into-revision", "r2"},
	"heo participant remove": {"participant", "remove", "ident_1", "--conversation", "conv_1", "--revision", "r1"},
	"heo storage restore":    {"storage", "restore", "backup-that-does-not-exist"},
}

// destructiveLeaves returns every leaf of the tree whose side effect
// is destructive, kit's three spellings of it.
func destructiveLeaves(a *app) []string {
	var out []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if c.HasSubCommands() {
			return
		}
		switch se, _ := cli.GetSideEffect(c); se {
		case cli.SideEffectDestructive, cli.SideEffectDestructiveLocal, cli.SideEffectDestructiveShared:
			out = append(out, c.CommandPath())
		}
	}
	walk(a.r.Cmd)
	slices.Sort(out)
	return out
}

// TestDestructiveLeavesAreGatedAtExecute drives each destructive leaf
// through Root.Execute, the only path that installs kit's confirmation
// gate: a Root that cli.New built and nothing executed has none, so a
// test on the constructed tree would pass for a leaf that deletes
// unasked. Without a terminal and without --confirm=yes the leaf is
// refused (exit 5, UNAUTHORIZED) before its handler runs; with
// --confirm=yes the gate lets it through to the handler.
func TestDestructiveLeavesAreGatedAtExecute(t *testing.T) {
	upgradeSandbox(t)

	want := make([]string, 0, len(destructiveGateArgs))
	for path := range destructiveGateArgs {
		want = append(want, path)
	}
	slices.Sort(want)
	got := destructiveLeaves(newApp())
	// The e2e build adds a destructive leaf of its own (extra_e2e.go);
	// the release tree is what ships.
	got = slices.DeleteFunc(got, func(p string) bool { return p == "heo destroy" })
	if !slices.Equal(got, want) {
		t.Fatalf("destructive leaves = %v, gate table covers %v: add the new leaf to destructiveGateArgs", got, want)
	}

	for _, path := range want {
		args := destructiveGateArgs[path]
		t.Run(path, func(t *testing.T) {
			for _, mode := range [][]string{nil, {"--confirm=no"}} {
				r := runHeo(t, append(slices.Clone(args), mode...)...)
				if r.exit != 5 || !strings.Contains(r.stderr, "refused: --confirm=no (or non-TTY default)") {
					t.Errorf("%v: exit %d, want 5 with the confirmation refusal\nstderr: %s", mode, r.exit, r.stderr)
				}
			}
			r := runHeo(t, append(slices.Clone(args), "--confirm=yes")...)
			if r.exit == 5 || strings.Contains(r.stderr, "refused: --confirm=no") {
				t.Errorf("--confirm=yes was refused by the gate: exit %d\nstderr: %s", r.exit, r.stderr)
			}
			if r.exit == 0 {
				t.Errorf("--confirm=yes ran to success on a command line with no store behind it")
			}
		})
	}
}

// TestCapabilityRoundtrip: every non-interactive leaf of the tree
// answers --help with exit 0 (12fc F1: a caller can discover each
// capability from the CLI itself).
func TestCapabilityRoundtrip(t *testing.T) {
	upgradeSandbox(t)
	harness.AssertCapabilityRoundtrip(t, newApp().r.Cmd)
}
