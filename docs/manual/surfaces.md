# REST, gRPC and MCP: serving heo to other programs

The CLI reaches the daemon over the profile's control socket and needs
no setup (see [control.md](control.md)). To let another program call
heo, enable one of the HTTP surfaces:

| Surface | Service | Default address | What a client speaks |
| --- | --- | --- | --- |
| REST | `api` | `127.0.0.1:8080` | JSON over HTTP; discovery at `/v1/commands`, schemas at `/openapi.json` |
| gRPC | `rpc` | `127.0.0.1:8082` | `cmdsurface.v1.Commands` over Connect, gRPC or gRPC-Web |
| MCP | `mcp` | `127.0.0.1:8081` | MCP over streamable HTTP at `/mcp` |

All three serve the same commands, with the same schemas and the same
permission checks, as the control socket. All three listen on loopback
only and require a credential, on loopback too.

## Set up

```sh
heo service setup api mcp
heo service restart          # or heo service start, if no daemon runs
```

`service setup` does three things for the selected profile
(`--profile`):

1. Mints the profile's **surface token**, 32 random bytes, into the
   profile's credential backend (`credentials.backend`; an aps-bound
   profile keeps it in heo's own store, since its aps secrets are
   read-only to heo). One token per profile, shared by the three
   surfaces.
2. Writes the profile file: `services.<surface>.enabled: true`,
   `services.<surface>.addr`, and `credentials.surface_token_ref`, the
   store key of the token (`heo/<profile dir>/surface-token`). Never
   the token.
3. Prints where each surface listens and how to call it.

```text
profile  heo://profile/default
config   /home/me/.config/heo/profiles/default.yaml (updated)
api      on  http://127.0.0.1:8080  (changed)
rpc      off
mcp      on  http://127.0.0.1:8081/mcp  (changed)
token    minted: heo/default/surface-token in the keyring store, acts as uid:501
```

The address is `--addr <surface>=<host:port>` when you give one, else
the one already configured, else the default above, else the next free
port. Only a loopback IP is accepted (`127.0.0.0/8` or `::1`): `0.0.0.0`,
a LAN address and host names such as `localhost` are refused (exit 2).
Serving beyond the machine is not supported in this release.

A running daemon picks the surfaces up on restart, not before.

### On a terminal, and anywhere else

On a terminal (stdin and stderr both terminals), `heo service setup`
with no arguments asks which surfaces to enable and, for each, the
address, offering the defaults. It asks only for what the arguments
left out: `heo service setup api` asks just for the api's address.
Prompts go to stderr; stdout stays the result.

Anywhere else (a script, CI, an agent) it never prompts. With nothing
to do it refuses with `USAGE`, exit 2, naming the arguments:

```text
USAGE: nothing to set up: name the surfaces to enable (api, rpc, mcp), or pass --disable, --addr, --rotate-token or --show-token; without a terminal setup never prompts
Fix: heo service setup api
```

### Preview

```sh
heo service setup api rpc --dry-run
```

prints the plan, the keys it would set and whether it would mint or
rotate the token, and writes nothing. The plan never carries the token.

### Re-run, disable

Setup is safe to re-run: the same arguments keep the token and leave
the profile file as it is (`changed: false`). `--disable <surface>`
turns one off; the token stays.

## The token

A request bearing the token acts as **`uid:<n>` of the user who ran
setup**: the actor the control socket names that same user. Your
authorization grants, audit trail and the daemon's own owner roles see
one identity on every surface (see [authorization.md](authorization.md)).

The token is printed only when you ask, once, on stdout:

```sh
export HEO_TOKEN="$(heo service setup --show-token --format json | jq -r .token.value)"
```

It never appears in configuration, `config list`, `config validate`,
logs, `--dry-run` plans, or setup's output without `--show-token`.

To replace it:

```sh
heo service setup --rotate-token --show-token
```

The old token is refused from the next request on; the daemon reads the
token from the store on every request, so no restart is needed. Rotate
whenever a token may have leaked.

## Plugin tokens

The profile's token acts as the owner's uid. A hook plugin must not:
it has its own actor, `plugin:<name>`, and its own token.

```sh
heo plugin token mint triage       # prints "triage.<secret>" once
heo plugin token revoke triage
```

The token is stored in the same credential backend as the surface
token, under `heo/<profile dir>/plugin-token/<name>`, and is sent the
same way, as a bearer token on REST, gRPC and MCP. A request bearing it
acts as `plugin:<name>` and as no other actor; it needs no surface
token to be configured. It never reaches the control socket, which
refuses a plugin actor. Minting again replaces the token and revoking
deletes it; either is effective from the next request, no restart.

A plugin holds no role until `authorization.grants` names it (see
[authorization.md](authorization.md#a-plugins-actor)). Both commands
are CLI-only: no served surface invokes them.

## Call

Send the token as a bearer token on every request. A request without
it, or with any other, is refused with `401` before any command runs.

### REST

```sh
curl -s -H "Authorization: Bearer $HEO_TOKEN" http://127.0.0.1:8080/v1/commands
curl -s -H "Authorization: Bearer $HEO_TOKEN" 'http://127.0.0.1:8080/v1/commands/config/get?arg=runtime.tenant'
```

```json
{"exit_code":0,"stdout":"default\n"}
```

Discovery (`/v1/commands`) lists every command with its method and
route; read commands are `GET` with flags as query parameters,
everything else `POST` with a JSON body `{"flags":{...},"args":[...]}`.

### gRPC

```sh
curl -s http://127.0.0.1:8082/cmdsurface.v1.Commands/Invoke \
  -H "Authorization: Bearer $HEO_TOKEN" -H 'Content-Type: application/json' \
  -d '{"path":["config","get"],"args":["runtime.tenant"]}'
```

The same call works from any Connect, gRPC or gRPC-Web client: send
`authorization: Bearer <token>` as request metadata.

### MCP

An MCP client that speaks streamable HTTP:

```json
{
  "mcpServers": {
    "heo": {
      "type": "http",
      "url": "http://127.0.0.1:8081/mcp",
      "headers": {"Authorization": "Bearer ${HEO_TOKEN}"}
    }
  }
}
```

Each command the surface may run is one tool, named by its path with
dots (`config.get`). For an agent that spawns heo itself, use MCP over
stdio instead: `heo service start mcp --stdio` needs no token, since it
relays to the daemon over the control socket as the spawning user (see
[control.md](control.md#mcp-for-an-agent-you-spawn)).

## Refusals

| You see | Why | Do |
| --- | --- | --- |
| `401`, `unauthenticated` (gRPC), `authentication required` | no token, another token, or a rotated one | send the current token; `heo service setup --show-token` |
| `403`, `heo.authz.no_role` | the token's subject holds no role in this profile | grant it one (see [authorization.md](authorization.md)), or mint the token as the daemon's owner |
| `403`, `heo.authz.plugin_decide` on `operation held approve` or `reject` | the token's subject is a `plugin:` actor, which never decides held operations | decide as a person ([approvals.md](approvals.md#on-rest-grpc-and-mcp)) |
| start exits 2, `no surface token stored` | the profile names a token the store does not hold | `heo service setup <surface>` |
| start exits 2, `no authenticator configured` | a surface is enabled with no token and no kit authenticator | `heo service setup <surface>` |
| start exits 2, `--no-auth is refused` | heo never serves an HTTP surface without authentication | drop the flag |
| `404` or `NotFound` on `event watch`; `invocable: false`, `withheld-by-command` in REST discovery | the command streams and is served on the control socket only | run `heo event watch` at the terminal |

MCP cannot pass a command's positional argument, so a command that needs
one offers a flag for it: `operation get --key`, and `operation held
show`, `approve`, `reject` and `history` take `--operation <id>` (the
tool's `operation` argument: a string, or a JSON array of IDs on
approve and reject). `operation trust import` takes its file as the JSON
object itself in `--file` (the tool's `file` argument): the daemon cannot read
your files. Any array flag, such as `--kind` on `operation
held list`, is a JSON array over MCP. Pass the positional or the flag,
never both.

Destructive commands run over the control socket only; REST, gRPC and
MCP refuse them. So does `event watch`, which streams until stopped: it
is not offered on REST, gRPC or MCP, where it would hold the daemon (see
[deliveries-and-events.md](deliveries-and-events.md#terminal-only)).

For what a desk client can build from these reads, see [desk-feeds.md](desk-feeds.md).

## Another authenticator

To authenticate with your own identity provider instead of the surface
token, set a kit authenticator: `services.<surface>.auth.mode` (or
`services.all.auth.mode`) to `jwt`, `jwks`, `oidc`, `apikey` or `mtls`,
with its settings (see [control.md](control.md#the-other-surfaces)). A
kit authenticator takes precedence over the surface token for its
surfaces. The actor is then the subject of the credential it verified.
