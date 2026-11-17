package authz_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

const (
	tenant  model.TenantID = "default"
	owner   model.ActorID  = "uid:501"
	service model.ActorID  = "svc:heo-runtime"
)

func newPolicy(t *testing.T, set map[string]any) *authz.Policy {
	t.Helper()
	v := viper.New()
	for k, val := range authz.Defaults() {
		v.SetDefault(k, val)
	}
	for k, val := range set {
		v.Set(k, val)
	}
	p, err := authz.FromConfig(v, tenant, owner, service)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ask(t *testing.T, p *authz.Policy, actor model.ActorID, action string, vis model.Visibility) policy.Decision {
	t.Helper()
	d, err := p.Authorize(context.Background(), policy.Authorization{
		Principal: model.Principal{Tenant: tenant, Actor: actor},
		Action:    action,
		Resource:  model.Ref{Tenant: tenant, Kind: model.KindConversation, Inbox: "in-1", ID: "c-1"},
		Audience:  model.Audience{Visibility: vis},
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.PolicyVersion != authz.PolicyVersion {
		t.Errorf("%s %s: policy version %q", actor, action, d.PolicyVersion)
	}
	return d
}

func grants(entries ...map[string]any) []any {
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	return out
}

// TestDenyByDefault: an actor the configuration does not name holds
// nothing, and an action no role carries is denied to every role.
func TestDenyByDefault(t *testing.T) {
	p := newPolicy(t, nil)
	for _, action := range []string{model.ActionMessageRead, model.ActionConversationRead, model.ActionChannelRead} {
		if d := ask(t, p, "uid:4242", action, ""); d.Allow || d.ReasonCode != authz.ReasonNoRole {
			t.Errorf("unknown actor %s = %+v", action, d)
		}
	}
	if d := ask(t, p, owner, "made.up", ""); d.Allow || d.ReasonCode != authz.ReasonNotGranted {
		t.Errorf("unknown action = %+v", d)
	}
	if ok, reason := p.Admits("uid:4242"); ok || reason != authz.ReasonNoRole {
		t.Errorf("Admits(unknown) = %v %s", ok, reason)
	}
}

// TestOwnerDefaultsToDeskAndOperator: the uid the daemon runs as holds
// the desk and operator roles unless authorization.owner says
// otherwise; a narrower list narrows it, an empty list leaves it
// nothing.
func TestOwnerDefaultsToDeskAndOperator(t *testing.T) {
	p := newPolicy(t, nil)
	got := p.Roles(owner)
	if len(got) != 2 || !slices.Contains(got, authz.RoleDesk) || !slices.Contains(got, authz.RoleOperator) {
		t.Fatalf("owner roles = %v, want [desk operator]", got)
	}
	if d := ask(t, p, owner, model.ActionMessageReply, model.Public); !d.Allow {
		t.Errorf("owner reply = %+v", d)
	}
	if d := ask(t, p, owner, model.ActionMessageNote, model.Internal); !d.Allow {
		t.Errorf("owner note = %+v", d)
	}
	for _, action := range []string{model.ActionChannelConfigure, model.ActionInboxConfigure, model.ActionOperationReplay} {
		if d := ask(t, p, owner, action, ""); !d.Allow {
			t.Errorf("default owner %s = %+v", action, d)
		}
	}
	desk := newPolicy(t, map[string]any{authz.KeyOwner: []any{"desk"}})
	if d := ask(t, desk, owner, model.ActionChannelConfigure, ""); d.Allow {
		t.Errorf("owner: [desk] configures channels: %+v", d)
	}
	none := newPolicy(t, map[string]any{authz.KeyOwner: []any{}})
	if ok, _ := none.Admits(owner); ok {
		t.Error("owner admitted with authorization.owner: []")
	}
	if d := ask(t, none, owner, model.ActionMessageReply, model.Public); d.Allow {
		t.Errorf("owner without roles replies: %+v", d)
	}
}

// TestRolesGrantTheirActions: each role's actions are allowed to an
// actor holding it, and to no actor without it.
func TestRolesGrantTheirActions(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []any{},
		authz.KeyGrants: grants(
			map[string]any{"actor": "uid:10", "roles": []any{"operator"}},
			map[string]any{"actor": "uid:11", "roles": []any{"desk"}},
			map[string]any{"actor": "uid:12", "roles": []any{"viewer"}},
			map[string]any{"actor": "Visitor.Example", "roles": []any{"external"}},
		),
	})
	cases := []struct {
		action string
		allow  map[model.ActorID]bool
	}{
		{model.ActionChannelConfigure, map[model.ActorID]bool{"uid:10": true}},
		{model.ActionInboxConfigure, map[model.ActorID]bool{"uid:10": true}},
		{model.ActionOperationReplay, map[model.ActorID]bool{"uid:10": true}},
		{model.ActionOperationReconcile, map[model.ActorID]bool{"uid:10": true, "uid:11": true}},
		{model.ActionPrivacyExport, map[model.ActorID]bool{"uid:10": true}},
		{model.ActionMessageReply, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionMessageNote, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionMessageEdit, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionMessageDelete, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionMessageReact, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionMessageReceipt, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionConversationMembership, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionChannelImport, map[model.ActorID]bool{"uid:10": true, "uid:11": true}},
		{model.ActionConversationTransition, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionIdentityLink, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionContactWrite, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionContactMerge, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionIdentityLinkOverride, map[model.ActorID]bool{"uid:10": true}},
		{model.ActionContactRead, map[model.ActorID]bool{"uid:11": true, "uid:12": true}},
		{model.ActionOperationRead, map[model.ActorID]bool{"uid:10": true, "uid:11": true}},
		{model.ActionConversationRead, map[model.ActorID]bool{"uid:11": true, "uid:12": true, "Visitor.Example": true}},
		{model.ActionSubscriptionPoll, map[model.ActorID]bool{"uid:11": true, "uid:12": true, "Visitor.Example": true}},
		{model.ActionTaskRead, map[model.ActorID]bool{"uid:11": true, "uid:12": true}},
		{model.ActionTaskAct, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionChannelReceive, map[model.ActorID]bool{}},
		{model.ActionConversationAssign, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionConversationSnooze, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionConversationLink, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionConversationMarkRead, map[model.ActorID]bool{"uid:11": true}},
		{model.ActionConversationReadWorkflow, map[model.ActorID]bool{"uid:11": true, "uid:12": true}},
		{model.ActionInteractionRead, map[model.ActorID]bool{"uid:11": true, "uid:12": true}},
		{model.ActionInteractionRespond, map[model.ActorID]bool{"uid:11": true}},
	}
	for _, c := range cases {
		for _, actor := range []model.ActorID{"uid:10", "uid:11", "uid:12", "Visitor.Example"} {
			if d := ask(t, p, actor, c.action, ""); d.Allow != c.allow[actor] {
				t.Errorf("%s %s = %+v, want allow %v", actor, c.action, d, c.allow[actor])
			}
		}
	}
	// Roles add up: one actor holding two roles has both.
	both := newPolicy(t, map[string]any{authz.KeyGrants: grants(
		map[string]any{"actor": "uid:20", "roles": []any{"operator", "desk"}})})
	if !ask(t, both, "uid:20", model.ActionChannelConfigure, "").Allow || !ask(t, both, "uid:20", model.ActionMessageReply, model.Public).Allow {
		t.Error("operator+desk lacks one role's actions")
	}
}

// TestInternalContentDeskOnly: content whose audience is not public is
// for desk actors alone, whatever action reaches it — reading a
// message, an attachment, an event; the public part stays readable to
// every reading role.
func TestInternalContentDeskOnly(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []any{},
		authz.KeyGrants: grants(
			map[string]any{"actor": "uid:10", "roles": []any{"operator"}},
			map[string]any{"actor": "uid:11", "roles": []any{"desk"}},
			map[string]any{"actor": "uid:12", "roles": []any{"viewer"}},
			map[string]any{"actor": "ext-1", "roles": []any{"external"}},
		),
	})
	for _, action := range []string{model.ActionMessageRead, model.ActionAttachmentRead, model.ActionConversationRead} {
		for _, vis := range []model.Visibility{model.Internal, model.Restricted, "secret"} {
			for _, actor := range []model.ActorID{"uid:10", "uid:12", "ext-1"} {
				if d := ask(t, p, actor, action, vis); d.Allow {
					t.Errorf("%s %s %s = allowed", actor, action, vis)
				}
			}
			if d := ask(t, p, "uid:11", action, vis); d.Allow != (vis != "secret") {
				t.Errorf("desk %s %s = %+v", action, vis, d)
			}
		}
		for _, actor := range []model.ActorID{"uid:11", "uid:12", "ext-1"} {
			if d := ask(t, p, actor, action, model.Public); !d.Allow {
				t.Errorf("%s %s public = %+v", actor, action, d)
			}
		}
	}
	if d := ask(t, p, "uid:12", model.ActionMessageRead, model.Internal); d.ReasonCode != authz.ReasonDeskOnly {
		t.Errorf("viewer internal read reason = %q", d.ReasonCode)
	}
}

// TestServiceActorReceivesOnly: the runtime's own actor reads and
// receives on channels and stages the media that arrives with provider
// content, and does nothing else; provider content, which enters as it,
// cannot reply, configure or read conversations or attachments. A grant
// naming it is refused.
func TestServiceActorReceivesOnly(t *testing.T) {
	p := newPolicy(t, nil)
	for _, action := range []string{model.ActionChannelRead, model.ActionChannelReceive,
		model.ActionAttachmentPrepare, model.ActionAttachmentUpload} {
		for _, vis := range []model.Visibility{"", model.Public, model.Internal} {
			if !ask(t, p, service, action, vis).Allow {
				t.Errorf("service %s under %q denied", action, vis)
			}
		}
	}
	for _, action := range []string{model.ActionMessageReply, model.ActionMessageRead, model.ActionChannelConfigure,
		model.ActionConversationRead, model.ActionOperationReplay, model.ActionInboxConfigure, model.ActionAttachmentRead,
		model.ActionTaskRead, model.ActionTaskAct, model.ActionMessageReceipt, model.ActionConversationMembership,
		model.ActionChannelImport, model.ActionConversationAssign, model.ActionConversationSnooze,
		model.ActionConversationLink, model.ActionConversationMarkRead, model.ActionConversationReadWorkflow,
		model.ActionInteractionRead, model.ActionInteractionRespond} {
		if ask(t, p, service, action, model.Public).Allow {
			t.Errorf("service %s allowed", action)
		}
	}
	if ok, _ := p.Admits(service); ok {
		t.Error("the service actor is admitted to daemon commands")
	}
	v := viper.New()
	v.Set(authz.KeyGrants, grants(map[string]any{"actor": string(service), "roles": []any{"desk"}}))
	if _, err := authz.FromConfig(v, tenant, owner, service); err == nil {
		t.Error("a grant to the service actor was accepted")
	}
}

// TestForeignTenantDenied: a principal of another tenant is denied,
// whatever its actor holds here.
func TestForeignTenantDenied(t *testing.T) {
	p := newPolicy(t, nil)
	d, err := p.Authorize(context.Background(), policy.Authorization{
		Principal: model.Principal{Tenant: "other", Actor: owner}, Action: model.ActionMessageReply,
		Resource: model.Ref{Tenant: "other", Kind: model.KindConversation, ID: "c"},
	})
	if err != nil || d.Allow || d.ReasonCode != authz.ReasonForeignTenant {
		t.Errorf("foreign tenant = %+v, %v", d, err)
	}
}

// TestCheckReportsOffline: config validate's findings for the policy.
func TestCheckReportsOffline(t *testing.T) {
	type want struct {
		key, msg string
		warning  bool
	}
	cases := []struct {
		name string
		set  map[string]any
		want []want
	}{
		{"defaults", nil, nil},
		{"unknown owner role", map[string]any{authz.KeyOwner: []any{"admin"}},
			[]want{{authz.KeyOwner, `unknown role "admin"`, false}}},
		{"unknown grant role", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": string(owner), "roles": []any{"root"}})},
			[]want{{authz.KeyGrants + "[0].roles", `unknown role "root"`, false}}},
		{"missing actor", map[string]any{authz.KeyGrants: grants(map[string]any{"roles": []any{"desk"}})},
			[]want{{authz.KeyGrants + "[0].actor", "required", false}}},
		{"invalid actor", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": " uid:1", "roles": []any{"desk"}})},
			[]want{{authz.KeyGrants + "[0].actor", "not a valid actor", false}}},
		{"aps profile as actor", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": "aps:noor", "roles": []any{"desk"}})},
			[]want{{authz.KeyGrants + "[0].actor", "a profile, not an actor", false}}},
		{"service actor", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": string(service), "roles": []any{"desk"}})},
			[]want{{authz.KeyGrants + "[0].actor", "reserved", false}}},
		{"unknown field", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": string(owner), "roles": []any{"desk"}, "inboxes": []any{"x"}})},
			[]want{{authz.KeyGrants + "[0].inboxes", "unknown field", false}}},
		{"not a list", map[string]any{authz.KeyGrants: "uid:10=desk"},
			[]want{{authz.KeyGrants, "a list", false}}},
		{"empty roles", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": string(owner), "roles": []any{}})},
			[]want{{authz.KeyGrants + "[0].roles", "grants nothing", true}}},
		{"duplicate actor", map[string]any{authz.KeyGrants: grants(
			map[string]any{"actor": "uid:10", "roles": []any{"desk"}},
			map[string]any{"actor": "uid:10", "roles": []any{"viewer"}})},
			[]want{{authz.KeyGrants + "[1].actor", "already granted", true}, {authz.KeyGrants + "[0].actor", "cannot reach", true}}},
		{"unreachable uid", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": "uid:10", "roles": []any{"desk"}})},
			[]want{{authz.KeyGrants + "[0].actor", "cannot reach", true}}},
		{"reachable with http", map[string]any{"services.api.enabled": true,
			authz.KeyGrants: grants(map[string]any{"actor": "uid:10", "roles": []any{"desk"}})}, nil},
		{"reachable without same uid", map[string]any{"services.socket.enabled": true, "services.socket.auth.peer.require_same_uid": false,
			authz.KeyGrants: grants(map[string]any{"actor": "uid:10", "roles": []any{"desk"}})}, nil},
		{"owner granted explicitly", map[string]any{authz.KeyGrants: grants(map[string]any{"actor": string(owner), "roles": []any{"operator"}})}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := viper.New()
			for k, val := range authz.Defaults() {
				v.SetDefault(k, val)
			}
			v.SetDefault("services.socket.enabled", true)
			v.SetDefault("services.socket.auth.peer.require_same_uid", true)
			for k, val := range c.set {
				v.Set(k, val)
			}
			got := authz.Check(v, owner, service)
			if len(got) != len(c.want) {
				t.Fatalf("problems = %+v, want %d", got, len(c.want))
			}
			for i, w := range c.want {
				g := got[i]
				if g.Key != w.key || !strings.Contains(g.Message, w.msg) || g.Warning != w.warning {
					t.Errorf("problem %d = %+v, want %+v", i, g, w)
				}
			}
			_, err := authz.FromConfig(v, tenant, owner, service)
			blocking := false
			for _, w := range c.want {
				blocking = blocking || !w.warning
			}
			if (err != nil) != blocking {
				t.Errorf("FromConfig err = %v, want blocking %v", err, blocking)
			}
		})
	}
}

// TestOwnerRolesFromString: -c authorization.owner=desk,operator and the
// HEO_AUTHORIZATION_OWNER environment variable give a comma list.
func TestOwnerRolesFromString(t *testing.T) {
	p := newPolicy(t, map[string]any{authz.KeyOwner: "desk, operator"})
	if got := p.Roles(owner); len(got) != 2 {
		t.Fatalf("roles = %v", got)
	}
	p = newPolicy(t, map[string]any{authz.KeyOwner: ""})
	if got := p.Roles(owner); len(got) != 0 {
		t.Fatalf("roles = %v, want none", got)
	}
}

// TestWorkflowActionsFollowTheRoles: assigning, snoozing, linking and
// marking read are the desk's; reading the workflow state (assignee,
// snooze, links) is the desk's and the viewer's; an external actor and
// an operator without desk hold none of them, at any audience.
func TestWorkflowActionsFollowTheRoles(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []any{},
		authz.KeyGrants: grants(
			map[string]any{"actor": "uid:10", "roles": []any{"operator"}},
			map[string]any{"actor": "uid:11", "roles": []any{"desk"}},
			map[string]any{"actor": "uid:12", "roles": []any{"viewer"}},
			map[string]any{"actor": "ext-1", "roles": []any{"external"}},
		),
	})
	writes := []string{model.ActionConversationAssign, model.ActionConversationSnooze,
		model.ActionConversationLink, model.ActionConversationMarkRead}
	for _, action := range append([]string{model.ActionConversationReadWorkflow}, writes...) {
		for _, actor := range []model.ActorID{"uid:10", "uid:11", "uid:12", "ext-1", "nobody"} {
			want := actor == "uid:11" || (actor == "uid:12" && action == model.ActionConversationReadWorkflow)
			if d := ask(t, p, actor, action, ""); d.Allow != want {
				t.Errorf("%s %s = %+v, want allow %v", actor, action, d, want)
			}
		}
	}
	for _, role := range []authz.Role{authz.RoleViewer, authz.RoleExternal, authz.RoleOperator} {
		for _, action := range writes {
			if slices.Contains(authz.Actions(role), action) {
				t.Errorf("%s holds %s", role, action)
			}
		}
	}
	if slices.Contains(authz.Actions(authz.RoleExternal), model.ActionConversationReadWorkflow) {
		t.Error("external holds conversation.read_workflow")
	}
}

// TestOnlyDeskActorsAreEligibleAssignees: uma asks the authorizer
// whether the assignee itself may assign on the conversation; heo grants
// it to desk actors alone, so a viewer, an external actor, an operator
// without desk, an actor with no role, the runtime's own actor and the
// bridge's are all refused as assignees.
func TestOnlyDeskActorsAreEligibleAssignees(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []any{},
		authz.KeyGrants: grants(
			map[string]any{"actor": "uid:10", "roles": []any{"operator"}},
			map[string]any{"actor": "uid:11", "roles": []any{"desk"}},
			map[string]any{"actor": "uid:12", "roles": []any{"viewer"}},
			map[string]any{"actor": "ext-1", "roles": []any{"external"}},
			map[string]any{"actor": "uid:13", "roles": []any{"operator", "desk"}},
		),
	})
	for actor, want := range map[model.ActorID]bool{
		"uid:11": true, "uid:13": true,
		"uid:10": false, "uid:12": false, "ext-1": false, "uid:99": false, service: false, authz.BridgeActor: false,
	} {
		if d := ask(t, p, actor, model.ActionConversationAssign, ""); d.Allow != want {
			t.Errorf("assignee %s = %+v, want eligible %v", actor, d, want)
		}
	}
}

// TestPluginGrants: plugin:<name> is a grantable actor class that
// holds only the roles it is granted; a svc: grant stays an error.
func TestPluginGrants(t *testing.T) {
	p := newPolicy(t, map[string]any{authz.KeyGrants: []any{
		map[string]any{"actor": "plugin:triage", "roles": []any{"viewer"}},
	}})
	pl := model.ActorID("plugin:triage")
	if ok, _ := p.Admits(pl); !ok {
		t.Error("granted plugin not admitted")
	}
	if d := ask(t, p, pl, model.ActionSubscriptionPoll, model.Public); !d.Allow {
		t.Errorf("plugin viewer poll = %+v", d)
	}
	if d := ask(t, p, pl, model.ActionMessageReply, model.Public); d.Allow {
		t.Error("plugin viewer may reply")
	}
	if d := ask(t, p, "plugin:other", model.ActionMessageRead, model.Public); d.Allow || d.ReasonCode != authz.ReasonNoRole {
		t.Errorf("ungranted plugin = %+v", d)
	}
	if got := p.Roles(owner); !slices.Contains(got, authz.RoleDesk) {
		t.Errorf("owner roles = %v", got)
	}
	if !authz.IsPlugin(pl) || authz.IsPlugin(owner) || authz.IsPlugin("svc:heo-bridge") {
		t.Error("IsPlugin wrong")
	}
	for _, bad := range []string{"svc:heo-bridge", "svc:anything", "plugin:", "plugin:Bad Name", "plugin:a/b", "plugin:svc:x"} {
		v := viper.New()
		v.Set(authz.KeyGrants, []any{map[string]any{"actor": bad, "roles": []any{"viewer"}}})
		if _, err := authz.FromConfig(v, tenant, owner, service); err == nil {
			t.Errorf("grant naming %q accepted", bad)
		}
	}
}

// TestObserverRole: observer is viewer plus the channel and operation
// reads the event bridge holds, so a plugin sees events about channels
// and operations; viewer stays as it was and observer writes nothing.
func TestObserverRole(t *testing.T) {
	p := newPolicy(t, map[string]any{
		authz.KeyOwner: []any{},
		authz.KeyGrants: grants(
			map[string]any{"actor": "plugin:obs", "roles": []any{"observer"}},
			map[string]any{"actor": "uid:12", "roles": []any{"viewer"}},
		),
	})
	for _, a := range authz.Actions(authz.RoleViewer) {
		if d := ask(t, p, "plugin:obs", a, model.Public); !d.Allow {
			t.Errorf("observer lacks viewer's %s: %+v", a, d)
		}
	}
	for _, a := range []string{model.ActionChannelRead, model.ActionOperationRead,
		model.ActionSubscriptionOpen, model.ActionSubscriptionPoll, model.ActionSubscriptionAck} {
		if d := ask(t, p, "plugin:obs", a, model.Public); !d.Allow {
			t.Errorf("observer denied %s: %+v", a, d)
		}
	}
	for _, a := range []string{model.ActionChannelRead, model.ActionOperationRead} {
		if d := ask(t, p, "uid:12", a, model.Public); d.Allow {
			t.Errorf("viewer gained %s: %+v", a, d)
		}
	}
	for _, a := range []string{model.ActionMessageReply, model.ActionMessageNote, model.ActionConversationTransition,
		model.ActionChannelConfigure, model.ActionOperationReplay, model.ActionOperationReconcile,
		model.ActionContactWrite, model.ActionTaskAct, model.ActionPrivacyExport} {
		if d := ask(t, p, "plugin:obs", a, model.Public); d.Allow {
			t.Errorf("observer allowed write %s: %+v", a, d)
		}
	}
}

// TestTrustWriteIsOperatorOnly: importing trust seeds what plugins may
// run unreviewed, so only the operator role holds it, and a plugin actor
// never does, whatever roles it holds.
func TestTrustWriteIsOperatorOnly(t *testing.T) {
	p := newPolicy(t, map[string]any{authz.KeyGrants: grants(
		map[string]any{"actor": "uid:700", "roles": []any{"desk"}},
		map[string]any{"actor": "uid:701", "roles": []any{"operator"}},
		map[string]any{"actor": "plugin:triage", "roles": []any{"desk", "operator"}},
	)})
	if d := ask(t, p, "uid:701", model.ActionTrustWrite, model.Public); !d.Allow {
		t.Errorf("operator trust.write = %+v", d)
	}
	if d := ask(t, p, "uid:700", model.ActionTrustWrite, model.Public); d.Allow || d.ReasonCode != authz.ReasonNotGranted {
		t.Errorf("desk trust.write = %+v", d)
	}
	if d := ask(t, p, "plugin:triage", model.ActionTrustWrite, model.Public); d.Allow || d.ReasonCode != authz.ReasonPluginDecide {
		t.Errorf("plugin trust.write = %+v, want %s", d, authz.ReasonPluginDecide)
	}
	// The runtime writes the counters for a person's manual change
	// (operation trust set), after the command checked the caller.
	if d := ask(t, p, service, model.ActionTrustWrite, model.Public); !d.Allow {
		t.Errorf("runtime trust.write = %+v", d)
	}
}
