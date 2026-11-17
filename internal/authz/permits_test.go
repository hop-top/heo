package authz_test

import (
	"testing"

	"hop.top/heo/internal/authz"
	"hop.top/uma/model"
)

// TestPermitsChannelAndInboxActions: configuring channels and inboxes
// is the operator's; reading channels too; inboxes are readable by
// every role that reads conversations. A denial names the policy's
// reason; another tenant is never permitted.
func TestPermitsChannelAndInboxActions(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []string{"operator"},
		authz.KeyGrants: grants(
			map[string]any{"actor": "uid:1002", "roles": []any{"viewer"}},
			map[string]any{"actor": "uid:1003", "roles": []any{"desk"}},
		),
	})
	principal := func(a model.ActorID) model.Principal { return model.Principal{Tenant: tenant, Actor: a} }
	cases := []struct {
		actor  model.ActorID
		action string
		ok     bool
		reason string
	}{
		{owner, model.ActionChannelConfigure, true, ""},
		{owner, model.ActionChannelRead, true, ""},
		{owner, model.ActionInboxConfigure, true, ""},
		{owner, model.ActionInboxRead, true, ""},
		{"uid:1002", model.ActionInboxRead, true, ""},
		{"uid:1002", model.ActionChannelRead, false, authz.ReasonNotGranted},
		{"uid:1002", model.ActionInboxConfigure, false, authz.ReasonNotGranted},
		{"uid:1003", model.ActionChannelConfigure, false, authz.ReasonNotGranted},
		{"uid:1003", model.ActionInboxRead, true, ""},
		{"uid:4242", model.ActionInboxRead, false, authz.ReasonNoRole},
		{service, model.ActionChannelConfigure, false, authz.ReasonNotGranted},
	}
	for _, tc := range cases {
		ok, reason := p.Permits(principal(tc.actor), tc.action)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("%s %s = %v %q, want %v %q", tc.actor, tc.action, ok, reason, tc.ok, tc.reason)
		}
	}
	if ok, reason := p.Permits(model.Principal{Tenant: "other", Actor: owner}, model.ActionChannelRead); ok || reason != authz.ReasonForeignTenant {
		t.Errorf("foreign tenant = %v %q", ok, reason)
	}
}

// TestPermitsUnderAudience: reading content under an audience is the
// policy's audience rule, asked before any read: internal and
// restricted content, and a visibility the policy does not know, are
// for desk actors alone; public content for every reading role; the
// operator reads no conversation content at all.
func TestPermitsUnderAudience(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []string{"operator"},
		authz.KeyGrants: grants(
			map[string]any{"actor": "uid:1002", "roles": []any{"viewer"}},
			map[string]any{"actor": "uid:1003", "roles": []any{"desk"}},
			map[string]any{"actor": "uid:1004", "roles": []any{"external"}},
		),
	})
	principal := func(a model.ActorID) model.Principal { return model.Principal{Tenant: tenant, Actor: a} }
	cases := []struct {
		actor  model.ActorID
		vis    model.Visibility
		ok     bool
		reason string
	}{
		{"uid:1003", model.Internal, true, ""},
		{"uid:1003", model.Restricted, true, ""},
		{"uid:1003", model.Public, true, ""},
		{"uid:1002", model.Public, true, ""},
		{"uid:1002", model.Internal, false, authz.ReasonDeskOnly},
		{"uid:1002", model.Restricted, false, authz.ReasonDeskOnly},
		{"uid:1002", "secret", false, authz.ReasonDeskOnly},
		{"uid:1004", model.Public, true, ""},
		{"uid:1004", model.Internal, false, authz.ReasonDeskOnly},
		{owner, model.Public, false, authz.ReasonNotGranted},
		{"uid:4242", model.Public, false, authz.ReasonNoRole},
	}
	for _, tc := range cases {
		aud := model.Audience{Visibility: tc.vis}
		ok, reason := p.PermitsUnder(principal(tc.actor), model.ActionMessageRead, aud)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("%s read %s = %v %q, want %v %q", tc.actor, tc.vis, ok, reason, tc.ok, tc.reason)
		}
		if got := p.Sees(principal(tc.actor), aud); got != tc.ok {
			t.Errorf("%s sees %s = %v, want %v", tc.actor, tc.vis, got, tc.ok)
		}
	}
}
