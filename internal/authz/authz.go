// Package authz is heo's authorization policy: who may do what in the
// profile's runtime, read from the profile's configuration.
//
// One Policy answers both questions heo asks. The control gate asks
// whether a verified actor may reach the daemon at all (Admits); uma
// asks, for every operation, every read and every queued operation a
// worker dequeues, whether the actor may perform the action on the
// resource under its audience (Authorize). Both read the same roles,
// so every surface decides identically.
//
// The model is deny by default. An actor holds the roles the
// configuration grants it, and nothing else:
//
//	authorization:
//	  owner: [desk, operator]    # roles of the uid the daemon runs as
//	  grants:
//	    - actor: uid:1001
//	      roles: [operator, desk]
//	    - actor: uid:1002
//	      roles: [viewer]
//
// Roles are fixed: operator (channels, inboxes, delivery retry and
// reconcile, privacy, importing a channel's provider history), desk
// (conversations, replies, internal notes, contacts, delivery
// reconcile, provider membership and receipts, importing a
// conversation's provider history, and a conversation's workflow:
// assignment, snooze, links and its own read marker, and answering the
// interactions a provider reports),
// viewer (read-only, public content, and a conversation's assignee,
// snooze and links), observer (viewer plus channel and operation reads,
// for plugins) and external (public conversations, messages,
// attachments and events only: no workflow state at all). Content whose
// audience is not public — internal notes, restricted messages and
// their attachments and events — is for desk actors alone, whatever
// action reaches it.
//
// The runtime's own actor reads and receives on channels and stages
// the media that arrives with provider content, and nothing else:
// provider content enters as it, so provider content can never reply,
// configure or read on anyone's behalf. The event bridge has an actor
// of its own (BridgeActor) that only reads, for uma's event
// subscription, so that read access never reaches provider content.
package authz

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"hop.top/heo/internal/credentials"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// Configuration keys.
const (
	// KeyOwner lists the roles of the uid the daemon runs as.
	KeyOwner = "authorization.owner"
	// KeyGrants lists actors and the roles each holds.
	KeyGrants = "authorization.grants"
)

// ActionAssistRead is heo's own action, not uma's: running an LLM
// helper (summarize, triage) over a conversation the caller reads. It is
// what keeps the `external` role, which reads public conversations, from
// sending their text to a model; `desk` and `viewer` hold it. It adds no
// uma right: the helper still reads every message as the caller, so uma's
// and heo's audience rules decide what reaches the model. A draft is
// gated by the right to reply (message.reply, desk).
const ActionAssistRead = "heo.assist.read"

// ActionHookRead and ActionHookManage are heo's own actions, not uma's,
// for the hook verbs: reading the plugins' state, dead letters and a
// test run of a plugin (read), and running, replaying or dropping them
// (manage). They add no uma right: whatever a plugin's operations do is
// still decided as the plugin's actor. Only the operator role holds
// them.
const (
	ActionHookRead   = "heo.hook.read"
	ActionHookManage = "heo.hook.manage"
)

// PolicyVersion names every decision the policy makes.
const PolicyVersion = "heo-authz/1"

// Role is a fixed set of actions.
type Role string

// The roles.
const (
	RoleOperator Role = "operator"
	RoleDesk     Role = "desk"
	RoleViewer   Role = "viewer"
	RoleObserver Role = "observer"
	RoleExternal Role = "external"
)

// Roles lists every role, in documentation order.
var Roles = []Role{RoleOperator, RoleDesk, RoleViewer, RoleObserver, RoleExternal}

// Reason codes of a decision.
const (
	ReasonAllowed       = "heo.authz.allowed"
	ReasonService       = "heo.authz.runtime_service"
	ReasonBridge        = "heo.authz.runtime_bridge"
	ReasonApprovalChat  = "heo.authz.runtime_approval_chat"
	ReasonNoRole        = "heo.authz.no_role"
	ReasonNotGranted    = "heo.authz.not_granted"
	ReasonDeskOnly      = "heo.authz.desk_only"
	ReasonForeignTenant = "heo.authz.foreign_tenant"
	// ReasonPluginDecide: a plugin actor asked to decide a held operation.
	ReasonPluginDecide = "heo.authz.plugin_decide"
)

// Actions every reading role shares: public conversations, messages,
// attachments and the events that carry them.
var publicReads = []string{
	model.ActionInboxRead,
	model.ActionConversationRead,
	model.ActionMessageRead,
	model.ActionAttachmentRead,
	model.ActionSubscriptionOpen,
	model.ActionSubscriptionPoll,
	model.ActionSubscriptionAck,
	model.ActionSubscriptionClose,
	model.ActionEventList,
}

// Reads beyond conversations: contacts, identities, labels, work
// items, tasks, streams and interactions.
var directoryReads = []string{
	model.ActionContactRead,
	model.ActionContactReadAll,
	model.ActionIdentityRead,
	model.ActionIdentityHistory,
	model.ActionLabelRead,
	model.ActionWorkItemRead,
	model.ActionTaskRead,
	model.ActionStreamRead,
	model.ActionInteractionRead,
}

var viewerActions = concat(publicReads, directoryReads, []string{model.ActionConversationReadWorkflow, ActionAssistRead})

var roleActions = map[Role][]string{
	RoleOperator: {
		model.ActionChannelConfigure,
		model.ActionChannelRead,
		model.ActionInboxConfigure,
		model.ActionInboxRead,
		model.ActionOperationRead,
		model.ActionOperationReplay,
		model.ActionOperationReconcile,
		model.ActionOperationApprove,
		model.ActionOperationReject,
		model.ActionOperationEdit,
		model.ActionIdentityLinkOverride,
		model.ActionPrivacyRedact,
		model.ActionPrivacyRedactCascade,
		model.ActionPrivacyExport,
		model.ActionExtensionQuery,
		model.ActionChannelImport,
		model.ActionTrustWrite,
		ActionHookRead,
		ActionHookManage,
	},
	RoleDesk: concat(publicReads, directoryReads, []string{
		model.ActionConversationTransition,
		model.ActionConversationSignal,
		model.ActionConversationStart,
		model.ActionConversationLabel,
		model.ActionConversationAssign,
		model.ActionConversationSnooze,
		model.ActionConversationLink,
		model.ActionConversationMarkRead,
		model.ActionConversationStateAccount,
		model.ActionConversationStateShared,
		model.ActionConversationReadWorkflow,
		model.ActionMessageReply,
		model.ActionMessageNote,
		model.ActionMessageEdit,
		model.ActionMessageDelete,
		model.ActionMessageReact,
		model.ActionMessageReceipt,
		model.ActionMessageEphemeral,
		model.ActionConversationMembership,
		model.ActionChannelImport,
		model.ActionContactWrite,
		model.ActionContactMerge,
		model.ActionIdentityLink,
		model.ActionIdentityRelink,
		model.ActionIdentityUnlink,
		model.ActionWorkItemAct,
		model.ActionTaskAct,
		model.ActionOperationRead,
		model.ActionOperationReconcile,
		model.ActionOperationApprove,
		model.ActionOperationReject,
		model.ActionOperationEdit,
		model.ActionAttachmentPrepare,
		model.ActionAttachmentUpload,
		model.ActionStreamWrite,
		model.ActionInteractionRespond,
		model.ActionLabelConfigure,
		model.ActionExtensionQuery,
		model.ActionExtensionExecute,
		ActionAssistRead,
	}),
	RoleViewer: viewerActions,
	// observer is viewer plus the channel and operation reads the event
	// bridge holds: what a plugin needs to receive events about both.
	RoleObserver: concat(viewerActions, []string{model.ActionChannelRead, model.ActionOperationRead}),
	RoleExternal: publicReads,
}

// serviceActions are the runtime's own: binding receivers and webhooks,
// accepting ingress and staging the media that arrives with it (uma
// prepares and uploads an inbound attachment as the sink's principal).
// Staging writes bytes under the audience the provider set; reading
// them back stays with the roles. The trust ladder reads the trust
// counters as the runtime: a plugin actor that is held never reads
// the counts that would release it. It also writes the counters for a
// person's manual change (`operation trust set`): the command checks
// the caller holds the desk or operator role first, and uma's own
// write rights are the runtime's, never a plugin's.
var serviceActions = []string{
	model.ActionChannelRead, model.ActionChannelReceive,
	model.ActionAttachmentPrepare, model.ActionAttachmentUpload,
	model.ActionTrustRead, model.ActionTrustReset, model.ActionTrustWrite,
}

// BridgeActor is the actor the event bridge reads uma's event log as.
// It is not the runtime's actor (ServiceActor in the daemon): provider
// content enters as that one, and nothing the bridge may read is
// readable by provider content. Like it, it is reserved (every svc:
// actor is) and admitted to no daemon command.
const BridgeActor model.ActorID = "svc:heo-bridge"

// bridgeActions are what uma asks of a subscriber before it delivers an
// event: the subscription's own actions (never close: the subscription
// outlives the process, and never list), the event's inbox and the
// resource each builtin event kind is about. They are reads only. They
// apply at every audience: the bridge republishes references to
// internal events too, and its bus keeps those for desk-scoped
// subscribers (internal/bridge). Events of a registered extension kind
// need that kind's own action, which the bridge does not hold: they are
// not bridged.
var bridgeActions = []string{
	model.ActionSubscriptionOpen, model.ActionSubscriptionPoll, model.ActionSubscriptionAck,
	model.ActionInboxRead, model.ActionChannelRead, model.ActionContactRead, model.ActionIdentityRead,
	model.ActionConversationRead, model.ActionMessageRead, model.ActionOperationRead, model.ActionTaskRead,
	model.ActionWorkItemRead, model.ActionAttachmentRead, model.ActionStreamRead, model.ActionInteractionRead,
	model.ActionConversationReadWorkflow,
}

// ApprovalChatActor is the actor the daemon's approval chat service runs
// as: it reads uma's event log like the bridge, reads the messages and
// identities of the conversations it was configured for, and speaks in
// them (a reply, the edit that closes an announcement). It decides
// nothing: a decision runs as the member's mapped actor. Like the
// others it is reserved (every svc: actor is) and admitted to no daemon
// command.
const ApprovalChatActor model.ActorID = "svc:heo-approval-chat"

// approvalChatActions are the bridge's reads and the two writes of its
// announcements.
var approvalChatActions = concat(bridgeActions, []string{model.ActionMessageReply, model.ActionMessageEdit})

// decidesHeld reports whether action decides a held operation, or seeds
// the trust that decides them: a plugin never earns its own.
func decidesHeld(action string) bool {
	switch action {
	case model.ActionOperationApprove, model.ActionOperationReject, model.ActionOperationEdit, model.ActionTrustWrite:
		return true
	}
	return false
}

// PluginActor is the actor of the plugin called name: plugin:<name>.
// Unlike svc: actors it is grantable, a plugin holds only the roles
// granted to it, and it is authenticated by its own token, never by uid.
func PluginActor(name string) model.ActorID { return credentials.PluginActor(name) }

// IsPlugin reports whether actor is a plugin's.
func IsPlugin(actor model.ActorID) bool {
	return strings.HasPrefix(string(actor), credentials.PluginActorPrefix)
}

func concat(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

// Actions returns the actions role grants.
func Actions(role Role) []string { return slices.Clone(roleActions[role]) }

// Defaults are the policy's built-in values: the daemon's own uid holds
// desk and operator, and no one else holds anything.
func Defaults() map[string]any {
	return map[string]any{KeyOwner: []string{string(RoleDesk), string(RoleOperator)}}
}

// OwnerActor is the actor of the uid this process runs as: the
// installation owner when this process is the daemon.
func OwnerActor() model.ActorID { return model.ActorID("uid:" + strconv.Itoa(os.Geteuid())) }

// Policy is the profile's authorization policy. It is immutable.
type Policy struct {
	tenant  model.TenantID
	service model.ActorID
	roles   map[model.ActorID][]Role
}

var _ policy.Authorizer = (*Policy)(nil)

// New builds the policy for tenant from cfg: owner holds cfg.Owner,
// each granted actor its roles, service the runtime's own actions.
func New(cfg Config, tenant model.TenantID, owner, service model.ActorID) *Policy {
	p := &Policy{tenant: tenant, service: service, roles: map[model.ActorID][]Role{}}
	add := func(actor model.ActorID, roles []Role) {
		for _, r := range roles {
			if !slices.Contains(p.roles[actor], r) {
				p.roles[actor] = append(p.roles[actor], r)
			}
		}
	}
	add(owner, cfg.Owner)
	for _, g := range cfg.Grants {
		add(g.Actor, g.Roles)
	}
	return p
}

// Roles returns the roles actor holds.
func (p *Policy) Roles(actor model.ActorID) []Role { return slices.Clone(p.roles[actor]) }

// Admits reports whether actor may reach the daemon's commands at all:
// it holds a role. What it may do there is Authorize's decision, made
// for every operation. The reason is set when it may not.
func (p *Policy) Admits(actor model.ActorID) (bool, string) {
	if len(p.roles[actor]) == 0 {
		return false, ReasonNoRole
	}
	return true, ""
}

// Authorize is uma's policy hook: may a.Principal perform a.Action on
// a.Resource under a.Audience. Only an explicit grant allows; content
// whose audience is set and not public needs the desk role besides.
func (p *Policy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	decide := func(allow bool, reason string) (policy.Decision, error) {
		return policy.Decision{Allow: allow, PolicyVersion: PolicyVersion, ReasonCode: reason}, nil
	}
	deny := func(reason string) (policy.Decision, error) { return decide(false, reason) }
	if a.Principal.Tenant != p.tenant {
		return deny(ReasonForeignTenant)
	}
	if a.Principal.Actor == BridgeActor {
		if slices.Contains(bridgeActions, a.Action) {
			return decide(true, ReasonBridge)
		}
		return deny(ReasonNotGranted)
	}
	if a.Principal.Actor == ApprovalChatActor {
		if slices.Contains(approvalChatActions, a.Action) {
			return decide(true, ReasonApprovalChat)
		}
		return deny(ReasonNotGranted)
	}
	if a.Principal.Actor == p.service {
		if slices.Contains(serviceActions, a.Action) {
			return decide(true, ReasonService)
		}
		return deny(ReasonNotGranted)
	}
	roles := p.roles[a.Principal.Actor]
	if len(roles) == 0 {
		return deny(ReasonNoRole)
	}
	// Proposing grants no right to decide: whatever roles a plugin
	// actor holds, it never approves, rejects or edits a held operation.
	if a.Principal.Actor.IsPlugin() && decidesHeld(a.Action) {
		return deny(ReasonPluginDecide)
	}
	granted := false
	for _, r := range roles {
		if slices.Contains(roleActions[r], a.Action) {
			granted = true
			break
		}
	}
	if !granted {
		return deny(ReasonNotGranted)
	}
	switch vis := a.Audience.Visibility; vis {
	case "", model.Public:
	case model.Internal, model.Restricted:
		if !slices.Contains(roles, RoleDesk) {
			return deny(ReasonDeskOnly)
		}
	default:
		// A visibility this policy does not know fails closed.
		return deny(ReasonDeskOnly)
	}
	return decide(true, ReasonAllowed)
}

// Permits reports whether principal may perform action at all — the
// tenant and role checks Authorize makes before any audience rule —
// and the decision's reason code when it may not. Commands ask it
// before reaching the runtime, so a caller without the role is refused
// UNAUTHORIZED on every surface, where uma alone would answer a list
// with nothing and a lookup with not found. uma still decides every
// operation after it.
func (p *Policy) Permits(principal model.Principal, action string) (bool, string) {
	return p.PermitsUnder(principal, action, model.Audience{})
}

// PermitsUnder is Permits for content under audience: the audience
// rule besides, so a command reading internal notes refuses a caller
// who is not a desk actor before any read.
func (p *Policy) PermitsUnder(principal model.Principal, action string, audience model.Audience) (bool, string) {
	d, err := p.Authorize(context.Background(), policy.Authorization{Principal: principal, Action: action, Audience: audience})
	if err != nil || !d.Allow {
		return false, d.ReasonCode
	}
	return true, ""
}

// Sees reports whether principal may read a message under audience.
// heo filters every message it shows through it, whatever uma
// returned: content that is not public never reaches a caller who is
// not a desk actor, on any surface.
func (p *Policy) Sees(principal model.Principal, audience model.Audience) bool {
	ok, _ := p.PermitsUnder(principal, model.ActionMessageRead, audience)
	return ok
}

// Grant is one configured actor and its roles.
type Grant struct {
	Actor model.ActorID
	Roles []Role
}

// Config is the authorization section, read.
type Config struct {
	Owner  []Role
	Grants []Grant
}

// Problem is one finding about the policy's configuration.
type Problem struct {
	Key     string
	Message string
	Warning bool
}

// problemsError joins blocking problems into one error, nil when every
// problem is a warning.
func problemsError(probs []Problem) error {
	var msgs []string
	for _, p := range probs {
		if !p.Warning {
			msgs = append(msgs, p.Key+": "+p.Message)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("authorization policy: %s", strings.Join(msgs, "; "))
}

// roleList reads a list of role names: a YAML list, or a comma list
// from -c or the environment.
func roleList(raw any) ([]string, bool) {
	switch v := raw.(type) {
	case nil:
		return nil, true
	case string:
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, true
	case []string:
		return v, true
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, false
			}
			out = append(out, strings.TrimSpace(s))
		}
		return out, true
	}
	return nil, false
}

func parseRoles(key string, raw any, probs []Problem) ([]Role, []Problem) {
	names, ok := roleList(raw)
	if !ok {
		return nil, append(probs, Problem{Key: key, Message: "want a list of roles: " + roleNames()})
	}
	var out []Role
	for _, n := range names {
		r := Role(n)
		if !slices.Contains(Roles, r) {
			probs = append(probs, Problem{Key: key, Message: fmt.Sprintf("unknown role %q; roles: %s", n, roleNames())})
			continue
		}
		out = append(out, r)
	}
	return out, probs
}

func roleNames() string {
	names := make([]string, len(Roles))
	for i, r := range Roles {
		names[i] = string(r)
	}
	return strings.Join(names, ", ")
}

// grantFields are the fields of one grant.
var grantFields = []string{"actor", "roles"}

// read parses the authorization section from get, reporting every
// structural problem. service is the runtime's reserved actor.
func read(get func(string) any, service model.ActorID) (Config, []Problem) {
	var cfg Config
	var probs []Problem
	cfg.Owner, probs = parseRoles(KeyOwner, get(KeyOwner), probs)
	raw := get(KeyGrants)
	if raw == nil {
		return cfg, probs
	}
	list, ok := raw.([]any)
	if !ok {
		return cfg, append(probs, Problem{Key: KeyGrants, Message: "want a list of {actor, roles} entries"})
	}
	seen := map[model.ActorID]bool{}
	for i, e := range list {
		key := fmt.Sprintf("%s[%d]", KeyGrants, i)
		m, ok := e.(map[string]any)
		if !ok {
			probs = append(probs, Problem{Key: key, Message: "want an entry with actor and roles"})
			continue
		}
		for _, f := range sortedKeys(m) {
			if !slices.Contains(grantFields, f) {
				probs = append(probs, Problem{Key: key + "." + f, Message: "unknown field; fields: actor, roles"})
			}
		}
		actor, bad := grantActor(key+".actor", m["actor"], service)
		if bad != nil {
			probs = append(probs, *bad)
		}
		roles, rp := parseRoles(key+".roles", m["roles"], nil)
		probs = append(probs, rp...)
		if len(rp) == 0 && len(roles) == 0 {
			probs = append(probs, Problem{Key: key + ".roles", Warning: true, Message: "empty: the grant grants nothing"})
		}
		if bad != nil {
			continue
		}
		if seen[actor] {
			probs = append(probs, Problem{Key: key + ".actor", Warning: true,
				Message: fmt.Sprintf("%s already granted above: its roles add up", actor)})
		}
		seen[actor] = true
		cfg.Grants = append(cfg.Grants, Grant{Actor: actor, Roles: roles})
	}
	return cfg, probs
}

func grantActor(key string, raw any, service model.ActorID) (model.ActorID, *Problem) {
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", &Problem{Key: key, Message: "required: uid:<n>, plugin:<name>, or the subject a token surface verifies"}
	}
	actor := model.ActorID(s)
	if err := actor.Validate(); err != nil {
		return "", &Problem{Key: key, Message: fmt.Sprintf("%q is not a valid actor: %v", s, err)}
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "aps:") || strings.HasPrefix(lower, "heo:"):
		return "", &Problem{Key: key, Message: fmt.Sprintf(
			"%q names a profile, not an actor: a profile binding grants no authority; grant the uid or token subject that calls", s)}
	case actor == service || strings.HasPrefix(lower, "svc:"):
		return "", &Problem{Key: key, Message: fmt.Sprintf("%q is reserved for the runtime's own actor", s)}
	case strings.HasPrefix(lower, credentials.PluginActorPrefix):
		if err := credentials.ValidPluginName(strings.TrimPrefix(s, credentials.PluginActorPrefix)); err != nil {
			return "", &Problem{Key: key, Message: fmt.Sprintf("%q is not a plugin actor: %v", s, err)}
		}
	}
	return actor, nil
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
