package e2e

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// eventPageJSON is `event list --format json`.
type eventPageJSON struct {
	Items    []model.Event
	Next     model.Cursor
	Complete bool
}

// listEvents runs `event list` and decodes its page.
func (s *deskSite) listEvents(args ...string) eventPageJSON {
	s.t.Helper()
	var p eventPageJSON
	s.json(&p, append([]string{"event", "list"}, args...)...)
	return p
}

// listAllEvents pages `event list` with the given arguments, each page
// continued at the previous page's next, until it is complete.
func (s *deskSite) listAllEvents(limit string, args ...string) (events []model.Event, pages []eventPageJSON) {
	s.t.Helper()
	cursor := ""
	for range 500 {
		a := append([]string{"--limit", limit}, args...)
		if cursor != "" {
			a = append(a, "--cursor", cursor)
		}
		p := s.listEvents(a...)
		events, pages = append(events, p.Items...), append(pages, p)
		if p.Complete {
			return events, pages
		}
		if p.Next == "" || string(p.Next) == cursor {
			s.t.Fatalf("page did not advance: next %q at %q", p.Next, cursor)
		}
		cursor = string(p.Next)
	}
	s.t.Fatal("event list never completed")
	return nil, nil
}

// TestEventList drives the release binary on Postgres through a real
// widget conversation: pages continued at their next show every event
// once and in order, the same ones a watch from the beginning shows; a
// list from the tail is empty and its next continues without a gap or a
// repeat; --inbox and --kind narrow it, and a bad one is refused; a
// cursor behind event retention is CURSOR_EXPIRED, never a list that
// starts at what is left; and listing opens no subscription. An
// external participant lists no internal-note event, a desk actor does.
func TestEventList(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)
	reply := func(text, key string) replyJSON {
		t.Helper()
		var r replyJSON
		s.json(&r, "message", "reply", id, "--text", text, "--idempotency-key", key)
		return r
	}
	created := func(msg model.ID) func(model.Event) bool {
		return func(ev model.Event) bool { return ev.Kind == model.EventMessageCreated && ev.Resource.ID == msg }
	}
	has := func(evs []model.Event, match func(model.Event) bool) bool {
		for _, ev := range evs {
			if match(ev) {
				return true
			}
		}
		return false
	}

	r1 := reply("first reply", "r1")
	s.waitDelivery(r1.Operation.ID, model.ProviderAccepted, model.Delivered)
	var note noteJSON
	s.json(&note, "note", "add", id, "--text", "internal secret", "--placement", "local")
	noteEvent := created(note.Message)

	// Pages show what a watch from the beginning shows, once, in order.
	w := s.s.watch("--consumer", "ref", "--from", "beginning", "--format", "json")
	watched := w.until(noteEvent)
	if code, stderr := w.interrupt(); code != 0 {
		t.Fatalf("watch exit %d\n%s", code, stderr)
	}
	all, pages := s.listAllEvents("2")
	if len(pages) < 3 {
		t.Fatalf("%d pages of 2 over %d events", len(pages), len(all))
	}
	for i, p := range pages {
		if len(p.Items) > 2 {
			t.Fatalf("page %d holds %d events, scan limit 2", i, len(p.Items))
		}
	}
	if !has(all, created(r1.Message)) || !has(all, noteEvent) {
		t.Fatalf("listed events lack the reply or the note:\n%v", all)
	}
	seen := map[model.ID]bool{}
	for _, ev := range all {
		if seen[ev.ID] {
			t.Fatalf("event %s listed twice", ev.ID)
		}
		seen[ev.ID] = true
	}
	for i, ev := range watched {
		if all[i].ID != ev.ID {
			t.Fatalf("event %d = %s, the watch saw %s", i, all[i].ID, ev.ID)
		}
	}
	var noteID model.ID
	for _, ev := range all {
		if noteEvent(ev) {
			noteID = ev.ID
			if ev.Audience.Visibility == model.Public {
				t.Fatalf("the note's event is public: the audience checks below prove nothing (%+v)", ev.Audience)
			}
		}
	}

	// Text: a row per event, and where the next page starts on stderr.
	r := s.heo(0, "event", "list", "--limit", "2")
	for _, want := range []string{"KIND", "RECORDED", "VISIBILITY", string(all[0].ID)} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("event list lacks %q:\n%s", want, r.stdout)
		}
	}
	if !strings.Contains(r.stderr, "more: --cursor "+string(pages[0].Next)) {
		t.Errorf("event list does not say where the next page starts:\n%s", r.stderr)
	}

	// From the tail: empty, and its next continues without a gap or a
	// repeat.
	tail := s.listEvents("--from", "tail")
	if len(tail.Items) != 0 || !tail.Complete || tail.Next == "" {
		t.Fatalf("tail = %+v", tail)
	}
	r2 := reply("second reply", "r2")
	after := s.listEvents("--cursor", string(tail.Next), "--limit", "100")
	if !has(after.Items, created(r2.Message)) {
		t.Fatalf("a list from the tail's next lacks the new reply:\n%v", after.Items)
	}
	for _, ev := range after.Items {
		if seen[ev.ID] {
			t.Fatalf("a list from the tail repeated %s %s", ev.Kind, ev.ID)
		}
	}

	// The next is a checkpoint a watch starts at.
	w = s.s.watch("--consumer", "from-list", "--cursor", string(tail.Next), "--format", "json")
	w.until(created(r2.Message))
	if code, stderr := w.interrupt(); code != 0 {
		t.Fatalf("watch from a list's next: exit %d\n%s", code, stderr)
	}

	// Filters.
	byKind := s.listEvents("--kind", model.EventMessageCreated, "--limit", "100")
	if len(byKind.Items) == 0 {
		t.Fatal("--kind message.created lists nothing")
	}
	for _, ev := range byKind.Items {
		if ev.Kind != model.EventMessageCreated {
			t.Fatalf("--kind message.created listed %s", ev.Kind)
		}
	}
	byInbox := s.listEvents("--inbox", "webin", "--limit", "100")
	if len(byInbox.Items) == 0 {
		t.Fatal("--inbox webin lists nothing")
	}
	for _, ev := range byInbox.Items {
		if ev.Inbox != s.conv.Inbox.ID {
			t.Fatalf("--inbox webin listed an event of inbox %s", ev.Inbox)
		}
	}
	wantStderr(t, s.heo(3, "event", "list", "--inbox", "nowhere"), "NOT_FOUND", "nowhere")
	wantStderr(t, s.heo(2, "event", "list", "--kind", "no.such.kind"), "VALIDATION")
	wantStderr(t, s.heo(2, "event", "list", "--cursor", string(tail.Next), "--from", "tail"), "USAGE", "pass one")
	wantStderr(t, s.heo(2, "event", "list", "--from", "middle"), "USAGE")
	wantStderr(t, s.heo(2, "event", "list", "--limit", "0"), "USAGE")

	// An external participant lists public events only; a page of
	// events it may not read is not the end of the list.
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
	s.d = s.start()
	ext, extPages := s.listAllEvents("1")
	if len(ext) == 0 {
		t.Fatal("external lists nothing")
	}
	if has(ext, noteEvent) {
		t.Fatal("an external participant listed the internal note's event")
	}
	empty := 0
	for _, ev := range ext {
		if ev.Audience.Visibility != model.Public {
			t.Fatalf("external listed a %s event %s", ev.Audience.Visibility, ev.Kind)
		}
	}
	for _, p := range extPages {
		if len(p.Items) == 0 && !p.Complete {
			empty++
		}
	}
	if empty == 0 {
		t.Fatal("no empty-but-incomplete page: the internal events were not scanned past")
	}
	if !has(ext, created(r1.Message)) {
		t.Fatal("external does not list the public reply")
	}
	for _, format := range []string{"json", "text"} {
		out := s.heo(0, "event", "list", "--limit", "100", "--format", format)
		if strings.Contains(out.stdout, string(noteID)) {
			t.Fatalf("external %s list shows the internal note's event:\n%s", format, out.stdout)
		}
	}

	// Back to the desk, which lists the note's event.
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", "")
	s.d = s.start()
	if deskAll, _ := s.listAllEvents("100"); !has(deskAll, noteEvent) {
		t.Fatal("the desk does not list the internal note's event")
	}

	// Purged past a cursor: CURSOR_EXPIRED, with the way back; the
	// beginning still lists what is left.
	stale := string(pages[0].Next)
	reply("unread reply", "r3")
	long := 1000 * time.Hour
	purger := s.db.engine(nil, func(cfg *uma.Config) {
		cfg.Retention = uma.RetentionPolicy{Events: time.Nanosecond, Operations: long, Ingress: long, Attempts: long,
			Staging: long, Exports: long, Contacts: long}
	})
	if rep, err := purger.Runtime().Purge(t.Context(), uma.RuntimeCall{
		Principal: model.Principal{Tenant: "default", Actor: "svc:e2e"}, Call: model.Call{Correlation: "e2e"},
	}); err != nil || rep.Events == 0 {
		t.Fatalf("purge = %+v, %v", rep, err)
	}
	wantStderr(t, s.heo(4, "event", "list", "--cursor", stale), "CURSOR_EXPIRED", "purged", "event list --from beginning", "reconcile")
	if left := s.listEvents("--from", "beginning", "--limit", "100"); len(left.Items) == 0 && left.Next == "" {
		t.Fatalf("beginning after the purge = %+v", left)
	}
	s.stop(s.d)
}
