package desk

import (
	"context"
	"fmt"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// Attestation is the identity verifier heo registers with uma, under
// model.EvidenceKindAttestation, its only one: the requesting desk actor
// attests that an identity belongs to a contact, or detaches it under a
// reason. It is the lowest grade of evidence (model.LinkAssuranceAsserted)
// and heo's only way to link: heo never matches identities on its own.
//
// Verify binds its facts to the request uma computed after reading the
// identity — the intent (link, relink or unlink), the identity, its
// current contact and, to link, the target — and to the requesting
// actor; uma checks them against that request before any policy
// decision. The assurance is always asserted, whatever the caller's
// evidence says: no caller input reaches it.
type Attestation struct{}

// AttestationVersion is recorded on every link record Attestation
// vouched for.
const AttestationVersion = "heo-attestation/1"

// LinkReason is the reason code a link or relink attests: the identity
// belongs to the target contact.
const LinkReason = "same_contact"

var (
	_ policy.IdentityVerifier  = Attestation{}
	_ policy.VersionedVerifier = Attestation{}
)

// IdentityVerifiers are the verifiers heo registers with uma
// (uma.Config.IdentityVerifiers).
func IdentityVerifiers() map[string]policy.IdentityVerifier {
	return map[string]policy.IdentityVerifier{model.EvidenceKindAttestation: Attestation{}}
}

// Version names the verifier on the link records it vouched for.
func (Attestation) Version() string { return AttestationVersion }

// Verify returns attestation evidence bound to req and to p's actor.
// It reads and writes nothing. The caller's evidence contributes its
// reference only, an opaque proof ID uma holds to one link.
func (Attestation) Verify(_ context.Context, p model.Principal, req model.LinkRequest) (model.Evidence, error) {
	binding := model.EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current}
	reason := LinkReason
	switch req.Intent {
	case model.LinkClassLink, model.LinkClassRelink:
		if req.Target == nil {
			return model.Evidence{}, model.NewError(model.Validation, "attestation: target required")
		}
		binding.Target = req.Target
	case model.LinkClassUnlink:
		reason = req.Reason
	default:
		return model.Evidence{}, model.NewError(model.Validation, "attestation: intent not supported")
	}
	facts, err := model.NewEvidenceFacts(model.AttestationFacts{EvidenceBinding: binding, Actor: p.Actor, ReasonCode: reason})
	if err != nil {
		return model.Evidence{}, err
	}
	return model.Evidence{
		Verifier: model.EvidenceKindAttestation, Reference: req.Evidence.Reference,
		Facts: facts, Assurance: model.LinkAssuranceAsserted,
	}, nil
}

// attest is the evidence a link or unlink request carries: the
// attestation verifier and the caller's reference. It declares no
// assurance — uma refuses one — and no facts: the verifier states them.
func attest(reference string) model.Evidence {
	return model.Evidence{Verifier: model.EvidenceKindAttestation, Reference: reference}
}

// LinkPlan is a validated identity link, the identity as it is now and
// the contact it moves to.
type LinkPlan struct {
	Request  model.LinkIdentity
	Identity model.Identity
	Target   model.Contact
	// Class is what uma will record: link off the identity's
	// provisional contact, relink from any other; empty when the
	// identity is already on the target, which changes nothing.
	Class    model.LinkClass
	Warnings []string
	deferred error
}

// PlanLink builds and checks the request linking the identity id, read
// at revision rev, to the contact contact (record IDs) under operation
// op, on the caller's attestation backed by reference (optional). It
// checks the revision as a channel plan does (see channels.Mode) and
// that the contact is of the identity's kind; uma verifies the
// attestation and authorizes the link when it is submitted.
func (s Service) PlanLink(ctx context.Context, id, contact string, rev model.Revision, reference string, op model.OperationID, m channels.Mode) (LinkPlan, error) {
	cur, err := s.FindIdentity(ctx, id)
	if err != nil {
		return LinkPlan{}, err
	}
	deferred, err := channels.CheckRevision("identity", id, cur.Revision, rev, m)
	if err != nil {
		return LinkPlan{}, err
	}
	target, err := s.FindContact(ctx, contact)
	if err != nil {
		return LinkPlan{}, err
	}
	if target.Kind != cur.Kind {
		return LinkPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"contact %s is a %s contact and identity %s a %s identity: an identity joins a contact of its own kind only",
			contact, target.Kind, id, cur.Kind))
	}
	req := model.LinkIdentity{
		Command:  model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Identity: cur.Ref, Contact: target.Ref, Evidence: attest(reference),
	}
	if err := req.Validate(); err != nil {
		return LinkPlan{}, err
	}
	p := LinkPlan{Request: req, Identity: cur, Target: target, deferred: deferred}
	if cur.Contact == target.Ref {
		p.Warnings = append(p.Warnings, fmt.Sprintf("identity %s is already on contact %s: committing changes nothing", id, contact))
		return p, nil
	}
	hist, err := s.History(ctx, cur.Ref)
	if err != nil {
		return LinkPlan{}, err
	}
	p.Class = model.LinkClassLink
	if len(hist) > 0 {
		p.Class = model.LinkClassRelink
	}
	return p, nil
}

// Changed is an identity link or unlink as committed: the identity as
// it is after it, the link history record it appended (nil when it
// changed nothing) and uma's acceptance.
type Changed struct {
	Identity   model.Identity
	Record     *model.IdentityLink
	Acceptance model.Acceptance
}

// Link submits p.
func (s Service) Link(ctx context.Context, p LinkPlan) (Changed, error) {
	acc, err := s.Client.Contacts().Link(ctx, p.Request)
	if err != nil {
		return Changed{}, channels.Explain(err, p.deferred)
	}
	return s.changed(ctx, p.Identity, acc)
}

// changed reads the identity id back after acc and the record acc's
// operation appended to its history.
func (s Service) changed(ctx context.Context, id model.Identity, acc model.Acceptance) (Changed, error) {
	out := Changed{Acceptance: acc}
	var err error
	if out.Identity, err = s.FindIdentity(ctx, string(id.Ref.ID)); err != nil {
		return out, err
	}
	hist, err := s.History(ctx, id.Ref)
	if err != nil {
		return out, err
	}
	for i := range hist {
		if hist[i].Operation.ID == acc.Operation.Ref.ID {
			out.Record = &hist[i]
		}
	}
	return out, nil
}

// UnlinkPlan is a validated identity unlink and the identity as it is
// now.
type UnlinkPlan struct {
	Request  model.UnlinkIdentity
	Identity model.Identity
	deferred error
}

// PlanUnlink builds and checks the request detaching the identity id,
// read at revision rev, to a fresh contact uma allocates, under
// operation op, for reason (an audit reason code, required). reference,
// when set, backs it with the caller's attestation; without it the
// unlink carries no evidence, which uma allows.
func (s Service) PlanUnlink(ctx context.Context, id, reason string, rev model.Revision, reference string, op model.OperationID, m channels.Mode) (UnlinkPlan, error) {
	if reason == "" {
		return UnlinkPlan{}, model.NewError(model.Validation, "reason: required")
	}
	cur, err := s.FindIdentity(ctx, id)
	if err != nil {
		return UnlinkPlan{}, err
	}
	deferred, err := channels.CheckRevision("identity", id, cur.Revision, rev, m)
	if err != nil {
		return UnlinkPlan{}, err
	}
	req := model.UnlinkIdentity{
		Command:  model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Identity: cur.Ref, Reason: reason,
	}
	if reference != "" {
		req.Evidence = attest(reference)
	}
	if err := req.Validate(); err != nil {
		return UnlinkPlan{}, err
	}
	return UnlinkPlan{Request: req, Identity: cur, deferred: deferred}, nil
}

// Unlink submits p.
func (s Service) Unlink(ctx context.Context, p UnlinkPlan) (Changed, error) {
	acc, err := s.Client.Contacts().Unlink(ctx, p.Request)
	if err != nil {
		return Changed{}, channels.Explain(err, p.deferred)
	}
	return s.changed(ctx, p.Identity, acc)
}
