# Conversations, messages and notes

Work the inbox from the terminal: read what came in, reply, start an
exchange, keep internal notes, fetch attachments, label, assign, snooze,
link and resolve conversations.

- A **conversation** is uma's local record of an exchange in one
  inbox. Its status is `open`, `pending`, `resolved` or `closed`, and
  that status is local: resolving a conversation changes nothing at the
  provider. A GitHub issue's or Zendesk ticket's own status is the
  provider's, and shown beside the conversation's, which is an issue or a
  ticket itself: see [issues-and-tickets.md](issues-and-tickets.md).
  What a conversation is for each provider (a mail thread, a Slack room or
  thread, an issue) is set by the inbox's mode; see
  [provider-mapping.md](provider-mapping.md).
- A **message** is one entry in a conversation: inbound from the
  provider, or written through uma (a reply, a first message, a note). Every message has
  a visibility: `public`, or `internal` for a note.
- A **note** is an internal message, for desk actors alone. It is never
  public and never a reply.
- An **attachment** is a file a message carries: a customer's upload,
  a mail attachment. uma keeps its bytes in the profile's attachment
  directory and its metadata beside the message; `heo storage backup`
  carries both.
- A **label** is a name a conversation can hold, such as `billing`.
  Labels are defined once for the whole tenant and reused across every
  inbox and channel.
  A mail provider's own labels and categories are another thing, changed with
  `conversation tag` ([below](#tag-a-mail-message-at-the-provider)).

Every command here runs in the profile's daemon, which owns the data
store. Start it first with `heo service start`; with none running,
these commands exit 70. Channels and inboxes come first: see
[channels.md](channels.md).

IDs are record IDs, as `list` prints them (`conv_123`, `msg_123` in the
examples below).

## Read the inbox

```sh
heo conversation list                         # every inbox you may read
heo conversation list --inbox web --status open,pending
heo conversation list --work-item --work-item-status open   # issues and tickets, by the work item's status
heo conversation list --sort last-activity --updated-since 2026-10-01T09:00:00Z   # newest change first, only those changed after
heo conversation get conv_123                 # status, reply route, revision
heo message list --conversation conv_123      # its messages, oldest first
heo message get msg_123
```

Each listed conversation shows its last activity (`last_activity`): the
newest of its newest message you may read (imported history does not
count), the time its snooze was set and the time you last moved your own
read marker. `--sort last-activity` lists the newest change first, so a
conversation moves to the top when a message arrives, a read marker moves
(marking unread included) or a snooze starts. `--updated-since` keeps the
conversations whose last activity is after a time, for a client that
refreshes incrementally. uma keeps no activity time, so heo derives it by
reading each conversation the other filters keep: a sorted listing reads
them all before its first page, and its cursor names the last item of its
page.

Listings aggregate across every inbox you may read, a page at a time:
`--limit` (1 to 100, default 50) and `--cursor` with the value the
previous page printed (`more: --cursor ...` on stderr, `next` in JSON).
Each row shows where it came from:

| Column | Meaning |
| --- | --- |
| `INBOX` | the inbox the conversation belongs to |
| `VISIBILITY` | `public`, or `internal` for a note |
| `ORIGIN` | `inbound <adapter>` from the provider, or `uma <actor>` for a reply or note and who submitted it |

`message list` filters by `--inbox` (key or ID), `--conversation` and
the search filters ([Search messages](#search-messages)). `conversation
list` filters by `--inbox`, `--status` and labels ([Labels](#labels)).
`--format json` returns uma's records as they are.

For a cross-inbox attention or activity feed, see [desk-feeds.md](desk-feeds.md): what
these listings and the event log give a desk client, and what is missing.

## Search messages

Search is `message list` with more filters; there is no separate command.

```sh
heo message list --text "invoice refund"            # holds both words
heo message list --text refund --since 2026-10-01 --until 2026-11-01
heo message list --text refund --label billing --label-none spam
heo message list --label-all billing,urgent --inbox web
```

- **`--text`** keeps messages whose text holds every word given, in any
  order, ignoring case. Words match whole: `refund` does not match
  `refunded`. Only message text is searched, not subjects, attachment
  names or the text of a redacted message. uma limits the text (256
  bytes, 16 words); a text with no word, or past a limit, is
  `VALIDATION` (exit 2).
- **`--since`, `--until`** bound the message's own time, from `--since`
  up to but not including `--until`. Each takes an RFC 3339 time
  (`2026-10-01T09:00:00Z`) or a date (midnight UTC). Anything else is a
  usage error (exit 2), and so is a `--since` that is not before
  `--until`. For mail, the message's time is its `Date` header.
- **`--label`, `--label-all`, `--label-none`** work as on `conversation
  list` ([Labels](#labels)) and select messages by the labels their
  conversation holds when you read the page, so a label removed stops
  matching at once. An unknown name is `NOT_FOUND` (exit 3).
- Filters combine with each other, `--inbox` and `--conversation` by
  AND.

uma does the searching, for you: it looks at no message you may not read,
so an internal note matches only for a desk actor. A `viewer` or `external`
caller searching for a note's words finds nothing, and cannot tell
whether such a note exists. heo keeps no index and does not filter the
results again, apart from checking, as it does on every read, that no
internal content reaches a caller who may not see it. Results come in
the order uma recorded the messages, with no ranking.

A search page can be short, or empty, and still not be the end: uma looks
at a bounded number of stored matches per call, and the page says so.
Keep going while the listing is incomplete: `more: --cursor ...` on
stderr, `complete: false` in JSON. Stop only on `complete: true`, never
on an empty page. A search over many messages you cannot read (for
example, a great many internal notes) can show that such messages exist,
by the number of empty pages it takes to reach the end; uma documents
this limit.

## Reply

```sh
heo message reply conv_123 --text "Thanks, we're on it." --dry-run
heo message reply conv_123 --text "Thanks, we're on it."
```

A reply is always public. It leaves through the conversation's inbox,
on the reply route uma stored for the conversation, and answers the
latest inbound message you may read.

`--dry-run` shows exactly where it goes and who sees it — inbox,
channel, destination (the reply route), the recipients it adds and
visibility — and writes nothing.

### Reply to all, cc and bcc

A reply goes to the reply route's address alone, unless it names more:

```sh
heo message reply conv_123 --text "Looping you in." --reply-all --dry-run
heo message reply conv_123 --text "Looping you in." --cc lead@example.com,ops@example.com --bcc audit@example.com
```

- `--reply-all` adds the other recipients (To and Cc) of the message the
  reply answers. The adapter reads that message when it sends and drops
  the mailbox itself, so heo does not know the list in advance and
  neither the preview nor the output names it. A reply that answers no
  message adds none.
- `--cc` and `--bcc` add explicit bare addresses (`name@host`, no display
  name), repeated or comma separated, at most 50 in all. Addresses are
  normalized and deduplicated; one named in both lists keeps its cc
  place. A bcc address appears in no header any recipient sees.
- The route's address stays the first recipient: nothing removes it.
- They need `reply_recipients` on the conversation's channel (declared by
  `smtp.mail`, `imap.mail`, `google.mail` and `microsoft.mail`). A
  channel without it refuses, `UNSUPPORTED`, exit 2, in a preview and on
  submission, before anything is stored. `heo channel get <channel>` lists
  the capability, and `heo channel check <channel> --remote` shows its
  live state and the limits the adapter reports (reply-all, cc, bcc, and
  how many addresses a reply carries, the route's included).
- `--dry-run` reads those limits and refuses what they do not allow: a
  flag the channel does not send, or more addresses than it carries.
- A plugin actor's reply that adds recipients is held as class
  `reply:send`, and always held whatever the count: an added cc is a
  fact the trust ladder never lets through ([approvals.md](approvals.md)).
  The held reply keeps its recipients; `operation held edit` cannot
  change them.

The real command returns once uma accepted the reply durably. Its
operation is `queued`, never delivered: the daemon's workers deliver it
next, checking your permission again first. The output names the
operation; rerun with the same key to see it again.
`heo delivery get <operation>` follows it from there: see
[deliveries-and-events.md](deliveries-and-events.md).

## Change a message at its provider

`heo message edit` and `heo message delete` change a message on the
platform it lives on, and `heo reaction add` and `reaction remove` add
or remove your reaction. They work only on channels whose adapter
declares `edit`, `delete` or `react`, which `heo channel get` shows; any
other channel refuses them, exit 2. All four are queued like a reply: the
message changes when the provider applied the change. See
[provider-verbs.md](provider-verbs.md).

`heo reaction list --message M` shows the reactions on a message, and
`heo participant list --conversation C` the participants of a
conversation. `heo participant add` and `participant remove` change them
at the provider, `heo receipt send` tells the provider a message was
delivered or read, and `heo history fetch` reads its past messages into
heo; each works only where the adapter declares `membership`, `receipt`
or `history`. For a desk actor, `heo conversation get` shows which of
these the conversation's channel can do now.

A mail provider's own tags are changed with `heo conversation tag`
([below](#tag-a-mail-message-at-the-provider)); it needs `extension_execute`
and the adapter's label or category effect, which only `google.mail` and
`microsoft.mail` ship.
## Start an exchange

```sh
heo message send --inbox support --to address:ada@example.com --text "Your order shipped." --dry-run
heo message send --inbox support --to address:ada@example.com --text "Your order shipped."
heo message send --inbox sms --to phone:+15550123 --text "See you at 10." --idempotency-key remind-42
```

`message reply` answers a conversation that exists; `message send`
starts one, with someone who may never have written in. The message is
public and leaves through `--inbox` (key or ID).

`--to` is the recipient, as the route the channel sends on:

| `--to` | Recipient |
| --- | --- |
| `<kind>:<id>` | a route on the inbox's channel, in the adapter's identity namespace: `address:ada@example.com` (mail), `phone:+15550123` (SMS) |
| `<namespace>/<kind>:<id>` | the same, naming the namespace, when the adapter declares more than one (`heo adapter get <type>` lists them) |
| `identity:<id>` | an identity of the inbox (record ID): the route uma stored for its conversation |
| `contact:<id>` | a contact (record ID) with exactly one identity in the inbox; with several, pick one with `identity:<id>` |
| `room:<id>` | a provider room, in a `per_room` inbox; `slack.chat`, `telegram.messages`, `discord.chat` and `microsoft.teams` name it `<type>/room:<id>`, in their own namespace |
| `thread:<id>` | a provider thread, in a `per_thread` inbox |

A `per_room` or `per_thread` inbox is addressed by the room or thread,
never by a person: it takes only its own kind (`room:general`), and
refuses a sender route, the other kind or a bare id with `VALIDATION`
(exit 2) at the preview as for real. `identity:` and `contact:` need a
per-contact inbox. The preview names the room (`destination   room
general`), the first message is delivered to it, and a message that
arrives in the same room joins the conversation the send opened. See
[Choose a selection mode](channels.md#choose-a-selection-mode).

`thread:<id>` names a thread by its id alone, so it reaches the
conversation of a thread the provider reports without a room. A thread
the provider reports inside a room has that room as part of its
reference, which `--to` cannot write: answer such a thread with `message
reply` on its conversation, which routes to the thread and its room.

The conversation is chosen when uma accepts the message, by the inbox's
selection policy. The built-in rules reuse the recipient's open or
pending conversation, never reopen a resolved or closed one, and
otherwise create one; an email inbox without `lock_to_single` starts a
new thread each time. The output names the conversation the message
went to.

Only an adapter that declares `start_conversation` can send first:
`smtp.mail`, `imap.mail`, `twilio.sms`, `slack.chat`, `telegram.messages`, `relay.messages` (with a callback), `discord.chat` and `microsoft.teams` do; the widget, WhatsApp, Gmail,
Microsoft 365 and ticket adapters do not (see
[adapters.md](adapters.md)). Any other inbox is refused with
`UNSUPPORTED` (exit 2), at the preview as for real, and nothing is
written. A disabled inbox is `DISABLED` (exit 4).

`--dry-run` shows exactly where it goes and who sees it — inbox,
channel, adapter, destination (the route) and visibility — and that the
conversation will be selected or created, and writes nothing. The
destination is the route as the adapter spells it for the send: an
iMessage address typed in capitals previews lower-cased, with `as typed`
showing what `--to` said, only when the two differ. A recipient the adapter
refuses fails the preview as it fails the send (`VALIDATION`, exit 2).

As with a reply, the command returns once uma accepted the message
durably: its operation is `queued`, never delivered, and the daemon's
workers deliver it next, checking your permission again first. Rerun
with the same `--idempotency-key`, recipient and text to get the stored
operation back (`replayed`): nothing is sent twice, and no second
conversation is opened.

## Notes

```sh
heo note add conv_123 --text "Customer is on the legacy plan." --placement local
heo note list --conversation conv_123
heo note get msg_123
```

Where a note lives is the inbox's choice, set by the operator with
`heo inbox update <inbox> --deliver-notes=<bool>`:

| Placement | The inbox | The note |
| --- | --- | --- |
| `local` | keeps notes (`deliver notes false`) | commits in uma; nothing reaches the provider |
| `provider` | delivers notes (`deliver notes true`, adapter declares `internal_note`) | commits in uma and is queued to the provider as a private note |

`--placement` is required and must match the inbox. A mismatch is
`CONFLICT` (exit 4), naming the inbox and both fixes: the placement it
uses, or the `inbox update` that changes it. heo never moves a note to
the provider, or keeps it back, behind your back. `heo inbox get
<inbox>` shows `deliver notes`.

Who sees notes:

- `desk` actors read them: in `note list` and `note get`, and among the
  messages of `message list` and `message get`, marked `internal`.
- Any other role never sees one. The `note` commands refuse it
  (`UNAUTHORIZED`, exit 5, reason `heo.authz.desk_only`); `message
  list` leaves notes out; `message get` on a note is not found (exit 3).
  This holds in text and JSON, and on every surface: the control
  socket, REST, gRPC and MCP. heo checks every message it returns
  against your roles, besides uma's own check.

## Export a message's raw bytes

```sh
heo message raw msg_123
heo message raw msg_123 -o message.eml --force
heo message raw msg_123 -o - | sha256sum
```

`message raw` reads the provider's whole copy of the message `<message>`
(record ID) and writes it where `-o` says: for mail, the RFC 5322
message with every header and part, byte for byte as the provider serves
it (media type `message/rfc822`). Stdout is the default and carries the
bytes alone; `-o <file>` writes that file, owner-only (0600) and never over
an existing one without `--force` (exit 4); `-o <dir>` writes
`<message>.eml` in it. With `--format json` and `-o`, stdout reports the
file (message, media type, size, path), never the bytes.

It is a read: the provider is asked each time, and heo and uma keep
nothing. It is authorized as `message get` is (any role that reads the
message, under its audience). Served over REST, rpc or MCP it returns JSON:
`message`, `media_type`, `size` and `content`, the bytes base64.

Refusals: the channel's adapter does not declare `raw_read` (exit 2,
`UNSUPPORTED`; `google.mail`, `microsoft.mail` and `imap.mail` do); the
provider's copy is over the adapter's size bound (exit 1,
`PROVIDER_PERMANENT`, never truncated); the message was redacted or deleted,
or has no provider copy (exit 4, `CONFLICT`); the provider no longer holds it
or you may not read it (exit 3).

## Attachments

```sh
heo attachment list --message msg_123
heo attachment list --conversation conv_123
heo attachment get inb_1/att_1
heo attachment download inb_1/att_1
heo attachment download inb_1/att_1 -o invoice.pdf --force
heo attachment download inb_1/att_1 -o - | sha256sum
```

An attachment is named `<inbox>/<attachment>`, both record IDs: `list`
prints it as `ref`, and `get` and `download` take it. `list` shows each
attachment's name, media type, size, audience and the messages that hold
it (an attachment two messages share is listed once); `get` shows the
same for one, with its digest. Neither reads the bytes.

`download` writes the bytes where `-o` says:

| `-o` | The bytes go to |
| --- | --- |
| a file | that file |
| a directory | a file named after the attachment, in it |
| `-` | stdout, the bytes alone; the summary is a note on stderr |
| none | a file named after the attachment, in the current directory |

The attachment's name comes from the provider, so only its last element
is used: `../../x/photo.png` is saved as `photo.png`. heo never
overwrites a file: without `--force` an existing file is `CONFLICT`
(exit 4), checked before any byte moves; `--force` writes a sibling and
renames it into place, so the old file is replaced whole or not at all.
A new file is owner-only (0600). With `--format json`, a download to a
file reports the attachment and the path on stdout, and never the bytes.
uma checks the bytes against the length and digest it recorded; they
cross the control socket once, to the command, and reach no log. Over
REST, gRPC or MCP, where there is no terminal to write to, `download`
returns the bytes in the answer (base64 in JSON) and writes nothing on
the daemon's host.

uma decides every read against the attachment's own audience and the
messages that hold it, at the moment of the read, so what changes later
changes what the next command does:

- The provider narrows a message to internal content: a caller limited
  to public content can no longer read the attachment (`UNAUTHORIZED`,
  exit 5), and the message is not there for them either. An attachment
  two messages share stays readable while one of them is. A `desk`
  actor still reads it.
- A message is redacted (`operator`'s privacy tools): uma removes the
  bytes of every attachment it names, a shared one too. The attachment
  is gone (`NOT_FOUND`, exit 3, "gone"), and no listing shows it. A
  caller who reads only public content cannot tell a redacted
  attachment from one narrowed away from them, and gets `UNAUTHORIZED`
  for both.

`list` never shows an attachment you may not read. A page covers the
messages uma returned for it (`--limit`, `--cursor` with the previous
page's `next`) and is never empty while more follow.

## Labels

```sh
heo label create billing
heo label create urgent --description "Answer within the hour" --color "#d00000"
heo label list

heo conversation label conv_123 --add billing,urgent
heo conversation label conv_123 --remove urgent

heo conversation list --label billing,urgent        # holds any of them
heo conversation list --label-all billing,urgent    # holds all of them
heo conversation list --label-none spam             # holds none of them
heo conversation list --label billing --label-none urgent --inbox web
```

A label is uma's record, defined once for the tenant: one `billing`
serves every inbox and every channel. Labels are kept locally for every
channel, including channels whose provider has labels of its own; heo
never matches a provider's labels to these by name.

- **Names.** Unique under case folding: `Billing` and `billing` are one
  name, so creating the second is `CONFLICT` (exit 4). Every command
  finds a label by its name in any case. A name may not hold a comma,
  since names are passed as comma lists.
- **Adding and removing.** `--add` or `--remove`, one per call, each
  taking names repeated (`--add billing --add urgent`) or
  comma-separated (`--add billing,urgent`). Adding a label the
  conversation already holds, or removing one it does not, succeeds and
  changes nothing; the output says so on stderr. Labeling is local:
  nothing changes at the provider.
- **Unknown names.** A name no label holds is `NOT_FOUND` (exit 3) and
  nothing changes. heo never creates a label for you: the fix names
  `heo label create <name>`.
- **Filters.** `--label` keeps conversations holding any of the names,
  `--label-all` those holding all of them, `--label-none` those holding
  none. Each takes names repeated or comma-separated. Filters combine
  with each other, `--inbox` and `--status` by AND, across every inbox
  you may read. uma applies them; heo only turns names into labels. An
  unknown name is `NOT_FOUND` (exit 3); a name in both `--label-none`
  and another label filter is `VALIDATION` (exit 2).
- **Events.** uma emits one `conversation.label_added` or
  `conversation.label_removed` event per label it actually changed;
  adding a held label or removing an absent one emits nothing.

The output of `conversation label` lists the labels the conversation
holds after the write, by name, and those it added or removed.

### Keep a reply as a provider draft

`message reply` sends. `message reply-draft` leaves the reply in the
mailbox's own Drafts for a person to finish and send in their mail
client, and sends nothing. (`message draft` is a different thing: it
writes text with a model for you to read, and touches no mailbox.)

```sh
heo message reply-draft conv_123 --text "Thanks, we'll look into it." --dry-run
heo message reply-draft conv_123 --text "Thanks, we'll look into it."
heo message reply-draft conv_123 --draft r-77 --text "Updated wording."
heo message reply-draft conv_123 --draft r-77 --delete
```

- The draft is composed as a reply is: from the mailbox, to the
  conversation's reply route address, with the subject, `In-Reply-To` and
  `References` of the message the peer wrote, in that thread. It adds
  recipients only when asked: `--reply-all`, `--cc` and `--bcc` take the
  same values as on `message reply` ([above](#reply-to-all-cc-and-bcc)),
  need `reply_recipients` on the channel, and are held as an added cc.
  Gmail applies them on create and update; Outlook on create only, so a
  person's changes to an existing draft's recipients stay. A delete takes
  none. A conversation the peer wrote no message in still drafts; the
  provider then starts a new thread.
- It is the mailbox's draft, not a message of uma's: `message list`
  never shows it, and the conversation does not change.
- `--draft ID` is the provider's draft ID. The create reports none up
  front: the provider makes the draft when the daemon's workers run the
  operation, and `heo delivery get <operation>` shows it among the
  provider references. Use a new `--idempotency-key` for each update or
  delete.
- An update **overwrites** a person's edit made since, and a delete is
  permanent (the provider keeps no copy). Both touch only a draft this
  command made for this conversation; any other draft of the mailbox,
  one a person wrote included, is refused by the adapter (a final
  `VALIDATION` on the operation).
- Gmail and Outlook only (`google.mail`, `microsoft.mail`: the adapter
  must ship its `reply_draft` effect); any other channel is `UNSUPPORTED`,
  exit 2, before anything is written. It needs the draft scope the
  adapter lists (Gmail: `gmail.modify` covers it).
- A plugin actor's draft is held while `held_operations` is on, as class
  `reply:draft`, and applied once when approved ([approvals.md](approvals.md)).

### Unsubscribe from a sender

`conversation unsubscribe` asks the sender of the message a mail
conversation replies to (the latest the peer wrote) to stop mailing the
mailbox:

```sh
heo conversation unsubscribe conv_123 --dry-run
heo conversation unsubscribe conv_123 --idempotency-key unsub-123
heo conversation unsubscribe conv_123 --method mailto
```

**This is external and cannot be undone.** A request leaves for a third
party; the sender may take days to honour it and may ignore it.

What the adapter does, which is uma's and not heo's (heo passes only the
method; there is no flag for a URL or an address, and the adapter reads
the message's headers again from the provider, taking none from the
caller):

| Method | Used only when | What happens |
| --- | --- | --- |
| one-click ([RFC 8058](https://www.rfc-editor.org/rfc/rfc8058)) | exactly one `List-Unsubscribe` naming one `https` URI (default port, no credentials, a public host), exactly one `List-Unsubscribe-Post: List-Unsubscribe=One-Click`, and a valid DKIM signature of the sender's organization covering both | one `POST` of `List-Unsubscribe=One-Click`, through the adapter's own HTTP client: no cookies, no authorization, 15 seconds |
| mailto | the first `mailto` URI names one address in the sender's own organization, with no `to`, `cc` or `bcc` field | a text mail from the mailbox to that address |

- `auto` (the default) uses one-click when the message qualifies, else
  mailto; `one_click` and `mailto` allow only that one.
- Every other URI (`http`, `ftp`, `tel`, any scheme) is never followed,
  and the adapter makes no `GET`. A redirect from the one-click endpoint
  is never followed (a `3xx` is a final refusal).
- The URI is the sender's choice, so the adapter guards it against
  reaching your own network: it refuses a host that is, or resolves to,
  a loopback, private, link-local (the cloud metadata address included),
  shared, reserved, NAT64 or 6to4 address, dials only an address it
  vetted, and uses no proxy.
- A message that qualifies for neither method is refused by the adapter
  before any request: a final answer on the operation (`heo delivery
  get`), not an error of the command. The mailto rule limits where mail
  goes; the `From` header is the sender's claim, so it does not
  authenticate who sent it.
- heo adds no check of its own on top: it does not read the header,
  cannot say beforehand which method will run (`--dry-run` states the
  rules, not the outcome), and neither follows a URL nor sends the mail
  itself. The uma stored message carries `list_unsubscribe` as found,
  but heo does not read it yet.
- Held for a plugin actor while `held_operations` is on, as class
  `unsubscribe`; the approval is the person's consent RFC 8058 asks for.
  See [approvals.md](approvals.md#unsubscribe-is-held-like-any-write-and-is-the-one-a-yes-cannot-undo).
- Gmail and Outlook only; a conversation the peer wrote no message in is
  `CONFLICT`, exit 4.

### List and create provider mailboxes

A mail provider keeps mailboxes of its own: a Gmail label, an Exchange
folder or category, an IMAP mailbox. `provider-label` lists and creates
them; they are not `heo label`s (uma's own, local, matched to no
provider's), and the `provider-` prefix keeps the two apart:

```sh
heo provider-label list
heo provider-label list --channel support-mail --format json
heo provider-label create Clients/Acme --channel support-mail --dry-run
heo provider-label create Clients/Acme --channel support-mail
```

A mailbox is `channel` (its key, else its ID), `id` (the provider's own:
`Label_12`, a folder ID, an IMAP mailbox name), `name` (`/` separating a
hierarchy, whatever the provider spells it with), `kind` (`label`,
`folder` or `category`) and `role` (`inbox`, `archive`, `trash`, `junk`,
`sent`, `drafts`, or empty).

- `list` prints them in one stable order (channel, kind, name, ID), the same
  on every run; `--format json` is the listing every `list` prints,
  `{"items": [...], "next": "", "complete": true}`, an item with those keys
  and `items` `[]` when there are none; the whole list is one page. Build a
  name-to-ID map from `items` per channel; names are unique within a kind
  there. It reads the provider as you each time (the
  adapter may keep the list a few minutes); uma keeps nothing. Without
  `--channel` it lists every mail channel you reach through your inboxes
  that can list, and leaves out one that cannot; `--channel <key or ID>`
  lists one and refuses a channel that cannot (`UNSUPPORTED`, exit 2) or is
  not yours (exit 3).
- `create <name>` makes the mailbox in the account of `--channel` (the one
  channel that can create when omitted; several are refused, exit 2, with
  their names). `--kind` picks `label`, `folder` or `category` where the
  provider has more than one. It is **idempotent by name**: the provider
  compares names as it does itself (Gmail and Exchange without regard to
  case), and an existing mailbox is the result, never a second one;
  `--dry-run` reads the list and says when the name exists.
- It is an external write, queued like a reply: the command returns once
  uma accepted it durably, never applied. `heo delivery get <operation>`
  follows it, and once it settled its `provider_references` hold the new
  mailbox ID; `provider-label list` shows it too. To make sure a mailbox
  exists: list, create the name when it is not there, wait for the delivery,
  list again.
- A plugin actor's create is held while `held_operations` is on, as class
  `provider-label:create` ([approvals.md](approvals.md)). It targets the
  channel, which a `desk` actor may not read and an `operator` may not run,
  so the approval is by an actor holding both roles.
- Only a channel whose adapter declares `extension_query` and
  `<type>.mailboxes` (list), or `extension_execute` and
  `<type>.mailbox_create` (create), can do it: `google.mail`,
  `microsoft.mail` and `imap.mail`, when the channel reports the capability
  available now. Listing needs `extension.query` (`desk`, `operator`),
  creating `extension.execute` (`desk`).

`conversation move` and `conversation tag --add|--remove` take a mailbox by
ID **or by name** and resolve a name through that list before they send:

- A value that is the ID of a listed mailbox is that ID: an ID wins over a
  mailbox whose name is spelled like it.
- Else the mailbox of that name, spelled exactly, else without regard to
  case. Two mailboxes with the name (a label and a category, `Billing` and
  `billing`) are refused (`VALIDATION`, exit 2) with their IDs: nothing is
  guessed.
- A value the list does not hold is sent as given, and so is every value on
  a channel that cannot list (its adapter lacks the extension, or it is
  unavailable now): IDs work as they always did. A list that fails to read
  fails the command.
- The operation, the hold class and the plan carry the IDs sent; `--dry-run`
  prints what each name resolved to (`resolved`). `--role` is no name and is
  never resolved.

### Change a conversation's provider state

A provider keeps its own state for a conversation: a mailbox archives, files,
reads, stars and trashes it, and a chat workspace archives a room or thread.
Nine commands change it, one capability for each pair:

| Command | Needs | Changes at the provider |
| --- | --- | --- |
| `conversation archive`, `conversation unarchive` | `archive` | the archive: out of the inbox, or back |
| `conversation move` | `move` | where it is filed: `--to <ID or name>` (a folder or label), `--role inbox\|archive\|trash\|junk`, or `--add` and `--remove` provider labels, by ID or name ([provider mailboxes](#list-and-create-provider-mailboxes)) |
| `conversation read`, `conversation unread` | `read_state` | the provider's read state |
| `conversation flag`, `conversation unflag` | `flag` | a star (`--kind star`, the default) or an important flag (`--kind important`) |
| `conversation trash`, `conversation untrash` | `trash` | the account's copy in the trash; the other party's copy stays |

```sh
heo conversation archive conv_123 --dry-run
heo conversation archive conv_123 --idempotency-key arch-123
heo conversation move conv_123 --role junk          # how a mailbox marks spam
heo conversation flag conv_123 --kind important
heo conversation read conv_123
```

- It is the provider's state, not uma's: the conversation's status
  (`resolve`, `reopen`), its labels (`conversation label`) and your read
  marker (`mark-read`, `mark-unread`) are never touched, and `conversation
  read` is not `mark-read`. A provider's labels and folders are not heo's
  labels, and not the message tags of `conversation tag`. `--to`, `--add`
  and `--remove` take a mailbox by ID or by name, resolved through the
  channel's mailbox list ([below](#list-and-create-provider-mailboxes)).
- Queued like a reply: the command returns once uma accepted it durably,
  `delivery get <operation>` follows it, and uma's provider state for the
  conversation (`conversation get`) changes only once the provider applied
  it. A change made in the mailbox client reaches uma the same way.
- Each is refused, exit 2 (`UNSUPPORTED`), where the channel's adapter does not
  declare the capability, at the preview and on submission alike. The
  preview also reads what the channel reports now: its scope (`account`,
  only the connected account's own view, as a mailbox, or `shared`, what
  everyone sees, as a chat room) and its limits, and refuses a flag kind, a
  role or a label move the provider does not have (Gmail files by label,
  Graph and IMAP in one folder, so a label move is Gmail's alone).
- `--revision` applies the change only if the conversation is still at that
  revision (conflict, exit 4). Rerunning under the same
  `--idempotency-key` returns the same operation.
- Held for a plugin actor while `held_operations` is on, under the trust
  classes the ladder already names: `archive`, `read`, `unread`, `star`,
  `unstar` and `trash` (a move into the trash role), `spam` (a move into
  the junk role) and `move_to` (any other move). See
  [approvals.md](approvals.md#trust-ladder).
- Desk role. See [provider-verbs.md](provider-verbs.md) for which adapters
  declare which verbs.

### Tag a mail message at the provider

A mailbox keeps tags of its own: Gmail's labels, Outlook's categories.
`conversation label` never touches them (a label is uma's and local);
`conversation tag` does, on a channel whose adapter ships the effect:

```sh
heo conversation tag conv_123 --add Label_12,STARRED --remove INBOX --dry-run
heo conversation tag conv_123 --add Label_12,STARRED --remove INBOX
heo conversation tag conv_123 --set Billing,Urgent
heo conversation tag conv_123 --set= --idempotency-key untag-123
```

- **Gmail** (`google.mail`): `--add` and `--remove` take labels by ID or
  by name, repeated or comma-separated, and may go together: `Label_12`,
  a system label such as `STARRED`, `INBOX` or `UNREAD`, or a name such as
  `Clients/Acme`, which is resolved to its ID through the account's label
  list ([provider mailboxes](#list-and-create-provider-mailboxes)). Each
  label once, at most 100 per list. The call is idempotent at Gmail: adding a label the
  message holds changes nothing.
- **Outlook** (`microsoft.mail`): `--set` takes category names and
  **replaces the whole list**; an empty value clears it. Each name
  once, sent as given. Graph has no add or remove, so heo cannot work out
  the difference for you.
- One way per call: `--set` goes alone (a usage error, exit 2, with
  `--add` or `--remove`). An adapter that ships the other effect is
  refused, exit 2 (`UNSUPPORTED`), saying which flags it takes.
- The IDs and names are your mailbox's, not uma's: heo does not match
  them to `heo label` names. A Gmail name the account's list holds is sent
  as its ID; a value the list does not hold is sent as given, and whether it
  exists is the provider's answer, shown on the operation (`heo delivery
  get`). `--dry-run` prints what each name resolved to. A value may not
  hold a comma.
- The change acts on **one message**, the one the conversation replies
  to: the latest the peer wrote. The preview names it (`message`). The
  rest of the thread is not touched, and a conversation the peer has
  written no message in has nothing to tag (`CONFLICT`, exit 4).

It is an **external change**: the mailbox shows it in every client. It is
queued like a reply and **never applied when the command returns**:
`heo delivery get <operation>` follows it. uma records no provider tags,
so nothing in `conversation get` or `conversation list` changes, and
the `conversation.label_added` events are for `conversation label` only.
Rerunning with the same `--idempotency-key` and arguments returns the
same operation, marked `replayed`.

The adapter checks the fields offline: `--dry-run` refuses an empty or
repeated ID, more than 100, and a blank or repeated category
(`VALIDATION`, exit 2), and uma refuses the same on the real run, before
anything is stored. Only a channel whose adapter declares
`extension_execute` and ships Gmail's label change or Outlook's
category change can do this; `smtp.mail`, `imap.mail`, the chat and
Twilio adapters and `github.issues` are refused before anything is
written, exit 2, at the preview and on the real run. The token needs
the provider's permission to change messages (Gmail's modify scope,
Graph's `Mail.ReadWrite`); that is checked when the operation is
applied. Desk role, like `conversation label`: a viewer is refused
(exit 5), preview included.

## Assign, snooze, link and read

The desk works a conversation with four local verbs. All of it is
uma's: the state lives in uma's own tables beside the conversation, so
it is in `heo storage backup` and comes back with `heo storage restore`,
and none of it changes anything at a provider.

```sh
heo conversation assign conv_123 --to self        # you take it
heo conversation assign conv_123 --to uid:1002    # or hand it to a desk actor
heo conversation unassign conv_123

heo conversation snooze conv_123 --for 2h         # hide it for two hours
heo conversation snooze conv_123 --until 2026-10-20T09:00:00Z
heo conversation unsnooze conv_123

heo conversation link conv_123 --conversation conv_456
heo conversation link conv_123 --work-item wi_7    # an issue or a ticket
heo conversation unlink conv_123 --conversation conv_456

heo conversation mark-read conv_123               # read, through now
heo conversation mark-read conv_123 --through msg_42
heo conversation mark-unread conv_123 --from msg_42
```

Each takes `--dry-run`, which prints the change, including what it
replaces, and writes nothing. `assign`, `snooze` and `link` and their
inverses are uma operations: `--idempotency-key` replays them, and
`--revision` (from `conversation get`) applies the change only while the
conversation is still at that revision; a stale one is a conflict (exit
4) that says who holds the conversation now. Without `--revision` the
change applies to whatever the conversation is.

- **Assign.** A conversation has one assignee, a desk actor. `--to self`
  is the actor running the command, which the daemon takes from the
  authenticated caller, never from the command line. Only an actor that
  holds the `desk` role can be assigned; a viewer, an external actor or
  an unknown one is refused (exit 5), in the preview too. The preview
  shows who holds the conversation now. Assignment is part of the
  conversation's revision, as labels are: an `assign` between your
  `conversation get` and a `resolve` at the old revision makes the
  `resolve` a conflict. Every change is an event naming who held it
  before (`event list --kind conversation.assigned`), desk only.
- **Snooze.** `--until` takes an RFC 3339 time or a date (midnight UTC);
  `--for` a duration (`90m`, `2h`, `3d`), which the daemon resolves
  against its clock when the command runs. A time that is not in the
  future is `VALIDATION` (exit 2). The conversation leaves the default
  `conversation list` and comes back at that time, or as soon as a live
  inbound message is recorded after the snooze, whichever is first;
  history imported with `history fetch` never wakes it. Nothing is
  scheduled: uma works the wake out when it reads the conversation, so a
  snooze survives a restart and waking emits no event. `--for` resolves a
  new time on every run, so replay a snooze with `--until`, the time the
  output prints. A snooze is independent of the status.
- **Link.** A link relates a conversation to another conversation or
  a work item (an issue or a ticket). It merges no history, changes no reply scope, allows
  no reply across inboxes, and reaches no provider. Between two
  conversations both hold and show it; a link to an issue or ticket is
  held by the conversation, and the work item's own `conversation get` lists
  the conversations that link to it. You need the right on both ends: one you
  may not read is not found (exit 3), exactly as one that does not exist.
  `--dry-run` shows both ends. A conversation holds at most 64 links.
- **Read.** Your read marker is yours alone: another actor's marker is
  independent, and no one reads it. `mark-read` moves it through the
  newest message recorded, or through `--through`; `mark-unread` moves it
  before the newest live inbound message, or before `--from`. Messages
  imported from a provider's history never count as unread. The marker
  is local: it sends no receipt to the provider (`receipt send` does
  that), does not change the conversation or its revision, and emits no
  event. The same mark again changes nothing.

`conversation get` shows the assignee, the snooze while it is in effect,
the links you may read and, for a desk actor, its own read marker.

### Filters

```sh
heo conversation list --unread                 # unread for you
heo conversation list --assignee self          # your queue; or an actor ID, or none
heo conversation list --snoozed only           # include | only | exclude (default)
heo conversation list --linked
heo conversation list --assignee none --status open --inbox web --label billing
```

uma applies them, evaluated by the store, so pages are full; heo never
filters a page itself. They combine with `--inbox`, `--status` and the
label filters by AND. The default hides snoozed conversations;
`--snoozed include` lists them with the rest. `--linked` keeps the
conversations that hold a link you may read. `--unread` is unread for
you: the marker you keep, and messages from others recorded after it.

### Who sees it

The desk sees and changes all of it. A `viewer` reads the assignee,
the snooze and the links (and the default list hides snoozed
conversations from it too), but changes nothing and keeps no read
marker. An `external` participant sees none of it on any surface: no
field in `conversation get`, no event, no filter (asking for one is
exit 5), and a snoozed conversation is not hidden from it, since it
cannot see the snooze.

## Resolve and reopen

```sh
heo conversation get conv_123                       # revision rev_123
heo conversation resolve conv_123 --revision rev_123
heo conversation reopen conv_123 --revision rev_456
```

Transitions name the revision you read. If the conversation changed
since, it is `CONFLICT` (exit 4) and nothing changes: read it again,
then decide. The conversation lifecycle decides which moves are allowed,
and uma applies it when the transition commits: `closed` is terminal,
so reopening a closed conversation is `CONFLICT` (exit 4). Moving a
conversation to the status it already has commits and changes nothing;
the preview warns about it.

Resolving is local. No provider ticket or thread closes, and setting a
work item's status (`heo conversation status`, or on the provider) never
closes its conversation. Neither moves the other
([issues-and-tickets.md](issues-and-tickets.md)).

A conversation's own archived state is the provider's, not uma's: see
[Change a conversation's provider state](#change-a-conversations-provider-state).

## Summarize, triage and draft

```sh
heo conversation summarize conv_123
heo conversation triage conv_123
heo message draft conv_123 --instruction "offer a refund" --tone friendly
```

A model reads the conversation as you and prints a summary, suggestions
(category, priority, sentiment, labels, next action) or the text of a
reply. They suggest and never act: no label is applied, no status
changes, and a draft is not sent. **The conversation's text is sent to the
model you configured**, a local one by default; a cloud provider needs an
explicit opt-in and `--send-to-cloud` on each call. A viewer's prompt holds
public messages only. See [llm-helpers.md](llm-helpers.md).

## Preview, retry, conflicts

- `--dry-run` on `message reply`, `message send`, `note add`, `conversation resolve`,
  `conversation reopen`, `conversation label` and `label create` prints
  the plan — its effect, then the
  destination, visibility, placement or status change as rows — and
  writes nothing. Running the command for real checks everything again:
  a preview is not a permission.
- Each write is a uma operation keyed by `--idempotency-key`. Without
  one, heo mints a key (`op_...`) and prints it as `operation key`. If
  you lose the answer, rerun the same command with the same key: you
  get the stored operation back (`replayed`), and nothing is sent twice.
  The same key with different arguments is `CONFLICT`.
  `heo operation get --key <key>` finds the operation without sending
  anything: see
  [deliveries-and-events.md](deliveries-and-events.md#recover-a-write-whose-answer-was-lost).

## Who may do what

| Command | Role |
| --- | --- |
| `conversation list`, `get`; `message list` (searching by `--text`, `--since`, `--until`), `get` | any role that reads conversations (`desk`, `viewer`, `external`) |
| `conversation resolve`, `reopen`; `message reply`, `send`; `message draft` | `desk` |
| `conversation summarize`, `triage` | `desk`, `viewer`: a viewer's prompt holds public messages only; see [llm-helpers.md](llm-helpers.md) |
| `note list`, `get`, `add` | `desk` |
| `message edit`, `message delete`; `reaction add`, `reaction remove`; `participant add`, `participant remove`; `receipt send` | `desk`; see [provider-verbs.md](provider-verbs.md) |
| `history fetch` | `desk` for a conversation, `operator` for a channel; see [provider-verbs.md](provider-verbs.md#history) |
| `reaction list`; `participant list` | any role that reads conversations (`desk`, `viewer`, `external`) |
| `task list`, `get` | `desk`, `viewer`; see [tasks.md](tasks.md) |
| `task cancel`, `task watch` | `desk`; see [tasks.md](tasks.md) |
| `attachment list`, `get`, `download` | any role that reads conversations (`desk`, `viewer`, `external`); content that is not public, `desk` only |
| `label list`; `conversation list` and `message list` with `--label`, `--label-all`, `--label-none` | `desk`, `viewer` |
| `label create`; `conversation label` | `desk` |
| `provider-label list` | `desk`, `operator` (policy action `extension.query`; mail channels whose adapters list mailboxes only) |
| `provider-label create` | `desk` (policy action `extension.execute`; mail channels whose adapters create mailboxes only) |
| `conversation tag` | `desk` (policy action `conversation.label`, as the label change; mail channels whose adapters ship the effect only: [provider-verbs.md](provider-verbs.md)) |
| `conversation unsubscribe` | `desk` (policy action `extension.execute`; mail channels whose adapters ship the effect only: [provider-verbs.md](provider-verbs.md)) |
| `conversation archive`, `unarchive`, `move`, `read`, `unread`, `flag`, `unflag`, `trash`, `untrash` | `desk` (policy action `conversation.state_account` or `conversation.state_shared`, by the scope the channel reports; channels whose adapters declare the capability only: [provider-verbs.md](provider-verbs.md)) |
| `conversation assign`, `unassign`, `snooze`, `unsnooze`, `link`, `unlink`, `mark-read`, `mark-unread` | `desk` |
| `conversation list` with `--unread`, `--assignee`, `--snoozed`, `--linked`; the assignee, snooze and links in `conversation get` | `desk`, `viewer` (the read marker is the desk's, its own) |

`operator` alone reads no conversation content: grant `desk` as well.
See [authorization.md](authorization.md).

To see who wrote a message, and to link senders who are the same
contact, see [contacts.md](contacts.md).
