package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// Conversation workflow: who is working a conversation (assign), when it
// comes back (snooze), what it relates to (link) and how far one actor
// has read it (mark-read). All of it is uma's: state, authorization,
// revision, events and filters. heo plans a change, previews it, submits
// it as the caller and shows the result; it keeps no state of its own and
// never filters a list itself. Nothing here reaches a provider: a
// receipt for a provider's sender is `receipt send`, apart from these.

// The values of `--snoozed`.
const (
	// SnoozeExclude leaves the snoozed conversations out; the default.
	SnoozeExclude = "exclude"
	// SnoozeInclude lists snoozed and awake conversations alike.
	SnoozeInclude = "include"
	// SnoozeOnly lists the snoozed ones alone.
	SnoozeOnly = "only"
)

// SnoozeModes lists the values of `--snoozed`.
func SnoozeModes() []string { return []string{SnoozeInclude, SnoozeOnly, SnoozeExclude} }

// SelfActor names the caller in `--to` and `--assignee`.
const SelfActor = "self"

// NoActor selects the unassigned conversations in `--assignee`.
const NoActor = "none"

// WakeRule is when a snoozed conversation comes back: stated in
// previews and help.
const WakeRule = "wakes at the time, or when a live inbound message is recorded after the snooze, whichever is first; imported history never wakes it"

// WorkflowFilter is the workflow part of a conversation listing: uma
// applies every field, evaluated by the store, so pages are full.
type WorkflowFilter struct {
	// Unread keeps the conversations holding a message unread for the
	// caller.
	Unread bool
	// Assignee is an actor ID, SelfActor (the caller, resolved by uma) or
	// NoActor; empty is unset.
	Assignee string
	// Snoozed is SnoozeInclude, SnoozeOnly or SnoozeExclude; empty is the
	// default: SnoozeExclude for a caller that may read workflow state,
	// no filter for one that may not (they see no snooze).
	Snoozed string
	// Linked, when set, keeps the conversations holding a link (true) or
	// none (false).
	Linked *bool
}

// Asked reports whether f asks for a filter that needs the right to read
// workflow state: any but the default and --snoozed=include.
func (f WorkflowFilter) Asked() bool {
	return f.Unread || f.Assignee != "" || f.Linked != nil || f.Snoozed == SnoozeOnly || f.Snoozed == SnoozeExclude
}

// can reports whether heo's policy lets the caller perform action.
func (s Service) can(action string) bool {
	return s.Permits != nil && s.Actor != "" && s.Permits(s.Actor, action)
}

// CanReadWorkflow reports whether the caller may read workflow state:
// assignee, snooze and links. Desk and viewer may.
func (s Service) CanReadWorkflow() bool { return s.can(model.ActionConversationReadWorkflow) }

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// workflowFilter sets f's filters on out. A filter the caller asked for
// needs the right to read workflow state, else it is refused:
// UNAUTHORIZED, not a list that is quietly empty. The default hides
// snoozed conversations from callers that see snoozes.
func (s Service) workflowFilter(f WorkflowFilter, out *model.Filter) error {
	if f.Snoozed != "" && !slices.Contains(SnoozeModes(), f.Snoozed) {
		return model.NewError(model.Validation, fmt.Sprintf("snoozed %q: want one of %s", f.Snoozed, strings.Join(SnoozeModes(), ", ")))
	}
	if f.Asked() && !s.CanReadWorkflow() {
		return model.NewError(model.Unauthorized, "the workflow filters (unread, assignee, snoozed, linked) need the right to read workflow state (conversation.read_workflow): the desk and viewer roles hold it")
	}
	switch f.Snoozed {
	case SnoozeOnly:
		out.Snoozed = boolPtr(true)
	case SnoozeExclude:
		out.Snoozed = boolPtr(false)
	case "":
		if s.CanReadWorkflow() {
			out.Snoozed = boolPtr(false)
		}
	}
	switch a := f.Assignee; a {
	case "":
	case SelfActor:
		out.Assignee = &model.AssigneeFilter{Self: true}
	case NoActor:
		out.Assignee = &model.AssigneeFilter{None: true}
	default:
		af := model.AssigneeFilter{Actor: model.ActorID(a)}
		if err := af.Validate(); err != nil {
			return err
		}
		out.Assignee = &af
	}
	if f.Unread {
		out.Unread = &model.UnreadFilter{}
	}
	if f.Linked != nil {
		out.Linked = boolPtr(*f.Linked)
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }

// who names an actor for a message.
func who(a *model.ActorID) string {
	if a == nil {
		return "unassigned"
	}
	return "assigned to " + string(*a)
}

// stale is the conflict of a revision the conversation is no longer at:
// it names the revision and who holds the conversation now, so the
// caller sees what changed.
func stale(c model.Conversation, id string, rev model.Revision) error {
	return model.NewError(model.Conflict, fmt.Sprintf(
		"conversation %s is at revision %s, not %s: it changed since it was read; it is now %s", id, c.Revision, rev, who(c.Assignee)))
}

// checkRev refuses, in a preview, or leaves to uma, on submission (see
// channels.Mode), a revision the conversation is not at. A revision the
// caller did not name is not sent: the change then applies to whatever
// the conversation is, and a retry under the same key replays.
func checkRev(c model.Conversation, id string, rev model.Revision, m channels.Mode) (deferred, err error) {
	if rev == "" || rev == c.Revision {
		return nil, nil
	}
	if m == channels.Preview {
		return nil, stale(c, id, rev)
	}
	return stale(c, id, rev), nil
}

// AssignPlan is a validated assignment or unassignment.
type AssignPlan struct {
	Request model.ConversationAssignment
	// Conversation is the conversation as it is now: its current assignee
	// is what the change replaces.
	Conversation model.Conversation
	// Assignee is who the conversation would be assigned to; empty for an
	// unassignment.
	Assignee model.ActorID
	Warnings []string
	deferred error
}

// PlanAssign builds and checks assigning the conversation id to to (an
// actor ID, or SelfActor for the authenticated caller; empty to clear
// the assignee) under operation op. rev, when set, is the revision the
// change applies to. In a preview it also refuses an assignee that is
// not eligible (model.Unauthorized: uma asks the same of the policy on
// submission) and a stale revision; a submission leaves both, and the
// conflict's description, to uma.
func (s Service) PlanAssign(ctx context.Context, id, to string, rev model.Revision, op model.OperationID, m channels.Mode) (AssignPlan, error) {
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return AssignPlan{}, err
	}
	var assignee model.ActorID
	switch to {
	case "":
	case SelfActor:
		if s.Actor == "" {
			return AssignPlan{}, model.NewError(model.Validation, "assignee: the caller is unknown, name an actor")
		}
		assignee = s.Actor
	default:
		assignee = model.ActorID(to)
	}
	p := AssignPlan{Conversation: conv, Assignee: assignee}
	p.Request = model.ConversationAssignment{
		Command:      model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Conversation: conv.Ref, Assignee: assignee,
	}
	if err := p.Request.Validate(); err != nil {
		return AssignPlan{}, err
	}
	if p.deferred, err = checkRev(conv, id, rev, m); err != nil {
		return AssignPlan{}, err
	}
	if m == channels.Preview && assignee != "" && s.Permits != nil && !s.Permits(assignee, model.ActionConversationAssign) {
		return AssignPlan{}, model.NewError(model.Unauthorized, fmt.Sprintf(
			"assignee %s is not eligible for conversation %s: only desk actors may be assigned", assignee, id))
	}
	switch {
	case assignee == "" && conv.Assignee == nil:
		p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s is not assigned: unassigning changes nothing", id))
	case assignee != "" && conv.Assignee != nil && *conv.Assignee == assignee:
		p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s is already assigned to %s: assigning changes nothing", id, assignee))
	}
	return p, nil
}

// Assign submits p and returns the conversation as it is after it. uma
// emits conversation.assigned or conversation.unassigned, naming who held
// it, unless nothing changed.
func (s Service) Assign(ctx context.Context, p AssignPlan) (model.Conversation, model.Acceptance, error) {
	submit := s.Client.Conversations().Assign
	if p.Assignee == "" {
		submit = s.Client.Conversations().Unassign
	}
	acc, err := submit(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, channels.Explain(err, p.deferred)
	}
	conv, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Request.Conversation})
	return conv, acc, err
}

// SnoozePlan is a validated snooze or unsnooze.
type SnoozePlan struct {
	Request model.ConversationSnooze
	// Conversation is the conversation as it is now: its current snooze,
	// when one is in effect, is what the change replaces or ends.
	Conversation model.Conversation
	// Until is when the snooze would end, resolved by heo's clock for a
	// duration; zero for an unsnooze.
	Until    time.Time
	Warnings []string
	deferred error
}

// PlanSnooze builds and checks snoozing the conversation id until the
// instant until, or for the duration forD from now on heo's clock; one of
// the two, in the future. A past time, none or both is model.Validation,
// said before anything is submitted. rev, when set, is the revision the
// change applies to.
func (s Service) PlanSnooze(ctx context.Context, id string, until *time.Time, forD time.Duration, rev model.Revision, op model.OperationID, m channels.Mode) (SnoozePlan, error) {
	if (until == nil) == (forD == 0) {
		return SnoozePlan{}, model.NewError(model.Validation, "snooze: exactly one of an end time (until) and a duration (for) is required")
	}
	now := s.now()
	var end time.Time
	switch {
	case forD != 0:
		if forD < 0 {
			return SnoozePlan{}, model.NewError(model.Validation, fmt.Sprintf("for %s: a duration must be positive", forD))
		}
		end = now.Add(forD)
	default:
		end = until.UTC()
	}
	if !end.After(now) {
		return SnoozePlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"until %s: must be in the future (now %s)", end.Format(time.RFC3339), now.Format(time.RFC3339)))
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return SnoozePlan{}, err
	}
	p := SnoozePlan{Conversation: conv, Until: end}
	p.Request = model.ConversationSnooze{
		Command:      model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Conversation: conv.Ref, Until: &end,
	}
	if err := p.Request.Validate(); err != nil {
		return SnoozePlan{}, err
	}
	p.deferred, err = checkRev(conv, id, rev, m)
	return p, err
}

// PlanUnsnooze builds and checks ending the snooze of the conversation
// id. Ending one that is not in effect (none, expired, or woken by an
// inbound message) changes nothing and is no error.
func (s Service) PlanUnsnooze(ctx context.Context, id string, rev model.Revision, op model.OperationID, m channels.Mode) (SnoozePlan, error) {
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return SnoozePlan{}, err
	}
	p := SnoozePlan{Conversation: conv}
	p.Request = model.ConversationSnooze{
		Command:      model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Conversation: conv.Ref,
	}
	if err := p.Request.Validate(); err != nil {
		return SnoozePlan{}, err
	}
	if p.deferred, err = checkRev(conv, id, rev, m); err != nil {
		return SnoozePlan{}, err
	}
	if conv.Snooze == nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s is not snoozed (none is in effect): unsnoozing changes nothing", id))
	}
	return p, nil
}

// Snooze submits p, a snooze or an unsnooze, and returns the conversation
// as it is after it. Nothing is scheduled: uma evaluates the snooze when
// the conversation is read, so it holds across restarts and its end emits
// no event.
func (s Service) Snooze(ctx context.Context, p SnoozePlan) (model.Conversation, model.Acceptance, error) {
	submit := s.Client.Conversations().Snooze
	if p.Request.Until == nil {
		submit = s.Client.Conversations().Unsnooze
	}
	acc, err := submit(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, channels.Explain(err, p.deferred)
	}
	conv, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Request.Conversation})
	return conv, acc, err
}

// LinkEnd is the far end of a link: exactly one of a conversation and a
// work item, by record ID.
type LinkEnd struct {
	Conversation string
	WorkItem     string
}

// ConversationLinkPlan is a validated link or unlink.
type ConversationLinkPlan struct {
	Request model.ConversationLinkChange
	Remove  bool
	// Conversation is the conversation as it is now.
	Conversation model.Conversation
	// Far is the far conversation, or Item the far work item, as the
	// caller reads them now.
	Far  *model.Conversation
	Item *model.WorkItem
	// Held reports whether the conversation holds the link now.
	Held     bool
	Warnings []string
	deferred error
}

// PlanConversationLink builds and checks linking the conversation id to end (or,
// with remove, unlinking it) under operation op. Both ends are resolved
// as the caller: one it may not read is not found, exactly as one that
// does not exist, so a refusal leaves no trace of it. uma authorizes the
// change on both ends when it commits. A link merges no history and
// changes no reply scope.
func (s Service) PlanConversationLink(ctx context.Context, id string, end LinkEnd, remove bool, rev model.Revision, op model.OperationID, m channels.Mode) (ConversationLinkPlan, error) {
	set := 0
	for _, v := range []string{end.Conversation, end.WorkItem} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return ConversationLinkPlan{}, model.NewError(model.Validation, "link: exactly one of a conversation and a work item is required")
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return ConversationLinkPlan{}, err
	}
	p := ConversationLinkPlan{Conversation: conv, Remove: remove}
	var link model.ConversationLink
	switch {
	case end.Conversation != "":
		far, err := s.FindConversation(ctx, end.Conversation)
		if err != nil {
			return ConversationLinkPlan{}, err
		}
		if far.Ref == conv.Ref {
			return ConversationLinkPlan{}, model.NewError(model.Validation, fmt.Sprintf("conversation %s: cannot be linked to itself", id))
		}
		p.Far, link = &far, model.ConversationLink{Conversation: &far.Ref}
	default:
		it, err := s.FindWorkItem(ctx, end.WorkItem)
		if err != nil {
			return ConversationLinkPlan{}, err
		}
		p.Item, link = &it, model.ConversationLink{WorkItem: &it.Ref}
	}
	p.Request = model.ConversationLinkChange{
		Command:      model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Conversation: conv.Ref, Link: link,
	}
	if err := p.Request.Validate(); err != nil {
		return ConversationLinkPlan{}, err
	}
	if p.deferred, err = checkRev(conv, id, rev, m); err != nil {
		return ConversationLinkPlan{}, err
	}
	p.Held = slices.ContainsFunc(conv.Links, func(l model.ConversationLink) bool { return model.CompareConversationLinks(l, link) == 0 })
	name := p.FarName()
	switch {
	case !remove && p.Held:
		p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s is already linked to %s: linking changes nothing", id, name))
	case remove && !p.Held:
		p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s is not linked to %s: unlinking changes nothing", id, name))
	}
	return p, nil
}

// FarName names the far end of the plan: `conversation <id>` or `work
// item <id>`.
func (p ConversationLinkPlan) FarName() string {
	switch {
	case p.Far != nil:
		return "conversation " + string(p.Far.Ref.ID)
	case p.Item != nil:
		return "work item " + string(p.Item.Ref.ID)
	}
	return "?"
}

// ApplyConversationLink submits p, a link or an unlink, and returns the conversation
// as it is after it.
func (s Service) ApplyConversationLink(ctx context.Context, p ConversationLinkPlan) (model.Conversation, model.Acceptance, error) {
	submit := s.Client.Conversations().Link
	if p.Remove {
		submit = s.Client.Conversations().Unlink
	}
	acc, err := submit(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, channels.Explain(err, p.deferred)
	}
	conv, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Request.Conversation})
	return conv, acc, err
}

// LinkedFromWorkItem returns the conversations the caller may read that
// link to the work item ref, in listing order. A work item holds no link
// itself: a conversation does, so heo reads the conversations uma lists
// as linked and keeps those naming the item. A caller that may not read
// workflow state sees none.
func (s Service) LinkedFromWorkItem(ctx context.Context, ref model.WorkItemRef) ([]model.ConversationRef, error) {
	if !s.CanReadWorkflow() {
		return nil, nil
	}
	var out []model.ConversationRef
	req := model.ListRequest{Call: call(), Filter: model.Filter{Linked: boolPtr(true)}, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Conversations().List(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, c := range p.Items {
			if slices.ContainsFunc(c.Links, func(l model.ConversationLink) bool { return l.WorkItem != nil && *l.WorkItem == ref }) {
				out = append(out, c.Ref)
			}
		}
		if p.Complete || p.Next == "" {
			return out, nil
		}
		req.Page.After = p.Next
	}
}

// ReadMarkPlan is a validated move of the caller's read marker.
type ReadMarkPlan struct {
	Request model.ReadMark
	// Unread is a mark-unread.
	Unread       bool
	Conversation model.Conversation
	// Message is the message named, when one was.
	Message *model.Message
	// Current is the caller's marker now; no revision when it has none.
	Current model.ReadMarker
	// Through is where the marker would stand, when it is known before
	// submitting: the named message's recording time (read), or just
	// before it (unread). Nil otherwise: now (read), or before the newest
	// live inbound message that was not imported (unread), as uma finds it.
	Through *time.Time
}

// PlanReadMark builds and checks moving the caller's read marker on the
// conversation id: read through the message msg (record ID, in that
// conversation) or, when empty, through now; or, with unread, from msg on
// or from the newest live inbound message. The marker is the caller's
// own: it moves nothing else, emits no event and sends no receipt.
func (s Service) PlanReadMark(ctx context.Context, id, msg string, unread bool) (ReadMarkPlan, error) {
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return ReadMarkPlan{}, err
	}
	p := ReadMarkPlan{Unread: unread, Conversation: conv, Request: model.ReadMark{Call: call(), Conversation: conv.Ref}}
	if msg != "" {
		m, err := s.FindMessage(ctx, msg, string(conv.Ref.ID), false)
		if err != nil {
			return ReadMarkPlan{}, err
		}
		p.Message, p.Request.Message = &m, &m.Ref
		through := m.RecordedAt
		if unread {
			through = through.Add(-time.Nanosecond)
		}
		p.Through = &through
	}
	if err := p.Request.Validate(); err != nil {
		return ReadMarkPlan{}, err
	}
	if p.Current, err = s.Client.Conversations().ReadMarker(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: conv.Ref}); err != nil {
		return ReadMarkPlan{}, err
	}
	return p, nil
}

// ApplyReadMark submits p and returns the caller's marker as it is after
// it: the same record when the call changed nothing.
func (s Service) ApplyReadMark(ctx context.Context, p ReadMarkPlan) (model.ReadMarker, error) {
	if p.Unread {
		return s.Client.Conversations().MarkUnread(ctx, p.Request)
	}
	return s.Client.Conversations().MarkRead(ctx, p.Request)
}

// ReadState is the caller's own read state of a conversation.
type ReadState struct {
	// Marked is false until the caller marks the conversation once.
	Marked      bool      `json:"marked"                 yaml:"marked"`
	ReadThrough time.Time `json:"read_through,omitzero"  yaml:"read_through,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"    yaml:"updated_at,omitempty"`
	Revision    string    `json:"revision,omitempty"     yaml:"revision,omitempty"`
}

// ReadState returns the caller's read state of conv, or nil when the
// caller keeps none: only a desk actor marks, and no one reads another's.
func (s Service) ReadState(ctx context.Context, conv model.Conversation) (*ReadState, error) {
	if !s.can(model.ActionConversationMarkRead) {
		return nil, nil
	}
	m, err := s.Client.Conversations().ReadMarker(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: conv.Ref})
	if err != nil {
		return nil, err
	}
	if m.Revision == "" {
		return &ReadState{}, nil
	}
	return &ReadState{Marked: true, ReadThrough: m.ReadThrough, UpdatedAt: m.UpdatedAt, Revision: string(m.Revision)}, nil
}
