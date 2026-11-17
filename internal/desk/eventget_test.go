package desk_test

import (
	"strings"
	"testing"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// TestGetEventReadsOneEvent: an event listed is read by its ID, as
// stored, by a caller who may read it.
func TestGetEventReadsOneEvent(t *testing.T) {
	w := newWorld(t)
	s := w.as(agent)
	all := flatten(listAll(t, s, desk.EventFilter{}, desk.ListStart{From: desk.StartBeginning}, 1000))
	if len(all) == 0 {
		t.Fatal("no events to read")
	}
	for _, want := range all {
		got, err := s.GetEvent(t.Context(), string(want.ID))
		if err != nil {
			t.Fatalf("event %s: %v", want.ID, err)
		}
		if got.ID != want.ID || got.Kind != want.Kind || got.Resource != want.Resource || got.Audience != want.Audience {
			t.Fatalf("event %s = %+v, listed as %+v", want.ID, got, want)
		}
	}
}

// TestGetEventAppliesAudience: an internal note's event is read by the
// desk actor and, for anyone else, is exactly an event that does not
// exist: the same code and the same message, whatever uma returned, so
// the answer does not say the event is there. A desk whose rule refuses
// internal content gets the same answer.
func TestGetEventAppliesAudience(t *testing.T) {
	w := newWorld(t)
	note := w.note("internal only")
	all := flatten(listAll(t, w.as(agent), desk.EventFilter{}, desk.ListStart{From: desk.StartBeginning}, 1000))
	ev := eventOf(t, all, note.ID)
	if ev.Audience.Visibility == model.Public {
		t.Fatalf("the note's event is public: the test proves nothing (%+v)", ev.Audience)
	}
	if got, err := w.as(agent).GetEvent(t.Context(), string(ev.ID)); err != nil || got.ID != ev.ID {
		t.Fatalf("desk reads the note's event: %+v, %v", got, err)
	}

	publicOnly := w.as(agent)
	publicOnly.Sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	missing := "evt-no-such"
	_, absent := w.as(agent).GetEvent(t.Context(), missing)
	wantCode(t, absent, model.NotFound, "not found")
	for name, s := range map[string]desk.Service{"viewer": w.as(viewer), "external": w.as(external), "public-only desk": publicOnly} {
		_, err := s.GetEvent(t.Context(), string(ev.ID))
		wantCode(t, err, model.NotFound, "not found")
		// Same wording as an event that does not exist, apart from the ID asked.
		if got, want := strings.ReplaceAll(err.Error(), string(ev.ID), "X"), strings.ReplaceAll(absent.Error(), missing, "X"); got != want {
			t.Fatalf("%s: unreadable event answers %q, a missing one %q", name, got, want)
		}
	}
}

// TestGetEventRefusals: no ID is a validation error; an ID that is not
// of this log, or malformed, is not found.
func TestGetEventRefusals(t *testing.T) {
	w := newWorld(t)
	s := w.as(agent)
	_, err := s.GetEvent(t.Context(), "")
	wantCode(t, err, model.Validation, "event")
	_, err = s.GetEvent(t.Context(), "evt nope/../x")
	wantCode(t, err, model.NotFound, "not found")
	_, err = s.GetEvent(t.Context(), "evt-9999999")
	wantCode(t, err, model.NotFound, "not found")
}
