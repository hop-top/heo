package desk_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// TestStartPlanRecipients: a start names its recipient as a route on the
// inbox's channel (in the adapter's one namespace, or a named one), as
// an identity of the inbox or as a contact with one identity there; an
// identity or contact resolves to the route uma stored for its
// conversation. A malformed recipient, a namespace the adapter does not
// declare, a scope reference in a per-contact inbox, an identity of
// another inbox and a contact with no identity there are refused.
func TestStartPlanRecipients(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	route := model.ExternalRef{Channel: w.ch, Namespace: adapterstest.Namespace, Kind: "user", ID: "cy"}

	_, err := s.PlanStart(ctx, "local", "user:cy", " ", "op-1")
	wantCode(t, err, model.Validation, "text")
	_, err = s.PlanStart(ctx, "local", "", "hi", "op-1")
	wantCode(t, err, model.Validation, "recipient")
	_, err = s.PlanStart(ctx, "nope", "user:cy", "hi", "op-1")
	wantCode(t, err, model.NotFound, "nope")
	for _, to := range []string{"cy", "user:", ":cy"} {
		_, err = s.PlanStart(ctx, "local", to, "hi", "op-1")
		wantCode(t, err, model.Validation, "want <kind>:<id>")
	}

	for _, to := range []string{"user:cy", adapterstest.Namespace + "/user:cy"} {
		p, err := s.PlanStart(ctx, "local", to, "hi", "op-1")
		if err != nil || p.Destination() != route || p.Inbox.Key != "local" || p.Adapter != adapterstest.Type ||
			p.Identity != nil || p.Request.Inbox != p.Inbox.Ref || desk.Text(p.Request.Content) != "hi" {
			t.Fatalf("plan --to %s = %+v, %v", to, p, err)
		}
	}
	_, err = s.PlanStart(ctx, "local", "com.example.other/user:cy", "hi", "op-1")
	wantCode(t, err, model.Validation, `recipient namespace "com.example.other"`)
	_, err = s.PlanStart(ctx, "local", "room:general", "hi", "op-1")
	wantCode(t, err, model.Validation, "not a room reference")

	ada := w.identityOf("ada")
	p, err := s.PlanStart(ctx, "local", "identity:"+string(ada.Ref.ID), "hi", "op-1")
	if err != nil || p.Destination() != w.local.ReplyRoute || p.Identity == nil || *p.Identity != ada.Ref || *p.Contact != ada.Contact {
		t.Fatalf("plan --to identity = %+v, %v; want route %+v", p, err, w.local.ReplyRoute)
	}
	p, err = s.PlanStart(ctx, "local", "contact:"+string(ada.Contact.ID), "hi", "op-1")
	if err != nil || p.Destination() != w.local.ReplyRoute || *p.Identity != ada.Ref {
		t.Fatalf("plan --to contact = %+v, %v", p, err)
	}
	_, err = s.PlanStart(ctx, "provider", "identity:"+string(ada.Ref.ID), "hi", "op-1")
	wantCode(t, err, model.Validation, "belongs to inbox")
	_, err = s.PlanStart(ctx, "provider", "contact:"+string(ada.Contact.ID), "hi", "op-1")
	wantCode(t, err, model.NotFound, "no identity in inbox provider")
	_, err = s.PlanStart(ctx, "local", "identity:00000000-0000-7000-8000-000000000404", "hi", "op-1")
	wantCode(t, err, model.NotFound, "identity")
}

// TestStartUnsupported: an inbox whose channel's adapter does not
// declare start_conversation cannot start a conversation, refused at
// planning (model.Unsupported) before any recipient is looked at; with
// no adapter lookup, every start is refused.
func TestStartUnsupported(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	s.Adapter = func(ctx context.Context, ref model.ChannelRef) (adapter.Descriptor, error) {
		d, err := w.adapter(ctx, ref)
		d.Capabilities = []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend}
		return d, err
	}
	_, err := s.PlanStart(ctx, "local", "user:cy", "hi", "op-1")
	wantCode(t, err, model.Unsupported, "adapter "+string(adapterstest.Type)+" cannot start a conversation")
	_, err = s.PlanStart(ctx, "local", "malformed", "hi", "op-1")
	wantCode(t, err, model.Unsupported, "cannot start a conversation")
	s.Adapter = nil
	_, err = s.PlanStart(ctx, "local", "user:cy", "hi", "op-1")
	wantCode(t, err, model.Unsupported, "adapter is unknown")
}

// TestStartSubmits: a start is queued with its message in a
// conversation the inbox's selection created, and the workers deliver
// it; a retry under the same operation replays it and sends nothing
// twice; a start to the identity of an open conversation selects that
// conversation; a viewer may plan but uma refuses its start.
func TestStartSubmits(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	p, err := s.PlanStart(ctx, "local", "user:cy", "welcome aboard", "op-start")
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.Start(ctx, p)
	if err != nil || acc.Operation.State != model.Queued || acc.Replayed || acc.Operation.ResultMessage == nil {
		t.Fatalf("start = %+v, %v", acc, err)
	}
	msg := *acc.Operation.ResultMessage
	if msg.Conversation == w.local.Ref || msg.Conversation.Inbox != p.Inbox.Ref {
		t.Fatalf("start message %+v: want a new conversation in inbox local", msg)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted {
		t.Fatalf("start delivery = %s %+v", got.State, got.Failure)
	}
	again, err := s.Start(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref || *again.Operation.ResultMessage != msg {
		t.Fatalf("retry = %+v, %v; want the replayed operation", again, err)
	}
	msgs, err := s.ListMessages(ctx, desk.Filter{Conversation: string(msg.Conversation.ID)}, model.PageRequest{Limit: 10})
	if err != nil || len(msgs.Items) != 1 || desk.Text(msgs.Items[0].Content) != "welcome aboard" {
		t.Fatalf("conversation messages = %+v, %v; want the one start", msgs.Items, err)
	}
	conv, err := s.FindConversation(ctx, string(msg.Conversation.ID))
	if err != nil || conv.ReplyRoute.ID != "cy" || conv.Status != model.ConversationOpen {
		t.Fatalf("started conversation = %+v, %v", conv, err)
	}

	ada := w.identityOf("ada")
	p, err = s.PlanStart(ctx, "local", "identity:"+string(ada.Ref.ID), "again", "op-ada")
	if err != nil {
		t.Fatal(err)
	}
	acc, err = s.Start(ctx, p)
	if err != nil || acc.Operation.ResultMessage == nil || acc.Operation.ResultMessage.Conversation != w.local.Ref {
		t.Fatalf("start to ada = %+v, %v; want her open conversation", acc, err)
	}

	v := w.as(viewer)
	p, err = v.PlanStart(ctx, "local", "user:dee", "hi", "op-viewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Start(ctx, p); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer start: %v, want unauthorized", err)
	}
}

// normalizingWorld is a world whose engine runs the fake as a factory
// that implements adapter.RecipientNormalizer, and the desk of agent
// asking it as heo's runtime does.
func normalizingWorld(t *testing.T) (*world, desk.Service) {
	t.Helper()
	var nf *adapterstest.NormalizingFactory
	w := newWorld(t, func(c *uma.Config) {
		nf = adapterstest.Normalizing(c.Adapters[0].(*adapterstest.Factory))
		c.Adapters = []adapter.Factory{nf}
	})
	s := w.as(agent)
	s.RecipientNormalizer = func(context.Context, model.ChannelRef) (adapter.RecipientNormalizer, error) { return nf, nil }
	return w, s
}

// startRoute is the recipient route of a start to id on the fake.
func startRoute(w *world, id string) model.ExternalRef {
	return model.ExternalRef{Channel: w.ch, Namespace: adapterstest.Namespace, Kind: "user", ID: id}
}

// TestStartPlanNormalizesRecipient: a start plan names the recipient
// under the spelling the adapter's RecipientNormalizer returns, as uma's
// start does, so the destination a preview shows is the one the send
// uses: the typed spelling is kept (AsTyped) only when the two differ,
// and the conversation the plan's start opens is routed to the plan's
// destination. A recipient the adapter leaves alone, and one on an
// adapter with no normalizer (or no hook), is unchanged.
func TestStartPlanNormalizesRecipient(t *testing.T) {
	w, s := normalizingWorld(t)
	ctx := t.Context()

	typed := startRoute(w, "Bob@Example.com")
	p, err := s.PlanStart(ctx, "local", "user:Bob@Example.com", "hi", "op-norm")
	if err != nil {
		t.Fatal(err)
	}
	if want := startRoute(w, "bob@example.com"); p.Destination() != want || p.AsTyped == nil || *p.AsTyped != typed {
		t.Fatalf("plan = destination %+v, as typed %v; want %+v as typed %+v", p.Destination(), p.AsTyped, want, typed)
	}
	acc, err := s.Start(ctx, p)
	if err != nil || acc.Operation.ResultMessage == nil {
		t.Fatalf("start = %+v, %v", acc, err)
	}
	conv, err := s.FindConversation(ctx, string(acc.Operation.ResultMessage.Conversation.ID))
	if err != nil || conv.ReplyRoute != p.Destination() {
		t.Fatalf("started conversation routed to %+v, %v; the plan said %+v", conv.ReplyRoute, err, p.Destination())
	}

	for _, id := range []string{"cy", "bob@example.com", "+15550100003"} {
		p, err := s.PlanStart(ctx, "local", "user:"+id, "hi", "op-same")
		if err != nil || p.Destination() != startRoute(w, id) || p.AsTyped != nil {
			t.Fatalf("plan --to user:%s = destination %+v, as typed %v, %v; want it unchanged", id, p.Destination(), p.AsTyped, err)
		}
	}

	// A factory without the interface, and a desk without the hook,
	// judge nothing.
	for name, hook := range map[string]func(context.Context, model.ChannelRef) (adapter.RecipientNormalizer, error){
		"no interface": func(context.Context, model.ChannelRef) (adapter.RecipientNormalizer, error) { return nil, nil },
		"no hook":      nil,
	} {
		plain := s
		plain.RecipientNormalizer = hook
		p, err := plain.PlanStart(ctx, "local", "user:Bob@Example.com", "hi", "op-plain")
		if err != nil || p.Destination() != typed || p.AsTyped != nil {
			t.Fatalf("%s: plan = destination %+v, as typed %v, %v; want the typed recipient", name, p.Destination(), p.AsTyped, err)
		}
	}
}

// TestStartPlanNormalizationRefusals: what uma refuses of a normalizer's
// answer on submission is refused at planning in the same code and
// words: a refusal is a validation error keeping the adapter's safe
// message (or a fixed one for any other error), and a normalized
// recipient that differs in more than its ID, or is no valid reference,
// is a permanent adapter fault.
func TestStartPlanNormalizationRefusals(t *testing.T) {
	w, s := normalizingWorld(t)
	ctx := t.Context()
	for _, tc := range []struct {
		id   string
		code model.ErrorCode
		msg  string
	}{
		{adapterstest.RefusePrefix + "x", model.Validation, "recipient: " + adapterstest.RefuseMessage},
		{adapterstest.RefuseUnsafePrefix + "x", model.Validation, "recipient: refused by the adapter"},
		{adapterstest.FaultKindPrefix + "x", model.Permanent, "adapter returned an invalid normalized recipient"},
		{adapterstest.FaultEmptyPrefix + "x", model.Permanent, "adapter returned an invalid normalized recipient"},
	} {
		_, planErr := s.PlanStart(ctx, "local", "user:"+tc.id, "hi", "op-refuse")
		wantCode(t, planErr, tc.code, tc.msg)
		if strings.Contains(planErr.Error(), "10.0.0.7") {
			t.Errorf("%s: the refusal leaks the adapter's own error: %v", tc.id, planErr)
		}
		// uma's own answer to the same start.
		_, subErr := s.Client.Conversations().Start(ctx, model.StartConversation{
			Command: w.cmd(), Inbox: w.local.Ref.Inbox, Recipient: startRoute(w, tc.id), Content: desk.TextContent("hi")})
		wantCode(t, subErr, tc.code, tc.msg)
		var pe, se *model.Error
		if !errors.As(planErr, &pe) || !errors.As(subErr, &se) || pe.Code != se.Code || pe.SafeMessage != se.SafeMessage {
			t.Errorf("%s: preview refused %v, submission %v; want the same error", tc.id, planErr, subErr)
		}
	}
}
