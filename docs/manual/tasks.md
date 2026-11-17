# Tasks: list, get, watch, cancel

A **task** is uma's record of work your host does for a remote agent:
the agent sends a message to one of your agent inboxes, work follows, and
the task carries that work's state (`submitted`, `working`,
`input_required`, `auth_required`, then one of `completed`, `failed`,
`canceled`, `rejected`, which is final). A conversation holds one agent
context and any number of tasks, each with its own life.

heo's task commands are the **inbox side** of that exchange: you see the
tasks your inboxes hold, follow one, and answer a remote agent that asked
to cancel. They do not open tasks or report progress; that is the host
application's job through uma, and the remote agent reaches its tasks
through its own protocol adapter, never through heo. A message in an
agent inbox is content. It never becomes a heo command.

| Command | What it does |
| --- | --- |
| `heo task list` | the tasks you may read, most recent status first |
| `heo task get <task>` | one task: state, whether a cancel was asked, artifacts, revision |
| `heo task watch <task>` | the task's events, until it is in a final state |
| `heo task cancel <task> --revision R` | moves the task to `canceled` |

None of them is provider-dependent: no adapter capability gates a task
verb, so they are in the command tree on every surface and never refused
for a channel's adapter. What gates them is the conversation: a task
exists only in a **per-agent-context** conversation, and uma refuses
anything else.

## Where tasks come from

Tasks come from an agent protocol adapter. uma ships `a2a.messages`
(experimental), which makes it an A2A server endpoint: a remote agent
sends a message, and either the host opens a task for it or the agent
asks for one (`returnImmediately`). **This heo build registers no agent
protocol adapter, so until one is configured every task list is empty**,
and `heo adapter list` says which adapters the binary carries. Two things
stand between a release heo and a working A2A inbox, and both are
decisions still open:

- `heo inbox create` refuses `--mode per_agent_context` (`UNSUPPORTED`),
  the only inbox mode that holds tasks;
- the adapter reads and records cancel requests as the runtime's own
  actor, which holds no `task.read`, `conversation.read`, `message.read`
  or `task.act` (see [authorization.md](authorization.md#the-runtimes-own-actor)).

The task commands themselves work on whatever tasks the store holds.

## List and get

```sh
heo task list
heo task list --status working,input_required
heo task list --inbox agents --conversation conv_123 --limit 20
heo task get task_123
```

```text
ID        INBOX   CONVERSATION  STATE    CANCEL REQUESTED  STATUS AT             REVISION
task_123  agents  conv_123      working  yes               2026-10-12T16:02:11Z  rev_9
```

`--inbox` and `--conversation` take a key or a record ID; `--status`
takes task states, repeated or comma-separated. uma applies all three
and honours no other filter on tasks, so heo offers none and filters
nothing itself. Pages work as everywhere: follow `next` (`--cursor`)
until `complete`; a page can be short and still not the last. A task you
may not read is left out without trace. A message that opened no task is
not a task: a task is the host's choice.

`task get` shows the task as uma stores it: the conversation, inbox and
agent context, the state, `cancel_requested`, the status message and the
artifacts it produced, and the `revision` that `task cancel --revision`
takes. No such task, or one you may not read, is `NOT_FOUND`, exit 3.

`cancel_requested: true` means the **remote agent asked** to cancel and
nobody has answered yet. The task keeps its state: uma never moves a task
itself, and neither does the request.

## Cancel

```sh
heo task cancel task_123 --revision rev_9 --dry-run
heo task cancel task_123 --revision rev_9 --idempotency-key stop-42
```

`cancel` is a uma task update to `canceled` that answers a pending cancel
request, if there is one. `--revision` is required and always sent: a
task that changed since you read it, a remote cancel request included
(it bumps the revision), is `CONFLICT`, exit 4, naming the current
revision; read the task again. A task already in a final state never
changes, and canceling it is `CONFLICT` too.

The cancellation is a change **local to uma, committed at once**: it is
not queued and reaches no provider. When the command returns, the task is
canceled, and the output reads it back from uma rather than assuming it:

```text
task              task_123
conversation      conv_123
inbox             agents
state             working -> canceled
cancel requested  no
revision          rev_10
operation         op_77 (committed)
operation key     stop-42
note: committed: the task is canceled, a change local to uma; nothing is pushed to the remote agent, which sees it when it next reads the task
note: this answers the remote agent's cancel request
```

Nothing is pushed to the remote agent: it learns the state the next time
it reads the task. The output says so rather than claim more.

`--dry-run` shows the task's state, whether a cancel was requested and
the effect, and writes nothing; running it for real checks everything
again. The write is a uma operation under `--idempotency-key`: rerun the
same command with the same key and you get the same operation
(`replayed`), never a second cancel. It is in `heo operation list` and
`heo operation get --key` like any write.

## Watch

```sh
heo task watch task_123
heo task watch task_123 --timeout 10m --format json
```

`watch` prints the events of **that task**, as they are committed, and
ends by itself when the task reaches a final state: its `task.updated`
events (state, artifacts, status) and the `message.created` events of the
status messages it carries. One JSON object per line under `--format
json` (NDJSON); one row per line otherwise: time, kind, task, what
changed, event ID.

```text
2026-10-12T16:02:11Z  task.updated    task:task_123  state working -> canceled  evt_88
task task_123 reached canceled
```

It ends once the task is final and its events are written, exit 0,
whichever final state: the state is on the last line and on stderr, so
read it to tell how. Starting on a task that is already final writes
whatever the consumer has not seen (nothing, for a new consumer at the
default `--from tail`) and ends.

The watch is a durable subscription named `--consumer` (default
`heo-task-<task>`), narrowed by uma to task and message events of the
task's inbox. Each batch is acknowledged once its lines are written, so
rerunning with the same consumer resumes after the last line written:
nothing is skipped, and an event may repeat only if a watch stopped
between writing a batch and acknowledging it (deduplicate by event ID).
A new consumer starts at `--from tail` (from now on) or `beginning` (the
oldest retained event). The subscription is closed when the watch ends at
a final state. A position behind event retention is `CURSOR_EXPIRED`,
exit 4: read the task with `task get`, then watch under a new consumer.

`--timeout` stops waiting after that long: exit 6 (`TRANSIENT`), the task
was still being worked; follow it again with the same consumer. Ctrl-C or
SIGTERM acknowledges what was written, prints where it stopped and exits
0. With no daemon the watch fails at once (exit 70); an attached watch
waits for a restarting daemon up to `--reconnect-timeout` (default 30s,
see [deliveries and
events](deliveries-and-events.md#when-the-daemon-goes-away)).

Task events are internal content, so only `desk` actors may watch; a
`viewer` reads tasks with `task get` and `task list`. A watch is terminal
only: REST, gRPC, MCP and SSE do not serve it, since a stream held open
would hold the daemon. Discovery lists it as `withheld-by-command`, a
call is not found, and a served call that reaches the daemon without the
terminal's steps is refused at once. The terminal drives the same watch
over the control socket, one short step at a time, like `event watch` and
`operation watch`.

## Who may

| Command | Role |
| --- | --- |
| `task list`, `task get` | `desk`, `viewer` (`task.read`) |
| `task cancel` | `desk` (`task.act`), previews included |
| `task watch` | `desk`: task events are internal |

An `external` actor reads no tasks: `UNAUTHORIZED`, exit 5. A `viewer` who
tries `cancel` or `watch` is refused the same way, naming the reason.
`operator` alone reads no tasks; grant `desk` as well. Every command is
decided again by uma for the task, and for every event of a watch. See
[authorization.md](authorization.md).

## Refusals

| Situation | Exit | Code |
| --- | --- | --- |
| No such task, or one you may not read | 3 | `NOT_FOUND` |
| `--status` is not a task state | 2 | `VALIDATION` |
| `--revision` missing | 2 | `USAGE` |
| Stale `--revision`; the task is in a final state | 4 | `CONFLICT` |
| Role does not allow it | 5 | `UNAUTHORIZED` |
| `watch --timeout` passed with the task still open | 6 | `TRANSIENT` |
| Position behind event retention (`watch`) | 4 | `CURSOR_EXPIRED` |
| No daemon running | 70 | `PREREQUISITE` |

A list with `--conversation` naming a conversation that is not a
per-agent-context one holds no tasks and lists none; it is not an error.

## Next Steps

- [Commands Reference](commands.md#task)
- [Deliveries and events](deliveries-and-events.md)
- [Authorization](authorization.md)
