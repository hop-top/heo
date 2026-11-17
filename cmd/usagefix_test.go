package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/kit/go/console/output"
)

// usageEnvelope is the part of kit's error envelope these tests read.
type usageEnvelope struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	SuggestedFix string `json:"suggested_fix"`
	ExitCode     int    `json:"exit_code"`
}

// TestUsageErrorCarriesSuggestedFix: a usage error heo raises itself
// carries the same corrective pointer kit gives the usage errors it
// raises: the exact --help invocation for the command that was run.
// One is rejected in a handler (an invalid --scope), one before any
// handler runs, while the configuration loads (a bad -c token).
func TestUsageErrorCarriesSuggestedFix(t *testing.T) {
	upgradeSandbox(t)
	for name, tc := range map[string]struct {
		args []string
		fix  string
	}{
		"handler": {
			[]string{"config", "set", "runtime.tenant", "acme", "--scope", "nowhere"},
			"run 'heo config set --help' for usage",
		},
		"config load": {
			[]string{"-c", "no-such-file.yaml", "config", "list"},
			"run 'heo config list --help' for usage",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := runHeo(t, append(tc.args, "--format", "json")...)
			if r.exit != 2 {
				t.Fatalf("exit = %d, want 2\nstderr: %s", r.exit, r.stderr)
			}
			var e usageEnvelope
			if err := json.Unmarshal([]byte(r.stderr), &e); err != nil {
				t.Fatalf("stderr is not an envelope: %v\n%s", err, r.stderr)
			}
			if e.Code != "USAGE" {
				t.Fatalf("code = %q, want USAGE", e.Code)
			}
			if e.SuggestedFix != tc.fix {
				t.Errorf("suggested_fix = %q, want %q", e.SuggestedFix, tc.fix)
			}
		})
	}
}

// TestUsageFixLeavesOtherErrorsAlone pins the wrapper's contract: it
// fills only a usage error that names no fix. A fix a call site wrote
// (a specific one beats the generic pointer), every other error code
// and a nil error pass through untouched.
func TestUsageFixLeavesOtherErrorsAlone(t *testing.T) {
	root := &cobra.Command{Use: "heo"}
	leaf := &cobra.Command{Use: "thing"}
	root.AddCommand(leaf)

	specific := output.UsageError("bad")
	specific.SuggestedFix = "use --x"
	for name, tc := range map[string]struct {
		in   error
		want string
	}{
		"bare usage":       {output.UsageError("bad"), "run 'heo thing --help' for usage"},
		"wrapped usage":    {fmt.Errorf("context: %w", output.UsageError("bad")), "run 'heo thing --help' for usage"},
		"specific kept":    {specific, "use --x"},
		"not found kept":   {output.NotFoundError("gone"), ""},
		"plain error kept": {errors.New("boom"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := withUsageFix(leaf, tc.in)
			var oe *output.Error
			if !errors.As(got, &oe) {
				if tc.want != "" {
					t.Fatalf("lost the envelope: %v", got)
				}
				return
			}
			if oe.SuggestedFix != tc.want {
				t.Errorf("suggested_fix = %q, want %q", oe.SuggestedFix, tc.want)
			}
		})
	}
	if withUsageFix(leaf, nil) != nil {
		t.Error("nil error became non-nil")
	}
}
