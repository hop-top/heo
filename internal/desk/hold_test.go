package desk_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// holding turns s's hold point on and returns the events it emitted.
func holding(s desk.Service, p desk.HoldPolicy) (desk.Service, *[]desk.HeldCreated) {
	var got []desk.HeldCreated
	s.Holds = &desk.Holds{Policy: p, Expiry: time.Hour, OnBehalfOf: "uid:1001",
		Emit: func(_ context.Context, e desk.HeldCreated) { got = append(got, e) }}
	return s, &got
}

type passAll struct{}

func (passAll) Decide(context.Context, desk.HoldQuery) (desk.HoldDecision, error) {
	return desk.HoldDecision{}, nil
}

func TestTrustKey(t *testing.T) {
	key, labels := desk.TrustKey("plugin:triage", "uid:1001", "support", "label", "Finance")
	if key != "plugin:triage/uid:1001/support/label:Finance" {
		t.Fatalf("key = %q", key)
	}
	if labels["proposer"] != "plugin:triage" || labels["on_behalf_of"] != "uid:1001" || labels["channel"] != "support" ||
		labels["verb"] != "label" || labels["class"] != "Finance" {
		t.Fatalf("labels = %v", labels)
	}
	// Empty parts are dashes; a slash in a part never splits the key.
	if key, _ := desk.TrustKey("plugin:a", "", "", "reply", ""); key != "plugin:a/-/-/reply" {
		t.Fatalf("empty parts: %q", key)
	}
	if a, _ := desk.TrustKey("p", "", "x/y", "v", "c"); strings.Count(a, "/") != 3 {
		t.Fatalf("slash split the key: %q", a)
	}
	// Past uma's bound the key keeps a head and a hash: long keys differ.
	long := strings.Repeat("é", 200)
	k1, l1 := desk.TrustKey("plugin:a", "", "", "tag", long+"1")
	k2, _ := desk.TrustKey("plugin:a", "", "", "tag", long+"2")
	if len(k1) > model.MaxTrustKeyBytes || k1 == k2 || model.ValidateTrustKey(k1) != nil {
		t.Fatalf("long keys: %d bytes, equal=%v", len(k1), k1 == k2)
	}
	if err := model.ValidateTrustLabels(l1); err != nil {
		t.Fatalf("labels: %v", err)
	}
}

// TestHoldEditDeleteReactHeld: the verbs uma can hold are stored held
// under the proposal heo built: preview, trust key, reason, expiry, and
// announced once; a person's own write is queued.
func TestHoldEditDeleteReactHeld(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)
	inbox := string(w.local.Ref.Inbox.ID)
	s, events := holding(w.as(plugin), nil)

	ep, err := s.PlanEdit(ctx, id, conv, m.Revision, "new", "h-edit", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Hold(ctx, &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
		Channel: inbox, Target: m.Ref.Ref(), Preview: map[string]any{"effects": []string{"edit"}}})
	if err != nil || h == nil {
		t.Fatalf("hold = %v, %v", h, err)
	}
	acc, err := s.Edit(ctx, ep)
	if acc, err = h.Settle(ctx, acc, err); err != nil || acc.Operation.State != model.Held {
		t.Fatalf("edit = %v, %v", acc.Operation.State, err)
	}
	v, err := s.GetHeld(ctx, string(acc.Operation.Ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	wantKey := "plugin:triage/uid:1001/" + inbox + "/edit:message"
	if v.Proposer != "plugin:triage" || v.OnBehalfOf != "uid:1001" || v.TrustKey != wantKey || v.TrustLabels["verb"] != "edit" ||
		string(v.Preview) != `{"effects":["edit"]}` || v.Reason == "" || v.ExpiresAt == nil || !v.ExpiresAt.Equal(w.clock.Now().Add(time.Hour)) {
		t.Fatalf("held view = %+v", v)
	}
	if len(*events) != 1 || (*events)[0].Operation != string(acc.Operation.Ref.ID) || (*events)[0].TrustKey != wantKey ||
		(*events)[0].Kind != model.ActionMessageEdit || (*events)[0].Status != "held" {
		t.Fatalf("events = %+v", *events)
	}
	// A replay is the same held operation and announces nothing.
	h, _ = s.Hold(ctx, &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
		Channel: inbox, Target: m.Ref.Ref(), Preview: map[string]any{"effects": []string{"edit"}}})
	again, err := s.Edit(ctx, ep)
	if again, err = h.Settle(ctx, again, err); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref || len(*events) != 1 {
		t.Fatalf("replay = %+v, %v, events %d", again, err, len(*events))
	}

	dp, err := s.PlanDelete(ctx, id, conv, m.Revision, "h-del", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err = s.Hold(ctx, &dp.Request.Command, desk.HoldInput{Action: model.ActionMessageDelete, Verb: "delete", Class: "message", Channel: inbox, Preview: "p"})
	if err != nil {
		t.Fatal(err)
	}
	dacc, err := s.Delete(ctx, dp)
	if dacc, err = h.Settle(ctx, dacc, err); err != nil || dacc.Operation.State != model.Held {
		t.Fatalf("delete = %v, %v", dacc.Operation.State, err)
	}

	rp, err := s.PlanReact(ctx, id, conv, "👍", false, "", "h-react", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err = s.Hold(ctx, &rp.Request.Command, desk.HoldInput{Action: model.ActionMessageReact, Verb: "react", Class: "message", Channel: inbox, Preview: "p"})
	if err != nil {
		t.Fatal(err)
	}
	racc, err := s.React(ctx, rp)
	if racc, err = h.Settle(ctx, racc, err); err != nil || racc.Operation.State != model.Held {
		t.Fatalf("react = %v, %v", racc.Operation.State, err)
	}
}

// TestHoldPersonNeverHeld: a person's write is not offered to the
// policy and is queued, holding on.
func TestHoldPersonNeverHeld(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m := w.firstMessage()
	s, events := holding(w.as(agent), nil)
	p, err := s.PlanEdit(ctx, string(m.Ref.ID), string(w.local.Ref.ID), m.Revision, "new", "p-edit", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Preview: "p"})
	if err != nil || h != nil || p.Request.Hold != nil {
		t.Fatalf("hold = %v, %v, proposal %v", h, err, p.Request.Hold)
	}
	acc, err := s.Edit(ctx, p)
	if acc, err = h.Settle(ctx, acc, err); err != nil || acc.Operation.State != model.Queued || len(*events) != 0 {
		t.Fatalf("edit = %v, %v", acc.Operation.State, err)
	}
}

// TestHoldOffAndPass: holding off, or a policy that passes, queues a
// plugin's write as before.
func TestHoldOffAndPass(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)
	off := w.as(plugin)
	pass, events := holding(w.as(plugin), passAll{})
	for i, s := range []desk.Service{off, pass} {
		p, err := s.PlanReact(ctx, id, conv, "👍", false, "", model.OperationID([]string{"o-off", "o-pass"}[i]), channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionMessageReact, Verb: "react", Preview: "p"})
		if err != nil || h != nil {
			t.Fatalf("service %d: hold = %v, %v", i, h, err)
		}
		acc, err := s.React(ctx, p)
		if err != nil || acc.Operation.State != model.Queued {
			t.Fatalf("service %d: react = %v, %v", i, acc.Operation.State, err)
		}
	}
	if len(*events) != 0 {
		t.Fatalf("events = %+v", *events)
	}
}

// TestHoldRefusesWithoutPreview: a plugin write whose verb offers no
// preview is refused while holding is on.
func TestHoldRefusesWithoutPreview(t *testing.T) {
	w := newWorld(t)
	s, _ := holding(w.as(plugin), nil)
	_, err := s.Hold(t.Context(), &model.Command{}, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit"})
	wantCode(t, err, model.Unsupported, "no --dry-run preview")
}

// TestHoldUnlistedActionIsCapabilityError: an action heo has not
// probed on uma is refused as a capability error before anything is
// submitted, never queued, applied or held silently.
func TestHoldUnlistedActionIsCapabilityError(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s, events := holding(w.as(plugin), nil)
	cmd := &model.Command{}
	h, err := s.Hold(ctx, cmd, desk.HoldInput{Action: model.ActionContactWrite, Verb: "contact", Class: "create", Preview: "p"})
	if h != nil || cmd.Hold != nil {
		t.Fatal("proposal set for an action heo cannot hold")
	}
	wantCode(t, err, model.Unsupported, "cannot be held yet")
	if len(*events) != 0 {
		t.Fatalf("events = %+v", *events)
	}
	for _, a := range []string{model.ActionMessageReply, model.ActionMessageNote, model.ActionConversationStart,
		model.ActionConversationLabel, model.ActionConversationTransition, model.ActionLabelConfigure} {
		if !desk.CanHold(a) {
			t.Errorf("%s is probed and should be holdable", a)
		}
	}
}

// TestHoldSettleRefusesAppliedWrite: a write uma committed instead of
// holding is an error, not a pass.
func TestHoldSettleRefusesAppliedWrite(t *testing.T) {
	w := newWorld(t)
	m := w.firstMessage()
	s, _ := holding(w.as(plugin), nil)
	p, err := s.PlanReact(t.Context(), string(m.Ref.ID), string(w.local.Ref.ID), "👍", false, "", "s-react", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Hold(t.Context(), &p.Request.Command, desk.HoldInput{Action: model.ActionMessageReact, Verb: "react", Preview: "p"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Settle(t.Context(), model.Acceptance{Operation: model.Operation{State: model.Queued}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not held") {
		t.Fatalf("settle = %v", err)
	}
}
