package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// operationCount is how many operations uma holds: a provider receipt
// would be one.
func (s *deskSite) operationCount() int {
	s.t.Helper()
	page, err := s.db.operator("default").Operations().List(s.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 500},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return len(page.Items)
}

// TestWorkflowLinksAndReadMarkers drives the release binary on Postgres
// through two real widget conversations: a link previews both ends,
// shows from both, merges no history, is idempotent and conflicts on a
// stale revision; read markers are the caller's own, idempotent, never an
// event or an operation, and `--unread` follows them.
func TestWorkflowLinksAndReadMarkers(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSiteWith(t, noBridge)
		s.restartAs(workflowProfile)
		a, va := s.conv, &visitor{token: s.token, conv: s.conv}
		vb := newVisitor(t, s)
		b := vb.conv
		aid, bid := string(a.ID), string(b.ID)

		// Links: bad arguments, then the preview of both ends.
		wantExit(t, s.heo(2, "conversation", "link", aid), 2, "USAGE")
		wantExit(t, s.heo(2, "conversation", "link", aid, "--conversation", bid, "--work-item", "wi_1"), 2, "USAGE")
		wantExit(t, s.heo(2, "conversation", "link", aid, "--conversation", aid), 2, "VALIDATION", "itself")
		wantExit(t, s.heo(3, "conversation", "link", aid, "--conversation", "conv_404"), 3, "NOT_FOUND")
		wantExit(t, s.heo(3, "conversation", "link", aid, "--work-item", "wi_404"), 3, "NOT_FOUND")
		a0, b0 := s.view(a.ID), s.view(b.ID)
		msgsA := len(s.messages("--conversation", aid))
		var plan planJSON
		s.json(&plan, "conversation", "link", aid, "--conversation", bid, "--dry-run")
		if len(plan.Effects) != 2 || plan.Effects[0].Target != "conversation:"+aid || plan.Effects[1].Target != "conversation:"+bid ||
			!strings.Contains(string(plan.Args["far"]), bid) || !strings.Contains(plan.Effects[0].Detail, "no history merges") {
			t.Fatalf("link preview = %+v", plan)
		}
		if got := s.view(a.ID); got.Revision != a0.Revision || len(s.eventsOf(workflowKinds...)) != 0 {
			t.Fatalf("--dry-run wrote: %+v", got)
		}

		// Link: both ends hold it and moved; a replay; again is a no-op.
		var out workflowOutJSON
		s.json(&out, "conversation", "link", aid, "--conversation", bid, "--idempotency-key", "ln-1")
		if !out.Changed || len(out.Conversation.Links) != 1 || out.Conversation.Links[0].Conversation == nil ||
			out.Conversation.Links[0].Conversation.ID != b.ID || out.Operation.State != model.Committed {
			t.Fatalf("link = %+v", out)
		}
		if got := s.view(b.ID); len(got.Links) != 1 || got.Links[0].Conversation.ID != a.ID || got.Revision == b0.Revision {
			t.Fatalf("the far end = %+v", got)
		}
		if text := s.heo(0, "conversation", "get", bid).stdout; !strings.Contains(text, "conversation "+aid) {
			t.Errorf("conversation get does not show the link:\n%s", text)
		}
		s.json(&out, "conversation", "link", bid, "--conversation", aid)
		if out.Changed || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "already linked") {
			t.Fatalf("linking again, from the other end = %+v", out)
		}
		wantExit(t, s.heo(4, "conversation", "link", aid, "--conversation", bid, "--revision", string(a0.Revision)), 4, "CONFLICT")
		// No history merge, no reply-scope change.
		if got := len(s.messages("--conversation", aid)); got != msgsA || len(s.messages("--conversation", bid)) != 1 {
			t.Errorf("linking moved messages: %d in a, was %d", got, msgsA)
		}
		if got := s.view(a.ID); got.ReplyRoute != a0.ReplyRoute || got.Status != a0.Status {
			t.Errorf("linking changed the reply route or status: %+v", got)
		}
		if got := s.listed("--linked"); !slices.Equal(got, refIDs(a, b)) {
			t.Errorf("--linked = %v", got)
		}
		linked := s.eventsOf(model.EventConversationLinked)
		if len(linked) != 2 {
			t.Fatalf("linked events = %d, want one per end", len(linked))
		}
		for _, ev := range linked {
			if ev.Audience.Visibility != model.Internal {
				t.Errorf("linked event audience = %+v", ev.Audience)
			}
		}
		s.json(&out, "conversation", "unlink", aid, "--conversation", bid)
		if !out.Changed || len(out.Conversation.Links) != 0 || len(s.view(b.ID).Links) != 0 {
			t.Fatalf("unlink = %+v", out)
		}
		s.json(&out, "conversation", "unlink", aid, "--conversation", bid)
		if out.Changed || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "not linked") {
			t.Fatalf("unlinking again = %+v", out)
		}
		if n := len(s.eventsOf(model.EventConversationUnlinked)); n != 2 {
			t.Errorf("unlinked events = %d, want 2", n)
		}

		// Read markers: nothing is read yet.
		if got := s.listed("--unread"); !slices.Equal(got, refIDs(a, b)) {
			t.Fatalf("--unread before any mark = %v", got)
		}
		first := s.messages("--conversation", aid)[0].Ref.ID
		bFirst := s.messages("--conversation", bid)[0].Ref.ID
		events, ops := len(s.eventsOf(workflowKinds...)), s.operationCount()
		revA := s.view(a.ID).Revision
		s.json(&plan, "conversation", "mark-read", aid, "--dry-run")
		if len(plan.Effects) != 1 || !strings.Contains(string(plan.Args["current"]), "never marked") || !strings.Contains(string(plan.Args["new"]), "now") ||
			!strings.Contains(plan.Effects[0].Detail, "no receipt") {
			t.Fatalf("mark-read preview = %+v", plan)
		}
		if v := s.view(a.ID); v.Read == nil || v.Read.Marked {
			t.Fatalf("--dry-run wrote a marker: %+v", v.Read)
		}
		var mark readMarkOutJSON
		s.json(&mark, "conversation", "mark-read", aid)
		if mark.Conversation != a.ID || string(mark.Actor) != me() || mark.ReadThrough.IsZero() || !mark.Changed {
			t.Fatalf("mark-read = %+v", mark)
		}
		if got := s.listed("--unread"); !slices.Equal(got, refIDs(b)) {
			t.Fatalf("--unread after marking a = %v", got)
		}
		if v := s.view(a.ID); v.Read == nil || !v.Read.Marked || v.Revision != revA {
			t.Fatalf("get after marking: read %+v, revision %s (was %s)", v.Read, v.Revision, revA)
		}
		if text := s.heo(0, "conversation", "get", aid).stdout; !strings.Contains(text, "read") || strings.Contains(text, "never marked") {
			t.Errorf("conversation get does not show the marker:\n%s", text)
		}
		// A new inbound message makes it unread again; reading through the
		// first one leaves the second unread; the same mark twice is a no-op.
		time.Sleep(10 * time.Millisecond)
		va.say(t, s, "second")
		if got := waitListed(t, s, []string{"--unread"}, refIDs(a, b)); got != nil {
			t.Fatalf("--unread after a new message = %v", got)
		}
		s.json(&mark, "conversation", "mark-read", aid, "--through", string(first))
		if !mark.Changed {
			t.Fatalf("mark-read --through = %+v", mark)
		}
		through := mark.ReadThrough
		s.json(&mark, "conversation", "mark-read", aid, "--through", string(first))
		if mark.Changed || !mark.ReadThrough.Equal(through) || len(mark.Notes) != 1 {
			t.Fatalf("the same mark twice = %+v", mark)
		}
		if got := s.listed("--unread"); !slices.Equal(got, refIDs(a, b)) {
			t.Fatalf("--unread after reading through the first message = %v", got)
		}
		s.json(&mark, "conversation", "mark-read", aid)
		s.json(&mark, "conversation", "mark-read", bid)
		if got := s.listed("--unread"); got != nil {
			t.Fatalf("--unread after reading both = %v", got)
		}
		s.json(&mark, "conversation", "mark-unread", bid, "--from", string(bFirst))
		if got := s.listed("--unread"); !slices.Equal(got, refIDs(b)) {
			t.Fatalf("--unread after mark-unread --from = %v", got)
		}
		s.json(&mark, "conversation", "mark-unread", aid)
		if got := s.listed("--unread"); !slices.Equal(got, refIDs(a, b)) {
			t.Fatalf("--unread after mark-unread = %v", got)
		}
		wantExit(t, s.heo(3, "conversation", "mark-read", aid, "--through", string(bFirst)), 3, "NOT_FOUND")
		wantExit(t, s.heo(3, "conversation", "mark-unread", aid, "--from", "msg_404"), 3, "NOT_FOUND")
		wantExit(t, s.heo(3, "conversation", "mark-read", "conv_404"), 3, "NOT_FOUND")

		// A marker is local and the caller's: no event, no operation (no
		// receipt), no revision.
		if got := len(s.eventsOf(workflowKinds...)); got != events {
			t.Errorf("marking emitted %d workflow events", got-events)
		}
		if got := s.operationCount(); got != ops {
			t.Errorf("marking queued %d operations", got-ops)
		}
		if v := s.view(a.ID); v.Revision != revA {
			t.Errorf("marking moved the conversation's revision")
		}
	})
}

// waitListed polls `conversation list` until it lists want, for the
// moment an inbound message needs to be recorded; it returns nil then,
// else the last listing.
func waitListed(t *testing.T, s *deskSite, args []string, want []model.ID) []model.ID {
	t.Helper()
	var got []model.ID
	for range 100 {
		if got = s.listed(args...); slices.Equal(got, want) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return got
}
