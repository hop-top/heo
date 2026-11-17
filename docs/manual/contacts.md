# Contacts and identities

Find out who is writing, say when two senders are the same contact,
and keep the contact list tidy.

- An **identity** is one sender address in one inbox: a widget
  visitor, an email address, a phone number. Every inbound message has
  one as its author.
- A **contact** is who stands behind one or more identities. Each
  identity points at exactly one contact.
- When a new sender writes in, ingress creates its identity on a
  **provisional contact** of its own. heo never merges senders by
  itself: no matching on names, addresses or message text. Two
  identities share a contact only when a desk actor links them, and two
  contacts become one only when a desk actor merges them.

Every command here runs in the profile's daemon, which owns the data
store. Start it first with `heo service start`; with none running,
these commands exit 70.

IDs are record IDs, as `list` prints them (`ct_123`, `id_123` in the
examples below).

## Read contacts and identities

```sh
heo contact list                         # every contact you may read
heo contact list --kind person
heo contact get ct_123                   # the contact and its identities
heo identity list                        # every identity you may read
heo identity list --contact ct_123
heo identity list --inbox web
heo identity get id_123                  # the identity and its link history
```

Listings page with `--limit` and `--cursor`, as the conversation
listings do. `identity list` shows each identity's inbox, contact,
kind (`person`, `bot`, `agent`), sender address and link assurance.
You see identities only in inboxes you may read.

`identity get` shows the identity's revision — what `link` and
`unlink` take as `--revision` — and then its **link history**, oldest
first:

| Column | Meaning |
| --- | --- |
| `CLASS` | `link` (off its provisional contact), `relink` (between contacts), `unlink`, or `merge` (moved with its whole contact by `contact merge`) |
| `FROM`, `TO` | the contacts it left and joined |
| `ACTOR` | who made the change |
| `ASSURANCE` | the link assurance it got: `asserted` for a link or merge, `none` for an unlink |
| `EVIDENCE` | the evidence kind and reference, such as `attestation TICKET-42` |
| `REASON` | the audit reason of an unlink or merge |

The history is append-only: every change stays listed.

A contact that was merged into another shows `merged into ct_456` in
`contact get`, and a `MERGED INTO` column in `contact list`; its
identities are on that contact now, so `contact get` of the merged one
lists none. The redirect is permanent.

## Create and edit a contact

A person you know before they write in: create the contact, and link
their identity to it when they do.

```sh
heo contact create --name "Ada Lovelace" --email ada@example.com --dry-run
heo contact create --name "Ada Lovelace" --email ada@example.com --phone "+1 202 555 0100"
heo contact get ct_123                                   # note its revision
heo contact update ct_123 --name "Ada King" --revision REV
heo contact update ct_123 --phone= --revision REV        # clear the phone numbers
```

`--name` is required. `--kind` is `person` (the default), `bot` or
`agent`, and never changes afterwards: identities only join a contact
of their own kind. A new contact holds no identity; only `identity link`
puts one on it.

Attributes (`--email`, `--phone`; repeat or comma-separate) are lookup
hints, never proof and never used to match anyone. heo stores an email
with its domain lowercased and a phone number as E.164 (`+` and
digits; spaces, dots, dashes and brackets are stripped), and refuses
other forms (`VALIDATION`, exit 2). None is verified by a desk write.
On `update`, an attribute flag replaces the contact's attributes of
that kind, `--email=` with no value clears them, and what you leave out
stays. Changing nothing commits nothing and leaves the revision as it
is.

## Merge two contacts

The same person ended up as two contacts, because they wrote from two
places and nobody linked them. Merge one into the other.

```sh
heo contact merge ct_456 ct_123 --reason same_person --dry-run
heo contact merge ct_456 ct_123 --reason same_person --revision REV --into-revision INTO_REV
```

`ct_456` is merged **into** `ct_123`. The dry run is uma's own preview,
and writes nothing:

- the **identities** on `ct_456` that move to `ct_123`;
- the **conversations** those identities own. They keep their owner and
  their reply route; only the contact their owner belongs to changes;
- the two **revisions** to merge at. A very long conversation list is cut
  short, and the preview says so.

Review it, then run the merge with those revisions: `--revision` is the
revision of the contact being merged (`ct_456`), `--into-revision` the
revision of the one merged into. heo moves exactly what you reviewed:
if either contact changed since, or the identities on `ct_456` are no
longer the ones the preview listed, the merge is `CONFLICT`, exit 4, and
nothing moves; preview again. Pass `--identity` for each identity you
reviewed to hold heo to that set explicitly.

What a merge does:

- every identity on `ct_456` moves to `ct_123`, and each one's link
  history gains a record of class `merge`, with you as the actor, link
  assurance `asserted` and your `--reason` (`identity get` shows it);
- `ct_456` stays in place as a redirect: `contact get ct_456` says
  `merged into ct_123`;
- neither contact's name or attributes change, and both revisions move;
- linking an identity to the merged contact is refused (`CONFLICT`,
  exit 4); link to the contact it was merged into.

Both contacts must be of one kind. `--reason` is required, an audit
reason code like `same_person`: one lowercase word or `snake_case`
name, kept even after the contacts' data is redacted, so never personal
data.

**There is no unmerge.** An identity can leave `ct_123` again with
`identity unlink` or `identity link`, but `ct_456` stays merged.
Because of that, `contact merge` is destructive: it asks for
confirmation on a terminal, and without one it is refused (exit 5)
unless you pass `--confirm=yes`. heo never merges on its own, and no
other command does it for you.

Only the control socket serves a merge; REST, gRPC and MCP withhold it
(see [surfaces.md](surfaces.md#refusals)). Kit rate-limits destructive
commands per caller: three in a burst, then one every five seconds; a
refused one exits 6 with the time to retry.

A merge is no more permissive than linking each identity yourself: uma
asks the desk role's `contact.merge` for the contact, and for each
identity the same relink permission `identity link` needs. An identity
whose link assurance is above `asserted` (it was linked on stronger
evidence) needs the `operator` role's link override too. One refusal
refuses the whole merge (`UNAUTHORIZED`, exit 5), and nothing moves.

## Link an identity to a contact

The same person wrote from two places: link one identity to the
other's contact.

```sh
heo identity get id_456                                   # note its revision
heo identity link id_456 --contact ct_123 --revision REV --dry-run
heo identity link id_456 --contact ct_123 --revision REV --reference TICKET-42
```

A link is your **attestation**: you state that the identity belongs to
the contact, and heo records it with you as the actor and link
assurance `asserted`. heo verifies no other kind of evidence, and
nothing you pass sets the assurance. `--reference` names an opaque
proof ID for the attestation, such as a ticket number — never personal
data. One reference backs one link only: reusing it is `CONFLICT`,
exit 4.

The contact must be of the identity's kind: a bot or agent identity
never joins a person contact (`VALIDATION`, exit 2).

A link is association only. The identity's conversations, messages and
reply routes stay where they are, replies still go out on the route
each conversation stored, and no one gains access to anything by it.
The contact the identity left is kept, with no identities, so you can
link back.

## Unlink an identity

The link was wrong: detach the identity, for a reason.

```sh
heo identity unlink id_456 --reason wrong_person --revision REV --dry-run
heo identity unlink id_456 --reason wrong_person --revision REV
```

uma moves the identity to a fresh contact of its kind and records the
unlink. Messages it wrote keep it as their author.

`--reason` is required (`USAGE`, exit 2, without it): an audit reason
code, one lowercase word or `snake_case` name such as `wrong_person` or
`shared_device`. It is kept in the history even after the contact's
data is redacted, so it must never hold personal data; anything else is
`VALIDATION`, exit 2. `--reference` optionally backs the unlink with
your attestation, as for a link.

## Revisions, previews and retries

`create`, `update`, `merge`, `link` and `unlink` work like every other
desk write:

- `--revision` is required (except `create`, and `merge --dry-run`) and
  is the record's revision as `identity get` or `contact get` shows it.
  If the record changed since — someone linked, unlinked, edited or
  merged it — the command is `CONFLICT`, exit 4, and writes nothing.
  Read it again and decide.
- `--dry-run` prints the change — identity, from and to, class,
  evidence, assurance, reason — and writes nothing.
- Each write is a uma operation under `--idempotency-key`: rerunning
  with the same key and arguments returns the same operation
  (`replayed`), even once the revision has moved on: a merge rerun under
  its key moves nothing twice. The same key with other arguments is
  `CONFLICT`. Without a key, heo mints one and prints it. A write is
  committed when the command prints it (`operation ... committed`);
  contact writes queue no provider work.
- Linking an identity to the contact it is on changes nothing: no
  history, revision unchanged.

## Who may do what

| Role | May |
| --- | --- |
| `desk` | read contacts, identities and link history; create, edit and merge contacts; link and unlink |
| `viewer` | read contacts, identities and link history |
| `operator` | override an identity's link assurance, which a merge of such an identity needs besides `desk`; nothing else here |
| `external` | none of these |

Any other caller is refused with `UNAUTHORIZED`, exit 5, on every
surface. See [authorization.md](authorization.md).
