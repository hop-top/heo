package desk_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/model"
)

// agentInbox adds a per-agent-context inbox to the world's channel: the
// only inbox that holds tasks.
func (w *world) agentInbox() model.InboxRef {
	w.t.Helper()
	acc, err := w.client(operator).Configuration().ConfigureInbox(w.t.Context(), model.ConfigureInbox{Command: w.cmd(), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: tenant}, Channel: w.ch, Key: "agents", Name: "agents", Destinations: []string{"agents"},
		Selection: model.Selection{Mode: model.ModePerAgentContext},
	}})
	if err != nil {
		w.t.Fatalf("agent inbox: %v", err)
	}
	ref, _ := acc.Operation.Result.AsInbox()
	return ref
}

// openTask has a remote agent send a message in agent context ctx that
// opens a task, as an agent protocol adapter would, and returns the task.
func (w *world) openTask(event, ctx string) model.Task {
	w.t.Helper()
	w.inject(adapterstest.Inject{
		Event: event, Message: event, From: "remote", Destination: "agents", Text: "do " + event,
		AgentContext: ctx, OpenTask: true,
	})
	p, err := w.as(agent).ListTasks(w.t.Context(), desk.TaskFilter{}, model.PageRequest{Limit: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, t := range p.Items {
		if t.Context.ID == ctx {
			return t
		}
	}
	w.t.Fatalf("no task in context %s: %+v", ctx, p.Items)
	return model.Task{}
}

func taskIDs(ts []model.Task) map[string]bool {
	out := map[string]bool{}
	for _, t := range ts {
		out[string(t.Ref.ID)] = true
	}
	return out
}

// TestTaskListAndGetFilters: tasks are listed in every agent inbox the
// caller may read, narrowed by inbox, conversation and status; a message
// without a task is none; a lookup by ID finds a task and answers an
// unknown one not found; a bad status is uma's validation error.
func TestTaskListAndGetFilters(t *testing.T) {
	w := newWorld(t)
	in := w.agentInbox()
	one := w.openTask("e1", "ctx-1")
	two := w.openTask("e2", "ctx-2")
	// A message in a third context opens no task: a task is the host's choice.
	w.inject(adapterstest.Inject{Event: "e3", Message: "e3", From: "remote", Destination: "agents", Text: "just talk", AgentContext: "ctx-3"})
	ctx := t.Context()
	s := w.as(agent)

	all, err := s.ListTasks(ctx, desk.TaskFilter{}, model.PageRequest{Limit: 10})
	if err != nil || len(all.Items) != 2 || !all.Complete {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if one.State != model.TaskSubmitted || one.Ref.Conversation.Inbox != in {
		t.Fatalf("task = %+v", one)
	}
	byInbox, err := s.ListTasks(ctx, desk.TaskFilter{Inbox: "agents"}, model.PageRequest{Limit: 10})
	if err != nil || len(byInbox.Items) != 2 {
		t.Fatalf("by inbox = %+v, %v", byInbox, err)
	}
	other, err := s.ListTasks(ctx, desk.TaskFilter{Inbox: "local"}, model.PageRequest{Limit: 10})
	if err != nil || len(other.Items) != 0 {
		t.Fatalf("by other inbox = %+v, %v", other.Items, err)
	}
	byConv, err := s.ListTasks(ctx, desk.TaskFilter{Conversation: string(one.Ref.Conversation.ID)}, model.PageRequest{Limit: 10})
	if err != nil || len(byConv.Items) != 1 || byConv.Items[0].Ref.ID != one.Ref.ID {
		t.Fatalf("by conversation = %+v, %v", byConv.Items, err)
	}
	if _, err := s.ListTasks(ctx, desk.TaskFilter{Conversation: "nope"}, model.PageRequest{Limit: 10}); !model.IsCode(err, model.NotFound) {
		t.Fatalf("unknown conversation: %v", err)
	}

	// Cancel the second: the status filter tells them apart.
	plan, err := s.PlanCancel(ctx, string(two.Ref.ID), "", two.Revision, "op-c2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, plan); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[string]string{model.TaskCanceled: string(two.Ref.ID), model.TaskSubmitted: string(one.Ref.ID)} {
		p, err := s.ListTasks(ctx, desk.TaskFilter{Statuses: []string{status}}, model.PageRequest{Limit: 10})
		if err != nil || len(p.Items) != 1 || string(p.Items[0].Ref.ID) != want {
			t.Fatalf("status %s = %v, %v; want %s", status, taskIDs(p.Items), err, want)
		}
	}
	p, err := s.ListTasks(ctx, desk.TaskFilter{Statuses: []string{model.TaskWorking}}, model.PageRequest{Limit: 10})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("working = %+v, %v", p.Items, err)
	}
	_, err = s.ListTasks(ctx, desk.TaskFilter{Statuses: []string{"bogus"}}, model.PageRequest{Limit: 10})
	wantCode(t, err, model.Validation, "task states")

	// Get resolves the record ID, in the conversation when named.
	got, err := s.GetTask(ctx, string(one.Ref.ID), "")
	if err != nil || got.Ref != one.Ref || got.Revision != one.Revision {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.GetTask(ctx, string(one.Ref.ID), string(one.Ref.Conversation.ID)); err != nil {
		t.Fatalf("get in its conversation: %v", err)
	}
	if _, err := s.GetTask(ctx, string(one.Ref.ID), string(two.Ref.Conversation.ID)); !model.IsCode(err, model.NotFound) {
		t.Fatalf("get in another conversation: %v", err)
	}
	wantCode(t, errOf(s.GetTask(ctx, "nope", "")), model.NotFound, "task nope")
	wantCode(t, errOf(s.GetTask(ctx, "", "")), model.Validation, "record ID")

	// A viewer reads tasks; an external actor does not.
	if v, err := w.as(viewer).GetTask(ctx, string(one.Ref.ID), ""); err != nil || v.Ref != one.Ref {
		t.Fatalf("viewer get = %+v, %v", v, err)
	}
	if vl, err := w.as(viewer).ListTasks(ctx, desk.TaskFilter{}, model.PageRequest{Limit: 10}); err != nil || len(vl.Items) != 2 {
		t.Fatalf("viewer list = %+v, %v", vl, err)
	}
	if xl, err := w.as(external).ListTasks(ctx, desk.TaskFilter{}, model.PageRequest{Limit: 10}); err != nil || len(xl.Items) != 0 {
		t.Fatalf("external list = %+v, %v", xl.Items, err)
	}
	if _, err := w.as(external).GetTask(ctx, string(one.Ref.ID), ""); !model.IsCode(err, model.NotFound) {
		t.Fatalf("external get: %v, want not found", err)
	}
}

func errOf[T any](_ T, err error) error { return err }

// spyClient records the list requests made through its Tasks.
type spyClient struct {
	uma.Client
	lists *[]model.ListRequest
}

func (c spyClient) Tasks() uma.Tasks { return spyTasks{Tasks: c.Client.Tasks(), lists: c.lists} }

type spyTasks struct {
	uma.Tasks
	lists *[]model.ListRequest
}

func (c spyTasks) List(ctx context.Context, req model.ListRequest) (model.Page[model.Task], error) {
	*c.lists = append(*c.lists, req)
	return c.Tasks.List(ctx, req)
}

// TestTaskFiltersAreUmas: the inbox, conversation and statuses of a
// listing reach uma as its filter, and uma's page is the page: heo
// filters nothing itself.
func TestTaskFiltersAreUmas(t *testing.T) {
	w := newWorld(t)
	in := w.agentInbox()
	one := w.openTask("e1", "ctx-1")
	w.openTask("e2", "ctx-2")
	var lists []model.ListRequest
	s := w.as(agent)
	s.Client = spyClient{Client: s.Client, lists: &lists}

	_, err := s.ListTasks(t.Context(), desk.TaskFilter{Inbox: "agents", Conversation: string(one.Ref.Conversation.ID), Statuses: []string{"working", "submitted"}},
		model.PageRequest{Limit: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 {
		t.Fatalf("uma saw %d list calls, want 1", len(lists))
	}
	f := lists[0].Filter
	if len(f.Inboxes) != 1 || f.Inboxes[0] != in || f.Conversation == nil || *f.Conversation != one.Ref.Conversation ||
		strings.Join(f.Statuses, ",") != "working,submitted" || lists[0].Page.Limit != 7 {
		t.Fatalf("uma's filter = %+v page %+v", f, lists[0].Page)
	}
}

// TestTaskCancelSendsExpectedRevision: a cancellation is a task update
// to canceled that always carries the revision it replaces; uma commits
// it at once; a retry under the same operation replays; a stale
// revision and a terminal task are conflicts, in a preview and (left to
// uma) on submission.
func TestTaskCancelSendsExpectedRevision(t *testing.T) {
	w := newWorld(t)
	w.agentInbox()
	task := w.openTask("e1", "ctx-1")
	ctx := t.Context()
	s := w.as(agent)
	id := string(task.Ref.ID)

	p, err := s.PlanCancel(ctx, id, "", task.Revision, "op-cancel", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	req := p.Request
	if req.ExpectedRevision != task.Revision || req.Target != task.Ref || req.Action.Name != model.SchemaTaskUpdate || req.Operation != "op-cancel" {
		t.Fatalf("request = %+v", req)
	}
	d, err := req.Decode()
	if err != nil || d.Update == nil || d.Update.State != model.TaskCanceled || d.Update.StatusMessage != nil || len(d.Update.Artifacts) != 0 {
		t.Fatalf("action = %+v, %v", d, err)
	}
	_, err = s.PlanCancel(ctx, id, "", "", "op-x", channels.Submit)
	wantCode(t, err, model.Validation, "revision")
	_, err = s.PlanCancel(ctx, "nope", "", task.Revision, "op-x", channels.Submit)
	wantCode(t, err, model.NotFound, "task nope")

	// Stale: a preview refuses it, naming both revisions; on submission
	// uma refuses it and heo's message is the one shown.
	_, err = s.PlanCancel(ctx, id, "", "old-rev", "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, string(task.Revision))
	stale, err := s.PlanCancel(ctx, id, "", "old-rev", "op-stale", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Cancel(ctx, stale)
	wantCode(t, err, model.Conflict, "changed since it was read")

	// A viewer reads tasks and may not cancel: uma refuses the act.
	vp, err := w.as(viewer).PlanCancel(ctx, id, "", task.Revision, "op-viewer", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.as(viewer).Cancel(ctx, vp); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer cancel: %v", err)
	}

	done, err := s.Cancel(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if done.Acceptance.Operation.State != model.Committed || done.Acceptance.Replayed || done.After == nil ||
		done.After.State != model.TaskCanceled || done.After.Revision == task.Revision || done.After.Ref != task.Ref {
		t.Fatalf("cancel = %+v after %+v", done.Acceptance, done.After)
	}
	again, err := s.Cancel(ctx, p)
	if err != nil || !again.Acceptance.Replayed || again.Acceptance.Operation.Ref != done.Acceptance.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want a replay of %v", again.Acceptance, err, done.Acceptance.Operation.Ref)
	}

	// Terminal now: a preview says so; a submission is uma's conflict.
	cur, err := s.GetTask(ctx, id, "")
	if err != nil || cur.State != model.TaskCanceled {
		t.Fatalf("task = %+v, %v", cur, err)
	}
	_, err = s.PlanCancel(ctx, id, "", cur.Revision, "op-t", channels.Preview)
	wantCode(t, err, model.Conflict, "terminal task never changes")
	late, err := s.PlanCancel(ctx, id, "", cur.Revision, "op-late", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Cancel(ctx, late)
	wantCode(t, err, model.Conflict, "terminal")
}

// TestTaskWatchFollowsOneTask: a watch subscribes by kind and inbox,
// polls the events that concern its task alone — task.updated of that
// task and the status messages bound to it — and reads the task before
// each batch, so a task seen terminal has all its events in the polls
// that follow. Another task's events are in the batch and are dropped.
func TestTaskWatchFollowsOneTask(t *testing.T) {
	w := newWorld(t)
	in := w.agentInbox()
	one := w.openTask("e1", "ctx-1")
	two := w.openTask("e2", "ctx-2")
	ctx := t.Context()
	s := w.as(agent)

	sub, err := s.OpenWatch(ctx, desk.WatchStart{
		Consumer: desk.TaskConsumer(string(one.Ref.ID)), From: desk.StartTail,
		Filter: model.Filter{Kinds: desk.TaskWatchKinds, Inboxes: []model.InboxRef{in}},
	}, "op-open")
	if err != nil {
		t.Fatal(err)
	}
	poll := func() desk.TaskPoll {
		t.Helper()
		p, err := s.PollTask(ctx, one.Ref, sub.Ref)
		if err != nil {
			t.Fatal(err)
		}
		if p.Token != "" {
			if _, err := s.AckWatch(ctx, sub.Ref, p.Token); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	if p := poll(); len(p.Events) != 0 || p.Token != "" || p.Task.State != model.TaskSubmitted {
		t.Fatalf("idle poll = %+v", p)
	}

	// Another task works, then ours gets a status message and a state.
	update := func(t0 model.Task, op string, u model.TaskUpdate) {
		t.Helper()
		data, _ := json.Marshal(u)
		cur, err := s.GetTask(ctx, string(t0.Ref.ID), "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.client(agent).Tasks().Act(ctx, model.TaskAction{
			Command: model.Command{Call: call, Operation: model.OperationID(op), ExpectedRevision: cur.Revision},
			Target:  t0.Ref, Action: model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: data},
		})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	update(two, "op-two", model.TaskUpdate{State: model.TaskWorking})
	status := desk.TextContent("halfway")
	update(one, "op-one-status", model.TaskUpdate{State: model.TaskWorking, StatusMessage: &status})
	p := poll()
	var kinds []string
	for _, ev := range p.Events {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == model.EventTaskUpdated && ev.Resource != one.Ref.Ref() {
			t.Errorf("another task's event: %+v", ev.Resource)
		}
	}
	if strings.Join(kinds, ",") != model.EventMessageCreated+","+model.EventTaskUpdated || p.Task.State != model.TaskWorking {
		t.Fatalf("poll kinds = %v, task %s", kinds, p.Task.State)
	}
	if p.Token == "" {
		t.Fatal("a batch that scanned events carries a token")
	}

	// A status message of the other task is no event of ours.
	other := desk.TextContent("elsewhere")
	update(two, "op-two-status", model.TaskUpdate{StatusMessage: &other})
	if p := poll(); len(p.Events) != 0 || p.Token == "" {
		t.Fatalf("other task's status poll = %+v; want its events scanned and dropped", p)
	}

	update(one, "op-one-done", model.TaskUpdate{State: model.TaskCompleted})
	p = poll()
	if p.Task.State != model.TaskCompleted || len(p.Events) != 1 || p.Events[0].Kind != model.EventTaskUpdated {
		t.Fatalf("terminal poll = %+v", p)
	}
	if p := poll(); len(p.Events) != 0 || p.Token != "" {
		t.Fatalf("drained poll = %+v", p)
	}

	// A viewer reads tasks but not their internal events: it is no watcher.
	if vp, err := w.as(viewer).PollTask(ctx, one.Ref, sub.Ref); err == nil && len(vp.Events) > 0 {
		t.Fatalf("viewer saw task events: %+v", vp.Events)
	}

	if err := s.CloseWatch(ctx, sub.Ref, "op-close"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := s.CloseWatch(ctx, sub.Ref, "op-close2"); !model.IsCode(err, model.Conflict) {
		t.Fatalf("closing a closed subscription: %v, want conflict", err)
	}
}
