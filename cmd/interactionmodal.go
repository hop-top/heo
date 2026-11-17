package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

// Flags of the modal verbs.
const (
	flagView       = "view"
	flagViewID     = "view-id"
	flagExternalID = "external-id"
	flagHash       = "hash"
	// viewStdin is --view's name for standard input.
	viewStdin = "-"
	// viewReadLimit bounds what is read for --view before the view is
	// compacted: the size uma bounds a view at (slack.MaxViewBytes) is of
	// the compact form, and a file indents it. The bound that decides is
	// uma's, checked on the compact view.
	viewReadLimit = 4 * slack.MaxViewBytes
)

// modalTrigger reads the trigger_id an interaction carries and when it
// stops working: the adapter's own reader, slack.TriggerOf. An e2e build
// widens it to its fake provider (extra_e2e.go).
var modalTrigger = slack.TriggerOf

// readViewInput reads --view for a command typed at a terminal, or relayed
// from an agent: "-" is standard input, a value that is a JSON object is
// the view itself, anything else a file. At most viewReadLimit bytes are
// read.
func readViewInput(cmd *cobra.Command, name string) ([]byte, error) {
	var r io.Reader
	switch trimmed := strings.TrimSpace(name); {
	case trimmed == "":
		return nil, output.UsageError("--" + flagView + ": name the file holding the modal view, or - for standard input")
	case strings.HasPrefix(trimmed, "{"):
		return []byte(trimmed), nil
	case trimmed == viewStdin:
		r = cmd.InOrStdin()
	default:
		f, err := os.Open(name) //nolint:gosec // G304: the caller named the file
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return nil, output.UsageError(fmt.Sprintf("--%s %s: %v", flagView, name, err))
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, viewReadLimit+1))
	if err != nil {
		return nil, output.UsageError(fmt.Sprintf("--%s: %v", flagView, err))
	}
	if len(b) > viewReadLimit {
		return nil, output.UsageError(fmt.Sprintf("--%s: larger than Slack takes (%d KiB once compact)", flagView, slack.MaxViewBytes>>10))
	}
	return b, nil
}

// modalView returns the compact view the leaf was given. In the process
// the caller typed the command in, --view names a file or standard input,
// which the daemon cannot see: that process reads it, checks it and sends
// the compact view in the flag's place. The daemon (and a served surface)
// takes the flag as the view itself.
func (a *app) modalView(cmd *cobra.Command) (json.RawMessage, error) {
	route, _ := control.RouteOf(cmd.Context(), a.control.servesControl())
	return modalViewAt(cmd, route)
}

// modalViewAt is modalView for a command that runs at route.
func modalViewAt(cmd *cobra.Command, route control.Route) (json.RawMessage, error) {
	raw, _ := cmd.Flags().GetString(flagView)
	if route == control.Forward {
		b, err := readViewInput(cmd, raw)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	view, err := desk.CheckView([]byte(raw))
	if err != nil {
		return nil, output.UsageError(strings.TrimPrefix(err.Error(), "validation: "))
	}
	if err := cmd.Flags().Set(flagView, string(view)); err != nil {
		return nil, err
	}
	return view, nil
}

// modalOut is what a modal verb reports: what it ran against, the
// operation that carries it to Slack and its key. The trigger_id is the
// provider's single-use credential and is never shown; the view's ID is
// Slack's to name, and arrives as a provider reference of the operation.
type modalOut struct {
	Effect       string        `json:"effect"                 yaml:"effect"`
	Interaction  model.ID      `json:"interaction,omitempty"  yaml:"interaction,omitempty"`
	Conversation model.ID      `json:"conversation,omitempty" yaml:"conversation,omitempty"`
	Inbox        model.ID      `json:"inbox"                  yaml:"inbox"`
	ViewID       string        `json:"view_id,omitempty"      yaml:"view_id,omitempty"`
	ExternalID   string        `json:"external_id,omitempty"  yaml:"external_id,omitempty"`
	Hash         string        `json:"hash,omitempty"         yaml:"hash,omitempty"`
	TriggerUntil *time.Time    `json:"trigger_until,omitempty" yaml:"trigger_until,omitempty"`
	Operation    operationView `json:"operation"              yaml:"operation"`
	OperationKey string        `json:"operation_key"          yaml:"operation_key"`
	Notes        []string      `json:"notes,omitempty"        yaml:"notes,omitempty"`
}

func modalOutOf(p desk.ModalPlan, acc model.Acceptance, key string) modalOut {
	o := modalOut{
		Effect: p.Effect, Inbox: p.Inbox.Ref.ID, ViewID: p.View.ViewID, ExternalID: p.View.ExternalID, Hash: p.View.Hash,
		Operation: operationOf(acc), OperationKey: key,
	}
	if p.SpendsTrigger() {
		o.Interaction, o.Conversation = p.Interaction.Ref.ID, p.Interaction.Ref.Conversation.ID
		o.ExternalID, o.TriggerUntil = p.ExternalID, &p.ExpiresAt
	}
	o.Notes = modalNotes(p, o)
	return o
}

func modalRows(o modalOut) [][2]string {
	rows := [][2]string{{"effect", o.Effect}}
	if o.Interaction != "" {
		rows = append(rows, [2]string{"interaction", string(o.Interaction)}, [2]string{"conversation", string(o.Conversation)})
	}
	rows = append(rows, [2]string{"inbox", string(o.Inbox)})
	if o.ViewID != "" {
		rows = append(rows, [2]string{"view", o.ViewID})
	}
	if o.ExternalID != "" {
		rows = append(rows, [2]string{"external id", o.ExternalID})
	}
	if o.Hash != "" {
		rows = append(rows, [2]string{"hash", o.Hash})
	}
	if o.TriggerUntil != nil {
		rows = append(rows, [2]string{"trigger valid until", stamp(*o.TriggerUntil)})
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

// modalNotes says what a queued modal leaves open: Slack has not been
// heard from, the trigger_id's window, and where the opened view's ID
// will be.
func modalNotes(p desk.ModalPlan, o modalOut) []string {
	notes := queuedNote(o.Operation)
	op := o.Operation.ID
	if !p.SpendsTrigger() {
		return append(notes, fmt.Sprintf("the view changes only when Slack accepts the update: `heo delivery get %s` follows the operation", op))
	}
	notes = append(notes, fmt.Sprintf(
		"the modal shows only if Slack accepts the trigger_id before %s (single-use, %s after Slack sent it); a later or spent one is refused and costs nothing, "+
			"and a lost answer stays unknown and is never sent again: `heo delivery get %s` follows the operation",
		stamp(p.ExpiresAt), slack.TriggerLifetime, op))
	update := fmt.Sprintf("`heo interaction update-modal --inbox %s --view-id <id> --view <file>`", o.Inbox)
	if p.ExternalID != "" {
		update += fmt.Sprintf(", or by --external-id %s", p.ExternalID)
	}
	return append(notes, fmt.Sprintf(
		"Slack names the view it opened once it accepts it: the settled operation's provider reference of kind view (`heo operation get %s`, `provider refs`) is the ID to update it with: %s",
		op, update))
}

// modalPlan renders a plan for --dry-run: the external effect at Slack,
// the window the trigger_id gives it, and nothing queued.
func modalPlan(p desk.ModalPlan, key string) *cli.Plan {
	r := p.Request
	args := map[string]any{
		"inbox": p.Inbox.Ref.ID, "visibility": r.Audience.Visibility,
		"change":   p.Effect + " (the view is checked offline by the adapter)",
		"delivery": "queued, not shown: Slack shows the modal only once it accepts the trigger_id inside its window",
	}
	var detail string
	switch {
	case p.SpendsTrigger():
		args["interaction"], args["conversation"] = p.Interaction.Ref.ID, p.Interaction.Ref.Conversation.ID
		args["trigger"] = fmt.Sprintf("the interaction's own (single-use), valid until %s; not shown", stamp(p.ExpiresAt))
		args["expires_at"] = p.ExpiresAt.UTC().Format(time.RFC3339)
		detail = fmt.Sprintf("external: Slack shows a modal to the person who acted (%s), sent as %s; queued for the daemon's workers, "+
			"which are woken at once, and refused with no call once the trigger_id stops working at %s", p.Interaction.Actor.Identity.ID, p.Effect, stamp(p.ExpiresAt))
	default:
		view := p.View.ViewID
		if view == "" {
			view = "external_id " + p.View.ExternalID
		}
		args["view"] = view
		detail = fmt.Sprintf("external: Slack replaces the contents of modal view %s, sent as %s; queued for the daemon's workers", view, p.Effect)
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := effectArgs(args, p.Declaration)
	target := "slack:modal inbox:" + string(p.Inbox.Ref.ID)
	if p.SpendsTrigger() {
		target = "slack:modal interaction:" + string(p.Interaction.Ref.ID)
	}
	checked = append(checked, "view")
	if p.SpendsTrigger() {
		checked = append(checked, "interaction", "trigger window")
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("send", target, false, detail)}, PrerequisitesChecked: checked}
}

// modalFlags is what the three verbs share.
func (a *app) modalFlags(c *cobra.Command) {
	c.Flags().String(flagView, "", "The modal view, a Slack view object as JSON: a file, - for standard input, or the JSON itself (required)")
	_ = c.MarkFlagRequired(flagView)
	providerDependent(c, model.CapabilityExtensionExecute)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &modalOut{}, Version: "1.0"})
}

// interactionModalCmd is `interaction open-modal` and `interaction
// push-modal`: the two effects that spend an interaction's trigger_id.
func (a *app) interactionModalCmd(effect, use, short, long, example string) *cobra.Command {
	c := &cobra.Command{
		Use: use, Short: short, Long: long, Example: example, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			view, err := a.modalView(cmd)
			if err != nil {
				return err
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			conv, _ := cmd.Flags().GetString(flagConversation)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[modalOut], error) {
				var w written[modalOut]
				err := a.desk(ctx, c, model.ActionExtensionExecute, internalAudience, func(s desk.Service) error {
					p, err := s.PlanModal(ctx, effect, args[0], conv, view, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = modalPlan(p, key)
						return nil
					}
					acc, err := s.SubmitModal(ctx, p)
					if err != nil {
						return err
					}
					w.Out = modalOutOf(p, acc, key)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, modalRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the interaction belongs to (record ID); narrows the lookup")
	a.modalFlags(c)
	return c
}

func (a *app) interactionOpenModalCmd() *cobra.Command {
	return a.interactionModalCmd(slack.ExtensionViewsOpen, "open-modal <interaction>",
		"Open a Slack modal in answer to an interaction",
		`Open the modal view --view in answer to the interaction <interaction>
(record ID), on Slack: the person who pressed the button or typed the
command sees it. --view is a Slack modal view object as JSON, from a
file, from standard input (-) or given inline; at most `+fmt.Sprint(slack.MaxViewBytes>>10)+` KiB once
compact, the bound Slack states.

A modal can only be opened with the trigger_id Slack sent with the
interaction, and Slack honours it for `+slack.TriggerLifetime.String()+` after it sent it,
once. heo takes the trigger_id from the interaction and the instant it
stops working from its deadline (`+"`interaction get`"+` shows it as
acknowledge by), and refuses a later one at once, DEADLINE_EXPIRED
(exit 1), at the preview and at submission, with nothing queued. That
window is much shorter than a person typing: this is a verb for a
script or an agent that reacts to the `+"`interaction.received`"+` event
(`+"`heo event watch`"+`). An interaction that is redacted
is a conflict (exit 4), one with no trigger_id (Slack named it by a
digest of its payload, or it is not Slack's) a usage error (exit 2),
and one you may not read does not exist (exit 3). Answering needs the
same right as `+"`interaction respond`"+` (desk role).

Only a channel whose adapter declares extension_execute and ships
Slack's views.open can do this: --dry-run refuses any other, exit 2
(UNSUPPORTED), as does the real run, before anything is written.
--dry-run also asks the adapter, offline, to check the view
(VALIDATION, exit 2) and writes and sends nothing.

The modal is queued as a uma operation and the command returns once uma
accepted it durably, with the daemon's workers woken at once, so the
`+slack.TriggerLifetime.String()+` are not also spent waiting out their poll. The command never says the
modal is open: Slack shows it only once it accepts the request, and
the operation (`+"`delivery get <operation>`"+`) says what Slack answered. An
attempt that gets there late is refused by the adapter with no call, and
a lost answer is left unknown and never sent again, because the
trigger_id may already be spent. When Slack accepts the view it names
it: the settled operation's provider references (`+"`operation get`"+`) carry
the view ID, which `+"`interaction update-modal`"+` takes. Committed under
--idempotency-key: rerunning with the same key and view returns the same
operation (replayed); without one, heo mints a key and prints it.
A retry past the window is refused as expired even where the first
attempt got through: `+"`operation get --key`"+` finds it.`,
		`  heo interaction open-modal ix_123 --view approve.json --dry-run
  heo interaction open-modal ix_123 --view approve.json --idempotency-key open-42
  heo interaction open-modal ix_123 --view - --conversation conv_9 --dry-run`)
}

func (a *app) interactionPushModalCmd() *cobra.Command {
	return a.interactionModalCmd(slack.ExtensionViewsPush, "push-modal <interaction>",
		"Stack another Slack modal view on the modal an interaction came from",
		`Push the modal view --view onto the stack of the modal the interaction
<interaction> (record ID) came from, on Slack: a button inside a modal
opens the next step. Slack allows an app three views in a stack; the third
push is refused. It takes the same view as `+"`interaction open-modal`"+` and
spends the interaction's trigger_id the same way: single-use, valid for
`+slack.TriggerLifetime.String()+`, refused here at once (DEADLINE_EXPIRED, exit 1) once it has
stopped working. heo cannot tell whether the interaction came from a
modal or from a message: Slack refuses the wrong one, and the operation
says so.

Only a channel whose adapter declares extension_execute and ships
Slack's views.push can do this: --dry-run refuses any other, exit 2
(UNSUPPORTED), as does the real run, before anything is written. The
preview, the queueing, the idempotency key and the pushed view's ID are
those of `+"`interaction open-modal`"+`.`,
		`  heo interaction push-modal ix_124 --view confirm.json --dry-run
  heo interaction push-modal ix_124 --view confirm.json --idempotency-key push-42`)
}

func (a *app) interactionUpdateModalCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update-modal",
		Short: "Replace the contents of an open Slack modal view",
		Long: `Replace the contents of an open modal view with --view, on Slack. Name
the view by the ID Slack gave it (--view-id, a provider reference of the
settled ` + "`interaction open-modal`" + ` or ` + "`push-modal`" + ` operation: read it with
` + "`heo operation get <operation>`" + `, the ` + "`provider refs`" + ` row) or by the
external_id the view was opened with (--external-id, a field of the
view): exactly one. --hash is the hash Slack last gave the view; Slack
refuses an update to a view that has changed since (CONFLICT).

--inbox (key or ID) names the Slack workspace's inbox the view lives
in: the update is aimed at it, because a view is not tied to a
conversation. An update uses no trigger_id and has no window of its own;
Slack shows the new contents once it accepts the request.

Only a channel whose adapter declares extension_execute and ships
Slack's views.update can do this: --dry-run refuses any other, exit 2
(UNSUPPORTED), as does the real run. --dry-run also asks the adapter,
offline, to check the view and the way it is named (VALIDATION, exit 2)
and writes and sends nothing. The update is queued like any other:
` + "`delivery get <operation>`" + ` follows it. Committed under --idempotency-key.
Desk role.`,
		Example: `  heo interaction update-modal --inbox rooms --view-id V0123456789 --view approved.json --dry-run
  heo interaction update-modal --inbox rooms --external-id deploy-42 --view approved.json
  heo interaction update-modal --inbox rooms --view-id V0123456789 --hash 156772938.1827394 --view -`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := desk.ViewRef{}
			ref.ViewID, _ = cmd.Flags().GetString(flagViewID)
			ref.ExternalID, _ = cmd.Flags().GetString(flagExternalID)
			ref.Hash, _ = cmd.Flags().GetString(flagHash)
			inbox, _ := cmd.Flags().GetString(flagInbox)
			view, err := a.modalView(cmd)
			if err != nil {
				return err
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[modalOut], error) {
				var w written[modalOut]
				err := a.desk(ctx, c, model.ActionExtensionExecute, internalAudience, func(s desk.Service) error {
					p, err := s.PlanViewUpdate(ctx, inbox, ref, view, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = modalPlan(p, key)
						return nil
					}
					acc, err := s.SubmitModal(ctx, p)
					if err != nil {
						return err
					}
					w.Out = modalOutOf(p, acc, key)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, modalRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagInbox, "", "Inbox of the Slack workspace the view lives in (key or ID) (required)")
	_ = c.MarkFlagRequired(flagInbox)
	c.Flags().String(flagViewID, "", "The view's ID, as Slack named it when it opened the view")
	c.Flags().String(flagExternalID, "", "The external_id the view was opened with")
	c.Flags().String(flagHash, "", "The hash Slack last gave the view, so a stale update is refused")
	c.MarkFlagsOneRequired(flagViewID, flagExternalID)
	c.MarkFlagsMutuallyExclusive(flagViewID, flagExternalID)
	a.modalFlags(c)
	return c
}
