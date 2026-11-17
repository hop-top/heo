package desk_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

// verbsWorld is a world whose fake declares membership, receipt and
// history, besides edit, delete and react.
func verbsWorld(t *testing.T) *world {
	t.Helper()
	return newWorldFake(t, func(f *adapterstest.Factory) {
		f.DeclareEffects()
		f.DeclareMembership()
		f.DeclareReceipts()
		f.DeclareHistory()
	})
}

func (w *world) refetch(c model.Conversation) model.Conversation {
	w.t.Helper()
	got, err := w.as(agent).FindConversation(w.t.Context(), string(c.Ref.ID))
	if err != nil {
		w.t.Fatal(err)
	}
	return got
}

// TestParticipantPlanAndLifecycle: adding a participant is a provider
// effect under membership. A preview resolves the identity (or the
// contact's one identity), refuses an adapter that does not declare
// membership, and refuses what uma would refuse in the claiming unit
// (already a participant, not a participant, the owner); the revision
// is always sent; a submission is queued, and the participants change
// only once the provider applied it.
func TestParticipantPlanAndLifecycle(t *testing.T) {
	w := verbsWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	w.inject(adapterstest.Inject{Event: "e-cy", Message: "m-cy", From: "cy", Destination: "local", Text: "hi"})
	cy := w.identityOf("cy")
	conv := w.refetch(w.local)
	id := string(conv.Ref.ID)

	// A revision is required, and sent.
	_, err := s.PlanParticipant(ctx, id, string(cy.Ref.ID), false, "", "op-p", channels.Submit)
	wantCode(t, err, model.Validation, "revision")
	p, err := s.PlanParticipant(ctx, id, string(cy.Ref.ID), false, conv.Revision, "op-add", channels.Preview)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if p.Request.ExpectedRevision != conv.Revision || p.Request.Identity != cy.Ref || p.Request.Conversation != conv.Ref {
		t.Fatalf("request = %+v", p.Request)
	}
	if d := p.Declaration; d == nil || d.Capability != model.CapabilityMembership || d.Adapter != adapterstest.Type {
		t.Fatalf("declaration = %+v", d)
	}
	// The contact's one identity resolves too; an unknown ID is not found.
	pc, err := s.PlanParticipant(ctx, id, string(cy.Contact.ID), false, conv.Revision, "op-add", channels.Preview)
	if err != nil || pc.Identity.Ref != cy.Ref {
		t.Fatalf("by contact = %+v, %v", pc.Identity.Ref, err)
	}
	// An identity of another inbox is not one of this conversation's.
	other := w.identityOf("bob")
	_, err = s.PlanParticipant(ctx, id, string(other.Ref.ID), false, conv.Revision, "op-x", channels.Preview)
	wantCode(t, err, model.NotFound, "no identity or contact")
	_, err = s.PlanParticipant(ctx, id, "nobody", false, conv.Revision, "op-x", channels.Preview)
	wantCode(t, err, model.NotFound, "no identity or contact")
	// Removing a non-participant, in a preview, is the conflict uma
	// would answer.
	_, err = s.PlanParticipant(ctx, id, string(cy.Ref.ID), true, conv.Revision, "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "not a participant")
	// A stale revision is refused in a preview, left to uma on submission.
	_, err = s.PlanParticipant(ctx, id, string(cy.Ref.ID), false, "stale", "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision")
	stale, err := s.PlanParticipant(ctx, id, string(cy.Ref.ID), false, "stale", "op-stale", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan at a stale revision: %v", err)
	}
	_, err = s.AddParticipant(ctx, stale)
	wantCode(t, err, model.Conflict, "is at revision")

	// A submission carries no declaration: uma gates it.
	sub, err := s.PlanParticipant(ctx, id, string(cy.Ref.ID), false, conv.Revision, "op-add", channels.Submit)
	if err != nil || sub.Declaration != nil {
		t.Fatalf("submit plan = %+v, %v", sub, err)
	}
	acc, err := s.AddParticipant(ctx, sub)
	if err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("add = %+v, %v", acc, err)
	}
	if cur := w.refetch(conv); len(cur.Participants) != len(conv.Participants) || cur.Revision != conv.Revision {
		t.Fatalf("participants changed before the provider applied the add: %+v", cur.Participants)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Fatalf("add delivery = %s %+v", got.State, got.Failure)
	}
	cur := w.refetch(conv)
	if !slices.ContainsFunc(cur.Participants, func(x model.Participant) bool { return x.Identity == cy.Ref }) || cur.Revision == conv.Revision {
		t.Fatalf("participants after the add = %+v at %s", cur.Participants, cur.Revision)
	}
	select {
	case e := <-w.fake.Effects():
		if e.Kind != "member" || e.Remove || e.Participant.ID != "cy" {
			t.Fatalf("provider effect = %+v", e)
		}
	default:
		t.Fatal("the provider was never asked to add cy")
	}

	// Now a participant: adding again is a conflict in a preview; the
	// removal plans; the owner cannot be removed.
	_, err = s.PlanParticipant(ctx, id, string(cy.Ref.ID), false, cur.Revision, "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "already a participant")
	rm, err := s.PlanParticipant(ctx, id, string(cy.Ref.ID), true, cur.Revision, "op-rm", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	acc, err = s.RemoveParticipant(ctx, rm)
	if err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("remove = %+v, %v", acc, err)
	}
	w.settle(acc.Operation.Ref)
	if left := w.refetch(conv); slices.ContainsFunc(left.Participants, func(x model.Participant) bool { return x.Identity == cy.Ref }) {
		t.Fatalf("cy still a participant: %+v", left.Participants)
	}
	if conv.Owner == nil {
		t.Fatal("the conversation has no owner")
	}
	_, err = s.PlanParticipant(ctx, id, string(conv.Owner.ID), true, w.refetch(conv).Revision, "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "owner")
}

// TestParticipantPreviewRefusesUndeclared: an adapter that does not
// declare membership is refused in a preview, before any plan, for add
// and remove, naming the capability and where to look; an unknown
// adapter likewise. A viewer is refused by uma (the action is the
// desk's).
func TestParticipantPreviewRefusesUndeclared(t *testing.T) {
	w := verbsWorld(t)
	ctx := t.Context()
	w.inject(adapterstest.Inject{Event: "e-cy", Message: "m-cy", From: "cy", Destination: "local", Text: "hi"})
	cy := w.identityOf("cy")
	conv := w.refetch(w.local)
	s := w.as(agent)
	s.Adapter = undeclared(w)
	for _, remove := range []bool{false, true} {
		_, err := s.PlanParticipant(ctx, string(conv.Ref.ID), string(cy.Ref.ID), remove, conv.Revision, "op-x", channels.Preview)
		wantCode(t, err, model.Unsupported, "does not declare membership")
		if !strings.Contains(err.Error(), "heo channel get "+string(w.ch.ID)) {
			t.Errorf("refusal %q does not name channel get", err)
		}
	}
	s.Adapter = nil
	_, err := s.PlanParticipant(ctx, string(conv.Ref.ID), string(cy.Ref.ID), false, conv.Revision, "op-x", channels.Preview)
	wantCode(t, err, model.Unsupported, "adapter is unknown")

	// uma refuses an undeclared adapter on submission, before storing.
	un := newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareEffects() })
	un.inject(adapterstest.Inject{Event: "e-cy", Message: "m-cy", From: "cy", Destination: "local", Text: "hi"})
	ucy, uconv := un.identityOf("cy"), un.refetch(un.local)
	up, err := un.as(agent).PlanParticipant(ctx, string(uconv.Ref.ID), string(ucy.Ref.ID), false, uconv.Revision, "op-x", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = un.as(agent).AddParticipant(ctx, up)
	wantCode(t, err, model.Unsupported, "")
	// A viewer may not.
	vp, err := w.as(viewer).PlanParticipant(ctx, string(conv.Ref.ID), string(cy.Ref.ID), false, conv.Revision, "op-v", channels.Submit)
	if err != nil {
		t.Fatalf("viewer plan: %v", err)
	}
	_, err = w.as(viewer).AddParticipant(ctx, vp)
	wantCode(t, err, model.Unauthorized, "")
}

// TestReceiptPlan: a receipt is a provider effect under receipt for a
// message that arrived from the provider. A preview refuses an adapter
// that does not declare receipt, a message sent through heo and a bad
// kind; a submission sends no revision, is queued and, once sent,
// changes nothing in uma: the operation is the record.
func TestReceiptPlan(t *testing.T) {
	w := verbsWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	in := w.received("ev-r1")
	id, conv := string(in.Ref.ID), string(w.local.Ref.ID)

	_, err := s.PlanReceipt(ctx, id, conv, "seen", "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "want read or delivered")
	p, err := s.PlanReceipt(ctx, id, conv, model.ReceiptRead, "op-r", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if d := p.Declaration; d == nil || d.Capability != model.CapabilityReceipt || p.Request.Kind != model.ReceiptRead || p.Request.ExpectedRevision != "" {
		t.Fatalf("plan = %+v declaration %+v", p.Request, d)
	}
	// What heo sent is not receipted.
	sent := w.firstMessage()
	if sent.Origin == nil {
		t.Fatalf("the first message is not one heo sent: %+v", sent.Origin)
	}
	_, err = s.PlanReceipt(ctx, string(sent.Ref.ID), conv, model.ReceiptDelivered, "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "did not arrive from the provider")
	// An undeclared adapter is refused before a plan.
	u := w.as(agent)
	u.Adapter = undeclared(w)
	_, err = u.PlanReceipt(ctx, id, conv, model.ReceiptRead, "op-x", channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare receipt")

	sub, err := s.PlanReceipt(ctx, id, conv, model.ReceiptRead, "op-r", channels.Submit)
	if err != nil || sub.Declaration != nil {
		t.Fatalf("submit plan = %+v, %v", sub, err)
	}
	acc, err := s.SendReceipt(ctx, sub)
	if err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("receipt = %+v, %v", acc, err)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Fatalf("receipt delivery = %s %+v", got.State, got.Failure)
	}
	select {
	case e := <-w.fake.Effects():
		if e.Kind != "receipt" || e.Receipt != model.ReceiptRead || e.Target.ID != "ev-r1" {
			t.Fatalf("provider effect = %+v", e)
		}
	default:
		t.Fatal("the provider was never sent the receipt")
	}
	// Nothing else changed: the message is as it was.
	after, err := s.FindMessage(ctx, id, conv, false)
	if err != nil || after.Revision != in.Revision {
		t.Fatalf("message after the receipt = %+v, %v", after.Revision, err)
	}
	// A retry under the same key replays; another kind under it conflicts.
	again, err := s.SendReceipt(ctx, sub)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	other, err := s.PlanReceipt(ctx, id, conv, model.ReceiptDelivered, "op-r", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SendReceipt(ctx, other)
	wantCode(t, err, model.Conflict, "")
}

// TestHistoryImport: a bounded import reads the provider's window
// through the daemon's runtime, as the runtime's own actor, for the
// requester. It imports each message once (a second run imports
// nothing), reports a window the provider could not return whole as a
// gap, stops at its limit with a cursor to resume, refuses an undeclared
// adapter before reading, and is refused to a requester without
// channel.import.
func TestHistoryImport(t *testing.T) {
	w := verbsWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	now := time.Now().UTC()
	var items []adapterstest.Inject
	for i := range 5 {
		items = append(items, adapterstest.Inject{Event: fmt.Sprintf("h-ev-%d", i), Message: fmt.Sprintf("h-%d", i),
			From: "ada", Destination: "local", Text: fmt.Sprintf("past %d", i), OccurredAt: now.Add(time.Duration(i-10) * time.Hour)})
	}
	w.fake.HoldHistory(w.ch.ID, adapterstest.HistoryHold{Items: items})
	run := func(req uma.ImportRequest) (uma.ImportReport, error) {
		return w.e.Runtime().Import(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call}, req)
	}
	scope := desk.HistoryScope{Conversation: string(w.local.Ref.ID)}

	pl, err := s.PlanHistory(ctx, scope, agent, time.Time{}, time.Time{}, "", 3, channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if d := pl.Declaration; d == nil || d.Capability != model.CapabilityHistory || pl.Request.Requester != agent ||
		!pl.Request.Since.Equal(desk.DefaultHistorySince) || pl.Request.Conversation == nil || pl.Request.Channel != nil {
		t.Fatalf("plan = %+v", pl)
	}
	// Bounded: three of five, a cursor to resume.
	rep, err := run(pl.Request)
	if err != nil || rep.Imported != 3 || rep.Complete || rep.Next == "" || rep.Gaps != 0 {
		t.Fatalf("first run = %+v, %v", rep, err)
	}
	pl.Request.After = rep.Next
	rep, err = run(pl.Request)
	if err != nil || rep.Imported != 2 || rep.Skipped != 0 || !rep.Complete || rep.Next != "" {
		t.Fatalf("resumed run = %+v, %v", rep, err)
	}
	// Idempotent: the whole window again imports nothing.
	pl.Request.After, pl.Request.Limit = "", 0
	rep, err = run(pl.Request)
	if err != nil || rep.Imported != 0 || rep.Skipped != 5 || !rep.Complete {
		t.Fatalf("second run = %+v, %v", rep, err)
	}
	got := 0
	for _, m := range w.messages(w.local.Ref) {
		if m.ImportedBy == agent {
			got++
		}
	}
	if got != 5 {
		t.Fatalf("%d messages imported by the requester, want 5", got)
	}

	// A window the provider holds only part of is a gap, not a whole.
	w.fake.HoldHistory(w.ch.ID, adapterstest.HistoryHold{Items: items, Horizon: now.Add(-5 * time.Hour)})
	rep, err = run(pl.Request)
	if err != nil || rep.Gaps != 1 || !rep.Complete {
		t.Fatalf("run past the provider's horizon = %+v, %v; want a gap", rep, err)
	}

	// A channel scope is the operator's: the desk may not read the
	// channel, so for it there is none.
	_, err = s.PlanHistory(ctx, desk.HistoryScope{Channel: string(w.ch.ID)}, agent, time.Time{}, time.Time{}, "", 0, channels.Preview)
	wantCode(t, err, model.NotFound, "no channel")
	op := w.as(operator)
	cp, err := op.PlanHistory(ctx, desk.HistoryScope{Channel: string(w.ch.ID)}, operator, time.Time{}, time.Time{}, "", 0, channels.Preview)
	if err != nil || cp.Request.Channel == nil || cp.Request.Conversation != nil {
		t.Fatalf("channel plan = %+v, %v", cp, err)
	}
	// Exactly one scope; a bounded limit.
	_, err = s.PlanHistory(ctx, desk.HistoryScope{}, agent, time.Time{}, time.Time{}, "", 0, channels.Preview)
	wantCode(t, err, model.Validation, "exactly one")
	_, err = s.PlanHistory(ctx, desk.HistoryScope{Channel: "a", Conversation: "b"}, agent, time.Time{}, time.Time{}, "", 0, channels.Preview)
	wantCode(t, err, model.Validation, "exactly one")
	_, err = s.PlanHistory(ctx, scope, agent, time.Time{}, time.Time{}, "", uma.MaxImportLimit+1, channels.Preview)
	wantCode(t, err, model.Validation, "limit")
	// An upper end of the window is passed on, and must lie after its start.
	end := now.Add(-5 * time.Hour)
	up, err := s.PlanHistory(ctx, scope, agent, time.Time{}, end, "", 0, channels.Preview)
	if err != nil || !up.Request.Until.Equal(end) {
		t.Fatalf("plan with until = %+v, %v", up, err)
	}
	_, err = s.PlanHistory(ctx, scope, agent, end, end, "", 0, channels.Preview)
	wantCode(t, err, model.Validation, "until")

	// A viewer holds no channel.import: uma refuses before any read.
	vreq := pl.Request
	vreq.Requester = viewer
	reads := len(w.fake.HistoryReads())
	if _, err := run(vreq); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("import for a viewer = %v, want unauthorized", err)
	}
	if n := len(w.fake.HistoryReads()); n != reads {
		t.Fatalf("a refused import read the provider %d times", n-reads)
	}

	// An undeclared adapter is refused in a preview, before any plan.
	us := w.as(agent)
	us.Adapter = undeclared(w)
	_, err = us.PlanHistory(ctx, scope, agent, time.Time{}, time.Time{}, "", 0, channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare history")
}

// TestVerbsLiveView: the desk reads the live status of the provider
// verbs on a conversation's channel through uma, as a read of the
// conversation (conversation.read), never a channel read: a verb whose
// capability the adapter declares is available; one it does not is
// unsupported; a viewer, who may not read internal content, is refused.
func TestVerbsLiveView(t *testing.T) {
	w := verbsWorld(t)
	ctx := t.Context()
	vs, err := w.as(agent).Verbs(ctx, w.local)
	if err != nil {
		t.Fatalf("desk view: %v", err)
	}
	state := map[string]string{}
	for _, v := range vs {
		state[v.Verb] = v.State
	}
	for verb, want := range map[string]string{
		"message edit": "available", "participant add": "available", "participant remove": "available",
		"receipt send": "available", "history fetch": "available", "conversation update": "unsupported",
	} {
		if state[verb] != want {
			t.Errorf("%s = %q, want %q (all: %v)", verb, state[verb], want, state)
		}
	}
	if _, err := w.as(viewer).Verbs(ctx, w.local); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer view = %v, want unauthorized", err)
	}
	// The plain fake declares none of the new verbs.
	pw := newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareEffects() })
	pv, err := pw.as(agent).Verbs(ctx, pw.local)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range pv {
		if (v.Verb == "receipt send" || v.Verb == "history fetch" || v.Verb == "participant add") && v.State != "unsupported" {
			t.Errorf("plain fake: %s = %s", v.Verb, v.State)
		}
	}
}
