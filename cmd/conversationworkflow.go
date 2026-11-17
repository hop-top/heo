package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags the workflow leaves use.
const (
	flagFor     = "for"
	flagThrough = "through"
	flagUnread  = "unread"
	// flagAssignee, flagSnoozed and flagLinked are the workflow filters
	// of `conversation list`.
	flagAssignee = "assignee"
	flagSnoozed  = "snoozed"
	flagLinked   = "linked"
)

// workflowOut is what a workflow change reports: the conversation as it
// is after it, whether the call changed anything, the operation and its
// key.
type workflowOut struct {
	Conversation model.Conversation `json:"conversation"    yaml:"conversation"`
	Changed      bool               `json:"changed"         yaml:"changed"`
	Operation    operationView      `json:"operation"       yaml:"operation"`
	OperationKey string             `json:"operation_key"   yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty" yaml:"notes,omitempty"`
}

func workflowRows(o workflowOut) [][2]string {
	return append(conversationRows(o.Conversation), operationRows(o.Operation, o.OperationKey)...)
}

// addOptionalRevisionFlag adds --revision for a change that applies to
// whatever the conversation is when none is named: the revision from
// `conversation get`, when the caller wants the change refused if the
// conversation moved since. A retry under the same key replays only with
// the same revision.
func addOptionalRevisionFlag(c *cobra.Command) {
	c.Flags().String(flagRevision, "", "Apply only if the conversation is still at this revision, from `conversation get`; stale is a conflict (exit 4)")
}

// workflowPlan is the preview of a conversation workflow change.
func workflowPlan(cv model.Conversation, rev model.Revision, key string, extra map[string]any, effects []cli.Effect, warnings []string, checked ...string) *cli.Plan {
	args := map[string]any{"conversation": cv.Ref.ID, "inbox": cv.Ref.Inbox.ID}
	if rev != "" {
		args["expected_revision"] = rev
	}
	if key != "" {
		args["operation_key"] = key
	}
	for k, v := range extra {
		args[k] = v
	}
	return &cli.Plan{Args: args, Effects: effects, PrerequisitesChecked: append([]string{"authorization", "conversation"}, append(checked, "request")...), Warnings: warnings}
}

// assignedText says who holds cv.
func assignedText(cv model.Conversation) string {
	if cv.Assignee == nil {
		return "unassigned"
	}
	return "assigned to " + string(*cv.Assignee)
}

func (a *app) conversationAssignCmd(unassign bool) *cobra.Command {
	verb, short := "assign", "Assign a conversation to a desk actor (local; nothing changes at the provider)"
	long := `Assign the conversation <conversation> (record ID) to the desk actor --to
names: an actor ID such as uid:1001, or ` + "`self`" + `, the actor that runs the
command — heo takes it from the authenticated caller, never from the
command line. A conversation has one assignee: assigning replaces the
current one, which --dry-run shows. Only an actor that holds the desk role
may be assigned (anyone else is refused, exit 5). Assigning the current
assignee changes nothing and succeeds.`
	example := "  heo conversation assign conv_123 --to self\n  heo conversation assign conv_123 --to uid:1002 --revision rev_9\n  heo conversation assign conv_123 --to self --dry-run"
	if unassign {
		verb, short = "unassign", "Clear a conversation's assignee (local)"
		long = `Clear the assignee of the conversation <conversation> (record ID). The
event the change emits records who cleared it. Unassigning a conversation
nobody holds changes nothing and succeeds.`
		example = "  heo conversation unassign conv_123\n  heo conversation unassign conv_123 --dry-run"
	}
	c := &cobra.Command{
		Use:   verb + " <conversation>",
		Short: short,
		Long: long + `

Assignment is local to uma and part of the conversation's revision, so a
change made between your ` + "`conversation get`" + ` and a later write at that
revision makes the later write a conflict (exit 4), as for labels. --revision
applies the change only if the conversation is still at that revision; a
stale one is a conflict that names who holds the conversation now. Every
change is an event in the log (` + "`event list --kind conversation.assigned`" + `),
desk only; no provider is told.

Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed); without
one, heo mints a key and prints it. --dry-run prints the change and
writes nothing. Desk role.`,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			to, _ := cmd.Flags().GetString(flagTo)
			if !unassign && strings.TrimSpace(to) == "" {
				return output.UsageError("--" + flagTo + ": an actor ID, or self, is required")
			}
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[workflowOut], error) {
				var w written[workflowOut]
				err := a.desk(ctx, c, model.ActionConversationAssign, internalAudience, func(s desk.Service) error {
					p, err := s.PlanAssign(ctx, args[0], to, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					cv := p.Conversation
					if cli.IsDryRun(cmd) {
						detail := fmt.Sprintf("%s -> %s, local to uma (nothing changes at the provider); uma emits conversation.%sed",
							assignedText(cv), map[bool]string{true: "unassigned", false: "assigned to " + string(p.Assignee)}[unassign], verb)
						w.Plan = workflowPlan(cv, rev, key, map[string]any{"assignee": orDash(string(p.Assignee)), "current": assignedText(cv)},
							[]cli.Effect{planEffect("update", "conversation:"+string(cv.Ref.ID), true, detail)}, p.Warnings, "assignee", "revision")
						return nil
					}
					conv, acc, err := s.Assign(ctx, p)
					if err != nil {
						return err
					}
					w.Out = workflowOut{Conversation: conv, Changed: conv.Revision != cv.Revision, Operation: operationOf(acc), OperationKey: key, Notes: p.Warnings}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, workflowRows, res.Out.Notes)
		},
	}
	if !unassign {
		c.Flags().String(flagTo, "", "Actor to assign, such as uid:1001, or self for the caller (required)")
	}
	addOptionalRevisionFlag(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &workflowOut{}, "1.0")
	return c
}

// durationFlag reads --for: a Go duration (90m, 2h30m) or whole days
// (3d). It is positive.
func durationFlag(cmd *cobra.Command, name string) (time.Duration, error) {
	v, _ := cmd.Flags().GetString(name)
	v = strings.TrimSpace(v)
	bad := output.UsageError(fmt.Sprintf("--%s %q: want a positive duration such as 90m, 2h or 3d", name, v))
	var d time.Duration
	if days, ok := strings.CutSuffix(v, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 3650 {
			return 0, bad
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(v); err != nil || d <= 0 {
			return 0, bad
		}
	}
	return d, nil
}

func snoozeText(cv model.Conversation) string {
	if cv.Snooze == nil {
		return "not snoozed"
	}
	return "snoozed until " + stamp(cv.Snooze.Until)
}

func (a *app) conversationSnoozeCmd(unsnooze bool) *cobra.Command {
	verb, short := "snooze", "Hide a conversation until a time or an inbound message (local)"
	long := `Snooze the conversation <conversation> (record ID) until --until (an
RFC 3339 time, or a date, which is midnight UTC) or for --for (a duration
such as 90m, 2h or 3d, resolved by heo's clock when the command runs): one
of the two, in the future (otherwise exit 2). A snoozed conversation is
hidden from ` + "`conversation list`" + ` (see --snoozed there) and wakes at the
time, or when a live inbound message is recorded after the snooze, whichever is
first; history imported from a provider never wakes it. Nothing is
scheduled: uma works the wake out when it reads the conversation, so a
snooze survives a daemon restart and waking emits no event. A snooze is
independent of the conversation's status and local to uma: nothing changes
at the provider. Snoozing again replaces the snooze. The output and
--dry-run show the resolved time; rerunning with --for resolves a later
time, so replay a command with --until.`
	example := "  heo conversation snooze conv_123 --for 2h\n  heo conversation snooze conv_123 --until 2026-10-20T09:00:00Z --dry-run"
	if unsnooze {
		verb, short = "unsnooze", "End a conversation's snooze early (local)"
		long = `End the snooze of the conversation <conversation> (record ID) now: it
shows in the default list again. Unsnoozing a conversation that is not
snoozed (none, expired, or woken by an inbound message) changes nothing and
succeeds.`
		example = "  heo conversation unsnooze conv_123\n  heo conversation unsnooze conv_123 --dry-run"
	}
	c := &cobra.Command{
		Use:   verb + " <conversation>",
		Short: short,
		Long: long + `

--revision applies the change only if the conversation is still at that
revision, from ` + "`conversation get`" + `: a stale one is a conflict (exit 4).
Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed); without
one, heo mints a key and prints it. --dry-run prints the change and
writes nothing. Desk role.`,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			var until *time.Time
			var forD time.Duration
			if !unsnooze {
				if until, err = timeFlag(cmd, flagUntil); err != nil {
					return err
				}
				if cmd.Flags().Changed(flagFor) {
					if forD, err = durationFlag(cmd, flagFor); err != nil {
						return err
					}
				}
			}
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[workflowOut], error) {
				var w written[workflowOut]
				err := a.desk(ctx, c, model.ActionConversationSnooze, internalAudience, func(s desk.Service) error {
					var p desk.SnoozePlan
					var err error
					if unsnooze {
						p, err = s.PlanUnsnooze(ctx, args[0], rev, opID(key), planMode(cmd))
					} else {
						p, err = s.PlanSnooze(ctx, args[0], until, forD, rev, opID(key), planMode(cmd))
					}
					if err != nil {
						return err
					}
					cv := p.Conversation
					if cli.IsDryRun(cmd) {
						extra := map[string]any{"current": snoozeText(cv)}
						detail := "ends the snooze now, local to uma (nothing changes at the provider); uma emits conversation.unsnoozed"
						if !unsnooze {
							extra["until"], extra["wake"] = p.Until.Format(time.RFC3339), desk.WakeRule
							detail = fmt.Sprintf("%s -> snoozed until %s; %s; local to uma (nothing changes at the provider); uma emits conversation.snoozed",
								snoozeText(cv), p.Until.Format(time.RFC3339), desk.WakeRule)
						}
						w.Plan = workflowPlan(cv, rev, key, extra,
							[]cli.Effect{planEffect("update", "conversation:"+string(cv.Ref.ID), true, detail)}, p.Warnings, "until", "revision")
						return nil
					}
					conv, acc, err := s.Snooze(ctx, p)
					if err != nil {
						return err
					}
					w.Out = workflowOut{Conversation: conv, Changed: conv.Revision != cv.Revision, Operation: operationOf(acc), OperationKey: key, Notes: p.Warnings}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, workflowRows, res.Out.Notes)
		},
	}
	if !unsnooze {
		c.Flags().String(flagUntil, "", "Snooze until this RFC 3339 time, or date (midnight UTC)")
		c.Flags().String(flagFor, "", "Snooze for this long from now: 90m, 2h, 3d")
		c.MarkFlagsOneRequired(flagUntil, flagFor)
		c.MarkFlagsMutuallyExclusive(flagUntil, flagFor)
	}
	addOptionalRevisionFlag(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &workflowOut{}, "1.0")
	return c
}

// linkName is how output names a link's far end.
func linkName(l model.ConversationLink) string {
	switch {
	case l.Conversation != nil:
		return "conversation " + string(l.Conversation.ID)
	case l.WorkItem != nil:
		return "work item " + string(l.WorkItem.ID)
	}
	return "?"
}

func (a *app) conversationLinkCmd(unlink bool) *cobra.Command {
	verb, short := "link", "Link a conversation to another conversation or a work item (local)"
	long := `Link the conversation <conversation> (record ID) to exactly one of: another
conversation (--conversation) or a work item, an issue or a ticket
(--work-item), by record ID; ` + "`conversation get`" + ` shows a conversation's work
item and its ID. A link is association only: it merges no history, changes
no reply scope, allows no reply across inboxes and tells no provider.
Between two conversations it is symmetric: both hold it and both show it
(` + "`conversation get`" + `); a link to a work item is held by the
conversation (the work item's own ` + "`conversation get`" + ` lists the conversations
that link to it). ` + "`conversation list --linked`" + ` finds the linked ones.

You need the right on both ends: a conversation or work item you may
not read is not found (exit 3), exactly as one that does not exist.
Linking what is already linked changes nothing and succeeds. A
conversation holds at most 64 links.`
	example := "  heo conversation link conv_123 --conversation conv_456\n  heo conversation link conv_123 --work-item wi_7 --dry-run\n  heo conversation link conv_123 --work-item wi_9"
	if unlink {
		verb, short = "unlink", "Remove a link between a conversation and another conversation or a work item (local)"
		long = `Remove the link between the conversation <conversation> (record ID) and
exactly one of: another conversation (--conversation) or a work item, an
issue or a ticket (--work-item). Between two conversations both let go. You need the
same rights as to link. Removing a link that is not held changes nothing
and succeeds.`
		example = "  heo conversation unlink conv_123 --conversation conv_456\n  heo conversation unlink conv_123 --work-item wi_7 --dry-run"
	}
	c := &cobra.Command{
		Use:   verb + " <conversation>",
		Short: short,
		Long: long + `

--revision applies the change only if the conversation is still at that
revision, from ` + "`conversation get`" + `: a stale one is a conflict (exit 4).
Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed); without
one, heo mints a key and prints it. --dry-run shows both ends and writes
nothing. Desk role.`,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			var end desk.LinkEnd
			end.Conversation, _ = cmd.Flags().GetString(flagConversation)
			end.WorkItem, _ = cmd.Flags().GetString(flagWorkItem)
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[workflowOut], error) {
				var w written[workflowOut]
				err := a.desk(ctx, c, model.ActionConversationLink, internalAudience, func(s desk.Service) error {
					p, err := s.PlanConversationLink(ctx, args[0], end, unlink, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					cv := p.Conversation
					if cli.IsDryRun(cmd) {
						w.Plan = conversationLinkPlan(p, rev, key, verb)
						return nil
					}
					conv, acc, err := s.ApplyConversationLink(ctx, p)
					if err != nil {
						return err
					}
					w.Out = workflowOut{Conversation: conv, Changed: conv.Revision != cv.Revision, Operation: operationOf(acc), OperationKey: key, Notes: p.Warnings}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, workflowRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagConversation, "", "Far end: another conversation (record ID)")
	c.Flags().String(flagWorkItem, "", "Far end: a work item, an issue or a ticket (record ID, from `conversation get`)")
	c.MarkFlagsOneRequired(flagConversation, flagWorkItem)
	c.MarkFlagsMutuallyExclusive(flagConversation, flagWorkItem)
	addOptionalRevisionFlag(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &workflowOut{}, "1.0")
	return c
}

// conversationLinkPlan previews a link change on both ends.
func conversationLinkPlan(p desk.ConversationLinkPlan, rev model.Revision, key, verb string) *cli.Plan {
	cv := p.Conversation
	what := "adds a link to"
	if verb == "unlink" {
		what = "removes the link to"
	}
	effects := []cli.Effect{planEffect("update", "conversation:"+string(cv.Ref.ID), true, fmt.Sprintf(
		"%s %s, local to uma (nothing changes at a provider, no history merges, no reply scope changes); uma emits conversation.%sed",
		what, p.FarName(), verb))}
	far := p.FarName()
	if p.Far != nil {
		far += " (inbox " + string(p.Far.Ref.Inbox.ID) + ")"
		effects = append(effects, planEffect("update", "conversation:"+string(p.Far.Ref.ID), true, fmt.Sprintf(
			"%s conversation %s as well: a link between conversations is held by both; uma emits conversation.%sed there too", what, cv.Ref.ID, verb)))
	} else {
		effects[0].Detail += "; the work item holds no link, the conversation alone does"
	}
	held := "no"
	if p.Held {
		held = "yes"
	}
	return workflowPlan(cv, rev, key, map[string]any{"far": far, "current": "linked: " + held},
		effects, p.Warnings, "far end (readable by you)", "revision")
}

// readMarkOut is what moving a read marker reports: the caller's marker
// as it is after the call, and whether the call changed it.
type readMarkOut struct {
	Conversation model.ID      `json:"conversation"          yaml:"conversation"`
	Actor        model.ActorID `json:"actor"                 yaml:"actor"`
	ReadThrough  time.Time     `json:"read_through"          yaml:"read_through"`
	UpdatedAt    time.Time     `json:"updated_at"            yaml:"updated_at"`
	Revision     string        `json:"revision"              yaml:"revision"`
	Changed      bool          `json:"changed"               yaml:"changed"`
	Notes        []string      `json:"notes,omitempty"       yaml:"notes,omitempty"`
}

func readMarkRows(o readMarkOut) [][2]string {
	return [][2]string{
		{"conversation", string(o.Conversation)},
		{"actor", string(o.Actor)},
		{"read through", stamp(o.ReadThrough)},
		{"updated", stamp(o.UpdatedAt)},
		{"changed", map[bool]string{true: "yes", false: "no"}[o.Changed]},
	}
}

func markText(m model.ReadMarker) string {
	if m.Revision == "" {
		return "none (never marked: every message is unread)"
	}
	return "read through " + stamp(m.ReadThrough)
}

func (a *app) conversationMarkCmd(unread bool) *cobra.Command {
	verb, short, flag := "mark-read", "Mark a conversation read for you", flagThrough
	long := `Mark the conversation <conversation> (record ID) read for you: every
message recorded until now, or, with --through <message> (record ID, in
that conversation), the messages up to and including it.`
	example := "  heo conversation mark-read conv_123\n  heo conversation mark-read conv_123 --through msg_42\n  heo conversation mark-read conv_123 --dry-run"
	if unread {
		verb, short, flag = "mark-unread", "Mark a conversation unread for you", flagFrom
		long = `Mark the conversation <conversation> (record ID) unread for you: from
the newest message the provider delivered live, or, with --from <message>
(record ID, in that conversation), from that message on. Messages imported
from a provider's history never count as unread, so marking before one
changes nothing.`
		example = "  heo conversation mark-unread conv_123\n  heo conversation mark-unread conv_123 --from msg_42\n  heo conversation mark-unread conv_123 --dry-run"
	}
	c := &cobra.Command{
		Use:   verb + " <conversation>",
		Short: short,
		Long: long + `

Read state is yours alone: another actor's marker is independent, and no
one reads it. It is local to uma — it sends no receipt to the provider
(` + "`receipt send`" + ` does that), changes neither the conversation nor its
revision, and emits no event. ` + "`conversation list --unread`" + ` lists what is unread
for you; ` + "`conversation get`" + ` shows your marker. The call is idempotent: the
same mark again changes nothing. --dry-run shows your current mark and the
new one. Desk role.`,
		Example: example,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			msg, _ := cmd.Flags().GetString(flag)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[readMarkOut], error) {
				var w written[readMarkOut]
				err := a.desk(ctx, c, model.ActionConversationMarkRead, internalAudience, func(s desk.Service) error {
					p, err := s.PlanReadMark(ctx, args[0], msg, unread)
					if err != nil {
						return err
					}
					cv := p.Conversation
					if cli.IsDryRun(cmd) {
						next := "now (every message recorded until the call)"
						switch {
						case p.Through != nil:
							next = "read through " + stamp(*p.Through)
						case unread:
							next = "before the newest live inbound message that was not imported, as uma finds it when the call commits"
						}
						args := map[string]any{"conversation": cv.Ref.ID, "inbox": cv.Ref.Inbox.ID, "current": markText(p.Current), "new": next}
						if p.Message != nil {
							args["message"] = p.Message.Ref.ID
						}
						w.Plan = &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "read_marker:"+string(cv.Ref.ID), true, fmt.Sprintf(
							"your read marker: %s -> %s; yours alone, local to uma (no receipt reaches the provider, the conversation's revision does not move, no event)",
							markText(p.Current), next))}, PrerequisitesChecked: []string{"authorization", "conversation", "message", "request"}}
						return nil
					}
					m, err := s.ApplyReadMark(ctx, p)
					if err != nil {
						return err
					}
					w.Out = readMarkOut{Conversation: cv.Ref.ID, Actor: m.Actor, ReadThrough: m.ReadThrough, UpdatedAt: m.UpdatedAt,
						Revision: string(m.Revision), Changed: m.Revision != p.Current.Revision}
					if !w.Out.Changed {
						w.Out.Notes = []string{"your marker was already there: nothing changed"}
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, readMarkRows, res.Out.Notes)
		},
	}
	if unread {
		c.Flags().String(flagFrom, "", "Message (record ID) from which on the conversation is unread")
	} else {
		c.Flags().String(flagThrough, "", "Message (record ID) through which the conversation is read; default: everything recorded until now")
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &readMarkOut{}, "1.0")
	return c
}

// workflowStateRows is what a conversation shows of its workflow state:
// assignee, snooze and links, where there is any, and, for a caller that
// may read it, a dash where there is none.
func workflowStateRows(c model.Conversation, reads bool) [][2]string {
	var rows [][2]string
	if c.Assignee != nil {
		rows = append(rows, [2]string{"assignee", string(*c.Assignee)})
	} else if reads {
		rows = append(rows, [2]string{"assignee", "-"})
	}
	if s := c.Snooze; s != nil {
		rows = append(rows, [2]string{"snoozed", fmt.Sprintf("until %s (set by %s at %s)", stamp(s.Until), s.By, stamp(s.At))})
	} else if reads {
		rows = append(rows, [2]string{"snoozed", "-"})
	}
	if len(c.Links) > 0 {
		names := make([]string, 0, len(c.Links))
		for _, l := range c.Links {
			names = append(names, linkName(l))
		}
		rows = append(rows, [2]string{"links", strings.Join(names, ", ")})
	} else if reads {
		rows = append(rows, [2]string{"links", "-"})
	}
	return rows
}
