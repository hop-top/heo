# Configuration Reference: heo

## Profiles

A profile is one heo daemon: its own config file, data, control socket
and tenant. Pick one per command with `--profile <ref>` or
`HEO_PROFILE`; without either, heo uses the profile `default`.

| `<ref>` | Means |
|---------|-------|
| `<name>` or `heo://profile/<name>` | heo profile `<name>` (`[a-z][a-z0-9-]{0,15}`, not starting with `aps-`) |
| `aps:<id>` or `aps://profile/<id>` | heo profile bound to the existing aps profile `<id>` |

Any other scheme is a usage error (exit 2). An aps profile that does not
exist is not found (exit 3); heo keeps its own state for it under
`aps-<id>` and never writes aps state. heo never picks a profile from
aps's environment (`APS_PROFILE`): bind one explicitly.

A named heo profile other than `default` needs its config file; create
it with `heo --profile <name> config set <key> <value>`.

## Files and precedence

Lowest to highest:

1. built-in defaults
2. `/etc/heo/config.yaml`
3. user file: `$XDG_CONFIG_HOME/heo/config.yaml` (on macOS without
   `XDG_CONFIG_HOME`: `~/Library/Application Support/heo/config.yaml`)
4. profile file: `profiles/<profile>.yaml` next to the user file
5. `-c <path>` files, in order
6. environment: `HEO_<KEY>`, upper case, dots as underscores
   (`HEO_SERVICES_SOCKET_PATH`)
7. flags bound to a key
8. `-c key=value`

The environment beats `-c <path>` files. heo reads only these files: a
repository's `kit.yaml`, `.kit.yaml` or `.kit/config.yaml` does not
configure heo, and the working directory never matters. The daemon
reads `services.*` from the same layers, and only from them: what
`heo config get services.<key>` reports is what `heo service start`
binds. A `profile` key
in any file or `-c` override is refused: the profile is selected only by
`--profile` or `HEO_PROFILE`.

## Commands

```sh
heo config list                      # every effective value, origin, detail
heo config get services.socket.path  # one value; --format json adds origin
heo config set runtime.tenant acme   # writes the profile file
heo config set --scope user KEY VAL  # writes the user file
heo config validate                  # offline checks (+ stored channels via the daemon); errors exit 2
heo config paths                     # the file chain, highest first
```

Credential material is redacted from `list` and `get`, and `set` refuses
a value that matches a known credential pattern: keep credentials in the
secret store and reference them. Changes apply when the daemon starts.

## Defaults

| Key | Default |
|-----|---------|
| `services.socket.enabled` | `true` |
| `services.socket.path` | `<runtime dir>/heo/<profile dir>/heo.sock` |
| `services.socket.auth.mode` | `peer` |
| `services.socket.auth.peer.require_same_uid` | `true` |
| `services.api.enabled`, `services.rpc.enabled`, `services.mcp.enabled` | `false`; `heo service setup` enables them (see [surfaces.md](surfaces.md)) |
| `credentials.surface_token_ref` | empty: the store key of the profile's surface token, which REST, gRPC and MCP over HTTP accept; `heo service setup` sets it |
| `services.workers.enabled`, `services.receivers.enabled` | `true` |
| `services.bridge.enabled` | `true`: the event bridge republishes uma's events on the daemon's bus (see [runtime.md](runtime.md#event-bridge)); one durable subscription, nothing else, until a hook subscribes. `false` runs none |
| `services.webhook.enabled` | `false` |
| `services.hooks.enabled` | `false`: the hook runner starts the `plugins:` section's plugins, one durable subscription each (see [runtime.md](runtime.md#hook-runner)) |
| `services.workers.count` / `poll` / `lease` / `drain_timeout` | `4` / `1s` / `1m` / `20s` |
| `services.workers.held_sweep_every` | `0s`: how often workers expire held operations past their deadline and wake snoozed conversations whose snooze ended; `0s` is uma's own cadence (1m), `off` runs neither sweep (no `conversation.snooze_ended` event, and an ended snooze stays on the record) |
| `services.receivers.settle_timeout` | `10s`: how long a channel retrying a failed start holds readiness; below `services.receivers.ready_timeout` |
| `services.receivers.start_spread` | `5m`: window over which the daemon start spreads the first connection of Gmail and Graph receivers, each at a stable per-channel point in it (uma's start spread), so the first polls of many mailboxes do not fall in the same second; `0s` starts them all at once; never delays readiness |
| `services.webhook.addr` | `127.0.0.1:8083` (loopback; `allow_remote: true` for another) |
| `services.webhook.public_base_url` | empty: the URL providers reach the listener at |
| `adapters.smtp.addr` | empty: `smtp.mail` channels send only; the MX listener otherwise (see [adapters.md](adapters.md#inbound-mail-smtpmail)) |
| `adapters.smtp.allow_remote` | `false`: a non-loopback `adapters.smtp.addr` needs `true`, and STARTTLS |
| `adapters.smtp.tls.cert_file` / `adapters.smtp.tls.key_ref` | empty: no STARTTLS; the listener's PEM chain file and the credential reference of its key (purpose `smtp.tls`; see [STARTTLS](adapters.md#starttls)) |
| `adapters.smtp.allow_remote_without_tls` | `false`: `true` accepts a non-loopback listener without STARTTLS (mail in plain text) |
| `adapters.widget.retain` | `256`: delivered widget events kept per visitor in the data store (see [Delivered events](adapters.md#delivered-events)) |
| `adapters.relay.callback_hosts` | empty: the hosts a `relay.messages` channel's callback and lookup URLs may name, as the URL writes them (`gateway.example.com`, or `host:port`); empty means no channel can send. A YAML list, or a comma list from `config set`, `-c` or the environment (see [Relay gateway](adapters.md#relay-gateway-relaymessages)) |
| `adapters.relay.allow_http` | `false`: `true` accepts `http` callbacks on the listed hosts, the credential travelling in clear text |
| `adapters.teams.authority` | `https://login.microsoftonline.com`: the Microsoft Entra endpoint a `microsoft.teams` channel mints its Bot Connector token at, with the bot's client secret or certificate; `https`, or `http` on a loopback host (see [Teams](adapters.md#microsoft-teams-microsoftteams)) |
| `adapters.relay.timeout` | `15s`: the bound on one callback or lookup request, above zero and at most `10m` |
| `services.all.idempotency.enabled` | `false` |
| `runtime.tenant` | the profile dir name (`default`, `work`, `aps-noor`) |
| `runtime.retention.operations` | `720h`: the operation horizon on uma's retention policy: how long uma keeps an operation, and the key that finds it, after its last update. heo keeps no operation history of its own, and does not run uma's purge today, so nothing is removed at the horizon yet. A positive duration |
| `held_operations.enabled` | `false`: when `true`, a write by a `plugin:` actor is stored held for a human decision instead of being queued; a person's own writes are never held. Only verbs that can be previewed (`--dry-run`) and that uma can hold are accepted from a plugin; the others are refused (see [deliveries-and-events.md](deliveries-and-events.md#held-operations)) |
| `held_operations.expiry` | `72h`: how long a held operation waits for a decision before it expires. A positive duration |
| `held_operations.on_behalf_of` | empty: the actor the plugins act for, recorded on each held operation and in its trust key |
| `held_operations.trust.enabled` | `false`: when `true`, the trust ladder decides which plugin writes are held instead of holding all of them (see [Trust ladder](deliveries-and-events.md#trust-ladder)) |
| `held_operations.trust.unattended` | `true`: the plugins run with no human present, so a `reply` send is always held. `false` lets a trusted send through |
| `held_operations.trust.thresholds` | a YAML map of class to the clean approvals after which it is automatic, merged over the defaults: `read`, `unread`, `star`, `unstar`, `label`, `archive`, `move_to` 3; `reply:draft`, `trash`, `spam` 5; `reply:send` 8; `attention` 0 (always automatic); `lane:unsolicited` 5; `lane:newsletter` 3; any other class 15. `unsubscribe` takes no threshold: it is always held, and an entry for it is a validation error. An exact class (`label:finance`) beats its base (`label`) |
| `held_operations.trust.never_auto` | empty: a YAML list of classes that never turn automatic whatever their count (an exact class such as `trash`, or a base such as `label`); a batch approval still decides them, `operation trust list` shows why they are held. `unsubscribe`, a class with threshold `0` and a class in `seed` are validation errors, see [approvals.md](approvals.md#classes-that-never-turn-automatic) |
| `held_operations.trust.inboxes` | empty: a YAML map of inbox ID to that inbox's policy: `thresholds` (a class to its clean approvals, ahead of `held_operations.trust.thresholds` for this inbox) and `first_reply` (`hold`, the default: a reply to a contact heo never wrote to on the inbox is held whatever the count; `allow`: it follows the ordinary `reply:send` rules). Inbox IDs match without regard to case. Chat, messaging and tracker classes (`react:message`, `status`, `edit:message`, ...) ship no threshold; see [approvals.md](approvals.md#thresholds-for-one-inbox) |
| `held_operations.trust.seed` | empty: classes automatic from day one for a person and mailbox with no correction on record. Only `read`, `unread` and `archive` are accepted; every other class earns its trust |
| `held_operations.trust.approval_always` | empty: inbox IDs whose every plugin write is held, whatever the trust |
| `held_operations.trust.supervised.enabled` | `false`: opt-in. `true` holds every class, trusted or seeded, for the first `held_operations.trust.supervised.days` days (default `7`) of a person on a mailbox |
| `held_operations.trust.supervised.started` | empty: YAML map of `<on behalf of>/<inbox ID>` to the RFC 3339 time the person started on that mailbox. A pair with no start is held |
| `held_operations.chat.enabled` | `false`: announce held operations to the conversations in `held_operations.chat.conversations` and take decisions from them (see [Approving from a chat conversation](deliveries-and-events.md#approving-from-a-chat-conversation)) |
| `held_operations.chat.conversations` | empty: a YAML list of `inbox`, `conversation` (record IDs) and `members`, a list of `identity` (uma identity record ID) and `actor` (who it decides as; never a `plugin:` actor); a conversation may set `slack_thread_replies: true` (default `false`) so a bare `approve`/`reject` in a reply anywhere in a Slack announcement's thread decides, see [Approving from a chat conversation](deliveries-and-events.md#approving-from-a-chat-conversation) |
| `authorization.owner` | `[desk, operator]`: the roles of the uid the daemon runs as (see [authorization.md](authorization.md)) |
| `authorization.grants` | empty: no other actor holds any role |
| `llm.provider` / `llm.model` / `llm.endpoint` | `ollama` / `llama3.2` (a placeholder: set your model) / empty, meaning `http://127.0.0.1:11434`: the model the LLM helpers ask; local by default (see [llm-helpers.md](llm-helpers.md)) |
| `llm.timeout` / `llm.max_prompt_chars` | `120s` / `12000`: one model call's limit; the prompt budget, at least `3000` |
| `llm.cloud.<name>.enabled` / `llm.cloud.<name>.credential_ref` | `false` / empty: the opt-in to a cloud provider `<name>` and the reference of its key (purpose `llm.<name>`); a cloud call also needs `llm.provider: <name>` and `--send-to-cloud` |

`heo config list` prints the rest.

## Credentials

Configuration names where credentials live, never a credential. Each
profile has one credential backend.

A heo profile keeps credentials in the kit secret store chosen by
`credentials.backend`:

| Backend | Settings | Notes |
|---------|----------|-------|
| `keyring` (default) | `credentials.keyring.service` (`heo`) | OS keychain |
| `file` | `credentials.file.dir` (`<data dir>/heo/<profile dir>/secrets`) | one 0600 file per secret, unencrypted at rest |
| `agefile` | `credentials.agefile.path`, `credentials.agefile.identity_file` | read-only; cannot hold the surface token |
| `openbao` | `credentials.openbao.addr`, `credentials.openbao.mount` (`secret`) | token read from the OS keyring, key `heo/<profile dir>/openbao-token` |

`credentials.surface_token_ref` names the store key of the profile's
surface token (`heo/<profile dir>/surface-token`), never the token:
`heo service setup` mints the token into the backend and sets the key
(see [surfaces.md](surfaces.md)).

Any other backend, kit's `env` included, fails `heo config validate`
with the supported list. A token, password or unknown key under
`credentials.*` fails it too.

A credential reference `<ref>` is stored under the key
`uma/<tenant>/<ref>` as a JSON envelope, for example
`{"v":1,"kind":"password","user":"support@example.com","password":"..."}`.
heo reads the store on every connection attempt, so a rotated value is
used on the next one. When a provider rejects a credential that has not
been rotated since, heo stops handing it out as new and reports it as
rejected until a new one is stored.

### One secret per purpose

An adapter asks for a reference's credential for a named purpose
(`imap`, `meta.app_secret`, `storage.postgres`, ...; each adapter's
purposes are listed in [adapters.md](adapters.md#credential-purposes)).
heo looks under two keys, in order:

1. `uma/<tenant>/<ref>/<purpose>`: that purpose's own entry;
2. `uma/<tenant>/<ref>`: the entry every purpose without its own
   shares.

A reference is missing only when neither is stored. Store one shared
entry when a single secret serves every purpose (a mailbox login used
for IMAP and submission), and an entry per purpose when they differ (a
WhatsApp access token, app secret and verify token). The two mix: a
purpose's own entry overrides the shared one for that purpose only.

Each entry rotates on its own. A provider rejecting the credential
served for one purpose marks it rejected for that purpose only, even
when it came from the shared entry; storing a new value where that
purpose resolves (its own entry, or the shared one when it has none)
clears it.

A purpose is one key segment: letters, digits and `._@+-`, starting
with a letter, digit or `_`. A reference containing `/` is escaped
within its segment, so `wa/main` for purpose `meta.graph` is
`uma/<tenant>/wa%2Fmain/meta.graph`.

The `file` backend keeps one file per entry under
`credentials.file.dir`, a purpose's own file beside the shared one, so
a reference holds both:

| Key | File |
|-----|------|
| `uma/<tenant>/<ref>` | `<dir>/uma/<tenant>/<ref>` |
| `uma/<tenant>/<ref>/<purpose>` | `<dir>/uma/<tenant>/<ref>@<purpose>` |

The `@` separates the reference from the purpose, so an `@` inside
either is written `%40` in the file name: reference `ops@example.com`
is the file `ops%40example.com`, and its `imap` entry
`ops%40example.com@imap`. Purpose files kept in a directory named after
the reference (`<ref>/<purpose>`), the layout of earlier builds, are
not read: every lookup of that reference fails, and the diagnosis
names the move to `<ref>@<purpose>`.

When a credential is missing, the diagnosis names both keys it looked
under; once found, the key that held it. Neither ever carries a
credential.

### OAuth grants

A provider that takes a bearer token (Gmail, Microsoft Graph) expires
it within the hour. For such a credential, store the grant under the
reference instead of a token: heo mints an access token from it,
caches it and refreshes it shortly before it expires, so no one
rotates a token by hand. The grant is a JSON envelope; it names
secrets by store key and never holds one:

```json
{"v":1,"kind":"refresh_token","token_url":"https://oauth2.googleapis.com/token",
 "client_id":"1234.apps.example.com","scopes":["https://mail.google.com/"],
 "secret_ref":"uma/default/gmail-refresh","client_secret_ref":"uma/default/gmail-client"}
```

| `kind` | Needs | Secret `secret_ref` names |
|--------|-------|---------------------------|
| `client_credentials` | `client_id`, `secret_ref` | the client secret |
| `client_assertion` | `client_id`, `client_cert_ref` | (a `client_cert` envelope: certificate and RSA key) |
| `jwt_bearer` | `secret_ref`, optional `subject` | a service account key JSON |
| `refresh_token` | `client_id`, `secret_ref`, optional `client_secret_ref` | the refresh token |

`token_url` is https (plain http only for a loopback host), and
`secret_ref`, `client_secret_ref` and `client_cert_ref` are store keys
of `secret` envelopes (`{"v":1,"kind":"secret","value":"..."}`), in the
same store as the grant. A purpose resolves a grant like any other
entry, its own entry first, then the shared one, and the credential it
gets is an `oauth2` one: the access token and its expiry, plus the
`user` and `authzid` of the cached token envelope when another writer
stored them (the mailbox identity IMAP XOAUTH2 names; a token heo mints
carries none). An entry
that holds a credential envelope instead is used as it is.

Where the store can be written (`keyring`, `file`, `openbao`), heo
keeps the access token beside the grant (`<key>.token.<digest>`, a
`oauth2` envelope, so other processes and tools read it too) and writes
a rotated refresh token back to `secret_ref` before using it. Callers
and processes asking at once share one refresh. When a provider
rejects an access token, heo drops it and mints a new one once; a
token the provider issues again unchanged is reported as rejected. A
grant the provider no longer honours (the user revoked consent, or the
refresh token expired) is `unauthorized`: consent again, then store
the new refresh token.

An aps-bound profile and the `agefile` backend cannot be written, so
there the access token is kept in memory only and re-minted when the
process restarts. A `refresh_token` grant is refused there, since a
provider may rotate the refresh token on any use and heo could not keep
the new one: `heo config validate` fails the channel (code
`grant_not_writable`) and the message names the backends that work:

```text
a refresh_token grant needs a writable credential backend to keep a
rotated refresh token, and the agefile backend is read-only: use a
native profile with credentials.backend set to one of keyring, file,
openbao
```

`heo channel check --remote` reports each purpose's credential with the
grant kind (`grant_kind`) and the access token's expiry (`expires_at`),
never a token. It mints a token to do so when none is cached.

### aps-bound profiles

An aps-bound profile (`aps:<id>`) resolves references in the aps
profile's secrets file, `<aps data>/profiles/<id>/secrets.env`: the
reference is the secret name, the value the same envelope in single
quotes:

```sh
SUPPORT_MAILBOX='{"v":1,"kind":"password","user":"support@example.com","password":"..."}'
```

A purpose's own entry is the secret `<ref>__<PURPOSE>`: the reference,
two underscores, then the purpose upper-cased with every `.`, `-`, `@`
and `+` turned into `_`. heo tries it first, then `<ref>`:

| Reference | Purpose | Own entry | Shared entry |
|-----------|---------|-----------|--------------|
| `SUPPORT_MAILBOX` | `imap` | `SUPPORT_MAILBOX__IMAP` | `SUPPORT_MAILBOX` |
| `SUPPORT_MAILBOX` | `smtp.submission` | `SUPPORT_MAILBOX__SMTP_SUBMISSION` | `SUPPORT_MAILBOX` |
| `WHATSAPP` | `meta.webhook.verify_token` | `WHATSAPP__META_WEBHOOK_VERIFY_TOKEN` | `WHATSAPP` |

Avoid naming another reference `<ref>__<something>`: it would read as a
purpose entry of `<ref>`.

heo only reads that file. The profile's surface token stays in heo's
own store (`credentials.backend`) under `heo/aps-<id>/surface-token`.

## Storage

Each profile owns one data store. `storage.driver` picks the driver;
its settings live under `storage.<driver>`. This release ships one
driver, `postgres` (PostgreSQL 15 or later). `heo config validate`
rejects a driver name the binary does not have and lists the ones it
does.

| Key | Default | Meaning |
|-----|---------|---------|
| `storage.driver` | `postgres` | driver for this profile's store |
| `storage.backup.dir` | `<data dir>/heo/<profile dir>/backups` | where backups go, owner-only |
| `storage.blobs.dir` | `<data dir>/heo/<profile dir>/blobs` | where attachment bytes are kept, owner-only (heo sets the directory 0700 on every start); `heo storage backup` and `restore` carry it with the store |
| `storage.postgres.dsn` | empty: libpq defaults and `PG*` variables | connection string or URL, **without** a password |
| `storage.postgres.password_ref` | empty | credential reference for the password (or login and password) |
| `storage.postgres.max_conns` | `0`: pgx default | pool size |
| `storage.postgres.pg_dump`, `storage.postgres.pg_restore` | `pg_dump`, `pg_restore` | client tools for backups and restores; their major version must be at least the server's |

```sh
heo config set storage.postgres.dsn 'host=db.internal dbname=heo_work user=heo'
heo config set storage.postgres.password_ref heo/postgres
```

A DSN that carries a password is refused: the password comes from the
credential backend and reaches `pg_dump`/`pg_restore` through their
environment, never their command line.

`storage.postgres.password_ref` resolves through the profile's
credential backend (see [Credentials](#credentials)), the same store
and reference format channels use, for purpose `storage.postgres`. A
reference with nothing stored fails with the backend's diagnosis: the
store key it looked under and how to fix it, never a credential.

Attachment bytes live in `storage.blobs.dir`, not in the database.
`heo storage backup` writes both into one file, a tar archive in
`storage.backup.dir`: a manifest, the store's dump, and the blob
directory's files. The manifest lists every entry with its size and
SHA-256 checksum, and the attachment count and total size. `heo storage
restore` reads the whole archive against that manifest before it
changes anything, then replaces the store and the blob directory
together. One file is all there is to keep, copy or ship. It is as
large as the attachments, so size `storage.backup.dir` for them; the
backup heo takes before a migration holds the store alone, since a
migration does not touch attachments. The local directory is the only
blob backend today; one that kept bytes elsewhere would back them up
itself, and heo's archives would hold the store alone.

Backups and restores run `pg_dump` and `pg_restore` on the host heo
runs on: install PostgreSQL client tools whose major version is at
least the server's.

Only one heo may own a store at a time: a second daemon on the same
database and schema is refused, and the refusal names the holder. heo
backs the store up into `storage.backup.dir` before every migration,
refuses a store a newer heo migrated (no downgrade; upgrade heo or
restore a backup taken before that migration), and does not start
workers on a store whose schema is not its own.
`heo storage` backs up, restores and migrates the store and reports
its versions; see [commands](commands.md#storage). `heo upgrade` reads
`upgrade.*`; see [upgrade](upgrade.md).

## Plugins

`plugins:` lists the hook plugins the runner starts, once
`services.hooks.enabled` is set (see [runtime.md](runtime.md#hook-runner);
a plugin's result is not applied yet). Each is a process heo runs
or a socket it connects to, and reacts to events by returning operations
(see [hooks](hooks.md) and
[deliveries and events](deliveries-and-events.md#hook-plugins)).

```yaml
plugins:
  - name: triage                  # lowercase letters, digits, '-'; unique
    command: /usr/local/bin/heo-triage   # or socket: /run/triage.sock
    args: [--verbose]
    mode: exec                    # exec (default) or stream
    filter:
      kinds: [message.received, conversation.changed]
      channels: [mail]
      inboxes: [support]
    timeout: 30s
    concurrency: 4
    dry_run: false                # true: log what results would run, run nothing
    retry:
      max_attempts: 5
      backoff:
        initial: 1s
        max: 5m

authorization:
  grants:
    - actor: plugin:triage
      roles: [observer, desk]
```

| Field | Meaning | Default |
|-------|---------|---------|
| `name` | the plugin's name; its actor is `plugin:<name>` | required |
| `command`, `args` | executable (a path, or a name found on `PATH`) and its arguments | one of `command` or `socket` is required |
| `socket` | absolute unix socket path of a plugin server that is already running; heo connects and speaks the stream framing, concurrency 1, no token handover (see [hooks](hooks.md#what-a-plugin-process-gets-and-what-is-isolated)); the socket must be owned by the daemon's uid and not world-writable | none |
| `mode` | `exec`: a fresh process per event. `stream`: one process fed newline-delimited envelopes | `exec` |
| `filter.kinds` | event kinds from the [hooks catalog](hooks.md#catalog) | all catalog kinds |
| `filter.channels`, `filter.inboxes` | channel and inbox keys or IDs the event must belong to | all |
| `timeout` | longest one event may take | `30s` |
| `concurrency` | events in flight at once, 1 to 64; `stream` and `socket` are 1 | `1` |
| `dry_run` | record the operations a result would run in the daemon log and run none of them | `false` |
| `retry.max_attempts` | attempts per event, the first included, 1 to 100; after the last the event is dead-lettered | `5` |
| `retry.backoff.initial`, `.max` | wait before the second attempt, doubling up to the cap | `1s`, `5m` |

A plugin has no roles of its own: it holds exactly what
`authorization.grants` gives `plugin:<name>` (see
[authorization](authorization.md#a-plugins-actor)).

`heo config validate` refuses:

- an event kind not in the catalog;
- a plugin with no `authorization.grants` entry for `plugin:<name>`;
- a `command` that is not found or is not executable;
- two plugins with one name;
- a missing or invalid name, neither or both of `command` and `socket`,
  an unknown field, a mode other than `exec` or `stream`, and a timeout,
  concurrency or retry value out of range, and a `socket` plugin with
  `concurrency` above 1.

A `socket` is not probed: the plugin may start after heo; events wait in retry
until it is there.

## Daemon flags and keys

`heo service start` flags set the same values as these keys for one
run. The service reads its flag itself, so a flag beats every layer
for its key, `-c key=value` included.

| Flag | Key |
|------|-----|
| `--socket` | `services.socket.path` |
| `--addr` | `services.api.addr` |
| `--rpc-addr` | `services.rpc.addr` |
| `--mcp-addr` | `services.mcp.addr` |
| `--stdio` | `services.mcp.transport: stdio` |
| `--enable`, `--disable` | `services.<service>.enabled` (this run only) |

`--policy <name>` on `service start` names the policy served calls run
under: `kit-default`, or `policies/<name>.yaml` next to the user file
(`$XDG_CONFIG_HOME/heo/policies/<name>.yaml`). A policy allows or
refuses commands by side-effect class; for example, served writes
refused:

```yaml
name: deny-writes
allow:
  write: []
  destructive: []
```
