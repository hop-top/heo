# The daemon's runtime services

`heo service start` runs the profile's daemon in the foreground. Beside
the control surfaces (`socket`, and `api`, `rpc`, `mcp` when enabled) it
runs five runtime services on the profile's data store:

| Service | Does | Enabled by default |
| --- | --- | --- |
| `workers` | Runs queued operations (sends, replies, redactions), `services.workers.count` at once | yes |
| `receivers` | Runs each receiving channel's persistent receiver, one per provider account | yes |
| `webhook` | Serves channels' webhooks on its own listener; answers `/healthz` and `/readyz` | no |
| `bridge` | Republishes uma's committed events, as references, on the daemon's in-process bus for hooks | yes |
| `hooks` | Runs the configured hook plugins (exec or stream, any language), one durable subscription each; see [write a plugin](hooks.md#write-a-plugin-in-any-language) | no |

```sh
heo service start                   # socket, workers, receivers, bridge
heo service start --enable webhook  # plus the webhook listener, this run
heo service start workers           # exactly one service
heo service list                    # what is registered and enabled
```

With no channel configured, the runtime services start and report
ready: an empty install needs no provider.

## The data store

The first runtime service to start opens the profile's data store
through its storage driver (`storage.driver`, see
[configuration.md](configuration.md)) as its **exclusive owner**. The
start is refused, exit 1, when:

- another daemon owns the store — the refusal names it; stop it first;
- the store's schema is older than this heo's, or was never initialized
  — run `heo storage migrate` (it backs the store up first), then start
  again;
- a newer heo migrated the store — install that release, or restore a
  backup taken before the migration with `heo storage restore <path>`;
- the store cannot be reached.

Ownership is checked every 10 seconds while the daemon runs. A daemon
that loses it stops every runtime service and exits 1, since another
daemon may now own the store.

## One receiver per provider account

Two channels on one provider account (one bot, one mailbox, one
phone number) would receive the same traffic twice. At start,
`receivers` and `webhook` refuse, exit 1, when:

- two channels of the profile name the same account — the refusal
  lists them; remove or reconfigure all but one;
- a daemon of another profile on this host already receives the
  account — the refusal names its profile and pid; stop it or remove
  the channel from one profile.

Every channel that receives counts, through a persistent receiver or
over the webhook; [adapters.md](adapters.md#one-receiver-per-provider-account)
lists what each adapter's account is. Accounts are claimed with a lock
file per account under
`<runtime dir>/heo/_accounts/`, named by a hash: the account itself is
never written to disk. A claim goes with its process, crash included.

While the daemon runs, the same conflict never stops a service.
`channel create` and `update` already refuse an account another channel
of the profile receives (exit 4). A channel created or updated onto an
account another profile's daemon receives fails on its own scope
instead (`channel:<id>`, state `failed`, the reason naming who receives
the account). Its webhook route answers 503, and the other channels
keep running. When the account is freed,
because the other channel moved off it or the other daemon stopped, the
next channel or inbox write gives it to the waiting channel. An updated
channel's account changes hands in one step: its old claim is released
once its receiver has stopped, and the new account is claimed before
its new receiver starts.

## Channel changes while running

Every committed `channel` and `inbox` write applies to the running
`receivers` and `webhook` at once, without a restart. The daemon compares
the stored channels with the ones it runs, by channel and revision:

- a new channel starts;
- a channel whose revision changed is stopped and started again: its
  receiver is replaced, and its webhook route is replaced on the
  new configuration. Requests open on the old route end;
- a channel that no longer offers a webhook loses its route (404) and
  its `webhook` scope; one whose configuration now switches its
  receiver off (a Telegram bot moved to `"receive":"webhook"`) loses its
  receiver and its `receivers` scope;
- every other channel is left alone: its receiver keeps running, and
  requests open on its route continue.

Scopes follow the change. A replaced channel reads `starting` until its
listener binds, if it has one, and a stopped channel's scope is removed.
Inbox changes apply to the next inbound message, since each inbound
message is filed by the inbox as stored.

## Workers

```yaml
services:
  workers:
    count: 4             # workers running at once, 1-256
    poll: 1s             # idle wait between claims
    lease: 1m            # a claim's hold on its item; at least 5s
    drain_timeout: 20s   # bound on the drain at shutdown
    held_sweep_every: 0s # held-operation expiry sweep; 0s is uma's 1m, off for none
```

Each worker claims one item at a time. Work a crashed daemon held is
claimable again once its lease expires.

## Event bridge

```yaml
services:
  bridge:
    enabled: true   # the default; false runs no bridge
```

The bridge tails the events uma commits as a durable subscription of
its own, `heo-bridge`, and republishes each on the daemon's bus for
hooks and notifications, audience-filtered and as references only. What
it publishes, how subscribers are scoped and what delivery it gives are
in [deliveries and events](deliveries-and-events.md#events-on-the-daemons-bus).
The bus is not an outbox: a hook that needs every event exactly once
reads `heo event watch` or `heo event list`.

The subscription is the bridge's only cost. It is not `event watch`'s:
it reads as its own actor and keeps its own checkpoint in uma's
consumer state, so a watch and the bridge never move each other's
position, and a restart resumes where the last run stopped. A failure
of uma leaves the bridge degraded and retrying, not the daemon unready;
`service status` says so, in the `bridge` section.

## Hook runner

```yaml
services:
  hooks:
    enabled: true   # off by default; runs the plugins: section
```

The runner starts one reader per plugin in `plugins:` (see
[configuration](configuration.md#plugins)). Each reads uma's log as
a durable subscription of its own, `plugin-<name>-<hash>` (see
[hooks](hooks.md#subscription-naming)), as the plugin's own actor
`plugin:<name>`, so a plugin receives what its roles allow and no more.
A new subscription starts at the tail; a restart resumes at its
checkpoint.

- **Ordering.** A batch is handled in log order. Events of one
  conversation reach the plugin one at a time; those of different
  conversations run in parallel up to the plugin's `concurrency`. A
  `stream` plugin is one ordered process.
- **Cursor.** The batch is acknowledged only after every event in it was
  applied, or recorded as a dead letter: exit 2, or `retry.max_attempts`
  spent. A dead letter that cannot be recorded, a result that cannot be
  applied, or a stop in the middle leaves the batch unacknowledged, and
  it is read again (at least once).
- **Isolation.** Each plugin has its own subscription, goroutines,
  timeout and process, and shares nothing with the others, the workers,
  the receivers or the bridge: a plugin that hangs, fails or crashes
  holds only itself, and a plugin that fails to open is degraded and
  retried while the rest run. A plugin runs with a minimal environment
  (`PATH` only), not the daemon's.
- **Applying.** A plugin's operations run in order as `plugin:<name>`
  through the command tree, the path a REST request for the verb takes,
  so the policy decides each (see
  [deliveries and events](deliveries-and-events.md#hook-plugins)).
  `plugins[].dry_run: true` records what would run, in the daemon log,
  and runs nothing.
- **Failures.** An attempt fails as one class: `timeout`, `crash` (killed
  by a signal, an exit code other than 0, 1 and 2, no start), `malformed`
  (not a valid result, or over the stdout cap), `retryable_exit` (exit 1),
  `operation` (an operation of a valid result failed to apply),
  `permanent` (exit 2, or a refusal that retrying will not change) or
  `error`. All but `permanent` are retried, up to `retry.max_attempts`,
  after a wait that doubles from `retry.backoff.initial` to
  `retry.backoff.max` with equal jitter: the wait is drawn from the upper
  half of that delay, so plugins that fail together do not retry together.
  The attempt count is kept on disk, so a restart does not renew an
  event's attempts.
- **Dead letters.** An event that fails for good is kept in a per-plugin
  list on disk (`hook-dead` beside the plugins' directories): the event
  ID and kind, the attempts, the failure class and message, the
  operation index that failed, and the first and last attempt times. It
  holds references, not content. A dead letter can be replayed (the
  event is read again as the plugin and run through it; operations
  already applied are skipped under their keys) or discarded; both stay
  listed as history.
- **Health.** `heo.runtime.hooks.<action>` on the bus, desk scope:
  `retry`, `dead_lettered`, `recovered`, `replayed` and `discarded`, each
  with the plugin, the event ID, the attempt, the class and, for an
  operation, its index; never the failure's text.
- **Not yet.** The `plugin token` environment and `socket` plugins come
  later.

`service status` shows each plugin in the `hooks` section.

## Webhook listener

```yaml
services:
  webhook:
    enabled: true
    addr: 127.0.0.1:8083                            # loopback by default
    public_base_url: https://hooks.example.com/heo  # what providers are given
```

A channel whose adapter receives over HTTP is mounted at
`/webhooks/<channel id>`; its provider-facing URL is
`public_base_url` followed by that path. `public_base_url` is the
provider-facing base only, never a UI URL, and carries no credentials:
the adapter verifies every request by the provider's signature. The
listener binds loopback; put a proxy or tunnel in front of it, or set
`services.webhook.allow_remote: true` to bind another address. It is
separate from every control surface: no command is reachable on it.

## Readiness

Each runtime service is ready once what it needs is acquired: the
store open and owned, the webhook listener bound. The receivers
service is ready once every channel has settled: its receiver is up
(see [How a receiver reads](#how-a-receiver-reads)), or the channel
waits or failed. An `smtp.mail` channel's MX listener must be bound
too; until then its scope reads `starting`. A channel that
cannot receive — its adapter is missing from this build, it fails to
open, or it fails for good — is reported on its own scope,
`channel:<id>`, and the service is **degraded**, not down: the other
channels keep receiving. A receiver lease a stopped daemon still holds
is waited out (scope `waiting`).

A channel whose adapter has not connected yet, or whose first
connections fail with a retryable failure (a provider that cannot be
reached, a lease take that lost a race with another channel's), has not
settled: its scope reads `starting` with the failure as its reason, and
the service waits for the retry. `services.receivers.settle_timeout`
(default `10s`) bounds that wait: a channel still connecting or
retrying then reads `waiting`, the service reports ready and degraded,
and the channel keeps retrying until it serves. A daemon started with a
provider unreachable is therefore ready after that timeout, degraded. The timeout must be below
`services.receivers.ready_timeout` (default `30s`), so a slow start
degrades the service rather than failing it. A `google.mail` or `microsoft.mail`
channel's first start at daemon start is spread: uma holds its first
connection by a stable per-channel offset within `services.receivers.start_spread`
(default `5m`, the adapters' poll interval; `0s` for none), so the first polls
of the profile's mailboxes do not line up. The adapters also spread each poll
wait and each watch and subscription renewal by plus or minus 10%, and uma
spreads receiver lease renewals, so the schedules do not drift back into
phase. uma reports no state for the hold, so until the window ends the
channel reads `starting` with the reason `first connection spread over
<window>`, and it counts as settled: readiness does not wait for it, and
mail arrives from its first poll. A channel added or changed while the
daemon runs starts at once, and so does a receiver restarted after a
failure, after its backoff plus or minus 10%. A channel whose adapter does not currently receive over
HTTP (a mailbox with push or notifications off) is not mounted on the
webhook listener.

Which way a channel receives follows the channel's capability status,
as uma reports it for the channel, not only what its adapter can do: a
receiver runs when `receive` is available, a route is mounted when
`receive_http` is. A way the channel's own configuration switches off
(status `disabled`, such as the way a Telegram channel's `receive` does
not name) is neither started nor mounted, has no scope and degrades
nothing; `heo channel check --remote` lists it. Any other unavailable
reason, such as a withdrawn permission, is a fault: the receiver or
route is tried, and its scope reports why it fails.

`/readyz` on the webhook listener answers 200 when the configuration
validates, the store is open and owned, and every runtime service this
run started is ready (degraded counts as ready); otherwise 503, naming
the failing checks (`config`, `storage`, `workers`, `receivers`,
`webhook`) on a loopback listener. `/healthz` answers 200 while the
listener serves. When the REST surface (`api`) is enabled, its own
`/readyz` also waits on the runtime services.

The daemon writes its pidfile, `<runtime dir>/heo/<profile dir>/heo.pid`,
once every service of the run is ready, and removes it at exit. A
daemon refused at start never writes it.

## How a receiver reads

A receive call that runs is not a receiver that serves: it may be
connecting, or waiting to reconnect. The daemon asks uma what the call
is doing and shows the answer on the channel's scope in `heo service
status` and in `heo channel check --remote` (the `receiving` report,
which also names the phase, the link, the failures and when the next
connection opens):

| Scope | When |
| --- | --- |
| `starting` | the channel has not settled: its adapter's link is not up yet, or its call is retrying a failure, until `services.receivers.settle_timeout`; an `smtp.mail` listener is not bound yet |
| `ready` | the adapter reports its provider link up (the Discord gateway, Slack Socket Mode, Telegram polling, an IMAP session), or the adapter reports no link and its call runs with no failure since it last accepted a message |
| `waiting` | the call is retrying a failure (the reason names the failure's code, how many failures and when the next connection opens), or the link is down or not up, or an adapter that reports no link has reconnected after failures and accepted nothing since; also a receiver lease another daemon holds and a provider's own conflict, see below. Not receiving yet |
| `degraded` | the receiver still receives, by polling, while its adapter reports a part it relies on failing (push or change notifications), or a credential those need is missing; see below |
| `failed` | the call ended on a failure that is not retried; the reason is the error |

A mail receiver whose push registration (`google.mail`) or change
notification subscription (`microsoft.mail`) fails keeps polling: mail
still arrives, so the call is not failing, but the channel is not
healthy. uma reports the link `degraded` with a stable code, and the
scope reads `degraded`, not `waiting`: it receives, it is only
not healthy, with the
code and what it means, for example `push registration refused
(google.mail.push_denied): mail arrives by polling only`. The scope's
json carries the code as `code`, `heo channel check --remote` as
`detail` beside `link degraded`, and the receivers section counts it as
`degraded` in its digest. A missing `graph.notifications` secret reads
`degraded` the same way, since mail arrives by polling. It is ready again once the adapter's next
registration succeeds. Permanent causes degrade at the first failed
attempt, transient ones after three failed rounds in a row or when the
live registration would expire first. A code heo does not know is shown
as it is.

| Code | Meaning |
| --- | --- |
| `google.mail.push_denied` | push registration refused (permission) |
| `google.mail.push_rejected` | push registration rejected (another permanent cause) |
| `google.mail.push_unavailable` | push registration unavailable (transient) |
| `microsoft.mail.subscription_rejected` | change-notification subscription rejected |
| `microsoft.mail.client_state_missing` | notification secret missing |
| `microsoft.mail.subscription_unavailable` | subscription unavailable (transient) |

A channel with both this and a missing credential its configuration
needs (see [adapters](adapters.md)) reads one reason: the credential's,
which names what stays off; a degradation of another cause follows it.

A failure that states its own wait, such as a rate limit's reset
(Discord's identify budget), is waited out from the first failure, up
to 24 hours, with no connection opened before then. A channel whose
receiver would only open its next connection after
`services.receivers.settle_timeout` settles `waiting` at once. Other
retryable failures are retried by uma with a growing, jittered backoff
of 1 second up to 5 minutes; after five connections in a row that
accepted nothing the call ends, and the daemon starts it again after
its own backoff, 1 second doubling to 5 minutes (starting over once a
call has run for 5 minutes), or after the wait the last failure stated,
if longer. The scope stays `waiting` until the next call connects.

The status lives in the memory of the daemon that runs the call and is
read once per channel at most every 2 seconds, sooner while a channel
starts or changes; a scope can lag the call by that much. A lease held
by another process on the same store shows its owner and when it
expires, and no phase: ask that process. A crash loses nothing the
daemon keeps: the next start begins at `starting`.

## Status

`heo service status` asks the profile's daemon how it stands: kit's
rows (service, ready, address, pid), then heo's sections, which the
daemon computes itself, so they describe the daemon, not the caller.

| Section | Reports |
| --- | --- |
| `heo` | profile, tenant, version, pid |
| `storage` | `state` (`ready`, `closed`, `lost`, `failed`) and, while open, the store's driver, server, store library and schema versions and their compatibility |
| `store-owner` | who holds the store's ownership: `owner` (`heo/<profile dir>`), `pid`, `host`, `since` when, and `verified_at`, the last time the periodic check found it still held. An error once ownership is lost |
| `workers`, `receivers`, `webhook` | the service's state, its listener, and each scope's state (`starting`, `ready`, `waiting`, `degraded`, `failed`) and, for a receiver degraded as above, its `code`; `not_ready` names the scopes that are not ready, a channel by its key (its scope, `channel:<id>`, when it has none) |
| `bridge` | the bridge's `state` and `consumer`, its `checkpoint`, the `last_event` it published and when uma recorded it (`last_event_at`), `published` since it started and `dropped` for subscribers that fell behind; `not_ready` says why it is retrying. How far it is behind the log head is not known cheaply, so a bridge that keeps up shows a recent `last_event_at` |
| `hooks` | one entry per plugin: its `consumer`, `mode` and `state`; the subscription's `cursor`; `lag`, how long the oldest event of the batch in hand has waited since uma recorded it (empty once caught up), with `in_flight` events not yet handled; `delivered`, `dead_lettered` and `skipped` since the runner started; and `last_error` (the event, when, and why). `not_ready` names the plugins that are retrying |
| `queue` | the operation queue's depth: `waiting` (queued, and retries scheduled for later), `in_flight` (claimed by a worker, lease unexpired), `dead_lettered` (quarantined after repeated unfinished claims; runs again only once replayed). Counts only, read from the store on each request |
| `versions` | `heo`, `kit` and `uma` as built, the `store` library, the store's `schema` against this binary's `library_schema`, and `compat` |

A runtime section with a scope that is not ready (`starting`, `waiting`,
`degraded` or `failed`) reads `degraded`, not `ok`, and its DETAIL cell is a digest
that does not grow with the channel count, such as `20/200 not ready
(failed 20): gm-000, ms-000, gm-001 +17 more` or `5/200 not ready
(failed 2, degraded 3)`: the counts, then the
first three by name. The same digest is `error_message` in json, where
`scopes` and `not_ready` list every one with its reason. Degraded is not
an error: the service is ready (see [Readiness](#readiness)), so the
exit code is 0.

A section with nothing to report — no runtime service started, so no
store open — reads `empty`. No section carries the store's connection
string, a credential or a credential reference.

```sh
heo service status                # rows, then one line per section
heo service status --format json  # every field
```

With no daemon running, `service status` exits 3 (`NOT_FOUND`).

## Stopping and restarting

`heo service stop`, SIGTERM, SIGINT or SIGHUP stops the daemon in the
reverse of start order:

1. the hook runner stops: a plugin's delivery in hand is cancelled and
   its batch left unacknowledged, to be read again on the next start;
2. the webhook listener stops accepting and finishes requests in
   flight;
3. receivers stop;
4. workers drain: nothing new is claimed, claimed work finishes,
   within `services.workers.drain_timeout`;
5. the event bridge publishes and acknowledges the batch in hand, and
   leaves its subscription at its checkpoint;
6. the store is closed and its ownership released;
7. the control socket closes last.

A clean drain exits 0: a delivery in flight finishes, and the next
start does not send it again. Work still running at the drain deadline
is abandoned to its lease; the daemon then exits 1 and says so. The
next start's workers take the item up once that lease expires
(`services.workers.lease`); an abandoned send may already have reached
the provider, so it is never sent again blindly: unless the adapter can
reconcile it with the provider, its operation ends `outcome_unknown`. `heo service stop`
waits for the drain (`--wait`, exit 6 if it outlasts it) and exits 0;
with no daemon running it reports `not_running`, exit 0, and creates
nothing on disk.

| Signal | Effect |
| --- | --- |
| SIGTERM, SIGINT | the drain above; exit 0 |
| SIGHUP | the same: a closed terminal stops the daemon gracefully. heo has no reload, so there is no other meaning to give it |
| a second one of any of these | aborts the drain at once: the sends in flight are interrupted and stay leased (settled by the next start as above, never lost, never sent again), the daemon logs `drain aborted by second signal` (once for each service still stopping) and exits 1 within a fraction of a second |

Nothing here sends SIGKILL to anything: a stop, however often repeated
with `heo service stop`, never escalates; only a second signal sent to
the daemon itself does.

`heo service restart` drains the same way, then runs the daemon again
in the same process: the pid, and so the pidfile, stay; the binary and
the configuration are read afresh. It prints the new run's `service
status`.

Each profile's daemon answers `service stop`, `restart` and `status`
on a kit control socket of its own, `<runtime dir>/heo/control-<profile
dir>.sock` (with a `.lock` beside it), apart from the profile's heo
socket. A second daemon for the same profile is refused, exit 4, before
it starts anything.
