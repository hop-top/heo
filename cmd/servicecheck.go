package cmd

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/output"
)

// checkStart refuses, before kit starts anything, a `service start`
// that would serve an HTTP surface (REST, gRPC, MCP over HTTP) without
// an authenticator — loopback is no exception — or on a surface token
// that is not stored, a daemon whose
// authorization policy does not read, and an MCP stdio process with no
// daemon to relay to: over stdio, MCP forwards every daemon command
// over the control socket and never becomes the daemon itself.
func (a *app) checkStart(c *cobra.Command, args []string) error {
	if list, _ := c.Flags().GetBool("list"); list {
		// `serve --list` (deprecated spelling of `service list`)
		// starts nothing.
		return nil
	}
	v := a.r.Viper
	starts := startSelection(c, args, func(svc string) bool { return v.GetBool("services." + svc + ".enabled") })
	for _, svc := range settings.HTTPSurfaces {
		if !starts(svc) || !listensOnTCP(c, v, svc) || settings.AuthModeConfigured(v, svc) {
			continue
		}
		if settings.SurfaceTokenRef(v) != "" {
			if err := a.checkSurfaceToken(c, svc); err != nil {
				return err
			}
			continue
		}
		e := output.UsageError(fmt.Sprintf(
			"services.%s: no authenticator configured; heo serves every HTTP surface with authentication, on loopback too", svc))
		e.SuggestedFix = fmt.Sprintf("run `heo service setup %s` (a per-profile token), or set services.%s.auth.mode "+
			"(jwt, jwks, oidc, apikey or mtls) or services.all.auth.mode", svc, svc)
		return e
	}
	if err := checkPolicy(v); err != nil {
		return err
	}
	if stdioRelay(c, args, v) {
		return a.controlClient().Check(c.Context())
	}
	return nil
}

// listensOnTCP reports whether the HTTP surface svc, started by c,
// listens on TCP: --stdio selects stdio for this run, as kit's mcp
// service reads it, over services.mcp.transport.
func listensOnTCP(c *cobra.Command, v *viper.Viper, svc string) bool {
	if svc == "mcp" {
		if f := c.Flags().Lookup("stdio"); f != nil && f.Changed && f.Value.String() == "true" {
			return false
		}
	}
	return settings.ListensOnTCP(v, svc)
}

// stdioRelay reports whether c with args is an MCP stdio process: the
// relay an agent spawns, which forwards to the profile's daemon and is
// never the daemon. --mcp-addr asks for HTTP: kit refuses that
// combination with stdio itself.
func stdioRelay(c *cobra.Command, args []string, v *viper.Viper) bool {
	if len(args) != 1 || args[0] != "mcp" || listensOnTCP(c, v, "mcp") {
		return false
	}
	f := c.Flags().Lookup("mcp-addr")
	return f == nil || !f.Changed || f.Value.String() == ""
}

// checkPolicy refuses a daemon whose authorization policy does not
// read: it would refuse every caller. Warnings start.
func checkPolicy(v *viper.Viper) error {
	var msgs []string
	for _, p := range authz.Check(v, authz.OwnerActor(), daemon.ServiceActor) {
		if !p.Warning {
			msgs = append(msgs, p.Key+": "+p.Message)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	e := output.UsageError("authorization policy: " + strings.Join(msgs, "; "))
	e.SuggestedFix = "fix the authorization section; `heo config validate` lists every problem"
	return e
}

// startSelection reports which services `service start` with args
// would run: the one named, or, with none named, every enabled one,
// after --enable and --disable (enable wins), as kit resolves them.
func startSelection(c *cobra.Command, args []string, enabled func(string) bool) func(string) bool {
	if len(args) == 1 {
		return func(svc string) bool { return svc == args[0] }
	}
	enable, _ := c.Flags().GetStringSlice("enable")
	disable, _ := c.Flags().GetStringSlice("disable")
	return func(svc string) bool {
		switch {
		case slices.Contains(enable, svc):
			return true
		case slices.Contains(disable, svc):
			return false
		}
		return enabled(svc)
	}
}
