.PHONY: build build-windows test record record-github record-twilio record-slack record-telegram record-imessage record-discord record-teams record-llm conformance lint links spec docs-gen docs-check check clean setup symlink release \
       promote promote-alpha promote-beta promote-rc \
       promote-release

check: lint docs-check test spec links build-windows

build:
	mkdir -p bin
	go build -buildvcs=false -o bin/heo .

# build-windows cross-compiles every package, tests included (vet
# type-checks _test.go files), for windows. windows is build-only: no
# release archives, since the CLI's daemon channel is a unix socket with
# peer-credential auth. This target keeps the tree compiling there.
build-windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -buildvcs=false ./...
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go vet -buildvcs=false ./...

# symlink installs ./bin/heo into the first writable user-bin
# directory on $PATH ($XDG_BIN_HOME → ~/.local/bin → ~/bin on Unix;
# %USERPROFILE%\bin → %USERPROFILE%\.local\bin → %LOCALAPPDATA%\Programs
# on Windows, where the link is a .cmd shim because os.Symlink there
# requires Admin or Developer Mode). Override the target dir with
# `make symlink SYMLINK_DIR=/path/to/bin`. Re-running with the same
# target is a no-op; pass FORCE=1 to replace a link with a different
# target.
symlink: build
	kit symlink --target ./bin/heo --name heo \
		$(if $(SYMLINK_DIR),--dir $(SYMLINK_DIR),) \
		$(if $(FORCE),--force,)

# test replays the e2e cassettes (XRR_MODE unset is replay): nothing
# reaches a provider and no recorded tool runs.
test:
	go test -buildvcs=false ./...

# record re-records the e2e cassettes against the fixtures the tests own
# (a local provider stand-in, a local tool script); it needs Docker.
# The github.issues, twilio.sms, slack.chat, telegram.messages, iMessage,
# discord.chat and microsoft.teams recordings skip here: they are live, see
# record-github, record-twilio, record-slack, record-telegram, record-imessage,
# record-discord and record-teams.
# Refused when CI is set. Read `git diff tests/e2e/testdata` before
# committing, then run `make test`. See docs/DEVELOPING.md.
record:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	XRR_MODE=record go test -buildvcs=false -count=1 -run 'TestXRR' ./tests/e2e/
	go test -buildvcs=false -count=1 ./internal/xrrx/
	@git status --short tests/e2e/testdata

# record-github re-records the github.issues cassettes and webhook
# fixtures against a private sandbox repository you own. It is the one
# target that reaches a provider: it needs HEO_RECORD_GITHUB_REPO
# (owner/name, private; the recording refuses a public one before every
# write) and HEO_RECORD_GITHUB_TOKEN, and is refused when CI is set.
# Read `git diff tests/e2e/testdata` before committing, then run
# `make test`. See docs/DEVELOPING.md.
record-github:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-github: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_GITHUB_REPO" ] || [ -z "$$HEO_RECORD_GITHUB_TOKEN" ]; then \
		echo "make record-github: set HEO_RECORD_GITHUB_REPO (owner/name of a private sandbox) and HEO_RECORD_GITHUB_TOKEN" >&2; exit 1; fi
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -run '^TestXRRGitHub' ./tests/e2e/

# record-twilio re-records the twilio.sms cassettes against a Twilio account
# you own. It is the one target that texts a real phone, and it is narrow:
# at most HEO_RECORD_TWILIO_MAX_SENDS (default 2) messages are ever sent,
# counted in a ledger that outlives the run, and only to
# HEO_RECORD_TWILIO_TO, a number you own; see docs/DEVELOPING.md before
# running it. It needs HEO_RECORD_TWILIO_ACCOUNT_SID and
# HEO_RECORD_TWILIO_AUTH_TOKEN in the environment (never a file or an
# argument) and is refused when CI is set. Read `git diff tests/e2e/testdata`
# before committing, then run `make test`.
record-twilio:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-twilio: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_TWILIO_ACCOUNT_SID" ] || [ -z "$$HEO_RECORD_TWILIO_AUTH_TOKEN" ] || [ -z "$$HEO_RECORD_TWILIO_TO" ]; then \
		echo "make record-twilio: set HEO_RECORD_TWILIO_ACCOUNT_SID, HEO_RECORD_TWILIO_AUTH_TOKEN and HEO_RECORD_TWILIO_TO (your own number, E.164)" >&2; exit 1; fi
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -run '^TestXRRTwilio' ./tests/e2e/

# record-slack records the slack.chat cassettes against a Slack workspace
# you own: one throwaway channel the app's bot is a member of. It posts real
# messages there, and it is narrow: at most HEO_RECORD_SLACK_MAX_POSTS
# (default 3) chat.postMessage requests are ever made, counted in a ledger
# that outlives the run, and only that channel is written or read; see
# docs/DEVELOPING.md before running it. It needs the bot token
# (HEO_RECORD_SLACK_BOT_TOKEN, in the environment only, never a file or an
# argument) and the workspace, bot user and channel IDs, and is refused when
# CI is set. Read `git diff tests/e2e/testdata` before committing, then run
# `make test`. HEO_RECORD_SLACK_REHEARSE=1 reaches nothing: see the guide.
record-slack:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-slack: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_SLACK_BOT_TOKEN" ] || [ -z "$$HEO_RECORD_SLACK_TEAM_ID" ] || [ -z "$$HEO_RECORD_SLACK_BOT_USER_ID" ] || [ -z "$$HEO_RECORD_SLACK_CHANNEL_ID" ]; then \
		echo "make record-slack: set HEO_RECORD_SLACK_BOT_TOKEN, HEO_RECORD_SLACK_TEAM_ID, HEO_RECORD_SLACK_BOT_USER_ID and HEO_RECORD_SLACK_CHANNEL_ID (a throwaway channel; HEO_RECORD_SLACK_BOT_ID is optional)" >&2; exit 1; fi
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -run '^TestXRRSlack' ./tests/e2e/

# record-telegram records the telegram.messages cassettes against a bot you
# own: one chat of yours the bot is in. It sends real messages there, and it
# is narrow: at most HEO_RECORD_TELEGRAM_MAX_SENDS (default 3) sendMessage
# requests are ever made, counted in a ledger that outlives the run, and only
# that chat is written; see docs/DEVELOPING.md before running it. It needs
# the bot token (HEO_RECORD_TELEGRAM_BOT_TOKEN, in the environment only,
# never a file or an argument), the bot's username and the chat's ID, and is
# refused when CI is set. Read `git diff tests/e2e/testdata` before
# committing, then run `make test`. HEO_RECORD_TELEGRAM_REHEARSE=1 reaches
# nothing: see the guide.
record-telegram:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-telegram: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_TELEGRAM_BOT_TOKEN" ] || [ -z "$$HEO_RECORD_TELEGRAM_BOT_USERNAME" ] || [ -z "$$HEO_RECORD_TELEGRAM_CHAT_ID" ]; then \
		echo "make record-telegram: set HEO_RECORD_TELEGRAM_BOT_TOKEN, HEO_RECORD_TELEGRAM_BOT_USERNAME and HEO_RECORD_TELEGRAM_CHAT_ID (your own chat the bot is in)" >&2; exit 1; fi
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -run '^TestXRRTelegram' ./tests/e2e/

# record-imessage records the path through uma's reference iMessage gateway
# (relay.messages is the operator's own relay, so the adapter itself has
# nothing to record): heo sends one message through the gateway and a backend
# to a handle you own, and your reply comes back through the backend's webhook.
# HEO_RECORD_IMESSAGE_BACKEND is blooio or bluebubbles, each with its own
# credentials (HEO_RECORD_IMESSAGE_BLOOIO_API_KEY, _FROM, _WEBHOOK_SECRET; or
# HEO_RECORD_IMESSAGE_BLUEBUBBLES_URL, _PASSWORD, _ACCOUNT, _WEBHOOK_TOKEN),
# in the environment only, never a file or an argument. It is narrow: at most
# HEO_RECORD_IMESSAGE_MAX_SENDS (default 2) sends are ever made, counted in a
# ledger that outlives the run, and only to HEO_RECORD_IMESSAGE_TO, a handle
# you own; HEO_RECORD_IMESSAGE_LISTEN is the address the backend's webhook
# reaches. You reply from that handle's device when the test asks. Refused
# when CI is set. Read `git diff tests/e2e/testdata` before committing, then
# run `make test`. HEO_RECORD_IMESSAGE_REHEARSE=1 reaches nothing; see
# docs/DEVELOPING.md.
record-imessage:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-imessage: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_IMESSAGE_BACKEND" ] || [ -z "$$HEO_RECORD_IMESSAGE_TO" ] || [ -z "$$HEO_RECORD_IMESSAGE_LISTEN" ]; then \
		echo "make record-imessage: set HEO_RECORD_IMESSAGE_BACKEND (blooio or bluebubbles), HEO_RECORD_IMESSAGE_TO (a handle you own) and HEO_RECORD_IMESSAGE_LISTEN (where the backend's webhook reaches), and the backend's credentials (docs/DEVELOPING.md)" >&2; exit 1; fi
	@case "$$HEO_RECORD_IMESSAGE_BACKEND" in \
		blooio) test -n "$$HEO_RECORD_IMESSAGE_BLOOIO_API_KEY" -a -n "$$HEO_RECORD_IMESSAGE_BLOOIO_FROM" -a -n "$$HEO_RECORD_IMESSAGE_BLOOIO_WEBHOOK_SECRET" || { \
			echo "make record-imessage: set HEO_RECORD_IMESSAGE_BLOOIO_API_KEY, HEO_RECORD_IMESSAGE_BLOOIO_FROM and HEO_RECORD_IMESSAGE_BLOOIO_WEBHOOK_SECRET" >&2; exit 1; } ;; \
		bluebubbles) test -n "$$HEO_RECORD_IMESSAGE_BLUEBUBBLES_URL" -a -n "$$HEO_RECORD_IMESSAGE_BLUEBUBBLES_PASSWORD" -a -n "$$HEO_RECORD_IMESSAGE_BLUEBUBBLES_ACCOUNT" -a -n "$$HEO_RECORD_IMESSAGE_BLUEBUBBLES_WEBHOOK_TOKEN" || { \
			echo "make record-imessage: set HEO_RECORD_IMESSAGE_BLUEBUBBLES_URL, _PASSWORD, _ACCOUNT and _WEBHOOK_TOKEN" >&2; exit 1; } ;; \
		*) echo "make record-imessage: HEO_RECORD_IMESSAGE_BACKEND must be blooio or bluebubbles" >&2; exit 1 ;; \
	esac
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -timeout 20m -run '^TestXRRIMessage' ./tests/e2e/

# record-discord records the discord.chat cassettes against an application you
# own: its bot in one server of yours, and one channel of that server. It
# sends real messages there, and it is narrow: at most
# HEO_RECORD_DISCORD_MAX_SENDS (default 3) Create Message requests are ever
# made, counted in a ledger that outlives the run, and only that channel's
# own messages are changed; see docs/DEVELOPING.md before running it. Only REST
# is recorded: the gateway is never reached. It needs the bot token
# (HEO_RECORD_DISCORD_BOT_TOKEN, in the environment only, never a file or an
# argument) and the application, bot user, server and channel IDs, and is
# refused when CI is set. Read `git diff tests/e2e/testdata` before committing,
# then run `make test`. HEO_RECORD_DISCORD_REHEARSE=1 reaches nothing: see the
# guide.
record-discord:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-discord: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_DISCORD_BOT_TOKEN" ] || [ -z "$$HEO_RECORD_DISCORD_APPLICATION_ID" ] || [ -z "$$HEO_RECORD_DISCORD_BOT_USER_ID" ] || [ -z "$$HEO_RECORD_DISCORD_GUILD_ID" ] || [ -z "$$HEO_RECORD_DISCORD_CHANNEL_ID" ]; then \
		echo "make record-discord: set HEO_RECORD_DISCORD_BOT_TOKEN, HEO_RECORD_DISCORD_APPLICATION_ID, HEO_RECORD_DISCORD_BOT_USER_ID, HEO_RECORD_DISCORD_GUILD_ID and HEO_RECORD_DISCORD_CHANNEL_ID (a server and channel you own)" >&2; exit 1; fi
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -run '^TestXRRDiscord' ./tests/e2e/

# record-teams records the microsoft.teams cassettes against a bot you own,
# in a tenant you own: its personal chat with one user of yours. It sends
# real messages there, and it is narrow: at most HEO_RECORD_TEAMS_MAX_SENDS
# (default 3) message activities are ever sent, counted in a ledger that
# outlives the run, and only that chat is written; see docs/DEVELOPING.md
# before running it. It needs a Bot Connector access token you minted from
# the app's client credentials by hand; the recorded daemon holds a stand-in
# secret and never the app's (HEO_RECORD_TEAMS_CONNECTOR_TOKEN, in the
# environment only, never a file or an argument; it lasts an hour), the
# app and tenant IDs, the service URL, your Teams user ID and the personal
# chat's ID, and is refused when CI is set. Read `git diff tests/e2e/testdata`
# before committing, then run `make test`. HEO_RECORD_TEAMS_REHEARSE=1
# reaches nothing: see the guide.
record-teams:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-teams: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	@if [ -z "$$HEO_RECORD_TEAMS_CONNECTOR_TOKEN" ] || [ -z "$$HEO_RECORD_TEAMS_APP_ID" ] || [ -z "$$HEO_RECORD_TEAMS_TENANT_ID" ] || [ -z "$$HEO_RECORD_TEAMS_SERVICE_URL" ] || [ -z "$$HEO_RECORD_TEAMS_USER_ID" ] || [ -z "$$HEO_RECORD_TEAMS_CONVERSATION_ID" ]; then \
		echo "make record-teams: set HEO_RECORD_TEAMS_CONNECTOR_TOKEN, HEO_RECORD_TEAMS_APP_ID, HEO_RECORD_TEAMS_TENANT_ID, HEO_RECORD_TEAMS_SERVICE_URL, HEO_RECORD_TEAMS_USER_ID and HEO_RECORD_TEAMS_CONVERSATION_ID (your own bot, tenant and user)" >&2; exit 1; fi
	HEO_RECORD_LIVE=1 XRR_MODE=record go test -buildvcs=false -count=1 -run '^TestXRRTeams' ./tests/e2e/

# record-llm re-records the cassettes of the LLM helpers (summarize, triage,
# draft) against a model served on this machine; replay (`make test`) needs
# none. LLM_ENDPOINT names the local server (default: Ollama's own port);
# the recorded model is the one tests/e2e/llm_test.go names, which the
# server must have pulled. Only a loopback or private-network endpoint is
# accepted, the conversation is synthetic, and a cloud model is never
# recorded. Refused when CI is set. Read `git diff tests/e2e/testdata`
# before committing. See docs/DEVELOPING.md.
LLM_ENDPOINT ?= http://127.0.0.1:11434
record-llm:
	@if [ -n "$$CI" ] && [ "$$CI" != "false" ] && [ "$$CI" != "0" ]; then \
		echo "make record-llm: refused, CI is set; record locally and commit the cassettes" >&2; exit 1; fi
	HEO_E2E_LLM_ENDPOINT=$(LLM_ENDPOINT) XRR_MODE=record go test -buildvcs=false -count=1 -timeout 30m -run 'TestRecordedLLM' ./tests/e2e/
	go test -buildvcs=false -count=1 ./internal/xrrx/
	@git status --short tests/e2e/testdata

# conformance runs the 12-factor AI-CLI gate (spec-12fc's pipeline) and
# rewrites the evidence in docs/conformance, tests/conformance and
# .12fc.json. It is not part of `make check`: it needs kit, jq, python3 and
# the private scenario library (HEO_SCENARIOS; without it the graded
# factors report skip), and the gate script clones spec-12fc unless
# SPEC_12FC_DIR names a checkout. About 5 s after the build. The static
# checks that need no tools are Go tests and run in `make test`. See
# docs/DEVELOPING.md.
conformance:
	scripts/conformance.sh

lint:
	golangci-lint run ./...

# docs-gen rewrites the command reference and both cheatsheets from
# `heo spec` and the command tree; commit the result. docs-check fails
# when the committed files are not what docs-gen would write (about 1 s;
# `make test` runs the same check).
docs-gen:
	go test -buildvcs=false -count=1 -run '^(TestGeneratedDocsAreCurrent|TestProviderMatrixIsCurrent)$$' ./cmd -update-docs

docs-check:
	go test -buildvcs=false -count=1 -run '^(TestGeneratedDocsAreCurrent|TestGeneratedDocsFitTheLineBudget|TestRefdocMetaMatchesTheTree|TestGrammarCatalogIsAccountedFor|TestProviderMatrixIsCurrent)$$' ./cmd

# spec fails when any command lacks a kit/side-effect annotation.
spec: build
	./bin/heo spec coverage --min 100 >/dev/null

links:
	@if command -v lychee >/dev/null 2>&1; then \
		lychee --no-progress .; \
	else \
		echo "lychee not installed; skipping link check"; \
	fi

clean:
	rm -rf bin/ dist/

setup:
	go mod download
	@command -v lychee >/dev/null 2>&1 || cargo install lychee

release:
	goreleaser release --clean

promote:
	@scripts/promote-release.sh

promote-alpha promote-beta promote-rc promote-release:
	@scripts/promote-release.sh $(subst promote-,,$@)
