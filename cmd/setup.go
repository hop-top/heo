package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/core/config"
)

const serviceSetupLong = `Enable REST (api), gRPC (rpc) or MCP over HTTP (mcp) for the selected
profile, on loopback, with the profile's surface token as their
authenticator. The control socket needs no setup.

  heo service setup api mcp

writes the profile file: services.<surface>.enabled and
services.<surface>.addr for each surface, and
credentials.surface_token_ref, the store key of the token. The address
is --addr <surface>=<host:port>, else the one configured, else kit's
default port (api 8080, mcp 8081, rpc 8082 on 127.0.0.1), else the
next free one. Only loopback IP addresses are accepted.

The token is 32 random bytes in the profile's credential backend
(credentials.backend; an aps-bound profile keeps it in heo's own
store), one per profile, shared by the three surfaces. A request
bearing it acts as uid:<n> of the user who ran setup, the actor the
control socket names that user. A re-run keeps the token;
--rotate-token replaces it, and the old one is refused from the next
request on. The token is printed only with --show-token, once, on
stdout; never in configuration or logs.

A running daemon applies the surfaces on restart (heo service restart);
a rotated token needs no restart.

On a terminal (stdin and stderr), setup asks for what the arguments
left out, on stderr. Anywhere else it never prompts: with nothing to
do it refuses (exit 2), naming the arguments. --dry-run prints the
plan, without the token, and writes nothing. --disable <surface> turns
a surface off.`

// setupResult is what `service setup` reports. The token's value is
// in it only with --show-token.
type setupResult struct {
	Profile string `json:"profile" yaml:"profile"`
	// Config is the profile file setup writes.
	Config string `json:"config" yaml:"config"`
	// Changed reports whether this run wrote or minted anything.
	Changed  bool           `json:"changed" yaml:"changed"`
	Surfaces []setupSurface `json:"surfaces" yaml:"surfaces"`
	Token    *setupToken    `json:"token,omitempty" yaml:"token,omitempty"`
	// Restart says how a running daemon picks the change up, empty
	// when it needs nothing.
	Restart string `json:"restart,omitempty" yaml:"restart,omitempty"`
}

// setupSurface is one HTTP surface after setup.
type setupSurface struct {
	Name    string `json:"name" yaml:"name"`
	Enabled bool   `json:"enabled" yaml:"enabled"`
	Addr    string `json:"addr,omitempty" yaml:"addr,omitempty"`
	// URL is where a client reaches the surface.
	URL     string `json:"url,omitempty" yaml:"url,omitempty"`
	Changed bool   `json:"changed" yaml:"changed"`
}

// setupToken describes the surface token, never its value unless
// --show-token asked for it.
type setupToken struct {
	// Ref is the token's store key, credentials.surface_token_ref.
	Ref string `json:"ref" yaml:"ref"`
	// Backend is the credential backend holding it.
	Backend string `json:"backend" yaml:"backend"`
	// Subject is the actor a bearer of the token acts as.
	Subject string `json:"subject" yaml:"subject"`
	// Action is what this run did: minted, rotated or kept.
	Action string `json:"action" yaml:"action"`
	// Value is the token, with --show-token only.
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
}

// Token actions.
const (
	tokenMinted  = "minted"
	tokenRotated = "rotated"
	tokenKept    = "kept"
)

// setupWrite is one profile-file key setup sets.
type setupWrite struct {
	key   string
	value any
}

func (a *app) serviceSetupCmd() *cobra.Command {
	var addrs, disable []string
	var rotate, show bool
	c := &cobra.Command{
		Use:   "setup [api|rpc|mcp ...]",
		Short: "Enable REST, gRPC or MCP over HTTP with the profile's token",
		Long:  serviceSetupLong,
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := parseSetupInputs(args, addrs, disable, rotate, show)
			if err != nil {
				return err
			}
			return a.serviceSetup(cmd, in)
		},
	}
	c.Flags().StringArrayVar(&addrs, "addr", nil, "Listen address of a surface, <surface>=<host:port>, loopback only (repeatable)")
	c.Flags().StringArrayVar(&disable, "disable", nil, "Turn a surface off (repeatable)")
	c.Flags().BoolVar(&rotate, "rotate-token", false, "Replace the surface token; the old one is refused from the next request on")
	c.Flags().BoolVar(&show, "show-token", false, "Print the surface token on stdout, once")
	cli.SetSideEffect(c, cli.SideEffectWrite)
	// --rotate-token mints a new token on every run.
	cli.SetIdempotency(c, cli.IdempotencyNo)
	// Never invocable on a served surface: it configures them.
	c.Annotations[cmdmeta.KeySelfHosting] = "true"
	allowMissingProfile(c)
	setOutputSchema(c, &setupResult{}, "1.0")
	_ = cli.SetExamples(c, []cli.Example{
		{Title: "Enable REST and MCP on loopback", Command: "heo service setup api mcp"},
		{Title: "Pick the address", Command: "heo service setup rpc --addr rpc=127.0.0.1:9082"},
		{Title: "Replace the token and print the new one", Command: "heo service setup --rotate-token --show-token"},
		{Title: "See the plan, write nothing", Command: "heo service setup api --dry-run"},
	})
	return c
}

// serviceSetup completes in on a terminal, then plans, and unless
// --dry-run, applies: the token first, so configuration never names a
// token that is not stored.
func (a *app) serviceSetup(cmd *cobra.Command, in setupInputs) error {
	v := a.r.Viper
	ref := a.state.Profile()
	current := currentSurfaces(v)
	enabled := enabledSurfaces(v)
	proposals := proposeAddrs(current, in.addrs, portFree)
	var err error
	switch {
	case setupInteractive():
		if in, err = promptSetup(cmd.Context(), in, enabled, proposals, os.Stdin, os.Stderr); err != nil {
			return err
		}
	case in.empty():
		e := output.UsageError("nothing to set up: name the surfaces to enable (api, rpc, mcp), " +
			"or pass --disable, --addr, --rotate-token or --show-token; without a terminal setup never prompts")
		e.SuggestedFix = "heo service setup api"
		return e
	}

	after := map[string]bool{}
	for _, svc := range settings.HTTPSurfaces {
		after[svc] = (slices.Contains(enabled, svc) || slices.Contains(in.enable, svc)) && !slices.Contains(in.disable, svc)
	}
	addrOf := map[string]string{}
	for _, svc := range settings.HTTPSurfaces {
		switch {
		case in.addrs[svc] != "" && !after[svc]:
			e := output.UsageError(fmt.Sprintf("--addr %s: %s is not enabled", svc, svc))
			e.SuggestedFix = "heo service setup " + svc + " --addr " + svc + "=" + in.addrs[svc]
			return e
		case in.addrs[svc] != "":
			addrOf[svc] = in.addrs[svc]
		case after[svc]:
			addrOf[svc] = proposals[svc]
		}
	}
	seen := map[string]string{}
	for _, svc := range settings.HTTPSurfaces {
		if other, dup := seen[addrOf[svc]]; dup && addrOf[svc] != "" {
			return output.UsageError(fmt.Sprintf("%s and %s would both listen on %s: give one another --addr", other, svc, addrOf[svc]))
		}
		seen[addrOf[svc]] = svc
	}

	file, err := readProfileFile(a.state.ProfilePath())
	if err != nil {
		return err
	}
	needToken := in.rotate || in.show
	for _, on := range after {
		needToken = needToken || on
	}
	tokenRef := settings.SurfaceTokenRef(v)
	if tokenRef == "" {
		tokenRef = credentials.SurfaceTokenKey(ref)
	}
	var writes []setupWrite
	want := func(key string, value any) {
		if !file.IsSet(key) || fmt.Sprint(file.Get(key)) != fmt.Sprint(value) {
			writes = append(writes, setupWrite{key, value})
		}
	}
	for _, svc := range settings.HTTPSurfaces {
		switch {
		case after[svc]:
			want("services."+svc+".enabled", true)
			want("services."+svc+".addr", addrOf[svc])
		case slices.Contains(in.disable, svc) && (slices.Contains(enabled, svc) || file.GetBool("services."+svc+".enabled")):
			want("services."+svc+".enabled", false)
		}
	}
	if needToken {
		want(credentials.KeySurfaceTokenRef, tokenRef)
	}

	// The token: what exists, and what this run does about it.
	var tok *setupToken
	var b *credentials.Backend
	if needToken {
		if b, err = a.openBackend(cmd.Context()); err != nil {
			return fmt.Errorf("credential backend: %w", err)
		}
		meta, exists, err := credentials.LookupSurfaceToken(cmd.Context(), b.SurfaceTokens(), tokenRef)
		if err != nil {
			return err
		}
		// heo's own kit store (credentials.backend), an aps-bound
		// profile's too: its aps secrets are read-only to heo.
		tok = &setupToken{Ref: tokenRef, Backend: credentials.ConfigFrom(v.GetString).Backend, Subject: string(authz.OwnerActor()), Action: tokenKept}
		switch {
		case !exists:
			tok.Action = tokenMinted
		case in.rotate:
			tok.Action = tokenRotated
		default:
			tok.Subject = string(meta.Subject)
		}
	}

	if cli.IsDryRun(cmd) {
		return cli.RenderPlan(cmd, setupPlan(in, a.state.ProfilePath(), writes, tok))
	}

	if tok != nil && tok.Action != tokenKept {
		value, err := credentials.MintSurfaceToken(cmd.Context(), b.SurfaceTokens(), tokenRef,
			authz.OwnerActor(), time.Now())
		if err != nil {
			return err
		}
		if in.show {
			tok.Value = value
		}
	} else if tok != nil && in.show {
		if tok.Value, err = credentials.RevealSurfaceToken(cmd.Context(), b.SurfaceTokens(), tokenRef); err != nil {
			return err
		}
	}
	path := a.state.ProfilePath()
	for _, w := range writes {
		if err := config.SetValue(w.key, w.value, config.ScopeUser, config.Options{UserConfigPath: path}); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}

	res := setupResult{
		Profile: ref.String(),
		Config:  path,
		Changed: len(writes) > 0 || (tok != nil && tok.Action != tokenKept),
		Token:   tok,
	}
	for _, svc := range settings.HTTPSurfaces {
		s := setupSurface{Name: svc, Enabled: after[svc], Changed: wrote(writes, "services."+svc+".")}
		if after[svc] {
			s.Addr, s.URL = addrOf[svc], surfaceURL(svc, addrOf[svc])
		}
		res.Surfaces = append(res.Surfaces, s)
	}
	if len(writes) > 0 {
		res.Restart = "heo service restart (a running daemon applies the surfaces on restart; heo service start otherwise)"
	}
	if a.structured() {
		return output.Dispatch(cmd, v, res)
	}
	return writeSetup(cmd.OutOrStdout(), res)
}

// currentSurfaces is each HTTP surface's configured listen address,
// from every configuration source.
func currentSurfaces(v *viper.Viper) map[string]string {
	out := map[string]string{}
	for _, svc := range settings.HTTPSurfaces {
		if addr := v.GetString("services." + svc + ".addr"); addr != "" {
			out[svc] = addr
		}
	}
	return out
}

// enabledSurfaces are the HTTP surfaces the configuration enables.
func enabledSurfaces(v *viper.Viper) []string {
	var out []string
	for _, svc := range settings.HTTPSurfaces {
		if v.GetBool("services." + svc + ".enabled") {
			out = append(out, svc)
		}
	}
	return out
}

// readProfileFile reads the profile file alone, to write only what
// differs from it. A missing file is empty.
func readProfileFile(path string) (*viper.Viper, error) {
	f := viper.New()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	f.SetConfigFile(path)
	f.SetConfigType("yaml")
	if err := f.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return f, nil
}

func wrote(writes []setupWrite, prefix string) bool {
	return slices.ContainsFunc(writes, func(w setupWrite) bool { return strings.HasPrefix(w.key, prefix) })
}

// setupPlan is the --dry-run plan: keys and the token's fate, never
// its value.
func setupPlan(in setupInputs, path string, writes []setupWrite, tok *setupToken) cli.Plan {
	p := cli.Plan{Args: map[string]any{
		"enable": in.enable, "disable": in.disable, "addr": in.addrs,
		"rotate_token": in.rotate, "show_token": in.show,
	}}
	if tok != nil {
		switch tok.Action {
		case tokenMinted:
			p.Effects = append(p.Effects, cli.Effect{Kind: "create", Target: "secret:" + tok.Ref,
				Detail: fmt.Sprintf("mint the surface token (32 random bytes) in the %s store, subject %s", tok.Backend, tok.Subject)})
		case tokenRotated:
			p.Effects = append(p.Effects, cli.Effect{Kind: "update", Target: "secret:" + tok.Ref,
				Detail: fmt.Sprintf("rotate the surface token in the %s store, subject %s; the current one stops working", tok.Backend, tok.Subject)})
		}
	}
	for _, w := range writes {
		p.Effects = append(p.Effects, cli.Effect{Kind: "update", Target: path, Reversible: true,
			Detail: fmt.Sprintf("set %s = %v", w.key, w.value)})
	}
	if len(writes) > 0 {
		p.Warnings = append(p.Warnings, "a running daemon applies the surfaces on restart: heo service restart")
	}
	return p
}
