package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/cli/service"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/core/xdg"
)

const serviceStartLong = `Run the profile's daemon in the foreground, for a service manager
(launchd, systemd) or a terminal. With no argument it starts every
enabled service under one lifecycle; naming one starts exactly that
service, enabled or not, provided its configuration validates.

Of the control surfaces only the control socket is enabled by
default: an owner-only Unix socket, one per profile, admitting same-uid
peers. REST (api), gRPC (rpc) and MCP over HTTP stay off until enabled:
heo service setup <service> enables one with the profile's token, or
--enable <service> for one run, or services.<service>.enabled in the
profile config.

The runtime services run on the profile's data store, which the first
of them opens as its exclusive owner: a store another daemon owns, or
one whose schema is not this heo's, refuses the start. workers runs
queued operations (services.workers.count at once); receivers
runs each channel's receiver, one per provider account; webhook, off
by default, serves channels' webhooks on its own loopback listener
(services.webhook.addr) and answers /healthz and /readyz; bridge
republishes uma's committed events, as references only, on the daemon's
in-process bus for hooks (services.bridge.enabled, default true; see
the deliveries-and-events manual); hooks, off by default
(services.hooks.enabled), runs the plugins: section's hook plugins, one
durable subscription each. With no channel configured they start and
are ready.

One daemon per profile: a second start for a profile whose daemon
runs is refused (exit 4) before anything starts. service stop,
service restart and service status reach the daemon of the selected
profile only.

SIGTERM, SIGINT or service stop stops acceptance, drains workers within
services.workers.drain_timeout, lets the bridge finish the batch in
hand, closes the store and exits 0; a second signal aborts the drain. service restart drains the same way, then
runs again in the same process, re-reading the configuration. Each
service logs its resolved address on its ready_reported line. Once
every service is ready the daemon writes its pidfile,
<runtime dir>/heo/<profile>/heo.pid, removed at exit.

Every HTTP surface requires authentication, on loopback too: a start
that would serve REST, gRPC or MCP over HTTP without the profile's
surface token (credentials.surface_token_ref, minted by service setup)
or services.<service>.auth.mode (or services.all.auth.mode) is
refused, and so are --no-auth, --insecure-remote and
--insecure-no-policy.

MCP over stdio (service start mcp --stdio) is for an agent that spawns
heo itself. It never becomes the daemon: it relays every daemon command
over the profile's control socket, as the spawning user, and refuses to
start (exit 70) when no daemon answers there.

--policy names the policy served calls run under; the start itself is
not gated by it. The daemon reads services.* only from heo's own
configuration (see heo config paths): a kit.yaml, .kit.yaml or
.kit/config.yaml in the working directory configures nothing.`

// refusedStartFlags are kit serve flags heo refuses outright: every
// HTTP surface heo serves requires authentication, on loopback too,
// and stays on loopback. Kit mounts --no-auth only once the api
// service has an authenticator; the other two are always present.
var refusedStartFlags = []string{"no-auth", "insecure-remote", "insecure-no-policy"}

// mountService adapts kit's `service` group to heo: the daemon's
// checks around `service start` (and its hidden, deprecated `serve`
// spelling), heo's sections in `service status`, and heo's own leaf,
// `service setup`. Kit owns the group and its start, list, stop,
// restart and status leaves. A leaf added here is management
// vocabulary the control socket withholds, so a daemon-backed command
// never goes here (`daemon ping` is in heo's own `daemon` group).
func (a *app) mountService() {
	grp := service.ServiceCommand(a.r)
	start := service.ServeCommand(a.r)
	if grp == nil || start == nil {
		panic("kit mounted no service group: no service is registered")
	}
	grp.Short = "Run, inspect, stop and restart the profile's daemon"
	grp.Long = `Run the daemon that serves the selected profile's command tree over
its control socket, and over REST, gRPC and MCP when enabled; list its
services; and ask it, over kit's per-profile control socket, for its
status, to stop, or to restart in place.`
	start.Short = "Run the profile's daemon in the foreground"
	start.Long = serviceStartLong
	for _, c := range []*cobra.Command{start, rootServe(a.r)} {
		if c != nil {
			a.guardStart(c)
		}
	}
	grp.AddCommand(a.serviceSetupCmd())
	a.registerServiceStatus()
}

// rootServe is kit's hidden, deprecated root `serve`, nil once kit
// drops it.
func rootServe(r *cli.Root) *cobra.Command {
	for _, c := range r.Cmd.Commands() {
		if c.Name() == "serve" && c.Annotations[cmdmeta.KeySelfHosting] == "true" {
			return c
		}
	}
	return nil
}

// guardStart runs heo's start checks before kit's RunE, takes over the
// shutdown signals (see watchShutdown), and removes the pidfile when
// the run ends, whatever the outcome. A restart
// re-executes inside kit's RunE and keeps the pid, so it keeps the
// pidfile too.
func (a *app) guardStart(c *cobra.Command) {
	run := c.RunE
	c.RunE = func(cmd *cobra.Command, args []string) error {
		if err := refuseStartFlags(cmd); err != nil {
			return err
		}
		if err := a.checkStart(cmd, args); err != nil {
			return err
		}
		defer a.pid.remove()
		defer a.removeRelayLock()
		ctx, release := watchShutdown(cmd.Context(), a.runtime.AbortDrain)
		defer release()
		cmd.SetContext(ctx)
		return run(cmd, args)
	}
}

// refuseStartFlags refuses refusedStartFlags when set on the command
// line.
func refuseStartFlags(c *cobra.Command) error {
	for _, name := range refusedStartFlags {
		if f := c.Flags().Lookup(name); f != nil && f.Changed {
			e := output.UsageError(fmt.Sprintf(
				"--%s is refused: heo serves every HTTP surface with authentication, on loopback only", name))
			e.SuggestedFix = "drop --" + name
			return e
		}
	}
	return nil
}

// controlScope is the kit control scope of the selected profile: its
// directory name (<name>, or aps-<id>), so each profile's daemon has a
// control socket of its own, <runtime dir>/heo/control-<pdir>.sock,
// beside the profile's own directory. A restart re-executes with the
// same arguments and environment, so it resolves the same profile.
//
// An MCP stdio relay is not the profile's daemon and must not take its
// control socket: it gets a scope of its own process,
// <pdir>.stdio-<pid>, which `service stop` never addresses (the relay
// ends with its input) and which a re-execution keeps.
//
// A start creates that runtime directory first: kit places the socket
// under it only when it exists, so a daemon started before it existed
// and a client run after would otherwise disagree on the path. Only a
// start creates it: a stop, restart or status with no daemon changes
// nothing on disk, like daemon ping, since without the directory there
// is no daemon to reach.
func (a *app) controlScope(c *cobra.Command) (string, error) {
	if a.state == nil {
		return "", errors.New("control scope: configuration not loaded")
	}
	start := a.isStart(c)
	if start {
		if _, err := settings.ControlDir(); err != nil {
			return "", err
		}
	}
	scope := a.state.Profile().Dir()
	if start && stdioRelay(c, c.Flags().Args(), a.r.Viper) {
		scope += relayScopeInfix + strconv.Itoa(os.Getpid())
		a.relayScope = scope
	}
	return scope, nil
}

// relayScopeInfix separates a profile directory from a relay's pid in
// its control scope. Profile directories hold no '.'.
const relayScopeInfix = ".stdio-"

// isStart reports whether c is kit's `service start` or its deprecated
// `serve` spelling.
func (a *app) isStart(c *cobra.Command) bool {
	return c == service.ServeCommand(a.r) || (c == rootServe(a.r) && c != nil)
}

// removeRelayLock removes the lock file kit leaves beside a relay's
// control socket once the relay's run has ended: its scope names this
// process alone, so nothing else can hold or want it.
func (a *app) removeRelayLock() {
	if a.relayScope == "" {
		return
	}
	dir, err := xdg.RuntimeDir(settings.Tool)
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, "control-"+a.relayScope+".sock.lock"))
}
