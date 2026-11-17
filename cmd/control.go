package cmd

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli/service"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/model"
)

// controlState tracks whether this process serves the profile's
// control socket, which makes it the daemon: the process daemon-backed
// commands run in. It is set when kit starts the socket service,
// before the socket binds, so no request can reach the socket before
// the process knows it is the daemon.
type controlState struct {
	serving  atomic.Bool
	started  bus.Topic
	finished map[bus.Topic]bool
}

func newControlState() *controlState {
	topics := serve.DefaultTopics(serve.DefaultTopicPrefix)
	return &controlState{
		started: topics[serve.ObjectService+"."+serve.ActionStarted],
		finished: map[bus.Topic]bool{
			topics[serve.ObjectService+"."+serve.ActionStopped]: true,
			topics[serve.ObjectService+"."+serve.ActionFailed]:  true,
		},
	}
}

// Publish follows the socket service's lifecycle.
func (s *controlState) Publish(_ context.Context, e bus.Event) error {
	p, ok := e.Payload.(serve.EventPayload)
	if !ok || p.Service != service.SocketServiceName {
		return nil
	}
	switch {
	case e.Topic == s.started:
		s.serving.Store(true)
	case s.finished[e.Topic]:
		s.serving.Store(false)
	}
	return nil
}

// servesControl reports whether this process is the daemon.
func (s *controlState) servesControl() bool { return s.serving.Load() }

// publishers fans one lifecycle event out to several publishers.
type publishers []serve.Publisher

func (ps publishers) Publish(ctx context.Context, e bus.Event) error {
	var errs []error
	for _, p := range ps {
		if err := p.Publish(ctx, e); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// controlClient is the client for the selected profile's daemon.
func (a *app) controlClient() control.Client {
	return control.Client{
		Path:  a.r.Viper.GetString(settings.KeySocketPath),
		Start: a.heoCommand("service start"),
	}
}

// daemonCommand marks c daemon-backed: it needs the profile's runtime,
// so it runs in the daemon (see daemonRun).
func daemonCommand(c *cobra.Command) *cobra.Command {
	control.MarkDaemon(c)
	return c
}

// daemonRun runs a daemon-backed command where the profile's runtime
// lives, and returns its output for the command to render.
//
//   - In the daemon, a served call runs op here, for the actor the
//     transport verified and the profile's tenant.
//   - At the terminal, and in an MCP stdio process relaying for the
//     agent that spawned it, the command is sent over the control
//     socket; the daemon's answer, output or error envelope, comes
//     back unchanged. No daemon running is PREREQUISITE, exit 70; none
//     is started.
//   - A served call reaching any other process is refused: re-sending
//     it would act under this process's identity, not the caller's.
func daemonRun[T any](a *app, cmd *cobra.Command, args []string,
	op func(ctx context.Context, c control.Caller) (T, error),
) (T, error) {
	return daemonRunWith(a, cmd, args, op, control.Decode[T])
}

// daemonRunReport is daemonRun for a command that reports a verdict and
// fails with failCode when the report holds a failure: forwarded, the
// daemon's report comes back for this process to render and judge.
func daemonRunReport[T any](a *app, cmd *cobra.Command, args []string, failCode string,
	op func(ctx context.Context, c control.Caller) (T, error),
) (T, error) {
	return daemonRunWith(a, cmd, args, op, func(res control.Result) (T, error) {
		return control.DecodeReport[T](res, failCode)
	})
}

func daemonRunWith[T any](a *app, cmd *cobra.Command, args []string,
	op func(ctx context.Context, c control.Caller) (T, error),
	decode func(control.Result) (T, error),
) (T, error) {
	var zero T
	ctx := cmd.Context()
	route, meta := control.RouteOf(ctx, a.control.servesControl())
	switch route {
	case control.Local:
		caller, err := control.CallerOf(meta, model.TenantID(a.r.Viper.GetString(settings.KeyTenant)))
		if err != nil {
			return zero, err
		}
		return op(ctx, caller)
	case control.Forward:
		res, err := a.controlClient().Invoke(ctx, control.RequestFor(cmd, args))
		if err != nil {
			return zero, err
		}
		if res.ExitCode == 0 && res.Stderr != "" {
			// Warnings the command wrote in the daemon reach the
			// caller as they would have here.
			_, _ = cmd.ErrOrStderr().Write([]byte(res.Stderr))
		}
		return decode(res)
	default:
		return zero, a.notDaemon()
	}
}

// notDaemon refuses a served call that reached a process other than the
// daemon.
func (a *app) notDaemon() error {
	e := output.PrerequisiteError(
		"this process does not serve the profile's control socket, so it does not run the runtime: " +
			"daemon commands are served by the daemon")
	e.SuggestedFix = "enable this surface on the daemon (`" + a.heoCommand("service start") +
		"` with services.<surface>.enabled) instead of in a separate process"
	return e
}
