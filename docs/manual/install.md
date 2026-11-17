# Installing and running heo

No heo release has been published yet: there is no release archive, Homebrew cask or module
version to install. Build from source, then set up the four things a daemon needs before it can
receive anything: a database, a credential backend, a provider and, for most providers, a URL the
provider can reach.

If you only want to see heo work, skip to the [tutorial](getting-started.md): it runs the whole
path with the web widget and `curl`.

## Build

```sh
git clone https://github.com/hop-top/heo
cd heo
make build          # writes bin/heo
make symlink        # optional: links bin/heo into a user bin dir on PATH
heo --version
```

Needs the Go version `go.mod` names. `go.mod` replaces the uma modules with a local checkout:
building needs that checkout where it points.

Once releases are published, `heo upgrade` keeps the binary current; see [upgrade](upgrade.md).

## 1. A database

heo keeps conversations, messages and the delivery ledger in PostgreSQL 15 or later, one database
or schema per profile. Backups and restores also need `pg_dump` and `pg_restore` on the host, of a
major version at least the server's.

```sh
heo config set storage.postgres.dsn 'host=db.internal dbname=heo user=heo'
heo config set storage.postgres.password_ref postgres-password
heo storage status                  # reachable? what schema? (compat: uninitialized)
heo storage migrate                 # create the schema
```

The DSN never carries a password; the password comes from the credential backend through
`password_ref` (or from libpq's own `PGPASSWORD`). Only one daemon may own a store at a time. See
[Storage](configuration.md#storage).

## 2. Credentials

Configuration names where a credential lives, never the credential. Each profile has one backend,
`credentials.backend`:

| Backend | Where | Use it for |
| --- | --- | --- |
| `keyring` (default) | the OS keychain | a workstation |
| `file` | one `0600` file per secret under `credentials.file.dir` | a server, tests; unencrypted at rest |
| `agefile` | an age-encrypted file, read-only | secrets managed elsewhere |
| `openbao` | an OpenBao mount | a shared secret service |

A credential is a JSON envelope stored under the key `uma/<tenant>/<reference>`. With the `file`
backend that is a file you create:

```sh
heo config set credentials.backend file
dir="$(heo config get credentials.file.dir --format json | jq -r .value)"
mkdir -p -m 700 "$dir/uma/default"
(umask 077; printf '{"v":1,"kind":"password","user":"heo","password":"%s"}' "$DB_PASSWORD" \
  > "$dir/uma/default/postgres-password")
```

`default` is the profile's tenant (`heo config get runtime.tenant`). Channels refer to a credential
by the same kind of reference (`--credential-ref`); an adapter asks for one secret per purpose and
falls back to the reference's shared entry. The kinds, purposes and per-adapter examples are in
[Credentials](configuration.md#credentials) and
[Credential purposes](adapters.md#credential-purposes). A rotated value is read on the next
connection; after storing or rotating a channel's credential, restart the daemon.

## 3. A provider

```sh
heo adapter list                    # the providers this build carries and their maturity
heo adapter get smtp.mail           # configuration schema, credential purposes, permissions
heo channel create --type native.webwidget --key web \
  --configuration '{"sites":["site-1"]}' --credential-ref widget-key --dry-run
```

A channel is one configured connection; an inbox is a destination of it. The steps, with the
adapter-specific configuration, are in [channels](channels.md) and [adapters](adapters.md).

## 4. Ingress: what the provider must reach

Each adapter receives by connecting out to the provider, by listening itself (mail) or by a
webhook the provider calls. Only the last two need anything from your network.

| Adapter | Receives by | You must provide |
| --- | --- | --- |
| `native.webwidget` | webhook | a public `https` URL for the webhook listener, served under your site's own origin (heo sends no CORS headers) |
| `twilio.sms`, `twilio.whatsapp`, `meta.whatsapp`, `github.issues` | webhook | a public `https` URL; the provider calls it and heo checks its signature |
| `slack.chat` | webhook (request URL), or Socket Mode receiver (experimental; Slack allows one) | for the request URL, a public `https` URL, which Slack calls with signed requests; nothing for Socket Mode, connecting out |
| `telegram.messages` | long polling receiver, or webhook (Telegram allows one) | nothing for polling, connecting out; for a webhook, a public `https` URL on port 443, 80, 88 or 8443, which Telegram calls with your secret token |
| `relay.messages` | webhook, posted by your gateway | a URL your gateway reaches (public `https`, or on a private network); it signs each request. Replies: heo connects out, to a host in `adapters.relay.callback_hosts` |
| `discord.chat` | gateway receiver, connecting out over a WebSocket; webhook for interactions | nothing for the gateway; for the Interactions Endpoint URL, a public `https` URL, which Discord calls signed with your application's key |
| `microsoft.teams` | webhook (the Azure Bot's messaging endpoint) | a public `https` URL on port 443, which the Bot Connector calls with a signed token; heo connects out to `login.botframework.com`, `login.microsoftonline.com` and the Bot Connector |
| `zendesk.tickets` | polling receiver; webhook | nothing for polling; a public `https` URL for the webhook |
| `gitlab.issues`, `jira.issues`, `linear.issues` | webhook | a public `https` URL, which the provider calls with a secret token (GitLab) or a signature (Jira, Linear) |
| `google.mail`, `microsoft.mail` | receiver; webhook with push or change notifications | nothing for the receiver; a public `https` URL to turn push on |
| `imap.mail` | receiver, connecting out over IDLE | nothing inbound |
| `smtp.mail` | its own SMTP listener | an MX record for your domain at the listener's host, port 25, and a certificate for STARTTLS |

**Webhooks.** The webhook listener is off by default and binds loopback. Enable it, give it the
URL providers use, and put a TLS-terminating reverse proxy or a tunnel in front of it:

```yaml
services:
  webhook:
    enabled: true
    addr: 127.0.0.1:8083
    public_base_url: https://hooks.example.com/heo
```

A channel is served at `<public_base_url>/webhooks/<channel id>`; that is the URL you give the
provider. The listener answers `/healthz` and `/readyz` for a proxy's checks and exposes no
command. heo trusts no request: it verifies each by the provider's signature, so the webhook
secret is a credential purpose of the channel. See [Webhook listener](runtime.md#webhook-listener).

**Mail.** The `smtp.mail` listener accepts mail for the addresses a channel lists and is off
until `adapters.smtp.addr` is set. A non-loopback listener needs `adapters.smtp.allow_remote:
true` and STARTTLS: the certificate chain is a file, its private key is a credential:

```yaml
adapters:
  smtp:
    addr: 0.0.0.0:25
    allow_remote: true
    tls:
      cert_file: /etc/heo/mx.example.com.pem
      key_ref: mx-tls
```

Binding port 25 needs the privilege to do so, or a relay forwarding to a high port. `heo config
validate` reads the certificate, the key and the expiry. See
[Inbound mail](adapters.md#inbound-mail-smtpmail).

**Web widget.** The browser talks to the webhook routes of its channel; serve them from your
site's origin through the proxy. See [Web widget](adapters.md#web-widget-nativewebwidget).

## 5. Profiles

A profile is one daemon: its own configuration file, data store, control socket, credential
backend and tenant. `default` needs nothing; name another with `--profile` or `HEO_PROFILE`:

```sh
heo --profile work config set runtime.tenant acme
heo --profile work storage migrate
HEO_PROFILE=work heo service start
```

| Reference | Means |
| --- | --- |
| `work`, `heo://profile/work` | the heo profile `work` |
| `aps:noor`, `aps://profile/noor` | a heo profile bound to the existing aps profile `noor` |

An aps-bound profile reads channel credentials from that aps profile's `secrets.env` and keeps
its own state under `aps-noor`; binding grants no authority. See
[Profiles](configuration.md#profiles) and
[aps-bound profiles](configuration.md#aps-bound-profiles).

## 6. Run the daemon

```sh
heo service start                   # every enabled service, in the foreground
heo service status                  # in another terminal: services, readiness, heo sections
heo daemon ping                     # who the daemon serves you as
heo service restart                 # drain, re-read the configuration, run again
heo service stop                    # drain and wait for exit
```

`service start` runs in the foreground so an OS service manager (launchd, systemd) can own it;
SIGTERM drains it. Nothing starts a daemon for you: a command that needs one exits 70 and names
`heo service start`. With no channel configured the daemon starts and reports ready. See
[runtime](runtime.md).

## 7. Let other programs call it

The CLI reaches the daemon over the profile's control socket and needs no setup. To serve REST,
gRPC or MCP over HTTP, on loopback with the profile's bearer token:

```sh
heo service setup api mcp           # mint the token, enable the surfaces on loopback
heo service restart                 # the daemon applies surfaces at start
```

`heo service setup api --dry-run` shows the plan; `--show-token` prints the token once;
`--rotate-token` replaces it. For an agent that spawns heo itself, `heo service start mcp --stdio`
relays over the socket. See [surfaces](surfaces.md).

## Verify

```sh
heo config validate                 # offline checks; errors exit 2
heo service status
heo channel check web --remote      # the adapter, the credential and what the channel can do
```
