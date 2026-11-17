# Channels and inboxes

Connect heo to a provider and decide where inbound lands.

- An **adapter** is an implementation that ships in the heo binary
  (`smtp.mail`, `native.webwidget`, ...).
- A **channel** is one configured connection of an adapter: its
  configuration, and a reference to its credential.
- An **inbox** binds destinations of one channel (a site, an address,
  a number) to a conversation selection mode. Inbound lands in an
  inbox, and desk commands filter by inbox.

Channels and inboxes live in the profile's data store. The daemon owns
that store, so every channel and inbox command runs in the daemon. Start
it first with `heo service start`. With no daemon running, these
commands exit 70 and name the command that starts one.

## Set up a channel

1. Pick the adapter. Each one declares its capabilities, its maturity,
   its configuration schema and the credential purposes it resolves:

   ```sh
   heo adapter list
   heo adapter get native.webwidget
   ```

   `adapter` commands read the binary itself. They need no daemon, no
   store and no network.

2. Store the credential in the profile's credential backend under a
   reference. See [Credentials](configuration.md#credentials) and
   [Credential purposes](adapters.md#credential-purposes). heo takes the
   reference and never the credential itself. A `--credential-ref` or
   `--configuration` that looks like a credential is refused (exit 2).

3. Preview the channel, then create it:

   ```sh
   heo channel create --type native.webwidget --key web \
     --configuration '{"sites":["site-1"]}' --credential-ref widget-key --dry-run
   heo channel create --type native.webwidget --key web \
     --configuration '{"sites":["site-1"]}' --credential-ref widget-key
   ```

   `--configuration` is the adapter's configuration as a JSON object.
   [adapters.md](adapters.md) describes it for each adapter. `--key`
   names the channel, so other commands can take `web` in place of its
   ID. Creating a channel opens no connection and calls no provider.

4. Create its inbox:

   ```sh
   heo inbox create --channel web --key webin --name "Website" \
     --destination site-1 --mode per_contact \
     --lock-to-single=true --lifecycle-rules contact-lifecycle/v1
   ```

   `--mode` is `per_contact`, `per_room`, `per_thread` or
   `per_work_item`. `per_contact` also needs `--lock-to-single` and
   `--lifecycle-rules contact-lifecycle/v1`; the other modes take
   neither. uma validates the selection. See
   [Choose a selection mode](#choose-a-selection-mode). `--deliver-notes` delivers internal notes to the provider
   as private notes, and only works with an adapter that declares
   `internal_note`.

5. Check the channel:

   ```sh
   heo channel check web
   heo channel check web --remote
   ```

   No restart is needed. The running daemon starts the channel's
   receiver and mounts its webhook route as soon as `channel create`
   commits. `heo daemon ping` shows it on its scope, `channel:<id>`.

## Choose a selection mode

The mode decides what one conversation is: inbound is filed by it, and
a first message (`heo message send`) is addressed by it.

| Mode | One conversation per | Inbound must carry | `message send --to` |
| --- | --- | --- | --- |
| `per_contact` | contact identity (the sender) | a sender | `<kind>:<id>`, the sender's route |
| `per_room` | provider room | a room reference | `room:<id>` |
| `per_thread` | provider thread | a thread reference | `thread:<id>` |
| `per_work_item` | issue or ticket | a work item reference | cannot start one |

A room or thread conversation has no owner: no contact and no identity
belong to it, and everyone who writes in the room or thread joins the
same conversation. Replies go to the room or thread, whoever wrote last.
A message that carries no reference of its mode's kind is refused, and
files nowhere. Most adapters heo ships file by contact or by work
item; `per_room` and `per_thread` are for a chat adapter that reports
rooms and threads, which `slack.chat`, `telegram.messages`, `discord.chat` and
`microsoft.teams` do (see [Slack](adapters.md#slack-slackchat),
[Telegram](adapters.md#telegram-telegrammessages),
[Discord](adapters.md#discord-discordchat) and
[Teams](adapters.md#microsoft-teams-microsoftteams)).

The per-contact settings (`--lock-to-single`, `--lifecycle-rules`) belong
to `per_contact` alone: another mode refuses them (exit 2). Updating an
inbox to another mode drops them, and returning to `per_contact` asks
for them again. Conversations already filed keep the mode they were
filed under.

```sh
heo inbox create --channel chat --key team --destination workspace-1 --mode per_room
heo inbox update team --revision 01a1... --mode per_thread
```

## Change a channel or inbox

Every update names the revision it replaces, as `get` shows it:

```sh
heo channel get web                 # revision 01a1...
heo channel update web --revision 01a1... --configuration '{"sites":["site-1","site-2"]}'
heo inbox update webin --revision 01b2... --destination site-1,site-2
```

Fields you leave out keep their value. `--destination` replaces the
whole list. A channel never changes its adapter type, and an inbox never
moves to another channel. If the record changed after you read it, the
update fails with `CONFLICT` (exit 4) and writes nothing. Read it again,
then decide.

### What the running daemon does with a change

Every write applies to the running daemon once it commits. You never
restart it for a channel or inbox change:

- **Channel created:** its receiver starts, and its webhook route is
  mounted.
- **Channel updated:** its receiver and its webhook route are replaced
  with ones built on the new configuration. Requests still open on the
  old route end, so a widget visitor's event stream reconnects and
  resumes. A channel whose adapter no longer receives over HTTP (a
  mailbox with push turned off) loses its route, and the route answers
  404.
- **Inbox created, updated, enabled or disabled:** the next inbound
  message is filed by the inbox as stored, so a new destination, a
  selection change or a disabled inbox applies to it.
- **Every other channel** keeps its receiver, its route and the
  requests open on it.

The provider account rule still holds while the daemon runs. The write
itself refuses an account another channel of the profile receives (see
below). If a channel is created or updated onto an account that the
daemon of another profile on this host receives, it does not take that
account. It fails on its own scope, and its route answers 503. Every
other channel keeps running. Once the account is free, the next
channel or inbox write gives it to the waiting channel. See
[runtime.md](runtime.md#one-receiver-per-provider-account).

## Turn an inbox off and on

```sh
heo inbox disable webin --revision 01b2...
heo inbox enable webin --revision 01c3...
```

A disabled inbox rejects inbound: a widget visitor's message gets HTTP
422, and a mail or message to its destinations is refused. Nothing else
stops:

- the channel keeps its connection, and its other inboxes keep
  receiving;
- sends already queued, replies included, still deliver;
- existing conversations stay as they are.

Channels have no on/off switch. Availability lives on the inbox.

## Preview, retry, conflicts

- `--dry-run` on every write prints the exact record it would write,
  plus the current record for an update, and writes nothing. It runs
  the checks the real write runs: uma's request and adapter validation,
  the revision, key and destination uniqueness, and the provider
  account rule. One check stays out: a named `--selection-policy` is
  checked only on submission, and the preview warns about it.
- Each write is a uma operation keyed by `--idempotency-key`. Without
  one, heo mints a key (`op_...`) and prints it as `operation key`. If
  you lose the answer, rerun the same command with the same key. You
  get the stored operation back (`replayed`), not a second write and
  not a conflict with your first attempt. The same key with different
  arguments is a conflict.
- These refusals write nothing, and each exits 4 (`CONFLICT`):
  - a key another channel or inbox already holds;
  - a destination another inbox of the same channel already receives
    (each destination has one inbox, so a receiver never has to choose);
  - a provider account another channel of the profile already receives
    (each account has one receiver; see
    [One receiver per provider account](adapters.md#one-receiver-per-provider-account)).

## Authorize a mailbox

A consumer Gmail account and a Microsoft 365 mailbox reached with
delegated permissions need a person to sign in once. `heo channel
authorize <channel>` runs that consent and keeps the refresh token as the
grant of the channel's credential reference; from then on heo mints and
refreshes every access token itself ([OAuth
grants](configuration.md#oauth-grants)).

```sh
heo channel authorize gmail --client-id 1234.apps.googleusercontent.com --client-secret-ref gmail-client --dry-run
heo channel authorize gmail --client-id 1234.apps.googleusercontent.com --client-secret-ref gmail-client
heo channel authorize outlook --client-id 6a1b0c9d-0000-4000-8000-000000000000 --directory organizations
heo channel authorize gmail          # again, after the grant stopped working: no flags
```

- You bring the OAuth client: a Google "desktop app" client (it has a
  secret; store it under a credential reference and pass
  `--client-secret-ref`) or a Microsoft Entra app registration with public
  client flows allowed and `http://localhost` as a redirect. heo registers none.
- It is the authorization code flow with PKCE. heo listens on `127.0.0.1`
  on an ephemeral port for the one redirect, prints the consent URL and opens
  your browser (`--no-browser` leaves that to you). The URL carries its own
  state and challenge; no other local request can start or finish the login.
  Run it on the machine whose browser you sign in with.
- Scopes: the adapter's own (`channel get` shows them; Microsoft adds
  `offline_access`), or `--scope`. Another user's Graph mailbox needs
  `Mail.ReadWrite.Shared`. `--directory` is the Entra tenant: `common`
  (default), `organizations`, `consumers`, a tenant ID or a domain. Only the
  global Microsoft cloud is mapped; for a national cloud store the grant by
  hand.
- The grant goes under the channel's credential reference, the refresh token
  under `<ref>.refresh_token` (`--refresh-ref`), in a writable backend:
  `keyring`, `file` or `openbao`; an aps-bound profile and `agefile` are
  refused. A reference that holds another kind of credential is refused
  unless `--replace` says it may be lost; a refresh_token grant is replaced,
  that is a second consent. A purpose with an entry of its own keeps it and
  is named in the output.
- The output names the keys and the first access token's expiry, never a
  token. `heo channel check <channel> --remote` then shows the grant.
- `--dry-run` prints the scopes, the client, the keys and the consent URL
  (state, challenge and port left open), opens nothing and writes nothing.
- Operator role, through the running daemon, which is asked for the channel.
  Terminal only: REST, gRPC and MCP do not serve it and a plugin may not run it.
- Declined consent, a provider that issues no refresh token and a timeout
  (`--timeout`, five minutes) store nothing.

Google lifetime, from Google's documentation and not probed against a live
project here: an OAuth client whose consent screen is in publishing status
"Testing" gets refresh tokens that expire after seven days when it asks for
a Gmail scope (all are sensitive or restricted), so run `channel authorize`
weekly, or move the project to "In production" (an unverified app shows a
warning and is capped at 100 users) or use a Workspace "Internal" app.
Google's page also says the refresh token stops working when the user
revokes the app's access, after six months without use, and when the
account holds more live tokens than the limit (100 per account and client).
The seven-day expiry is **documented by Google and still to be probed on a
live project**; until then treat it as the cost of "Testing", not as a
measured fact.

## Mailbox access: setups, what each grants, how to revoke

Four ways to give a channel a mailbox. They differ in who consents, whose
mailboxes the credential can reach, and what ends the access. All four end
as a grant stored under the channel's credential reference, which heo turns
into short-lived access tokens ([OAuth grants](configuration.md#oauth-grants));
the channel's `google.mail` or `microsoft.mail` configuration is the same
whichever you pick ([adapters.md](adapters.md#gmail-googlemail)).

| Setup | Who consents | Reaches | Grant kind | Ends when |
| --- | --- | --- | --- | --- |
| Gmail, user consent | the mailbox's user, once | that user's mailbox | `refresh_token` | the user removes the app, six months unused, the project's "Testing" expiry |
| Gmail, service account with domain-wide delegation | a Workspace super administrator | any user of the domain the scopes name, one at a time by `subject` | `jwt_bearer` | the delegation entry or the key is removed |
| Microsoft 365, delegated | the mailbox's user, once | that user's mailbox (others' with `Mail.ReadWrite.Shared`) | `refresh_token` | the user or an administrator revokes it, 90 days unused |
| Microsoft 365, app-only with a certificate | a tenant administrator | every mailbox of the tenant, unless restricted | `client_assertion` | the permission, the certificate or the service principal is removed |

### Gmail with user consent

`heo channel authorize` (above). The OAuth client is yours; the consent screen's
publishing status decides the refresh token's life. The grant covers the scopes
the adapter lists (`gmail.readonly`, `gmail.send`, `gmail.modify`) for that
user's mailbox and nothing else.

Revoke: the user removes the app under their Google Account's third-party
access (`myaccount.google.com/linkedapps`), or you delete the credential
entry. Either ends the grant: heo then reports the credential `unauthorized`
and the receiver stops for that channel. Deleting the entry does not revoke
the token at Google; removing the app does. Data heo already stored stays.

### Gmail with a service account and domain-wide delegation

For a Google Workspace domain whose mailboxes no person signs in to for heo. No
browser step and no seven-day expiry: Google says the refresh-token limit does
not apply to service accounts, and the grant mints a token from the key each
time.

1. In a Google Cloud project, create a service account and a JSON key for
   it (Keys, Add key, Create new key). The file is the only copy of the
   private key. An organization policy can forbid creating keys; ask for an
   exception for this project.
2. A super administrator opens the Admin console, Security, Access and data
   control, API controls, Manage domain-wide delegation, Add new, and enters
   the service account's numeric **Client ID** (not its email address) and
   the scopes: the three Gmail scopes above. It can take up to 24 hours to
   reach every user.
3. Store the key as a `secret` whose value is the key JSON, and the grant
   under the channel's credential reference. `subject` is the mailbox the
   channel reads; the token acts with that user's permissions only.

```json
{"v":1,"kind":"jwt_bearer","token_url":"https://oauth2.googleapis.com/token",
 "scopes":["https://www.googleapis.com/auth/gmail.readonly",
           "https://www.googleapis.com/auth/gmail.send",
           "https://www.googleapis.com/auth/gmail.modify"],
 "subject":"support@example.com","secret_ref":"uma/default/gmail-sa-key"}
```

One channel per mailbox: a second mailbox is a second channel with its own
reference and `subject`, reusing the key entry. Set the channel's `user` to
the same address.

What it grants: every user of the domain can be impersonated for the listed
scopes by anyone holding the key, so keep the key in a backend you trust and
list only the scopes the adapter needs. Service accounts are not subject to the
domain's policies.

Revoke: remove the client ID's entry under Manage domain-wide delegation
(all impersonation stops), or narrow its scopes; delete or disable the key
in the Cloud console (the token request then fails); delete the credential
entry in heo. Tokens already minted work until they expire, within the hour.

### Gmail push: the Pub/Sub topic

Without a topic the receiver polls. For push, in the project that owns the
OAuth client or the service account: create a topic (any name under the
project), grant `publish` on it to `gmail-api-push@system.gserviceaccount.com`
(a domain-restricted-sharing policy can block that grant; add an exception
for the service account), set the channel's `topic`
(`projects/<project>/topics/<topic>`), and create the push subscription with
authentication as [adapters.md](adapters.md#gmail-googlemail) says. The adapter
keeps the mailbox's `users.watch` on the topic and renews it daily; Gmail
stops pushing for a mailbox whose watch lapses after seven days. A refused
watch leaves the channel polling and `degraded` (`google.mail.push_denied`,
`push_rejected`, `push_unavailable`). Remove the topic setting, or delete the
subscription, to turn push off; history polling covers the mailbox either way.

### Microsoft 365, delegated

`heo channel authorize outlook ...` (above): the signed-in user consents to
`Mail.ReadWrite`, `Mail.Send` and `offline_access`, for their own mailbox;
another user's mailbox needs `Mail.ReadWrite.Shared` and that user's grant of
access to it. Microsoft's default refresh token lifetime is 90 days for this
flow, and each use issues a new one, which heo stores. Set `mailbox` to the user
or leave it empty for the signed-in user.

Revoke: the user removes the app from their account's app permissions, or an
administrator revokes the user's refresh tokens (sign-in sessions), or removes
the app's permission grants for the user or the tenant in Enterprise
applications, Permissions. A revoked refresh token is `unauthorized` in
`channel check --remote`; consent again with `channel authorize`. A password
change does not revoke the token of a confidential client, which is what a
`refresh_token` grant with a client secret is.

### Microsoft 365, app-only with a certificate

For a mailbox no person signs in for. The app acts as itself, with
**application permissions**, so a tenant administrator must consent and there
is no signed-in user: set `mailbox` to the user ID or principal name.

1. Register an app in Microsoft Entra, single tenant. Under Certificates &
   secrets, Certificates, upload the public certificate. (A trusted
   certificate is what Microsoft recommends; a self-signed one is for testing.)
2. Under API permissions add the Microsoft Graph **application** permissions
   `Mail.ReadWrite` and `Mail.Send`, and have an administrator grant consent.
3. Store the certificate and its RSA private key as a `client_cert` entry,
   and the grant under the channel's credential reference:

```json
{"v":1,"kind":"client_assertion",
 "token_url":"https://login.microsoftonline.com/<tenant id>/oauth2/v2.0/token",
 "client_id":"<application id>","scopes":["https://graph.microsoft.com/.default"],
 "client_cert_ref":"uma/default/graph-cert"}
```

`.default` asks for every application permission consented to the app, so the
token carries exactly what step 2 granted.

**Restrict the app to named mailboxes.** Consented as above, `Mail.ReadWrite`
and `Mail.Send` reach every mailbox of the tenant. Limit them before the
channel goes live, with Exchange Online RBAC for Applications (it replaces
Application Access Policies):

```powershell
Connect-ExchangeOnline
New-ServicePrincipal -AppId <application id> -ObjectId <service principal object id> -DisplayName "heo"
New-ManagementScope -Name "heo mailboxes" -RecipientRestrictionFilter "MemberOfGroup -eq '<group distinguished name>'"
New-ManagementRoleAssignment -App <service principal object id> -Role "Application Mail Full Access" -CustomResourceScope "heo mailboxes"
Test-ServicePrincipalAuthorization -Identity <service principal object id> -Resource support@example.com
```

Use the IDs of the Enterprise applications page (the object ID there), not the
App registrations page. Put the named mailboxes in a mail-enabled security group
or select them by an attribute in the filter. Then **remove the tenant-wide
`Mail.ReadWrite` and `Mail.Send` consents in Entra**: the two systems add up, so
a leftover unscoped consent leaves the app unrestricted. `Application Mail Full
Access` carries `Mail.ReadWrite` and `Mail.Send`. Changes take 30 minutes to 2
hours to apply to an app in use; `Test-ServicePrincipalAuthorization` shows the
result at once and `InScope` says whether a mailbox is reachable.

What it grants: read, write, delete and send for the mailboxes in scope, as
the app, with no user present and no sign-in prompt. Anyone holding the
certificate's private key has it.

Revoke: remove the role assignment (Exchange) and the application permissions
(Entra, Enterprise applications, Permissions, revoke), delete the certificate
from the app registration (new token requests fail), or delete the service
principal (Exchange drops its assignments with it). Tokens already minted
last up to an hour. Delete the credential entries in heo too.

## Checks

`heo channel check [channel]` checks one channel, or every channel when
you name none. It writes nothing and reports these findings:

| Finding | Severity |
| --- | --- |
| adapter missing from this build | error |
| configuration the adapter rejects | error |
| provider account another channel receives | error |
| inbox selection invalid, destination bound twice, note delivery the adapter does not declare | error |
| no credential reference while the adapter needs one | warning |
| no inbox, inbox without destinations, disabled inbox | warning |

The local check reads only the data store and uses no network.

`--remote` probes one channel as you, the caller. It opens the
adapter's instance with the channel's credentials, which may reach the
provider. For each capability the adapter declares, it reports:

- whether the capability is available, after the provider's permissions
  where the adapter probes them, and after what this uma version runs;
- whether the channel needs it: receiving for an enabled inbox with
  destinations, `send` for replies, `internal_note` for note delivery.

A capability the channel needs but cannot use is an error. The report
also gives a state for each provider-dependent verb (`message edit`,
`message delete`, `reaction add`, `reaction remove`, `participant add`,
`participant remove`, `receipt send`, `history fetch`, `interaction respond`,
`interaction open-modal`, `interaction push-modal`, `interaction update-modal`,
`conversation status`, `conversation update`, `conversation tag`, `message reply-draft`, `conversation unsubscribe`, `conversation archive`, `conversation unarchive`, `conversation move`, `conversation read`, `conversation unread`, `conversation flag`, `conversation unflag`, `conversation trash`, `conversation untrash`) and for the reply recipient flags of `message reply` and `message reply-draft` (`--reply-all`, `--cc`, `--bcc`, which need `reply_recipients`; the report adds the limits the adapter states: reply-all, cc, bcc and how many addresses a reply carries):
`available`, `unauthorized`, `unavailable`, `unsupported` or `disabled`, each
with the error code and exit status a command hitting it would give. A verb is never a required capability, so one
that is not available is not an error finding. Each
credential purpose of the adapter is diagnosed as `ok`, `missing`,
`expired`, `rejected` or `malformed`, and no secret is ever read out. One only a way of
receiving or `send` uses is skipped when the channel's own configuration
switches that way off (a polled Telegram bot's webhook secret, the callback
secret of a `relay.messages` channel that names no callback). Where an
adapter takes one of several credentials for the same thing, exactly one must
be stored (a Teams bot's client secret or its certificate): none is one error
finding naming them all, more than one a `conflict` error, and the ones not
stored are not listed. A credential that works but will not for long is a
warning, not an error, and the check still exits 0: a Teams certificate within
30 days of its `not_after`.
`--remote` with `--offline` exits 2.

The report also says how the running daemon runs each way the adapter
receives (`receiving` in JSON): the persistent receiver (a Discord gateway,
a Slack socket, a poll; way `receive`) and the webhook route (way
`receive_http`), each `starting`, `ready`, `waiting`, `degraded` or `failed` with its
reason, the receiver with the holder of its receiver lease and when it
lapses, the route with its path. A way the channel's configuration switches
off reads `off`, and `not_running` says the daemon runs no service for it. A
receiver that stopped is an error finding and one that is waiting or degraded a warning,
when an enabled inbox with destinations needs it.

The receiver also says what its receive call is doing, from the daemon that
runs it: `phase` (`running`, or `retrying` after a failed connection), `link`
(`connecting` or `connected`, for an adapter that reports its provider link:
the Discord gateway, Slack Socket Mode, Telegram polling, IMAP), `failures`
(the connections in a row that accepted nothing), `last_failure` (the last
one's error code) and `retry_at` (when the next connection opens). A receiver
reads `ready` once its link is up, `waiting` while the call retries or the
link is down, and `degraded` while it receives but part of it fails; see [How a receiver reads](runtime.md#how-a-receiver-reads).
Fields that do not apply are left out. A lease another daemon holds shows its
`owner` and `until` alone, with no phase: ask that daemon.

```json
{"way": "receive", "state": "waiting", "reason": "rate_limited: retrying at 2026-10-15T13:00:00Z after 1 failure",
 "owner": "heo/default/receiver", "until": "2026-10-15T12:01:00Z",
 "phase": "retrying", "failures": 1, "last_failure": "rate_limited", "retry_at": "2026-10-15T13:00:00Z"}
```

`channel check` exits 2 when any finding is an error. It prints the
report first. Use `--format json` for agents.

`heo channel get <channel>` also lists, per verb, whether the channel's
adapter declares what it needs, computed offline from the binary and the
record with no provider reached. See
[provider-verbs.md](provider-verbs.md).

With `--format json` each verb row also has a `limits` object, always
present and `{}` when the adapter declares nothing for the verb (or the
verb is unsupported). Its keys are omitted when empty:

```json
{"verb": "conversation status", "capability": "work_item_act", "state": "declared",
 "limits": {"transitions": [{"to": "closed", "from": ["open"], "reasons": ["completed", "not_planned"]}]}}
{"verb": "reaction add", "capability": "react", "state": "declared",
 "limits": {"reactions": "single"}}
{"verb": "conversation update", "capability": "extension_execute", "state": "declared",
 "limits": {"effects": ["github.issues.update"]}}
```

| `limits` key | On | Meaning |
| --- | --- | --- |
| `reactions` | `reaction add`, `reaction remove` | `single` (one reaction per reactor, as on Telegram) or `multiple` |
| `transitions` | `conversation status` | work item statuses the adapter moves items `to`, `from` which statuses, with the `reasons` it accepts |
| `effects` | `extension_execute` verbs | the adapter's extension effects the verb can run (for `conversation update`, the update effect it ships for its work items) |

These are read from the adapter's descriptor, so they are what the
adapter declares, not what the provider account allows. uma's descriptor
declares no sizes, scopes or label rules per verb, so none is reported;
`channel check --remote` is the live check.

`channel get --format json` also has `required_permissions`, once for the
channel and next to `verbs`: what the adapter's descriptor lists that the
credential needs. It is adapter-wide, not per verb, always present, and
`[]` when the adapter lists none or is not in the build. What an entry
means depends on the adapter kind:

| Adapter kind | Entries are | Example |
| --- | --- | --- |
| Gmail | OAuth scopes | `https://www.googleapis.com/auth/gmail.send` |
| Microsoft Graph mail | Graph permissions | `Mail.ReadWrite`, `Mail.Send` |
| Slack | bot token scopes | `chat:write`, `channels:history` |
| Discord | bot permissions in the guild | `SEND_MESSAGES`, `ADD_REACTIONS` |
| GitHub, Zendesk | token permissions or scopes | `issues:write`, `agent` |
| WhatsApp | Meta permission | `whatsapp_business_messaging` |
| IMAP, SMTP | credential purposes | `imap`, `smtp.submission`, `smtp.relay` |

```json
{"key": "gh", "type": "github.issues", "verbs": [],
 "required_permissions": ["issues:read", "issues:write", "metadata:read"]}
```

Like `limits`, it is what the descriptor declares, not what the account
holds; `channel check --remote` is the live check.

`heo config validate` runs the local check on every channel through the
running daemon. Findings show up as issues with origin `store`. With no
daemon running, it skips them and says so under `channels.skipped`. It
never opens the store behind the daemon's back.

## Who may do what

| Command | Role |
| --- | --- |
| `adapter list`, `adapter get` | none: offline |
| `channel list`, `get`, `check` | `operator` |
| `channel create`, `update` | `operator` |
| `inbox list`, `get` | any role that reads conversations (`desk`, `viewer`, `external`) and `operator` |
| `inbox create`, `update`, `enable`, `disable` | `operator` |

Previews need the same role as the write. A caller whose roles do not
grant the command is refused with `UNAUTHORIZED` (exit 5) on every
surface, before anything is read. See
[authorization.md](authorization.md).

Once inbound lands, work it with the desk commands: see
[conversations.md](conversations.md).
