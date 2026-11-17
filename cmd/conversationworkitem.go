package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags of the work item verbs of `conversation` (--set is shared with
// `conversation tag`).
const (
	flagField          = "field"
	flagWorkItem       = "work-item"
	flagWorkItemStatus = "work-item-status"
)

// workItemRef is how output names a work item at its provider: its
// namespace's own reference, a parent and an ID as parent#id, or the ID.
func workItemRef(w model.WorkItem) string {
	if w.External.ParentID != "" {
		return w.External.ParentID + "#" + w.External.ID
	}
	return w.External.ID
}

// workItemRows is the work item bound to a conversation, as rows: its
// status beside the provider's own, its binding, who it is assigned to,
// its fields as the provider or uma holds them, and its link back.
func workItemRows(w model.WorkItem, linkedFrom []model.ConversationRef) [][2]string {
	rows := [][2]string{
		{"work item", string(w.Ref.ID)},
		{"work item reference", orDash(workItemRef(w))},
		{"work item subject", orDash(w.Subject)},
		{"work item status", orDash(w.Status)},
		{"work item provider status", orDash(w.RawStatus)},
		{"work item binding", orDash(w.Binding)},
		{"work item requester", orDash(w.Requester.ID)},
		{"work item assignees", orDash(assigneeNames(w))},
		{"work item url", orDash(w.URL)},
		{"work item revision", string(w.Revision)},
	}
	if len(w.LocalFields) > 0 {
		rows = append(rows, [2]string{"work item uma-owned fields", strings.Join(w.LocalFields, ", ")})
	}
	for _, f := range w.Fields {
		rows = append(rows, [2]string{"work item field " + f.Name, string(f.Data)})
	}
	if w.Successor != nil {
		rows = append(rows, [2]string{"work item successor", routeName(*w.Successor)})
	}
	if len(linkedFrom) > 0 {
		names := make([]string, 0, len(linkedFrom))
		for _, r := range linkedFrom {
			names = append(names, "conversation "+string(r.ID))
		}
		rows = append(rows, [2]string{"work item linked from", strings.Join(names, ", ")})
	}
	return rows
}

func assigneeNames(w model.WorkItem) string {
	ids := make([]string, 0, len(w.Assignees))
	for _, a := range w.Assignees {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ", ")
}

// workItemCell is a work item in a listing table: its reference and
// status, a dash for a conversation that has none.
func workItemCell(w *model.WorkItem) string {
	if w == nil {
		return "-"
	}
	return strings.TrimSpace(orDash(workItemRef(*w)) + " " + w.Status)
}

// workItemFilterOf reads --work-item and --work-item-status of
// `conversation list`: only the ones the caller gave are set.
func workItemFilterOf(cmd *cobra.Command) desk.WorkItemFilter {
	var f desk.WorkItemFilter
	if cmd.Flags().Changed(flagWorkItem) {
		v, _ := cmd.Flags().GetBool(flagWorkItem)
		f.Only = &v
	}
	f.Statuses, _ = cmd.Flags().GetStringSlice(flagWorkItemStatus)
	return f
}

// workItemStatusOut is what a status change reports: the work item as uma holds
// it now, the status asked for and the reason, and the operation and its
// key. A status the provider owns is still as it was: it changes when the
// provider reports it.
type workItemStatusOut struct {
	Conversation model.Conversation `json:"conversation"       yaml:"conversation"`
	WorkItem     model.WorkItem     `json:"work_item"          yaml:"work_item"`
	Status       string             `json:"status"             yaml:"status"`
	Reason       string             `json:"reason,omitempty"   yaml:"reason,omitempty"`
	Local        bool               `json:"local,omitempty"    yaml:"local,omitempty"`
	Operation    operationView      `json:"operation"          yaml:"operation"`
	OperationKey string             `json:"operation_key"      yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty"    yaml:"notes,omitempty"`
}

// reasonText is the reason of a change as a plan says it: the one asked
// for, or whose it is.
func reasonText(reason string) string {
	if reason != "" {
		return reason
	}
	return "the adapter's default"
}

// statusPlan renders a status change plan for --dry-run: for a status the
// provider owns the external change at the provider, otherwise the change
// in uma.
func statusPlan(p desk.StatusPlan, key string) *cli.Plan {
	w := p.Item
	args := map[string]any{
		"conversation": p.Conversation.Ref.ID, "work_item": w.Ref.ID, "inbox": w.Ref.Conversation.Inbox.ID,
		"from": w.Status, "status": p.Status, "expected_revision": p.Request.ExpectedRevision, "current": w,
	}
	if p.Reason != "" {
		args["reason"] = p.Reason
	}
	if key != "" {
		args["operation_key"] = key
	}
	if p.Local {
		effect := planEffect("update", "work_item:"+string(w.Ref.ID), true, fmt.Sprintf(
			"work item status %s -> %s, local to uma (the status is uma's own on this channel; nothing changes at the provider); "+
				"conversation %s stays %s",
			orDash(w.Status), p.Status, p.Conversation.Ref.ID, p.Conversation.Status))
		return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: []string{"authorization", "work item", "binding", "revision", "request"}}
	}
	args["destination"] = w.External
	args["delivery"] = "queued, not applied: the work item changes in uma only when the provider reports it"
	checked := declarationArgs(args, p.Declaration)
	effect := planEffect("update", "work_item:"+string(w.Ref.ID), true, fmt.Sprintf(
		"external: %s %s status %s (%s) -> %s (reason: %s), sent to the provider as a work item act; "+
			"queued, not applied: the work item changes in uma only when the provider reports it, and conversation %s stays %s",
		w.External.Namespace, workItemRef(w), orDash(w.Status), orDash(w.RawStatus), p.Status, reasonText(p.Reason),
		p.Conversation.Ref.ID, p.Conversation.Status))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: append(checked, "work item", "binding", "transition", "revision", "request")}
}

// statusNotes says what a queued change leaves open: the operation is
// accepted, the provider has not been heard from, and the work item shown
// is the one uma holds.
func statusNotes(o workItemStatusOut) []string {
	if o.Local {
		return nil
	}
	notes := queuedNote(o.Operation)
	if o.WorkItem.Status == o.Status {
		return notes
	}
	return append(notes, fmt.Sprintf(
		"the work item changes only when the provider reports the change: `heo conversation get %s` shows it, `heo delivery get %s` the operation; "+
			"the conversation's own status is not affected",
		o.Conversation.Ref.ID, o.Operation.ID))
}

func statusRows(o workItemStatusOut) [][2]string {
	w := o.WorkItem
	asked := o.Status
	if o.Reason != "" {
		asked += ", reason " + o.Reason
	}
	status := fmt.Sprintf("%s (asked: %s)", orDash(w.RawStatus), asked)
	if !o.Local && w.Status != o.Status {
		status = fmt.Sprintf("%s (asked: %s; not applied until the provider reports it)", orDash(w.RawStatus), asked)
	}
	if o.Local {
		status = fmt.Sprintf("%s (set in uma)", w.Status)
	}
	return append([][2]string{
		{"conversation", string(o.Conversation.Ref.ID)},
		{"work item", string(w.Ref.ID)},
		{"reference", orDash(workItemRef(w))},
		{"status", status},
		{"revision", string(w.Revision)},
	}, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) conversationStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status <conversation>",
		Short: "Set the status of the conversation's issue or ticket",
		Long: `Set the status of the work item of the conversation <conversation>
(record ID) to --set: ` + strings.Join(desk.WorkItemStatuses(), ", ") + `, uma's statuses for what a tracker
calls open, pending, solved, closed and the like. A conversation of a
per_work_item inbox is an issue or a ticket; any other has no work item
and is refused.

This is not the conversation's own status: ` + "`conversation resolve`" + ` and
` + "`conversation reopen`" + ` move that, locally, and change nothing at the
provider. The two lifecycles are independent: setting the work item's
status never moves the conversation, and resolving the conversation never
sets the work item's.

The status is the provider's, so this is an external change: uma asks the
provider through the transitions the channel's adapter declares
(work_item_act). --reason names the reason the provider records for it,
one of those the adapter declares for the transition, such as completed or
not_planned for a close; without it the adapter's default is used.
` + "`channel get`" + ` and the verbs of ` + "`conversation get`" + ` show, per channel,
whether the verb is declared and which transitions.

The change is queued as a uma operation: the command returns once uma
accepted it durably, never applied. The daemon's workers send it to the
provider, and the work item shown by ` + "`conversation get`" + ` changes only when
the provider reports the change. The output says queued and what was asked,
never that the status is set. ` + "`delivery get <operation>`" + ` follows the
operation. Where the channel's configuration makes the status uma's own
(local_fields), it commits in uma at once and reaches no provider; a reason
is then refused.

--revision names the revision of the work item being changed, as
` + "`conversation get`" + ` shows it; without it heo sends the revision it reads
now. A work item that changed since is a conflict (exit 4) naming the
current revision.

A transition or a reason the adapter does not declare is refused
(UNSUPPORTED, exit 2), as is a channel whose adapter declares none and a
work item already in the status; --dry-run refuses them from the adapter's
declarations, and the real run by uma, before anything is written. A work
item the provider no longer binds (deleted, merged, moved) is a conflict
(exit 4). The channel's own configuration and the provider account's rights
are checked when the operation is applied; a failure shows on ` + "`delivery get`" + `.
--dry-run prints the exact change and where it goes, and writes and sends
nothing. Committed under --idempotency-key: rerunning with the same key,
status, reason and revision returns the same operation (replayed), so a
retry never sends a second change; without a key, heo mints one and prints
it. Desk role. With held operations on, a plugin actor's change of a status
the provider owns is held; one of a status uma owns cannot be held yet.`,
		Example: `  heo conversation status conv_123 --set closed --reason not_planned --dry-run
  heo conversation status conv_123 --set open --revision rev_9
  heo conversation status conv_123 --set resolved --idempotency-key fixed-123`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			status, _ := cmd.Flags().GetString(flagSet)
			reason, _ := cmd.Flags().GetString(flagReason)
			if err := desk.CheckStatus(status, reason); err != nil {
				return output.UsageError("--" + flagSet + ": " + strings.TrimPrefix(err.Error(), "validation: "))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[workItemStatusOut], error) {
				var w written[workItemStatusOut]
				err := a.deskHeld(ctx, c, model.ActionWorkItemAct, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanStatus(ctx, args[0], status, reason, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = statusPlan(p, key)
						return nil
					}
					h, err := s.HoldStatus(ctx, &p, statusPlan(p, key))
					if err != nil {
						return err
					}
					it, acc, err := s.SetStatus(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = workItemStatusOut{Conversation: p.Conversation, WorkItem: it, Status: p.Status, Reason: p.Reason, Local: p.Local,
						Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = statusNotes(w.Out)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, statusRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagSet, "", "Status to set: "+strings.Join(desk.WorkItemStatuses(), ", ")+" (required)")
	_ = c.MarkFlagRequired(flagSet)
	c.Flags().String(flagReason, "", "Why, as the provider records it, one the adapter declares for the transition (default: the adapter's)")
	c.Flags().String(flagRevision, "", "Revision of the work item being changed, from `conversation get` (default: the revision read now)")
	providerDependent(c, model.CapabilityWorkItemAct)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &workItemStatusOut{}, "1.0")
	return c
}

// updateOut is what a field update reports: the work item as uma holds it
// now, the fields asked for as they were sent, where they went, the
// operation and its key. A provider's fields are still as they were: they
// change when the provider reports them.
type updateOut struct {
	Conversation model.Conversation         `json:"conversation"      yaml:"conversation"`
	WorkItem     model.WorkItem             `json:"work_item"         yaml:"work_item"`
	Fields       map[string]json.RawMessage `json:"fields"            yaml:"fields"`
	Local        bool                       `json:"local,omitempty"   yaml:"local,omitempty"`
	Operation    operationView              `json:"operation"         yaml:"operation"`
	OperationKey string                     `json:"operation_key"     yaml:"operation_key"`
	Notes        []string                   `json:"notes,omitempty"   yaml:"notes,omitempty"`
}

func fieldMap(fs []desk.Field) map[string]json.RawMessage {
	m := make(map[string]json.RawMessage, len(fs))
	for _, f := range fs {
		m[f.Key] = f.Value
	}
	return m
}

func fieldText(fs []desk.Field) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.Key + "=" + string(f.Value)
	}
	return strings.Join(parts, " ")
}

// updatePlan renders a field update plan for --dry-run: the change in
// uma for fields it owns, otherwise the external change at the provider.
func updatePlan(p desk.UpdatePlan, key string) *cli.Plan {
	w := p.Item
	args := map[string]any{
		"conversation": p.Conversation.Ref.ID, "work_item": w.Ref.ID, "inbox": w.Ref.Conversation.Inbox.ID,
		"change": fieldText(p.Fields), "current": w,
	}
	if key != "" {
		args["operation_key"] = key
	}
	if p.Local() {
		args["expected_revision"] = p.Act.ExpectedRevision
		effect := planEffect("update", "work_item:"+string(w.Ref.ID), true, fmt.Sprintf(
			"fields %s, local to uma (they are uma's own on this channel; nothing changes at the provider); conversation %s stays %s",
			fieldText(p.Fields), p.Conversation.Ref.ID, p.Conversation.Status))
		return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: []string{"authorization", "work item", "binding", "revision", "request"}}
	}
	args["destination"] = w.External
	args["visibility"] = p.Effect.Audience.Visibility
	args["delivery"] = "queued, not applied: the work item changes in uma only when the provider reports it"
	checked := effectArgs(args, p.Declaration)
	effect := planEffect("update", "work_item:"+string(w.Ref.ID), true, fmt.Sprintf(
		"external: %s %s, sent through the adapter's update effect %s, which sets %s; "+
			"queued, not applied: the work item changes in uma only when the provider reports it, and conversation %s stays %s",
		w.External.Namespace, workItemRef(w), p.Effect.Action.Name, fieldText(p.Fields), p.Conversation.Ref.ID, p.Conversation.Status))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: append(checked, "work item", "binding", "route", "request")}
}

// effectArgs adds what a preview checked of the adapter to a plan's
// arguments, as declarationArgs does for a capability: the adapter, and
// the `capability` line naming the capability, the effect and what was
// not checked.
func effectArgs(args map[string]any, d *desk.EffectDeclaration) []string {
	checked := []string{"authorization"}
	if d == nil {
		return checked
	}
	args["adapter"] = d.Adapter
	args["capability"] = d.Line()
	return append(checked, "adapter capability and effect (declared)")
}

func updateRows(o updateOut) [][2]string {
	w := o.WorkItem
	rows := [][2]string{
		{"conversation", string(o.Conversation.Ref.ID)},
		{"work item", string(w.Ref.ID)},
		{"reference", orDash(workItemRef(w))},
	}
	keys := make([]string, 0, len(o.Fields))
	for k := range o.Fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		label := k + " asked"
		if o.Local {
			label = k + " set"
		}
		rows = append(rows, [2]string{label, string(o.Fields[k])})
	}
	rows = append(rows, [2]string{"revision", string(w.Revision)})
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func updateNotes(o updateOut) []string {
	if o.Local {
		return nil
	}
	return append(queuedNote(o.Operation), fmt.Sprintf(
		"the work item changes only when the provider reports the update: `heo conversation get %s` shows it, `heo delivery get %s` the operation; "+
			"the conversation and the work item's status are not affected",
		o.Conversation.Ref.ID, o.Operation.ID))
}

func (a *app) conversationUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update <conversation>",
		Short: "Change fields of the issue or ticket",
		Long: `Change the fields of the work item of the conversation <conversation>
(record ID) with --field key=value, repeated for several: assignees, labels,
a milestone, a ticket's tags and the like. Which keys exist, and what they
take, is the provider's and the adapter's, not heo's: heo passes each
through and the adapter, or uma, accepts or refuses it. A value that is
JSON (a number, true, null, a list, an object) is sent as such, anything
else as a string: --field milestone=3, --field status=solved,
--field 'labels=["bug","ui"]'. A key given twice is refused. The status of a
work item has its own verb, ` + "`conversation status`" + `.

Where a field goes is decided by who owns it on the conversation's
channel, which its configuration sets (local_fields):
  - a field uma owns changes in uma at once, as a local operation that
    reaches no provider, on any work item channel. subject and assignees
    are fields of their own; every other key is an entry of the work
    item's fields, named under the adapter's namespace (labels becomes
    <namespace>.labels). Assignees are provider IDs, a JSON list or
    comma separated.
  - every other field is the provider's. It goes through the update
    effect the channel's adapter ships for its work items, an external
    change queued like a reply, and the adapter validates the fields.
    An adapter that ships none leaves the work item ingest-only for the
    provider's fields: UNSUPPORTED, exit 2, and ` + "`conversation get`" + ` shows the
    verb unsupported on the channel.
One update is of one kind: fields of both owners are two updates.

The effect is queued as a uma operation: the command returns once uma
accepted it durably, never applied, and the work item shown by
` + "`conversation get`" + ` changes when the provider reports it. The output says
queued and what was asked, never that the work item has the labels.
` + "`delivery get <operation>`" + ` follows the operation. The conversation and the
work item's status are not touched. Whether the logins, labels and
milestone exist is the provider's answer, shown on the operation. --dry-run
asks the adapter, offline, to check the fields (VALIDATION, exit 2) and
prints the exact change and where it goes; it writes and sends nothing. A
work item the provider no longer binds is a conflict (exit 4). --revision
applies to the fields uma owns only: the work item's revision, from
` + "`conversation get`" + `; stale is a conflict. Committed under --idempotency-key:
rerunning with the same key and fields returns the same operation
(replayed), so a retry never sends a second update; without one, heo mints
a key and prints it. Desk role. With held operations on, a plugin actor's
update of the provider's fields is held, its trust class the sorted keys;
one of fields uma owns cannot be held yet.`,
		Example: `  heo conversation update conv_123 --field 'labels=["bug","ui"]' --dry-run
  heo conversation update conv_123 --field milestone=3 --field 'assignees=["ada"]'
  heo conversation update conv_123 --field status=solved --idempotency-key solve-123`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _ := cmd.Flags().GetStringArray(flagField)
			fields, err := desk.ParseFields(raw)
			if err != nil {
				return output.UsageError("--" + flagField + ": " + strings.TrimPrefix(err.Error(), "validation: "))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[updateOut], error) {
				var w written[updateOut]
				err := a.deskHeld(ctx, c, model.ActionWorkItemAct, internalAudience, func(s desk.Service) error {
					p, err := s.PlanUpdate(ctx, args[0], fields, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = updatePlan(p, key)
						return nil
					}
					h, err := s.HoldUpdate(ctx, &p, updatePlan(p, key))
					if err != nil {
						return err
					}
					it, acc, err := s.Update(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = updateOut{Conversation: p.Conversation, WorkItem: it, Fields: fieldMap(p.Fields), Local: p.Local(),
						Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = updateNotes(w.Out)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, updateRows, res.Out.Notes)
		},
	}
	c.Flags().StringArray(flagField, nil, "Field to set, key=value; repeat for several (required)")
	_ = c.MarkFlagRequired(flagField)
	c.Flags().String(flagRevision, "", "Revision of the work item, from `conversation get`, for fields uma owns (default: the revision read now)")
	providerDependent(c, model.CapabilityExtensionExecute)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &updateOut{}, "1.0")
	return c
}
