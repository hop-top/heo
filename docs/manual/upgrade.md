# Upgrading heo

Upgrading has two parts: the binary (`heo upgrade`) and each profile's
data store (`heo storage migrate`). Neither does the other's job.

## Check what you run

```sh
heo --version
heo storage status          # heo, kit, uma, store versions and schema
```

## Upgrade the binary

```sh
heo service stop            # for every profile whose daemon runs
heo upgrade --dry-run       # what would be installed, nothing downloaded
heo upgrade
```

`heo upgrade` looks up the latest release, downloads this platform's
archive next to the running binary, verifies it against the release's
SHA-256 checksums file, and swaps it in. Only a newer version is
installed; it never downgrades. It does not touch any data store.

No heo release has been published yet: until one is, `heo upgrade`
exits 3 (`NOT_FOUND`, "no heo release is published") and changes
nothing.

| Situation | Exit | What to do |
|-----------|------|------------|
| Already the latest release | 0 | nothing |
| A heo daemon is running (any profile) | 4 `CONFLICT` | `heo service stop`, upgrade, start it again |
| Installed by Homebrew | 2 `UNSUPPORTED` | `brew upgrade heo` |
| No release published, or none for this platform | 3 `NOT_FOUND` | nothing to upgrade to |
| Release feed unreachable | 6 `UNAVAILABLE` | check the network, retry |
| `--offline` | 2 `USAGE` | rerun without `--offline` |
| Download fails verification | 1 | the installed binary is unchanged |

Releases come from GitHub, `hop-top/heo`. A mirror can serve instead:

| Key | Default | Meaning |
|-----|---------|---------|
| `upgrade.github_repo` | `hop-top/heo` | owner/repo whose latest GitHub release is installed |
| `upgrade.release_url` | empty | a feed answering `{"version", "url", "notes"}`; replaces the GitHub lookup |
| `upgrade.checksum_url` | empty | checksums file for the feed's archives; required with `upgrade.release_url` |

`heo upgrade` is never callable over a served surface (socket, REST,
gRPC, MCP).

## Migrate each data store

A new heo may need a newer schema. The daemon refuses to start on a
store whose schema is not its own, so after upgrading:

```sh
heo storage status          # compat: migration_required?
heo storage migrate --dry-run
heo storage migrate         # backs the store up first, then migrates
heo service start
```

Assignments, snoozes, links and read markers are uma's own state, in
uma's tables: the backup holds them, the restore brings them back, and a
migration from a store that predates them fills the assignee and snooze
columns from the conversations already stored. A read marker has no older
home, so a store restored from before they existed has none (every message
is unread until you mark it).

Held operations, the writes a `plugin:` actor proposed and a person has
yet to decide, are uma's own state too: they live in the operation ledger
and need no heo migration. The backup and the restore carry them with the
rest of the store, decided or not. The held and expired states are new
values of the delivery state: `--state` on `operation list` and `delivery
list` accepts them, and a script that switches on a state keeps a default
branch.

Run it once per profile (`heo --profile <ref> storage migrate`). See
[commands](commands.md#storage) for the `storage` group.

## Rolling back

`heo upgrade` never installs an older version. To go back, install
the previous release's archive by hand.

An older heo refuses a store a newer one migrated (`newer_schema`,
exit 1). To run the older binary, restore the backup `storage migrate`
took before migrating, with the daemon stopped:

```sh
heo storage restore <path-from-migrate-output> --confirm=yes
```

Data written since that backup is lost; the restore backs the
replaced data up first and prints where. The backup before a migration
holds the store alone, so attachments are left as they are.
