package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// effectsSite is a daemon with two fake channels on Postgres: "fx",
// whose adapter declares edit, delete and react, and "plain", whose
// adapter declares none of them. Each has an inbox and one inbound
// message from ada.
type effectsSite struct {
	*cliSite
	d       *daemon
	fx      model.Channel
	plain   model.Channel
	fxMsg   model.Message
	plMsg   model.Message
	effects string
}

// fakeOf creates a channel of fake type typ on account through heo.
func (c *cliSite) fakeOf(typ model.TypeKey, key string, cfg adapterstest.Config) model.Channel {
	c.t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		c.t.Fatal(err)
	}
	var out channelOutJSON
	c.json(&out, "channel", "create", "--type", string(typ), "--key", key, "--configuration", string(b))
	return out.Channel
}

// injectReceipt has ch accept an inbound envelope and reports whether
// the sink took it as a duplicate.
func (c *cliSite) injectReceipt(ch model.Channel, in adapterstest.Inject) bool {
	c.t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		c.t.Fatal(err)
	}
	url := "http://" + c.hook + "/webhooks/" + string(ch.Ref.ID) + adapterstest.InjectRoute
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // loopback test URL
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var r struct{ Duplicate bool }
				if err := json.Unmarshal(b, &r); err != nil {
					c.t.Fatalf("receipt %s: %v", b, err)
				}
				return r.Duplicate
			}
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				c.t.Fatalf("inject %s = %d %s", in.Event, resp.StatusCode, b)
			}
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("inject %s: the channel's webhook never answered (%v)", in.Event, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// noRateLimit is a profile that turns off the served rate limit: a
// destructive command is counted three at a time per minute, and a test
// that previews and runs several would spend that.
const noRateLimit = "services:\n  all:\n    rate_limit:\n      enabled: false\n"

func newEffectsSite(t *testing.T, profile string) *effectsSite {
	t.Helper()
	c := newCLISite(t, noBridge)
	e := &effectsSite{cliSite: c, effects: c.s.path("effects.log")}
	c.s.env["HEO_E2E_FAKE_EFFECT_LOG"] = e.effects
	if profile != "" {
		c.s.write("cfg/heo/profiles/default.yaml", profile)
	}
	e.d = c.start()
	e.fx = c.fakeOf(adapterstest.EffectsType, "fx", adapterstest.Config{Account: "acct-fx"})
	e.plain = c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
	for _, in := range []struct{ ch, key, dest string }{{"fx", "fxin", "fx-1"}, {"plain", "plin", "pl-1"}} {
		c.heo(0, "inbox", "create", "--channel", in.ch, "--key", in.key, "--destination", in.dest,
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	}
	e.fxMsg = e.receive(e.fx, "fxin", "fx-1", "ev-fx-1", "pm-fx-1", "hello from ada")
	e.plMsg = e.receive(e.plain, "plin", "pl-1", "ev-pl-1", "pm-pl-1", "hello from ada")
	return e
}

// receive injects a public message from ada and returns it as listed.
func (e *effectsSite) receive(ch model.Channel, inbox, dest, event, provider, text string) model.Message {
	e.t.Helper()
	e.inject(ch, adapterstest.Inject{Event: event, Message: provider, From: "ada", Destination: dest, Text: text})
	var msgs pageJSON[model.Message]
	e.json(&msgs, "message", "list", "--inbox", inbox)
	for _, m := range msgs.Items {
		if desk.Text(m.Content) == text {
			return m
		}
	}
	e.t.Fatalf("message %s not listed: %+v", provider, msgs.Items)
	return model.Message{}
}

func (e *effectsSite) message(m model.Message) model.Message {
	e.t.Helper()
	var got model.Message
	e.json(&got, "message", "get", string(m.Ref.ID))
	return got
}

// appliedLines is what the fake provider was asked, one line each.
func (e *effectsSite) appliedLines() []string {
	b, _ := os.ReadFile(e.effects)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ledger counts the operations uma holds: what a refused or previewed
// verb must leave as it was.
func (e *effectsSite) ledger() int {
	e.t.Helper()
	c := e.db.operator("default")
	req := model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100}}
	n := 0
	for {
		p, err := c.Operations().List(e.t.Context(), req)
		if err != nil {
			e.t.Fatal(err)
		}
		n += len(p.Items)
		if p.Complete || p.Next == "" {
			return n
		}
		req.Page.After = p.Next
	}
}

type providerOpJSON struct {
	Message      model.ID
	Conversation model.ID
	Inbox        model.ID
	Visibility   model.Visibility
	Revision     model.Revision
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

type reactJSON struct {
	Message      model.ID
	Emoji        string
	Remove       bool
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// TestMessageEditLifecycle drives the e2e build on Postgres through the
// full edit lifecycle on a fake provider that declares edit: a preview
// names the platform effect, the audience and what it checked of the
// adapter, and writes nothing; the edit is queued durably with the
// revision always sent, delivered, applied by the provider; the
// provider's echo through ingress is no duplicate and changes nothing;
// a stale revision is a conflict naming the current one; an adapter
// that does not declare edit is refused at the preview and on
// submission, exit 2, with nothing written; a viewer is refused.
func TestMessageEditLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, "")
		m := e.fxMsg
		id, rev := string(m.Ref.ID), string(m.Revision)
		edit := []string{"message", "edit", id, "--revision", rev, "--text", "hello, corrected"}

		// The preview.
		var plan planJSON
		e.json(&plan, append(edit, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "message:"+id ||
			!strings.Contains(plan.Effects[0].Detail, "at its provider") || !strings.Contains(plan.Effects[0].Detail, "applied only when the provider confirms") {
			t.Fatalf("edit preview effects = %+v", plan.Effects)
		}
		for k, want := range map[string]string{
			"audience": `"public"`, "text": `"hello, corrected"`, "expected_revision": `"` + rev + `"`,
			"adapter": `"` + string(adapterstest.EffectsType) + `"`,
		} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		for _, want := range []string{"edit declared by " + string(adapterstest.EffectsType), "live availability not checked", "heo channel check " + string(e.fx.Ref.ID) + " --remote"} {
			if !strings.Contains(capLine, want) {
				t.Errorf("capability line %q lacks %q", capLine, want)
			}
		}
		r := e.heo(0, append(edit, "--dry-run")...)
		for _, want := range []string{"capability", "audience", "public", "message"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text preview lacks %q:\n%s", want, r.stdout)
			}
		}
		start := e.ledger()
		e.heo(0, append(edit, "--dry-run")...)
		if n := e.ledger(); n != start || len(e.appliedLines()) != 0 {
			t.Fatalf("--dry-run left %d operations, was %d; provider saw %v", n, start, e.appliedLines())
		}

		// The edit: queued durably with the revision, never applied.
		var out, again providerOpJSON
		e.json(&out, append(edit, "--idempotency-key", "edit-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.Message != m.Ref.ID ||
			out.OperationKey != "edit-1" || out.Visibility != model.Public {
			t.Fatalf("edit = %+v", out)
		}
		e.json(&again, append(edit, "--idempotency-key", "edit-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed edit = %+v, first %+v", again, out)
		}
		d := e.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered)
		if d.State == model.Failed {
			t.Fatalf("edit failed: %+v", d.Failure)
		}
		// The key finds the operation, as for every heo write.
		if byKey := e.operation("--key", "edit-1"); byKey.ID != out.Operation.ID || byKey.Kind != model.ActionMessageEdit {
			t.Errorf("operation get --key edit-1 = %+v, want %s", byKey, out.Operation.ID)
		}
		got := e.message(m)
		if desk.Text(got.Content) != "hello, corrected" || got.LastEdit == nil || got.Revision == m.Revision {
			t.Fatalf("message after the edit = %+v", got)
		}
		if lines := e.appliedLines(); len(lines) != 1 || lines[0] != "edit pm-fx-1 hello, corrected" {
			t.Fatalf("provider was asked %q, want one edit of pm-fx-1", lines)
		}
		// The text output says the edit is queued, not applied.
		txt := e.heo(0, "message", "edit", id, "--revision", string(got.Revision), "--text", "hello, again", "--idempotency-key", "edit-2")
		if !strings.Contains(txt.stdout, "queued") || !strings.Contains(txt.stderr, "queued: accepted durably") ||
			!strings.Contains(txt.stderr, "only once the provider applies it") || !strings.Contains(txt.stdout, "operation key") {
			t.Errorf("text edit:\nstdout: %s\nstderr: %s", txt.stdout, txt.stderr)
		}
		// The provider's line is its call, not the end of the operation: the
		// message takes the new text when the operation settles.
		e.waitDelivery(e.operation("--key", "edit-2").ID, model.ProviderAccepted, model.Delivered)
		e.eventually("the second edit applied", func() bool { return len(e.appliedLines()) == 2 })

		// The provider's echo of the edit comes back through ingress: not
		// a duplicate, and no second change.
		if dup := e.injectReceipt(e.fx, adapterstest.Inject{
			Event: "ev-echo-1", Message: "pm-fx-1", Kind: model.EnvelopeMessageEdited, From: "ada", Destination: "fx-1", Text: "hello, again",
		}); dup {
			t.Error("the echo of our own edit was taken as a duplicate")
		}
		var msgs pageJSON[model.Message]
		e.json(&msgs, "message", "list", "--inbox", "fxin")
		if len(msgs.Items) != 1 || desk.Text(msgs.Items[0].Content) != "hello, again" {
			t.Fatalf("messages after the echo = %+v, want the one edited message", msgs.Items)
		}

		// A stale revision: conflict, naming the current one, nothing queued.
		before := e.ledger()
		stale := e.heo(4, "message", "edit", id, "--revision", rev, "--text", "too late")
		wantStderr(t, stale, "CONFLICT", "message "+id+" is at revision "+string(e.message(m).Revision), "not "+rev)
		wantStderr(t, e.heo(4, "message", "edit", id, "--revision", rev, "--text", "too late", "--dry-run"), "CONFLICT", "is at revision")
		if n := e.ledger(); n != before {
			t.Errorf("a stale edit queued %d operations", n-before)
		}

		// An adapter that does not declare edit: refused at the preview
		// and on submission, nothing written.
		pe := []string{"message", "edit", string(e.plMsg.Ref.ID), "--revision", string(e.plMsg.Revision), "--text", "nope"}
		wantStderr(t, e.heo(2, append(pe, "--dry-run")...), "UNSUPPORTED", "does not declare edit", "heo channel get "+string(e.plain.Ref.ID))
		wantStderr(t, e.heo(2, append(pe, "--idempotency-key", "edit-plain")...), "UNSUPPORTED")
		if n := e.ledger(); n != before || len(e.appliedLines()) != 2 {
			t.Errorf("an undeclared edit wrote: operations %d, was %d; provider %v", n, before, e.appliedLines())
		}
		// A blank text and a message that is not there.
		wantStderr(t, e.heo(2, "message", "edit", id, "--revision", rev, "--text", " "), "VALIDATION", "text")
		wantStderr(t, e.heo(3, "message", "edit", "msg_404", "--revision", rev, "--text", "x"), "NOT_FOUND")

		// A viewer may not edit, preview included.
		e.stop(e.d)
		e.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		e.d = e.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, e.s.run(nil, append([]string{"message", "edit", id, "--revision", string(e.message(m).Revision), "--text", "denied"}, extra...)...), "heo.authz.not_granted")
		}
		if len(e.appliedLines()) != 2 {
			t.Errorf("provider saw %v", e.appliedLines())
		}
		e.stop(e.d)
	})
}

func (e *effectsSite) eventually(what string, cond func() bool) {
	e.t.Helper()
	eventually(e.t, what, cond)
}

// TestMessageDeleteLifecycle: a delete is destructive-shared, so the
// terminal asks for confirmation; the preview says it leaves a
// tombstone and is no privacy redaction and writes nothing; the delete
// is queued durably with the revision always sent, applied by the
// provider, and the message is a tombstone afterwards; an echo of the
// deletion through ingress is no duplicate and changes nothing more; a
// stale revision conflicts; an undeclared adapter is refused at the
// preview and on submission, exit 2, nothing written.
func TestMessageDeleteLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, noRateLimit)
		m := e.fxMsg
		id, rev := string(m.Ref.ID), string(m.Revision)
		del := []string{"message", "delete", id, "--revision", rev}

		var plan planJSON
		e.json(&plan, append(del, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "delete" || plan.Effects[0].Target != "message:"+id ||
			!strings.Contains(plan.Effects[0].Detail, "tombstone") || !strings.Contains(plan.Effects[0].Detail, "not a privacy redaction") {
			t.Fatalf("delete preview effects = %+v", plan.Effects)
		}
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if !strings.Contains(capLine, "delete declared by "+string(adapterstest.EffectsType)) || !strings.Contains(capLine, "live availability not checked") {
			t.Errorf("capability line = %q", capLine)
		}
		start := e.ledger()
		e.heo(0, append(del, "--dry-run")...)
		if e.ledger() != start {
			t.Fatal("--dry-run queued a delete")
		}

		// The terminal asks for confirmation; refused, nothing is sent.
		r := e.s.run(nil, append(del, "--format", "json")...)
		e.s.want(r, 5, del...)
		if e.ledger() != start || len(e.appliedLines()) != 0 {
			t.Fatal("an unconfirmed delete wrote")
		}

		var out providerOpJSON
		e.json(&out, append(del, "--confirm=yes", "--idempotency-key", "del-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Message != m.Ref.ID || out.OperationKey != "del-1" {
			t.Fatalf("delete = %+v", out)
		}
		d := e.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered)
		if d.State == model.Failed {
			t.Fatalf("delete failed: %+v", d.Failure)
		}
		got := e.message(m)
		if !got.Deleted || got.Redacted || desk.Text(got.Content) != "" {
			t.Fatalf("message after the delete = %+v, want a tombstone", got)
		}
		if lines := e.appliedLines(); len(lines) != 1 || lines[0] != "delete pm-fx-1" {
			t.Fatalf("provider was asked %q", lines)
		}
		txt := e.heo(0, "message", "get", id)
		if !strings.Contains(txt.stdout, "deleted") {
			t.Errorf("message get does not say it is deleted:\n%s", txt.stdout)
		}

		// The echo is no duplicate.
		if dup := e.injectReceipt(e.fx, adapterstest.Inject{
			Event: "ev-del-echo", Message: "pm-fx-1", Kind: model.EnvelopeMessageDeleted, From: "ada", Destination: "fx-1",
		}); dup {
			t.Error("the echo of our own delete was taken as a duplicate")
		}
		if got := e.message(m); !got.Deleted {
			t.Errorf("message after the echo = %+v", got)
		}

		// A deleted message cannot be deleted or edited again; a stale
		// revision conflicts.
		wantStderr(t, e.heo(4, append(del, "--confirm=yes", "--dry-run")...), "CONFLICT")
		wantStderr(t, e.heo(4, "message", "edit", id, "--revision", string(got.Revision), "--text", "x", "--dry-run"), "CONFLICT", "is deleted")
		wantStderr(t, e.heo(4, append(del, "--confirm=yes")...), "CONFLICT")

		// An adapter that does not declare delete.
		afterDelete := e.ledger()
		pd := []string{"message", "delete", string(e.plMsg.Ref.ID), "--revision", string(e.plMsg.Revision)}
		wantStderr(t, e.heo(2, append(pd, "--dry-run")...), "UNSUPPORTED", "does not declare delete")
		wantStderr(t, e.heo(2, append(pd, "--confirm=yes")...), "UNSUPPORTED")
		if n := e.ledger(); n != afterDelete || len(e.appliedLines()) != 1 {
			t.Errorf("an undeclared delete wrote: operations %d, was %d; provider %v", n, afterDelete, e.appliedLines())
		}
		if got := e.message(e.plMsg); got.Deleted {
			t.Error("the undeclared channel's message was deleted")
		}
		e.stop(e.d)
	})
}

// TestProviderVerbsOnEverySurface: the provider verbs are in the tree on
// every surface, whatever the channels are (discovery is per command,
// never per target); the delete, destructive-shared, is withheld from
// REST, rpc and MCP until the operator names the surface, while the
// socket runs it; discovery carries each verb's marker.
func TestProviderVerbsOnEverySurface(t *testing.T) {
	withE2EBinary(t, func() {
		s := newSandbox(t)
		x := startSurfaces(t, s)
		defer func() { s.want(result{exit: x.d.term()}, 0) }()
		token := x.auth.token(me())

		code, doc := x.rest(t, http.MethodGet, "/v1/commands", token, "")
		if code != http.StatusOK {
			t.Fatalf("discovery = %d", code)
		}
		entries := map[string]map[string]any{}
		cmds, _ := doc["commands"].([]any)
		for _, c := range cmds {
			e, _ := c.(map[string]any)
			name, _ := e["name"].(string)
			entries[name] = e
		}
		m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
		tools := m.tools(t)
		for name, tool := range map[string]string{
			"message edit": "message.edit", "message delete": "message.delete", "reaction add": "reaction.add",
			"reaction remove": "reaction.remove", "reaction list": "reaction.list", "participant list": "participant.list",
			"conversation status": "conversation.status", "conversation update": "conversation.update",
			"participant add": "participant.add", "receipt send": "receipt.send",
		} {
			e := entries[name]
			if e == nil {
				t.Errorf("REST discovery lacks %s", name)
				continue
			}
			if name == "message delete" || name == "participant remove" {
				if e["invocable"] != false || e["reason"] != "unauthorized-destructive" || tools[tool] {
					t.Errorf("message delete: REST %v, MCP tool listed %v; want withheld", e, tools[tool])
				}
				continue
			}
			if e["invocable"] != true || !tools[tool] {
				t.Errorf("%s: REST %v, MCP tool listed %v; want served", name, e, tools[tool])
			}
		}
		// A removal reaches a third party at the provider: destructive-shared,
		// withheld like the delete. An import holds the daemon's one served
		// call for as long as the provider takes: withheld from every
		// served surface and left on the socket.
		for name, tool := range map[string]string{"participant remove": "participant.remove", "history fetch": "history.fetch"} {
			e := entries[name]
			if e == nil || e["invocable"] != false || tools[tool] {
				t.Errorf("%s: REST %v, MCP tool listed %v; want withheld", name, e, tools[tool])
			}
		}
		if e := entries["history fetch"]; e != nil && e["reason"] == "unauthorized-destructive" {
			t.Errorf("history fetch is withheld as destructive, not as a long call: %v", e)
		}
		if code, body := x.rest(t, http.MethodPost, "/v1/commands/message/delete/msg_1", token, `{"flags":{"revision":"r","confirm":"yes"}}`); code < 400 {
			t.Errorf("REST ran a destructive delete: %d %v", code, body)
		}
		if code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"message", "delete"}, "args": []string{"msg_1"},
			"flags": map[string]any{"revision": "r", "confirm": "yes"}}); code < 400 || !strings.Contains(strings.ToLower(asString(body)), "permission_denied") {
			t.Errorf("rpc delete = %d %v", code, body)
		}
		// The socket reaches the command: it is refused for want of a
		// runtime, not for its tier.
		r := sockCall(t, x.d.addr("socket"), map[string]any{"path": []string{"message", "delete"}, "args": []string{"msg_1"},
			"flags": map[string]any{"revision": "r", "confirm": "yes"}})
		if strings.Contains(asString(r), "unauthorized-destructive") || strings.Contains(strings.ToLower(asString(r)), "not_invocable") {
			t.Errorf("socket withholds the delete: %v", r)
		}
	})
}

type reactionListJSON struct {
	Message   model.ID
	Complete  bool
	Reactions []model.MessageReaction
}

func (e *effectsSite) reactions(m model.Message) reactionListJSON {
	e.t.Helper()
	var l reactionListJSON
	e.json(&l, "reaction", "list", "--message", string(m.Ref.ID))
	return l
}

// TestReactionLifecycle: reactions list, add and remove on a fake
// provider that declares react. A list says plainly when its set is not
// the provider's authoritative one; an add is previewed, queued
// durably, applied, and listed with who made it; the provider's own
// reaction, arriving through ingress, is listed beside it by identity;
// a remove clears ours. An adapter that does not declare react is
// refused, exit 2, at the preview and on submission with nothing
// written; a viewer lists but may not add; a malformed emoji is a usage
// error.
func TestReactionLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, "")
		m := e.fxMsg
		id := string(m.Ref.ID)

		// A fresh message: no reactions, and uma does not claim to know
		// them all.
		if l := e.reactions(m); l.Message != m.Ref.ID || len(l.Reactions) != 0 || l.Complete {
			t.Fatalf("fresh reactions = %+v, want none and complete false", l)
		}
		r := e.heo(0, "reaction", "list", "--message", id)
		if !strings.Contains(r.stderr, "complete: false") {
			t.Errorf("text list does not say it is incomplete:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
		}

		add := []string{"reaction", "add", "--message", id, "👍"}
		var plan planJSON
		e.json(&plan, append(add, "--dry-run")...)
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if len(plan.Effects) != 1 || plan.Effects[0].Target != "message:"+id || string(plan.Args["emoji"]) != `"👍"` ||
			!strings.Contains(capLine, "react declared by "+string(adapterstest.EffectsType)) || !strings.Contains(plan.Effects[0].Detail, "applied only when the provider confirms") {
			t.Fatalf("add preview = %+v capability %q", plan, capLine)
		}
		start := e.ledger()
		e.heo(0, append(add, "--dry-run")...)
		if e.ledger() != start {
			t.Fatal("--dry-run queued a reaction")
		}

		var out, again reactJSON
		e.json(&out, append(add, "--idempotency-key", "react-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Remove || out.Emoji != "👍" || out.OperationKey != "react-1" {
			t.Fatalf("add = %+v", out)
		}
		e.json(&again, append(add, "--idempotency-key", "react-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed add = %+v", again)
		}
		if d := e.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("add failed: %+v", d.Failure)
		}
		l := e.reactions(m)
		if len(l.Reactions) != 1 || l.Reactions[0].Emoji != "👍" || l.Reactions[0].Actor == "" || l.Reactions[0].Count != 1 {
			t.Fatalf("reactions after the add = %+v", l)
		}
		if lines := e.appliedLines(); len(lines) != 1 || lines[0] != "react pm-fx-1 👍" {
			t.Fatalf("provider was asked %q", lines)
		}
		txt := e.heo(0, "reaction", "list", "--message", id)
		if !strings.Contains(txt.stdout, "👍") || !strings.Contains(txt.stderr, "complete: false") {
			t.Errorf("text list after the add:\nstdout: %s\nstderr: %s", txt.stdout, txt.stderr)
		}

		// A reaction from the provider's side comes in through ingress.
		e.inject(e.fx, adapterstest.Inject{Event: "ev-react-1", Message: "pm-fx-1", Kind: model.EnvelopeReactionAdded, From: "ada", Destination: "fx-1", Text: "🎉"})
		l = e.reactions(m)
		if len(l.Reactions) != 2 {
			t.Fatalf("reactions after ada's = %+v, want two", l)
		}
		var theirs *model.MessageReaction
		for i := range l.Reactions {
			if l.Reactions[i].Emoji == "🎉" {
				theirs = &l.Reactions[i]
			}
		}
		if theirs == nil || theirs.By == nil || theirs.Actor != "" {
			t.Fatalf("ada's reaction = %+v, want one attributed to her identity", theirs)
		}

		// Remove ours: the provider is asked, and the list drops it.
		var rm reactJSON
		e.json(&rm, "reaction", "remove", "--message", id, "👍")
		if !rm.Remove || rm.Operation.State != model.Queued {
			t.Fatalf("remove = %+v", rm)
		}
		if d := e.waitDelivery(rm.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("remove failed: %+v", d.Failure)
		}
		l = e.reactions(m)
		if len(l.Reactions) != 1 || l.Reactions[0].Emoji != "🎉" {
			t.Fatalf("reactions after the remove = %+v, want ada's alone", l)
		}
		if lines := e.appliedLines(); len(lines) != 2 || lines[1] != "unreact pm-fx-1 👍" {
			t.Fatalf("provider was asked %q", lines)
		}

		// Refusals.
		wantStderr(t, e.heo(2, "reaction", "add", "--message", id, "two words"), "VALIDATION", "emoji")
		wantStderr(t, e.heo(3, "reaction", "add", "--message", "msg_404", "👍"), "NOT_FOUND")
		undeclared := []string{"reaction", "add", "--message", string(e.plMsg.Ref.ID), "👍"}
		before := e.ledger()
		wantStderr(t, e.heo(2, append(undeclared, "--dry-run")...), "UNSUPPORTED", "does not declare react", "heo channel get "+string(e.plain.Ref.ID))
		wantStderr(t, e.heo(2, append(undeclared, "--idempotency-key", "react-plain")...), "UNSUPPORTED")
		wantStderr(t, e.heo(2, "reaction", "remove", "--message", string(e.plMsg.Ref.ID), "👍"), "UNSUPPORTED")
		if e.ledger() != before || len(e.appliedLines()) != 2 {
			t.Errorf("an undeclared reaction wrote: operations %d, was %d; provider %v", e.ledger(), before, e.appliedLines())
		}
		// Listing needs no capability: the plain channel lists too.
		if l := e.reactions(e.plMsg); len(l.Reactions) != 0 {
			t.Errorf("plain reactions = %+v", l)
		}

		// A viewer lists but may not react, preview included.
		e.stop(e.d)
		e.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		e.d = e.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, e.s.run(nil, append([]string{"reaction", "add", "--message", id, "👍"}, extra...)...), "heo.authz.not_granted")
		}
		if l := e.reactions(m); len(l.Reactions) != 1 {
			t.Errorf("viewer reactions = %+v", l)
		}
		e.stop(e.d)
		e.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
		e.d = e.start()
		if l := e.reactions(m); len(l.Reactions) != 1 {
			t.Errorf("external reactions = %+v", l)
		}
		e.stop(e.d)
	})
}

type participantsJSON struct {
	Conversation model.ID
	Participants []struct {
		Identity, Role, Assurance string
		Contact                   model.ID
	}
}

// TestParticipantList: a conversation's participants, with the
// identity's assurance where the caller may read identities; every role
// that reads the conversation lists them.
func TestParticipantList(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, "")
		conv := string(e.fxMsg.Ref.Conversation.ID)
		var idents pageJSON[model.Identity]
		e.json(&idents, "identity", "list", "--inbox", "fxin")
		if len(idents.Items) != 1 {
			t.Fatalf("identities = %+v", idents.Items)
		}
		ada := idents.Items[0]

		var p participantsJSON
		e.json(&p, "participant", "list", "--conversation", conv)
		if p.Conversation != e.fxMsg.Ref.Conversation.ID || len(p.Participants) != 1 || p.Participants[0].Identity != string(ada.Ref.ID) ||
			p.Participants[0].Role == "" || p.Participants[0].Assurance != ada.Assurance || p.Participants[0].Assurance == "" ||
			p.Participants[0].Contact != ada.Contact.ID {
			t.Fatalf("participants = %+v, identity %+v", p, ada)
		}
		r := e.heo(0, "participant", "list", "--conversation", conv)
		for _, want := range []string{"IDENTITY", "ROLE", "ASSURANCE", string(ada.Ref.ID), ada.Assurance} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text list lacks %q:\n%s", want, r.stdout)
			}
		}
		wantStderr(t, e.heo(3, "participant", "list", "--conversation", "conv_404"), "NOT_FOUND")

		// A viewer lists, an external actor too, without the identity.
		e.stop(e.d)
		e.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		e.d = e.start()
		e.json(&p, "participant", "list", "--conversation", conv)
		if len(p.Participants) != 1 || p.Participants[0].Assurance != ada.Assurance {
			t.Errorf("viewer participants = %+v", p)
		}
		e.stop(e.d)
		e.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [external]\n")
		e.d = e.start()
		e.json(&p, "participant", "list", "--conversation", conv)
		if len(p.Participants) != 1 || p.Participants[0].Assurance != "" || p.Participants[0].Contact != "" {
			t.Errorf("external participants = %+v, want the identity without its assurance", p)
		}
		e.stop(e.d)
	})
}

// TestProviderVerbRefusedAfterAcceptance: a verb whose capability the
// adapter declares passes the preview and uma's submission even when
// the provider no longer allows it; uma checks again at the attempt and
// the failure is the operation's state, read with `delivery get` (exit
// 0), never the command's exit: a withdrawn permission fails it as
// unauthorized, a capability the adapter's configuration switched off
// as unsupported, and neither is retried.
func TestProviderVerbRefusedAfterAcceptance(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		ch := c.fakeOf(adapterstest.EffectsType, "fx", adapterstest.Config{Account: "acct-1",
			Disabled: []model.Capability{model.CapabilityReact}, Withdrawn: []model.Capability{model.CapabilityEdit}})
		c.heo(0, "inbox", "create", "--channel", "fx", "--key", "fxin", "--destination", "fx-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		c.inject(ch, adapterstest.Inject{Event: "ev-1", Message: "pm-1", From: "ada", Destination: "fx-1", Text: "hello"})
		var msgs pageJSON[model.Message]
		c.json(&msgs, "message", "list", "--inbox", "fxin")
		m := msgs.Items[0]
		id := string(m.Ref.ID)

		// Previews pass: the adapter declares both.
		c.heo(0, "reaction", "add", "--message", id, "👍", "--dry-run")
		c.heo(0, "message", "edit", id, "--revision", string(m.Revision), "--text", "x", "--dry-run")

		var react reactJSON
		c.json(&react, "reaction", "add", "--message", id, "👍")
		if react.Operation.State != model.Queued {
			t.Fatalf("react = %+v, want it queued", react)
		}
		dr := c.waitDelivery(react.Operation.ID, model.Failed, model.Rejected, model.DeadLettered)
		if dr.Failure == nil || dr.Failure.Code != model.Unsupported {
			t.Fatalf("react on a disabled capability = %s %+v, want failed as unsupported", dr.State, dr.Failure)
		}

		var edit providerOpJSON
		c.json(&edit, "message", "edit", id, "--revision", string(m.Revision), "--text", "x")
		de := c.waitDelivery(edit.Operation.ID, model.Failed, model.Rejected, model.DeadLettered)
		if de.Failure == nil || de.Failure.Code != model.Unauthorized {
			t.Fatalf("edit without permission = %s %+v, want failed as unauthorized", de.State, de.Failure)
		}
		c.stop(d)
	})
}

// TestWidgetProviderVerbs runs the provider verbs against a maintained
// adapter that declares them, the release build's native.webwidget: a
// reply to a visitor is edited, reacted to and deleted through heo, and
// the visitor's event stream carries each change. No provider is
// reached, uma being the widget's provider; adapters that reach a real
// one are recorded in cassettes elsewhere.
func TestWidgetProviderVerbs(t *testing.T) {
	site := newWidgetSite(t, "")
	d := site.start()
	token, conv := site.visit()
	run := func(exit int, args ...string) result {
		t.Helper()
		r := site.s.run(nil, args...)
		site.s.want(r, exit, args...)
		return r
	}
	asJSON := func(v any, args ...string) {
		t.Helper()
		r := run(0, append(args, "--format", "json")...)
		if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
			t.Fatalf("heo %v: %v\n%s", args, err, r.stdout)
		}
	}
	live := widgetEvents(t, site.route, token, "")

	var sent replyJSON
	asJSON(&sent, "message", "reply", string(conv.ID), "--text", "on it")
	if ev := next(t, live); ev.name != widget.EventMessage || !strings.Contains(ev.data, "on it") {
		t.Fatalf("reply event = %+v", ev)
	}
	// The event is the adapter's call, not the end of the operation: the
	// message revision the edit must name is the one the reply settles on.
	site.s.settleAccepted(d, sent.Operation.ID)
	var msg model.Message
	asJSON(&msg, "message", "get", string(sent.Message))

	// The preview names the widget as the adapter that declares edit.
	var plan planJSON
	asJSON(&plan, "message", "edit", string(msg.Ref.ID), "--revision", string(msg.Revision), "--text", "on it, now", "--dry-run")
	var capLine string
	_ = json.Unmarshal(plan.Args["capability"], &capLine)
	if !strings.Contains(capLine, "edit declared by "+string(widget.Type)) {
		t.Fatalf("capability line = %q", capLine)
	}

	var edit providerOpJSON
	asJSON(&edit, "message", "edit", string(msg.Ref.ID), "--revision", string(msg.Revision), "--text", "on it, now")
	if ev := next(t, live); ev.name != widget.EventMessageEdit || !strings.Contains(ev.data, "on it, now") {
		t.Fatalf("edit event = %+v", ev)
	}
	site.s.settleAccepted(d, edit.Operation.ID)
	asJSON(&msg, "message", "get", string(sent.Message))
	if desk.Text(msg.Content) != "on it, now" {
		t.Fatalf("message after the edit = %q", desk.Text(msg.Content))
	}

	var react reactJSON
	asJSON(&react, "reaction", "add", "--message", string(msg.Ref.ID), "👍")
	if ev := next(t, live); ev.name != widget.EventReaction || !strings.Contains(ev.data, "👍") {
		t.Fatalf("reaction event = %+v", ev)
	}
	site.s.settleAccepted(d, react.Operation.ID)
	var rl reactionListJSON
	asJSON(&rl, "reaction", "list", "--message", string(msg.Ref.ID))
	if len(rl.Reactions) != 1 || rl.Reactions[0].Emoji != "👍" {
		t.Fatalf("reactions = %+v", rl)
	}

	asJSON(&msg, "message", "get", string(sent.Message))
	var del providerOpJSON
	asJSON(&del, "message", "delete", string(msg.Ref.ID), "--revision", string(msg.Revision), "--confirm=yes")
	if ev := next(t, live); ev.name != widget.EventMessageDelete {
		t.Fatalf("delete event = %+v", ev)
	}
	site.s.settleAccepted(d, del.Operation.ID)
	asJSON(&msg, "message", "get", string(sent.Message))
	if !msg.Deleted {
		t.Fatalf("message after the delete = %+v", msg)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}
