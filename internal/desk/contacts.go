package desk

import (
	"context"
	"fmt"
	"strings"

	"hop.top/uma/model"
)

// Contacts and identities. An identity is one sender address in one
// inbox, and points at exactly one contact; ingress gives each new
// identity a provisional contact of its own. heo never matches
// identities to contacts: an identity moves to another contact only when
// a desk actor links it, on their attestation (see Attestation), and
// uma records every move in the identity's link history.

// ContactKinds lists the contact kinds.
func ContactKinds() []string {
	return []string{model.ContactKindPerson, model.ContactKindBot, model.ContactKindAgent}
}

// ListContacts returns one page of the contacts the caller may read,
// narrowed to kinds when set.
func (s Service) ListContacts(ctx context.Context, kinds []string, page model.PageRequest) (model.Page[model.Contact], error) {
	for _, k := range kinds {
		if !model.IsContactKind(k) {
			return model.Page[model.Contact]{}, model.NewError(model.Validation, fmt.Sprintf(
				"kind %q: want one of %s", k, strings.Join(ContactKinds(), ", ")))
		}
	}
	return s.Client.Contacts().List(ctx, model.ListRequest{Call: call(), Filter: model.Filter{Kinds: kinds}, Page: page})
}

// FindContact returns the contact with record ID id among those the
// caller may read.
func (s Service) FindContact(ctx context.Context, id string) (model.Contact, error) {
	if err := checkID("contact", id); err != nil {
		return model.Contact{}, err
	}
	req := model.ListRequest{Call: call(), Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Contacts().List(ctx, req)
		if err != nil {
			return model.Contact{}, err
		}
		for _, c := range p.Items {
			if string(c.Ref.ID) == id {
				return c, nil
			}
		}
		if p.Complete || p.Next == "" {
			return model.Contact{}, model.NewError(model.NotFound, fmt.Sprintf("contact %s: not found", id))
		}
		req.Page.After = p.Next
	}
}

// IdentityFilter narrows an identity listing: Contact by record ID,
// Inbox by key or ID.
type IdentityFilter struct {
	Contact string
	Inbox   string
}

func (s Service) identityFilter(ctx context.Context, f IdentityFilter) (model.Filter, error) {
	var out model.Filter
	if f.Contact != "" {
		if err := checkID("contact", f.Contact); err != nil {
			return out, err
		}
		out.Contact = &model.ContactRef{Tenant: s.Tenant, ID: model.ID(f.Contact)}
	}
	if f.Inbox != "" {
		in, err := s.FindInbox(ctx, f.Inbox)
		if err != nil {
			return out, err
		}
		out.Inboxes = []model.InboxRef{in.Ref}
	}
	return out, nil
}

// ListIdentities returns one page of the identities the caller may
// read, in the inboxes it may read, narrowed by f.
func (s Service) ListIdentities(ctx context.Context, f IdentityFilter, page model.PageRequest) (model.Page[model.Identity], error) {
	filter, err := s.identityFilter(ctx, f)
	if err != nil {
		return model.Page[model.Identity]{}, err
	}
	return s.Client.Contacts().Identities(ctx, model.ListRequest{Call: call(), Filter: filter, Page: page})
}

// ContactIdentities returns every identity of the contact ref the
// caller may read.
func (s Service) ContactIdentities(ctx context.Context, ref model.ContactRef) ([]model.Identity, error) {
	req := model.ListRequest{Call: call(), Filter: model.Filter{Contact: &ref}, Page: model.PageRequest{Limit: scanPage}}
	out := []model.Identity{}
	for {
		p, err := s.Client.Contacts().Identities(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			return out, nil
		}
		req.Page.After = p.Next
	}
}

// FindIdentity returns the identity with record ID id among those the
// caller may read.
func (s Service) FindIdentity(ctx context.Context, id string) (model.Identity, error) {
	if err := checkID("identity", id); err != nil {
		return model.Identity{}, err
	}
	req := model.ListRequest{Call: call(), Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Contacts().Identities(ctx, req)
		if err != nil {
			return model.Identity{}, err
		}
		for _, i := range p.Items {
			if string(i.Ref.ID) == id {
				return i, nil
			}
		}
		if p.Complete || p.Next == "" {
			return model.Identity{}, model.NewError(model.NotFound, fmt.Sprintf("identity %s: not found", id))
		}
		req.Page.After = p.Next
	}
}

// History returns the identity ref's link history the caller may read,
// oldest first.
func (s Service) History(ctx context.Context, ref model.IdentityRef) ([]model.IdentityLink, error) {
	req := model.ListRequest{Call: call(), Filter: model.Filter{Identity: &ref}, Page: model.PageRequest{Limit: scanPage}}
	out := []model.IdentityLink{}
	for {
		p, err := s.Client.Contacts().History(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		if p.Complete || p.Next == "" {
			return out, nil
		}
		req.Page.After = p.Next
	}
}
