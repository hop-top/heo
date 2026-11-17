# The control socket: how commands reach the daemon

Every profile has one daemon (`heo service start`), and only the daemon
touches the profile's data store. Commands that need the runtime run
**in the daemon**. At the terminal, `heo` sends them over the profile's
control socket and prints the daemon's answer; output, error envelope
and exit code are the daemon's, rendered in your `--format`.

```sh
heo service start &      # the daemon, in the foreground (or under launchd/systemd)
heo daemon ping          # who the daemon serves you as, and its runtime state
```

```text
profile    heo://profile/default
tenant     default
actor      uid:501
surface    socket
daemon     pid 4242, heo 0.1.0
ready      true
storage    ready
workers    ready
receivers  ready
webhook    not_started
```

Commands that need no runtime never contact a daemon: `--help`,
`config`, `spec`, `storage`, `upgrade`, `service list`. `service
status`, `stop` and `restart` do, but over kit's control socket for the
profile, `<runtime dir>/heo/control-<profile dir>.sock`, not this one;
with no daemon running, `status` and `restart` exit 3 and `stop` exits
0.

## No daemon running

A daemon command with no daemon running fails with `PREREQUISITE`,
exit 70, naming the command that starts one. Nothing is started for
you, and nothing is written to the store directly.

```text
PREREQUISITE: no heo daemon is running: nothing listens on /run/user/501/heo/default/heo.sock
Fix: start it with `heo service start`, then rerun
```

Exit 70 is not a retry signal: start the daemon, then rerun. A watch
that was already attached is the one exception: it waits out a daemon
that restarts, `--reconnect-timeout` at most (see [deliveries and
events](deliveries-and-events.md#when-the-daemon-goes-away)).

## Where the socket is, and whose it must be

The socket is `<runtime dir>/heo/<profile dir>/heo.sock`, or
`services.socket.path`. Before sending anything, `heo` checks that:

- the socket's directory is a real directory (not a symbolic link),
  owned by you, with no permission for anyone else (`chmod 700`);
- the socket is a socket, owned by you.

Anything else is refused with `UNAUTHORIZED`, exit 5, and nothing is
sent: a socket another user placed, or could replace, would receive
your commands and answer as if it were the daemon. Point
`services.socket.path` at a directory you own instead.

## Who a command acts for

The daemon decides, never the request:

- over the control socket, the actor is the peer's uid, as the kernel
  reports it: `uid:<n>`. Only your own uid is admitted
  (`services.socket.auth.peer.require_same_uid`);
- over REST, gRPC or MCP over HTTP, the actor is the subject of the
  credential the surface verified;
- the tenant is always the profile's `runtime.tenant`.

A `caller` or `tenant` field a request carries is ignored. A caller no
transport verified — a bare claim, or an owner-only socket without peer
authentication — is refused (`DENIED` on the wire, exit 5 at the
terminal). A verified actor the authorization policy grants no role is
refused the same way, with the reason `heo.authz.no_role`. What each
actor may then do is decided per operation by the same policy; see
[authorization.md](authorization.md).

## The other surfaces

The daemon serves the same commands, with the same schemas and the
same permission checks, over REST (`api`), gRPC (`rpc`) and MCP when
those are enabled. Each one needs an authenticator, on loopback too.
`heo service setup <service>` enables one with the profile's surface
token, a bearer token acting as your uid; see
[surfaces.md](surfaces.md). A start that would serve one with neither
that token nor `services.<service>.auth.mode` (or the shared
`services.all.auth.mode`) is refused, exit 2. A kit authenticator
takes precedence over the token; for example, JWTs signed by a key you
hold:

```yaml
services:
  api:
    enabled: true
  all:
    auth:
      mode: jwt            # or jwks, oidc, apikey, mtls
      jwt:
        public_key_files: [/home/me/.config/heo/signer.pem]
```

Destructive commands run over the control socket only. There, your
confirmation is asked at the terminal (`--confirm=yes` without one) and
carried to the daemon. REST, gRPC and MCP refuse them.

`event watch` also runs on the control socket only. The terminal drives
it as short calls, one per step (open, lease a batch, acknowledge it),
so the daemon is never held by a stream; the steps are heo's own, behind
a hidden `--step` flag. See
[deliveries-and-events.md](deliveries-and-events.md#terminal-only).

## MCP for an agent you spawn

```sh
heo service start mcp --stdio
```

runs MCP over the process's stdin and stdout, for an agent that spawns
heo itself. That process is not the daemon: it relays every daemon
command over the control socket, so the daemon verifies the spawning
user as the socket's peer, and the store stays the daemon's. With no
daemon running it refuses to start, exit 70. Its pid is never written
to the pidfile.
