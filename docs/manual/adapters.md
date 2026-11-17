# Adapters

heo ships these adapters: direct-message adapters, chat adapters for
Slack, Telegram, Discord and Microsoft Teams, a relay to a gateway you run for SMS, WhatsApp and iMessage, and two ticket adapters whose conversations are bound to a provider work item (see
[issues-and-tickets.md](issues-and-tickets.md)). A channel is one configured connection of an
adapter: its type, its configuration (stored in the profile's data
store) and a credential reference resolved through the profile's
credential backend (see [configuration.md](configuration.md#credentials)).

| Type | Provider | Receives | Sends | Sends first |
| --- | --- | --- | --- | --- |
| `native.webwidget` | a chat widget on your own site | webhook | yes | no |
| `smtp.mail` | inbound mail (MX) and a submission relay | own SMTP listener | yes | yes |
| `imap.mail` | a mailbox over IMAP and SMTP submission | receiver | yes | yes |
| `google.mail` | Gmail API | receiver; webhook with Pub/Sub push | yes | no |
| `microsoft.mail` | Microsoft Graph mail | receiver; webhook with change notifications | yes | no |
| `twilio.sms` | Twilio Programmable Messaging, SMS/MMS | webhook | yes | yes |
| `twilio.whatsapp` | Twilio WhatsApp senders | webhook | yes | no |
| `meta.whatsapp` | WhatsApp Cloud API | webhook | yes | no |
| `slack.chat` | a Slack app installed in one workspace: rooms, threads, direct messages | webhook (request URL) or receiver (Socket Mode, experimental), never both at Slack | yes | yes |
| `telegram.messages` | a Telegram bot: chats, forum topics, private chats | webhook or receiver (long polling), never both at Telegram | yes | yes |
| `relay.messages` | SMS, WhatsApp or iMessage through a gateway you run (iMessage first) | webhook | yes | yes |
| `discord.chat` | a Discord bot application: server channels, threads, direct messages | receiver (the gateway, a WebSocket); webhook (Interactions Endpoint URL) | yes | yes |
| `microsoft.teams` | a Microsoft Teams bot: channels, channel threads, group and personal chats | webhook (Bot Framework messaging endpoint) | yes | yes |
| `github.issues` | GitHub issues and their comments | webhook | opt-in comments | no |
| `zendesk.tickets` | Zendesk Support tickets | receiver (polling); webhook | yes; ticket updates | no |
| `gitlab.issues` | GitLab issues and their notes (gitlab.com or self-managed) | webhook | opt-in notes | no |
| `jira.issues` | Jira Cloud issues and their comments | webhook (admin webhook) | opt-in comments | no |
| `linear.issues` | Linear issues and their comments | webhook | opt-in comments | no |

`heo adapter get <type>` and `heo adapter list` also show **verbs**: the
provider-dependent commands the adapter's declared capabilities enable
(`message edit`, `message delete`, `reaction add`, `reaction remove`,
`participant add`, `participant remove`, `receipt send`, `history fetch`,
`message raw`, `interaction respond`, `interaction open-modal`, `interaction push-modal`,
`interaction update-modal`, `conversation status`, `conversation update`,
`conversation tag`, `message reply-draft`,
`conversation unsubscribe`, `conversation archive`, `conversation unarchive`,
`conversation move`, `conversation read`, `conversation unread`,
`conversation flag`, `conversation unflag`, `conversation trash`,
`conversation untrash`).
`native.webwidget` declares the first four, `receipt send` and
`interaction respond`, `slack.chat` the first four, `history fetch`,
`interaction respond` and the three modal verbs, `telegram.messages` the same without `history
fetch`, `discord.chat` the same with the participant verbs (the members of a
thread), `microsoft.teams` the first four, `github.issues` the first four, `history
fetch`, `conversation status` and `conversation update`, `twilio.sms` `history fetch`,
`meta.whatsapp` the reactions, `zendesk.tickets` `conversation update`,
`gitlab.issues` `message edit`, `message delete`, `history fetch` and `conversation status`,
`jira.issues` and `linear.issues` `message edit` and `message delete`,
`google.mail` and `microsoft.mail` `message raw`, `history fetch` and the tag change,
`imap.mail` `message raw` and `history fetch`, `relay.messages` none; only
`discord.chat` declares the participant verbs; see
[provider-verbs.md](provider-verbs.md).

**Sends first** is the `start_conversation` capability: `heo message
send` can start an exchange with someone who never wrote in (see
[conversations.md](conversations.md#start-an-exchange)). The others
only answer conversations that exist.

All of them are `experimental` in this release. What each declares for
the provider-dependent verbs, and which tests and recordings back it, is
in [provider-matrix.md](provider-matrix.md).

## Before any provider

How each provider's threads, rooms and issues become conversations and
messages, and which inbox modes each takes, is in
[provider-mapping.md](provider-mapping.md).

1. Start the daemon once with no channel: it reports ready with zero
   inboxes (see [runtime.md](runtime.md)).
2. Choose a credential backend and store each channel's credential
   under its reference, as a JSON envelope (`{"v":1,"kind":...}`); see
   [Credentials](configuration.md#credentials).
3. For every adapter that receives over a webhook, publish the webhook
   listener and set the URL providers reach it at:

   ```yaml
   services:
     webhook:
       enabled: true
       addr: 127.0.0.1:8083
       public_base_url: https://hooks.example.com/heo
   ```

   A channel is served at `<public_base_url>/webhooks/<channel id>`. The
   listener binds loopback: put a TLS-terminating reverse proxy or a
   tunnel in front of it. Providers need a public **https** URL.
4. Create the channel and its inbox with `heo channel create` and
   `heo inbox create`, and check it with `heo channel check --remote`.
   The running daemon starts receiving it as soon as it is created; see
   [channels.md](channels.md).

A channel has one credential reference, and the adapter asks for it
per purpose. Each purpose resolves its own entry when one is stored,
else the reference's shared entry (see
[One secret per purpose](configuration.md#one-secret-per-purpose)), so
an adapter needing several secrets gets each from its own entry; see
[Credential purposes](#credential-purposes).

A credential that is missing or rejected stops that channel's receiver
and reports it on its scope (`channel:<id>`), while every other channel
keeps working. After storing or rotating it, restart the daemon.

### Credential purposes

The purposes each adapter asks for. A purpose's own entry is
`uma/<tenant>/<ref>/<purpose>`; on an aps-bound profile it is the
secret `<ref>__<PURPOSE>` (see
[aps-bound profiles](configuration.md#aps-bound-profiles)).

| Type | Purpose | Kind | Used for |
| --- | --- | --- | --- |
| `native.webwidget` | `native.webwidget.session` | `secret`, 32+ bytes | signing visitor session tokens |
| `smtp.mail` | `smtp.relay` | `password`, `oauth2` or `client_cert` | relay submission |
| | `smtp.tls` | `secret` (PEM private key) | MX listener STARTTLS key, under the host reference `adapters.smtp.tls.key_ref` (see [STARTTLS](#starttls)) |
| `imap.mail` | `imap` | `password`, `oauth2` or `client_cert` | IMAP retrieval |
| | `smtp.submission` | `password`, `oauth2` or `client_cert` | SMTP submission |
| `google.mail` | `gmail` | `oauth2` | Gmail API |
| `microsoft.mail` | `graph` | `oauth2` | Graph API |
| | `graph.notifications` | `secret` | change-notification client state |
| `twilio.sms`, `twilio.whatsapp` | `twilio.rest` | `secret` or `password` | API calls |
| | `twilio.webhook` | `secret` or `password` | webhook signatures |
| `meta.whatsapp` | `meta.graph` | `oauth2` or `secret` | Graph API access token |
| | `meta.app_secret` | `secret` | webhook signatures |
| | `meta.webhook.verify_token` | `secret` | subscription handshake |
| `slack.chat` | `slack.chat.api` | `secret` or `oauth2` | Web API calls: the bot token (`xoxb-...`) |
| | `slack.chat.signing` | `secret` | verifying every request Slack sends: the app's signing secret; not needed under `receive: socket_mode` |
| | `slack.chat.socket` | `secret` | opening Socket Mode connections: the app-level token (`xapp-...`, scope `connections:write`); not needed under `receive: events_api` |
| `relay.messages` | `relay.webhook` | `secret` or `password` (the key) | verifying every message the gateway posts; with `ed25519`, the public key |
| | `relay.callback` | `secret`, printable without spaces | the callback and the lookup; not needed on a channel with no callback |
| `telegram.messages` | `telegram.messages.api` | `secret` | Bot API calls: the bot token (`<bot ID>:...`) |
| | `telegram.messages.webhook` | `secret`, 16+ characters of `A-Za-z0-9_-` | the webhook's secret token, checked on every request Telegram sends; not needed to receive by polling |
| `discord.chat` | `discord.chat.api` | `secret` | REST calls and the gateway: the bot token |
| | `discord.chat.interactions` | `secret` (the key, hex) | verifying every request Discord sends to the Interactions Endpoint URL: the application's public key; not needed under `receive: gateway` |
| `microsoft.teams` | `microsoft.teams.client_secret` | `secret` | Bot Connector calls: the bot's client secret (the secret's value, not its ID). heo mints and refreshes the Connector access token with it, and sends that token only to the channel's `service_url`. Receiving needs no credential: the adapter verifies each request against the Bot Framework's published keys. Store this or the certificate, never both |
| | `microsoft.teams.client_certificate` | `client_cert` (PEM certificate and RSA private key) | the same calls, with a certificate registered for the app in place of the client secret: heo signs a client assertion with the key to mint the token. Store this or the client secret, never both |
| `github.issues` | `github.issues.api` | `oauth2` or `secret` | API calls (App installation token or personal access token) |
| | `github.issues.webhook` | `secret` | webhook signatures |
| `zendesk.tickets` | `zendesk.tickets.api` | `secret` or `password` (`auth: api_token`); `oauth2` or `secret` (`auth: oauth`) | Support API calls |
| | `zendesk.tickets.webhook` | `secret` | webhook signatures |
| `gitlab.issues` | `gitlab.issues.api` | `oauth2` or `secret` (a personal, project or group access token) | API calls |
| | `gitlab.issues.webhook` | `secret` | the webhook's secret token (`X-Gitlab-Token`) |
| `jira.issues` | `jira.issues.api` | `password` (account email and API token), `secret` (API token, with the channel's `email`) or `oauth2` (with `cloud_id`) | REST calls |
| | `jira.issues.webhook` | `secret` | admin webhook signatures |
| `linear.issues` | `linear.issues.api` | `secret` (personal API key) or `oauth2` | GraphQL calls |
| | `linear.issues.webhook` | `secret` | webhook signatures |

Purposes sharing a value need only the shared entry: one login for an
IMAP mailbox's two servers, one Twilio auth token for API calls and
webhooks. Where values differ, store an entry per purpose; on the
`file` backend that entry is the file `<ref>@<purpose>` beside the
shared `<ref>` (see
[configuration.md](configuration.md#one-secret-per-purpose)).

For example, a `meta.whatsapp` channel with credential reference `wa`
in tenant `default`:

| Key | Value |
| --- | --- |
| `uma/default/wa/meta.graph` | `{"v":1,"kind":"secret","value":"<system user access token>"}` |
| `uma/default/wa/meta.app_secret` | `{"v":1,"kind":"secret","value":"<app secret>"}` |
| `uma/default/wa/meta.webhook.verify_token` | `{"v":1,"kind":"secret","value":"<verify token you chose>"}` |

A `slack.chat` channel with credential reference `slack` in tenant `default`
holds different secrets, so an entry each (on the `file` backend, the files
`slack@slack.chat.api`, `slack@slack.chat.signing` and
`slack@slack.chat.socket`). A channel with `receive: events_api` needs the
first two, one with `receive: socket_mode` the first and the last, one with
`receive` empty all three:

| Key | Value |
| --- | --- |
| `uma/default/slack/slack.chat.api` | `{"v":1,"kind":"secret","value":"<bot token>"}` |
| `uma/default/slack/slack.chat.signing` | `{"v":1,"kind":"secret","value":"<signing secret>"}` |
| `uma/default/slack/slack.chat.socket` | `{"v":1,"kind":"secret","value":"<app-level token>"}` |

A `telegram.messages` channel with credential reference `telegram` in tenant
`default` holds two different secrets as well (on the `file` backend, the
files `telegram@telegram.messages.api` and
`telegram@telegram.messages.webhook`; a bot that is only polled needs the
first):

| Key | Value |
| --- | --- |
| `uma/default/telegram/telegram.messages.api` | `{"v":1,"kind":"secret","value":"<bot token>"}` |
| `uma/default/telegram/telegram.messages.webhook` | `{"v":1,"kind":"secret","value":"<secret token you chose>"}` |

A `discord.chat` channel with credential reference `discord` in tenant
`default` holds two different values (on the `file` backend, the files
`discord@discord.chat.api` and `discord@discord.chat.interactions`); a
channel with `receive: gateway` needs the first only:

| Key | Value |
| --- | --- |
| `uma/default/discord/discord.chat.api` | `{"v":1,"kind":"secret","value":"<bot token>"}` |
| `uma/default/discord/discord.chat.interactions` | `{"v":1,"kind":"secret","value":"<public key, hex>"}` |

A `microsoft.teams` channel with credential reference `teams` in tenant
`default` holds one entry, the bot's client secret (on the `file` backend, the
file `teams@microsoft.teams.client_secret`), or its certificate and key (the file
`teams@microsoft.teams.client_certificate`), not both:

| Key | Value |
| --- | --- |
| `uma/default/teams/microsoft.teams.client_secret` | `{"v":1,"kind":"secret","value":"<client secret>"}` |
| `uma/default/teams/microsoft.teams.client_certificate` | `{"v":1,"kind":"client_cert","certificate_pem":"<PEM>","private_key_pem":"<PEM>"}` |

A `microsoft.mail` channel `m365` with change notifications: its token
under `uma/default/m365/graph` (or the shared `uma/default/m365`), and
`uma/default/m365/graph.notifications` holding a random `secret`.

The same `meta.whatsapp` channel on an aps-bound profile, reference
`WA`, in the aps profile's `secrets.env`:

```sh
WA__META_GRAPH='{"v":1,"kind":"secret","value":"..."}'
WA__META_APP_SECRET='{"v":1,"kind":"secret","value":"..."}'
WA__META_WEBHOOK_VERIFY_TOKEN='{"v":1,"kind":"secret","value":"..."}'
```

### One receiver per provider account

heo refuses to start when two channels would receive the same provider
account, within a profile or across the profiles of one host (see
[runtime.md](runtime.md#one-receiver-per-provider-account)). The
account each adapter is keyed on:

| Type | Account |
| --- | --- |
| `native.webwidget` | the channel itself (never shared) |
| `smtp.mail` | the listener, `adapters.smtp.addr` |
| `imap.mail` | IMAP host + mailbox address + folder |
| `google.mail` | `user`; for `me`, the credential reference |
| `microsoft.mail` | cloud + `mailbox`; for the signed-in user, the credential reference |
| `twilio.sms`, `twilio.whatsapp` | account SID + `messaging_service_sid`, else `from` |
| `meta.whatsapp` | `phone_number_id` |
| `slack.chat` | API host + `team_id` + `bot_user_id` |
| `telegram.messages` | API host + `bot_id`, whether the bot is received by webhook or by polling |
| `relay.messages` | medium + the callback URL (host and path); a channel with no callback, medium + the credential reference |
| `discord.chat` | API host + `application_id`, whatever servers the bot is in and however the channel receives |
| `microsoft.teams` | tenant + `app_id`: the Bot Connector posts every activity of a bot registration to its one messaging endpoint |
| `github.issues` | API host + `account_id`; without one, API host + the credential reference |
| `zendesk.tickets` | `subdomain` |
| `gitlab.issues` | API host + `account_id`; without one, API host + the credential reference |
| `jira.issues` | site host + `account_id`; without one, site host + the credential reference |
| `linear.issues` | API host + `account_id`; without one, API host + the credential reference |

Addresses and hosts compare case-insensitively. Two spellings of one
account (a user ID and a user principal name, a number and the
Messaging Service holding it) are not recognized as one.

## Web widget (`native.webwidget`)

Configuration: `{"sites": ["support", ...]}` — the inbox destinations
visitors may be on.

Credential: purpose `native.webwidget.session`, a `secret` of at least
32 bytes: the key session tokens are signed with. Rotating it ends
every session.

Routes, under the channel's webhook route:

| Request | Does |
| --- | --- |
| `POST .../session?site=<site>` | issues a visitor session token (`site` may be omitted when the channel lists one) |
| `POST .../messages` | a visitor message; `Authorization: Bearer <token>` |
| `POST .../actions`, `POST .../submissions` | a visitor's button press or command, and a submitted form: recorded as an interaction ([provider-verbs.md](provider-verbs.md#interactions)) |
| `POST .../uploads`, `POST .../interactions` | file uploads, and form option and field checks |
| `GET .../events?token=<token>` | the visitor's event stream (Server-Sent Events) |

Every visitor is anonymous: heo has no login of the embedding site to
verify a user against. heo sends no CORS headers: serve these routes
under your site's own origin through the reverse proxy. This release
has no attachment store and no interaction validator: file uploads and
form option and field checks are refused. Presses, commands and submitted
forms are recorded: `heo interaction list` shows them and `heo interaction
respond` answers them on the visitor's event stream.

### Delivered events

Each event the daemon delivers to a visitor (replies, streams, typing
signals) is kept in the profile's data store, numbered per visitor. The
event's number is its SSE `id`, so a browser that reconnects — after a
network drop or a daemon restart — sends it back as `Last-Event-ID` and
receives exactly the events after it, including those delivered while
it was away.

heo keeps the latest events per visitor and trims older ones:

```yaml
adapters:
  widget:
    retain: 256   # events kept per visitor; at least 1
```

A browser resuming from a cursor older than the oldest kept event first
receives a `reset` event and should reload its history. Expired typing
signals and ephemeral replies are never replayed. `heo config validate`
refuses a `retain` below 1; the daemon refuses to start with one.

A store last migrated by a heo that kept these events in memory needs
a newer schema: the daemon refuses to start on it until you run
`heo storage migrate` (it backs the store up first; see
[upgrade](upgrade.md#migrate-each-data-store)). Events delivered before
then are not carried over: a browser reconnecting with an older cursor
receives a `reset`.

## Inbound mail (`smtp.mail`)

Configuration: `hostname` (greeting and EHLO name), `recipients` (each
accepted address mapped to an inbox `destination`; others are refused
at RCPT), `from`, and the submission `relay` (`host` and one of
`tls_port`, `starttls_port`, `plain_port`).

Credential: purpose `smtp.relay`, a `password` for the relay.

Ingress: heo listens for mail itself, on

```yaml
adapters:
  smtp:
    addr: 0.0.0.0:25       # empty: smtp.mail channels send only
    allow_remote: true     # required for a non-loopback address
    tls:
      cert_file: /etc/heo/mx.example.com.pem  # PEM chain, leaf first
      key_ref: mx-tls                         # private key, in the credential backend
```

Point the domain's MX record at that host. Binding port 25 needs the
privilege to do so (or a relay forwarding to a high port on loopback).
A profile has one MX listener, so one receiving `smtp.mail` channel:
list every address it accepts in its `recipients`.

All four mail adapters (`smtp.mail`, `imap.mail`, `google.mail`,
`microsoft.mail`) declare `reply_recipients`, so `message reply` takes
`--reply-all`, `--cc` and `--bcc` on them ([conversations.md](conversations.md#reply-to-all-cc-and-bcc)):
the reply goes to the reply route's address and to the recipients named,
at most 50 in cc and bcc together. The SMTP relay delivers to every
recipient in one submission and keeps bcc out of the headers. Reply-all
adds the To and Cc of the message answered, minus the mailbox's own
address.

### STARTTLS

With `tls.cert_file` and `tls.key_ref` set, the listener offers
STARTTLS (TLS 1.2 or later). The certificate chain is a file; the
private key never is: store it in the profile's credential backend
under `key_ref`, purpose `smtp.tls`, as a `secret` envelope whose value
is the PEM key (PKCS#8, PKCS#1 or SEC 1; RSA, ECDSA or Ed25519):

```json
{"v":1,"kind":"secret","value":"-----BEGIN PRIVATE KEY-----\n...\n-----END PRIVATE KEY-----\n"}
```

Set both or neither. With neither, STARTTLS is not offered and senders
deliver in plain text.

The listener does not bind until the pair loads. After that it
re-reads the file and resolves the key at most once a minute, on the
next handshake, so a renewed certificate applies without a restart:
replace the file and the stored key together. A reload that fails
keeps serving the previous pair.

`heo config validate` reads the file and resolves the key: the chain
must parse, the key must match it, and the leaf must be within its
validity period. It warns when the leaf expires within 14 days. The
listener itself still serves an expired certificate, because senders
using opportunistic TLS fall back to plain text when a handshake fails.

A non-loopback listener without STARTTLS is refused. To accept mail
from other hosts in plain text anyway, set
`adapters.smtp.allow_remote_without_tls: true`. Mail then crosses the
network unencrypted, readable and alterable on the path, and senders
that require TLS (MTA-STS, DANE) cannot deliver; validate warns while
it is set.

## IMAP mailbox (`imap.mail`)

Configuration: `address`, `imap` and `submission` servers (`host` and
a port each), optional `mailbox` (default `INBOX`) and `destination`.

Credentials: purposes `imap` and `smtp.submission`, each a `password`
(or `oauth2` where the server takes XOAUTH2). One shared entry serves
both when one login is valid on both servers; otherwise store an entry
for each.

Ingress: none to publish; the receiver connects out and uses IDLE. The
channel reads `ready` while an authenticated session stands, and `waiting`
while the receiver reconnects.

History: `heo history fetch` reads received mail from the server, oldest
first, by arrival time (`INTERNALDATE`); see [Mail
history](provider-verbs.md#mail-history). Optional configuration
`history_mailboxes`, a list of mailbox names as the server lists them,
names the mailboxes it walks, each to its end, in that order; empty walks
`mailbox`. Sent, drafts, junk and trash are refused, by name when the
channel is saved and by the server's special-use flags when a mailbox is
read. A message copied between listed mailboxes is stored once.

## Gmail (`google.mail`)

Configuration: `user` (the mailbox address; `me` or empty for the
token's user), optional `addresses`, and for push: `topic`
(`projects/<project>/topics/<topic>`), `push_audience`,
`push_service_account`.

Credential: purpose `gmail`, an `oauth2` access token with the Gmail
scopes (`gmail.readonly`, `gmail.send`, `gmail.modify`). heo does not refresh
a token you store; for a consumer account store a grant instead with
`heo channel authorize` ([channels.md](channels.md#authorize-a-mailbox)), and
heo mints and refreshes the token itself. For a Workspace domain, a service
account with domain-wide delegation is a `jwt_bearer` grant with the mailbox
as `subject`. What each setup grants and how to revoke it:
[channels.md](channels.md#mailbox-access-setups-what-each-grants-how-to-revoke).

Ingress: without `topic` the receiver polls history and nothing is
mounted. The topic needs `publish` granted to
`gmail-api-push@system.gserviceaccount.com`, and the adapter renews the
mailbox's watch daily ([channels.md](channels.md#gmail-push-the-pubsub-topic)).
With push, create a Pub/Sub push subscription on the topic
with authentication enabled, endpoint
`<public_base_url>/webhooks/<channel id>`, audience `push_audience`,
service account `push_service_account`.

Verb: `conversation tag --add|--remove` changes Gmail labels, by label ID, on
the message a conversation replies to ([conversations.md](conversations.md#tag-a-mail-message-at-the-provider)).
It needs the Gmail modify scope on the token. `provider-label list` reads the account's labels
(`users.labels.list`) and `provider-label create` makes one, and a label name given to `conversation tag`
or `conversation move` is resolved through that list ([conversations.md](conversations.md#list-and-create-provider-mailboxes)).
`message reply-draft` keeps a reply in Drafts (`gmail.modify` covers
`users.drafts.*`; with `--reply-all`, `--cc` and `--bcc` on create and on
update), and `conversation unsubscribe` reads the message
(`gmail.readonly` or `gmail.modify`) and, for a mailto, sends (`gmail.send`):
see [conversations.md](conversations.md#unsubscribe-from-a-sender) for what
it will and will not do.

History: `heo history fetch` reads the inbox's received mail through the
Gmail API, oldest first, by Gmail's own received time; see [Mail
history](provider-verbs.md#mail-history). It needs a read scope beyond
`gmail.metadata`, which cannot search. Optional configuration
`history_mailboxes`, a list of label IDs (not names), walks those labels
instead of `INBOX`; spam, trash, drafts and sent are refused. Attachment
bytes are fetched with the message.

Push: a failed `users.watch` does not stop the receiver, since history
polling covers the mailbox. The channel reads `degraded` with the code
`google.mail.push_denied`, `push_rejected` or `push_unavailable` until a
watch succeeds ([how a receiver reads](runtime.md#how-a-receiver-reads)).

## Microsoft 365 mail (`microsoft.mail`)

Configuration: `mailbox` (user ID or principal name; empty for the
signed-in user), optional `addresses`, `cloud`, and for change
notifications: `notification_url`.

Credentials: purpose `graph`, an `oauth2` access token (not refreshed
by heo; store a grant instead: `refresh_token` from `heo channel authorize`
for a signed-in user, `client_assertion` for an app-only certificate, which
needs `mailbox` set and, to reach named mailboxes only, an Exchange RBAC
scope: [channels.md](channels.md#microsoft-365-app-only-with-a-certificate)); purpose `graph.notifications`, a `secret` the notification
client state is derived from. They are different kinds: with change
notifications, store each in its own entry (or the token as the shared
entry and the secret as the `graph.notifications` entry).

Ingress: without `notification_url` the receiver polls with delta
queries: polling only, by your choice, and the channel needs no
`graph.notifications` secret. With `notification_url` set to
`<public_base_url>/webhooks/<channel id>`, Graph validates the endpoint
on subscription, and the `graph.notifications` secret is required. Graph
keeps polling either way, so a channel with a `notification_url` and no
secret still receives mail, but creates no subscription. heo does not
let that pass as ready: `config validate` and `channel check` report an
error finding `credential:graph.notifications` (`missing`), and the
channel's receiver reads `degraded`, with the reason, in `service status`
(the receivers service reads `degraded`) until the secret is stored. A
subscription Graph refuses reads `degraded` too, with the code
`microsoft.mail.subscription_rejected`, `client_state_missing` or
`subscription_unavailable` ([how a receiver reads](runtime.md#how-a-receiver-reads));
mail still arrives by polling. Store
it as a `secret`, or remove `notification_url` to poll only.

History: `heo history fetch` reads the inbox's received mail from Graph,
oldest first, by `receivedDateTime`; see [Mail
history](provider-verbs.md#mail-history). Optional configuration
`history_mailboxes`, a list of folder IDs or well-known names, walks those
folders instead of `inbox`, each to its end in the listed order (oldest
first within a folder, not across them); deleted items, junk, drafts and
sent are refused, and a message copied into several listed folders is
imported once. File attachments are downloaded with the message, one
request each, at most 10 a message; a throttled download stops the call
with the cursor of that page.

Verb: `conversation tag --set` replaces the Outlook categories of the message a
conversation replies to ([conversations.md](conversations.md#tag-a-mail-message-at-the-provider)).
It needs `Mail.ReadWrite` on the mailbox; the names are the mailbox's master
category names. `message reply` with `--reply-all` replies to all through
Graph's reply-all, and `--cc` and `--bcc` become the message's cc and bcc
recipients. `message reply-draft` keeps a reply in Drafts (the recipients on
create only: an update leaves a person's changes to them) and
`conversation unsubscribe` reads the message's MIME form and, for a mailto,
drafts and sends ([conversations.md](conversations.md#unsubscribe-from-a-sender)).

## Twilio SMS and WhatsApp (`twilio.sms`, `twilio.whatsapp`)

Configuration: `account_sid`, `from` (E.164) or
`messaging_service_sid`, and `webhook_url`.

Credentials: purposes `twilio.rest` and `twilio.webhook`; store the
account's auth token as a `secret` in the shared entry (it both
authenticates API calls and verifies webhook signatures).

Ingress: set `webhook_url` to exactly
`<public_base_url>/webhooks/<channel id>`, and enter the same URL as the
number's (or Messaging Service's, or WhatsApp sender's) incoming
message webhook. Twilio signs the URL it calls: any difference fails
every request with 403. Without `webhook_url` the channel sends only.
This release has no attachment store: an incoming message carrying
media is answered 503, which Twilio retries and then gives up on.

## WhatsApp Cloud API (`meta.whatsapp`)

Configuration: `phone_number_id`, optional `api_version`.

Credentials: purposes `meta.graph` (the access token),
`meta.app_secret` (signs webhooks) and `meta.webhook.verify_token`
(the subscription handshake): three different values, each in its own
entry (see the example under
[Credential purposes](#credential-purposes)). The verify token is any
string you choose and enter in the app's webhook settings.

Ingress: the app's webhook callback URL is
`<public_base_url>/webhooks/<channel id>`, subscribed to the
`messages` field.

## Slack (`slack.chat`)

One channel per workspace install of one Slack app. Slack sends the app's
messages, edits, deletions and reactions (the Events API), button presses
and modal submissions (interactivity), and slash commands one of two ways,
a switch in the app's settings: as signed requests to the app's one
**request URL**, or, with **Socket Mode** on, over WebSocket connections
heo opens (experimental). heo answers through the Web API either way.

Configuration (JSON, unknown fields refused):

| Field | Required | Meaning |
| --- | --- | --- |
| `team_id` | yes | the workspace ID (`T...`); requests naming another workspace are acknowledged and dropped. Bind the inbox to it as its destination |
| `bot_user_id` | yes | the app's bot user (`U...`); what the app says is never reported back |
| `bot_id` | no | the app's bot (`B...`), for legacy bot messages |
| `app_id` | no | accept requests of this app only (`A...`) |
| `api_url` | no | the Web API root; default `https://slack.com/api/`, https only |
| `mentions_only` | no | report a channel message only when it mentions the bot; direct messages always |
| `history_channels` | no | up to 10 conversation IDs a channel-wide `history fetch` reads |
| `webhook_tolerance_seconds` | no | replay window of signed requests, 1 to 300; default 300 |
| `receive` | no | how the app takes its payloads, `events_api` (the request URL) or `socket_mode`: heo runs only that way. Empty runs both; see **Socket Mode** below |

```sh
heo channel create --type slack.chat --key chat --credential-ref slack \
  --configuration '{"team_id":"T0123ABCD","bot_user_id":"U0123ABCD","receive":"events_api"}'
```

Credentials: purposes `slack.chat.api` (the bot token), `slack.chat.signing`
(the signing secret of the app's Basic Information page) for the request
URL, and `slack.chat.socket` (an app-level token, `xapp-...`, with the
`connections:write` scope, from the same page) for Socket Mode; see
[Credential purposes](#credential-purposes). `heo channel check <channel>
--remote` diagnoses the ones the channel's `receive` needs. Without the
signing secret the route answers 503 and Slack retries; a request that is
not signed with it, or is older than the replay window, is refused 401, and
a `url_verification` request that fails the check gets no challenge back.

Ingress: in the app's settings, give one URL,
`<public_base_url>/webhooks/<channel id>`, for Event Subscriptions,
Interactivity and each slash command. Slack's `url_verification` is
answered by the adapter. Subscribe the bot to `message.channels`,
`message.groups`, `message.im`, `message.mpim`, `app_mention`,
`reaction_added` and `reaction_removed`, and give it the scopes
`app_mentions:read`, `channels:history`, `chat:write`, `groups:history`,
`im:history`, `im:write`, `mpim:history`, `reactions:read` and
`reactions:write`.

**Socket Mode.** Experimental, and nothing to publish: turn Socket Mode on in
the app's settings, store the app-level token, and set `receive` to
`socket_mode`. The `receivers` service opens the connection and takes the
same payloads over it; the signing secret is not used. The channel reads
`starting` until Slack's first `hello`, `ready` while a connection stands, and
`waiting` while the socket is dialled again. Slack sends an app's
payloads one way at a time ("When you toggle Socket Mode on, you'll only
receive events and interactive payloads over your WebSocket connections—not
over HTTP", [Using Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)),
and heo never toggles it: say which in `receive`. `events_api` mounts the
route and starts no receiver, `socket_mode` starts the receiver and mounts no
route (the route answers 404). The way switched off is configuration, not a
fault, and needs no credential.

With `receive` empty heo runs both, and the one Slack does not use is idle:
the receiver needs the app-level token too, and without it its channel scope
fails (`not_found: credential not found`) and the `receivers` service reads
`degraded`. Set `receive` to the way the app uses.

Inbox: one inbox per workspace, bound to the workspace's `team_id`, in any
selection mode (see [Choose a selection mode](channels.md#choose-a-selection-mode)):

| Mode | One conversation per | Replies and sends go to |
| --- | --- | --- |
| `per_room` | channel, private channel or direct message | the room |
| `per_thread` | thread: a message outside a thread roots one | the thread |
| `per_contact` | Slack user | the user's direct message |

```sh
heo inbox create --channel chat --key rooms --destination T0123ABCD --mode per_room
heo message send --inbox rooms --to slack.chat/room:C0123ABCD --text "Release is out."
```

The room is named with its namespace, `slack.chat/room:<id>`: the bare
`room:<id>` resolves to the adapter's identity namespace, `slack.chat.user`,
which holds users. A Slack thread lives inside a channel, and `--to` cannot
name both: `message send` cannot start a thread in a `per_thread` inbox.
Answer a thread Slack reported with `message reply` on its conversation,
which posts into the thread.

Outbound: text is sent literally, so a message cannot mention anyone or
format through text, and carries no attachment. A message is edited and
deleted only if the app's bot posted it: Slack refuses another author's
(`unauthorized`), and a refused edit never becomes a new message.
Reactions use a table of 26 common emoji; another emoji is refused as
unsupported before any call. An interaction is answered at its response
URL, in each mode, a follow-up too (Slack allows five in thirty minutes),
without the bot token.

**Modals.** A button press or a slash command can be answered with a modal:
`interaction open-modal` shows a view, `push-modal` stacks another on it
(Slack allows three), `update-modal` changes one that is open. Slack opens a
modal only with the `trigger_id` it sent with the interaction, which works
once and for three seconds, so these verbs are for a script or an agent that
reacts to `interaction.received`, not for typing. heo reads the trigger from
the interaction and refuses a late attempt itself; the view is Slack's modal
JSON from a file or standard input. The opened view's ID comes back on the
settled operation, for an update. The app needs Interactivity switched on
with the request URL above (or Socket Mode), as for any button. Details:
[Slack modals](provider-verbs.md#slack-modals).

`history fetch` reads a room, a direct message or a thread, or the
`history_channels`. Slack limits history reads to one request per minute and
15 messages per page for apps outside its Marketplace installed after 29
May 2025: such an app reads slowly.

Not run against a real workspace yet: the verbs are exercised in process
against a scripted Web API, and live recording is pending a workspace (see
[provider-matrix.md](provider-matrix.md)).

## Telegram (`telegram.messages`)

One channel per bot. A bot takes its updates one of two ways, and Telegram
allows only one at a time: it calls the bot's **webhook**, or heo reads
them by **long polling** (`getUpdates`). heo answers through the Bot API.
It reports chats (private chats, groups, supergroups, channels), the forum
topics of a supergroup, messages and their edits, reactions, the press of
an inline keyboard button and bot commands.

Configuration (JSON, unknown fields refused):

| Field | Required | Meaning |
| --- | --- | --- |
| `bot_id` | yes | the bot's user ID, the number before the colon of its token. What the bot says is never reported back. Bind the inbox to it, as its destination |
| `bot_username` | yes | the bot's username without `@`, 5 to 32 letters, digits or `_`, ending in `bot`: what a mention and `/command@bot` name |
| `api_url` | no | the Bot API root; default `https://api.telegram.org/`, https only |
| `mentions_only` | no | report a message of a group, supergroup or channel only when it addresses the bot (a mention, a reply to it, a command for it); private chats always |
| `poll_timeout_seconds` | no | the long-polling timeout, 1 to 50; default 30 |
| `receive` | no | how the bot takes its updates, `poll` (long polling) or `webhook`: heo runs only that way. Empty runs both; see **Long polling** below |

```sh
heo channel create --type telegram.messages --key bot --credential-ref telegram \
  --configuration '{"bot_id":123456789,"bot_username":"acme_support_bot","receive":"poll"}'
```

Credentials: purposes `telegram.messages.api` (the bot token BotFather
issued, `123456789:AA...`) and `telegram.messages.webhook` (the webhook's
secret token, 16 to 256 characters of `A-Z`, `a-z`, `0-9`, `_` and `-`, which
you choose); see [Credential purposes](#credential-purposes). `heo channel
check <channel> --remote` diagnoses the webhook secret unless `receive` is
`poll`. Telegram takes
the bot token in the request path, so an HTTP proxy or log that records URLs
records it: heo does not log it.

**Webhook.** Set it once, outside heo, with the bot token and your secret
token ([`setWebhook`](https://core.telegram.org/bots/api#setwebhook)):

```sh
curl -sS "https://api.telegram.org/bot$TOKEN/setWebhook" \
  -d url="<public_base_url>/webhooks/<channel id>" \
  -d secret_token="$SECRET" \
  -d 'allowed_updates=["message","edited_message","channel_post","edited_channel_post","message_reaction","callback_query"]'
```

Telegram sends `message_reaction` only when it is named there and the bot is
an administrator of the chat. A request without the secret token, or with
another, is refused 401 and reaches nothing; with no webhook secret stored
the route answers 503 and Telegram retries. A secret shorter than 16 bytes is
refused whatever the request carries. Telegram calls only ports 443, 80, 88
and 8443: put [the webhook listener](runtime.md) behind a reverse proxy on
one of them.

**Long polling.** Nothing to publish and nothing to set: the `receivers`
service reads the bot's updates, 100 at a time, and resumes after a restart
from the last update it recorded; Telegram keeps an update it was not told it
handled for 24 hours. It needs no webhook secret. One daemon reads one bot.

A bot with a webhook set refuses `getUpdates` (409), and heo never sets or
deletes a webhook: that is a decision about the bot. Say which way the bot
uses in the channel's `receive`, and heo runs only that one: `poll` starts
the receiver and mounts no route (the route answers 404), `webhook` mounts
the route and starts no receiver. The way switched off is configuration, not
a fault: it has no scope in `heo service status` and degrades nothing.
Changing `receive` with `channel update` applies at once (see
[Channel changes while running](runtime.md#channel-changes-while-running)).

With `receive` empty heo runs both, and the one Telegram does not serve is
idle. The receiver of a bot with a webhook set then shows its channel
`waiting`, with Telegram's reason (`provider conflict: Telegram holds a
webhook for this bot`), and asks again after 1, 2, 4 seconds and so on up to
5 minutes, so deleting the webhook (`deleteWebhook`) switches the bot to
polling on its own; until then the receivers service reads `degraded`. A
receiver that polls reads `ready` once Telegram has answered a poll, an
empty one included, and `waiting` after a failed read. A
refused token (revoked, or one that names no bot) fails the
receiver's scope for good, naming the bot token; the other channels run.

Inbox: one inbox per bot, bound to the bot's `bot_id`, in any selection mode
(see [Choose a selection mode](channels.md#choose-a-selection-mode)):

| Mode | One conversation per | Replies and sends go to |
| --- | --- | --- |
| `per_room` | chat | the chat |
| `per_thread` | forum topic of a chat (a message outside a topic cannot be placed: the webhook answers 422) | the topic |
| `per_contact` | user, whatever chat they wrote in | the private chat with the user |

```sh
heo inbox create --channel bot --key rooms --destination 123456789 --mode per_room
heo message send --inbox rooms --to telegram.messages/room:-1001234567890 --text "Release is out."
```

The chat is named with its namespace, `telegram.messages/room:<chat ID>`: the
bare `room:<id>` is read in the adapter's identity namespace,
`telegram.messages.user`, which holds users, so the preview accepts it and
the adapter refuses the route when the send runs (the operation fails
`validation`, nothing reaches Telegram). A group's chat ID is negative
(`-100...` once it is a supergroup). A forum topic lives inside a chat, and
`--to` cannot name both: answer a topic Telegram reported with `message reply`
on its conversation, which posts into the topic. A `per_contact` inbox names a
person, `--to user:<user ID>`; Telegram lets a bot write first only to a user
who started it, and refuses any other (`unauthorized`, with Telegram's words:
"bot can't initiate conversation with a user"). A public channel's `@username`
is not a route.

Outbound: text is sent literally, with no formatting and no mention, and
carries no attachment. A message is edited and deleted only if the bot
posted it, and a bot deletes within 48 hours: Telegram refuses another
author's or an older one (`unauthorized`), and a refused edit never becomes a
new message. A bot holds one reaction per message, from the 73 emoji
Telegram lists: another emoji is refused as unsupported before any call, and
a new one replaces the old. A button press and a command are interactions
([provider-verbs.md](provider-verbs.md#interactions)): heo answers one as a
message, as the notification only the presser sees (a callback query, not a
command) or as an edit of the message the button sits on, within 30 seconds
of reading it, and one answer each: a further reply is an ordinary message.
`history fetch` is refused: the Bot API cannot read past messages.

Not run against a real bot yet: the verbs are exercised in process against a
scripted Bot API, and live recording is pending a bot (see
[provider-matrix.md](provider-matrix.md)).

## Relay gateway (`relay.messages`)

For an SMS, WhatsApp or iMessage provider that has no adapter of its own
(for SMS and WhatsApp, prefer [Twilio](#twilio-sms-and-whatsapp-twiliosms-twiliowhatsapp)
or [Meta](#whatsapp-cloud-api-metawhatsapp) where they fit). heo reaches the
provider through a **gateway you run**, a small service in front of it:

```
provider ──► gateway ──signed POST──► heo   /webhooks/<channel id>
provider ◄── gateway ◄──POST /uma/reply── heo   (callback; optional lookup)
```

The gateway posts each incoming message to the channel's webhook route,
authenticated the way the channel says; heo posts each reply to a callback
URL the gateway serves, and may ask a second URL, the lookup, what became of
a reply whose answer was lost. heo never talks to the provider, and states no
support for any particular provider: the gateway is yours. One channel is one
gateway and one medium (`imessage`, `sms` or `whatsapp`); the medium picks the
identity namespace, `relay.imessage`, `relay.sms` or `relay.whatsapp`, so one
number on two mediums is two identities. uma's own reference gateway
(`cmd/imessagegateway` in the uma repository, in front of Blooio or
BlueBubbles Server) is a starting point, not a supported provider list. This
page documents iMessage; `sms` and `whatsapp` channels run through the same
contract and are exercised in process only.

**Operator note on iMessage.** Apple offers no API for iMessage outside Apple
Messages for Business. A hosted relay or BlueBubbles sends from a real Apple
ID under Apple's iCloud terms, which restrict automated and bulk or unsolicited
messages, and Apple may suspend an account that breaks them. That exposure is
yours: choose the provider, the Apple ID and the traffic accordingly.

Configuration (JSON, unknown fields refused, so a secret pasted into it is an
error):

| Field | Required | Meaning |
| --- | --- | --- |
| `medium` | yes | `imessage`, `sms` or `whatsapp` |
| `auth` | yes | how the gateway proves each webhook is its own: `scheme` (`bearer`, `token`, `hmac`, `ed25519`) and its `header`, `hash`, `encoding`, `prefix`, `payload`, `timestamp` (`header`, `format`, `tolerance`) and `replay` (`source`, `header`, `ttl`) as uma's webhook authentication documents them. There is no unauthenticated mode |
| `inbound.fields` | no | the JSON keys of what the gateway posts, per role (`from`, `to`, `body`, `message_id`, `media`), each a list tried in order; the defaults are those names |
| `inbound.generate_missing_id`, `inbound.max_body_bytes` | no | accept a message with no ID under a random one (a retry then becomes a second message); body limit up to 1 MiB, default 64 KiB |
| `callback.url` | to send | where each reply is posted: `https`, a host the host setting below lists, no credentials, query or fragment. Without it the channel only receives |
| `callback.auth`, `callback.header` | no | the callback credential as `Authorization: Bearer` (default) or as the whole value of one header |
| `callback.message_id` | no | the keys of the gateway's answer that carry its ID for the message; default `message_id` |
| `callback.lookup_url` | no | the lookup, under the same rules as `callback.url`; without it `reconcile` is disabled |

```sh
cfg='{
  "medium": "imessage",
  "auth": {"scheme": "hmac", "header": "X-Webhook-Signature",
           "timestamp": {"header": "X-Webhook-Timestamp"}, "replay": {"source": "signature"}},
  "callback": {"url": "https://gateway.example.com/uma/reply",
               "lookup_url": "https://gateway.example.com/uma/lookup"}}'
heo channel create --type relay.messages --key imessage --credential-ref gateway --configuration "$cfg"
```

That `auth` is what uma's reference gateway signs with: an HMAC-SHA-256 over
the timestamp, a dot and the body, with the timestamp in a header.

**Where a callback may go is the host's decision, never a channel's.** The
callback carries a credential, so a channel cannot choose the host it is sent
to: the hosts a callback and a lookup may name are heo settings, empty by
default, which refuses every channel that names a callback (a channel that
only receives needs none):

| Key | Default | Meaning |
| --- | --- | --- |
| `adapters.relay.callback_hosts` | empty | the hosts, as they appear in the URL (`gateway.example.com`, or `host:port` when the URL names a port), that a channel's `callback.url` and `callback.lookup_url` may name; a YAML list in the profile file, or a comma list from `config set`, `-c` or the environment. No scheme, path or wildcard |
| `adapters.relay.allow_http` | `false` | accept `http` callbacks on the listed hosts, for a gateway on this machine or a private network; the credential then travels in clear text, which `config validate` warns of for a host off this machine |
| `adapters.relay.timeout` | `15s` | the bound on one callback or lookup request, up to `10m`; a gateway that answers once its backend has sent the message (BlueBubbles) needs more |

A channel naming another host is refused when it is created, exit 2, without
naming the URL back; the redirect of an answer is never followed, so the
credential goes to the listed host only. A malformed setting stops heo from
composing its adapters, naming the key.

Credentials: purposes `relay.webhook` (the secret, or for `ed25519` the public
key, that authenticates what the gateway posts; a `secret`, or a `password`
whose secret is the key) and `relay.callback` (what heo presents to the
callback and the lookup: a `secret` of printable characters without spaces); see
[Credential purposes](#credential-purposes). The callback credential is not
needed, and not checked by `heo channel check --remote`, on a channel with no
callback.

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$WEBHOOK_SECRET"  > "$dir/uma/default/gateway@relay.webhook"
            printf '{"v":1,"kind":"secret","value":"%s"}' "$CALLBACK_SECRET" > "$dir/uma/default/gateway@relay.callback")
```

**Receiving.** The gateway POSTs a JSON object to
`<public_base_url>/webhooks/<channel id>`:

```json
{"from": "alice@example.com", "to": "+15550100002", "body": "hello", "message_id": "m-1"}
```

A message needs `from`, `to`, `message_id` and `body` or `media`. Authentication
comes first, before a byte of the body is parsed: a missing or wrong signature
or a stale timestamp is 401, a malformed header 400, a signed delivery already
accepted 409 (with `replay` on), an authenticated body that is not a message
400, no stored webhook secret 503 (the gateway retries once it is), and a
message heo took, new or duplicate, 200 `{"status":"accepted"}`. A message is
filed once however often it is delivered: its event is keyed on the sender and
the gateway's message ID. Bind the inbox to the address the gateway reports as
`to` (the line's number, or the Apple ID the Mac sends from), in a
`per_contact` inbox locked to one conversation. An iMessage address is a phone
number, kept as sent, or an Apple ID's email address, lower-cased. Replay
rejection keeps its record in memory: a restart of heo forgets it, so a request
captured inside the timestamp window can be accepted again after one.

**Sending.** A reply, and a first message, go to the callback as
`{"medium", "to", "body", "in_reply_to", "operation_id", "attempt_id"}` with the
callback credential and the operation as the `Idempotency-Key`, the same across
the attempts of one reply; `in_reply_to` is the gateway's own ID of the message
answered. The gateway's answer is the outcome: 2xx accepted (heo keeps the ID
it names), 401 or 403 unauthorized, 429 rate limited, a refusal or a redirect
final, and a connection lost, a timeout or a 5xx `outcome_unknown`. With a
`lookup_url`, heo asks the gateway, with the operation as `operation_id`,
before it ever sends again: `sent` settles the reply, `not_found` (once two
minutes, or twice the timeout, have passed) sends it again under the same key.
Without one `reconcile` is reported `disabled` and a reply whose answer was lost
stays `outcome_unknown`, never sent again, until you settle it from the
gateway's own log. The gateway must keep each key it has acted on, and answer
`not_found` only for a key it has no record of; uma's reference gateway does.
Text only is sent; a non-public audience is refused as unsupported, as are
edits, deletions, reactions, receipts and history, which the relay contract
does not have.

**Recipient.** The channel declares three identity namespaces, so a recipient
names one: `heo message send --inbox imessage --to relay.imessage/phone:+15550100003
--text "..."`. An Apple ID's email address is written in the same `phone` kind
(`relay.imessage/phone:alice@example.com`), in any letter case: a start
lower-cases the address, so `Bob@Example.com` and the `bob@example.com` he later
writes from are one identity and one conversation, and the gateway is sent the
lower-cased address. The `--dry-run` preview names that lower-cased address, the
one the send uses, and shows the address as typed beside it (`as typed`) only
when the two differ. A phone
number, and every address on the SMS and WhatsApp mediums, is used as given.
Replaying a start under its `--idempotency-key` with the address in another case
is the same operation. An identity made by a start from before the upgrade, with
a mixed-case address, stays a separate identity: merge its contact into the
other with [`contact merge`](contacts.md#merge-two-contacts), and replay of such
an old start under its original key is `CONFLICT` (exit 4), so start again under
a new key. The bare
`phone:+1555...` is refused (VALIDATION, exit 2) as ambiguous among the three
mediums; another medium's namespace passes the preview, which knows only the
adapter's namespaces, and the adapter refuses it when the send runs (the
operation fails `validation`, nothing reaches the gateway). Only an adapter that
declares `start_conversation` sends first: the gateway decides whether it takes
a first message to a handle.

One channel per gateway and medium: two channels of one medium on one callback
URL (host and path, case-insensitively) would be handed one stream, and heo
refuses to start with both. A channel that only receives names no gateway and
is keyed on its credential reference.

Not run against a real backend yet: the verbs run in process against a scripted
gateway, and live recording is pending an iMessage backend (see
[provider-matrix.md](provider-matrix.md) and [DEVELOPING.md](../DEVELOPING.md)).

## Discord (`discord.chat`)

One channel per Discord application: its bot, in every server it was added to,
and its direct messages. Messages, edits, deletions and reactions reach heo
only over Discord's **gateway**, a WebSocket the `receivers` service holds open
as the bot. Slash commands, buttons, selects and modal submissions
(interactions) arrive one of two ways, a switch in the application's settings:
over the same gateway, or, once you set an **Interactions Endpoint URL**, as
signed requests to the channel's webhook route. heo answers through the REST
API either way.

Configuration (JSON, unknown fields refused):

| Field | Required | Meaning |
| --- | --- | --- |
| `application_id` | yes | the application (a snowflake). Bind the inbox to it as its destination; an interaction of another application is refused |
| `bot_user_id` | yes | the bot user (a snowflake, from Get Current User with the token): what the bot says is never reported back. A gateway whose READY names another user ends the receiver: the token is another bot's |
| `api_url` | no | the REST root; default `https://discord.com/api/v10/`, https only |
| `gateway_url` | no | the gateway when Discord names none; default `wss://gateway.discord.gg`. Also bounds which hosts a gateway URL may name |
| `mentions_only` | no | default **true**: report a guild message only when it mentions the bot, and connect without the privileged `MESSAGE_CONTENT` intent; direct messages and interactions are always reported. See below |
| `history_channels` | no | up to 10 channel IDs a channel-wide `history fetch` reads |
| `webhook_tolerance_seconds` | no | replay window of signed requests, 1 to 300; default 300 |
| `receive` | no | `gateway` or `interactions`: heo runs only that way. Empty runs both; see **Choosing how to receive** |

```sh
heo channel create --type discord.chat --key bot --credential-ref discord \
  --configuration '{"application_id":"123456789012345678","bot_user_id":"123456789012345679","receive":"gateway"}'
```

Credentials: purposes `discord.chat.api` (the bot token) and
`discord.chat.interactions` (the application's public key, hex, from General
Information in the Developer Portal; it is not secret, and is a credential so
it rotates with the others); see [Credential purposes](#credential-purposes).
The public key is needed only for the Interactions Endpoint URL: under
`receive: gateway` it is not asked for and `heo channel check --remote` does
not diagnose it. Install the bot with the `bot` scope and, in the channels it
serves, View Channel, Send Messages, Send Messages in Threads, Read Message
History and Add Reactions (Manage Threads to remove thread members).

**Message content is privileged.** Without the `MESSAGE_CONTENT` intent Discord
sends a guild message's text empty, except for messages that mention the bot,
direct messages and the bot's own. So the default, `mentions_only`, reports
exactly what Discord fills: mentions, direct messages and interactions. Turn it
off only for an application granted the intent in the Developer Portal: heo
then asks for it and reports every guild message the bot can see. Asking for an
intent the application was not granted makes Discord close the gateway with
4014; heo does not retry that (see **The gateway**). A support desk that reads
free text in guild channels should plan on mentions, direct messages or slash
commands.

**Choosing how to receive.** An application takes its interactions "in two
mutually-exclusive ways": over the gateway by default, or at the Interactions
Endpoint URL once one is set. Messages only ever arrive over the gateway. The
channel's `receive` says which ways heo runs, and the way it leaves off is
configuration, not a fault: it has no scope, degrades nothing, needs no
credential and is listed `off` by `heo channel check --remote`.

| `receive` | gateway receiver | route (`/webhooks/<channel id>`) | Use when |
| --- | --- | --- | --- |
| `gateway` | runs | 404, not mounted | no Interactions Endpoint URL is set: everything over the gateway |
| `interactions` | not started, no connection | mounted | only slash commands, buttons and modals matter, and no process may hold a WebSocket |
| empty | runs | mounted | both: the gateway for messages, the endpoint for interactions once its URL is set. Needs the public key too |

heo never sets the Interactions Endpoint URL: that is the application owner's
setting. Set it to `<public_base_url>/webhooks/<channel id>`. Discord checks it
when you save, with a PING and a request whose signature is wrong, and removes
a URL that accepts one: the route verifies the Ed25519 signature over the
timestamp and the body, against the stored public key, before it reads
anything, and answers every failure 401 (a missing or malformed signature, a
stale timestamp, another key, a changed body). With no public key stored it
answers 503. A command, a button press or a modal submission is recorded once
however often it is delivered and answered `200` with a deferral inside
Discord's three seconds; the answer itself follows through
`interaction respond`.

**The gateway.** The `receivers` service holds one connection per application
(an application has one bot token, so one session, and heo refuses two
channels of one application, see below). It asks Discord for the gateway and
its Identify budget, identifies with the bot token and the intents, heartbeats,
and records messages, edits, deletions, reactions and interactions in order
per channel. What heo does when it ends:

| Discord does | heo |
| --- | --- |
| drops the connection, asks to reconnect, a missed heartbeat acknowledgement, a resumable close | resumes the session with its ID and last sequence number; Discord replays what was missed. The link reads `connecting` meanwhile and the channel `waiting`, `ready` again once the session resumes |
| invalidates the session | identifies again after a short random wait |
| close 4014: a privileged intent was not granted | the channel reads `failed` with the way out (`MESSAGE_CONTENT` on in the Developer Portal, or `mentions_only` back on); never retried |
| close 4004, a token refused by Discord, close 4010 to 4013 | `failed`, never retried: the credential or the application will not mend itself |
| the day's 1000 Identify calls spent | never identifies (past the budget Discord resets the bot token); the channel reads `waiting` with `rate_limited` and when Discord's reset lets the next connection open, and asks again then |
| connections failing in a row | the gateway tries three times (`connecting`), then ends the connection; uma's receiver retries at its own backoff, and after five in a row it ends the call and heo starts it again with its own, 1 second doubling to 5 minutes |

Stopping the daemon drops the connection without a close frame of 1000 or 1001,
which would end the session at Discord. The session and the last sequence
number every earlier dispatch of which was handled are kept with the channel,
so a restarted daemon **resumes the same session** instead of identifying again
and Discord replays what was dispatched meanwhile; a message replayed twice is
a duplicate and recorded once. One daemon holds a channel's gateway at a time:
the receiver lease is held under the daemon's owner name, and `heo channel
check <channel> --remote` shows the holder.

`heo service status` shows each channel's receiver scope, and `heo channel
check <channel> --remote` shows the same as the `receiving` report: the
receiver (way `receive`) with its state (`starting`, `ready`, `waiting`,
`failed`), the reason and the lease holder, and the route (way `receive_http`)
with its path. A receiver that stopped is an error finding, a waiting one a
warning, when an inbox with destinations needs it. The gateway reports its
link: the receiver reads `starting` until the session is up (READY or
RESUMED), `ready` while it is, and `waiting` while the gateway reconnects or
uma retries after a failure; `phase`, `link`, `failures` and `retry_at` say
which (see [the receive call](channels.md#checks)). What ends the
call for good is reported with Discord's reason.

Inbox: one inbox per application, bound to the application ID, in any selection
mode (see [Choose a selection mode](channels.md#choose-a-selection-mode)):

| Mode | One conversation per | Replies and sends go to |
| --- | --- | --- |
| `per_room` | channel (a guild text channel or a direct message); a thread's messages belong to the channel it hangs off | the channel |
| `per_thread` | thread channel or forum post (a message outside a thread is refused) | the thread |
| `per_contact` | user, whatever channel they wrote in | the user's direct message |

```sh
heo inbox create --channel bot --key rooms --destination 123456789012345678 --mode per_room
heo message send --inbox rooms --to discord.chat/room:123456789012345680 --text "Release is out."
```

The channel is named with its namespace, `discord.chat/room:<channel ID>`: the
bare `room:<id>` is read in the adapter's identity namespace,
`discord.chat.user`, which holds users, so the preview accepts it and the
adapter refuses the route when the send runs (the operation fails
`validation`, nothing reaches Discord). A thread lives under a channel, and
`--to thread:<id>` cannot name both: answer a thread Discord reported with
`message reply` on its conversation, which posts into the thread. A
`per_contact` inbox names a person, `--to user:<user ID>`: heo opens the direct
message channel (Create DM) and posts there. Discord asks that a bot write
first only to a user who asked for it, and refuses one it may not message
(`unauthorized`).

Outbound: text is sent as written (Discord's markdown applies) with every
mention suppressed, so no text pings anyone, up to 2000 characters (more is
refused `validation`, nothing is cut), and carries no attachment. A message is
edited and deleted only if the bot posted it (deleting another author's needs
Manage Messages in the guild): Discord refuses another author's edit
(`unauthorized`), and a refused edit never becomes a new message. Reactions
take any Unicode emoji; a custom emoji is refused. `participant add` and
`participant remove` add and remove a user of a **thread**; a channel or user
route is `unsupported`. Every message carries a nonce, so a retried send of one
operation never posts twice inside Discord's few minutes.

An interaction is answered through its own webhook, which takes no bot token
and lives 15 minutes, after the deferral heo sent when it recorded it. A
command's answer edits the loading message and is public (the deferral fixed
its visibility, so an ephemeral answer to a command is refused
`unsupported`); a button press or modal answers as a follow-up message,
ephemeral if you ask, or edits the message the control sits on. A follow-up to a
command waits for its first answer. See
[provider-verbs.md](provider-verbs.md#interactions).

`history fetch` reads a channel, a thread, a direct message or the
`history_channels`; the bot needs View Channel and Read Message History.

**One receiver per application.** An application has one bot token and one
Interactions Endpoint URL, so two channels of one application, whichever way
each receives, would take the gateway session from each other or be handed one
stream: heo refuses to create the second (`CONFLICT`, exit 4) and refuses to
start a daemon that holds two, and another daemon of the host holding the
application refuses the start. The server does not matter: one channel serves
the bot in every server it is in.

Rate limits are the adapter's: Discord's 50 requests a second and its
per-route buckets are kept, a wait over a second is refused before the request
(`rate_limited`) and a 429 is never retried inline.

Not run against Discord yet: the verbs are exercised in process against a
scripted REST API and gateway, and the gateway is **unverified against
Discord**: a WebSocket session is no HTTP exchange and cannot be recorded in
a cassette. Live recording of the REST verbs is pending a bot token
(`make record-discord`, see [provider-matrix.md](provider-matrix.md)). Sharding
is not built: a bot Discord requires to shard is refused.

## Microsoft Teams (`microsoft.teams`)

One channel per bot: a single-tenant Azure Bot registration (one Microsoft
App ID) in one Microsoft Entra tenant. Teams does not call heo as a Slack app
does and offers nothing to poll: the **Bot Connector** service posts every
activity of the bot to its one **messaging endpoint**, which is the channel's
webhook route, and heo answers through the Bot Connector REST API. It reports
messages in channels, group chats and personal chats, their edits and
deletions, reactions and the press of an Adaptive Card's `Action.Execute`.

Configuration (JSON, unknown fields refused, so a client secret put there by
mistake is caught):

| Field | Required | Meaning |
| --- | --- | --- |
| `app_id` | yes | the bot's Microsoft App ID, a GUID: the audience every inbound token must name. The bot's own Teams account is `28:<app_id>`; what it says is never reported back |
| `tenant_id` | yes | the Microsoft Entra tenant the bot is registered and installed in, a GUID. Activities of another tenant are acknowledged and dropped. Bind the inbox to it, as its destination |
| `service_url` | yes | the Bot Connector service heo sends to, `https` with no user info or query: the `serviceUrl` Teams puts on the bot's activities. The public cloud's is `https://smba.trafficmanager.net/teams/`; the other clouds have their own |

```sh
heo channel create --type microsoft.teams --key teams --credential-ref teams \
  --configuration '{"app_id":"<app ID>","tenant_id":"<tenant ID>","service_url":"https://smba.trafficmanager.net/teams/"}'
heo inbox create --channel teams --key rooms --destination "$TENANT_ID" --mode per_room
```

**Messaging endpoint.** In the Azure Bot's **Configuration**, set the
messaging endpoint to `<public_base_url>/webhooks/<channel id>` (a public
`https` URL; see [Before any provider](#before-any-provider)) and add the
Microsoft Teams channel. heo needs no secret to receive: the adapter checks the
Bot Connector token of every request before anything reaches heo, against the
Bot Framework's published signing keys, which it reads (and caches for a day)
through heo's HTTP client from `login.botframework.com`. A request with no
token, with one that fails any check (RS256 signature, issuer
`https://api.botframework.com`, audience `app_id`, expiry, the `serviceurl`
claim) is refused 401 and reaches nothing; one signed by a key the Bot
Framework does not endorse for Teams is refused 403; while the keys cannot be
read the route answers 503 and the Bot Connector retries. A message heo cannot
file (a group chat message in a `per_thread` inbox) is refused 422. Tokens
issued by Microsoft Entra, such as the Bot Framework Emulator's, are not
accepted.

**Credential.** One of two, never both: the bot's **client secret**
(purpose `microsoft.teams.client_secret`, a `secret` credential) or a
**certificate** registered for its app (purpose
`microsoft.teams.client_certificate`, a `client_cert` credential). heo mints
the Bot Connector access token itself: a client credentials grant to the
channel's tenant,
`POST https://login.microsoftonline.com/<tenant ID>/oauth2/v2.0/token` with the
channel's `app_id`, the credential and `scope=https://api.botframework.com/.default`
(see [Bot Connector authentication](https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-authentication)).
It keeps the token in memory, per channel, and mints the next one five minutes
before it expires; callers that need a token while one is being minted share
that request. The token is only ever sent to the channel's `service_url`;
sending needs it, receiving does not. Nothing is written to the credential
store, and neither the credential nor a token is logged or put in an error.

Client secret (Azure portal: the app's **Certificates & secrets**; use the
secret's *value*, shown once, not its ID):

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$APP_SECRET" \
  > "$dir/uma/default/teams@microsoft.teams.client_secret")
```

Certificate (upload `cert.pem` to the app's **Certificates & secrets**, under
**Certificates**; its thumbprint is what Entra finds the key by). The key must
be RSA, and the PEM files are the certificate (leaf first) and its private key:

```sh
openssl req -x509 -newkey rsa:2048 -sha256 -days 730 -nodes \
  -keyout key.pem -out cert.pem -subj "/CN=heo-teams-bot"
(umask 077; jq -n --rawfile c cert.pem --rawfile k key.pem \
  '{v:1,kind:"client_cert",certificate_pem:$c,private_key_pem:$k}' \
  > "$dir/uma/default/teams@microsoft.teams.client_certificate")
```

With a certificate, the request carries no secret but a **client assertion**
([RFC 7523](https://www.rfc-editor.org/rfc/rfc7523)): a JWT signed RS256 with
the key, whose `x5t#S256` header is the certificate's SHA-256 thumbprint, for
the tenant's token endpoint as audience, the app as issuer and subject, a
unique `jti` and a ten-minute window. A new one is signed for each mint.

- **Exactly one.** A channel whose reference holds both a client secret and a
  certificate, under either purpose or the shared entry, is refused: a send
  fails `conflict` naming both purposes before anything leaves, and `heo channel
  check <channel> --remote` reports it. With neither, the send fails `not_found`
  and the check names both purposes. Remove the one you no longer use.
- **Rotation.** The credential is read from the store at every mint: store the
  app's new secret, or a new certificate and key (registered with the app
  first), and the next mint uses it, with no restart. You can switch from one
  to the other the same way, by storing the one and removing the other. A token
  minted with the old credential stays valid until it expires. After a failed
  mint, one with a credential changed since is tried at once instead of waiting
  out the backoff.
- **Expiry.** `heo channel check <channel> --remote` reports a certificate
  within 30 days of its `not_after` as a warning (`expires_soon`, exit 0) and
  one past it as an error (`expired`, exit 2): register and store the next
  certificate before then. heo refuses to sign with an expired certificate.
- **Failure.** A mint that fails (Entra 5xx or 429, or unreachable) is retried
  with backoff, 2 seconds doubling to 5 minutes, or the wait Entra asks for in
  `Retry-After`; a send during it fails `transient` or `rate_limited` and is
  retried by the worker, and a cached token still valid is used meanwhile. A
  credential Entra refuses (`invalid_client`, such as AADSTS7000215 for a
  secret or AADSTS700027 for a certificate it does not know) fails the send
  `unauthorized` naming Entra's own error code, never the secret or the key,
  and is remembered as rejected: `heo channel check <channel> --remote` reports
  it as an error finding, and `credential` diagnoses it `rejected` until it is
  rotated. When the Bot Connector refuses a token (401), the next token is
  minted afresh (at most once every 30 seconds).
- **Where the credential may go.** `adapters.teams.authority`, a host setting and
  never a channel field, is the Entra endpoint, `https://login.microsoftonline.com`
  by default. It must be `https`, or `http` on a loopback host (a stand-in
  endpoint of your own). The sovereign clouds' endpoints are not supported yet:
  their Bot Connector scope differs.

Inbox: one inbox per bot, bound to the tenant (`--destination "$TENANT_ID"`),
in any selection mode (see [Choose a selection
mode](channels.md#choose-a-selection-mode)):

| Mode | One conversation per | Replies and sends go to |
| --- | --- | --- |
| `per_room` | Teams conversation: a channel, a group chat or a personal chat | the conversation; a send to a channel starts a new post |
| `per_thread` | channel thread: a root post starts one and its replies join it (a chat has no threads: the webhook answers 422) | the thread, as `<channel>;messageid=<root>` |
| `per_contact` | user, whatever conversation they wrote in | the personal chat with the user |

```sh
heo message send --inbox rooms --to 'microsoft.teams/room:19:abc123@thread.tacv2' --text "Release is out."
```

The conversation is named with its namespace, `microsoft.teams/room:<conversation
ID>` (a channel's is `19:...@thread.tacv2`, a group chat's `19:...@thread.v2`,
a personal chat's `a:...`): the bare `room:<id>` is read in the adapter's
identity namespace, `microsoft.teams.user`, which holds users, so the preview
accepts it and the adapter refuses the route when the send runs (the operation
fails `validation`, nothing reaches Teams). A channel thread lives inside a
channel, and `--to` cannot name both: `thread:<root>` names no channel and is
refused the same way when sent. Answer a thread Teams reported with `message
reply` on its conversation, which posts into the thread. A `per_contact` inbox
names a person, `--to user:<Teams user ID>` (`29:...`). A message is named
`<conversation ID>|<activity ID>` at Teams.

**Starting a conversation (proactive messages).** `message send` can write
first, within what Teams allows a bot, and the limits are Teams's:

- The Bot Connector service to call is the channel's `service_url`, fixed in its
  configuration: heo stores no per-conversation service URL or conversation
  reference beyond the conversation ID, so a tenant Teams moves to another
  service needs a `channel update`.
- To a **user**, the personal chat is created or returned by the Bot Connector,
  and Teams does that only when the bot's Teams app is **installed for the
  user** (or the user is in a team where it is installed). Otherwise Teams
  refuses it and the send ends `rejected` as `unauthorized`.
- To a **channel** or **group chat**, you must already know its conversation
  ID (from a message Teams reported, or from the Teams client), and the app
  must be installed there; the bot cannot post in a private channel. A channel
  conversation ID cannot be invented.

In channels and group chats Teams delivers the bot only messages that
**@mention** it, unless the Teams app's manifest asks for the
resource-specific permissions `ChannelMessage.Read.Group` or
`ChatMessage.Read.Chat`; a message's payload says whether the bot was
mentioned. Personal chats deliver every message.

Outbound: text is sent literally (plain, never markdown), with no mention, and
at most 80 KB; heo's commands send text only, so no card or attachment. A
message is edited and deleted only if the bot posted it: Teams refuses a
person's (`unauthorized`), and a refused edit never becomes a new message.
Reactions are Teams's six types, 👍 ❤️ 😆 😮 😢 😡: another emoji is refused as
unsupported before any call. A Connector answer ends the operation: a
conversation Teams does not know `rejected` as `not_found`, a bot outside the
roster `rejected` as `unauthorized`, throttling `rate_limited` after its
`Retry-After`, and a server error or a lost answer `outcome_unknown`, never
sent again (nothing reads a message back to settle it).

An `Action.Execute` on an Adaptive Card the bot sent is an interaction
([provider-verbs.md](provider-verbs.md#interactions)): the adapter records it,
then answers Teams's invoke with the empty acknowledgement within its five
seconds, and heo reads it with `interaction list`. Teams gives no response
credential and the adapter declares no `respond`, so `interaction respond` is
refused (`unsupported`): answer with `message reply` on the interaction's
conversation, or `message edit` on the card's message. heo cannot send a card
itself; a card another component of the same bot registration posted can be
pressed, and dialogs (`task/fetch`) are answered 501. `history fetch` is
refused: the Bot Connector cannot read past messages.

Not run against a real tenant yet: the verbs and the token checks are
exercised in process against a scripted Bot Connector and scripted signing
keys, and live recording is pending a tenant (see
[provider-matrix.md](provider-matrix.md)). Activities as Teams itself sends
them, a message in a channel thread's edit or reaction, and what the Teams
client shows for plain text are unverified (see uma's adapter page).

## GitHub issues (`github.issues`)

One channel per GitHub account: a GitHub App installation or a
personal access token. Each issue is a work item, bound to a
conversation holding its description and comments; `conversation list`
and `conversation get` read them, `conversation status` closes and reopens
them and `conversation update` replaces their assignees, labels and
milestone at GitHub (see [issues-and-tickets.md](issues-and-tickets.md)).

Configuration: optional `api_url` (GitHub Enterprise Server, e.g.
`https://ghe.example.com/api/v3/`; default github.com), `replies`
(post replies as issue comments; off by default), `account_id` (the
numeric GitHub user the credential acts as; required with `replies`),
and `local_fields`. `conversation status` needs none of `replies` and
`account_id`; it needs write access to issues on the API credential, and
a status `local_fields` hands to uma changes in uma, not at GitHub.
`conversation update` needs none of them either, only the same write access;
the fields `local_fields` hands to uma (`assignees`, or `fields` for labels
and milestone) change in uma, not at GitHub.
Credentials: purposes `github.issues.api` (an App installation token
as `oauth2`, or a personal access token as `secret`; it needs read
access to issues and metadata, and write access to send replies) and
`github.issues.webhook` (the webhook secret). Store them in separate
entries when they differ.

Inbox: bind repositories by full name (`owner/repo`) as destinations,
in mode `per_work_item`:

```sh
heo inbox create --channel gh --key issues --destination owner/repo --mode per_work_item
```

Ingress: subscribe a repository, organization or App webhook to the
`issues` and `issue_comment` events, content type `application/json`,
at `<public_base_url>/webhooks/<channel id>`, with the same secret.
Pull request comments and other events are acknowledged and dropped.
GitHub has no private comments: internal notes stay in uma.

## Zendesk tickets (`zendesk.tickets`)

One channel per Zendesk account. Each ticket is a work item, bound to a
conversation holding its comments; `conversation list` and `conversation get`
read them and `conversation update --field status=...` changes a ticket's
status at Zendesk (see [issues-and-tickets.md](issues-and-tickets.md)).

Configuration: `subdomain` (`https://<subdomain>.zendesk.com`), `auth`
(`api_token` or `oauth`), `email` (the agent an API token belongs to,
with `auth: api_token` and a `secret` credential),
`webhook_tolerance_seconds` (1 to 3600: how far a signed webhook's
timestamp may be from now), optional `poll_interval_seconds` (at least
6, default 30), `poll_lookback_seconds` and `local_fields`.

```sh
heo channel create --type zendesk.tickets --key zd --credential-ref zd \
  --configuration '{"subdomain":"acme","auth":"api_token","email":"agent@acme.example","webhook_tolerance_seconds":300}'
```

Credentials: purposes `zendesk.tickets.api` (the API token as a
`secret`, or a `password` of agent address and token; an OAuth access
token with `auth: oauth`) and `zendesk.tickets.webhook` (the webhook's
signing secret).

Inbox: bind brand IDs as destinations, in mode `per_work_item`.

Ingress: the receiver polls Zendesk's incremental ticket events, so
nothing a webhook missed is lost. A webhook subscribed to ticket events
at `<public_base_url>/webhooks/<channel id>` brings changes sooner.
Public comments are public messages; internal notes, and comments whose
visibility Zendesk does not state, are restricted: desk actors alone
read them.

Outbound: replies are public comments. An inbox with
`--deliver-notes` sends internal notes as private comments. `heo conversation
update` sends a ticket update (Zendesk's `zendesk.tickets.update`
extension effect, found as the one the adapter ships for its work items). Every update carries a marker in its audit, so its
echo, when Zendesk reports the change back, is never recorded as a new
message, and an update Zendesk may have applied is reconciled before it
is ever sent again.

## GitLab, Jira and Linear issues (`gitlab.issues`, `jira.issues`, `linear.issues`)

Three more issue trackers, each one channel per account or site. Each
issue is a work item bound to a conversation holding its description and
comments, so `conversation` and `message` read and answer them (each
conversation lists its work item), and `message edit` and `message delete`
work on the comments once `replies` is on. `conversation status` closes
and reopens a `gitlab.issues` issue through the transitions it declares
(no reasons), as it does a GitHub issue. None of the three ships an
extension effect, so the provider's own fields are ingest-only through
`conversation update` (the fields uma owns, `local_fields`, still change
in uma), and `jira.issues` and `linear.issues` change no issue state at all
(they declare no work item act). The adapters are experimental, and no
account has run them through heo.

Common to all three: `replies` (post replies as comments; off by
default, and `send`, `edit` and `delete` read `unsupported: config` in
`heo channel get` until it is on), `account_id` (the user the credential
acts as; required with `replies`), `local_fields`. Inboxes use mode
`per_work_item`. Store the API credential and the webhook secret in
separate entries when they differ.

| | `gitlab.issues` | `jira.issues` | `linear.issues` |
| --- | --- | --- | --- |
| Configuration | `api_url` (self-managed instance, `https`; default gitlab.com), `account_id` (numeric user ID; a project access token's bot user) | `site_url` (required, `https://<site>.atlassian.net`), `cloud_id` (with an OAuth 2 credential), `email` (with a bare API token), `account_id` (Atlassian `accountId`) | `api_url` (default Linear's GraphQL endpoint), `account_id` (user UUID, lower case) |
| Inbox destination | the project's full path (`group/subgroup/project`) | the project key (`SBX`) | the team key (`ENG`) |
| API credential | `gitlab.issues.api`: `oauth2`, or a `secret` access token with `api` scope (`read_api` reads only) | `jira.issues.api`: `password`, `secret` with `email`, or `oauth2` with `cloud_id` | `linear.issues.api`: a `secret` API key (sent without a prefix) or `oauth2` |
| Webhook | `Issue Hook` and `Note Hook`, the secret token as `gitlab.issues.webhook` | an admin webhook (System, WebHooks) for the issue and comment events, with `jira.issues.webhook` as its secret | `Issue` and `Comment` events, signing secret as `linear.issues.webhook`; a delivery more than a minute from the clock is refused |
| Verbs | `message edit`, `message delete`, `history fetch`, `conversation status` | `message edit`, `message delete` | `message edit`, `message delete` |

Webhook URL for all three: `<public_base_url>/webhooks/<channel id>`,
over https. GitLab sends its secret token as plain text, which GitLab's
own documentation calls weaker than a signing token: serve the webhook
over https only. Two channels on one account are refused as for any
other adapter (the account keys are in the table above).

What heo has not done for them: any test that runs one of these
adapters beyond its registration, account key, `replies` switch and, for
`gitlab.issues`, the close and reopen in process. See [provider-matrix.md](provider-matrix.md).
