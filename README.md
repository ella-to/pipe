# Pipe

```
██████╗░██╗██████╗░███████╗
██╔══██╗██║██╔══██╗██╔════╝
██████╔╝██║██████╔╝█████╗░░
██╔═══╝░██║██╔═══╝░██╔══╝░░
██║░░░░░██║██║░░░░░███████╗
╚═╝░░░░░╚═╝╚═╝░░░░░╚══════╝
```

<div align="center">

[![Go Reference](https://pkg.go.dev/badge/ella.to/pipe.svg)](https://pkg.go.dev/ella.to/pipe)
[![Go Report Card](https://goreportcard.com/badge/ella.to/pipe)](https://goreportcard.com/report/ella.to/pipe)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)


**Pipe** is a Go library for establishing peer-to-peer WebRTC data channels with a simple `io.ReadWriteCloser` interface. It abstracts away the complexity of WebRTC signaling, ICE negotiation, and data channel management, letting you focus on building real-time applications.

</div>

## Features

- **Simple API**: Dial and listen like standard Go networking
- **`io.ReadWriteCloser` interface**: Use familiar patterns for reading/writing data
- **`Peer` overlay**: String-addressed, many-to-many datagram messaging (`net.PacketConn`-style `WriteTo`/`ReadFrom`) with on-demand dialing, idle eviction, and automatic reconnect
- **Pluggable signaling**: Bring your own signaling mechanism (SSE, WebSocket, etc.)
- **Built-in STUN/TURN servers**: Optional packages for self-hosted ICE infrastructure
- **Dynamic credentials**: Time-limited TURN authentication (REST API style)
- **NAT traversal**: Configurable ICE servers and NAT 1:1 IP mapping

## Installation

```bash
go get ella.to/pipe@v0.1.6
```

## Quick Start

### Basic Usage

```go
package main

import (
    "context"
    "fmt"
    "io"
    "log"

    "ella.to/pipe"
    "ella.to/pipe/signal/sse"
)

func main() {
    // Set up signaling (using SSE in this example)
    sig, err := sse.NewClient("https://your-signal-server.com")
    if err != nil {
        log.Fatal(err)
    }

    ctx := context.Background()

    // Peer A: Listener
    go func() {
        listener, _ := pipe.CreateListener("alice", sig)
        conn, _ := listener.Listen(ctx)
        defer conn.Close()

        conn.WaitReady(ctx)
        
        buf := make([]byte, 1024)
        n, _ := conn.Read(buf)
        fmt.Printf("Received: %s\n", buf[:n])
    }()

    // Peer B: Dialer
    dialer, _ := pipe.CreateDialer("bob", sig)
    conn, _ := dialer.Dial(ctx, "alice")
    defer conn.Close()

    conn.WaitReady(ctx)
    conn.Write([]byte("Hello, Alice!"))
}
```

### Connection Lifecycle

```go
// Create a connection
conn, err := dialer.Dial(ctx, "peer-id")
if err != nil {
    return err
}
defer conn.Close()

// Wait until the data channel is ready
if err := conn.WaitReady(ctx); err != nil {
    return err
}

// Check connection state
if conn.IsReady() {
    // Connection is established
}

if conn.IsClosed() {
    // Connection has been closed
}

// Block until connection closes
conn.WaitClosed(ctx)
```

### Large Data Transfers with Sync

When transferring large amounts of data, use `Sync()` to wait for the local buffer to drain before proceeding:

```go
// Transfer a large file
file, _ := os.Open("large-file.bin")
defer file.Close()

n, err := io.Copy(conn, file)
if err != nil {
    return err
}

// Wait for all buffered data to be sent to the network
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := conn.Sync(ctx); err != nil {
    return fmt.Errorf("sync failed: %w", err)
}

// Now safe to close or proceed - data has left the local buffer
fmt.Printf("Sent %d bytes\n", n)
```

> **Note**: `Sync()` only guarantees data left the local buffer, not that the remote peer received it. For end-to-end confirmation, implement application-level acknowledgment.

### Stream Multiplexing

Pipe supports multiplexing multiple independent streams over a single connection using the `Stream()` method. This is useful when you need multiple logical channels (e.g., control channel + data channel) without establishing separate WebRTC connections.

```go
// Dialer side
dialer, _ := pipe.CreateDialer("alice", sig)
conn, _ := dialer.Dial(ctx, "bob")
defer conn.Close()

// Open multiple streams
controlStream, _, _ := conn.Stream()  // First stream for control messages
dataStream, _, _ := conn.Stream()     // Second stream for data transfer

go handleControl(controlStream)
transferData(dataStream)
```

```go
// Listener side  
listener, _ := pipe.CreateListener("bob", sig)
conn, _ := listener.Listen(ctx)
defer conn.Close()

// Accept streams (matched in order with dialer's opens)
controlStream, _, _ := conn.Stream()  // Accepts first stream
dataStream, _, _ := conn.Stream()     // Accepts second stream

go handleControl(controlStream)
receiveData(dataStream)
```

**Key points:**
- Streams are independent `io.ReadWriteCloser` instances
- Dialer calls `Stream()` to open streams; Listener calls `Stream()` to accept them
- Use `Stream([]byte("token"))` to attach optional metadata when opening a stream
- Once `Stream()` is called, direct `Read()`/`Write()` on `Conn` returns `ErrMultiplexingActive`
- Use `conn.NumStreams()` to check the number of active streams
- Powered by [smux](https://github.com/xtaci/smux) for efficient multiplexing

### Stream Initial Value (Optional Metadata)

Use `Stream` to attach optional metadata (for example, a token or stream type) when opening a stream. The receiving side gets it from the second return value.

```go
// Dialer side: open stream with metadata
stream, _, _ := conn.Stream([]byte("token:abc123"))
defer stream.Close()

// Listener side: accept stream and read metadata
stream, tokenBytes, _ := conn.Stream()
defer stream.Close()

token := string(tokenBytes)
```

### Connection Status Monitoring

```go
import "github.com/pion/webrtc/v4"

pipe.SetStatusFunc(func(c *pipe.Conn, status webrtc.PeerConnectionState) {
    log.Printf("Connection %s: %s", c.ID(), status)
})
```

## Peer: Many-to-Many Datagram Messaging

While `Conn` is a single point-to-point connection you dial or accept explicitly,
a `Peer` is a higher-level overlay that lets one node talk to **many** other nodes
by id, without managing each connection by hand. It behaves like a
[`net.PacketConn`](https://pkg.go.dev/net#PacketConn) keyed by a string peer id
instead of a `net.Addr`:

```go
WriteTo(peerID string, b []byte) (int, error)        // send a datagram to peerID
ReadFrom(b []byte) (n int, from string, err error)   // receive the next datagram + sender
Close() error                                         // detach and tear everything down
```

A `Peer` is simultaneously a dialer and a listener under the hood. Each remote
peer is reached over a single bidirectional WebRTC connection that is **dialed on
demand** the first time you `WriteTo` it, kept warm while in use, **evicted after
30s idle**, and **transparently re-established** on the next `WriteTo`.

### Quick Example

```go
package main

import (
    "fmt"
    "log"
    "net/http/httptest"

    "ella.to/pipe"
    "ella.to/pipe/signal/sse"
)

func main() {
    // 1. A signaling server that enforces unique peer ids (HTTP 409 on duplicates).
    signalServer, err := pipe.NewPeerSignalServer()
    if err != nil {
        log.Fatal(err)
    }
    ts := httptest.NewServer(signalServer) // in production, mount on your own http.Server
    defer ts.Close()

    // 2. Each peer connects through its own signaling client.
    aliceSig, _ := sse.NewClient(ts.URL)
    bobSig, _ := sse.NewClient(ts.URL)

    alice, err := pipe.NewPeer("alice", aliceSig)
    if err != nil {
        log.Fatal(err)
    }
    defer alice.Close()

    bob, err := pipe.NewPeer("bob", bobSig)
    if err != nil {
        log.Fatal(err)
    }
    defer bob.Close()

    // 3. alice sends to bob — the WebRTC connection is dialed automatically.
    if _, err := alice.WriteTo("bob", []byte("hello bob")); err != nil {
        log.Fatal(err)
    }

    // 4. bob receives the datagram and learns who sent it.
    buf := make([]byte, 64*1024)
    n, from, err := bob.ReadFrom(buf)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("bob received %q from %q\n", buf[:n], from)
    // bob received "hello bob" from "alice"

    // 5. bob replies over the same bidirectional connection.
    bob.WriteTo("alice", []byte("hi alice"))
    n, from, _ = alice.ReadFrom(buf)
    fmt.Printf("alice received %q from %q\n", buf[:n], from)
    // alice received "hi alice" from "bob"
}
```

### Echo Server: Reply to Whoever Sent the Datagram

Because `ReadFrom` returns the sender's id, building a server that responds to
many clients is just a loop:

```go
peer, _ := pipe.NewPeer("echo-server", sig)
defer peer.Close()

buf := make([]byte, 64*1024)
for {
    n, from, err := peer.ReadFrom(buf)
    if err != nil {
        if errors.Is(err, io.ErrClosedPipe) {
            return // peer was closed
        }
        log.Printf("read error: %v", err)
        continue
    }

    // Echo the datagram straight back to its sender.
    if _, err := peer.WriteTo(from, buf[:n]); err != nil {
        log.Printf("write to %s: %v", from, err)
    }
}
```

### Configuration

```go
peer, err := pipe.NewPeer("alice", sig,
    pipe.WithPeerIdleTimeout(60*time.Second),    // evict sessions idle this long (default 30s)
    pipe.WithPeerDialTimeout(15*time.Second),    // bound each on-demand dial (default 30s)
    pipe.WithPeerRecvBuffer(1024),               // inbound datagram queue depth (default 256)
    pipe.WithPeerWebRTCOptions(&pipe.WebRTCOptions{ // custom ICE servers, etc.
        ICEServers: myICEServers,
    }),
)
```

### Testing with In-Memory Signaling

For tests, swap the SSE client for the in-memory bus — no HTTP server required:

```go
import "ella.to/pipe/signal/testsignal"

bus := testsignal.New()
alice, _ := pipe.NewPeer("alice", bus)
bob, _ := pipe.NewPeer("bob", bus)
```

### Observability

```go
peer.ID()              // this peer's own id
peer.ConnectedPeers()  // ids of peers with a currently live session, e.g. ["bob", "carol"]
```

### Behavior & Semantics

- **On-demand dialing**: `WriteTo` reuses a live connection if present, otherwise
  dials the peer through the signal channel. Concurrent writes to the same new
  peer share a single dial.
- **Automatic reconnect**: if a cached session is dead (e.g. it was evicted, or the
  remote went away), `WriteTo` evicts it and re-dials within the **same** call.
- **Idle eviction**: a session with no successful read or write for the idle
  timeout (default 30s) is closed and dropped; the next `WriteTo` re-establishes it.
- **Datagram semantics**: message boundaries are preserved (length-framed). If your
  `ReadFrom` buffer is smaller than the datagram, the excess is **discarded**, just
  like `net.PacketConn`. Max datagram size is **1 MiB** (`ErrDatagramTooLarge`).
- **Sender attribution**: the initiator announces its id in a one-frame handshake,
  so `ReadFrom` always reports a correct `from`.
- **Close**: tears down every session and detaches from the signal server (closing
  the long-lived SSE subscription, which frees the id). After `Close`, `WriteTo`
  and `ReadFrom` return `io.ErrClosedPipe`. `Close` is idempotent.
- **Unique ids**: `PeerSignalServer` rejects a second peer claiming an id that is
  already registered with **HTTP 409 Conflict**, until the holder disconnects.

> **Mesh topology:**
>
> ```
>           ┌─────────┐
>           │  alice  │
>           └────┬────┘
>        WriteTo │ "bob"      WriteTo "carol"
>          ┌─────┴─────┐──────────────┐
>          ▼           ▼              ▼
>     ┌─────────┐ ┌─────────┐    ┌─────────┐
>     │   bob   │ │  carol  │ …  │   dave  │
>     └─────────┘ └─────────┘    └─────────┘
>   each edge = one on-demand WebRTC connection, evicted when idle
> ```

### Hosting the Peer Signal Server

`PeerSignalServer` is a drop-in `http.Handler` (it wraps the SSE server and adds
id-uniqueness enforcement):

```go
signalServer, err := pipe.NewPeerSignalServer()
if err != nil {
    log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/signal", signalServer)
log.Fatal(http.ListenAndServe(":8080", mux))
```

Peers then point their SSE client at it: `sse.NewClient("http://your-host:8080/signal")`.

## Signaling

Pipe requires a signaling mechanism to exchange WebRTC offers, answers, and ICE candidates between peers. The library provides a pluggable interface:

```go
type Signal interface {
    Sender
    Receiver(inbox string) (Receiver, error)
}

type Sender interface {
    Send(ctx context.Context, inbox string, msg *Msg) error
}

type Receiver interface {
    Receive(ctx context.Context) (*Msg, error)
}
```

### Built-in SSE Signaling

```go
import "ella.to/pipe/signal/sse"

// Server-side
server := sse.NewServer()
http.Handle("/signal", server)

// Client-side
client, err := sse.NewClient("http://localhost:8080/signal")
```

### In-Memory Signaling (for testing)

```go
import "ella.to/pipe/signal/testsignal"

sig := testsignal.New()

// Both peers use the same signal
listener, _ := pipe.CreateListener("A", sig)
dialer, _ := pipe.CreateDialer("B", sig)
```

## Custom ICE Configuration

### Adding TURN Relays (recommended)

The default configuration ships with Google's public STUN servers, which are
enough for many NAT scenarios but cannot relay traffic when both peers are
behind restrictive NATs/firewalls. In that case you need a TURN server.

`WithPeerTURN` / `TURNServers` are **additive**: they add relays on top of the
existing STUN servers, so you don't have to re-list the defaults.

```go
turnServer := pipe.NewTURNServer(
    "turn:turn.example.com:3478?transport=udp", // URL
    "user",                                      // username
    "pass",                                      // credential
)

// Peer API
p, err := pipe.NewPeer("my-id", sig, pipe.WithPeerTURN(turnServer))

// Dialer/Listener API
opts := &pipe.WebRTCOptions{
    TURNServers: []pipe.TURNServer{turnServer},
}
dialer, err := pipe.CreateDialerWithOptions("my-id", sig, opts)
listener, err := pipe.CreateListenerWithOptions("my-id", sig, opts)
```

If you run the bundled TURN server (see [Self-Hosted TURN Server](#self-hosted-turn-server)),
you can feed its `ICEServerFor(username)` straight into `ICEServers`.

### Replacing the Full ICE Server List

Set `ICEServers` to take complete control. When non-empty it **replaces** the
default STUN servers (any `TURNServers` are still appended on top).

```go
import "github.com/pion/webrtc/v4"

opts := &pipe.WebRTCOptions{
    ICEServers: []webrtc.ICEServer{
        // STUN server
        {
            URLs: []string{"stun:stun.example.com:3478"},
        },
        // TURN server with credentials
        {
            URLs:           []string{"turn:turn.example.com:3478?transport=udp"},
            Username:       "user",
            Credential:     "pass",
            CredentialType: webrtc.ICECredentialTypePassword,
        },
        // TURNS (TLS) server
        {
            URLs:           []string{"turns:turn.example.com:5349?transport=tcp"},
            Username:       "user",
            Credential:     "pass",
            CredentialType: webrtc.ICECredentialTypePassword,
        },
    },
}

dialer, err := pipe.CreateDialerWithOptions("my-id", sig, opts)
listener, err := pipe.CreateListenerWithOptions("my-id", sig, opts)
```

### Force Relay-Only (TURN)

Use relay-only mode to verify that your TURN server actually works: the
connection uses only relay candidates and **fails** rather than falling back to
a direct path.

```go
// Peer API
p, err := pipe.NewPeer("my-id", sig,
    pipe.WithPeerTURN(turnServer),
    pipe.WithPeerForceRelay(),
)

// Dialer/Listener API
opts := &pipe.WebRTCOptions{
    TURNServers: []pipe.TURNServer{turnServer},
    ForceRelay:  true,
}
```

### Verifying the Connection Path

When a connection is established, pipe logs (via `slog`) how the data path was
formed and whether TURN was required:

```
INFO connection established id=my-id role=dialer path="relay (TURN)" required_turn=true
     protocol=udp local_candidate=relay  local_addr=203.0.113.10:54032
                   remote_candidate=srflx remote_addr=198.51.100.7:51820
```

Enable `slog` debug level to also see the ICE configuration, signaling
handshake, and ICE/connection state transitions — useful for diagnosing why a
connection never establishes:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})))
```

### NAT 1:1 IP Mapping

When running behind NAT with a known public IP:

```go
opts := &pipe.WebRTCOptions{
    NAT1To1IPs: []string{"203.0.113.10"}, // Your public IP
}
```

## Self-Hosted STUN Server

```go
import "ella.to/pipe/stun"

server := &stun.Server{}
err := server.Start(stun.Config{
    ListenAddr: ":3478",
    PublicAddr: "stun.example.com:3478",
})
if err != nil {
    log.Fatal(err)
}
defer server.Close()

// Get ICE server configuration for clients
iceServer := server.ICEServer()
// Returns: webrtc.ICEServer{URLs: []string{"stun:stun.example.com:3478"}}
```

## Self-Hosted TURN Server

### Basic TURN Server

```go
import "ella.to/pipe/turn"

server := &turn.Server{}
err := server.Start(turn.Config{
    ListenAddr: ":3478",        // UDP
    PublicIP:   "203.0.113.10", // Your server's public IP
    Realm:      "example.com",
    Users: []turn.User{
        {Username: "alice", Password: "secret123"},
        {Username: "bob", Password: "secret456"},
    },
})
if err != nil {
    log.Fatal(err)
}
defer server.Close()

// Get ICE server for a specific user
iceServer := server.ICEServerFor("alice")
```

### TURN with TCP and TLS

```go
server := &turn.Server{}
err := server.Start(turn.Config{
    ListenAddr:    ":3478",           // UDP
    TCPListenAddr: ":3478",           // TCP (same port is fine)
    TLSListenAddr: ":5349",           // TURNS (TLS)
    TLSCertFile:   "/path/cert.pem",
    TLSKeyFile:    "/path/key.pem",
    PublicIP:      "203.0.113.10",
    Realm:         "example.com",
    Users: []turn.User{
        {Username: "user", Password: "pass"},
    },
})
```

### Dynamic (REST) Credentials

Time-limited credentials using HMAC-SHA1 (compatible with standard TURN REST API):

```go
server := &turn.Server{}
err := server.Start(turn.Config{
    ListenAddr: ":3478",
    PublicIP:   "203.0.113.10",
    Realm:      "example.com",
    Dynamic: &turn.DynamicAuth{
        Secret: "your-shared-secret",
        MaxTTL: 24 * time.Hour,
    },
})

// Generate credentials for a client (valid for 1 hour)
username, credential := server.GenerateRESTCredentials(1 * time.Hour)

// Client uses these credentials
iceServer := webrtc.ICEServer{
    URLs:       []string{"turn:turn.example.com:3478"},
    Username:   username,   // Unix timestamp of expiry
    Credential: credential, // HMAC-SHA1(secret, username)
}
```

## Best Practices

### 1. Always Set Timeouts

```go
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

conn, err := dialer.Dial(ctx, "peer-id")
```

### 2. Handle Connection States

```go
pipe.SetStatusFunc(func(c *pipe.Conn, status webrtc.PeerConnectionState) {
    switch status {
    case webrtc.PeerConnectionStateConnected:
        log.Printf("Connected to %s", c.ID())
    case webrtc.PeerConnectionStateFailed:
        log.Printf("Connection to %s failed", c.ID())
    case webrtc.PeerConnectionStateDisconnected:
        log.Printf("Disconnected from %s", c.ID())
    }
})
```

### 3. Graceful Shutdown

```go
// Close connection gracefully
if err := conn.Close(); err != nil {
    log.Printf("Close error: %v", err)
}

// Or wait for remote close
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
conn.WaitClosed(ctx)
```

### 4. Use Multiple ICE Servers

```go
opts := &pipe.WebRTCOptions{
    ICEServers: []webrtc.ICEServer{
        // Primary TURN
        {URLs: []string{"turn:turn1.example.com:3478"}, Username: "u", Credential: "p"},
        // Backup TURN
        {URLs: []string{"turn:turn2.example.com:3478"}, Username: "u", Credential: "p"},
        // STUN fallback
        {URLs: []string{"stun:stun.l.google.com:19302"}},
    },
}
```

### 5. Production TURN Deployment

- **Use TLS**: Always enable TURNS for production
- **Rotate secrets**: Use dynamic credentials with short TTLs
- **Monitor**: Track allocations and bandwidth usage
- **Firewall**: Open UDP ports for relay range (default: 49152-65535)
- **Multiple regions**: Deploy TURN servers close to users

### 6. Error Handling

```go
conn, err := dialer.Dial(ctx, "peer-id")
if err != nil {
    if errors.Is(err, context.DeadlineExceeded) {
        // Connection timed out
    }
    return err
}

if err := conn.WaitReady(ctx); err != nil {
    // ICE negotiation failed
    conn.Close()
    return err
}
```

## Architecture

```
┌─────────────┐                              ┌─────────────┐
│   Peer A    │                              │   Peer B    │
│             │                              │             │
│  Dialer     │◄──────── Signaling ─────────►│  Listener   │
│             │       (SSE/WebSocket)        │             │
│             │                              │             │
│  WebRTC     │◄══════ Data Channel ════════►│  WebRTC     │
│  Connection │        (P2P / TURN)          │  Connection │
└─────────────┘                              └─────────────┘
       │                                            │
       │              ┌───────────┐                 │
       └──────────────│ STUN/TURN │─────────────────┘
                      │  Server   │
                      └───────────┘
```

## API Reference

### Core Types

| Type | Description |
|------|-------------|
| `Conn` | WebRTC data channel connection implementing `io.ReadWriteCloser` |
| `Dialer` | Creates outbound connections |
| `Listener` | Accepts inbound connections |
| `Peer` | String-addressed, many-to-many datagram overlay (`net.PacketConn`-style) |
| `PeerSignalServer` | `http.Handler` signaling server that enforces unique peer ids |
| `WebRTCOptions` | Configuration for ICE servers and NAT traversal |

### Functions

| Function | Description |
|----------|-------------|
| `CreateDialer(id, sig)` | Create a dialer with default options |
| `CreateDialerWithOptions(id, sig, opts)` | Create a dialer with custom ICE configuration |
| `CreateListener(id, sig)` | Create a listener with default options |
| `CreateListenerWithOptions(id, sig, opts)` | Create a listener with custom ICE configuration |
| `NewPeer(id, sig, opts...)` | Create a many-to-many datagram peer |
| `NewPeerSignalServer(opts...)` | Create a signaling `http.Handler` that enforces unique peer ids |
| `WithPeerIdleTimeout(d)` | Peer option: idle timeout before a session is evicted (default 30s) |
| `WithPeerDialTimeout(d)` | Peer option: bound on each on-demand dial (default 30s) |
| `WithPeerRecvBuffer(n)` | Peer option: inbound datagram queue depth (default 256) |
| `WithPeerWebRTCOptions(opts)` | Peer option: ICE/NAT configuration for dialing and listening |
| `SetStatusFunc(fn)` | Set global connection status callback |

### Conn Methods

| Method | Description |
|--------|-------------|
| `Read(p []byte)` | Read data from the connection |
| `Write(p []byte)` | Write data to the connection |
| `Close()` | Close the connection |
| `Sync(ctx)` | Wait for buffered data to be sent to the network |
| `ID()` | Get the connection identifier |
| `IsReady()` | Check if connection is established |
| `IsClosed()` | Check if connection is closed |
| `WaitReady(ctx)` | Block until connection is ready |
| `WaitClosed(ctx)` | Block until connection is closed |
| `Stream(initialValue...)` | Open/accept stream with optional initial metadata; returns received init bytes |

### Peer Methods

| Method | Description |
|--------|-------------|
| `WriteTo(peerID, b)` | Send a datagram to `peerID`, dialing/reconnecting on demand; returns `len(b)` |
| `ReadFrom(b)` | Receive the next datagram from any peer; returns `(n, from, err)` |
| `Close()` | Tear down all sessions and detach from the signal server (idempotent) |
| `ID()` | This peer's own id |
| `ConnectedPeers()` | Ids of peers with a currently live session |

After `Close`, `WriteTo` and `ReadFrom` return `io.ErrClosedPipe`. Other sentinel
errors: `ErrEmptyPeerID`, `ErrSelfPeer`, `ErrDatagramTooLarge`.

## License

[MIT License](LICENSE)

## Contributing

Contributions are welcome! Please open an issue or submit a pull request.
