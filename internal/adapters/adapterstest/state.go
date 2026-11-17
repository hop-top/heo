package adapterstest

import (
	"context"
	"slices"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// StateOptions is what a fake declares of provider conversation state: the
// capabilities it declares (archive, move, read_state, flag, trash) and the
// limits it reports for them, as a mail or chat adapter does live.
type StateOptions struct {
	// Caps are the state capabilities declared; every one when empty.
	Caps []model.Capability
	// Scope is whose view a change touches; account when empty.
	Scope model.StateScope
	// Shape is the move shape; labels when empty.
	Shape model.MoveShape
	// Kinds are the flag kinds the provider has; star and important when
	// empty.
	Kinds []string
	// Roles are the destination roles the provider resolves; all four
	// when empty.
	Roles []string
}

// StateCapabilities are the five state capabilities.
var StateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// StateChange is one provider state change the fake applied: the verb
// (archive, unarchive, move, read, unread, flag, unflag, trash, untrash)
// and what it named.
type StateChange struct {
	Verb        string
	Kind        string
	Destination model.MoveDestination
	Add, Remove []string
}

// detail is the line's tail: the flag kind, the destination or the label
// changes.
func (c StateChange) detail() string {
	switch {
	case c.Kind != "":
		return " kind=" + c.Kind
	case c.Destination.Role != "":
		return " role=" + c.Destination.Role
	case c.Destination.ID != "":
		return " to=" + c.Destination.ID
	}
	var parts []string
	for _, a := range c.Add {
		parts = append(parts, "+"+a)
	}
	for _, r := range c.Remove {
		parts = append(parts, "-"+r)
	}
	if len(parts) > 0 {
		return " " + strings.Join(parts, " ")
	}
	return ""
}

// DeclareState makes f declare the state capabilities of o and apply
// each change: the provider accepts it and records it (see Effects,
// LogEffects as "state <verb> <route> ..."). Its capability status
// reports the limits of o, as a live adapter does. Call it before the
// engine starts.
func (f *Factory) DeclareState(o StateOptions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(o.Caps) == 0 {
		o.Caps = slices.Clone(StateCapabilities)
	}
	if o.Scope == "" {
		o.Scope = model.ScopeAccount
	}
	if o.Shape == "" {
		o.Shape = model.MoveShapeLabels
	}
	if len(o.Kinds) == 0 {
		o.Kinds = []string{model.FlagStar, model.FlagImportant}
	}
	if len(o.Roles) == 0 {
		o.Roles = []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}
	}
	f.state = &o
}

// stateLimits is the limits the fake reports for c, when it declares it.
func (f *Factory) stateLimits(c model.Capability) (model.Extension, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c == model.CapabilityReplyRecipients && f.recipients != nil {
		return f.recipients.Extension(), true
	}
	if f.state == nil || !slices.Contains(f.state.Caps, c) {
		return model.Extension{}, false
	}
	l := model.StateLimits{Scope: f.state.Scope}
	switch c {
	case model.CapabilityFlag:
		l.Kinds = slices.Clone(f.state.Kinds)
	case model.CapabilityMove:
		l.Shape, l.Roles = f.state.Shape, slices.Clone(f.state.Roles)
	}
	return l.Extension(), true
}

func (i *instance) stateEffect(r adapter.Effect, c StateChange) (adapter.Outcome, error) {
	return i.effect(Effect{Kind: "state", State: c}, &r.Route)
}

// Archive applies an archive or an unarchive, after DeclareState.
func (i *instance) Archive(_ context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	v := "archive"
	if r.Undo {
		v = "unarchive"
	}
	return i.stateEffect(r.Effect, StateChange{Verb: v})
}

// Move applies a move, after DeclareState.
func (i *instance) Move(_ context.Context, r adapter.MoveRequest) (adapter.Outcome, error) {
	return i.stateEffect(r.Effect, StateChange{Verb: "move", Destination: r.Destination, Add: r.Add, Remove: r.Remove})
}

// SetReadState marks a conversation read or unread, after DeclareState.
func (i *instance) SetReadState(_ context.Context, r adapter.ReadStateRequest) (adapter.Outcome, error) {
	v := "unread"
	if r.Read {
		v = "read"
	}
	return i.stateEffect(r.Effect, StateChange{Verb: v})
}

// Flag sets or clears a flag, after DeclareState.
func (i *instance) Flag(_ context.Context, r adapter.FlagRequest) (adapter.Outcome, error) {
	v := "flag"
	if r.Undo {
		v = "unflag"
	}
	return i.stateEffect(r.Effect, StateChange{Verb: v, Kind: r.Kind})
}

// Trash trashes or restores a conversation, after DeclareState.
func (i *instance) Trash(_ context.Context, r adapter.TrashRequest) (adapter.Outcome, error) {
	v := "trash"
	if r.Undo {
		v = "untrash"
	}
	return i.stateEffect(r.Effect, StateChange{Verb: v})
}
