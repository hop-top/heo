// Package cmd wires heo's commands onto a kit root.
//
// The root carries the kit global flags (--format, --quiet, --no-color,
// -c/--config, -V/--verbose, ...), heo's own --profile global, the
// reserved `spec` verb (`spec coverage` is the side-effect annotation
// gate), the `config`, `service`, `daemon`, `storage`, `adapter`, `channel`,
// `inbox`, `conversation`, `label`, `message`, `note`, `contact` and `identity`
// groups, and `upgrade`,
// the one top-level verb (self-update, by declared exception). Kit owns
// the `service` group: `service start` supervises the built-in api,
// socket, mcp and rpc services and heo's runtime services (workers,
// receivers, webhook, bridge; see internal/daemon), and `service list`,
// `stop`, `restart` and `status` reach the selected profile's daemon
// over kit's per-profile control socket. heo adds `service setup` and
// its sections in `service status` (see service.go), and its own
// `daemon ping`, served on the control socket (see ping.go). Root `status` is
// kit's tool status. Of the surfaces, only the control socket is
// enabled by default; of the runtime, workers, receivers and the event
// bridge. See internal/settings and internal/daemon for every default.
package cmd

import (
	"context"
	"errors"
	"maps"
	"os"
	"sync/atomic"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"hop.top/heo/internal/approvechat"
	"hop.top/heo/internal/assist"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/profile"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/storage"
	"hop.top/heo/internal/version"
	speccli "hop.top/kit/go/ai/toolspec/cli"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/mcpserve"
	"hop.top/kit/go/console/cli/policy"
	"hop.top/kit/go/console/cli/rpcserve"
	"hop.top/kit/go/console/cli/service"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/core/config"
)

// Names of heo's root global and its environment variable.
const (
	profileFlag = "profile"
	envPrefix   = "HEO"
	profileEnv  = envPrefix + "_PROFILE"
)

// annotAllowMissingProfile marks a command that may run for a named
// profile whose config file does not exist yet: `config set` creates
// it, `config path`/`paths` show where it would be.
const annotAllowMissingProfile = "heo/allow-missing-profile"

// app is one heo command tree and the configuration it loaded.
type app struct {
	r       *cli.Root
	state   *settings.State
	storage *storage.Registry
	runtime *daemon.Host
	pid     *pidfile
	control *controlState
	// hub is the bus the event bridge publishes on, and the daemon's
	// lifecycle events too (see internal/bridge). Made lazily: a
	// process that publishes nothing touches nothing.
	hub *bridge.Hub
	// relayScope is the kit control scope of this process when it is
	// an MCP stdio relay (see controlScope).
	relayScope string
	authz      policyState
	// creds is the credential backend the open runtime resolves channel
	// credentials through: it remembers which ones a provider rejected,
	// so `channel check --remote` diagnoses with it.
	creds atomic.Pointer[credentials.Backend]
}

// root is built once, at package initialization.
var root = newApp()

func newApp() *app {
	a := &app{storage: newStorageRegistry(), hub: bridge.NewHub()}
	a.observeBus()
	a.runtime = a.newRuntimeHost()
	a.pid = newPIDFile(a)
	a.control = newControlState()
	runtimeServices := make([]serve.Service, 0, 4)
	for _, s := range a.runtime.Services() {
		runtimeServices = append(runtimeServices, s)
	}
	a.r = cli.New(cli.Config{
		Name:    "heo",
		Version: version.Version(),
		Short:   "Messaging CLI and daemon for uma: unified inbox, replies and recovery from the terminal",
		Globals: []cli.Flag{{
			Name: profileFlag,
			Usage: "Profile to act on: <name>, aps:<id>, heo://profile/<name> or aps://profile/<id> " +
				"(env " + profileEnv + "; default \"default\")",
		}},
		// A leaf that redefines a kit global, uses a reserved name or
		// nests too deep fails validation instead of logging.
		SignatureStrictness: cli.SignatureStrictnessReject,
		Hooks: cli.Hooks{
			PrePersistentRunE: func(cmd *cobra.Command, args []string) error {
				return withUsageFix(cmd, a.load(cmd, args))
			},
		},
	},
		// `status`: the tool status (profile, environment, auth,
		// effective configuration), no daemon needed; heo adds its
		// profile section. Daemon status is `service status`.
		cli.WithStatus(cli.StatusConfig{}),
		// heo's loader (load) is the only source of services.*: kit
		// layers no file of its own (no cwd kit.yaml, .kit.yaml or
		// .kit/config.yaml, no user file), binds no variable and applies
		// no -c token for `service start` and `service list`.
		service.WithOwnServiceConfig(),
		// One daemon per profile, each with a kit control socket of its
		// own: `service stop|restart|status` reach the selected
		// profile's daemon only, and a second start for a profile whose
		// daemon runs is refused (CONFLICT) before anything starts.
		service.WithServiceControlScope(a.controlScope),
		// --policy <name>: kit-default, or <user config>/heo/policies/
		// <name>.yaml. On `service start` it is the policy served calls
		// run under.
		cli.WithPolicy(func(name string) (policy.Policy, error) { return policy.LoadNamed(settings.Tool, name) }),
		// `service start api`: the command tree over REST, with discovery at
		// /v1/commands and an OpenAPI document. Registered but off by
		// default (services.api.enabled: false); `service setup` enables
		// it. Its /readyz answers for the runtime services too. REST,
		// gRPC and MCP over HTTP authenticate with the profile's surface
		// token (surfaceAuth) unless services.<svc>.auth.mode selects a
		// kit authenticator.
		service.WithAPI(service.APIConfig{
			DependsOn: []string{daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook},
			Auth:      a.surfaceAuth,
		}),
		// `service start socket`: the same tree over an owner-only Unix socket,
		// one per profile, admitting same-uid peers verified by the
		// kernel. The only service enabled by default, and the CLI's one
		// path to the daemon (internal/control): destructive commands
		// are permitted on it, and on no other surface.
		service.WithSocket(service.SocketConfig{Policy: control.SocketPolicy()}),
		// `service start mcp`: the same tree as MCP tools, over HTTP or stdio.
		// Registered but off by default. It links the MCP SDK, about
		// 2 MB of a stripped release binary.
		mcpserve.With(mcpserve.Config{Auth: a.surfaceAuth}),
		// `service start rpc`: the same tree as the cmdsurface.v1.Commands
		// service for Connect, gRPC and gRPC-Web clients. Registered but
		// off by default.
		rpcserve.With(rpcserve.Config{Auth: a.surfaceAuth}),
		// The runtime: `service start bridge|workers|receivers|webhook`.
		// Workers, receivers and the event bridge are on by default,
		// the webhook listener off; see internal/daemon. Registered
		// after the control surfaces, so kit stops them first: ingress
		// stops, then workers drain, then the bridge reads what they
		// committed, while the control socket still answers.
		service.WithServices(runtimeServices...),
		// Writes the profile's pidfile once the run is up, and marks
		// the process that serves the control socket as the daemon.
		// The lifecycle events also ride the bus, for desk scoped
		// subscribers: a public subscriber never receives them.
		service.WithServiceBus(publishers{a.pid, a.control, a.hub}),
		// Daemon commands run only for a caller the transport
		// verified and heo's authorization policy admits, on every
		// surface alike (internal/control, internal/authz). The same
		// policy then decides every uma operation.
		service.WithPermission(control.Gate(a.control.servesControl, a.admit)),
	)
	// `spec`: the machine-readable manifest agents read, and `spec
	// coverage`, which lists every command that declares no
	// kit/side-effect. `spec coverage --min 100` keeps it at 100%.
	if err := speccli.RegisterSpecCommand(a.r, "1.0"); err != nil {
		panic(err)
	}
	a.mountService()
	// Kit mounts `token` (claims, decode, verify) once the api service
	// has an authenticator in code. heo's one token is the surface
	// token `service setup` mints, so kit's verbs stay hidden.
	for _, c := range a.r.Cmd.Commands() {
		if c.Name() == "token" {
			c.Hidden = true
		}
	}
	a.r.RegisterStatusProvider(profileStatusName, a.profileStatus)
	a.r.Cmd.AddCommand(a.configCmd(), a.daemonCmd(), a.storageCmd(), a.upgradeCmd(),
		a.adapterCmd(), a.channelCmd(), a.inboxCmd(), a.conversationCmd(), a.labelCmd(), a.providerLabelCmd(), a.messageCmd(), a.noteCmd(), a.attachmentCmd(),
		a.contactCmd(), a.identityCmd(), a.deliveryCmd(), a.eventCmd(), a.operationCmd(), a.attentionCmd())
	a.r.Cmd.AddCommand(a.reactionCmd(), a.participantCmd(), a.receiptCmd(), a.historyCmd(), a.interactionCmd())
	a.r.Cmd.AddCommand(a.taskCmd(), a.pluginCmd(), a.hookCmd())
	a.r.Cmd.AddCommand(a.extraCommands()...)
	fixUsageErrors(a.r.Cmd)
	applyGuidance(a.r.Cmd)
	return a
}

// Execute runs the CLI and returns the error the root reports.
func Execute() error {
	return root.r.Execute(context.Background())
}

// ExitCode maps an error returned by Execute onto the process exit
// code. Kit's structured errors carry their own — 2 for a usage
// error, 3 for not found, 5 for unauthorized, and so on — and an
// error without one exits 1.
func ExitCode(err error) int {
	type withCLIError interface{ AsCLIError() *output.Error }
	var ce withCLIError
	if errors.As(err, &ce) {
		if out := ce.AsCLIError(); out != nil && out.ExitCode != 0 {
			return out.ExitCode
		}
	}
	return 1
}

// load resolves the profile, then layers its configuration into
// root.Viper before any command runs. It runs once per process.
//
// The profile comes from --profile, then HEO_PROFILE, else `default`:
// never from a config file, and never from aps's own environment
// (APS_PROFILE). An aps-bound profile must exist in aps.
//
// The rest resolves through internal/settings: defaults, then
// /etc/heo/config.yaml, the user file, the profile file, -c files,
// HEO_<KEY> environment variables, flags, and -c key=value.
func (a *app) load(cmd *cobra.Command, _ []string) error {
	if a.state != nil {
		return nil
	}
	v := a.r.Viper
	if err := config.BindEnv(v, envPrefix); err != nil {
		return err
	}
	in, err := a.resolveProfile(cmd)
	if err != nil {
		return err
	}
	if err := profile.CheckExists(in.Profile); err != nil {
		return err
	}
	if in.SystemPath, in.UserPath, in.ProfilePath, err = settings.Paths(in.Profile); err != nil {
		return err
	}
	if in.Files, in.Overrides, err = a.r.ConfigArgs(); err != nil {
		return output.UsageError(err.Error())
	}
	in.EnvPrefix = envPrefix
	in.ExtraDefaults = a.storage.Defaults()
	maps.Copy(in.ExtraDefaults, upgradeDefaults())
	maps.Copy(in.ExtraDefaults, daemon.Defaults())
	maps.Copy(in.ExtraDefaults, composition.Defaults())
	maps.Copy(in.ExtraDefaults, authz.Defaults())
	maps.Copy(in.ExtraDefaults, assist.Defaults())
	in.RequireProfileFile = cmd.Annotations[annotAllowMissingProfile] != "true"
	in.Flags = func(key string) (string, bool) {
		if f := cmd.Flags().Lookup(key); f != nil && f.Changed {
			return f.Name, true
		}
		return "", false
	}
	state, err := settings.Load(v, in)
	if err != nil {
		return err
	}
	a.state = state
	gateApprovalChat(v)
	return nil
}

// gateApprovalChat keeps the approval-chat service out of the daemon
// unless held_operations.chat.enabled is true: like the webhook
// listener, a service nothing asked for is not registered, so it
// neither starts nor takes a place in the stop order or the drain. An
// explicit services.approval-chat.enabled: false still wins. A config
// that does not parse is left for the service's own validation.
func gateApprovalChat(v *viper.Viper) {
	if cfg, err := approvechat.Load(v); err == nil && !cfg.Enabled {
		v.Set("services."+daemon.NameApprovalChat+".enabled", false)
	}
}

// resolveProfile reads --profile, then HEO_PROFILE (through the env
// binding), before any config file is merged.
func (a *app) resolveProfile(cmd *cobra.Command) (settings.Inputs, error) {
	raw := a.r.Viper.GetString(profileFlag)
	in := settings.Inputs{ProfileOrigin: settings.OriginDefault, ProfileDetail: "built-in"}
	switch {
	case flagChanged(cmd, profileFlag):
		in.ProfileOrigin, in.ProfileDetail = settings.OriginFlag, "--"+profileFlag
	case os.Getenv(profileEnv) != "":
		in.ProfileOrigin, in.ProfileDetail = settings.OriginEnv, profileEnv
	}
	ref, err := profile.Parse(raw)
	if err != nil {
		return in, err
	}
	in.Profile = ref
	return in, nil
}

func flagChanged(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}
