package desk_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/kit/go/core/consent"
	"hop.top/uma/model"
)

// editArgs is the stored intent of an edit, with new text.
func editArgs(t *testing.T, w *world, text string) model.Extension {
	// The proposer's claim fixes audience, base and deadline; an edit may
	// change the content only.
	t.Helper()
	m := w.firstMessage()
	data, err := json.Marshal(map[string]any{
		"message": m.Ref, "audience": m.Audience, "base": map[string]any{"revision": m.Revision, "deleted": false, "redacted": false},
		"wait_until": w.clock.Now().UTC().Add(model.DefaultStreamDeadline),
		"content":    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: "uma.intent.edit", Version: 1, Data: data}
}

func history(t *testing.T, s desk.Service, id model.ID) []desk.HeldAuditRecord {
	t.Helper()
	recs, err := s.HeldHistory(t.Context(), string(id))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func events(recs []desk.HeldAuditRecord) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Event)
	}
	return out
}

func TestHeldAuditExpiryIsARecordNoOneDecided(t *testing.T) {
	w := heldWorld(t)
	op := mustHold(t, w, "audit-expire", "late")
	stop := sweeper(w, time.Minute)
	defer stop()
	w.clock.Advance(2 * time.Hour)
	awaitState(t, w.as(agent), op.Ref.ID, "expired")

	recs := history(t, w.as(agent), op.Ref.ID)
	last := recs[len(recs)-1]
	if got := strings.Join(events(recs), ","); got != "proposed,expired" || last.Cause != consent.CauseExpired || last.Subject != "" || last.At == nil {
		t.Fatalf("expiry = %s %+v", got, recs)
	}
}

func TestHeldAuditEditRecordsBeforeAndAfter(t *testing.T) {
	w := heldWorld(t)
	op := mustHold(t, w, "audit-edit", "draft")
	args := editArgs(t, w, "better")
	w.clock.Advance(time.Minute)
	if _, err := w.client(agent).Operations().Edit(t.Context(), model.EditHeld{DecideHeld: decideArgs(op), Args: args}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	recs := history(t, w.as(agent), op.Ref.ID)
	if got := strings.Join(events(recs), ","); got != "proposed,edited,approved" {
		t.Fatalf("events = %s", got)
	}
	p, e, a := recs[0], recs[1], recs[2]
	if p.Subject != string(plugin) || p.At == nil || !p.At.Equal(time.Unix(1_700_000_000, 0)) || string(p.Shown) != `{"text":"draft"}` ||
		p.Capability != consent.Capability(model.ActionMessageEdit) || p.TrustKey == "" || p.Operation != string(op.Ref.ID) {
		t.Fatalf("proposed = %+v", p)
	}
	if e.Subject != string(agent) || e.At == nil || !e.At.After(*p.At) ||
		!strings.Contains(string(e.Before), "draft") || !strings.Contains(string(e.After), "better") {
		t.Fatalf("edited = %+v (before %s, after %s)", e, e.Before, e.After)
	}
	if a.Subject != string(agent) || a.Decision != consent.Allow || a.Note != "looked" || a.At == nil ||
		string(a.Shown) != `{"text":"draft"}` || !strings.Contains(string(a.Applied), "better") {
		t.Fatalf("approved = %+v", a)
	}
}

func TestHeldAuditApproveAndReject(t *testing.T) {
	w := heldWorld(t)
	op := mustHold(t, w, "audit-approve", "x")
	if _, err := w.client(agent).Operations().Approve(t.Context(), decideArgs(op)); err != nil {
		t.Fatal(err)
	}
	recs := history(t, w.as(agent), op.Ref.ID)
	if got := strings.Join(events(recs), ","); got != "proposed,approved" || recs[1].Subject != string(agent) ||
		recs[1].Decision != consent.Allow || len(recs[1].Applied) != 0 {
		t.Fatalf("approve = %s %+v", got, recs)
	}

	w = heldWorld(t)
	op = mustHold(t, w, "audit-reject", "y")
	if _, err := w.client(agent).Operations().Reject(t.Context(), decideArgs(op)); err != nil {
		t.Fatal(err)
	}
	recs = history(t, w.as(agent), op.Ref.ID)
	if got := strings.Join(events(recs), ","); got != "proposed,rejected" || recs[1].Subject != string(agent) ||
		recs[1].Decision != consent.Deny || recs[1].Note != "looked" || string(recs[1].Shown) != `{"text":"y"}` {
		t.Fatalf("reject = %s %+v", got, recs)
	}
}

func TestHeldAuditReaderWithoutTargetReadSeesNothing(t *testing.T) {
	w := heldWorld(t)
	op := mustHold(t, w, "audit-hidden", "secret draft")
	if recs, err := w.as(agent).ListHeldHistory(t.Context(), desk.OperationFilter{}); err != nil || len(recs) == 0 {
		t.Fatalf("a reader sees the trail: %v, %v", recs, err)
	}
	w.gate.hide = map[model.ID]bool{w.firstMessage().Ref.ID: true}
	recs, err := w.as(agent).ListHeldHistory(t.Context(), desk.OperationFilter{})
	if err != nil || len(recs) != 0 {
		t.Fatalf("list = %+v, %v", recs, err)
	}
	_, err = w.as(agent).HeldHistory(t.Context(), string(op.Ref.ID))
	wantCode(t, err, model.NotFound, "")
}

func TestHeldAuditOfNeverHeldOperation(t *testing.T) {
	w := heldWorld(t)
	plain := replyOp(t, w.as(agent), w, "plain")
	_, err := w.as(agent).HeldHistory(t.Context(), string(plain.ID))
	wantCode(t, err, model.NotFound, "")
}
