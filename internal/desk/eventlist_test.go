package desk_test

import (
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

// listAll pages s's event list from start with the given scan limit,
// following Next until the list is complete, and returns the pages.
func listAll(t *testing.T, s desk.Service, f desk.EventFilter, start desk.ListStart, limit int) [][]model.Event {
	t.Helper()
	var pages [][]model.Event
	for range 500 {
		p, err := s.ListEvents(t.Context(), f, start, limit)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, p.Items)
		if p.Complete {
			return pages
		}
		if p.Next == "" || p.Next == start.Cursor {
			t.Fatalf("page did not advance: next %q at %q", p.Next, start.Cursor)
		}
		start = desk.ListStart{Cursor: p.Next}
	}
	t.Fatal("list never completed")
	return nil
}

func flatten(pages [][]model.Event) []model.Event {
	var out []model.Event
	for _, p := range pages {
		out = append(out, p...)
	}
	return out
}

func internalIn(evs []model.Event) int {
	n := 0
	for _, ev := range evs {
		if ev.Audience.Visibility != model.Public && ev.Audience.Visibility != "" {
			n++
		}
	}
	return n
}

// TestListEventsPagesWithoutSkipping: a list in small pages, each
// continued at the previous page's next, shows exactly the events a
// watch from the beginning shows, in order, none twice; a page bounds
// the events it scans; a list continued from where the last one ended
// shows only what came after.
func TestListEventsPagesWithoutSkipping(t *testing.T) {
	w := newWorld(t)
	w.note("one")
	w.note("two")
	s := w.as(agent)

	sub, err := s.OpenWatch(t.Context(), desk.WatchStart{Consumer: "ref", From: desk.StartBeginning}, "open-ref")
	if err != nil {
		t.Fatal(err)
	}
	want := drain(t, s, sub.Ref)
	if len(want) < 6 {
		t.Fatalf("only %d events to page over", len(want))
	}

	pages := listAll(t, s, desk.EventFilter{}, desk.ListStart{From: desk.StartBeginning}, 2)
	if len(pages) < 3 {
		t.Fatalf("%d pages of 2 over %d events", len(pages), len(want))
	}
	for i, p := range pages {
		if len(p) > 2 {
			t.Fatalf("page %d holds %d events, scan limit 2", i, len(p))
		}
	}
	got := flatten(pages)
	if len(got) != len(want) {
		t.Fatalf("listed %d events, watch saw %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("event %d = %s, watch saw %s", i, got[i].ID, want[i].ID)
		}
	}

	// A list at the tail is empty and complete, and its next continues.
	tail, err := s.ListEvents(t.Context(), desk.EventFilter{}, desk.ListStart{From: desk.StartTail}, 10)
	if err != nil || len(tail.Items) != 0 || !tail.Complete || tail.Next == "" {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	n := w.note("after the tail")
	after, err := s.ListEvents(t.Context(), desk.EventFilter{}, desk.ListStart{Cursor: tail.Next}, 10)
	if err != nil || len(after.Items) == 0 || !after.Complete {
		t.Fatalf("after the tail = %+v, %v", after, err)
	}
	var found bool
	for _, ev := range after.Items {
		found = found || (ev.Kind == model.EventMessageCreated && ev.Resource.ID == n.ID)
		if eventIDs(want)[ev.ID] {
			t.Fatalf("a list from the tail repeated %s", ev.ID)
		}
	}
	if !found {
		t.Fatalf("the note's event is not in %+v", after.Items)
	}
}

// TestListEventsFilters: the list narrows to an inbox (by key or ID)
// and to event kinds; an inbox that does not exist is not found, an
// unknown kind is refused, and neither is silently ignored.
func TestListEventsFilters(t *testing.T) {
	w := newWorld(t)
	s := w.as(agent)
	start := desk.ListStart{From: desk.StartBeginning}

	all := flatten(listAll(t, s, desk.EventFilter{}, start, 1000))
	byInbox := map[model.ID]int{}
	for _, ev := range all {
		byInbox[ev.Inbox]++
	}
	if len(byInbox) < 2 {
		t.Fatalf("events of %d inboxes, want both", len(byInbox))
	}

	for _, ref := range []string{"provider", string(w.remote.Ref.Inbox.ID)} {
		got := flatten(listAll(t, s, desk.EventFilter{Inbox: ref}, start, 1000))
		if len(got) == 0 || len(got) != byInbox[w.remote.Ref.Inbox.ID] {
			t.Fatalf("--inbox %s: %d events, inbox holds %d", ref, len(got), byInbox[w.remote.Ref.Inbox.ID])
		}
		for _, ev := range got {
			if ev.Inbox != w.remote.Ref.Inbox.ID {
				t.Fatalf("--inbox %s listed an event of inbox %s", ref, ev.Inbox)
			}
		}
	}

	created := flatten(listAll(t, s, desk.EventFilter{Kinds: []string{model.EventMessageCreated}}, start, 1000))
	if len(created) == 0 || len(created) >= len(all) {
		t.Fatalf("%d message.created of %d events", len(created), len(all))
	}
	for _, ev := range created {
		if ev.Kind != model.EventMessageCreated {
			t.Fatalf("--kind message.created listed %s", ev.Kind)
		}
	}

	both := flatten(listAll(t, s, desk.EventFilter{Inbox: "local", Kinds: []string{model.EventMessageCreated}}, start, 1000))
	if len(both) == 0 || len(both) >= len(created) {
		t.Fatalf("inbox and kind together = %d events of %d", len(both), len(created))
	}

	_, err := s.ListEvents(t.Context(), desk.EventFilter{Inbox: "nowhere"}, start, 10)
	wantCode(t, err, model.NotFound, "nowhere")
	_, err = s.ListEvents(t.Context(), desk.EventFilter{Kinds: []string{"no.such.kind"}}, start, 10)
	wantCode(t, err, model.Validation, "")
}

// TestListEventsAppliesAudiencePerEvent: an internal note's event is
// listed to the desk actor and to no one else, whatever uma returned: a
// viewer and an external participant never see it, and neither does a
// desk whose rule refuses internal content. A page of events the caller
// may not read is empty but not complete, and its next moves past them,
// so the list goes on to the events after.
func TestListEventsAppliesAudiencePerEvent(t *testing.T) {
	w := newWorld(t)
	note := w.note("internal only")
	start := desk.ListStart{From: desk.StartBeginning}

	deskSees := flatten(listAll(t, w.as(agent), desk.EventFilter{}, start, 1000))
	if internalIn(deskSees) == 0 || !eventIDs(deskSees)[eventOf(t, deskSees, note.ID).ID] {
		t.Fatalf("desk events hold no internal note event (note %s): the test proves nothing", note.ID)
	}

	publicOnly := w.as(agent)
	publicOnly.Sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	for name, s := range map[string]desk.Service{"viewer": w.as(viewer), "external": w.as(external), "public-only desk": publicOnly} {
		got := flatten(listAll(t, s, desk.EventFilter{}, start, 1000))
		if len(got) == 0 {
			t.Fatalf("%s sees no events at all", name)
		}
		if internalIn(got) != 0 {
			t.Fatalf("%s listed a non-public event", name)
		}
		for _, ev := range got {
			if ev.Resource.ID == note.ID {
				t.Fatalf("%s listed the internal note's event %s", name, ev.ID)
			}
		}
	}

	// One event per page: pages of events the external may not read are
	// empty, yet the list moves on and reaches the events after them.
	pages := listAll(t, w.as(external), desk.EventFilter{}, start, 1)
	var hidden int
	for _, p := range pages {
		if len(p) == 0 {
			hidden++
		}
	}
	if hidden == 0 {
		t.Fatalf("no empty page among %d: the internal events were not scanned past", len(pages))
	}
	last := pages[len(pages)-1]
	if len(last) > 1 {
		t.Fatalf("last page = %d events", len(last))
	}
}

func eventOf(t *testing.T, evs []model.Event, resource model.ID) model.Event {
	t.Helper()
	for _, ev := range evs {
		if ev.Resource.ID == resource && ev.Kind == model.EventMessageCreated {
			return ev
		}
	}
	t.Fatalf("no message.created event of %s", resource)
	return model.Event{}
}

// TestListEventsShowsRetentionGap: a list continued at a position
// that event retention purged is cursor_expired, never a list that
// quietly starts at what is left; restarting at the beginning works.
func TestListEventsShowsRetentionGap(t *testing.T) {
	w := newWorld(t, retainHour)
	ctx := t.Context()
	s := w.as(agent)
	first, err := s.ListEvents(ctx, desk.EventFilter{}, desk.ListStart{From: desk.StartBeginning}, 2)
	if err != nil || first.Next == "" || first.Complete {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	w.clock.Advance(2 * time.Hour)
	w.note("keeps the log non-empty")
	if rep, err := w.e.Runtime().Purge(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call}); err != nil || rep.Events == 0 {
		t.Fatalf("purge = %+v, %v", rep, err)
	}
	_, err = s.ListEvents(ctx, desk.EventFilter{}, desk.ListStart{Cursor: first.Next}, 2)
	wantCode(t, err, model.CursorExpired, "retention")
	restart, err := s.ListEvents(ctx, desk.EventFilter{}, desk.ListStart{From: desk.StartBeginning}, 1000)
	if err != nil || len(restart.Items) == 0 {
		t.Fatalf("restart at the beginning = %+v, %v", restart, err)
	}
}

// TestListEventsNeedsAStart: a list names where it starts.
func TestListEventsNeedsAStart(t *testing.T) {
	w := newWorld(t)
	_, err := w.as(agent).ListEvents(t.Context(), desk.EventFilter{}, desk.ListStart{}, 10)
	wantCode(t, err, model.Validation, "")
	_, err = w.as(agent).ListEvents(t.Context(), desk.EventFilter{}, desk.ListStart{From: "middle"}, 10)
	wantCode(t, err, model.Validation, "")
}
