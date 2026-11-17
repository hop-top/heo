package desk_test

import (
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// inboundConv has contact who write to the "local" inbox and returns the
// conversation that arrived.
func (w *world) inboundConv(event, who string) model.Conversation {
	w.t.Helper()
	w.inject(adapterstest.Inject{Event: event, Message: event, From: who, Destination: "local", Text: "hello from " + who})
	p, err := w.as(agent).ListConversations(w.t.Context(), desk.Filter{Inbox: "local"}, model.PageRequest{Limit: 50})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, c := range p.Items {
		for _, m := range w.messages(c.Ref) {
			if len(m.External) > 0 && strings.Contains(m.External[0].ID, event) {
				return c
			}
		}
	}
	w.t.Fatalf("no conversation for %s", event)
	return model.Conversation{}
}

func (w *world) plannedFirstReply(conv model.Conversation) bool {
	w.t.Helper()
	s, _ := holding(w.as(plugin), passAll{})
	p, err := s.PlanReply(w.t.Context(), string(conv.Ref.ID), "hi", model.OperationID("fr-"+string(conv.Ref.ID)))
	if err != nil {
		w.t.Fatal(err)
	}
	return p.Facts.FirstReplyUnknown
}

// TestFirstReplyFactIsHeos: heo computes the fact from its own records of
// what was sent to the contact on the inbox: a contact never written to is
// unknown; one the desk wrote to, in this conversation or another of the
// inbox, is not; a conversation the desk opened is not.
func TestFirstReplyFactIsHeos(t *testing.T) {
	w := optInWorld(t)
	zed := w.inboundConv("zed-1", "zed")
	if !w.plannedFirstReply(zed) {
		t.Fatal("a contact never written to was taken as known")
	}
	// A person's reply is a prior write: the next reply is not a first.
	acc, err := w.as(agent).Reply(t.Context(), mustPlan(t, w.as(agent), zed, "hello zed"))
	if err != nil {
		t.Fatal(err)
	}
	w.settle(acc.Operation.Ref)
	if w.plannedFirstReply(zed) {
		t.Fatal("a contact written to was still unknown")
	}
	// ada was started from this side (the desk opened it): known, and her
	// reply, which arrives in the same conversation, is no first reply.
	ada := w.inboundConv("ada-1", "ada")
	if ada.Ref != w.local.Ref {
		t.Fatalf("ada's message opened %s, want her conversation %s", ada.Ref.ID, w.local.Ref.ID)
	}
	if w.plannedFirstReply(ada) {
		t.Fatal("a contact the desk opened a conversation with was unknown")
	}
	// Another new contact is unknown again: the fact is per contact.
	if !w.plannedFirstReply(w.inboundConv("yan-1", "yan")) {
		t.Fatal("a second unknown contact was taken as known")
	}
}

// Off while holding is off: the plan reads nothing it does not need.
func TestFirstReplyFactOnlyWhileHolding(t *testing.T) {
	w := optInWorld(t)
	zed := w.inboundConv("zed-2", "zed")
	p, err := w.as(plugin).PlanReply(t.Context(), string(zed.Ref.ID), "hi", "nh-1")
	if err != nil || p.Facts.FirstReplyUnknown {
		t.Fatalf("plan with no hold point = %+v, %v", p.Facts, err)
	}
}

func mustPlan(t *testing.T, s desk.Service, conv model.Conversation, text string) desk.ReplyPlan {
	t.Helper()
	p, err := s.PlanReply(t.Context(), string(conv.Ref.ID), text, model.OperationID("mp-"+text))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// decideReply runs the plan's facts and a clean count through the trust
// policy of cfg for a plugin's reply on conv's inbox.
func decideReply(t *testing.T, w *world, cfg trustcfg.Config, conv model.Conversation, clean int64) desk.HoldDecision {
	t.Helper()
	s, _ := holding(w.as(plugin), passAll{})
	p, err := s.PlanReply(t.Context(), string(conv.Ref.ID), "hi", model.OperationID("dr-"+string(conv.Ref.ID)))
	if err != nil {
		t.Fatal(err)
	}
	pol := ladder(cfg, func(model.ChannelRef, string) (model.TrustEntry, error) { return model.TrustEntry{Clean: clean}, nil })
	d, err := pol.Decide(t.Context(), desk.HoldQuery{Proposer: plugin, OnBehalfOf: "uid:1001", Channel: string(conv.Ref.Inbox.ID),
		Verb: "reply", Class: "send", Facts: p.Facts})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestFirstReplyPolicyPerInbox: the default holds a first reply to an
// unknown contact whatever the count; an inbox set to allow lets it earn
// like any reply:send; a known contact is unaffected either way.
func TestFirstReplyPolicyPerInbox(t *testing.T) {
	w := optInWorld(t)
	zed := w.inboundConv("zed-3", "zed")
	ada := w.inboundConv("ada-3", "ada")
	inbox := string(zed.Ref.Inbox.ID)
	base := trustcfg.Defaults()
	base.Unattended = false

	d := decideReply(t, w, base, zed, 99)
	if !d.Hold || d.Reason != trustcfg.FirstReplyReason {
		t.Fatalf("default inbox: %+v", d)
	}
	allow := base
	allow.Inboxes = map[string]trustcfg.InboxPolicy{inbox: {FirstReply: trustcfg.FirstReplyAllow}}
	if d := decideReply(t, w, allow, zed, 7); !d.Hold || !strings.Contains(d.Reason, "training") {
		t.Fatalf("allow below the threshold: %+v", d)
	}
	if d := decideReply(t, w, allow, zed, 8); d.Hold {
		t.Fatalf("allow at the threshold: %+v", d)
	}
	other := base
	other.Inboxes = map[string]trustcfg.InboxPolicy{"some-other-inbox": {FirstReply: trustcfg.FirstReplyAllow}}
	if d := decideReply(t, w, other, zed, 99); !d.Hold || d.Reason != trustcfg.FirstReplyReason {
		t.Fatalf("another inbox's setting applied here: %+v", d)
	}
	for name, cfg := range map[string]trustcfg.Config{"default": base, "allow": allow} {
		if d := decideReply(t, w, cfg, ada, 8); d.Hold {
			t.Fatalf("known contact held under %s: %+v", name, d)
		}
	}
}
