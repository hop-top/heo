package desk

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"

	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

// Extension effects. A few provider verbs are uma extension effects, an
// adapter's own typed changes: `conversation update` runs the update
// effect an adapter ships for its work items, `conversation tag` Gmail's
// labels or Outlook's categories. uma runs
// them through Extensions.Execute, authorizes each as the policy action
// its definition names, and queues them as an operation like any other
// write. heo gates them on two things the channel's adapter says:
// that it declares extension_execute, and that it ships the effect the
// verb runs. extension_execute alone says only that the adapter runs
// some effect of its own, and uma checks no more at submission, so heo
// checks both in a preview and again before it submits.

// effectAudience is the audience of an extension effect: the operation
// and its events are the desk's business.
var effectAudience = model.Audience{Visibility: model.Internal}

// EffectDeclaration is what heo checked of the channel's adapter for an
// extension effect: the capability and the effect it ships.
type EffectDeclaration struct {
	Declaration
	// Effect is the extension effect the verb runs.
	Effect string
}

// Line is the plan's `capability` line: what was checked, and what was
// not.
func (d EffectDeclaration) Line() string {
	return fmt.Sprintf("%s declared by %s, which ships %s; live availability not checked (operator: heo channel check %s --remote)",
		d.Capability, d.Adapter, d.Effect, d.Channel.ID)
}

// effectDeclared checks, offline, that the adapter of inbox in's channel
// declares extension_execute and ships the effect name, for verb:
// model.Unsupported otherwise, naming what is missing and where to look.
// hint, when set, is added to a refusal for a missing effect: what the
// adapter does ship that does the same.
func (s Service) effectDeclared(ctx context.Context, in model.Inbox, verb, name, hint string) (EffectDeclaration, error) {
	d, _, err := s.declared(ctx, in, verb, model.CapabilityExtensionExecute)
	if err != nil {
		return EffectDeclaration{}, err
	}
	if s.Effects == nil {
		return EffectDeclaration{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: its channel's adapter effects are unknown here", channels.Name(in.Key, in.Ref.ID)))
	}
	defs, err := s.Effects(ctx, in.Channel)
	if err != nil {
		return EffectDeclaration{}, err
	}
	if !adapters.ShipsEffect(defs, name, model.CapabilityExtensionExecute) {
		msg := fmt.Sprintf("inbox %s: adapter %s declares %s and ships no effect %s, which `%s` needs; `heo channel get %s` lists the verbs the channel supports",
			channels.Name(in.Key, in.Ref.ID), d.Adapter, model.CapabilityExtensionExecute, name, verb, in.Channel.ID)
		if hint != "" {
			msg += "; " + hint
		}
		return EffectDeclaration{}, model.NewError(model.Unsupported, msg)
	}
	return EffectDeclaration{Declaration: d, Effect: name}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// MailTag is a change to the tags a mail provider keeps on a message:
// Gmail's labels, added and removed by ID, or Outlook's categories, set
// whole by name.
type MailTag struct {
	// Add and Remove are Gmail label IDs.
	Add, Remove []string
	// Set is the whole list of Outlook category names; an empty list
	// clears them. Nil when not asked.
	Set *[]string
}

// Effect is the extension effect the change runs: Outlook's categories
// for a set, Gmail's labels otherwise.
func (t MailTag) Effect() string {
	if t.Set != nil {
		return microsoft.ExtensionCategories
	}
	return google.ExtensionLabels
}

// CheckMailTag checks the change asked for: the usage rules, which need
// no record. The adapter's own rules (distinct IDs, at most 100 per
// list) are its own.
func CheckMailTag(t MailTag) error {
	switch {
	case t.Set != nil && len(t.Add)+len(t.Remove) > 0:
		return model.NewError(model.Validation, "tags: --set replaces the whole list of categories and takes no --add or --remove")
	case t.Set == nil && len(t.Add)+len(t.Remove) == 0:
		return model.NewError(model.Validation, "tags: name labels to --add or --remove (Gmail), or categories to --set (Outlook)")
	}
	return nil
}

// Data is the effect's action data, as the adapter reads it.
func (t MailTag) Data() (json.RawMessage, error) {
	if t.Set != nil {
		return json.Marshal(map[string]any{"set": nonNil(*t.Set)})
	}
	m := map[string]any{}
	if len(t.Add) > 0 {
		m["add"] = t.Add
	}
	if len(t.Remove) > 0 {
		m["remove"] = t.Remove
	}
	return json.Marshal(m)
}

// TagPlan is a validated change of a mail conversation's tags: the
// mail adapter's effect on the conversation, and the provider message
// it lands on.
type TagPlan struct {
	Request      model.ExecuteExtension
	Conversation model.Conversation
	Tag          MailTag
	// Resolved are the provider mailbox names of --add and --remove the
	// plan turned into the label IDs it sends; Tag holds the IDs.
	Resolved []Resolved
	// Message is the provider message the change acts on: the one the
	// conversation's reply route names, the latest the peer wrote.
	Message string
	// Declaration is set by a preview only.
	Declaration *EffectDeclaration
}

// PlanTag builds and checks the change of the tags on the message
// conversation id replies to, under operation op. It writes nothing. A
// conversation whose reply route names no provider message has nothing
// to tag (model.Conflict). The channel's adapter must declare
// extension_execute and ship the effect the change runs (Gmail's labels
// for --add and --remove, Outlook's categories for --set), in a
// preview and on submission; the other one is refused with a pointer to
// the flag the adapter does take. A preview also asks the adapter,
// offline, to check the fields (model.Validation); a submission leaves
// that to uma.
func (s Service) PlanTag(ctx context.Context, id string, t MailTag, op model.OperationID, mode channels.Mode) (TagPlan, error) {
	if err := CheckMailTag(t); err != nil {
		return TagPlan{}, err
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return TagPlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return TagPlan{}, err
	}
	hint := "this adapter's tags are changed with --set"
	if t.Set != nil {
		hint = "this adapter's tags are changed with --add and --remove"
	}
	d, err := s.effectDeclared(ctx, in, "conversation tag", t.Effect(), s.otherTagHint(ctx, in, t, hint))
	if err != nil {
		return TagPlan{}, err
	}
	if conv.ReplyRoute.ParentID == "" {
		return TagPlan{}, model.NewError(model.Conflict, fmt.Sprintf(
			"conversation %s: its reply route names no provider message to tag; the peer has written none", id))
	}
	var resolved []Resolved
	if t.Set == nil {
		// Gmail's labels are filed by ID: a name the account's list holds
		// is its ID.
		ids, res, err := s.resolveMailboxes(ctx, in, slices.Concat(t.Add, t.Remove))
		if err != nil {
			return TagPlan{}, err
		}
		if len(res) > 0 {
			t.Add, t.Remove, resolved = slices.Clone(ids[:len(t.Add)]), slices.Clone(ids[len(t.Add):]), res
		}
	}
	data, err := t.Data()
	if err != nil {
		return TagPlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: conv.Ref.Ref(), Audience: effectAudience,
		Action: model.Extension{Name: t.Effect(), Version: effectVersion(t), Data: data},
	}
	if err := req.Validate(); err != nil {
		return TagPlan{}, err
	}
	p := TagPlan{Request: req, Conversation: conv, Tag: t, Resolved: resolved, Message: conv.ReplyRoute.ParentID}
	if mode == channels.Preview {
		p.Declaration = &d
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return TagPlan{}, err
		}
	}
	return p, nil
}

func effectVersion(t MailTag) uint32 {
	if t.Set != nil {
		return microsoft.ExtensionVersion
	}
	return google.ExtensionVersion
}

// otherTagHint is the pointer a refusal carries when the adapter ships
// the other mail effect than the one asked for, and none otherwise.
func (s Service) otherTagHint(ctx context.Context, in model.Inbox, t MailTag, hint string) string {
	if s.Effects == nil {
		return ""
	}
	defs, err := s.Effects(ctx, in.Channel)
	if err != nil {
		return ""
	}
	other := google.ExtensionLabels
	if t.Set == nil {
		other = microsoft.ExtensionCategories
	}
	if adapters.ShipsEffect(defs, other, model.CapabilityExtensionExecute) {
		return hint
	}
	return ""
}

// TagConversation submits p: uma queues the change, and the daemon's
// workers send it to the mail provider. It returns the conversation as
// uma holds it, which the change does not touch: the tags are the
// provider's, and uma records none of them.
func (s Service) TagConversation(ctx context.Context, p TagPlan) (model.Conversation, model.Acceptance, error) {
	acc, err := s.Client.Extensions().Execute(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, err
	}
	c, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Conversation.Ref})
	return c, acc, err
}
