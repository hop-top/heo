# heo

Messaging CLI and daemon for uma: unified inbox, replies and recovery from the terminal.

[![Release](https://img.shields.io/github/v/release/hop-top/heo?include_prereleases)](https://github.com/hop-top/heo/releases)
[![Go reference](https://pkg.go.dev/badge/hop.top/heo.svg)](https://pkg.go.dev/hop.top/heo)
[![CI](https://github.com/hop-top/heo/actions/workflows/ci-go.yml/badge.svg)](https://github.com/hop-top/heo/actions/workflows/ci-go.yml)
[![12-factor AI-CLI](https://img.shields.io/endpoint?url=https%3A%2F%2Fraw.githubusercontent.com%2Fhop-top%2Fheo%2Fnext%2F.12fc.json)](docs/DEVELOPING.md#conformance)

> [!WARNING]
> Pre-1.0 and not yet released. Commands, flags, configuration keys and the data store schema
> can change between versions, and no release archive exists yet: build from source and pin the
> commit you run.

I want to **use** heo: start at [SKILL.md](SKILL.md) and the
[manual](docs/manual/getting-started.md).
I want to **modify** heo: start at [docs/DEVELOPING.md](docs/DEVELOPING.md).

## What this is, and is not

heo is the terminal and daemon front end of uma, the engine that keeps conversations, messages
and a delivery ledger across messaging providers. One daemon per profile owns the data store,
receives from your channels (mail, web widget, SMS and WhatsApp, GitHub issues, Zendesk tickets)
and delivers what you write. The CLI, and optionally REST, gRPC and MCP on loopback, drive that
daemon.

Perfect for:

- working a shared inbox from a terminal: read, reply, add internal notes, label, resolve;
- scripts and agents that need a typed, previewable, replayable interface to the same inbox:
  every verb declares its side effect, writes take `--dry-run` and `--idempotency-key`, and an
  answer lost in transit is recovered by key;
- recovering a send whose outcome is unknown, from `heo delivery` and `heo operation`.

Not for:

- a hosted service or a helpdesk UI: heo is a local daemon and a CLI, and serves loopback only in
  this release;
- a mail server for general use: the `smtp.mail` listener accepts mail for the addresses you
  list, nothing more.

## Install

No release is published yet. Build from source (Go 1.27.3, see `mise.toml`):

```sh
git clone https://github.com/hop-top/heo
cd heo
make build          # writes bin/heo
make symlink        # optional: link bin/heo into a user bin directory on PATH
heo --version
```

heo stores everything in PostgreSQL 15 or later. See the [install guide](docs/manual/install.md)
for the database, a provider, credentials and the URLs providers must reach.

## Quick start

With PostgreSQL running and a database for heo:

```sh
heo config set storage.postgres.dsn 'host=localhost dbname=heo user=heo'
heo storage migrate                     # create the schema
heo service start                       # the daemon, in the foreground
```

In another terminal:

```sh
heo service status                      # every service ready?
heo adapter list                        # the providers this build carries
heo channel list
heo conversation list --status open
heo message list --conversation conv_123
heo message reply conv_123 --text "Thanks, we're on it." --dry-run
heo message reply conv_123 --text "Thanks, we're on it."
heo delivery get op_123                 # did it leave
heo service stop                        # drain and exit
```

Every ID is a record ID, as the listing printed it. The [getting started
guide](docs/manual/getting-started.md) walks through the same path with a channel of your own.

## Features

- Conversations, messages, internal notes, attachments, labels, contacts and identities, on one
  grammar: `heo <resource> <verb>`. The [command reference](docs/manual/command-reference.md)
  lists every verb with its side effect, who may run it and the uma call behind it.
- LLM helpers: `conversation summarize`, `conversation triage` and `message draft` ask a model
  you run locally by default (cloud only when you opt in, per call); they suggest and never act.
  See [LLM helpers](docs/manual/llm-helpers.md).
- Previewable writes: `--dry-run` shows the exact effect and writes nothing; `--idempotency-key`
  makes a retry replay instead of repeat.
- Approvals: a plugin actor's write can be held until a person approves it, with a trust ladder,
  an audit trail and chat approval. See [approvals](docs/manual/approvals.md).
- Recovery: `heo operation get --key`, `heo delivery reconcile` and `heo delivery retry` settle
  a write whose outcome is unknown.
- Provider verbs gated by what the channel's adapter declares: edit, delete, react, answer a
  button press or command, close an issue, change a ticket.
- Profiles: one daemon, data store, socket and credential backend per profile; bind one to an aps
  profile with `--profile aps:<id>`.
- Surfaces: the control socket always; REST, gRPC and MCP on loopback with a bearer token after
  `heo service setup`.
- Roles: operator, desk, viewer and external, denied by default.
- Agent friendly: a machine-readable manifest (`heo spec`), structured errors with a
  `suggested_fix`, graded against the 12-factor AI-CLI conformance spec (the badge above).

## Development

```sh
make build
make check          # lint, generated docs, tests, spec coverage, links, windows build
make docs-gen       # regenerate the command reference and cheatsheets
```

See [docs/DEVELOPING.md](docs/DEVELOPING.md) for the tests, cassettes, conformance and the
generated docs, and [CONTRIBUTING.md](CONTRIBUTING.md) for how to send a change.

## License

[MIT](LICENSE).
