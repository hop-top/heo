# Deliveries and events

Follow what happens after a write: find a write again when its answer
never reached you, whether a reply reached the provider, how to recover
one that did not, and the events uma commits, listed or streamed.

- An **operation** is uma's record that it accepted a write, which the
  daemon's workers then carry out. Every write prints its operation
  (`op_123` below) and the **operation key** it was submitted under.
- A **delivery** is the operation behind an outbound write — a reply, a
  note the provider receives, a conversation start.
- An **event** is a change uma committed: a message received or
  created, an interaction received (a button press, a command or a form
  a chat provider reported), a conversation changed, a delivery updated.
  An event is a reference to what changed, never a message body.

Every command here runs in the profile's daemon. Start it first with
`heo service start`; with none running, these commands exit 70.

## Recover a write whose answer was lost

A write returns its operation ID and key once uma accepted it. If the
connection dropped before you saw them, the write may still have been
accepted. The key is the one thing you chose (`--idempotency-key`) or
that heo minted and printed, so find the operation by it:

```sh
heo operation get --key send-42
```

```text
id            op_123
state         queued
terminal      no
key           send-42
kind          conversation.start
target        inbox:in_1
message       msg_9
conversation  conv_7
revision      r1
```

uma finds an operation by the full identity of the write — you, its
kind, its target and its key — and answers for **your own** writes
alone: a key another caller used, or one uma never accepted, is
`NOT_FOUND` (exit 3). The key is enough: with no kind or target, heo
reads your operations in uma's ledger, newest change first, for the key
(heo keeps no file of its own). A key you used for more than one
operation names them all (`CONFLICT`, exit 4); narrow it with `--kind`
(such as `message.reply`) and `--target` (`<kind>:<id>`, or
`<kind>:<inbox>/<id>` for a record in an inbox). With both, uma answers
directly and nothing is read through:

```sh
heo operation get --key send-42 --kind conversation.start --target inbox:in_1
```

A key uma no longer holds (past its operation retention, see
`runtime.retention.operations`) is `NOT_FOUND` too, which does **not**
mean the command was never accepted.

The other way to recover is to run the identical command again with the
same key. uma returns the stored operation (`replayed`) and sends
nothing twice; a changed payload under the same key is `CONFLICT`
(exit 4).

`operation get <operation>` reads one by its ID, and `delivery get`
adds the retry advice. A state is an answer, not a failure: `failed` and
`outcome_unknown` operations exit 0 with the state in the output.

## Follow an operation

```sh
heo operation watch op_123
heo operation watch op_123 --timeout 2m --format json
```

`operation watch` prints each change of state as it happens — one JSON
object per line under `--format json` (NDJSON), one row per line
otherwise (time, state, operation, failure) — and exits by the state the
operation ends in:

| Ends in | Exit |
| --- | --- |
| `delivered`, `provider_accepted`, `committed` | 0 |
| `failed`, `rejected`, `dead_lettered` | that of its failure: a final provider failure `PROVIDER_PERMANENT` 1, a refused permission `UNAUTHORIZED` 5, and so on |
| `outcome_unknown` | 8, `OUTCOME_UNKNOWN`: never 0 and never 6 |
| still `queued` or `retry_pending` when `--timeout` passes | 6, `TRANSIENT`: follow it again |

`queued` and `retry_pending` are being worked, so the watch waits.
`outcome_unknown` ends it because the daemon may reconcile it later,
and that can take as long as the provider's settle window; watch again,
or read it with `delivery get`. Ctrl-C or SIGTERM stops the watch,
prints where it stopped and exits 0. A watch follows a daemon that
restarts the way `event watch` does (see [Watch events](#watch-events)).

## List what you submitted

```sh
heo operation list
heo operation list --kind message.reply --since 2026-10-01
heo operation list --state outcome_unknown --format json
```

Your operations as uma's ledger holds them, newest change first, so an
operation that settled moves to the top: key, kind, state, when it last
changed, the operation ID and the target (`--format json` has them all;
a narrow terminal drops the right-hand columns of the table). The key,
kind and target are what recovers an operation by `operation get --key`.

| Flag | Keeps |
| --- | --- |
| `--kind` | operations of these kinds, such as `message.reply` or `conversation.start` (repeat or comma-separate); a kind nothing has lists nothing |
| `--state` | operations in these states (repeat or comma-separate) |
| `--inbox` | operations on one inbox (key or ID): its conversations, messages and tasks, and writes aimed at it |
| `--conversation` | operations on one conversation (ID), or that produced a message in it |
| `--since`, `--until` | operations last changed in `[since, until)` (RFC 3339 or a date) |
| `--limit`, `--cursor` | a page at a time; the next page's cursor is printed |

A page can be short, or empty, and still have more: uma matches the kind
against the operations it scanned and leaves out the ones you may not
read without trace. Follow `next` until the listing is `complete`.

The list is **yours**: heo asks uma for the operations you sent. uma
itself lists every operation a caller may read (`operation.read`),
whoever sent it, and `operation get <operation>` and `delivery get`
read any one by ID the same way; the listing narrows to the caller's own
so a desk does not see its colleagues' writes. A write uma refused
before accepting it is not an operation and is not listed.

## List deliveries

```sh
heo delivery list --state outcome_unknown
heo delivery list --inbox support --state failed,dead_lettered
heo delivery list --conversation conv_123 --since 2026-10-01
```

The outbound writes you submitted — replies, notes, conversation starts,
issue actions — newest change first, each with its operation, state,
kind, conversation, inbox, when it last changed and the key it was
submitted under. A `verdict` (JSON) says what a state allows: `retry`, `wait`,
`done`, `resubmit` or `reconcile`, as in the table below.

| Flag | Keeps |
| --- | --- |
| `--state` | deliveries in these states (repeat or comma-separate) |
| `--inbox` | one inbox's (key or ID) |
| `--conversation` | one conversation's (ID); a conversation start belongs to the conversation it created. Name `--inbox` too when your role cannot read conversations (the operator's cannot) |
| `--since`, `--until` | deliveries last changed in `[since, until)` (RFC 3339 or a date) |
| `--limit`, `--cursor` | a page at a time; the next page's cursor is printed |

`--state outcome_unknown` is what to reconcile, `--state dead_lettered`
what `delivery retry` may replay, `--state failed` what to send again
under a new key.

The list is uma's operation ledger restricted to the outbound kinds
(`message.reply`, `message.note`, `conversation.start`,
`work_item.act`), asked as you: it shows your own writes only, and the
state uma holds now. A write made in another profile or on another
host is not yours here; one uma no longer holds is not listed.
`operation get --key` with `--kind` and `--target` still finds one
while uma keeps it.

## Check a delivery

```sh
heo delivery get op_123
heo delivery get op_123 --format json
```

It shows the state, what the operation produced (the message and its
conversation), the provider references known so far, the failure with
uma's retry advice, `next attempt` when uma has scheduled one (when it
reconciles an `outcome_unknown` delivery, or sends a `retry_pending` one
again), and `next`: what to do now.

| State | Meaning | Next |
| --- | --- | --- |
| `queued` | accepted durably; no attempt answered yet | wait: the daemon's workers run it |
| `retry_pending` | an attempt failed without applying anything | wait: uma retries it on its own, at `next attempt` (`retry after` when the provider set one) |
| `provider_accepted`, `delivered` | the provider has it | nothing |
| `committed` | a local write: nothing goes to a provider | nothing |
| `failed`, `rejected` | final: uma never runs it again | fix the cause, then send it again as a new operation, under a new `--idempotency-key` |
| `outcome_unknown` | the provider may or may not have applied it | reconcile first (`delivery reconcile`, below); `next attempt` is when uma asks on its own |
| `dead_lettered` | quarantined after attempts that never finished | `heo delivery retry op_123` once the cause is fixed |

A write's success means durable acceptance, never delivery: a reply
returns `queued`, and `delivery get` follows it from there.

## Retry a dead-lettered delivery

```sh
heo delivery retry op_123 --dry-run
heo delivery retry op_123
```

`delivery retry` replays a `dead_lettered` delivery, and nothing else.
It checks the state before submitting anything:

| State | Result |
| --- | --- |
| `dead_lettered` | queued again; the daemon's workers run it, checking the original actor's permission first |
| `outcome_unknown` | refused: `OUTCOME_UNKNOWN`, exit 8, with the reconcile path |
| `failed`, `rejected` | refused: `CONFLICT`, exit 4 — send it again under a new `--idempotency-key` |
| any other | refused: `CONFLICT`, exit 4 — nothing to retry |

`--dry-run` shows the replay and submits nothing. The retry is a uma
operation keyed by `--idempotency-key`: rerunning it with the same key
returns the same replay (`replayed`) and queues nothing twice, even after
the delivery moved on. Without a key, heo mints one and prints it.

Delivery is at least once. If an earlier attempt reached the provider
before the work was quarantined, the provider may receive the message
twice: a retry never promises otherwise.

### Reconcile an unknown outcome

`outcome_unknown` means the provider may already have the message — a
connection dropped after the request was sent, say. Retrying blindly
could deliver it twice, so heo refuses to retry it. It is reconciled
instead: the channel's adapter looks the message up at its provider (its
sent copy, audit trail or echo), and only its answer changes the
delivery. uma's workers ask on their own schedule, shown as `next
attempt` in `delivery get`; `delivery reconcile` asks now.

```sh
heo delivery reconcile op_123 --dry-run     # what would be asked, of whom; asks nothing
heo delivery reconcile op_123
```

```text
id              op_123
state           provider_accepted
was             outcome_unknown
reconciliation  settled
adapter         mail.imap (declares reconcile)
provider refs   message 18c2f4a
next            nothing to do: provider_accepted
revision        r5
```

What the provider's answer does, and the exit code:

| Provider's answer | Delivery after | `reconciliation` | Exit |
| --- | --- | --- | --- |
| the message is there | `provider_accepted` or `delivered`, with the provider's references | `settled` | 0 |
| reported failed for good | `failed` or `rejected` (send it again under a new `--idempotency-key`) | `settled` | 0 |
| no record, past the adapter's settle window | `retry_pending`: it was not applied, so uma sends it again at `next attempt` | `not_applied` | 0 |
| proves nothing, such as no record inside the settle window | unchanged: `outcome_unknown`, with `next attempt` when uma has one scheduled | `pending` | 8, `OUTCOME_UNKNOWN`: the outcome is still unknown, never 0 |
| the provider cannot be asked | unchanged; nothing is recorded | | 6, `UNAVAILABLE` (transient): reconcile again later |
| the adapter declares no reconcile | unchanged | | 2, `UNSUPPORTED`: nothing will reconcile it; the next step is the provider |
| a worker is carrying it out right now | unchanged | | 4, `CONFLICT`: reconcile again in a moment |
| any state but `outcome_unknown` | as it is; the provider is not asked | `not_needed`, with what to do next as `delivery get` says | 0 |

heo never infers an outcome: `settled`, `not_applied` and `pending`
report what uma holds after the adapter's answer, and the command
sends nothing itself. A reconcile that finds no record makes the
delivery `retry_pending`; uma's workers then send it again, and it ends
as that attempt does. Reconciling is idempotent: repeating it, or
running it after uma settled the delivery, is safe and asks the
provider nothing more. It claims no operation key, so it takes no
`--idempotency-key`.

`--dry-run` reads the delivery and the adapter behind it and shows what
would be asked; it asks the provider nothing. Reconciling spends a
provider call, so it needs its own permission (uma's
`operation.reconcile`): the `desk` and `operator` roles hold it, and
`viewer` and `external` actors are refused (`UNAUTHORIZED`, exit 5)
before the provider is asked.

When the adapter cannot reconcile, or you cannot wait, do it by hand:

1. Look for the message at the provider, by the provider references
   `delivery get` shows, or by the conversation.
2. Found: nothing to do.
3. Not found: send it again as a new operation, under a new
   `--idempotency-key`.

Poll `delivery get`, or watch `delivery.updated` events
(`heo event watch`), to see uma settle it.

## List events

```sh
heo event list                                    # oldest retained first
heo event list --inbox web --kind message.created --limit 20
heo event list --from tail --format json          # nothing yet: its next is the head
heo event list --cursor pg.42 --format json       # continue at a position
```

`event list` reads one page of the events you may read, oldest first:
recorded time, kind, resource, inbox, visibility, event ID. Under
`--format json` the page is `items`, `next` and `complete`. Unlike a
watch it keeps nothing: no subscription is opened and no checkpoint
moves, so it is safe to repeat and to run beside a watch.

`--inbox` (key or ID) and `--kind` (repeat or comma-separate) narrow the
list. They are the only filters uma's event log honours; `event list`
offers no `--conversation` or `--since`. `--kind interaction.received` lists
the interactions a provider reported, next to `message.received`; the event
names the interaction (the resource), never what it carried. `heo
interaction get <interaction>` reads it, and `heo interaction respond`
answers it. To read one event you have an
ID for, use `event get` (below).

### Page until complete

`--limit` (default 50, at most 100) bounds the events uma **scans** for
the page, not the events shown. An event you may not read is dropped
without a trace and still counts, so a page can be short, or empty, and
not be the last. Only `complete: true` is the end. Continue at the
page's `next`:

```sh
heo event list --limit 100 --format json          # → "next": "pg.100", "complete": false
heo event list --limit 100 --cursor pg.100 --format json
```

In the table format the next page is printed on stderr:
`more: --cursor pg.100`. `next` is also a checkpoint: it continues a
list, and `heo event watch --cursor pg.100` starts a new consumer there.
A list from the head continued by `next` shows each later event once.

### Start and gaps

| Start | Reads |
| --- | --- |
| `--from beginning` (default) | the oldest retained event onward |
| `--from tail` | nothing: an empty, complete page whose `next` is the head |
| `--cursor <position>` | after that position, from an earlier page's `next` or a watch's checkpoint |

heo never skips history silently. A `--cursor` behind what event
retention kept is refused, `CURSOR_EXPIRED` (exit 4), exactly as a
watch is, never a list that quietly starts at what is left. The events
in between are gone: start over with `--from beginning`, then reconcile
from current state — `heo conversation list`, `heo delivery get`.
`--cursor` and `--from` together are a usage error (exit 2).

### What a list shows

uma authorizes the call and then every event, one at a time, and heo
authorizes each again before it is shown. Content that is not public —
internal notes, restricted delivery updates — is listed to desk actors
alone. A `viewer` or `external` actor lists public events only, in every
format; the events it may not read are skipped as if absent. `event list`
is a plain request and reply, so REST, gRPC, MCP and the control socket
all serve it.

## Get one event

```sh
heo event get 0190f3c2-7a1e-7c11-b2d0-3f6a9c1e4b55
heo event get 0190f3c2-7a1e-7c11-b2d0-3f6a9c1e4b55 --format json
```

`event get <event>` reads the event with that ID, the one a list or a
watch printed: its kind, the resource it concerns, the inbox, its
visibility, who and when, and the schema of its payload. Like a list it
shows a reference, never a message body, and it keeps nothing: no
subscription is opened and no checkpoint moves. REST, gRPC, MCP and the
control socket all serve it.

Every way an event can be out of reach looks the same. An event that
does not exist, one past event retention, one of another tenant and one
whose content is not public (an internal note's) to a `viewer` or
`external` actor all exit 3 with `NOT_FOUND` and one message, `event
<id>: not found`, so the answer never says whether the event is there.
uma applies the rule, and heo applies the caller's audience rule again
before showing anything, as it does for every listed event.

## Watch events

```sh
heo event watch                                   # from now on
heo event watch --from beginning --format json    # oldest retained, NDJSON
heo event watch --consumer audit                  # a subscription of its own
```

`event watch` streams until interrupted: one JSON object per line under
`--format json` (NDJSON), one row per line otherwise — recorded time,
kind, resource, inbox, visibility, event ID.

The stream is a durable subscription, named by `--consumer` (default
`heo-cli`) and owned by you. heo acknowledges each batch only after its
lines are written, which moves the subscription's checkpoint past it.
Ctrl-C or SIGTERM acknowledges what was written, prints where it
stopped and exits 0:

```text
stopped: consumer heo-cli at checkpoint pg.42; rerun with --consumer heo-cli to resume
```

Rerun with the same consumer to resume right after the last line
written. Delivery is at least once: an event repeats only if a watch
died between writing a batch and acknowledging it. Deduplicate by event
ID.

### When the daemon goes away

`event watch`, `task watch` and `operation watch` treat a daemon that
does not answer the same way:

| The watch | The daemon does not answer | Result |
| --- | --- | --- |
| is starting | `PREREQUISITE`, exit 70, with the command that starts one; no waiting and no retry | rerun once the daemon is up |
| is attached | one line on stderr, `daemon unavailable, waiting up to 30s`, then retries every few seconds | carries on where it was when the daemon returns, with no gap and no repeat |
| is attached, and the daemon is still away after `--reconnect-timeout` | `PREREQUISITE`, exit 70 | start the daemon, rerun with the same consumer to resume |

`--reconnect-timeout` (default `30s`) is the longest run of continuous
unavailability a watch waits out, long enough for `heo service restart`
or a service manager's restart; `0` fails at once. `--timeout`, where a
watch has one, still ends the wait: exit 6. Anything else the daemon
refuses as transient, its rate limit say, is retried without that bound.

Where a watch starts:

| Consumer | Start |
| --- | --- |
| open (watched before) | its own checkpoint, always |
| new | `--from tail` (default: from now on), `--from beginning` (oldest retained) or `--cursor <checkpoint>` |

### Gaps and expired checkpoints

heo never skips history silently.

- `--cursor` on a consumer that is open at another checkpoint would
  skip or repeat the events between the two: refused, `CONFLICT`
  (exit 4). Drop `--cursor` to resume where the consumer is, or pass a
  new `--consumer` to start at the cursor.
- Event retention purges old events. A checkpoint behind what is
  retained is refused, `CURSOR_EXPIRED` (exit 4), on start or while
  watching. The events in between are gone: start a new consumer with
  `--from beginning` (or `--from tail`), then reconcile from current
  state — `heo conversation list`, `heo delivery get`.

### Who sees what

Every event is authorized for you, one at a time: by uma when the batch
is leased, and by heo again before the line is written. Content that is
not public — internal notes, restricted delivery updates — reaches desk
actors alone. A `viewer` or `external` actor sees public events only,
in every format.

### Pace

A watch at the terminal drives the daemon one control socket call at a
time: lease a batch, write it, acknowledge it. An idle watch polls once
a second. The daemon rate-limits each caller's writes; a watch backs off
and carries on when it is throttled.

### React to an interaction

A script that answers a button press or a slash command reads
`interaction.received` as it lands, takes the event's resource ID and
calls `heo interaction get <interaction>` or the answer itself. Slack gives
a modal three seconds from its request (`heo interaction open-modal`,
[Slack modals](provider-verbs.md#slack-modals)), and the watch's pace is
part of them: an idle watch polls once a second, so an event can reach the
script up to a second after uma committed it. After that, `open-modal` takes
about 50 ms from the command's start to its exit on a local daemon, and the
workers are woken as it queues; Slack's own answer is the rest. A script
that needs the event sooner reads the bus's file sink (below), which is
written as the event is published.

```sh
heo event watch --consumer modals --format json   # NDJSON; keep kind interaction.received
heo interaction open-modal <interaction> --view approve.json --idempotency-key open-<event>
```

Use the event ID in the idempotency key: the watch repeats an event only
after a crash, and a retry under the same key is the same operation, not a
second modal.

### Terminal only

`event watch` and `operation watch` are for the terminal. REST, gRPC and
MCP do not serve them:
a stream held open would hold the daemon's one call at a time, and an
MCP tool call that never returns is no tool call. Discovery on REST
describes it as not invocable (`withheld-by-command`), a call is
refused as not found (REST `404`, gRPC `NotFound`, MCP an unknown tool),
and no HTTP configuration turns it on. To follow events from another
program, run `heo event watch --format json` and read its lines; to
follow an operation, `heo operation watch --format json`, or poll
`operation get`, which every surface serves.

The terminal's watch is short calls on the control socket, so it never
holds the daemon: other callers are served while it streams. A call
that reaches the daemon asking for the stream itself, without
those steps, is refused at once.

## Events on the daemon's bus

The daemon republishes the events uma commits on an in-process bus, the
seam for hooks and notifications that run beside it (heo ships no hook
runner yet; what a subscriber sees is fixed here). This is the bridge, the
`bridge` runtime service (see [runtime](runtime.md#event-bridge)). It is
on by default and costs one durable subscription, `heo-bridge`, until
something subscribes.

The bus is a convenience, not a log. It is in memory, best-effort and
not durable, and it is **not an outbox**: nothing is retried, queued or
remembered for a subscriber that was not listening, or fell behind.
A hook that must see every event exactly once reads uma's log itself,
with `heo event watch` (a checkpoint of its own, acknowledged after each
batch) or `heo event list`, and deduplicates by event ID.

### Topics

A topic is `[Source].[Category].[Object].[Action]`, four lowercase
segments. A domain event goes out under

```text
uma.<public|desk>.<object>.<action>
```

The category is the audience. An event whose audience is public goes
out under `uma.public`; every other event, internal notes, labels,
contacts, and any audience heo does not know, goes out under
`uma.desk`. The object and action are the kind's: `message.created` is
`uma.public.message.created` (or `uma.desk.message.created` for an
internal note), `conversation.label_added` is
`uma.desk.conversation.label_added`, and `interaction.received` is
`uma.public.interaction.received` for an interaction the provider reports
as public (the web widget's are); the conversation workflow events
(`conversation.assigned`, `unassigned`, `snoozed`, `unsnoozed`, `linked`,
`unlinked`) are internal, so they go out under `uma.desk` too. Marking a
conversation read or unread, and a snooze ending, emit no event. A kind that is not an object and
action pair, such as a namespaced extension kind, goes out as
`extension.emitted`, with its kind in the payload. Patterns follow kit's
bus: `*` matches one segment, `#` the trailing segments.

The bridge's own health goes out apart from the domain events, under
`heo.runtime.bridge.<action>`: `started`, `degraded`, `recovered` and
`stopped`, each with the consumer, its state and, when degraded, the
error's code and never its text. The daemon's lifecycle events
(`kit.serve.service.started`, ...) ride the same bus.

### Who receives what

A subscriber has a scope. A **public** scope receives `uma.public.*.*`
events and nothing else, whatever pattern it subscribes with: a hook
that subscribes to `#` still receives no internal note, label, health
or lifecycle event. A **desk** scope receives everything. A subscriber
is public unless it is given the desk scope.

### What an event carries

References, never content. The payload is:

```json
{
  "event": "0190f3c2-7a1e-7c11-b2d0-3f6a9c1e4b55",
  "kind": "message.created",
  "tenant": "default",
  "inbox": "0190f3c2-...",
  "resource": {"kind": "message", "id": "0190f3c2-...", "inbox": "0190f3c2-..."},
  "conversation": "0190f3c2-...",
  "message": "0190f3c2-...",
  "operation": "0190f3c2-...",
  "revision": "...",
  "occurred_at": "2026-10-12T09:00:00Z",
  "recorded_at": "2026-10-12T09:00:00Z"
}
```

`label` is set for label events. An interaction event names the
conversation, and `interaction.received` the message the control sits on
when uma resolved one; `interaction.updated` (a response given, or the
interaction redacted) names the conversation and the operation. Neither
names who acted, the command or action, what was submitted or the
provider's response token. A workflow event names the conversation
and the operation, never who was assigned, until when, or the far end of a
link. Read what changed through heo, which
authorizes the read for the reader: `heo event get <event>`, `heo message
get <message>`. A message body, an attachment's name or bytes, a
contact's name or attributes and a label's name are never on the bus.

### Delivery

The bridge reads uma's log as the durable subscription `heo-bridge`,
as an actor of its own that only reads (see
[authorization](authorization.md#the-event-bridges-actor)), and
acknowledges a batch only after every event in it was published. A
restart resumes at the checkpoint with no loss; a crash between
publishing and acknowledging repeats that batch, so delivery to the bus
is at least once. The first start reads from the tail: events committed
before the bridge first ran are not replayed.

A subscriber that cannot keep up, or never returns, loses its own
events once its queue (256 events) is full, and the loss is counted
(`dropped` in `service status`). It never slows the daemon, the bridge
or another subscriber. The bridge itself falls behind only when uma
does not answer; it then reports itself degraded and retries with a
growing delay, and the daemon stays ready.

`service status` shows the bridge's last published event and when uma
recorded it. The distance to the log head is not shown, since positions
in uma's log are opaque.

An operator who sets kit's `KIT_BUS_SINK=jsonl` and `KIT_BUS_SINK_PATH`
makes every event on the bus, desk events included, also go to that
file. It is a file the operator owns, not a subscriber; it holds
references only.

## Held operations

With `held_operations.enabled: true`, a write by a `plugin:` actor is
not queued: uma stores it held, with its `--dry-run` preview, and the
command returns the operation's ID with state `held`. A person's own
writes are never held. Nothing is applied, and no delivery exists,
until a human decides.

A held operation expires after `held_operations.expiry` with no
decision: uma's sweep, which the daemon's workers run every
`services.workers.held_sweep_every` (`0s`, the default, is uma's own
cadence of one minute; `off` runs none), moves it to `expired`. An
expired operation counts neither for nor against trust, and no longer
blocks a new proposal.

While an operation of the same kind on the same target is held, another
proposal for it is not stored: the proposer gets that operation back,
state `held` with `already_held: true`. A proposer that may not read the
held operation is refused with CONFLICT, "already held", and told
nothing of it.

Every held operation carries a trust key built from the proposer, the
actor it acts for (`held_operations.on_behalf_of`), the mailbox (the
inbox), the verb and the target class, for example
`plugin:triage/uid:1001/support/label:Finance`. The default policy holds
every plugin write.

Holding needs two things of a verb. It must support `--dry-run`: the
preview is stored with the operation, so a plugin actor's verb without
one is refused. And uma must be able to hold it: that is
`message reply`, `message send`, `message edit`,
`message delete`, `reaction add` or `remove`, `note add`,
`conversation label`, `conversation tag`, `message reply-draft`,
`conversation unsubscribe`, the state verbs such as
`conversation close`, the provider state verbs (`conversation archive`,
`unarchive`, `move`, `read`, `unread`, `flag`, `unflag`, `trash`, `untrash`)
and `label create`. Any other write is refused for
a plugin actor with UNSUPPORTED, "verb cannot be held", exit 2: it is
never applied unreviewed.

### Trust ladder

With `held_operations.trust.enabled: true` the ladder replaces holding
everything. uma counts the consecutive clean approvals under each trust
key; heo only reads the count. A class is automatic once its count
reaches its threshold (`held_operations.trust.thresholds`); a reject
or an edit sends the key back to zero, so it is held again.

`held_operations.trust.never_auto` lists classes that never turn
automatic whatever the count (`trash`, `spam`): they stay at asking in
batches. Held whatever the trust: a mailbox in `held_operations.trust.approval_always`,
a reply that adds recipients, a first reply to a contact never written to
on the inbox (`held_operations.trust.inboxes.<inbox>.first_reply: allow`
lifts it for that inbox), a reply whose Reply-To differs from its
From, a send while `held_operations.trust.unattended` is `true`, and an
operation carried over from an earlier batch.

`held_operations.trust.seed` lists `read`, `unread` and `archive` classes
that are automatic for a person and mailbox from the first write, until
a correction is recorded under them. No other class can be seeded.
`held_operations.trust.supervised` is an opt-in first week that holds
every class, seeded or trusted, for 7 days from the person's start on
the mailbox.

Holding publishes `heo.operation.held.created` on the daemon's bus, desk
scope only, with the operation, its kind and target, the proposer and
the trust key, and no content.

### Audit trail

`operation held history [<operation>]` shows who proposed, edited,
approved, rejected or let expire each held operation, when, the preview
the decider was shown, and for an edit the arguments before and after.
It is read from uma's held operation and its events, never kept by heo,
so a reader sees only what uma shows them. `--format jsonl` writes one
record per line. Records use kit's consent audit vocabulary:
`capability` (the operation kind), `decision` (`allow` or `deny`) and,
for an expiry, `reason: expired`.

### List trust

`heo operation trust list --inbox <inbox>` lists the trust keys uma counts
for that inbox's channel, in key order, a page at a time (`--limit`,
`--cursor`). Each row shows the parts of the key (proposer, on-behalf-of
actor, mailbox, verb, class), the clean count, the last correction and
whether the ladder runs the key automatically under the current
configuration, seeded classes and always-held rules included.
`--label name=value` narrows it; `--format json` adds the totals, the
`threshold`, the `level` (`ask_each`, `ask_in_batches`, `auto`,
`never_auto`) and, for a held key, why. A person changes a count by hand
with `operation trust set`
([approvals.md](approvals.md#promote-and-demote-by-hand)). It reads only; the desk and operator roles may run it.

### Import trust

`heo operation trust import --file <path>` seeds uma's trust counters
with the approvals a deployment earned elsewhere, so a mailbox's plugins
keep the trust they had. `--file -` reads standard input. The file is
JSON with no message data, subjects or addresses; a field outside the
schema is refused.

```json
{
  "version": 1,
  "entries": [
    {
      "channel": "support",
      "proposer": "plugin:triage",
      "on_behalf_of": "uid:1001",
      "verb": "state",
      "class": "archive",
      "clean": 12,
      "last_correction_at": "2026-10-02T09:30:00Z"
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `version` | required: `1` |
| `channel` | required: the inbox key or ID; the key is built with the inbox's ID, as a write builds it |
| `proposer` | required: the `plugin:` actor that proposed the writes; only plugin actors are held, so only they earn trust |
| `on_behalf_of` | optional: the actor the proposer acted for |
| `verb`, `class` | `verb` required; `class` as the verb takes it, the verb and class `trust list` shows: `reply:send`, `note:local` or `note:provider`, `state:<state>` (the provider state verbs too: `state:archive`, `state:star`), `move:<class>`, `status:<status>`, `fields:<keys>`, `label:<name>` and `unlabel:<name>`, `tag:<name>`, `start` (no class), `edit`, `delete` and `react` on `message` |
| `clean` | required: consecutive clean approvals since the last correction, zero or more |
| `last_correction_at` | optional: RFC 3339 time of the latest correction; it counts as one correction |

The five key parts are the ones heo builds a key from, so an entry names
exactly the key a write would count under. A field outside the schema, a
`version` other than `1`, an unknown inbox, a verb heo does not hold, a
class the verb does not take, a proposer that is not a `plugin:` actor
and two entries for one key are refused. `label-create` has no mailbox
in its key and the ladder never reads its counters, so it is refused. So is
`unsubscribe`: it is irreversible and always held, whatever the count.

Every entry is checked before any is written: one refusal refuses the
file and nothing is written (exit 2, the report printed first). uma
merges and never replaces. Each count becomes the larger of the stored
and the imported one and the last correction the later, so importing the
same file again changes nothing (`unchanged`) and an import never lowers
what live decisions earned. The consecutive clean count is raised only
while no stored correction is later than the file's: a later correction
ended the streak the file describes. uma writes key by key, not as one
unit: a write that fails part way (reported `failed`, the rest
`not_written`) leaves the earlier keys imported; run the import again.

`--dry-run` reads the stored counters and writes nothing: it shows each
key as the import would leave it and which keys would become automatic
under the current trust configuration (thresholds, seed, always-held
mailboxes, supervised week). The result gives the entries `imported`,
`unchanged`, `refused` and `failed`, and the number that become
automatic; `--format json` has the per-entry key, status, reason, counts
and the ladder's answer. Importing is the `operator` role's
(`trust.write`); the `desk` role cannot, and a `plugin:` actor never can,
whatever roles it holds, so a plugin does not seed its own trust.

On MCP, REST and gRPC the daemon cannot read your files: pass the JSON
object itself as the `file` argument (a flag over REST and gRPC, an
argument of the `operation.trust.import` tool over MCP). At the terminal,
`--file` also takes a path, `-` or the object.

### Approving from a chat conversation

A held operation can be announced to a conversation on one of the
daemon's own channels (Telegram, Slack, Teams) and decided from it:

```yaml
held_operations:
  chat:
    enabled: true
    conversations:
      - inbox: <inbox record ID>
        conversation: <conversation record ID>
        slack_thread_replies: false          # Slack only; see below
        members:
          - identity: <identity record ID>   # `heo identity list`
            actor: uid:1001                  # who it decides as
```

The `approval-chat` service of the daemon (`heo service status` shows it)
does the announcing and the listening. It reads uma's event log through a
durable subscription of its own, as the actor `svc:heo-approval-chat`,
which may read events, the messages of the configured conversations and
reply and edit there, and decides nothing. It runs nothing, and opens no
store, unless `held_operations.chat.enabled` is true.

`operation.held` posts the operation's ID, kind, target, proposer,
reason, expiry and preview, and the grammar below. `operation.decided`
and `operation.expired` edit that message to the outcome (who decided,
or expired) with no preview.

Grammar. A reply to the announcement, `approve [note]` or
`reject [note]`, decides that announcement's operation: the verb first,
case-insensitive. The announcement's reply link is the one the channel
reports and uma resolves: a Telegram reply, a Discord reply, a Teams
reply. A message that is not a reply to the announcement cannot decide by
the verb alone.

Slack has no quote reply: uma reports every reply in a thread as an
answer to the thread's root, so a reply to heo's own answer cannot be
told from one to the announcement. A Slack thread reply therefore does
not decide by the verb alone, unless the conversation sets
`slack_thread_replies: true`. `approve #<operation>` and reactions decide
either way. The setting changes nothing in other channels, whose quote
replies name the message they answer.

 `approve #<operation> [note]` or
`reject #<operation> [note]` anywhere in the conversation names the
operation itself; said as a reply to the announcement of another
operation it is ignored. A reaction on the announcement: 👍 or ✅
approves, 👎 or ❌ rejects. Everything else is conversation and is
ignored. Editing or deleting a message never decides: only a new message
is read.

The announcements' message references are kept in
`approval-chat/approval-chat.json` beside the plugins' directory in the
daemon's state directory (owner-only), with the IDs of the replies and
reactions already handled. A restarted daemon therefore edits an
announcement it posted before, does not announce an operation again, and
still reads a bare reply to the old announcement. When the operation is
decided or expires the reference is kept, marked closed with the outcome
and the time, for 7 days (and dropped on the next save or load after
that): a reply to such an announcement is logged (`already decided` or
`expired`, with the operation and the message ID), posts nothing and
decides nothing. A state file written without closed references loads as
open ones. Without a state directory the references live in memory only.

The approver is the actor the sender's uma identity is mapped to in
`members`, never the display name. The decision runs as that actor
through the same path as `operation held approve|reject`, so heo's
authorization, uma's rules and the plugin guard apply: a mapped actor
without the right is told so, a `plugin:` actor in `members` is refused
by configuration and again at decision time, and an operation already
decided or expired is answered politely and left alone. A sender with no
mapping is ignored and logged by identity ID. A reply or reaction the
provider delivers twice is decided once, keyed by the message's own ID;
a reply to an announcement whose operation is closed is logged and
ignored. The
service's own messages, and those of system actors, never decide, and
`held_operations.chat` refuses a member mapped to `svc:heo-approval-chat`.

Trust boundary. Configuring a conversation grants its mapped members
visibility of what the announcement shows. The preview, target,
proposer and reason are included only when every mapped member of that
conversation may read the operation; if any may not, the announcement
carries the operation ID alone, and if none may, nothing is posted.
Members of the conversation that are not in `members` cannot be
checked and see the announcement: map everyone in an approval
conversation, or keep it private.

## Who may do what

| Command | Role |
| --- | --- |
| `operation held history` | `desk`, `operator`; the records of operations you may read |
| `operation list`, `operation get`, `operation watch` | `desk`, `operator`; `viewer` and `external` have no operation reads |
| `delivery list`, `delivery get`, `delivery reconcile` | `desk`, `operator` (reconcile spends a provider call: `operation.reconcile`) |
| `delivery retry` | `operator` |
| `event list`, `event get` | `desk`; `viewer` and `external` for public events |
| `event watch` | `desk`; `viewer` and `external` for public events |
| the bus bridge | none: it runs in the daemon as its own actor, not as a caller |

See [authorization.md](authorization.md).

## Hook plugins

A plugin is an executable heo runs for you: it receives events and returns
operations. The runner delivers events when `services.hooks.enabled` is set
(see [runtime](runtime.md#hook-runner)). The contract below is what the
runner holds to.

- **Identity.** A plugin acts as `plugin:<name>`, holding only the roles
  the configuration grants that actor (see
  [authorization](authorization.md)). It is not the runtime's or the
  bridge's actor.
- **Threat model.** A plugin runs as the daemon's OS user. What confines it
  is what its actor may do through heo, not the operating system. To run a
  plugin under another uid or in a container, do so in the deployment
  (the configured command can be `docker run ...` or `sudo -u ...`); heo
  does not enforce it.
- **Delivery.** Each plugin reads uma's log as a durable subscription of
  its own, beside `heo-bridge`, acknowledged only after its result was
  applied or recorded as failed. A new plugin's subscription starts at the tail,
  like the bridge. Delivery is at least once; the event ID
  repeats on a redelivery and `attempt` counts it. Applying a result is
  exactly once: every operation carries an idempotency key, the plugin's
  own `idempotency_key` or, by default, one derived from the plugin, the
  event ID and a hash of the operation's content (verb, args and flags as
  canonical JSON).
- **Envelope.** One JSON object per event, references only: the event's
  ID, kind, inbox, resource and the conversation, message, label or
  operation it names, never content. Schema:
  [hook-envelope.schema.json](../hook-envelope.schema.json). The kinds a
  plugin may subscribe to, their audience scopes and the consumer naming
  rule are in [hooks](hooks.md).
- **Result.** A JSON object with an ordered `operations` list in the
  command vocabulary (`verb`, `args`, `flags`, `idempotency_key`). Empty
  stdout means no operations. Schema:
  [hook-result.schema.json](../hook-result.schema.json).
  Operations run in order as the plugin's actor and stop at the first
  failure. Earlier ones stay applied; the index of the failing one is
  recorded with the delivery; the retry re-sends the event, and the applied
  prefix replays as no-ops under its keys.

  An operation is a request to the daemon's command surface for one verb:
  `verb` is the command path (`note add`), `args` its positional arguments
  and `flags` its flags by long name (`{"text": "hi", "placement":
  "local"}`), exactly as a REST call names them. It runs through the same
  code as the CLI verb, as `plugin:<name>`, so what the plugin's roles allow
  is decided by the policy for every uma call. A result that fails the schema,
  or names a verb a plugin may not run, is **rejected whole**: nothing is
  applied and the delivery is retried like any failed one. A plugin may not
  run a verb that does not exist, that runs at the terminal only, that hosts
  itself (`plugin token mint`), that is destructive (no served surface runs
  those), that acts on the daemon's machine (`storage backup`) or that cannot
  be replayed; nor set `--idempotency-key`, `--dry-run` or `--confirm`, nor
  a flag the verb lacks. An authorization or usage refusal of one operation
  is permanent: the event is dead-lettered at once.
  Exactly once rests on uma: a verb that commits a uma operation takes the
  key as its operation ID, so uma returns the stored operation for a repeat
  of the same content and refuses the same key with other content.
  `plugins[].dry_run: true` records each operation, key included, in the
  daemon log and applies none.
- **Process modes.** *Exec*: a new process per event, envelope on stdin,
  result on stdout. *Subscriber*: one long-running process fed one
  envelope per line, answering one result per line carrying the same
  `event_id`, for runtimes with a costly start-up.
- **Exit codes** (exec mode; in subscriber mode they apply when the process
  exits). `0` ok, apply the result. `1` retryable failure: nothing is
  applied, the event is redelivered with jittered backoff. `2` permanent
  failure. Any other code, or a signal, is a crash and retryable.
- **Dead letters.** Exit 2, or running out of the bounded retry policy
  (maximum attempts with backoff), writes a dead-letter record that can be
  listed and discarded (`heo hook dead-letter`, [hooks.md](hooks.md#operating-the-runner)), and the plugin's stream moves on to the next
  event, as deliveries do. The record names the failure class and the
  operation that failed; the bus reports it as
  `heo.runtime.hooks.dead_lettered`.

