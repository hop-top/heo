package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// Provider conversation state: what the provider itself holds about a
// conversation (a mailbox's archive, folder or labels, read state, star
// and trash; a chat room's archive), apart from uma's own status, labels
// and the caller's read marker, none of which these verbs touch. uma
// carries one capability per verb pair and one operation per change; the
// change reaches the provider only when a worker runs it, and uma's
// provider state follows only once the provider applied it.

// StateChange is one provider state change asked of a conversation.
type StateChange struct {
	// Verb is the change.
	Verb model.StateVerb
	// Kind is a flag's kind, star or important.
	Kind string
	// Destination is a move's target: a provider ID or a role.
	Destination model.MoveDestination
	// Add and Remove are a move's provider label IDs.
	Add, Remove []string
}

// StateVerbs are the state verbs, in documentation order.
var StateVerbs = []model.StateVerb{
	model.StateVerbArchive, model.StateVerbUnarchive, model.StateVerbMove,
	model.StateVerbRead, model.StateVerbUnread,
	model.StateVerbFlag, model.StateVerbUnflag,
	model.StateVerbTrash, model.StateVerbUntrash,
}

// StateVerbName is the heo command that runs v, as the verb tables name
// it: "conversation archive", "conversation read".
func StateVerbName(v model.StateVerb) string { return "conversation " + string(v) }

// CheckStateChange checks the change asked for: the usage rules, which
// need no record and no adapter. What a channel accepts (a flag kind, a
// role, a label move) is the channel's, checked by a preview and by uma.
func CheckStateChange(c StateChange) error {
	if _, ok := c.Verb.Capability(); !ok {
		return model.NewError(model.Validation, fmt.Sprintf("verb %q: not a provider state verb", c.Verb))
	}
	switch c.Verb {
	case model.StateVerbFlag, model.StateVerbUnflag:
		if !model.IsFlagKind(c.Kind) {
			return model.NewError(model.Validation, fmt.Sprintf("kind %q: want %s or %s", c.Kind, model.FlagStar, model.FlagImportant))
		}
	case model.StateVerbMove:
		labels := len(c.Add)+len(c.Remove) > 0
		switch {
		case c.Destination.ID != "" && c.Destination.Role != "":
			return model.NewError(model.Validation, "destination: name a provider ID (--to) or a role (--role), not both")
		case !c.Destination.IsZero() && labels:
			return model.NewError(model.Validation, "destination: --to and --role are exclusive with --add and --remove")
		case c.Destination.IsZero() && !labels:
			return model.NewError(model.Validation, "destination: name where to move it (--to or --role) or the labels to change (--add, --remove)")
		case c.Destination.Role != "" && !model.IsDestinationRole(c.Destination.Role):
			return model.NewError(model.Validation, fmt.Sprintf("role %q: want inbox, archive, trash or junk", c.Destination.Role))
		}
	}
	return nil
}

// Asked says what the change asks, for a plan and a row.
func (c StateChange) Asked() string {
	switch c.Verb {
	case model.StateVerbFlag, model.StateVerbUnflag:
		return string(c.Verb) + " " + c.Kind
	case model.StateVerbMove:
		var parts []string
		switch {
		case c.Destination.Role != "":
			parts = append(parts, "to the "+c.Destination.Role+" role")
		case c.Destination.ID != "":
			parts = append(parts, "to "+c.Destination.ID)
		}
		if len(c.Add) > 0 {
			parts = append(parts, "add "+strings.Join(c.Add, ", "))
		}
		if len(c.Remove) > 0 {
			parts = append(parts, "remove "+strings.Join(c.Remove, ", "))
		}
		return "move " + strings.Join(parts, "; ")
	}
	return string(c.Verb)
}

// HoldClass is the verb family and target class the hold point and the
// trust ladder see the change under. The classes the ladder's table
// names are the ones its verbs map to: read, unread, star, unstar,
// archive, trash and spam are state:<class>, and any other move is
// move. A move into the trash or junk role is the trash or spam it
// is, so a threshold cannot be skirted by naming the folder.
func (c StateChange) HoldClass() (verb, class string) {
	switch c.Verb {
	case model.StateVerbFlag:
		return "state", c.Kind
	case model.StateVerbUnflag:
		return "state", "un" + c.Kind
	case model.StateVerbMove:
		switch c.Destination.Role {
		case model.RoleTrash:
			return "state", "trash"
		case model.RoleJunk:
			return "state", "spam"
		}
		switch {
		case c.Destination.Role != "":
			return "move", c.Destination.Role
		case c.Destination.ID != "":
			return "move", c.Destination.ID
		}
		var ids []string
		for _, a := range c.Add {
			ids = append(ids, "+"+a)
		}
		for _, r := range c.Remove {
			ids = append(ids, "-"+r)
		}
		return "move", strings.Join(ids, ",")
	}
	return "state", string(c.Verb)
}

// StatePlan is a validated provider state change: the command it will
// be submitted under, the conversation as it is now and, from a
// preview, what was checked of the adapter and what the channel reports
// of the verb.
type StatePlan struct {
	// Command is the call and operation the change is submitted under,
	// and where a hold proposal is set.
	Command      model.Command
	Change       StateChange
	Conversation model.Conversation
	// Resolved are the provider mailbox names of a move the plan turned
	// into the IDs it files by; Change holds the IDs.
	Resolved []Resolved
	// Declaration is set by a preview only.
	Declaration *Declaration
	// Limits and Scope are what the channel reported of the verb now,
	// read by a preview and by a plugin actor's hold. Limits is nil when
	// nothing was read.
	Limits *model.StateLimits
	// Action is the uma action the change is authorized as: by the
	// scope the channel reports, conversation.state_account when none
	// was read.
	Action   string
	deferred error
}

// request builds the uma request of the change.
func (p StatePlan) request() (any, error) {
	ref, c := p.Conversation.Ref, p.Change
	var req interface{ Validate() error }
	switch c.Verb {
	case model.StateVerbArchive, model.StateVerbUnarchive:
		req = model.ArchiveConversation{Command: p.Command, Conversation: ref, Undo: c.Verb == model.StateVerbUnarchive}
	case model.StateVerbMove:
		req = model.MoveConversation{Command: p.Command, Conversation: ref, Destination: c.Destination, Add: c.Add, Remove: c.Remove}
	case model.StateVerbRead, model.StateVerbUnread:
		req = model.SetConversationRead{Command: p.Command, Conversation: ref, Read: c.Verb == model.StateVerbRead}
	case model.StateVerbFlag, model.StateVerbUnflag:
		req = model.FlagConversation{Command: p.Command, Conversation: ref, Kind: c.Kind, Undo: c.Verb == model.StateVerbUnflag}
	case model.StateVerbTrash, model.StateVerbUntrash:
		req = model.TrashConversation{Command: p.Command, Conversation: ref, Undo: c.Verb == model.StateVerbUntrash}
	default:
		return nil, model.NewError(model.Validation, fmt.Sprintf("verb %q: not a provider state verb", c.Verb))
	}
	return req, req.Validate()
}

// resolveMove resolves the provider mailbox names of a move (its
// destination ID and its labels) to IDs, as resolveMailboxes does; a role
// is no name and is left alone. ch is not changed.
func (s Service) resolveMove(ctx context.Context, in model.Inbox, ch StateChange) (StateChange, []Resolved, error) {
	var vals []string
	if ch.Destination.ID != "" {
		vals = append(vals, ch.Destination.ID)
	}
	vals = append(append(vals, ch.Add...), ch.Remove...)
	ids, res, err := s.resolveMailboxes(ctx, in, vals)
	if err != nil || len(res) == 0 {
		return ch, nil, err
	}
	if ch.Destination.ID != "" {
		ch.Destination.ID, ids = ids[0], ids[1:]
	}
	ch.Add, ch.Remove = slices.Clone(ids[:len(ch.Add)]), slices.Clone(ids[len(ch.Add):])
	if len(ch.Add) == 0 {
		ch.Add = nil
	}
	if len(ch.Remove) == 0 {
		ch.Remove = nil
	}
	return ch, res, nil
}

// PlanState builds and checks the provider state change ch of
// conversation id, at revision rev when set, under operation op. It
// writes nothing. The channel's adapter must declare the verb's
// capability, in a preview and on submission alike. A preview also reads
// the channel's live status of the capability, as uma's gate does at
// submission, and refuses what the channel does not offer: a capability
// that is unavailable, a flag kind, a role or a label move the provider
// does not hold. A plugin actor's change is read the same way, to know
// the scope it is authorized as; a submission otherwise leaves the live
// checks, and a stale revision, to uma.
func (s Service) PlanState(ctx context.Context, id string, ch StateChange, rev model.Revision, op model.OperationID, mode channels.Mode) (StatePlan, error) {
	if err := CheckStateChange(ch); err != nil {
		return StatePlan{}, err
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return StatePlan{}, err
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return StatePlan{}, err
	}
	capability, _ := ch.Verb.Capability()
	decl, _, err := s.declared(ctx, in, StateVerbName(ch.Verb), capability)
	if err != nil {
		return StatePlan{}, err
	}
	var resolved []Resolved
	if ch.Verb == model.StateVerbMove {
		if ch, resolved, err = s.resolveMove(ctx, in, ch); err != nil {
			return StatePlan{}, err
		}
	}
	p := StatePlan{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Change:  ch, Resolved: resolved, Conversation: conv, Action: model.ActionConversationStateAccount,
	}
	if rev != "" {
		if p.deferred, err = channels.CheckRevision("conversation", id, conv.Revision, rev, mode); err != nil {
			return StatePlan{}, err
		}
	}
	if _, err := p.request(); err != nil {
		return StatePlan{}, err
	}
	if mode == channels.Preview {
		p.Declaration = &decl
	}
	if mode == channels.Preview || s.Holding() {
		l, err := s.stateLimits(ctx, conv, ch, capability)
		switch {
		case err == nil:
			p.Limits = &l
			if a, ok := l.Scope.Action(); ok {
				p.Action = a
			}
		case mode == channels.Preview:
			return StatePlan{}, err
		}
	}
	return p, nil
}

// stateLimits reads the channel's live status of capability c for the
// conversation, as the caller (uma's Conversations.Capabilities,
// authorized as a read of the conversation), and checks the change
// against the limits it reports, as uma's gate does at submission. It
// opens the channel's adapter and may reach the provider.
func (s Service) stateLimits(ctx context.Context, conv model.Conversation, ch StateChange, c model.Capability) (model.StateLimits, error) {
	name := StateVerbName(ch.Verb)
	statuses, err := s.Client.Conversations().Capabilities(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: conv.Ref})
	if err != nil {
		return model.StateLimits{}, err
	}
	i := slices.IndexFunc(statuses, func(st model.CapabilityStatus) bool { return st.Name == c })
	if i < 0 {
		return model.StateLimits{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"conversation %s: its channel's adapter does not report %s, which `%s` needs", conv.Ref.ID, c, name))
	}
	st := statuses[i]
	if !st.Available {
		code := st.Reason
		if code == "" {
			code = model.Unavailable
		}
		return model.StateLimits{}, model.NewError(code, fmt.Sprintf(
			"conversation %s: %s is %s on its channel now, so `%s` is refused", conv.Ref.ID, c, code, name))
	}
	l, err := model.ParseStateLimits(st.Limits)
	if err != nil {
		return model.StateLimits{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"conversation %s: its channel reports no readable limits for %s, so `%s` is refused", conv.Ref.ID, c, name))
	}
	return l, checkStateLimits(name, ch, l)
}

// checkStateLimits refuses a change the reported limits do not allow:
// a flag kind the provider has not, a role it does not resolve, or a
// label move on a provider that files a conversation in one folder.
func checkStateLimits(name string, ch StateChange, l model.StateLimits) error {
	switch ch.Verb {
	case model.StateVerbFlag, model.StateVerbUnflag:
		if !slices.Contains(l.Kinds, ch.Kind) {
			return model.NewError(model.Unsupported, fmt.Sprintf(
				"%s: this channel's provider has no %s flag; it has %s", name, ch.Kind, listOrNone(l.Kinds)))
		}
	case model.StateVerbMove:
		if l.Shape == model.MoveShapeFolder && len(ch.Add)+len(ch.Remove) > 0 {
			return model.NewError(model.Unsupported, name+
				": this channel's provider files a conversation in one folder, so it takes a destination (--to or --role), not label changes")
		}
		if r := ch.Destination.Role; r != "" && !slices.Contains(l.Roles, r) {
			return model.NewError(model.Unsupported, fmt.Sprintf(
				"%s: this channel's provider does not resolve the %s role; it resolves %s", name, r, listOrNone(l.Roles)))
		}
	}
	return nil
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}

// SubmitState submits p: uma queues the change durably, and the
// daemon's workers send it to the provider. It returns the conversation
// as uma holds it, whose provider state moves only once the provider
// applied the change, never its status, labels or read marker.
func (s Service) SubmitState(ctx context.Context, p StatePlan) (model.Conversation, model.Acceptance, error) {
	req, err := p.request()
	if err != nil {
		return model.Conversation{}, model.Acceptance{}, err
	}
	conv := s.Client.Conversations()
	var acc model.Acceptance
	switch r := req.(type) {
	case model.ArchiveConversation:
		acc, err = conv.Archive(ctx, r)
	case model.MoveConversation:
		acc, err = conv.Move(ctx, r)
	case model.SetConversationRead:
		acc, err = conv.SetRead(ctx, r)
	case model.FlagConversation:
		acc, err = conv.Flag(ctx, r)
	case model.TrashConversation:
		acc, err = conv.Trash(ctx, r)
	}
	if err != nil {
		return model.Conversation{}, acc, channels.Explain(err, p.deferred)
	}
	c, err := conv.Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Conversation.Ref})
	return c, acc, err
}
