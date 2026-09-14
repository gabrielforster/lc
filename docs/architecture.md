# Architecture

## Why the direction inverts

A machine behind NAT cannot accept inbound connections. That is the entire
problem, and no amount of work on the server side changes it.

So nothing reaches in. The agent **dials out** to the server and holds that one
connection open. When somebody connects to the server, the server asks the agent
— over the connection the agent already opened — to carry a new stream. The
agent dials the local service and both halves pipe bytes.

Only one connection crosses the NAT boundary, and it is opened from the inside.
Everything else rides inside it.

```
              ┌──────────────── VPS (lcd) ────────────────┐         NAT
 browser ────▶│ :443/:80  HTTP reverse proxy, by Host     │           │
 mc client ──▶│ :25565    sniffing router, by handshake   │─ yamux ───┼─▶ agent (lc) ─▶ local services
 tcp client ─▶│ :20000+   per-tunnel listener             │  session  │
              │ :7000     control listener                │◀── dials ─┤
              └───────────────────────────────────────────┘           │
```

## Two kinds of frontend

The server routes at two different layers, deliberately.

**L4 — raw TCP and Minecraft.** Byte piping. The routing key comes either from
which port the connection arrived on (raw TCP has no key in the stream) or from
peeking at the first bytes without consuming them (Minecraft). Nothing between
the client and the local service interprets the traffic.

**L7 — HTTP and HTTPS.** A real `httputil.ReverseProxy`. Because TLS terminates
on the server, it already holds the parsed request, so routing through the
standard library's proxy gets correct forwarded headers, keep-alive reuse and
WebSocket upgrades for free. Rebuilding those over a byte pipe would be work
spent reimplementing something already correct.

## Where protocol knowledge lives

Minecraft is not a special case inside the server. It is two small pieces
plugged into general seams:

- a `sniff.Sniffer`, which the L4 router asks for a routing key, and
- an `agent.Transform`, which the agent applies to a connection's first bytes
  before handing it to the local service.

The router knows only "ask the sniffer for a hostname". Adding TLS SNI routing
later means writing one more `Sniffer`, not touching the router.

## Packages

| Package | Responsibility |
|---|---|
| `internal/netutil` | Peek-without-consuming, half-closing `Join`, idle-connection reclamation |
| `internal/muxproto` | The agent/server control protocol |
| `internal/store` | SQLite: tokens, grants, domain claims, port reservations |
| `internal/registry` | Grant enforcement, plus which agent serves what *right now* |
| `internal/server` | Control listener, L4 router, L7 proxy, TLS certificate sources |
| `internal/agent` | Session lifecycle, reconnect, per-kind transforms |
| `internal/sniff` | Routing keys from a connection's opening bytes |
| `internal/mcproto` | The slice of the Minecraft protocol this needs |
| `internal/mctest`, `internal/lctest` | Fake Minecraft peers; in-process end-to-end harness |

## Durable versus live state

The split between `store` and `registry` is load-bearing.

**`store` is durable.** Who owns which domain, which port is reserved for which
tunnel. It survives restarts, because it must: an agent that reconnects has to
get its hostname and port back, or every reconnect would hand users a new
address.

**`registry` is live.** Which agent session is serving a tunnel at this instant.
A restart forgets all of it, correctly — those sessions are gone.

So disconnecting releases the live tunnel while leaving the claim intact. That
is what makes reconnection invisible to users.

## Two subtleties worth knowing

**Half-close, and its opposite.** A plain pair of `io.Copy` calls leaks
connections. When one side finishes sending, the peer must see EOF while still
being able to send; if that signal is dropped, a peer waiting to receive never
learns the other side is done. The failure mode is gradual resource exhaustion
rather than anything visible, so `netutil.Join` handles it once, centrally,
instead of at each call site. `yamux.Stream.Close` is already a half-close — it
sends a FIN and leaves reads working — but it is spelled `Close`, so it is
adapted rather than used directly.

The inverse case matters just as much. A half-close is only correct when a copy
ended in a *clean* EOF. If it ended in an error the connection is broken, not
finished, and half-closing would leave the opposite direction blocked forever on
a peer that is gone — precisely the connection an idle timeout is trying to
reclaim. So `Join` tears both sides down on error, and expires their deadlines
first, because closing alone would not wake a goroutine already blocked reading
a yamux stream.

**Peeking.** A sniffer must not consume the bytes it inspects: the local service
needs an intact protocol. `netutil.PeekConn` routes reads through the same
buffered reader that `Peek` fills, so there is no separate replay path that
could be forgotten. Peeking is also incremental — a packet's length is decoded
first and exactly that many bytes are then peeked. Asking for a fixed upper
bound would block forever, since every real handshake is far smaller than any
sensible bound.
