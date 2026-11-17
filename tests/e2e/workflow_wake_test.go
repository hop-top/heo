package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// endedEvents lists the conversation.snooze_ended events with their
// payloads.
func (s *deskSite) endedEvents() []model.ConversationSnoozeEndedPayload {
	s.t.Helper()
	var out []model.ConversationSnoozeEndedPayload
	for _, ev := range s.eventsOf(model.EventConversationSnoozeEnded) {
		var p model.ConversationSnoozeEndedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil || ev.Audience.Visibility != model.Internal {
			s.t.Fatalf("snooze_ended event = %+v (%v)", ev, err)
		}
		out = append(out, p)
	}
	return out
}

// TestSnoozeWakeEmitsEventsFromTheDaemon drives the release binary on
// Postgres through two real widget conversations: the daemon's sweep, on
// the configured cadence, ends a snooze whose time passed and one an
// inbound message ended, once each, and `event list` shows one
// conversation.snooze_ended per wake, narrowed by `--inbox`.
func TestSnoozeWakeEmitsEventsFromTheDaemon(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSiteWith(t, "    held_sweep_every: 1s\n")
		s.restartAs(workflowProfile)
		a := s.conv
		vb := newVisitor(t, s)
		b := vb.conv
		aid, bid := string(a.ID), string(b.ID)
		var out workflowOutJSON

		// By time.
		soon := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
		s.json(&out, "conversation", "snooze", aid, "--until", soon)
		if got := s.endedEvents(); len(got) != 0 {
			t.Fatalf("snooze_ended before the time: %+v", got)
		}
		waitEnded(t, s, 1)
		got := s.endedEvents()
		if got[0].Cause != model.WakeCauseUntil || string(got[0].Conversation.ID) != aid {
			t.Errorf("first wake = %+v", got[0])
		}
		if v := s.view(a.ID); v.Snooze != nil {
			t.Errorf("the sweep left the snooze: %+v", v.Snooze)
		}

		// By a message.
		s.json(&out, "conversation", "snooze", bid, "--for", "24h")
		vb.say(t, s, "anyone there?")
		waitEnded(t, s, 2)
		got = s.endedEvents()
		if got[1].Cause != model.WakeCauseMessage || string(got[1].Conversation.ID) != bid {
			t.Errorf("second wake = %+v", got[1])
		}

		// One event per wake, however long the sweep keeps running.
		time.Sleep(3 * time.Second)
		if n := len(s.endedEvents()); n != 2 {
			t.Errorf("%d snooze_ended events after two wakes", n)
		}
		inbox := string(a.Inbox.ID)
		if n := len(s.eventsOfInbox(inbox, model.EventConversationSnoozeEnded)); n != 2 {
			t.Errorf("--inbox lists %d wakes, want 2", n)
		}
	})
}

// TestSnoozeEndedFilterOnEverySurface: with the sweep off a snooze that
// ran out stays on the record, and `--snooze-ended` lists exactly that
// conversation on the CLI, REST, gRPC and MCP; `=false` lists the
// others; an external actor is refused; no event was written.
func TestSnoozeEndedFilterOnEverySurface(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSiteWith(t, "    held_sweep_every: off\n")
		s.restartAs(workflowProfile)
		a := s.conv
		b := newVisitor(t, s).conv
		aid, bid := string(a.ID), string(b.ID)
		var out workflowOutJSON
		s.json(&out, "conversation", "snooze", aid, "--for", "24h")
		s.json(&out, "conversation", "snooze", bid, "--until", time.Now().Add(2*time.Second).UTC().Format(time.RFC3339Nano))

		// CLI: the snooze still runs for a, and b's has not ended yet.
		if got := s.listed("--snooze-ended"); len(got) != 0 {
			t.Fatalf("--snooze-ended while both run = %v", got)
		}
		deadline := time.Now().Add(15 * time.Second)
		for !slices.Equal(s.listed("--snooze-ended"), refIDs(b)) {
			if time.Now().After(deadline) {
				t.Fatalf("--snooze-ended = %v, want [%s]", s.listed("--snooze-ended"), bid)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if got := s.listed("--snooze-ended=false", "--snoozed", "include"); !slices.Equal(got, refIDs(a)) {
			t.Errorf("--snooze-ended=false = %v, want [%s]", got, aid)
		}
		if got := s.listed("--snooze-ended", "--snoozed", "only"); len(got) != 0 {
			t.Errorf("--snooze-ended --snoozed only = %v: an ended snooze is not in effect", got)
		}
		if got := s.endedEvents(); len(got) != 0 {
			t.Errorf("no sweep ran, yet snooze_ended = %+v", got)
		}
		help := s.heo(0, "conversation", "list", "--help").stdout
		if !strings.Contains(help, "--snooze-ended") {
			t.Errorf("conversation list --help lacks --snooze-ended")
		}

		x, token, _ := serveDesk(t, s)
		for surf, r := range surfaceCall(t, x, token, []string{"conversation", "list"}, nil, map[string]any{"snooze-ended": true}) {
			if got := listedIn(r.Body); r.Code != http.StatusOK || !got[bid] || got[aid] {
				t.Errorf("%s --snooze-ended = %d %s", surf, r.Code, r.Body)
			}
		}
		m := mcpCall(t, x, token, "conversation.list", map[string]any{"snooze-ended": true})
		if got := listedIn(m.Body); m.Code != http.StatusOK || !got[bid] || got[aid] {
			t.Errorf("mcp --snooze-ended = %d %s", m.Code, m.Body)
		}
	})
}

func waitEnded(t *testing.T, s *deskSite, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for len(s.endedEvents()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d snooze_ended events after 30s", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
