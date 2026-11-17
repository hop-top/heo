package authz_test

import (
	"slices"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/uma/model"
)

// bridgeReads are exactly the actions uma asks of a subscriber before
// it delivers an event: the subscription's own, the event's inbox and
// the resource each builtin kind is about.
var bridgeReads = []string{
	model.ActionSubscriptionOpen, model.ActionSubscriptionPoll, model.ActionSubscriptionAck,
	model.ActionInboxRead, model.ActionChannelRead, model.ActionContactRead, model.ActionIdentityRead,
	model.ActionConversationRead, model.ActionMessageRead, model.ActionOperationRead, model.ActionTaskRead,
	model.ActionWorkItemRead, model.ActionAttachmentRead, model.ActionStreamRead, model.ActionInteractionRead,
	model.ActionConversationReadWorkflow,
}

// TestBridgeActorReadsEventsOnly: the event bridge's own actor holds the
// subscription actions and the reads uma asks before it delivers an
// event, at every audience (it republishes references to internal
// events), and nothing else: it cannot write, configure, list events or
// read what no event is about. It is not the runtime's actor, so
// provider content, which enters as that one, gains nothing; it is not
// admitted to daemon commands; a grant naming it is refused.
func TestBridgeActorReadsEventsOnly(t *testing.T) {
	p := newPolicy(t, nil)
	for _, action := range bridgeReads {
		for _, vis := range []model.Visibility{"", model.Public, model.Internal, model.Restricted} {
			d := ask(t, p, authz.BridgeActor, action, vis)
			if !d.Allow || d.ReasonCode != authz.ReasonBridge {
				t.Errorf("bridge %s under %q = %+v", action, vis, d)
			}
		}
	}
	for _, role := range authz.Roles {
		for _, action := range authz.Actions(role) {
			if slices.Contains(bridgeReads, action) {
				continue
			}
			if ask(t, p, authz.BridgeActor, action, model.Public).Allow {
				t.Errorf("bridge allowed %s, which the %s role holds beyond the event reads", action, role)
			}
		}
	}
	for _, action := range []string{model.ActionEventList, model.ActionSubscriptionClose, model.ActionChannelReceive,
		model.ActionAttachmentUpload, model.ActionMessageReply, model.ActionMessageNote, model.ActionTaskAct,
		model.ActionLabelRead, model.ActionContactReadAll, model.ActionPrivacyExport} {
		if ask(t, p, authz.BridgeActor, action, model.Public).Allow {
			t.Errorf("bridge allowed %s", action)
		}
	}
	// The runtime's own actor gains none of it.
	for _, action := range bridgeReads {
		if action != model.ActionChannelRead && ask(t, p, service, action, model.Public).Allow {
			t.Errorf("runtime actor allowed %s", action)
		}
	}
	if ok, _ := p.Admits(authz.BridgeActor); ok {
		t.Error("the bridge actor is admitted to daemon commands")
	}
	v := viper.New()
	v.Set(authz.KeyGrants, grants(map[string]any{"actor": string(authz.BridgeActor), "roles": []any{"desk"}}))
	if _, err := authz.FromConfig(v, tenant, owner, service); err == nil {
		t.Error("a grant to the bridge actor was accepted")
	}
}

// TestBridgeActorForeignTenantDenied: the bridge reads the profile's
// tenant only.
func TestBridgeActorForeignTenantDenied(t *testing.T) {
	p := newPolicy(t, nil)
	ok, reason := p.Permits(model.Principal{Tenant: "other", Actor: authz.BridgeActor}, model.ActionSubscriptionPoll)
	if ok || reason != authz.ReasonForeignTenant {
		t.Errorf("foreign tenant = %v %q", ok, reason)
	}
}
