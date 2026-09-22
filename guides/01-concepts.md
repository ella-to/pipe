# Concepts

```
  alice                     signaling server                      bob
    |  offer, candidates  ------------>  |  ------------>             |
    |             <------------  |  <------------  answer, candidates |
    |                                                                 |
    |<============ ICE checks, then DTLS + SCTP: your bytes ========>|
    |              (direct, or through a TURN relay)                  |
```

1. Each program creates a `pipe.Endpoint` with a **peer ID** (`"alice"`) and a
   **signaler** (how it reaches the signaling server).
2. One calls `Listen`, the other calls `Dial(ctx, "bob")`.
3. Pipe exchanges a WebRTC offer, an answer, and ICE candidates through the
   signaler. ICE finds a working path, DTLS encrypts it, and SCTP makes it
   reliable.
4. You get a `*pipe.Conn`, which is a `net.Conn`.

The signaling server sees only who connects to whom and their addresses. Your
bytes go peer to peer, or through a relay that sees only ciphertext.

## The pieces

| Piece | Needed when | In this repo |
| --- | --- | --- |
| Signaling | Always | `signaling/sse` (HTTP), `signaling/memory` (one process) |
| STUN | Peers are on different networks | `relay` answers STUN, or use a public server |
| TURN | Direct paths fail (symmetric NAT, UDP blocked) | `relay`, or coturn |

## Candidate types

`conn.Stats().LocalCandidate` and `RemoteCandidate` tell you which path was
used.

| Type | Meaning | Needs |
| --- | --- | --- |
| `host` | Local interface address | Nothing |
| `srflx` | Public address seen by STUN | STUN |
| `prflx` | Learned during connectivity checks | Nothing |
| `relay` | Address on a TURN server | TURN and credentials |

| Peers | STUN | TURN |
| --- | --- | --- |
| Same LAN | no | no |
| Home networks | yes | fallback for the 10 to 20% that fail |
| Must always work | yes | yes |

## What a `Conn` is

- A reliable, ordered **byte stream**. `Read` may return part of a `Write`.
  Frame your messages.
- Deadlines, `io.Copy`, `bufio`, `encoding/json`, `crypto/tls`, and
  `net/http` all work on it.
- **No half-close.** Closing either side ends both directions.
- `Close` delivers everything already written, then the peer reads `io.EOF`.
- Connectivity loss triggers an ICE restart on the same `Conn`. If recovery
  fails, reads and writes return `pipe.ErrDisconnected`. Pipe never reconnects
  silently under a stream.

## Signaling is an interface

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

Use `signaling/sse` between machines, `signaling/memory` inside one process, or
write your own ([03-signaling-server.md](03-signaling-server.md)).

## Who pays for what

| Component | Traffic |
| --- | --- |
| Signaling | A few KiB per connection |
| STUN | A few packets per connection |
| TURN | Every relayed byte, in and out |

Next: [02-quickstart.md](02-quickstart.md).
