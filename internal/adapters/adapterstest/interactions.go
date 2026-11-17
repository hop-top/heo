package adapterstest

import (
	"cmp"
	"slices"
	"time"

	"hop.top/uma/model"
)

// InteractionType is the type key of the fake variant that reports
// interactions (it declares interaction.received) and cannot answer
// them: it declares no respond. It is the receive-only account of a
// chat provider, registered beside Type in the e2e build, so a test
// holds a channel whose interactions heo may read and may not answer.
const InteractionType model.TypeKey = "test.heo.fake.interactions"

// InjectInteraction is the structured interaction an Inject reports
// instead of a message: the sender is its actor, the Event the
// provider's interaction ID.
type InjectInteraction struct {
	// Kind is a model.Interaction* kind; it defaults to an action.
	Kind string `json:"kind,omitempty"`
	// Action is the provider's action ID of an action, Command the
	// name of a command and Arguments the text typed after it.
	Action    string `json:"action,omitempty"`
	Command   string `json:"command,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Message is the provider message the control sits on, none when
	// empty.
	Message string `json:"message,omitempty"`
	// RespondUntil is the end of the provider's respond window, none
	// when zero. A window needs a token: the fake's is made up.
	RespondUntil time.Time `json:"respond_until,omitzero"`
	// AcknowledgeFor is how long the provider gives the handler to
	// acknowledge the interaction, counted from OccurredAt; zero is
	// three seconds, as Slack gives. A test that cannot act inside three
	// seconds names a longer one.
	AcknowledgeFor time.Duration `json:"acknowledge_for,omitempty"`
}

// DeclareInteractions makes f declare interaction.received, as an
// adapter that reports button presses and slash commands does. It
// declares no respond: the fake cannot answer one. Call it before the
// engine starts.
func (f *Factory) DeclareInteractions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interactions = true
}

// events are the events f's descriptor declares beyond the core's.
func (f *Factory) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.interactions {
		return nil
	}
	return slices.Clone([]string{model.EventInteractionReceived})
}

// interactionDeadline is how long the fake's provider gives a handler to
// acknowledge an interaction.
const interactionDeadline = 3 * time.Second

// fillInteraction turns env, built for in, into an interaction
// envelope: no message, the sender as the actor.
func (i *instance) fillInteraction(env *model.Envelope, in Inject) {
	x := in.Interaction
	kind := x.Kind
	if kind == "" {
		kind = model.InteractionAction
	}
	ix := model.InteractionInput{
		ID: in.Event, Kind: kind, Action: x.Action, Command: x.Command, Arguments: x.Arguments,
		Actor: env.Sender, Deadline: env.OccurredAt.Add(cmp.Or(x.AcknowledgeFor, interactionDeadline)),
	}
	if x.Message != "" {
		ref := model.ProviderMessageRef(i.ch.Ref, x.Message)
		ix.Message = &ref
	}
	if !x.RespondUntil.IsZero() {
		ix.ResponseToken, ix.ResponseExpiresAt = "fake-response-token", x.RespondUntil
	}
	env.Kind, env.MessageID, env.Interaction = model.EnvelopeInteraction, "", &ix
}
