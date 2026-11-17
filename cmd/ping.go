package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/version"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
)

// pingResult is what `daemon ping` reports: who the daemon serves the
// caller as, and the runtime's state, as the daemon sees them.
type pingResult struct {
	// Profile is the profile the daemon serves.
	Profile string `json:"profile" yaml:"profile"`
	// Tenant is the profile's tenant, from the daemon's configuration.
	Tenant string `json:"tenant" yaml:"tenant"`
	// Actor is the caller as the transport verified it: uid:<n> over
	// the control socket.
	Actor string `json:"actor" yaml:"actor"`
	// Surface is the transport the call reached the daemon on.
	Surface string `json:"surface" yaml:"surface"`
	// PID and Version identify the daemon process.
	PID     int    `json:"pid" yaml:"pid"`
	Version string `json:"version" yaml:"version"`
	// Runtime is the runtime services' and data store's state.
	Runtime daemon.Status `json:"runtime" yaml:"runtime"`
}

const servicePingLong = `Ask the selected profile's daemon to answer over its control socket:
the actor and tenant it serves you as, its pid and version, and the
state of its runtime (data store, workers, receivers, webhook).

The actor is what the daemon verified, never what a request claims:
uid:<n> of the socket's peer. The tenant is the profile's
runtime.tenant. ping is served over the control socket only, never
over REST, gRPC or MCP; service status reports the same runtime state
over kit's control socket.

Read-only. Nothing is started: with no daemon running it fails with
PREREQUISITE (exit 70) and the command that starts one.`

// daemonCmd builds the non-runnable `daemon` group: heo's questions to
// the selected profile's running daemon. It is heo's own, mounted after
// cli.New, so the control socket serves its leaves; kit withholds
// every leaf of its `service` group from the socket, heo's included.
func (a *app) daemonCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "daemon",
		Short: "Ask the profile's running daemon about itself",
		Long: `Ask the selected profile's running daemon, over its control socket,
how it serves you. Starting, stopping and inspecting the daemon's
services is the service group's.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.daemonPingCmd())
	return grp
}

func (a *app) daemonPingCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ping",
		Short: "Ask the profile's daemon who you are to it and how its runtime stands",
		Long:  servicePingLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, a.ping)
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return writePing(cmd.OutOrStdout(), res)
		},
	}
	daemonCommand(c)
	cli.SetWithholdFrom(c, cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &pingResult{}, Version: "1.0"})
	_ = cli.SetExamples(c, []cli.Example{
		{Title: "Check the daemon answers", Command: "heo daemon ping"},
		{Title: "JSON for agents", Command: "heo daemon ping --format json"},
	})
	return c
}

// ping runs in the daemon.
func (a *app) ping(_ context.Context, c control.Caller) (pingResult, error) {
	return pingResult{
		Profile: a.state.Profile().String(),
		Tenant:  string(c.Tenant),
		Actor:   string(c.Actor),
		Surface: string(c.Surface),
		PID:     os.Getpid(),
		Version: version.Version(),
		Runtime: a.runtime.Status(),
	}, nil
}

func writePing(w io.Writer, p pingResult) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := [][2]string{
		{"profile", p.Profile},
		{"tenant", p.Tenant},
		{"actor", p.Actor},
		{"surface", p.Surface},
		{"daemon", fmt.Sprintf("pid %d, heo %s", p.PID, p.Version)},
		{"ready", fmt.Sprint(p.Runtime.Ready)},
		{"storage", string(p.Runtime.Storage.State)},
	}
	for _, s := range p.Runtime.Services {
		rows = append(rows, [2]string{s.Name, string(s.State)})
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}
