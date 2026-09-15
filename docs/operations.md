# Running lc

## The command line

Both binaries are [cobra](https://github.com/spf13/cobra) command trees, so
`--help` works at every level and describes exactly the command you asked about:

```
lcd                          run the server
lcd admin token              mint a credential
lcd admin grant              allow a token to claim a host, zone or port
lcd admin list               tokens with their grants, domains and ports
lcd completion <shell>       shell completion script

lc                           run the agent
lc domains claim <domain>    claim a hostname
lc domains list              hostnames this token has claimed
lc domains release <domain>  give one back
lc completion <shell>        shell completion script
```

> [!IMPORTANT]
> **Flags take two dashes.** cobra parses with pflag, which follows POSIX: a
> single dash introduces short flags only, so `-control :7000` is read as the
> shorthand flags `-c -o -n ...` and rejected. Write `--control :7000`.
>
> Versions before the cobra migration accepted a single dash. If you have a
> systemd unit or script from then, this is the only change it needs — and the
> binaries detect it, so the error tells you what to write instead:
>
> ```
> $ lcd -control :7000
> lcd: unknown shorthand flag: 'c' in -control
>
> Flags now take two dashes: --control
> ```

Completion covers subcommands, flag names and the value sets for `--tls` and
`lcd admin grant --kind`:

```sh
lcd completion zsh  > "${fpath[1]}/_lcd"
lc  completion zsh  > "${fpath[1]}/_lc"
# bash, fish and powershell are also available; each prints its own install
# instructions under `completion <shell> --help`.
```

## Server flags (`lcd`)

| Flag | Default | Meaning |
|---|---|---|
| `--db` | `lc.db` | SQLite state file |
| `--control` | `:7000` | Where agents connect |
| `--http` | `:8080` | Public HTTP listener, empty to disable |
| `--https` | *(off)* | Public HTTPS listener |
| `--minecraft` | *(off)* | Public Minecraft listener, e.g. `:25565` |
| `--port-min`, `--port-max` | `20000`, `20100` | Range for automatically assigned TCP ports |
| `--public-host` | `127.0.0.1` | Hostname users reach this server on, used when reporting a tcp tunnel's address |
| `--allow-custom-domains` | `false` | Let agents claim unreserved hostnames |
| `--reserved-hosts` | *(none)* | Comma-separated names the server keeps for itself |
| `--max-conns` | `256` | Concurrent public connections per tunnel, 0 for unlimited |
| `--idle-timeout` | `15m` | Close proxied connections after this long without traffic, 0 to disable |
| `--tls` | `autocert` | `autocert`, `files` or `selfsigned` |
| `--tls-cert`, `--tls-key` | | Certificate and key, for `--tls=files` |
| `--tls-cache` | `lc-certs` | Certificate cache directory, for `--tls=autocert` |
| `--debug` | `false` | Verbose logging |

## Agent config (`lc.json`)

```json
{
  "server": "vps.example.com:7000",
  "token": "<secret from lcd admin token>",
  "idle_timeout": "15m",
  "tunnels": [
    {"name": "web",      "kind": "http",      "host": "web.example.com", "local_addr": "127.0.0.1:3000"},
    {"name": "survival", "kind": "minecraft", "host": "mc.example.com",  "local_addr": "127.0.0.1:25565"},
    {"name": "ssh",      "kind": "tcp",       "local_addr": "127.0.0.1:22"},
    {"name": "db",       "kind": "tcp",       "local_addr": "127.0.0.1:5432", "public_port": 20005}
  ]
}
```

`name` must be unique within an agent — it is what a port reservation is keyed
to, so renaming a tunnel gives it a new port. `host` is required for `http` and
`minecraft`; `public_port` is optional for `tcp` and means "give me this exact
port", which succeeds only if it is free or already yours.

`idle_timeout` is optional and covers the agent's side only; the server enforces
its own on the public side.

## Administration

Tokens and grants live in the SQLite file. Until the UI exists, `lcd admin`
manages them:

```sh
lcd admin token --label laptop          # mint a credential, printing the secret once
lcd admin grant --token 1 --kind port_auto
lcd admin grant --token 1 --kind wildcard --value .mc.example.com
lcd admin list                         # tokens with their grants, domains and ports
```

Token secrets are stored **hashed** and shown only at creation — a listing
cannot leak a working credential, which matters once a UI can display them.

### Grant kinds

| Kind | Value | Permits |
|---|---|---|
| `host` | `mc.example.com` | That exact hostname |
| `wildcard` | `.mc.example.com` | Any name *below* that zone; the zone itself is excluded |
| `port` | `20005` | That exact public port |
| `port_auto` | *(empty)* | Automatic port assignment from the configured range |

A token may also use any hostname it has already claimed, and any port already
reserved to it, without a matching grant.

## Ports are reservations, not assignments

An automatically assigned port is recorded in SQLite against
`(token, tunnel name)`. The same tunnel gets the same port back after a
reconnect *or* a server restart.

This is why ports live in the database rather than in memory: purely dynamic
allocation would hand users a new address every time the home connection
blipped.

## Idle timeouts

A peer that vanishes — a closed laptop, a dropped NAT mapping, a crashed client
— never sends a FIN. Nothing errors; the connection simply sits there holding a
public socket, a yamux stream and a socket to the local service, and counting
against `--max-conns`. Because nothing fails, this is invisible until resources
run out.

`--idle-timeout` closes connections that have moved no bytes in either direction
for that long. Traffic in **either** direction counts, so a long transfer is
never interrupted. The server reclaims on its own: it does not depend on the
local service noticing anything, so a service that holds connections open
without reacting is reclaimed like any other.

The agent's optional `idle_timeout` is separate and covers only its own side —
a local service still holding a connection after the server has gone away. The
server's setting is the one that governs public connections.

The timeout cannot distinguish a dead peer from a legitimately quiet one, which
is the tradeoff to be aware of:

- **Minecraft** is safe at any setting — the server sends keepalives every few
  seconds, so a connected player is never idle.
- **HTTP** is safe — pooled connections are closed by the transport's own
  90-second idle timeout well before this one applies.
- **SSH, database sessions and similar** can legitimately sit silent for hours.
  Raise `--idle-timeout`, disable it with `0`, or enable the protocol's own
  keepalive (`ServerAliveInterval` for SSH).

## TLS

| Mode | Use |
|---|---|
| `autocert` | Production. Let's Encrypt, with the host allowlist read from claimed domains at handshake time — so the server cannot be used to mint certificates for names nobody owns |
| `files` | A certificate you supply. Reloaded per handshake, so replacing the files takes effect without a restart |
| `selfsigned` | Development. Exercises the real HTTPS path with no DNS and no ACME round trip; clients must be told to trust the generated CA |

Autocert needs port 80 reachable for HTTP-01 challenges, and a domain only
becomes eligible once it is claimed *and* its DNS points at the server.

## Custom domains

With `--allow-custom-domains`, an agent can claim an unclaimed hostname at
runtime, first-come-first-served:

```sh
lc --config lc.json domains claim mc.example.com
lc --config lc.json domains list
lc --config lc.json domains release mc.example.com
```

A refusal says why: `taken`, `reserved`, `disabled` or `invalid`.

Claiming only records ownership on the server. **You** must point the name's DNS
at the server before it resolves — and until it does, no certificate can be
issued for it either.

## Logs worth recognising

| Message | Meaning |
|---|---|
| `tunnel open` / `tunnel closed` | An agent registered or disconnected |
| `conflict: host already served by a live tunnel` | Two agents claim one hostname; the second retries until the first disconnects |
| `tunnel at connection limit` | `--max-conns` reached; connections are refused before the agent is dialled |
| `no tunnel for host` | Someone reached a listener with a hostname nobody serves |
| `local dial failed` | The agent could not reach the local service — usually it is not running |
