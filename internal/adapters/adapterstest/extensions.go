package adapterstest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/mailboxes"
	"hop.top/uma/adapter/mail/microsoft"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

// The fake variants that ship an extension effect of another adapter,
// each under its uma definition, registered beside Type in the e2e
// build: a test holds a channel of each and sees which verbs each
// enables. They stand in for the adapters that own the effects, so what
// runs against them is heo's gate and queue and not the provider's
// request.
const (
	// UpdatesType ships GitHub's issue update, github.issues.update.
	UpdatesType model.TypeKey = "test.heo.fake.updates"
	// MailboxType declares raw_read and the provider mailbox list and
	// create extensions of its own type, as the mail adapters do, over a
	// small mailbox list (see MailboxFixtures).
	MailboxType model.TypeKey = "test.heo.fake.mailbox"
	// LabelsType ships Gmail's label change, google.mail.labels.
	LabelsType model.TypeKey = "test.heo.fake.labels"
	// CategoriesType ships Outlook's category change,
	// microsoft.mail.categories.
	CategoriesType model.TypeKey = "test.heo.fake.categories"
	// DraftsType ships Gmail's reply draft, google.mail.reply_draft.
	DraftsType model.TypeKey = "test.heo.fake.drafts"
	// UnsubscribeType ships Gmail's unsubscribe, google.mail.unsubscribe.
	UnsubscribeType model.TypeKey = "test.heo.fake.unsubscribe"
	// TicketEffectType ships Zendesk's ticket update only: it declares
	// extension_execute, yet runs none of the effects above.
	TicketEffectType model.TypeKey = "test.heo.fake.ticketeffect"
	// StateType declares every provider conversation state capability
	// as a mailbox does: account scope, labels-shaped moves, star and
	// important flags, every role. It ships no effect.
	StateType model.TypeKey = "test.heo.fake.state"
	// StateFolderType declares archive, move and flag as a chat
	// workspace or a folder mailbox does: shared scope, folder-shaped
	// moves to the inbox and archive roles, and the star flag only. It
	// declares neither read_state nor trash.
	StateFolderType model.TypeKey = "test.heo.fake.statefolder"
	// ViewsType ships Slack's modal effects, slack.chat.views.open,
	// views.push and views.update, and reports the interactions that
	// carry a trigger_id. The view an open or a push shows is named back
	// by a reference, as Slack's answer does.
	ViewsType model.TypeKey = "test.heo.fake.views"
)

// EffectDefinitions are the definitions uma's adapter packages register
// for the effect named: the same ones heo registers beside the adapter
// that owns it.
func EffectDefinitions(effect string) []adapter.ExtensionDefinition {
	var all []adapter.ExtensionDefinition
	all = append(all, github.NewFactory(github.Options{}).Extensions()...)
	all = append(all, google.NewFactory(google.Options{}).Extensions()...)
	all = append(all, imap.NewFactory(imap.Options{}).Extensions()...)
	all = append(all, microsoft.NewFactory(microsoft.Options{}).Extensions()...)
	all = append(all, zendesk.NewFactory().Extensions()...)
	all = append(all, slack.NewFactory(slack.Options{}).Extensions()...)
	return slices.DeleteFunc(all, func(d adapter.ExtensionDefinition) bool { return d.Name != effect })
}

// DeclareEffect makes f declare extension_execute and ship the effect
// named (an uma definition, see EffectDefinitions), and run it: each
// effect is accepted by the provider and recorded. With LogEffects set
// it appends "effect <name> <parent> <id> <data>" to the file, parent
// and id being the route the effect was sent on ("-" when it has no
// parent). The fields are checked as the owning adapter's factory does,
// offline. Call it before the engine starts.
func (f *Factory) DeclareEffect(effect string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shipped = append(f.shipped, EffectDefinitions(effect)...)
}

// shippedEffects are the definitions f ships.
func (f *Factory) shippedEffects() []adapter.ExtensionDefinition {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.shipped)
}

// ValidateAction checks the fields of an effect f ships as the adapter
// that owns the effect does, without I/O (adapter.ActionValidator): the
// fake judges nothing else.
func (f *Factory) ValidateAction(aud model.Audience, action model.Extension) []model.ValidationIssue {
	if !slices.ContainsFunc(f.shippedEffects(), func(d adapter.ExtensionDefinition) bool { return d.Name == action.Name }) {
		return nil
	}
	f.mu.Lock()
	boxes := f.boxes
	f.mu.Unlock()
	if boxes != nil && action.Name == model.MailboxCreateName(boxes.typ) {
		if _, issue := mailboxes.CheckCreate(action, model.MailboxLabel); issue != nil {
			return []model.ValidationIssue{*issue}
		}
		return nil
	}
	var owner adapter.Factory
	switch action.Name {
	case github.ExtensionUpdate:
		owner = github.NewFactory(github.Options{})
	case google.ExtensionLabels:
		owner = google.NewFactory(google.Options{})
	case microsoft.ExtensionCategories:
		owner = microsoft.NewFactory(microsoft.Options{})
	case google.ExtensionReplyDraft, google.ExtensionUnsubscribe:
		owner = google.NewFactory(google.Options{})
	case microsoft.ExtensionReplyDraft, microsoft.ExtensionUnsubscribe:
		owner = microsoft.NewFactory(microsoft.Options{})
	case slack.ExtensionViewsOpen, slack.ExtensionViewsPush, slack.ExtensionViewsUpdate:
		owner = slack.NewFactory(slack.Options{})
	default:
		return nil
	}
	if v, ok := owner.(adapter.ActionValidator); ok {
		return v.ValidateAction(aud, action)
	}
	return nil
}

// Execute applies an effect f ships, after DeclareEffect: it records it
// and reports the provider accepted it. The provider's own checks are
// the owning adapter's, not the fake's, so the action is judged as
// ValidateAction judges it and refused as that adapter would.
func (i *instance) Execute(_ context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	if !slices.ContainsFunc(i.f.shippedEffects(), func(d adapter.ExtensionDefinition) bool { return d.Name == r.Action.Name }) {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Unsupported, SafeMessage: "the fake ships no such effect", Retry: model.NoRetry}}, nil
	}
	i.f.mu.Lock()
	boxes := i.f.boxes
	i.f.mu.Unlock()
	if boxes != nil && r.Action.Name == model.MailboxCreateName(boxes.typ) {
		return i.createMailbox(r)
	}
	if issues := i.f.ValidateAction(r.Audience, r.Action); len(issues) > 0 {
		return adapter.Outcome{State: model.Rejected, Failure: &model.Error{
			Code: model.Validation, SafeMessage: issues[0].Message, Retry: model.NoRetry}}, nil
	}
	i.f.mu.Lock()
	path := i.f.effectFile
	i.f.mu.Unlock()
	parent := r.Route.ParentID
	if parent == "" {
		parent = "-"
	}
	appendLine(path, fmt.Sprintf("effect %s %s %s %s", r.Action.Name, parent, r.Route.ID, compact(r.Action.Data)))
	out := adapter.Outcome{State: model.ProviderAccepted}
	if r.Action.Name == slack.ExtensionViewsOpen || r.Action.Name == slack.ExtensionViewsPush {
		// Slack names the view it showed, for a later update.
		out.References = []model.ExternalRef{{
			Channel: i.ch.Ref, Namespace: slack.Namespace, Kind: slack.RefKindView,
			ID: fmt.Sprintf("V%09d", viewSeq.Add(1)),
		}}
	}
	return out, nil
}

// viewSeq numbers the views the fake shows.
var viewSeq atomic.Int64

// compact is data without insignificant white space.
func compact(data json.RawMessage) string {
	var b json.RawMessage
	if err := json.Unmarshal(data, &b); err != nil {
		return string(data)
	}
	out, err := json.Marshal(b)
	if err != nil {
		return string(data)
	}
	return string(out)
}

// MailboxFixtures are the mailboxes MailboxType's fake starts with: the
// inbox, a label, a nested label and a label whose name differs from
// another's only in case.
func MailboxFixtures() []model.ProviderMailbox {
	return []model.ProviderMailbox{
		{ID: "INBOX", Name: "INBOX", Kind: model.MailboxLabel, Role: model.RoleInbox},
		{ID: "Label_1", Name: "Clients/Acme", Kind: model.MailboxLabel},
		{ID: "Label_2", Name: "Billing", Kind: model.MailboxLabel},
	}
}
