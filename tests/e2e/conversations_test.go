package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// opJSON is the operation a write reports.
type opJSON struct {
	ID       model.ID
	State    model.DeliveryState
	Durable  bool
	Replayed bool
}

type replyJSON struct {
	Message      model.ID
	Conversation model.ID
	Visibility   model.Visibility
	Destination  model.ExternalRef
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

type noteJSON struct {
	Message    model.ID
	Visibility model.Visibility
	Placement  string
	Operation  opJSON
}

type conversationOutJSON struct {
	Conversation model.Conversation
	Operation    opJSON
}

// desk is a cliSite whose widget channel and inbox were configured
// through heo, with one visitor conversation in it.
type deskSite struct {
	*cliSite
	d     *daemon
	route string
	token string
	conv  model.ConversationRef
}

// newDeskSite configures the widget through heo's commands, restarts
// the daemon so its receivers and webhook take the channel, and has a
// visitor open a conversation.
func newDeskSite(t *testing.T) *deskSite { return newDeskSiteWith(t, "") }

// newDeskSiteWith is newDeskSite with extra config appended to the
// services section (see newCLISite).
func newDeskSiteWith(t *testing.T, extra string) *deskSite {
	t.Helper()
	c := newCLISite(t, extra)
	d := c.start()
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(widget.Type), "--key", "web",
		"--configuration", `{"sites":["site-1"]}`, "--credential-ref", "widget-key")
	c.heo(0, "inbox", "create", "--channel", "web", "--key", "webin", "--destination", "site-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	c.stop(d)
	d = c.start()
	route := "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	site := &widgetSite{t: t, db: c.db, s: c.s, hook: c.hook, route: route}
	token, conv := site.visit()
	return &deskSite{cliSite: c, d: d, route: route, token: token, conv: conv}
}

func (s *deskSite) messages(args ...string) []model.Message {
	s.t.Helper()
	var p pageJSON[model.Message]
	s.json(&p, append([]string{"message", "list"}, args...)...)
	return p.Items
}

func (s *deskSite) conversation() model.Conversation {
	s.t.Helper()
	var got model.Conversation
	s.json(&got, "conversation", "get", string(s.conv.ID))
	return got
}

// TestConversationMessageAndNoteCommands drives the release binary on
// Postgres through a real widget visitor: inbound appears in the
// conversation and message listings with its origin and visibility; a
// reply preview shows its exact destination and visibility and writes
// nothing; a reply is queued with its operation, replays under its key,
// then reaches the visitor; a note's placement must be its inbox's;
// resolve and reopen move the conversation at its revision, and uma's
// lifecycle refuses reopening a closed one.
func TestConversationMessageAndNoteCommands(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)

	// Inbound: listed across inboxes, with its inbox, status, origin and
	// visibility.
	var convs pageJSON[model.Conversation]
	s.json(&convs, "conversation", "list")
	if len(convs.Items) != 1 || convs.Items[0].Ref != s.conv || convs.Items[0].Status != model.ConversationOpen {
		t.Fatalf("conversation list = %+v", convs.Items)
	}
	r := s.heo(0, "conversation", "list", "--inbox", "webin")
	for _, want := range []string{id, string(s.conv.Inbox.ID), "open"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("conversation list lacks %q:\n%s", want, r.stdout)
		}
	}
	s.json(&convs, "conversation", "list", "--status", "resolved,closed")
	if len(convs.Items) != 0 {
		t.Fatalf("list --status resolved,closed = %+v", convs.Items)
	}
	wantStderr(t, s.heo(2, "conversation", "list", "--status", "archived"), "VALIDATION", `status "archived"`)
	wantStderr(t, s.heo(3, "conversation", "get", "conv_404"), "NOT_FOUND")
	msgs := s.messages("--conversation", id)
	if len(msgs) != 1 || msgs[0].Audience.Visibility != model.Public || msgs[0].Origin != nil || msgs[0].Ref.Conversation != s.conv {
		t.Fatalf("message list = %+v", msgs)
	}
	inbound := msgs[0]
	r = s.heo(0, "message", "get", string(inbound.Ref.ID))
	for _, want := range []string{"hello", "public", "inbound"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("message get lacks %q:\n%s", want, r.stdout)
		}
	}

	// A reply preview: exact destination and visibility, nothing written.
	conv := s.conversation()
	reply := []string{"message", "reply", id, "--text", "we are on it"}
	var plan planJSON
	s.json(&plan, append(reply, "--dry-run")...)
	var dest model.ExternalRef
	_ = json.Unmarshal(plan.Args["destination"], &dest)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "send" || plan.Effects[0].Target != "conversation:"+id ||
		string(plan.Args["visibility"]) != `"public"` || string(plan.Args["inbox"]) != `"webin"` ||
		dest != conv.ReplyRoute || dest.ID == "" {
		t.Fatalf("reply preview = %+v (destination %+v, route %+v)", plan, dest, conv.ReplyRoute)
	}
	r = s.heo(0, append(reply, "--dry-run")...)
	for _, want := range []string{"destination   " + dest.Kind + " " + dest.ID, "visibility    public", "inbox         webin"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("text reply preview lacks %q:\n%s", want, r.stdout)
		}
	}
	if got := s.messages("--conversation", id); len(got) != 1 {
		t.Fatalf("--dry-run wrote %+v", got)
	}

	// The reply: queued with its operation, never delivered at return;
	// the same key replays; the workers deliver it to the visitor.
	var sent, again replyJSON
	s.json(&sent, append(reply, "--idempotency-key", "reply-1")...)
	if sent.Operation.State != model.Queued || !sent.Operation.Durable || sent.Operation.Replayed || sent.Operation.ID == "" ||
		sent.Message == "" || sent.Visibility != model.Public || sent.Destination != conv.ReplyRoute || sent.OperationKey != "reply-1" {
		t.Fatalf("reply = %+v", sent)
	}
	s.json(&again, append(reply, "--idempotency-key", "reply-1")...)
	if !again.Operation.Replayed || again.Operation.ID != sent.Operation.ID || again.Message != sent.Message {
		t.Fatalf("replayed reply = %+v, first %+v", again, sent)
	}
	events := widgetEvents(t, s.route, s.token, "")
	if ev := next(t, events); ev.name != widget.EventMessage || !strings.Contains(ev.data, "we are on it") {
		t.Fatalf("visitor's event = %+v, want the reply", ev)
	}
	op := model.OperationRef{Tenant: "default", ID: sent.Operation.ID}
	if got := operationState(t, s.db.operator("default"), op); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Fatalf("reply operation = %s %+v", got.State, got.Failure)
	}
	r = s.heo(0, "message", "reply", id, "--text", "one more")
	if !strings.Contains(r.stdout, "queued") || !strings.Contains(r.stderr, "queued: accepted durably") ||
		strings.Contains(r.stdout+r.stderr, "delivered") || !strings.Contains(r.stdout, "operation key") {
		t.Errorf("text reply:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	if got := s.messages("--conversation", id); len(got) != 3 || got[1].Origin == nil || got[1].Origin.Actor != model.ActorID(me()) {
		t.Fatalf("messages after the replies = %+v", got)
	}

	// Notes: the placement must be the inbox's (local here).
	r = s.heo(4, "note", "add", id, "--text", "vip customer", "--placement", "provider")
	wantStderr(t, r, "CONFLICT", "inbox webin, which places notes local", "--placement local", "--deliver-notes=true")
	wantStderr(t, s.heo(2, "note", "add", id, "--text", "x", "--placement", "both"), "USAGE", "--placement")
	s.heo(2, "note", "add", id, "--text", "x")
	s.json(&plan, "note", "add", id, "--text", "vip customer", "--placement", "local", "--dry-run")
	if len(plan.Effects) != 1 || string(plan.Args["visibility"]) != `"internal"` || string(plan.Args["placement"]) != `"local"` ||
		!strings.Contains(plan.Effects[0].Detail, "nothing reaches the provider") {
		t.Fatalf("note preview = %+v", plan)
	}
	var notes pageJSON[model.Message]
	s.json(&notes, "note", "list")
	if len(notes.Items) != 0 {
		t.Fatalf("note --dry-run wrote %+v", notes.Items)
	}
	var noted noteJSON
	s.json(&noted, "note", "add", id, "--text", "vip customer", "--placement", "local", "--idempotency-key", "note-1")
	if noted.Visibility != model.Internal || noted.Placement != "local" || noted.Operation.State != model.Committed || noted.Message == "" {
		t.Fatalf("note add = %+v", noted)
	}
	s.json(&noted, "note", "add", id, "--text", "vip customer", "--placement", "local", "--idempotency-key", "note-1")
	if !noted.Operation.Replayed {
		t.Fatalf("replayed note = %+v", noted)
	}
	s.json(&notes, "note", "list", "--conversation", id)
	if len(notes.Items) != 1 || notes.Items[0].Ref.ID != noted.Message || notes.Items[0].Audience.Visibility != model.Internal {
		t.Fatalf("note list = %+v", notes.Items)
	}
	r = s.heo(0, "note", "get", string(noted.Message))
	if !strings.Contains(r.stdout, "vip customer") || !strings.Contains(r.stdout, "internal") {
		t.Errorf("note get:\n%s", r.stdout)
	}
	wantStderr(t, s.heo(3, "note", "get", string(inbound.Ref.ID)), "NOT_FOUND", "not an internal note")
	if got := s.messages("--conversation", id); len(got) != 4 || got[3].Audience.Visibility != model.Internal {
		t.Fatalf("desk message list = %+v, want the note among them", got)
	}

	// Resolve and reopen, at the revision read.
	conv = s.conversation()
	wantStderr(t, s.heo(4, "conversation", "resolve", id, "--revision", "stale"), "CONFLICT", "is at revision "+string(conv.Revision))
	s.json(&plan, "conversation", "resolve", id, "--revision", string(conv.Revision), "--dry-run")
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || string(plan.Args["state"]) != `"resolved"` ||
		s.conversation().Status != model.ConversationOpen {
		t.Fatalf("resolve preview = %+v, or it wrote", plan)
	}
	var moved conversationOutJSON
	s.json(&moved, "conversation", "resolve", id, "--revision", string(conv.Revision), "--idempotency-key", "resolve-1")
	if moved.Conversation.Status != model.ConversationResolved || moved.Operation.State != model.Committed {
		t.Fatalf("resolve = %+v", moved)
	}
	s.json(&moved, "conversation", "resolve", id, "--revision", string(conv.Revision), "--idempotency-key", "resolve-1")
	if !moved.Operation.Replayed {
		t.Fatalf("replayed resolve = %+v", moved)
	}
	s.json(&moved, "conversation", "reopen", id, "--revision", string(moved.Conversation.Revision))
	if moved.Conversation.Status != model.ConversationOpen {
		t.Fatalf("reopen = %+v", moved)
	}

	// Closed is terminal: uma refuses reopening it, exit 4.
	if _, err := s.db.operator("default").Conversations().Transition(t.Context(), model.Transition{
		Command:      model.Command{Call: model.Call{Correlation: "e2e"}, Operation: "close-1", ExpectedRevision: moved.Conversation.Revision},
		Conversation: s.conv, State: model.ConversationClosed,
	}); err != nil {
		t.Fatal(err)
	}
	s.json(&convs, "conversation", "list", "--status", "closed")
	if len(convs.Items) != 1 {
		t.Fatalf("list --status closed = %+v", convs.Items)
	}
	closed := s.conversation()
	wantStderr(t, s.heo(4, "conversation", "reopen", id, "--revision", string(closed.Revision)), "CONFLICT", "terminal")
	s.stop(s.d)
}

// TestInternalNotesHiddenFromViewers: a note the desk added is for desk
// actors alone on every surface heo serves. A viewer — at the terminal
// over the control socket, over REST and over MCP — is refused the note
// commands (exit 5, the policy's desk-only reason) and finds no note
// among the messages, listed or by ID, in text or JSON; a desk actor on
// the same daemon reads it.
func TestInternalNotesHiddenFromViewers(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)
	var noted noteJSON
	s.json(&noted, "note", "add", id, "--text", "internal secret", "--placement", "local")
	s.stop(s.d)

	// The owner becomes a viewer; a token subject holds the desk role.
	const deskActor = "uid:4243"
	s.s.write("cfg/heo/profiles/default.yaml",
		"authorization:\n  owner: [viewer]\n  grants:\n    - actor: \""+deskActor+"\"\n      roles: [desk]\n")
	auth := s.s.useHTTPAuth()
	api, mcp := freeAddr(t), freeAddr(t)
	d := s.s.start(nil, nil, []string{"socket", "workers", "receivers", "api", "mcp"}, "-c", auth.config,
		"service", "start", "--enable", "api", "--enable", "mcp", "--addr", api, "--mcp-addr", mcp)
	defer func() { s.s.want(result{exit: d.term()}, 0) }()

	// The terminal, over the control socket.
	for _, args := range [][]string{
		{"note", "list"},
		{"note", "get", string(noted.Message)},
		{"note", "list", "--format", "json"},
		{"note", "add", id, "--text", "x", "--placement", "local"},
	} {
		wantDenied(t, s.s.run(nil, args...), "heo.authz.")
	}
	wantStderr(t, s.heo(5, "note", "list"), "UNAUTHORIZED", "heo.authz.desk_only")
	msgs := s.messages()
	if len(msgs) == 0 {
		t.Fatal("viewer reads no messages at all")
	}
	for _, m := range msgs {
		if m.Ref.ID == noted.Message || m.Audience.Visibility != model.Public {
			t.Fatalf("viewer message list (json) holds %+v", m)
		}
	}
	for _, args := range [][]string{{"message", "list"}, {"message", "list", "--conversation", id}, {"message", "list", "--format", "yaml"}} {
		if r := s.heo(0, args...); strings.Contains(r.stdout, "internal secret") || strings.Contains(r.stdout, string(noted.Message)) {
			t.Fatalf("viewer %v shows the note:\n%s", args, r.stdout)
		}
	}
	wantStderr(t, s.heo(3, "message", "get", string(noted.Message)), "NOT_FOUND")

	// REST.
	get := func(token, route string) (int, string) {
		t.Helper()
		resp, err := authed(http.MethodGet, "http://"+api+route, token, "")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	viewer, desk := auth.token(me()), auth.token(deskActor)
	list := "/v1/commands/message/list?conversation=" + url.QueryEscape(id)
	if code, body := get(viewer, list); code != http.StatusOK || strings.Contains(body, "internal secret") ||
		strings.Contains(body, string(noted.Message)) || !strings.Contains(body, "hello") {
		t.Fatalf("REST message list as viewer = %d %s", code, body)
	}
	if code, body := get(viewer, "/v1/commands/note/list"); code != http.StatusForbidden || !strings.Contains(body, "heo.authz.desk_only") ||
		strings.Contains(body, "internal secret") {
		t.Fatalf("REST note list as viewer = %d %s", code, body)
	}
	if code, body := get(desk, list); code != http.StatusOK || !strings.Contains(body, "internal secret") {
		t.Fatalf("REST message list as desk = %d %s", code, body)
	}
	if code, body := get(desk, "/v1/commands/note/list"); code != http.StatusOK || !strings.Contains(body, "internal secret") {
		t.Fatalf("REST note list as desk = %d %s", code, body)
	}

	// MCP.
	m := newMCPClient(t, "http://"+mcp+"/mcp", viewer)
	page := m.callTool(t, "message.list", map[string]any{})
	if b, _ := json.Marshal(page); strings.Contains(string(b), "internal secret") || !strings.Contains(string(b), "hello") {
		t.Fatalf("MCP message list as viewer = %s", b)
	}
	res := m.request(t, "tools/call", map[string]any{"name": "note.list", "arguments": map[string]any{}})
	if b, _ := json.Marshal(res); res["isError"] != true || strings.Contains(string(b), "internal secret") ||
		!strings.Contains(string(b), "heo.authz.desk_only") {
		t.Fatalf("MCP note list as viewer = %s", b)
	}
	page = newMCPClient(t, "http://"+mcp+"/mcp", desk).callTool(t, "note.list", map[string]any{})
	if b, _ := json.Marshal(page); !strings.Contains(string(b), "internal secret") {
		t.Fatalf("MCP note list as desk = %s", b)
	}
}
