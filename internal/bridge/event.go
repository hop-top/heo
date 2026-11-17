// Package bridge republishes uma's committed domain events on kit's
// in-process bus, for hooks and notifications that run beside the
// daemon.
//
// The bus is a convenience, not a log. It is in-memory, best-effort and
// not durable; it is not uma's outbox and promises no retry. A consumer
// that must see every event exactly once reads uma's event log itself
// (`heo event list`, `heo event watch`). The bridge only tails that log
// as a durable subscription of its own and republishes what it reads.
//
// What reaches the bus is a reference, never content:
//
//   - the payload names the event, its kind, the resource, the inbox,
//     the conversation, message, label or operation the event is about,
//     the revision and the times (Payload); a message body, an
//     attachment name or bytes, a contact's name or attributes and a
//     label's name are never in it;
//   - the conversation workflow events (assigned, unassigned, snoozed,
//     unsnoozed, snooze_ended, linked, unlinked) name the conversation
//     and, when a command made the change, the operation, and not who
//     was assigned, until when, why a snooze ended or the far end of a
//     link; uma emits them internal, so they go out desk only. Marking a
//     conversation read or unread emits conversation.read_marked and
//     conversation.unread_marked, which uma delivers to the marker's own
//     actor alone: the bridge reads as its own actor, never a marker's,
//     so they do not reach the bus; the mapping names the conversation
//     and not the actor, for a consumer that is the marker's actor;
//   - the audience is the topic: public events go out under
//     uma.public.<object>.<action>, everything else under
//     uma.desk.<object>.<action>, and a subscriber that is not desk
//     scoped receives the first only (Hub.Subscribe).
//
// The bridge's own health goes out under heo.runtime.bridge.<action>,
// apart from the domain events, to desk scoped subscribers only.
package bridge

import (
	"encoding/json"
	"strings"
	"time"

	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/model"
)

// Topic parts. A topic is [Source].[Category].[Object].[Action]: for a
// domain event uma, its audience, the kind's object and action.
const (
	// SourceDomain is the source of uma's domain events.
	SourceDomain = "uma"
	// CategoryPublic is the category of events anyone may know of;
	// CategoryDesk that of the rest, for desk scoped subscribers.
	CategoryPublic = "public"
	CategoryDesk   = "desk"
	// ObjectExtension and ActionEmitted name an event of a kind that
	// is not a builtin object and action pair, such as a namespaced
	// extension kind; the kind is in the payload.
	ObjectExtension = "extension"
	ActionEmitted   = "emitted"
)

// Resource is the thing an event is about.
type Resource struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Inbox string `json:"inbox,omitempty"`
}

// Payload is what a domain event carries on the bus: references only.
// A consumer reads the content through heo, which authorizes the read
// for it, so redaction, audience and per-actor rights apply there and
// not to a copy on the bus. Adding a field here adds something to every
// consumer, the file sink included: it has to be an identifier or a
// time, never content.
type Payload struct {
	// Event is uma's event ID: `heo event get <id>` reads the event.
	Event string `json:"event"`
	// Kind is uma's event kind, such as message.created.
	Kind   string `json:"kind"`
	Tenant string `json:"tenant"`
	Inbox  string `json:"inbox,omitempty"`
	// Resource is what the event is about.
	Resource Resource `json:"resource"`
	// Conversation, Message, Label and Operation are the references the
	// event's payload names, when it names them.
	Conversation string `json:"conversation,omitempty"`
	Message      string `json:"message,omitempty"`
	Label        string `json:"label,omitempty"`
	Operation    string `json:"operation,omitempty"`
	// Revision is the resource's revision after the change.
	Revision   string    `json:"revision,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Map returns the bus topic and payload for a uma event. Only an
// audience that is exactly public goes out under the public category;
// internal, restricted, absent and unknown audiences go out desk only.
func Map(ev model.Event) (bus.Topic, Payload) {
	p := Payload{
		Event: string(ev.ID), Kind: ev.Kind, Tenant: string(ev.Tenant), Inbox: string(ev.Inbox),
		Resource: Resource{Kind: string(ev.Resource.Kind), ID: string(ev.Resource.ID), Inbox: string(ev.Resource.Inbox)},
		Revision: string(ev.Revision), OccurredAt: ev.OccurredAt, RecordedAt: ev.RecordedAt,
	}
	refs(ev, &p)
	return DomainTopic(ev.Kind, ev.Audience), p
}

// DomainTopic is the topic of an event of kind under audience.
func DomainTopic(kind string, a model.Audience) bus.Topic {
	category := CategoryDesk
	if a.Visibility == model.Public {
		category = CategoryPublic
	}
	object, action := ObjectExtension, ActionEmitted
	if o, act, ok := strings.Cut(kind, "."); ok && !strings.Contains(act, ".") {
		if t := bus.Topic(strings.Join([]string{SourceDomain, category, o, act}, ".")); bus.Validate(t) == nil {
			return t
		}
	}
	return bus.Topic(strings.Join([]string{SourceDomain, category, object, action}, "."))
}

// refs fills the references an event's builtin payload names. Only
// identifiers are read from it; a payload of a schema or version this
// build does not read, or that does not parse, adds nothing.
func refs(ev model.Event, p *Payload) {
	if ev.Payload.Name != "uma."+ev.Kind || ev.Payload.Version != model.PayloadVersion1 {
		return
	}
	read := func(v any) bool { return json.Unmarshal(ev.Payload.Data, v) == nil }
	op := func(r *model.OperationRef) {
		if r != nil {
			p.Operation = string(r.ID)
		}
	}
	switch ev.Kind {
	case model.EventMessageReceived:
		var v model.MessageReceivedPayload
		if read(&v) {
			p.Conversation, p.Message = string(v.Message.Conversation.ID), string(v.Message.ID)
		}
	case model.EventInteractionReceived:
		// The interaction is the resource. Not who acted, the command,
		// the action or what was submitted: the consumer reads the
		// interaction through heo (`heo interaction get`).
		var v model.InteractionReceivedPayload
		if read(&v) {
			p.Conversation = string(v.Interaction.Conversation.ID)
			if v.Message != nil {
				p.Message = string(v.Message.ID)
			}
		}
	case model.EventInteractionUpdated:
		var v model.InteractionUpdatedPayload
		if read(&v) {
			p.Conversation = string(v.Interaction.Conversation.ID)
			op(v.Operation)
		}
	case model.EventMessageCreated:
		var v model.MessageCreatedPayload
		if read(&v) {
			p.Conversation, p.Message = string(v.Message.Conversation.ID), string(v.Message.ID)
			op(&v.Operation)
		}
	case model.EventMessageUpdated:
		var v model.MessageUpdatedPayload
		if read(&v) {
			p.Conversation, p.Message = string(v.Message.Conversation.ID), string(v.Message.ID)
			op(v.Operation)
		}
	case model.EventConversationChanged:
		var v model.ConversationChangedPayload
		if read(&v) {
			p.Conversation = string(v.Conversation.ID)
			op(v.Operation)
		}
	case model.EventConversationLabelAdded, model.EventConversationLabelRemoved:
		var v model.ConversationLabelPayload
		if read(&v) {
			p.Conversation, p.Label = string(v.Conversation.ID), string(v.Label.ID)
			op(v.Operation)
		}
	case model.EventConversationAssigned, model.EventConversationUnassigned:
		// Not who was assigned: the consumer reads that through heo.
		var v model.ConversationAssignmentPayload
		if read(&v) {
			p.Conversation = string(v.Conversation.ID)
			op(v.Operation)
		}
	case model.EventConversationSnoozed, model.EventConversationUnsnoozed:
		var v model.ConversationSnoozePayload
		if read(&v) {
			p.Conversation = string(v.Conversation.ID)
			op(v.Operation)
		}
	case model.EventConversationSnoozeEnded:
		// Not the cause or when: the consumer reads the conversation.
		var v model.ConversationSnoozeEndedPayload
		if read(&v) {
			p.Conversation = string(v.Conversation.ID)
		}
	case model.EventConversationReadMarked, model.EventConversationUnreadMarked:
		// Not whose marker: uma delivers it to that actor alone.
		var v model.ReadMarkerPayload
		if read(&v) {
			p.Conversation = string(v.Conversation.ID)
		}
	case model.EventConversationLinked, model.EventConversationUnlinked:
		// Not the far end: it may be a conversation or work item the
		// consumer may not read.
		var v model.ConversationLinkPayload
		if read(&v) {
			p.Conversation = string(v.Conversation.ID)
			op(v.Operation)
		}
	case model.EventOperationHeld, model.EventOperationDecided, model.EventOperationExpired:
		// The operation only. Not its key, its proposer, who decided or
		// what: the consumer reads the operation through heo, which
		// authorizes the read for it.
		var v model.HeldOperationPayload
		if read(&v) {
			op(&v.Operation)
		}
	case model.EventDeliveryUpdated:
		var v model.DeliveryUpdatedPayload
		if read(&v) {
			op(&v.Operation)
		}
	case model.EventWorkItemUpdated:
		var v model.WorkItemUpdatedPayload
		if read(&v) {
			p.Conversation = string(v.WorkItem.Conversation.ID)
			op(v.Operation)
		}
	case model.EventTaskUpdated:
		var v model.TaskUpdatedPayload
		if read(&v) {
			p.Conversation = string(v.Task.Conversation.ID)
			op(v.Operation)
			if v.Message != nil {
				p.Message = string(v.Message.ID)
			}
		}
	}
}
