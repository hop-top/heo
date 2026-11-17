package desk

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// Contact writes. A contact is created, edited and merged only when a
// desk actor asks: heo never matches contacts by itself, and a merge is
// the actor's explicit decision, previewed by uma and committed at the
// revisions it reported.

// ContactAttributes are the lookup hints a contact carries, as the
// caller typed them. heo normalizes them as uma stores them (a lowercased
// email domain, an E.164 phone number); none is ever verified by a
// desk write.
type ContactAttributes struct {
	Email []string
	Phone []string
	// EmailSet and PhoneSet say, on an update, that the kind is given and
	// replaces the contact's own, an empty list clearing it; a kind left
	// unset stays as it is. A creation reads the lists alone.
	EmailSet bool
	PhoneSet bool
}

// attributes returns the attributes the lists name, normalized.
func (a ContactAttributes) attributes() ([]model.Attribute, error) {
	var out []model.Attribute
	for _, v := range a.Email {
		n, err := model.NormalizeEmail(v)
		if err != nil {
			return nil, model.NewError(model.Validation, fmt.Sprintf("email %q: %s", v, reason(err)))
		}
		out = append(out, model.Attribute{Kind: model.AttributeEmail, Value: n})
	}
	for _, v := range a.Phone {
		n, err := model.NormalizePhone(v)
		if err != nil {
			return nil, model.NewError(model.Validation, fmt.Sprintf("phone %q: %s", v, reason(err)))
		}
		out = append(out, model.Attribute{Kind: model.AttributePhone, Value: n})
	}
	return out, nil
}

func reason(err error) string {
	if me, ok := err.(*model.Error); ok && me.SafeMessage != "" {
		return me.SafeMessage
	}
	return err.Error()
}

// sortAttributes orders attributes as uma stores them: by kind, then
// value.
func sortAttributes(attrs []model.Attribute) []model.Attribute {
	out := slices.Clone(attrs)
	slices.SortFunc(out, func(a, b model.Attribute) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Value, b.Value))
	})
	return out
}

// ContactCreatePlan is a validated contact creation.
type ContactCreatePlan struct {
	Request model.CreateContact
}

// PlanContactCreate builds and checks the request creating a contact of
// kind (person when empty) named name, with attrs, under operation op.
// The contact holds no identity: only a link puts one on it.
func (s Service) PlanContactCreate(_ context.Context, kind, name string, attrs ContactAttributes, op model.OperationID) (ContactCreatePlan, error) {
	list, err := attrs.attributes()
	if err != nil {
		return ContactCreatePlan{}, err
	}
	req := model.CreateContact{
		Command: model.Command{Call: call(), Operation: op}, Tenant: s.Tenant,
		Kind: kind, DisplayName: name, Attributes: sortAttributes(list),
	}
	if err := req.Validate(); err != nil {
		return ContactCreatePlan{}, err
	}
	return ContactCreatePlan{Request: req}, nil
}

// Contacted is a contact as a write left it and uma's acceptance.
type Contacted struct {
	Contact    model.Contact
	Acceptance model.Acceptance
}

// CreateContact submits p and returns the contact it created.
func (s Service) CreateContact(ctx context.Context, p ContactCreatePlan) (Contacted, error) {
	acc, err := s.Client.Contacts().Create(ctx, p.Request)
	if err != nil {
		return Contacted{}, err
	}
	out := Contacted{Acceptance: acc}
	if acc.Operation.Result == nil {
		return out, model.NewError(model.Permanent, "contact create: uma answered with no contact")
	}
	ref, err := acc.Operation.Result.AsContact()
	if err != nil {
		return out, err
	}
	out.Contact, err = s.FindContact(ctx, string(ref.ID))
	return out, err
}

// ContactChange is what a contact update sets. A nil Name leaves the
// name as it is; Attributes replace the kinds they name.
type ContactChange struct {
	Name       *string
	Attributes ContactAttributes
}

func (c ContactChange) empty() bool {
	return c.Name == nil && !c.Attributes.EmailSet && !c.Attributes.PhoneSet
}

// ContactUpdatePlan is a validated contact update, the contact as it is
// now and what it changes.
type ContactUpdatePlan struct {
	Request  model.UpdateContact
	Current  model.Contact
	Changes  []string
	Warnings []string
	deferred error
}

// PlanContactUpdate builds and checks the request replacing the contact
// id, read at revision rev, with c applied under operation op. uma
// replaces the name and the attributes whole, so the request carries the
// contact's own values for whatever c leaves alone. A revision other
// than the contact's is a conflict, final in a preview and left to uma
// on submission (see channels.Mode).
func (s Service) PlanContactUpdate(ctx context.Context, id string, rev model.Revision, c ContactChange, op model.OperationID, m channels.Mode) (ContactUpdatePlan, error) {
	if c.empty() {
		return ContactUpdatePlan{}, model.NewError(model.Validation, "contact: nothing to change")
	}
	cur, err := s.FindContact(ctx, id)
	if err != nil {
		return ContactUpdatePlan{}, err
	}
	deferred, err := channels.CheckRevision("contact", id, cur.Revision, rev, m)
	if err != nil {
		return ContactUpdatePlan{}, err
	}
	name := cur.DisplayName
	if c.Name != nil {
		name = *c.Name
	}
	set, err := c.Attributes.attributes()
	if err != nil {
		return ContactUpdatePlan{}, err
	}
	attrs := make([]model.Attribute, 0, len(cur.Attributes)+len(set))
	for _, a := range cur.Attributes {
		if (a.Kind == model.AttributeEmail && c.Attributes.EmailSet) || (a.Kind == model.AttributePhone && c.Attributes.PhoneSet) {
			continue
		}
		attrs = append(attrs, a)
	}
	attrs = sortAttributes(append(attrs, set...))
	req := model.UpdateContact{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Contact: cur.Ref, DisplayName: name, Attributes: attrs,
	}
	if err := req.Validate(); err != nil {
		return ContactUpdatePlan{}, err
	}
	p := ContactUpdatePlan{Request: req, Current: cur, deferred: deferred}
	if name != cur.DisplayName {
		p.Changes = append(p.Changes, model.ContactChangeDisplayName)
	}
	if !slices.Equal(attrs, sortAttributes(cur.Attributes)) {
		p.Changes = append(p.Changes, model.ContactChangeAttributes)
	}
	if len(p.Changes) == 0 {
		p.Warnings = append(p.Warnings, fmt.Sprintf("contact %s already has these values: committing changes nothing", id))
	}
	if cur.MergedInto != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("contact %s was merged into %s: edit that one", id, cur.MergedInto.ID))
	}
	return p, nil
}

// UpdateContact submits p and returns the contact as it is after it.
func (s Service) UpdateContact(ctx context.Context, p ContactUpdatePlan) (Contacted, error) {
	acc, err := s.Client.Contacts().Update(ctx, p.Request)
	if err != nil {
		return Contacted{}, channels.Explain(err, p.deferred)
	}
	out := Contacted{Acceptance: acc}
	out.Contact, err = s.FindContact(ctx, string(p.Request.Contact.ID))
	return out, err
}

// MergeInput is a contact merge as the caller states it: the contacts
// by record ID, each at the revision the caller read, and the audit
// reason code. Identities, when set, names the identities the caller
// reviewed; the merge refuses to move any other set.
type MergeInput struct {
	From, Into             string
	Revision, IntoRevision model.Revision
	Reason                 string
	Identities             []string
}

// MergePlan is a validated contact merge: uma's preview of what moves
// and the request that moves exactly that.
type MergePlan struct {
	Request model.MergeContacts
	From    model.Contact
	Into    model.Contact
	// Preview is uma's: the identities and conversations a merge moves
	// and both contacts' revisions. Empty once From is merged into Into.
	Preview model.MergePreview
	// Merged says From is already merged into Into: a rerun under the
	// key of the merge that did it replays, any other key conflicts.
	Merged   bool
	Warnings []string
	deferred error
}

// PlanMerge builds and checks the request merging the contact in.From
// into in.Into under operation op. What moves is uma's PreviewMerge,
// never heo's guess: the plan carries uma's preview and a request with
// the identities it lists. A preview takes the contacts' current
// revisions when in names none; a submission needs both, and a stale
// one is a conflict, final in a preview and left to uma on submission
// (see channels.Mode).
//
// A contact already merged into in.Into is not previewed again: its
// identities are the ones its link history recorded as merged, which is
// the request a replay of the merge carries.
func (s Service) PlanMerge(ctx context.Context, in MergeInput, op model.OperationID, m channels.Mode) (MergePlan, error) {
	if in.Reason == "" {
		return MergePlan{}, model.NewError(model.Validation, "reason: required")
	}
	from, err := s.FindContact(ctx, in.From)
	if err != nil {
		return MergePlan{}, err
	}
	into, err := s.FindContact(ctx, in.Into)
	if err != nil {
		return MergePlan{}, err
	}
	rev, irev := in.Revision, in.IntoRevision
	if m == channels.Preview {
		rev, irev = cmp.Or(rev, from.Revision), cmp.Or(irev, into.Revision)
	}
	deferred, err := channels.CheckRevision("contact", in.From, from.Revision, rev, m)
	if err != nil {
		return MergePlan{}, err
	}
	d2, err := channels.CheckRevision("contact", in.Into, into.Revision, irev, m)
	if err != nil {
		return MergePlan{}, err
	}
	deferred = cmp.Or(deferred, d2)
	p := MergePlan{From: from, Into: into, deferred: deferred}
	req := model.MergeContacts{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		From:    from.Ref, Into: into.Ref, ExpectedIntoRevision: irev, Reason: in.Reason,
	}
	switch {
	case from.MergedInto != nil && *from.MergedInto == into.Ref:
		p.Merged = true
		p.deferred = model.NewError(model.Conflict, fmt.Sprintf("contact %s was already merged into %s", in.From, in.Into))
		p.Warnings = append(p.Warnings, fmt.Sprintf("contact %s is already merged into %s: nothing moves", in.From, in.Into))
		if req.Identities, err = s.mergedIdentities(ctx, from.Ref, into.Ref); err != nil {
			return MergePlan{}, err
		}
	case from.MergedInto != nil:
		return MergePlan{}, model.NewError(model.Conflict, fmt.Sprintf(
			"contact %s was merged into %s: merge that one", in.From, from.MergedInto.ID))
	case into.MergedInto != nil:
		return MergePlan{}, model.NewError(model.Conflict, fmt.Sprintf(
			"contact %s was merged into %s: merge into that one", in.Into, into.MergedInto.ID))
	default:
		pv, err := s.Client.Contacts().PreviewMerge(ctx, model.PreviewMerge{Call: call(), From: from.Ref, Into: into.Ref})
		if err != nil {
			return MergePlan{}, err
		}
		if len(in.Identities) > 0 && !sameIdentities(in.Identities, pv.Identities) {
			return MergePlan{}, model.NewError(model.Conflict, fmt.Sprintf(
				"the identities on contact %s are not the ones you reviewed: preview the merge again", in.From))
		}
		p.Preview, req.Identities = pv, pv.Identities
	}
	if err := req.Validate(); err != nil {
		return MergePlan{}, err
	}
	p.Request = req
	return p, nil
}

// sameIdentities reports whether the record IDs name exactly the
// identities of refs.
func sameIdentities(ids []string, refs []model.IdentityRef) bool {
	if len(ids) != len(refs) {
		return false
	}
	for _, r := range refs {
		if !slices.Contains(ids, string(r.ID)) {
			return false
		}
	}
	return true
}

// mergedIdentities returns the identities a merge of from into into
// moved, as the link history of from recorded them.
func (s Service) mergedIdentities(ctx context.Context, from, into model.ContactRef) ([]model.IdentityRef, error) {
	req := model.ListRequest{Call: call(), Filter: model.Filter{Contact: &from}, Page: model.PageRequest{Limit: scanPage}}
	var out []model.IdentityRef
	for {
		p, err := s.Client.Contacts().History(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, l := range p.Items {
			if l.Class == model.LinkClassMerge && l.From == from && l.To == into {
				out = append(out, l.Identity)
			}
		}
		if p.Complete || p.Next == "" {
			return out, nil
		}
		req.Page.After = p.Next
	}
}

// Merged is a contact merge as committed: both contacts as they are
// after it, the link history records it appended, one per identity
// moved, and uma's acceptance.
type Merged struct {
	From       model.Contact
	Into       model.Contact
	Links      []model.IdentityLink
	Acceptance model.Acceptance
}

// MergeContacts submits p.
func (s Service) MergeContacts(ctx context.Context, p MergePlan) (Merged, error) {
	acc, err := s.Client.Contacts().Merge(ctx, p.Request)
	if err != nil {
		return Merged{}, channels.Explain(err, p.deferred)
	}
	out := Merged{Acceptance: acc}
	if out.From, err = s.FindContact(ctx, string(p.From.Ref.ID)); err != nil {
		return out, err
	}
	if out.Into, err = s.FindContact(ctx, string(p.Into.Ref.ID)); err != nil {
		return out, err
	}
	req := model.ListRequest{Call: call(), Filter: model.Filter{Contact: &p.From.Ref}, Page: model.PageRequest{Limit: scanPage}}
	for {
		pg, err := s.Client.Contacts().History(ctx, req)
		if err != nil {
			return out, err
		}
		for _, l := range pg.Items {
			if l.Operation.ID == acc.Operation.Ref.ID {
				out.Links = append(out.Links, l)
			}
		}
		if pg.Complete || pg.Next == "" {
			return out, nil
		}
		req.Page.After = pg.Next
	}
}
