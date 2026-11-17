package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// taskRow is a task in a listing table.
type taskRow struct {
	ID           string `json:"id"           yaml:"id"           table:"ID,id"`
	Inbox        string `json:"inbox"        yaml:"inbox"        table:"INBOX"`
	Conversation string `json:"conversation" yaml:"conversation" table:"CONVERSATION"`
	State        string `json:"state"        yaml:"state"        table:"STATE"`
	Cancel       string `json:"cancel"       yaml:"cancel"       table:"CANCEL REQUESTED"`
	StatusAt     string `json:"status_at"    yaml:"status_at"    table:"STATUS AT"`
	Revision     string `json:"revision"     yaml:"revision"     table:"REVISION"`
}

func taskRowOf(t model.Task) taskRow {
	return taskRow{
		ID: string(t.Ref.ID), Inbox: string(t.Ref.Conversation.Inbox.ID), Conversation: string(t.Ref.Conversation.ID),
		State: t.State, Cancel: yesNo(t.CancelRequested), StatusAt: stamp(t.StatusAt), Revision: string(t.Revision),
	}
}

func taskRows(t model.Task) [][2]string {
	status := "-"
	if t.StatusMessage != nil {
		status = string(t.StatusMessage.ID)
	}
	rows := [][2]string{
		{"id", string(t.Ref.ID)},
		{"conversation", string(t.Ref.Conversation.ID)},
		{"inbox", string(t.Ref.Conversation.Inbox.ID)},
		{"agent context", routeName(t.Context)},
		{"state", taskStateText(t.State)},
		{"cancel requested", cancelRequestedText(t)},
		{"status message", status},
		{"artifacts", fmt.Sprint(len(t.Artifacts))},
	}
	for _, a := range t.Artifacts {
		rows = append(rows, [2]string{"artifact", fmt.Sprintf("%s %s", a.ID, completeText(a.Complete))})
	}
	return append(rows,
		[2]string{"created", stamp(t.CreatedAt)},
		[2]string{"status at", stamp(t.StatusAt)},
		[2]string{"revision", string(t.Revision)},
	)
}

func completeText(complete bool) string {
	if complete {
		return "(complete)"
	}
	return "(incomplete)"
}

// taskStateText names a state, and says when it is final.
func taskStateText(state string) string {
	if model.IsTerminalTaskState(state) {
		return state + " (terminal: it never changes)"
	}
	return state
}

// cancelRequestedText says what a pending cancel request is: the remote
// agent asked, and only the host answers.
func cancelRequestedText(t model.Task) string {
	if t.CancelRequested {
		return "yes (the remote agent asked; `task cancel` answers it)"
	}
	return "no"
}

func (a *app) taskCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "task",
		Short: "List, show, watch and cancel the agent tasks of your agent inboxes",
		Long: `Inspect and answer the agent protocol tasks in your agent inboxes: work
your host does for a remote agent, one conversation per agent context and
any number of tasks in it, each with its own state (submitted, working,
input_required, auth_required, then one of completed, failed, canceled,
rejected, which is final).

Tasks are inbox-side only. A message in an agent inbox is content, never
a command to heo; the remote agent reaches its tasks through its own
adapter, and heo opens none. Nothing here is provider-dependent: no
adapter capability gates a task verb, and a conversation that is not a
per-agent-context conversation holds no tasks. This build registers no
agent protocol adapter, so until an inbox of one is configured a list is
empty.

Every command runs in the profile's daemon (none running exits 70).
Reading is open to the desk and viewer roles; cancel is the desk role's;
watching is the desk's, since task events are internal. Listing, showing
and canceling are served on every surface. Watching is for the terminal
and the control socket: REST, gRPC, MCP and SSE do not serve it, since a
stream held open would hold the daemon.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.taskListCmd(), a.taskGetCmd(), a.taskWatchCmd(), a.taskCancelCmd())
	return grp
}

func (a *app) taskListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the tasks of your agent inboxes, most recent status first",
		Long: `List the tasks you may read, across every agent inbox you may read,
most recent status first, a page at a time (--limit, --cursor with the
previous page's next). --inbox and --conversation (key or record ID) and
--status (` + strings.Join(desk.TaskStates(), ", ") + `;
repeat or comma-separate) narrow it. uma applies all three and no other
filter exists on tasks; heo filters nothing itself. Read-only.

A page can be short and still not be the last: follow next until
complete. A task you may not read is left out without trace. A message
without a task is no task: the host opens one, or the remote agent asks
for one when it sends. Reading roles: desk, viewer.`,
		Example: `  heo task list
  heo task list --status working,input_required
  heo task list --conversation conv_123 --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			var f desk.TaskFilter
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			f.Conversation, _ = cmd.Flags().GetString(flagConversation)
			f.Statuses, _ = cmd.Flags().GetStringSlice(flagStatus)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Task], error) {
				var out page[model.Task]
				err := a.desk(ctx, c, model.ActionTaskRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListTasks(ctx, f, pg)
					out = pageFrom(p)
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]taskRow, 0, len(res.Items))
			for _, t := range res.Items {
				rows = append(rows, taskRowOf(t))
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagInbox, "", "Only tasks of this inbox (key or ID)")
	c.Flags().String(flagConversation, "", "Only tasks of this conversation (record ID)")
	c.Flags().StringSlice(flagStatus, nil, "Only tasks in these states: "+strings.Join(desk.TaskStates(), ", "))
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &page[model.Task]{}, Version: "1.0"})
	return c
}

func (a *app) taskGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <task>",
		Short: "Show one task",
		Long: `Show the task <task>, by record ID: its conversation and inbox, the
agent context, its state, whether the remote agent asked to cancel it
(cancel_requested), the status message and the artifacts it produced,
and its revision (what ` + "`task cancel --revision`" + ` takes). --conversation
(record ID) narrows the lookup. Read-only. No such task, or one you may
not read, exits 3. Reading roles: desk, viewer.`,
		Example: `  heo task get task_123
  heo task get task_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			conv, _ := cmd.Flags().GetString(flagConversation)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (model.Task, error) {
				var out model.Task
				err := a.desk(ctx, c, model.ActionTaskRead, model.Audience{}, func(s desk.Service) (err error) {
					out, err = s.GetTask(ctx, args[0], conv)
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return writeRows(cmd.OutOrStdout(), taskRows(res))
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the task belongs to (record ID); narrows the lookup")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &model.Task{}, Version: "1.0"})
	return c
}

// cancelOut is what a cancellation reports: the task as uma stores it
// after the cancel (read again, not assumed), the state it was in, and
// the operation that moved it.
type cancelOut struct {
	Task            model.ID       `json:"task"             yaml:"task"`
	Conversation    model.ID       `json:"conversation"     yaml:"conversation"`
	Inbox           model.ID       `json:"inbox"            yaml:"inbox"`
	From            string         `json:"from"             yaml:"from"`
	State           string         `json:"state"            yaml:"state"`
	CancelRequested bool           `json:"cancel_requested" yaml:"cancel_requested"`
	Answered        bool           `json:"answered"         yaml:"answered"`
	Revision        model.Revision `json:"revision"         yaml:"revision"`
	Operation       operationView  `json:"operation"        yaml:"operation"`
	OperationKey    string         `json:"operation_key"    yaml:"operation_key"`
}

func cancelRows(o cancelOut) [][2]string {
	state := o.State
	if state == "" {
		state = "not re-read"
	}
	return append([][2]string{
		{"task", string(o.Task)},
		{"conversation", string(o.Conversation)},
		{"inbox", string(o.Inbox)},
		{"state", fmt.Sprintf("%s -> %s", o.From, state)},
		{"cancel requested", yesNo(o.CancelRequested)},
		{"revision", string(o.Revision)},
	}, operationRows(o.Operation, o.OperationKey)...)
}

// cancelNotes says what a cancellation did, as stored: committed means
// the task is canceled now, a local change; nothing is pushed to the
// remote agent, which sees it when it next reads the task.
func cancelNotes(o cancelOut) []string {
	switch {
	case o.Operation.State != model.Committed:
		return append(queuedNote(o.Operation), "the task is canceled only once the operation commits: `heo operation get "+string(o.Operation.ID)+"` shows it")
	case o.State != model.TaskCanceled:
		return []string{fmt.Sprintf("the operation committed, but the task reads %s, not canceled: `heo task get %s` shows it", orDash(o.State), o.Task)}
	}
	notes := []string{"committed: the task is canceled, a change local to uma; nothing is pushed to the remote agent, which sees it when it next reads the task"}
	if o.Answered {
		notes = append(notes, "this answers the remote agent's cancel request")
	}
	if o.Operation.Replayed {
		notes = append(notes, "replayed: the same operation key had already canceled it")
	}
	return notes
}

func (a *app) taskCancelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cancel <task>",
		Short: "Cancel a task, answering the remote agent's cancel request",
		Long: `Move the task <task> (record ID) to canceled and answer a cancel request
the remote agent made, if it made one. --conversation (record ID) narrows
the lookup.

--revision names the revision being canceled, as ` + "`task get`" + ` shows it, and
is always sent: a task that changed since is a conflict (exit 4) naming
the current revision, and nothing is written. A task already in a final
state (completed, failed, canceled, rejected) never changes again, and
canceling it is a conflict too (exit 4).

The cancellation is a local change in uma, committed at once: when the
command returns, the task is canceled, and the output reads it back. It
is not queued and reaches no provider: nothing is pushed to the remote
agent, which sees the state when it next reads the task. A cancel
request is the remote agent's ask and is answered only here; until then
the task keeps working and cancel_requested stays true.

--dry-run prints the task's state, whether a cancel was requested and
the effect, and writes nothing. Committed under --idempotency-key:
rerunning with the same key returns the same operation (replayed);
without one, heo mints a key and prints it. Desk role.`,
		Example: `  heo task cancel task_123 --revision rev_9 --dry-run
  heo task cancel task_123 --revision rev_9 --idempotency-key stop-42`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			conv, _ := cmd.Flags().GetString(flagConversation)
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[cancelOut], error) {
				var w written[cancelOut]
				err := a.desk(ctx, c, model.ActionTaskAct, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanCancel(ctx, args[0], conv, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					t := p.Task
					if cli.IsDryRun(cmd) {
						w.Plan = cancelPlan(t, rev, key)
						return nil
					}
					done, err := s.Cancel(ctx, p)
					if err != nil {
						return err
					}
					w.Out = cancelOut{
						Task: t.Ref.ID, Conversation: t.Ref.Conversation.ID, Inbox: t.Ref.Conversation.Inbox.ID,
						From: t.State, Answered: t.CancelRequested, Revision: t.Revision,
						Operation: operationOf(done.Acceptance), OperationKey: key,
					}
					if after := done.After; after != nil {
						w.Out.State, w.Out.CancelRequested, w.Out.Revision = after.State, after.CancelRequested, after.Revision
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return res.render(a, cmd, cancelRows, cancelNotes(res.Out))
		},
	}
	c.Flags().String(flagConversation, "", "Conversation the task belongs to (record ID); narrows the lookup")
	addRevisionFlag(c, "task")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &cancelOut{}, Version: "1.0"})
	return c
}

// cancelPlan renders the plan of a cancellation: the task as it is,
// what the cancel moves it to, and that nothing is queued or sent.
func cancelPlan(t model.Task, rev model.Revision, key string) *cli.Plan {
	args := map[string]any{
		"task": t.Ref.ID, "conversation": t.Ref.Conversation.ID, "inbox": t.Ref.Conversation.Inbox.ID,
		"from": t.State, "state": model.TaskCanceled, "cancel_requested": t.CancelRequested,
		"expected_revision": rev,
	}
	if key != "" {
		args["operation_key"] = key
	}
	detail := fmt.Sprintf("move task %s from %s to canceled, final; a change local to uma, committed at once (nothing is queued or pushed: the remote agent sees it when it reads the task)",
		t.Ref.ID, t.State)
	if t.CancelRequested {
		detail += "; answers the remote agent's pending cancel request"
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "task:"+string(t.Ref.ID), false, detail)},
		PrerequisitesChecked: []string{"authorization", "task", "revision", "request"}}
}
