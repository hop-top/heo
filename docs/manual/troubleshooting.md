# Troubleshooting heo

Find the exit code of the command that failed (`echo $?`) and the `suggested_fix` in its error:
most entries below start from one of them. For a daemon-backed command the first question is
always whether the daemon runs: `heo service status`.

| Exit | Code | Usually means | Go to |
| --- | --- | --- | --- |
| 2 | `USAGE`, `VALIDATION`, `UNSUPPORTED` | a flag, an argument, a configuration key, or a channel whose adapter lacks the capability | [Provider verbs](#provider-verbs), [Configuration](#configuration) |
| 3 | `NOT_FOUND` | the record, the daemon (`service status`) or a release does not exist | [The daemon](#the-daemon) |
| 4 | `CONFLICT` | a stale `--revision`, a store another daemon owns, a daemon already running | [Storage](#storage) |
| 5 | `UNAUTHORIZED` | no role for this actor, a socket that is not yours, or a destructive verb without `--confirm=yes` | [The daemon](#the-daemon) |
| 6 | `TRANSIENT` | retry with backoff; a burst of destructive verbs is rate limited | - |
| 8 | `OUTCOME_UNKNOWN` | a delivery the provider may have accepted | [Deliveries and events](deliveries-and-events.md) |
| 70 | `PREREQUISITE` | no daemon is running | [Exit 70](#exit-70-no-heo-daemon-is-running) |

## Install

### `heo: command not found`

`make build` writes `bin/heo` in the checkout; it is not on your `PATH` until you link it:

```sh
make symlink            # links bin/heo into the first writable user bin directory on PATH
which heo
heo --version
```

## Configuration

### A setting does not take effect

Configuration is layered, and the daemon reads it only when it starts. Ask where each value came
from, and restart the daemon:

```sh
heo config get services.socket.path --format json   # value, origin layer and file
heo config paths                                     # the files read, highest first
heo config validate                                  # offline checks; errors exit 2
heo service restart
```

heo reads only its own files, the environment (`HEO_<KEY>`) and `-c`. A repository's `kit.yaml`
or a file in the working directory does not configure it. See
[Files and precedence](configuration.md#files-and-precedence).

### Output is a table and a program needs JSON

Pass `--format json` (also `yaml`, `csv`, `text`). `-o <path>` writes the output to a file, it
does not choose a format.

## Debugging

`-V` raises the log verbosity to debug and `-VV` to trace. The daemon logs one `ready_reported`
line per service with its address when the service is ready.

## Getting help

1. Read the entry that matches the exit code, in the table above.
2. Collect `heo --version`, `heo storage status` (heo, kit, uma and store versions, schema) and
   `heo service status`.
3. Search the issues of the repository on GitHub, or open one with those, the command, its exit
   code and its error, and what you expected. Redact credentials: `heo config list` already does.

## The daemon

### Exit 70: no heo daemon is running

The command needs the profile's daemon and none answers on its control
socket. Start it (`heo service start`, or your service manager's unit)
and rerun; `heo daemon ping` checks it answers. With `--profile`,
start that profile's daemon. See [control.md](control.md).

A watch (`event watch`, `task watch`, `operation watch`) that was
already attached waits for the daemon first: it says `daemon
unavailable, waiting up to 30s` on stderr, and exits 70 only when
`--reconnect-timeout` runs out. If the daemon was restarted meanwhile
it carries on by itself; otherwise start the daemon and rerun with the
same `--consumer` to resume (see [deliveries and
events](deliveries-and-events.md#when-the-daemon-goes-away)).

### The daemon exits 1 after a second Ctrl-C

A second SIGTERM or SIGINT (Ctrl-C again) aborts the drain on purpose:
sends in flight are interrupted, stay leased and are settled by the next
start (`outcome_unknown` if the provider may have them); the daemon logs
`drain aborted by second signal` and exits 1. SIGHUP, which a closed
terminal sends, drains like the first SIGTERM. See
[runtime](runtime.md#stopping-and-restarting).

### Exit 5: control socket owned by another user, or open to other users

`heo` refuses a control socket whose directory or file is not yours
alone, before sending anything. Use the default path, or set
`services.socket.path` to a socket in a directory you own with mode
700.

### `service start` exits 2: no authenticator configured

REST, gRPC and MCP over HTTP need an authenticator, on loopback too.
Run `heo service setup <service>` to enable the surface with the
profile's token (see [surfaces.md](surfaces.md)), configure
`services.<service>.auth.mode` (or `services.all.auth.mode`), or leave
the surface off: the CLI needs only the control socket.

### `service start` exits 2: no surface token stored

The profile names a surface token (`credentials.surface_token_ref`)
that its credential backend does not hold: a backend switched since
setup, or a token deleted from it. Run `heo service setup <service>`
to mint one.

### REST, gRPC or MCP answers 401

The request carried no token, another one, or one rotated since. Read
the current one with `heo service setup --show-token`.

### A channel's receiver is `waiting` or never `ready`

`heo channel check <channel> --remote` reports the receiver (`receiving`, way
`receive`) as the daemon that runs it sees it, and `heo service status` shows
the same state on the channel's scope (`channel:<id>`):

- **`starting`, then `waiting`, `connecting to the provider`**: an adapter that
  reports its provider link (the Discord gateway, Slack Socket Mode, Telegram
  polling, IMAP) has not reached it. The daemon is ready, degraded, after
  `services.receivers.settle_timeout` and keeps trying. Look at the network and
  at the provider's status.
- **`waiting`, `<code>: retrying at <time> after <n> failures`** (`phase
  retrying`): the last connection failed with that error code; `retry_at` is
  when the next opens, later when the provider stated a wait (a rate limit's
  reset). The code is all uma keeps; the provider's message is in the
  daemon's log only once the call has given up.
- **`waiting`, `reconnected after <n> failures ... nothing received since`**
  (`phase running`, no `link`): an adapter that reports no link reconnected
  after failures. Nothing proves it is up until a message is accepted, and it
  reads `ready` again then.
- **`waiting`, `receiver lease held by <owner> until <time>`**: another process
  on the same store runs the receiver, or a stopped daemon's lease has not
  lapsed. The check shows the `owner` and `until` and no phase: ask the owner.
  The daemon tries again when the lease ends.
- **`degraded`, `<reason> (<code>)`**: the receiver still receives, by polling,
  while part of it fails: a push registration or change notification
  subscription refused, or the credential for it missing (`code` in the scope's
  json). Mail arrives; fix what the reason names to restore push. Unlike
  `waiting`, nothing is missing from the inbox meanwhile.
- **`failed`**: the call ended on an error that is not retried, the reason
  being the error. Fix what it names; a channel update or a daemon restart
  starts it again.

## Storage

### `storage backup`, `restore` or `migrate` exits 4: store owned

The profile's daemon holds the store; the message names the holder.
Stop it, run the command, start it again:

```sh
heo service stop
heo storage migrate
heo service start
```

### The daemon refuses the store: schema mismatch

`heo storage status` says what to do. `migration_required`: run
`heo storage migrate`. `newer_schema`: a newer heo migrated the store;
upgrade heo, or restore the backup taken before that migration.

### A migration failed

The store is still at its old schema and the error names the
pre-migrate backup. Fix the cause and rerun `heo storage migrate`; if
the store is unusable, `heo storage restore <that backup>
--confirm=yes` first.

### `storage restore` exits 2: the backup is not whole

The archive does not match its manifest: an entry was altered,
truncated, added or left out, or the file is not a heo backup. Nothing
was changed, not even a pre-restore backup. Restore another copy; do not
edit an archive. A backup that holds a newer schema than this heo (exit
1) needs `heo upgrade` first.

### A restore failed after the blob directory was replaced

The old blob directory is put back, so a failed restore can be rerun.
If the store failed to restore, it is empty until the rerun succeeds or
the `pre-restore` backup the command named is restored. If the process
was killed mid-restore, `<blob dir>.restore-<n>` (staging) may remain
beside the blob directory: the next restore removes it. A
`<blob dir>.replaced-<n>` is the previous blob directory, kept because no
backup held it; check the restored attachments, then delete it.

### `pg_dump not runnable`

Install the PostgreSQL client tools, or point
`storage.postgres.pg_dump` and `storage.postgres.pg_restore` at them.

### Exit 5: `heo.authz.no_role`

The daemon verified who you are, but its authorization policy grants
you no role. On your own machine, check that `authorization.owner` has
not been emptied. For another uid or a token subject, add a grant under
`authorization.grants`, then restart the daemon. See
[authorization.md](authorization.md).

### `service start` exits 2: authorization policy

The `authorization` section has an error, such as an unknown role or an
invalid actor. `heo config validate` lists each one.

## LLM helpers

### `conversation summarize`, `triage` or `message draft` exits 70: not reachable

`ollama at <host> is not reachable`: nothing answers at `llm.endpoint` (by
default `http://127.0.0.1:11434`). Start the server, or point `llm.endpoint` at
one that runs. heo does not fall back to another provider, local or cloud; that
is by design. `heo conversation summarize <conversation> --dry-run` shows the
provider and host a call would use without sending anything.

### Exit 70: a cloud provider that was not opted in

`llm.provider ... runs at <host>, which is not on this machine`: the endpoint is
not a loopback or private address, so it is a cloud provider. Either point
`llm.endpoint` at a server on this machine (use its address: a name such as
`gpu-box.lan` counts as cloud), or opt in with `llm.cloud.<name>.enabled true`
and name the provider in `llm.provider`. `auto` never selects a cloud provider.

### Exit 7: `CONSENT_REFUSED`

The provider is a cloud one and the call did not say `--send-to-cloud`. Nothing
was read or sent. Add the flag to allow that one call.

### Exit 3: the model is not on the server

`model "<name>" is not available`: pull it (`ollama pull <name>`) or set
`llm.model` to one the server serves.

### Exit 6: the call timed out, or the triage was not valid

A cold local model needs time to load and a thinking model can run long: raise
`llm.timeout`, use a smaller model, or run it again. Keep `llm.timeout` below
`services.all.timeouts.command`. `LLM_OUTPUT_INVALID` means the model twice
failed to answer in the triage schema; nothing was changed.

### The summary leaves out the start of a long conversation

The prompt keeps the newest messages that fit `llm.max_prompt_chars`; the answer
says `truncated` and how many messages went in. Raise the budget for a model
with a longer context.

## Provider verbs

### `message edit`, `message delete`, `reaction add` or another provider verb exits 2: UNSUPPORTED

The conversation's channel has an adapter that does not declare the
capability the verb needs (`edit`, `delete`, `react`, `membership` for
`participant add` and `remove`, `receipt`, `history`;
`work_item_act` for `conversation status`; `extension_execute` for
`conversation update`, `conversation tag`,
`message reply-draft` and `conversation unsubscribe`; `archive`, `move`,
`read_state`, `flag` or `trash` for the conversation state verbs).
Nothing was written, and retrying cannot help. `heo channel get <channel>` lists each verb as
`declared` or `unsupported` with the reason, and `heo adapter list`
shows which adapters enable which verbs. Mail and Twilio WhatsApp
adapters declare none of them; see
[provider-verbs.md](provider-verbs.md#what-each-maintained-adapter-declares).

A `github.issues` channel declares all three but reports `config` in
`channel get`: its configuration needs `replies` on (with `account_id`).
Update the channel (`heo channel update`), then ask again.

### `conversation status` exits 2: UNSUPPORTED

Nothing was written. The message says which of these it is: the
conversation has no work item (only a conversation of a `per_work_item`
inbox is an issue or a ticket); its channel has an adapter that does not
declare `work_item_act`, or not the transition or the `--reason` you asked
for (`jira.issues`, `linear.issues` and `zendesk.tickets` declare none);
or the work item is already in `--set`. `heo channel get <channel>` lists
`conversation status` as `declared` or `unsupported`, with the
transitions the adapter performs under `limits`. A `--set` that is not
one of `open`, `pending`, `resolved`, `closed`, or a reason that is not one
snake_case word, is a usage error (exit 2) before anything is asked of the
daemon. The conversation's own status is `conversation resolve` and
`conversation reopen`, which this verb never touches.

### `conversation status` succeeded but the status is unchanged

Expected, for a while. The command only queues the change. Follow it with
`heo delivery get <operation>`; once it is `delivered`, the provider
applied it, but the work item shown by `conversation get` changes only
when the provider's webhook of the change reaches heo (uma may get no
state back). If the operation ended `failed` or `rejected`, the provider
refused it (the account may not change issues in that repository): fix
the cause and send it again under a new `--idempotency-key`. If it is
`delivered` and the status never changes, check that the provider's
webhook delivers its issue events to `<public_base_url>/webhooks/<channel id>`.

### `conversation update` or `conversation tag` exits 2: UNSUPPORTED

Nothing was written. These two verbs need more than a capability: the
channel's adapter must declare `extension_execute` **and ship the effect
the verb runs**. For `conversation update` it is the effect the adapter
ships for its work items, found by its authorization as a work item act;
for `conversation tag`, Gmail's label change for `--add` and `--remove`
or Outlook's category change for `--set`. The message says which is
missing: no `extension_execute` at all, or an adapter that declares it for
effects of its own. For `conversation update` that means the provider's
own fields are ingest-only on that channel (`gitlab.issues`,
`jira.issues` and `linear.issues` ship none), and `conversation get`
shows the verb `unsupported`; fields uma owns there (`local_fields`) still
change. For `conversation tag` the message may name the other mail effect
and the flags the adapter takes. `heo channel get <channel>` lists each
verb as `declared` or `unsupported`, and names the effect the adapter
lacks.

### `conversation update` or `conversation tag` exits 2: VALIDATION

The adapter refused the fields before anything was stored: for
`conversation update`, a value the adapter does not take (GitHub: logins
that are not logins, a label or login named twice, a milestone that is not
a number or `null`, a field that is another operation's such as `state`),
fields of two owners in one update, or `status` where uma owns the status;
for `conversation tag`, a label, ID or category named twice (or an ID in
both `--add` and `--remove`), a blank name, more than 100 IDs in a list.
`--dry-run` shows the same refusal. A `--field` that is not `key=value`
or is given twice, and a call that names nothing to change, are usage
errors, also exit 2.

### `conversation update` or `conversation tag` succeeded but nothing changed

Expected, for a while: the command only queues the change. Follow it with
`heo delivery get <operation>`. Once it is `delivered`, the provider
applied it; `conversation get` then shows the new fields when the
provider's webhook of the change reaches heo, and a tag change is never
shown by heo, which records no provider tags: look at the mailbox. If the
operation ended `failed` or `rejected`, the provider refused it: a label
ID or category the mailbox does not hold, a login that is not a
collaborator, a milestone that does not exist, a token without write
access to the issues or to the message. Fix the cause and send it again
under a new `--idempotency-key`. `conversation tag` exits 4 (`CONFLICT`)
for a conversation the peer has written no message in: there is no
message to tag.

### `interaction open-modal` or `push-modal` exits 1: DEADLINE_EXPIRED

Nothing was queued. Slack honours a `trigger_id` for three seconds after it
sent the press, and heo refuses an attempt past that instant, at the preview
too. A person cannot type this in time: run the verb from a script that reacts
to the `interaction.received` event (the watch's own poll, up to a second,
comes out of the same three seconds), and open the next modal from the next
press. If a first attempt under the same `--idempotency-key` may have got
through, `heo operation get --key <key>` finds it. The other refusals are
before the queue as well: exit 4 for a redacted interaction, exit 2 for one
with no trigger_id (Slack named it by a digest of its payload), exit 3 for one
you may not read, exit 2 `UNSUPPORTED` for a channel whose adapter does not
ship the modal effect.

### A modal was queued, but nobody saw it

Follow `heo delivery get <operation>`. `rejected` with `deadline_expired` or
`conflict` is Slack saying the trigger_id was too old or already used
(another handler answered the same press first); nothing opened and nothing
is retried. `outcome_unknown` means the answer was lost after the request was
written: the trigger_id may be spent, so heo never sends it again, and
`delivery reconcile` cannot settle it, since a modal leaves nothing to look
up. `provider_accepted` means Slack took the view and showed it to the person
who pressed; the view's ID is on the operation's `provider refs` row, for
`interaction update-modal --view-id`. An update to a view that is gone or
whose `--hash` is stale is `rejected` (`not_found`, `conflict`).

### The preview passed, then the operation failed `unsupported` or `unauthorized`

`--dry-run` checks only that the adapter declares the capability, not
that the provider will take it now. uma checks again just before each
attempt, and a capability the channel's configuration switched off
(`github.issues` without `replies`, reported `disabled`) fails the
operation as `unsupported`; a provider permission withdrawn fails it as
`unauthorized`. Neither is retried, and `heo delivery retry` refuses
them. `heo delivery get <operation>` shows the state (it exits 0), and
`heo channel check <channel> --remote` shows each verb's live state:
`disabled` is `DISABLED` (exit 4), `unauthorized` is `PROVIDER_PERMISSION`
(exit 5: the provider account lacks the permission, a different remedy
from a heo role). `heo conversation get` shows the same states for one
conversation to a desk actor. Fix the cause (turn `replies` on, restore the token's scope),
then send the change again as a new operation under a new
`--idempotency-key`.

### `message edit` or `message delete` exits 4: CONFLICT

The message changed since you read its revision (the error names the
current one), was deleted or redacted, or an edit retried under the same
`--idempotency-key` finds the message's audience narrowed. Read it again
with `heo message get`, and retry with the new `--revision` and a new
key. A retry of an unchanged request under the same key is not a
conflict: it returns the stored operation (`replayed`).

### `message delete` is not invocable on REST, gRPC or MCP

By design: a provider deletion is destructive, and heo's served surfaces
refuse that tier until the operator names the surface. The control
socket allows it (`heo message delete … --confirm=yes` at a terminal).
Destructive commands are rate limited per caller, three at once and 12 a
minute: a burst answers `TRANSIENT` (exit 6) with the wait.

### `history fetch` says the history is not whole

The report carries `gaps` above 0 (`gap: true` in JSON): the provider
marked part of the window it was asked for as missing, such as messages
past its retention or out of the account's reach. heo imported what the
provider returned and cannot fill the rest; a rerun asks again and
imports nothing new. Narrowing `--since` to the part the provider still
holds avoids the gap, and does not recover what is gone. `complete: true`
alone never means the history is whole.

### `history fetch` stopped before the end

The report names a `next` cursor: the window is longer than `--limit`
(at most 500). Pass it as `--cursor` to continue; messages already
imported are skipped. If it failed instead, the error says how much was
imported before it; run it again, and nothing is imported twice.

### `reaction list` says `complete: false`

uma built the list from reaction events, not from the provider's own
snapshot, so the provider may hold reactions uma has not heard of. Some
providers (GitHub) send no reaction events. It is not an error.

### A Telegram channel is `waiting`: `provider conflict`, or receives nothing

A bot takes its updates by webhook or by `getUpdates`, never both. While a
webhook is set at Telegram, heo's receiver is refused (409), and `heo service
status` shows the channel `waiting` with `provider conflict: Telegram holds a
webhook for this bot`, and the receivers service `degraded`. That happens to
a channel whose `receive` is empty (heo runs both ways) or `poll`. If you
receive by webhook, say so: set the channel's `receive` to `webhook` with
`heo channel update`, which stops the receiver at once; the channel receives
at `<public_base_url>/webhooks/<channel id>`. To receive by polling instead,
delete the webhook at Telegram (`deleteWebhook` with the bot token): heo never
does, and its next try, within 5 minutes, takes the channel to `ready`
once Telegram has answered a poll. The refusal ends the receive call at once,
so the check shows no `phase` for it, and the wait grows by itself: that
`degraded` is expected while the bot uses the other way.

A channel that receives nothing by webhook: ask Telegram
(`getWebhookInfo`) for its `last_error_message`. 401 is a missing or wrong
secret token (it must be the value stored under the purpose
`telegram.messages.webhook`, at least 16 characters), 503 is no stored secret,
422 is a message heo cannot file (a per-thread inbox and a message outside a
topic), and a URL on a port other than 443, 80, 88 or 8443 is never called. A
channel `failed` with `Telegram refused the bot token` has a revoked or
mistyped token: store the new one and restart the daemon.

### A Slack channel is `failed` in the receivers: `credential not found`

`heo service status` shows a `slack.chat` channel `failed` under
`receivers` with `not_found: credential not found`, and the receivers
service `degraded`, while its webhook route is `ready`. The channel's
`receive` is empty, so heo runs both ways (the request URL and Socket Mode),
and no app-level token is stored under `slack.chat.socket`. If the app takes
its payloads at its request URL, set the channel's `receive` to `events_api`
with `heo channel update`: the receiver stops and needs no token. For Socket
Mode, set `receive` to `socket_mode` and store the app-level token
(`xapp-...`). `heo channel check <channel> --remote` names each credential the
channel's `receive` needs and reports the missing ones.

### A Teams channel receives nothing, or answers 401, 403 or 503

Look at the Azure Bot's messaging endpoint first: it must be the channel's
route, `<public_base_url>/webhooks/<channel id>`, on a public `https` URL. At
the route, 401 is a request with no Bot Connector token, or one whose token
fails a check: its audience is not the channel's `app_id` (the messaging
endpoint belongs to another bot registration), its `serviceurl` is not the
activity's, it is expired beyond five minutes of clock skew (check the host's
clock), or it was issued by Microsoft Entra rather than the Bot Connector (the
Bot Framework Emulator's are). 403 is a signing key the Bot Framework does not
endorse for Teams. 503 is the Bot Framework's signing keys unreadable through
heo's HTTP client (`login.botframework.com` unreachable from the host): the Bot
Connector retries. Nothing reaches heo from a 401, 403 or 503, so the status the
Bot Connector got is the evidence. A message that arrives and is not filed (422)
is a group chat message in a `per_thread` inbox. In a channel or group chat the
bot is delivered only the messages that @mention it, unless the Teams app's
manifest asks for `ChannelMessage.Read.Group` or `ChatMessage.Read.Chat`.

### A Teams send fails `unauthorized`, `not_found` or `validation`

heo mints the Bot Connector token from the app's client secret or certificate,
so a send that cannot be authorized names why. `failed` as `unauthorized` with `Microsoft Entra
token endpoint: HTTP 401: invalid_client (AADSTS7000215)` is a secret Entra
refuses: store the app's current client secret under
`microsoft.teams.client_secret` (its value, not its ID, and one that has not
expired in the Azure portal); the next send uses it, with no restart and
without waiting. The code after `invalid_client` is Entra's: `AADSTS7000222`
is an expired secret, `AADSTS700016` an `app_id` the tenant does not know,
`AADSTS90002` a `tenant_id` that does not exist. With a certificate the
refusal reads `the app's client certificate is refused`: `AADSTS700027` is a
certificate Entra does not know for that app (the one stored is not the one
uploaded to the app's **Certificates & secrets**, or it was removed there, or
it is not the stored key's), and `AADSTS700024` an assertion outside its
validity window, which is the host's clock (check it). Store the certificate and
key the app has, under `microsoft.teams.client_certificate`; the next send uses
them with no restart.
`heo channel check <channel> --remote` repeats the mint and reports it as an
error finding, with the same code. `transient` or `rate_limited` naming the
token endpoint is Entra answering 5xx or 429, or unreachable from the host
(`adapters.teams.authority`, by default `login.microsoftonline.com`): the worker
retries, and heo waits between mints, from 2 seconds to 5 minutes. `not found`
on the stored credential is nothing stored under `microsoft.teams.client_secret` or
`microsoft.teams.client_certificate`. `conflict` is both stored: keep one. A
certificate whose key is not RSA fails `validation`, and one past its `not_after`
`unauthorized` (`expired`), before anything reaches Entra; `heo channel check
<channel> --remote` warns 30 days before a certificate ends.
`unauthorized` with `the provider no longer accepts the OAuth grant` on a Gmail or Outlook
channel: the user revoked consent, or Google's testing status expired it after seven days; run
`heo channel authorize <channel>` again (no flags: the stored client and scopes are reused).
`unauthorized` with `the Bot Connector refused the credential` is the Bot Connector
refusing a token Entra issued (the app is not allowed to speak for that `service_url`
or the bot registration is not this app's). `rejected` as `unauthorized` from Teams
itself is a bot that is not in the conversation, or, writing first to a user, an
app that is not installed for them; `not_found` is a conversation Teams does not
know. `validation` on `--to room:<id>` is the bare room: name the conversation
with its namespace, `--to microsoft.teams/room:<conversation ID>`; `thread:<id>`
names no channel, so answer a thread with `message reply`.

### `message send` to a Telegram chat fails `validation`

`--to room:<id>` is read in the identity namespace, which holds users; the
preview accepts it and the adapter refuses it when the send runs. Name the chat
with its namespace: `--to telegram.messages/room:<chat ID>`.

### A `relay.messages` channel is refused: the callback

`channel create` or `channel update` exits 2 (`VALIDATION`) on the channel's
`callback`, without echoing the URL (a URL may be mistaken for a place to put a
secret). The callback and the lookup must be `https` (`http` only with
`adapters.relay.allow_http: true`), carry no credentials, query or fragment, and
name a host in `adapters.relay.callback_hosts`, written as the URL writes it (a
port is part of it: `gateway.example.com:8787`). That list is yours, not the
channel's, so a channel cannot widen it: add the gateway's host to the setting,
restart the daemon, and create the channel again.

### A `relay.messages` gateway's webhook is refused

The gateway sees the status of the route `<public_base_url>/webhooks/<channel
id>`. 401: no signature, another secret than the one stored under
`relay.webhook`, or a timestamp outside the window (check both clocks); 400: a
signature header that does not parse, or a body that is not a message (it needs
`from`, `to`, `message_id` and `body` or `media`); 409: the same signed delivery
twice, with `replay` on; 503: no `relay.webhook` secret stored, so the gateway
retries once it is. A message the gateway sends again, signed anew, is accepted
and filed once. After a restart of heo, replay rejection starts empty.

### A reply to a `relay.messages` channel stays `outcome_unknown`

The gateway's answer to the callback was lost (a timeout, a dropped
connection, a 5xx). With `callback.lookup_url` heo asks the gateway for the
operation, and settles it or sends again under the same `Idempotency-Key`; run
`heo delivery reconcile` to ask now. Without one, `reconcile` is `disabled` on
the channel (`heo channel check <channel> --remote`) and the reply is never sent
again: check the gateway's own log for the key (the operation ID), then send a
new reply if it never went out. A gateway that sets a longer timeout than
`adapters.relay.timeout` (15s) makes every slow send look lost: raise the
setting for a gateway whose backend answers late.

### `message send` to an iMessage handle fails `validation`

The channel declares `relay.sms`, `relay.whatsapp` and `relay.imessage`, so the
bare `phone:+15550100003` is refused at the preview: name the namespace,
`relay.imessage/phone:+15550100003`, or `relay.imessage/phone:alice@example.com`
for an Apple ID (in the `phone` kind; a start lower-cases it). The namespace of another
medium passes the preview and fails `validation` when the send runs, nothing
reaching the gateway.

### A Discord channel is `failed` or `waiting`, or receives no messages

`heo channel check <channel> --remote` shows the gateway's `receiving` state
and reason, with the link (`connecting` or `connected`), the failures counted
and when the next connection opens; `heo service status` shows the state and
reason on the channel's scope. `ready` means the gateway session is up, not
only that heo started the receiver.

- **`starting`, then `waiting`, `connecting to the provider`**: the gateway has
  not reached READY. After `services.receivers.settle_timeout` the channel
  reads `waiting` and the daemon is ready, degraded, while it keeps trying.
  Look at the network and at Discord's status; `last_failure` in the check
  names the last failure's code once one has ended a connection.

- **`failed`, `disabled: a privileged intent is not granted (close 4014)`**:
  the channel asked for `MESSAGE_CONTENT` (`mentions_only` is off) and the
  application was not granted it. Turn the intent on in the Developer Portal
  (Bot, Privileged Gateway Intents), or set `mentions_only` back to `true`
  with `heo channel update`; heo does not retry a 4014.
- **`failed`, authentication failed**: Discord refused the bot token (reset,
  or another application's). Store the new one under
  `discord.chat.api` and restart the daemon; a token whose bot is not
  `bot_user_id` ends the receiver as well.
- **`waiting`, `rate_limited: retrying at <time>` and a wait of hours**: the
  day's 1000 Identify calls are spent. heo never identifies past them (Discord
  would reset the token) and opens the next connection at `retry_at`, when
  Discord says it resets. Find what reconnects so often.
- **Receives no guild messages at all**: with `mentions_only` on (the default)
  Discord sends a guild message only when it mentions the bot; direct messages
  and interactions always come. Mention the bot (`@name`) or write to it.
- **The Interactions Endpoint URL will not save**: Discord probes it with a
  PING and a bad signature. The route answers 401 to a bad signature, 503 with
  no public key stored (store it under `discord.chat.interactions`), 404 when
  `receive` is `gateway`, and 405 to anything but POST. A reverse proxy that
  changes the body breaks the signature.
- **Two channels of one application** are refused (`CONFLICT`): one bot token
  holds one gateway session and one endpoint.

### `message send` to a Discord channel fails `validation`

`--to room:<id>` is read in the identity namespace, which holds users; the
preview accepts it and the adapter refuses it when the send runs. Name the
channel with its namespace: `--to discord.chat/room:<channel ID>`. A thread
cannot be named with its channel: answer it with `message reply`.
