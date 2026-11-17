package desk

import (
	"context"
	"encoding/json"
	"fmt"

	"hop.top/heo/internal/channels"
	"hop.top/uma/model"
)

// Tasks. A task is uma's inbox-side agent protocol task: work the host
// executes for a remote agent, in a per-agent-context conversation. It
// is no provider effect, so no adapter capability gates it: uma
// refuses what is not a task conversation, and every call runs as the
// caller, authorized as task.read (reads) and task.act (cancel).
//
// heo only reads tasks and answers a cancel. It never opens one, and a
// message in an agent inbox never becomes a heo command: the remote
// agent reaches its tasks through its adapter, never through heo.

// TaskStates lists the states a task can hold, in lifecycle order.
func TaskStates() []string {
	return []string{
		model.TaskSubmitted, model.TaskWorking, model.TaskInputRequired, model.TaskAuthRequired,
		model.TaskCompleted, model.TaskFailed, model.TaskCanceled, model.TaskRejected,
	}
}

// TaskFilter narrows a task list. Inbox and Conversation name records
// by key or ID and Statuses are task states. uma applies all three; it
// honours no other filter on tasks, so heo offers none.
type TaskFilter struct {
	Inbox        string
	Conversation string
	Statuses     []string
}

// ListTasks returns one page of the tasks the caller may read, most
// recent status first, narrowed by f. The filter is uma's: heo resolves
// the inbox and conversation names, and uma applies them with the
// statuses (a status that is not a task state is uma's validation
// error).
func (s Service) ListTasks(ctx context.Context, f TaskFilter, page model.PageRequest) (model.Page[model.Task], error) {
	filter, err := s.taskFilter(ctx, f)
	if err != nil {
		return model.Page[model.Task]{}, err
	}
	return s.Client.Tasks().List(ctx, model.ListRequest{Call: call(), Filter: filter, Page: page})
}

// taskFilter resolves f's inbox and conversation into uma's filter.
func (s Service) taskFilter(ctx context.Context, f TaskFilter) (model.Filter, error) {
	filter, err := s.inboxFilter(ctx, Filter{Inbox: f.Inbox})
	if err != nil {
		return filter, err
	}
	filter.Statuses = f.Statuses
	if f.Conversation != "" {
		conv, err := s.FindConversation(ctx, f.Conversation)
		if err != nil {
			return filter, err
		}
		filter.Conversation = &conv.Ref
	}
	return filter, nil
}

// FindTask returns the task with record ID id among those the caller
// may read, in the conversation conv (record ID) when it is set. uma
// addresses a task by its conversation, so a lookup by ID scans what the
// caller may read.
func (s Service) FindTask(ctx context.Context, id, conv string) (model.Task, error) {
	if err := checkID("task", id); err != nil {
		return model.Task{}, err
	}
	filter, err := s.taskFilter(ctx, TaskFilter{Conversation: conv})
	if err != nil {
		return model.Task{}, err
	}
	req := model.ListRequest{Call: call(), Filter: filter, Page: model.PageRequest{Limit: scanPage}}
	for {
		p, err := s.Client.Tasks().List(ctx, req)
		if err != nil {
			return model.Task{}, err
		}
		for _, t := range p.Items {
			if string(t.Ref.ID) == id {
				return t, nil
			}
		}
		if p.Complete || p.Next == "" {
			return model.Task{}, model.NewError(model.NotFound, fmt.Sprintf("task %s: not found", id))
		}
		req.Page.After = p.Next
	}
}

// GetTask returns the task with record ID id, as uma's Tasks.Get shows
// it: the ID is resolved among the tasks the caller may read, and the
// task read again by its reference.
func (s Service) GetTask(ctx context.Context, id, conv string) (model.Task, error) {
	t, err := s.FindTask(ctx, id, conv)
	if err != nil {
		return model.Task{}, err
	}
	return s.readTask(ctx, t.Ref)
}

// readTask reads the task ref through uma.
func (s Service) readTask(ctx context.Context, ref model.TaskRef) (model.Task, error) {
	return s.Client.Tasks().Get(ctx, model.Lookup[model.TaskRef]{Call: call(), Ref: ref})
}

// CancelPlan is a validated task cancellation: the request, and the
// task as it is now.
type CancelPlan struct {
	Request model.TaskAction
	Task    model.Task
	// deferred is the stale-revision conflict a submission leaves to uma.
	deferred error
}

// PlanCancel builds and checks the cancellation of task id (in
// conversation conv, record ID, when set) at revision rev, under
// operation op: a Tasks.Act update to canceled, with the revision always
// sent. In a preview it also refuses a terminal task and a stale
// revision (model.Conflict) before any plan; a submission leaves both
// to uma, which refuses them itself and replays a retried operation.
func (s Service) PlanCancel(ctx context.Context, id, conv string, rev model.Revision, op model.OperationID, mode channels.Mode) (CancelPlan, error) {
	t, err := s.FindTask(ctx, id, conv)
	if err != nil {
		return CancelPlan{}, err
	}
	p := CancelPlan{Task: t}
	if mode == channels.Preview && model.IsTerminalTaskState(t.State) {
		return CancelPlan{}, model.NewError(model.Conflict, fmt.Sprintf(
			"task %s is %s: a terminal task never changes, so it cannot be canceled", t.Ref.ID, t.State))
	}
	if p.deferred, err = channels.CheckRevision("task", id, t.Revision, rev, mode); err != nil {
		return CancelPlan{}, err
	}
	data, err := json.Marshal(model.TaskUpdate{State: model.TaskCanceled})
	if err != nil {
		return CancelPlan{}, err
	}
	p.Request = model.TaskAction{
		Command: model.Command{Call: call(), Operation: op, ExpectedRevision: rev},
		Target:  t.Ref,
		Action:  model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: data},
	}
	if err := p.Request.Validate(); err != nil {
		return CancelPlan{}, err
	}
	return p, nil
}

// Canceled is a task cancellation as uma committed it: the acceptance,
// and the task read again afterwards, nil when that read failed (the
// cancellation stands).
type Canceled struct {
	Acceptance model.Acceptance
	After      *model.Task
}

// Cancel submits p. uma commits a task action at once, as one local
// operation: when it answers, the task has moved. The task is read again
// so that what is reported is what is stored.
func (s Service) Cancel(ctx context.Context, p CancelPlan) (Canceled, error) {
	acc, err := s.Client.Tasks().Act(ctx, p.Request)
	if err != nil {
		return Canceled{}, channels.Explain(err, p.deferred)
	}
	out := Canceled{Acceptance: acc}
	ref := p.Request.Target
	if acc.Operation.ResultTask != nil {
		ref = *acc.Operation.ResultTask
	}
	if t, err := s.readTask(ctx, ref); err == nil {
		out.After = &t
	}
	return out, nil
}

// TaskWatchKinds are the event kinds a task watch subscribes to: the
// task's own changes, and the messages that carry its status content.
var TaskWatchKinds = []string{model.EventTaskUpdated, model.EventMessageCreated}

// TaskConsumer is the name of the durable subscription that watches
// task id.
func TaskConsumer(id string) string { return "heo-task-" + id }

// TaskRefOf is the reference of the task with record ID id in the
// conversation conv of inbox in, in the profile's tenant.
func (s Service) TaskRefOf(in, conv, id model.ID) model.TaskRef {
	inbox := model.InboxRef{Tenant: s.Tenant, ID: in}
	return model.TaskRef{Conversation: model.ConversationRef{Inbox: inbox, ID: conv}, ID: id}
}

// TaskPoll is one poll of a task watch: the task as it is, the events
// of the batch that concern it, and the token that acknowledges the
// whole scanned range.
type TaskPoll struct {
	Task   model.Task
	Events []model.Event
	Token  string
}

// PollTask leases the next batch of the subscription sub, which watches
// task ref, and keeps the events that concern the task: its task.updated
// events and the status messages bound to it. uma's subscription filter
// is by kind and inbox only, so the task is told apart here, by the
// resource an event names and the task a status message is bound to.
//
// The task is read before the batch: every event committed with a state
// the read shows is in the batch or an earlier one, so a task seen
// terminal has all its events in the polls that follow.
func (s Service) PollTask(ctx context.Context, ref model.TaskRef, sub model.SubscriptionRef) (TaskPoll, error) {
	t, err := s.readTask(ctx, ref)
	if err != nil {
		return TaskPoll{}, err
	}
	b, err := s.PollWatch(ctx, sub, 0)
	if err != nil {
		return TaskPoll{}, err
	}
	out := TaskPoll{Task: t, Token: b.Token, Events: make([]model.Event, 0, len(b.Events))}
	for _, ev := range b.Events {
		if s.concernsTask(ctx, ev, ref) {
			out.Events = append(out.Events, ev)
		}
	}
	return out, nil
}

// concernsTask reports whether ev is about task ref.
func (s Service) concernsTask(ctx context.Context, ev model.Event, ref model.TaskRef) bool {
	switch ev.Kind {
	case model.EventTaskUpdated:
		return ev.Resource == ref.Ref()
	case model.EventMessageCreated:
		var p model.MessageCreatedPayload
		if json.Unmarshal(ev.Payload.Data, &p) != nil || p.Origin != model.MessageOriginTaskStatus ||
			p.Message.Conversation != ref.Conversation {
			return false
		}
		m, err := s.Client.Messages().Get(ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: p.Message})
		return err == nil && m.Task != nil && *m.Task == ref
	}
	return false
}

// CloseWatch closes the subscription sub under operation op: a task
// watch that ended at the task's terminal state has nothing left to
// resume.
func (s Service) CloseWatch(ctx context.Context, sub model.SubscriptionRef, op model.OperationID) error {
	_, err := s.Client.Events().Close(ctx, model.Lookup[model.SubscriptionRef]{Call: call(), Ref: sub},
		model.Command{Call: call(), Operation: op})
	return err
}
