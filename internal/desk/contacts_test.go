package desk_test

import (
	"context"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// w0 is the world's clock.
func w0() time.Time { return time.Unix(1_700_000_000, 0) }

// identityOf returns the identity the conversation's counterpart holds.
func (w *world) identityOf(who string) model.Identity {
	w.t.Helper()
	p, err := w.as(agent).ListIdentities(w.t.Context(), desk.IdentityFilter{}, model.PageRequest{Limit: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, i := range p.Items {
		if i.SourceID == who {
			return i
		}
	}
	w.t.Fatalf("no identity for %s among %+v", who, p.Items)
	return model.Identity{}
}

// TestContactsAndIdentities: each counterpart the desk wrote to has an
// identity on a provisional contact of its own; the desk and a viewer
// list and find them by record ID, narrowed by kind, contact and inbox.
func TestContactsAndIdentities(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	ada, bob := w.identityOf("ada"), w.identityOf("bob")
	if ada.Contact == bob.Contact || ada.Ref.Inbox != w.local.Ref.Inbox || bob.Ref.Inbox != w.remote.Ref.Inbox {
		t.Fatalf("identities = %+v %+v", ada, bob)
	}
	for _, actor := range []model.ActorID{agent, viewer} {
		s := w.as(actor)
		p, err := s.ListContacts(ctx, nil, model.PageRequest{Limit: 10})
		if err != nil || len(p.Items) != 2 {
			t.Fatalf("%s contacts = %+v, %v", actor, p.Items, err)
		}
		c, err := s.FindContact(ctx, string(ada.Contact.ID))
		if err != nil || c.Ref != ada.Contact || c.Kind != model.ContactKindPerson {
			t.Fatalf("%s find contact = %+v, %v", actor, c, err)
		}
		got, err := s.FindIdentity(ctx, string(bob.Ref.ID))
		if err != nil || got.Ref != bob.Ref {
			t.Fatalf("%s find identity = %+v, %v", actor, got, err)
		}
	}
	s := w.as(agent)
	if p, err := s.ListContacts(ctx, []string{model.ContactKindBot}, model.PageRequest{Limit: 10}); err != nil || len(p.Items) != 0 {
		t.Fatalf("bot contacts = %+v, %v", p.Items, err)
	}
	_, err := s.ListContacts(ctx, []string{"robot"}, model.PageRequest{Limit: 10})
	wantCode(t, err, model.Validation, `kind "robot"`)
	p, err := s.ListIdentities(ctx, desk.IdentityFilter{Contact: string(ada.Contact.ID)}, model.PageRequest{Limit: 10})
	if err != nil || len(p.Items) != 1 || p.Items[0].Ref != ada.Ref {
		t.Fatalf("identities of ada's contact = %+v, %v", p.Items, err)
	}
	p, err = s.ListIdentities(ctx, desk.IdentityFilter{Inbox: "provider"}, model.PageRequest{Limit: 10})
	if err != nil || len(p.Items) != 1 || p.Items[0].Ref != bob.Ref {
		t.Fatalf("identities of inbox provider = %+v, %v", p.Items, err)
	}
	all, err := s.ContactIdentities(ctx, bob.Contact)
	if err != nil || len(all) != 1 {
		t.Fatalf("contact identities = %+v, %v", all, err)
	}
	_, err = s.FindContact(ctx, "nope")
	wantCode(t, err, model.NotFound, "contact nope")
	_, err = s.FindIdentity(ctx, "nope")
	wantCode(t, err, model.NotFound, "identity nope")
	if h, err := s.History(ctx, ada.Ref); err != nil || len(h) != 0 {
		t.Fatalf("history before any link = %+v, %v", h, err)
	}
}

// TestLinkAndUnlink: a desk actor links an identity to another contact
// on its attestation, at the identity's revision; a preview writes
// nothing; a stale revision is a conflict; the link history records the
// move with its actor and asserted assurance; an unlink needs a reason,
// detaches the identity to a fresh contact and is recorded too.
func TestLinkAndUnlink(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	ada, bob := w.identityOf("ada"), w.identityOf("bob")
	bobID, adaContact := string(bob.Ref.ID), string(ada.Contact.ID)

	// A stale revision: final in a preview, deferred to uma on submit,
	// where uma refuses it and heo explains it.
	_, err := s.PlanLink(ctx, bobID, adaContact, "stale", "", "link-1", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision "+string(bob.Revision))
	p, err := s.PlanLink(ctx, bobID, adaContact, "stale", "", "link-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Link(ctx, p)
	wantCode(t, err, model.Conflict, "is at revision "+string(bob.Revision))
	_, err = s.PlanLink(ctx, bobID, adaContact, "", "", "link-1", channels.Preview)
	wantCode(t, err, model.Validation, "revision being replaced is required")

	// The preview: the request as it would be submitted, nothing written.
	p, err = s.PlanLink(ctx, bobID, adaContact, bob.Revision, "ticket-7", "link-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if p.Class != model.LinkClassLink || p.Target.Ref != ada.Contact || p.Identity.Ref != bob.Ref ||
		p.Request.ExpectedRevision != bob.Revision || p.Request.Contact != ada.Contact {
		t.Fatalf("link plan = %+v", p)
	}
	// The request carries the attestation kind and the caller's
	// reference only: no assurance, no facts.
	if ev := p.Request.Evidence; ev.Verifier != model.EvidenceKindAttestation || ev.Reference != "ticket-7" ||
		ev.Assurance != "" || ev.Facts.Name != "" || len(ev.Facts.Data) != 0 {
		t.Fatalf("link evidence = %+v", ev)
	}
	if got := w.identityOf("bob"); got.Revision != bob.Revision || got.Contact != bob.Contact {
		t.Fatalf("planning wrote: %+v", got)
	}

	ch, err := s.Link(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Identity.Contact != ada.Contact || ch.Identity.LinkAssurance != model.LinkAssuranceAsserted ||
		ch.Identity.Revision == bob.Revision || ch.Acceptance.Operation.State != model.Committed {
		t.Fatalf("linked = %+v", ch)
	}
	r := ch.Record
	if r == nil || r.Class != model.LinkClassLink || r.From != bob.Contact || r.To != ada.Contact || r.Actor != agent ||
		r.Assurance != model.LinkAssuranceAsserted || r.Evidence.Verifier != model.EvidenceKindAttestation ||
		r.Evidence.Reference != "ticket-7" || r.VerifierVersion != desk.AttestationVersion ||
		r.RevisionBefore != bob.Revision || r.RevisionAfter != ch.Identity.Revision {
		t.Fatalf("link record = %+v", r)
	}
	facts, err := r.Evidence.DecodeFacts()
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := facts.(model.AttestationFacts); !ok || f.Actor != agent || f.ReasonCode != desk.LinkReason {
		t.Fatalf("link facts = %+v", facts)
	}
	// The same key replays, though the revision has moved since.
	p, err = s.PlanLink(ctx, bobID, adaContact, bob.Revision, "ticket-7", "link-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.Link(ctx, p); err != nil || !again.Acceptance.Replayed {
		t.Fatalf("replayed link = %+v, %v", again, err)
	}
	if h, err := s.History(ctx, bob.Ref); err != nil || len(h) != 1 {
		t.Fatalf("history after link = %+v, %v", h, err)
	}

	// Linking it where it is changes nothing.
	linked := w.identityOf("bob")
	p, err = s.PlanLink(ctx, bobID, adaContact, linked.Revision, "", "link-2", channels.Preview)
	if err != nil || p.Class != "" || len(p.Warnings) != 1 {
		t.Fatalf("no-op plan = %+v, %v", p, err)
	}

	// Unlink: a reason is required, and must be a reason code.
	_, err = s.PlanUnlink(ctx, bobID, "", linked.Revision, "", "unlink-1", channels.Preview)
	wantCode(t, err, model.Validation, "reason: required")
	_, err = s.PlanUnlink(ctx, bobID, "Wrong Person", linked.Revision, "", "unlink-1", channels.Preview)
	wantCode(t, err, model.Validation, "reason")
	_, err = s.PlanUnlink(ctx, bobID, "wrong_person", bob.Revision, "", "unlink-1", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision "+string(linked.Revision))
	u, err := s.PlanUnlink(ctx, bobID, "wrong_person", linked.Revision, "", "unlink-1", channels.Preview)
	if err != nil || !u.Request.Evidence.IsZero() || u.Request.Reason != "wrong_person" {
		t.Fatalf("unlink plan = %+v, %v", u, err)
	}
	ch, err = s.Unlink(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Identity.Contact == ada.Contact || ch.Identity.Contact == bob.Contact || ch.Identity.LinkAssurance != model.LinkAssuranceNone {
		t.Fatalf("unlinked = %+v", ch.Identity)
	}
	if r := ch.Record; r == nil || r.Class != model.LinkClassUnlink || r.Reason != "wrong_person" || r.From != ada.Contact ||
		r.To != ch.Identity.Contact || r.Actor != agent || !r.Evidence.IsZero() {
		t.Fatalf("unlink record = %+v", ch.Record)
	}
	h, err := s.History(ctx, bob.Ref)
	if err != nil || len(h) != 2 || h[0].Class != model.LinkClassLink || h[1].Class != model.LinkClassUnlink {
		t.Fatalf("history = %+v, %v", h, err)
	}

	// Off a contact it was linked to before, a link is a relink; an
	// unlink with a reference carries the actor's attestation.
	p, err = s.PlanLink(ctx, bobID, adaContact, ch.Identity.Revision, "", "link-3", channels.Preview)
	if err != nil || p.Class != model.LinkClassRelink {
		t.Fatalf("relink plan = %+v, %v", p, err)
	}
	if ch, err = s.Link(ctx, p); err != nil || ch.Record == nil || ch.Record.Class != model.LinkClassRelink {
		t.Fatalf("relink = %+v, %v", ch, err)
	}
	u, err = s.PlanUnlink(ctx, bobID, "shared_device", ch.Identity.Revision, "ticket-9", "unlink-2", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if ch, err = s.Unlink(ctx, u); err != nil || ch.Record == nil || ch.Record.Evidence.Verifier != model.EvidenceKindAttestation ||
		ch.Record.Evidence.Reference != "ticket-9" || ch.Record.Assurance != model.LinkAssuranceNone {
		t.Fatalf("attested unlink = %+v, %v", ch.Record, err)
	}
}

// TestLinkNeedsTheDeskRole: a viewer reads contacts and identities but
// links and unlinks nothing; uma's policy refuses it.
func TestLinkNeedsTheDeskRole(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	ada, bob := w.identityOf("ada"), w.identityOf("bob")
	s := w.as(viewer)
	p, err := s.PlanLink(ctx, string(bob.Ref.ID), string(ada.Contact.ID), bob.Revision, "", "link-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Link(ctx, p)
	wantCode(t, err, model.Unauthorized, "")
	u, err := s.PlanUnlink(ctx, string(bob.Ref.ID), "wrong_person", bob.Revision, "", "unlink-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Unlink(ctx, u)
	wantCode(t, err, model.Unauthorized, "")
	if got := w.identityOf("bob"); got.Contact != bob.Contact || got.Revision != bob.Revision {
		t.Fatalf("viewer moved %+v", got)
	}
}

// TestAttestationAssuranceIsItsOwn: the verifier vouches asserted, for
// the requesting actor and the request's binding, whatever assurance
// or facts the caller's evidence claims.
func TestAttestationAssuranceIsItsOwn(t *testing.T) {
	target := model.ContactRef{Tenant: tenant, ID: "c-2"}
	req := model.LinkRequest{
		Intent: model.LinkClassLink, Identity: model.IdentityRef{Inbox: model.InboxRef{Tenant: tenant, ID: "in-1"}, ID: "i-1"},
		Current: model.ContactRef{Tenant: tenant, ID: "c-1"}, ExpectedRevision: "r1", Target: &target,
		Evidence: model.Evidence{Verifier: model.EvidenceKindAttestation, Reference: "ref-1", Assurance: model.LinkAssuranceProven},
	}
	p := model.Principal{Tenant: tenant, Actor: agent}
	ev, err := desk.Attestation{}.Verify(context.Background(), p, req)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Assurance != model.LinkAssuranceAsserted || ev.Reference != "ref-1" || ev.Verifier != model.EvidenceKindAttestation {
		t.Fatalf("evidence = %+v", ev)
	}
	req.Evidence.Assurance = ""
	if err := ev.ValidateReturn(req, agent, w0()); err != nil {
		t.Fatalf("uma refuses the evidence: %v", err)
	}
	if err := ev.ValidateReturn(req, viewer, w0()); err == nil {
		t.Fatal("evidence vouched for the agent passes for another actor")
	}
	unlink := model.LinkRequest{Intent: model.LinkClassUnlink, Identity: req.Identity, Current: req.Current,
		ExpectedRevision: "r1", Reason: "wrong_person", Evidence: model.Evidence{Verifier: model.EvidenceKindAttestation}}
	ev, err = desk.Attestation{}.Verify(context.Background(), p, unlink)
	if err != nil || ev.ValidateReturn(unlink, agent, w0()) != nil {
		t.Fatalf("unlink evidence = %+v, %v", ev, err)
	}
	facts, _ := ev.DecodeFacts()
	if f, ok := facts.(model.AttestationFacts); !ok || f.ReasonCode != "wrong_person" || f.Target != nil {
		t.Fatalf("unlink facts = %+v", facts)
	}
	if _, err := (desk.Attestation{}).Verify(context.Background(), p, model.LinkRequest{Intent: "merge"}); !model.IsCode(err, model.Validation) {
		t.Fatalf("unknown intent = %v", err)
	}
}
