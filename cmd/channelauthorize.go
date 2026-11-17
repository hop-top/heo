package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/consent"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/credentials"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/cli/cmdmeta"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/storage/secret/oauth"
	"hop.top/kit/go/transport/socket"
	"hop.top/uma/model"
)

// Flags of `channel authorize`.
const (
	flagClientID        = "client-id"
	flagClientSecretRef = "client-secret-ref"
	flagScope           = "scope"
	flagDirectory       = "directory"
	flagLoginHint       = "login-hint"
	flagRefreshRef      = "refresh-ref"
	flagNoBrowser       = "no-browser"
	flagReplace         = "replace"
)

// authorizeInput is what the leaf was given.
type authorizeInput struct {
	Channel         string
	ClientID        string
	ClientSecretRef string
	Scopes          []string
	Directory       string
	LoginHint       string
	RefreshRef      string
	NoBrowser       bool
	Timeout         time.Duration
	Replace         bool
}

// authorizeOut is what `channel authorize` reports: where the grant and
// the refresh token were kept, what was asked of the provider and when
// the first access token expires. It never holds a token.
type authorizeOut struct {
	Channel       string    `json:"channel"                  yaml:"channel"`
	Type          string    `json:"type"                     yaml:"type"`
	Credential    string    `json:"credential_ref"           yaml:"credential_ref"`
	Provider      string    `json:"provider"                 yaml:"provider"`
	ClientID      string    `json:"client_id"                yaml:"client_id"`
	Scopes        []string  `json:"scopes"                   yaml:"scopes"`
	Grant         string    `json:"grant"                    yaml:"grant"`
	Backend       string    `json:"backend"                  yaml:"backend"`
	GrantKey      string    `json:"grant_key"                yaml:"grant_key"`
	RefreshKey    string    `json:"refresh_key"              yaml:"refresh_key"`
	Replaced      string    `json:"replaced,omitempty"       yaml:"replaced,omitempty"`
	Shadowed      []string  `json:"shadowed,omitempty"       yaml:"shadowed,omitempty"`
	AccessExpires time.Time `json:"access_token_expires_at"  yaml:"access_token_expires_at"`
	Notes         []string  `json:"notes,omitempty"          yaml:"notes,omitempty"`
}

func authorizeRows(o authorizeOut) [][2]string {
	rows := [][2]string{
		{"channel", o.Channel},
		{"credential ref", o.Credential},
		{"provider", o.Provider},
		{"client id", o.ClientID},
		{"scopes", strings.Join(o.Scopes, " ")},
		{"grant", o.Grant + " in the " + o.Backend + " backend"},
		{"grant key", o.GrantKey},
		{"refresh token key", o.RefreshKey},
	}
	if o.Replaced != "" {
		rows = append(rows, [2]string{"replaced", "a " + o.Replaced + " entry"})
	}
	if len(o.Shadowed) > 0 {
		rows = append(rows, [2]string{"shadowed", "purposes with an entry of their own keep it: " + strings.Join(o.Shadowed, ", ")})
	}
	if !o.AccessExpires.IsZero() {
		rows = append(rows, [2]string{"access token expires", o.AccessExpires.UTC().Format(time.RFC3339)})
	}
	return rows
}

// authorizeDeps are the parts of the consent that reach outside the
// process: the daemon that knows the channel, the credential backend, the
// provider and the browser. Tests replace each.
type authorizeDeps struct {
	// channel reads the channel as the caller, through the daemon.
	channel func(ctx context.Context, name string) (channelView, error)
	// purposes are the credential purposes the adapter of typ resolves.
	purposes func(typ model.TypeKey) []string
	// backend opens the profile's credential backend.
	backend func(ctx context.Context) (*credentials.Backend, error)
	// run signs the user in.
	run func(ctx context.Context, r consent.Request, ready func(string)) (consent.Outcome, error)
	// open shows url to the user; the URL is printed either way.
	open func(url string) error
	// http sends the token exchange; kit's default when nil.
	http *http.Client
	// provider overrides the provider's endpoints; the real ones when nil.
	provider func(consent.Provider) consent.Provider
	// stderr receives the prompt.
	stderr io.Writer
}

func (a *app) authorizeDeps(cmd *cobra.Command) authorizeDeps {
	return authorizeDeps{
		channel: a.channelThroughDaemon,
		purposes: func(typ model.TypeKey) []string {
			views, err := a.installedAdapters()
			if err != nil {
				return nil
			}
			for _, v := range views {
				if v.Type == typ {
					return v.CredentialPurposes
				}
			}
			return nil
		},
		backend: a.openBackend,
		run:     consent.Run,
		open:    openBrowser,
		stderr:  cmd.ErrOrStderr(),
	}
}

// channelThroughDaemon reads a channel through the control socket as the
// caller: the data store is the daemon's alone, and a channel read is the
// operator's.
func (a *app) channelThroughDaemon(ctx context.Context, name string) (channelView, error) {
	route, _ := control.RouteOf(ctx, a.control.servesControl())
	if route != control.Forward {
		return channelView{}, a.notDaemon()
	}
	res, err := a.controlClient().Invoke(ctx, socket.Request{
		Path: []string{"channel", "get"}, Args: []string{name}, Flags: map[string]any{"format": output.JSON},
	})
	if err != nil {
		return channelView{}, err
	}
	return control.Decode[channelView](res)
}

// openBrowser asks the system to show an https URL in the user's
// browser. It runs the platform's opener with the URL as its one
// argument, never through a shell.
func openBrowser(url string) error {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://127.0.0.1:") {
		return fmt.Errorf("refusing to open %q", url)
	}
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url) // #nosec G204 -- the opener is fixed, the URL is the one argument
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url) // #nosec G204
	default:
		c = exec.Command("xdg-open", url) // #nosec G204
	}
	return c.Start()
}

// authorizePlan is what a consent would do: where the user signs in,
// what is asked, and what is kept where.
func authorizePlan(ch channelView, p consent.Provider, r consent.Request, plan consent.Plan, st storeFacts) *cli.Plan {
	args := map[string]any{
		"channel": ch.Ref.ID, "adapter": string(ch.Type), "provider": p.Name, "client_id": r.ClientID,
		"scopes": p.Scopes, "consent_url": plan.ConsentURL, "redirect_url": plan.RedirectURL,
		"credential_ref": ch.CredentialRef, "grant_key": st.grantKey, "refresh_key": st.refreshKey,
		"backend": st.backend, "confidential_client": r.ClientSecret != "",
		"delivery": "nothing is opened, listened on or sent: a real run listens on 127.0.0.1 only, shows the consent URL (with its own state and PKCE challenge) and exchanges the code for a refresh token",
	}
	detail := fmt.Sprintf("keeps a refresh_token grant under credential reference %s (store key %s) and the refresh token under %s, in the %s backend; "+
		"every access token after that is minted from it", ch.CredentialRef, st.grantKey, st.refreshKey, st.backend)
	if st.replaces != "" {
		detail += "; replaces the " + st.replaces + " entry stored there"
	}
	checked := []string{"channel", "adapter consent flow", "credential backend writable", "client"}
	if r.ClientSecret != "" {
		checked = append(checked, "client secret (found, not shown)")
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "credential:"+ch.CredentialRef, st.replaces == "", detail)},
		PrerequisitesChecked: checked}
}

// storeFacts are what the credential backend says about the reference
// before anything is written.
type storeFacts struct {
	backend, grantKey, refreshKey string
	// replaces is the kind of entry that would be replaced.
	replaces string
}

// authorize runs the consent of channel in.Channel: the plan under
// --dry-run, else the sign-in and the store.
func (a *app) authorize(cmd *cobra.Command, in authorizeInput, d authorizeDeps) error {
	ctx := cmd.Context()
	ch, err := d.channel(ctx, in.Channel)
	if err != nil {
		return err
	}
	if ch.CredentialRef == "" {
		e := output.PrerequisiteError(fmt.Sprintf("channel %s has no credential reference to keep the grant under", channelNameOf(ch)))
		e.SuggestedFix = "heo channel update " + in.Channel + " --credential-ref <ref> --revision " + string(ch.Revision)
		return e
	}
	b, err := d.backend(ctx)
	if err != nil {
		return fmt.Errorf("credential backend: %w", err)
	}
	if err := b.CanKeepGrant(); err != nil {
		return err
	}
	entry, err := b.Entry(ctx, ch.CredentialRef)
	if err != nil {
		return err
	}
	if entry.Exists && (entry.Grant == nil || entry.Grant.Kind != oauth.RefreshToken) && !in.Replace {
		return model.NewError(model.Conflict, fmt.Sprintf(
			"reference %s already holds a %s credential: it would be lost; remove it or pass --replace",
			ch.CredentialRef, orUnreadable(entry.Kind)))
	}
	req, p, err := a.authorizeRequest(ctx, ch, in, entry, b, d)
	if err != nil {
		return err
	}
	refreshKey, err := b.Key(refreshRefOf(ch.CredentialRef, in.RefreshRef))
	if err != nil {
		return err
	}
	if entry.Grant != nil && entry.Grant.SecretRef != "" && in.RefreshRef == "" {
		refreshKey = entry.Grant.SecretRef
	}
	req.SecretRef = refreshKey
	st := storeFacts{backend: b.Name(), grantKey: entry.Key, refreshKey: refreshKey}
	if entry.Exists {
		st.replaces = orUnreadable(entry.Kind)
		if entry.Grant != nil {
			st.replaces += " grant"
		}
	}
	if cli.IsDryRun(cmd) {
		plan, err := consent.Preview(req)
		if err != nil {
			return err
		}
		return cli.RenderPlan(cmd, *authorizePlan(ch, p, req, plan, st))
	}

	say := func(format string, args ...any) { _, _ = fmt.Fprintf(d.stderr, format+"\n", args...) }
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, err := d.run(ctx, req, func(url string) {
		say("Sign in to %s as the mailbox's user at:\n\n  %s\n", p.Name, url)
		if in.NoBrowser {
			return
		}
		if err := d.open(url); err != nil {
			say("(no browser could be opened here: open the address above)")
		}
	})
	if err != nil {
		return err
	}
	defer out.Clear()

	stored, err := b.StoreRefreshGrant(ctx, credentials.GrantStore{
		Reference: ch.CredentialRef, Grant: out.Grant, RefreshSecret: out.RefreshSecret,
		Replace: in.Replace, Purposes: d.purposes(ch.Type),
	})
	if err != nil {
		return err
	}
	res := authorizeOut{
		Channel: channelNameOf(ch), Type: string(ch.Type), Credential: ch.CredentialRef, Provider: p.Name,
		ClientID: out.Grant.ClientID, Scopes: out.Grant.Scopes, Grant: string(oauth.RefreshToken), Backend: b.Name(),
		GrantKey: stored.GrantKey, RefreshKey: stored.RefreshKey, Replaced: stored.Replaced, Shadowed: stored.Shadowed,
		AccessExpires: out.ExpiresAt,
		Notes: []string{"the daemon reads the store on every attempt: nothing to restart",
			"heo channel check " + in.Channel + " --remote mints an access token and shows the grant"},
	}
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, res)
	}
	if err := writeRows(cmd.OutOrStdout(), authorizeRows(res)); err != nil {
		return err
	}
	for _, n := range res.Notes {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), n); err != nil {
			return err
		}
	}
	return nil
}

// authorizeRequest is the consent's request and provider: from the flags,
// else from the grant a reference already holds (a second consent after the
// first expired or was revoked needs none of them again).
func (a *app) authorizeRequest(ctx context.Context, ch channelView, in authorizeInput, entry credentials.Entry,
	b *credentials.Backend, d authorizeDeps,
) (consent.Request, consent.Provider, error) {
	g := oauth.Grant{}
	if entry.Grant != nil && entry.Grant.Kind == oauth.RefreshToken {
		g = *entry.Grant
	}
	clientID := in.ClientID
	if clientID == "" {
		clientID = g.ClientID
	}
	if clientID == "" {
		e := output.UsageError("--" + flagClientID + ": the OAuth client to consent to is required")
		e.SuggestedFix = "create an OAuth client for the mailbox's provider (see docs/manual/channels.md#authorize-a-mailbox) and pass its ID"
		return consent.Request{}, consent.Provider{}, e
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = g.Scopes
	}
	dir := in.Directory
	if dir == "" {
		dir = consent.DirectoryOf(g.TokenURL)
	}
	p, err := consent.ProviderFor(ch.Type, ch.Configuration.Data, consent.Options{Directory: dir, Scopes: scopes, LoginHint: in.LoginHint})
	if err != nil {
		return consent.Request{}, consent.Provider{}, err
	}
	if d.provider != nil {
		p = d.provider(p)
	}
	req := consent.Request{Provider: p, ClientID: clientID, Timeout: in.Timeout, HTTPClient: d.http}
	// The client secret: the one named, else the stored grant's when it is
	// still the same client.
	var secretKey string
	switch {
	case in.ClientSecretRef != "":
		if secretKey, err = b.Key(in.ClientSecretRef); err != nil {
			return consent.Request{}, consent.Provider{}, err
		}
	case in.ClientID == "" || in.ClientID == g.ClientID:
		secretKey = g.ClientSecretRef
	}
	if secretKey != "" {
		v, err := b.SecretAt(ctx, secretKey)
		if err != nil {
			return consent.Request{}, consent.Provider{}, fmt.Errorf("client secret: %w", err)
		}
		req.ClientSecret, req.ClientSecretRef = string(v), secretKey
		clear(v)
	}
	return req, p, nil
}

// refreshRefOf is the reference the refresh token is kept under: the one
// the caller named, else the channel's credential reference with
// ".refresh_token" appended.
func refreshRefOf(credentialRef, named string) string {
	if named != "" {
		return named
	}
	return credentialRef + ".refresh_token"
}

func orUnreadable(kind string) string {
	if kind == "" {
		return "unreadable"
	}
	return kind
}

// channelNameOf is how a channel is named for a person: its key, else its
// ID.
func channelNameOf(ch channelView) string {
	if ch.Key != "" {
		return ch.Key
	}
	return string(ch.Ref.ID)
}

func (a *app) channelAuthorizeCmd() *cobra.Command {
	var in authorizeInput
	c := &cobra.Command{
		Use:   "authorize <channel>",
		Short: "Run a mailbox's first consent and keep its refresh token",
		Long: `Sign the mailbox's user in once, with the provider's own consent screen,
and keep the refresh token the provider answers with as the grant of the
channel's credential reference. From then on heo mints and refreshes
every access token itself ([configuration.md](configuration.md), OAuth grants):
this runs once per mailbox, and again only if the provider stops honouring
the grant (the user revoked it, or Google's testing status expired it).

It is the authorization code flow with PKCE: heo listens on 127.0.0.1 on an
ephemeral port for the one redirect, prints the consent URL and opens it in
a browser (--no-browser leaves that to you). The URL carries its own state
and code challenge, no other local request can start or finish the login,
and nothing is kept unless the provider answers with a refresh token.

Gmail (google.mail) asks for offline access and the scopes the adapter
needs; Microsoft Graph (microsoft.mail) asks for its delegated permissions
and offline_access, in the Entra directory --directory names (common by
default). --client-id is the OAuth client you registered with the provider;
--client-secret-ref names a credential reference that holds its secret, for a
client that has one (a Google desktop client does). Run again after a revoked
or expired grant and the client, scopes and secret are taken from the grant
already stored: no flag is needed.

The grant is stored under the channel's credential reference
(` + "`channel get`" + ` shows it), the refresh token under <ref>.refresh_token (--refresh-ref
changes it), in the profile's credential backend, which must be writable:
keyring, file or openbao, not an aps-bound profile or agefile. A reference that
already holds something else, a static token or a password, is refused
unless --replace says it may be lost; a refresh_token grant is replaced, that
is what a second consent is. A purpose with an entry of its own keeps it, and the
output names it. Nothing but the grant's key and the expiry of the first
access token is printed: never a token.

--dry-run prints the scopes, the client, where it would store and the consent
URL, with the values minted per run left as <state>, <challenge> and
<port>, and opens nothing. This command needs a person at a terminal: it
is not served over REST, gRPC or MCP, and a plugin may not run it. The
operator role reads the channel through the running daemon. Run it on
the machine whose browser the user signs in with.`,
		Example: `  heo channel authorize gmail --client-id 1234.apps.googleusercontent.com --client-secret-ref gmail-client --dry-run
  heo channel authorize gmail --client-id 1234.apps.googleusercontent.com --client-secret-ref gmail-client
  heo channel authorize outlook --client-id 6a1b0c9d-0000-4000-8000-000000000000 --directory organizations
  heo channel authorize gmail --no-browser`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Channel = args[0]
			if in.Timeout <= 0 {
				return output.UsageError("--" + flagTimeout + ": want a positive duration")
			}
			return a.authorize(cmd, in, a.authorizeDeps(cmd))
		},
	}
	f := c.Flags()
	f.StringVar(&in.ClientID, flagClientID, "", "OAuth client ID registered with the provider (default: the client of the grant already stored)")
	f.StringVar(&in.ClientSecretRef, flagClientSecretRef, "", "Credential reference that holds the client's secret, for a confidential client")
	f.StringSliceVar(&in.Scopes, flagScope, nil, "Scopes to ask for, instead of the adapter's own (repeat or comma-separate)")
	f.StringVar(&in.Directory, flagDirectory, "", "Microsoft Entra directory to sign in to: common (default), organizations, consumers, a tenant ID or a domain")
	f.StringVar(&in.LoginHint, flagLoginHint, "", "Mailbox address to prefill on the sign-in page (default: the channel's own)")
	f.StringVar(&in.RefreshRef, flagRefreshRef, "", "Credential reference to keep the refresh token under (default: <credential ref>.refresh_token)")
	f.BoolVar(&in.NoBrowser, flagNoBrowser, false, "Print the consent URL and do not open a browser")
	f.DurationVar(&in.Timeout, flagTimeout, consent.DefaultTimeout, "How long to wait for the sign-in to complete")
	f.BoolVar(&in.Replace, flagReplace, false, "Replace a credential of another kind held by the reference")
	cli.SetSideEffect(c, cli.SideEffectWrite)
	// A second consent replaces the grant and the token under the same
	// keys: the stored state is the same however often it runs, but each
	// run asks the provider anew.
	cli.SetIdempotency(c, cli.IdempotencyNo)
	// Interactive: a person's browser, a local listener. Never served.
	c.Annotations[cmdmeta.KeySelfHosting] = "true"
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &authorizeOut{}, Version: "1.0"})
	return c
}
