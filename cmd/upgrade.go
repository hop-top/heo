package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/errmap"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/version"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/core/netpolicy"
	"hop.top/kit/go/core/upgrade"
	"hop.top/kit/go/core/xdg"
)

// Upgrade settings: where releases are looked up.
const (
	// keyUpgradeGitHub is the owner/repo whose latest GitHub release
	// heo upgrades to.
	keyUpgradeGitHub = "upgrade.github_repo"
	// keyUpgradeReleaseURL replaces the GitHub lookup with a feed
	// answering {"version", "url", "notes"}, for mirrors.
	keyUpgradeReleaseURL = "upgrade.release_url"
	// keyUpgradeChecksumURL is the checksums file a custom feed's
	// downloads are verified against; required with it.
	keyUpgradeChecksumURL = "upgrade.checksum_url"
)

// upgradeDefaults are the built-in upgrade settings.
func upgradeDefaults() map[string]any {
	return map[string]any{
		keyUpgradeGitHub:      "hop-top/heo",
		keyUpgradeReleaseURL:  "",
		keyUpgradeChecksumURL: "",
	}
}

// socketProbeTimeout bounds one control socket probe.
const socketProbeTimeout = 500 * time.Millisecond

// upgradeResult is what `heo upgrade` reports.
type upgradeResult struct {
	Current  string `json:"current"        yaml:"current"`
	Latest   string `json:"latest"         yaml:"latest"`
	Upgraded bool   `json:"upgraded"       yaml:"upgraded"`
	Path     string `json:"path,omitempty" yaml:"path,omitempty"`
	Source   string `json:"source"         yaml:"source"`
}

func (a *app) upgradeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Replace this heo binary with the latest release",
		Long: `Replace the running heo binary with the latest release: self-update
only. Data stores are not touched; migrate them with heo storage
migrate after upgrading.

Releases are looked up on GitHub (upgrade.github_repo, default
hop-top/heo), or at upgrade.release_url, a feed answering
{"version", "url", "notes"}, with upgrade.checksum_url. The release
archive for this platform is downloaded next to the binary, verified
against the release's SHA-256 checksums file, then swapped in. Only a
newer version is installed; heo never downgrades itself.

Refused (exit 4) while a heo daemon runs from this binary, for any
profile: stop it with heo service stop first, start it again after.
A binary installed by Homebrew is upgraded with brew instead (exit 2).
No release published: exit 3. Release feed unreachable: exit 6.
--offline: exit 2, nothing is fetched. --dry-run checks for a release
and prints the plan without downloading.`,
		Example: `  heo upgrade
  heo upgrade --dry-run
  heo upgrade --format json`,
		Args: cobra.NoArgs,
		RunE: a.runUpgrade,
	}
	cli.SetTopLevelVerb(c)
	cli.SetSideEffect(c, cli.SideEffectWriteLocal)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	// It replaces the binary a daemon runs from: never over a served
	// surface.
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[cmdmeta.KeySelfHosting] = "true"
	return c
}

func (a *app) runUpgrade(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if live := a.liveSockets(); len(live) > 0 {
		e := output.ConflictError("a heo daemon is running (control socket " +
			strings.Join(live, ", ") + "); heo upgrade replaces the binary it runs from")
		e.SuggestedFix = "stop it with `heo service stop` (with --profile for another profile), upgrade, then start it again"
		return e
	}
	self, err := executable()
	if err != nil {
		return errmap.Of(err)
	}
	if managedByHomebrew(self) {
		e := &output.Error{Code: "UNSUPPORTED", ExitCode: output.ExitUsage, Transience: output.TransiencePermanent,
			Message: "heo at " + self + " was installed by Homebrew"}
		e.SuggestedFix = "upgrade it with `brew upgrade heo`"
		return e
	}
	checker, source, err := a.upgradeChecker()
	if err != nil {
		return err
	}
	r := checker.Check(ctx)
	if r.Err != nil {
		return upgradeError(r.Err, source)
	}
	res := upgradeResult{Current: version.Number(), Latest: r.Latest, Path: self, Source: source}
	if r.UpdateAvail && r.URL == "" {
		e := output.NotFoundError(fmt.Sprintf("heo %s has no release archive for %s/%s", r.Latest, runtime.GOOS, runtime.GOARCH))
		e.SuggestedFix = "build it from source for this platform"
		return e
	}
	if r.UpdateAvail && cli.IsDryRun(cmd) {
		return cli.RenderPlan(cmd, cli.Plan{
			Effects: []cli.Effect{{Kind: "replace", Target: self, Reversible: false,
				Detail: fmt.Sprintf("heo %s → %s from %s, verified against its sha256 checksums", res.Current, r.Latest, r.URL)}},
			PrerequisitesChecked: []string{"no-daemon", "release"},
		})
	}
	if r.UpdateAvail {
		if err := checker.Upgrade(ctx); err != nil {
			return upgradeError(err, source)
		}
		res.Upgraded = true
	}
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, res)
	}
	w := cmd.OutOrStdout()
	if !res.Upgraded {
		_, err := fmt.Fprintf(w, "heo is up to date (%s; latest release %s)\n", res.Current, res.Latest)
		return err
	}
	_, err = fmt.Fprintf(w, "upgraded heo %s → %s at %s\n", res.Current, res.Latest, res.Path)
	return err
}

// upgradeChecker builds kit's checker from the upgrade settings. Every
// run checks afresh: no cached answer.
func (a *app) upgradeChecker() (*upgrade.Checker, string, error) {
	v := a.r.Viper
	opts := []upgrade.Option{
		upgrade.WithBinary(settings.Tool, version.Number()),
		upgrade.WithCacheTTL(0),
	}
	var source string
	if feed := v.GetString(keyUpgradeReleaseURL); feed != "" {
		sums := v.GetString(keyUpgradeChecksumURL)
		if sums == "" {
			e := output.UsageError(keyUpgradeReleaseURL + " is set without " + keyUpgradeChecksumURL +
				": every download is verified against a checksums file")
			e.SuggestedFix = "set " + keyUpgradeChecksumURL + " to the feed's checksums file"
			return nil, "", e
		}
		opts = append(opts, upgrade.WithReleaseURL(feed), upgrade.WithChecksumURL(sums))
		source = feed
	} else {
		repo := v.GetString(keyUpgradeGitHub)
		if repo == "" {
			return nil, "", output.UsageError("no release source: set " + keyUpgradeGitHub + " or " + keyUpgradeReleaseURL)
		}
		opts = append(opts, upgrade.WithGitHub(repo))
		source = "github.com/" + repo
	}
	return upgrade.New(opts...), source, nil
}

// statusRe finds the HTTP status kit's release lookup reports.
var statusRe = regexp.MustCompile(`returned (\d{3})`)

// upgradeError classifies a failed release lookup or install. Nothing
// is replaced unless the final swap succeeded, so every failure leaves
// the installed binary as it was.
func upgradeError(err error, source string) *output.Error {
	if errors.Is(err, netpolicy.ErrOffline) {
		e := output.UsageError("heo upgrade needs the network to reach " + source + ", and --offline is set")
		e.SuggestedFix = "rerun without --offline"
		return e
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		e := &output.Error{Code: "UNAVAILABLE", ExitCode: output.ExitTransient, Transience: output.TransienceTransient,
			Message: "cannot reach the release feed " + source + ": " + ue.Err.Error()}
		e.SuggestedFix = "check the network and retry, or set " + keyUpgradeReleaseURL + " to a reachable mirror"
		return e
	}
	if m := statusRe.FindStringSubmatch(err.Error()); m != nil {
		code, _ := strconv.Atoi(m[1])
		switch {
		case code == 404:
			e := output.NotFoundError("no heo release is published at " + source)
			e.SuggestedFix = "nothing to upgrade to yet; the installed binary is unchanged"
			return e
		case code >= 500 || code == 429:
			e := &output.Error{Code: "UNAVAILABLE", ExitCode: output.ExitTransient, Transience: output.TransienceTransient,
				Message: "the release feed " + source + " failed: " + err.Error()}
			e.SuggestedFix = "retry later"
			return e
		}
	}
	return &output.Error{Code: output.CodeGeneric, ExitCode: output.ExitGeneric, Transience: output.TransienceUnknown,
		Message: "upgrade failed; the installed binary is unchanged: " + err.Error()}
}

// liveSockets returns the control sockets a daemon answers on: the
// selected profile's configured one and every profile's default one
// under the runtime directory. A socket file nobody listens on is
// stale, not a daemon.
func (a *app) liveSockets() []string {
	paths := map[string]bool{}
	if p := a.r.Viper.GetString(settings.KeySocketPath); p != "" {
		paths[p] = true
	}
	if dir, err := xdg.RuntimeDir(settings.Tool); err == nil {
		matches, _ := filepath.Glob(filepath.Join(dir, "*", "heo.sock"))
		for _, m := range matches {
			paths[m] = true
		}
	}
	var live []string
	for p := range paths {
		c, err := net.DialTimeout("unix", p, socketProbeTimeout)
		if err != nil {
			continue
		}
		_ = c.Close()
		live = append(live, p)
	}
	sort.Strings(live)
	return live
}

// executable is the running binary's resolved path, the one an upgrade
// replaces.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// managedByHomebrew reports a binary inside a Homebrew cellar or
// caskroom: brew owns its upgrades.
func managedByHomebrew(path string) bool {
	slash := filepath.ToSlash(path)
	return strings.Contains(slash, "/Cellar/") || strings.Contains(slash, "/Caskroom/")
}
