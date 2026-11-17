package desk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// edited returns the intent of the held operation with its text replaced.
func edited(t *testing.T, v desk.HeldOperation, text string) json.RawMessage {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal(v.Intent, &in); err != nil {
		t.Fatalf("intent %s: %v", v.Intent, err)
	}
	in["content"] = map[string]any{"parts": []any{map[string]any{"kind": "text", "text": text}}}
	out, _ := json.Marshal(in)
	return out
}

func TestHeldShowsStoredIntent(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "in-1")
	v, err := w.as(agent).GetHeld(t.Context(), string(ref.ID))
	if err != nil || !bytes.Contains(v.Intent, []byte("edited in-1")) {
		t.Fatalf("intent = %s, %v; want the stored edit", v.Intent, err)
	}
}

func TestEditHeldAppliesEditedArgs(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "ed-1")
	s := w.as(agent)
	var recs []desk.DecisionRecord
	s.OnDecided = func(_ context.Context, r desk.DecisionRecord) { recs = append(recs, r) }
	v, _ := s.GetHeld(t.Context(), string(ref.ID))

	got, err := s.EditHeld(t.Context(), desk.EditRequest{ID: string(ref.ID), Args: edited(t, v, "fixed"), Note: "typo", Revision: model.Revision(v.Revision)})
	if err != nil || got.Status != string(model.Queued) || got.Decision != "edited" || got.DecidedBy != string(agent) || !got.Correction || got.Replayed {
		t.Fatalf("edit = %+v, %v", got, err)
	}
	if !bytes.Contains(got.EditedArgs, []byte("fixed")) || len(got.Edits) != 1 || !bytes.Contains(got.Edits[0].Before, []byte("edited ed-1")) {
		t.Fatalf("edited args %s, edits %+v", got.EditedArgs, got.Edits)
	}
	if len(recs) != 1 || recs[0].Decision != desk.DecideEdit || !recs[0].Correction || recs[0].Note != "typo" || recs[0].By != agent {
		t.Fatalf("records = %+v", recs)
	}
	// A second edit of a decided operation is a conflict, written nowhere.
	_, err = s.EditHeld(t.Context(), desk.EditRequest{ID: string(ref.ID), Args: edited(t, v, "again")})
	if err == nil || !model.IsCode(err, model.Conflict) || len(recs) != 1 {
		t.Fatalf("second edit = %v, records %d", err, len(recs))
	}
}

func TestEditHeldCountsACorrectionOnItsKeys(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "ck-1")
	s := w.as(agent)
	v, _ := s.GetHeld(t.Context(), string(ref.ID))
	op, _ := s.GetOperation(t.Context(), string(ref.ID))
	if _, err := s.EditHeld(t.Context(), desk.EditRequest{ID: string(ref.ID), Args: edited(t, v, "fixed"), CorrectedKeys: []string{"other/key"}}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.FindInbox(t.Context(), string(w.local.Ref.Inbox.ID))
	e, err := w.client(service).Operations().Trust(t.Context(), model.TrustLookup{Call: call, Channel: in.Channel, Key: "other/key"})
	if err != nil || e.Corrections != 1 {
		t.Fatalf("corrected key = %+v, %v", e, err)
	}
	if e, err := w.client(service).Operations().Trust(t.Context(), model.TrustLookup{Call: call, Channel: in.Channel, Key: op.Held.TrustKey}); err == nil && e.Corrections != 0 {
		t.Fatalf("the operation's own key moved though other keys were named: %+v", e)
	}
}

func TestEditHeldRefusals(t *testing.T) {
	w := newWorld(t)
	ref := holdEdit(t, w, "rf-1")
	s := w.as(agent)
	v, _ := s.GetHeld(t.Context(), string(ref.ID))
	args := edited(t, v, "fixed")
	still := func() {
		t.Helper()
		if got, err := s.GetHeld(t.Context(), string(ref.ID)); err != nil || got.Status != "held" {
			t.Fatalf("after a refused edit: %+v, %v", got, err)
		}
	}

	// A plugin never edits, whatever role it holds, and is refused before
	// anything is read.
	_, err := w.as(plugin).EditHeld(t.Context(), desk.EditRequest{ID: string(ref.ID), Args: args})
	wantCode(t, err, model.Unauthorized, "plugin")
	_, err = w.as(plugin).EditHeld(t.Context(), desk.EditRequest{ID: "no-such-op", Args: args})
	wantCode(t, err, model.Unauthorized, "plugin")
	still()

	_, err = s.EditHeld(t.Context(), desk.EditRequest{ID: string(ref.ID), Args: args, Revision: "not-it"})
	if !model.IsCode(err, model.Conflict) || desk.IsStale(err) {
		t.Fatalf("revision mismatch = %v", err)
	}
	for name, bad := range map[string]json.RawMessage{
		"empty": nil, "not json": json.RawMessage("{nope"),
		"not the stored shape": json.RawMessage(`{"message":{"id":"other"}}`),
	} {
		if _, err := s.EditHeld(t.Context(), desk.EditRequest{ID: string(ref.ID), Args: bad}); !model.IsCode(err, model.Validation) {
			t.Errorf("%s = %v, want a validation error", name, err)
		}
	}
	// Not held: not found, as for the other verbs.
	_, err = s.EditHeld(t.Context(), desk.EditRequest{ID: string(replyOp(t, s, w, "plain").ID), Args: args})
	wantCode(t, err, model.NotFound, "")
	still()
	if strings.Contains(string(v.EditedArgs), "fixed") {
		t.Fatal("edited args stored by a refused edit")
	}
}

// No corrected keys named, empty or nil, is the operation's own key: uma
// reads a non-nil empty list as "no key corrected", so heo never sends one.
func TestEditHeldDefaultsToTheOperationsOwnKey(t *testing.T) {
	for name, keys := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			op := holdClass(t, w, w.local, "dk-1", "reply", "draft", "training: reply:draft 0 of 5 clean approvals")
			s := w.as(agent)
			v, _ := s.GetHeld(t.Context(), string(op.Ref.ID))
			if _, err := s.EditHeld(t.Context(), desk.EditRequest{ID: string(op.Ref.ID), Args: replyArgs(t, v, "better"), CorrectedKeys: keys}); err != nil {
				t.Fatal(err)
			}
			if e := trustOf(t, w, w.local, op); e.Corrections != 1 {
				t.Fatalf("corrections of the operation's own key = %d, want 1", e.Corrections)
			}
		})
	}
}

// replyArgs is the intent of a held reply with its text replaced.
func replyArgs(t *testing.T, v desk.HeldOperation, text string) json.RawMessage {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal(v.Intent, &in); err != nil {
		t.Fatal(err)
	}
	in["content"] = map[string]any{"parts": []any{map[string]any{"kind": "text", "text": text}}}
	out, _ := json.Marshal(in)
	return out
}
