package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/model"
)

// Mode is how a plan holds the conflicts uma enforces itself at commit:
// a stale revision, or a key or destination another record holds.
type Mode int

const (
	// Preview holds every check final, as the write would meet it now:
	// what --dry-run shows.
	Preview Mode = iota
	// Submit leaves those conflicts to uma, which replays a retried
	// operation — same key, same request — and refuses anything else,
	// so a retry after a lost answer gets the stored operation instead
	// of a conflict with its own first attempt. heo's own checks stay
	// final.
	Submit
)

// conflicts collects the conflicts a plan leaves to uma.
type conflicts struct {
	m        Mode
	deferred error
}

// uma returns err in a preview; on submission it keeps the first such
// conflict, to explain uma's refusal should uma refuse, and returns nil.
func (c *conflicts) uma(err error) error {
	if c.m != Submit {
		return err
	}
	if c.deferred == nil {
		c.deferred = err
	}
	return nil
}

// explain is uma's refusal of a submission, as the conflict heo saw
// and left to uma describes it when there is one: heo's message names
// the record and revision.
func explain(err, deferred error) error {
	if deferred != nil && model.IsCode(err, model.Conflict) {
		return deferred
	}
	return err
}

// ChannelChange is what a channel create or update sets. A nil field is
// left as it is on update and empty on create.
type ChannelChange struct {
	// Type is the adapter type key; create only, a channel never
	// changes type.
	Type model.TypeKey
	// Key is the host-set name, unique among the tenant's channels.
	Key *string
	// Configuration is the adapter configuration's data, a JSON object;
	// its schema name and version are the adapter's.
	Configuration json.RawMessage
	// CredentialRef names the credential in the profile's credential
	// backend; never the credential itself.
	CredentialRef *string
}

func (c ChannelChange) empty() bool {
	return c.Key == nil && c.Configuration == nil && c.CredentialRef == nil
}

// ChannelPlan is a validated channel configure request, the record it
// would replace (nil for a creation) and advisories.
type ChannelPlan struct {
	Request  model.ConfigureChannel
	Current  *model.Channel
	Warnings []string
	// deferred is the first conflict left to uma on submission.
	deferred error
}

// PlanChannelCreate builds and checks the request creating a channel
// under operation op.
func (s Service) PlanChannelCreate(ctx context.Context, c ChannelChange, op model.OperationID, m Mode) (ChannelPlan, error) {
	if c.Type == "" {
		return ChannelPlan{}, model.NewError(model.Validation, "channel.type: required")
	}
	if _, ok := s.Adapters.Lookup(c.Type); !ok {
		return ChannelPlan{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"channel.type: no adapter %s in this build; `heo adapter list` shows the installed ones", c.Type))
	}
	ch := model.Channel{Ref: model.ChannelRef{Tenant: s.Tenant}, Type: c.Type}
	if err := s.applyChannel(&ch, c); err != nil {
		return ChannelPlan{}, err
	}
	req := model.ConfigureChannel{Command: model.Command{Call: call(), Operation: op}, Channel: ch}
	cs := &conflicts{m: m}
	warn, err := s.checkChannel(ctx, req, cs)
	return ChannelPlan{Request: req, Warnings: warn, deferred: cs.deferred}, err
}

// PlanChannelUpdate builds and checks the request replacing the channel
// ref at revision rev under operation op: the channel as it is now with
// c applied. A revision other than the channel's current one is a
// conflict: the caller would change what it did not read.
func (s Service) PlanChannelUpdate(ctx context.Context, ref string, rev model.Revision, c ChannelChange, op model.OperationID, m Mode) (ChannelPlan, error) {
	if c.Type != "" {
		return ChannelPlan{}, model.NewError(model.Validation, "channel.type: a channel never changes type")
	}
	if c.empty() {
		return ChannelPlan{}, model.NewError(model.Validation, "channel: nothing to change")
	}
	cur, err := s.FindChannel(ctx, ref)
	if err != nil {
		return ChannelPlan{}, err
	}
	cs := &conflicts{m: m}
	if err := checkRevision("channel", Name(cur.Key, cur.Ref.ID), cur.Revision, rev, cs); err != nil {
		return ChannelPlan{}, err
	}
	ch := cur.Clone()
	if err := s.applyChannel(&ch, c); err != nil {
		return ChannelPlan{}, err
	}
	req := model.ConfigureChannel{Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev}, Channel: ch}
	warn, err := s.checkChannel(ctx, req, cs)
	return ChannelPlan{Request: req, Current: &cur, Warnings: warn, deferred: cs.deferred}, err
}

// CheckRevision is checkRevision for another record's plan, kind and
// name naming it: a preview refuses a stale revision, a submission
// leaves it to uma and returns it as deferred, for Explain.
func CheckRevision(kind, name string, current, expected model.Revision, m Mode) (deferred, err error) {
	cs := &conflicts{m: m}
	err = checkRevision(kind, name, current, expected, cs)
	return cs.deferred, err
}

// Explain is uma's refusal of a submission as the conflict a plan
// deferred to uma describes it (see CheckRevision).
func Explain(err, deferred error) error { return explain(err, deferred) }

// checkRevision refuses a write at a revision the record is not at.
// uma compares the revision again when it commits.
func checkRevision(kind, name string, current, expected model.Revision, cs *conflicts) error {
	if expected == "" {
		return model.NewError(model.Validation, kind+": the revision being replaced is required")
	}
	if current != expected {
		return cs.uma(model.NewError(model.Conflict, fmt.Sprintf(
			"%s %s is at revision %s, not %s: it changed since it was read", kind, name, current, expected)))
	}
	return nil
}

// applyChannel sets c's fields on ch. The configuration takes the
// adapter's schema name and version.
func (s Service) applyChannel(ch *model.Channel, c ChannelChange) error {
	if c.Key != nil {
		ch.Key = *c.Key
	}
	if c.CredentialRef != nil {
		ch.CredentialRef = *c.CredentialRef
	}
	if c.Configuration != nil {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(c.Configuration, &obj); err != nil || obj == nil {
			return model.NewError(model.Validation, "channel.configuration: want a JSON object")
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, c.Configuration); err != nil {
			return model.NewError(model.Validation, "channel.configuration: want a JSON object")
		}
		ch.Configuration.Data = buf.Bytes()
	}
	if ch.Configuration.Data == nil {
		ch.Configuration.Data = json.RawMessage("{}")
	}
	if a, ok := s.Adapters.Lookup(ch.Type); ok {
		schema := a.Factory.Describe().ConfigurationSchema
		ch.Configuration.Name, ch.Configuration.Version = schema.Name, schema.Version
	}
	return nil
}

// checkChannel makes the checks submitting req makes, in uma and in
// heo: the request's shape, the adapter's validation of the
// configuration, the key free or the channel's own, and the provider
// account no other channel of the profile receives. A key conflict is
// uma's to decide on submission (see Mode); the provider account is
// heo's alone. It returns advisories that do not stop the write.
func (s Service) checkChannel(ctx context.Context, req model.ConfigureChannel, cs *conflicts) ([]string, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	ch := req.Channel
	if err := issuesError(s.Engine.ValidateChannel(ch.Type, ch.Configuration)); err != nil {
		return nil, err
	}
	if ch.Key != "" {
		held, err := s.Client.Configuration().ChannelByKey(ctx, model.LookupKey{Call: call(), Key: ch.Key})
		switch {
		case model.IsCode(err, model.NotFound):
		case err != nil:
			return nil, err
		case held.Ref != ch.Ref:
			if err := cs.uma(model.NewError(model.Conflict, "channel.key: already names channel "+string(held.Ref.ID))); err != nil {
				return nil, err
			}
		}
	}
	a, ok := s.Adapters.Lookup(ch.Type)
	if !ok || !adapters.Receives(a.Factory.Describe()) {
		return nil, nil
	}
	probe := ch
	if probe.Ref.ID == "" {
		// A new channel has no ID yet; an adapter keying its account on
		// the channel itself can collide with nothing.
		probe.Ref.ID = "new"
	}
	account, err := a.AccountOf(probe)
	if err != nil {
		return []string{"receives nothing: " + err.Error()}, nil
	}
	others, err := s.allChannels(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range others {
		if o.Ref == ch.Ref {
			continue
		}
		oa, ok := s.Adapters.Lookup(o.Type)
		if !ok || !adapters.Receives(oa.Factory.Describe()) {
			continue
		}
		if acct, err := oa.AccountOf(o); err == nil && acct == account {
			if cs.m == Submit && ch.Ref.ID == "" && sameChannel(o, ch) {
				// The channel a first attempt of this creation wrote:
				// uma replays it under the same operation key, and its
				// key, held, refuses any other creation.
				continue
			}
			return nil, model.NewError(model.Conflict, fmt.Sprintf(
				"channel.configuration: channel %s already receives this provider account; one account has one receiver",
				Name(o.Key, o.Ref.ID)))
		}
	}
	return nil, nil
}

// sameChannel reports whether stored is what creating want would
// write, under the same non-empty key.
func sameChannel(stored, want model.Channel) bool {
	return want.Key != "" && stored.Key == want.Key && stored.Type == want.Type &&
		stored.CredentialRef == want.CredentialRef && bytes.Equal(stored.Configuration.Data, want.Configuration.Data)
}

// issuesError returns the first adapter validation issue as an error,
// as uma reports it: an unregistered type or schema unsupported,
// anything else invalid.
func issuesError(issues []model.ValidationIssue) error {
	if len(issues) == 0 {
		return nil
	}
	i := issues[0]
	code := model.Validation
	switch model.ErrorCode(i.Code) {
	case model.Unsupported, model.SchemaMismatch:
		code = model.ErrorCode(i.Code)
	}
	msg := "channel." + i.Field
	if i.Message != "" {
		msg += ": " + i.Message
	}
	return model.NewError(code, msg)
}

// ConfigureChannel submits a planned request and returns the channel as
// stored after it, with uma's acceptance. A retry under the same
// operation ID returns the stored operation (Replayed).
func (s Service) ConfigureChannel(ctx context.Context, p ChannelPlan) (model.Channel, model.Acceptance, error) {
	req := p.Request
	acc, err := s.Client.Configuration().ConfigureChannel(ctx, req)
	s.committed(ctx)
	if err != nil {
		return model.Channel{}, acc, explain(err, p.deferred)
	}
	id := req.Channel.Ref.ID
	if r := acc.Operation.Result; r != nil {
		id = r.ID
	}
	ch, err := s.channelByID(ctx, id)
	return ch, acc, err
}

// InboxChange is what an inbox create, update, enable or disable sets.
// A nil field is left as it is on update and empty on create.
type InboxChange struct {
	// Channel is the channel's key or ID; create only, an inbox never
	// moves to another channel.
	Channel      string
	Key          *string
	Name         *string
	Destinations *[]string
	// Mode is the conversation selection mode. Leaving per-contact mode
	// clears LockToSingle and LifecycleRules unless they are set too:
	// they hold only in per-contact mode.
	Mode           *string
	Policy         *string
	LockToSingle   *bool
	LifecycleRules *string
	DeliverNotes   *bool
	Disabled       *bool
}

func (c InboxChange) empty() bool {
	return c.Key == nil && c.Name == nil && c.Destinations == nil && c.Mode == nil && c.Policy == nil &&
		c.LockToSingle == nil && c.LifecycleRules == nil && c.DeliverNotes == nil && c.Disabled == nil
}

// InboxPlan is a validated inbox configure request, the record it would
// replace (nil for a creation) and advisories.
type InboxPlan struct {
	Request  model.ConfigureInbox
	Current  *model.Inbox
	Warnings []string
	// deferred is the first conflict left to uma on submission.
	deferred error
}

// PlanInboxCreate builds and checks the request creating an inbox under
// operation op.
func (s Service) PlanInboxCreate(ctx context.Context, c InboxChange, op model.OperationID, m Mode) (InboxPlan, error) {
	ch, err := s.FindChannel(ctx, c.Channel)
	if err != nil {
		return InboxPlan{}, err
	}
	in := model.Inbox{Ref: model.InboxRef{Tenant: s.Tenant}, Channel: ch.Ref}
	applyInbox(&in, c)
	req := model.ConfigureInbox{Command: model.Command{Call: call(), Operation: op}, Inbox: in}
	cs := &conflicts{m: m}
	warn, err := s.checkInbox(ctx, req, ch, cs)
	return InboxPlan{Request: req, Warnings: warn, deferred: cs.deferred}, err
}

// PlanInboxUpdate builds and checks the request replacing the inbox ref
// at revision rev under operation op.
func (s Service) PlanInboxUpdate(ctx context.Context, ref string, rev model.Revision, c InboxChange, op model.OperationID, m Mode) (InboxPlan, error) {
	if c.Channel != "" {
		return InboxPlan{}, model.NewError(model.Validation, "inbox.channel: an inbox never moves to another channel")
	}
	if c.empty() {
		return InboxPlan{}, model.NewError(model.Validation, "inbox: nothing to change")
	}
	cur, err := s.FindInbox(ctx, ref)
	if err != nil {
		return InboxPlan{}, err
	}
	cs := &conflicts{m: m}
	if err := checkRevision("inbox", Name(cur.Key, cur.Ref.ID), cur.Revision, rev, cs); err != nil {
		return InboxPlan{}, err
	}
	ch, err := s.channelByID(ctx, cur.Channel.ID)
	if err != nil {
		return InboxPlan{}, err
	}
	in := cur.Clone()
	applyInbox(&in, c)
	req := model.ConfigureInbox{Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev}, Inbox: in}
	warn, err := s.checkInbox(ctx, req, ch, cs)
	return InboxPlan{Request: req, Current: &cur, Warnings: warn, deferred: cs.deferred}, err
}

func applyInbox(in *model.Inbox, c InboxChange) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&in.Key, c.Key)
	set(&in.Name, c.Name)
	if c.Destinations != nil {
		in.Destinations = slices.Clone(*c.Destinations)
	}
	if c.Mode != nil && *c.Mode != in.Selection.Mode {
		in.Selection.Mode = *c.Mode
		if *c.Mode != model.ModePerContact {
			in.Selection.LockToSingle, in.Selection.LifecycleRules = nil, ""
		}
	}
	set(&in.Selection.Policy, c.Policy)
	set(&in.Selection.LifecycleRules, c.LifecycleRules)
	if c.LockToSingle != nil {
		v := *c.LockToSingle
		in.Selection.LockToSingle = &v
	}
	if c.DeliverNotes != nil {
		in.DeliverNotes = *c.DeliverNotes
	}
	if c.Disabled != nil {
		in.Disabled = *c.Disabled
	}
}

// checkInbox makes the checks submitting req makes: the request's shape
// and selection configuration, destinations distinct and bound to no
// other inbox of the channel, note delivery only where the adapter
// declares it, and the key free or the inbox's own; key and destination
// conflicts are uma's to decide on submission (see Mode). uma alone
// checks a named selection policy is registered; that is an advisory
// here.
func (s Service) checkInbox(ctx context.Context, req model.ConfigureInbox, ch model.Channel, cs *conflicts) ([]string, error) {
	in := req.Inbox
	if err := firstErr(req.Validate(), in.Selection.Validate(), checkDestinations(in.Destinations)); err != nil {
		return nil, err
	}
	if in.Selection.Mode == model.ModePerAgentContext {
		return nil, model.NewError(model.Unsupported, "selection.mode: per-agent-context selection is not supported")
	}
	if in.DeliverNotes {
		a, ok := s.Adapters.Lookup(ch.Type)
		if !ok || !adapters.Declares(a.Factory.Describe(), model.CapabilityInternalNote) {
			return nil, model.NewError(model.Unsupported, "inbox.deliver_notes: channel adapter does not declare internal_note")
		}
	}
	if in.Key != "" {
		held, err := s.Client.Configuration().InboxByKey(ctx, model.LookupKey{Call: call(), Key: in.Key})
		switch {
		case model.IsCode(err, model.NotFound):
		case err != nil:
			return nil, err
		case held.Ref != in.Ref:
			if err := cs.uma(model.NewError(model.Conflict, "inbox.key: already names inbox "+string(held.Ref.ID))); err != nil {
				return nil, err
			}
		}
	}
	all, err := s.allInboxes(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range all {
		if o.Ref == in.Ref || o.Channel != in.Channel {
			continue
		}
		for _, d := range in.Destinations {
			if slices.Contains(o.Destinations, d) {
				if err := cs.uma(model.NewError(model.Conflict, fmt.Sprintf(
					"inbox.destinations: destination %q already bound to inbox %s", d, Name(o.Key, o.Ref.ID)))); err != nil {
					return nil, err
				}
			}
		}
	}
	var warn []string
	if in.Selection.Policy != "" {
		warn = append(warn, "selection.policy: whether a selector is registered under this name is checked at submission")
	}
	if len(in.Destinations) == 0 {
		warn = append(warn, "inbox.destinations: none, so the inbox receives nothing")
	}
	return warn, nil
}

func checkDestinations(ds []string) error {
	for i, d := range ds {
		if d == "" {
			return model.NewError(model.Validation, "inbox.destinations: must not be empty")
		}
		if slices.Contains(ds[:i], d) {
			return model.NewError(model.Validation, "inbox.destinations: must be distinct")
		}
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// ConfigureInbox submits a planned request and returns the inbox as
// stored after it, with uma's acceptance.
func (s Service) ConfigureInbox(ctx context.Context, p InboxPlan) (model.Inbox, model.Acceptance, error) {
	req := p.Request
	acc, err := s.Client.Configuration().ConfigureInbox(ctx, req)
	s.committed(ctx)
	if err != nil {
		return model.Inbox{}, acc, explain(err, p.deferred)
	}
	id := req.Inbox.Ref.ID
	if r := acc.Operation.Result; r != nil {
		id = r.ID
	}
	in, err := s.FindInbox(ctx, string(id))
	return in, acc, err
}
