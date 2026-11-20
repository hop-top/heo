# Approvals: holding plugin writes for a person's decision

A plugin actor (`plugin:<name>`, an automation acting on your behalf) can
be made to propose its writes instead of applying them. uma stores the
write held, with a preview of what it would do, and applies nothing until
a person approves it. This page is the whole story for an operator:
turn it on, tune how much trust a plugin earns, decide, audit.

People are never held: a person's own write runs as it always did.

## Enable

```yaml
held_operations:
  enabled: true
  expiry: 72h                    # how long a proposal waits for a decision
  on_behalf_of: uid:1001         # the person the plugins act for
```

Changes take effect when the daemon starts. With `enabled: false`, the
default, nothing is held and a plugin actor's writes run as any other
actor's. Every key: [configuration.md](configuration.md).

The plugin actor needs a role in `authorization.grants` to write at all
(see [authorization.md](authorization.md#held-operations)). The hold
point does not grant it anything: it only decides whether an allowed
write is applied now or proposed.

While holding is on, a plugin write is held when its verb can be
previewed (`--dry-run`) and uma can hold it. That is `message reply`,
`message send` (a conversation start), `message edit`, `message delete`,
`reaction add|remove`, `note add`, `conversation label`, `conversation
tag`, `message reply-draft`, `message forward`, `conversation unsubscribe`, `conversation` state verbs (`close`, `resolve`, `reopen` and the
others), the provider state verbs (`conversation archive`, `unarchive`, `move`, `read`, `unread`, `flag`, `unflag`,
`trash` and `untrash`), `conversation status` and `conversation update` of a provider-owned status or fields, `provider-label create`, and `label create`. A held write returns the operation's ID
with state `held`; nothing is written or delivered until it is approved,
and a reject or an expiry leaves nothing behind. A held `message send`
opens its conversation when approved, not before. `label create` and the
state verbs are held only because the daemon opts them into uma's
holding while `held_operations.enabled` is on.

An operation of the same kind on the same target already held is
returned as it is (`already_held: true`) instead of stored twice.

### What cannot be held

A plugin actor's write with no `--dry-run` preview, or one uma cannot
hold, is refused with `UNSUPPORTED`, "verb cannot be held", exit 2. It
is never applied unreviewed. Reads are never held. `conversation status`
and `conversation update` can be held only when the change goes to the
provider: a status or fields uma owns on the channel (`local_fields`)
commit in uma at once and uma has no hold for them, so they cannot be
held yet. The modal verbs have no hold path either.

Earned trust can be imported: `heo operation trust import --file trust.json`
([deliveries-and-events.md](deliveries-and-events.md#import-trust) has the
file format).

## Trust ladder

By default every plugin write is held. The ladder lets a class of write
become automatic once people have approved it often enough:

```yaml
held_operations:
  trust:
    enabled: true
    unattended: true             # no human present: a reply send is always held
    thresholds:
      label: 3
      "label:finance": 15
    seed: [read, unread, archive]
    never_auto: [trash, spam]
    approval_always: [<inbox ID>]
    supervised:
      enabled: true
      days: 7
```

uma counts consecutive clean approvals per trust key: proposer, the
actor it acts for, the mailbox (inbox), the verb and the target class,
for example `plugin:triage/uid:1001/support/label:Finance`. heo reads
the count; it never changes it. A class is automatic once its count
reaches its threshold. A reject or an edit sends the key back to zero.

The classes of the held verbs: `reply:send`, `note:local|provider`,
`start`, `label:<name>` (`conversation label` add and remove),
`tag:<name>` (counted under `label`), `state:<state>`,
`status:<status>` (`conversation status`: the work item's status, apart from the conversation's own `state`),
`fields:<keys>` (`conversation update`: the fields asked for, sorted and comma separated),
`label-create:<name>`, `provider-label:create` (`provider-label create`), `reply:draft` (`message reply-draft`: create,
update and delete alike), `forward` (`message forward`), `unsubscribe` (`conversation unsubscribe`) and the provider state verbs,
which are `state:<class>` or `move`: `archive`, `unarchive`, `read`, `unread`, `star` and `important` (`conversation flag`),
`unstar` and `unimportant` (`conversation unflag`), `trash`, `untrash`, and `move` (class: the role, the provider ID or the
label changes). A `move` into the trash role is `trash`, and one into the junk role is `spam`, so no threshold is
skirted by naming the folder; any other `move` counts as `move_to`. A class with no row below
(`unarchive`, `untrash`, `important`, `unimportant`) takes the default threshold.
A `message reply` or `message reply-draft` that names `--reply-all`,
`--cc` or `--bcc` adds recipients, so the "adds recipients" rule below
holds it every time, in its own class (`reply:send`, `reply:draft`). `label create` touches no mailbox, so uma has no mailbox
to count under: it is held every time.

| Class | Clean approvals before automatic |
| --- | --- |
| `read`, `unread`, `star`, `unstar`, `label`, `archive`, `move_to` | 3 |
| `reply:draft`, `trash`, `spam` | 5 |
| `reply:send` | 8 |
| `lane:unsolicited` | 5 |
| `lane:newsletter` | 3 |
| `attention` | 0 (always automatic) |
| `unsubscribe` | none: always held |
| any other class | 15 |

Classes of the chat, messaging and tracker verbs are named by the same
scheme, and heo ships no threshold of its own for them: each takes the
`any other class` fallback of 15 until a tenant sets it.

| Verb | Class | Notes |
| --- | --- | --- |
| `reaction add`, `reaction remove` | `react:message` | one class for both |
| `conversation archive` (Slack, Discord and the other providers' state verbs) | `archive` (`state:archive`) | default 3, as in the table above |
| `conversation status` | `status:<status>`, base `status` | `fields:<keys>` for `conversation update` |
| `message edit`, `message delete` | `edit:message`, `delete:message` | the plugin's own message |
| `message reply` | `reply:send`, per inbox | default 8; held while `unattended` |
| `note add` | `note:local`, `note:provider`, base `note` | |

`held_operations.trust.thresholds` is merged over these; an exact class
(`label:finance`) beats its base (`label`). `unsubscribe`
(`conversation unsubscribe`) is irreversible, so it never earns trust: a
plugin's unsubscribe is held every time, whatever the count. A
`thresholds.unsubscribe` entry is refused by `config validate` and at load,
`seed` cannot name it, and `operation trust import` refuses an `unsubscribe`
entry. `operation trust list` shows it as held with that reason, and a
batch approval skips it (see below): approve each one by itself.

### Classes that never turn automatic

`held_operations.trust.never_auto` lists classes capped at asking in
batches: a class there is held whatever its count, so a business can keep
`trash`, `spam` or `reply:send` under a person's eye while the rest of the
ladder earns trust. The count still grows and a batch approval still
decides the class (`held approve --class trash --inbox support`); only the
step to automatic is closed. An entry names an exact class (`trash`) or a
base (`label`, covering `label:finance`), as `thresholds` does.

- `operation trust import` keeps the counts of such a class and reports it
  as held, with its reason; the import never makes it automatic.
- `operation trust list` shows the reason: `never automatic: trash is
  capped at asking in batches`.
- `config validate`, and the daemon at load, refuse a `never_auto` that
  means nothing or contradicts the rest: an empty entry, `unsubscribe` (it
  is already always held), a class with threshold `0` (always automatic,
  such as `attention`), and a class that is also in `seed`.
- `unsubscribe` is unchanged: held every time, and a batch approval skips
  it; approve each one by itself.

Held whatever the count: a mailbox in `approval_always`, a first reply to
a contact never written to (unless the inbox says `allow`, see below), a reply that
adds recipients, a reply whose conversation's last inbound message names a Reply-To other
than its From (read from the headers the mail adapter recorded), a send
while `unattended` is `true`, and an operation carried over from an
earlier batch, and every `unsubscribe`.

### Thresholds for one inbox

A tenant may set a threshold for a single inbox, which beats the same
class in `thresholds` (an inbox's exact class, then its base, then the
ladder's exact class, then its base, then the fallback):

```yaml
held_operations:
  trust:
    inboxes:
      <inbox ID>:             # as in approval_always; case does not matter
        thresholds:
          "react:message": 1
        first_reply: hold      # hold (default) | allow
```

### A first reply to a contact never written to

A `reply:send` to a contact heo has never written to on that inbox is held
whatever the count, with the reason `first reply to a contact never
written to on this inbox`. heo computes the fact from its own records when
it plans the reply: the contact is the one behind the last inbound message
of the conversation (their identities on this inbox), and a write is any
message sent through uma in any conversation of the inbox that has one of
those identities as a participant, the conversation being answered
included. A conversation with no inbound message was opened from this
side, so its contact has been written to. A plugin cannot state the fact:
the verb offers none, and the plan sets it. What the caller cannot read
counts as not written, so the answer errs towards holding.

`first_reply: allow` on an inbox (a support or WhatsApp responder line,
say) drops this rule for that inbox only: the reply then follows its
`reply:send` threshold and every other rule (unattended, `approval_always`,
a cc, a Reply-To) like any other. It skips no approval by itself. A held
first reply carries its reason on the operation (`held show`); `trust list`
shows keys, which are not per contact, so it cannot show it. A batch
approval skips such an operation: decide each one.

### Example policy for a supervised personal inbox

An example, not a default: heo ships none of these values. A tenant that
wants reactions, archiving, statuses and its own edits to earn trust
quickly, and replies slowly, supplies them:

```yaml
held_operations:
  enabled: true
  trust:
    enabled: true
    unattended: false
    thresholds:
      "react:message": 3
      archive: 3
      status: 5
      "edit:message": 15
      "delete:message": 15
      "reply:send": 8
    inboxes:
      <chat inbox ID>:
        first_reply: hold
```

### A new person: seed and supervised week

`seed` lists classes that are automatic from the first write for a
person and mailbox with no correction on record. Only `read`, `unread`
and `archive` can be seeded; every other class earns its trust.

`supervised` is an opt-in first week: for `days` (default 7) from the
person's start on a mailbox, every class is held, seeded or trusted.
Record the start per person and mailbox:

```yaml
held_operations:
  trust:
    supervised:
      enabled: true
      started:
        "uid:1001/<inbox ID>": "2026-11-16T09:00:00Z"
```

A pair with no start is held. After the week the ladder applies, and a
seeded class is automatic unless a correction was recorded under it.

See what the ladder holds, per mailbox:

```sh
heo operation trust list --inbox support
```

Each row shows the key's parts, the clean count, the last correction and
whether the key runs automatically under the current configuration. With
`--format json` (and on REST and MCP) it also gives `threshold` (the
clean approvals the class needs; absent for `unsubscribe`), `level` and
`level_label`, and, for a held key, `reason`. The daemon derives the level
from the count and the configuration; a client never computes it:

| `level` | `level_label` | When |
| --- | --- | --- |
| `ask_each` | Ask each | count 0 |
| `ask_in_batches` | Ask in batches | count 1 up to the threshold |
| `auto` | Auto | count at or above the threshold, a seeded class with no correction, or threshold `0` |
| `never_auto` | Never auto | held whatever the count: a class in `never_auto`, `unsubscribe`, an `approval_always` mailbox, an unattended `reply:send` |

The supervised first week holds without moving the level.

### Promote and demote by hand

A person can set a key's count themselves, as the desk does to promote a
class that earned it or demote one that went wrong:

```sh
heo operation trust set <key> --inbox support --count 3 --note "earned elsewhere"
heo operation trust set <key> --inbox support --reset
heo operation trust set <key> --inbox support --count 3 --dry-run
```

`<key>` is the key `trust list` shows (`--key` over MCP, which cannot pass
a positional argument); `--inbox` names the mailbox whose channel counts
it. The key must already be counted, by a decision or an import.

- Desk or operator role. A plugin actor is refused, exit 5, REST `403`
  `heo.authz.plugin_decide`.
- It is neither an approval nor a correction: uma's approval and
  correction totals and the last correction stay as they are, so held
  history shows nothing. uma has no write that sets a count: a count above
  the stored one is an import of it, a count below is a reset and then an
  import. A count above the stored approvals raises that total to it.
- A class under `never_auto` takes a count below its threshold only (ask
  each, or in batches); at or above it is refused. `unsubscribe` takes
  none.
- It is audited as a decision is: the result names the actor, the time,
  the key, the count and level before and after and the note, the daemon
  logs it, and the surfaces' audit sinks record the call. uma keeps no
  event of a counter change.

## Decide

Everything runs in the profile's daemon. Deciding is for people with the
`desk` or `operator` role; a plugin actor never decides.

```sh
heo operation held list                       # held, newest change first
heo operation held show op_123                # proposer, reason, preview, revision
heo operation held approve op_123 --note "checked"
heo operation held reject op_123
heo operation held approve op_123 --dry-run   # what it would do, writes nothing
heo operation held show op_123 --format json
heo operation held edit op_123 --args edit.json --note "fixed the greeting"
heo operation held approve --class trash --inbox support --dry-run
heo operation held approve --class trash --inbox support
```

`list` also takes `--state` (`approved` ones are queued and later,
`rejected`, `expired`), `--kind`, `--inbox`, `--conversation`, `--since`
and `--until`.

- Approve queues the operation as proposed, under the key its proposal
  claimed; the workers apply it. Reject applies nothing and counts
  against the proposer's trust.
- Name several operations to decide each on its own. One that fails (not
  held, expired, stale, a conflict) is reported and the rest are still
  decided; the exit is 1 and the output lists every result.
- `--revision` compares against the revision `held show` printed (one
  operation only); a mismatch is a conflict, exit 4.
- An approval after the target changed, such as a new inbound message on
  the conversation, is refused as stale, exit 4, unless `--force`.
  `--force` skips only that check.
- A decision already made is returned as it stands (`replayed`) and
  writes nothing, so a rerun is safe.

### Edit

`held edit` replaces a held operation's arguments and approves it as
edited. Seed `--args` from `held show --format json`: save the output,
change its `intent` and pass the file (or `-` for standard input, or the
JSON itself). A document with an `intent` also names the operation and
the revision it was read at; `--revision` overrides that revision. A
document with no `intent` member is the intent itself. uma refuses
arguments that change the operation's target, audience or base, or that
are not the intent's shape (exit 2).

- An edit counts as a correction under the operation's trust key, or
  under the keys `--corrected-keys` names when the edit changed others.
- An edit after the target changed is refused as stale, exit 4. There is
  no `--force`: read the operation again with `held show` and edit what
  you now see.
- `--note`, `--revision`, `--dry-run` and `--idempotency-key` work as for
  approve. A plugin actor is refused, exit 5.

### Approve by selector

`held approve --class <class> --inbox <mailbox>` approves every held
operation that matches, in one call. `--class` is the ladder's class
name, as in `thresholds` (`trash`, `move_to`, `label`, `reply:send`,
`edit:message`, ...); `--inbox` is the mailbox, a key or ID; `--proposer` narrows
to one plugin actor.

- Both `--class` and `--inbox` are required. A selector with either
  missing is a usage error and approves nothing; it never means
  "everything".
- Only operations you may read match. `--dry-run` lists them, and the
  operations the rules below leave out, and writes nothing. Review it
  before the real run.
- At most `--max` operations are approved (default 50, at most 500). More
  matches are refused, nothing approved, never truncated: narrow the
  selector or raise `--max`.
- The always-held rules still exclude an operation: a cc added, a
  Reply-To that differs, an unattended send, a mailbox marked
  approval-always, a carry-over, the supervised first week. These are
  reported as `skipped` and stay held for a decision on their own.
- Every approval of a run shares one batch id, reported as `batch`. uma
  counts a batch once per trust key: approving fifty trashes adds one
  clean approval to their class, not fifty.
- Each operation is approved at the revision the selector listed: one
  that changed since is reported as a conflict and left held. The exit
  is 1 when any failed. `--note`, `--force` and `--idempotency-key` apply
  to every operation. A selector excludes `<operation>`, `--operation`
  and `--revision`.

### From a chat conversation

A held operation can be announced to a conversation on one of the
daemon's own channels and decided from it by replying or reacting. See
[Approving from a chat conversation](deliveries-and-events.md#approving-from-a-chat-conversation).
The decision runs as the actor the sender is mapped to, through the same
path as the commands above. On Slack a bare reply in the announcement's
thread does not decide unless `slack_thread_replies` is set; `approve
#<operation>` and reactions do.

### On REST, gRPC and MCP

Every verb here is served on the optional surfaces: the reads
(`held list`, `held show`, `held history`, `trust list`) as `GET` on
REST, the decisions (`held approve`, `held edit`, `held reject`) as `POST`.

- Reads follow what the caller may read: a person sees the operations
  uma shows them.
- Decisions are people's. A credential whose subject is a `plugin:` actor
  is refused on every surface, REST `403` with
  `heo.authz.plugin_decide`, whatever role it holds; the operation stays
  held. The surface token acts as the user who set it up, a person.
- MCP cannot pass a command's positional argument, so over MCP `held
  show`, `held approve`, `held edit`, `held reject` and `held history` take
  `--operation <id>` instead (repeat it on approve and reject; over
  MCP pass a JSON array): the `operation` argument of the tool. Name the operation one way, never
  both; neither is a usage error (`held history` with neither lists
  every held operation). See [surfaces.md](surfaces.md).
- A file path cannot be read by the daemon: over MCP, REST and gRPC pass
  `--args` as the JSON itself. The selector's `class`, `inbox`,
  `proposer` and `max` are plain arguments on every surface.

## Audit

```sh
heo operation held history                    # every held operation you may read
heo operation held history op_123
heo operation held history --state expired --since 2026-10-01
heo operation held history --format jsonl     # one record per line
```

The trail shows who proposed, edited, approved, rejected or let each
operation expire, when, the preview the decider was shown, and for an
edit the arguments before and after. It is derived from uma's held
operation and its events; heo keeps no copy, so a reader sees only what
uma shows them. Records use kit's consent audit vocabulary: `capability`
(the operation kind), `decision` (`allow` or `deny`) and, for an expiry,
`reason: expired`.

Holding also publishes `heo.operation.held.created` on the daemon's bus,
desk scope only, with the operation, kind, target, proposer and trust
key and no content.

## Expiry

An operation with no decision after `held_operations.expiry` (default
`72h`) moves to `expired`. The daemon's workers run the sweep every
`services.workers.held_sweep_every`: `0s`, the default, is uma's own
cadence of one minute; `off` runs none; the same sweep ends snoozes that
ran out. An expired operation counts
neither for nor against trust, applies nothing and no longer blocks a
new proposal for the same target. A decision on it is refused as a
conflict.
