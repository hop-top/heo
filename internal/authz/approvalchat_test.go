package authz_test

import (
	"slices"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/uma/model"
)

// TestApprovalChatActorReadsAndSpeaksOnly: the approval chat service's
// own actor holds the bridge's event reads, a reply and the edit of its
// own announcement, and nothing else: it decides no held operation,
// sends no note, reads no listing. It is not the runtime's actor, is
// admitted to no daemon command, and a grant naming it is refused.
func TestApprovalChatActorReadsAndSpeaksOnly(t *testing.T) {
	p := newPolicy(t, nil)
	allowed := append(slices.Clone(bridgeReads), model.ActionMessageReply, model.ActionMessageEdit)
	for _, action := range allowed {
		d := ask(t, p, authz.ApprovalChatActor, action, model.Public)
		if !d.Allow || d.ReasonCode != authz.ReasonApprovalChat {
			t.Errorf("approval chat %s = %+v", action, d)
		}
	}
	for _, role := range authz.Roles {
		for _, action := range authz.Actions(role) {
			if slices.Contains(allowed, action) {
				continue
			}
			if ask(t, p, authz.ApprovalChatActor, action, model.Public).Allow {
				t.Errorf("approval chat allowed %s, which the %s role holds beyond its own", action, role)
			}
		}
	}
	for _, action := range []string{model.ActionOperationApprove, model.ActionOperationReject, model.ActionOperationEdit,
		model.ActionMessageNote, model.ActionMessageDelete, model.ActionMessageReact, model.ActionEventList} {
		if ask(t, p, authz.ApprovalChatActor, action, model.Public).Allow {
			t.Errorf("approval chat allowed %s", action)
		}
	}
	if ok, _ := p.Admits(authz.ApprovalChatActor); ok {
		t.Error("the approval chat actor is admitted to daemon commands")
	}
	v := viper.New()
	v.Set(authz.KeyGrants, grants(map[string]any{"actor": string(authz.ApprovalChatActor), "roles": []any{"desk"}}))
	if _, err := authz.FromConfig(v, tenant, owner, service); err == nil {
		t.Error("a grant to the approval chat actor was accepted")
	}
	// The bridge gains nothing from it.
	if ask(t, p, authz.BridgeActor, model.ActionMessageReply, model.Public).Allow {
		t.Error("bridge allowed reply")
	}
}
