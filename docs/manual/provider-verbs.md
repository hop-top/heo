# Provider verbs: edit, delete, react, participants, receipts, history, interactions, modals, issue and ticket changes through conversations

Most heo commands read or write uma's own records. A few reach the other
side: they edit or delete a message at its platform, react to it, bring
someone into a conversation or take them out, tell a sender its message
arrived or was read, read the provider's past messages, answer a button
press or a slash command a chat provider reported, open or change a Slack
modal in answer to one, set the status of the issue or ticket a
conversation is, change its fields at the tracker, change the tags a mail provider keeps on a
message, or change what a provider holds about a whole conversation (its archive, folder, read
state, star and trash). These are the **provider-dependent verbs**. They work only on a
channel whose adapter can do the thing, and heo tells you which before you
try.

| Verb | Needs | What it does |
| --- | --- | --- |
| `message raw` | `raw_read` | reads the provider's whole copy of a message on demand (for mail, the RFC 5322 bytes) and writes it to a file or stdout; keeps nothing ([conversations.md](conversations.md#export-a-messages-raw-bytes)) |
| `message edit` | `edit` | replaces a message's text at the provider |
| `message delete` | `delete` | deletes a message at the provider; uma keeps a tombstone |
| `reaction add`, `reaction remove` | `react` | adds or removes your reaction |
| `participant add`, `participant remove` | `membership` | brings an identity into a conversation at the provider, or takes it out |
| `receipt send` | `receipt` | tells the provider a received message was delivered or read |
| `history fetch` | `history` | reads a bounded window of the provider's past messages into heo |
| `interaction respond` | `respond`, and `respond_follow_up` for `--follow-up` | answers a button press, slash command or form the provider reported, in the window it gives |
| `interaction open-modal`, `interaction push-modal`, `interaction update-modal` | `extension_execute`, with Slack's `views.open`, `views.push` or `views.update` | opens a modal in answer to an interaction, stacks another view on it, or changes an open one ([below](#slack-modals)) |
| `conversation status` | `work_item_act`, with the transitions the adapter declares | sets the status of the issue or ticket a conversation is, through the transition the adapter declares ([issues-and-tickets.md](issues-and-tickets.md#set-the-status)) |
| `conversation update` | `extension_execute`, with the update effect the adapter ships for its work items | changes the provider-owned fields of the issue or ticket a conversation is; fields uma owns change in uma on any channel ([issues-and-tickets.md](issues-and-tickets.md#change-fields)) |
| `message reply`, `message reply-draft` with `--reply-all`, `--cc`, `--bcc` | `reply_recipients` | addresses a reply beyond the reply route: the other recipients of the message answered, and explicit cc and bcc; a reply without them needs no capability ([conversations.md](conversations.md#reply-to-all-cc-and-bcc)) |
| `message reply-draft` | `extension_execute`, with Gmail's or Outlook's reply draft | keeps a reply in the mailbox's Drafts for a person to finish and send; sends nothing ([conversations.md](conversations.md#keep-a-reply-as-a-provider-draft)) |
| `conversation unsubscribe` | `extension_execute`, with Gmail's or Outlook's unsubscribe | asks the sender of the message a mail conversation replies to to stop mailing the mailbox, by what the message's own headers allow; irreversible ([conversations.md](conversations.md#unsubscribe-from-a-sender)) |
| `conversation tag` | `extension_execute`, with Gmail's label change (`--add`, `--remove`) or Outlook's category change (`--set`) | changes the provider's own tags on the message a mail conversation replies to ([conversations.md](conversations.md#tag-a-mail-message-at-the-provider)) |
| `provider-label list` | `extension_query`, with `<type>.mailboxes` | lists the mailboxes (Gmail labels, Exchange folders and categories, IMAP mailboxes) of the account a mail channel is ([conversations.md](conversations.md#list-and-create-provider-mailboxes)) |
| `provider-label create` | `extension_execute`, with `<type>.mailbox_create` | creates one, idempotent by name; `conversation move` and `conversation tag` resolve names through the list |
| `conversation archive`, `conversation unarchive` | `archive` | archives a conversation at the provider (out of a mailbox's inbox, a chat room or thread archived), or undoes it ([conversations.md](conversations.md#change-a-conversations-provider-state)) |
| `conversation move` | `move` | files a conversation at the provider: in a folder or label by its ID, in a special-use role (`inbox`, `archive`, `trash`, `junk`), or by adding and removing provider labels |
| `conversation read`, `conversation unread` | `read_state` | marks a conversation read or unread at the provider; never your local read marker |
| `conversation flag`, `conversation unflag` | `flag` | sets or clears a star or an important flag at the provider |
| `conversation trash`, `conversation untrash` | `trash` | moves the account's copy to the provider's trash, or restores it; deletes nothing at the peer |

`reaction list`, `participant list`, `interaction list` and `interaction
get` are not on the list: they read what uma recorded, on any channel, and
need no capability.

Two things look like these verbs and are not. `receipt send` is not the
local read state: it marks nothing read in heo. `conversation
mark-read` is the local read marker ([conversations.md](conversations.md#assign-snooze-link-and-read)),
and it sends no receipt; `conversation read` is the provider's read state,
and never moves the marker.

The nine conversation state verbs run through uma's provider conversation
state, one capability for each pair (`archive`, `move`, `read_state`, `flag`,
`trash`). The adapter reports their limits live, not in the offline table:
the scope (`account`, a mailbox's own view, or `shared`, what everyone sees,
as a chat room), the flag kinds, the move shape (`labels` or `folder`) and
the roles it resolves. `heo conversation get` and `heo channel check
--remote` show them per verb, and `--dry-run` refuses a flag kind, role or
label move the channel does not have. And `history fetch` is not event replay:
`event list` reads heo's own log, `history fetch` asks the provider.

Every one of these commands is in heo's command tree (help, `heo spec`),
whatever channels you have. A channel that cannot do the verb refuses
it; it is never hidden. Three of them are not served on every surface:

| Verb | CLI and control socket | REST, gRPC, MCP |
| --- | --- | --- |
| `message delete` | yes | withheld (destructive) |
| `participant remove` | yes | withheld (destructive) |
| `history fetch` | yes | withheld (long-running import) |
| every other verb above | yes | yes |

Discovery on a running daemon shows a withheld command as
`withheld-by-command`. The first line of each one's help says so:
*Provider-dependent: needs `<capability>` on the conversation's channel;
see `heo channel get`*.

## Check before you ask

Offline, from the binary and the channel record alone, with no provider
reached:

```sh
heo adapter get github.issues      # capabilities and the verbs they enable
heo channel get support-github     # per verb: declared or unsupported, and why
```

```text
verb message edit        declared (edit)
verb message delete      declared (delete)
verb reaction add        unsupported: config (react), replies is off in the channel configuration
verb participant add     unsupported: not_declared (membership)
verb receipt send        unsupported: not_declared (receipt)
verb history fetch       declared (history)
verb interaction respond unsupported: not_declared (respond)
verb interaction open-modal unsupported: not_declared (extension_execute), ships no extension_execute effect slack.chat.views.open
verb conversation status declared (work_item_act)
verb conversation update declared (extension_execute)
verb conversation tag    unsupported: not_declared (extension_execute), ships no extension_execute effect google.mail.labels or microsoft.mail.categories
verb message reply-draft unsupported: not_declared (extension_execute), ships no extension_execute effect google.mail.reply_draft or microsoft.mail.reply_draft
verb conversation unsubscribe unsupported: not_declared (extension_execute), ships no extension_execute effect google.mail.unsubscribe or microsoft.mail.unsubscribe
```

`unsupported` carries a reason:

| Reason | Meaning |
| --- | --- |
| `adapter_missing` | the channel's adapter is not in this build |
| `not_declared` | the adapter does not declare the capability; for each verb that runs an extension effect (`conversation update`, `conversation tag` and the three modal verbs), when it declares `extension_execute` for effects of its own and not the one the verb runs: for `conversation update`, an effect authorized as a work item act (GitHub's `github.issues.update` and Zendesk's `zendesk.tickets.update` are); for the others Gmail's `google.mail.labels`, Outlook's `microsoft.mail.categories` or Slack's `slack.chat.views.open`, `.push` and `.update`; the row then names what is missing. `conversation status` is declared by `work_item_act` alone; which transitions and reasons the adapter performs are its limits (`channel get` lists them under `limits`), checked when the verb runs |
| `config` | the adapter declares it and the channel's own configuration switches it off: `github.issues` without `replies` |
| `not_executed` | uma runs no operation for the capability (kept for namespaced capabilities; no builtin is in this state today) |

`declared` is not a promise. Whether the provider account may do it
right now, or whether the adapter's configuration allows it, is a live
question the operator asks:

```sh
heo channel check support-github --remote
```

It adds a state per verb: `available`, `unauthorized` (the provider
account lacks the permission, `PROVIDER_PERMISSION`, exit 5: refused like
a caller denied, but the remedy is the provider's grant, not a heo role),
`disabled` (the adapter's own configuration switched it off, `DISABLED`,
exit 4), `unavailable` (`UNAVAILABLE`, exit 6) or `unsupported`
(`UNSUPPORTED`, exit 2). The check opens the adapter and may reach the
provider, so it is the operator's (the desk role cannot read channels).

The desk has its own live view, for one conversation: `heo conversation
get` shows the same states per verb for the conversation's channel, from
uma's own capability status for it, to any desk actor, with no channel
read and no channel configuration. It opens the adapter and may reach
the provider, so it can be slow: past ten seconds, or on a failure, the
conversation is shown without verbs and says why. A viewer sees no
verbs.

```sh
heo conversation get conv_123
```

```text
verb message edit       disabled (DISABLED, exit 4)
verb participant add    available
verb receipt send       unauthorized (PROVIDER_PERMISSION, exit 5)
verb history fetch      unsupported (UNSUPPORTED, exit 2)
```

## What a refusal looks like

Where the check happens decides when you hear about it:

1. **`--dry-run`**: heo reads the target as you, then asks whether the
   channel's adapter declares the capability. If not, it exits 2
   (`UNSUPPORTED`) before drawing any plan, naming the adapter, the
   capability and `heo channel get <channel>`. A plan that passes carries
   a `capability` line: *`edit` declared by `github.issues`; live
   availability not checked*. That is exactly what was checked, and what
   was not: the provider account's rights and the channel's configuration
   are not.
2. **The real run**: uma makes the same declaration check when it accepts
   the request, before it stores anything. An undeclared capability is
   `UNSUPPORTED`, exit 2, and nothing is written; the operation key stays
   free.
3. **Every attempt**: the daemon's workers check again, live, just before
   the provider is called. A capability the provider or the channel has
   withdrawn since is not a command error; it ends the operation:
   `unauthorized` when the account lost the permission, `unsupported` when
   the adapter's configuration switched it off (`disabled`, as
   `github.issues` without `replies`). Neither is retried. Read it with
   `heo delivery get <operation>`: that command succeeds (exit 0) and the
   state is in its data. See
   [deliveries-and-events.md](deliveries-and-events.md).

A preview that passes therefore does not guarantee the provider will
take the change. It guarantees the adapter declares it.

## Queued is not applied

`message edit`, `message delete`, `reaction add`, `reaction remove`,
`participant add`, `participant remove`, `receipt send`, `interaction
respond`, the three modal verbs, `conversation status`, `conversation update` and
`conversation tag` are queued like a reply: the command returns once uma accepted the request durably,
with its operation. The message changes only when the provider applied it.
Until then `message get` shows the old text, and `heo delivery get` shows
the operation. The provider's own echo of the change (an edited message
coming back through ingress) is the same change, not a duplicate message.

**A script that reads after a verb waits for its operation.** The provider
can show the change a moment before uma records it: a widget's live event,
or an edit that appears on the platform. uma
records the outcome (the message's new revision, its reactions, its
tombstone) when the operation settles, and that comes after the provider
answered. A `message get` or a `--revision` guarded verb issued on the
provider's signal alone can read the message as it was, or fail with
`CONFLICT` (*it changed since it was read*). A reply settles the same way:
the revision you read right after it can move once it settles. So wait for
the operation to end, then read:

```sh
op="$(heo message edit "$msg" --revision "$rev" --text "On it, now." --format json | jq -r .operation.id)"
heo operation watch "$op"
heo message get "$msg"
```

`operation watch` exits 0 once the operation is `provider_accepted` or
`delivered`, and with the failure's own exit code if it ends otherwise (see
[Follow an operation](deliveries-and-events.md#follow-an-operation)); to
poll instead, read `heo operation get "$op"` until its state is no longer
`queued` or `retry_pending`.

A `conversation status` can wait longer still. The provider's answer to
the change may carry the work item's new state or not; when it does not,
the work item changes only when the provider's own webhook of the change
arrives, and `conversation get` shows the old status until then. The
command's output says *queued* and *not applied*, never that the status is
set. (Where uma owns the status, it commits at once and nothing is queued.)

A `conversation update` of the provider's fields waits the same way, for
the work item's assignees, labels and milestone, and a `conversation tag` leaves nothing to wait for in heo:
uma records no provider tags, so `heo delivery get` is the only place to
see it applied.
Each is committed under `--idempotency-key`: rerunning with the same key
and arguments returns the same operation (`replayed`), and nothing is
sent twice. Without a key heo mints one and prints it as `operation key`;
`heo operation get --key <key>` finds the operation later.

## Edit and delete

```sh
heo message get msg_123                     # note its revision
heo message edit msg_123 --revision rev_9 --text "Corrected: 10:30." --dry-run
heo message edit msg_123 --revision rev_9 --text "Corrected: 10:30."
heo message delete msg_123 --revision rev_9 --confirm=yes
```

- `--revision` is required and always sent. If the message changed since
  you read it (someone edited it, a reaction arrived, the provider sent
  an update) the command exits 4 (`CONFLICT`) naming the current
  revision, and writes nothing. Read the message again and retry with the
  new revision.
- An edit whose retry finds the message's *audience* narrowed (say a
  public message made internal) is a conflict too, not a replay: read it
  again and use a new `--idempotency-key`.
- An edit never changes the audience, and only the text. If the provider
  applied a newer edit than yours, the newer text stays.
- A deleted or redacted message cannot be edited or deleted again
  (`CONFLICT`).
- `delete` is a **tombstone**: the provider removes the message and uma
  keeps the record, with its text cleared and `deleted` set. It is not a
  privacy erasure. Redaction is a separate act, the operator's
  (`privacy.redact`).
- `--dry-run` prints the platform effect, the audience (a message's
  visibility, public or internal), the capability line and the operation
  key. Desk role.

### Why `message delete` is withheld on served surfaces

A provider deletion is in the destructive tier (`destructive-shared`).
heo's served surfaces refuse that tier until the operator names the
surface: REST, gRPC and MCP list `message delete` as not invocable
(`unauthorized-destructive`) and do not run it. The control socket, the
terminal's path to the daemon, allows it; at a terminal you confirm with
`--confirm=yes`. The socket counts destructive commands against a small
per-caller budget (3 at once, 12 a minute); `--dry-run` previews count
too. `message edit` and the reactions are ordinary shared writes and are
served everywhere.

## Reactions

```sh
heo reaction list --message msg_123
heo reaction add --message msg_123 👍 --dry-run
heo reaction add --message msg_123 👍
heo reaction remove --message msg_123 👍
```

- `reaction list` reads what uma stores for the message, for any role that
  can read it, on any channel. Each reaction has its emoji, who made it
  (an identity of the provider, or a desk actor of heo), and when.
  Where the provider gives only totals, a row is a count with no names.
- **`complete`** says how far to trust the list. `true`: uma holds the
  provider's authoritative set. `false`: it was built from reaction
  events, or cut at its size limit, so the provider may hold reactions
  uma never heard of. A fresh message with no events lists nothing and
  says `complete: false`; text output prints that on stderr, JSON carries
  `"complete": false`.
- An emoji is one token of 1 to 64 bytes, compared byte for byte (a
  variation selector or skin tone is another emoji). Which emoji a
  provider takes, and how many reactions one user may leave, is the
  provider's; heo reports the answer it gets, on the operation.
- `--revision` is optional for a reaction: uma compares it only when set.

## Participants

```sh
heo participant list --conversation conv_123
heo conversation get conv_123                        # note its revision
heo participant add --conversation conv_123 --revision rev_9 ident_42 --dry-run
heo participant add --conversation conv_123 --revision rev_9 ident_42
heo participant remove --conversation conv_123 --revision rev_9 ident_42 --confirm=yes
```

`participant list` shows the identities in a conversation, with their
role there, and (where you may read the inbox's identities) their
assurance level and contact. It reads what uma recorded, from the
provider's traffic and from the changes below, and needs no capability.

`participant add` and `participant remove` change who takes part at the
provider. `<who>` is an identity of the conversation's inbox, or a
contact that has exactly one identity there (with several, heo lists
them and asks for one).

- `--revision` is the conversation's revision and is always sent: if
  the conversation changed since you read it, the command exits 4
  (`CONFLICT`) naming the current revision, and writes nothing.
- Adding someone who already takes part, removing someone who does not,
  and removing the conversation's owner are conflicts (exit 4); a
  `--dry-run` says so before drawing a plan.
- The change is queued, and the participant list shows it only once the
  provider applied it: the identity joins with the role `participant`,
  or leaves, and the conversation moves to a new revision. A message
  from a removed person later adds them back to uma's list, as for any
  sender.
- `participant remove` takes a person out of a conversation at the
  provider and heo cannot undo it: bringing them back is a new `add`, if
  the provider lets it. It is in the destructive tier like `message
  delete`: withheld on REST, gRPC and MCP, confirmed with `--confirm=yes`
  at a terminal. `participant add` is an ordinary shared write.

## Receipts

```sh
heo receipt send --message msg_123 --kind read --dry-run
heo receipt send --message msg_123 --kind delivered
```

A receipt tells the provider that a message it delivered to you was
delivered or read, so its sender may see it. `--kind` is `read` or
`delivered`. Only a message that arrived from the provider takes one: a
message sent through heo, or the provider's echo of one, is a usage
error (exit 2), and a deleted or redacted message a conflict (exit 4). A
receipt names no revision.

The command never says the provider showed it. It says the receipt is
queued; once a worker sent it, `heo delivery get` says the provider
accepted it. What the sender is shown, if anything, is the provider's
choice, and heo cannot tell. A provider with no receipt of the kind
rejects it, and the operation says so. Nothing in heo changes when a
receipt is sent: the operation is the only record. Under one
`--idempotency-key` the same kind returns the same operation; another
kind is a conflict.

## Interactions

A chat provider reports a button press, a slash command or a submitted
form as an **interaction**, and expects an answer inside a window. uma
records it, never as a message, and commits an `interaction.received`
event next to `message.received`. heo reads the records and answers them:

```sh
heo interaction list --conversation conv_123
heo interaction get ix_123
heo interaction respond ix_123 --mode ephemeral --text "Approved." --dry-run
heo interaction respond ix_123 --mode message --text "Approved." --idempotency-key answer-42
heo interaction respond ix_123 --mode update --text "Approved by Ada." --follow-up
```

`interaction list` and `interaction get` read what uma recorded, on any
channel, and need no capability (roles `desk` and `viewer`; an interaction
that is not public is the desk's alone). A row says what was invoked (a
command, a button's action ID or a form's schema), when it was recorded,
until when a response is still possible, and whether it was answered. The
provider's response token is never shown, by any verb or event.

`interaction respond` needs `respond` on the channel's adapter. `--mode`
is required and says how the answer is shown; the audience follows from
it and heo never widens it:

| `--mode` | Shows | Audience |
| --- | --- | --- |
| `message` | a new message answering the interaction; uma commits it as a message of the conversation | public |
| `ephemeral` | the text to the interaction's actor only; no message is kept | restricted |
| `update` | replaces the provider message the control sits on, which the interaction must name | public |

- An interaction that is not public takes an `ephemeral` response only: a
  public one would widen who can see it (exit 2, `VALIDATION`).
- There is one **initial** response per interaction. Of two, one is
  accepted and the other exits 4 (`CONFLICT`), in a preview too. `--follow-up`
  asks for a follow-up instead: it never takes the initial response's
  place, may come before or after it, and needs the adapter to declare
  `respond_follow_up` besides (exit 2, `UNSUPPORTED`, where it does not).
- A redacted interaction takes none (exit 4). An `update` of an
  interaction that names no control message is a usage error (exit 2).
- The **window**. A provider may give each interaction an end to the time
  a response is still possible (`respond until` in `interaction get`).
  `--expires-at` (RFC 3339 or a date) bounds the attempt and may not pass
  it (exit 2). Without it the expiry is that window's end; where the
  provider states none, 15 minutes after uma recorded the interaction
  (the web widget states none). Pass a later `--expires-at` to answer after that. A
  window that has ended, or an expiry in the past, exits 1
  (`DEADLINE_EXPIRED`), at the preview and at submission, and again when
  the daemon's workers send it, which then makes no provider call. The
  default depends on the record alone, so a retry under the same
  `--idempotency-key` is the same request and replays.
- Acknowledging an interaction is not answering it. The adapter
  acknowledges the provider once uma holds the interaction durably; the
  answer comes later, from `interaction respond`, inside the window.

Like the other verbs here, `respond` is queued, never given: the command
returns once uma accepted it durably with its operation, and the daemon's
workers hand it to the provider, which shows it only once it accepts it.
An adapter that does not declare `respond` is refused, exit 2
(`UNSUPPORTED`), at the preview and on submission, before anything is
claimed or written. An adapter that only receives, such as a chat
provider's inbound webhook, reports interactions and cannot answer them.

## Slack modals

A modal is a window Slack opens for the person who pressed a button or typed
a command. Slack opens one only with the `trigger_id` it sent with that
interaction, which works once and for three seconds after Slack sent it.
Three verbs run `slack.chat`'s own effects:

| Verb | Slack call | Takes |
| --- | --- | --- |
| `interaction open-modal <interaction>` | `views.open` | `--view`, the interaction's trigger |
| `interaction push-modal <interaction>` | `views.push`: a view on the stack of the modal the interaction came from; Slack allows three | the same |
| `interaction update-modal` | `views.update` | `--inbox`, `--view-id` or `--external-id`, optionally `--hash`, and `--view` |

```sh
heo interaction open-modal ix_123 --view approve.json --dry-run
heo interaction open-modal ix_123 --view approve.json --idempotency-key open-42
heo operation get <operation>
heo interaction update-modal --inbox rooms --view-id V0123456789 --view approved.json
heo interaction update-modal --inbox rooms --external-id deploy-42 --view -
```

`--view` is a Slack modal view object as JSON: a file, `-` for standard
input, or the JSON itself. It is read where you run the command and sent
compact; at most 250 KiB compact, the bound uma states for a view
(`VALIDATION`, exit 2, before the daemon is asked). Whether the view is a
modal, its title and buttons, its 100 blocks and the rest of what Slack's
reference states, is the adapter's offline check: at the preview, and again
when the request is submitted. Block Kit itself is Slack's to judge.

**The window.** The trigger_id is the interaction's own and is never shown.
heo takes the instant it stops working from the interaction's deadline
(`acknowledge by` in `interaction get`: three seconds after Slack's request)
and refuses a later attempt itself, `DEADLINE_EXPIRED` (exit 1), at the
preview and at submission, naming the instant, with nothing queued. That
window is shorter than anyone types, so these verbs are for a script or an
agent that reacts to the `interaction.received` event (`heo event watch`;
see [deliveries-and-events.md](deliveries-and-events.md#react-to-an-interaction)). A
retry of an open under its key after the window is refused as well, even if
the first attempt got through: `heo operation get --key <key>` finds it.

**What heo checks that uma does not.** uma's extension call is aimed at a
conversation, not at an interaction, so the engine neither rechecks the
interaction nor binds the call to who pressed. heo does, before it queues
anything: the interaction exists and you may read it (`NOT_FOUND`, exit 3
otherwise); it is not redacted (`CONFLICT`, exit 4); Slack sent a trigger_id
with it (`VALIDATION`, exit 2: a payload without one is named by a digest);
its provider window has not ended (`DEADLINE_EXPIRED`); and you may answer
interactions at all, the right `interaction respond` needs
([authorization.md](authorization.md#extension-effects)). Slack binds the
trigger_id to the person who acted and refuses another's.

**Queued, and woken.** Like every verb here the modal is queued, never
shown by the command: it returns once uma accepted it durably, with the
operation. The three seconds are also spent inside heo, though: the command,
the queue and the worker's claim. The daemon's workers wait `services.workers.poll`
(a second) between rounds that find nothing, which on its own would eat up
to a third of the window, so `open-modal` and `push-modal` wake the idle
workers as they queue, and the call to Slack follows in milliseconds (an
in-process measurement without the network: 1 to 6 ms at the default poll,
against up to 0.96 s without the wake). `update-modal` spends no trigger and
does not wake them.

**What follows.** `heo delivery get <operation>` says what Slack answered:

| Outcome | Meaning |
| --- | --- |
| `provider_accepted` | Slack took the view. The operation's provider references (`operation get`, `provider refs`) hold a `view` and its ID, `V...`: what `update-modal --view-id` takes |
| `rejected`, `deadline_expired` | the attempt came after the trigger_id stopped working, or Slack said so (`expired_trigger_id`); no modal. The adapter refuses before any call, so a late attempt costs nothing |
| `rejected`, `conflict` | the trigger_id was already used, the stack is full (the third push), the hash is stale, the `external_id` is taken |
| `rejected`, `validation`, `not_found` | Slack refused the view, or the view to update is not there |
| `outcome_unknown` | the answer was lost after the request was written. The trigger_id may be spent, so it is never sent again and cannot be reconciled; `delivery reconcile` reports it unsupported. Open a new modal from the next press |

A view can be named for a later update two ways: by the ID Slack gives
(only once the open settled, from the operation), or by an `external_id` the
view carries when it is opened, unique in the workspace; the output of the
open says both. A button inside a modal names no view, so `external_id` is
the way to update a modal from the press that happens in it. An update is
aimed at an inbox, not an interaction, so it works whenever you hold the
view's name and `--hash`, if you pass one, still matches.

An adapter that does not declare `extension_execute`, or declares it for
other effects, is refused `UNSUPPORTED` (exit 2) at the preview and at
submission, before anything is written: the verb table of `channel get`
names the effect each verb lacks. The effects run as an internal operation;
the modal itself is no message and uma records nothing of it.

## Extension effects: conversation update, conversation tag, reply draft, unsubscribe and modals

Some verbs run an **extension effect**: a typed change an adapter ships
of its own, which uma queues through its extension call and authorizes
as the policy action the effect's definition names. They are not
generic: heo has no verb that runs an effect by name, and each verb here
takes the fields of the one effect it runs. `conversation update` is the
one that does not name an effect: it runs the update effect the channel's
adapter declares for its work items (shipped as authorized by
`work_item.act`), and passes `--field` keys through, so no provider is
named in heo.

| Verb | Effect | Fields | Authorized as |
| --- | --- | --- | --- |
| `conversation update` | the adapter's work item update effect (`github.issues.update`, `zendesk.tickets.update`), found as the one it ships authorized as `work_item.act` | `--field key=value`, passed through; the adapter validates them | `work_item.act` |
| `conversation tag` | `google.mail.labels` | `--add`, `--remove`: label IDs | `conversation.label` |
| `conversation tag` | `microsoft.mail.categories` | `--set`: the whole list of category names | `conversation.label` |
| `message reply-draft` | `google.mail.reply_draft`, `microsoft.mail.reply_draft` | `--text`; `--draft` to replace or, with `--delete`, remove one | `message.reply` |
| `conversation unsubscribe` | `google.mail.unsubscribe`, `microsoft.mail.unsubscribe` | `--method` (`auto`, `one_click`, `mailto`); no URL, no address | `extension.execute` |
| `interaction open-modal` | `slack.chat.views.open` | `--view`; the interaction's trigger | `extension.execute` |
| `interaction push-modal` | `slack.chat.views.push` | `--view`; the interaction's trigger | `extension.execute` |
| `interaction update-modal` | `slack.chat.views.update` | `--view`, `--view-id` or `--external-id`, `--hash` | `extension.execute` |

The capability alone does not enable them. `extension_execute` says an
adapter runs *some* effect of its own, and uma accepts any registered
effect for any adapter that declares it, so heo also requires the
adapter to **ship the effect the verb runs**. `github.issues` declares
`extension_execute` for its issue update, `zendesk.tickets` for the
ticket update and the internal note, `google.mail` for the label change,
the reply draft and the unsubscribe, `microsoft.mail` for the category change,
the reply draft and the unsubscribe, `slack.chat` for its three
modal effects; none of them enables the verb of another. The check is offline, from the binary, and runs at the
preview and again before the request is submitted, so a request that uma
alone would have queued and the adapter would have refused at the
attempt is refused here, exit 2 (`UNSUPPORTED`), before anything is
written. `heo channel get` lists each verb, and for a verb whose effect
the adapter lacks, names the effect.

All are the queued kind (see above), committed under
`--idempotency-key`, and all take the adapter's own offline check of the
fields at the preview (`VALIDATION`, exit 2); uma makes it again on
submission. None reads the provider: heo does not know which labels,
categories, logins, milestones or views exist, and the provider's answer to
an unknown one is the operation's failure. All are previewed in full by
`--dry-run`, and the preview's `capability` line names the capability
and the effect, and says that live availability, the provider account's
rights, was not checked. Details are in [issues-and-tickets.md](issues-and-tickets.md#change-fields),
[conversations.md](conversations.md#tag-a-mail-message-at-the-provider) and, for the
modals, [Slack modals](#slack-modals).

## History

```sh
heo history fetch --conversation conv_123 --dry-run
heo history fetch --conversation conv_123 --since 2026-09-01
heo history fetch --channel support --since 2026-09-01 --limit 200
heo history fetch --channel support-mail --since 2026-01-01 --until 2026-09-01
heo history fetch --conversation conv_123 --cursor pg.42
```

`history fetch` reads a window of the messages the provider still holds
for a conversation or a whole channel and records each one heo does
not have as inbound, as if it had arrived then. It is for a
conversation that began before heo saw it.

- Exactly one of `--channel` and `--conversation`. `--since` takes an
  RFC 3339 time or a date; without it the window starts where the
  provider's begins. `--until` ends the window (the same forms, the
  window is `since <= t < until`; without it the window has no upper
  end); it must be after `--since`, and the report and the preview echo
  it.
- It is bounded: `--limit` messages are read per call (default 100, at
  most 500). When the window is longer, the report names a `next` cursor
  and the text says so; pass it as `--cursor` to resume.
- Each message is imported once, by its provider message ID. Running it
  again imports nothing: the report counts those as `skipped`. A message
  that arrived live is skipped too, and left as it is.
- Imported history never makes a conversation unread and never wakes a
  snoozed one.
- The report says what it found: `imported`, `skipped`, and `gaps`, the
  pages the provider marked as missing part of the window (messages past
  its retention, or out of the account's reach). A gap means the history
  is not whole, and heo cannot fill it; `gap: true` in JSON, and the text
  says so. `complete: true` says only that the window was read to its
  end. heo never says the history is whole, and never replays what the
  provider no longer holds.
- A failure partway keeps what was imported before it, reports how much,
  and a rerun skips it.
- An adapter that does not declare `history` is refused, exit 2, before
  anything is read, at the preview and on the real run.
- `--dry-run` reads nothing from the provider and writes nothing; it
  names the scope, the window and what it checked of the adapter.

### Mail history

`imap.mail`, `google.mail` and `microsoft.mail` declare `history`, and
`history fetch` is the same command on them. What is mail's:

- It reads received mail only: the receive mailbox (the inbox, or
  `mailbox` on IMAP), oldest first, by the provider's received time
  (Gmail `internalDate`, Graph `receivedDateTime`, IMAP `INTERNALDATE`),
  which is what `--since` and `--until` bound, not the message's `Date`
  header. Sent mail, drafts, spam or junk and trash are never read. Each
  message is recorded as the receiver would have recorded it, with its
  replies linked to a parent the inbox already holds, so a parent
  comes before its replies when both are in the window.
- The mailbox is the channel's, not the call's: there is no `--mailbox`
  flag. To read other mailboxes (an archive folder, a label), list them in
  the channel's `history_mailboxes` (`heo channel update --configuration`);
  see [adapters.md](adapters.md#imap-mailbox-imapmail). Mail in a folder
  the channel does not list is not imported.
- A `--conversation` import reads the mail of the conversation's reply
  address, the sender: every message from that address in the channel's
  mailboxes, not only the conversation's thread. Each message is recorded
  in the conversation it belongs to, and the ones heo holds are skipped.
  `--channel` reads the whole mailbox.
- Attachments arrive with their bytes: `attachment list` and `attachment
  download` work on imported mail.
- To fill in the past beside a running receiver, set `--until` at the time
  the receiver first started: the window then ends where live mail
  begins, and what both read is skipped. A live reply that arrived before
  its older parent was imported stays unlinked, and the parent starts a
  conversation of its own.
- The inbox the mail goes to must exist (`heo inbox create` with the
  mailbox address as its destination) or the import stops with
  `NOT_FOUND`; a `per_thread` inbox refuses a message with no thread
  reference.
- Gaps: a listed message that cannot be read, or an IMAP mailbox whose
  `UIDVALIDITY` changed during the import, counts as a gap. Mail the
  provider deleted or no longer keeps is not a gap and is not reported.
- A throttled provider stops the call with its retry time; the report
  names the cursor of the page being read, and running it again resumes
  there. The import does not wait.
- Gmail's `gmail.metadata` scope cannot search, so a token with only it is
  refused with `UNSUPPORTED`.

There is no operation and no idempotency key: the import is not queued,
and its idempotence is the provider message ID. It reads the provider
page by page for as long as the provider takes, and heo's REST, gRPC and
MCP surfaces serve one call at a time, so `history fetch` is withheld
from them, as the streaming commands are, and runs on the control socket
(the terminal). Who may import is in the table below.

## What each maintained adapter declares

Read from the adapters' descriptors in this build
(`heo adapter list --format json` has a `verbs` list per adapter). The
fake provider heo's own tests drive, and the web widget, are run end to
end through heo; `github.issues` is also replayed from cassettes recorded
against a private sandbox, and `discord.chat`'s edit, deletion and reactions
from cassettes recorded in a throwaway server; `slack.chat`, `telegram.messages`, `relay.messages`, `discord.chat` and `microsoft.teams` run in process
against a scripted Web API, Bot API, relay gateway, Discord and Bot Connector; `google.mail` and `microsoft.mail` `history fetch` is replayed from the
answers uma recorded from a private mailbox, and `imap.mail`'s runs against an in-process IMAP server. The others are declared, not yet run against a live provider or a
recorded cassette by heo's tests. What backs each
adapter and verb, and the maturity apart from the capability, is in
[provider-matrix.md](provider-matrix.md).

| Adapter | `message edit` | `message delete` | `reaction add/remove` | `receipt send` | `history fetch` | `interaction respond` | `interaction open-modal`, `push-modal`, `update-modal` | `conversation status` | `conversation update` | `conversation tag` | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `native.webwidget` | yes | yes | yes | yes | no | yes | no | no | no | no | any message of a visitor's log, public audience only; exercised end to end. Answers a visitor's press or command in each mode, follow-ups included; the widget states no respond window, and an ephemeral answer stays on screen for 15 minutes |
| `github.issues` | yes | yes | yes | no | yes | no | no | yes | yes | no | edit, delete and react need channel configuration `replies` (with `account_id`); until it is on they report `disabled`. Reactions: 👍 👎 😄 😕 ❤️ 🎉 🚀 👀 only. An issue description can be edited, not deleted. The status change needs no `replies`: close (reason `completed` or `not_planned`) and reopen. The field update (assignees, labels, milestone, each a whole set) needs no `replies` either, and is previewed and queued in process, not yet recorded |
| `gitlab.issues` | yes | yes | no | no | yes | no | no | yes | no | no | declares the same close and reopen as `github.issues` and no reasons, so `conversation status` works on it; no update effect, so the provider's fields are ingest-only; edit and delete need `replies`; run in process, not recorded |
| `slack.chat` | yes | yes | yes | no | yes | yes | yes | no | no | no | edit and delete only the app's own messages; 26 reaction emoji; answers a button press or command at its response URL in each mode, follow-ups included; opens, stacks and updates a modal view in answer to a press or command, inside the three seconds Slack gives its trigger_id; history reads are slow for apps outside Slack's Marketplace; run in process, not yet recorded |
| `telegram.messages` | yes | yes | yes | no | no | yes | no | no | no | no | edit and delete only the bot's own messages, and delete within 48 hours; one reaction per message from the 73 emoji Telegram lists, a new one replacing the old; answers a button press as a message, a notification (callback query) or an edit of the button's message, once each, within 30 seconds, and a command only as a message; no history: the Bot API cannot read past messages; run in process, not yet recorded |
| `relay.messages` | no | no | no | no | no | no | no | no | no | no | declares none of the provider-dependent verbs: it sends text and receives messages, and the relay contract has no edit, deletion, reaction, receipt or history; run in process, not yet recorded |
| `discord.chat` | yes | yes | yes | no | yes | yes | no | no | no | no | edit and delete only the bot's own messages; any Unicode emoji, several per message; answers a command by editing its loading message (public) and a button press or modal as a follow-up (ephemeral on request) or an edit of the control's message, within 15 minutes of the interaction; participant verbs add and remove a user of a thread only; edit, deletion and reactions recorded in a throwaway server, threads and interaction answers run in process |
| `microsoft.teams` | yes | yes | yes | no | no | no | no | no | no | no | edit and delete only the bot's own messages; six reaction types (👍 ❤️ 😆 😮 😢 😡); no `interaction respond`: Teams gives an `Action.Execute` no response credential, so answer with `message reply` or `message edit` of the card's message; no history: the Bot Connector cannot read past messages; run in process, not yet recorded |
| `meta.whatsapp` | no | no | yes | no | no | no | no | no | no | no | one reaction per user per message; a new emoji replaces the old |
| `twilio.sms` | no | no | no | no | yes | no | no | no | no | no | reads the account's inbound texts |
| `zendesk.tickets` | no | no | no | no | no | no | no | no | yes | no | the update extension, found by its work item act authorization: `conversation update` sets a ticket's status and fields; no work item act, so no `conversation status`; see [issues-and-tickets.md](issues-and-tickets.md) |
| `google.mail` | no | no | no | no | yes | no | no | no | no | yes | received mail from the inbox or listed labels, with attachment bytes ([mail history](#mail-history)), replayed from uma's recording of a private mailbox; the label change on the message a conversation replies to (`--add`, `--remove`: label IDs) needs the Gmail modify scope and is previewed and queued in process, not yet recorded |
| `microsoft.mail` | no | no | no | no | yes | no | no | no | no | yes | received mail from the inbox or listed folders, with attachment bytes ([mail history](#mail-history)), replayed from uma's recording of a private mailbox; the category change on the message a conversation replies to (`--set`: the whole list) needs `Mail.ReadWrite` and is previewed and queued in process, not yet recorded |
| `imap.mail` | no | no | no | no | yes | no | no | no | no | no | received mail from the mailbox or listed mailboxes, with attachment bytes ([mail history](#mail-history)); run against an in-process IMAP server, no recording is possible |
| `smtp.mail`, `twilio.whatsapp` | no | no | no | no | no | no | no | no | no | no | these providers have no such call |

Only `discord.chat` declares `membership` (the members of a thread), so
`participant add` and `participant remove` are refused everywhere but there
and on the fake provider heo's tests drive. `smtp.mail` and Twilio
WhatsApp channels refuse every provider verb, exit 2, at the preview and on
submission; `imap.mail` refuses all but `message raw`, `history fetch` and the nine
conversation state verbs; `google.mail` and `microsoft.mail` refuse all but
those, `message raw`, `conversation tag`, `message reply-draft` and `conversation
unsubscribe`, and the tag verb takes only the flags of its own effect
(`--add` and `--remove` for Gmail, `--set` for Outlook); `slack.chat` and
`discord.chat` declare `conversation archive` and `conversation unarchive`
only. `github.issues` shows the `replies` switch in
`heo channel get` as reason `config`, and in `heo channel check --remote` as
`disabled`.

## Who may do what

| Command | Role |
| --- | --- |
| `message edit`, `message delete`, `reaction add`, `reaction remove`, `conversation status`, `conversation update` | `desk` (policy actions `message.edit`, `message.delete`, `message.react`, `work_item.act`; the adapter's update effect is authorized as `work_item.act` by its definition) |
| `conversation tag` | `desk` (policy action `conversation.label`, which uma's definition of both mail effects names; the same action as `conversation label`) |
| `message reply-draft` | `desk` (policy action `message.reply`, which uma's definition of both draft effects names; the same action as `message reply`) |
| `conversation unsubscribe` | `desk` (policy action `extension.execute`, which uma's definition of both unsubscribe effects names: a request to a third party, so its own permission, not a label's or a reply's) |
| `conversation archive`, `unarchive`, `move`, `read`, `unread`, `flag`, `unflag`, `trash`, `untrash` | `desk` (policy action `conversation.state_account` when the channel reports account scope, a mailbox's own view, `conversation.state_shared` when it reports shared scope, a chat room or a tracker; uma picks it from the scope and authorizes it as the caller at submission and again before every attempt) |
| `participant add`, `participant remove` | `desk`, on the conversation's internal audience (policy action `conversation.membership`) |
| `receipt send` | `desk` (policy action `message.receipt`) |
| `interaction respond` | `desk` (policy action `interaction.respond`, uma's, on the interaction) |
| `interaction open-modal`, `interaction push-modal` | `desk` (policy action `extension.execute`, which the effect's definition names, on the interaction's conversation, internal audience; heo also requires the right to answer interactions, `interaction.respond`, and to read the interaction) |
| `interaction update-modal` | `desk` (policy action `extension.execute`, on the inbox, internal audience) |
| `history fetch --conversation` | `desk`, which also reads the conversation (policy action `channel.import`, uma's, on the conversation) |
| `history fetch --channel` | `operator`, which also reads channels (`channel.import` on the channel) |
| `reaction list`, `participant list` | any role that reads the conversation (`desk`, `viewer`, `external`) |
| `interaction list`, `interaction get` | `desk` and `viewer` (policy action `interaction.read`); `external` reads none |
| `adapter list`, `adapter get` | anyone (offline) |
| `channel get`, `channel check` | `operator` |
| the verbs table of `conversation get` | `desk` |

A viewer or external actor is refused (exit 5) on the writes, preview
included. A desk actor asking for a whole channel's history, or an
operator without the desk role asking for a conversation's, is refused
the same way. The import itself runs as the runtime's own actor, the one
that receives on channels; the actor who asked is recorded on each
imported message. See [authorization.md](authorization.md).
