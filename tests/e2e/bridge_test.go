package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// noBridge is the services configuration of a site that counts the
// operations uma holds: it runs without the event bridge, whose
// acknowledgements are operations in that ledger too (the same as an
// `event watch`'s).
const noBridge = "  bridge:\n    enabled: false\n"

// busLine is one event the e2e build's bus recorders received, as the
// daemon of process PID logged it: the scope of the subscriber, the
// topic and the bridge's payload, which names references.
type busLine struct {
	PID     int    `json:"pid"`
	Scope   string `json:"scope"`
	Topic   string `json:"topic"`
	Payload struct {
		Event        string `json:"event"`
		Kind         string `json:"kind"`
		Conversation string `json:"conversation"`
		Message      string `json:"message"`
		Label        string `json:"label"`
		Operation    string `json:"operation"`
		Resource     struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"resource"`
	} `json:"payload"`
	raw string
}

// busLog is every line the file holds.
type busLog []busLine

// busLog reads the recorders' file, appended to by every daemon this
// site started.
func (s *deskSite) busLog() busLog {
	s.t.Helper()
	b, err := os.ReadFile(s.s.path("state", "e2e-bus.jsonl")) //nolint:gosec // a file under the test sandbox
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	var out busLog
	for _, raw := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var l busLine
		if err := json.Unmarshal(raw, &l); err != nil {
			s.t.Fatalf("bus log line: %v\n%s", err, raw)
		}
		l.raw = string(raw)
		out = append(out, l)
	}
	return out
}

// find returns the lines of scope on topic whose field is value; an
// empty field matches any.
func (l busLog) find(scope, topic, field, value string) []busLine {
	var out []busLine
	for _, e := range l {
		if e.Scope != scope || e.Topic != topic {
			continue
		}
		got := map[string]string{"message": e.Payload.Message, "label": e.Payload.Label, "operation": e.Payload.Operation}[field]
		if field == "" || got == value {
			out = append(out, e)
		}
	}
	return out
}

// events returns the IDs of the domain events scope received, by the
// process that received them.
func (l busLog) events(scope string, pid int) map[string]bool {
	out := map[string]bool{}
	for _, e := range l {
		if e.Scope == scope && (pid == 0 || e.PID == pid) && e.Payload.Event != "" {
			out[e.Payload.Event] = true
		}
	}
	return out
}

// await polls the log until cond holds of it.
func (s *deskSite) await(what string, cond func(busLog) bool) busLog {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		l := s.busLog()
		if cond(l) {
			return l
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("the bus never carried %s:\n%v", what, len(l))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// bridgeReady waits until d reports the event bridge ready.
func bridgeReady(t *testing.T, d *daemon) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !d.isReady("bridge") {
		if time.Now().After(deadline) {
			t.Fatalf("the bridge never reported ready:\n%s", d.log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBusBridge drives the e2e build on Postgres through a real widget
// conversation. A reply, an internal note and a label added and removed
// reach the bus as references: the reply on the public topic, the note
// and the label events on the desk topics only, which a public
// subscriber never receives; not a byte of the reply's text, the note's
// text or the label's name is in what either scope received. The
// bridge's own health and the daemon's lifecycle reach the desk scope
// and no other. After a graceful restart nothing the first run
// published is published again; after a kill -9 nothing is lost, only
// repeated: every event uma committed after the bridge first opened its
// subscription reached the bus.
func TestBusBridge(t *testing.T) {
	withE2EBinary(t, func() {
		const (
			body      = "BODY-SECRET-we-are-on-it"
			noteText  = "NOTE-SECRET-internal-only"
			labelName = "labelsecretname"
		)
		s := newDeskSite(t)
		bridgeReady(t, s.d)
		id := string(s.conv.ID)
		pid1 := s.d.cmd.Process.Pid

		var made labelOutJSON
		s.json(&made, "label", "create", labelName)
		var reply replyJSON
		s.json(&reply, "message", "reply", id, "--text", body, "--idempotency-key", "bus-reply")
		var note noteJSON
		s.json(&note, "note", "add", id, "--text", noteText, "--placement", "local")
		s.heo(0, "conversation", "label", id, "--add", labelName)
		s.heo(0, "conversation", "label", id, "--remove", labelName)
		labelID := string(made.Label.Ref.ID)
		s.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered)

		l := s.await("the reply, the note, the label and the delivery", func(l busLog) bool {
			return len(l.find("desk", "uma.public.message.created", "message", string(reply.Message))) > 0 &&
				len(l.find("desk", "uma.desk.message.created", "message", string(note.Message))) > 0 &&
				len(l.find("desk", "uma.desk.conversation.label_added", "label", labelID)) > 0 &&
				len(l.find("desk", "uma.desk.conversation.label_removed", "label", labelID)) > 0 &&
				len(l.find("public", "uma.public.message.created", "message", string(reply.Message))) > 0 &&
				len(l.find("desk", "uma.public.delivery.updated", "operation", string(reply.Operation.ID))) > 0
		})
		got := l.find("desk", "uma.public.message.created", "message", string(reply.Message))
		if len(got) != 1 || got[0].Payload.Conversation != id || got[0].Payload.Resource.Kind != "message" || got[0].Payload.Operation == "" {
			t.Errorf("reply events = %+v", got)
		}
		if n := l.find("desk", "uma.desk.message.created", "message", string(note.Message)); len(n) != 1 || n[0].Payload.Conversation != id {
			t.Errorf("note events = %+v", n)
		}
		for _, topic := range []string{"heo.runtime.bridge.started", "kit.serve.service.started"} {
			if len(l.find("desk", topic, "", "")) == 0 {
				t.Errorf("the desk scope never received %s", topic)
			}
		}

		// Public: uma.public.* only; nothing of the note or the labels,
		// the bridge's health or the daemon's lifecycle.
		for _, e := range l {
			if e.Scope != "public" {
				continue
			}
			if !strings.HasPrefix(e.Topic, "uma.public.") {
				t.Errorf("a public subscriber received %s", e.Topic)
			}
			if e.Payload.Message == string(note.Message) || e.Payload.Label != "" {
				t.Errorf("a public subscriber received the note's or a label's reference: %s", e.raw)
			}
		}
		// Not a byte of any content, to either scope.
		for _, e := range l {
			for _, secret := range []string{body, noteText, labelName, "we are on it"} {
				if strings.Contains(e.raw, secret) {
					t.Errorf("%q is on the bus: %s", secret, e.raw)
				}
			}
		}

		// service status names the bridge, its subscription and the last
		// event it published.
		status, data := s.s.serviceStatus().section(t, "bridge")
		if status != "ok" || data["consumer"] != "heo-bridge" || data["state"] != "ready" || data["last_event"] == nil ||
			data["published"] == nil || data["published"].(float64) < 1 {
			t.Errorf("bridge section = %s %v", status, data)
		}

		// Graceful restart: the batch in hand is acknowledged, so
		// nothing the first process published is published again.
		first := l.events("desk", pid1)
		s.stop(s.d)
		s.d = s.start()
		bridgeReady(t, s.d)
		pid2 := s.d.cmd.Process.Pid
		var reply2 replyJSON
		s.json(&reply2, "message", "reply", id, "--text", "after the restart", "--idempotency-key", "bus-reply-2")
		l = s.await("the reply after the restart", func(l busLog) bool {
			return len(l.find("desk", "uma.public.message.created", "message", string(reply2.Message))) > 0
		})
		for ev := range l.events("desk", pid2) {
			if first[ev] {
				t.Errorf("event %s was published again after a graceful restart", ev)
			}
		}

		// kill -9 right after a commit: nothing is lost; the batch in
		// hand may repeat.
		var reply3 replyJSON
		s.json(&reply3, "message", "reply", id, "--text", "before the kill", "--idempotency-key", "bus-reply-3")
		_ = s.d.cmd.Process.Kill()
		<-s.d.done
		s.d = s.start()
		bridgeReady(t, s.d)
		var reply4 replyJSON
		s.json(&reply4, "message", "reply", id, "--text", "after the kill", "--idempotency-key", "bus-reply-4")
		l = s.await("the replies around the kill", func(l busLog) bool {
			return len(l.find("desk", "uma.public.message.created", "message", string(reply3.Message))) > 0 &&
				len(l.find("desk", "uma.public.message.created", "message", string(reply4.Message))) > 0
		})

		// Every event uma committed from the first run's first event on
		// reached the bus, in some run.
		published := l.events("desk", 0)
		all, _ := s.listAllEvents("100")
		started := false
		var missing []string
		for _, ev := range all {
			if first[string(ev.ID)] {
				started = true
			}
			if started && !published[string(ev.ID)] {
				missing = append(missing, ev.Kind+" "+string(ev.ID))
			}
		}
		if !started {
			t.Fatal("no listed event was bridged")
		}
		// Events committed after the last wait (delivery of the last
		// replies) may still be on their way: allow them a moment.
		if len(missing) > 0 {
			time.Sleep(3 * time.Second)
			published = s.busLog().events("desk", 0)
			missing = missing[:0]
			started = false
			for _, ev := range all {
				if first[string(ev.ID)] {
					started = true
				}
				if started && !published[string(ev.ID)] {
					missing = append(missing, ev.Kind+" "+string(ev.ID))
				}
			}
		}
		if len(missing) > 0 {
			t.Errorf("committed events the bus never carried: %v", missing)
		}
	})
}
