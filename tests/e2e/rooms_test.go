package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

const fakeRouteLogEnv = "HEO_E2E_FAKE_ROUTE_LOG"

// routes reads the routes the fake provider was asked to send to, one
// "<operation> <kind> <parent|-> <id>" line each.
func routes(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// injectStatus hands ch an envelope once its webhook is up and returns
// the status the channel answered, refusals included.
func (c *cliSite) injectStatus(ch model.Channel, in adapterstest.Inject) int {
	c.t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		c.t.Fatal(err)
	}
	url := "http://" + c.hook + "/webhooks/" + string(ch.Ref.ID) + adapterstest.InjectRoute
	code := 0
	eventually(c.t, "the channel's webhook answers", func() bool {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // loopback test URL
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		code = resp.StatusCode
		return code != http.StatusNotFound && code != http.StatusServiceUnavailable
	})
	return code
}

func (c *cliSite) conversationsOf(inbox string) []model.Conversation {
	c.t.Helper()
	var p pageJSON[model.Conversation]
	c.json(&p, "conversation", "list", "--inbox", inbox)
	return p.Items
}

func (c *cliSite) messagesOf(conv model.ID) []model.Message {
	c.t.Helper()
	var p pageJSON[model.Message]
	c.json(&p, "message", "list", "--conversation", string(conv))
	return p.Items
}

// TestRoomInboxFlow drives the e2e build on Postgres through a per_room
// inbox: the inbox is created without the per-contact rules; a first
// message to a room reference opens the room's conversation, routed to
// the room, with no identity; a message received in that room joins it
// and a message in another room opens another; a reply goes to the room
// and the provider is asked to send there; recipients a room inbox
// cannot route to are refused before anything is written.
func TestRoomInboxFlow(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		routeLog := c.s.path("routes.log")
		c.s.env[fakeRouteLogEnv] = routeLog
		d := c.start()
		ch := c.fakeChannel("chat", adapterstest.Config{Account: "acct-rooms"})

		// The inbox: per_room needs none of the per-contact rules, and
		// refuses them.
		var in inboxOutJSON
		c.json(&in, "inbox", "create", "--channel", "chat", "--key", "rooms", "--destination", "ws-1", "--mode", model.ModePerRoom)
		if in.Inbox.Selection.Mode != model.ModePerRoom || in.Inbox.Selection.LockToSingle != nil || in.Inbox.Selection.LifecycleRules != "" {
			t.Fatalf("per_room inbox selection = %+v", in.Inbox.Selection)
		}
		r := c.heo(2, "inbox", "create", "--channel", "chat", "--key", "bad", "--destination", "ws-bad", "--mode", model.ModePerRoom, "--lock-to-single=true")
		wantStderr(t, r, "VALIDATION")
		if got := len(c.inboxes()); got != 1 {
			t.Fatalf("inboxes after the refused create = %d, want 1", got)
		}

		room := model.ExternalRef{Channel: ch.Ref, Namespace: adapterstest.Namespace, Kind: model.RefKindRoom, ID: "general"}
		send := []string{"message", "send", "--inbox", "rooms", "--to", "room:general", "--text", "welcome to the room"}

		// Refused before anything is written: a sender route in a room
		// inbox, a bare id, a thread reference, a record the room does
		// not have.
		nothing := func(when string) {
			t.Helper()
			if got := c.conversationsOf("rooms"); len(got) != 0 {
				t.Fatalf("%s wrote conversations %+v", when, got)
			}
		}
		for _, extra := range [][]string{{"--dry-run"}, nil} {
			wantStderr(t, c.heo(2, append([]string{"message", "send", "--inbox", "rooms", "--to", "user:cy", "--text", "hi"}, extra...)...),
				"VALIDATION", "per_room selection needs a room reference")
			wantStderr(t, c.heo(2, append([]string{"message", "send", "--inbox", "rooms", "--to", "thread:t1", "--text", "hi"}, extra...)...),
				"VALIDATION", "per_room selection needs a room reference")
			wantStderr(t, c.heo(2, append([]string{"message", "send", "--inbox", "rooms", "--to", "general", "--text", "hi"}, extra...)...),
				"VALIDATION", "<kind>:<id>")
		}
		wantStderr(t, c.heo(3, "message", "send", "--inbox", "rooms", "--to", "contact:ghost", "--text", "hi"), "NOT_FOUND")
		nothing("refused sends")

		// The preview names the room, the visibility and the inbox, and
		// writes nothing; the flag may come before the command's own.
		var plan planJSON
		c.json(&plan, append(send, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "send" || !strings.Contains(string(plan.Args["destination"]), `"room"`) ||
			!strings.Contains(string(plan.Args["destination"]), `"general"`) || string(plan.Args["visibility"]) != `"public"` {
			t.Fatalf("room send preview = %+v", plan)
		}
		r = c.heo(0, "--dry-run", "message", "send", "--inbox", "rooms", "--to", "room:general", "--text", "welcome to the room")
		for _, want := range []string{"destination   room general", "visibility    public", "inbox         rooms"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text room preview lacks %q:\n%s", want, r.stdout)
			}
		}
		nothing("--dry-run")

		// Start: the room's conversation, routed to the room, no owner,
		// no identity; delivered to the room; the same key replays.
		var sent, again sendJSON
		c.json(&sent, append(send, "--idempotency-key", "room-1")...)
		if sent.Operation.State != model.Queued || sent.Conversation == "" || sent.Destination != room {
			t.Fatalf("room send = %+v", sent)
		}
		c.json(&again, append(send, "--idempotency-key", "room-1")...)
		if !again.Operation.Replayed || again.Conversation != sent.Conversation || again.Message != sent.Message {
			t.Fatalf("replayed room send = %+v, first %+v", again, sent)
		}
		var conv model.Conversation
		c.json(&conv, "conversation", "get", string(sent.Conversation))
		if conv.ReplyRoute != room || conv.Owner != nil || conv.Mode != model.ModePerRoom || len(conv.External) != 1 || conv.External[0] != room {
			t.Fatalf("started room conversation = %+v", conv)
		}
		var idents pageJSON[model.Identity]
		c.json(&idents, "identity", "list", "--inbox", "rooms")
		if len(idents.Items) != 0 {
			t.Fatalf("a room start made identities %+v", idents.Items)
		}
		c.waitDelivery(sent.Operation.ID, model.ProviderAccepted, model.Delivered)

		// A second send to the room selects the same conversation.
		var second sendJSON
		c.json(&second, "message", "send", "--inbox", "rooms", "--to", "room:general", "--text", "and again")
		if second.Conversation != sent.Conversation || second.Operation.ID == sent.Operation.ID {
			t.Fatalf("second room send = %+v, want conversation %s", second, sent.Conversation)
		}

		// Received in the room: joins the started conversation, from its
		// own sender; another room opens another conversation.
		c.inject(ch, adapterstest.Inject{Event: "r1", Message: "pm-r1", From: "ada", Destination: "ws-1", Room: "general", Text: "hi all"})
		c.inject(ch, adapterstest.Inject{Event: "r2", Message: "pm-r2", From: "bob", Destination: "ws-1", Room: "general", Text: "hello ada"})
		c.inject(ch, adapterstest.Inject{Event: "r3", Message: "pm-r3", From: "ada", Destination: "ws-1", Room: "random", Text: "elsewhere"})
		convs := c.conversationsOf("rooms")
		if len(convs) != 2 {
			t.Fatalf("conversations = %+v, want one per room", convs)
		}
		if got := len(c.messagesOf(sent.Conversation)); got != 4 {
			t.Fatalf("messages in the general room = %d, want 2 sent + 2 received", got)
		}
		var random model.Conversation
		for _, cv := range convs {
			if cv.Ref.ID != sent.Conversation {
				random = cv
			}
		}
		if random.ReplyRoute.Kind != model.RefKindRoom || random.ReplyRoute.ID != "random" || random.Owner != nil {
			t.Fatalf("random room conversation = %+v", random)
		}

		// An envelope with no room cannot be filed in a room inbox.
		wantRefused := c.injectStatus(ch, adapterstest.Inject{Event: "r4", Message: "pm-r4", From: "ada", Destination: "ws-1", Text: "lost"})
		if wantRefused != 422 {
			t.Fatalf("inject without a room = %d, want 422", wantRefused)
		}

		// Reply: to the room, queued, then asked of the provider.
		var reply replyJSON
		c.json(&reply, "message", "reply", string(sent.Conversation), "--text", "thanks all", "--idempotency-key", "room-reply")
		if reply.Destination != room || reply.Operation.State != model.Queued {
			t.Fatalf("room reply = %+v", reply)
		}
		c.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered)
		eventually(t, "the provider asked to send to the room", func() bool {
			for _, l := range routes(t, routeLog) {
				if strings.HasSuffix(l, " room - general") && strings.HasPrefix(l, string(reply.Operation.ID)+" ") {
					return true
				}
			}
			return false
		})
		c.stop(d)
	})
}

// TestThreadInboxFlow drives the e2e build on Postgres through a
// per_thread inbox: messages received in a thread file under the
// thread, not the room; a thread of another room with the same id is
// another conversation; a reply goes to the thread, parent room
// included; a first message to a thread reference joins the
// conversation the same reference received opened.
func TestThreadInboxFlow(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		routeLog := c.s.path("routes.log")
		c.s.env[fakeRouteLogEnv] = routeLog
		d := c.start()
		ch := c.fakeChannel("chat", adapterstest.Config{Account: "acct-threads"})
		var in inboxOutJSON
		c.json(&in, "inbox", "create", "--channel", "chat", "--key", "threads", "--destination", "ws-1", "--mode", model.ModePerThread)
		if in.Inbox.Selection.Mode != model.ModePerThread || in.Inbox.Selection.LockToSingle != nil {
			t.Fatalf("per_thread inbox selection = %+v", in.Inbox.Selection)
		}

		// A room reference is no thread.
		for _, extra := range [][]string{{"--dry-run"}, nil} {
			wantStderr(t, c.heo(2, append([]string{"message", "send", "--inbox", "threads", "--to", "room:general", "--text", "hi"}, extra...)...),
				"VALIDATION", "per_thread selection needs a thread reference")
		}

		// Received: the same thread id in two rooms is two threads; a
		// message of a thread joins its conversation.
		c.inject(ch, adapterstest.Inject{Event: "t1", Message: "pm-t1", From: "ada", Destination: "ws-1", Room: "general", Thread: "100.1", Text: "question"})
		c.inject(ch, adapterstest.Inject{Event: "t2", Message: "pm-t2", From: "bob", Destination: "ws-1", Room: "general", Thread: "100.1", Text: "answer"})
		c.inject(ch, adapterstest.Inject{Event: "t3", Message: "pm-t3", From: "ada", Destination: "ws-1", Room: "general", Thread: "200.2", Text: "other thread"})
		c.inject(ch, adapterstest.Inject{Event: "t4", Message: "pm-t4", From: "ada", Destination: "ws-1", Room: "random", Thread: "100.1", Text: "same id, other room"})
		if code := c.injectStatus(ch, adapterstest.Inject{Event: "t5", Message: "pm-t5", From: "ada", Destination: "ws-1", Room: "general", Text: "no thread"}); code != 422 {
			t.Fatalf("inject without a thread = %d, want 422", code)
		}
		convs := c.conversationsOf("threads")
		if len(convs) != 3 {
			t.Fatalf("conversations = %+v, want one per (room, thread)", convs)
		}
		var first model.Conversation
		for _, cv := range convs {
			if cv.ReplyRoute.ID == "100.1" && cv.ReplyRoute.ParentID == "general" {
				first = cv
			}
			if cv.Mode != model.ModePerThread || cv.Owner != nil || cv.ReplyRoute.Kind != model.RefKindThread {
				t.Fatalf("thread conversation = %+v", cv)
			}
		}
		if first.Ref.ID == "" || len(c.messagesOf(first.Ref.ID)) != 2 {
			t.Fatalf("conversation of thread 100.1 in general = %+v", first)
		}

		// Reply: to the thread with its room.
		var reply replyJSON
		c.json(&reply, "message", "reply", string(first.Ref.ID), "--text", "see the thread", "--idempotency-key", "thread-reply")
		if reply.Destination != first.ReplyRoute {
			t.Fatalf("thread reply destination = %+v, want %+v", reply.Destination, first.ReplyRoute)
		}
		c.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered)
		eventually(t, "the provider asked to send to the thread", func() bool {
			for _, l := range routes(t, routeLog) {
				if l == string(reply.Operation.ID)+" thread general 100.1" {
					return true
				}
			}
			return false
		})

		// A start to a thread reference joins the conversation of the
		// same reference received: thread:<id> names a thread with no
		// parent room, as a provider whose thread ids stand alone
		// reports it.
		c.inject(ch, adapterstest.Inject{Event: "t6", Message: "pm-t6", From: "ada", Destination: "ws-1", Thread: "solo-1", Text: "standalone thread"})
		var solo model.Conversation
		for _, cv := range c.conversationsOf("threads") {
			if cv.ReplyRoute.ID == "solo-1" {
				solo = cv
			}
		}
		if solo.Ref.ID == "" || solo.ReplyRoute.ParentID != "" {
			t.Fatalf("conversation of the standalone thread = %+v", solo)
		}
		var sent sendJSON
		c.json(&sent, "message", "send", "--inbox", "threads", "--to", "thread:solo-1", "--text", "following up")
		if sent.Conversation != solo.Ref.ID || sent.Destination != solo.ReplyRoute {
			t.Fatalf("send to thread:solo-1 = %+v, want conversation %s routed %+v", sent, solo.Ref.ID, solo.ReplyRoute)
		}
		if got := len(c.messagesOf(solo.Ref.ID)); got != 2 {
			t.Fatalf("messages in the standalone thread = %d, want the received one and the start", got)
		}
		c.stop(d)
	})
}

// TestInboxModeChange drives the e2e build on Postgres through an
// inbox that changes selection mode: leaving per_contact drops its
// lock and lifecycle rules, so the inbox starts room conversations
// without them; a scoped inbox refuses the per-contact settings, on
// update as on create; returning to per_contact asks for them again.
func TestInboxModeChange(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		c.fakeChannel("chat", adapterstest.Config{Account: "acct-modes"})
		var in inboxOutJSON
		c.json(&in, "inbox", "create", "--channel", "chat", "--key", "flex", "--destination", "ws-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		if in.Inbox.Selection.LockToSingle == nil || in.Inbox.Selection.LifecycleRules == "" {
			t.Fatalf("per_contact inbox selection = %+v", in.Inbox.Selection)
		}

		// To a room inbox: the per-contact settings go with the mode.
		var out inboxOutJSON
		c.json(&out, "inbox", "update", "flex", "--revision", string(in.Inbox.Revision), "--mode", model.ModePerRoom)
		sel := out.Inbox.Selection
		if sel.Mode != model.ModePerRoom || sel.LockToSingle != nil || sel.LifecycleRules != "" {
			t.Fatalf("per_room selection after the update = %+v", sel)
		}
		var sent sendJSON
		c.json(&sent, "message", "send", "--inbox", "flex", "--to", "room:lobby", "--text", "opening")
		if sent.Conversation == "" || sent.Destination.Kind != model.RefKindRoom || sent.Destination.ID != "lobby" {
			t.Fatalf("room send after the mode change = %+v", sent)
		}

		// A scoped inbox takes no per-contact settings.
		rev := out.Inbox.Revision
		for _, extra := range [][]string{{"--lock-to-single=true"}, {"--lifecycle-rules", model.RuleSetContactLifecycleV1}} {
			r := c.heo(2, append([]string{"inbox", "update", "flex", "--revision", string(rev)}, extra...)...)
			wantStderr(t, r, "VALIDATION")
		}
		var got model.Inbox
		c.json(&got, "inbox", "get", "flex")
		if got.Revision != rev || got.Selection.LockToSingle != nil || got.Selection.Mode != model.ModePerRoom {
			t.Fatalf("inbox after the refused updates = %+v", got)
		}

		// Back to per_contact: the settings it needs are asked for again.
		wantStderr(t, c.heo(2, "inbox", "update", "flex", "--revision", string(rev), "--mode", model.ModePerContact), "VALIDATION")
		c.json(&out, "inbox", "update", "flex", "--revision", string(rev), "--mode", model.ModePerContact,
			"--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		if out.Inbox.Selection.Mode != model.ModePerContact || out.Inbox.Selection.LockToSingle == nil {
			t.Fatalf("per_contact selection after returning = %+v", out.Inbox.Selection)
		}

		// The room's conversation outlives the mode change, as stored.
		if convs := c.conversationsOf("flex"); len(convs) != 1 || convs[0].Mode != model.ModePerRoom {
			t.Fatalf("conversations after the round trip = %+v", convs)
		}
		c.stop(d)
	})
}
