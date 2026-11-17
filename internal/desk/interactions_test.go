package desk_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// interactionWorld is a world whose fake adapter reports interactions.
func interactionWorld(t *testing.T) *world {
	t.Helper()
	return newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareInteractions() })
}

// pressed has ada press the button action on the control message
// control (none when empty) in the local inbox and returns the record.
func (w *world) pressed(event, action, control string, mod ...func(*adapterstest.Inject)) model.Interaction {
	w.t.Helper()
	in := adapterstest.Inject{
		Event: event, From: "ada", Destination: "local",
		Interaction: &adapterstest.InjectInteraction{Action: action, Message: control},
	}
	for _, m := range mod {
		m(&in)
	}
	w.inject(in)
	p, err := w.client(agent).Interactions().List(w.t.Context(), model.ListRequest{Call: call, Page: model.PageRequest{Limit: 50}})
	if err != nil {
		w.t.Fatalf("interactions: %v", err)
	}
	for _, got := range p.Items {
		if slices.Contains(got.External, model.ProviderInteractionRef(w.ch, event)) {
			return got
		}
	}
	w.t.Fatalf("interaction %s not recorded: %+v", event, p.Items)
	return model.Interaction{}
}

// declaring is a world's adapter lookup that declares respond, and
// follow-ups too when followUp is set.
func declaring(w *world, followUp bool) func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
	return func(ctx context.Context, ref model.ChannelRef) (adapter.Descriptor, error) {
		d, err := w.adapter(ctx, ref)
		d.Capabilities = append([]model.Capability{model.CapabilityRespond}, d.Capabilities...)
		if followUp {
			d.Capabilities = append(d.Capabilities, model.CapabilityRespondFollowUp)
		}
		return d, err
	}
}

// TestInteractionsListAndGet: the desk lists the interactions a
// provider reported, narrowed by inbox, conversation and actor, and finds
// one by record ID; an unknown or malformed ID is not found. A viewer
// reads them too; an external actor reads none.
func TestInteractionsListAndGet(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	page := model.PageRequest{Limit: 10}

	if p, err := s.ListInteractions(ctx, desk.InteractionFilter{}, page); err != nil || len(p.Items) != 0 {
		t.Fatalf("list before any press = %+v, %v", p.Items, err)
	}
	first := w.pressed("ix-1", "approve", "pm-ctl")
	if first.Kind != model.InteractionAction || first.Action != "approve" || first.Ref.Conversation != w.local.Ref || first.MessageExternal == nil {
		t.Fatalf("recorded interaction = %+v", first)
	}

	all, err := s.ListInteractions(ctx, desk.InteractionFilter{}, page)
	if err != nil || len(all.Items) != 1 || all.Items[0].Ref != first.Ref {
		t.Fatalf("list = %+v, %v", all.Items, err)
	}
	for name, f := range map[string]desk.InteractionFilter{
		"inbox":        {Inbox: "local"},
		"conversation": {Conversation: string(w.local.Ref.ID)},
		"actor":        {Actor: string(first.Actor.Identity.ID)},
	} {
		if p, err := s.ListInteractions(ctx, f, page); err != nil || len(p.Items) != 1 {
			t.Errorf("list by %s = %+v, %v", name, p.Items, err)
		}
	}
	for name, f := range map[string]desk.InteractionFilter{
		"another inbox":        {Inbox: "provider"},
		"another conversation": {Conversation: string(w.remote.Ref.ID)},
	} {
		if p, err := s.ListInteractions(ctx, f, page); err != nil || len(p.Items) != 0 {
			t.Errorf("list in %s = %+v, %v", name, p.Items, err)
		}
	}
	_, err = s.ListInteractions(ctx, desk.InteractionFilter{Inbox: "nope"}, page)
	wantCode(t, err, model.NotFound, "nope")

	id := string(first.Ref.ID)
	got, err := s.FindInteraction(ctx, id, "")
	if err != nil || got.Ref != first.Ref {
		t.Fatalf("find = %+v, %v", got, err)
	}
	if got, err = s.FindInteraction(ctx, id, string(w.local.Ref.ID)); err != nil || got.Ref != first.Ref {
		t.Fatalf("find in its conversation = %+v, %v", got, err)
	}
	_, err = s.FindInteraction(ctx, id, string(w.remote.Ref.ID))
	wantCode(t, err, model.NotFound, "interaction "+id)
	_, err = s.FindInteraction(ctx, "ix_404", "")
	wantCode(t, err, model.NotFound, "interaction ix_404")
	_, err = s.FindInteraction(ctx, "a b", "")
	wantCode(t, err, model.NotFound, "interaction a b")

	if p, err := w.as(viewer).ListInteractions(ctx, desk.InteractionFilter{}, page); err != nil || len(p.Items) != 1 {
		t.Fatalf("viewer list = %+v, %v", p.Items, err)
	}
	if p, err := w.as(external).ListInteractions(ctx, desk.InteractionFilter{}, page); err == nil && len(p.Items) != 0 {
		t.Fatalf("external list = %+v: an external actor reads no interactions", p.Items)
	}
	if _, err := w.as(external).FindInteraction(ctx, id, ""); err == nil {
		t.Fatal("an external actor found an interaction")
	}
}

// TestInteractionNotPublicIsDeskOnly: an interaction whose audience is
// not public is for desk actors alone: a viewer neither lists nor finds
// it, as for any content.
func TestInteractionNotPublicIsDeskOnly(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	in := w.pressed("ix-int", "approve", "", func(i *adapterstest.Inject) { i.Audience = model.Audience{Visibility: model.Internal} })
	if _, err := w.as(agent).FindInteraction(ctx, string(in.Ref.ID), ""); err != nil {
		t.Fatalf("desk find: %v", err)
	}
	if p, err := w.as(viewer).ListInteractions(ctx, desk.InteractionFilter{}, model.PageRequest{Limit: 10}); err != nil || len(p.Items) != 0 {
		t.Fatalf("viewer list = %+v, %v; want none", p.Items, err)
	}
	_, err := w.as(viewer).FindInteraction(ctx, string(in.Ref.ID), "")
	wantCode(t, err, model.NotFound, "interaction "+string(in.Ref.ID))
}

func respondInput(mode, text string) desk.RespondInput {
	return desk.RespondInput{Mode: mode, Text: text}
}

// TestInteractionRespondPreviewRefusesUndeclared: a preview refuses an
// adapter that does not declare respond, with model.Unsupported naming
// the verb and where to look, before any plan; a follow-up needs
// respond_follow_up besides. The fake provider reports interactions and
// declares neither. A submission plans without the check: uma gates it,
// refusing the response before it claims anything.
func TestInteractionRespondPreviewRefusesUndeclared(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	in := w.pressed("ix-1", "approve", "pm-ctl")
	id := string(in.Ref.ID)

	_, err := s.PlanRespond(ctx, id, "", respondInput(model.ResponseModeMessage, "ok"), "op-r", channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare respond")
	if !strings.Contains(err.Error(), "interaction respond") || !strings.Contains(err.Error(), "heo channel get "+string(w.ch.ID)) {
		t.Errorf("refusal %q names neither the verb nor channel get", err)
	}

	// Respond declared, follow-ups not.
	s.Adapter = declaring(w, false)
	if _, err = s.PlanRespond(ctx, id, "", respondInput(model.ResponseModeMessage, "ok"), "op-r", channels.Preview); err != nil {
		t.Fatalf("preview where respond is declared: %v", err)
	}
	fu := respondInput(model.ResponseModeMessage, "ok")
	fu.FollowUp = true
	_, err = s.PlanRespond(ctx, id, "", fu, "op-r", channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare respond_follow_up")
	s.Adapter = declaring(w, true)
	if p, err := s.PlanRespond(ctx, id, "", fu, "op-r", channels.Preview); err != nil || !p.Request.FollowUp {
		t.Fatalf("follow-up preview where declared = %+v, %v", p.Request, err)
	}

	// No adapter lookup: a preview refuses everything.
	s.Adapter = nil
	_, err = s.PlanRespond(ctx, id, "", respondInput(model.ResponseModeMessage, "ok"), "op-r", channels.Preview)
	wantCode(t, err, model.Unsupported, "adapter is unknown")

	// A submission is not checked by heo; uma refuses it, and writes
	// nothing: no operation, no response claimed.
	s = w.as(agent)
	p, err := s.PlanRespond(ctx, id, "", respondInput(model.ResponseModeMessage, "ok"), "op-sub", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan: %v", err)
	}
	if p.Declaration != nil {
		t.Errorf("a submission carries a declaration: %+v", p.Declaration)
	}
	_, err = s.Respond(ctx, p)
	wantCode(t, err, model.Unsupported, "")
	got, err := s.FindInteraction(ctx, id, "")
	if err != nil || got.Response.Initial != nil || got.Response.FollowUps != 0 {
		t.Fatalf("interaction after the refused response = %+v, %v", got.Response, err)
	}
}

// TestInteractionRespondPlan: the plan carries what uma's request
// validates: the mode and the audience it implies, the text as content,
// the follow-up flag and an expiry bounded by the respond window. The
// expiry a caller does not name is derived from the record alone, so a
// retry under the same operation is the same request.
func TestInteractionRespondPlan(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	s.Adapter = declaring(w, true)
	until := w.clock.Now().Add(10 * time.Minute)
	windowed := w.pressed("ix-w", "approve", "pm-ctl", func(i *adapterstest.Inject) { i.Interaction.RespondUntil = until })
	open := w.pressed("ix-o", "approve", "pm-ctl2")
	if !windowed.RespondUntil.Equal(until) || !open.RespondUntil.IsZero() {
		t.Fatalf("respond windows = %v and %v", windowed.RespondUntil, open.RespondUntil)
	}

	for mode, wantVis := range map[string]model.Visibility{
		model.ResponseModeMessage: model.Public, model.ResponseModeUpdate: model.Public, model.ResponseModeEphemeral: model.Restricted,
	} {
		p, err := s.PlanRespond(ctx, string(windowed.Ref.ID), "", respondInput(mode, "done"), model.OperationID("op-"+mode), channels.Preview)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		r := p.Request
		if r.Mode != mode || r.Audience.Visibility != wantVis || desk.Text(r.Content) != "done" || r.Interaction != windowed.Ref || r.FollowUp {
			t.Errorf("%s: request = %+v", mode, r)
		}
		if r.Operation != model.OperationID("op-"+mode) || r.ExpiresAt.IsZero() {
			t.Errorf("%s: operation %q, expiry %v", mode, r.Operation, r.ExpiresAt)
		}
		if p.Declaration == nil || p.Declaration.Capability != model.CapabilityRespond {
			t.Errorf("%s: declaration = %+v", mode, p.Declaration)
		}
	}

	// The default expiry: the provider's window when it states one, else
	// a quarter of an hour after the record; the same on every try.
	for _, c := range []struct {
		in   model.Interaction
		want time.Time
	}{{windowed, until}, {open, open.RecordedAt.Add(desk.DefaultRespondWindow)}} {
		var seen []time.Time
		for range 2 {
			w.clock.Advance(time.Second)
			p, err := s.PlanRespond(ctx, string(c.in.Ref.ID), "", respondInput(model.ResponseModeMessage, "x"), "op-x", channels.Submit)
			if err != nil {
				t.Fatal(err)
			}
			seen = append(seen, p.Request.ExpiresAt)
		}
		if !seen[0].Equal(c.want) || !seen[1].Equal(c.want) {
			t.Errorf("default expiry of %s = %v, want %v on every try", c.in.Ref.ID, seen, c.want)
		}
	}

	// An expiry the caller names: bounded by the window, and free when the
	// provider states none.
	later := desk.RespondInput{Mode: model.ResponseModeMessage, Text: "x", ExpiresAt: until.Add(time.Minute)}
	_, err := s.PlanRespond(ctx, string(windowed.Ref.ID), "", later, "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "respond window")
	sooner := later
	sooner.ExpiresAt = until.Add(-time.Minute)
	if p, err := s.PlanRespond(ctx, string(windowed.Ref.ID), "", sooner, "op-x", channels.Preview); err != nil || !p.Request.ExpiresAt.Equal(sooner.ExpiresAt) {
		t.Fatalf("expiry inside the window = %+v, %v", p.Request.ExpiresAt, err)
	}
	if p, err := s.PlanRespond(ctx, string(open.Ref.ID), "", later, "op-x", channels.Preview); err != nil || !p.Request.ExpiresAt.Equal(later.ExpiresAt) {
		t.Fatalf("expiry where the provider states no window = %+v, %v", p.Request.ExpiresAt, err)
	}
	// A window that has passed is refused in a preview, as uma refuses it.
	w.clock.Advance(time.Hour)
	_, err = s.PlanRespond(ctx, string(windowed.Ref.ID), "", respondInput(model.ResponseModeMessage, "x"), "op-x", channels.Preview)
	wantCode(t, err, model.DeadlineExpired, "respond window")
}

// TestInteractionRespondValidation: what no adapter could take is
// refused as the caller wrote it: a mode that is not one of the three, no
// text, an update with no control message to replace, a public answer to
// an interaction that is not public.
func TestInteractionRespondValidation(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	s.Adapter = declaring(w, true)
	withControl := w.pressed("ix-1", "approve", "pm-ctl")
	noControl := w.pressed("ix-2", "approve", "")
	hidden := w.pressed("ix-3", "approve", "pm-ctl3", func(i *adapterstest.Inject) { i.Audience = model.Audience{Visibility: model.Internal} })

	_, err := s.PlanRespond(ctx, string(withControl.Ref.ID), "", respondInput("shout", "x"), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "want message, ephemeral or update")
	_, err = s.PlanRespond(ctx, string(withControl.Ref.ID), "", respondInput("", "x"), "op-x", channels.Submit)
	wantCode(t, err, model.Validation, "want message, ephemeral or update")
	_, err = s.PlanRespond(ctx, string(withControl.Ref.ID), "", respondInput(model.ResponseModeMessage, "  "), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "text")
	_, err = s.PlanRespond(ctx, "ix_404", "", respondInput(model.ResponseModeMessage, "x"), "op-x", channels.Preview)
	wantCode(t, err, model.NotFound, "interaction ix_404")

	// An update replaces the control's message: with none, it is refused.
	_, err = s.PlanRespond(ctx, string(noControl.Ref.ID), "", respondInput(model.ResponseModeUpdate, "x"), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "update")
	if _, err = s.PlanRespond(ctx, string(noControl.Ref.ID), "", respondInput(model.ResponseModeMessage, "x"), "op-x", channels.Preview); err != nil {
		t.Fatalf("a message answer needs no control message: %v", err)
	}

	// A public answer would widen the audience of an interaction that is
	// not public; an ephemeral answer, to its actor alone, never does.
	_, err = s.PlanRespond(ctx, string(hidden.Ref.ID), "", respondInput(model.ResponseModeMessage, "x"), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "ephemeral")
	_, err = s.PlanRespond(ctx, string(hidden.Ref.ID), "", respondInput(model.ResponseModeUpdate, "x"), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "ephemeral")
	if _, err = s.PlanRespond(ctx, string(hidden.Ref.ID), "", respondInput(model.ResponseModeEphemeral, "x"), "op-x", channels.Preview); err != nil {
		t.Fatalf("an ephemeral answer to a hidden interaction: %v", err)
	}
}

// TestInteractionsPassHeosAudienceRuleAgain: whatever uma returned, an
// interaction the caller's audience rule does not let it see is neither
// listed nor found, as every message and event is checked again.
func TestInteractionsPassHeosAudienceRuleAgain(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	in := w.pressed("ix-1", "approve", "")
	s := w.as(agent)
	if _, err := s.FindInteraction(ctx, string(in.Ref.ID), ""); err != nil {
		t.Fatalf("find: %v", err)
	}
	s.Sees = func(model.Audience) bool { return false }
	if p, err := s.ListInteractions(ctx, desk.InteractionFilter{}, model.PageRequest{Limit: 10}); err != nil || len(p.Items) != 0 {
		t.Fatalf("list = %+v, %v; want none", p.Items, err)
	}
	_, err := s.FindInteraction(ctx, string(in.Ref.ID), "")
	wantCode(t, err, model.NotFound, "interaction "+string(in.Ref.ID))
}

// TestInteractionRespondRefusesARedactedInteraction: a redacted
// interaction takes no response, refused in a preview as uma refuses it.
func TestInteractionRespondRefusesARedactedInteraction(t *testing.T) {
	w := interactionWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	s.Adapter = declaring(w, true)
	in := w.pressed("ix-1", "approve", "pm-ctl")
	_, err := w.client(operator).Privacy().Redact(ctx, model.Redact{
		Command: model.Command{Call: call, Operation: "op-redact", ExpectedRevision: in.Revision},
		Target:  in.Ref.Ref(), Interaction: &in.Ref, Reason: "test",
	})
	if err != nil {
		t.Fatalf("redact: %v", err)
	}
	_, err = s.PlanRespond(ctx, string(in.Ref.ID), "", respondInput(model.ResponseModeMessage, "x"), "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "redacted")
}
