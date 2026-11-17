package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/output"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Flags the conversation, message and note leaves share.
const (
	flagInbox        = "inbox"
	flagConversation = "conversation"
	flagStatus       = "status"
	flagText         = "text"
	flagSince        = "since"
	flagUntil        = "until"
	flagSort         = "sort"
	flagUpdatedSince = "updated-since"
	flagPlacement    = "placement"
)

// internalAudience is the audience of internal notes: reading them is
// for desk actors alone.
var internalAudience = model.Audience{Visibility: model.Internal}

// desk runs fn in the daemon with the caller's desk, once heo's policy
// permits the caller action on content under audience (see withClient).
// Every message the desk returns passes the caller's audience rule
// again, so content that is not public never reaches a caller who is
// not a desk actor, on any surface.
func (a *app) desk(ctx context.Context, c control.Caller, action string, audience model.Audience, fn func(desk.Service) error) error {
	return a.deskGuarded(ctx, c, action, audience, false, fn)
}

// deskHeld is desk for a verb that offers its write to the hold point
// though its action is shared with verbs that do not (extension.execute:
// the mail unsubscribe holds, the modal verbs do not; work_item.act: the
// provider's status and update effect hold, a write of uma's own fields
// does not).
// The action-wide hold guard would refuse it, or, were the action listed
// as hold-aware, let the others through unreviewed; the verb itself
// answers for the hold, which Service.Hold refuses unless the write has a
// preview and uma can hold it.
func (a *app) deskHeld(ctx context.Context, c control.Caller, action string, audience model.Audience, fn func(desk.Service) error) error {
	return a.deskGuarded(ctx, c, action, audience, true, fn)
}

func (a *app) deskGuarded(ctx context.Context, c control.Caller, action string, audience model.Audience, holds bool, fn func(desk.Service) error) error {
	return a.withClient(ctx, c, action, audience, func(rt daemon.Runtime, client uma.Client, pol *authz.Policy) error {
		if !holds {
			if err := a.holdGuard(c, action); err != nil {
				return err
			}
		}
		s := deskOf(rt, client, pol, c)
		s.Holds = a.holds()
		if err := a.trustLadder(rt, c, &s); err != nil {
			return err
		}
		return fn(s)
	})
}

// deskOf is the caller's desk over the open runtime.
func deskOf(rt daemon.Runtime, client uma.Client, pol *authz.Policy, c control.Caller) desk.Service {
	p := c.Principal()
	return desk.Service{Client: client, Tenant: c.Tenant, Sees: func(au model.Audience) bool { return pol.Sees(p, au) },
		Adapter: channelAdapter(rt, c.Tenant), Capabilities: channelCapabilities(rt, c.Tenant), Effects: channelEffects(rt, c.Tenant),
		Channels:            channelList(rt, c.Tenant),
		ValidateAction:      channelActionValidator(rt, c.Tenant),
		RecipientNormalizer: channelRecipientNormalizer(rt, c.Tenant),
		// A modal's trigger_id lives seconds: queueing one wakes the
		// workers, and the interaction's own trigger is read as the
		// adapter reads it.
		Trigger: modalTrigger, Kick: rt.Nudge,
		// The authenticated caller, as the transport verified it: what
		// `--to self` and `--assignee self` mean.
		Actor: p.Actor,
		Permits: func(actor model.ActorID, action string) bool {
			ok, _ := pol.Permits(model.Principal{Tenant: p.Tenant, Actor: actor}, action)
			return ok
		}}
}

// channelAdapter describes the adapter of a channel in tenant, which the
// runtime reads as its own actor: a desk actor reaches an inbox but may
// not read its channel. Only the adapter's declarations leave it, as uma
// reports them when refusing a start the adapter cannot make.
func channelAdapter(rt daemon.Runtime, tenant model.TenantID) func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
	return func(ctx context.Context, ref model.ChannelRef) (adapter.Descriptor, error) {
		svc, err := rt.Engine.For(model.Principal{Tenant: tenant, Actor: daemon.ServiceActor})
		if err != nil {
			return adapter.Descriptor{}, err
		}
		ch, err := channels.Service{Client: svc, Tenant: tenant}.FindChannel(ctx, string(ref.ID))
		if err != nil {
			return adapter.Descriptor{}, err
		}
		ad, ok := rt.Adapters.Lookup(ch.Type)
		if !ok {
			return adapter.Descriptor{}, model.NewError(model.Unsupported, fmt.Sprintf("adapter %s is not in this binary", ch.Type))
		}
		return ad.Factory.Describe(), nil
	}
}

// capabilityReadTimeout bounds one channel's capability status read: an
// adapter that probes its provider's permissions calls out to it.
const capabilityReadTimeout = 10 * time.Second

// channelCapabilities reads the live per-capability status of a channel
// in tenant as the runtime reads it (the receivers read it the same way,
// as the runtime's own actor): a desk actor may not read the channel,
// and only the statuses leave it, as uma reports them for the channel.
func channelCapabilities(rt daemon.Runtime, tenant model.TenantID) func(context.Context, model.ChannelRef) ([]model.CapabilityStatus, error) {
	return func(ctx context.Context, ref model.ChannelRef) ([]model.CapabilityStatus, error) {
		svc, err := rt.Engine.For(model.Principal{Tenant: tenant, Actor: daemon.ServiceActor})
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, capabilityReadTimeout)
		defer cancel()
		return svc.Configuration().Capabilities(ctx, model.Lookup[model.ChannelRef]{Call: model.Call{Correlation: desk.Correlation}, Ref: ref})
	}
}

// channelList lists every channel of tenant, read as the runtime reads
// them: a desk actor may not read channels, and the provider label verbs
// name one by key or ID among those its own inboxes reach.
func channelList(rt daemon.Runtime, tenant model.TenantID) func(context.Context) ([]model.Channel, error) {
	return func(ctx context.Context) ([]model.Channel, error) {
		svc, err := rt.Engine.For(model.Principal{Tenant: tenant, Actor: daemon.ServiceActor})
		if err != nil {
			return nil, err
		}
		var out []model.Channel
		page := model.PageRequest{Limit: 100}
		for {
			p, err := channels.Service{Client: svc, Tenant: tenant}.ListChannels(ctx, page)
			if err != nil {
				return nil, err
			}
			out = append(out, p.Items...)
			if p.Complete || p.Next == "" {
				return out, nil
			}
			page.After = p.Next
		}
	}
}

// channelEffects returns the extension effects the adapter of a channel in
// tenant ships, read as the runtime reads it: the definitions heo
// registered beside the adapter, which an extension effect's verb checks
// before it plans one.
func channelEffects(rt daemon.Runtime, tenant model.TenantID) func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
	return func(ctx context.Context, ref model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
		svc, err := rt.Engine.For(model.Principal{Tenant: tenant, Actor: daemon.ServiceActor})
		if err != nil {
			return nil, err
		}
		ch, err := channels.Service{Client: svc, Tenant: tenant}.FindChannel(ctx, string(ref.ID))
		if err != nil {
			return nil, err
		}
		ad, ok := rt.Adapters.Lookup(ch.Type)
		if !ok {
			return nil, model.NewError(model.Unsupported, fmt.Sprintf("adapter %s is not in this binary", ch.Type))
		}
		return ad.Extensions, nil
	}
}

// channelActionValidator asks the adapter factory of a channel in
// tenant, read as the runtime reads it, whether it accepts an extension
// action's fields: adapter.ActionValidator, offline, when the factory
// implements it.
func channelActionValidator(rt daemon.Runtime, tenant model.TenantID) func(context.Context, model.ChannelRef, model.Audience, model.Extension) ([]model.ValidationIssue, error) {
	return func(ctx context.Context, ref model.ChannelRef, aud model.Audience, action model.Extension) ([]model.ValidationIssue, error) {
		svc, err := rt.Engine.For(model.Principal{Tenant: tenant, Actor: daemon.ServiceActor})
		if err != nil {
			return nil, err
		}
		ch, err := channels.Service{Client: svc, Tenant: tenant}.FindChannel(ctx, string(ref.ID))
		if err != nil {
			return nil, err
		}
		ad, ok := rt.Adapters.Lookup(ch.Type)
		if !ok {
			return nil, model.NewError(model.Unsupported, fmt.Sprintf("adapter %s is not in this binary", ch.Type))
		}
		v, ok := ad.Factory.(adapter.ActionValidator)
		if !ok {
			return nil, nil
		}
		return v.ValidateAction(aud, action.Clone()), nil
	}
}

// channelRecipientNormalizer returns the adapter factory of a channel in
// tenant, read as the runtime reads it, as an
// adapter.RecipientNormalizer, which a start's preview asks as uma's
// start does; nil for a factory without the interface.
func channelRecipientNormalizer(rt daemon.Runtime, tenant model.TenantID) func(context.Context, model.ChannelRef) (adapter.RecipientNormalizer, error) {
	return func(ctx context.Context, ref model.ChannelRef) (adapter.RecipientNormalizer, error) {
		svc, err := rt.Engine.For(model.Principal{Tenant: tenant, Actor: daemon.ServiceActor})
		if err != nil {
			return nil, err
		}
		ch, err := channels.Service{Client: svc, Tenant: tenant}.FindChannel(ctx, string(ref.ID))
		if err != nil {
			return nil, err
		}
		ad, ok := rt.Adapters.Lookup(ch.Type)
		if !ok {
			return nil, model.NewError(model.Unsupported, fmt.Sprintf("adapter %s is not in this binary", ch.Type))
		}
		n, _ := ad.Factory.(adapter.RecipientNormalizer)
		return n, nil
	}
}

// deskFilter reads --inbox, --conversation, --status and the label
// filters where the leaf has them.
func deskFilter(cmd *cobra.Command) desk.Filter {
	var f desk.Filter
	f.Inbox, _ = cmd.Flags().GetString(flagInbox)
	f.Conversation, _ = cmd.Flags().GetString(flagConversation)
	f.Statuses, _ = cmd.Flags().GetStringSlice(flagStatus)
	f.LabelAny, _ = cmd.Flags().GetStringSlice(flagLabel)
	f.LabelAll, _ = cmd.Flags().GetStringSlice(flagLabelAll)
	f.LabelNone, _ = cmd.Flags().GetStringSlice(flagLabelNone)
	f.Workflow = workflowFilterOf(cmd)
	f.Sort, _ = cmd.Flags().GetString(flagSort)
	return f
}

// workflowFilterOf reads the workflow filters of `conversation list`
// where the leaf has them: only the ones the caller gave are set.
func workflowFilterOf(cmd *cobra.Command) desk.WorkflowFilter {
	var w desk.WorkflowFilter
	if cmd.Flags().Lookup(flagUnread) == nil {
		return w
	}
	w.Unread, _ = cmd.Flags().GetBool(flagUnread)
	w.Assignee, _ = cmd.Flags().GetString(flagAssignee)
	w.Snoozed, _ = cmd.Flags().GetString(flagSnoozed)
	if l, _ := cmd.Flags().GetBool(flagLinked); l {
		w.Linked = &l
	}
	return w
}

// timeLayouts are the forms --since and --until take: an RFC 3339
// instant, or a date, which is midnight UTC.
var timeLayouts = []string{time.RFC3339Nano, time.DateOnly}

// timeFlag reads the instant flag name holds: nil when it is unset.
func timeFlag(cmd *cobra.Command, name string) (*time.Time, error) {
	if !cmd.Flags().Changed(name) {
		return nil, nil
	}
	v, _ := cmd.Flags().GetString(name)
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
			return &t, nil
		}
	}
	return nil, output.UsageError(fmt.Sprintf("--%s %q: want an RFC 3339 time (2026-10-01T09:00:00Z) or a date (2026-10-01)", name, v))
}

// searchFilter adds to f the message search flags the leaf has: --text
// and the time bounds. Their meaning is uma's; heo reads them and checks
// that a search flag that was given is not blank.
func searchFilter(cmd *cobra.Command, f desk.Filter) (desk.Filter, error) {
	if cmd.Flags().Changed(flagText) {
		f.Text, _ = cmd.Flags().GetString(flagText)
		if strings.TrimSpace(f.Text) == "" {
			return f, output.UsageError("--" + flagText + ": the words to search for are required")
		}
	}
	var err error
	if f.Since, err = timeFlag(cmd, flagSince); err != nil {
		return f, err
	}
	f.Until, err = timeFlag(cmd, flagUntil)
	return f, err
}

// addTextFlag adds the required --text a write sends.
func addTextFlag(c *cobra.Command, what string) {
	c.Flags().String(flagText, "", what+" (required)")
	_ = c.MarkFlagRequired(flagText)
}

// conversationRow is a conversation in a listing table.
type conversationRow struct {
	ID       string `json:"id"       yaml:"id"       table:"ID,id"`
	Inbox    string `json:"inbox"    yaml:"inbox"    table:"INBOX"`
	Status   string `json:"status"   yaml:"status"   table:"STATUS"`
	Mode     string `json:"mode"     yaml:"mode"     table:"MODE"`
	WorkItem string `json:"work_item" yaml:"work_item" table:"WORK ITEM"`
	Assignee string `json:"assignee" yaml:"assignee" table:"ASSIGNEE"`
	Snoozed  string `json:"snoozed"  yaml:"snoozed"  table:"SNOOZED UNTIL"`
	Created  string `json:"created"  yaml:"created"  table:"CREATED"`
	Activity string `json:"last_activity" yaml:"last_activity" table:"LAST ACTIVITY"`
	Revision string `json:"revision" yaml:"revision" table:"REVISION"`
}

func conversationRowOf(c model.Conversation, w *model.WorkItem, activity time.Time) conversationRow {
	row := conversationRow{Activity: stamp(activity),
		ID: string(c.Ref.ID), Inbox: string(c.Ref.Inbox.ID), Status: c.Status, Mode: c.Mode, WorkItem: workItemCell(w), Assignee: "-", Snoozed: "-",
		Created: stamp(c.CreatedAt), Revision: string(c.Revision),
	}
	if c.Assignee != nil {
		row.Assignee = string(*c.Assignee)
	}
	if c.Snooze != nil {
		row.Snoozed = stamp(c.Snooze.Until)
	}
	return row
}

func conversationRows(c model.Conversation) [][2]string { return conversationRowsFor(c, false) }

// conversationRowsFor is conversationRows with, for a caller that may read
// workflow state, a dash where the conversation has no assignee, snooze
// or link: elsewhere a row shows only what is set, so a reader that may
// not read the state is not told there is none.
func conversationRowsFor(c model.Conversation, reads bool) [][2]string {
	labels := make([]string, 0, len(c.Labels))
	for _, l := range c.Labels {
		labels = append(labels, string(l.ID))
	}
	rows := [][2]string{
		{"id", string(c.Ref.ID)},
		{"inbox", string(c.Ref.Inbox.ID)},
		{"status", c.Status},
		{"mode", c.Mode},
		{"participants", fmt.Sprint(len(c.Participants))},
		{"reply route", routeName(c.ReplyRoute)},
		{"labels", orDash(strings.Join(labels, ", "))},
	}
	rows = append(rows, workflowStateRows(c, reads)...)
	return append(rows,
		[2]string{"created", stamp(c.CreatedAt)},
		[2]string{"revision", string(c.Revision)},
	)
}

// routeName is how output names a provider route: its kind and ID.
func routeName(r model.ExternalRef) string {
	return orDash(strings.TrimSpace(r.Kind + " " + r.ID))
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// messageRow is a message in a listing table: where it is, who can see
// it and where it came from.
type messageRow struct {
	ID           string `json:"id"           yaml:"id"           table:"ID,id"`
	Conversation string `json:"conversation" yaml:"conversation" table:"CONVERSATION"`
	Visibility   string `json:"visibility"   yaml:"visibility"   table:"VISIBILITY"`
	Origin       string `json:"origin"       yaml:"origin"       table:"ORIGIN"`
	Occurred     string `json:"occurred"     yaml:"occurred"     table:"OCCURRED"`
	Text         string `json:"text"         yaml:"text"         table:"TEXT"`
}

// previewLen bounds the text a listing shows of each message.
const previewLen = 60

func messageRowOf(m model.Message) messageRow {
	text := strings.Join(strings.Fields(desk.Text(m.Content)), " ")
	if r := []rune(text); len(r) > previewLen {
		text = string(r[:previewLen-1]) + "…"
	}
	return messageRow{
		ID: string(m.Ref.ID), Conversation: string(m.Ref.Conversation.ID), Visibility: string(m.Audience.Visibility),
		Origin: messageOrigin(m), Occurred: stamp(m.OccurredAt), Text: orDash(text),
	}
}

// messageOrigin is where m came from: a uma operation and the actor who
// submitted it, or the provider.
func messageOrigin(m model.Message) string {
	if m.Origin != nil {
		return "uma " + string(m.Origin.Actor)
	}
	if m.Source != "" {
		return "inbound " + string(m.Source)
	}
	return "inbound"
}

func messageRows(m model.Message) [][2]string {
	rows := [][2]string{
		{"id", string(m.Ref.ID)},
		{"conversation", string(m.Ref.Conversation.ID)},
		{"inbox", string(m.Ref.Conversation.Inbox.ID)},
		{"visibility", string(m.Audience.Visibility)},
		{"origin", messageOrigin(m)},
	}
	if m.Origin != nil {
		rows = append(rows, [2]string{"operation", string(m.Origin.Operation.ID)})
	}
	if m.InReplyTo != nil {
		rows = append(rows, [2]string{"in reply to", string(m.InReplyTo.ID)})
	}
	rows = append(rows,
		[2]string{"occurred", stamp(m.OccurredAt)},
		[2]string{"recorded", stamp(m.RecordedAt)},
		[2]string{"revision", string(m.Revision)},
	)
	if m.LastEdit != nil {
		rows = append(rows, [2]string{"edited", stamp(m.LastEdit.At)})
	}
	if m.Deleted {
		rows = append(rows, [2]string{"deleted", "yes (a tombstone; the provider deleted it)"})
	}
	return append(rows, [2]string{"text", orDash(desk.Text(m.Content))})
}

// queuedNote is what a queued write says about delivery: acceptance is
// durable, delivery is still to come, and the operation follows it.
func queuedNote(op operationView) []string {
	if op.State != model.Queued {
		return nil
	}
	accepted := "accepted"
	if op.Durable {
		accepted = "accepted durably"
	}
	return []string{fmt.Sprintf("queued: %s; the daemon's workers deliver it next (operation %s)", accepted, op.ID)}
}

// placementFailure is a note whose placement is not its inbox's, with
// the way to fix either side.
func (a *app) placementFailure(err error) error {
	var pe *desk.PlacementError
	if !errors.As(err, &pe) {
		return err
	}
	e := errmap.Of(err)
	in := pe.Inbox
	e.SuggestedFix = fmt.Sprintf("pass --placement %s, or change where the inbox places notes: `%s`",
		desk.InboxPlacement(in), a.heoCommand(fmt.Sprintf("inbox update %s --revision %s --deliver-notes=%v",
			channels.Name(in.Key, in.Ref.ID), in.Revision, pe.Want == desk.PlacementProvider)))
	return e
}

// planKeys are the plan arguments a desk write's preview prints after
// its effects, in this order: where the write goes and who can see it,
// or what an identity link moves and on what evidence.
var planKeys = []string{
	"identity", "work_item", "message", "interaction", "conversation", "inbox", "channel", "adapter", "capability", "destination", "recipients", "recipients_capability", "recipient_limits", "as_typed", "visibility", "audience",
	"mode", "text", "emoji", "follow_up", "expires_at", "placement", "since", "limit", "cursor",
	"name", "kind", "attributes", "from", "into", "contact", "class", "state", "status", "change", "resolved", "labels", "identities",
	"assignee", "until", "wake", "far", "current", "new",
	"conversations", "evidence", "assurance", "reason", "reference",
	"expected_revision", "expected_into_revision", "delivery", "operation_key",
}

// renderDeskWrite renders a desk write as written.render does; a
// preview in text also prints its destination and visibility as rows,
// which the plan's effects table may leave out on a narrow terminal.
func renderDeskWrite[R any](a *app, cmd *cobra.Command, w written[R], rows func(R) [][2]string, notes []string) error {
	if err := w.render(a, cmd, rows, notes); err != nil || w.Plan == nil || a.structured() {
		return err
	}
	var out [][2]string
	for _, k := range planKeys {
		if v, ok := w.Plan.Args[k]; ok {
			out = append(out, [2]string{strings.ReplaceAll(k, "_", " "), planValue(v)})
		}
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
		return err
	}
	return writeRows(cmd.OutOrStdout(), out)
}

// planValue prints a plan argument: a route by kind and ID, whether the
// plan was made here or decoded from the daemon's answer.
func planValue(v any) string {
	switch x := v.(type) {
	case model.ExternalRef:
		return routeName(x)
	case map[string]any:
		kind, _ := x["kind"].(string)
		id, _ := x["id"].(string)
		return routeName(model.ExternalRef{Kind: kind, ID: id})
	}
	return orDash(fmt.Sprint(v))
}
