package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

type sendJSON struct {
	Message      model.ID
	Conversation model.ID
	Inbox        model.ID
	Visibility   model.Visibility
	Destination  model.ExternalRef
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// TestMessageSendStartsExchange drives the e2e build on Postgres through
// a channel that can start conversations. The e2e fake adapter stands in
// for the provider; uma, the daemon, its workers and the operation
// ledger are the release ones. A send preview shows the exact
// destination and visibility and that the conversation will be selected
// or created, and writes nothing; the send creates the conversation, is
// queued durably with its operation, and is delivered; the same key
// replays without sending twice; a send to the recipient's identity
// selects that conversation; an inbox whose adapter cannot start a
// conversation (github.issues) is refused (UNSUPPORTED, exit 2) before
// anything is written; a widget recipient that names none of the
// widget's two identity namespaces is refused (VALIDATION, exit 2), and
// one that names its namespace is previewed; a role without the desk's
// grant is denied.
func TestMessageSendStartsExchange(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		fake := c.fakeChannel("fake", adapterstest.Config{Account: "acct-send"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "outbox", "--destination", "out-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		var web channelOutJSON
		c.json(&web, "channel", "create", "--type", string(widget.Type), "--key", "web",
			"--configuration", `{"sites":["site-1"]}`, "--credential-ref", "widget-key")
		c.heo(0, "inbox", "create", "--channel", "web", "--key", "webin", "--destination", "site-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		c.heo(0, "channel", "create", "--type", string(github.Type), "--key", "gh", "--configuration", `{}`, "--credential-ref", "gh")
		c.heo(0, "inbox", "create", "--channel", "gh", "--key", "issues", "--destination", "example/uma-sandbox", "--mode", model.ModePerWorkItem)
		route := model.ExternalRef{Channel: fake.Ref, Namespace: adapterstest.Namespace, Kind: "user", ID: "cy"}
		nothingWritten := func(when string) {
			t.Helper()
			var convs pageJSON[model.Conversation]
			c.json(&convs, "conversation", "list")
			var msgs pageJSON[model.Message]
			c.json(&msgs, "message", "list")
			if len(convs.Items) != 0 || len(msgs.Items) != 0 {
				t.Fatalf("%s wrote conversations %+v, messages %+v", when, convs.Items, msgs.Items)
			}
		}

		// The preview: exact destination and visibility, the conversation
		// still to select, nothing written.
		send := []string{"message", "send", "--inbox", "outbox", "--to", "user:cy", "--text", "welcome aboard"}
		var plan planJSON
		c.json(&plan, append(send, "--dry-run")...)
		var dest model.ExternalRef
		_ = json.Unmarshal(plan.Args["destination"], &dest)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "send" || !strings.HasPrefix(plan.Effects[0].Target, "inbox:") ||
			dest != route || string(plan.Args["visibility"]) != `"public"` || string(plan.Args["inbox"]) != `"outbox"` ||
			!strings.Contains(string(plan.Args["conversation"]), "selected or created") {
			t.Fatalf("send preview = %+v (destination %+v)", plan, dest)
		}
		r := c.heo(0, append(send, "--dry-run")...)
		for _, want := range []string{"destination   user cy", "visibility    public", "inbox         outbox", "selected or created"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text send preview lacks %q:\n%s", want, r.stdout)
			}
		}
		nothingWritten("--dry-run")

		// An adapter that cannot start a conversation: refused at the
		// preview and for real, nothing written.
		for _, extra := range [][]string{{"--dry-run"}, nil} {
			r = c.heo(2, append([]string{"message", "send", "--inbox", "issues", "--to", "user:x", "--text", "hi"}, extra...)...)
			wantStderr(t, r, "UNSUPPORTED", "adapter "+string(github.Type)+" cannot start a conversation", "start_conversation")
		}
		// The widget starts conversations with a visitor or a verified
		// user, two identity namespaces: a recipient naming neither is
		// refused, at the preview and for real; one naming its namespace
		// is previewed with that destination.
		for _, extra := range [][]string{{"--dry-run"}, nil} {
			r = c.heo(2, append([]string{"message", "send", "--inbox", "webin", "--to", "user:x", "--text", "hi"}, extra...)...)
			wantStderr(t, r, "VALIDATION", widget.NamespaceVisitor+", "+widget.NamespaceUser, "<namespace>/user:x")
		}
		c.json(&plan, "message", "send", "--inbox", "webin", "--to", widget.NamespaceUser+"/"+widget.KindUser+":x", "--text", "hi", "--dry-run")
		dest = model.ExternalRef{}
		_ = json.Unmarshal(plan.Args["destination"], &dest)
		if want := (model.ExternalRef{Channel: web.Channel.Ref, Namespace: widget.NamespaceUser, Kind: widget.KindUser, ID: "x"}); dest != want {
			t.Errorf("widget send preview destination = %+v, want %+v", dest, want)
		}
		wantStderr(t, c.heo(2, "message", "send", "--inbox", "outbox", "--to", "cy", "--text", "hi"), "VALIDATION", "<kind>:<id>")
		nothingWritten("refused sends")

		// The send: a new conversation, queued durably, delivered.
		var sent, again sendJSON
		c.json(&sent, append(send, "--idempotency-key", "send-1")...)
		if sent.Operation.State != model.Queued || !sent.Operation.Durable || sent.Operation.Replayed || sent.Operation.ID == "" ||
			sent.Message == "" || sent.Conversation == "" || sent.Visibility != model.Public || sent.Destination != route ||
			sent.OperationKey != "send-1" {
			t.Fatalf("send = %+v", sent)
		}
		op := model.OperationRef{Tenant: "default", ID: sent.Operation.ID}
		if got := operationState(t, c.db.operator("default"), op); got.State != model.ProviderAccepted && got.State != model.Delivered {
			t.Fatalf("send operation = %s %+v", got.State, got.Failure)
		}
		var conv model.Conversation
		c.json(&conv, "conversation", "get", string(sent.Conversation))
		if conv.ReplyRoute != route || conv.Status != model.ConversationOpen {
			t.Fatalf("started conversation = %+v", conv)
		}

		// The same key replays: same operation and message, sent once.
		c.json(&again, append(send, "--idempotency-key", "send-1")...)
		if !again.Operation.Replayed || again.Operation.ID != sent.Operation.ID || again.Message != sent.Message ||
			again.Conversation != sent.Conversation {
			t.Fatalf("replayed send = %+v, first %+v", again, sent)
		}
		var msgs pageJSON[model.Message]
		c.json(&msgs, "message", "list", "--conversation", string(sent.Conversation))
		if len(msgs.Items) != 1 || msgs.Items[0].Ref.ID != sent.Message || msgs.Items[0].Origin == nil {
			t.Fatalf("messages after the replay = %+v, want the one send", msgs.Items)
		}

		// To the recipient's identity: its open conversation is selected.
		var idents pageJSON[model.Identity]
		c.json(&idents, "identity", "list", "--inbox", "outbox")
		if len(idents.Items) != 1 || idents.Items[0].SourceID != "cy" {
			t.Fatalf("identities = %+v", idents.Items)
		}
		var byID sendJSON
		c.json(&byID, "message", "send", "--inbox", "outbox", "--to", "identity:"+string(idents.Items[0].Ref.ID), "--text", "following up")
		if byID.Conversation != sent.Conversation || byID.Destination != route || byID.Operation.ID == sent.Operation.ID {
			t.Fatalf("send to the identity = %+v, want conversation %s", byID, sent.Conversation)
		}
		r = c.heo(0, "message", "send", "--inbox", "outbox", "--to", "user:dee", "--text", "hello dee")
		if !strings.Contains(r.stdout, "queued") || !strings.Contains(r.stderr, "queued: accepted durably") ||
			strings.Contains(r.stdout+r.stderr, "delivered") || !strings.Contains(r.stdout, "operation key") {
			t.Errorf("text send:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
		}
		c.stop(d)

		// A viewer may not send, preview included.
		c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		d = c.start()
		denied := []string{"message", "send", "--inbox", "outbox", "--to", "user:cy", "--text", "denied"}
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, c.s.run(nil, append(denied, extra...)...), "heo.authz.not_granted")
		}
		c.json(&msgs, "message", "list", "--conversation", string(sent.Conversation))
		if len(msgs.Items) != 2 {
			t.Fatalf("messages after the viewer's sends = %+v, want the two the desk sent", msgs.Items)
		}
		c.stop(d)
	})
}
