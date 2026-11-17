package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma"
	"hop.top/uma/model"
)

// historyOut is what an import reports, as uma counted it: the window
// that was read, and what it did to uma's record. Gap says the provider
// could not return part of what was asked, so the history is not whole;
// Complete says only that the window was read to its end.
type historyOut struct {
	Channel      model.ID `json:"channel"                yaml:"channel"`
	Conversation model.ID `json:"conversation,omitempty" yaml:"conversation,omitempty"`
	Since        string   `json:"since"                  yaml:"since"`
	// Until is the end of the window, as given (--until); empty when the
	// window had no upper bound.
	Until    string `json:"until,omitempty"        yaml:"until,omitempty"`
	Limit    int    `json:"limit"                  yaml:"limit"`
	Imported int    `json:"imported"               yaml:"imported"`
	Skipped  int    `json:"skipped"                yaml:"skipped"`
	// Gaps counts the pages the provider marked as missing part of
	// the window; Gap is Gaps > 0.
	Gaps int  `json:"gaps" yaml:"gaps"`
	Gap  bool `json:"gap"  yaml:"gap"`
	// Complete is true once the window held no more to read; Next, when
	// it is not, is the cursor that resumes it (--cursor).
	Complete bool         `json:"complete"       yaml:"complete"`
	Next     model.Cursor `json:"next,omitempty" yaml:"next,omitempty"`
}

func historyRows(o historyOut) [][2]string {
	scope := "channel " + string(o.Channel)
	if o.Conversation != "" {
		scope = "conversation " + string(o.Conversation)
	}
	rows := [][2]string{
		{"scope", scope},
		{"since", o.Since},
	}
	if o.Until != "" {
		rows = append(rows, [2]string{"until", o.Until})
	}
	rows = append(rows, [][2]string{
		{"limit", fmt.Sprint(o.Limit)},
		{"imported", fmt.Sprint(o.Imported)},
		{"skipped", fmt.Sprint(o.Skipped)},
		{"gap pages", fmt.Sprint(o.Gaps)},
		{"complete", fmt.Sprint(o.Complete)},
	}...)
	if o.Next != "" {
		rows = append(rows, [2]string{"next", string(o.Next)})
	}
	return rows
}

// historyNotes says what a report does and does not establish. It
// never says the history is whole: only that the provider returned what
// it did, and when it said it could not return part.
func historyNotes(o historyOut) []string {
	var n []string
	if o.Gap {
		n = append(n, fmt.Sprintf(
			"the provider could not return part of the window (%d page(s)): the history is not whole, and what it could not return is not in heo", o.Gaps))
	}
	if !o.Complete && o.Next != "" {
		n = append(n, fmt.Sprintf("stopped at the limit of %d messages read: resume with --cursor %s", o.Limit, o.Next))
	}
	if o.Complete && !o.Gap {
		window := "from " + o.Since
		if o.Until != "" {
			window += " to " + o.Until
		}
		n = append(n, "the provider's window "+window+" was read to its end; heo cannot tell what the provider no longer keeps beyond what it reports")
	}
	if o.Imported > 0 {
		n = append(n, "imported messages arrive as inbound history: they wake no snooze and make no conversation unread")
	}
	if o.Imported == 0 && o.Skipped > 0 {
		n = append(n, "nothing new: every message read was already held")
	}
	return n
}

// historyFailure is an import that stopped: uma's failure with what it
// had done before it, so a retry is not blind. Everything imported
// before the failure stays, and a rerun skips it.
func historyFailure(err error, rep uma.ImportReport) error {
	msg, code := err.Error(), model.CodeOf(err)
	var me *model.Error
	if errors.As(err, &me) {
		msg = me.SafeMessage
	}
	if code == "" {
		code = model.Unavailable
	}
	return model.NewError(code, fmt.Sprintf(
		"%s (before it stopped: %d imported, %d skipped, %d gap page(s); run it again, what is imported is skipped)",
		msg, rep.Imported, rep.Skipped, rep.Gaps))
}

func (a *app) historyCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "history",
		Short: "Fetch past messages from the provider",
		Long: `Fetch the past messages a provider still holds for a channel or a
conversation, and record each as inbound history in heo's store. It
reads the provider, so it works only on channels whose adapter
declares history; it runs in the profile's daemon (none running exits
70).`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.historyFetchCmd())
	return grp
}

func (a *app) historyFetchCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fetch",
		Short: "Import past messages of a channel or conversation",
		Long: `Read a bounded window of the provider's own past messages, of the
channel --channel or the conversation --conversation (record ID), from
--since on (an RFC 3339 time or a date; without it, the beginning of
what the provider holds) and before --until (the same forms; without it,
no upper end), and record each message heo does not already
hold as inbound, as if it had arrived then. Exactly one of --channel
and --conversation.

Each message is imported once, by its provider message ID: running it
again over the same window imports nothing, and a message that arrived
live is skipped, left as it is. Messages with no provider message ID,
envelopes that are no new message, and messages that belong to another
inbox than the conversation's are skipped too. Imported history wakes no
snooze and makes no conversation unread.

Mail (imap.mail, google.mail, microsoft.mail) reads received mail only,
oldest first: the receive mailbox, or the mailboxes the channel's
configuration lists in history_mailboxes (there is no per-call mailbox
flag; sent mail, drafts, junk and trash are never read). Attachments
arrive with their bytes. A conversation on a mail channel reads the mail
of its reply address: every message from that sender in the channel's
mailboxes, each recorded in the conversation it belongs to. To import
the past without overlapping the live receiver, set --until at the time
the receiver first started.

It is bounded: at most --limit messages are read per call (default 100,
at most 500). The report says what was imported, what was skipped and
how many pages the provider marked as missing part of the window (the
gap): a gap means the history is not whole, and heo cannot supply
what the provider did not return. It never replays events and invents
nothing: an adapter that does not declare history is refused, exit 2
(UNSUPPORTED), before anything is read. When the window is longer than
the limit, the report names a cursor: pass it as --cursor to resume.
A failure partway keeps what was imported before it and reports it.

It records messages for everyone to read and reads the provider, so it is
not served on REST, SSE, gRPC or MCP: a call may last as long as the
provider takes to answer its pages, and those surfaces serve one call
at a time. Use the terminal or the control socket. A conversation import
is the desk role's (and needs the right to read that conversation); a
whole channel's import is the operator's (it needs the right to read
the channel).

--dry-run prints what would be read and recorded, says which adapter
declares history and that live availability is not checked, reads
nothing from the provider and writes nothing. There is no operation
key: the import is not a queued operation, and its idempotence is the
provider message ID.`,
		Example: `  heo history fetch --conversation conv_123 --dry-run
  heo history fetch --conversation conv_123 --since 2026-09-01
  heo history fetch --channel support --since 2026-09-01T00:00:00Z --limit 200
  heo history fetch --channel mail --since 2026-01-01 --until 2026-09-01
  heo history fetch --conversation conv_123 --cursor pg.42`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, since, until, limit, cursor, err := historyFlags(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[historyOut], error) {
				var w written[historyOut]
				err := a.withClient(ctx, c, model.ActionChannelImport, model.Audience{}, func(rt daemon.Runtime, client uma.Client, pol *authz.Policy) error {
					// Importing a channel is the operator's (it reads the
					// channel); importing a conversation is the desk's (it
					// reads the conversation). channel.import alone says
					// neither.
					needs := model.ActionConversationRead
					if scope.Channel != "" {
						needs = model.ActionChannelRead
					}
					if err := permitted(pol, c, needs, model.Audience{}); err != nil {
						return err
					}
					s := deskOf(rt, client, pol, c)
					p, err := s.PlanHistory(ctx, scope, c.Actor, since, until, cursor, limit, planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = historyPlan(p)
						return nil
					}
					rep, err := rt.Import(ctx, p.Request)
					if err != nil {
						return historyFailure(err, rep)
					}
					w.Out = historyOut{
						Channel: p.Channel.ID, Since: stamp(p.Request.Since), Until: stampUntil(p.Request.Until), Limit: effectiveImportLimit(limit),
						Imported: rep.Imported, Skipped: rep.Skipped, Gaps: rep.Gaps, Gap: rep.Gaps > 0,
						Complete: rep.Complete, Next: model.Cursor(rep.Next),
					}
					if p.Conversation != nil {
						w.Out.Conversation = p.Conversation.Ref.ID
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			var notes []string
			if res.Plan == nil {
				notes = historyNotes(res.Out)
			}
			return renderDeskWrite(a, cmd, res, historyRows, notes)
		},
	}
	c.Flags().String(flagChannel, "", "Channel whose past messages to import (key or ID); the operator's")
	c.Flags().String(flagConversation, "", "Conversation whose past messages to import (record ID); the desk's")
	c.Flags().String(flagSince, "", "Import messages from this time on: an RFC 3339 time or a date (default: the start of the provider's window)")
	c.Flags().String(flagUntil, "", "Import messages before this time only: an RFC 3339 time or a date (default: no upper bound); the window is --since <= t < --until")
	c.Flags().Int(flagLimit, 0, fmt.Sprintf("Most messages to read in this call (1 to %d; default %d)", uma.MaxImportLimit, uma.DefaultImportLimit))
	c.Flags().String(flagCursor, "", "Cursor from a previous report's next, to resume a window longer than the limit")
	providerDependent(c, model.CapabilityHistory)
	daemonCommand(c)
	// An import reads the provider page by page, as long as the provider
	// takes, and kit serves one call at a time on REST, SSE, gRPC and MCP:
	// it would hold the daemon, and an MCP tool call would not return. It
	// is kept on the control socket, like the streaming commands.
	cli.SetWithholdFrom(c, cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &historyOut{}, Version: "1.0"})
	return c
}

// effectiveImportLimit is the limit uma applies to limit.
func effectiveImportLimit(limit int) int {
	if limit == 0 {
		return uma.DefaultImportLimit
	}
	return limit
}

// historyFlags reads and checks --channel, --conversation, --since,
// --limit and --cursor.
func historyFlags(cmd *cobra.Command) (scope desk.HistoryScope, since, until time.Time, limit int, cursor string, err error) {
	scope.Channel, _ = cmd.Flags().GetString(flagChannel)
	scope.Conversation, _ = cmd.Flags().GetString(flagConversation)
	if (scope.Channel == "") == (scope.Conversation == "") {
		return scope, since, until, 0, "", output.UsageError("exactly one of --" + flagChannel + " and --" + flagConversation + " is required")
	}
	t, err := timeFlag(cmd, flagSince)
	if err != nil {
		return scope, since, until, 0, "", err
	}
	if t != nil {
		since = *t
	}
	u, err := timeFlag(cmd, flagUntil)
	if err != nil {
		return scope, since, until, 0, "", err
	}
	if u != nil {
		until = *u
		if !since.IsZero() && !until.After(since) {
			return scope, since, until, 0, "", output.UsageError("--" + flagUntil + " must be after --" + flagSince)
		}
	}
	limit, _ = cmd.Flags().GetInt(flagLimit)
	if limit < 0 || limit > uma.MaxImportLimit {
		return scope, since, until, 0, "", output.UsageError(fmt.Sprintf("--%s %d: want 1 to %d", flagLimit, limit, uma.MaxImportLimit))
	}
	cursor, _ = cmd.Flags().GetString(flagCursor)
	return scope, since, until, limit, cursor, nil
}

// historyPlan renders the plan of an import: what is read, what is
// recorded, what the preview checked of the adapter, and that nothing
// is read yet.
func historyPlan(p desk.HistoryPlan) *cli.Plan {
	args := map[string]any{
		"channel": p.Channel.ID, "since": stamp(p.Request.Since), "limit": effectiveImportLimit(p.Request.Limit),
	}
	if !p.Request.Until.IsZero() {
		args["until"] = stamp(p.Request.Until)
	}
	where := "channel " + string(p.Channel.ID)
	if p.Conversation != nil {
		args["conversation"] = p.Conversation.Ref.ID
		args["inbox"] = p.Conversation.Ref.Inbox.ID
		where = "conversation " + string(p.Conversation.Ref.ID)
	}
	if p.Request.After != "" {
		args["cursor"] = p.Request.After
	}
	checked := declarationArgs(args, p.Declaration)
	effect := planEffect("create", "history:"+where, false, fmt.Sprintf(
		"read up to %d of the provider's past messages of %s from %s and record each one heo does not already hold as inbound history, once, by provider message ID; nothing is sent to the provider, and nothing is read until the real run",
		effectiveImportLimit(p.Request.Limit), where, stamp(p.Request.Since)))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: append(checked, "scope", "request"),
		Warnings: []string{"a provider may hold only part of the window: the report counts the pages it could not return as a gap"}}
}

// stampUntil is the end of an import window as reported: empty when the
// window has no upper bound.
func stampUntil(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return stamp(t)
}
