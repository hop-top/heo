package cmd

import (
	"context"
	"strings"
	"time"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/settings"
	"hop.top/heo/internal/trustcfg"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/model"
)

// holdEmitter is the source heo's hold point publishes events under.
const holdEmitter = "heo.hold"

// holds is the hold point's configuration for the caller's desk: nil
// while held_operations.enabled is off, so nothing is held.
func (a *app) holds() *desk.Holds {
	v := a.r.Viper
	if !v.GetBool(settings.KeyHeldEnabled) {
		return nil
	}
	expiry, err := time.ParseDuration(v.GetString(settings.KeyHeldExpiry))
	if err != nil || expiry <= 0 {
		expiry = settings.DefaultHeldExpiry
	}
	return &desk.Holds{
		Expiry:     expiry,
		OnBehalfOf: model.ActorID(v.GetString(settings.KeyHeldOnBehalfOf)),
		Emit: func(ctx context.Context, ev desk.HeldCreated) {
			_ = a.hub.Publish(ctx, bus.NewEvent(bus.Topic(desk.HeldCreatedTopic), holdEmitter, ev))
		},
	}
}

// holdAware lists the actions whose verbs offer their write to the hold
// point (desk.Service.Hold). Every other write of a plugin actor is
// refused while holding is on, never applied unreviewed.
var holdAware = map[string]bool{
	model.ActionMessageReply:           true,
	model.ActionMessageEdit:            true,
	model.ActionMessageDelete:          true,
	model.ActionMessageReact:           true,
	model.ActionConversationTransition: true,
	model.ActionConversationLabel:      true,
	model.ActionLabelConfigure:         true,
	model.ActionMessageNote:            true,
	model.ActionConversationStart:      true,
}

// readOnlyAction reports whether action only reads.
func readOnlyAction(action string) bool {
	switch action {
	case model.ActionEventList, model.ActionExtensionQuery, model.ActionContactReadAll, model.ActionConversationReadWorkflow, model.ActionIdentityHistory:
		return true
	}
	return strings.HasSuffix(action, ".read")
}

// holdGuard refuses a plugin actor's write that cannot reach the hold
// point while holding is on: a write whose verb has no hold path
// would otherwise skip review.
func (a *app) holdGuard(c control.Caller, action string) error {
	if a.holds() == nil || !c.Actor.IsPlugin() || readOnlyAction(action) || holdAware[action] {
		return nil
	}
	return model.NewError(model.Unsupported, "verb cannot be held: it has no --dry-run preview to hold, and "+
		settings.KeyHeldEnabled+" is on, so a plugin actor may not run "+action)
}

// trustLadder replaces the hold-everything policy of s with the trust
// ladder when held_operations.trust.enabled is on. It reads uma's trust
// counters as the runtime, which counts and never decides.
func (a *app) trustLadder(rt daemon.Runtime, c control.Caller, s *desk.Service) error {
	if s.Holds == nil {
		return nil
	}
	cfg, err := trustcfg.Load(a.r.Viper)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return nil
	}
	svc, err := rt.Engine.For(model.Principal{Tenant: c.Tenant, Actor: daemon.ServiceActor})
	if err != nil {
		return err
	}
	find := *s
	s.Holds.Policy = desk.TrustPolicy{
		Config:  cfg,
		Channel: find.TrustChannel,
		Trust: func(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
			return svc.Operations().Trust(ctx, model.TrustLookup{Call: model.Call{Correlation: desk.Correlation}, Channel: ch, Key: key})
		},
	}
	return nil
}
