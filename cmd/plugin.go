package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/credentials"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/output"
)

// pluginTokenResult is what `plugin token mint` and `revoke` report.
// The token is in it only on mint, once.
type pluginTokenResult struct {
	Plugin string `json:"plugin" yaml:"plugin"`
	// Actor is who a bearer of the token acts as.
	Actor string `json:"actor" yaml:"actor"`
	// Action is "minted" or "revoked".
	Action string `json:"action" yaml:"action"`
	// Token is the bearer token, printed on mint only.
	Token string `json:"token,omitempty" yaml:"token,omitempty"`
}

// pluginCmd builds the `plugin` group.
func (a *app) pluginCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "plugin",
		Short: "Manage the actors of hook plugins",
		Long: `A hook plugin acts as its own actor, plugin:<name>, holding only the
roles authorization.grants gives it. The control socket never admits it:
it authenticates on REST, gRPC and MCP by its own bearer token.`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(grp)
	tok := &cobra.Command{
		Use:   "token",
		Short: "Mint and revoke a plugin's bearer token",
		Args:  cobra.NoArgs,
	}
	cli.SetHierarchical(tok)
	tok.AddCommand(a.pluginTokenMintCmd(), a.pluginTokenRevokeCmd())
	grp.AddCommand(tok)
	return grp
}

func (a *app) pluginTokenMintCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mint <name>",
		Short: "Mint a plugin's token, replacing any earlier one",
		Long: `Mint the bearer token of the plugin called <name> (lowercase letters,
digits and '-') into the profile's credential backend, and print it once
on stdout. A request bearing it acts as plugin:<name> and as no other
actor, on the HTTP surfaces. The control socket never admits that actor.
Minting again replaces the token: the old one is refused from the next
request on. The token is not stored anywhere else; keep it.

The plugin holds no role until authorization.grants names plugin:<name>.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := a.openBackend(cmd.Context())
			if err != nil {
				return fmt.Errorf("credential backend: %w", err)
			}
			tok, err := credentials.MintPluginToken(cmd.Context(), b.SurfaceTokens(), a.state.Profile(), args[0], time.Now())
			if err != nil {
				return output.UsageError(err.Error())
			}
			res := pluginTokenResult{Plugin: args[0], Actor: string(credentials.PluginActor(args[0])), Action: "minted", Token: tok}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), tok)
			return err
		},
	}
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyNo)
	// Never invocable on a served surface: a plugin must not mint tokens.
	c.Annotations[cmdmeta.KeySelfHosting] = "true"
	setOutputSchema(c, &pluginTokenResult{}, "1.0")
	_ = cli.SetExamples(c, []cli.Example{
		{Title: "Mint a token for the triage plugin", Command: "heo plugin token mint triage"},
	})
	return c
}

func (a *app) pluginTokenRevokeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke a plugin's token",
		Long: `Delete the token of the plugin called <name>. It is refused from the
next request on; the daemon reads the store on every request, so no
restart is needed. Revoking a plugin with no token succeeds.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := a.openBackend(cmd.Context())
			if err != nil {
				return fmt.Errorf("credential backend: %w", err)
			}
			if err := credentials.RevokePluginToken(cmd.Context(), b.SurfaceTokens(), a.state.Profile(), args[0]); err != nil {
				return output.UsageError(err.Error())
			}
			res := pluginTokenResult{Plugin: args[0], Actor: string(credentials.PluginActor(args[0])), Action: "revoked"}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked the token of %s\n", res.Actor)
			return err
		},
	}
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	c.Annotations[cmdmeta.KeySelfHosting] = "true"
	setOutputSchema(c, &pluginTokenResult{}, "1.0")
	_ = cli.SetExamples(c, []cli.Example{
		{Title: "Revoke the triage plugin's token", Command: "heo plugin token revoke triage"},
	})
	return c
}
