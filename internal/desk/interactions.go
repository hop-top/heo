package desk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// Interactions. A chat provider reports a button press, a slash command
// or a form as an interaction: uma records it, never as a message, and
// the provider expects an answer inside a window. heo reads the records
// and queues the answer; the daemon's workers give it to the provider
// through the adapter that reported the interaction.
//
// Reading needs no capability: the records are uma's. Answering is a
// provider effect, so it works only where the channel's adapter declares
// respond (and respond_follow_up for a follow-up). A preview checks that
// offline, as every provider verb does; a submission is left to uma,
// which refuses it before it claims anything.

// DefaultRespondWindow is how long after an interaction was recorded a
// response may be given when the provider states no window and the caller
// names no expiry. It is the window uma's bot companion gives the same
// interactions. It counts from the record, never from now, so a retry
// under the same operation is the same request: uma holds the expiry in
// the fingerprint of the response.
const DefaultRespondWindow = 15 * time.Minute

// InteractionFilter narrows an interaction listing. Inbox and
// Conversation name records by key or ID, and Actor an identity (record
// ID): the one that acted. uma applies all three.
type InteractionFilter struct {
	Inbox        string
	Conversation string
	Actor        string
}

// interactionFilter resolves f's names into uma's filter.
func (s Service) interactionFilter(ctx context.Context, f InteractionFilter) (model.Filter, error) {
	filter, err := s.inboxFilter(ctx, Filter{Inbox: f.Inbox})
	if err != nil {
		return filter, err
	}
	if f.Conversation != "" {
		conv, err := s.FindConversation(ctx, f.Conversation)
		if err != nil {
			return filter, err
		}
		filter.Conversation = &conv.Ref
	}
	if f.Actor != "" {
		who, err := s.FindIdentity(ctx, f.Actor)
		if err != nil {
			return filter, err
		}
		filter.Identity = &who.Ref
	}
	return filter, nil
}

// ListInteractions returns one page of the interactions the caller may
// read, in recording order, narrowed by f. uma skips the records the
// caller may not read; heo passes each through the caller's audience
// rule again, so an interaction that is not public never reaches a
// caller who is not a desk actor. A page can be short and still not be
// the last.
func (s Service) ListInteractions(ctx context.Context, f InteractionFilter, page model.PageRequest) (model.Page[model.Interaction], error) {
	filter, err := s.interactionFilter(ctx, f)
	if err != nil {
		return model.Page[model.Interaction]{}, err
	}
	p, err := s.Client.Interactions().List(ctx, model.ListRequest{Call: call(), Filter: filter, Page: page})
	if err != nil {
		return model.Page[model.Interaction]{}, err
	}
	p.Items = s.visibleInteractions(p.Items)
	return p, nil
}

func (s Service) visibleInteractions(ins []model.Interaction) []model.Interaction {
	out := make([]model.Interaction, 0, len(ins))
	for _, in := range ins {
		if s.seesAudience(in.Audience) {
			out = append(out, in)
		}
	}
	return out
}

// FindInteraction returns the interaction with record ID id among those
// the caller may read, in the conversation conv (record ID) when it is
// set. uma addresses an interaction by its conversation, so a lookup by
// ID scans what the caller may read.
func (s Service) FindInteraction(ctx context.Context, id, conv string) (model.Interaction, error) {
	if err := checkID("interaction", id); err != nil {
		return model.Interaction{}, err
	}
	filter, err := s.interactionFilter(ctx, InteractionFilter{Conversation: conv})
	if err != nil {
		return model.Interaction{}, err
	}
	req := model.ListRequest{Call: call(), Filter: filter, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Interactions().List(ctx, req)
		if err != nil {
			return model.Interaction{}, err
		}
		for _, in := range p.Items {
			if string(in.Ref.ID) != id {
				continue
			}
			if !s.seesAudience(in.Audience) {
				break
			}
			return in, nil
		}
		if p.Complete || p.Next == "" {
			return model.Interaction{}, model.NewError(model.NotFound, fmt.Sprintf("interaction %s: not found", id))
		}
		req.Page.After = p.Next
	}
}

// RespondInput is what the caller says of a response. Mode is one of
// model.ResponseModeMessage, ResponseModeEphemeral and ResponseModeUpdate;
// the audience follows from it and is never the caller's to widen.
// ExpiresAt, when zero, is the provider's respond window, else
// DefaultRespondWindow after the interaction was recorded.
type RespondInput struct {
	Mode      string
	Text      string
	FollowUp  bool
	ExpiresAt time.Time
}

// RespondPlan is a validated interaction response.
type RespondPlan struct {
	Request     model.Respond
	Interaction model.Interaction
	// Declaration is set by a preview only.
	Declaration *Declaration
}

// PlanRespond builds and checks the response in to interaction id (in
// conversation conv, record ID, when set) under operation op. The
// audience is the mode's: public for a message and an update, restricted
// to the actor for an ephemeral response; and a public response is
// refused for an interaction that is not public, which it would widen.
//
// In a preview it also refuses what uma would refuse at submission, so a
// plan is never drawn for it: an adapter that does not declare respond
// (and respond_follow_up for a follow-up), model.Unsupported; a redacted
// interaction or a second initial response, model.Conflict; an update
// with no control message to replace, or an expiry past the respond
// window, model.Validation; and a window that has ended,
// model.DeadlineExpired. A submission leaves those to uma, which decides
// them atomically with the claim.
func (s Service) PlanRespond(ctx context.Context, id, conv string, in RespondInput, op model.OperationID, mode channels.Mode) (RespondPlan, error) {
	if !model.IsResponseMode(in.Mode) {
		return RespondPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"mode %q: want %s, %s or %s", in.Mode, model.ResponseModeMessage, model.ResponseModeEphemeral, model.ResponseModeUpdate,
		))
	}
	if strings.TrimSpace(in.Text) == "" {
		return RespondPlan{}, model.NewError(model.Validation, "text: required")
	}
	ix, err := s.FindInteraction(ctx, id, conv)
	if err != nil {
		return RespondPlan{}, err
	}
	aud := model.Audience{Visibility: model.Public}
	switch {
	case in.Mode == model.ResponseModeEphemeral:
		aud.Visibility = model.Restricted
	case ix.Audience.Visibility != "" && ix.Audience.Visibility != model.Public:
		return RespondPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"interaction %s is %s: a %s response is public and would widen its audience; answer it with mode %s",
			ix.Ref.ID, ix.Audience.Visibility, in.Mode, model.ResponseModeEphemeral,
		))
	}
	expires := in.ExpiresAt
	if expires.IsZero() {
		expires = defaultRespondExpiry(ix)
	}
	p := RespondPlan{Interaction: ix}
	if mode == channels.Preview {
		if p.Declaration, err = s.previewRespond(ctx, ix, in); err != nil {
			return RespondPlan{}, err
		}
		if err := s.checkRespond(ix, in, expires); err != nil {
			return RespondPlan{}, err
		}
	}
	p.Request = model.Respond{
		Command: model.Command{Call: call(), Operation: op}, Interaction: ix.Ref, Mode: in.Mode, Audience: aud,
		Content: TextContent(in.Text), FollowUp: in.FollowUp, ExpiresAt: expires.UTC(),
	}
	if err := p.Request.Validate(); err != nil {
		return RespondPlan{}, err
	}
	return p, nil
}

// defaultRespondExpiry is the expiry of a response whose caller named
// none: the end of the provider's window, else the default window after
// the record. It is a function of the record alone.
func defaultRespondExpiry(ix model.Interaction) time.Time {
	if !ix.RespondUntil.IsZero() {
		return ix.RespondUntil
	}
	return ix.RecordedAt.Add(DefaultRespondWindow)
}

// previewRespond checks that the adapter of the interaction's channel
// declares what the response needs: respond, and respond_follow_up for a
// follow-up. Offline, like every provider verb's preview.
func (s Service) previewRespond(ctx context.Context, ix model.Interaction, in RespondInput) (*Declaration, error) {
	inbox, err := s.FindInbox(ctx, string(ix.Ref.Conversation.Inbox.ID))
	if err != nil {
		return nil, err
	}
	d, desc, err := s.declared(ctx, inbox, "interaction respond", model.CapabilityRespond)
	if err != nil {
		return nil, err
	}
	if in.FollowUp && !adapters.Declares(desc, model.CapabilityRespondFollowUp) {
		return nil, model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: adapter %s does not declare %s, which `interaction respond --follow-up` needs; `heo channel get %s` lists the verbs the channel supports",
			inbox.Key, desc.Type, model.CapabilityRespondFollowUp, inbox.Channel.ID,
		))
	}
	return &d, nil
}

// checkRespond refuses, in a preview, the responses uma refuses on the
// stored interaction alone.
func (s Service) checkRespond(ix model.Interaction, in RespondInput, expires time.Time) error {
	now := s.now()
	switch {
	case ix.Redacted:
		return model.NewError(model.Conflict, fmt.Sprintf("interaction %s is redacted: it cannot be answered", ix.Ref.ID))
	case !in.FollowUp && ix.Response.Initial != nil:
		return model.NewError(model.Conflict, fmt.Sprintf(
			"interaction %s already has an initial response (operation %s): --follow-up adds another", ix.Ref.ID, ix.Response.Initial.ID,
		))
	case in.Mode == model.ResponseModeUpdate && ix.MessageExternal == nil:
		return model.NewError(model.Validation, "mode: update needs the provider message the control sits on, and this interaction names none")
	case !ix.RespondUntil.IsZero() && !now.Before(ix.RespondUntil):
		return model.NewError(model.DeadlineExpired, fmt.Sprintf(
			"interaction %s: its respond window ended at %s", ix.Ref.ID, ix.RespondUntil.UTC().Format(time.RFC3339),
		))
	case !ix.RespondUntil.IsZero() && expires.After(ix.RespondUntil):
		return model.NewError(model.Validation, fmt.Sprintf(
			"expires_at: must not pass the interaction's respond window, which ends at %s", ix.RespondUntil.UTC().Format(time.RFC3339),
		))
	case !expires.After(now):
		hint := ""
		if in.ExpiresAt.IsZero() {
			hint = fmt.Sprintf(" (the default is %s after the interaction was recorded): name a later expires_at", DefaultRespondWindow)
		}
		return model.NewError(model.DeadlineExpired, fmt.Sprintf(
			"expires_at %s has passed%s", expires.UTC().Format(time.RFC3339), hint,
		))
	}
	return nil
}

// Respond submits p: uma queues the response durably, claiming the
// initial response atomically with it, and the daemon's workers give it
// to the provider. Nothing reaches the provider before the workers run.
func (s Service) Respond(ctx context.Context, p RespondPlan) (model.Acceptance, error) {
	return s.Client.Interactions().Respond(ctx, p.Request)
}
