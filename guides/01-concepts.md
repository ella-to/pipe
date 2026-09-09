# How two pipe peers find each other and talk

This guide explains the moving parts behind a pipe connection: what a peer ID
is, what the signaling server does, what STUN and TURN are for, and what
actually crosses the network at each step. Read it once and the rest of the
guides will make sense; skip it and the first NAT you meet will be confusing.

## The one-paragraph version

Two programs each create a `pipe.Endpoint` with a name (a peer ID) and a
`Signaler`. One calls `Listen`, the other calls `Dial("name")`. Pipe uses the
signaler to exchange a WebRTC offer, an answer, and network candidates. ICE
tries every pair of candidates until one works: a direct LAN path, a path
across NATs discovered with STUN, or, when nothing else works, a TURN relay.
Once a path exists, DTLS encrypts it and SCTP carries a reliable ordered stream
over it. You get a `net.Conn` and never see any of that.

```
  alice                          signaling server                       bob
    |  POST offer (SDP)  ------------->  |  -------- event: offer ------>  |
    |  <------ event: answer ---------  |  <------- POST answer (SDP) ---  |
    |  POST candidate ... ------------>  |  -------- event: candidate -->   |
    |  <----- event: candidate -------  |  <------- POST candidate ...     |
    |                                                                      |
    |<=================== ICE connectivity checks (UDP) ==================>|
    |<=================== DTLS + SCTP: your bytes ========================>|
```

The signaling server sees only the messages above the line. Your bytes take
the lower path, peer to peer, or through a TURN relay you control.

## Peer IDs

A peer ID is a string, at most 128 bytes, printable, no surrounding
whitespace. `"alice"`, `"laptop-7f3a"`, and `"user:42/device:9"` are all fine.

A peer ID is a *routing* name. Pipe delivers signals to whoever the signaling
transport says owns that name. Whether that binding can be trusted is entirely
the signaling transport's job. The bundled HTTP signaler (`signaling/sse`)
authenticates every request and refuses signals whose `From` does not match the
authenticated identity, so with it a peer ID *is* authenticated. With a
signaler that trusts client-supplied names, it is not.

Two endpoints may not share one peer ID at the same time on the same signaling
server. Give each device its own.

## Signaling

Pipe defines the signaling *protocol* (the `pipe.Signal` envelope: version,
IDs, kind, from, to, payload) and leaves the *transport* to you through two
small interfaces:

```go
type Signaler interface {
	Open(ctx context.Context, local PeerID) (SignalConn, error)
}

type SignalConn interface {
	Send(ctx context.Context, msg Signal) error
	Receive(ctx context.Context) (Signal, error)
	Close() error
}
```

The repository ships two transports:

- `signaling/memory`: an in-process hub. Both peers must live in one process.
  It is for tests and single-binary demos, and it can inject faults.
- `signaling/sse`: HTTP. Peers receive signals over a Server-Sent Events
  stream and send them with POST. A small server routes envelopes between
  authenticated peers. This is what two machines use. See
  [03-signaling-server.md](03-signaling-server.md).

Delivery is at-least-once. Duplicates and reordering are tolerated by pipe
(each signal carries a random ID and receivers keep a window of seen IDs), so
a transport does not need to be perfect, only honest about what it delivered.

What signaling carries per connection: one offer, one answer, a handful of
candidates each way (typically 2 to 10), an end-of-candidates marker each way,
and a close notice. Everything is JSON, and an SDP body is a few KiB. A
Raspberry Pi can signal for thousands of peers.

## ICE, candidates, and why direct paths sometimes fail

Each peer gathers **candidates**: addresses at which it might be reachable.

| Type | Meaning | Needs |
| --- | --- | --- |
| `host` | An address on a local interface. | Nothing. |
| `srflx` (server-reflexive) | The public address a STUN server saw the peer come from. | A STUN server. |
| `prflx` (peer-reflexive) | An address learned during connectivity checks. | Nothing extra. |
| `relay` | An address on a TURN server that forwards to the peer. | A TURN server and credentials. |

ICE pairs every local candidate with every remote candidate and sends
connectivity checks (STUN Binding requests) over each pair. The first pair
that works in both directions is nominated. Preference goes to host, then
reflexive, then relay.

Why a direct path fails:

- **Both peers behind NAT.** A `host` address like `192.168.1.20` means
  nothing to the other side. STUN gives each peer its public `ip:port`, and
  for most home NATs, sending to that pair from the other peer works. This is
  hole punching and it is what STUN exists for.
- **Symmetric NAT or a strict firewall.** Some NATs allocate a fresh port per
  destination, so the port STUN saw is not the port the peer will use. Some
  networks (corporate, mobile carriers, hotels) block UDP to unknown hosts
  altogether. Hole punching fails.
- **No UDP at all.** Then even the relay must be reached over TCP
  (`turn:...?transport=tcp`) or TLS (`turns:`).

TURN covers all of those. A peer asks the TURN server for an **allocation**, a
public `ip:port` on the server. Whatever arrives there is forwarded to the
peer over the connection the peer opened to the server, which works from
behind any NAT because the peer initiated it. Traffic then costs the relay
operator bandwidth, which is why relays need credentials and budgets.

Rule of thumb for what you need:

| Peers | STUN | TURN |
| --- | --- | --- |
| Same LAN | no | no |
| Home networks, typical NATs | yes | fallback for the 10 to 20% that fail |
| Anything that must always work | yes | yes |

Pipe's `ICETransportPolicy` lets you force relay-only, which is how you test
that your relay works and how you keep peer addresses from being revealed to
each other.

## What a connection is made of

Once ICE nominates a pair:

1. **DTLS** handshakes over it. Each side's certificate fingerprint was in the
   SDP exchanged through signaling, so a signaling server that was honest
   yields an end-to-end encrypted link that the server cannot read. The TURN
   server, if any, forwards ciphertext only.
2. **SCTP** runs inside DTLS and provides one reliable, ordered stream with
   its own congestion control.
3. **One DataChannel** labeled `pipe.stream.v1` carries pipe's frames. Each
   frame is an 8-byte header and a payload of at most `FramePayload` bytes
   (16 KiB by default). Data frames carry your bytes; ping and pong frames
   implement keepalive; a close frame ends the stream cleanly.

`pipe.Conn` presents that as a byte stream: `Read` may return part of what a
`Write` sent, deadlines work, `Close` is idempotent, and a peer's orderly
close is reported as `io.EOF`. The compatibility tests in `test/compat` run
`io.Copy`, `bufio`, `encoding/json`, `encoding/gob`, TLS 1.3, and `net/http`
over it.

There is no half-close. When either side closes, both directions end. When the
protocol you run needs "I am done sending but still listening", frame it
yourself (a length prefix, a terminator, or a message type).

## Lifecycle of a connection

```
new -> signaling -> connecting -> connected -> recovering -> connected
                                       \                        /
                                        ---> closing -> closed <-
```

- `Dial` returns when the DataChannel is open, so the first `Write` never
  waits for negotiation.
- Connectivity loss on an established connection triggers **ICE restart**:
  the dialing side sends a new offer through signaling, both sides gather
  fresh candidates, and the same `Conn` continues. The number of attempts and
  the backoff are `Config.Reconnect`.
- Anything that would require a new PeerConnection (a failed restart budget,
  a peer that vanished) closes the `Conn` with `ErrDisconnected`. Pipe never
  silently reconnects underneath a stream, because that could lose, duplicate,
  or reorder bytes without you knowing.
- `Close` sends a close frame, waits (off your goroutine, bounded) for the
  peer to acknowledge everything written, then tears the transport down. The
  peer reads what you wrote and then gets `io.EOF`.

## Who pays for what

| Component | Traffic | Runs where |
| --- | --- | --- |
| Signaling server | KiB per connection | Any small VM; behind TLS |
| STUN server | A few packets per connection | Same host as TURN; public STUN also works |
| TURN server | All relayed bytes, both directions | A host with a public IP and bandwidth to spare |

Signaling and STUN are nearly free. TURN is the cost center, and
[07-multi-user-relay.md](07-multi-user-relay.md) shows how to give free users a
512 KiB/s budget and paying users a larger one from a single server.

## Where to go next

- Two machines talking in ten minutes: [02-quickstart.md](02-quickstart.md)
- Running and securing the signaling server: [03-signaling-server.md](03-signaling-server.md)
- STUN: [04-stun.md](04-stun.md)
- Your own TURN relay: [05-turn-relay.md](05-turn-relay.md)
- Threat model and hardening: [06-security.md](06-security.md)
- Plans, quotas, ephemeral credentials: [07-multi-user-relay.md](07-multi-user-relay.md)
- Docker: [08-docker.md](08-docker.md)
- Using the Go API well: [09-client-api.md](09-client-api.md)
- Operations, tuning, troubleshooting: [10-operations.md](10-operations.md)
- Wire protocol reference: [11-protocol.md](11-protocol.md)
