# How providers map onto conversations

heo never models a provider's own objects. Every channel adapter maps what
its provider has (a mail thread, a Slack channel, a GitHub issue) onto the
same few records, and every heo verb works on those records:

- A **conversation** is one exchange in one inbox.
- A **message** is one entry in it: a mail, a chat message, an issue comment.
- A **work item** is a tracker's issue or ticket. Its status is the
  provider's and lives beside the conversation; it never moves the
  conversation's own status, and resolving a conversation never closes the
  issue ([issues-and-tickets.md](issues-and-tickets.md)).
- A message's **reply parent** is the message it answers, when the provider
  says so.

Which provider object becomes a conversation is decided by the inbox's
**mode** (`heo inbox create --mode`), not by heo.

## Inbox modes

| Mode | A conversation is | Works on |
| --- | --- | --- |
| `per_contact` | one person: with `--lock-to-single true` every message from them, with `false` one per thread they start | every adapter; the only mode for mail, SMS and WhatsApp |
| `per_room` | one room: a channel, a group or a direct message, with its threads inside it | Slack, Teams, Discord, Telegram, the web widget (the session) |
| `per_thread` | one thread; a message outside a thread roots its own | Slack, Teams channels, Discord, Telegram forum topics |
| `per_work_item` | one issue or ticket: the conversation is the work item | GitHub, GitLab, Jira, Linear, Zendesk |
| `per_agent_context` | one agent context | A2A |

A mode an adapter cannot place a message in is refused when the inbox is
created or when the message arrives: mail carries no room, so a mail inbox
in `per_room` or `per_thread` is refused.

## By provider

| Provider | Conversation | Message | Work item | Reply parent |
| --- | --- | --- | --- | --- |
| Mail: `google.mail`, `microsoft.mail`, `imap.mail`, `smtp.mail`, `http.mail` | `per_contact`. A mail answering one in the inbox (In-Reply-To, then References) joins that conversation; otherwise the sender's one conversation (locked) or a new one per thread (unlocked). Gmail's thread and Graph's conversation are hints only | one received mail; sent mail and drafts are not imported | none | the first valid In-Reply-To; a forward or quote is not a reply |
| `slack.chat` | `per_room`: channel, private channel, DM or group DM; `per_thread`: a thread, and every top-level message (DMs too) roots one; `per_contact`: the user across rooms | a message or a thread reply | none | the thread's root |
| `microsoft.teams` | `per_room`: a personal chat, group chat or channel; `per_thread`: a channel thread (chats have none); `per_contact`: the user | an activity | none | `replyToId` |
| `discord.chat` | `per_room`: the channel, its threads' messages included; `per_thread`: the thread channel (a message outside a thread is refused); `per_contact`: the user | a message | none | the referenced message |
| `telegram.messages` | `per_room`: the chat; `per_thread`: a forum topic; `per_contact`: the private chat with the user | a message; a bot command is an interaction | none | the replied-to message |
| `twilio.sms`, `twilio.whatsapp`, `meta.whatsapp`, `relay.messages` | `per_contact` | one SMS or WhatsApp or iMessage message | none | WhatsApp only |
| `native.webwidget` | `per_contact`: the visitor; `per_room`: the session | a chat entry | none | none |
| `a2a.messages` | `per_agent_context`: one A2A context | an A2A message | none (an A2A task is not a work item) | none |
| `github.issues`, `gitlab.issues`, `jira.issues`, `linear.issues` | `per_work_item`: the issue | the description first, then each comment; editing the description edits that message | the issue | none |
| `zendesk.tickets` | `per_work_item`: the ticket | a comment; an internal note stays internal | the ticket (a merged ticket follows the one it merged into) | none |

## What does not carry over

- **Mail recipients.** Cc and Bcc are kept on the message, not as
  participants; a reply goes to the reply route, never to everyone
  (reply-all is not available).
- **Mail history by conversation.** `history fetch --conversation` on mail
  reads every mail from that conversation's sender in the listed mailboxes,
  and each lands in the conversation it belongs to, which may be another
  one. A colleague's reply in the same thread is not from the sender and is
  not fetched.
- **Slack threads in a room.** Live thread replies join a `per_room`
  conversation, but `history fetch` on a room reads only top-level messages
  and reports the gap. Under `per_contact`, a reply is linked to its thread
  only when the thread's root is in the same conversation, and heo's answer
  goes to the user's direct message, not the thread.
- **Slack direct messages.** Under `per_thread` every top-level message in a
  DM roots its own conversation; DMs and group DMs report no archive.
- **Pull requests and system notes.** GitHub pull request comments and
  GitLab system notes are not messages; a deleted GitLab note is never
  reported.
- **Tracker status and fields.** A status change (`conversation status`)
  reaches the provider only along a transition the adapter declares:
  GitHub and GitLab close and reopen; Jira, Linear and Zendesk declare
  none, so the status is the provider's alone there. Zendesk's own status
  and a tracker's other fields (assignees, labels, a milestone) go
  through the update effect the adapter ships for its work items
  (`conversation update --field key=value`), where it ships one: GitHub
  and Zendesk do, GitLab, Jira and Linear ship none, and their fields
  are ingest-only unless the channel's configuration makes them uma's
  ([issues-and-tickets.md](issues-and-tickets.md)).

Each adapter's page in [adapters.md](adapters.md) has its setup and its own
limits; [provider-matrix.md](provider-matrix.md) has what heo has tested
against each.
