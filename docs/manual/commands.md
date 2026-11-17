# Commands Reference: heo

What each command does, in prose. For one table of every verb with its
side effect, who may run it and which uma call backs it, see the
generated [command reference](command-reference.md); for the verbs the
grammar names and this build lacks, its
[Grammar coverage](command-reference.md#grammar-coverage).

## Global Flags

```
--help, -h        Show help
--version, -v     Show version
--verbose, -V     Increase log verbosity
--format FMT      Output format (csv|human|json|table|text|yaml)
--profile REF     Profile: <name>, aps:<id>, heo://profile/<name>,
                  aps://profile/<id> (env HEO_PROFILE)
```

## Commands

### `config`

Inspect and change configuration for the selected profile. See
[configuration.md](configuration.md).

```sh
heo config list | get <key> | set <key> <value> | validate | path | paths
```

`validate` also runs `channel check` over every stored channel and
inbox through the running daemon; with none running it reports them
unchecked (`channels.skipped`) rather than opening the store.

### `service`

Run, inspect, stop and restart the selected profile's daemon. The group
is kit's; heo adds `service setup`.

```sh
heo service start [service]   # run in the foreground until stopped
heo service list              # registered services and their state
heo service status            # the running daemon: services, readiness, heo sections
heo service stop              # drain the running daemon and wait for it to exit
heo service restart           # drain, then run again in the same process
heo service setup [api|rpc|mcp ...]   # enable REST, gRPC or MCP over HTTP with the profile's token
```

Commands that need the runtime run in the daemon: `heo` sends them over
the profile's control socket and prints the daemon's answer. With no
daemon running they exit 70 (`PREREQUISITE`) naming `heo service
start`; nothing starts one for you. See [control.md](control.md).

`service start` with no argument starts every enabled service: the
control socket (`socket`) and the runtime services `workers` and
`receivers` (see [runtime.md](runtime.md)). Name one service to start
exactly that one, or add others for one run with
`--enable <service>`:

```sh
heo service start --enable rpc --rpc-addr 127.0.0.1:8082   # needs an authenticator
heo service start mcp --stdio                               # relays to the running daemon
```

REST, gRPC and MCP over HTTP refuse to start without an authenticator
(the profile's surface token, or `services.<service>.auth.mode` or
`services.all.auth.mode`), loopback included (exit 2). `mcp --stdio` relays every daemon command over the
control socket and refuses to start with no daemon running (exit 70).

**Flags:** `--enable`, `--disable`, `--socket`, `--addr` (api),
`--rpc-addr`, `--mcp-addr`, `--stdio`, `--ready-timeout`,
`--stop-timeout`, `--shutdown-timeout`, `--policy`, `--dry-run`.
`--mcp-addr` with `--stdio` is a usage error (exit 2). `--no-auth`,
`--insecure-remote` and `--insecure-no-policy` are refused (exit 2).
The address flags are per-run overrides, read by their service and
above every configuration layer for their key, `-c key=value`
included.

`--policy <name>` names the policy served calls run under:
`kit-default`, or a file at `<user config>/heo/policies/<name>.yaml`
(see [configuration.md](configuration.md)). It does not gate the start
itself.

One daemon per profile. Each profile's daemon has a control socket of
its own, so `service stop`, `restart` and `status` reach the daemon of
the selected profile (`--profile`) and no other. A second `service
start` for a profile whose daemon runs exits 4 (`CONFLICT`) before
anything starts.

| Command | No daemon | Otherwise |
| --- | --- | --- |
| `service status` | exit 3 (`NOT_FOUND`) | kit's rows (service, ready, address, pid), then heo's sections: `heo` (profile, tenant, version, pid), `storage`, `store-owner`, `workers`, `receivers`, `webhook`, `queue`, `versions` (see [runtime](runtime.md#status)) |
| `service stop` | exit 0, `not_running` | drains like the first SIGTERM, waits (`--wait`), exit 0 `stopped`; still draining at the bound: exit 6 |
| `service restart` | exit 3 (`NOT_FOUND`) | drains, re-executes in place (same pid), re-reads the configuration, prints the new run's status |

Each service logs `ready_reported ... address=<addr>` when it is ready.
SIGTERM, SIGINT or SIGHUP drains every service and exits 0; a second
signal aborts the drain and exits 1. A store another daemon owns, a store whose schema is
not this heo's, and two channels on one provider account refuse the
start (exit 1); see [runtime.md](runtime.md).

No served surface runs `service start`. The rest of the group is
management vocabulary, withheld from every served surface, the
profile's control socket included: `service status`, `stop`
and `restart` reach the daemon over kit's own control channel.

`service setup` enables REST, gRPC or MCP over HTTP for the profile,
on loopback: it mints the profile's surface token into the credential
backend (kept on a re-run), writes `services.<service>.enabled`,
`services.<service>.addr` and `credentials.surface_token_ref` to the
profile file, and prints how to call each surface. On a terminal it
asks for what the arguments left out; anywhere else it never prompts,
and with nothing to do exits 2. See [surfaces.md](surfaces.md).

```sh
heo service setup api mcp                         # loopback, default or next free ports
heo service setup rpc --addr rpc=127.0.0.1:9082   # loopback IPs only
heo service setup --rotate-token --show-token     # replace the token, print it once
heo service setup api --dry-run                   # the plan; nothing written
```

**Flags:** `--addr <service>=<host:port>` (repeatable), `--disable
<service>` (repeatable), `--rotate-token`, `--show-token`, `--dry-run`.
The token is printed only with `--show-token`. A daemon applies the
surfaces on restart; a rotated token is refused from the next request
on.

`heo status` is the tool status: profile (with heo's `heo-profile`
section), environment, auth and effective configuration. It needs no
daemon. `heo serve` is the hidden, deprecated spelling of `service
start` (`serve --list` is `service list`).

### `daemon`

Ask the selected profile's running daemon how it serves you. The group
is heo's own, so the profile's control socket serves it; REST, gRPC
and MCP never do.

```sh
heo daemon ping               # ask the running daemon who you are and how it stands
```

`daemon ping` reports the actor and tenant the daemon serves you as,
its pid and version, and the state of its runtime (data store,
workers, receivers, webhook). It starts nothing: with no daemon
running it exits 70 (`PREREQUISITE`) naming `heo service start`.

### `adapter`

The adapters this binary carries, read from the binary itself: no
daemon, store or network. See [channels.md](channels.md).

```sh
heo adapter list              # type, maturity, capabilities, verbs, configuration schema
heo adapter get <type>        # + credential purposes, permissions, identity namespaces
```

Maturity is reported apart from capabilities. Capabilities are what the
adapter declares; `channel check --remote` shows what a configured
channel can do right now. `verbs` lists the provider-dependent heo
verbs the declared capabilities enable (`message edit` for `edit`, and
so on; see [provider-verbs.md](provider-verbs.md)). An unknown type
exits 3.

### `channel`

Configured connections to providers. Runs in the daemon (exit 70
without one). Operator role. See [channels.md](channels.md).

```sh
heo channel list [--limit N] [--cursor C]
heo channel get <channel>                         # key or ID; shows the revision and the provider verbs it supports
heo channel create --type <type> [--key K] [--configuration JSON] [--credential-ref REF]
heo channel update <channel> --revision REV [--key K] [--configuration JSON] [--credential-ref REF]
heo channel check [channel] [--remote]
heo channel authorize <channel> [--client-id ID] [--client-secret-ref REF] [--scope S,...] [--directory D] [--no-browser] [--replace]   # terminal only: first consent of a Gmail or Microsoft 365 mailbox
```

- **create/update** are uma operations under `--idempotency-key`
  (minted and printed as `operation key` when absent; a retry with the
  same key replays). `--dry-run` prints the exact record and writes
  nothing. A stale `--revision`, a key another channel holds, or a
  provider account another channel receives is `CONFLICT`, exit 4. An
  adapter this build lacks, or a configuration it rejects, exits 2.
  Credentials are references: a value that looks like a credential is
  refused (exit 2). The receivers take changes at the daemon's next
  start.
- **get** shows the channel and `verbs`: for each provider-dependent
  verb, the capability it needs and whether the channel's adapter
  declares it (`declared`, or `unsupported` with a reason:
  `adapter_missing`, `not_declared`, `not_executed`, `config`). Computed
  offline: no adapter is opened, no provider reached. See
  [provider-verbs.md](provider-verbs.md).
- **check** writes nothing. Without `--remote` it reads the data store
  only. `--remote` (one channel) opens the adapter with the channel's
  credentials and reports each capability (available, and required by
  the channel's inboxes), each provider-dependent verb's live state
  (`available`, `unauthorized`, `unavailable`, `unsupported`,
  `disabled`, with its error code and exit status) and each credential
  purpose's diagnosis, never its material. `--remote --offline` exits 2. Any error finding
  exits 2 after the report.

There is no `channel enable`/`disable`: availability is the inbox's.

### `inbox`

Destinations of a channel bound to a selection mode, and the
availability switch. Runs in the daemon (exit 70 without one). See
[channels.md](channels.md).

```sh
heo inbox list [--limit N] [--cursor C]
heo inbox get <inbox>
heo inbox create --channel <channel> --mode MODE [--key K] [--name N] [--destination D]...
                 [--lock-to-single=BOOL] [--lifecycle-rules R] [--selection-policy P] [--deliver-notes]
heo inbox update <inbox> --revision REV [same fields]
heo inbox disable <inbox> --revision REV
heo inbox enable <inbox> --revision REV
```

- **disable** rejects inbound to the inbox. The channel keeps its
  connection and queued sends, replies included, still deliver.
  **enable** accepts inbound again.
- Writes take `--revision`, `--idempotency-key` and `--dry-run` as
  channel writes do. A destination another inbox of the channel
  receives is `CONFLICT`, exit 4.
- Reads are open to every role that reads conversations. Writes need
  the operator role.

### `conversation`

Conversations across every inbox you may read, and their local
lifecycle. Runs in the daemon (exit 70 without one). See
[conversations.md](conversations.md).

```sh
heo conversation list [--inbox I] [--status S,...] [--label L,...] [--label-all L,...]
                      [--label-none L,...] [--unread] [--assignee A] [--snoozed MODE] [--snooze-ended[=false]] [--linked]
                      [--work-item[=false]] [--work-item-status open|pending|resolved|closed,...]
                      [--sort last-activity] [--updated-since TIME] [--limit N] [--cursor C]
heo conversation get <conversation>                 # record ID; shows the revision
heo conversation resolve <conversation> --revision REV
heo conversation reopen <conversation> --revision REV
heo conversation status <conversation> --set open|pending|resolved|closed [--reason R] [--revision WORK_ITEM_REV]   # external: the issue's or ticket's status
heo conversation update <conversation> --field KEY=VALUE ... [--revision WORK_ITEM_REV]                          # external: the issue's or ticket's fields
heo conversation label <conversation> --add L,... | --remove L,...
heo conversation tag <conversation> --add ID,... [--remove ID,...] | --set NAME,...
heo conversation unsubscribe <conversation> [--method auto|one_click|mailto]   # external, irreversible
heo conversation archive <conversation> [--revision REV]      # external: the provider's archive
heo conversation unarchive <conversation> [--revision REV]
heo conversation move <conversation> --to ID | --role ROLE | --add ID,... [--remove ID,...]   # external: where the provider files it (ROLE: inbox, archive, trash or junk)
heo conversation read <conversation> [--revision REV]         # external: the provider's read state, not mark-read
heo conversation unread <conversation> [--revision REV]
heo conversation flag <conversation> [--kind KIND]            # external: a star or an important flag (KIND: star or important)
heo conversation unflag <conversation> [--kind KIND]
heo conversation trash <conversation> [--revision REV]        # external: the provider's trash
heo conversation untrash <conversation> [--revision REV]
heo conversation assign <conversation> --to ACTOR|self [--revision REV]
heo conversation unassign <conversation> [--revision REV]
heo conversation snooze <conversation> --until T | --for D [--revision REV]
heo conversation unsnooze <conversation> [--revision REV]
heo conversation link <conversation> --conversation C | --work-item W [--revision REV]
heo conversation unlink <conversation> --conversation C | --work-item W [--revision REV]
heo conversation mark-read <conversation> [--through MESSAGE]
heo conversation mark-unread <conversation> [--from MESSAGE]
heo conversation summarize <conversation> [--send-to-cloud]
heo conversation triage <conversation> [--send-to-cloud]
```

- **resolve/reopen** are local: nothing changes at the provider. uma
  applies the conversation lifecycle when the transition commits:
  `closed` is terminal, and moving it is `CONFLICT`, exit 4. They take
  `--revision`, `--idempotency-key` and `--dry-run` as channel writes
  do. Desk role.
- **label** adds or removes tenant labels by name, `--add` or
  `--remove` (repeat or comma-separate). Idempotent: adding a held
  label or removing an absent one succeeds and changes nothing. An
  unknown name is `NOT_FOUND`, exit 3, with `heo label create <name>`
  as the fix; heo never creates one. `--idempotency-key` and
  `--dry-run` as channel writes do. Desk role.
- **tag** changes the provider's own tags on the message a mail
  conversation replies to: Gmail labels by ID (`--add`, `--remove`) or
  Outlook categories by name (`--set`, the whole list; empty clears it).
  Not `label`: uma records none of it. An external change, queued like a
  reply, never applied when the command returns. The channel's adapter
  must declare `extension_execute` and ship the effect the flags need,
  else `UNSUPPORTED`, exit 2, at `--dry-run` and on the real run; the
  adapter's own offline check of the fields is `VALIDATION`, exit 2; a
  conversation the peer wrote no message in is `CONFLICT`, exit 4.
  Provider-dependent (`extension_execute`, see
  [provider-verbs.md](provider-verbs.md)). Desk role.
- **assign, snooze, link, mark-read** and their inverses are uma's local
  workflow state: nothing changes at a provider. Desk role; a viewer reads
  the assignee, snooze and links. See
  [conversations.md](conversations.md#assign-snooze-link-and-read).
- **list** workflow filters: `--unread`, `--assignee ACTOR|self|none`,
  `--snoozed include|only|exclude` (default `exclude`), `--snooze-ended[=false]`
  (snooze over, not yet woken by the sweep) and `--linked`,
  applied by uma and combined by AND.
- **list** label filters: `--label` any of, `--label-all` all of,
  `--label-none` none of; combined by AND, applied by uma. An unknown
  name exits 3. Desk and viewer roles.
- **summarize/triage** ask the configured model about a conversation and print the answer;
  triage suggests a category, priority, sentiment, labels and a next action and applies nothing.
  Desk and viewer roles. The text is sent to the model: local by default, cloud only on opt-in and
  `--send-to-cloud`. See [llm-helpers.md](llm-helpers.md).
- **archive, unarchive, move, read, unread, flag, unflag, trash, untrash** change what the
  provider holds about the conversation, through the capability the channel's adapter declares
  (`conversation archive`: `archive`; `move`: `move`; `read`: `read_state`; `flag`: `flag`;
  `trash`: `trash`). They are queued like a reply and never touch uma's status, labels or your
  read marker; `read` is the provider's, `mark-read` is yours. See
  [conversations.md](conversations.md#change-a-conversations-provider-state).

### `label`

The tenant's conversation labels: one definition reused across every
inbox and channel, stored locally. Runs in the daemon (exit 70 without
one). See [conversations.md](conversations.md#labels).

```sh
heo label list [--limit N] [--cursor C]
heo provider-label list [--channel CH]                 # the mail provider's own mailboxes: channel, id, name, kind, role
heo provider-label create <name> [--channel CH] [--kind K]   # idempotent by name; queued
heo label create <name> [--description D] [--color #rrggbb]
```

- **create**: names are unique under case folding; a taken one is
  `CONFLICT`, exit 4. No commas in a name. `--idempotency-key` and
  `--dry-run` as channel writes do. Desk role.
- **list**: desk and viewer roles.

### `message`

Messages across every inbox you may read, public replies, and first
messages that start an exchange. Runs in the daemon (exit 70 without
one). See [conversations.md](conversations.md).

```sh
heo message list [--inbox I] [--conversation C] [--text WORDS] [--since T] [--until T]
                 [--label L] [--label-all L] [--label-none L] [--limit N] [--cursor C]
heo message get <message> [--conversation C]
heo message raw <message> [--conversation C] [-o FILE|DIR|-] [--force]   # the provider's whole copy: RFC 5322 for mail
heo message reply <conversation> --text T [--reply-all] [--cc A,..] [--bcc A,..]
heo message reply-draft <conversation> --text T | --draft ID [--text T | --delete] [--reply-all] [--cc A,..] [--bcc A,..]   # a draft in the mailbox; sends nothing
heo message send --inbox I --to R --text T
heo message edit <message> --revision REV --text T [--conversation C]
heo message delete <message> --revision REV [--conversation C] --confirm=yes
heo message draft <conversation> [--instruction TEXT] [--tone TONE] [--include-notes] [--send-to-cloud]
```

- **list/get** show each message's visibility (`public`, `internal`)
  and origin (`inbound <adapter>`, `uma <actor>`). Internal notes
  appear for desk actors only: for any other role a note is not there,
  listed or by ID. **list** also searches: `--text` (every word,
  whole words, any case), `--since` and `--until` (an RFC 3339 time or a
  date; from `--since` up to, not including, `--until`) and the label
  filters of `conversation list`, which select messages by their
  conversation's labels. uma applies them and searches only what you may
  read, so an internal note matches for desk actors alone. A search page
  can be empty and incomplete: follow the cursor until complete. See
  [conversations.md](conversations.md#search-messages).
- **reply** is always public. It returns once uma accepted it durably:
  `queued`, with its operation, never delivered; the daemon's workers
  deliver it. `--dry-run` shows the exact destination (inbox, channel,
  reply route) and visibility and writes nothing. `--idempotency-key`
  replays. Desk role.
- **send** starts an exchange: a public first message through `--inbox`
  to `--to` — `<kind>:<id>` or `<namespace>/<kind>:<id>` (a route on the
  inbox's channel), `identity:<id>` or `contact:<id>`. The inbox's
  selection policy picks the conversation or creates one. Only adapters declaring `start_conversation` send
  first; any other inbox is `UNSUPPORTED`, exit 2, before anything is
  written. Returns `queued` with its operation and conversation, never
  delivered. `--dry-run` shows the exact destination and visibility and
  that the conversation will be selected or created, and writes
  nothing. `--idempotency-key` replays without sending twice. Desk
  role. See [conversations.md](conversations.md#start-an-exchange).
- **edit** replaces a message's text at its provider; **delete** deletes
  it there and leaves a tombstone in uma (no privacy erasure). Both are
  provider-dependent: only a channel whose adapter declares `edit` or
  `delete` takes them, any other is `UNSUPPORTED`, exit 2, at `--dry-run`
  and on the real run, before anything is written. `--revision` is
  required and always sent; a stale one is `CONFLICT`, exit 4, naming
  the current revision. Queued, never applied, when the command
  returns: the message changes when the provider applied it.
  `--dry-run` shows the platform effect, the audience and the
  `capability` line. `--idempotency-key` replays. Desk role. `delete`
  is destructive: REST, gRPC and MCP withhold it, the socket runs it
  with `--confirm=yes`. See [provider-verbs.md](provider-verbs.md).
- **draft** drafts the text of a reply with the configured model and sends nothing: no operation,
  no message. `--format text` prints the draft alone on stdout. Desk role. See
  [llm-helpers.md](llm-helpers.md).
- There is no `message search`: search is `message list` with filters.

### `reaction`

Reactions on a message. Runs in the daemon (exit 70 without one). See
[provider-verbs.md](provider-verbs.md#reactions).

```sh
heo reaction list --message M [--conversation C]
heo reaction add --message M <emoji> [--conversation C] [--revision REV]
heo reaction remove --message M <emoji> [--conversation C] [--revision REV]
```

- **list** reads the reactions uma stores, any channel, any role that
  reads the message. `complete: false` means the set did not come from
  an authoritative provider snapshot; the provider may hold others.
- **add/remove** are provider-dependent (`react`): `UNSUPPORTED`, exit
  2, on a channel whose adapter does not declare it. Queued, never
  applied. `--dry-run` shows the effect, audience and `capability`
  line. `--idempotency-key` replays. Desk role.

### `participant`

```sh
heo participant list --conversation C
```

The identities taking part in a conversation, with role and, where you
may read identities, assurance. Any role that reads the conversation.

```sh
heo participant add --conversation C --revision R IDENTITY --dry-run
heo participant add --conversation C --revision R IDENTITY
heo participant remove --conversation C --revision R IDENTITY --confirm=yes
```

`add` and `remove` bring an identity (or a contact with one identity in
the inbox) into the conversation at its provider, or take it out. Only
on channels whose adapter declares `membership`; the removal is
destructive-shared. Queued, then applied by the provider; `--revision`
is the conversation's and always sent. Desk role. See
[provider-verbs.md](provider-verbs.md#participants).

### `receipt`

```sh
heo receipt send --message M --kind read --dry-run
heo receipt send --message M --kind delivered
```

Tells the provider a received message was delivered or read. Only on
channels whose adapter declares `receipt`. Queued; it says nothing of
whether the provider showed it. Not the local read state. Desk role.

### `history`

```sh
heo history fetch --conversation C --since 2026-09-01
heo history fetch --channel support --limit 200
```

Reads a bounded window of the provider's past messages, imports each
one once and reports `imported`, `skipped` and any `gap`. Only on
channels whose adapter declares `history`; terminal only. A
conversation is the desk's, a channel the operator's. See
[provider-verbs.md](provider-verbs.md#history).

### `interaction`

```sh
heo interaction list [--inbox I] [--conversation C] [--actor IDENTITY] [--limit N] [--cursor C]
heo interaction get <interaction> [--conversation C]
heo interaction respond <interaction> --mode message|ephemeral|update --text T
                        [--follow-up] [--expires-at T] [--conversation C] --dry-run
heo interaction open-modal <interaction> --view FILE|- [--conversation C] --dry-run
heo interaction push-modal <interaction> --view FILE|- [--conversation C] --dry-run
heo interaction update-modal --inbox I --view-id V|--external-id X [--hash H] --view FILE|- --dry-run
```

The button presses, slash commands and forms a chat provider reported,
which uma records as interactions (never as messages, one
`interaction.received` event each), and the answers to them. `list` and
`get` read what uma recorded on any channel, with no capability: desk and
viewer roles. `respond` is provider-dependent: it works only on channels
whose adapter declares `respond` (`respond_follow_up` for `--follow-up`)
and is refused otherwise, exit 2 (`UNSUPPORTED`), at the preview and on
submission. Queued, never given; one initial response per interaction
(a second is `CONFLICT`, exit 4), any number of follow-ups; `--expires-at`
may not pass the provider's respond window. Desk role. See
[provider-verbs.md](provider-verbs.md#interactions).

`open-modal`, `push-modal` and `update-modal` answer an interaction with a
Slack modal view (`--view`: a file, `-` for standard input, or the JSON) and
change one that is open (`--view-id` from the settled operation's provider
references, or `--external-id`). They run Slack's own extension effects, so
they need `extension_execute` with that effect, not `respond`, and are
refused otherwise, exit 2 (`UNSUPPORTED`), at the preview and on submission.
Slack honours the interaction's `trigger_id` for three seconds, once: heo
refuses a later attempt, exit 1 (`DEADLINE_EXPIRED`), a redacted
interaction, exit 4, and wakes the workers as it queues. They are for a
script reacting to `interaction.received`. Desk role. See
[Slack modals](provider-verbs.md#slack-modals).

### `task`

Agent protocol tasks in your agent inboxes: work your host does for a
remote agent. Inbox-side only: heo opens no task and a message in an
agent inbox is never a command. Not provider-dependent. Runs in the
daemon (exit 70 without one). See [tasks.md](tasks.md).

```sh
heo task list [--inbox KEY] [--conversation C] [--status STATE,...] [--limit N] [--cursor C]
heo task get <task> [--conversation C]
heo task cancel <task> --revision REV [--conversation C] [--dry-run] [--idempotency-key K]
heo task watch <task> [--consumer NAME] [--from tail|beginning] [--timeout D] [--reconnect-timeout D]
```

- **list** and **get** read tasks, most recent status first; uma applies
  the inbox, conversation and status filters and heo none. Empty until an
  agent protocol adapter (`a2a.messages`) is configured: this build
  registers none. `cancel_requested` means the remote agent asked.
  Served on every surface. Desk and viewer roles.
- **cancel** moves a task to `canceled` and answers a cancel request.
  `--revision` is required and always sent: stale or final is `CONFLICT`,
  exit 4. A change local to uma, committed at once and read back; nothing
  is pushed to the remote agent. `--dry-run` writes nothing.
  `--idempotency-key` replays. Desk role.
- **watch** streams the task's `task.updated` and status `message.created`
  events (NDJSON under `--format json`) and ends, exit 0, once the task
  is final; `--timeout` passing first is exit 6. Desk role: task events
  are internal. Terminal only: REST, gRPC and MCP do not serve it
  (discovery says `withheld-by-command`).

### `note`

Internal notes: desk role only, on every surface (any other role:
`UNAUTHORIZED`, exit 5). Runs in the daemon (exit 70 without one). See
[conversations.md](conversations.md#notes).

```sh
heo note list [--inbox I] [--conversation C] [--limit N] [--cursor C]
heo note get <note> [--conversation C]
heo note add <conversation> --text T --placement local|provider
```

- **add** always adds an internal note. `--placement` is required and
  must be the inbox's (`local` when it keeps notes, `provider` when it
  delivers them, `inbox update --deliver-notes`): a mismatch is
  `CONFLICT`, exit 4, naming the fix. A local note commits; a provider
  note is queued. `--dry-run` and `--idempotency-key` as for replies.
- **get** on a message that is not a note exits 3.

### `attachment`

The attachments of messages you may read: list them, show one, download
its bytes. Runs in the daemon (exit 70 without one). Any role that
reads conversations; what is not public is for `desk`. See
[conversations.md](conversations.md#attachments).

```sh
heo attachment list (--message M | --conversation C) [--limit N] [--cursor C]
heo attachment get <attachment>
heo attachment download <attachment> [-o FILE|DIR|-] [--force]
```

`<attachment>` is `<inbox>/<attachment>`, both record IDs, as `list`
prints it in `ref`.

- **list** needs `--message` or `--conversation` (usage error, exit 2,
  without either). Rows carry name, media type, size, audience and the
  holding messages; no bytes are read. An attachment you may not read
  is not listed.
- **get** shows one attachment's metadata and digest, authorized as a
  download is. Not readable by you: exit 5; bytes redacted: exit 3.
- **download** writes the bytes to `-o` (file; directory; `-` for
  stdout, bytes alone), else to a file named after the attachment in
  the current directory. It never overwrites: an existing file is
  `CONFLICT`, exit 4, unless `--force`. Files are created 0600.
  Narrowed away from you: `UNAUTHORIZED`, exit 5; redacted: `NOT_FOUND`
  ("gone"), exit 3.

### `contact`

Contacts: who stands behind channel identities. Runs in the daemon
(exit 70 without one). See [contacts.md](contacts.md).

```sh
heo contact list [--kind person|bot|agent,...] [--limit N] [--cursor C]
heo contact get <contact>                           # record ID; + its identities
heo contact create --name N [--kind K] [--email E ...] [--phone P ...]
heo contact update <contact> --revision REV [--name N] [--email E ...] [--phone P ...]
heo contact merge <from> <into> --reason CODE --dry-run
heo contact merge <from> <into> --reason CODE --revision REV --into-revision REV [--identity I ...]
```

- Read: desk and viewer roles. `get` and `list` show a contact merged
  into another as `merged into <contact>`.
- **create** and **update** are desk writes: `--idempotency-key`,
  `--dry-run`, and for `update` a required `--revision` (stale:
  `CONFLICT`, exit 4). `--email` and `--phone` replace the contact's
  attributes of that kind; `--email=` clears them.
- **merge** moves every identity on `<from>` to `<into>` and leaves
  `<from>` as a redirect. `--dry-run` prints uma's own preview: the
  identities and conversations that move and the two revisions to merge
  at. Executing needs `--reason` (an audit reason code), `--revision`
  (`<from>`'s) and `--into-revision` (`<into>`'s); stale or changed
  identities: `CONFLICT`, exit 4. It is destructive: it asks for
  confirmation on a terminal, `--confirm=yes` without one, and only the
  control socket serves it; REST, gRPC and MCP withhold it. Kit
  rate-limits destructive commands per caller: a burst of three, then
  one every five seconds (exit 6, retry after). Desk role; an identity
  whose link assurance is above `asserted` also needs `operator`.
  See [contacts.md](contacts.md#merge-two-contacts).

### `identity`

Channel identities and the contact each belongs to. Runs in the daemon
(exit 70 without one). See [contacts.md](contacts.md).

```sh
heo identity list [--contact C] [--inbox I] [--limit N] [--cursor C]
heo identity get <identity>                         # + link history; shows the revision
heo identity link <identity> --contact C --revision REV [--reference R]
heo identity unlink <identity> --reason CODE --revision REV [--reference R]
```

- **link** moves the identity to another contact of its kind on your
  attestation (assurance `asserted`); heo never links by itself.
  **unlink** detaches it to a fresh contact; `--reason` (an audit reason
  code) is required. Both are association only, recorded in the link
  history with you as the actor, and take `--revision` (stale:
  `CONFLICT`, exit 4), `--idempotency-key` and `--dry-run`. Desk role.
  Linking to a contact that was merged into another is `CONFLICT`: link
  to the contact it was merged into.

### Issues and tickets

There is no `issue` or `ticket` group: an issue or a ticket is a
conversation of a `per_work_item` inbox, read with `conversation list`
(`--work-item`, `--work-item-status`) and `conversation get`, which show
its work item, and changed with `conversation status` and `conversation
update`. See [issues-and-tickets.md](issues-and-tickets.md).

- **status** sets the work item's status at the provider (`--set open`,
  `pending`, `resolved` or `closed`; `--reason` the provider's, one the
  adapter declares): an external change, queued as a uma operation and
  sent by the daemon's workers, never applied when the command returns.
  The work item changes in uma when the provider reports it; the
  conversation never moves. `--revision` is the work item's, defaults to
  the revision read now and is always sent: stale is `CONFLICT`, exit 4.
  A channel whose adapter does not declare `work_item_act` with the
  transition and the reason, or a work item already in the status, is
  `UNSUPPORTED`, exit 2, at `--dry-run` and on the real run, before
  anything is written. Where uma owns the status (`local_fields`) it
  commits in uma. Provider-dependent (`work_item_act`, see
  [provider-verbs.md](provider-verbs.md)). Desk role.
- **update** changes fields with `--field key=value`, repeated: the
  fields the channel's configuration makes uma's commit in uma; the
  provider's go through the update effect its adapter ships for work
  items (`extension_execute`, found by its `work_item.act` authorization),
  an external change queued like `status`, else `UNSUPPORTED`, exit 2
  (ingest-only). The adapter's own offline check of the fields is
  `VALIDATION`, exit 2. Provider-dependent (`extension_execute`). Desk
  role.
- Both take `--dry-run` and `--idempotency-key`, and a plugin actor's
  change that goes to the provider is held while holding is on.

### `delivery`

Deliveries: the uma operation behind an outbound write. Runs in the
daemon (exit 70 without one). See
[deliveries-and-events.md](deliveries-and-events.md).

```sh
heo delivery list [--state S] [--inbox I] [--conversation C] [--since T] [--until T] [--limit N] [--cursor C]
heo delivery get <operation>
heo delivery reconcile <operation>
heo delivery retry <operation>
```

- **list** shows the outbound writes you submitted, newest change first,
  each with its state: replies, notes, conversation starts and issue
  actions. `--state` (repeat or comma-separate), `--inbox` (key or ID),
  `--conversation` and the window `--since`/`--until` (the time of the
  last change) narrow it. It is uma's operation ledger restricted to the
  outbound kinds, so it shows your own writes only. Desk and operator
  roles.
- **reconcile** asks the adapter of an `outcome_unknown` delivery, now,
  what became of its unresolved attempt, and reports the delivery
  afterwards: `settled` (provider accepted it, or failed it for good;
  exit 0), `not_applied` (no record at the provider: `retry_pending`,
  uma sends it again at `next attempt`; exit 0), or `pending` (the
  answer proved nothing; still `outcome_unknown`, `next attempt` shown;
  exit 8, `OUTCOME_UNKNOWN`). An adapter that declares no reconcile is
  `UNSUPPORTED` (exit 2); a provider that cannot be asked is
  `UNAVAILABLE` (exit 6) and nothing is recorded; any other state is
  `not_needed` and the provider is not asked (exit 0). Idempotent; it
  sends nothing itself. `--dry-run` shows what would be asked and asks
  nothing. Desk and operator roles (uma's `operation.reconcile`).
- **get** shows the state (`queued`, `retry_pending`,
  `provider_accepted`, `delivered`, `failed`, `rejected`,
  `outcome_unknown`, `dead_lettered`, `committed`), the failure with
  uma's retry advice, and what to do next. Desk and operator roles.
- **retry** replays a `dead_lettered` delivery only, after a safety
  check: `outcome_unknown` exits 8 (`OUTCOME_UNKNOWN`) with the
  reconcile path; `failed` and `rejected` exit 4 (send it again under a
  new `--idempotency-key`); any other state exits 4. `--dry-run`
  previews; `--idempotency-key` replays. A delivery sent again may reach
  the provider twice. Operator role.

### `event`

Committed events, listed a page at a time or watched through a durable
subscription. Runs in the daemon (exit 70 without one). See
[deliveries-and-events.md](deliveries-and-events.md#list-events).

```sh
heo event list [--inbox KEY] [--kind KIND,...] [--from beginning|tail | --cursor POSITION] [--limit N]
heo event get <event>
heo event watch [--consumer NAME] [--from tail|beginning | --cursor CHECKPOINT] [--reconnect-timeout D]
```

- **list** reads one page, oldest first, without a subscription: it
  opens nothing and moves no checkpoint. Each event is a reference (kind,
  resource, inbox, visibility, ID), never a message body. `--limit`
  bounds the events uma scans, not the ones shown: events you may not
  read count against it, so a page can be short or empty and still not
  the last. Continue at `next` (`--cursor`) until `complete` is true.
  A position behind retention: `CURSOR_EXPIRED`, exit 4. Every event is
  authorized for the caller, by uma and again by heo: non-public
  content is listed to desk actors alone. Served on every surface.
- **get** reads one event by the ID a list or a watch printed, with the
  same content, and keeps nothing. An event that does not exist, is past
  retention, or is not yours to read (an internal note's, to a `viewer`
  or `external` actor) is `NOT_FOUND`, exit 3, with one message: the
  answer never says whether the event is there. Served on every surface.
- **watch** streams until interrupted: NDJSON under `--format json`,
  one row per line otherwise. Each batch is acknowledged once written;
  Ctrl-C/SIGTERM acknowledges, prints the checkpoint and exits 0.
  Rerunning the consumer resumes after the last line written. No
  daemon: exit 70 at once; an attached watch waits for a returning
  daemon up to `--reconnect-timeout` (30s, 0 = at once) like `task
  watch` and `operation watch`. A
  `--cursor` the open consumer is not at: `CONFLICT`, exit 4. A
  position behind retention: `CURSOR_EXPIRED`, exit 4. Every event is
  authorized again for the caller: non-public content reaches desk
  actors alone. Terminal only: REST, gRPC and MCP do not serve it
  (discovery says `withheld-by-command`; a call is not found).
- `--conversation` and `--since` are not filters of `event list`: uma's
  event log honours the inbox and the kind alone. `conversation list`
  and `message list --conversation` show a conversation's content.

### `operation`

Operations: uma's record that it accepted a write. Find one again,
including by the key it was submitted under, and follow it. Runs in the
daemon (exit 70 without one). See
[deliveries-and-events.md](deliveries-and-events.md#recover-a-write-whose-answer-was-lost).

```sh
heo operation list [--kind KIND] [--state S] [--inbox I] [--conversation C] [--since T] [--until T] [--limit N] [--cursor C]
heo operation get <operation>
heo operation get --key KEY [--kind KIND --target KIND:ID]
heo operation watch <operation> [--timeout D] [--reconnect-timeout D]
```

- **list** shows your operations as uma's ledger holds them, newest
  change first: key, kind, state, time, operation ID, target. `--kind`,
  `--state` (repeat or comma-separate), `--inbox`, `--conversation` and
  the window `--since`/`--until` (the time of the last change) narrow
  it. Only your own operations, whatever else your role may read by ID.
- **get** shows an operation's state, what it produced and its failure.
  `--key` recovers a write whose answer was lost: with `--kind` and
  `--target` uma finds the operation directly; otherwise heo reads your
  operations for the key, `--kind` narrowing the read. A key used for
  more than one operation is `CONFLICT` (exit 4). Another caller's key,
  or one never accepted, is `NOT_FOUND` (exit 3). A state is an answer:
  exit 0.
- **watch** prints each change of state and exits by the state the
  operation ends in: 0 delivered, accepted or committed; its failure's
  exit when failed, rejected or dead-lettered; 8 `OUTCOME_UNKNOWN`;
  6 `TRANSIENT` when `--timeout` passes first. Terminal only: REST,
  gRPC and MCP do not serve it (discovery says `withheld-by-command`).
- Desk and operator roles.

### `storage`

Inspect, back up, restore and migrate the selected profile's data
store. `status` needs nothing else and works while the daemon runs;
`backup`, `restore` and `migrate` take exclusive ownership of the
store, so they are refused (exit 4, `CONFLICT`) while its daemon runs:
stop it with `heo service stop` first.

```sh
heo storage status            # heo, kit, uma, store versions; schema compatibility; attachment files
heo storage backup            # new owner-only archive: store + attachments; prints its path
heo storage restore <path>    # destructive: confirm, or --confirm=yes
heo storage migrate           # backup first, then migrate; never downgrades
```

- **status** reports `compat`: `compatible`, `uninitialized`,
  `migration_required` or `newer_schema`, with the next step. It also
  counts the attachment files in `storage.blobs.dir` and their size
  (`blobs` under `--format json`: `dir`, `exists`, `files`, `bytes`)
  from file sizes alone, without reading them. It does not check that
  every attachment record has its file; there is no bounded way to ask
  the store for them all. Exits 0 whatever the compatibility; an
  unreachable store exits 6.
- **backup** writes one owner-only (0600) archive to
  `storage.backup.dir` and prints its path alone; `--format json` adds
  driver, schema, reason, time and the attachment `blobs` (`files`,
  `bytes`). The archive is a tar file: `manifest.json`, `store.dump` (for
  postgres, a `pg_dump` custom-format archive of uma's tables) and
  `blobs/`, a copy of `storage.blobs.dir`. The manifest lists every entry
  with its size and SHA-256 checksum. A store with no schema has nothing
  to back up (exit 4); a directory holding anything but files and
  directories is refused.
- **restore** replaces the store's data and schema, and the attachment
  files, with a backup. It reads the backup through first and refuses,
  before changing or backing up anything, one whose manifest this heo
  cannot read, that another storage driver wrote, whose entries do not
  match their sizes and checksums, or that is missing or adds an entry
  (exit 2, `VALIDATION`); a schema newer than the binary's is refused
  too (exit 1, `SCHEMA_MISMATCH`). The blob directory is staged beside
  the live one and swapped in whole; if the store then fails to
  restore, the old directory is put back. On a terminal it asks first;
  without one it is refused (exit 5) unless `--confirm=yes` is passed,
  and it never prompts. It backs the current data up first (reason
  `pre-restore`, attachments included) and reports that file; restoring
  it undoes the restore. When the store has no schema there is nothing
  to back up, and the blob directory a restore replaces is kept beside
  the live one and reported (`replaced_blobs`). A missing file exits 3.
  A backup from before a migration restores the older schema: run
  `storage migrate` after it. A backup with no attachments, as the
  one taken before a migration is or as an older heo wrote it (a bare
  `pg_dump` file), restores the store and leaves the blob directory as
  it is; the result says so (`blobs_untouched`).
- **migrate** backs an older store up (reason `pre-migrate`), then
  migrates it; initializes an empty one; leaves a current one alone.
  A store a newer heo migrated is refused before anything is written
  (exit 1, `SCHEMA_MISMATCH`). It reports the schema before and after
  and the backup. A failed migration leaves the old schema, names the
  backup and exits non-zero.

`--dry-run` on `backup`, `restore` and `migrate` checks ownership and
prints the plan, naming the attachments; nothing is written, and
`restore` reads and verifies the backup but asks nothing.

### `upgrade`

Replace the running binary with the latest release (self-update; no
data store is touched). The one command at the top level. See
[upgrade](upgrade.md).

```sh
heo upgrade --dry-run
heo upgrade
```

Refused while a heo daemon runs from this binary (exit 4). Exits 3
while no release is published.

### `spec`

The capability manifest: every verb with its side effect, idempotency,
flags, examples and next steps, for a program to read. Offline: no
daemon, store or network. The [command reference](command-reference.md)
is rendered from it.

```sh
heo spec --format json
heo spec --version                # the manifest schema version alone
heo spec coverage --min 100       # exit 4 while a verb lacks a side-effect tier
```

### `token`

Offline helpers for the bearer tokens a served surface accepts when
`services.<service>.auth.mode` is `jwt`, `jwks` or `oidc`. None of them
signs anything. See [surfaces](surfaces.md).

```sh
heo token claims --sub ada --scopes read,write --expires 1h   # a claims template for a signer
heo token decode <token>                                      # the payload, not verified
heo token verify <token> --service api                        # exit 0 valid, 5 refused
```

### `help`

Show help for any command.

```sh
heo help [command]
```
