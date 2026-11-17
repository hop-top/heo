package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// deskPeer is a second desk actor of the workflow tests: it holds the
// desk role through a configured grant, and is what an assignee other
// than the test's own user is.
const deskPeer = "uid:4243"

const workflowProfile = "authorization:\n  grants:\n    - actor: \"" + deskPeer + "\"\n      roles: [desk]\n"

type workflowOutJSON struct {
	Conversation model.Conversation
	Changed      bool
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

type readMarkOutJSON struct {
	Conversation model.ID
	Actor        model.ActorID
	ReadThrough  time.Time `json:"read_through"`
	UpdatedAt    time.Time `json:"updated_at"`
	Revision     string
	Changed      bool
	Notes        []string
}

type conversationViewJSON struct {
	model.Conversation
	Workflow bool
	Read     *struct {
		Marked      bool
		ReadThrough time.Time `json:"read_through"`
		UpdatedAt   time.Time `json:"updated_at"`
	}
}

// visitor is a widget visitor with a conversation of its own.
type visitor struct {
	token string
	conv  model.ConversationRef
	n     int
}

// newVisitor has a new visitor on site-1 open a conversation, and
// returns it with the session that sends it more.
func newVisitor(t *testing.T, s *deskSite) *visitor {
	t.Helper()
	list := func() map[model.ConversationRef]bool {
		page, err := s.db.operator("default").Conversations().List(t.Context(), model.ListRequest{
			Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100},
		})
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.ConversationRef]bool{}
		for _, c := range page.Items {
			out[c.Ref] = true
		}
		return out
	}
	before := list()
	code, body := postJSON(t, s.route+"/session?"+composition.WidgetSiteParam+"=site-1", "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("session = %d %s", code, body)
	}
	v := &visitor{token: sess.Token}
	v.say(t, s, "hello")
	for ref := range list() {
		if !before[ref] {
			v.conv = ref
			return v
		}
	}
	t.Fatal("the visitor opened no conversation")
	return nil
}

// say has the visitor send text: a live inbound message.
func (v *visitor) say(t *testing.T, s *deskSite, text string) {
	t.Helper()
	v.n++
	body := fmt.Sprintf(`{"id":"v-%d","text":%q}`, v.n, text)
	if code, out := postJSON(t, s.route+"/messages", body, "Authorization", "Bearer "+v.token); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d %s", code, out)
	}
}

func (s *deskSite) listed(args ...string) []model.ID {
	s.t.Helper()
	var p pageJSON[model.Conversation]
	s.json(&p, append([]string{"conversation", "list"}, args...)...)
	var out []model.ID
	for _, c := range p.Items {
		out = append(out, c.Ref.ID)
	}
	slices.Sort(out)
	return out
}

func (s *deskSite) view(id model.ID) conversationViewJSON {
	s.t.Helper()
	var v conversationViewJSON
	s.json(&v, "conversation", "get", string(id))
	return v
}

// eventsOf lists the events of kinds.
func (s *deskSite) eventsOf(kinds ...string) []model.Event {
	s.t.Helper()
	all, _ := s.listAllEvents("100", "--kind", strings.Join(kinds, ","))
	return all
}

// restartAs runs the daemon again under profile.
func (s *deskSite) restartAs(profile string) {
	s.t.Helper()
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", profile)
	s.d = s.start()
}

func wantExit(t *testing.T, r result, code int, stderr ...string) {
	t.Helper()
	if r.exit != code {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", r.exit, code, r.stdout, r.stderr)
	}
	for _, w := range stderr {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("stderr lacks %q:\n%s", w, r.stderr)
		}
	}
}

var workflowKinds = []string{
	model.EventConversationAssigned, model.EventConversationUnassigned, model.EventConversationSnoozed,
	model.EventConversationUnsnoozed, model.EventConversationLinked, model.EventConversationUnlinked,
}

// TestWorkflowAssignAndSnooze drives the release binary on Postgres
// through two real widget conversations: assign resolves `self` as the
// caller, previews the current assignee, conflicts on a stale revision
// naming it, refuses an assignee that is not a desk actor and records
// every change in the log; snooze resolves `--for` by the daemon's
// clock, hides the conversation from the default list until its time or
// an inbound message, holds across a restart, and wakes without an
// event; the list's `--snoozed` modes and its help say so.
func TestWorkflowAssignAndSnooze(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSite(t)
		s.restartAs(workflowProfile)
		a, vb := s.conv, newVisitor(t, s)
		va := &visitor{token: s.token, conv: s.conv}
		b := vb.conv
		aid := string(a.ID)
		self := me()

		// Assign: a preview shows the change and writes nothing.
		start := s.view(a.ID)
		if !start.Workflow || start.Assignee != nil || start.Snooze != nil || len(start.Links) != 0 || start.Read == nil || start.Read.Marked {
			t.Fatalf("a fresh conversation = %+v", start)
		}
		r := s.heo(0, "conversation", "get", aid)
		for _, want := range []string{"assignee", "snoozed", "links", "read"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("conversation get lacks %q:\n%s", want, r.stdout)
			}
		}
		var plan planJSON
		s.json(&plan, "conversation", "assign", aid, "--to", "self", "--dry-run")
		if len(plan.Effects) != 1 || plan.Effects[0].Target != "conversation:"+aid ||
			!strings.Contains(plan.Effects[0].Detail, "unassigned -> assigned to "+self) || string(plan.Args["assignee"]) != `"`+self+`"` {
			t.Fatalf("assign preview = %+v", plan)
		}
		if v := s.view(a.ID); v.Revision != start.Revision || len(s.eventsOf(workflowKinds...)) != 0 {
			t.Fatalf("--dry-run wrote: %+v", v)
		}
		wantExit(t, s.heo(2, "conversation", "assign", aid), 2, "USAGE")
		wantExit(t, s.heo(5, "conversation", "assign", aid, "--to", "uid:9999", "--dry-run"), 5, "UNAUTHORIZED", "not eligible")
		wantExit(t, s.heo(5, "conversation", "assign", aid, "--to", "uid:9999"), 5, "UNAUTHORIZED", "not eligible")
		wantExit(t, s.heo(3, "conversation", "assign", "conv_404", "--to", "self"), 3, "NOT_FOUND")
		if v := s.view(a.ID); v.Assignee != nil || v.Revision != start.Revision {
			t.Fatalf("a refused assignment changed the conversation: %+v", v)
		}

		// `self` is the authenticated caller; a replay under its key.
		var out, again workflowOutJSON
		s.json(&out, "conversation", "assign", aid, "--to", "self", "--idempotency-key", "as-1")
		if out.Conversation.Assignee == nil || string(*out.Conversation.Assignee) != self || !out.Changed ||
			out.Operation.State != model.Committed || out.OperationKey != "as-1" || out.Conversation.Revision == start.Revision {
			t.Fatalf("assign self = %+v", out)
		}
		s.json(&again, "conversation", "assign", aid, "--to", "self", "--idempotency-key", "as-1")
		if !again.Operation.Replayed || again.Conversation.Revision != out.Conversation.Revision {
			t.Fatalf("replayed assign = %+v", again)
		}
		s.json(&again, "conversation", "assign", aid, "--to", self)
		if again.Changed || len(again.Notes) != 1 || !strings.Contains(again.Notes[0], "already assigned") {
			t.Fatalf("assigning the assignee = %+v", again)
		}
		// The preview of a reassignment shows the current assignee; a
		// stale revision conflicts and names it.
		s.json(&plan, "conversation", "assign", aid, "--to", deskPeer, "--dry-run")
		if !strings.Contains(plan.Effects[0].Detail, "assigned to "+self+" -> assigned to "+deskPeer) {
			t.Fatalf("reassign preview = %+v", plan.Effects)
		}
		wantExit(t, s.heo(4, "conversation", "assign", aid, "--to", deskPeer, "--revision", string(start.Revision)),
			4, "CONFLICT", "assigned to "+self, string(out.Conversation.Revision))
		wantExit(t, s.heo(4, "conversation", "assign", aid, "--to", deskPeer, "--revision", string(start.Revision), "--dry-run"),
			4, "CONFLICT", "assigned to "+self)
		s.json(&out, "conversation", "assign", aid, "--to", deskPeer, "--revision", string(out.Conversation.Revision))
		if out.Conversation.Assignee == nil || string(*out.Conversation.Assignee) != deskPeer {
			t.Fatalf("reassign = %+v", out)
		}
		if text := s.heo(0, "conversation", "get", aid).stdout; !strings.Contains(text, deskPeer) {
			t.Errorf("conversation get does not show the assignee:\n%s", text)
		}
		s.json(&out, "conversation", "unassign", aid)
		if out.Conversation.Assignee != nil || !out.Changed {
			t.Fatalf("unassign = %+v", out)
		}
		s.json(&out, "conversation", "unassign", aid)
		if out.Changed || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "not assigned") {
			t.Fatalf("unassigning twice = %+v", out)
		}
		// The log keeps who held it.
		var assigned []model.ConversationAssignmentPayload
		for _, ev := range s.eventsOf(model.EventConversationAssigned) {
			var p model.ConversationAssignmentPayload
			if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
				t.Fatal(err)
			}
			if ev.Audience.Visibility != model.Internal {
				t.Errorf("assigned event audience = %+v", ev.Audience)
			}
			assigned = append(assigned, p)
		}
		if len(assigned) != 2 || assigned[0].Assignee == nil || string(*assigned[0].Assignee) != self || assigned[0].Previous != nil ||
			string(*assigned[1].Assignee) != deskPeer || assigned[1].Previous == nil || string(*assigned[1].Previous) != self {
			t.Fatalf("assigned events = %+v", assigned)
		}
		if cleared := s.eventsOf(model.EventConversationUnassigned); len(cleared) != 1 || string(cleared[0].Actor) != self {
			t.Fatalf("unassigned events = %+v", cleared)
		}

		// Assignee filter.
		s.json(&out, "conversation", "assign", aid, "--to", "self")
		if got := s.listed("--assignee", "self"); !slices.Equal(got, refIDs(a)) {
			t.Errorf("--assignee self = %v", got)
		}
		if got := s.listed("--assignee", deskPeer); got != nil {
			t.Errorf("--assignee %s = %v", deskPeer, got)
		}
		if got := s.listed("--assignee", "none"); !slices.Equal(got, refIDs(b)) {
			t.Errorf("--assignee none = %v", got)
		}
		if got := s.listed("--assignee", "none", "--inbox", "webin", "--status", "open"); !slices.Equal(got, refIDs(b)) {
			t.Errorf("--assignee none --inbox --status = %v", got)
		}
		wantExit(t, s.heo(2, "conversation", "list", "--assignee", "bad\x07actor"), 2, "VALIDATION")

		// Snooze: invalid arguments, then the preview.
		wantExit(t, s.heo(2, "conversation", "snooze", aid), 2, "USAGE")
		wantExit(t, s.heo(2, "conversation", "snooze", aid, "--for", "1h", "--until", "2999-01-01"), 2, "USAGE")
		wantExit(t, s.heo(2, "conversation", "snooze", aid, "--until", "2020-01-01"), 2, "VALIDATION", "future")
		wantExit(t, s.heo(2, "conversation", "snooze", aid, "--for", "soon"), 2, "USAGE")
		wantExit(t, s.heo(2, "conversation", "snooze", aid, "--for", "-1h"), 2)
		before := time.Now()
		s.json(&plan, "conversation", "snooze", aid, "--for", "90m", "--dry-run")
		var until time.Time
		if err := json.Unmarshal(plan.Args["until"], &until); err != nil || until.Before(before.Add(89*time.Minute)) || until.After(time.Now().Add(91*time.Minute)) {
			t.Fatalf("--for 90m resolved to %s (%v)", plan.Args["until"], err)
		}
		if !strings.Contains(string(plan.Args["wake"]), "inbound message") || !strings.Contains(plan.Effects[0].Detail, "not snoozed -> snoozed until") {
			t.Fatalf("snooze preview = %+v", plan)
		}
		if len(s.listed()) != 2 {
			t.Fatal("a preview hid a conversation")
		}

		// Snooze for an hour: hidden by default, found by the modes.
		s.json(&out, "conversation", "snooze", aid, "--for", "1h", "--idempotency-key", "sn-1")
		if out.Conversation.Snooze == nil || !out.Changed || string(out.Conversation.Snooze.By) != self ||
			!out.Conversation.Snooze.Until.After(time.Now().Add(55*time.Minute)) {
			t.Fatalf("snooze = %+v", out)
		}
		for _, tc := range []struct {
			args []string
			want []model.ID
		}{
			{nil, refIDs(b)},
			{[]string{"--snoozed", "exclude"}, refIDs(b)},
			{[]string{"--snoozed", "include"}, refIDs(a, b)},
			{[]string{"--snoozed", "only"}, refIDs(a)},
			{[]string{"--snoozed", "only", "--assignee", "self"}, refIDs(a)},
			{[]string{"--snoozed", "include", "--assignee", "self"}, refIDs(a)},
			{[]string{"--assignee", "self"}, nil},
		} {
			if got := s.listed(tc.args...); !slices.Equal(got, tc.want) {
				t.Errorf("conversation list %v = %v, want %v", tc.args, got, tc.want)
			}
		}
		wantExit(t, s.heo(2, "conversation", "list", "--snoozed", "sometimes"), 2, "VALIDATION")
		help := s.heo(0, "conversation", "list", "--help").stdout
		for _, want := range []string{"--snoozed", "default exclude", "left out by default", "--unread", "--assignee", "--linked"} {
			if !strings.Contains(help, want) {
				t.Errorf("conversation list --help lacks %q", want)
			}
		}
		if v := s.view(a.ID); v.Snooze == nil || !strings.Contains(s.heo(0, "conversation", "get", aid).stdout, "until") {
			t.Fatalf("a snoozed conversation is not found by ID with its snooze: %+v", v.Snooze)
		}

		// The snooze holds across a restart; an inbound message wakes it
		// before its time, with no event.
		s.restartAs(workflowProfile)
		if got := s.listed(); !slices.Equal(got, refIDs(b)) {
			t.Fatalf("after a restart the default list = %v", got)
		}
		events := len(s.eventsOf(workflowKinds...))
		va.say(t, s, "anyone there?")
		deadline := time.Now().Add(15 * time.Second)
		for !slices.Equal(s.listed(), refIDs(a, b)) {
			if time.Now().After(deadline) {
				t.Fatalf("an inbound message did not bring the conversation back: %v", s.listed())
			}
			time.Sleep(100 * time.Millisecond)
		}
		if v := s.view(a.ID); v.Snooze != nil {
			t.Errorf("a woken conversation shows its snooze: %+v", v.Snooze)
		}
		if got := len(s.eventsOf(workflowKinds...)); got != events {
			t.Errorf("waking emitted %d events", got-events)
		}

		// Expiry, after a restart: nothing was scheduled, nothing runs.
		soon := time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339Nano)
		s.json(&out, "conversation", "snooze", aid, "--until", soon)
		if !slices.Equal(s.listed(), refIDs(b)) {
			t.Fatal("the snooze did not hide the conversation")
		}
		events = len(s.eventsOf(workflowKinds...))
		s.stop(s.d)
		time.Sleep(6 * time.Second)
		s.d = s.start()
		if got := s.listed(); !slices.Equal(got, refIDs(a, b)) {
			t.Fatalf("an expired snooze, after a restart, still hides the conversation: %v", got)
		}
		if got := len(s.eventsOf(workflowKinds...)); got != events {
			t.Errorf("expiry emitted %d events", got-events)
		}

		// Early unsnooze, idempotent; a snoozed conversation replaced.
		s.json(&out, "conversation", "snooze", aid, "--for", "1h")
		s.json(&plan, "conversation", "unsnooze", aid, "--dry-run")
		if !strings.Contains(plan.Effects[0].Detail, "ends the snooze now") || !strings.Contains(string(plan.Args["current"]), "snoozed until") {
			t.Fatalf("unsnooze preview = %+v", plan)
		}
		s.json(&out, "conversation", "unsnooze", aid)
		if out.Conversation.Snooze != nil || !out.Changed || !slices.Equal(s.listed(), refIDs(a, b)) {
			t.Fatalf("unsnooze = %+v", out)
		}
		s.json(&out, "conversation", "unsnooze", aid)
		if out.Changed || len(out.Notes) != 1 || !strings.Contains(out.Notes[0], "not snoozed") {
			t.Fatalf("unsnoozing twice = %+v", out)
		}
		wantExit(t, s.heo(4, "conversation", "snooze", aid, "--for", "1h", "--revision", string(start.Revision)), 4, "CONFLICT", "assigned to "+self)
	})
}
