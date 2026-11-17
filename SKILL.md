---
name: heo
description: "Use heo to read, reply to and recover messages in a shared inbox from the terminal or over REST, gRPC or MCP: mail, web widget, SMS, WhatsApp, GitHub issues and Zendesk tickets through uma. Triggers: unified inbox, reply to a customer, list conversations, add an internal note, label or resolve a conversation, merge contacts, delivery stuck or outcome unknown, retry or reconcile a send, heo service start or setup, heo spec."
---

# heo

Messaging CLI and daemon for uma. This file is for anyone, human or agent, who wants to USE heo.
To change heo, read [docs/DEVELOPING.md](docs/DEVELOPING.md).

## Before any call

1. A daemon owns the data store. Daemon-backed verbs with no daemon exit 70 (`PREREQUISITE`) and
   name the fix; nothing starts one for you. A human runs `heo service start`.
2. `heo service status` says whether every service is ready.
3. Ask the tool, not this page: `heo spec --format json` lists every verb with its side effect,
   flags, examples and next steps. The [command reference](docs/manual/command-reference.md) is
   rendered from it.

## Rules

- Pass `--format json` and parse stdout. Errors are a JSON envelope on stderr: `code`, `message`,
  `suggested_fix`, `transience`.
- IDs are record IDs as a listing printed them (`conv_123`, `msg_123`, `op_123`, `ct_123`).
  Never construct one.
- Preview every write with `--dry-run` first. It prints the exact effect and writes nothing.
- Writes are uma operations. Keep `--idempotency-key` (yours, or the one heo printed) so a retry
  replays. A reply or note returns `queued`: accepted, not delivered.
- Writes that change a record take `--revision` from the read you just did. A stale one exits 4
  (`CONFLICT`): re-read, then decide.
- Exit 6 is transient: back off. Exit 8 is `OUTCOME_UNKNOWN`: the provider may have acted, so
  `heo delivery reconcile`, never a blind resend. Exit 5 is a refusal: do not retry unchanged.
- Destructive verbs (`contact merge`, `message delete`, `storage restore`) need `--confirm=yes`
  without a terminal, and are served over the control socket only.
- Held writes (`held_operations.enabled`): a plugin actor's write waits for a person. Only a
  person approves or rejects; a plugin is refused (exit 5), on every surface. MCP cannot name the
  operation of `held show|approve|reject`: use REST, gRPC or the terminal.
- Internal notes are for the `desk` role alone. A note is never a reply.

## Intent to command

| I want to | Run | Read |
| --- | --- | --- |
| see what is waiting | `heo conversation list --status open` | [conversations](docs/manual/conversations.md) |
| read a conversation | `heo message list --conversation conv_123` | [conversations](docs/manual/conversations.md) |
| search messages | `heo message list --text refund --since 2026-10-01` | [conversations](docs/manual/conversations.md#search-messages) |
| reply | `heo message reply conv_123 --text "On it." --dry-run`, then without `--dry-run` | [conversations](docs/manual/conversations.md#reply) |
| write to someone first | `heo message send --inbox support --to address:ada@example.com --text "Hello." --dry-run` | [conversations](docs/manual/conversations.md#start-an-exchange) |
| leave a note | `heo note add conv_123 --text "Legacy plan." --placement local` | [conversations](docs/manual/conversations.md#notes) |
| summarize, triage or draft a reply with a local model | `heo conversation summarize conv_123 --dry-run`; `heo conversation triage conv_123`; `heo message draft conv_123 --format text` | [LLM helpers](docs/manual/llm-helpers.md) |
| label, resolve, reopen | `heo conversation label conv_123 --add billing`; `heo conversation resolve conv_123 --revision rev_123` | [conversations](docs/manual/conversations.md#labels) |
| assign, snooze, link, mark read | `heo conversation assign conv_123 --to self`; `heo conversation snooze conv_123 --for 2h`; `heo conversation list --assignee self --unread` | [conversations](docs/manual/conversations.md#assign-snooze-link-and-read) |
| list or create a mail provider's own labels or folders | `heo provider-label list --format json`; `heo provider-label create Clients/Acme --channel support-mail` | [conversations](docs/manual/conversations.md#list-and-create-provider-mailboxes) |
| export a mail message's RFC 5322 bytes | `heo message raw msg_123 -o message.eml` | [conversations](docs/manual/conversations.md#export-a-messages-raw-bytes) |
| fetch an attachment | `heo attachment download inb_1/att_1 -o invoice.pdf` | [conversations](docs/manual/conversations.md#attachments) |
| find who someone is | `heo contact list`; `heo identity list --contact ct_123` | [contacts](docs/manual/contacts.md) |
| merge two contacts | `heo contact merge ct_456 ct_123 --reason same_person --dry-run` | [contacts](docs/manual/contacts.md#merge-two-contacts) |
| know whether a reply left | `heo delivery get op_123` | [deliveries and events](docs/manual/deliveries-and-events.md) |
| recover a lost answer | `heo operation get --key reply-42` | [deliveries and events](docs/manual/deliveries-and-events.md#recover-a-write-whose-answer-was-lost) |
| settle an unknown outcome | `heo delivery reconcile op_123`; a dead letter: `heo delivery retry op_123` | [deliveries and events](docs/manual/deliveries-and-events.md) |
| review what a plugin proposed | `heo operation held list`; `heo operation held show op_123` | [approvals](docs/manual/approvals.md) |
| approve or reject a held write (people only) | `heo operation held approve op_123 --dry-run`, then without; `heo operation held reject op_123` | [approvals](docs/manual/approvals.md#decide) |
| see who decided what | `heo operation held history --format jsonl` | [approvals](docs/manual/approvals.md#audit) |
| follow events | `heo event list --from tail`; `heo event watch` | [deliveries and events](docs/manual/deliveries-and-events.md#list-events) |
| react to events with your own program (any language) | `heo plugin token mint triage`; `heo hook test triage --event ev_123`; `heo hook list` | [write a plugin](docs/manual/hooks.md#write-a-plugin-in-any-language) |
| set the status of an issue or ticket (close, reopen) | `heo conversation status conv_123 --set closed --reason completed --dry-run` | [issues and tickets](docs/manual/issues-and-tickets.md#set-the-status) |
| change an issue's or ticket's assignees, labels, milestone or fields | `heo conversation update conv_123 --field 'labels=["bug","ui"]' --dry-run` | [issues and tickets](docs/manual/issues-and-tickets.md#change-fields) |
| first consent of a Gmail or Microsoft 365 mailbox (terminal, opens a browser) | `heo channel authorize gmail --client-id ID --dry-run`, then without | [channels](docs/manual/channels.md#authorize-a-mailbox) |
| tag a mail message at Gmail or Outlook | `heo conversation tag conv_123 --add Label_12 --dry-run`; `heo conversation tag conv_123 --set Billing` | [conversations](docs/manual/conversations.md#tag-a-mail-message-at-the-provider) |
| reply to all, or cc and bcc, on a mail reply (needs `reply_recipients`) | `heo message reply conv_123 --text "Looping you in." --reply-all --cc lead@example.com --dry-run` | [conversations](docs/manual/conversations.md#reply-to-all-cc-and-bcc) |
| keep a reply as a draft in Gmail or Outlook (sends nothing) | `heo message reply-draft conv_123 --text "Thanks." --dry-run` | [conversations](docs/manual/conversations.md#keep-a-reply-as-a-provider-draft) |
| unsubscribe from a mail sender (external, irreversible) | `heo conversation unsubscribe conv_123 --dry-run` | [conversations](docs/manual/conversations.md#unsubscribe-from-a-sender) |
| archive or unarchive a conversation at its provider (not uma's status or labels) | `heo conversation archive conv_123 --dry-run` | [conversations](docs/manual/conversations.md#change-a-conversations-provider-state) |
| file a conversation at its provider: a role (junk is spam), a folder or labels | `heo conversation move conv_123 --role junk --dry-run` | [conversations](docs/manual/conversations.md#change-a-conversations-provider-state) |
| mark read at the provider (`mark-read` is only your marker), star, trash | `heo conversation read conv_123`; `... flag conv_123`; `... trash conv_123` | [conversations](docs/manual/conversations.md#change-a-conversations-provider-state) |
| edit, delete, react | `heo message edit msg_123 --revision rev_123 --text "Fixed."` | [provider verbs](docs/manual/provider-verbs.md) |
| list and answer a chat provider's button presses and commands | `heo interaction list`; `heo interaction respond ix_123 --mode ephemeral --text "Done." --dry-run` | [provider verbs](docs/manual/provider-verbs.md#interactions) |
| open or change a Slack modal (a trigger lives 3 s) | `heo interaction open-modal ix_123 --view v.json --dry-run`; `heo interaction update-modal --inbox rooms --view-id V1 --view v.json` | [provider verbs](docs/manual/provider-verbs.md#slack-modals) |
| connect a provider | `heo adapter get smtp.mail`; `heo channel create`; `heo inbox create` | [channels](docs/manual/channels.md), [adapters](docs/manual/adapters.md) |
| run the daemon | `heo service start`, `heo service status`, `heo service stop` | [runtime](docs/manual/runtime.md) |
| call heo over REST, gRPC or MCP | `heo service setup api mcp` | [surfaces](docs/manual/surfaces.md) |
| switch profile or bind aps | `heo --profile work config list`; `heo --profile aps:noor config list` | [configuration](docs/manual/configuration.md#profiles) |
| back up, migrate, upgrade | `heo storage backup`; `heo storage migrate`; `heo upgrade --dry-run` | [upgrade](docs/manual/upgrade.md) |
| it failed | read the exit code and `suggested_fix` | [troubleshooting](docs/manual/troubleshooting.md) |

Roles and what each may do: [authorization](docs/manual/authorization.md). Every verb with its
side effect and uma backing: [command reference](docs/manual/command-reference.md). Cheatsheets:
[agents](docs/cheatsheet-agent.md), [people](docs/cheatsheet-human.md). First run:
[install](docs/manual/install.md) and [getting started](docs/manual/getting-started.md).

## Not in this release

Some verbs the grammar names do not exist: `channel enable|disable` (availability is the inbox's)
and `message search` (it is `message list --text`).
[Grammar coverage](docs/manual/command-reference.md#grammar-coverage) lists each with its reason.
`held` editing, deciding by selector and trust import are not available
([approvals](docs/manual/approvals.md#what-cannot-be-held)).
Discovery (`heo spec`) is the authority: do not probe for a verb.
