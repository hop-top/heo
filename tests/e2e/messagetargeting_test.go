package e2e

import (
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

// recordIDs maps the provider message IDs of a conversation's messages to
// their record IDs, and lists the provider IDs in order.
func (s *effectSite) recordIDs(conv model.Conversation) (map[string]string, []string) {
	s.t.Helper()
	var p pageJSON[model.Message]
	s.json(&p, "message", "list", "--conversation", string(conv.Ref.ID))
	byProvider, order := map[string]string{}, []string{}
	for _, m := range p.Items {
		for _, x := range m.External {
			if x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
				byProvider[x.ID] = string(m.Ref.ID)
				order = append(order, x.ID)
			}
		}
	}
	return byProvider, order
}

// peerMsg has the peer write one message the provider names gm.
func (s *effectSite) peerMsg(ch model.Channel, dest, event, gm string) {
	s.t.Helper()
	s.inject(ch, adapterstest.Inject{Event: event, Message: gm, From: peerMail, Destination: dest, Text: "hello " + event, Parent: gm})
}

// TestStateVerbsTargetNamedMessages reproduces a lock-to-single inbox: the
// peer's A1, A2 (a reply) and B1 (unrelated) are one conversation, so a
// state verb with no --message acts on all three. With --message it acts
// on exactly the messages named, a name that is not a message of the
// conversation is a usage error that applies nothing, and a preview lists
// the messages it resolves to.
func TestStateVerbsTargetNamedMessages(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		s.peerMsg(s.st, "st-1", "ev-a1", "gm-a1")
		s.peerMsg(s.st, "st-1", "ev-a2", "gm-a2")
		s.peerMsg(s.st, "st-1", "ev-b1", "gm-b1")
		var p pageJSON[model.Conversation]
		s.json(&p, "conversation", "list", "--inbox", "stin")
		if len(p.Items) != 1 {
			t.Fatalf("conversations = %d, want the peer's mail in one", len(p.Items))
		}
		conv := p.Items[0]
		cid := string(conv.Ref.ID)
		rec, order := s.recordIDs(conv)
		if len(order) != 3 {
			t.Fatalf("provider messages = %v, want a1, a2 and b1", order)
		}
		a1, a2, b1 := rec["gm-a1"], rec["gm-a2"], rec["gm-b1"]

		// A preview lists what the change resolves to.
		var plan planJSON
		s.json(&plan, "conversation", "archive", cid, "--dry-run")
		items := string(plan.Args["items"])
		for _, want := range []string{a1, a2, b1, "gm-a1", "gm-a2", "gm-b1"} {
			if !strings.Contains(items, want) {
				t.Errorf("preview items lack %s: %s", want, items)
			}
		}
		if !strings.Contains(plan.Effects[0].Detail, "all 3 provider message") {
			t.Errorf("preview detail = %s", plan.Effects[0].Detail)
		}
		s.json(&plan, "conversation", "archive", cid, "--message", a2, "--dry-run")
		items = string(plan.Args["items"])
		if !strings.Contains(items, "gm-a2") || strings.Contains(items, "gm-a1") || strings.Contains(items, "gm-b1") ||
			!strings.Contains(plan.Effects[0].Detail, "limited to the 1 message(s)") {
			t.Errorf("targeted preview items = %s, detail = %s", items, plan.Effects[0].Detail)
		}
		if string(plan.Args["change"]) != `"archive on message `+a2+`"` {
			t.Errorf("targeted preview change = %s", plan.Args["change"])
		}

		// A name that is not a message of the conversation applies nothing.
		before := s.ledger()
		other := s.mail(s.sf, "sfin", "sf-1", "ev-sf", "ev-sf")
		foreign, _ := s.recordIDs(other)
		for _, bad := range []string{"00000ad0-0000-7000-8000-0000000009ff", "nope", foreign["ev-sf"]} {
			for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "bad-" + strings.ReplaceAll(bad[:4], " ", "")}} {
				r := s.heo(2, append([]string{"conversation", "archive", cid, "--message", a1, "--message", bad}, extra...)...)
				wantStderr(t, r, "--message "+bad, "not a message of conversation")
			}
		}
		wantStderr(t, s.heo(2, "conversation", "archive", cid, "--message", ""), "--message")
		if n := s.ledger(); n != before || len(s.applied()) != 0 {
			t.Fatalf("a refused --message wrote: operations %d, was %d; provider saw %v", n, before, s.applied())
		}

		// --message acts on exactly the messages named, for every verb.
		for i, c := range []struct {
			args []string
			want string
		}{
			{[]string{"archive", "--message", a2}, "state archive"},
			{[]string{"read", "--message", b1, "--message", a1}, "state read"},
			{[]string{"flag", "--message", a2}, "state flag"},
			{[]string{"trash", "--message", a2}, "state trash"},
			{[]string{"move", "--role", "junk", "--message", a1}, "state move"},
		} {
			args := append([]string{"conversation", c.args[0], cid}, c.args[1:]...)
			s.heo(0, append(args, "--idempotency-key", "t-"+c.args[0])...)
			s.wantApplied(i + 1)
		}
		want := []string{
			"state archive", " items=gm-a2",
			"state read", " items=gm-a1,gm-b1",
			"state flag", " items=gm-a2",
			"state trash", " items=gm-a2",
			"state move", " items=gm-a1",
		}
		got := s.applied()
		for i := 0; i < len(want); i += 2 {
			line := got[i/2]
			if !strings.HasPrefix(line, want[i]+" ") || !strings.Contains(line, want[i+1]) || strings.Count(line, "gm-") != strings.Count(want[i+1], "gm-") {
				t.Errorf("provider was asked %q, want %s with%s only", line, want[i], want[i+1])
			}
		}

		// No --message: unchanged, the whole conversation (no named items).
		s.heo(0, "conversation", "unarchive", cid, "--idempotency-key", "t-whole")
		s.wantApplied(6)
		if last := s.applied()[5]; !strings.HasPrefix(last, "state unarchive ") || strings.Contains(last, "items=") {
			t.Errorf("an untargeted change = %q, want the whole conversation, no items", last)
		}
		// The set is part of the operation: another set under the same key conflicts.
		s.heo(4, "conversation", "archive", cid, "--message", a1, "--idempotency-key", "t-archive")
	})
}

// TestTagAndUnsubscribeTargetNamedMessage: tag and unsubscribe act on the
// first message of the conversation by default, and on the one --message
// names, a message of the conversation, else a usage error.
func TestTagAndUnsubscribeTargetNamedMessage(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		s.peerMsg(s.lb, "lb-1", "ev-l1", "gm-l1")
		s.peerMsg(s.lb, "lb-1", "ev-l2", "gm-l2")
		s.peerMsg(s.un, "un-1", "ev-u1", "gm-u1")
		s.peerMsg(s.un, "un-1", "ev-u2", "gm-u2")
		convOf := func(inbox string) (model.Conversation, map[string]string) {
			var p pageJSON[model.Conversation]
			s.json(&p, "conversation", "list", "--inbox", inbox)
			if len(p.Items) != 1 {
				t.Fatalf("conversations in %s = %d", inbox, len(p.Items))
			}
			rec, _ := s.recordIDs(p.Items[0])
			return p.Items[0], rec
		}
		lb, lrec := convOf("lbin")
		un, urec := convOf("unin")
		lid, uid := string(lb.Ref.ID), string(un.Ref.ID)

		var plan planJSON
		s.json(&plan, "conversation", "tag", lid, "--add", "Label_9", "--dry-run")
		if string(plan.Args["message"]) != `"gm-l1"` {
			t.Errorf("default tag preview message = %s, want the first message gm-l1", plan.Args["message"])
		}
		s.json(&plan, "conversation", "tag", lid, "--add", "Label_9", "--message", lrec["gm-l2"], "--dry-run")
		if string(plan.Args["message"]) != `"gm-l2"` {
			t.Errorf("targeted tag preview message = %s, want gm-l2", plan.Args["message"])
		}
		s.json(&plan, "conversation", "unsubscribe", uid, "--message", urec["gm-u2"], "--dry-run")
		if string(plan.Args["message"]) != `"gm-u2"` {
			t.Errorf("targeted unsubscribe preview message = %s, want gm-u2", plan.Args["message"])
		}

		before := s.ledger()
		wantStderr(t, s.heo(2, "conversation", "tag", lid, "--add", "Label_9", "--message", urec["gm-u1"]), "not a message of conversation")
		wantStderr(t, s.heo(2, "conversation", "unsubscribe", uid, "--message", "nope", "--dry-run"), "not a message of conversation")
		if n := s.ledger(); n != before || len(s.applied()) != 0 {
			t.Fatalf("a refused --message wrote: operations %d, was %d; provider saw %v", n, before, s.applied())
		}

		s.heo(0, "conversation", "tag", lid, "--add", "Label_9", "--idempotency-key", "tg-def")
		s.heo(0, "conversation", "tag", lid, "--add", "Label_8", "--message", lrec["gm-l2"], "--idempotency-key", "tg-msg")
		s.heo(0, "conversation", "unsubscribe", uid, "--message", urec["gm-u2"], "--idempotency-key", "un-msg")
		s.wantApplied(3)
		got := strings.Join(s.applied(), "\n")
		for _, want := range []string{"effect google.mail.labels gm-l1 ", "effect google.mail.labels gm-l2 ", "effect google.mail.unsubscribe gm-u2 "} {
			if !strings.Contains(got, want) {
				t.Errorf("provider was asked\n%s\nwant %q", got, want)
			}
		}
	})
}
