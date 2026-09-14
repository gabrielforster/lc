# Minecraft tunnels

Java Edition only. Bedrock is UDP, and this tunnel carries TCP.

## How routing works

The first packet a Java client sends is the handshake, and it contains the
address the player typed:

| Field | Type | |
|---|---|---|
| packet length | VarInt | |
| packet id | VarInt | `0x00` |
| protocol version | VarInt | |
| **server address** | String | **the routing key** |
| server port | u16 | |
| next state | VarInt | 1 = status ping, 2 = login |

The server peeks at that field without consuming it, looks up which tunnel
serves that hostname, and pipes the connection there. So **one port serves every
Minecraft tunnel** — the same trick that lets a hosted tunnel service hand out
`something.example.com` on the standard port.

Two details matter. The bytes must not be consumed, or the Minecraft server
receives a truncated handshake. And peeking is incremental — the packet length
is decoded first, then exactly that many bytes are read — because asking for a
fixed upper bound blocks forever on a handshake smaller than that bound, which
is every real handshake.

Clients modded with Forge append `\0FML\0` to the address, so the routing key is
taken up to the first null byte. Comparison is case-insensitive and ignores a
trailing dot.

## Real player IPs

Without extra work, every player appears to the server as `127.0.0.1` — the
agent's own connection. That breaks bans, region plugins, and anything else that
looks at an address.

The agent fixes this by rewriting the handshake's address field in the
BungeeCord format, four null-separated values:

```
hostname \0 client IP \0 UUID \0 properties JSON
```

The ordering is the awkward part. The payload belongs in the **handshake**, but
the UUID it carries derives from the username, which arrives in the **next**
packet. So the agent holds the handshake, reads Login Start, computes the UUID,
then writes both on.

The UUID is the version-3 UUID of `OfflinePlayer:<name>` — exactly what a
vanilla server computes when `online-mode=false`. Matching it is what keeps
player data, bans and whitelist entries pointing at the same player. The
properties field is an empty JSON array, because this tunnel does not
authenticate with Mojang and so has no signed profile to pass along.

Server-list pings (next state 1) send no login packet, so they pass through
untouched. Holding one while waiting for a login that never arrives would hang
the client's server list.

## Server configuration

```properties
online-mode=false
```

Then point a tunnel at it:

```json
{"name": "survival", "kind": "minecraft", "host": "mc.example.com", "local_addr": "127.0.0.1:25565"}
```

## Security: online-mode=false, and why the firewall is load-bearing

> [!WARNING]
> **`online-mode=false` disables Mojang session verification entirely.** The
> server no longer checks that a connecting player owns the account they claim.
>
> The whitelist then matches on **username alone**. It answers "is this name on
> the list", not "does this person own this account" — so anyone who can reach
> the server's port directly can type a whitelisted name and join as that
> player, with their permissions and their inventory.
>
> **Firewall port 25565 so it is reachable only through the tunnel.** That rule
> is not hardening you add later; it is the only thing standing between the
> setup and trivial impersonation.

Why the tradeoff exists at all: IP forwarding requires the server to trust what
the proxy puts in the handshake. A server that trusts that field *and* is
directly reachable will trust anyone who connects to it and writes one.

So the two settings are a package. `online-mode=false` is safe **only** while
the tunnel is the sole path to the port. If you remove the firewall rule, or
bind the Minecraft server to a public interface, or expose it through a second
tunnel without forwarding, the protection is gone.

### Checking it

From another machine, the port must not answer:

```sh
nc -vz your-server-address 25565   # expect: refused or filtered
```

If that connects, the firewall rule is missing or not doing what you think.

### What would remove the tradeoff

The tunnel performing the Mojang handshake itself — authenticating players at
the edge, the way BungeeCord and Velocity do — would let the Minecraft server
keep `online-mode=true` and trust forwarded data because the proxy verified it.
That means implementing encryption and session-server calls. **It is not
implemented**, and it is the right fix if this is ever exposed to people you do
not know.

Until then, treat `-allow-custom-domains` and a public Minecraft port as a
friends-and-family arrangement backed by a firewall rule, not as multi-tenant
hosting.

## Nicer addresses

An `SRV` record lets players type a bare hostname with no `:25565`:

```
_minecraft._tcp.mc.example.com.  IN SRV 0 5 25565 mc.example.com.
```

A wildcard `A` record (`*.mc.example.com`) means handing out a new subdomain
costs no DNS work at all.

## Verifying a real server

```sh
lcd -db lc.db -control :7000 -minecraft :25565 -public-host mc.example.com
lc -config lc.json
```

Join through the public hostname. In the server console, `/list` and the join
message should show real addresses rather than `127.0.0.1`. If they show
`127.0.0.1`, the tunnel is carrying the connection but the rewrite is not being
applied — check that the tunnel's `kind` is `minecraft` and not `tcp`.
