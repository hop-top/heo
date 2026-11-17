package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// eventByID finds the listed event satisfying match.
func eventByID(t *testing.T, evs []model.Event, match func(model.Event) bool) model.Event {
	t.Helper()
	for _, ev := range evs {
		if match(ev) {
			return ev
		}
	}
	t.Fatalf("no such event among %d", len(evs))
	return model.Event{}
}

// TestEventGet drives the release binary on Postgres through a real
// widget conversation: an event a list printed is read by its ID with
// the same content, in JSON and in text, and nothing is opened or moved;
// an unknown ID is NOT_FOUND; an external participant reads the public
// reply's event, and the internal note's event answers exactly as an
// event that does not exist (same code, same message), so it never says
// the event is there; a desk actor reads it. REST serves the command,
// as does gRPC.
func TestEventGet(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)
	var r replyJSON
	s.json(&r, "message", "reply", id, "--text", "first reply", "--idempotency-key", "g1")
	s.waitDelivery(r.Operation.ID, model.ProviderAccepted, model.Delivered)
	var note noteJSON
	s.json(&note, "note", "add", id, "--text", "internal secret", "--placement", "local")

	all, _ := s.listAllEvents("100")
	created := func(msg model.ID) func(model.Event) bool {
		return func(ev model.Event) bool { return ev.Kind == model.EventMessageCreated && ev.Resource.ID == msg }
	}
	reply := eventByID(t, all, created(r.Message))
	secret := eventByID(t, all, created(note.Message))
	if secret.Audience.Visibility == model.Public || reply.Audience.Visibility != model.Public {
		t.Fatalf("audiences: reply %q, note %q: the checks below prove nothing", reply.Audience.Visibility, secret.Audience.Visibility)
	}

	// Every listed event is read back, as listed.
	for _, want := range []model.Event{reply, secret, all[0]} {
		var got model.Event
		s.json(&got, "event", "get", string(want.ID))
		if got.ID != want.ID || got.Kind != want.Kind || got.Resource != want.Resource || got.Inbox != want.Inbox ||
			got.Audience != want.Audience || got.Payload.Name != want.Payload.Name || !got.RecordedAt.Equal(want.RecordedAt) {
			t.Fatalf("event get = %+v, listed as %+v", got, want)
		}
	}
	out := s.heo(0, "event", "get", string(secret.ID))
	for _, want := range []string{"id", string(secret.ID), "kind", model.EventMessageCreated, "visibility", string(secret.Audience.Visibility),
		"resource", "message:" + string(note.Message), "payload"} {
		if !strings.Contains(out.stdout, want) {
			t.Errorf("event get lacks %q:\n%s", want, out.stdout)
		}
	}

	// Reading opens no subscription: a tail list still sees the head.
	if tail := s.listEvents("--from", "tail"); len(tail.Items) != 0 || tail.Next == "" {
		t.Fatalf("tail after gets = %+v", tail)
	}

	// An event that does not exist, and a malformed or missing ID.
	missing := s.heo(3, "event", "get", "evt_no_such_event")
	wantStderr(t, missing, "NOT_FOUND", "event evt_no_such_event: not found")
	if r := s.heo(2, "event", "get"); !strings.Contains(r.stderr, "USAGE") && !strings.Contains(r.stderr, "accepts 1 arg") {
		t.Errorf("event get without an ID:\n%s", r.stderr)
	}

	// An external participant: the public event, not the internal one,
	// which answers as one that does not exist.
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
	s.d = s.start()
	var pub model.Event
	s.json(&pub, "event", "get", string(reply.ID))
	if pub.ID != reply.ID {
		t.Fatalf("external reads the public event as %+v", pub)
	}
	hidden := s.heo(3, "event", "get", string(secret.ID))
	wantStderr(t, hidden, "NOT_FOUND", "event "+string(secret.ID)+": not found")
	if strings.Contains(hidden.stdout, string(secret.Resource.ID)) || strings.Contains(hidden.stderr, string(note.Message)) {
		t.Fatalf("a refused read leaks the event's resource:\n%s\n%s", hidden.stdout, hidden.stderr)
	}
	// The same bytes, once the ID is set aside: nothing tells the two apart.
	norm := func(r result, id string) string { return strings.ReplaceAll(r.stderr, id, "ID") }
	if a, b := norm(hidden, string(secret.ID)), norm(missing, "evt_no_such_event"); a != b {
		t.Fatalf("unreadable event answers\n%s\nmissing event answers\n%s", a, b)
	}

	// Back to the desk, which reads the internal note's event.
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", "")
	s.d = s.start()
	var again model.Event
	s.json(&again, "event", "get", string(secret.ID))
	if again.ID != secret.ID {
		t.Fatalf("desk reads the note's event as %+v", again)
	}

	// Served beside the list: REST and gRPC run it, with the ID.
	x, token, _ := serveDesk(t, s)
	defer func() { s.s.want(result{exit: x.d.term()}, 0) }()
	within(t, 10*time.Second, "REST event get", func() {
		code, body := x.rest(t, http.MethodGet, "/v1/commands/event/get?arg="+string(reply.ID), token, "")
		if code != http.StatusOK || !strings.Contains(asString(body), string(reply.ID)) {
			t.Errorf("REST event get = %d %v", code, body)
		}
	})
	within(t, 10*time.Second, "gRPC event get", func() {
		code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"event", "get"}, "args": []string{string(secret.ID)}})
		if code != http.StatusOK || !strings.Contains(asString(body), string(secret.ID)) {
			t.Errorf("rpc event get = %d %v", code, body)
		}
	})
	within(t, 10*time.Second, "REST event get of a missing event", func() {
		if code, body := x.rest(t, http.MethodGet, "/v1/commands/event/get?arg=evt_no_such_event", token, ""); code != http.StatusNotFound {
			t.Errorf("REST event get of a missing event = %d %v, want 404", code, body)
		}
	})
}
