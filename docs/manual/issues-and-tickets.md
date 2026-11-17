# Issues and tickets

An issue or a ticket is a thread, which is a conversation, and a comment
is a message. heo has no `issue` or `ticket` command and no verb named
for a provider: a tracker is read with `conversation list` and
`conversation get`, its comments with `message list`, and changed with
`conversation status` and `conversation update`. The channel's adapter
maps the provider onto them
([provider-mapping.md](provider-mapping.md)).

- A conversation of a **`per_work_item` inbox** is an issue or a ticket:
  the conversation is the **work item**, bound by ingress. `github.issues`,
  `gitlab.issues`, `jira.issues`, `linear.issues` and `zendesk.tickets`
  channels report them ([adapters.md](adapters.md)).
- A work item's **status** is normalized to uma's `open`, `pending`,
  `resolved` and `closed`, and the provider's own status is kept beside
  it. It is the provider's unless the channel's configuration makes it
  uma's (`local_fields`).
- A conversation's own status is uma's (`conversation resolve`,
  `conversation reopen`; see [conversations.md](conversations.md)). The
  two lifecycles are independent: setting a work item's status leaves its
  conversation as it is, and resolving the conversation never closes the
  issue.

Every command here runs in the profile's daemon. Start it first with
`heo service start`; with none running, these commands exit 70. IDs are
record IDs, as `conversation list` prints them (`conv_123` below).

## Browse a tracker

```sh
heo conversation list --work-item                    # issues and tickets only
heo conversation list --work-item --work-item-status open,pending
heo conversation list --inbox issues --work-item-status closed
heo conversation list --work-item=false              # conversations that are not trackers
```

Each conversation of a `per_work_item` inbox lists its work item: the
provider's reference and uma's status of it. `--work-item` keeps the
conversations that have a work item (`=false`, those that have none),
and `--work-item-status` those whose work item is in one of the given
uma statuses, not the provider's. uma has no filter by work item, so heo
reads each conversation's work item and scans until the page is full;
the filters combine by AND with `--inbox`, `--status`, the label and the
workflow filters.

## Read an issue or a ticket

```sh
heo conversation get conv_123          # the conversation and its work item
heo message list --conversation conv_123
```

`conversation get` shows the conversation, as for any other, and then
its work item:

| Row | Meaning |
| --- | --- |
| `work item` | the work item's record ID |
| `work item reference` | the provider's reference: `owner/repo#7` for an issue in a repository, the number for a ticket |
| `work item subject`, `url`, `requester`, `assignees` | as the provider last reported them (or as uma holds them, for the fields uma owns) |
| `work item status` | uma's status |
| `work item provider status` | the provider's own: `open`, `closed:completed`, `closed:not_planned` for GitHub; `new`, `open`, `pending`, `hold`, `solved` for Zendesk |
| `work item binding` | `active`, or what became of the record at the provider: `deleted`, `merged`, `moved` (with its successor), `inaccessible` |
| `work item uma-owned fields` | the fields the channel's configuration makes uma's (`local_fields`), when any |
| `work item field <name>` | each of the work item's fields: labels, a milestone, a ticket's tags, under the adapter's own names |
| `work item linked from` | the conversations that link to it (`conversation link --work-item`) |
| `work item revision` | the work item's revision, which `conversation status` and `conversation update` take |

In JSON the work item is the `work_item` object beside the conversation's
own fields; a conversation with no work item has none. The comments are
the conversation's messages: public comments appear for every reader,
internal notes (and Zendesk comments whose visibility Zendesk does not
state) for desk actors only.

For a desk actor `conversation get` also lists, per provider-dependent
verb, whether the channel can do it, so `conversation status` and
`conversation update` show `declared` or `unsupported` per channel (see
[provider-verbs.md](provider-verbs.md)).

## Set the status

```sh
heo conversation status conv_123 --set closed --reason not_planned --dry-run
heo conversation status conv_123 --set closed --reason not_planned --revision rev_9
heo conversation status conv_123 --set open
```

`--set` is one of uma's work item statuses: `open`, `pending`,
`resolved`, `closed`. `--reason` is the reason the provider records for
the transition, one the adapter declares for it (`completed` or
`not_planned` for a GitHub close); without it the adapter's default is
used. heo checks only that a reason is one lowercase snake_case word: what
it may be is the adapter's declaration.

This is an **external change**: uma asks the provider through the
transitions the channel's adapter declares (`work_item_act`), and
everyone who watches the issue sees it. A transition the adapter does not
declare is refused, and verb discovery shows that per channel.
`--dry-run` prints exactly what would be sent and where, and sends
nothing:

```text
work item          wi_456
conversation       conv_123
inbox              inbox_123
adapter            github.issues
capability         work_item_act declared by github.issues; live availability not checked (operator: heo channel check ch_1 --remote)
destination        work_item 7
from               open
status             closed
expected revision  rev_9
delivery           queued, not applied: the work item changes in uma only when the provider reports it
```

Without `--dry-run`, the change is queued as a uma operation and the
command returns with it. **It is never applied when the command
returns**, and heo does not say the status is set:

```text
conversation   conv_123
work item      wi_456
reference      owner/repo#7
status         open (asked: closed, reason not_planned; not applied until the provider reports it)
revision       rev_9
operation      op_123 (queued)
operation key  op_...
```

The daemon's workers then send it. The provider's answer may carry the
new state or not (a lost answer is settled by asking the provider); when
it does not, `conversation get` shows the old status until the provider's
own webhook of the change arrives, which can be some time after the
operation shows `delivered`. That webhook is an echo of your own change:
heo records the new status and nothing else. `heo delivery get
<operation>` follows the operation. If the provider refuses it (the
account may not change issues), the operation ends `rejected` or `failed`
and the work item keeps its status.

- **`--revision`** is the work item's revision, as `conversation get`
  shows it. A revision is always sent: yours, or, if you give none, the
  one heo reads a moment before. A work item that changed since is
  `CONFLICT`, exit 4, naming the current revision, and nothing is written.
- **Retry**: rerunning with the same `--idempotency-key`, status, reason
  and `--revision` returns the same operation, marked `replayed`, even
  after the provider reported the change, so a retry never sends a second
  change. Without `--revision`, a retry after the work item changed
  carries a new revision and is not a replay.
- A work item **already in the status** you ask for is refused
  (`UNSUPPORTED`, exit 2): a transition goes from one status to another.
- The conversation **does not move**: setting the status never resolves
  or closes it, and `heo conversation resolve` and `heo conversation
  reopen` change nothing at the provider.
- Where the channel's configuration makes the status uma's own
  (`local_fields` lists `status`), the change commits in uma at once, as a
  local operation that reaches no provider, and takes no reason.

Which channels can do it follows the adapters' declarations, not a
provider name: `github.issues` and `gitlab.issues` declare the close and
the reopen (GitHub with the reasons `completed`, `not_planned` and
`reopened`); `jira.issues`, `linear.issues` and `zendesk.tickets` declare
no transition, so the verb is refused there, exit 2, and their status is
the provider's alone (Zendesk's own status is set with `conversation
update --field status=...`, below). `--dry-run` says so before drawing a
plan, and so does the real run, before anything is written. The channel's
configuration and the provider account's rights are checked again when
the operation is applied; a failure shows on `delivery get`, not on this
command.

| Situation | Answer |
| --- | --- |
| `--set` missing or not a work item status; a reason that is not one snake_case word | usage error, exit 2 |
| no such conversation | `NOT_FOUND`, exit 3 |
| a stale `--revision`; the work item was deleted, transferred or lost at the provider | `CONFLICT`, exit 4 |
| the adapter declares no work item act, the transition or the reason; the work item is already in the status; the conversation has no work item | `UNSUPPORTED`, exit 2 |
| a role other than desk | `UNAUTHORIZED`, exit 5 |

## Change fields

```sh
heo conversation update conv_123 --field 'labels=["bug","ui"]' --dry-run
heo conversation update conv_123 --field 'assignees=["ada","grace"]' --field milestone=3
heo conversation update conv_123 --field milestone=null --field 'labels=[]' --idempotency-key reset-123
heo conversation update conv_123 --field status=solved
```

`--field key=value` is repeated for several fields. Which keys exist and
what they take is the provider's and the adapter's, not heo's: heo passes
each through and the adapter, or uma, accepts or refuses it. A value that
is JSON (a number, `true`, `null`, a list, an object) is sent as such,
anything else as a string, so `milestone=3` is the number 3 and
`status=solved` the string `"solved"`. A key given twice is refused. The
status of a work item has its own verb above.

Where a field goes follows who owns it on the channel, which its
configuration sets (`local_fields`):

| Owner | What happens |
| --- | --- |
| **uma** (`local_fields` lists `subject`, `assignees` or `fields`) | the change commits in uma at once, as a local operation, on any work item channel; it reaches no provider. `subject` and `assignees` are fields of their own, every other key is an entry of `fields`, named under the adapter's namespace (`labels` becomes `<namespace>.labels`); assignees are provider IDs, a JSON list or comma separated. `--revision` is the work item's, as for a status |
| **the provider** (every other field) | it goes through the update effect the channel's adapter ships for its work items, an external change queued like a reply. heo finds the effect from the adapter's declarations: the one it ships, authorized as a work item act. The adapter validates the fields |
| **nobody** (the adapter ships no update effect) | refused, `UNSUPPORTED`, exit 2: the provider's own fields are ingest-only, and `conversation get` shows the verb `unsupported` on the channel |

One update is of one kind: fields of both owners are two updates (a
validation error). `status` is never an update field where uma owns the
status (use `conversation status`).

`--dry-run` prints exactly what would be sent and where, asks the
adapter, offline, to check the fields (`VALIDATION`, exit 2), and sends
nothing:

```text
work item     wi_456
conversation  conv_123
inbox         inbox_123
adapter       github.issues
capability    extension_execute declared by github.issues, which ships github.issues.update; live availability not checked (operator: heo channel check ch_1 --remote)
destination   work_item 7
visibility    internal
change        labels=["bug","ui"]
delivery      queued, not applied: the work item changes in uma only when the provider reports it
```

Without `--dry-run`, a provider's update is queued as a uma operation and
the command returns with it. **It is never applied when the command
returns**, and the output says what was asked, not that the work item has
it (`labels asked  ["bug","ui"]`). `heo delivery get <operation>` follows
it; `conversation get` shows the new fields once the provider's own
webhook reaches heo. The update touches neither the work item's status nor
the conversation. If the provider refuses it (a login that is not a
collaborator, a milestone that does not exist), the operation ends
`rejected` or `failed` and the work item keeps what it had.

- **Retry**: rerunning with the same `--idempotency-key` and fields
  returns the same operation, marked `replayed`, so a retry never sends a
  second update. The same key with other fields is `CONFLICT` (exit 4).
- A provider's update names no `--revision`; one is refused for it.
- The adapter checks the fields offline: `--dry-run` refuses what its
  rules refuse (GitHub: logins that are not logins, a label or login named
  twice, a milestone that is not a number or `null`), and uma refuses the
  same on the real run, before anything is stored.

Tracker labels are the provider's own, set here (`--field labels=...`
where the adapter takes them). They are not the labels `heo label`
defines, which are uma's and local to it
([conversations.md](conversations.md#labels)), and not the mail tags
`conversation tag` changes.

| Situation | Answer |
| --- | --- |
| no `--field`; a field that is not `key=value`; a key twice | usage error, exit 2 |
| fields the adapter refuses offline; fields of two owners; a `status` field where uma owns the status | `VALIDATION`, exit 2 |
| no such conversation | `NOT_FOUND`, exit 3 |
| the work item was deleted, transferred or lost at the provider; the key was used with other fields; a stale `--revision` | `CONFLICT`, exit 4 |
| the adapter ships no update effect (or more than one); the conversation has no work item | `UNSUPPORTED`, exit 2 |
| a role other than desk | `UNAUTHORIZED`, exit 5 |

## Held operations

With `held_operations.enabled` on, a plugin actor's change of a status
the provider owns (`conversation status`) and its update of the
provider's fields (`conversation update`) are offered to the hold point:
uma stores them held with nothing queued, approving applies them once,
rejecting applies nothing. The trust classes are `status:<status>` for a
status and `fields:<keys>` for an update, the keys sorted and comma
separated, so a change of other fields earns its trust of its own
([authorization.md](authorization.md)). A change of a status or fields
that uma owns cannot be held yet: uma has no hold for a local work item
write, so a plugin actor's is refused (`UNSUPPORTED`) while holding is on.
A person's is never held.

## Roles

| Command | Roles |
| --- | --- |
| `conversation list`, `conversation get` (with their work item), `message list` | desk, viewer |
| `conversation status`, `conversation update` | desk |

A viewer sees public comments only. See
[authorization.md](authorization.md).
