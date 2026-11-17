package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

// taskSite is a daemon on Postgres with three channels: "agent", a fake
// whose inbox "agents" is per-agent-context and opens tasks for the
// messages it is told to; "remote", uma's a2a.messages adapter, whose
// inbox "remote-in" a remote agent reaches over JSON-RPC; and "plain",
// a fake with an ordinary per-contact inbox, which holds no tasks.
type taskSite struct {
	*cliSite
	d      *daemon
	agent  model.Channel
	remote model.Channel
	plain  model.Channel
}

func newTaskSite(t *testing.T, profile string) *taskSite {
	t.Helper()
	c := newCLISite(t, noBridge)
	c.s.write("cfg/heo/profiles/default.yaml", noRateLimit+profile)
	s := &taskSite{cliSite: c}
	s.d = c.start()
	s.agent = c.fakeOf(adapterstest.Type, "agent", adapterstest.Config{Account: "acct-agent"})
	s.plain = c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
	cfg, err := json.Marshal(map[string]any{
		"destination": "remote-in", "public_url": "http://127.0.0.1/a2a", "name": "Desk agent", "description": "e2e", "version": "0.1.0",
		"skills": []map[string]any{{"id": "desk", "name": "Desk", "description": "e2e", "tags": []string{"e2e"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var out channelOutJSON
	c.json(&out, "channel", "create", "--type", "a2a.messages", "--key", "remote", "--configuration", string(cfg))
	s.remote = out.Channel
	// heo's inbox create refuses per-agent-context selection, so the agent
	// inboxes are configured beside it, as a host that embeds uma does.
	s.agentInbox(s.agent, "agents")
	s.agentInbox(s.remote, "remote-in")
	c.heo(0, "inbox", "create", "--channel", "plain", "--key", "plin", "--destination", "pl-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	return s
}

// agentInbox configures the per-agent-context inbox key of ch, bound to
// the destination of the same name, through uma.
func (s *taskSite) agentInbox(ch model.Channel, key string) {
	s.t.Helper()
	_, err := s.db.operator("default").Configuration().ConfigureInbox(s.t.Context(), model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID("inbox-" + key)},
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: "default"}, Channel: ch.Ref, Key: key, Name: key, Destinations: []string{key},
			Selection: model.Selection{Mode: model.ModePerAgentContext},
		},
	})
	if err != nil {
		s.t.Fatalf("agent inbox %s: %v", key, err)
	}
}

// restart runs the daemon again under profile, on top of noRateLimit.
func (s *taskSite) restart(profile string) {
	s.t.Helper()
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", noRateLimit+profile)
	s.d = s.start()
}

// open has the fake agent channel accept a message in agent context ctx
// that opens a task, and returns the task.
func (s *taskSite) open(event, ctx string) model.Task {
	s.t.Helper()
	s.injectReceipt(s.agent, adapterstest.Inject{
		Event: event, Message: event, From: "remote", Destination: "agents", Text: "do " + event,
		AgentContext: ctx, OpenTask: true,
	})
	return s.taskInContext(ctx)
}

// taskInContext finds the task of the agent context (the context's ID
// is the fake's own, or a2a's allocated one).
func (s *taskSite) taskInContext(ctx string) model.Task {
	s.t.Helper()
	for _, t := range s.tasks("--limit", "100") {
		if t.Context.ID == ctx {
			return t
		}
	}
	s.t.Fatalf("no task in context %s: %+v", ctx, s.tasks("--limit", "100"))
	return model.Task{}
}

func (s *taskSite) tasks(args ...string) []model.Task {
	s.t.Helper()
	var p pageJSON[model.Task]
	s.json(&p, append([]string{"task", "list"}, args...)...)
	return p.Items
}

func (s *taskSite) task(id string) model.Task {
	s.t.Helper()
	var got model.Task
	s.json(&got, "task", "get", id)
	return got
}

// rpcReply is one JSON-RPC answer of the A2A endpoint.
type rpcReply struct {
	Status int
	Result json.RawMessage
	Error  *struct {
		Code    int
		Message string
	}
}

// rpc has remote agent a call method on the A2A channel's endpoint.
func (s *taskSite) rpc(agent, method string, params any) rpcReply {
	s.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		s.t.Fatal(err)
	}
	url := "http://" + s.hook + "/webhooks/" + string(s.remote.Ref.ID)
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		req.Header.Set("Authorization", "Bearer "+adapterstest.A2ATokenPrefix+agent)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // loopback test URL
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				out := rpcReply{Status: resp.StatusCode}
				var env struct {
					Result json.RawMessage
					Error  *struct {
						Code    int
						Message string
					}
				}
				if err := json.Unmarshal(b, &env); err != nil {
					s.t.Fatalf("a2a %s = %d %s: %v", method, resp.StatusCode, b, err)
				}
				out.Result, out.Error = env.Result, env.Error
				return out
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("a2a %s: the channel's endpoint never answered (%v)", method, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// send has agent open a task with a message, as returnImmediately does,
// and returns the task as heo shows it.
func (s *taskSite) send(agent, messageID string) model.Task {
	s.t.Helper()
	r := s.rpc(agent, "SendMessage", map[string]any{
		"message":       map[string]any{"messageId": messageID, "role": "ROLE_USER", "parts": []map[string]any{{"text": "do " + messageID}}},
		"configuration": map[string]any{"returnImmediately": true},
	})
	var res struct {
		Task struct {
			ID        string
			ContextID string
		}
	}
	if r.Error != nil || json.Unmarshal(r.Result, &res) != nil || res.Task.ID == "" {
		s.t.Fatalf("SendMessage = %+v %s", r.Error, r.Result)
	}
	return s.task(res.Task.ID)
}

// remoteState is the state of task id as the remote agent reads it.
func (s *taskSite) remoteState(agent, id string) string {
	s.t.Helper()
	r := s.rpc(agent, "GetTask", map[string]any{"id": id})
	var res struct{ Status struct{ State string } }
	if r.Error != nil || json.Unmarshal(r.Result, &res) != nil {
		s.t.Fatalf("GetTask = %+v %s", r.Error, r.Result)
	}
	return res.Status.State
}

// host reports progress on task as the host application does: a Tasks.Act
// update by an operator's tool beside the daemon, at the task's current
// revision.
func (s *taskSite) host(task model.Task, op string, u model.TaskUpdate) {
	s.t.Helper()
	c := s.db.operator("default")
	cur, err := c.Tasks().Get(s.t.Context(), model.Lookup[model.TaskRef]{Call: model.Call{Correlation: "e2e"}, Ref: task.Ref})
	if err != nil {
		s.t.Fatal(err)
	}
	data, err := json.Marshal(u)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := c.Tasks().Act(s.t.Context(), model.TaskAction{
		Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID(op), ExpectedRevision: cur.Revision},
		Target:  task.Ref, Action: model.Extension{Name: model.SchemaTaskUpdate, Version: 1, Data: data},
	}); err != nil {
		s.t.Fatalf("host update %s: %v", op, err)
	}
}

// ledger counts the operations uma holds: what a refused or previewed
// verb must leave as it was.
func (s *taskSite) ledger() int {
	s.t.Helper()
	c := s.db.operator("default")
	req := model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100}}
	n := 0
	for {
		p, err := c.Operations().List(s.t.Context(), req)
		if err != nil {
			s.t.Fatal(err)
		}
		n += len(p.Items)
		if p.Complete || p.Next == "" {
			return n
		}
		req.Page.After = p.Next
	}
}

type cancelJSON struct {
	Task            model.ID
	Conversation    model.ID
	Inbox           model.ID
	From, State     string
	CancelRequested bool `json:"cancel_requested"`
	Answered        bool
	Revision        model.Revision
	Operation       opJSON
	OperationKey    string `json:"operation_key"`
}

// TestTaskListAndGet drives the e2e build on Postgres: a profile with no
// task says so with an empty complete page; tasks opened by an agent
// inbox are listed with uma's filters (inbox, conversation, status) and
// pages, a message that opened none is not a task, a bad status is
// uma's validation error (exit 2) and an unknown conversation or task
// is not found (exit 3); a conversation that is not a per-agent-context
// one holds no tasks. A viewer reads tasks; an external actor may not.
func TestTaskListAndGet(t *testing.T) {
	withE2EBinary(t, func() {
		s := newTaskSite(t, "")
		var empty pageJSON[model.Task]
		s.json(&empty, "task", "list")
		if len(empty.Items) != 0 || !empty.Complete {
			t.Fatalf("no tasks yet, list = %+v", empty)
		}
		if r := s.heo(0, "task", "list"); strings.TrimSpace(r.stdout) != "" && strings.Contains(r.stdout, "task_") {
			t.Fatalf("empty text list: %q", r.stdout)
		}

		one := s.open("e1", "ctx-1")
		two := s.open("e2", "ctx-2")
		// A message in a third context opens no task: a task is the
		// host's choice (or the remote agent's, asking for one).
		s.injectReceipt(s.agent, adapterstest.Inject{Event: "e3", Message: "e3", From: "remote", Destination: "agents", Text: "just talk", AgentContext: "ctx-3"})
		remote := s.send("agent-r", "m-1")
		if one.State != model.TaskSubmitted || two.State != model.TaskSubmitted {
			t.Fatalf("tasks = %+v %+v", one, two)
		}

		all := s.tasks()
		if len(all) != 3 {
			t.Fatalf("tasks = %d, want the three that were opened (not the message that opened none)", len(all))
		}
		ids := map[model.ID]bool{}
		for _, tk := range all {
			ids[tk.Ref.ID] = true
		}
		if !ids[one.Ref.ID] || !ids[two.Ref.ID] || !ids[remote.Ref.ID] {
			t.Fatalf("listed %v", ids)
		}
		if got := s.tasks("--inbox", "agents"); len(got) != 2 {
			t.Fatalf("--inbox agents = %d tasks", len(got))
		}
		if got := s.tasks("--inbox", "remote-in"); len(got) != 1 || got[0].Ref.ID != remote.Ref.ID {
			t.Fatalf("--inbox remote-in = %+v", got)
		}
		if got := s.tasks("--conversation", string(one.Ref.Conversation.ID)); len(got) != 1 || got[0].Ref.ID != one.Ref.ID {
			t.Fatalf("--conversation = %+v", got)
		}

		// uma applies the status filter; a cancel makes two apart.
		var c cancelJSON
		s.json(&c, "task", "cancel", string(two.Ref.ID), "--revision", string(two.Revision))
		if got := s.tasks("--status", "canceled"); len(got) != 1 || got[0].Ref.ID != two.Ref.ID {
			t.Fatalf("--status canceled = %+v", got)
		}
		if got := s.tasks("--status", "submitted,working"); len(got) != 2 {
			t.Fatalf("--status submitted,working = %d tasks", len(got))
		}
		if got := s.tasks("--status", "working"); len(got) != 0 {
			t.Fatalf("--status working = %+v", got)
		}
		wantStderr(t, s.heo(2, "task", "list", "--status", "bogus"), "VALIDATION", "task states")
		wantStderr(t, s.heo(3, "task", "list", "--conversation", "nope"), "NOT_FOUND")
		wantStderr(t, s.heo(2, "task", "list", "--limit", "0"), "USAGE")

		// Pages are uma's: one task a page, continued by the cursor.
		var seen []model.ID
		cursor := ""
		for range 5 {
			args := []string{"--limit", "1"}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			var p struct {
				Items    []model.Task
				Next     model.Cursor
				Complete bool
			}
			s.json(&p, append([]string{"task", "list"}, args...)...)
			if len(p.Items) > 1 {
				t.Fatalf("page of %d with --limit 1", len(p.Items))
			}
			for _, tk := range p.Items {
				seen = append(seen, tk.Ref.ID)
			}
			if p.Complete {
				break
			}
			cursor = string(p.Next)
		}
		if len(seen) != 3 {
			t.Fatalf("paged %v, want the three tasks", seen)
		}

		// Get.
		got := s.task(string(one.Ref.ID))
		if got.Ref != one.Ref || got.State != model.TaskSubmitted || got.CancelRequested || got.Revision != one.Revision {
			t.Fatalf("get = %+v", got)
		}
		r := s.heo(0, "task", "get", string(one.Ref.ID))
		for _, want := range []string{"submitted", "cancel requested", "revision", string(one.Revision)} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text get lacks %q:\n%s", want, r.stdout)
			}
		}
		s.json(&got, "task", "get", string(one.Ref.ID), "--conversation", string(one.Ref.Conversation.ID))
		wantStderr(t, s.heo(3, "task", "get", string(one.Ref.ID), "--conversation", string(two.Ref.Conversation.ID)), "NOT_FOUND")
		wantStderr(t, s.heo(3, "task", "get", "task_404"), "NOT_FOUND")

		// A message that is no task: it is not found as one, and the
		// ordinary conversation holds none. Tasks live only in
		// per-agent-context conversations; asking for one elsewhere is an
		// empty list, not an error.
		s.injectReceipt(s.plain, adapterstest.Inject{Event: "p1", Message: "p1", From: "ada", Destination: "pl-1", Text: "hello"})
		var convs pageJSON[model.Conversation]
		s.json(&convs, "conversation", "list", "--inbox", "plin")
		if len(convs.Items) != 1 {
			t.Fatalf("plain conversations = %+v", convs.Items)
		}
		if got := s.tasks("--conversation", string(convs.Items[0].Ref.ID)); len(got) != 0 {
			t.Fatalf("a per-contact conversation holds tasks: %+v", got)
		}
		if got := s.tasks("--inbox", "plin"); len(got) != 0 {
			t.Fatalf("a per-contact inbox holds tasks: %+v", got)
		}

		// A viewer reads tasks; an external actor may not.
		s.restart("authorization:\n  owner: [viewer]\n")
		if got := s.tasks(); len(got) != 3 {
			t.Fatalf("viewer lists %d tasks", len(got))
		}
		if v := s.task(string(one.Ref.ID)); v.Ref != one.Ref {
			t.Fatalf("viewer get = %+v", v)
		}
		s.restart("authorization:\n  owner: [external]\n")
		wantDenied(t, s.s.run(nil, "task", "list"), "heo.authz.not_granted")
		wantDenied(t, s.s.run(nil, "task", "get", string(one.Ref.ID)), "heo.authz.not_granted")
		s.stop(s.d)
	})
}

// TestTaskCancel drives the e2e build on Postgres with a remote agent
// over uma's a2a.messages adapter: the agent opens a task and asks to
// cancel it, which leaves the task working with cancel_requested set;
// the preview names the effect, says it answers the request and writes
// nothing; the cancel sends the revision, commits at once, clears the
// request and the agent reads the task canceled; a replay under the
// same key is no second cancel; a stale revision and a final task are
// conflicts (exit 4), previews included; the operation is in heo's
// history; a viewer is refused, previews included.
func TestTaskCancel(t *testing.T) {
	withE2EBinary(t, func() {
		s := newTaskSite(t, "")
		task := s.send("agent-r", "m-1")
		id := string(task.Ref.ID)
		s.host(task, "host-working", model.TaskUpdate{State: model.TaskWorking})
		task = s.task(id)
		if task.State != model.TaskWorking || task.CancelRequested {
			t.Fatalf("task = %+v", task)
		}

		// The remote agent asks to cancel: the task is still working.
		r := s.rpc("agent-r", "CancelTask", map[string]any{"id": id})
		if r.Error != nil {
			t.Fatalf("CancelTask = %+v", r.Error)
		}
		asked := s.task(id)
		if asked.State != model.TaskWorking || !asked.CancelRequested || asked.Revision == task.Revision {
			t.Fatalf("after the request, task = %+v", asked)
		}
		if out := s.heo(0, "task", "get", id).stdout; !strings.Contains(out, "the remote agent asked") {
			t.Errorf("text get does not say who asked:\n%s", out)
		}

		// The first revision is stale now: the request moved it.
		wantStderr(t, s.heo(4, "task", "cancel", id, "--revision", string(task.Revision)), "CONFLICT", "changed since it was read", string(asked.Revision))
		wantStderr(t, s.heo(4, "task", "cancel", id, "--revision", string(task.Revision), "--dry-run"), "CONFLICT", string(asked.Revision))
		// The revision is required.
		wantStderr(t, s.heo(2, "task", "cancel", id), "USAGE", "revision")
		wantStderr(t, s.heo(3, "task", "cancel", "task_404", "--revision", "x"), "NOT_FOUND")
		before := s.ledger()

		// The preview writes nothing.
		var plan planJSON
		s.json(&plan, "task", "cancel", id, "--revision", string(asked.Revision), "--dry-run")
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "task:"+id ||
			!strings.Contains(plan.Effects[0].Detail, "from working to canceled") || !strings.Contains(plan.Effects[0].Detail, "pending cancel request") ||
			!strings.Contains(plan.Effects[0].Detail, "committed at once") {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for k, want := range map[string]string{"from": `"working"`, "state": `"canceled"`, "cancel_requested": "true", "expected_revision": `"` + string(asked.Revision) + `"`} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		if after := s.task(id); after.Revision != asked.Revision || after.State != model.TaskWorking || s.ledger() != before {
			t.Fatalf("the preview wrote: task %+v, ledger %d -> %d", after, before, s.ledger())
		}

		// The cancel.
		var c cancelJSON
		s.json(&c, "task", "cancel", id, "--revision", string(asked.Revision), "--idempotency-key", "stop-1")
		if c.Task != task.Ref.ID || c.From != model.TaskWorking || c.State != model.TaskCanceled || c.CancelRequested || !c.Answered ||
			c.Operation.State != model.Committed || c.Operation.Replayed || c.OperationKey != "stop-1" || c.Revision == asked.Revision {
			t.Fatalf("cancel = %+v", c)
		}
		done := s.task(id)
		if done.State != model.TaskCanceled || done.CancelRequested || done.Revision != c.Revision {
			t.Fatalf("task after cancel = %+v", done)
		}
		if st := s.remoteState("agent-r", id); st != "TASK_STATE_CANCELED" {
			t.Fatalf("the remote agent reads %s", st)
		}
		if s.ledger() != before+1 {
			t.Errorf("ledger %d -> %d, want one operation", before, s.ledger())
		}
		text := s.heo(0, "task", "get", id).stdout
		if !strings.Contains(text, "canceled (terminal") {
			t.Errorf("text get does not say canceled is final:\n%s", text)
		}

		// Replay: the same key, the same operation, no second cancel.
		var again cancelJSON
		s.json(&again, "task", "cancel", id, "--revision", string(asked.Revision), "--idempotency-key", "stop-1")
		if !again.Operation.Replayed || again.Operation.ID != c.Operation.ID || s.ledger() != before+1 {
			t.Fatalf("replay = %+v, ledger %d", again, s.ledger())
		}

		// The key finds it under uma's own kind and target.
		op := s.operation("--key", "stop-1")
		if op.Kind != model.ActionTaskAct || op.State != model.Committed {
			t.Fatalf("operation get --key = %+v", op)
		}

		// A final task never changes: a new key at its current revision
		// is a conflict, in the preview and on submission.
		for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "stop-2"}} {
			r := s.heo(4, append([]string{"task", "cancel", id, "--revision", string(done.Revision)}, extra...)...)
			wantStderr(t, r, "CONFLICT", "terminal")
		}
		if s.ledger() != before+1 {
			t.Errorf("a refused cancel wrote: ledger %d", s.ledger())
		}
		// The remote agent cannot cancel a final task either.
		if r := s.rpc("agent-r", "CancelTask", map[string]any{"id": id}); r.Error == nil || r.Error.Code != -32002 {
			t.Errorf("CancelTask on a final task = %+v", r.Error)
		}

		// A task nobody asked about cancels on a revision it holds; a
		// stale one is a conflict naming the current revision.
		other := s.open("e9", "ctx-9")
		s.host(other, "host-w9", model.TaskUpdate{State: model.TaskWorking})
		cur := s.task(string(other.Ref.ID))
		stale := s.heo(4, "task", "cancel", string(other.Ref.ID), "--revision", string(other.Revision))
		wantStderr(t, stale, "CONFLICT", string(cur.Revision))
		if got := s.task(string(other.Ref.ID)); got.State != model.TaskWorking {
			t.Fatalf("a stale cancel moved the task: %+v", got)
		}
		var oc cancelJSON
		s.json(&oc, "task", "cancel", string(other.Ref.ID), "--revision", string(cur.Revision))
		if oc.State != model.TaskCanceled || oc.Answered || oc.OperationKey == "" {
			t.Fatalf("cancel without a request = %+v", oc)
		}

		// A viewer reads tasks and may not cancel, preview included.
		third := s.open("e10", "ctx-10")
		s.restart("authorization:\n  owner: [viewer]\n")
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, s.s.run(nil, append([]string{"task", "cancel", string(third.Ref.ID), "--revision", string(third.Revision)}, extra...)...), "heo.authz.not_granted")
		}
		if got := s.task(string(third.Ref.ID)); got.State != model.TaskSubmitted {
			t.Fatalf("a viewer canceled: %+v", got)
		}
		s.stop(s.d)
	})
}

// TestTaskWatchFollowsATaskToItsEnd drives the e2e build on Postgres: a
// watch of a task prints, as NDJSON, the events of that task alone — the
// host's progress with its status message, the cancel — and ends, exit
// 0, by itself once the task is final, with its subscription closed;
// another task's events are not shown; a task already final ends the
// watch at once; a task still open when --timeout passes is exit 6; a
// viewer, who reads tasks but not their internal events, is refused.
func TestTaskWatchFollowsATaskToItsEnd(t *testing.T) {
	withE2EBinary(t, func() {
		s := newTaskSite(t, "")
		task := s.open("e1", "ctx-1")
		other := s.open("e2", "ctx-2")
		id := string(task.Ref.ID)

		// The preview opens nothing.
		var plan planJSON
		s.json(&plan, "task", "watch", id, "--dry-run")
		if len(plan.Effects) != 1 || !strings.HasPrefix(plan.Effects[0].Target, "subscription:heo-task-"+id) {
			t.Fatalf("watch preview = %+v", plan.Effects)
		}

		w := s.s.watchOf([]string{"task", "watch"}, id, "--format", "json")
		eventually(t, "the watch to open", func() bool { return strings.Contains(w.errb.String(), "watching task "+id) })
		status := model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "halfway there"}}}
		s.host(other, "other-w", model.TaskUpdate{State: model.TaskWorking})
		s.host(task, "task-w", model.TaskUpdate{State: model.TaskWorking, StatusMessage: &status, Artifacts: []model.ArtifactUpdate{{ID: "report", Content: status}}})
		evs := w.until(func(ev model.Event) bool {
			var p model.TaskUpdatedPayload
			return ev.Kind == model.EventTaskUpdated && json.Unmarshal(ev.Payload.Data, &p) == nil && p.State != nil && p.State.To == model.TaskWorking
		})
		var cur model.Task
		s.json(&cur, "task", "get", id)
		var c cancelJSON
		s.json(&c, "task", "cancel", id, "--revision", string(cur.Revision))
		evs = append(evs, w.until(func(ev model.Event) bool {
			var p model.TaskUpdatedPayload
			return ev.Kind == model.EventTaskUpdated && json.Unmarshal(ev.Payload.Data, &p) == nil && p.State != nil && p.State.To == model.TaskCanceled
		})...)

		// It ends by itself, exit 0, naming the state it ended in.
		select {
		case <-w.done:
		case <-time.After(30 * time.Second):
			t.Fatalf("the watch did not end at the final state:\n%s", w.errb.String())
		}
		if w.exit != 0 || !strings.Contains(w.errb.String(), "task "+id+" reached canceled") {
			t.Fatalf("watch exit %d\n%s", w.exit, w.errb.String())
		}
		var sawStatus, sawArtifacts bool
		for _, ev := range evs {
			switch ev.Kind {
			case model.EventTaskUpdated:
				if ev.Resource.ID != task.Ref.ID {
					t.Errorf("another task's event: %+v", ev)
				}
				var p model.TaskUpdatedPayload
				_ = json.Unmarshal(ev.Payload.Data, &p)
				sawArtifacts = sawArtifacts || strings.Contains(strings.Join(p.Changes, ","), "artifacts")
			case model.EventMessageCreated:
				var p model.MessageCreatedPayload
				_ = json.Unmarshal(ev.Payload.Data, &p)
				sawStatus = sawStatus || p.Origin == model.MessageOriginTaskStatus
			default:
				t.Errorf("event kind %s", ev.Kind)
			}
		}
		if !sawStatus || !sawArtifacts {
			t.Errorf("status message %v, artifacts %v in %d events", sawStatus, sawArtifacts, len(evs))
		}

		// A task already final ends a watch at once, with nothing new to
		// say for a consumer starting at tail.
		r := s.heo(0, "task", "watch", id)
		if r.stdout != "" || !strings.Contains(r.stderr, "reached canceled") {
			t.Errorf("watch of a final task:\nstdout %q\nstderr %q", r.stdout, r.stderr)
		}

		// A task still open when --timeout passes: exit 6.
		r = s.heo(6, "task", "watch", string(other.Ref.ID), "--timeout", "2s")
		wantStderr(t, r, "TRANSIENT", "still working", "--consumer heo-task-"+string(other.Ref.ID))
		// Its own consumer resumes: what happened while it was away is
		// delivered, then the watch ends with the task.
		s.host(other, "other-done", model.TaskUpdate{State: model.TaskCompleted})
		r = s.heo(0, "task", "watch", string(other.Ref.ID))
		if !strings.Contains(r.stderr, "reached completed") || !strings.Contains(r.stdout, "task.updated") {
			t.Errorf("resumed watch:\nstdout %q\nstderr %q", r.stdout, r.stderr)
		}
		wantStderr(t, s.heo(3, "task", "watch", "task_404"), "NOT_FOUND")

		// A viewer reads tasks, not their internal events.
		s.restart("authorization:\n  owner: [viewer]\n")
		wantDenied(t, s.s.run(nil, "task", "watch", string(other.Ref.ID)), "heo.authz.desk_only")
		s.stop(s.d)
	})
}

// TestTaskWatchIsSocketOnly drives the e2e build with the socket, REST,
// gRPC and MCP all serving: task watch is withheld from the three
// network surfaces and runs over the control socket, which carries the
// terminal's watch as short --step calls; a served call without --step
// is refused at once. task list, get and cancel, plain requests and
// replies, are served on every surface: a cancel over REST commits.
func TestTaskWatchIsSocketOnly(t *testing.T) {
	withE2EBinary(t, func() {
		s := newTaskSite(t, "")
		task := s.open("e1", "ctx-1")
		id := string(task.Ref.ID)
		x, token, sock := serveCLI(t, s.cliSite, s.d)
		defer func() { s.s.want(result{exit: x.d.term()}, 0) }()

		code, doc := x.rest(t, http.MethodGet, "/v1/commands", token, "")
		if code != http.StatusOK {
			t.Fatalf("discovery = %d", code)
		}
		seen := map[string]map[string]any{}
		cmds, _ := doc["commands"].([]any)
		for _, c := range cmds {
			e, _ := c.(map[string]any)
			if name, _ := e["name"].(string); strings.HasPrefix(name, "task ") {
				seen[name] = e
			}
		}
		if e := seen["task watch"]; e == nil || e["invocable"] != false || e["reason"] != "withheld-by-command" {
			t.Errorf("REST discovery of task watch = %v", e)
		}
		for _, name := range []string{"task list", "task get", "task cancel"} {
			if e := seen[name]; e == nil || e["invocable"] != true {
				t.Errorf("REST discovery of %s = %v", name, e)
			}
		}
		for _, route := range []string{"/v1/commands/task/watch", "/v1/commands/task/watch/stream"} {
			within(t, 10*time.Second, "REST "+route, func() {
				if code, body := x.rest(t, http.MethodGet, route, token, ""); code != http.StatusNotFound {
					t.Errorf("REST %s = %d %v, want 404", route, code, body)
				}
			})
		}
		resp, err := authed(http.MethodGet, "http://"+x.api+"/openapi.json", token, "")
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.Contains(string(spec), "task/watch") || !strings.Contains(string(spec), "task/list") {
			t.Errorf("OpenAPI = %d, task watch listed: %v, task list listed: %v", resp.StatusCode,
				strings.Contains(string(spec), "task/watch"), strings.Contains(string(spec), "task/list"))
		}
		within(t, 10*time.Second, "gRPC task watch", func() {
			code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"task", "watch"}, "args": []string{id}})
			if code != http.StatusNotFound || !strings.Contains(strings.ToLower(asString(body)), "not_found") {
				t.Errorf("rpc task watch = %d %v, want not_found", code, body)
			}
		})
		m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
		tools := m.tools(t)
		if tools["task.watch"] || !tools["task.list"] || !tools["task.get"] || !tools["task.cancel"] {
			t.Errorf("MCP tools: task.watch %v, task.list %v, task.get %v, task.cancel %v",
				tools["task.watch"], tools["task.list"], tools["task.get"], tools["task.cancel"])
		}
		within(t, 10*time.Second, "MCP task.watch", func() {
			b, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 99, "method": "tools/call",
				"params": map[string]any{"name": "task.watch", "arguments": map[string]any{"args": []string{id}}},
			})
			status, raw := m.postStatus(t, string(b))
			if status < 400 && (strings.Contains(string(raw), `"isError":false`) || !strings.Contains(string(raw), "error")) {
				t.Errorf("MCP task.watch = %d %s, want a refusal", status, raw)
			}
		})

		// The socket: no --step is refused at once; a step runs.
		within(t, 10*time.Second, "socket task watch without --step", func() {
			resp := sockCall(t, sock, map[string]any{"path": []string{"task", "watch"}, "args": []string{id}})
			res, _ := resp["result"].(map[string]any)
			if resp["ok"] == true && res != nil && res["exit_code"] == float64(0) {
				t.Errorf("socket ran a stream inside the daemon: %v", resp)
			}
			if !strings.Contains(asString(resp), "control socket one step at a time") {
				t.Errorf("socket refusal does not say what to do: %v", resp)
			}
		})
		step := resultData(t, sockCall(t, sock, map[string]any{"path": []string{"task", "watch"}, "args": []string{id},
			"flags": map[string]any{"step": `{"op":"open"}`}}))
		if step["subscription"] == nil || step["subscription"] == "" || step["state"] != model.TaskSubmitted || step["terminal"] != false {
			t.Errorf("socket open step = %v", step)
		}

		// Served surfaces read and cancel tasks.
		within(t, 10*time.Second, "REST task list", func() {
			if code, body := x.rest(t, http.MethodGet, "/v1/commands/task/list", token, ""); code != http.StatusOK {
				t.Errorf("REST task list = %d %v", code, body)
			}
		})
		within(t, 10*time.Second, "MCP task.list", func() { m.callTool(t, "task.list", map[string]any{}) })
		within(t, 10*time.Second, "REST task cancel", func() {
			code, body := x.rest(t, http.MethodPost, "/v1/commands/task/cancel", token,
				argsBody([]string{id}, map[string]any{"revision": string(task.Revision), "idempotency-key": "rest-stop"}))
			if code != http.StatusOK {
				t.Errorf("REST task cancel = %d %v", code, body)
			}
		})
		cur, err := s.db.operator("default").Tasks().Get(t.Context(), model.Lookup[model.TaskRef]{Call: model.Call{Correlation: "e2e"}, Ref: task.Ref})
		if err != nil || cur.State != model.TaskCanceled {
			t.Errorf("task after the REST cancel = %+v, %v", cur, err)
		}
	})
}

// serveCLI stops d and runs the daemon again with the socket, REST,
// gRPC and MCP serving, behind a token the test's uid holds.
func serveCLI(t *testing.T, c *cliSite, d *daemon) (x *surfaces, token, sock string) {
	t.Helper()
	c.stop(d)
	auth := c.s.useHTTPAuth()
	api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
	nd := c.s.start(nil, nil, []string{"socket", "workers", "receivers", "api", "rpc", "mcp"}, "-c", auth.config,
		"service", "start", "--enable", "api", "--enable", "rpc", "--enable", "mcp",
		"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("serving daemon log:\n%s", nd.log())
		}
	})
	return &surfaces{d: nd, auth: auth, api: api, rpc: rpc, mcp: mcp}, auth.token(me()), nd.addr("socket")
}
