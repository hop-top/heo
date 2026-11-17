package desk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"hop.top/uma/model"
)

// The hold point: between authorization and submission of a write, a
// plugin actor's operation is offered to a policy, and a held one is
// submitted to uma with a proposal (model.Command.Hold) instead of
// being queued. uma stores the held operation and counts decisions;
// heo supplies the policy, the preview and the trust key.

// HeldCreatedTopic is the bus topic heo publishes when it holds an
// operation: [source].[category].[object].[action], desk scoped.
const HeldCreatedTopic = "heo.operation.held.created"

// PreviewExtension is the name of the extension a held operation's
// preview is stored under: the verb's --dry-run output as JSON.
const PreviewExtension = "heo.preview"

// HoldQuery is what a hold policy decides on.
type HoldQuery struct {
	// Proposer is the plugin actor that wrote.
	Proposer model.ActorID
	// OnBehalfOf is the actor the proposer acts for, empty when none.
	OnBehalfOf model.ActorID
	// Channel is the mailbox: the inbox the write touches, empty for a
	// write that touches none.
	Channel string
	// Verb is the verb family (reply, state, status, fields, label, tag,
	// edit, delete, react, label-create) and Class the target class
	// within it (a label name, a state, a work item status, the fields
	// asked for, "send").
	Verb, Class string
	// Action is the uma action the write is authorized as.
	Action string
	Target model.Ref
	// Facts are what the write says about itself.
	Facts HoldFacts
}

// HoldFacts are what a write says about itself that a policy may hold
// it for, whatever the trust.
type HoldFacts struct {
	// AddedCC: a reply with recipients added beyond the original's.
	AddedCC bool
	// ReplyToDiffers: the original's Reply-To is not its From.
	ReplyToDiffers bool
	// CarriedOver: an operation carried over from an earlier batch.
	CarriedOver bool
	// FirstReplyUnknown: a reply to a contact heo never wrote to on this
	// inbox, computed by heo (PlanReplyTo), never stated by a caller.
	FirstReplyUnknown bool
}

// HoldDecision is a policy's answer.
type HoldDecision struct {
	Hold   bool
	Reason string
}

// HoldPolicy decides whether a plugin actor's write is held.
type HoldPolicy interface {
	Decide(ctx context.Context, q HoldQuery) (HoldDecision, error)
}

// holdAllReason is why HoldAll holds.
const holdAllReason = "plugin write: held for a decision"

// HoldAll is the default policy: every plugin write is held. The trust
// ladder replaces it.
type HoldAll struct{}

// Decide holds.
func (HoldAll) Decide(context.Context, HoldQuery) (HoldDecision, error) {
	return HoldDecision{Hold: true, Reason: holdAllReason}, nil
}

// HeldCreated is what is published when an operation is held:
// references and names, never the preview or any argument.
type HeldCreated struct {
	Operation string `json:"operation"`
	Kind      string `json:"kind"`
	Target    string `json:"target,omitempty"`
	Proposer  string `json:"proposer"`
	OnBehalf  string `json:"on_behalf_of,omitempty"`
	TrustKey  string `json:"trust_key"`
	Status    string `json:"status"`
}

// Holds is the hold point's configuration. A Service whose Holds is nil
// holds nothing.
type Holds struct {
	// Policy decides; nil is HoldAll.
	Policy HoldPolicy
	// Expiry is how long a held operation waits for a decision; zero
	// sets no deadline.
	Expiry time.Duration
	// OnBehalfOf is the actor the plugins act for, empty when none.
	OnBehalfOf model.ActorID
	// Emit receives the event of a newly held operation. Best effort.
	Emit func(context.Context, HeldCreated)
}

// HoldInput is what a write offers to the hold point.
type HoldInput struct {
	Action string
	Verb   string
	Class  string
	// Channel is the inbox the write touches.
	Channel string
	Target  model.Ref
	// Facts are passed to the policy as they are.
	Facts HoldFacts
	// Preview is the verb's --dry-run output. A write with none cannot
	// be held and is refused for a plugin actor when holding is on.
	Preview any
}

// holdable is the set of actions uma holds correctly: stored held with
// nothing written, applied once on approval, nothing on rejection. Each
// was probed against uma's store before it was listed. A write of any
// other action submitted with a proposal could be applied at once by
// uma, so it is refused here, before submission, as a capability error.
//
// A label create and a conversation transition are held only by a uma
// that opted them in (uma.Config.Held; the daemon does when holding is
// on); otherwise uma answers Unsupported and the write is refused.
var holdable = map[string]bool{
	model.ActionMessageEdit:   true,
	model.ActionMessageDelete: true,
	model.ActionMessageReact:  true,

	model.ActionMessageReply:           true,
	model.ActionMessageNote:            true,
	model.ActionConversationStart:      true,
	model.ActionConversationLabel:      true, // label add/remove and mailbox tag
	model.ActionLabelConfigure:         true,
	model.ActionConversationTransition: true,
	// An extension effect authorized as extension.execute: the mail
	// unsubscribe. uma stores it held, runs nothing, and applies it once
	// on approval (it writes no local record). Only a verb that offers its
	// write to Hold is held; the other effects authorized the same way
	// (the modal verbs) never reach this table and stay refused for a
	// plugin actor while holding is on (cmd's holdGuard).
	model.ActionExtensionExecute: true,
	// A provider conversation state change (archive, move, read, flag,
	// trash and their undo), authorized by the scope its channel
	// reports. uma queues it as provider work only: an approval writes
	// nothing local, a rejection or expiry leaves nothing.
	model.ActionConversationStateAccount: true,
	model.ActionConversationStateShared:  true,
	// A work item act that goes to the provider: a status the provider
	// owns, and the provider's own update effect (authorized the same
	// way). uma stores it held, queues nothing, and applies it once on
	// approval. An act of fields uma owns is not held: uma has no hold
	// for a local work item write, so HoldStatus and HoldUpdate refuse it.
	model.ActionWorkItemAct: true,
}

// CanHold reports whether uma can hold a write of action.
func CanHold(action string) bool { return holdable[action] }

func errCannotHold() error {
	return model.NewError(model.Unsupported, "verb cannot be held yet: uma has no way to apply it after approval, so a plugin actor cannot run it while held_operations.enabled")
}

// errAlreadyHeldHidden is the answer to a proposal that duplicates a
// held operation its proposer may not read: model.Conflict, so a caller
// tells it from any other refusal, and nothing about the held one.
func errAlreadyHeldHidden() error {
	return model.NewError(model.Conflict, "an operation of this kind on this target is already held; it is not shown to you, so nothing was proposed")
}

// Holding reports whether s holds the writes of its caller: holding is
// configured and the caller is a plugin actor. A person is never held.
func (s Service) Holding() bool { return s.Holds != nil && s.Actor.IsPlugin() }

// Pending is a submission that carries a hold proposal.
type Pending struct {
	h        *Holds
	proposer model.ActorID
	onBehalf model.ActorID
	trustKey string
}

// Hold consults the policy for the write described by in and, when it
// holds, sets cmd.Hold so the submission is stored held. It returns nil
// when nothing is held (holding off, a person, or the policy passes).
// A plugin write with no preview is refused: model.Unsupported.
func (s Service) Hold(ctx context.Context, cmd *model.Command, in HoldInput) (*Pending, error) {
	if !s.Holding() {
		return nil, nil
	}
	h := s.Holds
	if in.Preview == nil {
		return nil, model.NewError(model.Unsupported, fmt.Sprintf(
			"%s has no --dry-run preview: with held_operations.enabled, a plugin actor may only run verbs that can be previewed", in.Verb))
	}
	var pol HoldPolicy = HoldAll{}
	if h.Policy != nil {
		pol = h.Policy
	}
	d, err := pol.Decide(ctx, HoldQuery{
		Proposer: s.Actor, OnBehalfOf: h.OnBehalfOf, Channel: in.Channel,
		Verb: in.Verb, Class: in.Class, Action: in.Action, Target: in.Target, Facts: in.Facts,
	})
	if err != nil {
		return nil, err
	}
	if !d.Hold {
		return nil, nil
	}
	if !CanHold(in.Action) {
		return nil, errCannotHold()
	}
	raw, err := json.Marshal(in.Preview)
	if err != nil {
		return nil, fmt.Errorf("preview: %w", err)
	}
	key, labels := TrustKey(s.Actor, h.OnBehalfOf, in.Channel, in.Verb, in.Class)
	p := &model.HoldProposal{
		OnBehalfOf: h.OnBehalfOf, Reason: d.Reason, TrustKey: key, TrustLabels: labels,
		Preview: &model.Extension{Name: PreviewExtension, Version: 1, Data: raw},
	}
	if h.Expiry > 0 {
		at := s.now().Add(h.Expiry)
		p.ExpiresAt = &at
	}
	cmd.Hold = p
	return &Pending{h: h, proposer: s.Actor, onBehalf: h.OnBehalfOf, trustKey: key}, nil
}

// Settle reads the outcome of a submission made after Hold. A uma that
// cannot hold the verb (model.Unsupported) is a capability error, never
// a silent pass; a held operation is announced. It is safe on nil.
func (p *Pending) Settle(ctx context.Context, acc model.Acceptance, err error) (model.Acceptance, error) {
	if p == nil {
		return acc, err
	}
	if err != nil {
		switch {
		case model.CodeOf(err) == model.Unsupported:
			return acc, errCannotHold()
		case model.IsCode(err, model.Conflict) && strings.Contains(err.Error(), "already held"):
			return acc, errAlreadyHeldHidden()
		}
		return acc, err
	}
	if acc.Operation.State != model.Held && !acc.Replayed {
		return acc, fmt.Errorf("operation %s was %s, not held: uma did not hold a write proposed for a decision",
			acc.Operation.Ref.ID, acc.Operation.State)
	}
	if acc.Operation.State == model.Held && !acc.Replayed && p.h.Emit != nil {
		ev := HeldCreated{Operation: string(acc.Operation.Ref.ID), Proposer: string(p.proposer),
			OnBehalf: string(p.onBehalf), TrustKey: p.trustKey, Status: string(model.Held)}
		if k := acc.Operation.Key; k != nil {
			ev.Kind, ev.Target = k.Kind, string(k.Target.Kind)+"/"+string(k.Target.ID)
		}
		p.h.Emit(ctx, ev)
	}
	return acc, nil
}

// maxLabelBytes is uma's bound on a trust label name or value.
const maxLabelBytes = model.MaxTrustLabelBytes

// TrustKey builds the trust key of a write: proposer, the actor on
// whose behalf it acts, channel (mailbox), verb and target class, as
// "<proposer>/<on behalf>/<channel>/<verb>:<class>" with each part
// escaped and an empty one written "-", e.g.
// "plugin:triage/uid:1001/support/label:Finance". A key past uma's
// bound keeps its head and ends in a hash of the whole, so two long
// keys never collide. The labels list the parts for filtering.
func TrustKey(proposer, onBehalf model.ActorID, channel, verb, class string) (string, map[string]string) {
	seg := func(s string) string {
		if s == "" {
			return "-"
		}
		return strings.ReplaceAll(url.PathEscape(s), "/", "%2F")
	}
	vc := seg(verb)
	if class != "" {
		vc += ":" + seg(class)
	}
	key := strings.Join([]string{seg(string(proposer)), seg(string(onBehalf)), seg(channel), vc}, "/")
	if len(key) > model.MaxTrustKeyBytes {
		sum := sha256.Sum256([]byte(key))
		suffix := "~" + hex.EncodeToString(sum[:8])
		key = cut(key, model.MaxTrustKeyBytes-len(suffix)) + suffix
	}
	labels := map[string]string{"proposer": cut(string(proposer), maxLabelBytes), "verb": cut(verb, maxLabelBytes)}
	for k, v := range map[string]string{"on_behalf_of": string(onBehalf), "channel": channel, "class": class} {
		if v != "" {
			labels[k] = cut(v, maxLabelBytes)
		}
	}
	return key, labels
}

// cut shortens s to at most n bytes without splitting a rune.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
