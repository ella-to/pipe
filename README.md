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

## Two machines

Peers on different machines need a signaling server to exchange offers,
answers, and candidates, and usually STUN or TURN to find a network path.
Everything required ships in this repository:

```sh
# Somewhere both machines can reach: the HTTP signaling server.
export PIPE_SIGNAL_TOKENS="alice=$(openssl rand -hex 24),bob=$(openssl rand -hex 24)"
go run ./examples/signaling -listen :8080

# Machine A
go run ./examples/pipecat -signal http://signal.example:8080/pipe -token "$BOB_TOKEN" -id bob listen

# Machine B
echo hello | go run ./examples/pipecat -signal http://signal.example:8080/pipe -token "$ALICE_TOKEN" -id alice dial bob
```

In Go, the only difference from the example above is the signaler:

```go
signaler := &sse.Client{URL: "https://signal.example.net/pipe", Token: os.Getenv("PIPE_SIGNAL_TOKEN")}
ep, err := pipe.New(ctx, pipe.Config{
	ID:       "alice",
	Signaler: signaler,
	ICEServers: []pipe.ICEServer{
		{URLs: []string{"stun:stun.example.net:3478"}},
		{URLs: []string{"turn:relay.example.net:3478?transport=udp"}, Username: user, Credential: pass},
	},
})
```

The [guides](guides/README.md) walk through all of it: concepts, a quickstart,
the signaling server, STUN, running and securing your own TURN relay, giving
free and paying users different relay budgets, Docker, the Go API, operations,
and the wire protocol.

## What you get

A `pipe.Conn` is a real `net.Conn`: deadlines work, `Close` is idempotent, one
reader and one writer may run concurrently, and errors are `*net.OpError` with
network name `webrtc`. That is verified rather than asserted — [`test/compat`](test/compat)
drives `io.Copy`, `bufio`, `encoding/json`, `encoding/gob`, `tls.Conn` (TLS 1.3
handshake and transfer), and `net/http` (with keep-alive) over live connections.

`Close` returns at once and still delivers what you wrote: the stream waits in
the background for the peer to acknowledge written data before the transport
is torn down, so the peer reads everything and then `io.EOF`.

`Endpoint.Listen` returns a `*pipe.Listener`. It satisfies `net.Listener`, and
its `AcceptConn` method returns the concrete `*pipe.Conn` when you want
`Stats`, `State`, or `PeerID` without a type assertion.

## Signaling

Two peers cannot find each other without a third party to carry offers, answers,
and candidates. `pipe` defines the envelope and leaves the transport to you:

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

Two transports are included:

- [`signaling/sse`](signaling/sse): HTTP. Peers receive over a Server-Sent
  Events stream and send with POST; the server authenticates every request and
  refuses signals whose sender does not match the credential, so peer IDs
  delivered through it are authenticated. The event stream is produced and
  consumed with [`ella.to/sse`](https://pkg.go.dev/ella.to/sse). Run it with
  [`examples/signaling`](examples/signaling) or mount `sse.Server` in your own
  `http.ServeMux`.
- [`signaling/memory`](signaling/memory): an in-process hub for tests and
  same-process examples, with deterministic duplicate, drop, reorder, and
  disconnect fault injection.

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

Signaling is not STUN and not TURN. STUN and TURN servers are configured through
`Config.ICEServers` and are used by ICE to find a network path; signaling is how
the two peers exchange the descriptions in the first place.

## STUN and TURN

[`examples/turnserver`](examples/turnserver) is a STUN and TURN server built
on `pion/turn` that you can run as-is for a personal deployment. It supports
static users and ephemeral credentials from a shared secret, a relay port range
for firewalls and Docker, and **per-user plans**, so a free tier can be capped
at 512 KiB/s per relay socket while paying users get more:

```sh
go run ./examples/turnserver -plans 'free=512KiB/4,paid=8MiB/32' \
    -users 'alice=alice-secret:free,bob=bob-secret:paid' -stats 10s
```

[`examples/turnclient`](examples/turnclient) proves a connection went through
the relay and measures it; [`examples/turncred`](examples/turncred) mints
ephemeral credentials. [`examples/docker`](examples/docker) has a Dockerfile,
a compose stack, and a coturn configuration.

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
| `AllowPeer` | allow all | Refuse inbound offers from peers this returns false for. |
| `FramePayload` | 16 KiB | Bytes per DataChannel message. |
| `ReadBuffer` | 1 MiB | Unread-byte budget per connection. |
| `Logger`, `Metrics` | nop | `*slog.Logger` and a metrics sink. |
| `Pion` | none | Escape hatch for the underlying Pion configuration. |

TURN credentials come from your configuration or environment. They are never
logged, and neither are SDP, ICE credentials, or application payloads.

## Limitations

Read these before deploying.

- **A peer ID is only as authenticated as your signaling transport.** With
  `signaling/sse` and tokens, it is. With a transport that trusts client-supplied
  names, it is not. DTLS guarantees that only the negotiated party can send you
  bytes; for cryptographic peer identity independent of signaling, run mutual
  TLS over the pipe (see [guides/06-security.md](guides/06-security.md)).
- **No half-close.** There is no `CloseWrite`. Protocols that use FIN as an
  end-of-message marker need their own framing.
- **One reliable, ordered stream per connection** in protocol version 1.
- **Recovery is bounded.** Signaling reconnects and ICE restarts are transparent
  and keep the same `Conn`. Anything that would require replacing the
  `PeerConnection` closes the connection with `ErrDisconnected` instead of
  silently losing, duplicating, or reordering bytes.
- **No published scale numbers yet.** Capacity work is deliberately not claimed
  until it is measured; [guides/10-operations.md](guides/10-operations.md) says
  how to measure your own.

## Testing

```sh
go test ./...              # no network, no Docker, no root required
go test -race ./...
go test ./... -short       # skips the large-transfer cases
go test -bench . ./internal/frame   # stream throughput and allocation benchmark
```
