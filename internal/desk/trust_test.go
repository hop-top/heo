package desk_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// allowAll lets uma decide held operations and read trust, which heo's
// policy does not name yet.
type allowAll struct{}

func (allowAll) Authorize(context.Context, policy.Authorization) (policy.Decision, error) {
	return policy.Decision{Allow: true, PolicyVersion: "test"}, nil
}

func ladder(cfg trustcfg.Config, read func(ch model.ChannelRef, key string) (model.TrustEntry, error)) desk.TrustPolicy {
	return desk.TrustPolicy{Config: cfg,
		Channel: func(_ context.Context, inbox string) (model.ChannelRef, error) {
			return model.ChannelRef{Tenant: "t", ID: model.ID("ch-" + inbox)}, nil
		},
		Trust: func(_ context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
			return read(ch, key)
		}}
}

func TestTrustPolicyReadsTheKeyOfTheWrite(t *testing.T) {
	cfg := trustcfg.Defaults()
	var gotKey string
	var gotCh model.ChannelRef
	p := ladder(cfg, func(ch model.ChannelRef, key string) (model.TrustEntry, error) {
		gotKey, gotCh = key, ch
		return model.TrustEntry{Clean: 3}, nil
	})
	d, err := p.Decide(t.Context(), desk.HoldQuery{Proposer: "plugin:triage", OnBehalfOf: "uid:1001", Channel: "support", Verb: "tag", Class: "Finance"})
	want, _ := desk.TrustKey("plugin:triage", "uid:1001", "support", "tag", "Finance")
	if err != nil || d.Hold || gotKey != want || gotCh.ID != "ch-support" {
		t.Fatalf("decide = %+v, %v; key %q channel %v", d, err, gotKey, gotCh)
	}
}

func TestTrustPolicyCounts(t *testing.T) {
	cases := []struct {
		name     string
		entry    model.TrustEntry
		err      error
		q        desk.HoldQuery
		wantHold bool
		reason   string
	}{
		{"no counter is training", model.TrustEntry{}, model.NewError(model.NotFound, "x"), desk.HoldQuery{Verb: "state", Class: "read", Channel: "i"}, true, "training"},
		{"clean at threshold is automatic", model.TrustEntry{Clean: 3}, nil, desk.HoldQuery{Verb: "state", Class: "read", Channel: "i"}, false, ""},
		{"reset counter is training again", model.TrustEntry{Clean: 0, Approvals: 9, Corrections: 1}, nil, desk.HoldQuery{Verb: "state", Class: "read", Channel: "i"}, true, "training"},
		{"counter unreadable holds", model.TrustEntry{}, errors.New("boom"), desk.HoldQuery{Verb: "state", Class: "read", Channel: "i"}, true, "unreadable"},
		{"always-held fact wins over trust", model.TrustEntry{Clean: 99}, nil,
			desk.HoldQuery{Verb: "state", Class: "read", Channel: "i", Facts: desk.HoldFacts{CarriedOver: true}}, true, "carried"},
		{"added cc fact wins over trust", model.TrustEntry{Clean: 99}, nil,
			desk.HoldQuery{Verb: "reply", Class: "draft", Channel: "i", Facts: desk.HoldFacts{AddedCC: true}}, true, "cc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ladder(trustcfg.Defaults(), func(model.ChannelRef, string) (model.TrustEntry, error) { return c.entry, c.err })
			d, err := p.Decide(t.Context(), c.q)
			if err != nil || d.Hold != c.wantHold || !strings.Contains(d.Reason, c.reason) {
				t.Fatalf("decide = %+v, %v", d, err)
			}
		})
	}
}

func TestTrustPolicySeedAndSupervised(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Seed = []string{"read", "unread", "archive"}
	notFound := func(model.ChannelRef, string) (model.TrustEntry, error) {
		return model.TrustEntry{}, model.NewError(model.NotFound, "x")
	}
	for class, wantHold := range map[string]bool{"read": false, "unread": false, "archive": false, "trash": true, "star": true} {
		d, _ := ladder(cfg, notFound).Decide(t.Context(), desk.HoldQuery{OnBehalfOf: "uid:1", Channel: "i", Verb: "state", Class: class})
		if d.Hold != wantHold {
			t.Fatalf("seeded new person, %s: %+v", class, d)
		}
	}
	cfg.Supervised.Enabled = true
	d, _ := ladder(cfg, notFound).Decide(t.Context(), desk.HoldQuery{OnBehalfOf: "uid:1", Channel: "i", Verb: "state", Class: "read"})
	if !d.Hold || !strings.Contains(d.Reason, "supervised") {
		t.Fatalf("supervised: %+v", d)
	}
}

// TestTrustLadderOverUma: uma counts, the policy reads. A write is held
// until uma has counted enough clean approvals under its trust key.
func TestTrustLadderOverUma(t *testing.T) {
	w := newWorld(t, func(c *uma.Config) { c.Authorizer = allowAll{} })
	ctx := t.Context()
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)
	inbox := w.local.Ref.Inbox
	cfg := trustcfg.Defaults()
	cfg.Thresholds["edit:message"] = 1
	op := w.client(operator)
	policy := desk.TrustPolicy{Config: cfg,
		Channel: func(ctx context.Context, in string) (model.ChannelRef, error) {
			i, err := w.as(plugin).FindInbox(ctx, in)
			return i.Channel, err
		},
		Trust: func(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
			return op.Operations().Trust(ctx, model.TrustLookup{Call: model.Call{Correlation: desk.Correlation}, Channel: ch, Key: key})
		}}
	s, _ := holding(w.as(plugin), policy)

	propose := func(opID model.OperationID) (*desk.Pending, model.Acceptance, error) {
		ep, err := s.PlanEdit(ctx, id, conv, m.Revision, "new "+string(opID), opID, channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		h, err := s.Hold(ctx, &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
			Channel: string(inbox.ID), Target: m.Ref.Ref(), Preview: map[string]any{"effects": []string{"edit"}}})
		if err != nil {
			t.Fatal(err)
		}
		acc, err := s.Edit(ctx, ep)
		return h, acc, err
	}
	h, acc, err := propose("t-1")
	if h == nil {
		t.Fatalf("first write not held (training): %v", err)
	}
	if acc, err = h.Settle(ctx, acc, err); err != nil || acc.Operation.State != model.Held {
		t.Fatalf("first = %v, %v", acc.Operation.State, err)
	}
	if _, err := op.Operations().Approve(ctx, model.DecideHeld{Call: model.Call{Correlation: desk.Correlation}, Operation: acc.Operation.Ref, ExpectedRevision: acc.Operation.Revision}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if h, _, _ = propose("t-2"); h != nil {
		t.Fatal("second write held after one clean approval at threshold 1")
	}
}

// TestTrustPolicyNeverAutoCeiling: a class under never_auto is held at any
// count, with the reason a batch approval accepts; the other classes keep
// earning trust.
func TestTrustPolicyNeverAutoCeiling(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = false
	cfg.NeverAuto = []string{"trash", "spam"}
	p := ladder(cfg, func(model.ChannelRef, string) (model.TrustEntry, error) { return model.TrustEntry{Clean: 99}, nil })
	for class, wantHold := range map[string]bool{"trash": true, "spam": true, "archive": false} {
		d, err := p.Decide(t.Context(), desk.HoldQuery{Verb: "state", Class: class, Channel: "i"})
		if err != nil || d.Hold != wantHold {
			t.Fatalf("%s at 99: %+v, %v", class, d, err)
		}
		if wantHold && !strings.HasPrefix(d.Reason, trustcfg.NeverAutoReasonPrefix) {
			t.Fatalf("%s reason = %q", class, d.Reason)
		}
	}
}

// examplePolicy is the documented example for a supervised personal
// inbox: a tenant's configuration, not heo's default.
func examplePolicy() trustcfg.Config {
	c := trustcfg.Defaults()
	c.Unattended = false
	for k, n := range map[string]int{"react:message": 3, "archive": 3, "status": 5, "edit:message": 15, "delete:message": 15, "reply:send": 8} {
		c.Thresholds[k] = n
	}
	return c
}

// TestChatAndTrackerClassesEarnAsConfigured: for each class of the
// example policy, uma's real counters (on the fake provider's inbox) hold a
// plugin's write one clean approval short of the configured threshold and
// let it through at it.
func TestChatAndTrackerClassesEarnAsConfigured(t *testing.T) {
	cases := []struct {
		name, verb, class string
		need              int64
	}{
		{"reaction", "react", "message", 3},
		{"conversation archive", "state", "archive", 3},
		{"tracker status", "status", "done", 5},
		{"own message edit", "edit", "message", 15},
		{"own message delete", "delete", "message", 15},
		{"reply send", "reply", "send", 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, func(cfg *uma.Config) { cfg.Authorizer = allowAll{} })
			ctx := t.Context()
			in, err := w.as(plugin).FindInbox(ctx, "local")
			if err != nil {
				t.Fatal(err)
			}
			ops := w.client(operator).Operations()
			policy := func(cfg trustcfg.Config) desk.TrustPolicy {
				return desk.TrustPolicy{Config: cfg,
					Channel: func(context.Context, string) (model.ChannelRef, error) { return in.Channel, nil },
					Trust: func(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
						return ops.Trust(ctx, model.TrustLookup{Call: model.Call{Correlation: desk.Correlation}, Channel: ch, Key: key})
					}}
			}
			key, labels := desk.TrustKey(plugin, "uid:1001", string(in.Ref.ID), c.verb, c.class)
			q := desk.HoldQuery{Proposer: plugin, OnBehalfOf: "uid:1001", Channel: string(in.Ref.ID), Verb: c.verb, Class: c.class}
			seed := func(clean int64) {
				t.Helper()
				if _, err := ops.ImportTrust(ctx, model.ImportTrust{Call: model.Call{Correlation: desk.Correlation}, Channel: in.Channel,
					Key: key, Labels: labels, Clean: clean}); err != nil {
					t.Fatal(err)
				}
			}
			seed(c.need - 1)
			if d, _ := policy(examplePolicy()).Decide(ctx, q); !d.Hold || !strings.Contains(d.Reason, "training") {
				t.Fatalf("one short of %d: %+v", c.need, d)
			}
			seed(c.need)
			if d, _ := policy(examplePolicy()).Decide(ctx, q); d.Hold {
				t.Fatalf("at %d: %+v", c.need, d)
			}
		})
	}
}
