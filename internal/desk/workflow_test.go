package desk_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// listed returns the IDs of the conversations s lists under f, every
// page.
func listed(t *testing.T, s desk.Service, f desk.Filter) []model.ID {
	t.Helper()
	var out []model.ID
	page := model.PageRequest{Limit: 100}
	for {
		p, err := s.ListConversations(t.Context(), f, page)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		for _, c := range p.Items {
			out = append(out, c.Ref.ID)
		}
		if p.Complete || p.Next == "" {
			return out
		}
		page.After = p.Next
	}
}

// workflowEvents lists the workflow events of kind actor may read.
func (w *world) workflowEvents(actor model.ActorID, kind string) []model.Event {
	w.t.Helper()
	page, err := w.client(actor).Events().List(w.t.Context(), model.ListEvents{
		Call: call, Filter: model.Filter{Kinds: []string{kind}}, Start: model.StartBeginning, Limit: 100,
	})
	if err != nil {
		w.t.Fatalf("events %s as %s: %v", kind, actor, err)
	}
	return page.Items
}

// wake has ada send another message into the local conversation, a
// minute after the last thing that happened.
func (w *world) wake(event string) {
	w.t.Helper()
	w.clock.Advance(time.Minute)
	w.inject(adapterstest.Inject{Event: event, Message: event, From: "ada", Destination: "local", Text: "still there?"})
}

func holds(a *model.ActorID, want model.ActorID) bool { return a != nil && *a == want }

// TestAssignPlanAndLifecycle: `--to self` is the authenticated caller,
// whatever else is named; a preview shows the current assignee; a stale
// revision is a conflict that names it; only a desk actor is an eligible
// assignee (checked in a preview, and by uma on submission); assigning
// the current assignee, or unassigning an unassigned conversation,
// changes nothing; the log keeps who held it; a viewer reads the
// assignee and an external actor does not.
func TestAssignPlanAndLifecycle(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s, s2 := w.as(agent), w.as(agent2)
	id := string(w.local.Ref.ID)
	conv := w.refetch(w.local)

	p, err := s.PlanAssign(ctx, id, "self", "", "op-as-1", channels.Preview)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Assignee != agent || p.Request.Assignee != agent || p.Conversation.Assignee != nil || len(p.Warnings) != 0 {
		t.Fatalf("self preview = %+v", p)
	}
	if p.Request.ExpectedRevision != "" {
		t.Errorf("no revision named, yet one is sent: %q", p.Request.ExpectedRevision)
	}
	// Nothing was written by planning.
	if cur := w.refetch(conv); cur.Assignee != nil || cur.Revision != conv.Revision {
		t.Fatalf("a preview wrote: %+v", cur)
	}

	got, _, err := s.Assign(ctx, mustPlanAssign(t, s, id, "self", conv.Revision, "op-as-2"))
	if err != nil || !holds(got.Assignee, agent) || got.Revision == conv.Revision {
		t.Fatalf("assign self = %+v, %v", got, err)
	}
	// The second desk actor's `self` is itself, and its preview shows who
	// holds the conversation now.
	p2, err := s2.PlanAssign(ctx, id, "self", "", "op-as-3", channels.Preview)
	if err != nil || p2.Assignee != agent2 || !holds(p2.Conversation.Assignee, agent) {
		t.Fatalf("agent2 self preview = %+v, %v", p2, err)
	}

	// A stale revision conflicts and names the current assignee, in a
	// preview and on submission.
	for _, m := range []channels.Mode{channels.Preview, channels.Submit} {
		pl, err := s2.PlanAssign(ctx, id, string(agent2), conv.Revision, "op-as-stale", m)
		if m == channels.Preview {
			wantCode(t, err, model.Conflict, "assigned to "+string(agent))
			continue
		}
		if err != nil {
			t.Fatalf("submit plan: %v", err)
		}
		_, _, err = s2.Assign(ctx, pl)
		wantCode(t, err, model.Conflict, "assigned to "+string(agent))
		wantCode(t, err, model.Conflict, string(got.Revision))
	}
	if cur := w.refetch(conv); !holds(cur.Assignee, agent) {
		t.Fatalf("a stale assign changed the assignee: %+v", cur.Assignee)
	}

	// Reassign at the current revision.
	re, _, err := s2.Assign(ctx, mustPlanAssign(t, s2, id, string(agent2), got.Revision, "op-as-4"))
	if err != nil || !holds(re.Assignee, agent2) {
		t.Fatalf("reassign = %+v, %v", re, err)
	}
	evs := w.workflowEvents(agent, model.EventConversationAssigned)
	if len(evs) != 2 {
		t.Fatalf("%d assigned events, want 2", len(evs))
	}
	var last model.ConversationAssignmentPayload
	if err := json.Unmarshal(evs[1].Payload.Data, &last); err != nil || !holds(last.Assignee, agent2) || !holds(last.Previous, agent) {
		t.Fatalf("last assigned event = %+v, %v", last, err)
	}

	// Assigning the current assignee changes nothing, and says so.
	same, err := s2.PlanAssign(ctx, id, string(agent2), "", "op-as-5", channels.Preview)
	if err != nil || len(same.Warnings) != 1 || !strings.Contains(same.Warnings[0], "already assigned") {
		t.Fatalf("same assignee plan = %+v, %v", same, err)
	}
	again, _, err := s2.Assign(ctx, mustPlanAssign(t, s2, id, string(agent2), "", "op-as-5"))
	if err != nil || again.Revision != re.Revision || len(w.workflowEvents(agent, model.EventConversationAssigned)) != 2 {
		t.Fatalf("assigning the assignee moved the record: %+v, %v", again, err)
	}

	// Only a desk actor is eligible.
	for _, who := range []model.ActorID{viewer, external, operator, service, "uid:9999"} {
		_, err := s.PlanAssign(ctx, id, string(who), "", "op-as-6", channels.Preview)
		wantCode(t, err, model.Unauthorized, "not eligible")
		pl, err := s.PlanAssign(ctx, id, string(who), "", "op-as-7", channels.Submit)
		if err != nil {
			t.Fatalf("submit plan for %s: %v", who, err)
		}
		_, _, err = s.Assign(ctx, pl)
		wantCode(t, err, model.Unauthorized, "not eligible")
	}
	if cur := w.refetch(conv); !holds(cur.Assignee, agent2) || cur.Revision != re.Revision {
		t.Fatalf("a refused assignee changed the conversation: %+v", cur)
	}
	// The actor named is a valid one.
	_, err = s.PlanAssign(ctx, id, "bad\x00actor", "", "op-as-8", channels.Preview)
	wantCode(t, err, model.Validation, "assignee")

	// A viewer reads the assignee, an external actor does not, and neither
	// may change it.
	if v := w.refetchAs(viewer, conv); !holds(v.Assignee, agent2) {
		t.Errorf("viewer sees assignee %v", v.Assignee)
	}
	if x := w.refetchAs(external, conv); x.Assignee != nil || x.Snooze != nil || len(x.Links) != 0 {
		t.Errorf("external sees workflow state: %+v", x)
	}
	if len(w.workflowEvents(external, model.EventConversationAssigned)) != 0 {
		t.Error("external lists assignment events")
	}
	if len(w.workflowEvents(viewer, model.EventConversationAssigned)) != 0 {
		t.Error("viewer lists assignment events: they are internal")
	}
	for _, who := range []model.ActorID{viewer, external} {
		pl, err := w.as(who).PlanAssign(ctx, id, string(agent2), "", "op-as-9", channels.Submit)
		if err != nil {
			continue // not even readable
		}
		_, _, err = w.as(who).Assign(ctx, pl)
		wantCode(t, err, model.Unauthorized, "")
	}

	// Unassign: the event records who cleared it; again is a no-op.
	un, _, err := s.Assign(ctx, mustPlanAssign(t, s, id, "", re.Revision, "op-un-1"))
	if err != nil || un.Assignee != nil || un.Revision == re.Revision {
		t.Fatalf("unassign = %+v, %v", un, err)
	}
	cleared := w.workflowEvents(agent, model.EventConversationUnassigned)
	if len(cleared) != 1 || cleared[0].Actor != agent {
		t.Fatalf("unassigned events = %+v", cleared)
	}
	noop, err := s.PlanAssign(ctx, id, "", "", "op-un-2", channels.Preview)
	if err != nil || len(noop.Warnings) != 1 || !strings.Contains(noop.Warnings[0], "not assigned") {
		t.Fatalf("unassign of an unassigned conversation = %+v, %v", noop, err)
	}
}

func mustPlanAssign(t *testing.T, s desk.Service, id, to string, rev model.Revision, op model.OperationID) desk.AssignPlan {
	t.Helper()
	p, err := s.PlanAssign(t.Context(), id, to, rev, op, channels.Submit)
	if err != nil {
		t.Fatalf("plan assign %q: %v", to, err)
	}
	return p
}

func (w *world) refetchAs(actor model.ActorID, c model.Conversation) model.Conversation {
	w.t.Helper()
	got, err := w.as(actor).FindConversation(w.t.Context(), string(c.Ref.ID))
	if err != nil {
		w.t.Fatalf("%s reads %s: %v", actor, c.Ref.ID, err)
	}
	return got
}

// TestSnoozePlanAndLifecycle: --for is resolved by heo's clock, a time
// in the past is refused, the snooze hides the conversation from the
// default list until its time or an inbound message, an early unsnooze
// is idempotent, and the viewer sees it while the external actor neither
// sees it nor has the conversation hidden from them.
func TestSnoozePlanAndLifecycle(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	id := string(w.local.Ref.ID)
	conv := w.refetch(w.local)
	now := w.clock.Now()

	hour := now.Add(time.Hour).UTC()
	p, err := s.PlanSnooze(ctx, id, nil, time.Hour, "", "op-sn-1", channels.Preview)
	if err != nil || !p.Until.Equal(hour) || p.Request.Until == nil || !p.Request.Until.Equal(hour) {
		t.Fatalf("--for plan = %+v, %v", p, err)
	}
	at := now.Add(3 * time.Hour)
	if p, err = s.PlanSnooze(ctx, id, &at, 0, "", "op-sn-1", channels.Preview); err != nil || !p.Until.Equal(at.UTC()) {
		t.Fatalf("--until plan = %+v, %v", p, err)
	}
	past := now.Add(-time.Minute)
	for name, c := range map[string]func() (desk.SnoozePlan, error){
		"until in the past": func() (desk.SnoozePlan, error) {
			return s.PlanSnooze(ctx, id, &past, 0, "", "op-sn-x", channels.Submit)
		},
		"until now": func() (desk.SnoozePlan, error) { return s.PlanSnooze(ctx, id, &now, 0, "", "op-sn-x", channels.Submit) },
		"negative for": func() (desk.SnoozePlan, error) {
			return s.PlanSnooze(ctx, id, nil, -time.Hour, "", "op-sn-x", channels.Submit)
		},
		"neither": func() (desk.SnoozePlan, error) { return s.PlanSnooze(ctx, id, nil, 0, "", "op-sn-x", channels.Submit) },
		"both": func() (desk.SnoozePlan, error) {
			return s.PlanSnooze(ctx, id, &at, time.Hour, "", "op-sn-x", channels.Submit)
		},
	} {
		if _, err := c(); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: %v, want validation", name, err)
		}
	}

	// Snoozed: hidden from the default list, found by the others.
	sn, _, err := s.Snooze(ctx, mustPlanSnooze(t, s, id, nil, time.Hour, conv.Revision, "op-sn-2"))
	if err != nil || sn.Snooze == nil || !sn.Snooze.Until.Equal(hour) || sn.Snooze.By != agent {
		t.Fatalf("snooze = %+v, %v", sn, err)
	}
	local := w.local.Ref.ID
	for mode, want := range map[string]bool{"": false, desk.SnoozeExclude: false, desk.SnoozeInclude: true, desk.SnoozeOnly: true} {
		got := slices.Contains(listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Snoozed: mode}}), local)
		if got != want {
			t.Errorf("--snoozed=%q lists the snoozed conversation = %v, want %v", mode, got, want)
		}
	}
	if only := listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Snoozed: desk.SnoozeOnly}}); !slices.Equal(only, []model.ID{local}) {
		t.Errorf("--snoozed=only lists %v", only)
	}
	if _, err := s.ListConversations(ctx, desk.Filter{Workflow: desk.WorkflowFilter{Snoozed: "sometimes"}}, model.PageRequest{Limit: 10}); !model.IsCode(err, model.Validation) {
		t.Errorf("unknown --snoozed value: %v", err)
	}
	// The snooze is still found by ID, and a stale revision conflicts.
	if got := w.refetch(w.local); got.Snooze == nil {
		t.Error("a snoozed conversation is not found by ID with its snooze")
	}
	stale, err := s.PlanSnooze(ctx, id, nil, time.Hour, conv.Revision, "op-sn-3", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Snooze(ctx, stale)
	wantCode(t, err, model.Conflict, "is at revision")

	// The viewer sees the snooze and the default list hides it from them
	// too; the external actor sees neither a snooze nor a hidden
	// conversation, and may not ask for the filter.
	if v := w.refetchAs(viewer, conv); v.Snooze == nil {
		t.Error("viewer does not see the snooze")
	}
	if slices.Contains(listed(t, w.as(viewer), desk.Filter{}), local) {
		t.Error("viewer's default list holds the snoozed conversation")
	}
	if x := w.refetchAs(external, conv); x.Snooze != nil {
		t.Errorf("external sees the snooze: %+v", x.Snooze)
	}
	if !slices.Contains(listed(t, w.as(external), desk.Filter{}), local) {
		t.Error("external's list hides a snoozed conversation: they cannot see the snooze")
	}
	for _, f := range []desk.WorkflowFilter{{Snoozed: desk.SnoozeOnly}, {Snoozed: desk.SnoozeExclude}, {Assignee: "none"}, {Unread: true}} {
		_, err := w.as(external).ListConversations(ctx, desk.Filter{Workflow: f}, model.PageRequest{Limit: 10})
		wantCode(t, err, model.Unauthorized, "workflow")
	}
	if _, err := w.as(external).ListConversations(ctx, desk.Filter{Workflow: desk.WorkflowFilter{Snoozed: desk.SnoozeInclude}}, model.PageRequest{Limit: 10}); err != nil {
		t.Errorf("--snoozed=include for an external actor: %v", err)
	}

	// An inbound message wakes it, before its time.
	w.wake("e-wake-1")
	if !slices.Contains(listed(t, s, desk.Filter{}), local) {
		t.Error("an inbound message did not bring the conversation back")
	}
	if got := w.refetch(w.local); got.Snooze != nil {
		t.Errorf("a woken conversation still shows its snooze: %+v", got.Snooze)
	}
	// Unsnoozing one that is awake changes nothing and says so.
	awake, err := s.PlanUnsnooze(ctx, id, "", "op-us-0", channels.Preview)
	if err != nil || len(awake.Warnings) != 1 || !strings.Contains(awake.Warnings[0], "not snoozed") {
		t.Fatalf("unsnooze awake plan = %+v, %v", awake, err)
	}

	// Expiry: nothing runs, the list reads the clock.
	cur := w.refetch(w.local)
	if _, _, err = s.Snooze(ctx, mustPlanSnooze(t, s, id, nil, time.Hour, cur.Revision, "op-sn-4")); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(listed(t, s, desk.Filter{}), local) {
		t.Fatal("not snoozed after snoozing again")
	}
	w.clock.Advance(2 * time.Hour)
	if !slices.Contains(listed(t, s, desk.Filter{}), local) {
		t.Error("an expired snooze still hides the conversation")
	}

	// Early unsnooze: idempotent.
	if _, _, err = s.Snooze(ctx, mustPlanSnooze(t, s, id, nil, time.Hour, "", "op-sn-5")); err != nil {
		t.Fatal(err)
	}
	up, err := s.PlanUnsnooze(ctx, id, "", "op-us-1", channels.Preview)
	if err != nil || len(up.Warnings) != 0 || up.Conversation.Snooze == nil {
		t.Fatalf("unsnooze plan = %+v, %v", up, err)
	}
	un, _, err := s.Snooze(ctx, mustPlanUnsnooze(t, s, id, "op-us-1"))
	if err != nil || un.Snooze != nil || !slices.Contains(listed(t, s, desk.Filter{}), local) {
		t.Fatalf("unsnooze = %+v, %v", un, err)
	}
	rev := un.Revision
	un2, _, err := s.Snooze(ctx, mustPlanUnsnooze(t, s, id, "op-us-2"))
	if err != nil || un2.Revision != rev {
		t.Fatalf("second unsnooze moved the record: %+v, %v", un2, err)
	}
	if n := len(w.workflowEvents(agent, model.EventConversationUnsnoozed)); n != 1 {
		t.Errorf("%d unsnoozed events, want 1", n)
	}
}

func mustPlanSnooze(t *testing.T, s desk.Service, id string, until *time.Time, d time.Duration, rev model.Revision, op model.OperationID) desk.SnoozePlan {
	t.Helper()
	p, err := s.PlanSnooze(t.Context(), id, until, d, rev, op, channels.Submit)
	if err != nil {
		t.Fatalf("plan snooze: %v", err)
	}
	return p
}

func mustPlanUnsnooze(t *testing.T, s desk.Service, id string, op model.OperationID) desk.SnoozePlan {
	t.Helper()
	p, err := s.PlanUnsnooze(t.Context(), id, "", op, channels.Submit)
	if err != nil {
		t.Fatalf("plan unsnooze: %v", err)
	}
	return p
}

// TestReadMarksArePerActor: two desk actors keep independent read
// state; marking is idempotent, moves neither the conversation's
// revision nor the log and queues no operation (no provider receipt);
// a viewer and an external actor cannot mark; `--through` and `--from`
// name a message of the conversation.
func TestReadMarksArePerActor(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s, s2 := w.as(agent), w.as(agent2)
	id := string(w.local.Ref.ID)
	local := w.local.Ref.ID
	conv := w.refetch(w.local)

	w.wake("e-read-1")
	first := w.firstInbound(w.local.Ref)
	w.wake("e-read-2")
	for _, who := range []desk.Service{s, s2} {
		if !slices.Contains(listed(t, who, desk.Filter{Workflow: desk.WorkflowFilter{Unread: true}}), local) {
			t.Fatal("an unread conversation is not listed as unread")
		}
	}
	events := w.eventCount()
	ops := w.operationCount()

	p, err := s.PlanReadMark(ctx, id, "", false)
	if err != nil || p.Current.Revision != "" || p.Through != nil {
		t.Fatalf("mark-read plan = %+v, %v", p, err)
	}
	m, err := s.ApplyReadMark(ctx, p)
	if err != nil || m.Actor != agent || m.ReadThrough.IsZero() || m.Revision == "" {
		t.Fatalf("mark-read = %+v, %v", m, err)
	}
	if listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Unread: true}}) != nil {
		t.Error("the actor that marked still lists the conversation unread")
	}
	if !slices.Contains(listed(t, s2, desk.Filter{Workflow: desk.WorkflowFilter{Unread: true}}), local) {
		t.Error("marking read moved another actor's read state")
	}
	if cur := w.refetch(w.local); cur.Revision != conv.Revision {
		t.Errorf("marking read moved the conversation's revision: %s, was %s", cur.Revision, conv.Revision)
	}
	// Again: same marker, nothing moves.
	p, err = s.PlanReadMark(ctx, id, "", false)
	if err != nil || p.Current.Revision != m.Revision {
		t.Fatalf("second plan current = %+v, %v", p.Current, err)
	}
	m2, err := s.ApplyReadMark(ctx, p)
	if err != nil || m2.Revision != m.Revision {
		t.Fatalf("repeating mark-read moved the marker: %+v, %v", m2, err)
	}
	// Unread from a message: everything from it on is unread again, for
	// this actor alone.
	pu, err := s.PlanReadMark(ctx, id, string(first.Ref.ID), true)
	if err != nil || pu.Through == nil || !pu.Through.Equal(first.RecordedAt.Add(-time.Nanosecond)) {
		t.Fatalf("mark-unread plan = %+v, %v", pu, err)
	}
	mu, err := s.ApplyReadMark(ctx, pu)
	if err != nil || mu.Revision == m.Revision || !mu.ReadThrough.Before(m.ReadThrough) {
		t.Fatalf("mark-unread = %+v, %v", mu, err)
	}
	if !slices.Contains(listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Unread: true}}), local) {
		t.Error("mark-unread --from did not make the conversation unread")
	}
	// Read through the first message only: the second is still unread.
	pr, err := s.PlanReadMark(ctx, id, string(first.Ref.ID), false)
	if err != nil || pr.Through == nil || !pr.Through.Equal(first.RecordedAt) {
		t.Fatalf("mark-read --through plan = %+v, %v", pr, err)
	}
	if _, err := s.ApplyReadMark(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Unread: true}}), local) {
		t.Error("reading through the first message left nothing unread")
	}

	// The marker is the actor's own and shows in `get` for it alone.
	rs, err := s.ReadState(ctx, conv)
	if err != nil || rs == nil || !rs.Marked || !rs.ReadThrough.Equal(first.RecordedAt) {
		t.Fatalf("read state = %+v, %v", rs, err)
	}
	if rs2, err := s2.ReadState(ctx, conv); err != nil || rs2 == nil || rs2.Marked {
		t.Fatalf("the other actor's read state = %+v, %v", rs2, err)
	}
	for _, who := range []model.ActorID{viewer, external} {
		if rs, err := w.as(who).ReadState(ctx, conv); err != nil || rs != nil {
			t.Errorf("%s sees a read state: %+v, %v", who, rs, err)
		}
		pl, err := w.as(who).PlanReadMark(ctx, id, "", false)
		if err == nil {
			_, err = w.as(who).ApplyReadMark(ctx, pl)
		}
		wantCode(t, err, model.Unauthorized, "")
	}

	// A message of another conversation, or none, is refused.
	if _, err := s.PlanReadMark(ctx, string(w.remote.Ref.ID), string(first.Ref.ID), false); !model.IsCode(err, model.NotFound) {
		t.Errorf("a message of another conversation: %v", err)
	}
	if _, err := s.PlanReadMark(ctx, id, "no-such-message", false); !model.IsCode(err, model.NotFound) {
		t.Errorf("unknown message: %v", err)
	}

	// No event, no operation, no provider effect: a read marker is local
	// and per actor, never a receipt.
	if got := w.eventCount(); got != events {
		t.Errorf("marking wrote %d events", got-events)
	}
	if got := w.operationCount(); got != ops {
		t.Errorf("marking queued %d operations", got-ops)
	}
	select {
	case e := <-w.fake.Effects():
		t.Errorf("marking reached the provider: %+v", e)
	default:
	}
}

func (w *world) firstInbound(conv model.ConversationRef) model.Message {
	w.t.Helper()
	for _, m := range w.messages(conv) {
		if m.Origin == nil && len(m.External) > 0 {
			return m
		}
	}
	w.t.Fatal("no inbound message")
	return model.Message{}
}

func (w *world) eventCount() int {
	w.t.Helper()
	page, err := w.client(agent).Events().List(w.t.Context(), model.ListEvents{Call: call, Start: model.StartBeginning, Limit: 1000})
	if err != nil {
		w.t.Fatal(err)
	}
	return len(page.Items)
}

func (w *world) operationCount() int {
	w.t.Helper()
	page, err := w.client(operator).Operations().List(w.t.Context(), model.ListRequest{Call: call, Page: model.PageRequest{Limit: 100}})
	if err != nil {
		w.t.Fatal(err)
	}
	return len(page.Items)
}

// TestWorkflowFiltersAreUmas: the assignee, linked and unread filters
// reach uma and combine with the inbox and status filters; pages are
// full, since uma applies them.
func TestWorkflowFiltersAreUmas(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s, s2 := w.as(agent), w.as(agent2)
	local, remote := w.local.Ref.ID, w.remote.Ref.ID
	lc, rc := w.refetch(w.local), w.refetch(w.remote)

	if _, _, err := s.Assign(ctx, mustPlanAssign(t, s, string(local), "self", lc.Revision, "op-f-1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Assign(ctx, mustPlanAssign(t, s, string(remote), string(agent2), rc.Revision, "op-f-2")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who  desk.Service
		f    string
		want []model.ID
	}{
		{s, "self", []model.ID{local}},
		{s2, "self", []model.ID{remote}},
		{s, string(agent2), []model.ID{remote}},
		{s, "none", nil},
	} {
		got := listed(t, c.who, desk.Filter{Workflow: desk.WorkflowFilter{Assignee: c.f}})
		if !slices.Equal(got, c.want) {
			t.Errorf("--assignee %s as %s lists %v, want %v", c.f, c.who.Actor, got, c.want)
		}
	}
	// Combined with inbox and status: AND.
	for _, c := range []struct {
		f    desk.Filter
		want []model.ID
	}{
		{desk.Filter{Inbox: "local", Workflow: desk.WorkflowFilter{Assignee: "self"}}, []model.ID{local}},
		{desk.Filter{Inbox: "provider", Workflow: desk.WorkflowFilter{Assignee: "self"}}, nil},
		{desk.Filter{Statuses: []string{model.ConversationResolved}, Workflow: desk.WorkflowFilter{Assignee: "self"}}, nil},
		{desk.Filter{Statuses: []string{model.ConversationOpen}, Workflow: desk.WorkflowFilter{Assignee: "self"}}, []model.ID{local}},
	} {
		if got := listed(t, s, c.f); !slices.Equal(got, c.want) {
			t.Errorf("%+v lists %v, want %v", c.f, got, c.want)
		}
	}
	// Linked, and not linked.
	if got := listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Linked: boolp(true)}}); got != nil {
		t.Errorf("--linked lists %v before any link", got)
	}
	// A page is as large as asked: uma, not heo, applies the filter.
	p, err := s.ListConversations(ctx, desk.Filter{Workflow: desk.WorkflowFilter{Assignee: "none"}}, model.PageRequest{Limit: 1})
	if err != nil || len(p.Items) != 0 || !p.Complete {
		t.Errorf("a page of a filter nothing matches = %+v, %v", p, err)
	}
	// A bad assignee is validation.
	_, err = s.ListConversations(ctx, desk.Filter{Workflow: desk.WorkflowFilter{Assignee: "bad\x00actor"}}, model.PageRequest{Limit: 1})
	wantCode(t, err, model.Validation, "assignee")
}

func boolp(b bool) *bool { return &b }
