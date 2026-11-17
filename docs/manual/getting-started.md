# Getting started: your first conversation

A tutorial. You will run the daemon, connect the web widget, receive a visitor's message, reply
to it and watch the reply leave, using only your terminal and `curl`. About ten minutes.

You need a built `heo` (see [install](install.md#build)), PostgreSQL 15 or later with an empty
database and a user for heo, and `jq` and `openssl`. The commands below use the `default` profile
and the `file` credential backend, which keeps secrets in files on this machine; for anything
beyond a trial, read [install](install.md).

## 1. Point heo at the database

```sh
heo config set credentials.backend file
heo config set storage.postgres.dsn 'host=localhost dbname=heo user=heo'
heo config set storage.postgres.password_ref postgres-password
```

Store the database password where the reference points. A credential is a small JSON envelope in
a file under the backend's directory, named by the tenant and the reference:

```sh
dir="$(heo config get credentials.file.dir --format json | jq -r .value)"
mkdir -p -m 700 "$dir/uma/default"
(umask 077; printf '{"v":1,"kind":"password","user":"heo","password":"%s"}' "$DB_PASSWORD" \
  > "$dir/uma/default/postgres-password")
```

Create the schema:

```sh
heo storage status                  # compat: uninitialized
heo storage migrate                 # migrated schema 0 → 19
```

## 2. Start the daemon

The daemon owns the database and receives your channels. It runs in the foreground, so give it a
terminal of its own (or put `&` after it):

```sh
heo config set services.webhook.enabled true
heo config set services.webhook.public_base_url https://hooks.example.com/heo
heo service start
```

In another terminal, check it:

```sh
heo service status                  # every service ready
heo daemon ping                     # who the daemon serves you as: uid:<n>
```

No other command starts a daemon for you. A command that needs one and finds none exits 70 and
tells you to run `heo service start`.

## 3. Connect the web widget

The widget needs a signing secret of at least 32 bytes. Store it under the reference
`widget-key`:

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$(openssl rand -hex 32)" \
  > "$dir/uma/default/widget-key")
```

Create the channel and its inbox. Preview first; `--dry-run` writes nothing:

```sh
heo channel create --type native.webwidget --key web \
  --configuration '{"sites":["site-1"]}' --credential-ref widget-key --dry-run
heo channel create --type native.webwidget --key web \
  --configuration '{"sites":["site-1"]}' --credential-ref widget-key
heo inbox create --channel web --key webin --name "Website" --destination site-1 \
  --mode per_contact --lock-to-single=true --lifecycle-rules contact-lifecycle/v1
heo channel check web --remote      # the adapter, the credential, what the channel can do
```

The running daemon starts receiving the channel as soon as it is created.

## 4. A visitor writes in

In production your site talks to the channel's webhook URL through your reverse proxy. Here you
play the visitor against the listener directly:

```sh
channel="$(heo channel get web --format json | jq -r .ref.id)"
url="http://127.0.0.1:8083/webhooks/$channel"
token="$(curl -s -X POST -H 'Content-Type: application/json' -d '{}' "$url/session?site=site-1" \
  | jq -r .token)"
curl -s -X POST -H 'Content-Type: application/json' -H "Authorization: Bearer $token" \
  -d '{"id":"m-1","text":"Hi, my invoice is wrong."}' "$url/messages"
```

## 5. Read and reply

```sh
heo conversation list --status open
conv="$(heo conversation list --status open --format json | jq -r '.items[0].ref.id')"
heo message list --conversation "$conv"
```

Every ID is a record ID, as a listing printed it. Preview the reply, then send it:

```sh
heo message reply "$conv" --text "Thanks, we are checking your invoice." --dry-run
heo message reply "$conv" --text "Thanks, we are checking your invoice."
```

The reply returns `queued` with an `operation`: uma accepted it durably and the daemon's workers
deliver it. Follow the operation:

```sh
op="$(heo delivery list --format json | jq -r '.items[0].operation')"
heo delivery get "$op"              # state: provider_accepted
```

If the answer to a write ever gets lost, `heo operation get --key <operation key>` finds it again
by the key the write printed.

## 6. Work the conversation

```sh
heo label create billing
heo conversation label "$conv" --add billing
heo note add "$conv" --text "Customer is on the legacy plan." --placement local
heo conversation resolve "$conv" --revision "$(heo conversation get "$conv" --format json | jq -r .revision)"
```

A note is internal: only the `desk` role sees it, and it is never sent to the visitor. A write
that answers `TRANSIENT` (exit 6) lost a race with another write to the same conversation: run it
again.

## 7. Optional: connect Slack

A chat workspace works the same way, with two secrets instead of one: the
bot token and the signing secret of a Slack app you installed (see
[Slack](adapters.md#slack-slackchat) for the app's scopes and event
subscriptions, and for Socket Mode, which needs no public URL but an
app-level token instead of the signing secret). Store each under its own
purpose of the reference `slack`, and say the app takes its payloads at its
request URL (`"receive":"events_api"`):

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$SLACK_BOT_TOKEN" \
  > "$dir/uma/default/slack@slack.chat.api")
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$SLACK_SIGNING_SECRET" \
  > "$dir/uma/default/slack@slack.chat.signing")
heo channel create --type slack.chat --key chat --credential-ref slack \
  --configuration '{"team_id":"T0123ABCD","bot_user_id":"U0123ABCD","receive":"events_api"}'
heo inbox create --channel chat --key rooms --destination T0123ABCD --mode per_room
heo channel check chat --remote
```

Give Slack the URL `<public_base_url>/webhooks/<channel id>` for events,
interactivity and slash commands. Once Slack verifies it, a message in a
channel the bot is in opens a conversation; read and answer it as above.
To write first, name the channel with its namespace:

```sh
heo message send --inbox rooms --to slack.chat/room:C0123ABCD --text "Hello from heo."
```

## 8. Optional: connect Telegram

A Telegram bot (made with [@BotFather](https://t.me/BotFather)) works the same
way, and needs a public URL only if you receive by webhook rather than by
polling (see [Telegram](adapters.md#telegram-telegrammessages)). Store the bot
token; with a webhook, store the secret token you will give Telegram too:

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$TELEGRAM_BOT_TOKEN" \
  > "$dir/uma/default/telegram@telegram.messages.api")
heo channel create --type telegram.messages --key bot --credential-ref telegram \
  --configuration '{"bot_id":123456789,"bot_username":"acme_support_bot","receive":"poll"}'
heo inbox create --channel bot --key chats --destination 123456789 --mode per_room
```

With `receive` `poll` and no webhook set at Telegram, the daemon reads the
bot's updates (`"receive":"webhook"` serves the webhook route instead): a
message in a chat the bot is in opens a conversation; read and answer it as
above. To write first, name the chat with its namespace (a group's ID is
negative):

```sh
heo message send --inbox chats --to telegram.messages/room:-1001234567890 --text "Hello from heo."
```

## 9. Optional: connect iMessage

Apple has no iMessage API, so iMessage goes through a **gateway you run**: a
small service in front of an iMessage backend (a hosted relay such as Blooio,
or BlueBubbles Server on a Mac signed in to Messages). heo states no support
for any particular provider; uma's reference gateway
(`cmd/imessagegateway` in the uma repository) is a starting point. The
gateway posts each incoming message to heo, signed, and takes each reply at a
callback URL (see [Relay gateway](adapters.md#relay-gateway-relaymessages),
and its operator note on Apple's terms). Name the gateway's host as the only one
a callback may go to, then store the secret the gateway signs with and the one
heo presents to it:

```sh
heo config set adapters.relay.callback_hosts gateway.example.com
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$WEBHOOK_SECRET" \
  > "$dir/uma/default/gateway@relay.webhook")
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$CALLBACK_SECRET" \
  > "$dir/uma/default/gateway@relay.callback")
cfg='{
  "medium": "imessage",
  "auth": {"scheme": "hmac", "header": "X-Webhook-Signature",
           "timestamp": {"header": "X-Webhook-Timestamp"}, "replay": {"source": "signature"}},
  "callback": {"url": "https://gateway.example.com/uma/reply",
               "lookup_url": "https://gateway.example.com/uma/lookup"}}'
heo channel create --type relay.messages --key imessage --credential-ref gateway --configuration "$cfg"
heo inbox create --channel imessage --key messages --destination +15550100002 \
  --mode per_contact --lock-to-single=true --lifecycle-rules contact-lifecycle/v1
heo channel check imessage --remote
```

The destination is the address the gateway reports as `to`: the line's number,
or the Apple ID the Mac sends from. Point the gateway at
`<public_base_url>/webhooks/<channel id>` (restart the daemon after changing a
setting). A message opens a conversation; read and answer it as above. To write
first, name the namespace and the handle, a number or an Apple ID's email address
(a start lower-cases it, so its reply lands in the same conversation):

```sh
heo message send --inbox messages --to relay.imessage/phone:+15550100003 --text "Hello from heo."
```

## 10. Optional: connect Discord

A Discord application with a bot (made in the
[Developer Portal](https://discord.com/developers/applications)) receives
over the gateway: the daemon connects out, so it needs no public URL unless
you also use an Interactions Endpoint URL (see
[Discord](adapters.md#discord-discordchat)). Store the bot token and create
the channel; `bot_user_id` is the bot's user ID:

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$DISCORD_BOT_TOKEN" \
  > "$dir/uma/default/discord@discord.chat.api")
heo channel create --type discord.chat --key bot --credential-ref discord \
  --configuration '{"application_id":"123456789012345678","bot_user_id":"123456789012345679","receive":"gateway"}'
heo inbox create --channel bot --key rooms --destination 123456789012345678 --mode per_room
heo channel check bot --remote      # the gateway receiver, its lease and the route
```

A message that mentions the bot in a channel it can see, or a direct message to
it, opens a conversation; read and answer it as above. To write first, name the
channel with its namespace:

```sh
heo message send --inbox rooms --to discord.chat/room:123456789012345680 --text "Hello from heo."
```

## 11. Optional: connect Microsoft Teams

A Teams bot is an Azure Bot registration of a tenant you administer, with the
Teams channel on and its Teams app installed where it should speak (see
[Teams](adapters.md#microsoft-teams-microsoftteams) for the messaging endpoint,
the credential and what Teams lets a bot do first). heo needs one credential,
the app's client secret or a certificate registered for it; it mints and
refreshes the Bot Connector token itself:

```sh
(umask 077; printf '{"v":1,"kind":"secret","value":"%s"}' "$APP_SECRET" \
  > "$dir/uma/default/teams@microsoft.teams.client_secret")
heo channel create --type microsoft.teams --key teams --credential-ref teams \
  --configuration '{"app_id":"<app ID>","tenant_id":"<tenant ID>","service_url":"https://smba.trafficmanager.net/teams/"}'
heo inbox create --channel teams --key rooms --destination "$TENANT_ID" --mode per_room
heo channel check teams --remote    # mints a token: a refused secret is an error finding
```

To prove the app with a certificate instead, store it and its key under
`microsoft.teams.client_certificate`, and not the secret (see
[Teams](adapters.md#microsoft-teams-microsoftteams)); the check then warns 30 days
before the certificate ends.

Give the Azure Bot the messaging endpoint `<public_base_url>/webhooks/<channel
id>`. A message that @mentions the bot in a channel (or any message in a
personal chat) opens a conversation; read and answer it as above. To write
first, name the conversation with its namespace:

```sh
heo message send --inbox rooms --to 'microsoft.teams/room:19:abc123@thread.tacv2' --text "Hello from heo."
```

## 12. Stop

```sh
heo service stop                    # drain the daemon and wait for it to exit
```

## Where next

- Connect a real provider and the URL it reaches: [install](install.md), [channels](channels.md),
  [adapters](adapters.md).
- Call heo from a program: [surfaces](surfaces.md). Every verb: the
  [command reference](command-reference.md), [commands](commands.md) and the
  [cheatsheets](../cheatsheet-human.md).
- Roles, profiles, configuration: [authorization](authorization.md),
  [configuration](configuration.md).
- Hold a plugin's writes for a person's decision: [approvals](approvals.md).
- Something failed: [troubleshooting](troubleshooting.md).
