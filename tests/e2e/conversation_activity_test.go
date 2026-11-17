package e2e

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

// activityItemJSON is a conversation of `conversation list --format json`
// with its last activity.
type activityItemJSON struct {
	Ref          model.ConversationRef
	LastActivity time.Time `json:"last_activity"`
}

// byActivity lists the conversation IDs `conversation list --sort
// last-activity` shows, in order, and their last activity.
func (s *deskSite) byActivity(args ...string) ([]string, map[string]time.Time) {
	s.t.Helper()
	var p pageJSON[activityItemJSON]
	s.json(&p, append([]string{"conversation", "list", "--sort", "last-activity", "--snoozed", "include"}, args...)...)
	var ids []string
	at := map[string]time.Time{}
	for _, c := range p.Items {
		ids = append(ids, string(c.Ref.ID))
		at[string(c.Ref.ID)] = c.LastActivity
	}
	return ids, at
}

// orderOf is the conversation IDs of a surface's body, in order.
func orderOf(body string) []string {
	var out []string
	for _, m := range refRE.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestConversationLastActivitySortAndSince drives the release binary on
// Postgres: the list sorted by last activity reorders when a message
// arrives, a read marker moves and a snooze starts, and --updated-since
// keeps only what changed after it, on the CLI, REST, gRPC and MCP.
func TestConversationLastActivitySortAndSince(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSite(t)
		bridgeReady(t, s.d)
		a, va := s.conv, &visitor{token: s.token, conv: s.conv}
		b := newVisitor(t, s).conv
		aid, bid := string(a.ID), string(b.ID)
		pause := func() { time.Sleep(20 * time.Millisecond) }

		// b started last: it is the newest change.
		if got, at := s.byActivity(); !slices.Equal(got, []string{bid, aid}) || at[aid].IsZero() {
			t.Fatalf("initial order = %v %v, want [%s %s]", got, at, bid, aid)
		}
		// A message arrives in a.
		pause()
		va.say(t, s, "anyone there?")
		s.await("the message of a", func(busLog) bool {
			got, _ := s.byActivity()
			return slices.Equal(got, []string{aid, bid})
		})
		// A read marker moves in b.
		pause()
		var mark readMarkOutJSON
		s.json(&mark, "conversation", "mark-read", bid)
		if got, _ := s.byActivity(); !slices.Equal(got, []string{bid, aid}) {
			t.Fatalf("after mark-read of b = %v, want [%s %s]", got, bid, aid)
		}
		_, at := s.byActivity()
		changedAfter := at[bid]
		// A snooze starts on a.
		pause()
		var out workflowOutJSON
		s.json(&out, "conversation", "snooze", aid, "--for", "1h")
		if got, _ := s.byActivity(); !slices.Equal(got, []string{aid, bid}) {
			t.Fatalf("after snooze of a = %v, want [%s %s]", got, aid, bid)
		}
		// Only what changed after the point; the unfiltered rest is untouched.
		since := changedAfter.Format(time.RFC3339Nano)
		if got, _ := s.byActivity("--updated-since", since); !slices.Equal(got, []string{aid}) {
			t.Errorf("--updated-since %s = %v, want [%s]", since, got, aid)
		}
		if got, _ := s.byActivity("--updated-since", at[aid].Add(time.Hour).Format(time.RFC3339Nano)); len(got) != 0 {
			t.Errorf("--updated-since in the future = %v, want none", got)
		}
		// Pages continue where the last one ended.
		var p1 pageJSON[activityItemJSON]
		var raw struct{ Next string }
		s.json(&raw, "conversation", "list", "--sort", "last-activity", "--snoozed", "include", "--limit", "1")
		s.json(&p1, "conversation", "list", "--sort", "last-activity", "--snoozed", "include", "--limit", "1", "--cursor", raw.Next)
		if raw.Next == "" || len(p1.Items) != 1 || string(p1.Items[0].Ref.ID) != bid {
			t.Errorf("second page = %+v after %q, want [%s]", p1, raw.Next, bid)
		}

		x, token, _ := serveDesk(t, s)
		flags := map[string]any{"sort": "last-activity", "snoozed": "include"}
		since2 := map[string]any{"sort": "last-activity", "snoozed": "include", "updated-since": since}
		list := []string{"conversation", "list"}
		for surf, r := range surfaceCall(t, x, token, list, nil, flags) {
			if got := orderOf(r.Body); r.Code != http.StatusOK || !slices.Equal(got, []string{aid, bid}) {
				t.Errorf("%s sorted list = %d %v, want [%s %s]\n%s", surf, r.Code, got, aid, bid, r.Body)
			}
		}
		for surf, r := range surfaceCall(t, x, token, list, nil, since2) {
			if got := orderOf(r.Body); r.Code != http.StatusOK || !slices.Equal(got, []string{aid}) {
				t.Errorf("%s --updated-since list = %d %v, want [%s]\n%s", surf, r.Code, got, aid, r.Body)
			}
		}
		if r := mcpCall(t, x, token, "conversation.list", flags); r.Code != http.StatusOK || !slices.Equal(orderOf(r.Body), []string{aid, bid}) {
			t.Errorf("mcp sorted list = %d %v\n%s", r.Code, orderOf(r.Body), r.Body)
		}
		if r := mcpCall(t, x, token, "conversation.list", since2); r.Code != http.StatusOK || !slices.Equal(orderOf(r.Body), []string{aid}) {
			t.Errorf("mcp --updated-since list = %d %v\n%s", r.Code, orderOf(r.Body), r.Body)
		}
		// A read marker moved over REST reorders the list there too.
		pause()
		for surf, r := range surfaceCall(t, x, token, []string{"conversation", "mark-read"}, []string{bid}, nil) {
			if r.Code != http.StatusOK {
				t.Fatalf("%s mark-read = %d %s", surf, r.Code, r.Body)
			}
		}
		if r := mcpCall(t, x, token, "conversation.list", flags); !slices.Equal(orderOf(r.Body), []string{bid, aid}) {
			t.Errorf("mcp list after a marker moved = %v, want [%s %s]\n%s", orderOf(r.Body), bid, aid, r.Body)
		}
		// A bad sort or time is refused.
		for surf, r := range surfaceCall(t, x, token, list, nil, map[string]any{"sort": "name"}) {
			if r.Code == http.StatusOK {
				t.Errorf("%s --sort name = %d %s", surf, r.Code, r.Body)
			}
		}
		for surf, r := range surfaceCall(t, x, token, list, nil, map[string]any{"updated-since": "yesterday"}) {
			if r.Code == http.StatusOK {
				t.Errorf("%s --updated-since yesterday = %d %s", surf, r.Code, r.Body)
			}
		}
	})
}
