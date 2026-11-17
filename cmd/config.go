package cmd

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli"
	kitcfg "hop.top/kit/go/console/cli/config"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/core/config"
)

// Scopes `config set` can write.
const (
	scopeProfile = "profile"
	scopeUser    = "user"
)

// keyRe bounds a dotted config key: lower-case segments of letters,
// digits, '_' and '-'.
var keyRe = regexp.MustCompile(`^[a-z0-9_-]+(\.[a-z0-9_-]+)*$`)

func (a *app) configCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "config",
		Short: "Inspect and change heo configuration",
		Long: `Inspect and change heo's own configuration for the selected profile.

Values resolve from built-in defaults, /etc/heo/config.yaml, the user
config file, the profile file (profiles/<profile>.yaml next to it), -c
files, HEO_<KEY> environment variables, flags and -c key=value, highest
last. A repository's kit.yaml never configures heo.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.configGetCmd(), a.configSetCmd(), a.configListCmd(), a.configValidateCmd())
	kitcfg.RegisterPathSubcommands(grp, "heo", kitcfg.WithResolver(a.chain))
	for _, c := range grp.Commands() {
		if c.Name() == "path" || c.Name() == "paths" {
			allowMissingProfile(c)
		}
	}
	return grp
}

func allowMissingProfile(c *cobra.Command) {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[annotAllowMissingProfile] = "true"
}

// chain feeds kit's `config path`/`paths` heo's own resolution chain.
// It ignores the directory: heo's configuration never depends on it.
func (a *app) chain(string) []kitcfg.ResolvedPath {
	if a.state == nil {
		return nil
	}
	var out []kitcfg.ResolvedPath
	for _, l := range a.state.Chain() {
		rp := kitcfg.ResolvedPath{Path: l.Path, Source: string(l.Origin), Exists: l.Exists}
		if l.Origin == settings.OriginProfile {
			rp.Scope = a.state.Profile().String()
		}
		out = append(out, rp)
	}
	return out
}

func (a *app) configGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <key>",
		Short: "Print one effective configuration value",
		Long: `Print the effective value of <key> for the selected profile.

A leaf key prints its bare value; pass --format to get the value with
its origin (the layer that set it) and detail (file, variable or flag).
A section key such as services.socket lists every value below it.
Credential material is redacted. An unknown key exits 3.`,
		Example: `  heo config get services.socket.path
  heo config get runtime.tenant --format json
  heo --profile aps:noor config get services.socket`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.ToLower(args[0])
			entries := a.state.Lookup(key)
			if len(entries) == 0 {
				e := output.NotFoundError(fmt.Sprintf("no configuration value for %q", key))
				e.SuggestedFix = "list known keys with `heo config list`"
				return e
			}
			if len(entries) == 1 && entries[0].Key == key {
				if !formatChanged(cmd) {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), entries[0].Value)
					return err
				}
				return output.Dispatch(cmd, a.r.Viper, entries[0])
			}
			return output.Dispatch(cmd, a.r.Viper, entries)
		},
	}
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	return c
}

func (a *app) configListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List effective configuration with origins",
		Long: `List every effective configuration value heo knows for the selected
profile, sorted by key, with its origin (default, system, user,
profile, file, env, flag or override) and detail (the file path,
environment variable or flag). Credential material is redacted.`,
		Example: `  heo config list
  heo --profile work config list --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return output.Dispatch(cmd, a.r.Viper, a.state.Entries())
		},
	}
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	return c
}

func (a *app) configSetCmd() *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Write one configuration value",
		Long: `Write <key> = <value> into the selected profile's file, or with
--scope user into the user file shared by every profile. The file and
its directory are created when missing; comments and other keys are
kept. Numbers, true/false and null are written typed; anything else
as a string.

The profile is never a configuration key, and a value that matches a
known credential pattern is refused: store credentials in the secret
store and reference them. Takes effect on the next start; a running
daemon is not reloaded. --dry-run prints the change without writing.`,
		Example: `  heo config set services.socket.path /tmp/heo-work.sock
  heo --profile work config set runtime.tenant acme
  heo config set --scope user services.all.rate_limit.enabled false`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := strings.ToLower(args[0]), args[1]
			if err := checkSettable(key, value); err != nil {
				return err
			}
			path, err := a.scopePath(scope)
			if err != nil {
				return err
			}
			if cli.IsDryRun(cmd) {
				return cli.RenderPlan(cmd, cli.Plan{
					Args:    map[string]any{"key": key, "scope": scope},
					Effects: []cli.Effect{{Kind: "update", Target: path, Reversible: true, Detail: "set " + key}},
				})
			}
			if err := config.SetValue(key, config.ParseScalar(value), config.ScopeUser,
				config.Options{UserConfigPath: path}); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			origin := settings.OriginProfile
			if scope == scopeUser {
				origin = settings.OriginUser
			}
			return output.Dispatch(cmd, a.r.Viper, settings.Entry{
				Key: key, Value: settings.Display(key, config.ParseScalar(value)), Origin: origin, Detail: path,
			})
		},
	}
	c.Flags().StringVar(&scope, "scope", scopeProfile, "File to write: profile (the selected profile's file) or user (shared by every profile)")
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	allowMissingProfile(c)
	return c
}

// checkSettable refuses what `config set` must never write.
func checkSettable(key, value string) error {
	switch {
	case !keyRe.MatchString(key):
		return output.UsageError(fmt.Sprintf("invalid key %q: dotted lower-case segments of letters, digits, '_' and '-'", key))
	case key == settings.KeyProfile:
		e := output.UsageError("profile is not a configuration key")
		e.SuggestedFix = "select the profile with --profile <ref> or HEO_PROFILE"
		return e
	case settings.LooksLikeSecret(value):
		e := output.UsageError(fmt.Sprintf("refusing to write %s: the value looks like a credential", key))
		e.SuggestedFix = "store the credential in the secret store and set a reference to it instead"
		return e
	}
	return nil
}

func (a *app) scopePath(scope string) (string, error) {
	switch scope {
	case scopeProfile:
		return a.state.ProfilePath(), nil
	case scopeUser:
		return a.state.UserPath(), nil
	}
	return "", output.UsageError(fmt.Sprintf("invalid --scope %q: want %s or %s", scope, scopeProfile, scopeUser))
}

// validation is the result `config validate` reports.
type validation struct {
	Profile string           `json:"profile" yaml:"profile"`
	Valid   bool             `json:"valid"   yaml:"valid"`
	Issues  []settings.Issue `json:"issues"  yaml:"issues"`
	// Channels says whether the channels and inboxes in the data store
	// were checked, and why not.
	Channels channelsValidation `json:"channels" yaml:"channels"`
}

func (a *app) configValidateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate",
		Short: "Check the effective configuration",
		Long: `Check the selected profile's effective configuration without side
effects: nothing is written, nothing binds and no daemon is needed.

Checks the tenant against uma's principal rules, the control socket
path against the unix socket length limit, that no TCP surface (api,
rpc, mcp) is enabled without an authenticator, the credential backend
(credentials.backend: keyring, file, agefile or openbao) and its
settings, that no credential material sits under credentials.*, that
storage.driver names a driver this binary has and that driver's own
settings (for postgres: the DSN parses and holds no password), the
runtime services' settings (worker count and durations; a webhook
address on loopback unless services.webhook.allow_remote), the
smtp.mail listener (loopback unless adapters.smtp.allow_remote, and
then with STARTTLS unless adapters.smtp.allow_remote_without_tls; a
configured certificate is read and its key resolved in the credential
backend: the pair must match and be in date), and warns where a
setting is weaker than heo's default.

Channels and inboxes live in the profile's data store, which the daemon
owns: with a daemon running, they are checked through it, as
` + "`channel check`" + ` checks them (adapter validation, provider accounts,
inbox selection, destinations, note delivery; no network), and each finding is
an issue with origin "store". With none running they are not checked,
and the result says so (channels.skipped); nothing opens the store
behind the daemon's back.

Errors exit 2; warnings alone exit 0.`,
		Example: `  heo config validate
  heo --profile aps:noor config validate --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			issues := settings.Validate(a.r.Viper, a.state, a.storageCheck, a.runtimeCheck)
			stored, chans := a.validateChannels(cmd)
			issues = append(issues, stored...)
			res := validation{Profile: a.state.Profile().String(), Valid: !settings.HasErrors(issues), Issues: issues, Channels: chans}
			if res.Issues == nil {
				res.Issues = []settings.Issue{}
			}
			if err := a.renderValidation(cmd, res); err != nil {
				return err
			}
			if !res.Valid {
				e := output.UsageError(fmt.Sprintf("configuration for %s is invalid", res.Profile))
				e.Code = "VALIDATION"
				e.SuggestedFix = "fix the keys reported above with `heo config set` or in the file named in DETAIL"
				return e
			}
			return nil
		},
	}
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	return c
}

// renderValidation prints the whole result under json/yaml and the
// issue table otherwise, with a one-line verdict on stderr.
func (a *app) renderValidation(cmd *cobra.Command, res validation) error {
	switch a.r.Viper.GetString("format") {
	case "json", "yaml":
		return output.Dispatch(cmd, a.r.Viper, res)
	}
	if len(res.Issues) > 0 {
		if err := output.Dispatch(cmd, a.r.Viper, res.Issues); err != nil {
			return err
		}
	}
	w := cmd.ErrOrStderr()
	if res.Channels.Checked {
		_, _ = fmt.Fprintf(w, "channels: %d checked through the daemon\n", res.Channels.Count)
	} else {
		_, _ = fmt.Fprintf(w, "channels: not checked: %s\n", res.Channels.Skipped)
	}
	if res.Valid {
		_, err := fmt.Fprintf(w, "configuration for %s is valid\n", res.Profile)
		return err
	}
	return nil
}

func formatChanged(cmd *cobra.Command) bool {
	f := cmd.Flags().Lookup("format")
	return f != nil && f.Changed
}
