package desk

import (
	"context"
	"fmt"
	"strings"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// Labels are uma's: tenant-wide definitions reusable across every inbox,
// held by conversations as references. heo addresses them by name, as
// uma does under simple case folding (model.LabelNameKey), and adds no
// label state of its own: definitions, assignments and filters are all
// uma's, and so are the events an assignment emits.

// LabelUse is what a label was named for: the fix for an unknown one
// depends on it.
type LabelUse string

// The uses.
const (
	LabelAdd    LabelUse = "add"
	LabelRemove LabelUse = "remove"
	LabelFilter LabelUse = "filter"
)

// UnknownLabelError is a label name no label of the tenant holds. heo
// never creates a label implicitly.
type UnknownLabelError struct {
	Name string
	Use  LabelUse
}

func (e *UnknownLabelError) Error() string { return fmt.Sprintf("label %q: not found", e.Name) }

// Unwrap is the uma not-found the error is.
func (e *UnknownLabelError) Unwrap() error { return model.NewError(model.NotFound, e.Error()) }

// FindLabel returns the label named name under case folding, for use.
func (s Service) FindLabel(ctx context.Context, name string, use LabelUse) (model.Label, error) {
	if strings.TrimSpace(name) == "" {
		return model.Label{}, model.NewError(model.Validation, "label: a name is required")
	}
	l, err := s.Client.Labels().ByName(ctx, model.LookupKey{Call: call(), Key: name})
	if model.IsCode(err, model.NotFound) {
		return model.Label{}, &UnknownLabelError{Name: name, Use: use}
	}
	return l, err
}

// labels resolves names for use, in order, each label once.
func (s Service) labels(ctx context.Context, names []string, use LabelUse) ([]model.Label, error) {
	out := make([]model.Label, 0, len(names))
	seen := map[model.LabelRef]bool{}
	for _, n := range names {
		l, err := s.FindLabel(ctx, n, use)
		if err != nil {
			return nil, err
		}
		if !seen[l.Ref] {
			seen[l.Ref] = true
			out = append(out, l)
		}
	}
	return out, nil
}

func refs(ls []model.Label) []model.LabelRef {
	out := make([]model.LabelRef, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Ref)
	}
	return out
}

// ListLabels returns one page of the tenant's labels the caller may
// read.
func (s Service) ListLabels(ctx context.Context, page model.PageRequest) (model.Page[model.Label], error) {
	return s.Client.Labels().List(ctx, model.ListRequest{Call: call(), Page: page})
}

// LabelNames returns the names of the labels rs, in order.
func (s Service) LabelNames(ctx context.Context, rs []model.LabelRef) ([]string, error) {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		l, err := s.Client.Labels().Get(ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: r})
		if err != nil {
			return nil, err
		}
		out = append(out, l.Name)
	}
	return out, nil
}

// LabelChange is the label a create defines.
type LabelChange struct {
	Name        string
	Description string
	Color       string
}

// LabelPlan is a validated label creation.
type LabelPlan struct {
	Request model.ConfigureLabel
	// deferred is a taken name left to uma on submission.
	deferred error
}

// PlanLabelCreate builds and checks the creation of the label c under
// operation op: the request's shape, and the name free under case
// folding — final in a preview, uma's on submission (see channels.Mode),
// so a retry under the same operation replays.
func (s Service) PlanLabelCreate(ctx context.Context, c LabelChange, op model.OperationID, m channels.Mode) (LabelPlan, error) {
	req := model.ConfigureLabel{
		Command: model.Command{Call: call(), Operation: op},
		Label:   model.Label{Ref: model.LabelRef{Tenant: s.Tenant}, Name: c.Name, Description: c.Description, Color: c.Color},
	}
	if err := req.Validate(); err != nil {
		return LabelPlan{}, err
	}
	p := LabelPlan{Request: req}
	held, err := s.FindLabel(ctx, c.Name, LabelAdd)
	switch {
	case err == nil:
		taken := model.NewError(model.Conflict, fmt.Sprintf("label %q: the name is taken by label %s (%s)", c.Name, held.Name, held.Ref.ID))
		if m != channels.Submit {
			return LabelPlan{}, taken
		}
		p.deferred = taken
	case !model.IsCode(err, model.NotFound):
		return LabelPlan{}, err
	}
	return p, nil
}

// CreateLabel submits p and returns the label as stored.
func (s Service) CreateLabel(ctx context.Context, p LabelPlan) (model.Label, model.Acceptance, error) {
	acc, err := s.Client.Labels().Configure(ctx, p.Request)
	if err != nil {
		return model.Label{}, acc, channels.Explain(err, p.deferred)
	}
	if acc.Operation.Result == nil {
		return model.Label{}, acc, fmt.Errorf("label %q: operation %s carries no label", p.Request.Label.Name, acc.Operation.Ref.ID)
	}
	ref, err := acc.Operation.Result.AsLabel()
	if err != nil {
		return model.Label{}, acc, err
	}
	if acc.Operation.State == model.Held {
		// Held: uma reserved the ID and writes nothing until approved,
		// so there is no stored label to read; answer what was proposed.
		return model.Label{Ref: ref, Name: p.Request.Label.Name, Description: p.Request.Label.Description, Color: p.Request.Label.Color}, acc, nil
	}
	l, err := s.Client.Labels().Get(ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: ref})
	return l, acc, err
}

// LabelingPlan is a validated addition or removal of labels on a
// conversation, the conversation as it is now and what would change.
type LabelingPlan struct {
	Request      model.ConversationLabels
	Remove       bool
	Conversation model.Conversation
	// Labels are the labels named, each once, in order.
	Labels []model.Label
	// Changes are the labels the write adds or removes now: the others
	// the conversation already holds (add) or does not hold (remove).
	Changes  []model.Label
	Warnings []string
}

// PlanLabeling builds and checks adding (or, when remove is set,
// removing) the labels names on the conversation id under operation op.
// Every name must be a label of the tenant (*UnknownLabelError
// otherwise); heo never creates one. Adding a label the conversation
// holds, or removing one it does not, changes nothing and is no error.
func (s Service) PlanLabeling(ctx context.Context, id string, names []string, remove bool, op model.OperationID) (LabelingPlan, error) {
	if len(names) == 0 {
		return LabelingPlan{}, model.NewError(model.Validation, "labels: at least one label is required")
	}
	use := LabelAdd
	if remove {
		use = LabelRemove
	}
	conv, err := s.FindConversation(ctx, id)
	if err != nil {
		return LabelingPlan{}, err
	}
	ls, err := s.labels(ctx, names, use)
	if err != nil {
		return LabelingPlan{}, err
	}
	req := model.ConversationLabels{Command: model.Command{Call: call(), Operation: op}, Conversation: conv.Ref, Labels: refs(ls)}
	if err := req.Validate(); err != nil {
		return LabelingPlan{}, err
	}
	p := LabelingPlan{Request: req, Remove: remove, Conversation: conv, Labels: ls}
	held := map[model.LabelRef]bool{}
	for _, r := range conv.Labels {
		held[r] = true
	}
	for _, l := range ls {
		switch {
		case held[l.Ref] == remove:
			p.Changes = append(p.Changes, l)
		case remove:
			p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s does not hold label %s: removing it changes nothing", id, l.Name))
		default:
			p.Warnings = append(p.Warnings, fmt.Sprintf("conversation %s already holds label %s: adding it changes nothing", id, l.Name))
		}
	}
	return p, nil
}

// ApplyLabeling submits p and returns the conversation as it is after
// it. uma emits one label event per label it changed, none for a label
// that was already as asked.
func (s Service) ApplyLabeling(ctx context.Context, p LabelingPlan) (model.Conversation, model.Acceptance, error) {
	submit := s.Client.Conversations().AddLabels
	if p.Remove {
		submit = s.Client.Conversations().RemoveLabels
	}
	acc, err := submit(ctx, p.Request)
	if err != nil {
		return model.Conversation{}, acc, err
	}
	conv, err := s.Client.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: p.Request.Conversation})
	return conv, acc, err
}

// labelSelector resolves f's label filters into uma's selector: nil when
// f names no label. Unknown names are not found; what each part matches
// is uma's to decide.
func (s Service) labelSelector(ctx context.Context, f Filter) (*model.LabelSelector, error) {
	if len(f.LabelAny)+len(f.LabelAll)+len(f.LabelNone) == 0 {
		return nil, nil
	}
	var sel model.LabelSelector
	for _, part := range []struct {
		names []string
		to    *[]model.LabelRef
	}{{f.LabelAny, &sel.Any}, {f.LabelAll, &sel.All}, {f.LabelNone, &sel.None}} {
		ls, err := s.labels(ctx, part.names, LabelFilter)
		if err != nil {
			return nil, err
		}
		if len(ls) > 0 {
			*part.to = refs(ls)
		}
	}
	return &sel, nil
}
