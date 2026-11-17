package desk_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// A Slack button press carries its trigger_id in the interaction ID.
const (
	pressEvent = "action:398738663015.47445629121.aaaa"
	modalView  = `{"type":"modal","title":{"type":"plain_text","text":"Approve"},"submit":{"type":"plain_text","text":"Go"},` +
		`"blocks":[{"type":"input","block_id":"why","label":{"type":"plain_text","text":"Why"},"element":{"type":"plain_text_input","action_id":"why"}}]}`
)

// modalWorld is a world whose fake adapter reports interactions and ships
// Slack's modal effects, the effects registered with the engine. The fake
// is not slack.chat, so the world's desk reads an interaction's trigger
// as Slack's own reader does for a Slack interaction.
func modalWorld(t *testing.T, effects ...string) *world {
	t.Helper()
	if len(effects) == 0 {
		effects = []string{slack.ExtensionViewsOpen, slack.ExtensionViewsPush, slack.ExtensionViewsUpdate}
	}
	return newWorldFake(t, func(f *adapterstest.Factory) {
		f.DeclareInteractions()
		for _, e := range effects {
			f.DeclareEffect(e)
		}
	}, func(c *uma.Config) {
		for _, f := range c.Adapters {
			if fake, ok := f.(*adapterstest.Factory); ok {
				c.Extensions = fake.Adapter().Extensions
			}
		}
	})
}

// modalDesk is actor's desk with the hooks the daemon wires for a channel
// whose adapter ships the world's effects.
func (w *world) modalDesk(actor model.ActorID) (desk.Service, *int) {
	s := w.as(actor)
	kicks := new(int)
	s.Effects = func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
		return w.fake.Adapter().Extensions, nil
	}
	s.ValidateAction = func(_ context.Context, _ model.ChannelRef, aud model.Audience, a model.Extension) ([]model.ValidationIssue, error) {
		return w.fake.ValidateAction(aud, a), nil
	}
	s.Trigger = func(in model.Interaction) (slack.Trigger, bool) {
		in.Source = slack.Type
		return slack.TriggerOf(in)
	}
	s.Kick = func() { *kicks++ }
	return s, kicks
}

// pressedNow has ada press a button at the world's clock.
func (w *world) pressedNow(event string, mod ...func(*adapterstest.Inject)) model.Interaction {
	w.t.Helper()
	at := func(i *adapterstest.Inject) { i.OccurredAt = w.clock.Now() }
	return w.pressed(event, "approve", "", append([]func(*adapterstest.Inject){at}, mod...)...)
}

func rawJSON(v string) json.RawMessage { return json.RawMessage(v) }

// effectOperations counts the extension operations the caller made.
func effectOperations(t *testing.T, s desk.Service) int {
	t.Helper()
	p, err := s.ListOperations(t.Context(), desk.OperationFilter{Kinds: []string{model.ActionExtensionExecute}}, model.PageRequest{Limit: 50})
	if err != nil {
		t.Fatalf("operations: %v", err)
	}
	return len(p.Items)
}

// TestModalOpenFromAnInteraction: an open is the Slack effect on the
// interaction's conversation, with the interaction's own trigger_id and the
// instant its acknowledgement deadline ends as expires_at, the view
// compact, as an internal operation. A preview writes nothing and says what
// it checked; the submission queues the operation once and wakes the
// workers; a retry under the same key replays.
func TestModalOpenFromAnInteraction(t *testing.T) {
	w := modalWorld(t)
	ctx := t.Context()
	s, kicks := w.modalDesk(agent)
	// The provider's own window, not three seconds from now: expires_at is
	// the interaction's, wherever heo's clock stands.
	in := w.pressedNow(pressEvent, func(i *adapterstest.Inject) { i.Interaction.AcknowledgeFor = 45 * time.Second })
	if !in.Deadline.Equal(w.clock.Now().Add(45 * time.Second)) {
		t.Fatalf("deadline = %s", in.Deadline)
	}
	id := string(in.Ref.ID)

	prev, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(modalView), "op-open", channels.Preview)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if prev.Declaration == nil || prev.Declaration.Effect != slack.ExtensionViewsOpen || prev.Declaration.Capability != model.CapabilityExtensionExecute {
		t.Errorf("preview declaration = %+v, want extension_execute with %s", prev.Declaration, slack.ExtensionViewsOpen)
	}
	if n := effectOperations(t, s); n != 0 {
		t.Fatalf("a preview queued %d operations", n)
	}

	p, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(modalView), "op-open", channels.Submit)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.Declaration != nil {
		t.Errorf("a submission carries a declaration: %+v", p.Declaration)
	}
	r := p.Request
	if r.Target != in.Ref.Conversation.Ref() || r.Audience.Visibility != model.Internal ||
		r.Action.Name != slack.ExtensionViewsOpen || r.Action.Version != slack.ExtensionVersion {
		t.Fatalf("request = %+v, want the open effect on the interaction's conversation, internal", r)
	}
	data := dataOf(t, r.Action.Data)
	wantExpiry := in.Deadline.UTC().Format(time.RFC3339Nano)
	if data["trigger_id"] != "398738663015.47445629121.aaaa" || data["expires_at"] != wantExpiry {
		t.Errorf("data = %v, want the interaction's trigger_id expiring at its deadline %s", data, wantExpiry)
	}
	if !p.ExpiresAt.Equal(in.Deadline) {
		t.Errorf("plan expires %s, want the interaction's deadline %s", p.ExpiresAt, in.Deadline)
	}
	if strings.Contains(string(r.Action.Data), "  ") {
		t.Errorf("view is not compact: %s", r.Action.Data)
	}

	acc, err := s.SubmitModal(ctx, p)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if acc.Replayed || *kicks != 1 {
		t.Errorf("submit replayed=%v kicks=%d, want a first acceptance that woke the workers once", acc.Replayed, *kicks)
	}
	if n := effectOperations(t, s); n != 1 {
		t.Fatalf("operations = %d after the submission, want 1", n)
	}
	again, err := s.SubmitModal(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry under the same key = %+v, %v; want the same operation, replayed", again, err)
	}
	if n := effectOperations(t, s); n != 1 {
		t.Errorf("a retry queued another operation: %d", n)
	}

	// The same key with another view is another request.
	other := strings.Replace(modalView, "Approve", "Reject", 1)
	q, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(other), "op-open", channels.Submit)
	if err != nil {
		t.Fatalf("plan with another view: %v", err)
	}
	_, err = s.SubmitModal(ctx, q)
	wantCode(t, err, model.Conflict, "")
}

// TestModalPushIsTheOtherEffect: a push takes the same fields as an
// open and runs the push effect.
func TestModalPushIsTheOtherEffect(t *testing.T) {
	w := modalWorld(t)
	s, kicks := w.modalDesk(agent)
	in := w.pressedNow(pressEvent)
	p, err := s.PlanModal(t.Context(), slack.ExtensionViewsPush, string(in.Ref.ID), "", rawJSON(modalView), "op-push", channels.Preview)
	if err != nil || p.Request.Action.Name != slack.ExtensionViewsPush {
		t.Fatalf("push plan = %+v, %v", p.Request.Action, err)
	}
	if p.Declaration == nil || p.Declaration.Effect != slack.ExtensionViewsPush {
		t.Errorf("declaration = %+v, want the push effect", p.Declaration)
	}
	p, err = s.PlanModal(t.Context(), slack.ExtensionViewsPush, string(in.Ref.ID), "", rawJSON(modalView), "op-push", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitModal(t.Context(), p); err != nil || *kicks != 1 {
		t.Fatalf("submit = %v, kicks %d", err, *kicks)
	}
	// Not an effect of this verb.
	_, err = s.PlanModal(t.Context(), slack.ExtensionViewsUpdate, string(in.Ref.ID), "", rawJSON(modalView), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "effect")
}

// TestModalRefusedPastTheDeadline: heo refuses locally an interaction
// whose trigger_id has stopped working, at the preview and at the
// submission, with the instant it ended, and queues nothing. The
// instant is the deadline itself: at it, the adapter refuses too.
func TestModalRefusedPastTheDeadline(t *testing.T) {
	w := modalWorld(t)
	ctx := t.Context()
	s, kicks := w.modalDesk(agent)
	in := w.pressedNow(pressEvent)
	id := string(in.Ref.ID)

	w.clock.Advance(2999 * time.Millisecond)
	if _, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(modalView), "op-late", channels.Preview); err != nil {
		t.Fatalf("a millisecond inside the deadline: %v", err)
	}
	w.clock.Advance(time.Millisecond)
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(modalView), "op-late", mode)
		wantCode(t, err, model.DeadlineExpired, in.Deadline.UTC().Format(time.RFC3339))
		if !strings.Contains(err.Error(), "trigger_id") {
			t.Errorf("%v: the refusal does not say what expired", err)
		}
	}
	if n, k := effectOperations(t, s), *kicks; n != 0 || k != 0 {
		t.Errorf("a late attempt queued %d operations and woke the workers %d times", n, k)
	}
	// A retry under a key whose first attempt got through is told where to look.
	_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(modalView), "op-late", channels.Submit)
	if !strings.Contains(err.Error(), "operation get --key") {
		t.Errorf("%v: no pointer to the operation an earlier attempt may have queued", err)
	}
}

// TestModalRespectsTheRespondWindow: an interaction whose provider window
// has ended takes no modal either, whatever its trigger says.
func TestModalRespectsTheRespondWindow(t *testing.T) {
	w := modalWorld(t)
	s, _ := w.modalDesk(agent)
	in := w.pressedNow(pressEvent, func(i *adapterstest.Inject) {
		i.Interaction.RespondUntil = w.clock.Now().Add(-time.Second)
	})
	_, err := s.PlanModal(t.Context(), slack.ExtensionViewsOpen, string(in.Ref.ID), "", rawJSON(modalView), "op-x", channels.Submit)
	wantCode(t, err, model.DeadlineExpired, "respond window")
}

// TestModalRefusesWhatTheEngineDoesNotRecheck: the interaction must
// exist, be readable by the caller, be unredacted and carry a
// trigger_id; the engine rechecks none of it, so heo does.
func TestModalRefusesWhatTheEngineDoesNotRecheck(t *testing.T) {
	w := modalWorld(t)
	ctx := t.Context()
	s, _ := w.modalDesk(agent)
	plan := func(s desk.Service, id string, mode channels.Mode) error {
		_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(modalView), "op-x", mode)
		return err
	}

	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		wantCode(t, plan(s, "ix_404", mode), model.NotFound, "interaction ix_404")
		wantCode(t, plan(s, "a b", mode), model.NotFound, "interaction a b")
	}

	// Redacted: the provider reference survives, the interaction cannot be used.
	red := w.pressedNow("action:398738663015.47445629121.redd")
	if _, err := w.client(operator).Privacy().Redact(ctx, model.Redact{
		Command: model.Command{Call: call, Operation: "op-redact", ExpectedRevision: red.Revision},
		Target:  red.Ref.Ref(), Interaction: &red.Ref, Reason: "test",
	}); err != nil {
		t.Fatalf("redact: %v", err)
	}
	if got, ok := slackTrigger(w, red); !ok || got.ID == "" {
		t.Fatalf("a redacted interaction still names its trigger (%v): the test would pass for the wrong reason", ok)
	}
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		wantCode(t, plan(s, string(red.Ref.ID), mode), model.Conflict, "redacted")
	}

	// No trigger_id: Slack named the interaction by a digest of its payload.
	bare := w.pressedNow("action:sha256-0123456789abcdef")
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		wantCode(t, plan(s, string(bare.Ref.ID), mode), model.Validation, "trigger_id")
	}

	// An interaction that is not public is the desk's alone.
	hidden := w.pressedNow("action:398738663015.47445629121.hddn", func(i *adapterstest.Inject) {
		i.Audience = model.Audience{Visibility: model.Internal}
	})
	if err := plan(s, string(hidden.Ref.ID), channels.Preview); err != nil {
		t.Fatalf("desk, hidden interaction: %v", err)
	}
	blind := s
	blind.Sees = func(a model.Audience) bool { return a.Visibility == model.Public || a.Visibility == "" }
	wantCode(t, plan(blind, string(hidden.Ref.ID), channels.Preview), model.NotFound, "interaction")

	// Conversation narrowing.
	ok := w.pressedNow("action:398738663015.47445629121.cnvn")
	_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, string(ok.Ref.ID), string(w.remote.Ref.ID), rawJSON(modalView), "op-x", channels.Preview)
	wantCode(t, err, model.NotFound, "interaction")
	if _, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, string(ok.Ref.ID), string(w.local.Ref.ID), rawJSON(modalView), "op-x", channels.Preview); err != nil {
		t.Fatalf("in its own conversation: %v", err)
	}
}

func slackTrigger(w *world, in model.Interaction) (slack.Trigger, bool) {
	in.Source = slack.Type
	return slack.TriggerOf(in)
}

// TestModalNeedsTheRightToAnswerInteractions: opening a modal answers an
// interaction, so the caller needs the right to answer interactions
// besides the extension right uma checks; a viewer that reads the
// interaction is refused.
func TestModalNeedsTheRightToAnswerInteractions(t *testing.T) {
	w := modalWorld(t)
	ctx := t.Context()
	in := w.pressedNow(pressEvent)
	for name, actor := range map[string]model.ActorID{"viewer": viewer, "external": external} {
		s, _ := w.modalDesk(actor)
		for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
			_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, string(in.Ref.ID), "", rawJSON(modalView), "op-x", mode)
			if err == nil || (!model.IsCode(err, model.Unauthorized) && !model.IsCode(err, model.NotFound)) {
				t.Errorf("%s, mode %v: err = %v, want a refusal", name, mode, err)
			}
		}
	}
	// A desk actor whose policy withholds interaction.respond is refused
	// as unauthorized, though it holds the extension right.
	s, _ := w.modalDesk(agent)
	s.Permits = func(_ model.ActorID, action string) bool { return action != model.ActionInteractionRespond }
	_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, string(in.Ref.ID), "", rawJSON(modalView), "op-x", channels.Submit)
	wantCode(t, err, model.Unauthorized, model.ActionInteractionRespond)
	// The viewer who passes the plan is refused by uma at the submission.
	vs, _ := w.modalDesk(viewer)
	vs.Permits = func(model.ActorID, string) bool { return true }
	p, err := vs.PlanModal(ctx, slack.ExtensionViewsOpen, string(in.Ref.ID), "", rawJSON(modalView), "op-v", channels.Submit)
	if err != nil {
		t.Fatalf("viewer plan with a permissive policy: %v", err)
	}
	_, err = vs.SubmitModal(ctx, p)
	wantCode(t, err, model.Unauthorized, "")
	if n := effectOperations(t, vs); n != 0 {
		t.Errorf("a refused viewer queued %d operations", n)
	}
}

// TestModalNeedsTheEffect: the channel's adapter must declare
// extension_execute and ship the very effect the verb runs, at the
// preview and again at the submission; one that ships another effect, or
// none, is unsupported, and nothing is written.
func TestModalNeedsTheEffect(t *testing.T) {
	ctx := t.Context()
	for name, effects := range map[string][]string{
		"another adapter's effect": {github.ExtensionUpdate},
		"only the other modal":     {slack.ExtensionViewsPush, slack.ExtensionViewsUpdate},
	} {
		w := modalWorld(t, effects...)
		s, kicks := w.modalDesk(agent)
		in := w.pressedNow(pressEvent)
		for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
			_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, string(in.Ref.ID), "", rawJSON(modalView), "op-x", mode)
			wantCode(t, err, model.Unsupported, slack.ExtensionViewsOpen)
			if !strings.Contains(err.Error(), "interaction open-modal") || !strings.Contains(err.Error(), "heo channel get "+string(w.ch.ID)) {
				t.Errorf("%s: %q names neither the verb nor channel get", name, err)
			}
		}
		if n := effectOperations(t, s); n != 0 || *kicks != 0 {
			t.Errorf("%s: queued %d operations", name, n)
		}
	}
	// An adapter that declares no extension_execute at all.
	w := interactionWorld(t)
	s, _ := w.modalDesk(agent)
	in := w.pressedNow(pressEvent)
	s.Effects = nil
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, string(in.Ref.ID), "", rawJSON(modalView), "op-x", mode)
		wantCode(t, err, model.Unsupported, "extension_execute")
	}
}

// TestModalViewChecks: the view is a JSON object within uma's size
// bound, whatever the adapter then finds of it; the adapter's own rules
// are asked at the preview offline and again by uma at the submission,
// with nothing stored.
func TestModalViewChecks(t *testing.T) {
	w := modalWorld(t)
	ctx := t.Context()
	s, _ := w.modalDesk(agent)
	in := w.pressedNow(pressEvent)
	id := string(in.Ref.ID)

	tooLarge := `{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[],"private_metadata":"` +
		strings.Repeat("x", slack.MaxViewBytes) + `"}`
	for name, tc := range map[string]struct {
		view string
		want string
	}{
		"empty":         {"", "view"},
		"not JSON":      {"{type: modal", "view"},
		"an array":      {`[1]`, "object"},
		"a string":      {`"modal"`, "object"},
		"null":          {`null`, "object"},
		"trailing data": {modalView + ` {}`, "view"},
		"too large":     {tooLarge, "250 KiB"},
	} {
		for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
			_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(tc.view), "op-x", mode)
			wantCode(t, err, model.Validation, tc.want)
			if strings.Contains(err.Error(), "xxxx") {
				t.Errorf("%s: the refusal repeats the view", name)
			}
		}
	}

	// The adapter's rules, which a plan checks only in a preview; uma makes
	// the same check again at the submission.
	notModal := `{"type":"home","title":{"type":"plain_text","text":"T"},"blocks":[]}`
	_, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(notModal), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "modal")
	p, err := s.PlanModal(ctx, slack.ExtensionViewsOpen, id, "", rawJSON(notModal), "op-x", channels.Submit)
	if err != nil {
		t.Fatalf("a submission plan leaves the adapter's rules to uma: %v", err)
	}
	_, err = s.SubmitModal(ctx, p)
	wantCode(t, err, model.Validation, "modal")
	if n := effectOperations(t, s); n != 0 {
		t.Errorf("a refused view queued %d operations", n)
	}
	// An exact fit passes the size check.
	ok := `{"type":"modal","title":{"type":"plain_text","text":"T"},"blocks":[]}`
	if _, err := desk.CheckView([]byte(ok)); err != nil {
		t.Errorf("small view: %v", err)
	}
	pad := slack.MaxViewBytes - len(ok) - len(`,"private_metadata":""`)
	exact := ok[:len(ok)-1] + `,"private_metadata":"` + strings.Repeat("x", pad) + `"}`
	if got, err := desk.CheckView([]byte(exact)); err != nil || len(got) != slack.MaxViewBytes {
		t.Errorf("a view of exactly %d bytes: %d, %v", slack.MaxViewBytes, len(got), err)
	}
	if _, err := desk.CheckView([]byte(exact[:len(exact)-1] + `x"}`)); err == nil {
		t.Error("a view one byte over passed")
	}
}

// TestViewUpdateByIDAndByExternalID: an update names the view by the ID
// Slack gave it or by the external_id the view was opened with, one of
// them, optionally with the hash Slack gave it; it is aimed at the inbox,
// needs no interaction and no clock, and runs the update effect.
func TestViewUpdateByIDAndByExternalID(t *testing.T) {
	w := modalWorld(t)
	ctx := t.Context()
	s, kicks := w.modalDesk(agent)
	inbox := "local"

	byID, err := s.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "V0000000001", Hash: "156772938.1827394"}, rawJSON(modalView), "op-up", channels.Preview)
	if err != nil {
		t.Fatalf("preview by ID: %v", err)
	}
	if byID.Declaration == nil || byID.Declaration.Effect != slack.ExtensionViewsUpdate {
		t.Errorf("declaration = %+v", byID.Declaration)
	}
	r := byID.Request
	d := dataOf(t, r.Action.Data)
	if r.Action.Name != slack.ExtensionViewsUpdate || r.Target.Kind != model.KindInbox || d["view_id"] != "V0000000001" ||
		d["hash"] != "156772938.1827394" || d["external_id"] != nil || d["trigger_id"] != nil || d["expires_at"] != nil {
		t.Fatalf("by ID: %+v %v", r, d)
	}
	byExt, err := s.PlanViewUpdate(ctx, inbox, desk.ViewRef{ExternalID: "deploy-42"}, rawJSON(modalView), "op-up2", channels.Submit)
	if err != nil {
		t.Fatalf("by external ID: %v", err)
	}
	if d := dataOf(t, byExt.Request.Action.Data); d["external_id"] != "deploy-42" || d["view_id"] != nil || d["hash"] != nil {
		t.Fatalf("by external ID: %v", d)
	}
	acc, err := s.SubmitModal(ctx, byExt)
	if err != nil || acc.Replayed {
		t.Fatalf("submit: %+v, %v", acc, err)
	}
	if *kicks != 0 {
		t.Errorf("an update woke the workers: it spends no trigger_id")
	}
	// No clock: an update is not refused for the age of anything.
	w.clock.Advance(24 * time.Hour)
	if _, err := s.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "V0000000001"}, rawJSON(modalView), "op-up3", channels.Preview); err != nil {
		t.Fatalf("an update a day later: %v", err)
	}

	// Exactly one way to name the view.
	for name, ref := range map[string]desk.ViewRef{
		"neither": {}, "both": {ViewID: "V0000000001", ExternalID: "deploy-42"}, "only a hash": {Hash: "1.2"},
	} {
		_, err := s.PlanViewUpdate(ctx, inbox, ref, rawJSON(modalView), "op-x", channels.Preview)
		wantCode(t, err, model.Validation, "view")
		if err != nil && name == "neither" && !strings.Contains(err.Error(), "view_id") {
			t.Errorf("%v: does not say how to name the view", err)
		}
	}
	// The adapter's rules on the ID and on the view, in a preview.
	_, err = s.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "view-1"}, rawJSON(modalView), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "view_id")
	_, err = s.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "V0000000001"}, rawJSON(`{"type":"modal"}`), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "title")
	_, err = s.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "V0000000001"}, rawJSON(`nope`), "op-x", channels.Preview)
	wantCode(t, err, model.Validation, "view")
	_, err = s.PlanViewUpdate(ctx, "nope", desk.ViewRef{ViewID: "V0000000001"}, rawJSON(modalView), "op-x", channels.Preview)
	wantCode(t, err, model.NotFound, "nope")

	// The effect gate, at the preview and the submission.
	w2 := modalWorld(t, slack.ExtensionViewsOpen)
	s2, _ := w2.modalDesk(agent)
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := s2.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "V0000000001"}, rawJSON(modalView), "op-x", mode)
		wantCode(t, err, model.Unsupported, slack.ExtensionViewsUpdate)
		if !strings.Contains(err.Error(), "interaction update-modal") {
			t.Errorf("%v: does not name the verb", err)
		}
	}
	// A viewer is refused, and so is a caller who cannot act.
	v, _ := w.modalDesk(viewer)
	p, err := v.PlanViewUpdate(ctx, inbox, desk.ViewRef{ViewID: "V0000000001"}, rawJSON(modalView), "op-v", channels.Submit)
	if err == nil {
		_, err = v.SubmitModal(ctx, p)
	}
	wantCode(t, err, model.Unauthorized, "")
}
