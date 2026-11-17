package e2e

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const (
	extActor  = "uid:4244"
	viewActor = "uid:4245"
)

const workflowSurfacesProfile = "authorization:\n  grants:\n" +
	"    - actor: \"" + deskPeer + "\"\n      roles: [desk]\n" +
	"    - actor: \"" + extActor + "\"\n      roles: [external]\n" +
	"    - actor: \"" + viewActor + "\"\n      roles: [viewer]\n"

var refRE = regexp.MustCompile(`"ref":\{"id":"([^"]+)"`)

// listedIn is the IDs of the conversations a list's body holds: not the
// ones its links name.
func listedIn(body string) map[string]bool {
	out := map[string]bool{}
	for _, m := range refRE.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

// surfaceResult is what one surface answered: an HTTP-like status (a
// refusal is 403, whichever way the surface reports it) and the body.
type surfaceResult struct {
	Code int
	Body string
}

// surfaceCall runs one command on REST and gRPC, as the bearer of
// token. The MCP surface passes no positional argument to a heo command
// (a leaf there takes flags only), so mcpCall serves the leaves that take
// none.
func surfaceCall(t *testing.T, x *surfaces, token string, path []string, args []string, flags map[string]any) map[string]surfaceResult {
	t.Helper()
	out := map[string]surfaceResult{}
	method, q, body := http.MethodGet, url.Values{}, ""
	for _, a := range args {
		q.Set("arg", a)
	}
	if write := path[1] != "get" && path[1] != "list"; write {
		method, body = http.MethodPost, asString(map[string]any{"args": args, "flags": flags})
	} else {
		for k, v := range flags {
			for _, one := range strings.Split(strings.Trim(asString(v), `"`), ",") {
				q.Add(k, one)
			}
		}
	}
	route := "/v1/commands/" + strings.Join(path, "/")
	if len(q) > 0 {
		route += "?" + q.Encode()
	}
	code, raw := x.rest(t, method, route, token, body)
	out["rest"] = surfaceResult{code, asString(raw)}
	code, raw = x.invokeRPC(t, token, map[string]any{"path": path, "args": args, "flags": flags})
	res := surfaceResult{code, asString(raw)}
	if ec, _ := raw["exit_code"].(float64); ec != 0 {
		res.Code = http.StatusForbidden
		if ec != 5 {
			res.Code = http.StatusInternalServerError
		}
	}
	if data, ok := raw["data"]; ok {
		res.Body = asString(data)
	}
	out["rpc"] = res
	return out
}

// mcpCall runs a flags-only leaf on MCP.
func mcpCall(t *testing.T, x *surfaces, token string, tool string, flags map[string]any) surfaceResult {
	t.Helper()
	m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
	reply := m.request(t, "tools/call", map[string]any{"name": tool, "arguments": flags})
	code := http.StatusOK
	if reply["isError"] == true {
		code = http.StatusForbidden
	}
	body := asString(reply)
	if data, ok := reply["structuredContent"]; ok {
		body = asString(data)
	}
	return surfaceResult{code, body}
}

// TestWorkflowStateAcrossRolesAndSurfaces drives the release binary on
// Postgres: a desk actor's assignment, snooze, link and read marker are
// seen by the desk and (but for the marker) the viewer on the socket,
// REST, gRPC and MCP; an external participant sees none of it anywhere,
// and may neither change it nor filter by it; a second desk actor keeps
// its own read state and may be assigned, a viewer may not; the events
// reach the bus as references on the desk topics only.
func TestWorkflowStateAcrossRolesAndSurfaces(t *testing.T) {
	withE2EBinary(t, func() {
		s := newDeskSite(t)
		bridgeReady(t, s.d)
		s.restartAs(workflowSurfacesProfile)
		bridgeReady(t, s.d)
		a, va := s.conv, &visitor{token: s.token, conv: s.conv}
		b := newVisitor(t, s).conv
		aid, bid := string(a.ID), string(b.ID)
		self := me()
		va.say(t, s, "unread for everyone else")
		var out workflowOutJSON
		s.json(&out, "conversation", "assign", aid, "--to", "self")
		s.json(&out, "conversation", "snooze", aid, "--for", "1h")
		s.json(&out, "conversation", "link", aid, "--conversation", bid)
		var mark readMarkOutJSON
		s.json(&mark, "conversation", "mark-read", aid)

		// The bus: references on the desk topics, none public, nothing
		// of who or until when; marking read emits nothing.
		l := s.await("the workflow events", func(l busLog) bool {
			return len(l.find("desk", "uma.desk.conversation.assigned", "", "")) > 0 &&
				len(l.find("desk", "uma.desk.conversation.snoozed", "", "")) > 0 &&
				len(l.find("desk", "uma.desk.conversation.linked", "", "")) >= 2
		})
		for _, e := range l {
			if !strings.Contains(e.Topic, "conversation.assigned") && !strings.Contains(e.Topic, "conversation.snoozed") &&
				!strings.Contains(e.Topic, "conversation.linked") {
				continue
			}
			if e.Scope != "desk" || !strings.HasPrefix(e.Topic, "uma.desk.") {
				t.Errorf("a workflow event reached %s on %s", e.Scope, e.Topic)
			}
			if e.Payload.Conversation == "" || e.Payload.Operation == "" || strings.Contains(e.raw, "uid:") || strings.Contains(e.raw, "until") {
				t.Errorf("workflow event payload = %s", e.raw)
			}
		}
		for _, e := range l {
			if strings.Contains(e.Topic, "read") && strings.HasPrefix(e.Topic, "uma.") && !strings.Contains(e.Topic, "message") {
				t.Errorf("read state reached the bus: %s", e.raw)
			}
		}

		x, token, _ := serveDesk(t, s)
		desk, peer, ext, view := token, x.auth.token(deskPeer), x.auth.token(extActor), x.auth.token(viewActor)
		get := []string{"conversation", "get"}

		for who, tc := range map[string]struct {
			token             string
			state, read, none bool
		}{
			"desk":     {desk, true, true, false},
			"peer":     {peer, true, true, false},
			"viewer":   {view, true, false, false},
			"external": {ext, false, false, true},
		} {
			for surf, r := range surfaceCall(t, x, tc.token, get, []string{aid}, nil) {
				if r.Code != http.StatusOK {
					t.Errorf("%s %s: conversation get = %d %s", who, surf, r.Code, r.Body)
					continue
				}
				for _, k := range []string{`"assignee"`, `"snooze"`, `"links"`} {
					if has := strings.Contains(r.Body, k); has != tc.state {
						t.Errorf("%s %s: %s present = %v, want %v\n%s", who, surf, k, has, tc.state, r.Body)
					}
				}
				if has := strings.Contains(r.Body, `"marked"`); has != tc.read {
					t.Errorf("%s %s: a read state present = %v, want %v\n%s", who, surf, has, tc.read, r.Body)
				}
				if has := strings.Contains(r.Body, `"read_through"`); has != (who == "desk") {
					t.Errorf("%s %s: a marker present = %v\n%s", who, surf, has, r.Body)
				}
				if tc.none && (strings.Contains(r.Body, self) || strings.Contains(r.Body, bid) || strings.Contains(r.Body, `"workflow"`)) {
					t.Errorf("external %s: the conversation shows workflow state:\n%s", surf, r.Body)
				}
			}
		}
		// The peer's own read state is its own: never marked, though the
		// test's user marked the conversation.
		for surf, r := range surfaceCall(t, x, peer, get, []string{aid}, nil) {
			if !strings.Contains(r.Body, `"marked":false`) {
				t.Errorf("peer %s: sees another actor's marker:\n%s", surf, r.Body)
			}
		}

		// MCP takes no positional argument for a heo command: its list
		// shows the same state to the same roles.
		for who, tc := range map[string]struct {
			token string
			state bool
		}{"desk": {desk, true}, "viewer": {view, true}, "external": {ext, false}} {
			r := mcpCall(t, x, tc.token, "conversation.list", map[string]any{"snoozed": "include"})
			if r.Code != http.StatusOK || !strings.Contains(r.Body, aid) {
				t.Errorf("%s mcp list = %d %s", who, r.Code, r.Body)
			}
			for _, k := range []string{`"assignee"`, `"snooze"`, `"links"`} {
				if has := strings.Contains(r.Body, k); has != tc.state {
					t.Errorf("%s mcp list: %s present = %v, want %v\n%s", who, k, has, tc.state, r.Body)
				}
			}
		}
		for _, who := range []struct{ name, token string }{{"external", ext}, {"viewer", view}} {
			for _, tool := range []string{"conversation.assign", "conversation.snooze", "conversation.link", "conversation.mark-read"} {
				if r := mcpCall(t, x, who.token, tool, map[string]any{"args": []string{aid}, "to": "self", "for": "1h", "conversation": bid}); r.Code == http.StatusOK {
					t.Errorf("%s mcp %s = %d %s", who.name, tool, r.Code, r.Body)
				}
			}
		}
		if r := mcpCall(t, x, ext, "conversation.list", map[string]any{"snoozed": "only"}); r.Code == http.StatusOK {
			t.Errorf("external mcp list --snoozed only = %d %s", r.Code, r.Body)
		}

		// External: no write, no filter, no hidden conversation.
		writes := []struct {
			path  []string
			args  []string
			flags map[string]any
		}{
			{[]string{"conversation", "assign"}, []string{aid}, map[string]any{"to": "self"}},
			{[]string{"conversation", "unassign"}, []string{aid}, nil},
			{[]string{"conversation", "snooze"}, []string{aid}, map[string]any{"for": "1h"}},
			{[]string{"conversation", "unsnooze"}, []string{aid}, nil},
			{[]string{"conversation", "link"}, []string{aid}, map[string]any{"conversation": bid}},
			{[]string{"conversation", "unlink"}, []string{aid}, map[string]any{"conversation": bid}},
			{[]string{"conversation", "mark-read"}, []string{aid}, nil},
			{[]string{"conversation", "mark-unread"}, []string{aid}, nil},
		}
		for _, who := range []struct{ name, token string }{{"external", ext}, {"viewer", view}} {
			for _, w := range writes {
				for surf, r := range surfaceCall(t, x, who.token, w.path, w.args, w.flags) {
					if r.Code != http.StatusForbidden {
						t.Errorf("%s %s %v = %d, want 403: %s", who.name, surf, w.path, r.Code, r.Body)
					}
				}
			}
		}
		if got := s.view(a.ID); got.Assignee == nil || got.Snooze == nil || len(got.Links) != 1 {
			t.Fatalf("a refused write changed the conversation: %+v", got)
		}
		for _, flags := range []map[string]any{{"snoozed": "only"}, {"snoozed": "exclude"}, {"assignee": "none"}, {"unread": true}, {"linked": true}} {
			for surf, r := range surfaceCall(t, x, ext, []string{"conversation", "list"}, nil, flags) {
				if r.Code != http.StatusForbidden {
					t.Errorf("external %s list %v = %d, want 403: %s", surf, flags, r.Code, r.Body)
				}
			}
		}
		for surf, r := range surfaceCall(t, x, ext, []string{"conversation", "list"}, nil, nil) {
			if got := listedIn(r.Body); r.Code != http.StatusOK || !got[aid] || !got[bid] {
				t.Errorf("external %s list hides the snoozed conversation: %d %s", surf, r.Code, r.Body)
			}
		}
		for surf, r := range surfaceCall(t, x, view, []string{"conversation", "list"}, nil, nil) {
			if got := listedIn(r.Body); r.Code != http.StatusOK || got[aid] || !got[bid] {
				t.Errorf("viewer %s default list = %d %s", surf, r.Code, r.Body)
			}
		}

		// The log: workflow events are the desk's.
		for who, tc := range map[string]struct {
			token string
			want  bool
		}{"desk": {desk, true}, "peer": {peer, true}, "viewer": {view, false}, "external": {ext, false}} {
			for surf, r := range surfaceCall(t, x, tc.token, []string{"event", "list"}, nil, map[string]any{"limit": 100}) {
				if r.Code != http.StatusOK || strings.Contains(r.Body, "conversation.assigned") != tc.want {
					t.Errorf("%s %s event list = %d, assigned events present = %v, want %v:\n%s", who, surf, r.Code,
						strings.Contains(r.Body, "conversation.assigned"), tc.want, r.Body)
				}
			}
		}

		for who, tc := range map[string]struct {
			token string
			want  bool
		}{"desk": {desk, true}, "viewer": {view, false}, "external": {ext, false}} {
			r := mcpCall(t, x, tc.token, "event.list", map[string]any{"limit": 100})
			if r.Code != http.StatusOK || strings.Contains(r.Body, "conversation.assigned") != tc.want {
				t.Errorf("%s mcp event list = %d, assigned events present = %v, want %v", who, r.Code, strings.Contains(r.Body, "conversation.assigned"), tc.want)
			}
		}

		// A second desk actor: marks on its own, is eligible, and a viewer
		// is not.
		peerUnread := func() map[string]bool {
			t.Helper()
			r := surfaceCall(t, x, peer, []string{"conversation", "list"}, nil, map[string]any{"unread": true, "snoozed": "include"})["rest"]
			if r.Code != http.StatusOK {
				t.Fatalf("peer unread list = %d %s", r.Code, r.Body)
			}
			return listedIn(r.Body)
		}
		if got := peerUnread(); !got[aid] || !got[bid] {
			t.Errorf("the peer, who never marked, lists unread: %v", got)
		}
		for surf, r := range surfaceCall(t, x, peer, []string{"conversation", "mark-read"}, []string{aid}, nil) {
			if r.Code != http.StatusOK {
				t.Errorf("peer %s mark-read = %d %s", surf, r.Code, r.Body)
			}
		}
		if got := peerUnread(); got[aid] || !got[bid] {
			t.Errorf("the peer's unread list after marking: %v", got)
		}
		if got := s.listed("--unread", "--snoozed", "include"); len(got) != 1 || got[0] != b.ID {
			t.Errorf("the peer's marker moved the test user's unread state: %v", got)
		}
		s.json(&mark, "conversation", "mark-unread", aid)
		if got := s.listed("--unread", "--snoozed", "include"); len(got) != 2 {
			t.Errorf("mark-unread: the test user's unread list = %v", got)
		}
		if got := peerUnread(); got[aid] {
			t.Errorf("the test user's mark-unread moved the peer's marker: %v", got)
		}
		for surf, r := range surfaceCall(t, x, peer, []string{"conversation", "assign"}, []string{aid}, map[string]any{"to": viewActor}) {
			if r.Code != http.StatusForbidden || !strings.Contains(r.Body, "not eligible") {
				t.Errorf("peer %s assigns a viewer = %d %s", surf, r.Code, r.Body)
			}
		}
		for surf, r := range surfaceCall(t, x, peer, []string{"conversation", "assign"}, []string{aid}, map[string]any{"to": "self"}) {
			if r.Code != http.StatusOK || !strings.Contains(r.Body, deskPeer) {
				t.Errorf("peer %s assigns self = %d %s", surf, r.Code, r.Body)
			}
		}
		if got := s.view(a.ID); got.Assignee == nil || string(*got.Assignee) != deskPeer {
			t.Errorf("`--to self` as the peer assigned %v", got.Assignee)
		}

		// The socket, as an external participant and as a viewer.
		s.s.want(result{exit: x.d.term()}, 0)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
		s.d = s.start()
		ev := s.view(a.ID)
		if ev.Assignee != nil || ev.Snooze != nil || len(ev.Links) != 0 || ev.Workflow || ev.Read != nil {
			t.Fatalf("external sees workflow state on the socket: %+v", ev)
		}
		text := s.heo(0, "conversation", "get", aid).stdout
		for _, k := range []string{"assignee", "snoozed", "links", "read"} {
			if strings.Contains(text, k) {
				t.Errorf("external conversation get shows %q:\n%s", k, text)
			}
		}
		if got := s.listed(); len(got) != 2 {
			t.Errorf("external list = %v", got)
		}
		for _, args := range [][]string{
			{"conversation", "list", "--snoozed", "only"}, {"conversation", "list", "--unread"}, {"conversation", "list", "--assignee", "self"},
			{"conversation", "list", "--linked"}, {"conversation", "assign", aid, "--to", "self", "--dry-run"},
			{"conversation", "snooze", aid, "--for", "1h", "--dry-run"}, {"conversation", "link", aid, "--conversation", bid, "--dry-run"},
			{"conversation", "mark-read", aid, "--dry-run"}, {"conversation", "unassign", aid}, {"conversation", "unsnooze", aid},
			{"conversation", "unlink", aid, "--conversation", bid}, {"conversation", "mark-unread", aid},
		} {
			wantDenied(t, s.s.run(nil, args...), "heo.authz.")
		}
		if got := s.eventsOf(workflowKinds...); len(got) != 0 {
			t.Errorf("external lists %d workflow events", len(got))
		}
		s.restartAs("authorization:\n  owner: [viewer]\n")
		vv := s.view(a.ID)
		if !vv.Workflow || vv.Assignee == nil || vv.Snooze == nil || len(vv.Links) != 1 || vv.Read != nil {
			t.Fatalf("viewer view = %+v", vv)
		}
		if got := s.listed(); len(got) != 1 || got[0] != b.ID {
			t.Errorf("viewer default list = %v", got)
		}
		if got := s.listed("--assignee", "self"); got != nil {
			t.Errorf("viewer --assignee self = %v", got)
		}
		wantDenied(t, s.s.run(nil, "conversation", "mark-read", aid), "heo.authz.not_granted")
		wantDenied(t, s.s.run(nil, "conversation", "assign", aid, "--to", "self", "--dry-run"), "heo.authz.not_granted")
	})
}
