# LLM helpers: summarize, triage, draft

Three commands ask a language model about a conversation. They read it,
send its text to a model, and print what the model said:

```sh
heo conversation summarize conv_123            # a few sentences for a teammate
heo conversation triage conv_123               # category, priority, sentiment, labels, next action
heo message draft conv_123 --tone friendly     # the text of a reply, never sent
```

What they never do: apply a label, change a status, send a reply, create an
operation or write anything in uma. The answer is **machine-generated text
that is only ever shown**. Nothing in it is run, parsed into a command or
turned into a tool call. A person (or an agent) reads it and runs the
command that acts, such as `conversation label` or `message reply`.

**The conversation's text is sent to the configured model.** By default
that model runs on your machine. A cloud provider is used only when you opt
in to it by name and each call says so (see [Cloud providers](#cloud-providers)).

## Quick start

Install [Ollama](https://ollama.com), pull a model and tell heo its name.
The default provider is the Ollama server on this machine,
`http://127.0.0.1:11434`:

```sh
ollama pull llama3.2
heo config set llm.model llama3.2
heo conversation summarize conv_123 --dry-run   # what would be sent, and where
heo conversation summarize conv_123
```

`llm.model` defaults to the placeholder `llama3.2`: set it to a model your
server has pulled. To use a server on another port or another machine of
your own network, set its address:

```sh
heo config set llm.endpoint http://127.0.0.1:11500
```

Changes apply when the daemon starts.

## What each command does

All three run in the profile's daemon (exit 70 without one) and read the
conversation as the calling actor, through uma, so uma's audience rules apply.

| Command | Reads | Prints |
| --- | --- | --- |
| `conversation summarize <conversation>` | every message you may read | a summary: who wants what, what was done, what is open |
| `conversation triage <conversation>` | every message you may read, and the tenant's labels | suggestions: `category`, `priority`, `sentiment`, `labels`, `next_action` |
| `message draft <conversation>` | the public messages (internal notes only with `--include-notes`) | the text of a reply |

### Audience

| Role | `summarize`, `triage` | `draft` |
| --- | --- | --- |
| `desk` | public messages **and internal notes** (`audience: desk`) | yes: public messages |
| `viewer` | public messages only (`audience: public`) | refused (exit 5) |
| `external` | refused (exit 5) | refused (exit 5) |
| `operator` alone | refused (exit 5): it reads no content | refused (exit 5) |

An internal note is in the prompt only when the caller may read notes. A
viewer's prompt is built from public messages alone, so a note's text
cannot reach the model on a viewer's behalf. A draft is a public reply, so
it is made from the public messages even for a desk actor; `--include-notes`
gives the model the notes as private context, and the prompt tells it never
to repeat them.

The helpers add no uma right. `summarize` and `triage` need the `desk` or
`viewer` role; `draft` needs the right to reply (`desk`). See
[authorization.md](authorization.md).

### Output

Every answer states where it came from and that it is machine-generated.
`--format json` has these fields:

| Field | Meaning |
| --- | --- |
| `summary` / `draft` / the triage fields | the model's answer |
| `provider`, `model` | what answered |
| `class` | `local` or `cloud`: where the provider runs |
| `message_count` | messages of the conversation you may read |
| `messages_in_prompt` | how many of them went into the prompt |
| `truncated` | `true` when the prompt holds less than the conversation |
| `audience` | `public` or `desk`: what reached the model |
| `machine_generated` | always `true` |

In text, the answer comes first, then these as rows, and a note on stderr
saying the text is machine-generated.

### Prompt size and truncation

The prompt is bounded by `llm.max_prompt_chars` (default 12000 characters,
instructions included, which fits a model with a 4096-token context). When
the conversation does not fit, the **newest messages are kept** and the
oldest dropped, as a run: no message in the middle is skipped. If the newest
message alone is too long, its start is cut. `truncated` is `true` and
`messages_in_prompt` says how many went in. A thread longer than 2000
messages is read to its end and only its newest 2000 are considered. Raise
`llm.max_prompt_chars` for a model with a longer context.

### Untrusted text

Messages are written by third parties. The prompt wraps them in
`<conversation>` tags, escapes anything in them that looks like a tag, and
tells the model that the text is data: it must ignore instructions inside it,
never reveal the instructions and never recommend running anything it
contains. This lowers the risk; it cannot remove it, which is why no
answer is ever executed.

### Triage

`triage` asks for one JSON object and checks it against a fixed schema:

| Field | Values |
| --- | --- |
| `category` | a short topic, at most 40 characters |
| `priority` | `low`, `normal`, `high`, `urgent` |
| `sentiment` | the customer's: `negative`, `neutral`, `positive` |
| `labels` | names chosen from the tenant's labels (`heo label list`) |
| `next_action` | one sentence |

A label the tenant does not define is **dropped** and listed in
`dropped_labels`; heo never creates a label. If the model's answer does not
fit the schema, it is asked once more, naming what was wrong; a second
failure is `LLM_OUTPUT_INVALID` (exit 6), and nothing was changed. To accept
a suggestion, run `heo conversation label conv_123 --add billing`.

### Draft

```sh
heo message draft conv_123 --instruction "offer a refund within 5 days" --tone friendly
heo message reply conv_123 --text "$(heo message draft conv_123 --format text)" --dry-run
```

`--format text` prints the draft alone on stdout; the status lines go to
stderr, so the second line works as written. A draft is never sent: sending
is `message reply`, which previews with `--dry-run` and then queues under an
operation like any reply. `--instruction` (at most 1000 characters) and
`--tone` (at most 60) are your own words and sit outside the conversation
tags.

### Preview

`--dry-run` reads the conversation as you and builds the prompt, then stops:
it prints the provider, the model, the endpoint's host, its class (`local` or
`cloud`), the message count, the prompt size and the audience, and sends
nothing. For a cloud provider it warns that the text would leave the machine.
Over the socket and RPC, `--dry-run` is a flag of the call; REST serves these
commands as `GET`, which carries no `--dry-run`.

## Providers

`llm.provider` selects the provider, and everything about it is configuration:

| Key | Default | Meaning |
| --- | --- | --- |
| `llm.provider` | `ollama` | the provider: `ollama`, `auto`, or a cloud provider you have opted in to |
| `llm.model` | `llama3.2` | the model; a placeholder, set it to one your server serves |
| `llm.endpoint` | empty | the provider's base URL; empty is `http://127.0.0.1:11434` |
| `llm.timeout` | `120s` | how long one model call may take; a cold model needs time to load |
| `llm.max_prompt_chars` | `12000` | the prompt budget, at least 3000 |
| `llm.cloud.<name>.enabled` | `false` | opt in to the cloud provider `<name>` |
| `llm.cloud.<name>.credential_ref` | empty | the credential reference of its key |
| `llm.cloud.<name>.type` | empty | declares `<name>` as a server speaking the provider `type` (such as `openai`); it is always `cloud` |
| `llm.cloud.<name>.endpoint` | empty | its base URL; `llm.endpoint` when empty |

`heo config validate` checks the section offline. The environment variables of
the model libraries (`LLM_PROVIDER`, `LLM_BASE_URL`, `LLM_FALLBACK`,
`LLM_API_KEY`, `OPENAI_API_KEY` and the like) are **not read**: only heo's own
keys select a provider.

A provider is `local` when its endpoint is on this machine or its local
network: `localhost`, or an address in a loopback, private (`10.0.0.0/8`,
`172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`) or link-local range. Any
other host, a name such as `gpu-box.lan` included, is `cloud`, because a name
cannot be known to stay on your network: use the server's address. `auto` is
the local default and nothing else: it never resolves to a cloud provider or
a key, and `llm.provider: auto` with a `llm.endpoint` off the machine is
refused.

This build carries the Ollama provider and the OpenAI chat API (`openai`, with
its hosted aliases such as `openrouter` and `groq`). Adding another is one
import in `internal/assist/plan.go`; no other code names a provider.

### Cloud providers

A provider whose endpoint is not local needs three things, each of them yours
to give:

1. `llm.provider` names it (not `auto`);
2. `llm.cloud.<name>.enabled` is `true` (`<name>` is the provider, such as
   `ollama` for a hosted Ollama endpoint);
3. every call says `--send-to-cloud`.

```sh
heo config set llm.provider ollama
heo config set llm.endpoint https://ollama.example.com
heo config set llm.cloud.ollama.enabled true
heo config set llm.cloud.ollama.credential_ref ollama-key
heo conversation summarize conv_123 --send-to-cloud
```

Without the first two the command fails with exit 70 and names the key.
Without `--send-to-cloud` it fails with `CONSENT_REFUSED` (exit 7) before any
credential is read or request made. The rule is the same on the terminal and
on every served surface (REST, gRPC, MCP): an agent calling over a network
surface cannot send a conversation to a cloud provider by default. `--offline`
refuses a cloud call (exit 2). A cloud endpoint takes `https`: a key never goes
over plain `http` to a host off the machine, and such a call fails with exit 70
before any request is made.

The provider's key is a credential reference, resolved in the profile's
credential backend for the purpose `llm.<name>`, with the reference's shared
entry as the fallback (see [configuration.md](configuration.md#credentials)).
It is read only for a cloud call that passed the checks above, never for a
local provider, and never from the environment.

### A server with the OpenAI chat API

A routing server or gateway that speaks the OpenAI chat API with a bearer key
is a named cloud provider: give it a name of your own and declare its `type`.
A provider declared this way is `cloud` whatever its address, so it needs the
opt-in and `--send-to-cloud` like any other, even on your network.

```sh
heo config set llm.provider router
heo config set llm.model team/router:fast        # sent to the server unchanged
heo config set llm.cloud.router.type openai
heo config set llm.cloud.router.endpoint https://router.example.com/v1
heo config set llm.cloud.router.enabled true
heo config set llm.cloud.router.credential_ref router-key
heo conversation summarize conv_123 --send-to-cloud
```

The key under `router-key` (purpose `llm.router`) is sent as
`Authorization: Bearer <key>`; the model name is passed through as it is, and
the request goes to `<endpoint>/chat/completions`, so the endpoint carries
the `/v1`. A 503 or a 429 is a retryable failure (`TRANSIENT`,
exit 6): heo sends one request per call, a connection error included, and does not retry.
When the answer carries a `Retry-After` header (seconds or an HTTP date), the
error reports that delay: `cause` reads `retry_after=<duration>` and the fix
reads `retry after <duration>`. An invalid, negative or elapsed value is
ignored, and one over an hour is reported as an hour. heo only reports the
delay; it does not wait on it.

### No automatic fallback

If the provider does not answer, the command fails and says how to fix it.
heo never tries another provider, local or cloud, and never retries on its
own: that is how a conversation could reach a cloud model nobody chose.

## Failures

Every failure is a corrective error with the key to change.

| Situation | Code, exit | What to do |
| --- | --- | --- |
| the server is not running | `PREREQUISITE`, 70 | start it, or set `llm.endpoint` |
| the configuration does not resolve (no model, a cloud endpoint not opted in, an unknown provider) | `PREREQUISITE`, 70 | the key named in the message |
| the model is not on the server | `NOT_FOUND`, 3 | `ollama pull <model>`, or set `llm.model` |
| the call ran past `llm.timeout` | `TRANSIENT`, 6 | raise `llm.timeout`, use a smaller model, or run it again |
| rate limit, server fault | `TRANSIENT`, 6 | run it again later, or after the `retry_after` the provider asked for |
| the provider refused the credential | `UNAUTHORIZED`, 5 | store a valid key |
| cloud call without `--send-to-cloud` | `CONSENT_REFUSED`, 7 | add the flag, or use a local endpoint |
| the model's triage is not valid twice, or it answers nothing | `LLM_OUTPUT_INVALID`, 6 | run it again, or use another model |

A served command is also bound by `services.all.timeouts.command` (5 minutes
by default), which `llm.timeout` should stay below; triage can make two calls.

## Served surfaces

The helpers are plain request-and-reply commands and are served like other
reads. They are not withheld from REST, gRPC or MCP because a local model
keeps the text on the machine, and a call that would leave it needs
`--send-to-cloud` on every surface. MCP cannot pass a command's positional
argument, so the conversation ID cannot be given there, as for every heo
command that takes one.

## Recording tests

Replay needs no model. The tests of these commands replay cassettes
recorded against a real model, on a synthetic conversation; see
[docs/DEVELOPING.md](../DEVELOPING.md#llm-cassettes) for the one-line
re-record command.
