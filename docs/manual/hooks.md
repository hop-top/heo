# Hook plugins: events a plugin may subscribe to

A plugin reacts to events; this page lists which. The runner, the
envelope and the result are specified in
[Hook plugins](deliveries-and-events.md#hook-plugins). The runner
delivers events to plugins when `services.hooks.enabled` is set (see
[runtime](runtime.md#hook-runner)) and applies the operations a plugin's
result names ([applying a result](deliveries-and-events.md#hook-plugins)).

## What a plugin receives

An event is a reference to a committed change: its ID, kind, tenant,
inbox, the resource it concerns, and the conversation, message, label or
operation it names. **Never a message body**, a contact's name or
attributes, a label's name, who was assigned or the far end of a link.
Those are content: the plugin reads them through the normal read verbs
(`heo message get`, `heo conversation get`, `heo interaction get`, and so
on) as its own actor, so redaction, audience and the plugin's roles apply
at the read and not to a copy in the envelope. The wire shape is
[hook-envelope.schema.json](../hook-envelope.schema.json); its `event`
object has exactly the fields below and no others.

| Field | Is |
| --- | --- |
| `event`, `kind`, `tenant`, `inbox` | the event's ID and kind; where it happened |
| `resource` | `kind`, `id`, `inbox` of what the event is about |
| `conversation`, `message`, `label`, `operation` | the references the kind carries (the catalog says which), when the event's payload names them |
| `revision` | the resource's revision after the change |
| `occurred_at`, `recorded_at` | when it happened; when uma recorded it |

## What a plugin process gets, and what is isolated

Isolation is authorization, not the OS. A plugin runs as the daemon's OS
user; what confines it is what its actor, `plugin:<name>`, may do through
heo. The runner hardens the process it starts, in exec and stream mode
alike:

| | |
| --- | --- |
| Environment | scrubbed: `PATH` and the three variables below. The daemon's environment never reaches it, so no credential reference and not the profile's surface token |
| Token | only its own `plugin:<name>` token, from `heo plugin token mint <name>`, read from the credential backend at each start (a re-minted token is picked up by the next exec event or stream restart) |
| Working directory | its own, `<state dir>/heo/<profile>/plugins/<name>`, created `0700` |
| stdout | the result only; larger than 1 MiB (one line, in stream mode) is malformed output: a failed attempt, retried, and a stream process is killed |
| stderr | to the daemon's log, a line at a time (4 KiB per line, the rest dropped), under the plugin's name; the last 2 KiB are kept in a failure's reason |

| Variable | Is |
| --- | --- |
| `HEO_PLUGIN_NAME` | the plugin's configured name |
| `HEO_PLUGIN_TOKEN` | its bearer token, `<name>.<secret>`: send it as `Authorization: Bearer` |
| `HEO_URL` | base URL of the REST surface (`services.api.addr`), for read verbs; absent when `services.api` is not enabled |

The token acts as `plugin:<name>` on REST, gRPC and MCP and as no other
actor; the control socket never admits it. What it may do is the roles
`authorization.grants` gives that actor: grant `observer` (and `desk` for
desk events) for a plugin that only reads, and a verb outside the grants
is refused UNAUTHORIZED. A plugin with no grant holds no role.

**Socket plugins.** A plugin with `socket` instead of `command` is a
long-running server the operator starts. heo connects as a client and
speaks the stream framing: one envelope per line, one result line back
carrying the envelope's `event_id`. heo starts nothing and passes no
environment, so there is no `HEO_PLUGIN_TOKEN`: the operator gives the
server its token (`heo plugin token mint <name>`) when starting it.
Socket mode is one ordered connection, concurrency 1 like stream.

| Situation | Outcome |
| --- | --- |
| socket absent, refused or closed | failed attempt, retried under the plugin's backoff; heo reconnects at each attempt; after the last attempt the event is dead-lettered |
| connect | 5 s limit |
| no answer within `timeout` | failed attempt; the connection is dropped and re-made |
| result line over 1 MiB | malformed output: failed attempt; the connection is dropped |
| server restarts mid-stream | the event in flight fails and is retried; the next attempt reconnects |
| path is not a unix socket, is owned by another uid than the daemon's, or is writable by all users | refused before connecting, a failed attempt: another user could stand in for the plugin and read its events |

**Operations run under REST's controls.** The operations a plugin's
result names run through the same bridge a REST request does, with the
options `service start api` installs, resolved from `services.api.*` and
the `--policy` the daemon started under: the policy and its permission
rules decide each operation, audit sinks (`services.api.audit.sinks`)
record it with the actor `plugin:<name>`, and the rate limit
(`services.api.rate_limit`) bounds it. The limit keeps one bucket per
tier per actor, so a plugin cannot spend another's allowance, and a
throttled operation is retried later like any transient failure. A
refusal by the policy is permanent: the operation is not retried. The
limit's default follows `services.api.addr`: on when it binds beyond
loopback, off otherwise, and off while `services.api` is disabled.

**No token minted.** The hooks service refuses to start that plugin: its
scope in `service status` is `failed`, naming
`heo plugin token mint <name>`, and its subscription is not opened, so no
event is consumed or dead-lettered. Mint the token and restart the hooks
service. This is the same refusal a plugin that cannot be built gets; the
other plugins run. A token revoked while the daemon runs fails each later
start of that plugin's process (retryable) until a new one is minted.

**Not isolated.** The process can read any file the daemon's OS user can,
use the network, and run any program on its `PATH`; a plugin holding no
token still runs. Nothing enforces a uid, a container or a sandbox:
running a plugin under another uid or in a container is deployment
guidance you apply, not something heo checks. Treat a plugin's
executable as trusted code with the daemon's file access, bounded
through heo only by its grants.

## Audience scopes

A plugin sees an event only when its actor may read the event's audience,
the rule every other subscriber follows (`heo event watch`, the bridge):
an event is **public** only when uma committed it for the public audience;
every other audience (internal notes, restricted content, an absent or
unknown one) is **desk** and goes to an actor holding the `desk` role.
Each event is checked against the plugin's actor again before it is
delivered, and one it may not read is skipped as if absent. Grant a
plugin only the roles it needs: a plugin without `desk` never sees a desk
event. `observer` is the role for a plugin that only observes: the
reads the bridge holds, so it receives events about channels and
operations too (`viewer` does not); add `desk` for desk events and to act.

The catalog's scope column says which audience a kind has:

- `desk`: always desk, whatever the record.
- `record`: the audience of the record the event is about. Public for a
  public message and desk for an internal note, for example.

## Catalog

Every builtin kind uma commits is listed. A kind not listed here, or a
kind an extension registers, is not offered to plugins.

<!-- catalog:start -->
| Kind | Scope | References | When |
| --- | --- | --- | --- |
| `message.received` | record | `conversation`, `message` | a provider delivered an inbound message |
| `message.created` | record | `conversation`, `message`, `operation` | a message was authored through uma: a reply, a note, a conversation start |
| `message.updated` | record | `conversation`, `message`, `operation` | a message changed, an edit or a redaction say |
| `conversation.changed` | record | `conversation`, `operation` | a conversation's state or membership changed |
| `conversation.label_added` | desk | `conversation`, `label`, `operation` | a label was added to a conversation |
| `conversation.label_removed` | desk | `conversation`, `label`, `operation` | a label was removed from a conversation |
| `conversation.assigned` | desk | `conversation`, `operation` | an assignee was set or replaced |
| `conversation.unassigned` | desk | `conversation`, `operation` | an assignee was cleared |
| `conversation.snoozed` | desk | `conversation`, `operation` | a snooze was set |
| `conversation.unsnoozed` | desk | `conversation`, `operation` | a snooze was ended early |
| `conversation.linked` | desk | `conversation`, `operation` | a link was added, once per conversation end |
| `conversation.unlinked` | desk | `conversation`, `operation` | a link was removed, once per conversation end |
| `delivery.updated` | record | `operation` | an outbound write changed state |
| `interaction.received` | record | `conversation`, `message` | a provider reported a button press, command or form |
| `interaction.updated` | record | `conversation`, `operation` | an interaction's response state changed, or it was redacted |
| `work_item.updated` | record | `conversation`, `operation` | a ticket or issue changed |
| `task.updated` | desk | `conversation`, `message`, `operation` | an agent task opened or changed |
| `operation.held` | desk | `operation` | a plugin's operation was held for a human decision |
| `operation.decided` | desk | `operation` | a held operation was approved, rejected or edited |
| `operation.expired` | desk | `operation` | a held operation's deadline passed with no decision |
| `identity.linked` | desk | - | an identity was linked to a contact |
| `contact.created` | desk | - | a contact was created |
| `contact.updated` | desk | - | a contact changed |
| `contact.merged` | desk | - | two contacts were merged |
<!-- catalog:end -->

The operation events (`operation.held`, `operation.decided`,
`operation.expired`) name the operation and nothing else: not its
preview, its arguments, who proposed it or who decided. A plugin
receives one only while it holds `desk` and `observer` (the operation
read, internal audience) and its actor may read the operation's target;
for an operation held for a conversation it may not read, the event is
withheld as if absent. A plugin is never told that its own operation was
held, so it cannot answer a proposal by proposing again without a human
in between; the decision and the expiry of its own operation are
delivered.

`message.created` is a message authored through uma; an inbound provider
message is `message.received`. A kind with `-` names nothing but its
resource: the plugin reads the record through its own verb, for example
`heo contact get`.

A test fails when uma gains an event kind with no row here, when a row
names a kind uma lacks, or when a row's references differ from what the
bridge fills for that kind.

## Subscription naming

A plugin's subscription is a durable uma subscription, and uma fixes its
filter when the subscription is opened: reopening a consumer by name
returns the open one with its filter unchanged. So the consumer name is
derived from the plugin and its filter, and the filter cannot drift under
a name:

```text
plugin-<name>-<hash>
```

- `<name>` is the plugin name (`^[a-z][a-z0-9-]{0,62}$`).
- `<hash>` is the first 12 hex digits of the SHA-256 of the whole filter
  as canonical JSON: keys in alphabetical order, each list sorted and
  de-duplicated, an empty list as `[]`, for example
  `{"channels":[],"inboxes":["sales","support"],"kinds":["message.created","message.received"]}`.
  Every list counts, so a second inbox is a different subscription.
- Names that do not start with `plugin-` stay the runtime's and the
  CLI's: `heo-bridge`, `heo-cli` and a `--consumer` you choose.

uma's event log honours the kinds and the inboxes, any number of the
latter, so the runner opens the subscription with the kinds and the
inboxes resolved to their IDs and uma skips the rest. It does not honour
channels: `filter.channels` is checked by the runner, which acknowledges
an event of another channel at once, without delivering it.

Changing a plugin's filter therefore opens a **new** consumer. A filter
that is unchanged resumes the open one, so a restart or a reordered kind
list loses nothing. uma lists no subscriptions, so the daemon keeps, per
plugin, the consumers it opened and has not closed, each with the filter
uma holds for it, in `lineage.json` beside the dead letters. On a change
the runner

1. records the new consumer;
2. reopens the previous one, which keeps its position, to read its
   checkpoint, and opens the new one at that checkpoint;
3. records the previous one as retiring;
4. closes it, then clears the record.

No event between the two is skipped. The events between the old
checkpoint and what the old consumer had delivered but not yet
acknowledged are delivered again, and a crash at any step is finished at
the next start: a consumer opened but not yet recorded as handed over
keeps its position, and one whose close did not happen is closed then.
A widened filter does not replay earlier events of the kinds it added.
When the old consumer's checkpoint is empty, or without a store that
keeps the record (`hooks.Store` that is not a `FileStore`), the new
consumer opens at the tail. An empty filter hashes as
`{"channels":[],"inboxes":[],"kinds":[]}`.

## Operating the runner

The `hook` verbs run in the daemon, where the plugins' processes, tokens
and actors are, and need the `operator` role (`heo.hook.read` for the
reads, `heo.hook.manage` for retry, replay and drop). Each takes `--format json`.

| Verb | Does |
| --- | --- |
| `heo hook list` | each configured plugin: mode, runner state (`not_running` when the service holds none), `dry_run`, kinds, cursor, lag, delivered and dead-lettered counts, dead letters waiting |
| `heo hook test <plugin> --event <id>` | run the running plugin on one stored event and print its result; applies nothing |
| `heo hook dead-letter list [--plugin P] [--state dead\|replayed\|discarded\|all]` | the events plugins did not handle, oldest first |
| `heo hook dead-letter retry <plugin> <event>` | queue running a dead letter through its plugin again, under the keys of the first run |
| `heo hook replay <plugin> --from <position\|beginning> [--limit N]` | queue delivering the log to the plugin again from a position |
| `heo hook job get <id>` | the state and outcome of a queued retry or replay |
| `heo hook dead-letter drop <plugin> <event>` | give up on a dead letter: it stays listed as `discarded` and is not retried; nothing is run |

`hook test` reads the event as the plugin's own actor, so its audience
rule and filter apply: an event the plugin would not be delivered is
reported `skipped` and the plugin is not run. It runs the plugin once, as
attempt 1, then lists each operation of the result with the key it would
run under and whether the check made before anything runs would refuse it
(`allowed`, `refusal`); one refusal rejects the whole result
(`would_apply: false`), as in a delivery. Nothing is dispatched, no dead
letter or attempt count is written and the subscription does not move
(`applied` is always `false`). The check does not decide authorization:
that happens when an operation runs, as `plugin:<name>`. The exit code is
0 only when delivering the event would succeed. The plugin's own process
is the only thing that runs: one that acts outside heo acts here too.

`retry` and `replay` are **queued, not run in the command**: they answer at
once with a job (`queued`) and the daemon runs it on its own goroutine,
one job per plugin at a time. The operations a plugin returns run through
the command tree, which the running command holds, so running them inside
it would wait on itself. `hook job get <id>` shows the outcome (a retry's
letter, a replay's scanned, delivered, skipped and failed counts and the
`next` position), as do `hook dead-letter list --state all` and the hooks
section of `service status`. Jobs live in memory, the last 200, and end
with the daemon. `--dry-run` queues nothing: retry shows the plan, replay
lists the events it would deliver.

`replay` is a bounded walk of uma's event log from `--from`, a position
(the `next` of `heo event list`, or a plugin's cursor in `hook list`) or
`beginning`, read as the plugin's actor with its filter. It does not move
the plugin's subscription, which uma cannot seek: that keeps delivering
new events, without ordering against the replay. Each event is delivered
once under the keys of any earlier delivery, so operations already applied
are no-ops; a failure is recorded as a dead letter and the walk goes on.

`hook dead-letter drop` is destructive: it asks for confirmation on a
terminal and needs `--confirm=yes` without one, and REST, gRPC and MCP do
not serve it until the operator names the surface; the control socket
does. `--dry-run` previews it. `drop` is also destructive and acts on the
daemon's machine, so a plugin's result may never name it; the other `hook`
verbs need the `operator` role, which a plugin's actor holds only if
`authorization.grants` gives it.

## Write a plugin in any language

A plugin is any executable that reads envelopes on stdin and writes
results on stdout. Nothing ties it to a language or a library: read a
line of JSON, answer a line of JSON. The contract is in
[Hook plugins](deliveries-and-events.md#hook-plugins); the shapes are
[hook-envelope.schema.json](../hook-envelope.schema.json) and
[hook-result.schema.json](../hook-result.schema.json).

This plugin, in POSIX shell, labels the conversation of each message it
is delivered and leaves a local note on it. It works in both process
modes: exec sends one envelope and closes stdin, stream keeps sending
one per line, and the loop answers each with one result line carrying
the envelope's `event_id`.

<!-- example:plugin.sh -->
```sh
#!/bin/sh
# triage: label the conversation of an event and leave a local note.
# HEO_PLUGIN_NAME is set by the runner.
while IFS= read -r line || [ -n "$line" ]; do
	event=$(printf '%s' "$line" | sed -n 's/.*"event_id":"\([^"]*\)".*/\1/p')
	conv=$(printf '%s' "$line" | sed -n 's/.*"conversation":"\([^"]*\)".*/\1/p')
	if [ -z "$conv" ]; then
		printf '{"event_id":"%s","operations":[]}\n' "$event"
		continue
	fi
	printf '{"event_id":"%s","operations":[' "$event"
	printf '{"verb":"conversation label","args":["%s"],"flags":{"add":"seen-%s"}},' "$conv" "$HEO_PLUGIN_NAME"
	printf '{"verb":"note add","args":["%s"],"flags":{"text":"triaged by %s","placement":"local"}}' "$conv" "$HEO_PLUGIN_NAME"
	printf ']}\n'
done
```

Wire it up. The label is created once by an operator, the plugin
gets the roles its operations need, and the runner needs its token:

```yaml
services:
  hooks:
    enabled: true
plugins:
  - name: triage
    command: /usr/local/bin/triage.sh
    mode: exec              # or stream: one long-running process
    filter:
      kinds: [message.received]
authorization:
  grants:
    - actor: plugin:triage
      roles: [observer, desk]
```

```sh
heo label create seen-triage
heo plugin token mint triage
heo service start
```

Exit codes follow [the contract](deliveries-and-events.md#hook-plugins):
`0` applies the result, `1` is retried, `2` is permanent. Operations
are idempotent under keys derived from the event, so a redelivery, a
`heo hook replay` or a `heo hook dead-letter retry` applies each
operation once. Try a plugin on a stored event before enabling it with
`heo hook test triage --event <id>`; nothing is applied.

Operations are verbs of the command tree, so a plugin does what the CLI
does: labels (`conversation label`), local notes and replies
(`note add`, `message reply`). Provider-side drafts are not an
operation yet. Run a plugin in another language the same way: the
parsing is the only part that changes.
