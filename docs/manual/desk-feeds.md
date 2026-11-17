# Desk feeds: what a client can read today

A desk client that spans inboxes needs two feeds: an **attention feed**
(every conversation that waits on a person) and an **activity feed**
(what plugin actors did, and how to undo it). heo has no feed command
for either. This page lists what the existing reads give a client over
the CLI, REST and MCP, and what is missing.

The findings come from one probe run against a throwaway profile with
the fake channel adapter, two inboxes, a person and a plugin actor with
held operations and the trust ladder on (see [approvals.md](approvals.md)).
Nothing here is a promise of a future command.

## Surfaces

All reads below are served on the three surfaces unless a row says
otherwise (see [surfaces.md](surfaces.md)): `conversation list`,
`operation list`, `operation get`, `operation held list|show|history`,
`event list` and `event get` are REST routes (GET) and MCP tools
(`conversation.list`, `operation.held.list`, `event.list`, and so on)
with the same flags as the CLI. `event watch` is socket-only.

## Attention feed

Wanted: across inboxes, conversations unread for the actor, open or
pending, not snoozed, plus held operations; newest change first; a
cursor or an event to refresh.

| Part | Status | What exists, or the gap |
| --- | --- | --- |
| Across every inbox the actor may read | exists | `conversation list` without `--inbox` aggregates, a page at a time (`--limit`, `--cursor`) |
| Unread for the actor | exists | `--unread`; the marker is per actor |
| Open or pending | exists | `--status open,pending` |
| Not snoozed | exists | the default leaves snoozed ones out; `--snoozed only\|include` change it |
| Assigned to the actor | exists | `--assignee self` |
| Held operations alongside | partial | `operation held list` is a second read, newest change first, with `--inbox`, `--kind`, `--since`; no conversation filter reaches it from `conversation list`, and a held operation on a label or other tenant-wide target has no conversation |
| Newest change first | exists | `conversation list --sort last-activity`; each row carries `last_activity`: the newest of its newest readable message, its snooze start and the actor's own read marker update; a new message, a read marker move or a snooze start moves it to the top; on CLI, REST and MCP |
| Changes since a point | exists | `conversation list --updated-since TIME` keeps conversations whose `last_activity` is after it; no unread count on the record, `--unread` filters; heo derives the activity from messages, snooze and marker, reading each conversation the other filters keep |
| One merged page | missing | conversations and held operations are two lists with two cursors; the client merges them |
| Cursor to refresh | partial | `event list --cursor` returns committed events since a position and works on all surfaces; the events cover new messages (`message.received`), `conversation.changed`, `conversation.snoozed` and `operation.held`/`operation.decided`; marking read or unread and a snooze waking emit nothing, so an unread count cannot be kept from events alone |
| Push | partial | `event watch` streams on the socket; not on REST or MCP, so a web or MCP client polls `event list` |

## Activity feed

Wanted: per inbox, since a cursor, the operations plugin actors applied
(held then approved, or applied automatically under trust), with the
verb, the target and the inverse verb that undoes it.

| Part | Status | What exists, or the gap |
| --- | --- | --- |
| Held then approved or rejected | exists | `operation held list --state ...` lists held, queued, committed, rejected, expired and the delivery states; `operation held history` gives proposed, edited, approved, rejected and expired records with who, when, note and the preview shown (`--format jsonl`) |
| Per inbox | partial | `--inbox` on `held list`, `held history` and `operation list`; `event list --inbox X --kind operation.held,operation.decided` returns the operations held for, and decided on, the conversations and messages of inbox X on CLI, REST and MCP; an operation whose target has no inbox (a label, a channel or an inbox itself) carries an empty one and is not returned by an inbox filter |
| Since a cursor | partial | `--since` and `--until` filter by time on `held list`, `held history` and `operation list`; `held history` has no `--cursor` and returns everything it matches; `held list` and `operation list` page with `--cursor` |
| Applied automatically under trust | missing as a record | a trusted write is never held, so it is absent from `held list` and `held history`; it appears only in `operation list` read as the plugin's own token, and as `message.updated` and `delivery.updated` events with the plugin as actor; nothing marks it as automatic or names the trust key |
| Plugin actors' operations as a person | missing | `operation list` shows the caller's own operations only (a person sees their snoozes and channel setup, not the plugin's writes); `operation held list` and `held history` do read the plugin's held ones |
| Verb | partial | the operation has a `kind` (`message.edit`, `conversation.transition`, `label.configure`), not the CLI verb; a held operation shows the trust key and its labels (`verb`, `class`) |
| Target | exists | `target` on the operation (`message:<inbox>/<id>`, `conversation:<inbox>/<id>`, `label:<id>`) |
| Inverse verb | missing | no read names it; a held preview's effects carry only `reversible` (true or false) and a detail; the undo pairs (`archive` and `unarchive`, `trash` and `untrash`, `flag` and `unflag`) exist only in the command help |
| Stream | partial | `event list` on all surfaces, `event watch` on the socket; `operation.held`, `operation.decided` and `operation.expired` are internal-visibility events, readable by desk actors, each with the target's inbox, the operation's `verb` (its kind) and its `target` in the payload |

## Probe method

A throwaway data directory and profile on a build machine, a fake
channel adapter (no real account), a plugin token and a person token,
the three surfaces served on loopback. The probe injected inbound
messages in two inboxes, snoozed and marked conversations read, had the
plugin propose a label create and a conversation resolve (held), applied
one edit automatically after importing trust, approved the label, and
read every list above on each surface. The script is not kept in the
repository; the same steps run from the end-to-end helpers.
