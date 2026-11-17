package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/channels"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Work items. An issue or a ticket is a thread, which is a conversation:
// under a per_work_item inbox the conversation is the work item, bound by
// ingress, and its comments are the conversation's messages. heo has no
// verb of its own for a tracker: it reads the work item beside its
// conversation, and changes it through the conversation verbs.
//
// A work item's status is normalized (open, pending, resolved, closed)
// and the provider's own is kept beside it. The status is the provider's
// unless the channel's configuration makes it uma's (local_fields). The
// work item's lifecycle is independent of the conversation's: neither
// moves the other.

// WorkItemStatuses lists uma's work item statuses, each the declared
// mapping of a provider status.
func WorkItemStatuses() []string {
	return []string{model.WorkItemOpen, model.WorkItemPending, model.WorkItemResolved, model.WorkItemClosed}
}

// WorkItemOf returns the work item bound to the conversation ref, or nil
// when it has none or the caller may not read it. Only a conversation of
// a per_work_item inbox has one.
func (s Service) WorkItemOf(ctx context.Context, ref model.ConversationRef) (*model.WorkItem, error) {
	p, err := s.Client.WorkItems().List(ctx, model.ListRequest{Call: call(), Filter: model.Filter{Conversation: &ref}, Page: model.PageRequest{Limit: 1}})
	if err != nil || len(p.Items) == 0 {
		return nil, err
	}
	return &p.Items[0], nil
}

// FindWorkItem returns the work item with record ID id among those the
// caller may read.
func (s Service) FindWorkItem(ctx context.Context, id string) (model.WorkItem, error) {
	if err := checkID("work item", id); err != nil {
		return model.WorkItem{}, err
	}
	req := model.ListRequest{Call: call(), Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.WorkItems().List(ctx, req)
		if err != nil {
			return model.WorkItem{}, err
		}
		for _, w := range p.Items {
			if string(w.Ref.ID) == id {
				return w, nil
			}
		}
		if p.Complete || p.Next == "" {
			return model.WorkItem{}, model.NewError(model.NotFound, fmt.Sprintf("work item %s: not found", id))
		}
		req.Page.After = p.Next
	}
}

// WorkItemFilter narrows a conversation listing by the work item bound to
// each conversation.
type WorkItemFilter struct {
	// Only keeps the conversations that have a work item (true) or none
	// (false); nil keeps both.
	Only *bool
	// Statuses keep the conversations whose work item is in one of them
	// (uma's work item statuses); a conversation without a work item is
	// never kept.
	Statuses []string
}

// Asked reports whether the filter narrows anything.
func (f WorkItemFilter) Asked() bool { return f.Only != nil || len(f.Statuses) > 0 }

func (f WorkItemFilter) keeps(w *model.WorkItem) bool {
	if f.Only != nil && (w != nil) != *f.Only {
		return false
	}
	if len(f.Statuses) == 0 {
		return true
	}
	return w != nil && slices.Contains(f.Statuses, w.Status)
}

// ConversationItem is a conversation and the work item bound to it, nil
// for one that has none.
type ConversationItem struct {
	model.Conversation
	WorkItem *model.WorkItem `json:"work_item,omitempty" yaml:"work_item,omitempty"`
	// LastActivity is when the conversation last changed for the caller:
	// see Service.LastActivity.
	LastActivity time.Time `json:"last_activity,omitzero" yaml:"last_activity,omitempty"`
}

// ListConversationItems returns one page of the conversations f and wi
// select, each with its work item. uma filters conversations by f and
// has no filter by work item, so heo reads the work item of each
// conversation of a per_work_item inbox (the only ones that have one) and
// scans until the page holds page.Limit or the listing ends; the next
// page starts where the scan stopped.
func (s Service) ListConversationItems(ctx context.Context, f Filter, wi WorkItemFilter, page model.PageRequest) (model.Page[ConversationItem], error) {
	for _, st := range wi.Statuses {
		if !slices.Contains(WorkItemStatuses(), st) {
			return model.Page[ConversationItem]{}, model.NewError(model.Validation, fmt.Sprintf(
				"work item status %q: want one of %s", st, strings.Join(WorkItemStatuses(), ", ")))
		}
	}
	if f.Sort != "" && f.Sort != SortLastActivity {
		return model.Page[ConversationItem]{}, model.NewError(model.Validation, fmt.Sprintf(
			"sort %q: want %s", f.Sort, SortLastActivity))
	}
	if f.Sort == SortLastActivity {
		return s.listByActivity(ctx, f, wi, page)
	}
	out := model.Page[ConversationItem]{Items: []ConversationItem{}}
	next := page
	for {
		// Asking for no more than the items still wanted keeps the page
		// within its limit: none is passed over.
		next.Limit = page.Limit - len(out.Items)
		p, err := s.ListConversations(ctx, f, next)
		if err != nil {
			return model.Page[ConversationItem]{}, err
		}
		for _, c := range p.Items {
			it, ok, err := s.item(ctx, c, f, wi)
			if err != nil {
				return model.Page[ConversationItem]{}, err
			}
			if ok {
				out.Items = append(out.Items, it)
			}
		}
		out.Next, out.Complete = p.Next, p.Complete
		if p.Complete || p.Next == "" || len(out.Items) >= page.Limit {
			return out, nil
		}
		next.After = p.Next
	}
}

// item is c with its work item and last activity, and whether the work
// item filter and --updated-since keep it.
func (s Service) item(ctx context.Context, c model.Conversation, f Filter, wi WorkItemFilter) (ConversationItem, bool, error) {
	var w *model.WorkItem
	if c.Mode == model.ModePerWorkItem {
		var err error
		if w, err = s.WorkItemOf(ctx, c.Ref); err != nil {
			return ConversationItem{}, false, err
		}
	}
	if !wi.keeps(w) {
		return ConversationItem{}, false, nil
	}
	at, err := s.LastActivity(ctx, c)
	if err != nil {
		return ConversationItem{}, false, err
	}
	if f.UpdatedSince != nil && !at.After(*f.UpdatedSince) {
		return ConversationItem{}, false, nil
	}
	return ConversationItem{Conversation: c, WorkItem: w, LastActivity: at}, true, nil
}

// workItemOf returns the work item of conversation id and the
// conversation: model.Unsupported when it has none, pointing at the
// conversation's own lifecycle verbs; a conflict when the provider no
// longer binds it.
func (s Service) workItemOf(ctx context.Context, id string) (model.WorkItem, model.Conversation, error) {
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return model.WorkItem{}, model.Conversation{}, err
	}
	w, err := s.WorkItemOf(ctx, conv.Ref)
	if err != nil {
		return model.WorkItem{}, model.Conversation{}, err
	}
	if w == nil {
		return model.WorkItem{}, model.Conversation{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"conversation %s has no work item: only a conversation of a per_work_item inbox is an issue or a ticket; "+
				"its own status is changed with `conversation resolve` and `conversation reopen`", id))
	}
	if w.Binding != "" && w.Binding != model.WorkItemBindingActive {
		return model.WorkItem{}, model.Conversation{}, model.NewError(model.Conflict, fmt.Sprintf(
			"conversation %s: its work item is %s at the provider: it can no longer be changed", id, w.Binding))
	}
	return *w, conv, nil
}

// CheckStatus checks the status and the reason asked for: the usage
// rules, which need no record. Which reasons a transition takes is the
// channel adapter's declaration, checked by a preview and by uma.
func CheckStatus(status, reason string) error {
	if status == "" {
		return model.NewError(model.Validation, "status: required: "+strings.Join(WorkItemStatuses(), ", "))
	}
	if !slices.Contains(WorkItemStatuses(), status) {
		return model.NewError(model.Validation, fmt.Sprintf("status %q: want one of %s", status, strings.Join(WorkItemStatuses(), ", ")))
	}
	if reason != "" {
		if err := model.ValidateStatusReason(reason); err != nil {
			return model.NewError(model.Validation, fmt.Sprintf("reason %q: want one lowercase snake_case word, such as completed or not_planned", reason))
		}
	}
	return nil
}

// StatusPlan is a validated work item status change: uma's work item act,
// the work item and its conversation as they are now, and, from a
// preview, what was checked of the adapter.
type StatusPlan struct {
	Request      model.WorkItemAction
	Item         model.WorkItem
	Conversation model.Conversation
	// Status and Reason are what was asked: the status, and the reason
	// the provider records for it, empty for the provider's default.
	Status string
	Reason string
	// Local reports that uma owns the work item's status on its channel
	// (local_fields): the act commits in uma and reaches no provider.
	Local bool
	// Declaration is set by a preview only, and never for a local status.
	Declaration *Declaration
	deferred    error
}

// PlanStatus builds and checks setting the status of the work item of
// conversation id to status, with reason, under operation op. It writes
// nothing. The act always carries a revision: rev, or the work item's own
// when none is given.
//
// A work item already in status is refused in a preview, and by uma on
// submission: no adapter declares a transition from a status to itself.
// A work item whose conversation has none is refused (model.Unsupported),
// as is one the provider no longer binds (model.Conflict). A reason with a
// status uma owns is a validation error.
//
// A preview also refuses what uma would refuse at submission before any
// plan: an adapter that does not declare work_item_act, the transition or
// the reason (model.Unsupported), and a stale revision (model.Conflict).
func (s Service) PlanStatus(ctx context.Context, id, status, reason string, rev model.Revision, op model.OperationID, mode channels.Mode) (StatusPlan, error) {
	if err := CheckStatus(status, reason); err != nil {
		return StatusPlan{}, err
	}
	w, conv, err := s.workItemOf(ctx, id)
	if err != nil {
		return StatusPlan{}, err
	}
	local := slices.Contains(w.LocalFields, model.WorkItemFieldStatus)
	if local && reason != "" {
		return StatusPlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"reason %q: the status of conversation %s's work item is uma's own on this channel, and a local status takes no provider reason", reason, id))
	}
	// An item already in status is refused in a preview. A submission
	// leaves it to uma: a retry under the key of a change the provider
	// has since reported is a replay, not a second change, and only uma
	// knows whether the key is the one that made it.
	if !local && w.Status == status && mode == channels.Preview {
		return StatusPlan{}, errAlreadyStatus(id, status)
	}
	p := StatusPlan{Item: w, Conversation: conv, Status: status, Reason: reason, Local: local}
	if mode == channels.Preview && !local {
		in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
		if err != nil {
			return StatusPlan{}, err
		}
		d, desc, err := s.declared(ctx, in, "conversation status", model.CapabilityWorkItemAct)
		if err == nil {
			if aerr := adapters.ActDeclared(desc, w.Status, status, reason); aerr != nil {
				err = model.NewError(model.Unsupported, fmt.Sprintf(
					"inbox %s: %s; `conversation status` needs it; `heo channel get %s` lists the verbs the channel supports",
					channels.Name(in.Key, in.Ref.ID), strings.TrimPrefix(aerr.Error(), "unsupported: "), in.Channel.ID))
			}
		}
		if err != nil {
			return StatusPlan{}, err
		}
		p.Declaration = &d
	}
	if rev == "" {
		rev = w.Revision
	}
	if p.deferred, err = channels.CheckRevision("work item", string(w.Ref.ID), w.Revision, rev, mode); err != nil {
		return StatusPlan{}, err
	}
	update := model.WorkItemUpdate{Status: &status}
	if reason != "" {
		update.StatusReason = &reason
	}
	if p.Request, err = actOf(w, update, op, rev); err != nil {
		return StatusPlan{}, err
	}
	return p, nil
}

// actOf is the work item act applying update to w at revision rev.
func actOf(w model.WorkItem, update model.WorkItemUpdate, op model.OperationID, rev model.Revision) (model.WorkItemAction, error) {
	data, err := json.Marshal(update)
	if err != nil {
		return model.WorkItemAction{}, err
	}
	req := model.WorkItemAction{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev}, Target: w.Ref,
		Action: model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: data},
	}
	return req, req.Validate()
}

func errAlreadyStatus(id, status string) error {
	return model.NewError(model.Unsupported, fmt.Sprintf(
		"conversation %s: its work item is already %s: a status moves to another one", id, status))
}

// SetStatus submits p: uma queues the act for a status the provider owns
// and commits it for one uma owns. It returns the work item as uma holds
// it now: a provider's status changes only once the provider applied the
// act and reports it.
func (s Service) SetStatus(ctx context.Context, p StatusPlan) (model.WorkItem, model.Acceptance, error) {
	acc, err := s.Client.WorkItems().Act(ctx, p.Request)
	if err != nil {
		if p.Item.Status == p.Status && model.IsCode(err, model.Unsupported) {
			err = errAlreadyStatus(string(p.Conversation.Ref.ID), p.Status)
		}
		return model.WorkItem{}, acc, channels.Explain(err, p.deferred)
	}
	w, err := s.Client.WorkItems().Get(ctx, model.Lookup[model.WorkItemRef]{Call: call(), Ref: p.Item.Ref})
	return w, acc, err
}

// Field is one field of a work item update: its name and its value, JSON.
type Field struct {
	Key   string
	Value json.RawMessage
}

// ParseFields reads key=value arguments. A value that is JSON (a number,
// true, null, an array, an object, a quoted string) is taken as it is,
// anything else is a string, so `milestone=3` is the number 3 and
// `status=solved` the string "solved". A key given twice, or none, is a
// validation error.
func ParseFields(args []string) ([]Field, error) {
	out := make([]Field, 0, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, model.NewError(model.Validation, fmt.Sprintf("field %q: want key=value", a))
		}
		if slices.ContainsFunc(out, func(f Field) bool { return f.Key == k }) {
			return nil, model.NewError(model.Validation, fmt.Sprintf("field %q: given twice", k))
		}
		raw := json.RawMessage(strings.TrimSpace(v))
		if !json.Valid(raw) {
			raw, _ = json.Marshal(v)
		} else {
			var c bytes.Buffer
			if err := json.Compact(&c, raw); err == nil {
				raw = c.Bytes()
			}
		}
		out = append(out, Field{Key: k, Value: raw})
	}
	if len(out) == 0 {
		return nil, model.NewError(model.Validation, "fields: name at least one --field key=value")
	}
	return out, nil
}

// UpdatePlan is a validated work item update. A field uma owns on the
// channel (local_fields) commits in uma as a work item act; every other
// field is the provider's, and goes to the provider through the update
// effect the channel's adapter ships. Exactly one of Act and Effect is
// set.
type UpdatePlan struct {
	Fields       []Field
	Item         model.WorkItem
	Conversation model.Conversation
	Act          *model.WorkItemAction
	Effect       *model.ExecuteExtension
	// Declaration is set by a preview of a provider update only.
	Declaration *EffectDeclaration
	deferred    error
}

// Local reports that the update commits in uma.
func (p UpdatePlan) Local() bool { return p.Act != nil }

// localField is the work item field of uma that holds key when uma owns
// it, or "" when the provider does: subject and assignees are fields of
// their own, any other key is an entry of fields, and the status has its
// own verb.
func localField(w model.WorkItem, key string) string {
	f := model.WorkItemFieldFields
	switch key {
	case "subject":
		f = model.WorkItemFieldSubject
	case "assignees":
		f = model.WorkItemFieldAssignees
	case "status":
		f = model.WorkItemFieldStatus
	}
	if slices.Contains(w.LocalFields, f) {
		return f
	}
	return ""
}

// UpdateEffect returns the extension effect of the channel's adapter that
// updates a work item's provider-owned fields: the one it ships authorized
// as a work item act. The adapter declares it, so no provider is named
// here. None is model.Unsupported (the fields are ingest-only); more than
// one is ambiguous, and refused.
func (s Service) UpdateEffect(ctx context.Context, in model.Inbox, verb string) (EffectDeclaration, adapter.ExtensionDefinition, error) {
	d, desc, err := s.declared(ctx, in, verb, model.CapabilityExtensionExecute)
	if err != nil {
		if desc.Type != "" && model.IsCode(err, model.Unsupported) {
			// The adapter is known and declares no effects at all.
			return EffectDeclaration{}, adapter.ExtensionDefinition{}, errIngestOnly(in, desc.Type)
		}
		return EffectDeclaration{}, adapter.ExtensionDefinition{}, err
	}
	if s.Effects == nil {
		return EffectDeclaration{}, adapter.ExtensionDefinition{}, model.NewError(model.Unsupported, fmt.Sprintf(
			"inbox %s: its channel's adapter effects are unknown here", channels.Name(in.Key, in.Ref.ID)))
	}
	defs, err := s.Effects(ctx, in.Channel)
	if err != nil {
		return EffectDeclaration{}, adapter.ExtensionDefinition{}, err
	}
	found := adapters.UpdateEffects(defs)
	switch len(found) {
	case 1:
		return EffectDeclaration{Declaration: d, Effect: found[0].Name}, found[0], nil
	case 0:
		return EffectDeclaration{}, adapter.ExtensionDefinition{}, errIngestOnly(in, d.Adapter)
	}
	names := make([]string, len(found))
	for i, f := range found {
		names[i] = f.Name
	}
	return EffectDeclaration{}, adapter.ExtensionDefinition{}, model.NewError(model.Unsupported, fmt.Sprintf(
		"inbox %s: adapter %s ships more than one work item update effect (%s): heo cannot choose",
		channels.Name(in.Key, in.Ref.ID), d.Adapter, strings.Join(names, ", ")))
}

func errIngestOnly(in model.Inbox, typ model.TypeKey) error {
	return model.NewError(model.Unsupported, fmt.Sprintf(
		"inbox %s: adapter %s ships no work item update effect, so the provider's own fields are ingest-only here; "+
			"`heo channel get %s` lists the verbs the channel supports",
		channels.Name(in.Key, in.Ref.ID), typ, in.Channel.ID))
}

// PlanUpdate builds and checks changing fields of the work item of
// conversation id, under operation op. It writes nothing.
//
// Each field is uma's or the provider's, as the work item says
// (local_fields, set by the channel's configuration). An update of uma's
// fields commits through uma's work item act, at revision rev (the work
// item's own when none is given); an update of the provider's goes
// through the update effect the channel's adapter declares
// (UpdateEffect), and takes no revision. One update is of one kind: a
// mix is a validation error. A provider's field on a channel whose
// adapter ships no update effect is model.Unsupported (ingest-only).
//
// A preview of a provider update also asks the adapter, offline, to check
// the fields (model.Validation); a submission leaves that to uma.
func (s Service) PlanUpdate(ctx context.Context, id string, fields []Field, rev model.Revision, op model.OperationID, mode channels.Mode) (UpdatePlan, error) {
	if len(fields) == 0 {
		return UpdatePlan{}, model.NewError(model.Validation, "fields: name at least one --field key=value")
	}
	w, conv, err := s.workItemOf(ctx, id)
	if err != nil {
		return UpdatePlan{}, err
	}
	var local, provider []string
	for _, f := range fields {
		switch lf := localField(w, f.Key); {
		case lf == model.WorkItemFieldStatus:
			return UpdatePlan{}, model.NewError(model.Validation, "field status: the status is changed with `conversation status`")
		case lf != "":
			local = append(local, f.Key)
		default:
			provider = append(provider, f.Key)
		}
	}
	if len(local) > 0 && len(provider) > 0 {
		return UpdatePlan{}, model.NewError(model.Validation, fmt.Sprintf(
			"fields %s are uma's own on this channel and %s are the provider's: change them in two updates",
			strings.Join(local, ", "), strings.Join(provider, ", ")))
	}
	p := UpdatePlan{Fields: fields, Item: w, Conversation: conv}
	if len(local) > 0 {
		if rev == "" {
			rev = w.Revision
		}
		if p.deferred, err = channels.CheckRevision("work item", string(w.Ref.ID), w.Revision, rev, mode); err != nil {
			return UpdatePlan{}, err
		}
		u, err := localUpdate(w, fields)
		if err != nil {
			return UpdatePlan{}, err
		}
		act, err := actOf(w, u, op, rev)
		if err != nil {
			return UpdatePlan{}, err
		}
		p.Act = &act
		return p, nil
	}
	if rev != "" {
		return UpdatePlan{}, model.NewError(model.Validation, "revision: it applies to the fields uma owns; the provider's are not versioned by heo")
	}
	if conv.ReplyRoute != w.External {
		return UpdatePlan{}, model.NewError(model.Conflict, fmt.Sprintf(
			"conversation %s: it does not route to its work item", id))
	}
	in, err := s.FindInbox(ctx, string(conv.Ref.Inbox.ID))
	if err != nil {
		return UpdatePlan{}, err
	}
	d, def, err := s.UpdateEffect(ctx, in, "conversation update")
	if err != nil {
		return UpdatePlan{}, err
	}
	data := map[string]json.RawMessage{}
	for _, f := range fields {
		data[f.Key] = f.Value
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return UpdatePlan{}, err
	}
	req := model.ExecuteExtension{
		Command: model.Command{Call: call(), Operation: op}, Target: conv.Ref.Ref(), Audience: effectAudience,
		Action: model.Extension{Name: def.Name, Version: def.Version, Data: raw},
	}
	if err := req.Validate(); err != nil {
		return UpdatePlan{}, err
	}
	p.Effect = &req
	if mode == channels.Preview {
		p.Declaration = &d
		if err := s.validateAction(ctx, in.Channel, req.Audience, req.Action); err != nil {
			return UpdatePlan{}, err
		}
	}
	return p, nil
}

// localUpdate is the update of uma's own fields: subject and assignees
// are fields of their own, every other key an entry of the work item's
// fields, named under the namespace of the work item's adapter (a key
// with a dot is already a full name), replacing the entry of that name
// and keeping the others.
func localUpdate(w model.WorkItem, fields []Field) (model.WorkItemUpdate, error) {
	var u model.WorkItemUpdate
	var entries []model.Extension
	for _, f := range fields {
		switch f.Key {
		case "subject":
			var v string
			if json.Unmarshal(f.Value, &v) != nil {
				v = string(f.Value)
			}
			u.Subject = &v
		case "assignees":
			ids, err := assigneeIDs(f.Value)
			if err != nil {
				return u, err
			}
			refs := make([]model.ExternalRef, len(ids))
			for i, id := range ids {
				refs[i] = model.ExternalRef{Channel: w.External.Channel, Namespace: w.External.Namespace, Kind: userKind(w), ID: id}
			}
			u.Assignees = &refs
		default:
			// uma names a field of a work item by its adapter's namespace
			// and a key: labels on a work item of namespace x.y is x.y.labels.
			name := f.Key
			if !strings.Contains(name, ".") {
				name = w.External.Namespace + "." + name
			}
			entries = append(entries, model.Extension{Name: name, Version: 1, Data: f.Value})
		}
	}
	if len(entries) > 0 {
		merged := slices.DeleteFunc(slices.Clone(w.Fields), func(x model.Extension) bool {
			return slices.ContainsFunc(entries, func(e model.Extension) bool { return e.Name == x.Name })
		})
		merged = append(merged, entries...)
		u.Fields = &merged
	}
	return u, nil
}

// assigneeIDs reads the provider IDs of an assignees value: a JSON array
// of strings, or one string, comma separated.
func assigneeIDs(raw json.RawMessage) ([]string, error) {
	var ids []string
	if json.Unmarshal(raw, &ids) == nil {
		return ids, nil
	}
	var one string
	if json.Unmarshal(raw, &one) != nil {
		return nil, model.NewError(model.Validation, "field assignees: want provider IDs, a JSON array or comma separated")
	}
	ids = nil
	for _, id := range strings.Split(one, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// userKind is the reference kind of a person on the work item's adapter,
// as its requester or an assignee already names one.
func userKind(w model.WorkItem) string {
	for _, r := range append([]model.ExternalRef{w.Requester}, w.Assignees...) {
		if r.Kind != "" {
			return r.Kind
		}
	}
	return "user"
}

// Update submits p: a local update commits in uma at once; a provider's
// is queued, and the daemon's workers send it. It returns the work item
// as uma holds it now, which a provider's update changes when the provider
// reports it.
func (s Service) Update(ctx context.Context, p UpdatePlan) (model.WorkItem, model.Acceptance, error) {
	var acc model.Acceptance
	var err error
	if p.Act != nil {
		acc, err = s.Client.WorkItems().Act(ctx, *p.Act)
	} else {
		acc, err = s.Client.Extensions().Execute(ctx, *p.Effect)
	}
	if err != nil {
		return model.WorkItem{}, acc, channels.Explain(err, p.deferred)
	}
	w, err := s.Client.WorkItems().Get(ctx, model.Lookup[model.WorkItemRef]{Call: call(), Ref: p.Item.Ref})
	return w, acc, err
}

// HoldStatus offers the status change p to the hold point as the verb
// status of class status: nil when nothing is held. Only a status the
// provider owns can be held: uma has no hold for a write of its own
// work item status, so a plugin actor's change of one is refused while
// holding is on (model.Unsupported).
func (s Service) HoldStatus(ctx context.Context, p *StatusPlan, preview any) (*Pending, error) {
	if p.Local && s.Holding() {
		return nil, errCannotHold()
	}
	return s.Hold(ctx, &p.Request.Command, HoldInput{
		Action: model.ActionWorkItemAct, Verb: "status", Class: p.Status,
		Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Item.Ref.Ref(), Preview: preview})
}

// HoldUpdate offers the update p to the hold point as the verb fields,
// its class the fields asked for, sorted and comma separated: a change of
// other fields is another class, earning its trust of its own. Only an
// update that goes to the provider can be held; one of fields uma owns is
// refused while holding is on, as HoldStatus refuses a local status.
func (s Service) HoldUpdate(ctx context.Context, p *UpdatePlan, preview any) (*Pending, error) {
	var cmd *model.Command
	var target model.Ref
	if p.Local() {
		if s.Holding() {
			return nil, errCannotHold()
		}
		cmd, target = &p.Act.Command, p.Act.Target.Ref()
	} else {
		cmd, target = &p.Effect.Command, p.Effect.Target
	}
	keys := make([]string, len(p.Fields))
	for i, f := range p.Fields {
		keys[i] = f.Key
	}
	slices.Sort(keys)
	return s.Hold(ctx, cmd, HoldInput{
		Action: model.ActionWorkItemAct, Verb: "fields", Class: strings.Join(keys, ","),
		Channel: string(p.Conversation.Ref.Inbox.ID), Target: target, Preview: preview})
}
