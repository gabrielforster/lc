# Control protocol

The agent and server speak newline-delimited JSON over a
[yamux](https://github.com/hashicorp/yamux) session. Control traffic is tiny
next to tunnelled bytes, so readability beats compactness — a session can be
inspected with `nc`.

## Session setup

The agent dials, so the agent is the yamux **client** and the server is the
yamux **server**. Either side may open streams once the session exists, which is
what makes the inversion work: the server opens a stream *towards* the agent for
every inbound public connection.

```
agent                                   server
  │  TCP connect ───────────────────────▶│
  │  yamux.Client                        │  yamux.Server
  │                                      │
  │  open stream #1 (control)            │
  │  Hello{token, tunnels} ─────────────▶│
  │                                      │  authenticate, check grants,
  │                                      │  reserve ports / bind hostnames
  │◀───────────── HelloOK{tunnels[]}     │
  │                                      │
  │           ... a user connects ...    │
  │◀──── open stream #2                  │
  │◀──── StreamInit{tunnel_id, client}   │
  │  dial local service, pipe bytes ────▶│
```

If registration fails, the server replies `Error` instead of `HelloOK` and
closes. Tunnels registered before the failure are released, so a partial
registration never leaves half a session live.

## Messages

Every frame is `{"type": "...", "body": {...}}` on one line.

| Type | Direction | Purpose |
|---|---|---|
| `hello` | agent → server | Authenticate and request tunnels |
| `hello_ok` | server → agent | Accepted, with each tunnel's public address |
| `stream_init` | server → agent | Header on a new stream: which tunnel, and the client's address |
| `claim_domain` | agent → server | Request a custom hostname |
| `claim_result` | server → agent | Granted, or a typed reason why not |
| `list_domains` | agent → server | List hostnames this token owns |
| `domain_list` | server → agent | The answer |
| `release_domain` | agent → server | Give up a claim |
| `error` | server → agent | Replaces any success message |

An `error` frame is surfaced to the caller as an error whatever reply it was
waiting for, so callers do not have to check for it separately.

An unknown message type is answered with an `error` rather than dropping the
session, so a newer agent asking an older server for something it does not
understand stays usable.

## StreamInit and real client addresses

`StreamInit` carries `client_addr` on **every** inbound stream, regardless of
tunnel kind.

This is deliberate. It means forwarding a real client address is a per-tunnel
decision made on the agent — rewrite a Minecraft handshake, set an HTTP header,
or ignore it entirely for raw TCP — rather than a protocol change that would
have to be retrofitted later. The server always reports; each kind decides what
to do with it.

## Liveness

Session liveness uses yamux's built-in keepalive (`EnableKeepAlive`, 15s
interval) rather than a hand-rolled heartbeat. NAT tables drop idle mappings
silently, so the session needs traffic of its own to survive between
connections.

Reading the control stream doubles as disconnect detection on the server: the
read fails when the agent goes away, which is what triggers releasing its
tunnels.

## Reconnection

The home connection dropping is normal operation, not an error path. The agent
reconnects with exponential backoff, jittered so that many agents do not return
in lockstep after a server restart.

Two failures are **not** retried, because they will not fix themselves: `auth`
(unknown or disabled token) and `forbidden` (no grant covers the request). The
agent stops instead of spinning.

Because claims are durable, a reconnecting agent re-registers the same hostname
and gets back the same port.
