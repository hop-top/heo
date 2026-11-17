package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

// Modals. A Slack modal is opened with the trigger_id of an interaction
// (a button press, a slash command): single-use, valid for three seconds
// after Slack sent it, and no other call opens one. The slack.chat
// adapter ships three extension effects for it, views.open, views.push
// and views.update (see effects.go for how an effect is gated), and
// refuses an attempt at or after the expires_at the host sets.
//
// uma's extension call has no link to an interaction: it is aimed at a
// conversation, an inbox or the channel, and the engine neither
// rechecks the interaction (redaction, its window) nor binds the call to
// its actor. heo does what the engine does not. It reads the interaction
// as the caller, so one the caller may not read does not exist; refuses
// a redacted one, one with no trigger_id and one the caller may not
// answer; and takes expires_at from the interaction's own deadline,
// which is exactly when Slack stops honouring the trigger_id, refusing
// locally, with nothing queued, once it has passed. The adapter decides
// again at every attempt.

// ModalVerbs are the verbs that run the effects, by effect.
var modalVerbs = map[string]string{
	slack.ExtensionViewsOpen:   "interaction open-modal",
	slack.ExtensionViewsPush:   "interaction push-modal",
	slack.ExtensionViewsUpdate: "interaction update-modal",
}

// ModalVerb is the verb that runs effect, "" for one that is no modal
// effect.
func ModalVerb(effect string) string { return modalVerbs[effect] }

// CheckView checks a modal view as the caller gave it: a JSON object,
// no larger than uma's bound on a view once compact (slack.MaxViewBytes).
// It returns the view compact. Everything else about a view (its type,
// title, blocks) is the adapter's rule, asked of it offline at a
// preview and again by uma at the submission. A refusal never repeats
// the view.
func CheckView(raw []byte) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		if json.Valid(raw) {
			return nil, model.NewError(model.Validation, "view: must be a JSON object, a Slack modal view")
		}
		return nil, model.NewError(model.Validation, "view: not valid JSON")
	}
	if obj == nil {
		return nil, model.NewError(model.Validation, "view: must be a JSON object, a Slack modal view")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, model.NewError(model.Validation, "view: not valid JSON")
	}
	if compact.Len() > slack.MaxViewBytes {
		return nil, model.NewError(model.Validation, fmt.Sprintf(
			"view: %d bytes once compact, larger than Slack takes (%d KiB)", compact.Len(), slack.MaxViewBytes>>10))
	}
	return compact.Bytes(), nil
}

// ViewRef names the view an update changes: by the ID Slack gave it
// when it was opened, or by the external_id it was opened with, exactly
// one of them. Hash, when set, is Slack's guard against a stale update.
type ViewRef struct {
	ViewID     string
	ExternalID string
	Hash       string
}

// ModalPlan is a validated modal effect: the request, what it runs
// against and, from a preview, what was checked of the adapter.
type ModalPlan struct {
	Request model.ExecuteExtension
	// Effect is the extension effect the verb runs.
	Effect string
	// Inbox is the inbox of the interaction, or the one an update names.
	Inbox model.Inbox
	// Interaction is the interaction an open or push answers; zero for
	// an update.
	Interaction model.Interaction
	// ExpiresAt is the instant the trigger_id stops working, the
	// interaction's deadline; zero for an update.
	ExpiresAt time.Time
	// View names the view an update changes.
	View ViewRef
	// ExternalID is the external_id the view carries, "" when it has none:
	// what a later update can name it by.
	ExternalID string
	// Declaration is set by a preview only.
	Declaration *EffectDeclaration
}

// SpendsTrigger reports whether the effect uses a trigger_id, and so
// has only seconds to reach the provider.
func (p ModalPlan) SpendsTrigger() bool { return p.Effect != slack.ExtensionViewsUpdate }

// trigger reads the single-use trigger_id of an interaction and when it
// stops working.
func (s Service) trigger(in model.Interaction) (slack.Trigger, bool) {
	if s.Trigger != nil {
		return s.Trigger(in)
	}
	return slack.TriggerOf(in)
}

// viewExternalID is the external_id the compact view carries.
func viewExternalID(view json.RawMessage) string {
	var v struct {
		ExternalID string `json:"external_id"`
	}
	_ = json.Unmarshal(view, &v)
	return v.ExternalID
}

// PlanModal builds and checks the opening (effect slack.ExtensionViewsOpen)
// or stacking (slack.ExtensionViewsPush) of a modal view in answer to
// interaction id (in conversation conv, record ID, when set), under
// operation op. It writes nothing.
//
// The caller must be able to answer interactions as well as run the
// effect (the caller's own policy; uma authorizes the effect itself). The
// interaction must exist and be readable by the caller, must not be
// redacted (model.Conflict) and must carry a trigger_id (model.Validation).
// The channel's adapter must declare extension_execute and ship the
// effect (model.Unsupported), in a preview and on submission. A trigger_id
// that has stopped working, or an interaction whose provider window has
// ended, is model.DeadlineExpired, in a preview and on submission, with
// nothing queued. A preview also asks the adapter, offline, to check the
// view (model.Validation); a submission leaves that to uma.
func (s Service) PlanModal(ctx context.Context, effect, id, conv string, view json.RawMessage, op model.OperationID, mode channels.Mode) (ModalPlan, error) {
	verb := modalVerbs[effect]
	if verb == "" || effect == slack.ExtensionViewsUpdate {
		return ModalPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"effect %q: want %s or %s", effect, slack.ExtensionViewsOpen, slack.ExtensionViewsPush))
	}
	compact, err := CheckView(view)
	if err != nil {
		return ModalPlan{}, err
	}
	if !s.can(model.ActionInteractionRespond) {
		return ModalPlan{}, model.NewError(model.Unauthorized, fmt.Sprintf(
			"%s may not answer interactions (%s): `%s` answers one", s.Actor, model.ActionInteractionRespond, verb))
	}
	ix, err := s.FindInteraction(ctx, id, conv)
	if err != nil {
		return ModalPlan{}, err
	}
	if ix.Redacted {
		return ModalPlan{}, model.NewError(model.Conflict, fmt.Sprintf("interaction %s is redacted: it cannot be answered", ix.Ref.ID))
	}
	in, err := s.FindInbox(ctx, string(ix.Ref.Conversation.Inbox.ID))
	if err != nil {
		return ModalPlan{}, err
	}
	d, err := s.effectDeclared(ctx, in, verb, effect, "")
	if err != nil {
		return ModalPlan{}, err
	}
	trig, ok := s.trigger(ix)
	if !ok {
		return ModalPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"interaction %s carries no trigger_id: Slack sent none with it, or it is not a Slack interaction; "+
				"only a button press, slash command or shortcut Slack sent with one can open a modal", ix.Ref.ID))
	}
	data, err := json.Marshal(map[string]any{
		"trigger_id": trig.ID, "expires_at": trig.ExpiresAt.UTC().Format(time.RFC3339Nano), "view": compact,
	})
	if err != nil {
		return ModalPlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: ix.Ref.Conversation.Ref(), Audience: effectAudience,
		Action: model.Extension{Name: effect, Version: slack.ExtensionVersion, Data: data},
	}
	if err := req.Validate(); err != nil {
		return ModalPlan{}, err
	}
	p := ModalPlan{Request: req, Effect: effect, Inbox: in, Interaction: ix, ExpiresAt: trig.ExpiresAt, ExternalID: viewExternalID(compact)}
	if mode == channels.Preview {
		p.Declaration = &d
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return ModalPlan{}, err
		}
	}
	if err := s.checkTriggerWindow(ix, trig, string(op), mode); err != nil {
		return ModalPlan{}, err
	}
	return p, nil
}

// checkTriggerWindow refuses, as model.DeadlineExpired, an interaction
// whose provider window has ended and a trigger_id that has stopped
// working. The engine rechecks neither; the adapter refuses the second
// again at the attempt, with no call.
func (s Service) checkTriggerWindow(ix model.Interaction, trig slack.Trigger, op string, mode channels.Mode) error {
	now := s.now()
	if !ix.RespondUntil.IsZero() && !now.Before(ix.RespondUntil) {
		return model.NewError(model.DeadlineExpired, fmt.Sprintf(
			"interaction %s: its respond window ended at %s", ix.Ref.ID, ix.RespondUntil.UTC().Format(time.RFC3339)))
	}
	if now.Before(trig.ExpiresAt) {
		return nil
	}
	msg := fmt.Sprintf(
		"interaction %s: its trigger_id stopped working at %s, %s after Slack sent it (single-use, valid for %s): a modal can only be opened inside that window",
		ix.Ref.ID, trig.ExpiresAt.UTC().Format(time.RFC3339), slack.TriggerLifetime, slack.TriggerLifetime)
	if mode == channels.Submit {
		msg += fmt.Sprintf("; if an attempt under operation key %s got through before, `heo operation get --key %s` shows it", op, op)
	}
	return model.NewError(model.DeadlineExpired, msg)
}

// PlanViewUpdate builds and checks the update of the modal view ref names
// to view, aimed at inbox (key or ID) of the workspace the view lives in,
// under operation op. It writes nothing. It uses no trigger_id and has no
// window. The channel's adapter must declare extension_execute and ship
// the update effect (model.Unsupported), in a preview and on submission;
// a preview also asks the adapter, offline, to check the view and the
// way it is named (model.Validation).
func (s Service) PlanViewUpdate(ctx context.Context, inbox string, ref ViewRef, view json.RawMessage, op model.OperationID, mode channels.Mode) (ModalPlan, error) {
	const verb = "interaction update-modal"
	switch {
	case ref.ViewID == "" && ref.ExternalID == "":
		return ModalPlan{}, model.NewError(model.Validation, "view: name the view to update by view_id (the ID Slack gave it) or by external_id")
	case ref.ViewID != "" && ref.ExternalID != "":
		return ModalPlan{}, model.NewError(model.Validation, "view: name the view by view_id or by external_id, one of them")
	}
	compact, err := CheckView(view)
	if err != nil {
		return ModalPlan{}, err
	}
	in, err := s.FindInbox(ctx, inbox)
	if err != nil {
		return ModalPlan{}, err
	}
	d, err := s.effectDeclared(ctx, in, verb, slack.ExtensionViewsUpdate, "")
	if err != nil {
		return ModalPlan{}, err
	}
	fields := map[string]any{"view": compact}
	if ref.ViewID != "" {
		fields["view_id"] = ref.ViewID
	} else {
		fields["external_id"] = ref.ExternalID
	}
	if ref.Hash != "" {
		fields["hash"] = ref.Hash
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return ModalPlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: in.Ref.Ref(), Audience: effectAudience,
		Action: model.Extension{Name: slack.ExtensionViewsUpdate, Version: slack.ExtensionVersion, Data: data},
	}
	if err := req.Validate(); err != nil {
		return ModalPlan{}, err
	}
	p := ModalPlan{Request: req, Effect: slack.ExtensionViewsUpdate, Inbox: in, View: ref, ExternalID: viewExternalID(compact)}
	if mode == channels.Preview {
		p.Declaration = &d
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return ModalPlan{}, err
		}
	}
	return p, nil
}

// SubmitModal submits p: uma queues the effect durably and the daemon's
// workers send it to Slack. An effect that spends a trigger_id then wakes
// the workers at once, so the three seconds are not also spent waiting
// out their idle poll.
func (s Service) SubmitModal(ctx context.Context, p ModalPlan) (model.Acceptance, error) {
	acc, err := s.Client.Extensions().Execute(ctx, p.Request)
	if err != nil {
		return acc, err
	}
	if p.SpendsTrigger() && s.Kick != nil {
		s.Kick()
	}
	return acc, nil
}
