package adapterstest

import (
	"context"
	"slices"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mailboxes"
	"hop.top/uma/model"
)

// mailboxState is the provider mailbox list the fake keeps.
type mailboxState struct {
	typ   model.TypeKey
	boxes []model.ProviderMailbox
}

// DeclareMailboxes makes f declare the provider mailbox list and create
// extensions of its type, as the mail adapters do, over the mailboxes
// given: the list reads them, a create adds one by name, idempotent
// without regard to case, as the provider would. Call it after As and
// before the engine starts.
func (f *Factory) DeclareMailboxes(initial ...model.ProviderMailbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	typ := f.typ
	if typ == "" {
		typ = Type
	}
	f.boxes = &mailboxState{typ: typ, boxes: slices.Clone(initial)}
	f.shipped = append(f.shipped, mailboxes.ListDefinition(typ), mailboxes.CreateDefinition(typ))
}

// Mailboxes returns the mailboxes the fake holds now.
func (f *Factory) Mailboxes() []model.ProviderMailbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.boxes == nil {
		return nil
	}
	return slices.Clone(f.boxes.boxes)
}

// mailboxCapabilities are the capabilities the mailbox extensions need
// beside extension_execute, which any shipped effect declares.
func (f *Factory) mailboxCapabilities() []model.Capability {
	if f.boxes == nil {
		return nil
	}
	return []model.Capability{model.CapabilityExtensionQuery,
		model.Capability(model.MailboxListName(f.boxes.typ)), model.Capability(model.MailboxCreateName(f.boxes.typ))}
}

// Query answers the mailbox list read, after DeclareMailboxes
// (adapter.ExtensionReader).
func (i *instance) Query(_ context.Context, r model.QueryExtension) (model.Extension, error) {
	i.f.mu.Lock()
	defer i.f.mu.Unlock()
	if i.f.boxes == nil || r.Query.Name != model.MailboxListName(i.f.boxes.typ) {
		return model.Extension{}, model.NewError(model.Unsupported, "the fake ships no such read")
	}
	return mailboxes.Result(i.f.boxes.typ, slices.Clone(i.f.boxes.boxes))
}

// createMailbox applies a mailbox create: the box named, made when no box
// of that name holds it, and its provider reference.
func (i *instance) createMailbox(r adapter.ExecuteRequest) (adapter.Outcome, error) {
	c, issue := mailboxes.CheckCreate(r.Action, model.MailboxLabel)
	if issue != nil {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Validation, SafeMessage: issue.Message, Retry: model.NoRetry}}, nil
	}
	i.f.mu.Lock()
	defer i.f.mu.Unlock()
	st := i.f.boxes
	box, ok := mailboxes.Find(st.boxes, c.Name, c.Kind)
	if !ok {
		box = model.ProviderMailbox{ID: "Label_" + strconv.Itoa(len(st.boxes)+1), Name: c.Name, Kind: c.Kind}
		st.boxes = append(st.boxes, box)
	}
	appendLine(i.f.effectFile, "effect "+r.Action.Name+" - "+r.Route.ID+" "+compact(r.Action.Data))
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMailboxRef(i.ch.Ref, box.ID)}}, nil
}
