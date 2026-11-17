package desk_test

import (
	"errors"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

// retainHour keeps events an hour and everything else a long time.
func retainHour(c *uma.Config) {
	long := 1000 * time.Hour
	c.Retention = uma.RetentionPolicy{
		Events: time.Hour, Operations: long, Ingress: long, Attempts: long, Staging: long, Exports: long, Contacts: long,
	}
}

// drain polls s's subscription sub until a poll scans nothing, acking
// each batch, and returns every event it saw.
func drain(t *testing.T, s desk.Service, sub model.SubscriptionRef) []model.Event {
	t.Helper()
	var out []model.Event
	for range 100 {
		b, err := s.PollWatch(t.Context(), sub, 0)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b.Events...)
		if b.Token == "" {
			return out
		}
		if _, err := s.AckWatch(t.Context(), sub, b.Token); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("subscription never drained")
	return nil
}

func eventIDs(evs []model.Event) map[model.ID]bool {
	out := map[model.ID]bool{}
	for _, ev := range evs {
		out[ev.ID] = true
	}
	return out
}

// TestWatchCheckpoints: a watch acknowledges each batch, so reopening
// the consumer resumes after it; the checkpoint is a cursor another
// consumer starts at; a cursor the open subscription is not at is a
// gap, refused; a position behind retention is cursor_expired.
func TestWatchCheckpoints(t *testing.T) {
	w := newWorld(t, retainHour)
	ctx := t.Context()
	s := w.as(agent)
	sub, err := s.OpenWatch(ctx, desk.WatchStart{Consumer: "c1", From: desk.StartBeginning}, "open-1")
	if err != nil {
		t.Fatal(err)
	}
	start := sub.Checkpoint
	first := drain(t, s, sub.Ref)
	if len(first) == 0 {
		t.Fatal("no events from the beginning")
	}
	again, err := s.OpenWatch(ctx, desk.WatchStart{Consumer: "c1", From: desk.StartBeginning}, "open-2")
	if err != nil || again.Checkpoint == start {
		t.Fatalf("reopened at %s (start %s), %v: the acks did not move the checkpoint", again.Checkpoint, start, err)
	}
	if got := drain(t, s, sub.Ref); len(got) != 0 {
		t.Fatalf("resumed watch redelivered %d events", len(got))
	}
	w.note("after the checkpoint")
	if got := drain(t, s, sub.Ref); len(got) != 1 || got[0].Kind != model.EventMessageCreated {
		t.Fatalf("new events = %+v", got)
	}

	// Resuming at the cursor the subscription holds is fine; any other
	// is a gap.
	at, err := s.OpenWatch(ctx, desk.WatchStart{Consumer: "c1"}, "open-3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenWatch(ctx, desk.WatchStart{Consumer: "c1", Cursor: at.Checkpoint}, "open-4"); err != nil {
		t.Fatalf("resume at its own checkpoint: %v", err)
	}
	_, err = s.OpenWatch(ctx, desk.WatchStart{Consumer: "c1", Cursor: again.Checkpoint}, "open-5")
	var gap *desk.GapError
	if !errors.As(err, &gap) || gap.At != at.Checkpoint || gap.Asked != again.Checkpoint {
		t.Fatalf("err = %v, want a gap", err)
	}
	wantCode(t, err, model.Conflict, "skip or repeat")

	// Another consumer starts at a checkpoint: only what came after.
	c2, err := s.OpenWatch(ctx, desk.WatchStart{Consumer: "c2", Cursor: again.Checkpoint}, "open-6")
	if err != nil || c2.Checkpoint != again.Checkpoint {
		t.Fatalf("c2 = %+v, %v", c2, err)
	}
	if got := drain(t, s, c2.Ref); len(got) != 1 {
		t.Fatalf("c2 from the cursor = %d events, want the one after it", len(got))
	}
	if tail, err := s.OpenWatch(ctx, desk.WatchStart{Consumer: "c3"}, "open-7"); err != nil || len(drain(t, s, tail.Ref)) != 0 {
		t.Fatalf("tail = %+v, %v; want nothing old", tail, err)
	}

	// Purged past the checkpoint: cursor_expired, on poll and on open.
	w.note("unread, then purged")
	w.clock.Advance(2 * time.Hour)
	w.note("keeps the log non-empty")
	if rep, err := w.e.Runtime().Purge(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call}); err != nil || rep.Events == 0 {
		t.Fatalf("purge = %+v, %v", rep, err)
	}
	_, err = s.PollWatch(ctx, sub.Ref, 0)
	wantCode(t, err, model.CursorExpired, "")
	_, err = s.OpenWatch(ctx, desk.WatchStart{Consumer: "c4", Cursor: again.Checkpoint}, "open-8")
	wantCode(t, err, model.CursorExpired, "")
}

// TestWatchReauthorizesEachEvent: heo passes every event of a batch
// through the caller's audience rule again, whatever uma returned: a
// desk whose rule refuses internal content gets no note event, though
// uma would hand the desk actor one; the batch token still moves the
// checkpoint past it. A viewer never sees internal content either.
func TestWatchReauthorizesEachEvent(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	note := w.note("internal only")

	full := w.as(agent)
	sub, err := full.OpenWatch(ctx, desk.WatchStart{Consumer: "desk", From: desk.StartBeginning}, "open-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := full.PollWatch(ctx, sub.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	internal := 0
	for _, ev := range b.Events {
		if ev.Audience.Visibility != model.Public && ev.Audience.Visibility != "" {
			internal++
		}
	}
	if internal == 0 {
		t.Fatalf("desk batch holds no internal event (note %s): the test proves nothing", note.ID)
	}

	public := w.as(agent)
	public.Sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	sub, err = public.OpenWatch(ctx, desk.WatchStart{Consumer: "public", From: desk.StartBeginning}, "open-2")
	if err != nil {
		t.Fatal(err)
	}
	b, err = public.PollWatch(ctx, sub.Ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range b.Events {
		if ev.Audience.Visibility != model.Public {
			t.Fatalf("heo passed a %s event %s to a caller limited to public content", ev.Audience.Visibility, ev.Kind)
		}
	}
	if b.Token == "" {
		t.Fatal("no token for the batch")
	}
	if _, err := public.AckWatch(ctx, sub.Ref, b.Token); err != nil {
		t.Fatal(err)
	}
	if got := drain(t, public, sub.Ref); len(got) != 0 {
		t.Fatalf("after the ack: %d events", len(got))
	}

	v := w.as(viewer)
	sub, err = v.OpenWatch(ctx, desk.WatchStart{Consumer: "viewer", From: desk.StartBeginning}, "open-3")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range drain(t, v, sub.Ref) {
		if ev.Audience.Visibility != model.Public || eventIDs([]model.Event{ev})[note.ID] {
			t.Fatalf("viewer saw %+v", ev)
		}
	}
	if op := desk.AckOperation("t"); op.Validate() != nil || op != desk.AckOperation("t") || op == desk.AckOperation("u") {
		t.Fatalf("ack operation %q is not a valid operation ID derived from the token", op)
	}
}
