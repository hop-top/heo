package adapters

import (
	"fmt"
	"slices"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

// Verb is a heo command whose effect reaches the provider, so it works
// only where the channel's adapter declares Capability. The set is
// static: a build, an uma bump or a channel never adds or removes a verb
// from heo's tree, it only decides where the verb is accepted.
type Verb struct {
	// Name is the command path, such as "message edit".
	Name string
	// Capability is the builtin capability the verb needs.
	Capability model.Capability
	// Acts are the work item transitions the adapter must list among
	// its declared WorkItemActs, besides declaring Capability; none for
	// a verb that is not a work item act.
	Acts []Act
	// Extensions are the extension effects the verb executes, for an
	// extension_execute verb: the adapter must ship at least one of them
	// (Adapter.Extensions), besides declaring Capability. Each is a way
	// to do the verb, which chooses by its arguments. extension_execute
	// alone says only that the adapter runs some effect of its own; none
	// for any other verb.
	Extensions []string
	// WorkItemUpdate marks a verb that runs the extension effect an
	// adapter ships for updating its work items' provider-owned fields:
	// the effect it authorizes as a work item act (UpdateEffects). The
	// adapter declares which one, so the verb names no provider.
	WorkItemUpdate bool
}

// Act is a work item status transition: from the status From to To.
type Act struct{ From, To string }

// providerVerbs are the provider-dependent verbs heo registers, in
// documentation order.
var providerVerbs = []Verb{
	{Name: "message edit", Capability: model.CapabilityEdit},
	{Name: "message delete", Capability: model.CapabilityDelete},
	// message raw reads the provider's whole copy of a message on demand:
	// for mail, the RFC 5322 bytes. A read; nothing is kept.
	{Name: "message raw", Capability: model.CapabilityRawRead},
	{Name: "reaction add", Capability: model.CapabilityReact},
	{Name: "reaction remove", Capability: model.CapabilityReact},
	{Name: "participant add", Capability: model.CapabilityMembership},
	{Name: "participant remove", Capability: model.CapabilityMembership},
	{Name: "receipt send", Capability: model.CapabilityReceipt},
	{Name: "history fetch", Capability: model.CapabilityHistory},
	// interaction respond answers a button press, command or form the
	// provider reported. A follow-up needs respond_follow_up besides; the
	// verb is declared by respond alone, and a follow-up is refused where
	// the adapter lacks the second.
	{Name: "interaction respond", Capability: model.CapabilityRespond},
	// The modal verbs answer an interaction with a view instead of a
	// message: Slack's modal effects, one verb for each. They are not
	// `interaction respond`: that answers at the response URL and needs
	// respond, these run an extension effect and need extension_execute
	// with the effect itself, which an adapter declaring the capability
	// for other effects does not ship.
	{Name: "interaction open-modal", Capability: model.CapabilityExtensionExecute, Extensions: []string{slack.ExtensionViewsOpen}},
	{Name: "interaction push-modal", Capability: model.CapabilityExtensionExecute, Extensions: []string{slack.ExtensionViewsPush}},
	{Name: "interaction update-modal", Capability: model.CapabilityExtensionExecute, Extensions: []string{slack.ExtensionViewsUpdate}},
	// conversation status asks the provider to move a work item (an issue
	// or a ticket) to a status: uma's work item act. The adapter declares
	// the transitions it performs; one it does not declare is refused
	// when the verb runs, and the channel's limits list the ones it does.
	{Name: "conversation status", Capability: model.CapabilityWorkItemAct},
	// conversation update changes a work item's provider-owned fields
	// (assignees, labels, milestone, a ticket's tags and the like) through
	// the update effect the adapter ships. The fields uma owns on a
	// channel (local_fields) change in uma on any work item channel, and
	// need no adapter declaration.
	{Name: "conversation update", Capability: model.CapabilityExtensionExecute, WorkItemUpdate: true},
	// conversation tag sets the mailbox's own tags on the message a mail
	// conversation replies to: Gmail's labels (--add, --remove) or
	// Outlook's categories (--set). Either effect enables the verb; the
	// flags the caller gives choose which one runs, and the channel
	// refuses the one its adapter does not ship.
	{Name: "conversation tag", Capability: model.CapabilityExtensionExecute, Extensions: []string{google.ExtensionLabels, microsoft.ExtensionCategories}},
	// message reply-draft keeps a reply as a draft in the mailbox, for a
	// person to finish and send: each mail adapter's reply_draft effect.
	{Name: "message reply-draft", Capability: model.CapabilityExtensionExecute, Extensions: []string{google.ExtensionReplyDraft, microsoft.ExtensionReplyDraft}},
	// conversation unsubscribe asks the sender of the message a mail
	// conversation replies to to stop mailing the mailbox, by what the
	// message's own headers allow: each mail adapter's unsubscribe effect.
	{Name: "conversation unsubscribe", Capability: model.CapabilityExtensionExecute, Extensions: []string{google.ExtensionUnsubscribe, microsoft.ExtensionUnsubscribe}},
	// The conversation state verbs change what the provider itself holds
	// about a conversation, apart from uma's status, labels and read
	// marker: a mailbox's archive, folder or labels, read state, star and
	// trash, a chat room's archive. One capability carries each pair of
	// verbs, and the adapter reports its limits (scope, flag kinds, move
	// shape and roles) live, which the channel's live view shows.
	{Name: "conversation archive", Capability: model.CapabilityArchive},
	{Name: "conversation unarchive", Capability: model.CapabilityArchive},
	{Name: "conversation move", Capability: model.CapabilityMove},
	{Name: "conversation read", Capability: model.CapabilityReadState},
	{Name: "conversation unread", Capability: model.CapabilityReadState},
	{Name: "conversation flag", Capability: model.CapabilityFlag},
	{Name: "conversation unflag", Capability: model.CapabilityFlag},
	{Name: "conversation trash", Capability: model.CapabilityTrash},
	{Name: "conversation untrash", Capability: model.CapabilityTrash},
}

// ProviderVerbs returns the provider-dependent verbs.
func ProviderVerbs() []Verb { return slices.Clone(providerVerbs) }

// VerbsFor returns the names of the verbs capability c enables, none for
// a capability no heo verb uses.
func VerbsFor(c model.Capability) []string {
	out := []string{}
	for _, v := range providerVerbs {
		if v.Capability == c {
			out = append(out, v.Name)
		}
	}
	return out
}

// VerbsOf returns the names of the verbs a's declarations enable, in
// documentation order.
func VerbsOf(a Adapter) []string {
	out := []string{}
	for _, v := range providerVerbs {
		if v.declaredBy(a) == "" {
			out = append(out, v.Name)
		}
	}
	return out
}

// declaredBy says why a does not declare what v needs, or "" when it
// does: the capability, for a work item act each transition v names, and
// for an extension effect the effect itself.
func (v Verb) declaredBy(a Adapter) string {
	d := a.Factory.Describe()
	if !Declares(d, v.Capability) {
		return "does not declare " + string(v.Capability)
	}
	for _, act := range v.Acts {
		if err := ActDeclared(d, act.From, act.To, ""); err != nil {
			return fmt.Sprintf("declares no %s act from %s to %s", v.Capability, act.From, act.To)
		}
	}
	if v.WorkItemUpdate && len(UpdateEffects(a.Extensions)) == 0 {
		return fmt.Sprintf("ships no %s effect authorized as a work item act", v.Capability)
	}
	if len(v.Extensions) > 0 && !slices.ContainsFunc(v.Extensions, func(name string) bool {
		return ShipsEffect(a.Extensions, name, v.Capability)
	}) {
		return fmt.Sprintf("ships no %s effect %s", v.Capability, strings.Join(v.Extensions, " or "))
	}
	return ""
}

// ShipsEffect reports whether defs hold the effect name, which needs
// capability c: what an adapter ships beside declaring the capability.
func ShipsEffect(defs []adapter.ExtensionDefinition, name string, c model.Capability) bool {
	return slices.ContainsFunc(defs, func(x adapter.ExtensionDefinition) bool {
		return x.Name == name && x.RequiredCapability == c
	})
}

// UpdateEffects returns the effects defs hold that update a work item:
// extension_execute effects authorized as a work item act.
func UpdateEffects(defs []adapter.ExtensionDefinition) []adapter.ExtensionDefinition {
	var out []adapter.ExtensionDefinition
	for _, x := range defs {
		if x.Kind == model.ExtensionKindEffect && x.RequiredCapability == model.CapabilityExtensionExecute && x.AuthorizeAs == model.ActionWorkItemAct {
			out = append(out, x)
		}
	}
	return out
}

// ActDeclared reports, as model.Unsupported, whether d does not declare
// a work item act moving a work item from status from to status to
// with reason, empty meaning the adapter's default. It reads a
// descriptor the way uma does when it accepts the act, so a preview
// never disagrees with the submission: an undeclared target, source or
// reason is refused, and nothing is coerced.
func ActDeclared(d adapter.Descriptor, from, to, reason string) error {
	if !Declares(d, model.CapabilityWorkItemAct) {
		return model.NewError(model.Unsupported, "adapter does not declare "+string(model.CapabilityWorkItemAct))
	}
	i := slices.IndexFunc(d.WorkItemActs, func(x adapter.WorkItemTransition) bool { return x.To == to })
	if i < 0 || from == "" || !slices.Contains(d.WorkItemActs[i].From, from) {
		return model.NewError(model.Unsupported, fmt.Sprintf("adapter declares no work item transition from %q to %q", from, to))
	}
	if reason != "" && !slices.Contains(d.WorkItemActs[i].Reasons, reason) {
		return model.NewError(model.Unsupported, fmt.Sprintf("adapter does not declare the status reason %q for the transition to %q", reason, to))
	}
	return nil
}

// Verb states and reasons of a channel's offline verb table.
const (
	// VerbDeclared: the channel's adapter declares the capability.
	VerbDeclared = "declared"
	// VerbUnsupported: it does not, or cannot be reached.
	VerbUnsupported = "unsupported"

	// ReasonAdapterMissing: the channel's adapter is not in this build.
	ReasonAdapterMissing = "adapter_missing"
	// ReasonNotDeclared: the adapter does not declare the capability.
	ReasonNotDeclared = "not_declared"
	// ReasonNotExecuted: the adapter declares a capability uma executes
	// no operation for. No builtin capability is in this state at the
	// pinned uma; the reason is kept for namespaced or future ones.
	ReasonNotExecuted = "not_executed"
	// ReasonConfig: the adapter declares it, and the channel's own
	// configuration switches it off.
	ReasonConfig = "config"
)

// VerbState is one provider-dependent verb as a channel's adapter
// declares it, computed offline.
type VerbState struct {
	Verb       string           `json:"verb"                 yaml:"verb"       table:"VERB"`
	Capability model.Capability `json:"capability"           yaml:"capability" table:"CAPABILITY"`
	State      string           `json:"state"                yaml:"state"      table:"STATE"`
	Reason     string           `json:"reason,omitempty"     yaml:"reason,omitempty" table:"REASON"`
	// Detail says more about a config reason: which key.
	Detail string `json:"detail,omitempty" yaml:"detail,omitempty"`
	// Limits are what the adapter declares about the verb, beyond the
	// capability: always present, empty for a verb that is not declared
	// or that the adapter declares no limits for.
	Limits VerbLimits `json:"limits" yaml:"limits"`
}

// VerbLimits are the limits uma's adapter descriptor declares for a
// verb. Every field is omitted when the adapter declares nothing for it;
// none is a default heo chose. Sizes, scopes and label rules are not
// here: uma's descriptor declares none per verb.
type VerbLimits struct {
	// Reactions is how many reactions one reactor may hold on a message,
	// "single" or "multiple": reaction verbs only.
	Reactions string `json:"reactions,omitempty" yaml:"reactions,omitempty"`
	// Transitions are the work item status changes the adapter performs:
	// `conversation status` only.
	Transitions []Transition `json:"transitions,omitempty" yaml:"transitions,omitempty"`
	// Effects are the extension effects the adapter ships that the verb
	// can run: extension_execute verbs only.
	Effects []string `json:"effects,omitempty" yaml:"effects,omitempty"`
}

// Transition is a work item status the adapter moves items to, and the
// statuses it moves them from.
type Transition struct {
	To      string   `json:"to"                yaml:"to"`
	From    []string `json:"from"              yaml:"from"`
	Reasons []string `json:"reasons,omitempty" yaml:"reasons,omitempty"`
}

// VerbStates returns the verb table of channel ch: for each verb, whether
// its adapter declares the capability. It reads the binary and the
// channel record only, no provider and no store.
func (r *Registry) VerbStates(ch model.Channel) []VerbState {
	a, ok := r.Lookup(ch.Type)
	out := make([]VerbState, 0, len(providerVerbs))
	for _, v := range providerVerbs {
		out = append(out, v.state(ch, a, ok))
	}
	return out
}

// RecipientsVerb names the flags that address a reply beyond the reply
// route: `message reply` and `message reply-draft` take them, and a
// channel carries them only where its adapter declares
// reply_recipients. They are no verb of the table above (a reply
// without them works on every channel); the channel's view lists them as
// one more row of it.
const RecipientsVerb = "message reply, message reply-draft with --reply-all, --cc, --bcc"

// recipientsVerb is the row of the flags that address a reply.
var recipientsVerb = Verb{Name: RecipientsVerb, Capability: model.CapabilityReplyRecipients}

// RecipientsState is the row of the reply recipient flags for channel
// ch: whether its adapter declares reply_recipients, computed offline
// as the verb table is.
func (r *Registry) RecipientsState(ch model.Channel) VerbState {
	a, ok := r.Lookup(ch.Type)
	return recipientsVerb.state(ch, a, ok)
}

// state is v as channel ch's adapter a (found when ok) declares it.
func (v Verb) state(ch model.Channel, a Adapter, ok bool) VerbState {
	st := VerbState{Verb: v.Name, Capability: v.Capability, State: VerbDeclared}
	switch {
	case !ok:
		st.State, st.Reason = VerbUnsupported, ReasonAdapterMissing
	case v.declaredBy(a) != "":
		st.State, st.Reason = VerbUnsupported, ReasonNotDeclared
		if Declares(a.Factory.Describe(), v.Capability) {
			// The capability is declared, but not the transitions or
			// the effect the verb needs: say which.
			st.Detail = v.declaredBy(a)
		}
	case a.CapabilityOff != nil:
		if why, off := a.CapabilityOff(ch)[v.Capability]; off {
			st.State, st.Reason, st.Detail = VerbUnsupported, ReasonConfig, why
		}
	}
	if ok && st.State == VerbDeclared {
		st.Limits = v.limits(a)
	}
	return st
}

// RequiredPermissions returns what the descriptor of channel ch's adapter
// lists as RequiredPermissions: adapter-wide, not per verb. Empty, never
// nil, when the adapter lists none or is not in this build. What an entry
// means depends on the adapter kind: OAuth scopes, bot permissions, API
// permissions or credential purposes.
func (r *Registry) RequiredPermissions(ch model.Channel) []string {
	a, ok := r.Lookup(ch.Type)
	if !ok {
		return []string{}
	}
	return append([]string{}, a.Factory.Describe().RequiredPermissions...)
}

// limits reads what a's descriptor declares for v.
func (v Verb) limits(a Adapter) VerbLimits {
	d := a.Factory.Describe()
	var l VerbLimits
	if v.Capability == model.CapabilityReact {
		l.Reactions = "multiple"
		if d.Exchange.Reactions == adapter.ReactionsSingle {
			l.Reactions = "single"
		}
	}
	if v.Capability == model.CapabilityWorkItemAct {
		for _, t := range d.WorkItemActs {
			l.Transitions = append(l.Transitions, Transition{To: t.To, From: slices.Clone(t.From), Reasons: slices.Clone(t.Reasons)})
		}
	}
	for _, x := range a.Extensions {
		if x.RequiredCapability == v.Capability && slices.Contains(v.Extensions, x.Name) {
			l.Effects = append(l.Effects, x.Name)
		}
	}
	if v.WorkItemUpdate {
		for _, x := range UpdateEffects(a.Extensions) {
			l.Effects = append(l.Effects, x.Name)
		}
	}
	return l
}
