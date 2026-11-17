# Authorization: who may do what

Each profile's daemon decides every call against one authorization
policy, read from the profile's configuration. The policy denies by
default: an actor gets only the roles the configuration grants it.

The same policy decides on every surface, so the control socket, REST,
gRPC and MCP all give the same answer. It also decides inside the
runtime: every read, every command, and every queued reply or note,
which a worker checks again just before the provider sees it.

## Out of the box

You do not need to configure anything to work alone on your own
machine. The uid the daemon runs as is the installation owner, and it
holds the `desk` and `operator` roles: it works conversations and
configures channels and inboxes. Nobody else holds anything:

```yaml
authorization:
  owner: [desk, operator]
```

`heo config get authorization.owner --format json` shows the effective
value and where it came from (`default` until you set it).

`heo daemon ping` shows the actor the daemon serves you as:

```sh
heo daemon ping
```

```text
actor      uid:501
```

## Actors

The daemon works out the actor itself and never takes it from the
request (see [control.md](control.md#who-a-command-acts-for)):

- on the control socket, the actor is `uid:<n>`, the peer's uid as the
  kernel reports it;
- on REST, gRPC or MCP over HTTP, the actor is the subject of the
  credential the surface verified. For a heo surface token, that is
  `uid:<n>` of the user who minted it.

## Roles

| Role | May |
|------|-----|
| `operator` | configure channels and inboxes; decide held operations (`operation.approve`, `operation.reject`); import earned trust (`trust.write`, `operation trust import`: [deliveries-and-events.md](deliveries-and-events.md#import-trust)); import a channel's provider history (`channel.import`); read, reconcile and retry deliveries (operations), find operations by key; privacy redaction and export; override identity-link assurance; list and test hook plugins and read their dead letters (`heo.hook.read`), drop a dead letter (`heo.hook.manage`): [hooks.md](hooks.md#operating-the-runner) |
| `desk` | read conversations, messages, attachments and events, **internal notes included**; read and reconcile operations (an unknown delivery outcome), and find its own by key; decide held operations (`operation.approve`, `operation.reject`: [approvals.md](approvals.md)); reply, add notes, transition, start and label conversations; define labels; edit, delete and react to messages (`message.edit`, `message.delete`, `message.react`, on the platforms whose adapters declare them: [provider-verbs.md](provider-verbs.md)); add and remove participants (`conversation.membership`) and send receipts (`message.receipt`) likewise; import a conversation's provider history (`channel.import`, with the right to read the conversation); assign, snooze and link conversations and keep their own read marker (`conversation.assign`, `conversation.snooze`, `conversation.link`, `conversation.mark_read`), and read that workflow state (`conversation.read_workflow`); read and answer the interactions a provider reports (`interaction.read`, `interaction.respond`, on the platforms whose adapters declare `respond`: [provider-verbs.md](provider-verbs.md)), and open, stack and update a Slack modal in answer to one (`extension.execute`, on the channels whose adapters ship the effects); read contacts and identities, create, edit and merge contacts, link and unlink identities; read and act on work items and tasks, including setting the status of an issue or ticket and changing its fields through the conversation verbs (`work_item.act`); tag a mail message at Gmail or Outlook (`conversation.label`, the action of `conversation label`, on the channels whose adapters ship the effect: [provider-verbs.md](provider-verbs.md)) |
| `viewer` | read conversations, messages, reactions, participants, attachments, events, contacts, identities, labels, work items (issues, tickets), tasks and interactions: public content only; also a conversation's assignee, snooze and links (`conversation.read_workflow`), never another actor's read state |
| `observer` | everything `viewer` reads, plus channels (`channel.read`) and operations (`operation.read`): the reads the event bridge holds, for a plugin that receives events about both; no write |
| `external` | read conversations, messages, attachments and events: public content only; no assignee, snooze, link or read state, on any surface |

LLM helpers ([llm-helpers.md](llm-helpers.md)): `desk` and `viewer` run `conversation summarize` and
`conversation triage` (a viewer's prompt holds public messages only); `message draft` needs the right
to reply, `desk`. `external` and an `operator` alone run none. They add no uma right.

Tasks (`heo task`, [tasks.md](tasks.md)): `desk` and `viewer` read them (`task.read`), `desk` cancels (`task.act`) and watches them (task events are internal); `external` reads none.

An actor can hold several roles, and they add up. `operator` gives no
read access to conversation content: give the same actor `desk` as well
if it needs both.

### Held operations

With `held_operations.enabled`, a `plugin:` actor's write is held for a
person's decision ([approvals.md](approvals.md)). Who may do what:

| Action | Held by | Roles that hold it |
|--------|---------|--------------------|
| `operation.read` (`operation held list`, `show`, `history`, `operation trust list`) | reads the held operation and its audit trail, as the caller may read them | `desk`, `operator` |
| `operation.approve` (`operation held approve`, by name or by selector) | applies the proposal as proposed | `desk`, `operator` |
| `operation.edit` (`operation held edit`) | applies the proposal with corrected arguments | `desk`, `operator` |
| `operation.reject` (`operation held reject`) | applies nothing | `desk`, `operator` |
| `operation.approve` (`operation trust set`) | moves a trust key's count by hand: promote, demote, reset | `desk`, `operator` |
| `trust.write` (`operation trust import`) | seeds the trust counters that decide which plugin writes run unreviewed | `operator` |

`viewer` and `external` read no operations. A plugin actor is never
allowed to approve, edit, reject, set trust or import trust, whatever roles it holds: the policy
refuses it with `heo.authz.plugin_decide` (exit 5; REST 403; gRPC
`permission_denied`; an MCP tool error) and a decision made some other
way is refused again at the command. Granting a plugin `desk` lets it
propose writes; it adds no right to decide.

The same holds on REST, gRPC and MCP: an actor is the credential's
subject, a heo surface token is a person, and a token whose subject is a
`plugin:` actor (from a kit authenticator, see
[surfaces.md](surfaces.md#another-authenticator)) proposes and reads but
never decides. A chat approval decides as the actor its sender is mapped
to; a `plugin:` actor there is a configuration error.

### Extension effects

`conversation update`, `conversation tag`, `message reply-draft`,
`conversation unsubscribe` and the Slack
modal verbs (`interaction open-modal`, `push-modal`, `update-modal`) run
extension effects: changes an adapter ships of its own. uma authorizes each
as the policy action its definition names, as the caller, at submission and
again before every attempt: `work_item.act` for an adapter's work item update
effect (GitHub's issue update, Zendesk's ticket update), `conversation.label` for Gmail's label change and
Outlook's category change, `extension.execute` for Slack's three modal
effects. heo adds no action for them, and `desk` already holds all three,
so a `viewer` or `external` actor is refused (exit 5), preview included. `conversation.label` is the action of `conversation label` too:
a grant that lets an actor label conversations locally lets it tag mail
messages at the provider, on the channels whose adapters ship the effect.
The effects run as an internal operation: only desk actors see it and its
events.

The conversation state verbs (`conversation archive`, `unarchive`, `move`,
`read`, `unread`, `flag`, `unflag`, `trash`, `untrash`) are not extension
effects: uma carries them as provider conversation state and authorizes each
as `conversation.state_account` when the channel reports account scope (a
mailbox's own view) or `conversation.state_shared` when it reports shared scope
(what everyone sees, as a chat room), again before every attempt. `desk` holds
both, so a `viewer` or `external` actor is refused (exit 5), preview included.

`extension.execute` is uma's generic action for an extension effect that
names no more specific one; the `desk` role holds it, and today Slack's
modal effects and the mail adapters' mailbox create (`provider-label create`,
aimed at the channel) name it. The mailbox list (`provider-label list`) is a read
authorized as `extension.query`, which `desk` and `operator` hold. uma's effect call is aimed at a conversation, not at
an interaction, and checks nothing of the interaction, so heo makes the checks
the engine does not before it queues a modal: the caller must hold `interaction.respond` too, the right to answer
interactions, and may read the interaction (one it may not read does not
exist for it); the interaction must not be redacted, must carry a
trigger_id and must still be inside its window. Who pressed the button is
Slack's to bind: it refuses a trigger_id to anyone but the person it was
sent for. See [Slack modals](provider-verbs.md#slack-modals).

### Internal notes and restricted content

A message whose audience is not public, such as an internal note or a
restricted message, is for `desk` actors only. The same goes for its
attachments and for the events that carry it. For a `viewer` or an
`external` actor, the message does not exist: lists skip it, a direct
read is refused, and its events never reach that actor's subscriptions.
Having access to the conversation is not enough to see its notes.

### The runtime's own actor

Receivers, webhooks and workers act as `svc:heo-runtime`. That actor can
read channels, accept ingress on them and stage the media that arrives
with it, and nothing else. Provider content enters as this actor, so an
inbound message can never reply, change configuration or read anything
on anyone's behalf. A grant that names a `svc:` actor is a configuration
error.

That is also why this build cannot serve uma's `a2a.messages` adapter: its
task endpoint reads tasks, conversations and messages, and records a
remote agent's cancel request, as this actor, which holds none of
`task.read`, `conversation.read`, `message.read` and `task.act`. Granting
them is a decision about what provider content may read, not a
configuration option.

### The event bridge's actor

The event bridge acts as an actor of its own, `svc:heo-bridge`, not as
the runtime's, so what it may read is never open to provider content.
It holds the subscription actions (open, poll, acknowledge) and the
reads uma asks of a subscriber before it delivers an event: inbox,
channel, contact, identity, conversation, message, operation, task, work
item, attachment, stream and interaction reads, at every audience, since
it republishes references to internal events too and keeps them for desk
subscribers (see
[deliveries and events](deliveries-and-events.md#events-on-the-daemons-bus)).
It holds no write, no configuration, no event list, no close and no
`label.read`, and no daemon command admits it; a grant that names it is
a configuration error. Events of an extension kind need that kind's own
action, which it does not hold, so they are not bridged.

### A plugin's actor

A hook plugin acts as `plugin:<name>` (lowercase letters, digits and `-`).
Unlike a `svc:` actor it is grantable: it holds exactly the roles
`authorization.grants` gives it and nothing else, so a plugin can never
act with the runtime's or the owner's reach, and audit tells it apart
from both. No role is implied. A plugin that only observes events and
reads what they reference needs `observer`, which holds the subscription
actions (open, poll, acknowledge) and the content reads; one that acts
on what it sees needs `desk` besides. Events about channels and
operations need `channel.read` and `operation.read`, which `observer`
holds and `viewer` does not, so a `viewer` plugin does not receive them.
Internal notes and their events need `desk`, as for any actor.

The control socket never admits a plugin: it names peers by uid, and a
`plugin:` actor on the socket is refused whatever the policy says. A
plugin authenticates on REST, gRPC and MCP with a token of its own:

```sh
heo plugin token mint triage      # prints the token once
heo plugin token revoke triage
```

A request bearing the token acts as `plugin:triage` and as no other
actor, however it is presented. The token is stored in the credential
backend, apart from the profile's surface token; minting again replaces
it, revoking deletes it, and either takes effect on the next request.
See [surfaces.md](surfaces.md#plugin-tokens).

## Granting access

`authorization.grants` lists actors and the roles each one holds:

```yaml
authorization:
  owner: [desk]                 # you: conversations only, no channel configuration
  grants:
    - actor: uid:1002           # a colleague on this host
      roles: [desk]
    - actor: uid:1003
      roles: [viewer]
    - actor: partner-portal     # a token subject on an HTTP surface
      roles: [external]
    - actor: plugin:triage      # a hook plugin, by its own token
      roles: [viewer]
```

`authorization.owner` lists the roles of the uid the daemon runs as;
unset, it is `[desk, operator]`. Setting it replaces that default, so
list every role you keep. Set `owner: []` to take every role away from
it; it then holds only what a grant gives it. From the command line or
the environment, a
comma list works too: `-c authorization.owner=desk`, or
`HEO_AUTHORIZATION_OWNER=desk`.

Changes take effect when the daemon starts.

A grant must be reachable to be of any use. The control socket admits
only the daemon's own uid by default
(`services.socket.auth.peer.require_same_uid`). Another uid reaches the
daemon only when that setting is `false`, and a token subject only
through an enabled HTTP surface. `config validate` warns about a grant
that no enabled surface can reach.

### aps-bound profiles

Binding a profile to aps (`--profile aps:<id>`) grants no authority.
The actor is still the calling uid, and the aps profile is context only.
A grant whose actor names a profile (`aps:<id>`, `heo:…`) is refused.

## Refusals

| Situation | Answer |
|-----------|--------|
| the actor holds no role | refused before the command runs: `UNAUTHORIZED`, exit 5 at the terminal (`DENIED` on the socket wire); REST 403 `permission_denied`; gRPC `permission_denied`; an MCP tool error. Each one carries the reason `heo.authz.no_role` |
| the actor's roles do not grant the operation | the command fails with `UNAUTHORIZED`, exit 5, in its error envelope, the same on every surface |

A refused command never runs, so do not retry it unchanged: ask the
installation owner for a grant. A queued reply or note whose actor has
lost its grant by the time a worker picks it up fails `unauthorized`
and never reaches the provider.

## Checking the policy

```sh
heo config validate
```

This runs offline. The following are errors: an unknown role, an
unknown field in a grant, a missing or invalid actor, an actor that
names a profile, a reserved (`svc:`) actor, and a `plugin:` actor whose
name is not a plugin name. The following are warnings: a
grant with no roles, an actor granted twice (the roles add up), and a
grant no enabled surface can reach.

`heo service start` refuses to start when the policy has an error
(exit 2), because a daemon whose policy it cannot read would refuse
every caller.
