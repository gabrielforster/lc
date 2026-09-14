# lc

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

The direction inverts: nothing dials into the home network. The agent dials
**out** and holds one session open, and the server pushes a new multiplexed
stream down it for each inbound connection.

## Tunnel kinds

| Kind | Routed by | Notes |
|---|---|---|
| `tcp` | a dedicated public port | Assigned from a range, then **reserved** — the same tunnel gets the same port back across reconnects and server restarts |
| `http` | `Host` header | Proxied at L7, so `X-Forwarded-For`/`-Proto`, keep-alive and WebSocket upgrades work |
| `minecraft` | hostname in the Java handshake | One port serves every Minecraft tunnel; the agent rewrites the handshake so players keep their real IPs |

## Quick start, entirely local

```sh
go build -o lcd ./cmd/lcd
go build -o lc  ./cmd/lc

# Mint a credential and grant it what it may claim.
./lcd admin token -label laptop            # prints the secret once
./lcd admin grant -token 1 -kind port_auto
./lcd admin grant -token 1 -kind wildcard -value .mc.localhost

./lcd -control 127.0.0.1:7000 -http 127.0.0.1:8080 -minecraft 127.0.0.1:25565
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
./lc -config lc.json
curl -H 'Host: web.mc.localhost' http://127.0.0.1:8080/
```

## Custom domains

With `lcd -allow-custom-domains`, an agent can claim an unclaimed hostname at
runtime, first-come-first-served:

```sh
./lc -config lc.json domains claim mc.example.com
./lc -config lc.json domains list
./lc -config lc.json domains release mc.example.com
```

Claiming only records ownership on the server. The name resolves — and a
certificate can be issued for it — once **you** point its DNS at the server.

## TLS

`-tls` selects where certificates come from:

- `autocert` — Let's Encrypt. The host allowlist is the set of claimed domains,
  checked per handshake, so the server cannot be used to mint certificates for
  names nobody owns.
- `files` — a certificate and key you supply.
- `selfsigned` — development. Exercises the real HTTPS path with no DNS and no
  ACME round trip; clients must be told to trust the generated CA.

## Running a Minecraft server behind it

Real player IPs require the BungeeCord handshake rewrite, which the agent does
automatically for `minecraft` tunnels. On the Minecraft server:

```properties
online-mode=false
```

> [!WARNING]
> `online-mode=false` disables Mojang session verification. The whitelist then
> matches on **username**, so anyone who can reach the server's port directly
> can claim any username, whitelisted or not. **Firewall port 25565 so it is
> reachable only through the tunnel** — that rule is the only thing making the
> setup safe, and it is load-bearing.
>
> The proper fix is for the tunnel to perform the Mojang handshake itself,
> Velocity-style. That is not implemented.

For players to type a bare hostname with no `:25565`, add an `SRV` record on
`_minecraft._tcp`.

## State

Server state lives in a SQLite file (`-db`, default `lc.db`): tokens, their
grants, claimed domains and port reservations. Token secrets are stored hashed
and shown only once, at creation.

## Tests

```sh
go test ./...          # unit plus in-process end-to-end
go test -race ./...
```

The end-to-end tests run a real server and agent over real sockets, including a
fake Minecraft client and server, so no JVM is needed. For a manual check
against a real `server.jar`, point a `minecraft` tunnel at it and join through
the public hostname; `/list` should show real addresses rather than
`127.0.0.1`.

## Not implemented

Bedrock (it is UDP); the tunnel authenticating with Mojang; multi-server or HA.
The web UI is not built either, but the registry is SQLite rather than a config
file so that adding one later is a frontend over `internal/store`.
