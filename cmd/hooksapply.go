package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/hooks"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/service"
	kitlog "hop.top/kit/go/console/log"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// Exit codes of a command that a retry cannot change: a usage error and
// a refusal by authorization.
const (
	exitUsage        = 2
	exitUnauthorized = 5
)

// restBridgeOptions is the option set of the REST surface's bridge,
// built by the same kit function `service start api` uses: the
// --policy engine and its permission rules, heo's daemon gate, the
// audit sinks, the rate limit, the command deadline and the rest of
// what services.api.* configures. A plugin call meets exactly that.
//
// The exposure is the api service's: loopback unless services.api.addr
// binds beyond it. With the service off there is no listener, so the
// plugin is local and the rate limit stays at its loopback default.
func (a *app) restBridgeOptions() ([]cmdsurface.Option, error) {
	v := a.viper()
	exp := service.ServeExposure{Loopback: true}
	if v != nil {
		if addr := v.GetString("services.api.addr"); v.GetBool("services.api.enabled") && addr != "" {
			exp.Loopback = listensLocally(addr)
		}
		exp.InsecureNoPolicy = v.GetBool("services.api.insecure_no_policy")
	}
	return service.ServeBridgeOptionsExposed(a.r, service.APIServiceName, exp)
}

// listensLocally reports whether a listen address is reachable only from
// this machine.
func listensLocally(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hookDispatcher runs a plugin's operations through the command tree's
// own bridge, the code path a REST request for the verb takes: the
// plugin is a verified caller on the REST surface, the daemon gate
// admits its actor, and what it may do is decided again by the
// authorization policy for every uma call, as for any other caller.
// Verbs withheld from REST (terminal only) and verbs that never run in
// the daemon (self-hosting, such as `plugin token mint`) are refused by
// the bridge as they are for a REST caller.
type hookDispatcher struct {
	// a resolves root, admit and serve when the dispatcher is first
	// used: the runtime host is built before the command tree.
	a     *app
	root  *cobra.Command
	admit control.Admitter
	serve func() bool

	// shared resolves the bridge options the REST surface runs under:
	// the policy gate, audit sinks, rate limit and the rest of what
	// `service start api` installs. Nil builds the daemon gate alone.
	shared func() ([]cmdsurface.Option, error)

	once    sync.Once
	initErr error
	bridge  *cmdsurface.Bridge
	leaves  map[string]*cmdsurface.Leaf
}

// hookDispatcher is the dispatcher for the app's command tree.
func (a *app) hookDispatcher() *hookDispatcher {
	return &hookDispatcher{a: a}
}

func (d *hookDispatcher) init() {
	d.once.Do(func() {
		if d.a != nil {
			d.root, d.admit, d.serve = d.a.r.Cmd, d.a.admit, d.a.control.servesControl
			if d.shared == nil {
				d.shared = d.a.restBridgeOptions
			}
		}
		opts := []cmdsurface.Option{cmdsurface.WithPolicy(cmdsurface.DefaultPolicy())}
		if d.shared != nil {
			// The options kit builds for the REST surface carry heo's
			// gate (service.WithPermission), so none is added here.
			var shared []cmdsurface.Option
			shared, d.initErr = d.shared()
			opts = append(opts, shared...)
		} else {
			opts = append(opts, cmdsurface.WithPermission(control.Gate(d.serve, d.admit)))
		}
		d.bridge = cmdsurface.New(d.root, opts...)
		d.bridge.Expose("*", cmdsurface.SurfaceREST)
		d.leaves = map[string]*cmdsurface.Leaf{}
		for _, l := range d.bridge.Leaves() {
			d.leaves[l.PathKey()] = l
		}
	})
}

func writes(se cli.SideEffect) bool {
	s := string(se)
	return strings.HasPrefix(s, string(cli.SideEffectWrite)) || strings.HasPrefix(s, string(cli.SideEffectDestructive))
}

// keyed reports whether the verb takes the operation's idempotency key
// as --idempotency-key: kit registers it on every conditionally
// idempotent write, and uma commits those operations under it.
func keyed(c *cobra.Command) bool {
	se, _ := cli.GetSideEffect(c)
	i, _ := cli.GetIdempotency(c)
	return writes(se) && i == cli.IdempotencyConditional
}

// Check implements hooks.Dispatcher.
func (d *hookDispatcher) Check(c hooks.Call) error {
	d.init()
	if d.initErr != nil {
		return d.initErr
	}
	path := strings.Fields(c.Verb)
	l, ok := d.leaves[strings.Join(path, " ")]
	if !ok {
		if nonInvocable(d.bridge, path) {
			return fmt.Errorf("%q is not a verb that runs in the daemon (a group, or a verb that hosts itself): a plugin may not run it", c.Verb)
		}
		return fmt.Errorf("unknown verb %q", c.Verb)
	}
	if !l.Enabled[cmdsurface.SurfaceREST] {
		return fmt.Errorf("%q runs at the terminal only: a plugin may not run it", c.Verb)
	}
	se, _ := cli.GetSideEffect(l.Cmd)
	i, _ := cli.GetIdempotency(l.Cmd)
	switch {
	case !d.bridge.Policy().Allowed(l.Class, cmdsurface.SurfaceREST):
		return fmt.Errorf("%q is destructive, which no served surface runs: a plugin may not run it", c.Verb)
	case strings.HasSuffix(string(se), "-local"):
		return fmt.Errorf("%q acts on the daemon's machine: a plugin may not run it", c.Verb)
	case writes(se) && i != cli.IdempotencyConditional && i != cli.IdempotencyYes:
		return fmt.Errorf("%q cannot be replayed safely: a plugin may not run it", c.Verb)
	}
	for name := range c.Flags {
		if f := l.Cmd.LocalNonPersistentFlags().Lookup(name); f == nil && (name != idempotencyKeyFlag || !keyed(l.Cmd)) {
			return fmt.Errorf("%q has no flag --%s", c.Verb, name)
		}
	}
	return nil
}

const idempotencyKeyFlag = "idempotency-key"

func nonInvocable(b *cmdsurface.Bridge, path []string) bool {
	want := strings.Join(path, " ")
	for _, n := range b.NonInvocable() {
		// A descriptor's path starts at the root.
		if len(n.Path) > 1 && strings.Join(n.Path[1:], " ") == want {
			return true
		}
	}
	return false
}

// Dispatch implements hooks.Dispatcher.
func (d *hookDispatcher) Dispatch(ctx context.Context, actor model.ActorID, c hooks.Call) error {
	d.init()
	if d.initErr != nil {
		return hooks.Permanent(d.initErr)
	}
	path := strings.Fields(c.Verb)
	flags := make(map[string]any, len(c.Flags)+1)
	for k, v := range c.Flags {
		flags[k] = v
	}
	if l := d.leaves[strings.Join(path, " ")]; l != nil && keyed(l.Cmd) {
		flags[idempotencyKeyFlag] = c.Key
	}
	res, err := d.bridge.Invoke(ctx, cmdsurface.Invocation{
		Path: path, Args: c.Args, Flags: flags,
		Meta: cmdsurface.Meta{
			Surface: cmdsurface.SurfaceREST, Caller: string(actor), Established: cmdsurface.EstablishedVerified,
		},
	})
	if err != nil {
		if refused(err) {
			return hooks.Permanent(err)
		}
		return err
	}
	if res.ExitCode == 0 {
		return nil
	}
	e := fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	if res.ExitCode == exitUsage || res.ExitCode == exitUnauthorized {
		return hooks.Permanent(e)
	}
	return e
}

// refused reports a refusal by the bridge's gates: nothing about it
// changes on a retry.
func refused(err error) bool {
	for _, target := range []error{
		cmdsurface.ErrPermissionDenied, cmdsurface.ErrSurfaceNotEnabled, cmdsurface.ErrNotInvocable,
		cmdsurface.ErrDestructiveBlocked, cmdsurface.ErrUnknownCommand, cmdsurface.ErrAuthRefused,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// hookLogger is the daemon's logger for the hook applier's records,
// built from the loaded configuration when it first speaks.
type hookLogger struct{ a *app }

func (l hookLogger) log() hooks.Logger {
	v := l.a.viper()
	if v == nil {
		return nil
	}
	lg := kitlog.New(v)
	lg.SetOutput(os.Stderr)
	return lg
}

func (l hookLogger) Info(msg any, kv ...any) {
	if lg := l.log(); lg != nil {
		lg.Info(msg, kv...)
	}
}

func (l hookLogger) Error(msg any, kv ...any) {
	if lg := l.log(); lg != nil {
		lg.Error(msg, kv...)
	}
}
