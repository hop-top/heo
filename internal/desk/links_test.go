package desk_test

import (
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// TestLinkPlanAndLifecycle: a conversation links to another
// conversation, an issue or a ticket; both ends of a conversation link
// show it; a preview names both ends and writes nothing; a far end the
// caller cannot read is not found, without trace; linking is neither a
// history merge nor a reply-scope change; unlinking is idempotent; the
// viewer reads links and the external actor neither reads nor makes one.
func TestLinkPlanAndLifecycle(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	issue := w.issue()
	ticketConvID := string(w.ticket.ID)
	issueConvID := string(issue.Ref.Conversation.ID)

	tc, err := s.FindConversation(ctx, ticketConvID)
	if err != nil {
		t.Fatal(err)
	}
	ic, err := s.FindConversation(ctx, issueConvID)
	if err != nil {
		t.Fatal(err)
	}

	// The preview: both ends, and nothing written.
	p, err := s.PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, false, "", "op-l-0", channels.Preview)
	if err != nil || p.Far == nil || p.Far.Ref != ic.Ref || p.Conversation.Ref != tc.Ref || p.Held || len(p.Warnings) != 0 {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	if got, _ := s.FindConversation(ctx, ticketConvID); len(got.Links) != 0 || got.Revision != tc.Revision {
		t.Fatalf("a preview wrote: %+v", got)
	}

	// Link: both ends hold it, and each moved once.
	got, _, err := s.ApplyConversationLink(ctx, mustPlanLink(t, s, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, false, tc.Revision, "op-l-1"))
	if err != nil || len(got.Links) != 1 || got.Links[0].Conversation == nil || *got.Links[0].Conversation != ic.Ref {
		t.Fatalf("link = %+v, %v", got, err)
	}
	far, _ := s.FindConversation(ctx, issueConvID)
	if len(far.Links) != 1 || *far.Links[0].Conversation != tc.Ref || far.Revision == ic.Revision {
		t.Fatalf("the far end holds %+v at %s", far.Links, far.Revision)
	}
	// No history merge, no reply-scope change: each keeps its own messages
	// and reply route.
	if got.ReplyRoute != tc.ReplyRoute || far.ReplyRoute != ic.ReplyRoute || got.Status != tc.Status {
		t.Error("linking changed a reply route or status")
	}
	// Linking again changes nothing, and says so.
	again, err := s.PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, false, "", "op-l-2", channels.Preview)
	if err != nil || !again.Held || len(again.Warnings) != 1 || !strings.Contains(again.Warnings[0], "already linked") {
		t.Fatalf("link again plan = %+v, %v", again, err)
	}
	// A stale revision conflicts.
	_, err = s.PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{WorkItem: string(issue.Ref.ID)}, false, tc.Revision, "op-l-3", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision")

	// A work item end: the issue and the ticket, both work items.
	cur, _ := s.FindConversation(ctx, ticketConvID)
	wi, _, err := s.ApplyConversationLink(ctx, mustPlanLink(t, s, ticketConvID, desk.LinkEnd{WorkItem: string(issue.Ref.ID)}, false, cur.Revision, "op-l-4"))
	if err != nil || len(wi.Links) != 2 {
		t.Fatalf("link issue = %+v, %v", wi, err)
	}
	if !slices.ContainsFunc(wi.Links, func(l model.ConversationLink) bool { return l.WorkItem != nil && l.WorkItem.ID == issue.Ref.ID }) {
		t.Fatalf("links = %+v", wi.Links)
	}
	back, err := s.LinkedFromWorkItem(ctx, issue.Ref)
	if err != nil || !slices.Equal(back, []model.ConversationRef{tc.Ref}) {
		t.Fatalf("conversations linked to the issue = %v, %v", back, err)
	}
	// The ticket linked from the issue's conversation.
	tl, _, err := s.ApplyConversationLink(ctx, mustPlanLink(t, s, issueConvID, desk.LinkEnd{WorkItem: string(ticketItem)}, false, "", "op-l-5"))
	if err != nil || len(tl.Links) != 2 {
		t.Fatalf("link ticket = %+v, %v", tl, err)
	}

	// Filters by link.
	linked := listedT(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Linked: boolp(true)}})
	if !slices.Contains(linked, tc.Ref.ID) || !slices.Contains(linked, ic.Ref.ID) {
		t.Errorf("--linked lists %v", linked)
	}
	if none := listedT(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Linked: boolp(false)}}); slices.Contains(none, tc.Ref.ID) {
		t.Errorf("--linked=false lists a linked conversation: %v", none)
	}

	// A far end that does not exist, or the conversation itself, is
	// refused before anything is written.
	for name, end := range map[string]desk.LinkEnd{
		"unknown conversation": {Conversation: "00000ad0-0000-7000-8000-0000000009aa"},
		"unknown work item":    {WorkItem: "00000ad0-0000-7000-8000-0000000009ab"},
	} {
		_, err := s.PlanConversationLink(ctx, ticketConvID, end, false, "", "op-l-7", channels.Preview)
		if !model.IsCode(err, model.NotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
	_, err = s.PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{Conversation: ticketConvID}, false, "", "op-l-8", channels.Preview)
	wantCode(t, err, model.Validation, "itself")
	for name, end := range map[string]desk.LinkEnd{"none": {}, "two": {Conversation: issueConvID, WorkItem: string(issue.Ref.ID)}} {
		if _, err := s.PlanConversationLink(ctx, ticketConvID, end, false, "", "op-l-9", channels.Preview); !model.IsCode(err, model.Validation) {
			t.Errorf("%s ends: %v, want validation", name, err)
		}
	}

	// The viewer reads links; the external actor reads none and makes none.
	cur, _ = s.FindConversation(ctx, ticketConvID)
	if v, err := w.as(viewer).FindConversation(ctx, ticketConvID); err != nil || len(v.Links) != len(cur.Links) {
		t.Errorf("viewer sees links %+v, %v", v.Links, err)
	}
	if x, err := w.as(external).FindConversation(ctx, ticketConvID); err == nil && len(x.Links) != 0 {
		t.Errorf("external sees links %+v", x.Links)
	}
	if _, err := w.as(viewer).PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, true, "", "op-l-10", channels.Submit); err == nil {
		pl, _ := w.as(viewer).PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, true, "", "op-l-10", channels.Submit)
		_, _, err = w.as(viewer).ApplyConversationLink(ctx, pl)
		wantCode(t, err, model.Unauthorized, "")
	}

	// Unlink: both ends let go; again is a no-op.
	un, _, err := s.ApplyConversationLink(ctx, mustPlanLink(t, s, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, true, "", "op-u-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range un.Links {
		if l.Conversation != nil {
			t.Errorf("a conversation link survived unlink: %+v", l)
		}
	}
	far, _ = s.FindConversation(ctx, issueConvID)
	if slices.ContainsFunc(far.Links, func(l model.ConversationLink) bool { return l.Conversation != nil }) {
		t.Errorf("the far end kept the link: %+v", far.Links)
	}
	noop, err := s.PlanConversationLink(ctx, ticketConvID, desk.LinkEnd{Conversation: issueConvID}, true, "", "op-u-2", channels.Preview)
	if err != nil || len(noop.Warnings) != 1 || !strings.Contains(noop.Warnings[0], "not linked") {
		t.Fatalf("unlink of an absent link = %+v, %v", noop, err)
	}
}

func listedT(t *testing.T, s desk.Service, f desk.Filter) []model.ID { return listed(t, s, f) }

func mustPlanLink(t *testing.T, s desk.Service, id string, end desk.LinkEnd, remove bool, rev model.Revision, op model.OperationID) desk.ConversationLinkPlan {
	t.Helper()
	p, err := s.PlanConversationLink(t.Context(), id, end, remove, rev, op, channels.Submit)
	if err != nil {
		t.Fatalf("plan link %+v: %v", end, err)
	}
	return p
}

// TestLinkFarEndUnreadableIsNotFound: a conversation the caller may not
// read is not found as a link's far end, as if it did not exist, and
// does not appear among its links when someone else linked it.
func TestLinkFarEndUnreadableIsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	local, remote := string(w.local.Ref.ID), string(w.remote.Ref.ID)
	if _, _, err := s.ApplyConversationLink(ctx, mustPlanLink(t, s, local, desk.LinkEnd{Conversation: remote}, false, "", "op-h-1")); err != nil {
		t.Fatal(err)
	}
	// The remote conversation becomes unreadable for the agent.
	w.gate.hide = map[model.ID]bool{w.remote.Ref.ID: true}
	_, err := s.PlanConversationLink(ctx, local, desk.LinkEnd{Conversation: remote}, false, "", "op-h-2", channels.Preview)
	wantCode(t, err, model.NotFound, "conversation "+remote+": not found")
	_, errNone := s.PlanConversationLink(ctx, local, desk.LinkEnd{Conversation: "00000ad0-0000-7000-8000-0000000009ad"}, false, "", "op-h-3", channels.Preview)
	if errNone == nil || strings.ReplaceAll(errNone.Error(), "00000ad0-0000-7000-8000-0000000009ad", remote) != err.Error() {
		t.Errorf("an unreadable conversation is told apart from one that does not exist:\n  %v\n  %v", err, errNone)
	}
	// It is left out of the links shown, without trace.
	got, err := s.FindConversation(ctx, local)
	if err != nil || len(got.Links) != 0 {
		t.Fatalf("links shown = %+v, %v", got.Links, err)
	}
	// A plan made before it became unreadable, submitted anyway (a race),
	// is refused by uma and not applied.
	w.gate.hide = nil
	pl := mustPlanLink(t, s, local, desk.LinkEnd{Conversation: remote}, true, "", "op-h-4")
	w.gate.hide = map[model.ID]bool{w.remote.Ref.ID: true}
	if _, _, err = s.ApplyConversationLink(ctx, pl); !model.IsCode(err, model.Unauthorized) && !model.IsCode(err, model.NotFound) {
		t.Fatalf("unlinking an unreadable far end: %v", err)
	}
	w.gate.hide = nil
	if got := w.refetch(w.local); len(got.Links) != 1 {
		t.Errorf("a refused unlink changed the links: %+v", got.Links)
	}
}
