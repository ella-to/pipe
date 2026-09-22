# Client API

The runnable programs here use `signaling/memory` so they work in one process.
Swap in `&sse.Client{...}` to put the peers on different machines; nothing else
changes.

## Endpoint

One `Endpoint` per identity per process. It holds one signaling connection and
serves any number of connections in both directions.

```go
ep, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: signaler})
if err != nil {
	return err
}
defer ep.Close() // closes every connection, the listener, and signaling
```

- `ctx` bounds construction only. Cancelling it later does nothing; call `Close`.
- `pipe.New` copies `Config`. New settings (for example new TURN credentials)
  mean a new endpoint.

## Dial and Listen

```go
conn, err := ep.Dial(ctx, "bob") // *pipe.Conn, ready for I/O when it returns
```

```go
ln, err := ep.Listen() // *pipe.Listener, a net.Listener; one per endpoint
if err != nil {
	return err
}
defer ln.Close()
for {
	conn, err := ln.AcceptConn() // *pipe.Conn; Accept() returns net.Conn
	if err != nil {
		return err // net.ErrClosed after Close
	}
	go handle(conn)
}
```

- `Dial` is bounded by `ctx` and `Config.DialTimeout`, whichever ends first.
- Dials are independent: run many concurrently.
- `Accept` only returns fully negotiated connections.
- Closing the listener refuses new dialers with `not_listening` and leaves
  accepted connections open.

One-shot shortcuts that own a hidden endpoint:

```go
conn, err := pipe.Dial(ctx, cfg, "bob") // closing conn closes its endpoint
ln, err := pipe.Listen(ctx, cfg)        // closing ln closes its endpoint and its conns
```

## Config

Only `ID` and `Signaler` are required. Zero values select the defaults shown.

```go
cfg := pipe.Config{
	ID:       "alice",
	Signaler: &sse.Client{URL: "https://signal.example.net/pipe", Token: token},

	ICEServers: []pipe.ICEServer{
		{URLs: []string{"stun:relay.example.net:3478"}},
		{
			URLs:       []string{"turn:relay.example.net:3478?transport=udp", "turn:relay.example.net:3478?transport=tcp"},
			Username:   turnUser,
			Credential: turnPass,
		},
	},
	ICETransportPolicy: pipe.ICETransportPolicyAll, // or ICETransportPolicyRelay (requires TURN)

	DialTimeout: 30 * time.Second, // whole negotiation
	ICETimeout:  20 * time.Second, // connectivity; recovery budget when Reconnect is off

	KeepAlive: pipe.KeepAliveConfig{Interval: 15 * time.Second, Timeout: 5 * time.Second}, // off by default

	Reconnect: pipe.ReconnectPolicy{ // this is the default
		Enabled:        true,
		MaxAttempts:    3,
		AttemptTimeout: 10 * time.Second,
		Backoff:        pipe.Backoff{Initial: 500 * time.Millisecond, Maximum: 5 * time.Second, Factor: 2, Jitter: 0.2},
	},

	AcceptBacklog: 64,                                                  // inbound sessions pending
	AllowPeer:     func(peer pipe.PeerID) bool { return peer != "eve" }, // nil allows all

	FramePayload: 16 << 10, // per DataChannel message; same value on both ends
	ReadBuffer:   1 << 20,  // unread bytes per connection before backpressure

	Logger:  slog.Default(),
	Metrics: nil, // see Metrics below
}
```

Invalid values fail `pipe.New` with an error matching `pipe.ErrConfig`.

## Messages over a stream

A `Conn` is a byte stream with no half-close: frame your messages.
`encoding/json` and `encoding/gob` are self-delimiting and work directly.

```go
// jsonrpc/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

type request struct {
	Text string `json:"text"`
}

type response struct {
	Upper string `json:"upper"`
}

func main() {
	ctx := context.Background()
	hub := memory.New()

	server, err := pipe.New(ctx, pipe.Config{ID: "server", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	client, err := pipe.New(ctx, pipe.Config{ID: "client", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ln, err := server.Listen()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
				for {
					var req request
					if err := dec.Decode(&req); err != nil {
						return // io.EOF when the client closes
					}
					_ = enc.Encode(response{Upper: strings.ToUpper(req.Text)})
				}
			}()
		}
	}()

	conn, err := client.Dial(ctx, "server")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	for _, text := range []string{"hello", "pipe"} {
		if err := enc.Encode(request{Text: text}); err != nil {
			log.Fatal(err)
		}
		var resp response
		if err := dec.Decode(&resp); err != nil {
			log.Fatal(err)
		}
		fmt.Println(resp.Upper)
	}
}
```

Length-prefixed binary messages:

```go
func writeMessage(w io.Writer, msg []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

func readMessage(r io.Reader, limit uint32) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > limit {
		return nil, fmt.Errorf("message of %d bytes exceeds %d", n, limit)
	}
	msg := make([]byte, n)
	_, err := io.ReadFull(r, msg)
	return msg, err
}
```

`Conn` guarantees:

- One reader and one writer may run concurrently; concurrent writers are
  serialized and one `Write` is never interleaved with another.
- `Write` blocks when the peer is not reading (real backpressure).
- `Write` returning means accepted, not delivered. `Close` delivers everything
  written, then the peer reads `io.EOF`.
- Errors are `*net.OpError` with `Net: "webrtc"`; `io.EOF` is returned bare.

## HTTP over pipe

`*pipe.Listener` is a `net.Listener` and `*pipe.Conn` is a `net.Conn`, so
`net/http` needs no adapter.

```go
// httpover/main.go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

func main() {
	ctx := context.Background()
	hub := memory.New()

	server, err := pipe.New(ctx, pipe.Config{ID: "api", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()
	client, err := pipe.New(ctx, pipe.Config{ID: "app", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	ln, err := server.Listen()
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok, you are %s\n", r.RemoteAddr)
	})
	go func() {
		_ = (&http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	}()

	httpClient := &http.Client{
		Transport: &http.Transport{
			// The URL host is only a pool key; the peer ID is the destination.
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return client.Dial(ctx, "api")
			},
		},
		Timeout: 30 * time.Second,
	}

	for range 2 { // the second request reuses the pipe connection
		resp, err := httpClient.Get("http://api/status")
		if err != nil {
			log.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Print(string(body))
	}
}
```

`r.RemoteAddr` is `peer#session`, so handlers know which peer called.

## Errors

Every error matches a sentinel with `errors.Is`.

```go
func explain(err error) string {
	var rejected *pipe.RejectedError
	switch {
	case err == nil:
		return "connected"
	case errors.As(err, &rejected):
		switch rejected.Code {
		case pipe.RejectNotListening:
			return "peer is up but not listening; retry later"
		case pipe.RejectBusy:
			return "peer's backlog is full; back off"
		case pipe.RejectUnauthorized:
			return "peer's AllowPeer refused us; do not retry"
		default:
			return "rejected: " + string(rejected.Code)
		}
	case errors.Is(err, pipe.ErrPeerUnavailable):
		return "peer is not connected to signaling"
	case errors.Is(err, pipe.ErrTimeout):
		return "no path within DialTimeout: check STUN/TURN"
	case errors.Is(err, pipe.ErrSignaling):
		return "our signaling connection failed; rebuild the endpoint"
	case errors.Is(err, pipe.ErrDisconnected):
		return "connection lost and recovery gave up; redial"
	case errors.Is(err, pipe.ErrConfig):
		return "invalid config or arguments"
	default:
		return err.Error()
	}
}
```

| Sentinel | When |
| --- | --- |
| `ErrClosed` (`net.ErrClosed`) | Use after close |
| `ErrTimeout` | Deadline or timeout; also `os.ErrDeadlineExceeded` for I/O deadlines |
| `ErrSignaling` | Signaling transport failed; wraps the transport's error |
| `ErrNegotiation` | Offer/answer or DataChannel setup failed |
| `ErrICE` | No connectivity |
| `ErrProtocol` | Peer violated the protocol |
| `ErrPeerRejected` | Refused; `*RejectedError` has `Code`, `Reason`, `Peer` |
| `ErrPeerUnavailable` | Peer not reachable through signaling |
| `ErrDuplicatePeer` | Peer ID already registered |
| `ErrDisconnected` | Lost and not recovered |
| `ErrConfig` | Invalid `Config` or `Dial` argument |
| `ErrAlreadyListening` | Second `Listen` |

## Deadlines

```go
_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
n, err := conn.Read(buf)
if errors.Is(err, os.ErrDeadlineExceeded) {
	// idle
}
_ = conn.SetReadDeadline(time.Time{}) // clear
```

## Stats

```go
st := conn.Stats()
log.Printf("%s via %s/%s, in=%d out=%d, restarts=%d, connect=%v, rtt=%v",
	st.State, st.LocalCandidate, st.RemoteCandidate,
	st.BytesRead, st.BytesWritten, st.ICERestarts, st.ConnectDuration, st.KeepAliveRTT)
```

Also `conn.PeerID()`, `conn.SessionID()` (matches the peer's logs), and
`conn.State()`.

## Keepalive and recovery

```go
KeepAlive: pipe.KeepAliveConfig{Interval: 10 * time.Second, Timeout: 5 * time.Second},
Reconnect: pipe.ReconnectPolicy{
	Enabled:        true,
	MaxAttempts:    5,
	AttemptTimeout: 15 * time.Second,
	Backoff:        pipe.Backoff{Initial: time.Second, Maximum: 10 * time.Second, Factor: 2, Jitter: 0.2},
},
```

- A missed pong moves the connection to `StateRecovering` and starts an ICE
  restart. Without keepalive, a dead path can take tens of seconds to notice.
- Only the dialer restarts ICE; enable keepalive on the dialer.
- A restart keeps the same `Conn` with no lost or reordered bytes. When the
  budget runs out, I/O fails with `ErrDisconnected`; redial in your code.
- `Reconnect: pipe.ReconnectPolicy{Enabled: false}` closes after `ICETimeout`.

## Metrics

Implement three methods. They must be safe for concurrent use and must not
block.

```go
// metrics/main.go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

type memMetrics struct {
	mu     sync.Mutex
	counts map[string]int64
}

func key(name string, labels []pipe.Label) string {
	var b strings.Builder
	b.WriteString(name)
	for _, l := range labels {
		fmt.Fprintf(&b, " %s=%s", l.Key, l.Value)
	}
	return b.String()
}

func (m *memMetrics) Count(name string, delta int64, labels ...pipe.Label) {
	m.mu.Lock()
	m.counts[key(name, labels)] += delta
	m.mu.Unlock()
}

func (m *memMetrics) Gauge(name string, value int64, labels ...pipe.Label) {
	m.mu.Lock()
	m.counts[key(name, labels)] = value
	m.mu.Unlock()
}

func (m *memMetrics) Duration(name string, d time.Duration, labels ...pipe.Label) {
	log.Printf("%s %v", key(name, labels), d)
}

func main() {
	ctx := context.Background()
	hub := memory.New()
	metrics := &memMetrics{counts: map[string]int64{}}

	bob, err := pipe.New(ctx, pipe.Config{ID: "bob", Signaler: hub, Metrics: metrics})
	if err != nil {
		log.Fatal(err)
	}
	defer bob.Close()
	alice, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: hub, Metrics: metrics})
	if err != nil {
		log.Fatal(err)
	}
	defer alice.Close()

	ln, err := bob.Listen()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, conn)
		conn.Close()
	}()

	conn, err := alice.Dial(ctx, "bob")
	if err != nil {
		log.Fatal(err)
	}
	_, _ = conn.Write([]byte("hello"))
	conn.Close()

	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	for k, v := range metrics.counts {
		fmt.Println(k, v)
	}
}
```

Names, kinds, and labels are listed in
[10-operations.md](10-operations.md#metrics). Peer and session IDs are never
labels.

## Logging

`Config.Logger` is a `*slog.Logger`; nil discards. Lines carry `local`, and
per session `session`, `peer`, `role`. Info: connections, recovery. Warn:
signaling and keepalive failures. Debug: every dropped signal and teardown
reason. Credentials, SDP, candidates, and payloads are never logged.

## The Pion escape hatch

```go
Pion: pipe.PionOptions{
	ConfigureSettingEngine: func(se *webrtc.SettingEngine) {
		se.SetICETimeouts(5*time.Second, 15*time.Second, 2*time.Second) // disconnected, failed, keepalive
		se.SetSrflxAcceptanceMinWait(0)                                   // do not wait for a host pair
		se.SetNAT1To1IPs([]string{"203.0.113.10"}, webrtc.ICECandidateTypeHost)
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
		_ = se.SetEphemeralUDPPortRange(50000, 50100)
	},
	ConfigureConfiguration: func(c *webrtc.Configuration) {
		// applied to every PeerConnection after ICEServers and the policy
	},
},
```

Import `github.com/pion/webrtc/v4`. Pipe enables detached DataChannels and
blocking writes; do not undo those.

## Common mistakes

- One endpoint per connection. Create one per identity and `Dial` many times.
- Treating a short `Read` as a message boundary. Frame your messages.
- `Timeout` on the `http.Client` given to `sse.Client`. It kills the stream.
- The same peer ID on two devices. The newer one takes over.
- Relay-only without a TURN URL, or credentials on a STUN entry: `ErrConfig`.
- Expecting `AllowPeer` to authenticate. The signaler proves identity;
  `AllowPeer` filters it.
