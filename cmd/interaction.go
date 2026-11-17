package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags of the interaction verbs.
const (
	flagActor     = "actor"
	flagFollowUp  = "follow-up"
	flagExpiresAt = "expires-at"
)

// interactionRow is an interaction in a listing table.
type interactionRow struct {
	ID           string `json:"id"            yaml:"id"            table:"ID,id"`
	Conversation string `json:"conversation"  yaml:"conversation"  table:"CONVERSATION"`
	Kind         string `json:"kind"          yaml:"kind"          table:"KIND"`
	Name         string `json:"name"          yaml:"name"          table:"NAME"`
	Actor        string `json:"actor"         yaml:"actor"         table:"ACTOR"`
	Recorded     string `json:"recorded"      yaml:"recorded"      table:"RECORDED"`
	RespondUntil string `json:"respond_until" yaml:"respond_until" table:"RESPOND UNTIL"`
	Answered     string `json:"answered"      yaml:"answered"      table:"ANSWERED"`
}

func interactionRowOf(in model.Interaction) interactionRow {
	return interactionRow{
		ID: string(in.Ref.ID), Conversation: string(in.Ref.Conversation.ID), Kind: in.Kind, Name: interactionName(in),
		Actor: string(in.Actor.Identity.ID), Recorded: stamp(in.RecordedAt), RespondUntil: stamp(in.RespondUntil),
		Answered: answeredText(in.Response),
	}
}

// interactionName is what the interaction invoked: the command, the
// button's action ID or the form's schema, as the provider named it.
func interactionName(in model.Interaction) string {
	switch in.Kind {
	case model.InteractionCommand:
		return orDash(in.Command)
	case model.InteractionAction:
		return orDash(in.Action)
	}
	return orDash(in.Fields.Name)
}

// answeredText says whether an interaction has its initial response and
// how many follow-ups.
func answeredText(r model.InteractionResponse) string {
	switch {
	case r.Initial == nil && r.FollowUps == 0:
		return "no"
	case r.Initial == nil:
		return fmt.Sprintf("follow-ups only (%d)", r.FollowUps)
	case r.FollowUps == 0:
		return "yes"
	}
	return fmt.Sprintf("yes, %d follow-up(s)", r.FollowUps)
}

func interactionRows(in model.Interaction) [][2]string {
	rows := [][2]string{
		{"id", string(in.Ref.ID)},
		{"conversation", string(in.Ref.Conversation.ID)},
		{"inbox", string(in.Ref.Conversation.Inbox.ID)},
		{"kind", in.Kind},
		{"name", interactionName(in)},
		{"arguments", orDash(in.Arguments)},
	}
	if in.Fields.Name != "" {
		rows = append(rows, [2]string{"form", fmt.Sprintf("%s v%d: %s", in.Fields.Name, in.Fields.Version, strings.TrimSpace(string(in.Fields.Data)))})
	}
	rows = append(
		rows,
		[2]string{"actor", fmt.Sprintf("%s (%s)", in.Actor.Identity.ID, orDash(in.Actor.Kind))},
		[2]string{"control message", controlMessage(in)},
		[2]string{"visibility", string(in.Audience.Visibility)},
		[2]string{"occurred", stamp(in.OccurredAt)},
		[2]string{"recorded", stamp(in.RecordedAt)},
		[2]string{"acknowledge by", stamp(in.Deadline)},
		[2]string{"respond until", respondUntilText(in)},
		[2]string{"answered", answeredText(in.Response)},
	)
	if in.Response.Initial != nil {
		rows = append(rows, [2]string{"initial response", "operation " + string(in.Response.Initial.ID)})
	}
	rows = append(rows, [2]string{"source", string(in.Source)})
	if in.Redacted {
		rows = append(rows, [2]string{"redacted", "yes (arguments and submitted values are gone)"})
	}
	return append(rows, [2]string{"revision", string(in.Revision)})
}

// controlMessage names the message the control sits on: the local
// record when uma resolved one, else the provider's reference.
func controlMessage(in model.Interaction) string {
	switch {
	case in.Message != nil:
		return string(in.Message.ID)
	case in.MessageExternal != nil:
		return routeName(*in.MessageExternal) + " (provider; no local message)"
	}
	return "-"
}

func respondUntilText(in model.Interaction) string {
	if in.RespondUntil.IsZero() {
		return fmt.Sprintf("- (the provider states none; a response defaults to %s after it was recorded)", desk.DefaultRespondWindow)
	}
	return stamp(in.RespondUntil)
}

func (a *app) interactionCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "interaction",
		Short: "List and show the button presses, commands and forms a provider reported, and answer them",
		Long: `A chat provider reports a button press, a slash command or a submitted
form as an interaction, and expects an answer inside a window. uma
records it, never as a message, and emits an interaction.received event
(` + "`heo event list --kind interaction.received`" + `). These verbs read the
records and answer them.

Every command runs in the profile's daemon (none running exits 70).
Listing and showing are reads, open to the desk and viewer roles; they
need no adapter capability. Answering reaches the provider, so it works
only on channels whose adapter declares respond (and respond_follow_up
for a follow-up); it is the desk role's. ` + "`heo adapter list`" + ` shows which
adapters enable ` + "`interaction respond`" + `.

A modal is another way to answer: ` + "`open-modal`" + ` and ` + "`push-modal`" + ` show
a Slack modal view in answer to an interaction, and ` + "`update-modal`" + `
changes one that is open. They run Slack's own extension effects, so
they need extension_execute with that effect rather than respond, and
the interaction's trigger_id, which lives three seconds.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.interactionListCmd(), a.interactionGetCmd(), a.interactionRespondCmd(),
		a.interactionOpenModalCmd(), a.interactionPushModalCmd(), a.interactionUpdateModalCmd())
	return grp
}

func (a *app) interactionListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the interactions a provider reported, oldest first",
		Long: `List the interactions you may read, in recording order, a page at a
time (--limit, --cursor with the previous page's next). --inbox and
--conversation (key or record ID) and --actor (an identity, record ID:
who pressed, typed or submitted) narrow it; uma applies all three. Each
row says what was invoked (a command, a button's action ID or a form's
schema), when it was recorded, until when a response is still possible
and whether it was answered. Read-only.

A page can be short and still not be the last: follow next until
complete. An interaction you may not read is left out without trace; one
whose audience is not public is the desk role's alone. A redacted
interaction is listed without its arguments and values. Reading roles:
desk, viewer.`,
		Example: `  heo interaction list
  heo interaction list --conversation conv_123 --format json
  heo interaction list --actor idn_42`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			var f desk.InteractionFilter
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			f.Conversation, _ = cmd.Flags().GetString(flagConversation)
			f.Actor, _ = cmd.Flags().GetString(flagActor)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Interaction], error) {
				var out page[model.Interaction]
				err := a.desk(ctx, c, model.ActionInteractionRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListInteractions(ctx, f, pg)
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
			rows := make([]interactionRow, 0, len(res.Items))
			for _, in := range res.Items {
				rows = append(rows, interactionRowOf(in))
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagInbox, "", "Only interactions of this inbox (key or ID)")
	c.Flags().String(flagConversation, "", "Only interactions of this conversation (record ID)")
	c.Flags().String(flagActor, "", "Only interactions this identity performed (record ID)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[model.Interaction]{}, "1.0")
	return c
}

func (a *app) interactionGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <interaction>",
		Short: "Show one interaction",
		Long: `Show the interaction <interaction>, by record ID: what was invoked, with
its arguments or the form's values, who did it, the message the control
sits on, the two windows the provider gave (acknowledge by: the
provider's deadline for receiving it, long past once uma holds it;
respond until: the end of the period a response is still possible, none
when the provider states no window), and whether it was answered. --conversation
(record ID) narrows the lookup. Read-only; the provider's response token
is never shown. No such interaction, or one you may not read, exits 3.
Reading roles: desk, viewer.`,
		Example: `  heo interaction get ix_123
  heo interaction get ix_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conv, _ := cmd.Flags().GetString(flagConversation)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (model.Interaction, error) {
				var out model.Interaction
				err := a.desk(ctx, c, model.ActionInteractionRead, model.Audience{}, func(s desk.Service) (err error) {
					out, err = s.FindInteraction(ctx, args[0], conv)
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
			return writeRows(cmd.OutOrStdout(), interactionRows(res))
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the interaction belongs to (record ID); narrows the lookup")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &model.Interaction{}, "1.0")
	return c
}

// respondOut is what answering an interaction reports: the interaction,
// how the answer is shown and to whom, the message it commits (a message
// response only) and the operation that sends it — queued, never sent,
// when the command returns.
type respondOut struct {
	Interaction  model.ID         `json:"interaction"       yaml:"interaction"`
	Conversation model.ID         `json:"conversation"      yaml:"conversation"`
	Inbox        model.ID         `json:"inbox"             yaml:"inbox"`
	Mode         string           `json:"mode"              yaml:"mode"`
	Visibility   model.Visibility `json:"visibility"        yaml:"visibility"`
	FollowUp     bool             `json:"follow_up"         yaml:"follow_up"`
	ExpiresAt    time.Time        `json:"expires_at"        yaml:"expires_at"`
	Message      model.ID         `json:"message,omitempty" yaml:"message,omitempty"`
	Operation    operationView    `json:"operation"         yaml:"operation"`
	OperationKey string           `json:"operation_key"     yaml:"operation_key"`
}

func respondRows(o respondOut) [][2]string {
	rows := [][2]string{
		{"interaction", string(o.Interaction)},
		{"conversation", string(o.Conversation)},
		{"inbox", string(o.Inbox)},
		{"mode", o.Mode},
		{"visibility", string(o.Visibility)},
		{"follow-up", yesNo(o.FollowUp)},
		{"expires at", stamp(o.ExpiresAt)},
	}
	if o.Message != "" {
		rows = append(rows, [2]string{"message", string(o.Message)})
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

// respondNote is what a queued response says: given to the provider when
// the workers send it, inside the window, and what the mode shows.
func respondNote(o respondOut) []string {
	notes := queuedNote(o.Operation)
	if len(notes) == 0 {
		return nil
	}
	return append(notes, fmt.Sprintf(
		"the provider shows the response only once it accepts it, and only until %s: `heo delivery get %s` shows the operation",
		stamp(o.ExpiresAt), o.Operation.ID,
	))
}

// respondEffect says, in a plan, what each mode does at the provider.
func respondEffect(mode string, followUp bool) (kind, detail string) {
	what := "an initial response"
	if followUp {
		what = "a follow-up response"
	}
	switch mode {
	case model.ResponseModeEphemeral:
		return "send", "show " + what + " to the interaction's actor only, at the provider"
	case model.ResponseModeUpdate:
		return "update", "replace the provider message the control sits on with " + what
	}
	return "send", "post " + what + " as a new public message answering the interaction, at the provider"
}

// respondPlan renders the plan of an interaction response: the platform
// effect, the audience, what the preview checked of the adapter, the
// window and that acceptance is not delivery.
func respondPlan(ix model.Interaction, p desk.RespondPlan, text, key string) *cli.Plan {
	r := p.Request
	args := map[string]any{
		"interaction": ix.Ref.ID, "conversation": ix.Ref.Conversation.ID, "inbox": ix.Ref.Conversation.Inbox.ID,
		"audience": r.Audience.Visibility, "mode": r.Mode, "text": text, "follow_up": r.FollowUp,
		"expires_at": r.ExpiresAt.Format(time.RFC3339),
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := declarationArgs(args, p.Declaration)
	kind, detail := respondEffect(r.Mode, r.FollowUp)
	effect := planEffect(kind, "interaction:"+string(ix.Ref.ID), false, fmt.Sprintf(
		"%s; queued for the daemon's workers, given to the provider only when it accepts it, and not after %s", detail, args["expires_at"],
	))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: append(checked, "interaction", "respond window", "request")}
}

func (a *app) interactionRespondCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "respond <interaction>",
		Short: "Answer an interaction at its provider",
		Long: `Answer the interaction <interaction> (record ID) with --text, on the
platform that reported it. --mode says how it is shown, and is always
explicit; the audience follows from it and heo never widens it:

  message    a new public message answering the interaction; uma commits
             it as a message of the conversation with the operation
  ephemeral  shown to the interaction's actor only, no message is kept
  update     replaces the provider message the control sits on, which
             the interaction must name

There is one initial response per interaction: of two, one is accepted
and the other is a conflict (exit 4). --follow-up asks for a follow-up
instead, which never takes the initial response's place and may come
before or after it; it also needs the adapter to declare
respond_follow_up. An interaction that is not public takes an ephemeral
response only: a public one would widen who can see it. A redacted
interaction takes none (conflict, exit 4).

The response must be given inside the provider's window. --expires-at
(RFC 3339 or a date) bounds the attempt and may not pass the interaction's
respond window (` + "`interaction get`" + ` shows it); a window that has ended is
DEADLINE_EXPIRED (exit 1), at submission and again when the daemon's workers
send it, which then makes no provider call. Without --expires-at it is
the end of the provider's window, and where the provider states none,
` + desk.DefaultRespondWindow.String() + ` after the interaction was recorded; name a later
one to answer after that.

The response is queued: the command returns once uma accepted it
durably, with its operation, never sent. The daemon's workers give it to
the provider, which may still refuse it, reported on the operation. Only
a channel whose adapter declares respond can do this; any other is
refused, exit 2 (UNSUPPORTED), before anything is written.

--dry-run prints the platform effect, the audience and the window, says
which adapter declares respond and that live availability is not
checked, and writes nothing. Committed under --idempotency-key:
rerunning with the same key, mode, text and expiry returns the same
operation (replayed) and claims nothing twice; without one, heo mints a
key and prints it. Desk role.`,
		Example: `  heo interaction respond ix_123 --mode ephemeral --text "Approved." --dry-run
  heo interaction respond ix_123 --mode message --text "Approved." --idempotency-key answer-42
  heo interaction respond ix_123 --mode update --text "Approved by Ada." --follow-up`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			var in desk.RespondInput
			in.Mode, _ = cmd.Flags().GetString(flagMode)
			in.Text, _ = cmd.Flags().GetString(flagText)
			in.FollowUp, _ = cmd.Flags().GetBool(flagFollowUp)
			if t, err := timeFlag(cmd, flagExpiresAt); err != nil {
				return err
			} else if t != nil {
				in.ExpiresAt = *t
			}
			conv, _ := cmd.Flags().GetString(flagConversation)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[respondOut], error) {
				var w written[respondOut]
				err := a.desk(ctx, c, model.ActionInteractionRespond, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanRespond(ctx, args[0], conv, in, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = respondPlan(p.Interaction, p, in.Text, key)
						return nil
					}
					acc, err := s.Respond(ctx, p)
					if err != nil {
						return err
					}
					ref := p.Interaction.Ref
					w.Out = respondOut{
						Interaction: ref.ID, Conversation: ref.Conversation.ID, Inbox: ref.Conversation.Inbox.ID,
						Mode: in.Mode, Visibility: p.Request.Audience.Visibility, FollowUp: in.FollowUp, ExpiresAt: p.Request.ExpiresAt,
						Operation: operationOf(acc), OperationKey: key,
					}
					if m := acc.Operation.ResultMessage; m != nil {
						w.Out.Message = m.ID
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, respondRows, respondNote(res.Out))
		},
	}
	c.Flags().String(flagMode, "", "How the response is shown: message, ephemeral or update (required)")
	addTextFlag(c, "The response's text")
	c.Flags().Bool(flagFollowUp, false, "A follow-up response, not the initial one (needs respond_follow_up)")
	c.Flags().String(flagExpiresAt, "", "Latest time the response may be given, an RFC 3339 time or a date; default: the provider's window")
	c.Flags().String(flagConversation, "", "Conversation the interaction belongs to (record ID); narrows the lookup")
	_ = c.MarkFlagRequired(flagMode)
	providerDependent(c, model.CapabilityRespond)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &respondOut{}, "1.0")
	return c
}
