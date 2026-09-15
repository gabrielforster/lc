# Deploying lc

Two machines: a **VPS** with a public address running `lcd`, and the **machine
behind NAT** running `lc`. This walks through both, with and without a domain.

> [!IMPORTANT]
> The control connection between agent and server is currently **plaintext**,
> and the agent's token crosses it in the clear — as does all tunnelled traffic,
> since every tunnel rides inside that one session. Over the public internet,
> put the control port behind a VPN (WireGuard, Tailscale) or an SSH tunnel
> until [#12](https://github.com/gabrielforster/lc/issues/12) lands. The public
> frontends are unaffected: HTTPS still terminates properly on the server.

## What the VPS needs

Modest. The server shuffles bytes and holds sockets; it does not process them.

| | |
|---|---|
| CPU / RAM | 1 vCPU, 512 MB is plenty for a handful of tunnels. The smallest tier at any provider works |
| Disk | A few hundred MB. The SQLite file is kilobytes; certificates are smaller |
| Bandwidth | **The real constraint.** Every byte a user sends crosses the VPS twice — in from the user, out to the agent. A metered plan is consumed at twice the traffic you serve |
| OS | Anything with systemd. No runtime to install: Go builds a static binary |
| Kernel | Nothing special. Raise `LimitNOFILE` if you expect many concurrent connections |

Bandwidth deserves the emphasis. A Minecraft server with a few players is
negligible, but a busy HTTP tunnel or a file transfer is not, and the doubling
is easy to forget when reading a provider's quota.

## Ports

Only open what you actually enable.

| Port | Flag | Who reaches it | Required |
|---|---|---|---|
| `7000` | `--control` | The agent only | Always |
| `80` | `--http` | Everyone | For HTTP tunnels, and for ACME challenges |
| `443` | `--https` | Everyone | For HTTPS tunnels |
| `25565` | `--minecraft` | Players | For Minecraft tunnels |
| `20000-20100` | `--port-min` / `--port-max` | Everyone | For `tcp` tunnels |

Narrow the control port to the agent's address if it is static. It is the only
port where a leaked token is directly useful:

```sh
ufw allow from 203.0.113.9 to any port 7000 proto tcp   # the agent's address
ufw allow 80,443/tcp
ufw allow 25565/tcp
ufw allow 20000:20100/tcp
ufw enable
```

Binding 80 and 443 needs privilege. Rather than running as root, grant the
capability in the unit file — shown below.

## Installing the server

```sh
# On a build machine, or on the VPS if Go is installed.
git clone https://github.com/gabrielforster/lc.git && cd lc
CGO_ENABLED=0 go build -o lcd ./cmd/lcd

sudo install -m 0755 lcd /usr/local/bin/lcd
sudo useradd --system --home /var/lib/lc --create-home lc
```

`CGO_ENABLED=0` is deliberate: the SQLite driver is pure Go, so the binary is
static and can be built anywhere and copied to the VPS.

Optionally install shell completion for the subcommands, flags and the `--tls`
and `--kind` value sets:

```sh
lcd completion bash | sudo tee /etc/bash_completion.d/lcd >/dev/null
```

Mint a credential for the agent:

```sh
sudo -u lc lcd admin token --db /var/lib/lc/lc.db --label home
# token id: 1
# secret:   3f9a...    <- shown once; copy it now
```

Then grant it what it may claim — see [Running lc](operations.md) for the grant
kinds.

### systemd unit

`/etc/systemd/system/lcd.service`:

```ini
[Unit]
Description=lc tunnel server
After=network-online.target
Wants=network-online.target

[Service]
User=lc
Group=lc
WorkingDirectory=/var/lib/lc
ExecStart=/usr/local/bin/lcd \
  --db /var/lib/lc/lc.db \
  --control :7000 \
  --http :80 \
  --https :443 \
  --minecraft :25565 \
  --public-host tunnel.example.com \
  --tls autocert \
  --tls-cache /var/lib/lc/certs
Restart=always
RestartSec=5s

# Lets it bind 80 and 443 without running as root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/lc
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now lcd
journalctl -u lcd -f
```

`Restart=always` matters more than usual: there is no graceful drain yet
([#7](https://github.com/gabrielforster/lc/issues/7)), so a restart cuts
in-flight connections. Agents reconnect on their own with backoff.

## With a custom domain

The better setup. It gives you real certificates, memorable addresses, and as
many hostname-routed tunnels as you like.

### DNS

Point a name at the VPS, and a wildcard beneath it so new subdomains cost no DNS
work:

```
tunnel.example.com.      IN A     203.0.113.5
*.tunnel.example.com.    IN A     203.0.113.5
```

Then run with the domain as `--public-host`. To let agents claim their own
subdomains at runtime, add `--allow-custom-domains`, and reserve any name the
server itself uses:

```sh
lcd ... --public-host tunnel.example.com \
        --allow-custom-domains \
        --reserved-hosts tunnel.example.com
```

A token granted `wildcard .tunnel.example.com` can then register anything below
it. With `--tls autocert`, certificates are issued on first request for any
claimed name — the allowlist is read from the claims table, so the server cannot
be used to mint certificates for names nobody owns.

### For Minecraft players

An `SRV` record lets players type a bare hostname with no port:

```
_minecraft._tcp.mc.example.com.  IN SRV 0 5 25565 mc.example.com.
```

### Verifying

```sh
curl -I https://web.tunnel.example.com/          # real certificate, no warning
dig +short SRV _minecraft._tcp.mc.example.com    # if you added one
```

## Without a domain

Everything works except public TLS — and one limitation is worth knowing before
you commit to this.

### What works

**`tcp` tunnels work normally.** They route by port, so they never needed a
hostname:

```json
{"name": "ssh", "kind": "tcp", "local_addr": "127.0.0.1:22"}
```

Users connect to `203.0.113.5:20000`. Run `lcd --public-host 203.0.113.5` so the
address reported back to the agent is the one users should actually use.

**`http` and `minecraft` tunnels work too, using the IP as the hostname.** This
is not a special case in the code: HTTP routes on the `Host` header, which is
the IP when someone types `http://203.0.113.5/`, and a Minecraft client puts
whatever the player typed into its handshake. So set `host` to the server's IP:

```json
{"name": "web", "kind": "http", "host": "203.0.113.5", "local_addr": "127.0.0.1:9999"}
```

```sh
lcd admin grant --token 1 --kind host --value 203.0.113.5
```

Players connect to `203.0.113.5` directly, and real player IPs are still
forwarded.

### The limitation

**You get one hostname-routed tunnel in total, not one per kind.** Live tunnels
are keyed by hostname alone, so an `http` and a `minecraft` tunnel both claiming
the server's IP conflict, and the second is refused:

```
conflict: registry: host already served by a live tunnel
```

With a domain you would use two subdomains and never notice. Without one, the IP
is the only hostname you have. So pick one — HTTP **or** Minecraft — and use
`tcp` tunnels, which are unlimited, for everything else. Tracked in
[#11](https://github.com/gabrielforster/lc/issues/11).

### No public HTTPS

Let's Encrypt does not issue certificates for bare IP addresses, so `--tls
autocert` cannot work without a domain. The options:

- **Plain HTTP.** Fine for something behind a VPN or for testing; do not send
  credentials over it.
- **`--tls selfsigned`.** Real TLS, but browsers show a warning and clients need
  the CA added explicitly. Reasonable for a private API, useless for anything a
  browser visits casually.
- **`--tls files`** with a certificate from elsewhere, which still needs a name.

### A middle ground: wildcard DNS services

`sslip.io` and `nip.io` resolve any name containing an IP straight to that IP,
with nothing to configure:

```
203.0.113.5.sslip.io   →  203.0.113.5
web.203.0.113.5.sslip.io  →  203.0.113.5
```

That gives you real hostnames — so multiple hostname-routed tunnels, and the
collision above disappears. Certificates are less dependable: these domains are
shared by everybody, so Let's Encrypt rate limits are frequently already
exhausted, and issuance may simply fail. Treat HTTPS there as best-effort. For
anything you rely on, a cheap domain is a better answer.

## Setting up the client

The agent needs **no inbound ports and no public address** — that is the whole
point. It only needs to reach the server's control port.

```sh
CGO_ENABLED=0 go build -o lc ./cmd/lc
sudo install -m 0755 lc /usr/local/bin/lc
sudo mkdir -p /etc/lc
```

`/etc/lc/lc.json`:

```json
{
  "server": "tunnel.example.com:7000",
  "token": "3f9a...",
  "idle_timeout": "15m",
  "tunnels": [
    {"name": "web",      "kind": "http",      "host": "web.tunnel.example.com", "local_addr": "127.0.0.1:3000"},
    {"name": "survival", "kind": "minecraft", "host": "mc.example.com",          "local_addr": "127.0.0.1:25565"},
    {"name": "ssh",      "kind": "tcp",       "local_addr": "127.0.0.1:22"}
  ]
}
```

The file holds a credential, so keep it unreadable by other users:

```sh
sudo chmod 0600 /etc/lc/lc.json
sudo chown lc-agent:lc-agent /etc/lc/lc.json
```

Raise `idle_timeout` or remove it if you tunnel SSH and want long silent
sessions to survive — see [Running lc](operations.md#idle-timeouts).

### systemd unit

`/etc/systemd/system/lc.service`:

```ini
[Unit]
Description=lc tunnel agent
After=network-online.target
Wants=network-online.target

[Service]
User=lc-agent
Group=lc-agent
ExecStart=/usr/local/bin/lc --config /etc/lc/lc.json
Restart=always
RestartSec=5s

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl enable --now lc
journalctl -u lc -f
```

Expect `tunnel ready` lines naming each public address. `Restart=always` covers
the case the agent gives up: an `auth` or `forbidden` rejection stops it
deliberately rather than retrying, since neither fixes itself, and systemd will
keep retrying the whole process.

The agent must reach local services over the loopback or LAN, so run it on the
same machine as them, or somewhere that can dial them.

## Minecraft: the firewall rule is not optional

Forwarding real player IPs requires `online-mode=false` on the Minecraft server,
which disables Mojang session verification entirely. The whitelist then matches
on username alone.

**Bind the Minecraft server to loopback, or firewall its port, so the tunnel is
the only way in.** If it is reachable directly, anyone can claim any username:

```properties
server-ip=127.0.0.1
online-mode=false
```

Verify from another machine:

```sh
nc -vz your-home-address 25565   # must be refused or filtered
```

Full explanation in [the Minecraft docs](minecraft.md#security-online-modefalse-and-why-the-firewall-is-load-bearing),
and [#1](https://github.com/gabrielforster/lc/issues/1) tracks removing the
tradeoff.

## Checking it works

On the server:

```sh
sudo -u lc lcd admin list --db /var/lib/lc/lc.db   # tokens, grants, domains, ports
journalctl -u lcd | grep "tunnel open"
```

From anywhere:

```sh
curl -H 'Host: web.tunnel.example.com' http://203.0.113.5/    # HTTP tunnel
nc -vz 203.0.113.5 20000                                      # tcp tunnel
```

There is no status endpoint yet
([#9](https://github.com/gabrielforster/lc/issues/9)), so the logs and
`lcd admin list` are the available visibility.

## Common problems

| Symptom | Cause |
|---|---|
| `conflict: host already served by a live tunnel` | Two tunnels claim one hostname. Another agent holds it, or you hit [#11](https://github.com/gabrielforster/lc/issues/11) with an `http` and a `minecraft` tunnel on the same name |
| `unknown shorthand flag: 'c' in -control` | A command line from before the cobra migration. Flags take two dashes now: `--control`. The binary says so under the error |
| `auth: unknown or disabled token` | Wrong secret, or the wrong `--db` file. The agent stops rather than retrying |
| `forbidden` on registration | No grant covers the hostname or port. Check `lcd admin list` |
| `502` from an HTTP tunnel | No agent connected for that hostname, or the local service is down |
| A Minecraft client cannot connect | The hostname it typed must match the tunnel's `host` exactly — that string is the routing key |
| Certificate errors with `autocert` | DNS must point at the server and port 80 must be reachable, before a certificate can be issued |
| Everyone appears as `127.0.0.1` in Minecraft | The tunnel's `kind` is `tcp` rather than `minecraft`, so no handshake rewrite happens |
