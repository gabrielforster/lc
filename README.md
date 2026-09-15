# lc

**English** · [Português (BR)](README.pt-BR.md) · [Español](README.es.md)

A self-hosted reverse tunnel. It makes services on a machine behind NAT
reachable at a public address, **without asking anyone who connects to install
anything** — a browser, a plain TCP client and a vanilla Minecraft client all
just connect.

```
              ┌──────────────── VPS (lcd) ────────────────┐         NAT
 browser ────▶│ :443/:80  HTTP reverse proxy, by Host     │           │
 mc client ──▶│ :25565    sniffing router, by handshake   │─ yamux ───┼─▶ agent (lc) ─▶ local services
 tcp client ─▶│ :20000+   per-tunnel listener             │  session  │
              │ :7000     control listener                │◀── dials ─┤
              └───────────────────────────────────────────┘           │
```

Nothing dials into the home network. The agent dials **out** and holds one
session open; the server pushes a multiplexed stream down it for each inbound
connection.

## Tunnel kinds

| Kind | Routed by | Notes |
|---|---|---|
| `tcp` | a dedicated public port | Assigned from a range, then **reserved** — the same tunnel gets the same port back across reconnects and restarts |
| `http` | `Host` header | Proxied at L7, so `X-Forwarded-For`/`-Proto`, keep-alive and WebSocket upgrades work |
| `minecraft` | hostname in the Java handshake | One port serves every Minecraft tunnel; the agent rewrites the handshake so players keep their real IPs |

## Quick start, entirely local

```sh
go build -o lcd ./cmd/lcd
go build -o lc  ./cmd/lc

# Mint a credential and grant it what it may claim.
./lcd admin token --label laptop            # prints the secret once
./lcd admin grant --token 1 --kind port_auto
./lcd admin grant --token 1 --kind wildcard --value .mc.localhost

./lcd --control 127.0.0.1:7000 --http 127.0.0.1:8080 --minecraft 127.0.0.1:25565
```

`lc.json` on the machine behind NAT:

```json
{
  "server": "127.0.0.1:7000",
  "token": "<the secret printed above>",
  "tunnels": [
    {"name": "web",      "kind": "http",      "host": "web.mc.localhost",  "local_addr": "127.0.0.1:9999"},
    {"name": "survival", "kind": "minecraft", "host": "play.mc.localhost", "local_addr": "127.0.0.1:25565"},
    {"name": "ssh",      "kind": "tcp",       "local_addr": "127.0.0.1:22"}
  ]
}
```

```sh
./lc --config lc.json
curl -H 'Host: web.mc.localhost' http://127.0.0.1:8080/
```

Both binaries are cobra command trees — `lcd --help`, `lcd admin grant --help`,
`lc domains --help` — and ship shell completion via `lcd completion zsh`.

> [!IMPORTANT]
> Flags take **two** dashes: `--control :7000`, not `-control :7000`. If you have
> a script from before the cobra migration, that is the only change it needs.

## Documentation

| | |
|---|---|
| [Deployment](docs/deployment.md) | VPS setup, systemd, firewall, with and without a domain |
| [Architecture](docs/architecture.md) | How it works and why it is shaped this way |
| [Control protocol](docs/protocol.md) | The agent/server wire protocol |
| [Running lc](docs/operations.md) | The command line, flags, config, grants, TLS, idle timeouts |
| [Minecraft](docs/minecraft.md) | Handshake routing, real player IPs, **and the `online-mode=false` warning** |

> [!WARNING]
> Running a Minecraft server behind this requires `online-mode=false`, which
> disables Mojang session verification and makes the whitelist match on username
> alone. A firewall restricting port 25565 to the tunnel is then the only thing
> preventing trivial impersonation. Read
> [the security section](docs/minecraft.md#security-online-modefalse-and-why-the-firewall-is-load-bearing)
> before exposing a server.

## State

Server state lives in a SQLite file (`--db`, default `lc.db`): tokens, their
grants, claimed domains and port reservations. Token secrets are stored hashed
and shown only once, at creation.

## Tests

```sh
go test ./...
go test -race ./...
```

The end-to-end tests run a real server and agent over real sockets, including a
fake Minecraft client and server, so no JVM is needed.

## Status

Working today: raw TCP, HTTP and HTTPS with TLS termination, Minecraft with real
player IPs, custom domain claims, durable port reservations, reconnection with
backoff, per-tunnel connection caps and idle-connection reclamation.

**The `lc` web UI is not built yet — it is planned.** The registry is SQLite
rather than a config file specifically so that building it is a frontend over
`internal/store`, not a migration: tokens are already hashed rather than stored
in plaintext, and claim failures already carry typed reasons a UI can render.
Until then, `lcd admin` and `lc domains` cover the same ground from the command
line.

Not planned: Bedrock (it is UDP), multi-server or HA. The tunnel authenticating
with Mojang itself is not implemented but is the right fix if Minecraft tunnels
are ever exposed to strangers — see [the Minecraft docs](docs/minecraft.md).
