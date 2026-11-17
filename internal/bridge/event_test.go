package bridge_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/model"
)

func ext(t *testing.T, schema string, v any) model.Extension {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: schema, Version: model.PayloadVersion1, Data: raw}
}

func umaEvent(kind string, vis model.Visibility, res model.Ref, payload model.Extension) model.Event {
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: "ev-1", Kind: kind, Tenant: "acme", Inbox: "in-1",
		Resource: res, Actor: "uid:501", OccurredAt: time.Unix(100, 0).UTC(), RecordedAt: time.Unix(101, 0).UTC(),
		Correlation: "corr-1", Causation: "cause-1", Audience: model.Audience{Visibility: vis, PolicyRef: "pol-secret"},
		Revision: "rev-2", Payload: payload,
	}
}

var (
	convRef = model.ConversationRef{Inbox: model.InboxRef{Tenant: "acme", ID: "in-1"}, ID: "conv-1"}
	msgRef  = model.MessageRef{Conversation: convRef, ID: "msg-1"}
	msgRes  = model.Ref{Tenant: "acme", Kind: model.KindMessage, Inbox: "in-1", ID: "msg-1"}
)

// TestTopicsFollowKitNotation: every builtin kind maps onto a valid
// [Source].[Category].[Object].[Action] topic whose category is the
// audience, and a kind that cannot (a namespaced extension kind) maps
// onto the extension topic with its kind in the payload.
func TestTopicsFollowKitNotation(t *testing.T) {
	kinds := map[string]bus.Topic{
		model.EventMessageReceived:          "uma.public.message.received",
		model.EventMessageCreated:           "uma.public.message.created",
		model.EventMessageUpdated:           "uma.public.message.updated",
		model.EventConversationChanged:      "uma.public.conversation.changed",
		model.EventConversationLabelAdded:   "uma.public.conversation.label_added",
		model.EventConversationLabelRemoved: "uma.public.conversation.label_removed",
		model.EventConversationAssigned:     "uma.public.conversation.assigned",
		model.EventConversationUnassigned:   "uma.public.conversation.unassigned",
		model.EventConversationSnoozed:      "uma.public.conversation.snoozed",
		model.EventConversationUnsnoozed:    "uma.public.conversation.unsnoozed",
		model.EventConversationLinked:       "uma.public.conversation.linked",
		model.EventConversationUnlinked:     "uma.public.conversation.unlinked",
		model.EventDeliveryUpdated:          "uma.public.delivery.updated",
		model.EventWorkItemUpdated:          "uma.public.work_item.updated",
		model.EventIdentityLinked:           "uma.public.identity.linked",
		model.EventContactCreated:           "uma.public.contact.created",
		model.EventContactUpdated:           "uma.public.contact.updated",
		model.EventContactMerged:            "uma.public.contact.merged",
		model.EventTaskUpdated:              "uma.public.task.updated",
		model.EventInteractionReceived:      "uma.public.interaction.received",
		model.EventInteractionUpdated:       "uma.public.interaction.updated",
		"org.example.sample.thing":          "uma.public.extension.emitted",
		"Weird Kind":                        "uma.public.extension.emitted",
		"nodots":                            "uma.public.extension.emitted",
	}
	for kind, want := range kinds {
		topic, p := bridge.Map(umaEvent(kind, model.Public, msgRes, model.Extension{}))
		if topic != want {
			t.Errorf("%s: topic %q, want %q", kind, topic, want)
		}
		if err := bus.Validate(topic); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		if p.Kind != kind {
			t.Errorf("%s: payload kind %q", kind, p.Kind)
		}
		desk, _ := bridge.Map(umaEvent(kind, model.Internal, msgRes, model.Extension{}))
		if !strings.HasPrefix(string(desk), "uma.desk.") || strings.TrimPrefix(string(desk), "uma.desk.") != strings.TrimPrefix(string(want), "uma.public.") {
			t.Errorf("%s internal: topic %q", kind, desk)
		}
	}
}

// TestOnlyPublicAudienceIsPublic: an audience that is not exactly public,
// whatever it says, one this build does not know and none at all
// included, is desk-only.
func TestOnlyPublicAudienceIsPublic(t *testing.T) {
	for vis, want := range map[model.Visibility]string{
		model.Public: "uma.public.", model.Internal: "uma.desk.", model.Restricted: "uma.desk.",
		"": "uma.desk.", "secret": "uma.desk.", "PUBLIC": "uma.desk.", " public": "uma.desk.",
	} {
		topic, _ := bridge.Map(umaEvent(model.EventMessageCreated, vis, msgRes, model.Extension{}))
		if !strings.HasPrefix(string(topic), want) {
			t.Errorf("visibility %q: topic %q, want prefix %q", vis, topic, want)
		}
	}
}

// TestPayloadIsReferences: the payload names the event, its kind, the
// resource, the inbox and conversation and revision and times, and the
// message, label or operation the event is about: identifiers, nothing
// of the content.
func TestPayloadIsReferences(t *testing.T) {
	created := ext(t, model.SchemaMessageCreated, model.MessageCreatedPayload{
		Message: msgRef, Trigger: model.TriggerCommand, Origin: model.MessageOriginReply,
		Operation: model.OperationRef{Tenant: "acme", ID: "op-1"}, Source: "native.webwidget",
	})
	topic, p := bridge.Map(umaEvent(model.EventMessageCreated, model.Public, msgRes, created))
	if topic != "uma.public.message.created" {
		t.Fatalf("topic %q", topic)
	}
	want := bridge.Payload{
		Event: "ev-1", Kind: model.EventMessageCreated, Tenant: "acme", Inbox: "in-1",
		Resource:     bridge.Resource{Kind: "message", ID: "msg-1", Inbox: "in-1"},
		Conversation: "conv-1", Message: "msg-1", Operation: "op-1",
		Revision: "rev-2", OccurredAt: time.Unix(100, 0).UTC(), RecordedAt: time.Unix(101, 0).UTC(),
	}
	if p != want {
		t.Errorf("payload = %+v\nwant      %+v", p, want)
	}

	label := ext(t, model.SchemaConversationLabelAdded, model.ConversationLabelPayload{
		Conversation: convRef, Label: model.LabelRef{Tenant: "acme", ID: "lbl-1"}, Trigger: model.TriggerCommand,
		Operation: &model.OperationRef{Tenant: "acme", ID: "op-2"},
	})
	convRes := model.Ref{Tenant: "acme", Kind: model.KindConversation, Inbox: "in-1", ID: "conv-1"}
	_, lp := bridge.Map(umaEvent(model.EventConversationLabelAdded, model.Internal, convRes, label))
	if lp.Conversation != "conv-1" || lp.Label != "lbl-1" || lp.Operation != "op-2" || lp.Message != "" {
		t.Errorf("label payload = %+v", lp)
	}

	// The workflow events name the conversation and the operation that
	// made them, never who was assigned, until when, or the far end.
	op := &model.OperationRef{Tenant: "acme", ID: "op-4"}
	far := model.ConversationRef{Inbox: model.InboxRef{Tenant: "acme", ID: "in-2"}, ID: "conv-far"}
	assignee, prev := model.ActorID("uid:11"), model.ActorID("uid:12")
	until := time.Unix(5000, 0).UTC()
	for kind, payload := range map[string]model.Extension{
		model.EventConversationAssigned: ext(t, model.SchemaConversationAssigned, model.ConversationAssignmentPayload{
			Conversation: convRef, Assignee: &assignee, Previous: &prev, Trigger: model.TriggerCommand, Operation: op}),
		model.EventConversationUnassigned: ext(t, model.SchemaConversationUnassigned, model.ConversationAssignmentPayload{
			Conversation: convRef, Previous: &prev, Trigger: model.TriggerCommand, Operation: op}),
		model.EventConversationSnoozed: ext(t, model.SchemaConversationSnoozed, model.ConversationSnoozePayload{
			Conversation: convRef, Until: &until, Trigger: model.TriggerCommand, Operation: op}),
		model.EventConversationUnsnoozed: ext(t, model.SchemaConversationUnsnoozed, model.ConversationSnoozePayload{
			Conversation: convRef, Trigger: model.TriggerCommand, Operation: op}),
		model.EventConversationLinked: ext(t, model.SchemaConversationLinked, model.ConversationLinkPayload{
			Conversation: convRef, Link: model.ConversationLink{Conversation: &far}, Trigger: model.TriggerCommand, Operation: op}),
		model.EventConversationUnlinked: ext(t, model.SchemaConversationUnlinked, model.ConversationLinkPayload{
			Conversation: convRef, Link: model.ConversationLink{Conversation: &far}, Trigger: model.TriggerCommand, Operation: op}),
	} {
		topic, wp := bridge.Map(umaEvent(kind, model.Internal, convRes, payload))
		wantTopic := bus.Topic("uma.desk." + kind)
		if topic != wantTopic {
			t.Errorf("%s: topic %q, want %q", kind, topic, wantTopic)
		}
		want := bridge.Payload{
			Event: "ev-1", Kind: kind, Tenant: "acme", Inbox: "in-1",
			Resource:     bridge.Resource{Kind: "conversation", ID: "conv-1", Inbox: "in-1"},
			Conversation: "conv-1", Operation: "op-4",
			Revision: "rev-2", OccurredAt: time.Unix(100, 0).UTC(), RecordedAt: time.Unix(101, 0).UTC(),
		}
		if wp != want {
			t.Errorf("%s: payload = %+v\nwant      %+v", kind, wp, want)
		}
	}

	deliv := ext(t, model.SchemaDeliveryUpdated, model.DeliveryUpdatedPayload{
		Operation: model.OperationRef{Tenant: "acme", ID: "op-3"}, State: "failed",
		Failure: &model.DeliveryFailure{Code: model.Permanent, SafeMessage: "mailbox ada@example.com refused"},
	})
	_, dp := bridge.Map(umaEvent(model.EventDeliveryUpdated, model.Public,
		model.Ref{Tenant: "acme", Kind: model.KindOperation, ID: "op-3"}, deliv))
	if dp.Operation != "op-3" {
		t.Errorf("delivery payload = %+v", dp)
	}
}

// TestPayloadCarriesNoContent: whatever an event's payload holds, the
// bridge's payload carries none of it. The events here hold a body, an
// attachment name, contact attributes, a label name, a provider
// message, a failure text and a reaction in every field their payloads
// have, and under an extension kind and an unknown schema; not a byte
// of them is in the bridge's payload, and its fields are exactly the
// reference set.
func TestPayloadCarriesNoContent(t *testing.T) {
	secrets := []string{"BODY-SECRET", "ATTACHMENT-SECRET.pdf", "ada@example.com", "LABEL-SECRET", "NOTE-SECRET", "REACTION-SECRET",
		"uid:ASSIGNEE-SECRET", "FAR-END-SECRET"}
	hostile := map[string]any{
		"body": "BODY-SECRET", "text": "NOTE-SECRET", "name": "ATTACHMENT-SECRET.pdf", "label_name": "LABEL-SECRET",
		"attributes":   []any{map[string]any{"key": "email", "value": "ada@example.com"}},
		"reaction":     map[string]any{"emoji": "REACTION-SECRET"},
		"message":      map[string]any{"conversation": map[string]any{"id": "conv-1"}, "id": "msg-1", "content": "BODY-SECRET"},
		"conversation": map[string]any{"id": "conv-1", "title": "BODY-SECRET"},
		"label":        map[string]any{"id": "lbl-1", "name": "LABEL-SECRET"},
		"operation":    map[string]any{"id": "op-1", "note": "NOTE-SECRET"},
		"assignee":     "uid:ASSIGNEE-SECRET",
		"previous":     "uid:ASSIGNEE-SECRET",
		"link":         map[string]any{"conversation": map[string]any{"id": "FAR-END-SECRET"}, "work_item": map[string]any{"id": "FAR-END-SECRET"}},
		"until":        "2030-01-01T00:00:00Z",
	}
	for _, kind := range []string{
		model.EventMessageReceived, model.EventMessageCreated, model.EventMessageUpdated, model.EventConversationChanged,
		model.EventConversationLabelAdded, model.EventConversationLabelRemoved, model.EventDeliveryUpdated,
		model.EventConversationAssigned, model.EventConversationUnassigned, model.EventConversationSnoozed,
		model.EventConversationUnsnoozed, model.EventConversationLinked, model.EventConversationUnlinked,
		model.EventWorkItemUpdated, model.EventIdentityLinked, model.EventContactCreated, model.EventContactUpdated,
		model.EventContactMerged, model.EventTaskUpdated, model.EventInteractionReceived, model.EventInteractionUpdated,
		"org.example.sample.thing", "no.such.thing",
	} {
		for _, vis := range []model.Visibility{model.Public, model.Internal} {
			ev := umaEvent(kind, vis, msgRes, ext(t, "uma."+kind, hostile))
			topic, p := bridge.Map(ev)
			raw, err := json.Marshal(struct {
				T bus.Topic
				P bridge.Payload
			}{topic, p})
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range secrets {
				if strings.Contains(string(raw), s) {
					t.Errorf("%s/%s: %q reached the payload: %s", kind, vis, s, raw)
				}
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			_ = json.Unmarshal(mustJSON(t, p), &got)
			for k := range got {
				switch k {
				case "event", "kind", "tenant", "inbox", "resource", "conversation", "message", "label", "operation",
					"revision", "occurred_at", "recorded_at":
				default:
					t.Errorf("%s: payload field %q is not a reference", kind, k)
				}
			}
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestUnreadablePayloadStillBridged: an event whose payload names a
// schema this build does not read, or does not parse, still goes out
// with the envelope's references: a consumer re-reads the resource.
func TestUnreadablePayloadStillBridged(t *testing.T) {
	for name, payload := range map[string]model.Extension{
		"empty":      {},
		"newer":      {Name: model.SchemaMessageCreated, Version: 9, Data: json.RawMessage(`{"message":{"id":"x"}}`)},
		"malformed":  {Name: model.SchemaMessageCreated, Version: 1, Data: json.RawMessage(`{"message":`)},
		"wrong kind": {Name: model.SchemaDeliveryUpdated, Version: 1, Data: json.RawMessage(`{}`)},
	} {
		topic, p := bridge.Map(umaEvent(model.EventMessageCreated, model.Public, msgRes, payload))
		if topic != "uma.public.message.created" || p.Event != "ev-1" || p.Resource.ID != "msg-1" || p.Message != "" || p.Conversation != "" {
			t.Errorf("%s: %q %+v", name, topic, p)
		}
	}
}

// TestInteractionEventsNameTheirReferences: interaction.received names
// the conversation and the message the control sits on, and
// interaction.updated the conversation and the operation that answered;
// neither names the actor, the command, the action or what the visitor
// submitted. The interaction itself is the resource.
func TestInteractionEventsNameTheirReferences(t *testing.T) {
	res := model.Ref{Tenant: "acme", Kind: model.KindInteraction, Inbox: "in-1", ID: "ix-1"}
	ref := model.InteractionRef{Conversation: convRef, ID: "ix-1"}
	received := ext(t, model.SchemaInteractionReceived, model.InteractionReceivedPayload{
		Interaction: ref, Trigger: model.TriggerProvider, Source: "native.webwidget", Kind: model.InteractionAction,
		Actor: model.IdentityRef{Inbox: convRef.Inbox, ID: "ACTOR-SECRET"}, Action: "ACTION-SECRET", Command: "COMMAND-SECRET",
		Message: &msgRef,
	})
	topic, p := bridge.Map(umaEvent(model.EventInteractionReceived, model.Public, res, received))
	if topic != "uma.public.interaction.received" {
		t.Fatalf("topic %q", topic)
	}
	want := bridge.Payload{
		Event: "ev-1", Kind: model.EventInteractionReceived, Tenant: "acme", Inbox: "in-1",
		Resource:     bridge.Resource{Kind: "interaction", ID: "ix-1", Inbox: "in-1"},
		Conversation: "conv-1", Message: "msg-1",
		Revision: "rev-2", OccurredAt: time.Unix(100, 0).UTC(), RecordedAt: time.Unix(101, 0).UTC(),
	}
	if p != want {
		t.Errorf("received payload = %+v\nwant             %+v", p, want)
	}
	if raw := mustJSON(t, p); strings.Contains(string(raw), "SECRET") {
		t.Errorf("received payload carries a name or an identifier of the actor: %s", raw)
	}

	// A press on no message of ours names no message.
	bare := ext(t, model.SchemaInteractionReceived, model.InteractionReceivedPayload{
		Interaction: ref, Trigger: model.TriggerProvider, Kind: model.InteractionCommand, Command: "help",
	})
	if _, bp := bridge.Map(umaEvent(model.EventInteractionReceived, model.Public, res, bare)); bp.Conversation != "conv-1" || bp.Message != "" {
		t.Errorf("received without a control message = %+v", bp)
	}

	updated := ext(t, model.SchemaInteractionUpdated, model.InteractionUpdatedPayload{
		Interaction: ref, Change: model.InteractionChangeResponded, Trigger: model.TriggerCommand,
		Operation: &model.OperationRef{Tenant: "acme", ID: "op-9"},
	})
	topic, up := bridge.Map(umaEvent(model.EventInteractionUpdated, model.Internal, res, updated))
	if topic != "uma.desk.interaction.updated" {
		t.Fatalf("topic %q", topic)
	}
	if up.Conversation != "conv-1" || up.Operation != "op-9" || up.Message != "" {
		t.Errorf("updated payload = %+v", up)
	}
}
