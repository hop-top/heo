package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"hop.top/kit/go/console/output"
)

// withUsageFix gives a usage error that names no fix the pointer kit
// gives the usage errors it raises itself (an unknown flag, a missing
// argument): the --help invocation for the command that was run. Kit
// renders an error a handler returns as it arrives, so a USAGE heo
// raises from its own validation would otherwise leave the caller with
// a message and nowhere to go.
//
// Only a USAGE envelope with an empty fix is filled: a call site that
// knows the specific remedy keeps it, and every other error passes
// through.
func withUsageFix(cmd *cobra.Command, err error) error {
	var oe *output.Error
	if errors.As(err, &oe) && oe.Code == output.CodeUsage && oe.SuggestedFix == "" {
		oe.SuggestedFix = "run '" + cmd.CommandPath() + " --help' for usage"
	}
	return err
}

// fixUsageErrors wraps the handler of every runnable command under c
// with withUsageFix. It runs before kit installs its own chain, so the
// wrapped handler is the innermost one: its error is what kit renders.
// The load hook is wrapped the same way (see newApp). Argument counts
// and unknown flags are kit's, and already carry the pointer.
func fixUsageErrors(c *cobra.Command) {
	for _, sub := range c.Commands() {
		fixUsageErrors(sub)
	}
	if inner := c.RunE; inner != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			return withUsageFix(cmd, inner(cmd, args))
		}
	}
}
