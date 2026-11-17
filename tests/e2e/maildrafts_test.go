package e2e

import (
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

type replyDraftJSON struct {
	Conversation model.Conversation
	Message      string
	Change       string
	Draft        string
	Effect       string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

type unsubscribeJSON struct {
	Conversation model.Conversation
	Message      string
	Method       string
	Effect       string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

// TestMessageReplyDraftLifecycle drives the e2e build on Postgres through
// a reply draft on a fake provider that ships Gmail's reply_draft effect:
// a preview names the change, says nothing is sent and writes nothing; a
// create, an update and a delete each queue durably under their key,
// replay, and reach the provider with exactly the fields named (the text
// and the draft, never a recipient); the conversation uma holds and its
// messages are untouched. An adapter that ships another effect or none, a
// conversation of an unknown ID, a usage error and a viewer are refused
// before anything is written, with the exit codes the grammar gives.
func TestMessageReplyDraftLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		dr := s.mail(s.dr, "drin", "dr-1", "ev-dr", "gmail-msg-d")
		did := string(dr.Ref.ID)

		draft := []string{"message", "reply-draft", did, "--text", "Thanks, on it."}
		var plan planJSON
		s.json(&plan, append(draft, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "create" {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"external", "not sent", "google.mail.reply_draft", peerMail, "gmail-msg-d"} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		if _, cc := plan.Args["cc"]; cc || string(plan.Args["change_kind"]) != `"create"` {
			t.Errorf("preview args = %v", plan.Args)
		}
		if line := s.capabilityLine(plan); !strings.Contains(line, "ships google.mail.reply_draft") {
			t.Errorf("capability line = %q", line)
		}
		start := s.ledger()
		s.heo(0, append(draft, "--dry-run")...)
		wantStderr(t, s.heo(2, "message", "reply-draft", did), "--text is required")
		wantStderr(t, s.heo(2, "message", "reply-draft", did, "--delete"), "needs the draft ID")
		wantStderr(t, s.heo(2, "message", "reply-draft", did, "--draft", "r-1", "--delete", "--text", "x"), "takes no --text")
		wantStderr(t, s.heo(2, append(draft, "--draft", " r-1", "--dry-run")...), "VALIDATION")
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("--dry-run or a usage error wrote: operations %d, was %d; provider saw %v", n, start, s.applied())
		}

		var out, again replyDraftJSON
		s.json(&out, append(draft, "--idempotency-key", "rd-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "rd-1" ||
			out.Effect != "google.mail.reply_draft" || out.Message != "gmail-msg-d" || out.Change != "create" || out.Conversation.Ref != dr.Ref {
			t.Fatalf("draft = %+v", out)
		}
		s.json(&again, append(draft, "--idempotency-key", "rd-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed draft = %+v, first %+v", again, out)
		}
		if d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("draft failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if lines := s.applied(); lines[0] != `effect google.mail.reply_draft gmail-msg-d `+peerMail+` {"text":"Thanks, on it."}` {
			t.Fatalf("provider was asked %q", lines)
		}
		var got model.Conversation
		s.json(&got, "conversation", "get", did)
		if got.Revision != dr.Revision {
			t.Errorf("the draft changed the conversation uma holds: %+v", got)
		}
		var msgs pageJSON[model.Message]
		s.json(&msgs, "message", "list", "--inbox", "drin")
		if len(msgs.Items) != 1 {
			t.Errorf("a draft is no message of uma's: %d messages", len(msgs.Items))
		}

		s.heo(0, "message", "reply-draft", did, "--draft", "r-77", "--text", "v2", "--idempotency-key", "rd-2")
		s.heo(0, "message", "reply-draft", did, "--draft", "r-77", "--delete", "--idempotency-key", "rd-3")
		s.wantApplied(3)
		if lines := s.applied(); lines[1] != `effect google.mail.reply_draft gmail-msg-d `+peerMail+` {"draft":"r-77","text":"v2"}` ||
			lines[2] != `effect google.mail.reply_draft gmail-msg-d `+peerMail+` {"delete":true,"draft":"r-77"}` {
			t.Errorf("provider was asked %q", lines)
		}

		// Refusals: an adapter shipping another mail effect, one shipping none.
		before := s.ledger()
		lbc := s.mail(s.lb, "lbin", "lb-1", "ev-lb", "gmail-msg-1")
		wantStderr(t, s.heo(2, "message", "reply-draft", string(lbc.Ref.ID), "--text", "x", "--dry-run"), "UNSUPPORTED", "ships no effect google.mail.reply_draft")
		wantStderr(t, s.heo(2, "message", "reply-draft", string(lbc.Ref.ID), "--text", "x", "--idempotency-key", "x-1"), "UNSUPPORTED")
		pl := s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		wantStderr(t, s.heo(2, "message", "reply-draft", string(pl.Ref.ID), "--text", "x", "--dry-run"), "UNSUPPORTED", "does not declare extension_execute")
		wantStderr(t, s.heo(3, "message", "reply-draft", "00000ad0-0000-7000-8000-0000000009ff", "--text", "x"), "NOT_FOUND")
		if n := s.ledger(); n != before || len(s.applied()) != 3 {
			t.Errorf("a refused draft wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		s.stop(s.d)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.d = s.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, s.s.run(nil, append([]string{"message", "reply-draft", did, "--text", "x"}, extra...)...), "heo.authz.not_granted")
		}
		if len(s.applied()) != 3 {
			t.Errorf("provider saw %v", s.applied())
		}
		s.stop(s.d)
	})
}

// TestConversationUnsubscribeLifecycle drives the e2e build on Postgres
// through an unsubscribe on a fake provider that ships Gmail's
// unsubscribe effect: the preview says the request is irreversible and
// states what the adapter does and refuses, and writes nothing; the real
// run queues durably under its key, replays without a second request, and
// reaches the provider with the method alone (never a URL or an
// address); the conversation uma holds is untouched. A method that is not
// one of three, an adapter that ships another effect or none, a
// conversation the peer wrote no message in, an unknown conversation and
// a viewer are refused before anything is written.
func TestConversationUnsubscribeLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		un := s.mail(s.un, "unin", "un-1", "ev-un", "gmail-msg-u")
		uid := string(un.Ref.ID)

		var plan planJSON
		s.json(&plan, "conversation", "unsubscribe", uid, "--dry-run")
		if len(plan.Effects) != 1 || plan.Effects[0].Target != "mail:message gmail-msg-u" {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"irreversible", "third party", "google.mail.unsubscribe", "method auto", "RFC 8058", "no GET"} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		if string(plan.Args["method"]) != `"auto"` || string(plan.Args["message"]) != `"gmail-msg-u"` {
			t.Errorf("preview args = %v", plan.Args)
		}
		if line := s.capabilityLine(plan); !strings.Contains(line, "ships google.mail.unsubscribe") {
			t.Errorf("capability line = %q", line)
		}
		start := s.ledger()
		s.heo(0, "conversation", "unsubscribe", uid, "--method", "mailto", "--dry-run")
		wantStderr(t, s.heo(2, "conversation", "unsubscribe", uid, "--method", "get"), "want one of auto, one_click, mailto")
		wantStderr(t, s.heo(2, "conversation", "unsubscribe", uid, "--url", "https://evil.example/unsub"), "unknown flag")
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("--dry-run or a usage error wrote: operations %d, was %d; provider saw %v", n, start, s.applied())
		}

		var out, again unsubscribeJSON
		s.json(&out, "conversation", "unsubscribe", uid, "--idempotency-key", "un-1")
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "un-1" ||
			out.Effect != "google.mail.unsubscribe" || out.Message != "gmail-msg-u" || out.Method != "auto" || out.Conversation.Ref != un.Ref {
			t.Fatalf("unsubscribe = %+v", out)
		}
		s.json(&again, "conversation", "unsubscribe", uid, "--idempotency-key", "un-1")
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed unsubscribe = %+v, first %+v", again, out)
		}
		if d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("unsubscribe failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if lines := s.applied(); lines[0] != `effect google.mail.unsubscribe gmail-msg-u `+peerMail+` {"method":"auto"}` {
			t.Fatalf("provider was asked %q", lines)
		}
		if byKey := s.operation("--key", "un-1"); byKey.ID != out.Operation.ID || byKey.Kind != model.ActionExtensionExecute {
			t.Errorf("operation get --key un-1 = %+v", byKey)
		}
		var got model.Conversation
		s.json(&got, "conversation", "get", uid)
		if got.Revision != un.Revision {
			t.Errorf("the unsubscribe changed the conversation uma holds: %+v", got)
		}
		s.heo(0, "conversation", "unsubscribe", uid, "--method", "one_click", "--idempotency-key", "un-2")
		s.wantApplied(2)
		if lines := s.applied(); lines[1] != `effect google.mail.unsubscribe gmail-msg-u `+peerMail+` {"method":"one_click"}` {
			t.Errorf("provider was asked %q", lines)
		}

		before := s.ledger()
		lbc := s.mail(s.lb, "lbin", "lb-1", "ev-lb", "gmail-msg-1")
		wantStderr(t, s.heo(2, "conversation", "unsubscribe", string(lbc.Ref.ID), "--dry-run"), "UNSUPPORTED", "ships no effect google.mail.unsubscribe")
		wantStderr(t, s.heo(2, "conversation", "unsubscribe", string(lbc.Ref.ID), "--idempotency-key", "x-1"), "UNSUPPORTED")
		pl := s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		wantStderr(t, s.heo(2, "conversation", "unsubscribe", string(pl.Ref.ID), "--dry-run"), "UNSUPPORTED", "does not declare extension_execute")
		s.inject(s.un, adapterstest.Inject{Event: "ev-un-2", Message: "ev-un-2", From: "other@example.com", Destination: "un-1", Text: "hello"})
		var all pageJSON[model.Conversation]
		s.json(&all, "conversation", "list", "--inbox", "unin")
		var bare model.Conversation
		for _, c := range all.Items {
			if c.Ref.ID != un.Ref.ID {
				bare = c
			}
		}
		if bare.Ref.ID == "" {
			t.Fatalf("no second conversation among %+v", all.Items)
		}
		wantStderr(t, s.heo(4, "conversation", "unsubscribe", string(bare.Ref.ID), "--dry-run"), "CONFLICT", "names no provider message")
		wantStderr(t, s.heo(3, "conversation", "unsubscribe", "00000ad0-0000-7000-8000-0000000009ff"), "NOT_FOUND")
		if n := s.ledger(); n != before || len(s.applied()) != 2 {
			t.Errorf("a refused unsubscribe wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		s.stop(s.d)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.d = s.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, s.s.run(nil, append([]string{"conversation", "unsubscribe", uid}, extra...)...), "heo.authz.not_granted")
		}
		if len(s.applied()) != 2 {
			t.Errorf("provider saw %v", s.applied())
		}
		s.stop(s.d)
	})
}
