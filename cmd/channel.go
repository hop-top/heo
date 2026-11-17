package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags of channel create and update.
const (
	flagType          = "type"
	flagKey           = "key"
	flagConfiguration = "configuration"
	flagCredentialRef = "credential-ref" // #nosec G101 -- a flag name, not a credential
	flagRemote        = "remote"
)

// codeCheckFailed is the error code of a check that found errors: the
// same class as `config validate` finding invalid configuration.
const codeCheckFailed = "VALIDATION"

// channelRow is a channel in a listing table.
type channelRow struct {
	ID            string `json:"id"             yaml:"id"             table:"ID,id"`
	Key           string `json:"key"            yaml:"key"            table:"KEY"`
	Type          string `json:"type"           yaml:"type"           table:"TYPE"`
	Revision      string `json:"revision"       yaml:"revision"       table:"REVISION"`
	CredentialRef string `json:"credential_ref" yaml:"credential_ref" table:"CREDENTIAL REF"`
}

// channelOut is what a channel write reports: the channel as stored
// after it, the operation that wrote it and the key it ran under.
type channelOut struct {
	Channel      model.Channel `json:"channel"         yaml:"channel"`
	Operation    operationView `json:"operation"       yaml:"operation"`
	OperationKey string        `json:"operation_key"   yaml:"operation_key"`
	Notes        []string      `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// channelChecks is what `channel check` reports.
type channelChecks struct {
	Remote bool                    `json:"remote" yaml:"remote"`
	OK     bool                    `json:"ok"     yaml:"ok"`
	Checks []channels.ChannelCheck `json:"checks" yaml:"checks"`
}

func (a *app) channelCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "channel",
		Short: "Configure and check the profile's channels",
		Long: `Configure and check channels: each one a configured connection to a
provider (a mailbox, a phone number, a web widget), run by an adapter
(` + "`heo adapter list`" + `). Inboxes bind a channel's destinations
(` + "`heo inbox`" + `).

Every command but ` + "`authorize`" + ` runs in the profile's daemon, which owns the
data store; with none running they exit 70. ` + "`authorize`" + ` runs at your
terminal, reads the channel through the daemon and keeps a mailbox's
refresh token in the credential backend. Reading and configuring channels is
the operator role's (authorization.grants). Writes name the revision
they replace (--revision, from ` + "`channel get`" + `): a stale one is a
conflict, exit 4. --dry-run previews the exact request and writes
nothing. Credentials are referenced (--credential-ref), never passed.

The running daemon applies each committed change at once: a new
channel starts receiving and gets its webhook route, an updated one has
its receiver and route replaced, and every other channel keeps running
untouched.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.channelListCmd(), a.channelGetCmd(), a.channelCreateCmd(), a.channelUpdateCmd(), a.channelCheckCmd(), a.channelAuthorizeCmd())
	return grp
}

func (a *app) channelListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the profile's channels",
		Long: `List the profile's channels, a page at a time (--limit, --cursor with
the previous page's next). Read-only; the operator role reads channels.`,
		Example: `  heo channel list
  heo channel list --format json --limit 10`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Channel], error) {
				var out page[model.Channel]
				err := a.configure(ctx, c, model.ActionChannelRead, func(s channels.Service) error {
					p, err := s.ListChannels(ctx, pg)
					out = pageFrom(p)
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]channelRow, 0, len(res.Items))
			for _, ch := range res.Items {
				rows = append(rows, channelRow{
					ID: string(ch.Ref.ID), Key: orDash(ch.Key), Type: string(ch.Type),
					Revision: string(ch.Revision), CredentialRef: orDash(ch.CredentialRef),
				})
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[model.Channel]{}, "1.0")
	return c
}

// renderPage prints a listing's rows, and where the next page starts
// on stderr.
func renderPage[R any](a *app, cmd *cobra.Command, rows []R, next model.Cursor, complete bool) error {
	if err := output.Dispatch(cmd, a.r.Viper, rows); err != nil {
		return err
	}
	if !complete && next != "" {
		_, err := fmt.Fprintf(cmd.ErrOrStderr(), "more: --cursor %s\n", next)
		return err
	}
	return nil
}

// channelView is a channel with the provider-dependent verbs its
// adapter declares: computed from the binary and the record alone, with
// no adapter instance opened and no provider reached.
type channelView struct {
	model.Channel
	Verbs []adapters.VerbState `json:"verbs" yaml:"verbs"`
	// ReplyRecipients is whether the adapter declares reply_recipients:
	// the flags --reply-all, --cc and --bcc of `message reply` and
	// `message reply-draft`.
	ReplyRecipients adapters.VerbState `json:"reply_recipients" yaml:"reply_recipients"`
	// RequiredPermissions are what the adapter's descriptor lists, once
	// for the channel: always present, empty when none.
	RequiredPermissions []string `json:"required_permissions" yaml:"required_permissions"`
}

func (a *app) channelGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <channel>",
		Short: "Show one channel and the provider verbs it supports",
		Long: `Show the channel <channel>, by key or ID: its adapter type, its
configuration, its credential reference and its revision (what
` + "`channel update --revision`" + ` takes), and verbs: for each
provider-dependent verb (` + "`message edit`, `message delete`, `reaction add`, `reaction remove`, `participant add`, `participant remove`, `receipt send`, `history fetch`, `interaction respond`, `interaction open-modal`, `interaction push-modal`, `interaction update-modal`, `issue transition`, `issue update`, `ticket transition`, `conversation tag`, `message reply-draft`, `conversation unsubscribe`, `conversation archive`, `conversation unarchive`, `conversation move`, `conversation read`, `conversation unread`, `conversation flag`, `conversation unflag`, `conversation trash`, `conversation untrash`" + `)
the capability it needs and whether this channel's adapter declares it
(state declared or unsupported); and reply_recipients, which
` + "`message reply`" + ` and ` + "`message reply-draft`" + ` need only for --reply-all, --cc
and --bcc. An unsupported verb says why:
adapter_missing (the adapter is not in this build), not_declared (the
adapter lacks the capability), not_executed (uma runs no operation for
it) or config (the channel's own configuration switches it off, such as
github.issues without replies).

The verbs are computed from the binary and the channel record alone: no
adapter is opened and no provider is reached, so they say what the
channel is wired for, not what its provider account may do right now.
` + "`channel check --remote`" + ` reports that, live. Read-only. No such channel
exits 3.`,
		Example: `  heo channel get support-mail
  heo channel get support-mail --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (channelView, error) {
				var out channelView
				err := a.configure(ctx, c, model.ActionChannelRead, func(s channels.Service) (err error) {
					out.Channel, err = s.FindChannel(ctx, args[0])
					if err == nil {
						out.Verbs = s.Adapters.VerbStates(out.Channel)
						out.ReplyRecipients = s.Adapters.RecipientsState(out.Channel)
						out.RequiredPermissions = s.Adapters.RequiredPermissions(out.Channel)
					}
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return writeRows(cmd.OutOrStdout(), channelViewRows(res))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &channelView{}, "1.0")
	return c
}

func channelRows(ch model.Channel) [][2]string {
	return [][2]string{
		{"id", string(ch.Ref.ID)},
		{"key", orDash(ch.Key)},
		{"type", string(ch.Type)},
		{"revision", string(ch.Revision)},
		{"credential ref", orDash(ch.CredentialRef)},
		{"configuration", fmt.Sprintf("%s/v%d %s", ch.Configuration.Name, ch.Configuration.Version, ch.Configuration.Data)},
	}
}

// channelViewRows is a channel's fields and, after them, a row per
// provider-dependent verb: its state, its capability, and why not.
func channelViewRows(v channelView) [][2]string {
	rows := channelRows(v.Channel)
	for _, vs := range v.Verbs {
		rows = append(rows, [2]string{"verb " + vs.Verb, verbStateText(vs)})
	}
	return append(rows, [2]string{"verb " + v.ReplyRecipients.Verb, verbStateText(v.ReplyRecipients)})
}

// verbStateText is a verb's state, its capability and why not.
func verbStateText(vs adapters.VerbState) string {
	state := vs.State + " (" + string(vs.Capability) + ")"
	if vs.Reason != "" {
		state = vs.State + ": " + vs.Reason + " (" + string(vs.Capability) + ")"
		if vs.Detail != "" {
			state += ", " + vs.Detail
		}
	}
	return state
}

// addChannelFlags adds the fields channel create and update set.
func addChannelFlags(c *cobra.Command) {
	c.Flags().String(flagKey, "", "Name for the channel, unique in the profile (what commands take in place of its ID)")
	c.Flags().String(flagConfiguration, "", "Adapter configuration as a JSON object; `heo adapter get <type>` names its schema")
	c.Flags().String(flagCredentialRef, "", "Reference of the credential in the profile's credential backend (never the credential)")
}

// channelChange reads the channel fields set on the command line.
func channelChange(cmd *cobra.Command) channels.ChannelChange {
	var ch channels.ChannelChange
	if v, err := cmd.Flags().GetString(flagType); err == nil {
		ch.Type = model.TypeKey(v)
	}
	str := func(name string) *string {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			v := f.Value.String()
			return &v
		}
		return nil
	}
	ch.Key = str(flagKey)
	ch.CredentialRef = str(flagCredentialRef)
	if v := str(flagConfiguration); v != nil {
		ch.Configuration = json.RawMessage(*v)
	}
	return ch
}

// channelPlan renders a channel plan for --dry-run.
func channelPlan(p channels.ChannelPlan, key string) *cli.Plan {
	ch := p.Request.Channel
	args := map[string]any{"channel": ch}
	effect := planEffect("create", "channel:"+orNew(ch.Key, ch.Ref.ID), false,
		fmt.Sprintf("%s channel; ID and revision assigned at commit", ch.Type))
	checked := []string{"authorization", "request", "adapter-configuration", "key", "provider-account"}
	if p.Current != nil {
		args["expected_revision"] = p.Request.ExpectedRevision
		args["current"] = *p.Current
		effect = planEffect("update", "channel:"+channels.Name(ch.Key, ch.Ref.ID), true,
			fmt.Sprintf("replace revision %s; new revision assigned at commit", p.Request.ExpectedRevision))
		checked = append([]string{"revision"}, checked...)
	}
	if key != "" {
		args["operation_key"] = key
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: checked, Warnings: p.Warnings}
}

func orNew(key string, id model.ID) string {
	if key == "" && id == "" {
		return "(new)"
	}
	return channels.Name(key, id)
}

func (a *app) channelWriteRun(cmd *cobra.Command, args []string,
	plan func(ctx context.Context, s channels.Service, op model.OperationID) (channels.ChannelPlan, error),
) error {
	if err := refuseSecret(cmd, flagCredentialRef, flagConfiguration); err != nil {
		return err
	}
	key, err := operationKey(cmd)
	if err != nil {
		return err
	}
	res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[channelOut], error) {
		var w written[channelOut]
		err := a.configure(ctx, c, model.ActionChannelConfigure, func(s channels.Service) error {
			p, err := plan(ctx, s, opID(key))
			if err != nil {
				return err
			}
			if cli.IsDryRun(cmd) {
				w.Plan = channelPlan(p, key)
				return nil
			}
			ch, acc, err := s.ConfigureChannel(ctx, p)
			if err != nil {
				return err
			}
			w.Out = channelOut{Channel: ch, Operation: operationOf(acc), OperationKey: key, Notes: p.Warnings}
			return nil
		})
		return w, err
	})
	if err != nil {
		return err
	}
	return res.render(a, cmd, func(o channelOut) [][2]string {
		return append(channelRows(o.Channel), operationRows(o.Operation, o.OperationKey)...)
	}, res.Out.Notes)
}

func (a *app) channelCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a channel: a configured connection to a provider",
		Long: `Create a channel running the adapter --type, with its configuration
(--configuration, a JSON object the adapter validates) and the
reference of its credential in the profile's credential backend
(--credential-ref). Nothing reaches the provider: creating opens no
connection.

Refused before anything is written: an adapter this build lacks (exit
2), a configuration the adapter rejects (exit 2), a key another
channel holds (exit 4), and a provider account another channel of the
profile already receives (exit 4) — one account has one receiver.

Committed as a uma operation under --idempotency-key: rerunning with
the same key and arguments returns the same operation (replayed);
without one, heo mints a key and prints it. --dry-run prints the exact
channel that would be written and writes nothing. Once committed, the
running daemon starts the channel's receiver and webhook route; no
restart. Operator role.`,
		Example: `  heo channel create --type native.webwidget --key web --configuration '{"sites":["site-1"]}' --credential-ref widget-key
  heo channel create --type smtp.mail --key support-mail --configuration "$(cat smtp.json)" --credential-ref smtp-relay --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.channelWriteRun(cmd, args, func(ctx context.Context, s channels.Service, op model.OperationID) (channels.ChannelPlan, error) {
				return s.PlanChannelCreate(ctx, channelChange(cmd), op, planMode(cmd))
			})
		},
	}
	c.Flags().String(flagType, "", "Adapter type key, from `heo adapter list` (required)")
	_ = c.MarkFlagRequired(flagType)
	addChannelFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &channelOut{}, "1.0")
	return c
}

func (a *app) channelUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update <channel>",
		Short: "Change a channel at the revision you read",
		Long: `Replace fields of the channel <channel> (key or ID): its key,
configuration or credential reference. Fields not given keep their
value; the type never changes. --revision names the revision being
replaced, as ` + "`channel get`" + ` shows it: if the channel changed since,
the update is a conflict (exit 4) and nothing is written.

The same checks as create run first. Committed as a uma operation under
--idempotency-key (minted and printed when absent). --dry-run prints
the current channel and the exact replacement and writes nothing. Once
committed, the running daemon replaces the channel's receiver and
webhook route; requests open on the old route end. Operator role.`,
		Example: `  heo channel update support-mail --revision rev_123 --credential-ref smtp-relay-2
  heo channel update support-mail --revision rev_123 --configuration "$(cat smtp.json)" --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.channelWriteRun(cmd, args, func(ctx context.Context, s channels.Service, op model.OperationID) (channels.ChannelPlan, error) {
				return s.PlanChannelUpdate(ctx, args[0], revisionOf(cmd), channelChange(cmd), op, planMode(cmd))
			})
		},
	}
	addRevisionFlag(c, "channel")
	addChannelFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &channelOut{}, "1.0")
	return c
}

func (a *app) channelCheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "check [channel]",
		Short: "Check a channel's configuration, locally or against its provider",
		Long: `Check the channel [channel] (key or ID), or every channel with none
named. Nothing is written.

Local (default): the data store only, no network. The adapter is in
this build; the configuration passes the adapter's validation; the
provider account it receives is derivable and no other channel
receives it; each inbox of the channel holds a valid selection, its own
destinations, and note delivery its adapter declares. Warnings name
what works with a limit: no inbox, an inbox without destinations, a
disabled inbox (inbound rejected; queued sends still deliver).

--remote (one channel): an authenticated probe. The adapter instance is
opened with the channel's credentials, which may reach the provider,
and reports each declared capability: available or not, the provider's
permissions applied where the adapter probes them, and whether the
channel's own configuration needs it (receiving for an enabled inbox
with destinations, send for replies, internal_note for note delivery).
The report ends with a state per provider-dependent verb: available, or
why not (unauthorized: the provider account lacks the permission;
unavailable; unsupported; disabled: the adapter's own configuration
switched it off, github.issues without replies), with the envelope code
and exit status a command hitting that state would give (disabled is
DISABLED, exit 4; unsupported UNSUPPORTED, exit 2). Each credential purpose of the adapter is diagnosed — present, expired,
rejected, missing — without reading out any material, except one only a
way of receiving uses when the channel's configuration switches that
way off (slack.chat's signing secret under receive socket_mode, its
app-level token under events_api; telegram.messages' webhook secret
under poll; relay.messages' callback secret on a channel that names no callback;
discord.chat's public key under gateway).

Each way the adapter receives is also reported as the running daemon
runs it: the persistent receiver (a Discord gateway, a Slack socket, a
poll; way receive) with its state (starting, ready, waiting, failed),
the reason, and who holds its receiver lease and until when; and the
webhook route (way receive_http) with its path. A way the channel's
configuration switches off reads off. A receiver that stopped is an
error finding, one that is waiting a warning, when an enabled inbox
with destinations needs it. When the daemon runs the receive call it
also reports what the call is doing: phase (running, retrying), link
(connecting or connected, for an adapter that reports one), failures
and the last failure's code, and retry_at, when the next connection
opens. A receiver is ready once its link is up, or, for an adapter that
reports none, while it runs; it is waiting while it reconnects. A lease
another daemon holds shows its owner and expiry alone.
--remote with --offline is refused (exit 2).

Exits 2 when any finding is an error, after printing the report.
Operator role.`,
		Example: `  heo channel check
  heo channel check support-mail --remote
  heo channel check support-mail --remote --format json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, _ := cmd.Flags().GetBool(flagRemote)
			if remote && cli.IsOffline(cmd.Context()) {
				return output.UsageError("--remote probes the provider; it cannot run with --offline")
			}
			if remote && len(args) == 0 {
				return output.UsageError("--remote probes one channel: name it")
			}
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			res, err := daemonRunReport(a, cmd, args, codeCheckFailed, func(ctx context.Context, c control.Caller) (channelChecks, error) {
				checks, err := a.checkChannels(ctx, c, ref, remote)
				return channelChecks{Remote: remote, OK: allOK(checks), Checks: checks}, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				if err := output.Dispatch(cmd, a.r.Viper, res); err != nil {
					return err
				}
			} else if err := writeChecks(cmd.OutOrStdout(), res); err != nil {
				return err
			}
			if !res.OK {
				e := output.UsageError(fmt.Sprintf("%d of %d channels failed their check", failed(res.Checks), len(res.Checks)))
				e.Code = codeCheckFailed
				e.SuggestedFix = "fix the errors reported above, then check again"
				return e
			}
			return nil
		},
	}
	c.Flags().Bool(flagRemote, false, "Probe the provider with the channel's credentials (one channel)")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &channelChecks{}, "1.0")
	return c
}

// checkChannels runs in the daemon: the caller's checks, a remote one
// diagnosed with the runtime's own credential backend, which knows the
// credentials a provider rejected.
func (a *app) checkChannels(ctx context.Context, c control.Caller, ref string, remote bool) ([]channels.ChannelCheck, error) {
	var out []channels.ChannelCheck
	err := a.configure(ctx, c, model.ActionChannelRead, func(s channels.Service) error {
		o := channels.CheckOptions{Remote: remote}
		if b := a.creds.Load(); b != nil {
			o.Grants = b
			o.Credentials = b
		}
		if remote {
			o.Receiving = a.receiving
		}
		var err error
		out, err = s.Check(ctx, ref, o)
		return err
	})
	if out == nil {
		out = []channels.ChannelCheck{}
	}
	return out, err
}

// receiving is how this daemon runs one way a channel receives, as a
// remote check reports it.
func (a *app) receiving(ctx context.Context, ch model.Channel, way model.Capability) channels.ReceivingState {
	r := a.runtime.Receiving(ctx, ch.Ref, way)
	return channels.ReceivingState{
		Way: way, State: r.State, Reason: r.Reason, Since: r.Since, Path: r.Path, Owner: r.Owner, Until: r.Until,
		Phase: r.Phase, Link: r.Link, Failures: r.Failures, LastFailure: r.LastFailure, RetryAt: r.RetryAt, Detail: r.Detail,
	}
}

func allOK(checks []channels.ChannelCheck) bool { return failed(checks) == 0 }

func failed(checks []channels.ChannelCheck) int {
	n := 0
	for _, c := range checks {
		if !c.OK {
			n++
		}
	}
	return n
}

// receiveCall is what uma reports of the receive call in a check's text:
// phase, link, the failures counted with the last one's code, and when
// the next connection opens.
func receiveCall(rv channels.ReceivingState) string {
	var parts []string
	if rv.Phase != "" {
		parts = append(parts, "phase "+rv.Phase)
	}
	if rv.Link != "" {
		link := "link " + rv.Link
		if rv.Detail != "" {
			link += " (" + rv.Detail + ")"
		}
		parts = append(parts, link)
	}
	if rv.Failures > 0 {
		parts = append(parts, fmt.Sprintf("failures %d (last %s)", rv.Failures, rv.LastFailure))
	}
	if !rv.RetryAt.IsZero() {
		parts = append(parts, "retry at "+rv.RetryAt.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, ", ")
}

// writeChecks prints each channel's verdict, its findings and, after a
// remote check, its capabilities and credentials.
func writeChecks(w io.Writer, res channelChecks) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	pf := func(format string, a ...any) { _, _ = fmt.Fprintf(tw, format, a...) }
	if len(res.Checks) == 0 {
		pf("no channels\n")
	}
	for i, c := range res.Checks {
		if i > 0 {
			pf("\n")
		}
		verdict := "ok"
		if !c.OK {
			verdict = "FAILED"
		}
		pf("channel %s (%s): %s\n", channels.Name(c.Key, c.ID), c.Type, verdict)
		if c.Account != "" {
			pf("  account\t%s\n", c.Account)
		}
		for _, f := range c.Findings {
			pf("  %s\t%s\t%s\t%s\n", f.Severity, f.Scope, f.Code, f.Message)
		}
		if c.Remote == nil {
			continue
		}
		for _, cp := range c.Remote.Capabilities {
			state := "available"
			if !cp.Available {
				state = "unavailable (" + string(cp.Reason) + ")"
			}
			req := ""
			if cp.Required {
				req = "required"
			}
			pf("  capability\t%s\t%s\t%s\n", cp.Name, state, req)
		}
		for _, v := range c.Remote.Verbs {
			state := v.State
			if v.Exit != 0 {
				state = fmt.Sprintf("%s (%s, exit %d)", v.State, v.Code, v.Exit)
			}
			pf("  verb\t%s\t%s\t%s\n", v.Verb, state, v.Capability)
		}
		if rr := c.Remote.ReplyRecipients; rr.Verb != "" {
			state := rr.State
			if rr.Exit != 0 {
				state = fmt.Sprintf("%s (%s, exit %d)", rr.State, rr.Code, rr.Exit)
			}
			if l := rr.Recipients; l != nil {
				state += fmt.Sprintf(" reply-all=%t cc=%t bcc=%t max=%d", l.ReplyAll, l.Cc, l.Bcc, l.MaxRecipients)
			}
			pf("  verb\t%s\t%s\t%s\n", rr.Verb, state, rr.Capability)
		}
		for _, rv := range c.Remote.Receiving {
			detail := strings.TrimSpace(rv.Reason)
			if rv.Path != "" {
				detail = strings.TrimSpace(detail + " " + rv.Path)
			}
			if rv.Owner != "" {
				detail = strings.TrimSpace(detail + " lease " + rv.Owner + " until " + rv.Until.UTC().Format(time.RFC3339))
			}
			if call := receiveCall(rv); call != "" {
				detail = strings.TrimSpace(detail + "; " + call)
			}
			pf("  receiving\t%s\t%s\t%s\n", rv.Way, rv.State, detail)
		}
		for _, d := range c.Remote.Credentials {
			pf("  credential\t%s\t%s\t%s\n", d.Purpose, d.Status, strings.TrimSpace(d.Message))
		}
	}
	return tw.Flush()
}
