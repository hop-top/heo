package desk_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma"
	"hop.top/uma/model"
)

// optInWorld is a world whose engine opts the local writes heo holds
// into uma's holding, as the daemon does.
func optInWorld(t *testing.T, opts ...func(*uma.Config)) *world {
	return newWorld(t, append([]func(*uma.Config){func(c *uma.Config) {
		c.Held = uma.HeldPolicy{LabelConfigure: true, ConversationTransition: true}
	}}, opts...)...)
}

// proposal is a hold proposal set directly on a command: it probes what
// uma does with one, whatever heo's holdable table says.
func proposal() *model.HoldProposal {
	return &model.HoldProposal{
		Reason: "probe", TrustKey: "probe/key",
		Preview: &model.Extension{Name: desk.PreviewExtension, Version: 1, Data: []byte(`{"p":1}`)},
	}
}

// holdVerb is one write verb, proposed through heo's hold point.
// propose plans, offers the write to the hold point and submits it as s;
// snap reads the local effect an applied write would change.
type holdVerb struct {
	name string
	// class is the trust threshold class of the verb's writes.
	class   string
	propose func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error)
	snap    func(t *testing.T, w *world) string
}

func settle(h *desk.Pending, acc model.Acceptance, err error) (model.Acceptance, error) {
	return h.Settle(context.Background(), acc, err)
}

func holdVerbs(t *testing.T, w *world) []holdVerb {
	ctx := t.Context()
	l := w.label("finance")
	inboxOf := func(c model.Conversation) string { return string(c.Ref.Inbox.ID) }
	return []holdVerb{
		{"reply", "reply:send", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanReply(ctx, string(w.local.Ref.ID), "hi "+key, model.OperationID(key))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionMessageReply, Verb: "reply", Class: "send",
				Channel: inboxOf(w.local), Target: w.local.Ref.Ref(), Facts: p.Facts, Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			acc, err := s.Reply(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string { return msgSnap(t, w, w.local, false) }},
		{"note local", "note:local", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanNote(ctx, string(w.local.Ref.ID), "n "+key, desk.PlacementLocal, model.OperationID(key))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionMessageNote, Verb: "note", Class: "local",
				Channel: inboxOf(w.local), Target: w.local.Ref.Ref(), Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			acc, err := s.AddNote(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string { return msgSnap(t, w, w.local, true) }},
		{"note provider", "note:provider", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanNote(ctx, string(w.remote.Ref.ID), "n "+key, desk.PlacementProvider, model.OperationID(key))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionMessageNote, Verb: "note", Class: "provider",
				Channel: inboxOf(w.remote), Target: w.remote.Ref.Ref(), Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			acc, err := s.AddNote(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string { return msgSnap(t, w, w.remote, true) }},
		{"label add", "label:finance", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanLabeling(ctx, string(w.local.Ref.ID), []string{l.Name}, false, model.OperationID(key))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionConversationLabel, Verb: "label", Class: l.Name,
				Channel: inboxOf(w.local), Target: w.local.Ref.Ref(), Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			_, acc, err := s.ApplyLabeling(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string { return convSnap(t, w, w.local) }},
		{"label remove", "label:finance", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanLabeling(ctx, string(w.remote.Ref.ID), []string{l.Name}, true, model.OperationID(key))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionConversationLabel, Verb: "unlabel", Class: l.Name,
				Channel: inboxOf(w.remote), Target: w.remote.Ref.Ref(), Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			_, acc, err := s.ApplyLabeling(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string { return convSnap(t, w, w.remote) }},
		{"label create", "label-create:bill", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanLabelCreate(ctx, desk.LabelChange{Name: "bill-" + key}, model.OperationID(key), channels.Submit)
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionLabelConfigure, Verb: "label-create", Class: "bill",
				Target: model.Ref{Tenant: s.Tenant, Kind: model.KindLabel}, Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			_, acc, err := s.CreateLabel(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string {
			p, err := w.as(agent).ListLabels(t.Context(), model.PageRequest{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var n []string
			for _, x := range p.Items {
				n = append(n, x.Name)
			}
			sort.Strings(n)
			return strings.Join(n, ",")
		}},
		{"state closed", "state:closed", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			cur, _ := w.as(agent).FindConversation(ctx, string(w.local.Ref.ID))
			p, err := s.PlanTransition(ctx, string(w.local.Ref.ID), cur.Revision, "closed", model.OperationID(key), channels.Submit)
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionConversationTransition, Verb: "state", Class: "closed",
				Channel: inboxOf(w.local), Target: w.local.Ref.Ref(), Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			_, acc, err := s.Transition(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string { return convSnap(t, w, w.local) }},
		{"start", "start", func(t *testing.T, w *world, s desk.Service, key string) (model.Acceptance, error) {
			p, err := s.PlanStart(ctx, "local", "user:cy", "welcome "+key, model.OperationID(key))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionConversationStart, Verb: "start",
				Channel: string(p.Inbox.Ref.ID), Target: p.Inbox.Ref.Ref(), Preview: "p"})
			if err != nil {
				return model.Acceptance{}, err
			}
			acc, err := s.Start(ctx, p)
			return settle(h, acc, err)
		}, func(t *testing.T, w *world) string {
			p, err := w.as(agent).ListConversations(t.Context(), desk.Filter{}, model.PageRequest{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, c := range p.Items {
				m, _ := w.as(agent).ListMessages(t.Context(), desk.Filter{Conversation: string(c.Ref.ID)}, model.PageRequest{Limit: 100})
				n += 1000 + len(m.Items)
			}
			return fmt.Sprint(len(p.Items), n)
		}},
	}
}

var holdVerbNames = []string{"reply", "note local", "note provider", "label add", "label remove", "label create", "state closed", "start"}

// verbWorld is a world for verb i: label remove starts with the label
// on its conversation.
func verbWorld(t *testing.T, i int, opts ...func(*uma.Config)) (*world, holdVerb) {
	w := optInWorld(t, opts...)
	v := holdVerbs(t, w)[i]
	if v.name != holdVerbNames[i] {
		t.Fatalf("verb order: %s", v.name)
	}
	if v.name == "label remove" {
		w.labelConv(w.remote, false, "finance")
	}
	return w, v
}

// TestHeldVerbs: for each verb heo holds, a plugin's write is stored held
// with nothing written (probed on uma's store); approve applies it
// exactly once, a second approve is a replay; reject applies nothing.
func TestHeldVerbs(t *testing.T) {
	for i, name := range holdVerbNames {
		t.Run(name, func(t *testing.T) {
			w, v := verbWorld(t, i)
			plug, events := holding(w.as(plugin), nil)
			dec := w.as(agent)
			before := v.snap(t, w)

			acc, err := v.propose(t, w, plug, "rj-"+name)
			if err != nil || acc.Operation.State != model.Held {
				t.Fatalf("hold = %v, %v", acc.Operation.State, err)
			}
			if len(*events) != 1 {
				t.Fatalf("events = %+v", *events)
			}
			if got := v.snap(t, w); got != before {
				t.Fatalf("held write changed state: %q -> %q", before, got)
			}
			r, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideReject})
			if err != nil || r.Status != string(model.Rejected) {
				t.Fatalf("reject = %+v, %v", r, err)
			}
			w.drain()
			if got := v.snap(t, w); got != before {
				t.Fatalf("rejected write changed state: %q -> %q", before, got)
			}

			acc, err = v.propose(t, w, plug, "ap-"+name)
			if err != nil || acc.Operation.State != model.Held {
				t.Fatalf("hold = %v, %v", acc.Operation.State, err)
			}
			a, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove})
			if err != nil || a.Replayed {
				t.Fatalf("approve = %+v, %v", a, err)
			}
			w.drain()
			once := v.snap(t, w)
			if once == before {
				t.Fatalf("approved write applied nothing: %q", once)
			}
			again, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove})
			if err != nil || !again.Replayed {
				t.Fatalf("second approve = %+v, %v", again, err)
			}
			w.drain()
			if got := v.snap(t, w); got != once {
				t.Fatalf("second approve applied again: %q -> %q", once, got)
			}
		})
	}
}

// TestHeldVerbsGraduate: the trust ladder holds a verb's class until uma
// has counted its threshold of clean approvals under the write's trust
// key, then lets the write through.
func TestHeldVerbsGraduate(t *testing.T) {
	for i, name := range holdVerbNames {
		t.Run(name, func(t *testing.T) {
			w, v := verbWorld(t, i, func(c *uma.Config) { c.Authorizer = allowAll{} })
			op := w.client(operator)
			cfg := trustcfg.Defaults()
			cfg.Unattended = false
			cfg.Thresholds[v.class] = 2
			pol := desk.TrustPolicy{Config: cfg,
				Channel: func(ctx context.Context, in string) (model.ChannelRef, error) {
					i, err := w.as(plugin).FindInbox(ctx, in)
					return i.Channel, err
				},
				Trust: func(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
					return op.Operations().Trust(ctx, model.TrustLookup{Call: model.Call{Correlation: desk.Correlation}, Channel: ch, Key: key})
				}}
			plug, _ := holding(w.as(plugin), pol)
			dec := w.as(agent)
			for n := 1; n <= 2; n++ {
				acc, err := v.propose(t, w, plug, fmt.Sprintf("g-%d", n))
				if err != nil || acc.Operation.State != model.Held {
					t.Fatalf("write %d = %v, %v: want held while training", n, acc.Operation.State, err)
				}
				if _, err := dec.DecideHeld(t.Context(), desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil {
					t.Fatalf("approve %d: %v", n, err)
				}
			}
			acc, err := v.propose(t, w, plug, "g-3")
			if name == "label create" {
				// A label definition touches no mailbox, so uma has no
				// channel to count under: it is held every time.
				if err != nil || acc.Operation.State != model.Held {
					t.Fatalf("label create after threshold = %v, %v: want held (no mailbox to earn trust on)", acc.Operation.State, err)
				}
				return
			}
			if err != nil || acc.Operation.State == model.Held {
				t.Fatalf("write after threshold = %v, %v: want it through", acc.Operation.State, err)
			}
		})
	}
}

// TestHeldFactsForceHold: a reply that adds recipients, or whose
// original names a Reply-To other than its From, is held although its
// class has graduated; so is an unattended send.
func TestHeldFactsForceHold(t *testing.T) {
	w := optInWorld(t)
	ctx := t.Context()
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.Thresholds["reply:send"] = 0
	pol := desk.TrustPolicy{Config: cfg}
	for _, tc := range []struct {
		name  string
		facts desk.HoldFacts
		unatt bool
		held  bool
	}{
		{"none", desk.HoldFacts{}, false, false},
		{"adds cc", desk.HoldFacts{AddedCC: true}, false, true},
		{"reply-to differs", desk.HoldFacts{ReplyToDiffers: true}, false, true},
		{"carried over", desk.HoldFacts{CarriedOver: true}, false, true},
		{"unattended", desk.HoldFacts{}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pol.Config.Unattended = tc.unatt
			s, _ := holding(w.as(plugin), pol)
			p, err := s.PlanReply(ctx, string(w.local.Ref.ID), "hi", model.OperationID("f-"+tc.name))
			if err != nil {
				t.Fatal(err)
			}
			h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionMessageReply, Verb: "reply", Class: "send",
				Channel: string(w.local.Ref.Inbox.ID), Target: w.local.Ref.Ref(), Facts: tc.facts, Preview: "p"})
			if err != nil || (h != nil) != tc.held {
				t.Fatalf("hold = %v, %v; want held %v", h, err, tc.held)
			}
		})
	}
}

// TestReplyToDiffers: the fact is read from the Reply-To a mail adapter
// recorded with the last inbound message.
func TestReplyToDiffers(t *testing.T) {
	msg := func(from, payload string) model.Message {
		m := model.Message{Audience: model.Audience{Visibility: model.Public}}
		m.Author.External.ID = from
		if payload != "" {
			m.Metadata = []model.Extension{{Name: "google.message", Version: 1, Data: []byte(payload)}}
		}
		return m
	}
	for _, tc := range []struct {
		name string
		m    model.Message
		want bool
	}{
		{"no reply-to", msg("ada@example.com", `{"subject":"x"}`), false},
		{"no payload", msg("ada@example.com", ""), false},
		{"same address", msg("ada@example.com", `{"reply_to":["Ada <ADA@example.com>"]}`), false},
		{"other address", msg("ada@example.com", `{"reply_to":["bob@evil.test"]}`), true},
		{"imap address objects", msg("ada@example.com", `{"reply_to":[{"name":"Bob","address":"bob@evil.test"}]}`), true},
		{"imap same", msg("ada@example.com", `{"reply_to":[{"address":"ada@example.com"}]}`), false},
		{"one of many differs", msg("ada@example.com", `{"reply_to":["ada@example.com","bob@evil.test"]}`), true},
	} {
		if got := desk.ReplyToDiffers(tc.m); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPlanReplyFactsWhileHolding: a reply plan reads the facts only
// while holding is on; with no Reply-To on record it states none.
func TestPlanReplyFactsWhileHolding(t *testing.T) {
	w := optInWorld(t)
	s, _ := holding(w.as(plugin), nil)
	p, err := s.PlanReply(t.Context(), string(w.local.Ref.ID), "hi", "pf-1")
	if err != nil || p.Facts != (desk.HoldFacts{}) {
		t.Fatalf("facts = %+v, %v", p.Facts, err)
	}
}

// TestHeldTag: a mailbox tag change is stored held with no provider work
// queued; approve queues it exactly once, reject queues nothing.
func TestHeldTag(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	plug, events := holding(w.asKnowing(plugin), nil)
	dec := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	propose := func(key string) model.Acceptance {
		tag := desk.MailTag{Add: []string{"Label_12"}}
		p, err := plug.PlanTag(ctx, string(gm.ID), tag, model.OperationID(key), channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		h, err := plug.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionConversationLabel, Verb: "tag", Class: "Label_12",
			Channel: string(gm.Inbox.ID), Target: gm.Ref(), Preview: "p"})
		if err != nil {
			t.Fatal(err)
		}
		_, acc, err := plug.TagConversation(ctx, p)
		if acc, err = h.Settle(ctx, acc, err); err != nil {
			t.Fatal(err)
		}
		return acc
	}
	before := w.queued()
	acc := propose("tag-rj")
	if acc.Operation.State != model.Held || len(*events) != 1 || w.queued() != before {
		t.Fatalf("hold = %v, events %d, queued %d", acc.Operation.State, len(*events), w.queued()-before)
	}
	if r, err := dec.DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideReject}); err != nil || r.Status != string(model.Rejected) {
		t.Fatalf("reject = %+v, %v", r, err)
	}
	if w.queued() != before {
		t.Fatalf("rejected tag queued %d", w.queued()-before)
	}
	acc = propose("tag-ap")
	if r, err := dec.DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil || r.Replayed {
		t.Fatalf("approve = %+v, %v", r, err)
	}
	if r, err := dec.DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil || !r.Replayed {
		t.Fatalf("second approve = %+v, %v", r, err)
	}
	if w.queued() != before+1 {
		t.Fatalf("approved tag queued %d, want exactly 1", w.queued()-before)
	}
}

// drain runs the workers briefly so queued deliveries settle.
func (w *world) drain() {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(w.t.Context(), 150*time.Millisecond)
	defer cancel()
	_ = w.e.Runtime().Run(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call},
		uma.WorkerOptions{Owner: "drain", Poll: 5 * time.Millisecond, Lease: 10 * time.Second})
}

func convSnap(t *testing.T, w *world, c model.Conversation) string {
	t.Helper()
	got, err := w.as(agent).FindConversation(t.Context(), string(c.Ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	var l []string
	for _, x := range got.Labels {
		l = append(l, string(x.ID))
	}
	sort.Strings(l)
	return fmt.Sprintf("%s|%v", got.Status, l)
}

func msgSnap(t *testing.T, w *world, c model.Conversation, notes bool) string {
	t.Helper()
	s := w.as(agent)
	list := s.ListMessages
	if notes {
		list = s.ListNotes
	}
	p, err := list(t.Context(), desk.Filter{Conversation: string(c.Ref.ID)}, model.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(len(p.Items))
}
