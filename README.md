# pipe

Reliable WebRTC DataChannels behind ordinary Go networking APIs.

`pipe` gives you `net.Conn` and `net.Listener` over WebRTC. SDP, ICE, DTLS,
SCTP, and Pion's callback API stay inside the library; your code dials a peer by
name and reads and writes bytes.

```go
hub := memory.New() // any pipe.Signaler works; memory keeps this to one process

server, err := pipe.New(ctx, pipe.Config{ID: "bob", Signaler: hub})
if err != nil {
	return err
}
defer server.Close()

ln, err := server.Listen()
if err != nil {
	return err
}
defer ln.Close()

go func() {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go io.Copy(conn, conn) // an echo server, unchanged from TCP
	}
}()

client, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: hub})
if err != nil {
	return err
}
defer client.Close()

conn, err := client.Dial(ctx, "bob")
if err != nil {
	return err
}
defer conn.Close()
```

A runnable version is in [`examples/echo`](examples/echo):

```sh
go run ./examples/echo
```

[`examples/`](examples) also has a STUN + TURN server with a configurable
throughput budget and a client that proves it went through the relay:

```sh
go run ./examples/turnserver -rate 512KiB -stats 2s        # admin/admin on :3478
go run ./examples/turnclient -rate 512KiB -bytes 1MiB      # self-contained
```

## What you get

A `pipe.Conn` is a real `net.Conn`: deadlines work, `Close` is idempotent, one
reader and one writer may run concurrently, and errors are `*net.OpError` with
network name `webrtc`. That is verified rather than asserted — [`test/compat`](test/compat)
drives `io.Copy`, `bufio`, `encoding/json`, `encoding/gob`, `tls.Conn` (TLS 1.3
handshake and transfer), and `net/http` (with keep-alive) over live connections.

## Signaling

Two peers cannot find each other without a third party to carry offers, answers,
and candidates. `pipe` does not ship a mandatory signaling service; you provide
a `Signaler`:

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

The rules a transport must honor: one `Receive` caller at a time, `Send` safe
concurrently with `Receive`, both honoring their context, `Close` idempotent and
unblocking both. Delivery is **at-least-once** — duplicates and reordering are
expected and tolerated.

Writing a transport? Run the conformance suite against it:

```go
func TestConformance(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		NewSignaler:            func(t *testing.T) pipe.Signaler { return myTransport(t) },
		RejectsDuplicatePeers:  true,
		ReportsUnavailablePeer: true,
	})
}
```

[`signaling/memory`](signaling/memory) is an in-process hub for tests and
same-process examples. It also injects duplicate, drop, reorder, and disconnect
faults deterministically.

Signaling is not STUN and not TURN. STUN and TURN servers are configured through
`Config.ICEServers` and are used by ICE to find a network path; signaling is how
the two peers exchange the descriptions in the first place.

## Configuration

| Field | Default | Meaning |
| --- | --- | --- |
| `ID` | required | This endpoint's peer ID (a routing name). |
| `Signaler` | required | Transport for signaling envelopes. |
| `ICEServers` | none | STUN/TURN servers passed to ICE. |
| `ICETransportPolicy` | all | Set to relay-only to force TURN. |
| `DialTimeout` | 30s | Bound on a whole dial. |
| `ICETimeout` | 20s | Bound on connectivity establishment. |
| `KeepAlive` | off | Protocol ping/pong probes; set `Interval` to enable. |
| `Reconnect` | 3 attempts | ICE-restart recovery budget and backoff. |
| `AcceptBacklog` | 64 | Pending inbound connections before rejection. |
| `FramePayload` | 16 KiB | Bytes per DataChannel message. |
| `ReadBuffer` | 1 MiB | Unread-byte budget per connection. |
| `Logger`, `Metrics` | nop | `*slog.Logger` and a metrics sink. |
| `Pion` | none | Escape hatch for the underlying Pion configuration. |

TURN credentials come from your configuration or environment. They are never
logged, and neither are SDP, ICE credentials, or application payloads.

## Limitations

Read these before deploying.

- **A peer ID is a routing identity, not an authenticated one.** Authentication
  belongs to your signaling transport. DTLS fingerprint verification guarantees
  that only the negotiated party can send you bytes; it does not tell you who that
  party is. For cryptographic peer identity, run mutual TLS over the pipe.
- **No half-close.** There is no `CloseWrite`. Protocols that use FIN as an
  end-of-message marker need their own framing.
- **One reliable, ordered stream per connection** in protocol version 1.
- **Recovery is bounded.** Signaling reconnects and ICE restarts are transparent
  and keep the same `Conn`. Anything that would require replacing the
  `PeerConnection` closes the connection with `ErrDisconnected` instead of
  silently losing, duplicating, or reordering bytes.
- **No published scale numbers yet.** Capacity work is deliberately not claimed
  until it is measured.

## Testing

```sh
go test ./...              # no network, no Docker, no root required
go test -race ./...
go test ./... -short       # skips the large-transfer cases
```
