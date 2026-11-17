# Developing heo

## Tests

```sh
make test     # go test ./...  (replays cassettes; never records)
make check    # lint, docs-check, test, spec, links, windows build
```

## Generated docs

`docs/manual/command-reference.md`, `docs/cheatsheet-agent.md` and
`docs/cheatsheet-human.md` are generated from `heo spec` and the command
tree by `internal/refdoc`. Never edit them:

```sh
make docs-gen     # rewrite the three files
make docs-check   # fail when the committed files are stale (about 1 s)
```

What the manifest does not say, who may run a verb and which uma call
backs it, lives in one table, `internal/refdoc/meta.go`, beside `Gaps`
(the verbs the grammar names and this build lacks, each with its reason)
and `Catalog` (every verb the grammar names). `cmd/refdoc_test.go` pins
the table to the tree both ways: a new verb fails until it has a row, a
row for a removed verb fails, a gap for a verb that now ships fails, and
a catalog verb that is neither in the tree nor a gap fails. A new verb
therefore needs its row, then `make docs-gen`.

### Docs lint

Every command the Markdown shows is real. `cmd/docs_lint_test.go` (in
`make test`) reads each `heo ...` line of the fenced `sh`/`bash` blocks and
each `heo ...` code span in the repository's Markdown and holds it to the
command tree:

- a concrete invocation must resolve to a runnable leaf with flags it
  declares, an argument count its validator accepts and its required flags;
- a synopsis (`[--limit N]`, `a | b`, `...`) and a code span are held to the
  verb and the flags they name;
- the generated files are skipped (the guidance tests already parse the
  examples they are rendered from), and so are `heo --version` and
  `--help` on the root.

The same file fails on an internal reference in the Markdown (task or track
IDs, local paths, internal stores, tool or agent names) and on a prose line
over 100 columns (table rows: 256) in the files the docs set owns. Show
output in a `text` block: only `sh`, `bash`, `shell`, `zsh` and `console`
blocks are read as commands.

The process-level tests in `tests/e2e` build the heo binary and drive it
as a child process: exit codes, streams, files, signals. The tests that
need a store run against a Postgres container
(testcontainers); `HEO_REQUIRE_DOCKER=1` makes a missing container
runtime fail them instead of skipping them, as CI does.

Two builds are under test. The release build is what ships. The e2e build
(`-tags heoe2e`, built by the tests themselves) adds only what a test
needs to stand in for the outside world: scriptable fake adapters, a
destructive and a whoami command, and the cassette seam below. Nothing
tagged `heoe2e` reaches a release binary.

## Cassettes

heo calls out of its process in a few places. In the e2e build those
calls go through [xrr](https://pkg.go.dev/hop.top/xrr) cassettes, so a
test can replay an interaction that was recorded once, with no provider
and no tool involved.

| Seam | Where | In the e2e build |
| --- | --- | --- |
| Provider HTTP (every HTTP adapter's client) | `composition.NewHTTPClient` | recorded and replayed through uma's `xrrtest` transport |
| LLM helpers' model calls (`summarize`, `triage`, `draft`) | kit's llm adapters, which take no HTTP client and use `http.DefaultClient` | recorded and replayed: the e2e build routes the requests marked by `assist.WithCall`, and only those, through the same xrr transport (`llmSeam` in `cmd/xrr_e2e.go`) |
| `pg_dump`, `pg_restore` | `postgres.Runner` | recorded and replayed through xrr's exec adapter |
| `heo upgrade` release feed and download | kit's upgrade client | live, against a feed the test owns (`upgrade_test.go`); the client cannot be injected |
| Postgres wire protocol | pgx | live, against the test container |
| IMAP, SMTP | the mail adapters' own sockets | live: xrr has no channel for them |
| `api.github.com` | `github.issues`, through the same provider HTTP client | refused, except in a live recording of the private sandbox |
| `slack.com` Web API, `hooks.slack.com` | `slack.chat`, through the same provider HTTP client | `slack.com` refused, except in a live recording of the throwaway channel; `hooks.slack.com` always refused |
| `api.telegram.org` Bot API | `telegram.messages`, through the same provider HTTP client | refused, except in a live recording of the one chat |
| iMessage backends (`api.blooio.com`, a BlueBubbles server) | `relay.messages`'s gateway, which is the operator's own and not part of the daemon: uma's reference gateway runs as a process of the test, behind a proxy of the test's | recorded and replayed at that proxy; refused, except in a live recording to the one handle |
| `discord.com` REST API, Discord's gateway | `discord.chat`, through the same provider HTTP client | `discord.com` refused, except in a live recording of the one channel; the gateway (`*.discord.gg`) always refused |
| Bot Connector service (`smba.trafficmanager.net`, its regional and sovereign-cloud siblings) | `microsoft.teams`, through the same provider HTTP client | the recorded process is told a placeholder service host that resolves nowhere: refused, except in a live recording of the one personal chat; the Bot Framework's key set and Microsoft Entra's token endpoint are never reached |
| Credential backends (keyring, file, agefile, openbao) | kit's secret stores | live; heo starts no credential command, so there is no exec seam to record |

The wiring is `cmd/xrr_e2e.go` (tag `heoe2e`) over `internal/xrrx`. A
release build links neither: `internal/xrrx/graph_test.go` fails when
`go list -deps` of the release binary names `hop.top/xrr`,
`hop.top/uma/testing/xrrtest` or `internal/xrrx`, and when the e2e build
fails to link them (so the check cannot pass by not looking).

### Replay is the default

A parent test starts the child with `XRR_MODE` and `XRR_CASSETTE_DIR`
(`sandbox.xrrEnv(proc)` in `tests/e2e/xrr_test.go`). Each child process
gets its own directory,
`tests/e2e/testdata/cassettes/<TestName>/<process>/`, so two processes
never share a cassette and the same command in two of them cannot
collide.

- Replay never reaches the network and never starts the tool. A request
  with no cassette fails: the child returns an error, logs it to the
  test's miss log, and the parent fails the test at its end.
- A replay with no cassette directory fails at once and says how to
  record.
- Tests that need a provider in record mode start a stand-in the test
  owns; in replay the address is a closed port, so anything not answered
  from a cassette fails instead of reaching something.

### Record

```sh
make record                      # needs Docker; runs the TestXRR* tests
git diff tests/e2e/testdata      # read it
make test                        # replays what you recorded
```

`XRR_MODE=record` is refused when `CI` is set, and when
`XRR_REDACT_DISABLE` or `XRR_REDACT_ALLOW` is, which would let a
credential into a cassette. `make record` records only against fixtures
the test owns: a local HTTP stand-in, a local script. The ones
that reach a provider are `github.issues`, `twilio.sms`, `slack.chat`,
`telegram.messages`, the iMessage gateway path, `discord.chat` and
`microsoft.teams`, each behind its own flag and target ([Recording GitHub
issues](#recording-github-issues), [Recording Twilio
SMS](#recording-twilio-sms), [Recording Slack](#recording-slack), [Recording
Telegram](#recording-telegram), [Recording iMessage](#recording-imessage),
[Recording Discord](#recording-discord), [Recording Microsoft
Teams](#recording-microsoft-teams)); `make record` skips all of them.

The exception is a model served on this machine: `make record-llm` records
the LLM helpers against one (see [LLM cassettes](#llm-cassettes)). Never a
provider or model that is not on this machine.

Recording clears the cassette directory of each process it runs first,
then audits what it wrote. Re-record when the request a test makes
changes on purpose, not as routine maintenance: a cassette replays what
was recorded, not what the provider does today.

### What a cassette keeps

The scrub rules live in `internal/xrrx/scrub.go`; `xrrx.Audit` checks
their result and runs over every committed cassette in `go test ./...`
(`TestCommittedCassettesAreClean`) and over every fresh recording.

| Kept out | How |
| --- | --- |
| `Authorization`, `Cookie`, `Set-Cookie`, API-key headers | xrrtest records an allow-list of headers (`Content-Type`, `Location`, `Retry-After`, `X-RateLimit-*`) and drops credential names even when listed |
| Credential fields and tokens in JSON and form bodies and in query parameters | xrrtest scrubs them field by field to `<redacted:NAME>`; xrr's redactor is the backstop |
| Loopback `host:port` (the OS chose the port) | `loopback.invalid`, in the URL, bodies and response headers |
| Temp paths | `<tmp>` |
| Timestamps and `pid=` in a request | `<time>`, `pid=<pid>`: they would change the fingerprint on every run |
| The tool's environment (`PGPASSWORD`) | never recorded; xrr's redactor does not catch it |
| The connection string of `pg_dump` and `pg_restore` | `--dbname=<dsn>`; a run is keyed by tool name, arguments and a digest of stdin |

| The operation and attempt in the echo marker uma appends to a comment | `uma:<operation>:<attempt>`: new on every run, they would change the fingerprint |
| GitHub: the sandbox repository, the account's login and ID, other users, node IDs, avatars, emails, the per-user URLs | `owner/sandbox`, `sandbox-account` (ID 9100001), `sandbox-user`, `NODE_ID`; a user object keeps only its login, ID, type and two links ([Recording GitHub issues](#recording-github-issues)) |
| GitHub: `X-RateLimit-*` headers | dropped: they change on every call |
| Twilio: the account SID, every SID (`SM`, `MM`, `PN`...), the sending and receiving numbers, dates, billing fields, request-ID headers | `AC...01`, `SM...01` in order of appearance, `+15550100001` (sender), `+15550100002` (recipient), a fixed date, `USD`, dropped ([Recording Twilio SMS](#recording-twilio-sms)) |
| Twilio: the operation and attempt in a status callback URL | `operation-id`, `attempt-id`: they would change the fingerprint |
| Slack: the workspace, the bot user, the bot, the channel, the app, every other user | `T0000000001`, `U0000000001`, `B0000000001`, `C0000000001`, `A0000000001`, `U0000000010` on, in order of appearance ([Recording Slack](#recording-slack)) |
| Slack: the app's profile, the workspace's names, generated block IDs, permalinks, other people's messages | dropped |
| Slack: the operation and attempt in the attempt metadata of a post | `operation-id`, `attempt-id`: they would change the fingerprint |
| Telegram: the bot, the chat, every other user | `7000000001`, `-1000000000001`, `5000000010` on (negative from `-1000000000100` for another chat), in order of appearance ([Recording Telegram](#recording-telegram)) |
| Telegram: the bot's username, every name, title and language, the token in a request path | `heo_adapter_test_bot`; names dropped; a stand-in token |
| Discord: the application, the bot user, the server, the channel, every other user | `900000000000000101`, `...102`, `...103`, `...104`, `...200` on, in order of appearance ([Recording Discord](#recording-discord)) |
| Discord: the bot's username, every name, avatar and member, the token in the `Authorization` header | `heo-adapter-test-bot`; dropped; a stand-in token |
| Discord: the nonce of a message | `<nonce>`: it derives from an operation ID that is new on every run |
| Teams: the bot's app and tenant, the service host and region, the user, the personal chat, every other account, conversation and GUID | `00000000-0000-4000-8000-0000000000a1` (bot, as `28:<it>`), `...a2` (tenant), `smba.heo-adapter-test.invalid/teams/`, `29:heo-adapter-test-user`, `a:heo-adapter-test-chat`, `29:heo-adapter-test-other-N`, in order of appearance ([Recording Microsoft Teams](#recording-microsoft-teams)) |
| Teams: every name, email and object ID | dropped; the Bot Connector token is a stand-in |

What stays: response timestamps (the adapter parses them), issue and
comment IDs (the next request names them) and xrr's own `recorded_at`. A secret a provider echoes inside a string field with no
credential shape is not recognised, so read the diff.

A tool's stdout that holds a credential shape is replaced by xrr with
`<redacted:STDOUT>`; replay refuses it rather than hand the code that
placeholder as the tool's output.

### Recording GitHub issues

`TestXRRGitHubIssuesProviderVerbs` runs the real binary against
`github.issues`: reply, edit, delete, react, unreact, close, reopen and
close as not planned. In replay it needs nothing: the daemon answers from
`testdata/cassettes/TestXRRGitHubIssuesProviderVerbs/daemon` and the
webhook payloads that feed it come from `testdata/github`. Recording is the
only step that reaches GitHub, and it is deliberately narrow.

```sh
export HEO_RECORD_GITHUB_REPO=<owner>/heo-adapter-sandbox
export HEO_RECORD_GITHUB_TOKEN=<token>
make record-github
git diff tests/e2e/testdata       # read every byte, then
make test
```

- **The sandbox** is a repository you own and keep **private**, holding
  nothing but these test issues: `gh repo create <owner>/heo-adapter-sandbox
  --private`. The recording refuses to write unless the repository answers
  `private: true`, and asks again before every write
  (`TestSandboxTransportAssertsPrivateBeforeEveryWrite`).
- **The token** needs only issues read and write on that repository (a
  fine-grained personal access token limited to it). Put it in the
  environment of the recording process only. It reaches heo through the
  credential store of a throwaway profile under the test's temp directory;
  it is never a command-line argument, a file in the repository, or in a
  cassette.
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with
  `CI` set. `make record` does not set the second, so it skips this test.
- **What it reaches**: `https://api.github.com/repos/<repo>/issues...`
  and nothing else (issues, comments, reactions of that one repository).
  Any other host or path fails before it is sent.
- **What it creates**: one issue per recording, one comment on it, and
  the comments heo posts. The test deletes every comment on the issue and
  closes the issue when it ends; issues cannot be deleted through the
  REST API, so closed ones accumulate in the sandbox.
- **Inbound is not recorded**: GitHub reaches heo by webhook, and no
  public webhook URL is exposed. The test builds each webhook payload from
  the real issue or comment GitHub just returned, scrubbed by the same
  rules as the cassettes, signs it and posts it to the daemon's loopback
  listener. The fixtures are rewritten on every recording.
- **After each change** the test reads the sandbox back (comments,
  reactions, the issue's state and reason) and fails if GitHub did not do
  what heo reported.

The scrub (`internal/xrrx/github.go`) runs on every answer before it
reaches a cassette, a fixture or the adapter, and an answer that still
holds the repository, the login, an avatar or a token after it is not
handed on. `xrrx.Audit` runs over `tests/e2e/testdata`, cassettes and
fixtures, in every `go test ./...` and denies: GitHub tokens of every
shape (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), the
sandbox account and repository names, avatar and gravatar URLs, node IDs
and user logins that are not the placeholders, and email addresses. A
recording also denies the real login and repository it just used. The
audit is a safety net: read the diff.

### Recording Twilio SMS

`TestXRRTwilioSMSProviderVerbs` runs the real binary against `twilio.sms`:
a text to a number too short to exist, a send that starts a conversation,
a signed incoming text, a reply and a signed status callback. In replay it
needs nothing: the daemon answers from
`testdata/cassettes/TestXRRTwilioSMSProviderVerbs/daemon`, and the incoming
text and the callback come from `testdata/twilio`. Recording is the only
step that reaches Twilio, and it **sends real texts**, so it is the
narrowest recording in the repository.

```sh
export HEO_RECORD_TWILIO_ACCOUNT_SID=<account SID>    # from your secret manager,
export HEO_RECORD_TWILIO_AUTH_TOKEN=<auth token>      # never a file or an argument
export HEO_RECORD_TWILIO_TO=<your own number, E.164>
make record-twilio
git diff tests/e2e/testdata       # read every byte, then
make test
```

- **The account** is one you own. It is live, not a test account: every
  text bills and reaches a phone. The sending number is read once from the
  account's incoming numbers (`HEO_RECORD_TWILIO_FROM` names it when the
  account has more than one that can send SMS).
- **The recipient** is `HEO_RECORD_TWILIO_TO`, a phone you own, and no
  one else's: the transport refuses any other number before the request is
  built. The number is never in the repository, a cassette or a fixture.
- **The cap**: at most `HEO_RECORD_TWILIO_MAX_SENDS` (default 2) texts are
  ever sent, counted in a ledger directory (`HEO_RECORD_TWILIO_LEDGER`,
  default a `heo/twilio-record-sends` directory in the user cache) that
  outlives the run. A send takes its slot before the request leaves, so a
  failed run counts. The next one is refused with the cap in the message;
  deleting the ledger is a deliberate act of the account's owner. The test
  sends two, so one recording uses the whole cap.
- **The text** starts with the words "heo adapter test", is short, and carries no link,
  media or status callback: the callback URL heo sets is removed from the
  request Twilio receives (it names a URL that does not exist).
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with
  `CI` set. `make record` sets neither, so it skips this test.
- **What it reaches**: `https://api.twilio.com` and, under the account,
  `POST Messages.json` (counted), `GET Messages.json`, `GET Messages/<sid>`
  and, once, `GET IncomingPhoneNumbers.json`. Everything else is refused
  before it is sent: accounts, keys, numbers and their webhooks, calls,
  lookups, usage and balance, deletes. Nothing on the account or its numbers
  is changed.
- **A number too short to exist** (`+1555`) is the free error recording:
  Twilio answers 400 (error 21265) and creates nothing. If Twilio ever
  accepted one, it would count as a send and its answer would not be
  handed on.
- **The credential**: the auth token reaches the process that sends, as
  an environment variable, and is put in the `Authorization` header by the
  transport only. heo's own credential store holds a stand-in token, so the
  real one is in no file, signs nothing and is never in the daemon's
  configuration. The daemon is configured with the placeholder account and
  numbers; the transport swaps them for the real ones on the way out and
  back on the way in.
- **Inbound is not recorded**: Twilio reaches heo by webhook and no public
  URL is exposed. The incoming text and the status callback are fixtures
  (`testdata/twilio/*.form`, the form fields Twilio documents, with
  placeholder numbers and SIDs), stored **unsigned**. The test signs each
  with the stand-in token when it runs, so no committed file holds a
  signature; the audit fails one that does. A status callback that names
  no attempt settles nothing, and no recorded send can name one: the
  attempt's ID is new on every run.
- **Read back**: after the two sends the test fetches both messages from
  the account and fails unless each is the expected text from the sender to
  the recipient. Delivery to the phone is not asserted: a carrier may
  filter it.
- **Rehearsal**: `HEO_RECORD_TWILIO_REHEARSE=1` with made-up values for
  the account, token, sender and recipient runs the same test against
  canned answers: nothing is sent and nothing is counted. Use it to debug a
  change before spending a send. Its cassettes are worthless; the next
  recording clears them.

The scrub (`internal/xrrx/twilio.go`) runs on every answer before it
reaches a cassette or the adapter, and an answer that still holds a SID, a
number other than the placeholders, the account's digits or the token is
not handed on. Errors name no real value either. `xrrx.Audit` runs over
cassettes and fixtures in every `go test ./...` and denies: SIDs and
32-hex tokens that are not placeholders, any E.164 number that is not one
of the two placeholders, North American numbers written with separators
(555-0100..0199 aside), the account's names, Basic credentials and webhook
signatures. Ten bare digits are left alone, since IDs have that shape; a
recording denies the digits of the numbers it used instead. The audit is a
safety net: read the diff.

### Recording Slack

`TestXRRSlackChatProviderVerbs` runs the real binary against `slack.chat`:
a first message to a room, a reply, a signed incoming message, a signed
button press, an edit, a reaction added and removed, and two deletions.
In replay it needs nothing: the daemon answers from
`testdata/cassettes/TestXRRSlackChatProviderVerbs/daemon`, and the incoming
message and the button press come from `testdata/slack`. **No cassette is
committed yet**: with none, the test skips and says so, and the adapter's
verbs are covered in process by `TestSlackProviderVerbs` and
`TestSlackWebhook`, which answer from a scripted Web API and need no
workspace. Recording is the only step that reaches Slack, and it **posts
real messages**.

```sh
export HEO_RECORD_SLACK_BOT_TOKEN=<xoxb- token>      # from your secret manager,
export HEO_RECORD_SLACK_TEAM_ID=<T...>               # never a file or an argument
export HEO_RECORD_SLACK_BOT_USER_ID=<U...>
export HEO_RECORD_SLACK_CHANNEL_ID=<C... of a throwaway channel>
export HEO_RECORD_SLACK_BOT_ID=<B...>                # optional
make record-slack
git diff tests/e2e/testdata       # read every byte, then
make test
```

- **The workspace** is one you own and keep free of other people's data: a
  [developer sandbox](https://docs.slack.dev/tools/developer-sandboxes/) or a
  workspace of your own. Create an app (a blank app), give its bot the
  scopes `chat:write`, `reactions:read`, `reactions:write` and
  `channels:history` (`groups:history` for a private channel), install it,
  and copy the bot token. Create a channel only for this, and invite the app
  to it: the read-back needs the bot to be a member. The workspace and bot
  user IDs are in `auth.test`'s answer for the token.
- **The channel** is the one place the recording writes and reads. Its ID
  must start `C` or `G`: a direct message ID is refused. Archive the
  channel afterwards, and revoke the token or uninstall the app.
- **The cap**: at most `HEO_RECORD_SLACK_MAX_POSTS` (default 3)
  `chat.postMessage` requests are ever made, counted in a ledger directory
  (`HEO_RECORD_SLACK_LEDGER`, default a `heo/slack-record-posts` directory in
  the user cache) that outlives the run. A post takes its slot before the
  request leaves, so a failed run counts. The next one is refused with the
  cap in the message; deleting the ledger is a deliberate act of the
  workspace's owner. The test posts two messages. The third slot is for the
  one post Slack's refusal of the attempt metadata makes the adapter repeat
  without it.
- **The text** starts with the words "heo adapter test", is short, and
  carries no link, block or attachment, so it mentions no one.
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with
  `CI` set. `make record` sets neither, so it skips this test.
- **What it reaches**: `https://slack.com/api/` and these methods only,
  on that channel: `chat.postMessage` (counted), `chat.update` and
  `chat.delete` and the reactions methods on messages this recording posted,
  and `conversations.history` and `conversations.replies` (at most 20
  messages). Everything else is refused before it is sent: opening a direct
  message, users, files, auth, every admin method. `hooks.slack.com` is never
  reached: a response URL answers once, to the person who pressed the
  control, so the test previews `interaction respond` and does not send it.
- **Modals are not in this recording.** `interaction open-modal`,
  `push-modal` and `update-modal` run in process (`TestSlackModalVerbs`,
  `TestSlackModalBudget`) against the scripted Web API, and their built-binary
  refusals and queueing on a fake provider (`TestModalLifecycle`,
  `TestModalRefusals`). `views.open`, `views.push` and `views.update` are not
  among the methods above, so none of it has reached Slack. Recording them
  needs what a person alone can give: a workspace you own, an app with
  Interactivity switched on and its request URL reaching the recorded daemon
  (`public_base_url`), the app's signing secret, and someone pressing a button
  of the app's message, then the verb run inside the three seconds Slack gives
  the `trigger_id`. The allow-list gains the three methods only once that
  recording is made, and the recording must name the one channel's press, never
  a workspace's.
- **The credential**: the bot token reaches the process that sends, as an
  environment variable, and is put in the `Authorization` header by the
  transport only. heo's own credential store holds a stand-in for it and for
  the signing secret, so the real token is in no file, signs nothing and is
  never in the daemon's configuration. The daemon is configured with the
  placeholder workspace, bot and channel; the transport swaps them for the
  real ones on the way out and back on the way in.
- **Inbound is not recorded**: Slack reaches heo by webhook and no public URL
  is exposed. The incoming message and the button press are fixtures
  (`testdata/slack/*.json`, the shapes Slack documents, with placeholder IDs),
  stored **unsigned**. The test signs each with the stand-in secret when it
  runs; the audit fails a committed signature.
- **Read back**: after each change the test reads the channel's messages and
  fails unless they are exactly what heo reported: the texts, the edit, the
  reaction, and at the end nothing.
- **Rehearsal**: `HEO_RECORD_SLACK_REHEARSE=1` with made-up values for the
  token (`xoxb-...`), workspace, bot and channel runs the same test against
  an in-memory Slack: nothing is sent and nothing is counted. Use it to
  debug a change before spending a post. Its cassettes are worthless: delete
  `tests/e2e/testdata/cassettes/TestXRRSlackChatProviderVerbs` before
  committing, or record over them.

The scrub (`internal/xrrx/slack.go`) runs on every answer before it
reaches a cassette or the adapter, and an answer that still holds the
token, an ID that is not a placeholder, or a workspace's host is not handed
on. `xrrx.Audit` runs over cassettes and fixtures in every `go test ./...`
and denies: Slack tokens of every kind, IDs that are not placeholders,
request signatures, response and webhook URLs with a secret segment, and
workspace hosts; a recording also denies the literals it used. The audit
is a safety net: read the diff. What it cannot know: the `ts` of a message
is its time and stays, since the next request names it.

### Recording Telegram

`TestXRRTelegramMessagesProviderVerbs` runs the real binary against
`telegram.messages`: a first message to a chat, a reply, a signed incoming
message, button press and command, an edit, a reaction set and cleared, and
two deletions. In replay it needs nothing: the daemon answers from
`testdata/cassettes/TestXRRTelegramMessagesProviderVerbs/daemon`, and the
update fixtures come from `testdata/telegram`. **No cassette is committed
yet**: with none, the test skips and says so, and the adapter's verbs are
covered in process by `TestTelegramProviderVerbs`, `TestTelegramWebhook` and
`TestTelegramPolling`, which answer from a scripted Bot API and need no bot.
Recording is the only step that reaches Telegram, and it **sends real
messages**.

```sh
export HEO_RECORD_TELEGRAM_BOT_TOKEN=<token>        # from your secret manager,
export HEO_RECORD_TELEGRAM_BOT_USERNAME=<username>  # never a file or an argument
export HEO_RECORD_TELEGRAM_CHAT_ID=<chat ID>
make record-telegram
git diff tests/e2e/testdata       # read every byte, then
make test
```

- **The bot** is one you own and made for this: in Telegram, open
  [@BotFather](https://t.me/BotFather), send `/newbot`, and copy the token
  (`<bot ID>:<secret>`). It needs no administrator right: it edits, deletes
  and reacts to its own messages. Keep its webhook unset; the recording
  never sets or deletes one.
- **The chat** is the one place the recording writes: a throwaway group the
  bot is in, or your own private chat with the bot (send it `/start`
  first, since a bot cannot write to a person who never started it). Its
  ID is a number, never a channel's `@username`; the bot's own ID is
  refused. A group's ID is negative (`-100...` once it is a supergroup):
  send `/start@<username>` in the group, then read the ID from
  `getUpdates` with no webhook set. Delete the group afterwards, and
  revoke the token with BotFather (`/revoke`) or delete the bot.
- **The cap**: at most `HEO_RECORD_TELEGRAM_MAX_SENDS` (default 3)
  `sendMessage` requests are ever made, counted in a ledger directory
  (`HEO_RECORD_TELEGRAM_LEDGER`, default a `heo/telegram-record-sends`
  directory in the user cache) that outlives the run. A send takes its slot
  before the request leaves, so a failed run counts. The next one is
  refused with the cap in the message; deleting the ledger is a deliberate
  act of the bot's owner. The test sends two messages; the third slot is
  spare for a run that failed midway.
- **The text** starts with the words "heo adapter test", is short, and
  carries no link, mention, parse mode, keyboard or topic, so it mentions
  no one.
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with
  `CI` set. `make record` sets neither, so it skips this test.
- **What it reaches**: `https://api.telegram.org/` and these methods only,
  on that chat: `sendMessage` (counted), and `editMessageText`,
  `deleteMessage` and `setMessageReaction` on messages this recording sent.
  Everything else is refused before it is sent: `getUpdates` (it would take
  the bot's queue), `getMe`, every webhook method, files, chat
  administration, and `answerCallbackQuery`, which only a person pressing a
  button can make: the test previews `interaction respond` and does not send
  it. The Bot API has no call that reads past messages, so unlike Slack
  there is no read-back: the test checks the operations, and the cassettes
  hold what Telegram answered.
- **The credential**: the bot token reaches the process that sends, as an
  environment variable, and is put in the request path (Telegram takes it
  there, `bot<token>/<method>`) by the transport only. heo's own credential
  store holds a stand-in for it and for the webhook secret token, so the real
  token is in no file and never in the daemon's configuration. The daemon is
  configured with the placeholder bot and chat; the transport swaps them for
  the real ones on the way out and back on the way in. A transport error
  that spells out the request URL has the token replaced. Do not run a proxy
  or a logging HTTP client on the recording that writes URLs to disk.
- **Inbound is not recorded**: Telegram reaches heo by webhook or by long
  polling, and the recording daemon runs no receiver (`services.receivers`
  is off): a poll would take the bot's updates. The incoming message, the
  button press and the command are fixtures
  (`testdata/telegram/*.json`, the shapes the Bot API documents, with
  placeholder IDs). The test posts each with the stand-in secret token in
  `X-Telegram-Bot-Api-Secret-Token`; the audit fails a committed header.
- **Rehearsal**: `HEO_RECORD_TELEGRAM_REHEARSE=1` with made-up values for the
  token (`<digits>:<30 or more letters, digits, _ or ->`), username and chat
  runs the same test against an in-memory Bot API: nothing is sent and
  nothing is counted. Use it to debug a change before spending a send. Its
  cassettes are worthless: delete
  `tests/e2e/testdata/cassettes/TestXRRTelegramMessagesProviderVerbs`
  before committing, or record over them.

The scrub (`internal/xrrx/telegram.go`) runs on every answer before it
reaches a cassette or the adapter, and an answer that still holds the token,
the bot's or the chat's number, the username or a name is not handed on. A
user or chat object keeps only its ID, kind and the bot's own username; the
Bot API's error answers are Telegram's, which is the point of recording
them: the adapter reads a few of their phrases, and the Bot API documents
none. `xrrx.Audit` runs over cassettes and fixtures in every `go test
./...` and denies: a bot token other than the stand-in, the secret token
header, `first_name`, `last_name` and `language_code` keys, a username that
is not the placeholder, and an ID that is not one; a recording also denies
the literals it used. The audit is a safety net: read the diff. What it
cannot know: a message's `date` is its time and stays, and so does its
`message_id`, which the next request names.

### Recording iMessage

`relay.messages` is the operator's own relay, so heo cannot record the adapter
itself. What it records is the path through uma's reference iMessage gateway
(`cmd/imessagegateway` in the uma repository, in front of Blooio or of
BlueBubbles Server on a Mac): heo sends one first message to a handle you own,
the gateway sends it through the backend, the same callback repeated and the
lookup are answered from the gateway's key log without a second send, and your
reply comes back through the backend's webhook into the same conversation.
`TestXRRIMessageBlooio` and `TestXRRIMessageBlueBubbles` run it with the real
heo binary. In replay they need nothing: the gateway's requests to the backend
are answered from `testdata/cassettes/<TestName>/gateway`, and your reply is the
webhook in `testdata/imessage/<backend>-reply.json`. **No cassette is committed
yet**: with none, each test skips and says so, and the adapter is covered in
process by `TestRelayWebhook`, `TestRelaySend`, `TestRelayLostAnswerIsLookedUp`
and `TestRelayWithoutLookupStaysUnknown`, and over the release binary by
`TestRelayIMessageThroughTheBinary`, which answer from a gateway of the tests'
own and need no backend. Recording is the only step that reaches a backend, and
it **sends a real iMessage**.

The tests run the reference gateway as a process, built from the uma checkout
heo is built against (`go list -m hop.top/uma`; the checkout is only read and
the binary is written beside the test binary), with its backend address set to
a proxy of the test's. The proxy makes each request again to the real backend
through uma's `xrrtest` transport, which writes the cassette, behind the
sandbox of `internal/xrrx`. heo and the gateway talk over loopback; nothing
between them is recorded. heo's own credential store holds stand-ins for the
channel's webhook and callback secrets; only the gateway process is given the
backend's credentials.

```sh
export HEO_RECORD_IMESSAGE_BACKEND=blooio            # or bluebubbles
export HEO_RECORD_IMESSAGE_TO=<+number or email>     # a handle you own
export HEO_RECORD_IMESSAGE_LISTEN=127.0.0.1:8787     # where the backend's webhook reaches
export HEO_RECORD_IMESSAGE_BLOOIO_API_KEY=<key>      # from your secret manager,
export HEO_RECORD_IMESSAGE_BLOOIO_FROM=<+line>       # never a file or an argument
export HEO_RECORD_IMESSAGE_BLOOIO_WEBHOOK_SECRET=whsec_<...>
make record-imessage              # reply from the recipient's device when it asks
git diff tests/e2e/testdata       # read every byte, then
make test
```

For BlueBubbles set `HEO_RECORD_IMESSAGE_BLUEBUBBLES_URL`, `_PASSWORD`,
`_ACCOUNT` (the Apple ID the Mac sends from) and `_WEBHOOK_TOKEN` instead.
Setting up the backend (a Blooio line and webhook behind a tunnel; BlueBubbles
Server with Full Disk Access and Messages automation, its webhook at
`http://<HEO_RECORD_IMESSAGE_LISTEN>/webhook/<token>`) is the same as for uma's
own recording, which its guide `docs/recording/imessage.md` walks through, with
its clean-up. Record one backend at a time: `make record-imessage` runs the test
of `HEO_RECORD_IMESSAGE_BACKEND`.

- **The handle** is the one place the recording writes: a phone number in
  E.164 or an Apple ID's email address, on a device you hold, to reply from. It
  is never the sending line or account itself. Apple restricts automated and
  unsolicited messages from an Apple ID (see the operator note in
  [adapters.md](manual/adapters.md#relay-gateway-relaymessages)): one recording
  is one message and one reply.
- **The reply** is typed by you, from that handle's device, exactly `heo
  adapter test reply`, once, when the test logs that it waits (up to three
  minutes). The test keeps the webhook the gateway took, rewrites the recorded
  handle, line or account to its replay values (`+15550100001`, `+15550100002`,
  `uma-test@example.com`), blanks the names it carries and indents it for
  review.
- **The cap**: at most `HEO_RECORD_IMESSAGE_MAX_SENDS` (default 2) sends are
  ever made, counted in a ledger directory (`HEO_RECORD_IMESSAGE_LEDGER`,
  default a `heo/imessage-record-sends` directory in the user cache) that
  outlives the run. A send takes its slot before the request leaves, so a
  failed run counts; the next one is refused with the cap in the message, and
  deleting the ledger is a deliberate act of the backend's owner. The test
  sends one message; the second slot is spare for a run that failed midway.
- **The text** starts with the words "heo adapter test", is one short line and
  carries no link.
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with `CI`
  set. `make record` sets neither, so it skips these tests.
- **What it reaches**: the backend's send call only (Blooio `POST /v4/messages`
  on `api.blooio.com`; BlueBubbles `POST /api/v1/message/text` on your server),
  to the one handle, from the one line, with the fields the call carries and
  nothing else. A read, another path or host, another recipient, a group chat
  and a link are refused before they are sent.
- **What is not recorded**: the webhook the backend posts (it is the captured
  fixture), anything between heo and the gateway, and delivery to a device.
  BlueBubbles' `tempGuid` is the operation ID, which heo mints anew each run, so
  the recorded request holds `OPERATION_KEY` in its place.
- **Rehearsal**: `HEO_RECORD_IMESSAGE_REHEARSE=1` with made-up values for the
  backend's credentials runs the same test against canned answers: nothing is
  sent and nothing is counted, and a stand-in backend posts the reply. Use it to
  debug a change before spending a send. Its cassettes and fixture are
  worthless: delete `tests/e2e/testdata/cassettes/TestXRRIMessage*` and
  `tests/e2e/testdata/imessage` before committing, or record over them.
  `TestIMessageRehearsal` runs the same path, record then replay, in a
  temporary directory for each backend on every `make test`, so the harness is
  exercised without a backend.

`xrrx.Audit` runs over cassettes and fixtures in every `go test ./...` (see
above); a recording also denies the literals it used: the handle and its
digits, the line, the credentials, the webhook token and the server's address.
The audit is a safety net: read the diff. What it cannot know: a Blooio or
BlueBubbles answer carries ids of the backend's own (`chat_id`, `contact_id`,
message GUIDs), and a BlueBubbles answer is the whole serialized message, which
may name you, other handles or the Mac. Look for them with the commands at the
end of uma's guide, and replace what you do not want published by hand in the
response, never in a recorded request.

### Recording Discord

`TestXRRDiscordChatProviderVerbs` runs the real binary against
`discord.chat`: a first message to a channel, a reply, a command and a button
press (signed fixtures), an edit, a reaction set and cleared, and two
deletions. In replay it needs nothing: the daemon answers from
`testdata/cassettes/TestXRRDiscordChatProviderVerbs/daemon`, and the
interaction fixtures come from `testdata/discord`. The committed cassettes
were recorded in a throwaway server; with none, the test skips and says so.
The verbs the recording refuses (thread members, interaction answers) and the
gateway are covered in process by `TestDiscordProviderVerbs`,
`TestDiscordSelectionRoutes`, `TestDiscordGateway` and
`TestDiscordInteractionsEndpoint`, which answer from a scripted Discord (REST
and gateway) and need no application. Recording is the
only step that reaches Discord, and it **sends real messages**.

```sh
export HEO_RECORD_DISCORD_BOT_TOKEN=<token>      # from your secret manager,
export HEO_RECORD_DISCORD_APPLICATION_ID=<ID>    # never a file or an argument
export HEO_RECORD_DISCORD_BOT_USER_ID=<ID>
export HEO_RECORD_DISCORD_GUILD_ID=<ID>
export HEO_RECORD_DISCORD_CHANNEL_ID=<ID>
make record-discord
git diff tests/e2e/testdata       # read every byte, then
make test
```

- **The application** is one you own and made for this: in the
  [Developer Portal](https://discord.com/developers/applications), create an
  application, add a bot, copy its token (Bot, Reset Token), and install it in
  your server with the `bot` scope and the permissions View Channel, Send
  Messages, Read Message History and Add Reactions. It needs no privileged
  intent and no administrator right: it edits, deletes and reacts to its own
  messages. The application's ID is on General Information; the bot user's ID
  is the first part of the token, base64 (the recording refuses a token that
  is not the named bot's). Do not set an Interactions Endpoint URL: the
  recording never receives.
- **The server and channel** are the one place the recording writes: a
  throwaway server of yours with one text channel the bot can see. Turn on
  Developer Mode (User Settings, Advanced) and copy the server's and the
  channel's IDs. They are snowflakes, never a name; the same ID for two of the
  four is refused (an application that is its own bot user may share one).
  Delete the server afterwards and reset or delete the bot's token.
- **The cap**: at most `HEO_RECORD_DISCORD_MAX_SENDS` (default 3) Create
  Message requests are ever made, counted in a ledger directory
  (`HEO_RECORD_DISCORD_LEDGER`, default a `heo/discord-record-sends` directory
  in the user cache) that outlives the run. A send takes its slot before the
  request leaves, so a failed run counts. The next one is refused with the cap
  in the message; deleting the ledger is a deliberate act of the application's
  owner. The test sends two messages; the third slot is spare for a run that
  failed midway.
- **The text** starts with the words "heo adapter test", is short, and carries
  no link, mention, component or embed, and the request must say
  `allowed_mentions` `{"parse":[]}`, so it pings no one.
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with `CI`
  set. `make record` sets neither, so it skips this test; without
  `HEO_RECORD_DISCORD_BOT_TOKEN` a direct `go test` skips too.
- **What it reaches**: `https://discord.com/api/v10/channels/<channel>/...`
  and these routes only: Create Message (counted), and Edit Message, Delete
  Message, Create Reaction and Delete Own Reaction on messages this recording
  sent. Everything else is refused before it is sent: every read (history,
  reconciliation, Get Channel, Get Gateway Bot), Create DM, the typing
  indicator, threads and their members, webhooks and interaction callbacks (a
  token only a person's interaction makes: the test previews `interaction
  respond` and does not send it), and administration. Discord's gateway
  (`*.discord.gg`) is refused for every recording.
- **The credential**: the bot token reaches the process that sends, as an
  environment variable, and is put in the `Authorization: Bot` header by the
  transport only. heo's own credential store holds a stand-in for it and a
  stand-in Ed25519 public key for the Interactions Endpoint URL, so the real
  token is in no file and never in the daemon's configuration. The daemon is
  configured with the placeholder application, bot, server and channel; the
  transport swaps them for the real ones on the way out and back on the way
  in. A transport error that spells out the token has it replaced.
- **Inbound is not recorded**: Discord's messages reach heo over the gateway,
  a WebSocket session that is no HTTP exchange and has nothing to replay from a
  cassette, so the gateway receiver is tested in process only
  (`TestDiscordGateway*`) and is **unverified against Discord**. The recording
  daemon runs no receiver (`services.receivers` is off, and the channel's
  `receive` is `interactions`). The command and the button press are fixtures
  (`testdata/discord/*.json`, the shapes Discord documents, with placeholder
  IDs); the test signs each with a stand-in key at run time, and the audit
  fails a committed token.
- **The nonce**: every message carries a nonce derived from an operation ID
  that is new on every run, so the recorded request holds `<nonce>` in its
  place; without that the same request would not fingerprint the same in
  replay.
- **Rehearsal**: `HEO_RECORD_DISCORD_REHEARSE=1` with made-up values for the
  token (`<base64 of the bot user ID>.<6 characters>.<27 or more>`) and four
  made-up snowflakes runs the same test against an in-memory Discord: nothing
  is sent and nothing is counted. Use it to debug a change before spending a
  send. Its cassettes are worthless: delete
  `tests/e2e/testdata/cassettes/TestXRRDiscordChatProviderVerbs` before
  committing, or record over them.

The scrub (`internal/xrrx/discord_scrub.go`) runs on every answer before it
reaches a cassette or the adapter, and an answer that still holds the token or
one of its parts, the application's, the bot's, the server's or the channel's
ID, or a name key is not handed on. A user object keeps only its ID, whether it
is a bot and the bot's own username; a member is dropped. Discord's error
answers are Discord's, which is the point of recording them: the adapter reads
their codes. `xrrx.Audit` runs over cassettes and fixtures in every `go test
./...` and denies: a bot token other than the stand-in, `global_name`,
`display_name` and `discriminator` keys, a username that is not the
placeholder, and an account, server, channel or user ID that is not one; a
recording also denies the literals it used. The audit is a safety net: read the
diff. What it cannot know: a message's `id` and `timestamp` are its time and
stay, since the next request names the ID.

### Recording Microsoft Teams

`TestXRRTeamsProviderVerbs` runs the real binary against `microsoft.teams`: a
first message to a user (through the personal chat the Bot Connector opens), a
reply, an edit, a reaction set and cleared, and two deletions; the messaging
endpoint is checked for what it refuses. In replay it needs nothing: the
daemon answers from
`testdata/cassettes/TestXRRTeamsProviderVerbs/daemon`. **No cassette is
committed yet**: with none, the test skips and says so, and the adapter's
verbs are covered in process by `TestTeamsProviderVerbs`, `TestTeamsWebhook`
and the tests beside them, which answer from a scripted Bot Connector and
need no tenant. Recording is the only step that reaches Microsoft, and it
**sends real messages**.

```sh
export HEO_RECORD_TEAMS_CONNECTOR_TOKEN=<token>      # from your secret manager,
export HEO_RECORD_TEAMS_APP_ID=<app ID>              # never a file or an argument
export HEO_RECORD_TEAMS_TENANT_ID=<tenant ID>
export HEO_RECORD_TEAMS_SERVICE_URL=https://smba.trafficmanager.net/teams/
export HEO_RECORD_TEAMS_USER_ID=<29:...>
export HEO_RECORD_TEAMS_CONVERSATION_ID=<a:...>
make record-teams
git diff tests/e2e/testdata       # read every byte, then
make test
```

- **The tenant** is a Microsoft 365 tenant you own, with Teams and no
  customer data: never one people work in, since the test posts, edits and
  deletes. **The bot** is a single-tenant Azure Bot registration in it
  (Developer Portal bot when the tenant has no Azure subscription) with the
  Teams channel on, its Teams app installed for **one user of yours** with
  the personal scope. The setup, the tenant options and how to find the
  user's `29:...` ID and the personal chat's `a:...` ID are in the uma
  repository's "Record Teams cassettes" guide; the chat is the one the bot
  opens with that user, never a channel or a group chat, and the recording
  refuses those IDs.
- **The token** is a Bot Connector access token, a JWT, which **you mint** from
  the app's client credentials (a client credentials grant for the scope
  `https://api.botframework.com/.default`, see
  [Credentials](manual/adapters.md#microsoft-teams-microsoftteams)) and pass in
  the recording shell. heo mints its own token from the bot's client secret or
  certificate when it runs for real, but the recording never holds either: the
  daemon mints from a stand-in secret, and the sandbox answers that one request
  itself (see **The credential** below). The token lasts an hour, so mint it just
  before the run. The recording refuses anything that is not a JWT, so the
  app's client secret cannot be put here by mistake. Delete the client secret
  and uninstall the app afterwards.
- **The service URL** is the Bot Connector service of the tenant's region and
  cloud. Only Microsoft's hosts are accepted (`*.trafficmanager.net`,
  `*.teams.microsoft.com`, `*.teams.microsoft.us`), so the token is never sent
  elsewhere.
- **The cap**: at most `HEO_RECORD_TEAMS_MAX_SENDS` (default 3) message
  activities are ever sent, counted in a ledger directory
  (`HEO_RECORD_TEAMS_LEDGER`, default a `heo/teams-record-sends` directory in
  the user cache) that outlives the run. A send takes its slot before the
  request leaves, so a failed run counts. The next one is refused with the cap
  in the message; deleting the ledger is a deliberate act of the tenant's
  owner. The test sends two messages; the third slot is spare for a run that
  failed midway. A message a run left behind stays in your chat: a user cannot
  delete a bot's message, so run the test again or uninstall the app.
- **The text** starts with the words "heo adapter test", is short, and carries
  no link, mention, markup or card, so it mentions no one.
- **Flags**: `XRR_MODE=record` and `HEO_RECORD_LIVE=1`, both, never with `CI`
  set. `make record` sets neither, so it skips this test.
- **What it reaches**: the one Bot Connector service named, and these calls
  only, on the one personal chat: creating the chat with your user
  (`POST /v3/conversations`), sending a message (`POST .../activities`,
  counted), and updating, deleting and reacting to (`PUT` and `DELETE
  .../activities/{id}`, `.../reactions/{type}`) messages this recording sent.
  Everything else is refused before it is sent: a typing indicator, a card, a
  channel or group chat, any other user, a roster read, the Bot Framework's
  key set (`login.botframework.com`). Microsoft Entra
  (`login.microsoftonline.com`) is never reached: the daemon mints its Connector
  token there, and the sandbox answers only the placeholder bot's request, with
  the stand-in secret or the stand-in certificate's assertion, the stand-in
  token in return; any other request to it is refused. The Bot Connector has no call that reads past
  messages, so there is no read-back: the test checks the operations, and the
  cassettes hold what Teams answered.
- **The credential**: the Bot Connector token reaches the process that sends,
  as an environment variable, and is put in the `Authorization` header by the
  transport only. heo's own credential store holds a stand-in for the bot's
  client secret (purpose `microsoft.teams.client_secret`), from which the
  daemon mints a stand-in token, so the real token is in no file and never in
  the daemon's configuration, and the real secret is never held at all. The
  cassettes hold that exchange with the secret and the token redacted.
  `HEO_RECORD_TEAMS_CREDENTIAL=certificate` stores a stand-in certificate and
  key instead (purpose `microsoft.teams.client_certificate`), made for the run:
  the sandbox is told the certificate and answers only an assertion signed with
  its key for the placeholder bot, and the cassette holds the request with the
  assertion replaced by a placeholder. It is for a live recording or its
  rehearsal; replay is of the cassettes the stand-in secret recorded. The daemon
  is configured with the placeholder bot, tenant, service and user; the
  transport swaps them for the real ones on the way out and back on the way
  in. A transport error that spells out the request URL has the real names
  replaced. Do not run a proxy or a logging HTTP client on the recording that
  writes URLs or headers to disk.
- **Inbound is not recorded**: Teams signs every activity it posts with a
  Bot Connector token a replay cannot produce, and the recording's daemon
  never reads the Bot Framework's keys. The messaging endpoint is checked on
  the real listener for what it refuses without a token; messages, edits,
  reactions and card presses are covered in process
  (`TestTeamsWebhook`), where the test signs them itself against keys of its
  own.
- **Rehearsal**: `HEO_RECORD_TEAMS_REHEARSE=1` with made-up values (a JWT-shaped
  token, GUIDs, a Microsoft service host, a `29:...` and an `a:...` ID) runs
  the same test against an in-memory Bot Connector: nothing is sent and nothing
  is counted. Use it to debug a change before spending a send. Its cassettes
  are worthless: delete
  `tests/e2e/testdata/cassettes/TestXRRTeamsProviderVerbs` before committing,
  or record over them.

The scrub (`internal/xrrx/teams.go`, `teams_scrub.go`) runs on every answer
before it reaches a cassette or the adapter, and an answer that still holds
the token, the bot, the tenant, the service host, the user or the chat, a
token-shaped string or a name key is not handed on. `xrrx.Audit` runs over
cassettes and fixtures in every `go test ./...` and denies: a Bot Connector
token, a Microsoft service host, `name`, `email`, `aadObjectId` and similar
keys, and an account, a conversation or a GUID that is not a placeholder; a
recording also denies the literals it used. The audit is a safety net: read
the diff. What it cannot know: an activity ID, which the next request names,
and a message's timestamps, which stay.

### Adding a cassette test

1. Name it `TestXRR...` so `make record` finds it.
2. Start each child that calls out with `sandbox.xrrEnv("<process>")`
   (`cliSite.startXRR` does it for the daemon).
3. Own the peer. In record mode start a stand-in; in replay use a closed
   address (`newProviderStandIn`). Assert on the peer in record mode and
   on its silence in replay.
4. Make the request the same every run: no operation IDs, times or temp
   paths in what the binary sends. Where it cannot be, scrub it in
   `internal/xrrx`.
5. `make record`, read the diff, `make test`.

The fake provider adapter for this (`adapterstest.Provider`) sends one
POST per message to the stand-in with the channel's credential as a
bearer token.

### LLM cassettes

The cassettes of `conversation summarize`, `conversation triage` and `message
draft` are recorded against a real model, not a stand-in the test owns: what a
model answers is not something a test can write by hand. They replay with no
model and no network (`make test`); only re-recording needs one.

```sh
make record-llm                                      # a model on the default Ollama port
make record-llm LLM_ENDPOINT=http://127.0.0.1:11500  # another local endpoint
git diff --stat tests/e2e/testdata                   # read it
make test                                            # replays what you recorded
```

- The model is the one `tests/e2e/llm_test.go` names (`llmModel`); the server
  must have pulled it. Recording refuses an endpoint that is not on this machine
  (`assist.ClassOfHost`), a cloud model (a name ending in `:cloud`) and `CI`.
- The conversation is synthetic. Keep it so: a recording is committed.
- The recorded request carries a placeholder host (`loopback.invalid`), so the
  port you record on does not matter; no credential is sent to a local server.
- One test is one cassette directory (`TestRecordedLLM*`), so a model that wanders
  off (a thinking model sometimes does, and the call times out) costs a rerun of
  one test: `XRR_MODE=record HEO_E2E_LLM_ENDPOINT=... go test -run
  '^TestRecordedLLMTriage$' ./tests/e2e/`. They are not `TestXRR*`, so `make record`
  leaves them alone.
- The prompt is the request's fingerprint: change the wording of a prompt in
  `internal/assist` and the cassettes must be re-recorded. A prompt holds no ID,
  time or counter, so it is the same on every run.
- `TestLLMCassettesHonourTheAudience` reads the recordings: the internal note is in
  the desk's summary and triage requests and in no other.

The other LLM tests need no model: `TestLLM*` run the daemon against an empty
cassette in strict replay, so any request it makes is a recorded miss the test
reads. That is how they show a preview sends nothing, that a failed local model is
an error and not a fallback, and that a cloud provider is refused until opted in.

## Scenarios and the tests that cover them

| Scenario | Tests |
| --- | --- |
| start, stop | `TestServiceStartSocketOnlyAndSIGTERM`, `TestRuntimeEmptyInstallReadyAndDrains`, `TestServiceControlPerProfile` |
| signals | `TestDaemonSignals` (SIGINT, SIGTERM, SIGHUP: clean stop, socket and pidfile gone), `TestSIGHUPDrainsLikeSIGTERM` (the drain finishes the send), `TestSecondSignalAbortsDrain` (second signal: exit 1 in under a second, send left leased, settled once by the next run), `TestRuntimeEmptyInstallReadyAndDrains`, `TestEventWatch` (Ctrl-C of a watch) |
| watch loses its daemon | `TestWatchWithNoDaemonExits70` (event, task, operation: exit 70 at once), `TestWatchReconnectsWithinItsBound` (event, task, operation: `--reconnect-timeout` 2s gives up with the stderr line, 0 at once, 30s rides a restart), `TestRetry*` (the shared helper) |
| port contention | `TestPortContentionRefusesStartAndLeaksNothing` |
| competing owners | `TestRuntimeSecondDaemonOnSameStoreRefused`, `TestRuntimeDuplicateProviderAccountRefused`, `TestRuntimeProviderAccountClaimedAcrossProfiles`, `TestServiceControlPerProfile`, `TestForeignOwnedSocketRefused` |
| disconnected CLI | `TestDaemonAbsentExits70`, `TestMCPStdioRelaysOverSocket`, `TestServedCallOutsideDaemonRefused`, `TestOfflineCommandsNeedNoDaemon` |
| restart recovery with queued work | `TestServiceStopDuringDeliveryDrains` (queued before start; the next run sends nothing again), `TestServiceStopDrainDeadlineLeavesLease`, `TestCrashRecoversStaleStateAndQueuedWork`, `TestDeliveryGetAndRetry` |
| private-note access | `TestInternalNotesHiddenFromViewers`, `TestEventGet`, `TestEventList`, `TestMessageSearch` |
| stdout/stderr contract | `TestStdoutStderrContract`, and the per-command stream assertions throughout |
| surface parity: socket, REST, rpc, MCP | `TestServedPermissionGateParity`, `TestServedSurfacesConcurrent`, `TestDestructiveOnlyOnSocket`, `TestHTTPSurfacesRequireAuthOnLoopback`, `TestEventWatchIsSocketOnly`, `TestOperationWatchIsSocketOnly` |
| stop during drain (no escalation, no SIGKILL) | `TestServiceStopDuringDeliveryDrains`, `TestServiceStopDrainDeadlineLeavesLease`, `TestRepeatedStopDuringDrain` |
| repeated stop | `TestRepeatedStopDuringDrain`, `TestServiceStopWithNoDaemonExitsZero` |
| stop with no daemon (exit 0, no runtime dir created) | `TestServiceStopWithNoDaemonExitsZero` |
| socket path budget | `TestSocketPathBudget` |
| service setup, no terminal | `TestSetupNonTTYNeverPrompts` |
| cassette-backed: webhook-fed reply over HTTP | `TestXRRWebhookFedReplyOverHTTP` |
| cassette-backed: `pg_dump` | `TestXRRStorageBackupThroughPgDump` |
| cassette-backed: `github.issues` reply, edit, delete, react, close and reopen | `TestXRRGitHubIssuesProviderVerbs` |
| `slack.chat`: webhook signatures, events, interactions and every declared verb in process; the cassette test skips until recorded | `TestSlackWebhook`, `TestSlackProviderVerbs`, `TestSlackOneReceiverPerWorkspace`, `TestXRRSlackChatProviderVerbs` |
| `telegram.messages`: secret token, updates, long polling with its cursor, a webhook set at Telegram, every declared verb in process, one receiver per bot; the cassette test skips until recorded | `TestTelegramWebhook`, `TestTelegramWebhookCredentials`, `TestTelegramPolling`, `TestTelegramPollingConflict`, `TestTelegramPollingResumesAfterWebhook`, `TestTelegramPollingBadToken`, `TestTelegramProviderVerbs`, `TestTelegramSelectionRoutes`, `TestTelegramOneReceiverPerBot`, `TestXRRTelegramMessagesProviderVerbs` |
| `discord.chat`: the gateway (identify, resume after a close, a reconnect and a restart, the close that ends it for good, the identify budget), receive modes, the Ed25519 endpoint, every declared verb in process, one receiver per application; the cassette test skips until recorded | `TestDiscordGateway`, `TestDiscordGatewayAsksForMessageContent`, `TestDiscordGatewayReconnects`, `TestDiscordGatewayRestartResumes`, `TestDiscordGatewayDisabledIntent`, `TestDiscordGatewayRefusedToken`, `TestDiscordGatewayIdentifyLimit`, `TestDiscordReceiveModes`, `TestDiscordReceiveModeReconciled`, `TestDiscordInteractionsEndpoint`, `TestDiscordInteractionsKeyMissing`, `TestDiscordProviderVerbs`, `TestDiscordSelectionRoutes`, `TestDiscordOneReceiverPerApplication`, `TestXRRDiscordChatProviderVerbs` |
| provider verb matrix: every row backed by a test that exists | `TestEvidenceNamesRealTests`, `TestRowsClaimNoMoreThanTheirTests`, `TestEveryDeclaredVerbHasEvidence`, `TestProviderMatrixIsCurrent` |
| LLM helpers: recorded model answers, desk and viewer audience, triage suggests only, draft never sends | `TestRecordedLLMSummarizeAsDesk`, `TestRecordedLLMSummarizeAsViewer`, `TestRecordedLLMTriage`, `TestRecordedLLMDraft`, `TestLLMCassettesHonourTheAudience` |
| LLM helpers: preview sends nothing, roles, no fallback, cloud opt-in and consent, served surfaces | `TestLLMPreviewSendsNothingAndRolesGate`, `TestLLMNoFallbackWhenTheLocalModelIsDown`, `TestLLMLocalServerNotRunning`, `TestLLMCloudNeedsOptInAndConsent`, `TestLLMServedSurfaces` |

## Conformance

heo is gated against the 12-factor AI-CLI conformance spec in two layers.
The Go tests run in `make test`; the kit gate runs on demand with
`make conformance`.

| What | Where | Needs |
| --- | --- | --- |
| Command tree against kit's static contract: `kitconformance.AssertCLIWithOptions` with the guidance tier on (`EnforceGuidance`), plus the signature walk (`root.ValidateSignature()`, which `AssertCLI` once skipped) | `cmd/conformance_test.go` | nothing |
| Every documented example and next step is a command line that exists: its leaf, flags, argument count, required flags, and `--help` through `Execute` | `cmd/guidance_test.go` | nothing |
| A usage error heo raises carries a `suggested_fix` | `cmd/usagefix_test.go` | nothing |
| Every non-interactive leaf answers `--help` | `TestCapabilityRoundtrip` | nothing |
| Every destructive leaf is refused without `--confirm=yes`, through `Execute` (a tree that only `cli.New` built has no confirmation gate) | `TestDestructiveLeavesAreGatedAtExecute` | nothing |
| Every story step names a real command, flag and argument count | `cmd/stories_test.go` | nothing |
| `verify-no-leak`, `verify-stories`, `grade`, then spec-12fc's aggregate, matrix, badge and SARIF | `make conformance` (`scripts/conformance.sh`) | kit, jq, python3, the private scenarios |

A new destructive leaf fails `TestDestructiveLeavesAreGatedAtExecute`
until it has a row in `destructiveGateArgs`.

### Guidance on a new command

Kit's guidance tier wants `kit/examples` on every runnable leaf and
`kit/next-steps` on every leaf that is not a read. `cmd/guidance.go`
fills both, so a new command adds only the text:

- **Examples**: write the cobra `Example` block, one `heo ...` invocation
  per line. `applyGuidance` derives `kit/examples` from it, titles
  included, so there is one source. A leaf with no such text lists its
  examples in `leafExamples`, and they are rendered back into `Example`.
- **Next steps**: add the leaf to `leafNextSteps`: when it applies, the
  literal next invocation (`<angle brackets>` for what the caller fills
  in) and why. Point at the command that reads the result back or
  carries on, with its real flags.

`TestAssertCLI` names a leaf that has neither, and
`cmd/guidance_test.go` fails on an example or step that names a command,
flag or argument count the tree does not have.

### Running the gate

```sh
HEO_SCENARIOS=<scenario library> KIT=<kit> make conformance
git diff docs/conformance tests/conformance .12fc.json
```

It builds heo, scans the repository for scenario rubrics, records the
scenario's steps against `bin/heo` in a clean environment (a fixed
sandbox under `/tmp`, so the paths a command prints are the same on every
recording), starts the grading service on loopback for the length of the
run, and runs spec-12fc's `scripts/run.sh` over the result. Nothing is
uploaded. About 5 s after the build.

Build `kit` from the source `go.mod` resolves `hop.top/kit` to, not from
a release: kit's exit codes moved (leak 66, config 67, io 6, grade 68 and
69), and `kit` on `PATH` may predate them. `SPEC_12FC_DIR` names a
spec-12fc checkout; without it the script clones the commit
`SPEC_12FC_REF` pins.

The evidence it writes is committed, and rewritten on every run, so
commit it only when a grade, a factor or a cassette changed:

| File | What it is |
| --- | --- |
| `.12fc.json` | the badge, rendered by `kit conformance badge` from the matrix; never edited by hand |
| `docs/conformance/12fc-report.json` | each leaf's exit code and payload, as spec-12fc aggregates them |
| `docs/conformance/12fc-matrix.json` | one status per factor, derived from the leaves |
| `docs/conformance/12fc.sarif` | SARIF for code scanning; empty while nothing fails |
| `tests/conformance/cassette/` | what was recorded and graded: per step exit code, stdout, stderr, and the story |

Timestamps (`generatedAt`, `scored_at`, `recorded_at`), the commit the run
measured and step durations change on every run; that is the only churn
on an unchanged tree.

### Stories and scenarios

`e2e/stories/*.yaml` are the stories: what a user is trying to do, as plain
intent plus a command sequence. They are public, validated by
`kit conformance verify-stories`, and never executed by it.
`local-install-walkthrough` runs with no daemon, store or network, and is
the story that is recorded and graded. The other four (triage and reply,
follow a send, merge contacts, back up before an upgrade) need a daemon
and a store and are validated only.

A scenario is the rubric that grades a recording of a story. It lives
outside this repository on purpose: a rubric an agent can read is a
rubric it can satisfy, and `verify-no-leak` fails the build on one. The
cassette's `manifest.yaml` names its scenario by id, which the scanner
reads as scenario-shaped, so `.verifynoleak.allow` lists that one file.
Results are committed at grading tier 2 (a verdict and a status per
factor); tier 3 would put the assertions in the repository.

`verify-stories` parses every YAML file below `--paths` as a story, so the
gate is given `e2e/stories`, not `.`; the repository-wide scan is a
separate `verify-no-leak --paths=.` in the script.

### What each factor measures

A factor is `pass` in the matrix when the leaf spec-12fc maps it to
passes. That mapping is coarse, so this is what stands behind each one,
and what does not.

| Factor | Leaf | Behind the pass | Not measured |
| --- | --- | --- | --- |
| F1 Capability introspection | verify-stories | stories valid; `TestCapabilityRoundtrip`; `TestStoryStepsResolveInTheTree`; `spec --version` graded under F5 | no conformance leaf reads the whole `heo spec` manifest; `TestGeneratedDocsAreCurrent` does, to render the command reference |
| F2 Intent clarity | verify-stories | stories valid | nothing grades the grammar itself (`AssertCLI` and the signature walk do the shape) |
| F3 Structured I/O | grade | JSON on stdout, none on stderr, for four commands | the daemon-backed commands |
| F4 Corrective errors | grade | code, message and `suggested_fix` of a not-found, a prerequisite and a usage error | the errors no story step triggers; `cmd/usagefix_test.go` holds usage errors to the same rule |
| F5 Explicit contracts | grade | `spec --version` schema version; adapter contract fields | output schemas (`kit/output-schema`) are not validated against output |
| F6 Previewability | grade | `--dry-run` names its effect and a later read shows nothing written | previews that reach the daemon (`message send`, `contact merge`) |
| F7 Idempotency | grade | the same `config set` twice leaves one state; repeated `service stop` is a no-op | `--idempotency-key` replay, covered by `tests/e2e`, not graded |
| F8 State transparency | grade | every value names its origin layer; absence is reported, not hidden | daemon status when a daemon runs |
| F9 Contextual guidance | verify-stories | stories valid; kit's guidance tier (`TestAssertCLI`) and the example and next-step parse tests | the stories are not run; whether a next step is the best one is not measured |
| F10 Delegation safety | verify-no-leak | no scenario rubric anywhere in the repository | |
| F11 Exit code semantics | grade | 0, 2, 3 and 70 each observed where documented | 4, 5, 6 and 8, covered by `tests/e2e` |
| F12 Evolution guarantees | verify-stories | stories valid only | deprecation and schema-version policy are not measured by any kit leaf |

The graded recording covers the surface that works without a daemon.
Grading the daemon-backed surface needs a Postgres store, a running
daemon and a provider stand-in behind a single recorded story;
`tests/e2e` covers that behaviour but kit does not grade it.

### Skipped, unsupported and left

- **Typed-token confirmation** (`EnforceDestructiveToken`, kit's C2):
  not adopted. `message delete`, `contact merge` and `storage restore`
  are gated by kit's own destructive gate, `--confirm=yes`, which kit
  installs at `Execute` and heo tests for each of them. The typed token
  is a stricter tier kit leaves off by default: the leaf opts in with
  `SetDestructiveToken`, and then `--confirm=yes` alone no longer
  proceeds. The first run is refused and prints `--confirm-token=<sha>`,
  a digest of the command path, so it is a second round trip, not a
  secret. Adopting it changes the confirmation grammar every caller,
  story and served surface uses for these three commands, so it is a
  decision for a release, not a conformance fix. The gate it would flip
  names exactly those three leaves. Kit's dry-run rationale tier
  (`EnforceDryRunRationale`) passes today and is not enforced.
- **Strict toolspec tier of `verify-stories`**: spec-12fc does not pass
  `--strict-toolspec`, and heo's `spec` manifest is not the toolspec shape
  it reads. `TestStoryStepsResolveInTheTree` does the same check against
  the real command tree.
- **Cassette verbs** (`cassette_must_contain`, `dry_run_no_mutation`,
  `idempotency_replay_clean`): not used. The release binary has no xrr
  seam, so every cassette is empty and those verbs would pass without
  looking at anything. Preview and replay are graded on what the commands
  print.
- **CI**: no workflow runs the gate. spec-12fc's action installs kit from
  a release, and kit's latest release carries no binaries while `go
  install hop.top/kit/cmd/kit@<tag>` refuses a module with replace
  directives, so the install step cannot succeed today. Add the 12fcc
  workflow template once it can.
- **When `HEO_SCENARIOS` is unset**: `grade` is skipped, F3 to F8 and F11
  report `skip`, and kit renders the badge red with `5/5 pass`. A run
  without scenarios must not be committed.
- **spec-12fc's `run.sh`** predates kit's exit codes: it maps only exit 4
  to `skip`, so kit's io error (6) reads as `fail`.
