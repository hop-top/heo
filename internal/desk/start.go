package desk

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Starting a conversation. uma speaks first: the first message goes to
// a recipient no inbound message may have named, through an inbox, and
// the inbox's selection policy picks the recipient's open or pending
// conversation or creates one (uma Conversations.Start). Only a channel
// whose adapter declares start_conversation can do it.
//
// The recipient is the provider route uma sends on, in one of three
// forms:
//
//	<kind>:<id>              a route on the inbox's channel, in the
//	                         adapter's one identity namespace
//	<namespace>/<kind>:<id>  the same, naming the namespace
//	identity:<id>            the route of an identity of the inbox
//	                         (record ID): the one uma stored for its
//	                         conversation
//	contact:<id>             the route of the contact's one identity
//	                         in the inbox (record ID)

// Recipient prefixes naming a record rather than a route.
const (
	ToIdentity = "identity"
	ToContact  = "contact"
)

// startKinds are the route kinds a start in a scoped selection mode
// needs; scopeKinds are the kinds that scope a conversation and so are
// never a per-contact recipient. Both mirror uma's start check.
var (
	startKinds = map[string]string{model.ModePerRoom: model.RefKindRoom, model.ModePerThread: model.RefKindThread}
	scopeKinds = []string{model.RefKindRoom, model.RefKindThread, model.RefKindWorkItem}
)

// StartPlan is a validated start: the request, the inbox it leaves
// through, its channel's adapter, and the identity and contact the
// recipient was resolved from, when it was.
type StartPlan struct {
	Request  model.StartConversation
	Inbox    model.Inbox
	Adapter  model.TypeKey
	Identity *model.IdentityRef
	Contact  *model.ContactRef
	// AsTyped is the recipient as --to spelled it, set only when the
	// adapter's normalization changed it: Request.Recipient is the
	// normalized one, the route uma keys the start on. Nil when the two
	// are the same, and for a recipient taken from an identity or a
	// contact, which nothing typed.
	AsTyped *model.ExternalRef
}

// Destination is where the first message goes: the recipient route.
func (p StartPlan) Destination() model.ExternalRef { return p.Request.Recipient }

// PlanStart builds and checks the start of a conversation with to (see
// the recipient forms above) through the inbox inbox (key or ID), with
// the public text, under operation op. It checks what uma checks before
// accepting a start and heo can check without writing: the inbox
// enabled, its channel's adapter declaring start_conversation (else
// model.Unsupported), and a recipient its selection mode can route to,
// the recipient first under the spelling the adapter normalizes it to
// (adapter.RecipientNormalizer), as uma does: Request.Recipient and
// Destination are that route, and AsTyped is what --to spelled when it
// differs. uma checks it all again on submission.
func (s Service) PlanStart(ctx context.Context, inbox, to, text string, op model.OperationID) (StartPlan, error) {
	if strings.TrimSpace(text) == "" {
		return StartPlan{}, model.NewError(model.Validation, "text: required")
	}
	if strings.TrimSpace(to) == "" {
		return StartPlan{}, model.NewError(model.Validation, "to: a recipient is required")
	}
	in, err := s.FindInbox(ctx, inbox)
	if err != nil {
		return StartPlan{}, err
	}
	name := channels.Name(in.Key, in.Ref.ID)
	if s.Adapter == nil {
		return StartPlan{}, model.NewError(model.Unsupported, fmt.Sprintf("inbox %s: its channel's adapter is unknown here", name))
	}
	d, err := s.Adapter(ctx, in.Channel)
	if err != nil {
		return StartPlan{}, err
	}
	if !adapters.Declares(d, model.CapabilityStartConversation) {
		return StartPlan{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: adapter %s cannot start a conversation (it does not declare %s); `message reply` answers an existing one",
			name, d.Type, model.CapabilityStartConversation))
	}
	if in.Disabled {
		return StartPlan{}, model.NewError(model.Disabled, fmt.Sprintf("inbox %s is disabled", name))
	}
	p := StartPlan{Inbox: in, Adapter: d.Type}
	route, err := s.recipient(ctx, in, d, to, &p)
	if err != nil {
		return StartPlan{}, err
	}
	// uma normalizes the recipient before it checks the start, so the
	// plan names, and checks, the route the send will use.
	normalized, err := s.normalizeRecipient(ctx, in, route)
	if err != nil {
		return StartPlan{}, err
	}
	if normalized != route && p.Identity == nil {
		typed := route
		p.AsTyped = &typed
	}
	route = normalized
	if err := checkStartRoute(in, d, route); err != nil {
		return StartPlan{}, err
	}
	p.Request = model.StartConversation{Command: model.Command{Call: call(), Operation: op}, Inbox: in.Ref, Recipient: route, Content: TextContent(text)}
	if err := p.Request.Validate(); err != nil {
		return StartPlan{}, err
	}
	return p, nil
}

// Start submits p: uma selects or creates the conversation and queues
// the first message durably, and the daemon's workers deliver it. A
// retry under the same operation replays without selecting again.
func (s Service) Start(ctx context.Context, p StartPlan) (model.Acceptance, error) {
	return s.Client.Conversations().Start(ctx, p.Request)
}

// recipient resolves to into a route on in's channel, recording on p
// the identity and contact it came from.
func (s Service) recipient(ctx context.Context, in model.Inbox, d adapter.Descriptor, to string, p *StartPlan) (model.ExternalRef, error) {
	head, id, ok := strings.Cut(strings.TrimSpace(to), ":")
	if !ok || head == "" || id == "" {
		return model.ExternalRef{}, model.NewError(model.Validation, fmt.Sprintf(
			"to %q: want <kind>:<id>, <namespace>/<kind>:<id>, %s:<id> or %s:<id>", to, ToIdentity, ToContact))
	}
	switch head {
	case ToIdentity:
		return s.identityRoute(ctx, in, id, p)
	case ToContact:
		return s.contactRoute(ctx, in, id, p)
	}
	ns, kind, named := strings.Cut(head, "/")
	if !named {
		kind = head
		if len(d.IdentityNamespaces) != 1 {
			return model.ExternalRef{}, model.NewError(model.Validation, fmt.Sprintf(
				"to %q: adapter %s declares identity namespaces %s; name one: <namespace>/%s:%s",
				to, d.Type, orNone(d.IdentityNamespaces), kind, id))
		}
		ns = d.IdentityNamespaces[0]
	}
	return model.ExternalRef{Channel: in.Channel, Namespace: ns, Kind: kind, ID: id}, nil
}

// normalizeRecipient returns route under the spelling the inbox's
// channel adapter keys its identity on, as uma's start does before it
// checks one (services.normalizeRecipient): when the adapter factory
// implements adapter.RecipientNormalizer, and for a recipient of the
// inbox's channel; otherwise route unchanged. An error of the
// normalizer is a validation refusal that keeps its safe message
// ("recipient: ..."), or "recipient: refused by the adapter"; a result
// that differs from route in more than its ID, or is no valid
// reference, is a permanent adapter fault. Keep it in step with uma.
func (s Service) normalizeRecipient(ctx context.Context, in model.Inbox, route model.ExternalRef) (model.ExternalRef, error) {
	if s.RecipientNormalizer == nil || route.Channel != in.Channel {
		return route, nil
	}
	n, err := s.RecipientNormalizer(ctx, in.Channel)
	if err != nil {
		return model.ExternalRef{}, err
	}
	if n == nil {
		return route, nil
	}
	got, err := n.NormalizeRecipient(route)
	if err != nil {
		msg := "recipient: refused by the adapter"
		var me *model.Error
		if errors.As(err, &me) && me.SafeMessage != "" {
			msg = "recipient: " + me.SafeMessage
		}
		return model.ExternalRef{}, model.NewError(model.Validation, msg)
	}
	want := route
	want.ID = got.ID
	if got != want || got.Validate() != nil {
		return model.ExternalRef{}, model.NewError(model.Permanent, "adapter returned an invalid normalized recipient")
	}
	return got, nil
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "(none)"
	}
	return strings.Join(list, ", ")
}

// identityRoute is the route of the identity id of in: the reply route
// uma stored for a conversation the identity owns there.
func (s Service) identityRoute(ctx context.Context, in model.Inbox, id string, p *StartPlan) (model.ExternalRef, error) {
	ident, err := s.FindIdentity(ctx, id)
	if err != nil {
		return model.ExternalRef{}, err
	}
	name := channels.Name(in.Key, in.Ref.ID)
	if ident.Ref.Inbox != in.Ref {
		return model.ExternalRef{}, model.NewError(model.Validation, fmt.Sprintf(
			"identity %s belongs to inbox %s, not %s: an identity is one sender address in one inbox", id, ident.Ref.Inbox.ID, name))
	}
	req := model.ListRequest{Call: call(), Filter: model.Filter{Inboxes: []model.InboxRef{in.Ref}}, Page: model.PageRequest{Limit: scanPage}}
	for {
		page, err := s.Client.Conversations().List(ctx, req)
		if err != nil {
			return model.ExternalRef{}, err
		}
		for _, c := range page.Items {
			if c.Owner != nil && *c.Owner == ident.Ref && c.ReplyRoute.ID != "" {
				ref, contact := ident.Ref, ident.Contact
				p.Identity, p.Contact = &ref, &contact
				return c.ReplyRoute, nil
			}
		}
		if page.Complete || page.Next == "" {
			return model.ExternalRef{}, model.NewError(model.Validation, fmt.Sprintf(
				"identity %s owns no conversation in inbox %s to take its route from; pass --to <kind>:<id>", id, name))
		}
		req.Page.After = page.Next
	}
}

// contactRoute is the route of the contact id's one identity in in.
func (s Service) contactRoute(ctx context.Context, in model.Inbox, id string, p *StartPlan) (model.ExternalRef, error) {
	c, err := s.FindContact(ctx, id)
	if err != nil {
		return model.ExternalRef{}, err
	}
	all, err := s.ContactIdentities(ctx, c.Ref)
	if err != nil {
		return model.ExternalRef{}, err
	}
	var here []string
	for _, ident := range all {
		if ident.Ref.Inbox == in.Ref {
			here = append(here, string(ident.Ref.ID))
		}
	}
	name := channels.Name(in.Key, in.Ref.ID)
	switch len(here) {
	case 0:
		return model.ExternalRef{}, model.NewError(model.NotFound, fmt.Sprintf(
			"contact %s has no identity in inbox %s; pass --to <kind>:<id>", id, name))
	case 1:
		return s.identityRoute(ctx, in, here[0], p)
	}
	return model.ExternalRef{}, model.NewError(model.Validation, fmt.Sprintf(
		"contact %s has %d identities in inbox %s: pick one with --to %s:<id> (%s)", id, len(here), name, ToIdentity, strings.Join(here, ", ")))
}

// checkStartRoute checks that route can open a conversation in in, as
// uma's start check does: a per-contact inbox needs a sender route in a
// namespace the adapter declares, a per-room or per-thread inbox a room
// or thread reference; other modes cannot start a conversation.
func checkStartRoute(in model.Inbox, d adapter.Descriptor, route model.ExternalRef) error {
	if err := in.Selection.Validate(); err != nil {
		return err
	}
	switch mode := in.Selection.Mode; {
	case mode == model.ModePerContact:
		if !slices.Contains(d.IdentityNamespaces, route.Namespace) {
			return model.NewError(model.Validation, fmt.Sprintf(
				"recipient namespace %q: adapter %s declares %s", route.Namespace, d.Type, orNone(d.IdentityNamespaces)))
		}
		if slices.Contains(scopeKinds, route.Kind) {
			return model.NewError(model.Validation, fmt.Sprintf(
				"recipient: a per-contact inbox starts with a sender route, not a %s reference", route.Kind))
		}
	case startKinds[mode] != "":
		if route.Kind != startKinds[mode] {
			return model.NewError(model.Validation, fmt.Sprintf("recipient: %s selection needs a %s reference", mode, startKinds[mode]))
		}
	default:
		return model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: %s selection cannot start a conversation", channels.Name(in.Key, in.Ref.ID), mode))
	}
	return nil
}
