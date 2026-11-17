package desk_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// listKinds lists the events of kinds that who may read, through heo.
func listKinds(t *testing.T, who desk.Service, inbox string, kinds ...string) []model.Event {
	t.Helper()
	return flatten(listAll(t, who, desk.EventFilter{Inbox: inbox, Kinds: kinds}, desk.ListStart{From: desk.StartBeginning}, 100))
}

func markerPayload(t *testing.T, ev model.Event) model.ReadMarkerPayload {
	t.Helper()
	var p model.ReadMarkerPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatalf("read marker payload: %v", err)
	}
	return p
}

// TestReadMarkerEventsAreActorScoped: one event per actual move of a
// read marker, readable by the marker's own actor alone: another desk
// actor lists none, a repeated mark adds none, and `--inbox` narrows
// them. The payload is references, never a message.
func TestReadMarkerEventsAreActorScoped(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s, s2 := w.as(agent), w.as(agent2)
	id := string(w.local.Ref.ID)
	w.wake("e-marker-1")
	first := w.firstInbound(w.local.Ref)
	both := []string{model.EventConversationReadMarked, model.EventConversationUnreadMarked}

	p, err := s.PlanReadMark(ctx, id, "", false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.ApplyReadMark(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	evs := listKinds(t, s, "", both...)
	if len(evs) != 1 || evs[0].Kind != model.EventConversationReadMarked {
		t.Fatalf("after mark-read the actor lists %+v, want one read_marked", evs)
	}
	got := markerPayload(t, evs[0])
	if got.Actor != agent || got.Conversation.ID != w.local.Ref.ID || got.Revision != m.Revision {
		t.Errorf("payload = %+v, want actor %s, conversation %s, revision %s", got, agent, w.local.Ref.ID, m.Revision)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(evs[0].Payload.Data, &keys); err != nil || len(keys) != 3 {
		t.Errorf("a marker payload is references only (conversation, actor, revision): %s", evs[0].Payload.Data)
	}

	// Another desk actor sees none of them, whatever it asks for.
	if got := listKinds(t, s2, "", both...); len(got) != 0 {
		t.Errorf("another desk actor lists the marker's events: %+v", got)
	}
	if got := listKinds(t, s2, ""); slices.ContainsFunc(got, func(e model.Event) bool { return model.IsReadMarkerEvent(e.Kind) }) {
		t.Error("another desk actor's unfiltered list holds a marker event")
	}

	// Repeating changes nothing: no second event.
	again, err := s.PlanReadMark(ctx, id, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyReadMark(ctx, again); err != nil {
		t.Fatal(err)
	}
	if got := listKinds(t, s, "", both...); len(got) != 1 {
		t.Fatalf("a repeated mark-read left %d events, want 1", len(got))
	}

	// Unread from a message is a second kind, one event.
	pu, err := s.PlanReadMark(ctx, id, string(first.Ref.ID), true)
	if err != nil {
		t.Fatal(err)
	}
	mu, err := s.ApplyReadMark(ctx, pu)
	if err != nil {
		t.Fatal(err)
	}
	un := listKinds(t, s, "", model.EventConversationUnreadMarked)
	if len(un) != 1 || markerPayload(t, un[0]).Revision != mu.Revision {
		t.Fatalf("unread_marked events = %+v, want one at revision %s", un, mu.Revision)
	}

	// The actor's marks are in `--inbox` of the conversation's inbox only.
	if got := listKinds(t, s, "local", both...); len(got) != 2 {
		t.Errorf("--inbox local lists %d marker events, want 2", len(got))
	}
	if got := listKinds(t, s, "provider", both...); len(got) != 0 {
		t.Errorf("--inbox provider lists %d marker events, want 0", len(got))
	}

	// The other actor's own mark is its own event, not shared.
	p2, err := s2.PlanReadMark(ctx, id, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ApplyReadMark(ctx, p2); err != nil {
		t.Fatal(err)
	}
	if got := listKinds(t, s2, "", both...); len(got) != 1 || markerPayload(t, got[0]).Actor != agent2 {
		t.Errorf("the second actor lists %+v, want its own one event", got)
	}
	if got := listKinds(t, s, "", both...); len(got) != 2 {
		t.Errorf("the first actor lists %d marker events after the other's mark, want 2", len(got))
	}
	// A viewer holds no right to mark and lists none.
	if got := listKinds(t, w.as(viewer), "", both...); len(got) != 0 {
		t.Errorf("a viewer lists marker events: %+v", got)
	}
}

func snoozeEnded(t *testing.T, w *world, who model.ActorID) []model.Event {
	t.Helper()
	return w.workflowEvents(who, model.EventConversationSnoozeEnded)
}

func awaitSnoozeEnded(t *testing.T, w *world, n int) []model.Event {
	t.Helper()
	for range 400 {
		if evs := snoozeEnded(t, w, agent); len(evs) >= n {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("fewer than %d snooze_ended events", n)
	return nil
}

func endedPayload(t *testing.T, ev model.Event) model.ConversationSnoozeEndedPayload {
	t.Helper()
	var p model.ConversationSnoozeEndedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatalf("snooze_ended payload: %v", err)
	}
	return p
}

// TestSnoozeEndsWithAnEventFromTheSweeper: a snooze that reaches its
// time and one an inbound message ends each emit one event, from the
// runtime's sweep and not before; both desk actors read it, an external
// actor does not, and `--inbox` narrows it. No operation, no body.
func TestSnoozeEndsWithAnEventFromTheSweeper(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	id := string(w.local.Ref.ID)
	stop := sweeper(w, time.Minute)
	defer stop()

	// By time.
	cur := w.refetch(w.local)
	if _, _, err := s.Snooze(ctx, mustPlanSnooze(t, s, id, nil, time.Hour, cur.Revision, "op-se-1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(snoozeEnded(t, w, agent)); n != 0 {
		t.Fatalf("%d snooze_ended events while snoozed", n)
	}
	w.clock.Advance(2 * time.Hour)
	evs := awaitSnoozeEnded(t, w, 1)
	if len(evs) != 1 {
		t.Fatalf("%d events for one wake", len(evs))
	}
	p := endedPayload(t, evs[0])
	if p.Cause != model.WakeCauseUntil || p.Conversation.ID != w.local.Ref.ID || p.Trigger != model.TriggerSystem {
		t.Errorf("payload = %+v, want cause until on %s", p, w.local.Ref.ID)
	}
	if got := w.refetch(w.local); got.Snooze != nil {
		t.Errorf("the sweep left the snooze: %+v", got.Snooze)
	}

	// By a message.
	cur = w.refetch(w.local)
	if _, _, err := s.Snooze(ctx, mustPlanSnooze(t, s, id, nil, 24*time.Hour, cur.Revision, "op-se-2")); err != nil {
		t.Fatal(err)
	}
	w.wake("e-snooze-ended-1")
	w.clock.Advance(2 * time.Minute)
	evs = awaitSnoozeEnded(t, w, 2)
	if len(evs) != 2 {
		t.Fatalf("%d events after two wakes", len(evs))
	}
	if p := endedPayload(t, evs[1]); p.Cause != model.WakeCauseMessage {
		t.Errorf("second wake cause = %q, want message", p.Cause)
	}

	// Who reads it, and the inbox filter.
	if got := snoozeEnded(t, w, agent2); len(got) != 2 {
		t.Errorf("another desk actor lists %d, want 2", len(got))
	}
	if got := snoozeEnded(t, w, external); len(got) != 0 {
		t.Errorf("an external actor lists %+v", got)
	}
	if got := listKinds(t, s, "local", model.EventConversationSnoozeEnded); len(got) != 2 {
		t.Errorf("--inbox local lists %d, want 2", len(got))
	}
	if got := listKinds(t, s, "provider", model.EventConversationSnoozeEnded); len(got) != 0 {
		t.Errorf("--inbox provider lists %d, want 0", len(got))
	}
}

// TestSnoozeEndedFilterIsUmas: `SnoozeEnded` keeps the conversations
// whose snooze has run out and the sweep has not yet cleared; once it
// has, none. A caller that may not read workflow state is refused.
func TestSnoozeEndedFilterIsUmas(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	local, remote := w.local.Ref.ID, w.remote.Ref.ID
	cur := w.refetch(w.local)
	if _, _, err := s.Snooze(ctx, mustPlanSnooze(t, s, string(local), nil, time.Hour, cur.Revision, "op-sf-1")); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	ended := func(b *bool) []model.ID {
		return listed(t, s, desk.Filter{Workflow: desk.WorkflowFilter{Snoozed: desk.SnoozeInclude, SnoozeEnded: b}})
	}
	if got := ended(&yes); len(got) != 0 {
		t.Fatalf("snooze-ended lists %v while the snooze runs", got)
	}
	w.clock.Advance(2 * time.Hour)
	if got := ended(&yes); !slices.Equal(got, []model.ID{local}) {
		t.Errorf("snooze-ended lists %v, want [%s]", got, local)
	}
	if got := ended(&no); slices.Contains(got, local) || !slices.Contains(got, remote) {
		t.Errorf("snooze-ended=false lists %v", got)
	}
	stop := sweeper(w, time.Minute)
	defer stop()
	w.clock.Advance(2 * time.Minute)
	for range 400 {
		if len(ended(&yes)) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := ended(&yes); len(got) != 0 {
		t.Errorf("snooze-ended lists %v after the sweep", got)
	}
	_, err := w.as(external).ListConversations(ctx, desk.Filter{Workflow: desk.WorkflowFilter{SnoozeEnded: &yes}}, model.PageRequest{Limit: 10})
	wantCode(t, err, model.Unauthorized, "workflow")
}
