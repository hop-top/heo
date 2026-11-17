package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// conversationOut is what a conversation transition reports: the
// conversation as it is after it, the operation and its key.
type conversationOut struct {
	Conversation model.Conversation `json:"conversation"    yaml:"conversation"`
	Operation    operationView      `json:"operation"       yaml:"operation"`
	OperationKey string             `json:"operation_key"   yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty" yaml:"notes,omitempty"`
}

func (a *app) conversationCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "conversation",
		Short: "Read conversations across inboxes, move them through their lifecycle and change their issue or ticket",
		Long: `Read the conversations of every inbox you may read, and resolve or
reopen them. A conversation is uma's local record: its status (open,
pending, resolved, closed) is local, and resolving one changes nothing
at the provider — no ticket or thread closes.

An issue or a ticket is a conversation: a tracker's issue is a thread, its
comments are messages, and a conversation of a per_work_item inbox is bound
to the work item it is. ` + "`list`" + ` and ` + "`get`" + ` show the work item beside the
conversation: its status, the provider's own, its assignees and fields, its
URL and its binding. ` + "`status`" + ` sets its status at the provider and ` + "`update`" + `
changes its fields, each through what the channel's adapter declares, and
refused where it declares nothing; ` + "`conversation get`" + ` shows per channel
which verbs it declares. The work item's lifecycle is independent of the
conversation's: ` + "`status`" + ` never moves the conversation, and ` + "`resolve`" + ` never
closes the issue. Tracker labels are the provider's own: ` + "`update`" + ` sets
them, and they are not the labels below.

Conversations hold labels, uma's tenant-wide records (` + "`label`" + `):
` + "`conversation label`" + ` adds and removes them, and ` + "`conversation list`" + `
filters by them.

The desk also works them: ` + "`assign`" + ` and ` + "`unassign`" + ` name who holds one,
` + "`snooze`" + ` and ` + "`unsnooze`" + ` hide it until a time or an inbound message,
` + "`link`" + ` and ` + "`unlink`" + ` relate it to another conversation or a work item, and ` + "`mark-read`" + ` and ` + "`mark-unread`" + ` keep your own read marker. All
of it is uma's, local — nothing changes at a provider — and desk-only on
every surface: an external actor sees none of it.

` + "`tag`" + ` is the exception: it changes the tags a mail provider keeps on the
message a conversation replies to (Gmail's labels, Outlook's categories),
an external change queued like a reply; uma records none of them, and
they are not the labels above. ` + "`unsubscribe`" + ` is another: it asks the sender of
that message to stop mailing the mailbox, by what its own headers allow,
and cannot be undone.

` + "`archive`, `unarchive`, `move`, `read`, `unread`, `flag`, `unflag`, `trash` and `untrash`" + ` change what the
provider itself holds about a conversation: a mailbox's archive, folder or
labels, read state, star and trash, a chat room's archive. They are external changes
queued like a reply, each through the capability its channel's adapter declares
and the limits it reports; uma follows once the provider applied them, and they
never touch the status, the labels or the read marker above (` + "`read`" + ` is the provider's,
` + "`mark-read`" + ` is yours).
Every command runs in the profile's daemon (none running exits 70).
Reading is open to every role that reads conversations; transitions
and labeling are the desk role's. Which moves are allowed is the conversation
lifecycle's, applied by uma when the transition commits: a closed
conversation is terminal, and moving it is a conflict (exit 4).
Transitions name the revision they move (--revision, from
` + "`conversation get`" + `): a stale one is a conflict, exit 4. --dry-run
previews the exact change and writes nothing.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.conversationListCmd(), a.conversationGetCmd(),
		a.conversationMoveCmd("resolve", model.ConversationResolved), a.conversationMoveCmd("reopen", model.ConversationOpen),
		a.conversationStatusCmd(), a.conversationUpdateCmd(),
		a.conversationLabelCmd(), a.conversationTagCmd(), a.conversationUnsubscribeCmd(), a.conversationSummarizeCmd(), a.conversationTriageCmd(),
		a.conversationMarkCmd(false), a.conversationMarkCmd(true),
		a.conversationAssignCmd(false), a.conversationAssignCmd(true),
		a.conversationSnoozeCmd(false), a.conversationSnoozeCmd(true),
		a.conversationLinkCmd(false), a.conversationLinkCmd(true))
	for _, v := range desk.StateVerbs {
		grp.AddCommand(a.conversationStateCmd(v))
	}
	return grp
}

func (a *app) conversationListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List conversations across the inboxes you may read",
		Long: `List the conversations you may read, across every inbox you may read,
a page at a time (--limit, --cursor with the previous page's next), each
with the inbox it belongs to and its status. --inbox (key or ID) and
--status (` + strings.Join(desk.Statuses(), ", ") + `; repeat or
comma-separate) narrow it. Read-only.

Label filters take label names, repeated or comma-separated: --label
keeps conversations holding any of them, --label-all those holding all
of them, --label-none those holding none of them. Filters combine by
AND, and uma applies them. An unknown label name is not found (exit 3);
` + "`label list`" + ` shows the labels. Filtering by label needs the role to
read labels: desk and viewer.

Snoozed conversations (` + "`conversation snooze`" + `) are left out by default:
--snoozed=include lists them with the others, --snoozed=only lists just
them, --snoozed=exclude is the default. The other workflow filters are
--unread (conversations holding a message unread for you), --assignee
(an actor ID, self for you, or none for the unassigned) and --linked
(conversations that hold a link you may read). --snooze-ended keeps the
conversations whose snooze has ended (its time passed, or a live inbound
message arrived) and that the runtime has not yet woken: such a
conversation is transient, since the runtime's sweep clears the snooze
and emits conversation.snooze_ended within a minute; --snooze-ended=false
keeps the others. They combine with every filter above by AND, and uma applies
them, so pages are full. They need the right to read workflow state,
which desk and viewer hold: an external actor asking for one is refused
(exit 5), and sees every conversation it may read, snoozed or not.

Each conversation of a per_work_item inbox (an issue, a ticket) lists its
work item: the provider's reference and uma's status of it. That is how
trackers are browsed: --work-item keeps the conversations that have a work
item, --work-item=false those that have none, and --work-item-status
(` + strings.Join(desk.WorkItemStatuses(), ", ") + `; repeat or comma-separate) those whose work item
is in one of them. uma has no filter by work item, so heo reads each
conversation's work item and scans until the page is full; the work item
filters combine with every filter above by AND.

Each conversation lists its last activity (last_activity): the newest of
its newest message you may read (imported history does not count), the
time its snooze was set and the time you last moved your own read marker
(a snooze and a marker count only for a role that reads them). --sort
last-activity lists the newest change first, so the list reorders when a
message arrives, a read marker moves or a snooze starts; --updated-since
keeps the conversations whose last activity is after a time (RFC 3339 or
a date), for a client that refreshes incrementally. uma keeps no activity
time to order or filter by, so heo derives it from the messages, the
snooze and your marker, reading each conversation the other filters keep:
--sort reads them all before the first page, and a cursor of a sorted list
names the last item of its page, valid with the same filters and --sort.
Marking a conversation unread moves the marker, so it counts as a change.`,
		Example: `  heo conversation list
  heo conversation list --inbox web --status open,pending
  heo conversation list --label billing,urgent --label-none spam
  heo conversation list --label-all billing,urgent
  heo conversation list --unread --assignee self
  heo conversation list --snoozed only
  heo conversation list --snooze-ended
  heo conversation list --assignee none --linked
  heo conversation list --work-item --work-item-status open,pending
  heo conversation list --inbox issues --work-item-status closed
  heo conversation list --sort last-activity
  heo conversation list --updated-since 2026-10-01T09:00:00Z --sort last-activity
  heo conversation list --format json --limit 10`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			f := deskFilter(cmd)
			if f.UpdatedSince, err = timeFlag(cmd, flagUpdatedSince); err != nil {
				return err
			}
			// Every role that may read labels reads conversations; uma
			// authorizes each conversation listed.
			action := model.ActionConversationRead
			switch {
			case f.Workflow.Asked():
				// The workflow filters need the right to read workflow
				// state, which desk and viewer hold with the right to read
				// labels: an external actor holds neither.
				action = model.ActionConversationReadWorkflow
			case labelFiltered(f):
				action = model.ActionLabelRead
			}
			wi := workItemFilterOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[desk.ConversationItem], error) {
				var out page[desk.ConversationItem]
				err := a.desk(ctx, c, action, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListConversationItems(ctx, f, wi, pg)
					out = pageFrom(p)
					return a.labelFailure(err)
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]conversationRow, 0, len(res.Items))
			for _, cv := range res.Items {
				rows = append(rows, conversationRowOf(cv.Conversation, cv.WorkItem, cv.LastActivity))
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagInbox, "", "Only conversations of this inbox (key or ID)")
	c.Flags().StringSlice(flagStatus, nil, "Only conversations in these statuses: "+strings.Join(desk.Statuses(), ", "))
	addLabelFilterFlags(c, "conversations")
	c.Flags().Bool(flagUnread, false, "Only conversations holding a message unread for you")
	c.Flags().String(flagAssignee, "", "Only conversations assigned to this actor (such as uid:1001), to self, or to none")
	c.Flags().String(flagSnoozed, "", "Snoozed conversations: "+strings.Join(desk.SnoozeModes(), ", ")+" (default exclude: snoozed ones are left out)")
	c.Flags().Bool(flagSnoozeEnded, false, "Only conversations whose snooze has ended and the runtime has not yet woken (they are cleared within one sweep); =false only the others")
	c.Flags().Bool(flagLinked, false, "Only conversations that hold a link you may read")
	c.Flags().Bool(flagWorkItem, false, "Only conversations that have a work item (an issue or a ticket); =false only those that have none")
	c.Flags().StringSlice(flagWorkItemStatus, nil, "Only conversations whose work item is in these statuses: "+strings.Join(desk.WorkItemStatuses(), ", "))
	c.Flags().String(flagSort, "", "Order the list: "+strings.Join(desk.Sorts(), ", ")+" (newest change first; default uma's own order)")
	c.Flags().String(flagUpdatedSince, "", "Only conversations whose last activity is after this time (RFC 3339 or a date)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[desk.ConversationItem]{}, "1.0")
	return c
}

// conversationView is a conversation and, for a desk actor, the live
// state of the provider-dependent verbs on its channel: what uma reports
// of each capability now (available, or why not: unsupported,
// unauthorized, disabled, unavailable), statuses only. Verbs is empty
// for a caller who may not read internal content, and VerbsNote says
// why when the desk's read failed.
type conversationView struct {
	model.Conversation
	// Workflow reports that the caller may read workflow state, so an
	// empty assignee, snooze or links means none; without it they are
	// not shown at all.
	Workflow bool `json:"workflow,omitempty" yaml:"workflow,omitempty"`
	// Read is the caller's own read marker, for a caller that keeps one
	// (a desk actor); no one reads another actor's.
	Read      *desk.ReadState     `json:"read,omitempty"       yaml:"read,omitempty"`
	Verbs     []channels.VerbLive `json:"verbs,omitempty"      yaml:"verbs,omitempty"`
	VerbsNote string              `json:"verbs_note,omitempty" yaml:"verbs_note,omitempty"`
	// WorkItem is the issue or ticket the conversation is, for a
	// conversation of a per_work_item inbox: its status, the provider's
	// own, its assignees, fields, URL and binding. LinkedFrom are the
	// conversations that link to it (`conversation link --work-item`), for
	// a caller that may read workflow state.
	WorkItem   *model.WorkItem         `json:"work_item,omitempty"   yaml:"work_item,omitempty"`
	LinkedFrom []model.ConversationRef `json:"linked_from,omitempty" yaml:"linked_from,omitempty"`
}

// verbsTimeout bounds the live verb read of `conversation get`: it opens
// the channel's adapter and may reach the provider, and a slow one must
// not hold a read of the conversation.
const verbsTimeout = 10 * time.Second

func (a *app) conversationGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <conversation>",
		Short: "Show one conversation",
		Long: `Show the conversation <conversation>, by record ID: its inbox, status,
selection mode, participants, reply route and revision (what
` + "`--revision`" + ` takes). No such conversation, or one you may
not read, exits 3. ` + "`message list --conversation`" + ` shows its messages.

A conversation of a per_work_item inbox is an issue or a ticket, and shows
its work item: the provider's reference, subject and URL, its status (uma's
open, pending, resolved or closed) beside the provider's own, its binding,
requester, assignees and fields, the fields uma owns on the channel, the
conversations that link to it, and its revision (what ` + "`conversation status`" + `
and ` + "`conversation update`" + ` take). The work item's status is the
provider's: it changes when the provider reports it. The conversation's own
status is uma's (` + "`conversation resolve`" + `); neither moves the other.

For a desk actor it also shows, per provider-dependent verb, whether
the conversation's channel can do it now (verbs): available, or why not:
unsupported (the adapter does not declare it), disabled (the channel's
own configuration switched it off, such as github.issues without
replies), unauthorized (the provider account lacks the permission;
code PROVIDER_PERMISSION, exit 5 when a command meets it) or unavailable
(the provider cannot be reached). It is uma's live capability status for
the conversation: statuses only, no channel configuration. It opens the
channel's adapter and may reach the provider, so it can be slow; if it
fails or takes over ten seconds, the conversation is shown without
verbs, with the reason. An actor who may not read internal content sees no
verbs.

It also shows the conversation's workflow state: its assignee, its snooze
while one is in effect, and the links whose far end you may read, for the
desk and viewer roles; an external actor sees none of them. A desk actor
sees its own read marker too (read), never another actor's. Read-only
otherwise.`,
		Example: `  heo conversation get conv_123
  heo conversation get conv_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (conversationView, error) {
				var out conversationView
				err := a.desk(ctx, c, model.ActionConversationRead, model.Audience{}, func(s desk.Service) (err error) {
					out.Conversation, err = s.FindConversation(ctx, args[0])
					if err != nil {
						return err
					}
					out.Workflow = s.CanReadWorkflow()
					if out.Read, err = s.ReadState(ctx, out.Conversation); err != nil {
						return err
					}
					if out.WorkItem, err = s.WorkItemOf(ctx, out.Ref); err != nil {
						return err
					}
					if out.WorkItem != nil {
						if out.LinkedFrom, err = s.LinkedFromWorkItem(ctx, out.WorkItem.Ref); err != nil {
							return err
						}
					}
					if s.Sees == nil || !s.Sees(internalAudience) {
						return nil
					}
					vctx, cancel := context.WithTimeout(ctx, verbsTimeout)
					defer cancel()
					if out.Verbs, err = s.Verbs(vctx, out.Conversation); err != nil {
						out.Verbs, out.VerbsNote = nil, "verb availability could not be read: "+errmap.Of(err).Message
					}
					return nil
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return writeRows(cmd.OutOrStdout(), conversationViewRows(res))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &conversationView{}, "1.0")
	return c
}

// conversationViewRows is a conversation's fields and, after them, a row
// per provider-dependent verb: its live state, with the code and exit a
// command meeting that state would give.
func conversationViewRows(v conversationView) [][2]string {
	rows := conversationRowsFor(v.Conversation, v.Workflow)
	if v.WorkItem != nil {
		rows = append(rows, workItemRows(*v.WorkItem, v.LinkedFrom)...)
	}
	if v.Read != nil {
		read := "never marked: every message is unread"
		if v.Read.Marked {
			read = "through " + stamp(v.Read.ReadThrough) + " (marked " + stamp(v.Read.UpdatedAt) + ")"
		}
		rows = append(rows, [2]string{"read", read})
	}
	for _, vl := range v.Verbs {
		state := vl.State
		if vl.Code != "" {
			state = fmt.Sprintf("%s (%s, exit %d)", vl.State, vl.Code, vl.Exit)
		}
		rows = append(rows, [2]string{"verb " + vl.Verb, state})
	}
	if v.VerbsNote != "" {
		rows = append(rows, [2]string{"verbs", v.VerbsNote})
	}
	return rows
}

// transitionPlan renders a transition plan for --dry-run.
func transitionPlan(p desk.TransitionPlan, key string) *cli.Plan {
	cur := p.Current
	args := map[string]any{
		"conversation": cur.Ref.ID, "from": cur.Status, "state": p.Request.State,
		"expected_revision": p.Request.ExpectedRevision, "current": cur,
	}
	if key != "" {
		args["operation_key"] = key
	}
	effect := planEffect("update", "conversation:"+string(cur.Ref.ID), true, fmt.Sprintf(
		"status %s -> %s, local to uma (nothing changes at the provider); uma applies the conversation lifecycle when it commits",
		cur.Status, p.Request.State))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: []string{"authorization", "revision", "request"}, Warnings: p.Warnings}
}

// conversationMoveCmd builds `conversation resolve` and `conversation
// reopen`: a transition to state.
func (a *app) conversationMoveCmd(verb, state string) *cobra.Command {
	short, long := "Resolve a conversation (local; nothing closes at the provider)",
		`Resolve the conversation <conversation> (record ID): its local status
becomes resolved. Nothing changes at the provider: no ticket or thread
closes. A resolved conversation may be reopened.`
	if verb == "reopen" {
		short, long = "Reopen a conversation", `Reopen the conversation <conversation> (record ID): its local status
becomes open again. A closed conversation is terminal: reopening it is a
conflict (exit 4).`
	}
	c := &cobra.Command{
		Use:   verb + " <conversation>",
		Short: short,
		Long: long + `

--revision names the revision being moved, as ` + "`conversation get`" + `
shows it: if the conversation changed since, it is a conflict (exit 4)
and nothing is written. Whether the move is allowed is the conversation
lifecycle's, applied by uma when it commits; heo previews the change,
it does not judge it. Committed as a uma operation under
--idempotency-key: rerunning with the same key and arguments returns the
same operation (replayed); without one, heo mints a key and prints it.
--dry-run prints the change and writes nothing. Desk role.`,
		Example: fmt.Sprintf("  heo conversation %s conv_123 --revision rev_123\n  heo conversation %s conv_123 --revision rev_123 --dry-run", verb, verb),
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[conversationOut], error) {
				var w written[conversationOut]
				err := a.desk(ctx, c, model.ActionConversationTransition, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanTransition(ctx, args[0], rev, state, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = transitionPlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionConversationTransition, Verb: "state", Class: state,
						Channel: string(p.Current.Ref.Inbox.ID), Target: p.Current.Ref.Ref(), Preview: transitionPlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.Transition(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = conversationOut{Conversation: conv, Operation: operationOf(acc), OperationKey: key, Notes: p.Warnings}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, func(o conversationOut) [][2]string {
				return append(conversationRows(o.Conversation), operationRows(o.Operation, o.OperationKey)...)
			}, res.Out.Notes)
		},
	}
	addRevisionFlag(c, "conversation")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &conversationOut{}, "1.0")
	return c
}
