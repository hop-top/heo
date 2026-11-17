package desk_test

import (
	"slices"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

func attrsOf(t *testing.T, emails, phones []string) desk.ContactAttributes {
	t.Helper()
	return desk.ContactAttributes{Email: emails, Phone: phones}
}

// TestContactCreateAndUpdate: a desk actor creates a contact no identity
// points at, and edits its name and attributes at the revision it read.
// A preview writes nothing; a stale revision is a conflict; the request
// carries the revision it was planned at; a repeated key replays; a
// viewer is refused.
func TestContactCreateAndUpdate(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	before, err := s.ListContacts(ctx, nil, model.PageRequest{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}

	// Shape: a name is required; the kind and the attributes are checked.
	_, err = s.PlanContactCreate(ctx, "", "", desk.ContactAttributes{}, "mk-1")
	wantCode(t, err, model.Validation, "display_name")
	_, err = s.PlanContactCreate(ctx, "robot", "Zed", desk.ContactAttributes{}, "mk-1")
	wantCode(t, err, model.Validation, "kind")
	_, err = s.PlanContactCreate(ctx, "", "Zed", attrsOf(t, []string{"not an address"}, nil), "mk-1")
	wantCode(t, err, model.Validation, "email")
	_, err = s.PlanContactCreate(ctx, "", "Zed", attrsOf(t, nil, []string{"0612345678"}), "mk-1")
	wantCode(t, err, model.Validation, "phone")

	// Preview: the request as it would be submitted; nothing written.
	plan, err := s.PlanContactCreate(ctx, "", "Grace Hopper", attrsOf(t, []string{"grace@Example.com"}, []string{"+1 202-555-0100"}), "mk-1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.DisplayName != "Grace Hopper" ||
		plan.Request.Tenant != tenant || len(plan.Request.Attributes) != 2 {
		t.Fatalf("create plan = %+v", plan.Request)
	}
	want := []model.Attribute{{Kind: model.AttributeEmail, Value: "grace@example.com"}, {Kind: model.AttributePhone, Value: "+12025550100"}}
	if !slices.Equal(plan.Request.Attributes, want) {
		t.Fatalf("attributes = %+v, want normalized %+v", plan.Request.Attributes, want)
	}
	if p, err := s.ListContacts(ctx, nil, model.PageRequest{Limit: 100}); err != nil || len(p.Items) != len(before.Items) {
		t.Fatalf("planning wrote: %+v, %v", p.Items, err)
	}

	made, err := s.CreateContact(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	c := made.Contact
	if c.DisplayName != "Grace Hopper" || c.Kind != model.ContactKindPerson || c.Revision == "" || c.MergedInto != nil ||
		made.Acceptance.Operation.State != model.Committed || !slices.Equal(c.Attributes, want) {
		t.Fatalf("created = %+v", made)
	}
	if ids, err := s.ContactIdentities(ctx, c.Ref); err != nil || len(ids) != 0 {
		t.Fatalf("a created contact holds identities: %+v, %v", ids, err)
	}
	// The same key replays: no second contact.
	again, err := s.CreateContact(ctx, plan)
	if err != nil || !again.Acceptance.Replayed || again.Contact.Ref != c.Ref {
		t.Fatalf("replayed create = %+v, %v", again, err)
	}
	if p, _ := s.ListContacts(ctx, nil, model.PageRequest{Limit: 100}); len(p.Items) != len(before.Items)+1 {
		t.Fatalf("contacts after replay = %d, want %d", len(p.Items), len(before.Items)+1)
	}
	// Another payload under the key is a conflict.
	other, err := s.PlanContactCreate(ctx, "", "Someone Else", desk.ContactAttributes{}, "mk-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateContact(ctx, other)
	wantCode(t, err, model.Conflict, "")

	// Update: a revision is required, and a stale one is a conflict:
	// final in a preview, uma's to refuse on submission.
	id := string(c.Ref.ID)
	name := "Grace B. Hopper"
	_, err = s.PlanContactUpdate(ctx, id, "", desk.ContactChange{Name: &name}, "up-1", channels.Preview)
	wantCode(t, err, model.Validation, "revision being replaced is required")
	_, err = s.PlanContactUpdate(ctx, id, "stale", desk.ContactChange{Name: &name}, "up-1", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision "+string(c.Revision))
	sp, err := s.PlanContactUpdate(ctx, id, "stale", desk.ContactChange{Name: &name}, "up-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateContact(ctx, sp)
	wantCode(t, err, model.Conflict, "is at revision "+string(c.Revision))
	_, err = s.PlanContactUpdate(ctx, id, c.Revision, desk.ContactChange{}, "up-1", channels.Preview)
	wantCode(t, err, model.Validation, "nothing to change")

	// Preview: a name change keeps the attributes as they are.
	up, err := s.PlanContactUpdate(ctx, id, c.Revision, desk.ContactChange{Name: &name}, "up-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if up.Request.ExpectedRevision != c.Revision || up.Request.DisplayName != name || !slices.Equal(up.Request.Attributes, want) ||
		!slices.Equal(up.Changes, []string{model.ContactChangeDisplayName}) {
		t.Fatalf("update plan = %+v, changes %v", up.Request, up.Changes)
	}
	if got, _ := s.FindContact(ctx, id); got.Revision != c.Revision || got.DisplayName != "Grace Hopper" {
		t.Fatalf("planning wrote: %+v", got)
	}
	upd, err := s.UpdateContact(ctx, up)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Contact.DisplayName != name || upd.Contact.Revision == c.Revision || !slices.Equal(upd.Contact.Attributes, want) {
		t.Fatalf("updated = %+v", upd.Contact)
	}
	// The same key replays, though the revision has moved on.
	rp, err := s.PlanContactUpdate(ctx, id, c.Revision, desk.ContactChange{Name: &name}, "up-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.UpdateContact(ctx, rp); err != nil || !r.Acceptance.Replayed || r.Contact.Revision != upd.Contact.Revision {
		t.Fatalf("replayed update = %+v, %v", r, err)
	}

	// An attribute kind given replaces that kind alone; an empty list
	// clears it.
	cur := upd.Contact
	em := desk.ContactChange{Attributes: desk.ContactAttributes{Email: []string{"hopper@navy.example"}, EmailSet: true}}
	up, err = s.PlanContactUpdate(ctx, id, cur.Revision, em, "up-2", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	wantEm := []model.Attribute{{Kind: model.AttributeEmail, Value: "hopper@navy.example"}, {Kind: model.AttributePhone, Value: "+12025550100"}}
	if !slices.Equal(up.Request.Attributes, wantEm) || !slices.Equal(up.Changes, []string{model.ContactChangeAttributes}) {
		t.Fatalf("email update = %+v, changes %v", up.Request.Attributes, up.Changes)
	}
	clr := desk.ContactChange{Attributes: desk.ContactAttributes{PhoneSet: true}}
	up, err = s.PlanContactUpdate(ctx, id, cur.Revision, clr, "up-3", channels.Preview)
	if err != nil || len(up.Request.Attributes) != 1 || up.Request.Attributes[0].Kind != model.AttributeEmail {
		t.Fatalf("phone cleared = %+v, %v", up.Request.Attributes, err)
	}

	// The same name changes nothing, and the plan says so.
	same := upd.Contact.DisplayName
	up, err = s.PlanContactUpdate(ctx, id, cur.Revision, desk.ContactChange{Name: &same}, "up-4", channels.Preview)
	if err != nil || len(up.Changes) != 0 || len(up.Warnings) != 1 {
		t.Fatalf("no-op update plan = %+v, %v", up, err)
	}

	// A viewer reads contacts and edits none.
	v := w.as(viewer)
	vp, err := v.PlanContactCreate(ctx, "", "Mallory", desk.ContactAttributes{}, "mk-v")
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.CreateContact(ctx, vp)
	wantCode(t, err, model.Unauthorized, "")
	vu, err := v.PlanContactUpdate(ctx, id, cur.Revision, desk.ContactChange{Name: &name}, "up-v", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.UpdateContact(ctx, vu)
	wantCode(t, err, model.Unauthorized, "")
	if got, _ := s.FindContact(ctx, id); got.Revision != cur.Revision {
		t.Fatalf("viewer wrote: %+v", got)
	}
}

// merged is ada's and bob's contacts, ada's identity and the revisions
// read before a merge of ada's into bob's.
type mergeCase struct {
	from, into model.Contact
	ident      model.Identity
}

func (w *world) mergeCase() mergeCase {
	w.t.Helper()
	ada, bob := w.identityOf("ada"), w.identityOf("bob")
	s := w.as(agent)
	from, err := s.FindContact(w.t.Context(), string(ada.Contact.ID))
	if err != nil {
		w.t.Fatal(err)
	}
	into, err := s.FindContact(w.t.Context(), string(bob.Contact.ID))
	if err != nil {
		w.t.Fatal(err)
	}
	return mergeCase{from: from, into: into, ident: ada}
}

func (m mergeCase) input() desk.MergeInput {
	return desk.MergeInput{
		From: string(m.from.Ref.ID), Into: string(m.into.Ref.ID),
		Revision: m.from.Revision, IntoRevision: m.into.Revision, Reason: "same_person",
	}
}

// TestContactMerge: a desk actor merges one contact into another. The
// preview is uma's: the identities and the conversations that move and
// both revisions; it writes nothing. The merge moves exactly what was
// previewed, leaves the contact merged as a redirect, and records every
// identity's move in its link history. A stale revision, a contact
// already merged and a replayed key are told apart; a viewer is refused.
func TestContactMerge(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	m := w.mergeCase()

	// The preview is uma's, with the revisions to merge at.
	in := m.input()
	in.Revision, in.IntoRevision = "", ""
	plan, err := s.PlanMerge(ctx, in, "mg-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	pv := plan.Preview
	if pv.From != m.from.Ref || pv.Into != m.into.Ref || pv.FromRevision != m.from.Revision || pv.IntoRevision != m.into.Revision ||
		!slices.Equal(pv.Identities, []model.IdentityRef{m.ident.Ref}) ||
		!slices.Equal(pv.Conversations, []model.ConversationRef{w.local.Ref}) || pv.MoreConversations {
		t.Fatalf("preview = %+v", pv)
	}
	if plan.Request.ExpectedRevision != m.from.Revision || plan.Request.ExpectedIntoRevision != m.into.Revision ||
		!slices.Equal(plan.Request.Identities, pv.Identities) || plan.Request.Reason != "same_person" {
		t.Fatalf("merge request = %+v", plan.Request)
	}
	if got := w.mergeCase(); got.from.MergedInto != nil || got.from.Revision != m.from.Revision || got.into.Revision != m.into.Revision {
		t.Fatalf("planning wrote: %+v", got)
	}

	// Executing needs both revisions and a reason.
	noRev := m.input()
	noRev.Revision = ""
	_, err = s.PlanMerge(ctx, noRev, "mg-1", channels.Submit)
	wantCode(t, err, model.Validation, "revision being replaced is required")
	noInto := m.input()
	noInto.IntoRevision = ""
	_, err = s.PlanMerge(ctx, noInto, "mg-1", channels.Submit)
	wantCode(t, err, model.Validation, "revision being replaced is required")
	noReason := m.input()
	noReason.Reason = ""
	_, err = s.PlanMerge(ctx, noReason, "mg-1", channels.Preview)
	wantCode(t, err, model.Validation, "reason")
	badReason := m.input()
	badReason.Reason = "Same Person"
	_, err = s.PlanMerge(ctx, badReason, "mg-1", channels.Preview)
	wantCode(t, err, model.Validation, "reason")

	// A stale revision, on either contact: final in a preview, uma's to
	// refuse on submission.
	stale := m.input()
	stale.IntoRevision = "stale"
	_, err = s.PlanMerge(ctx, stale, "mg-1", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision "+string(m.into.Revision))
	sp, err := s.PlanMerge(ctx, stale, "mg-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MergeContacts(ctx, sp)
	wantCode(t, err, model.Conflict, "is at revision "+string(m.into.Revision))
	staleFrom := m.input()
	staleFrom.Revision = "stale"
	sp, err = s.PlanMerge(ctx, staleFrom, "mg-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MergeContacts(ctx, sp)
	wantCode(t, err, model.Conflict, "is at revision "+string(m.from.Revision))

	// The identities the caller reviewed must be the ones that move.
	rev := m.input()
	rev.Identities = []string{"id_other"}
	_, err = s.PlanMerge(ctx, rev, "mg-1", channels.Submit)
	wantCode(t, err, model.Conflict, "identities")
	rev.Identities = []string{string(m.ident.Ref.ID)}
	if _, err = s.PlanMerge(ctx, rev, "mg-1", channels.Preview); err != nil {
		t.Fatalf("reviewed identities refused: %v", err)
	}

	// A viewer reads, and merges nothing: uma refuses the preview.
	_, err = w.as(viewer).PlanMerge(ctx, m.input(), "mg-v", channels.Submit)
	wantCode(t, err, model.Unauthorized, "")

	// A contact of another kind does not take the identities.
	bot, err := s.PlanContactCreate(ctx, model.ContactKindBot, "Helper", desk.ContactAttributes{}, "mk-bot")
	if err != nil {
		t.Fatal(err)
	}
	botMade, err := s.CreateContact(ctx, bot)
	if err != nil {
		t.Fatal(err)
	}
	kind := m.input()
	kind.Into, kind.IntoRevision = string(botMade.Contact.Ref.ID), botMade.Contact.Revision
	_, err = s.PlanMerge(ctx, kind, "mg-k", channels.Preview)
	wantCode(t, err, model.Validation, "kind")

	// Merge.
	plan, err = s.PlanMerge(ctx, m.input(), "mg-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.MergeContacts(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	if done.From.MergedInto == nil || *done.From.MergedInto != m.into.Ref || done.From.Revision == m.from.Revision ||
		done.Into.Revision == m.into.Revision || done.Into.MergedInto != nil ||
		done.Acceptance.Operation.State != model.Committed || done.Acceptance.Replayed {
		t.Fatalf("merged = %+v", done)
	}
	if len(done.Links) != 1 || done.Links[0].Class != model.LinkClassMerge || done.Links[0].Identity != m.ident.Ref ||
		done.Links[0].From != m.from.Ref || done.Links[0].To != m.into.Ref || done.Links[0].Actor != agent ||
		done.Links[0].Reason != "same_person" {
		t.Fatalf("merge links = %+v", done.Links)
	}
	// What moved is what the preview listed.
	var moved []model.IdentityRef
	for _, l := range done.Links {
		moved = append(moved, l.Identity)
	}
	if !slices.Equal(moved, pv.Identities) {
		t.Fatalf("moved %v, previewed %v", moved, pv.Identities)
	}
	// The identity is on the contact merged into, its history shows
	// the move, and the contact merged reads as a redirect.
	if got := w.identityOf("ada"); got.Contact != m.into.Ref {
		t.Fatalf("identity after merge = %+v", got)
	}
	if h, err := s.History(ctx, m.ident.Ref); err != nil || len(h) != 1 || h[0].Class != model.LinkClassMerge {
		t.Fatalf("history = %+v, %v", h, err)
	}
	if c, err := s.FindContact(ctx, string(m.from.Ref.ID)); err != nil || c.MergedInto == nil || *c.MergedInto != m.into.Ref {
		t.Fatalf("contact merged = %+v, %v", c, err)
	}
	if ids, err := s.ContactIdentities(ctx, m.into.Ref); err != nil || len(ids) != 2 {
		t.Fatalf("identities of the contact merged into = %+v, %v", ids, err)
	}

	// The same key replays; nothing moves twice.
	again, err := s.PlanMerge(ctx, m.input(), "mg-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.MergeContacts(ctx, again)
	if err != nil || !r.Acceptance.Replayed || r.Acceptance.Operation.Ref != done.Acceptance.Operation.Ref {
		t.Fatalf("replayed merge = %+v, %v", r, err)
	}
	if h, _ := s.History(ctx, m.ident.Ref); len(h) != 1 {
		t.Fatalf("history after replay = %+v", h)
	}

	// Another key for the same merge is a conflict that names it; so is
	// a merge into or out of a contact already merged.
	again, err = s.PlanMerge(ctx, m.input(), "mg-2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MergeContacts(ctx, again)
	wantCode(t, err, model.Conflict, "already merged into "+string(m.into.Ref.ID))
	back := desk.MergeInput{From: string(m.into.Ref.ID), Into: string(m.from.Ref.ID), Revision: done.Into.Revision, IntoRevision: done.From.Revision, Reason: "same_person"}
	_, err = s.PlanMerge(ctx, back, "mg-3", channels.Preview)
	wantCode(t, err, model.Conflict, "merged into "+string(m.into.Ref.ID))
	third := desk.MergeInput{From: string(botMade.Contact.Ref.ID), Into: string(m.from.Ref.ID), Revision: botMade.Contact.Revision, IntoRevision: done.From.Revision, Reason: "same_person"}
	_, err = s.PlanMerge(ctx, third, "mg-4", channels.Preview)
	wantCode(t, err, model.Conflict, "merge into that one")

	// Linking to the contact merged is refused: the redirect names where
	// its identities went.
	link, err := s.PlanLink(ctx, string(w.identityOf("bob").Ref.ID), string(m.from.Ref.ID), w.identityOf("bob").Revision, "", "lk-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Link(ctx, link)
	wantCode(t, err, model.Conflict, "merged")
}
