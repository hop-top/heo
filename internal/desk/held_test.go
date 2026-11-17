package desk_test

import (
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

func TestHeldOfNeverHeldOperation(t *testing.T) {
	if _, ok := desk.HeldOf(model.Operation{State: model.Queued}); ok {
		t.Fatal("an operation without a held block mapped to a held view")
	}
}

func TestHeldOfMapsBlock(t *testing.T) {
	exp := time.Unix(1_700_003_600, 0).UTC()
	at := time.Unix(1_700_000_100, 0).UTC()
	op := model.Operation{
		Ref: model.OperationRef{Tenant: tenant, ID: "op_1"}, State: model.Held, Revision: "3",
		Key: &model.OperationKey{Caller: "plugin:triage", Kind: model.ActionMessageReply,
			Target: model.Ref{Kind: model.KindConversation, ID: "c1"}, ID: "k"},
		Held: &model.HeldBlock{
			Proposer: "plugin:triage", OnBehalfOf: "uid:1", Reason: "first contact",
			Preview: &model.Extension{Name: "p", Version: 1, Data: []byte(`{"would":"send"}`)},
			Intent:  &model.Extension{Name: "i", Version: 1, Data: []byte(`{"text":"a"}`)}, CarriedOverAt: &exp,
			TrustKey: "triage/reply", TrustLabels: map[string]string{"kind": "reply"}, ExpiresAt: &exp,
			Decision: model.HeldEdited, DecidedBy: "uid:2", DecidedAt: &at,
			EditedArgs: &model.Extension{Name: "a", Version: 1, Data: []byte(`{"text":"b"}`)},
			Edits: []model.HeldEdit{{By: "uid:2", At: at,
				Before: model.Extension{Data: []byte(`{"text":"a"}`)}, After: model.Extension{Data: []byte(`{"text":"b"}`)}}},
		},
	}
	v, ok := desk.HeldOf(op)
	if !ok || v.ID != "op_1" || v.Status != "held" || v.Kind != model.ActionMessageReply || v.Target != "conversation/c1" ||
		v.Proposer != "plugin:triage" || v.OnBehalfOf != "uid:1" || v.Reason != "first contact" ||
		string(v.Preview) != `{"would":"send"}` || string(v.Intent) != `{"text":"a"}` || v.CarriedOverAt == nil || v.TrustKey != "triage/reply" || v.TrustLabels["kind"] != "reply" ||
		v.ExpiresAt == nil || !v.ExpiresAt.Equal(exp) || v.Decision != "edited" || v.DecidedBy != "uid:2" ||
		string(v.EditedArgs) != `{"text":"b"}` || len(v.Edits) != 1 || string(v.Edits[0].After) != `{"text":"b"}` || v.Revision != "3" {
		t.Fatalf("view = %+v, %v", v, ok)
	}
}

// holdEdit has the plugin actor edit the first message of the local
// conversation as a proposal: uma stores it held.
func holdEdit(t *testing.T, w *world, key string) model.OperationRef {
	t.Helper()
	m := w.firstMessage()
	s := w.as(plugin)
	p, err := s.PlanEdit(t.Context(), string(m.Ref.ID), string(w.local.Ref.ID), m.Revision, "edited "+key, model.OperationID(key), channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	p.Request.Hold = &model.HoldProposal{
		Reason: "first contact", TrustKey: "triage/edit",
		Preview: &model.Extension{Name: desk.PreviewExtension, Version: 1, Data: []byte(`{"would":"send hi"}`)},
	}
	acc, err := s.Edit(t.Context(), p)
	if err != nil || acc.Operation.State != model.Held {
		t.Fatalf("hold: %+v, %v", acc.Operation.State, err)
	}
	return acc.Operation.Ref
}

func TestGetAndListHeld(t *testing.T) {
	w := newWorld(t)
	s := w.as(agent)
	held := holdEdit(t, w, "hold-me")
	plain := replyOp(t, s, w, "leave-me")

	v, err := s.GetHeld(t.Context(), string(held.ID))
	if err != nil || v.Status != "held" || v.Proposer != "plugin:triage" || v.Reason != "first contact" || string(v.Preview) != `{"would":"send hi"}` {
		t.Fatalf("GetHeld = %+v, %v", v, err)
	}
	_, err = s.GetHeld(t.Context(), string(plain.ID))
	wantCode(t, err, model.NotFound, "")

	p, err := s.ListHeld(t.Context(), desk.OperationFilter{}, model.PageRequest{Limit: 10})
	if err != nil || len(p.Items) != 1 || p.Items[0].ID != string(held.ID) {
		t.Fatalf("ListHeld = %+v, %v", p.Items, err)
	}
	// The held and expired states are valid filters.
	if _, err := s.ListOperations(t.Context(), desk.OperationFilter{States: []string{"held", "expired"}}, model.PageRequest{Limit: 10}); err != nil {
		t.Fatalf("state filter: %v", err)
	}
}

func replyOp(t *testing.T, s desk.Service, w *world, key string) model.OperationRef {
	t.Helper()
	p, err := s.PlanReply(t.Context(), string(w.local.Ref.ID), "hi "+key, model.OperationID(key))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.Reply(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return acc.Operation.Ref
}
