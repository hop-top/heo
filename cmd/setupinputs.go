package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"

	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli/mcpserve"
	"hop.top/kit/go/console/cli/rpcserve"
	"hop.top/kit/go/console/cli/service"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/console/wizard"
)

// setupInputs is what `service setup` was asked to do: from its
// arguments and flags, and, on a terminal, from the wizard for what
// they left out.
type setupInputs struct {
	// enable are the HTTP surfaces to enable, in settings.HTTPSurfaces
	// order.
	enable []string
	// disable are the HTTP surfaces to turn off.
	disable []string
	// addrs are the listen addresses asked for, by surface.
	addrs map[string]string
	// rotate replaces the surface token; show prints it.
	rotate, show bool
}

// empty reports whether nothing was asked: the input a terminal-less
// run cannot make up.
func (in setupInputs) empty() bool {
	return len(in.enable) == 0 && len(in.disable) == 0 && len(in.addrs) == 0 && !in.rotate && !in.show
}

// setupDefaultAddrs are kit's default listen addresses, the first a
// surface is offered.
var setupDefaultAddrs = map[string]string{
	"api": service.DefaultAPIAddr,
	"rpc": rpcserve.DefaultAddr,
	"mcp": mcpserve.DefaultAddr,
}

// parseSetupInputs validates the arguments and flags of `service
// setup`. Every refusal is USAGE, exit 2.
func parseSetupInputs(args, addrFlags, disable []string, rotate, show bool) (setupInputs, error) {
	in := setupInputs{addrs: map[string]string{}, rotate: rotate, show: show}
	for _, svc := range args {
		if err := knownSurface(svc); err != nil {
			return in, err
		}
		in.enable = appendSurface(in.enable, svc)
	}
	for _, svc := range disable {
		if err := knownSurface(svc); err != nil {
			return in, err
		}
		if slices.Contains(in.enable, svc) {
			return in, output.UsageError(fmt.Sprintf("%s is both enabled and disabled: name it once", svc))
		}
		in.disable = appendSurface(in.disable, svc)
	}
	for _, raw := range addrFlags {
		svc, addr, ok := strings.Cut(raw, "=")
		if !ok || addr == "" {
			e := output.UsageError(fmt.Sprintf("--addr %q: want <surface>=<host:port>", raw))
			e.SuggestedFix = "--addr api=127.0.0.1:8080"
			return in, e
		}
		if err := knownSurface(svc); err != nil {
			return in, err
		}
		if slices.Contains(in.disable, svc) {
			return in, output.UsageError(fmt.Sprintf("--addr %s: %s is being disabled", svc, svc))
		}
		if err := loopbackAddr(addr); err != nil {
			return in, addrError(svc, addr, err)
		}
		in.addrs[svc] = addr
	}
	return in, nil
}

func knownSurface(svc string) error {
	if slices.Contains(settings.HTTPSurfaces, svc) {
		return nil
	}
	e := output.UsageError(fmt.Sprintf("unknown surface %q: service setup configures %s (the control socket needs no setup)",
		svc, strings.Join(settings.HTTPSurfaces, ", ")))
	e.SuggestedFix = "heo service setup api"
	return e
}

// appendSurface adds svc once, keeping settings.HTTPSurfaces order.
func appendSurface(list []string, svc string) []string {
	if slices.Contains(list, svc) {
		return list
	}
	list = append(list, svc)
	slices.SortFunc(list, func(a, b string) int {
		return slices.Index(settings.HTTPSurfaces, a) - slices.Index(settings.HTTPSurfaces, b)
	})
	return list
}

// errNotLoopback is loopbackAddr's refusal of a reachable host.
var errNotLoopback = errors.New("not a loopback address: heo serves REST, gRPC and MCP on loopback only " +
	"(127.0.0.0/8 or ::1, by IP; a name such as localhost is not accepted)")

// loopbackAddr accepts host:port with host a loopback IP literal and
// a port in 1-65535. A host name is refused: what it resolves to is
// not heo's to decide.
func loopbackAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("not host:port")
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errNotLoopback
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("the port must be 1-65535")
	}
	return nil
}

func addrError(svc, addr string, err error) error {
	e := output.UsageError(fmt.Sprintf("--addr %s=%s: %v", svc, addr, err))
	e.SuggestedFix = fmt.Sprintf("--addr %s=%s", svc, setupDefaultAddrs[svc])
	return e
}

// proposeAddrs picks a listen address for every HTTP surface: a
// loopback address the configuration already gives it, else kit's
// default port, else the next port free on loopback. Addresses keep
// clear of one another and of the addresses in fixed.
func proposeAddrs(current, fixed map[string]string, free func(addr string) bool) map[string]string {
	taken := map[string]bool{}
	for _, m := range []map[string]string{current, fixed} {
		for _, addr := range m {
			taken[addr] = true
		}
	}
	out := map[string]string{}
	for _, svc := range settings.HTTPSurfaces {
		if addr := current[svc]; addr != "" && loopbackAddr(addr) == nil {
			out[svc] = addr
			continue
		}
		host, port, _ := net.SplitHostPort(setupDefaultAddrs[svc])
		first, _ := strconv.Atoi(port)
		pick := setupDefaultAddrs[svc]
		for p := first; p < first+100 && p <= 65535; p++ {
			addr := net.JoinHostPort(host, strconv.Itoa(p))
			if !taken[addr] && free(addr) {
				pick = addr
				break
			}
		}
		taken[pick] = true
		out[svc] = pick
	}
	return out
}

// portFree reports whether addr can be bound now.
func portFree(addr string) bool {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// setupInteractive reports whether `service setup` may prompt: stdin
// and stderr are both terminals. Prompts go to stderr, so stdout stays
// the result. Tests replace it.
var setupInteractive = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stderr.Fd())
}

// promptSetup asks, on a terminal, for what the arguments and flags
// left out: which surfaces, when nothing at all was asked, and the
// listen address of each surface being enabled that --addr did not
// give, offering defaults. It asks nothing when nothing is missing.
func promptSetup(ctx context.Context, in setupInputs, enabled []string, defaults map[string]string, r io.Reader, w io.Writer) (setupInputs, error) {
	in.addrs = maps.Clone(in.addrs)
	if in.addrs == nil {
		in.addrs = map[string]string{}
	}
	var steps []wizard.Step
	askSurfaces := in.empty()
	if askSurfaces {
		opts := []wizard.Option{
			{Value: "api", Label: "api: REST, with discovery at /v1/commands"},
			{Value: "rpc", Label: "rpc: Connect, gRPC and gRPC-Web"},
			{Value: "mcp", Label: "mcp: MCP over HTTP"},
		}
		s := wizard.MultiSelect("surfaces", "Surfaces to enable", opts).WithRequired()
		if len(enabled) > 0 {
			s = s.WithDefault(enabled)
		}
		steps = append(steps, s)
	}
	for _, svc := range settings.HTTPSurfaces {
		if in.addrs[svc] != "" || (!askSurfaces && !slices.Contains(in.enable, svc)) {
			continue
		}
		s := wizard.TextInput("addr."+svc, svc+" listen address (loopback)").
			WithDefault(defaults[svc]).
			WithValidateText(loopbackAddr)
		if askSurfaces {
			s = s.WithWhen("surfaces", func(v any) bool { list, _ := v.([]string); return slices.Contains(list, svc) })
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		return in, nil
	}
	wz, err := wizard.New(steps...)
	if err != nil {
		return in, err
	}
	if err := wizard.Run(ctx, wz, wizard.ForceLine(), wizard.WithInput(r), wizard.WithOutput(w)); err != nil {
		var abort *wizard.AbortError
		if errors.As(err, &abort) {
			return in, output.UsageError("setup aborted: nothing was written")
		}
		return in, err
	}
	res := wz.Results()
	for _, svc := range wizard.Strings(res, "surfaces") {
		if err := knownSurface(svc); err != nil {
			return in, err
		}
		in.enable = appendSurface(in.enable, svc)
	}
	for _, svc := range settings.HTTPSurfaces {
		if addr := wizard.String(res, "addr."+svc); addr != "" {
			in.addrs[svc] = addr
		}
	}
	return in, nil
}
