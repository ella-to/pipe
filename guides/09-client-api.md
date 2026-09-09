# Using the Go API

This guide is for the developer writing the program on either end of a pipe
connection. It goes through the `ella.to/pipe` package field by field:
endpoints and their lifetime, every `Config` option and its default, dialing
and accepting, what `pipe.Conn` promises as a `net.Conn`, errors, statistics,
metrics, running HTTP and TLS over a connection, ICE server configuration, and
the escape hatch into Pion. It assumes you have a working signaling server
([03-signaling-server.md](03-signaling-server.md)).

## Endpoint lifecycle

An `Endpoint` is one identity on one signaling connection. It serves any number
of outbound and inbound connections at once.

```go
ep, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: signaler})
if err != nil {
	return err
}
defer ep.Close()
```

`New` validates the configuration, builds the WebRTC engine, opens the
signaling connection, and starts the receive loop. **The context governs
construction only.** Cancelling it after `New` returned does nothing; the
endpoint lives until `Close`. That is deliberate: an endpoint is usually
process-scoped, and tying it to a request context would be a trap.

`Close` closes every connection (telling peers why, over signaling), the
listener, and the signaling connection, then waits for everything to release.
It is idempotent and safe to call from several goroutines. It blocks for up to
a few hundred milliseconds when connections are open, because each connection
lets its peer acknowledge what was written before the transport goes away
(see [Close semantics](#close-semantics)).

`ep.LocalID()` returns the peer ID and `ep.Addr()` a `net.Addr` whose
`Network()` is `"webrtc"` and whose `String()` is the peer ID.

## Config

Only `ID` and `Signaler` are required. Everything else has a documented
default applied by `New`; a zero value means "use the default", and `New`
rejects values that make no sense with an error matching `pipe.ErrConfig`.
`Config` is copied by `New` and never read again.

| Field | Default | Meaning |
| --- | --- | --- |
| `ID` | required | This endpoint's peer ID: non-empty, at most 128 bytes, valid UTF-8, no control characters, no surrounding whitespace. |
| `Signaler` | required | Opens the signaling connection. |
| `ICEServers` | none | STUN and TURN servers. With none, only host candidates are gathered, which works on a LAN only. |
| `ICETransportPolicy` | `ICETransportPolicyAll` | `ICETransportPolicyRelay` gathers relay candidates only and requires a TURN server in `ICEServers`. |
| `DialTimeout` | 30s | Bound on one whole negotiation, from `Dial` to an open DataChannel. |
| `ICETimeout` | 20s | Bound on connectivity establishment; also the recovery budget when `Reconnect` is disabled. |
| `KeepAlive` | off | Protocol ping/pong probes. Set `Interval` to enable; `Timeout` defaults to `Interval` and may not exceed it. |
| `Reconnect` | enabled, 3 attempts, 10s each, backoff 500ms to 5s ×2 with 20% jitter | ICE-restart recovery of an established connection. A zero value selects that default; set `Enabled: false` to turn it off. |
| `AcceptBacklog` | 64 | Inbound sessions negotiating or waiting in `Accept`. Beyond it, offers are rejected with `RejectBusy`. |
| `AllowPeer` | nil (allow all) | `func(PeerID) bool` consulted for every inbound offer before any resources are spent. `false` rejects with `RejectUnauthorized`. |
| `FramePayload` | 16 KiB | Application bytes per DataChannel message. Hard ceiling `MaxFramePayload` = 256 KiB; must fit the peer's SCTP maximum message size. |
| `ReadBuffer` | 1 MiB | Received but unread bytes held per connection before the sender is held back. Must be at least `FramePayload`. |
| `Logger` | discard | `*slog.Logger`. Pipe never falls back to the global logger. |
| `Metrics` | discard | Counters, gauges, durations; see [Metrics](#metrics). |
| `Pion` | none | `ConfigureSettingEngine` and `ConfigureConfiguration` hooks; see [The Pion escape hatch](#the-pion-escape-hatch). |

A fully spelled out configuration:

```go
cfg := pipe.Config{
	ID:       "alice",
	Signaler: &sse.Client{URL: signalURL, Token: token},
	ICEServers: []pipe.ICEServer{
		{URLs: []string{"stun:relay.example.net:3478"}},
		{
			URLs:       []string{"turn:relay.example.net:3478?transport=udp"},
			Username:   os.Getenv("PIPE_TURN_USERNAME"),
			Credential: os.Getenv("PIPE_TURN_PASSWORD"),
		},
	},
	DialTimeout:   45 * time.Second,
	KeepAlive:     pipe.KeepAliveConfig{Interval: 15 * time.Second, Timeout: 5 * time.Second},
	AcceptBacklog: 128,
	AllowPeer:     func(peer pipe.PeerID) bool { return strings.HasPrefix(string(peer), "device-") },
	Logger:        slog.Default(),
}
```

`AllowPeer` runs on the signaling receive loop, so it must be quick and safe
for concurrent use. It is an access-control list on top of an authenticated
signaler, not a substitute for one: with a transport that lets clients pick
any name, the `PeerID` it sees proves nothing. The bundled HTTP transport
authenticates peer IDs; see [06-security.md](06-security.md).

## Dialing

```go
ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()

conn, err := ep.Dial(ctx, "bob")
```

`Dial` returns a `*pipe.Conn` only once the connection is fully negotiated and
ready for I/O; the first `Write` never waits for ICE. It is bounded by both
`ctx` and `Config.DialTimeout`, whichever ends first. Cancelling `ctx` tears
the pending session down and releases its resources before `Dial` returns; if
the connection happened to complete at that very moment, it is closed rather
than leaked.

An endpoint cannot dial itself, and the peer ID must be valid; both are
`ErrConfig`. Dialing a peer that is not connected to signaling fails with an
error wrapping `ErrPeerUnavailable` when the transport can tell (the HTTP
transport can), otherwise with a timeout.

Dials are independent: run as many concurrently as you like against the same
or different peers, each yielding its own `Conn`.

## Listening and accepting

```go
ln, err := ep.Listen()
if err != nil {
	return err
}
defer ln.Close()

for {
	conn, err := ln.AcceptConn()
	if err != nil {
		if errors.Is(err, net.ErrClosed) {
			return nil // listener or endpoint closed
		}
		return err
	}
	go handle(conn)
}
```

`Listen` returns a `*pipe.Listener`, which satisfies `net.Listener`. `Accept`
returns the connection as a `net.Conn`, for `http.Server.Serve` and friends;
`AcceptConn` returns the concrete `*pipe.Conn` so that you can call `PeerID`,
`Stats`, or `State` without a type assertion.

An endpoint has at most one listener. A second `Listen` returns
`ErrAlreadyListening`, and calling `Listen` again after closing the listener is
not supported either; create a new endpoint if you need that. Closing the
listener stops new sessions from being admitted (dialers get `RejectNotListening`)
and closes connections that were negotiated but never accepted; connections
already returned by `Accept` stay open until you or the endpoint close them.

Only fully negotiated connections appear in `Accept`. Offers that are still
negotiating occupy a backlog slot but never surface, so `Accept` never hands
you something that can fail to connect.

## The package-level wrappers

```go
conn, err := pipe.Dial(ctx, cfg, "bob")   // net.Conn
ln, err := pipe.Listen(ctx, cfg)          // net.Listener
```

Each creates a hidden endpoint, does one thing with it, and ties the endpoint's
lifetime to the returned object: closing the connection closes its endpoint,
and closing the listener closes its endpoint and therefore every connection
accepted from it. They exist for programs that make exactly one connection or
serve from exactly one listener and want three lines instead of eight.
Anything that makes several connections should use `New` once: one endpoint
shares one signaling connection across all of them.

## Conn as a net.Conn

`*pipe.Conn` is a `net.Conn` over one reliable, ordered DataChannel. What
that means in practice:

- **Byte stream, not messages.** A `Read` may return part of what one `Write`
  sent, or the tail of one and the head of the next. Frame your messages (see
  below).
- **Deadlines work.** `SetDeadline`, `SetReadDeadline`, `SetWriteDeadline`
  behave as on a TCP connection: an expired deadline fails the blocked call
  with an error whose `Timeout()` is true and which matches
  `os.ErrDeadlineExceeded` and `pipe.ErrTimeout`. A deadline set while a
  `Write` is blocked applies to it.
- **Concurrency.** One reader and one writer may run at the same time.
  Several writers are serialized, and the frames of one `Write` never
  interleave with another's, so `fmt.Fprintf(conn, …)` from several
  goroutines produces whole lines.
- **Backpressure is real.** `Write` blocks when the peer is not reading, once
  the peer's `ReadBuffer` and SCTP's send window are full. Nothing is queued
  without bound.
- **Errors are `*net.OpError`** with `Net: "webrtc"`, except `io.EOF`, which
  is returned bare so that `err == io.EOF` keeps working.
- **No half-close.** There is no `CloseWrite`. Closing either side ends both
  directions.

Because there is no half-close, a protocol that uses "end of stream" as "end
of message" needs its own framing. Two common shapes:

```go
// Length-prefixed messages.
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
		return nil, fmt.Errorf("message of %d bytes exceeds limit %d", n, limit)
	}
	msg := make([]byte, n)
	_, err := io.ReadFull(r, msg)
	return msg, err
}
```

```go
// Newline-delimited text.
r := bufio.NewReader(conn)
for {
	line, err := r.ReadString('\n')
	if err != nil {
		break // io.EOF when the peer closed
	}
	fmt.Fprintf(conn, "%s", strings.ToUpper(line))
}
```

`encoding/json.NewEncoder/NewDecoder` and `encoding/gob` are themselves
self-delimiting and work directly on the connection.

Beyond `net.Conn`, a `Conn` offers `PeerID()`, `SessionID()` (the identifier
that correlates the negotiation in both peers' logs), `State()`, and `Stats()`.
`LocalAddr()` and `RemoteAddr()` are logical: `peer#session`.

## Close semantics

`conn.Close()`:

1. Marks the connection closed. Blocked and future `Read` and `Write` calls
   fail with `net.ErrClosed`; received but unread bytes are discarded, as on a
   TCP socket.
2. Sends a close frame to the peer, after everything already written.
3. Returns. It does not block on the network.
4. In the background, keeps the transport alive until the peer has
   acknowledged every byte written (including the close frame) or has closed
   its own side, bounded by two seconds, then keeps it a few hundred
   milliseconds longer so that this side's own acknowledgements reach the
   peer. Then it releases the PeerConnection.

Two consequences worth knowing. **Data written before `Close` is delivered**:
`Write` returning only means SCTP accepted the bytes, and closing a
PeerConnection aborts SCTP, so without step 4 the tail of a large write could
vanish; the integration test `TestCloseAfterWriteDeliversEverything` writes
8 MiB and closes immediately to check this. And **the peer reads what you
wrote, then gets `io.EOF`**, not an error: the close frame travels in order
after the data. Pipe also announces the close through signaling, which lets the
peer report EOF even when the transport is torn down abruptly underneath.

A peer that vanished, as opposed to one that closed, shows up as
`ErrDisconnected` on `Read` or `Write` once recovery has given up.

## Errors

Every error from this package matches at least one sentinel through
`errors.Is`. Never compare error strings.

| Sentinel | When |
| --- | --- |
| `ErrClosed` (`= net.ErrClosed`) | Use of a closed endpoint, listener, or connection. Generic networking code that checks `net.ErrClosed` keeps working. |
| `ErrTimeout` | A deadline or configured timeout expired. Also satisfies `net.Error` with `Timeout() == true`, and read/write deadline errors match `os.ErrDeadlineExceeded` too. |
| `ErrSignaling` | The signaling transport failed: could not open, send, or receive. The transport's own error is wrapped, so `errors.Is(err, sse.ErrUnauthorized)` also works. |
| `ErrNegotiation` | Offer/answer, DataChannel setup, or detach failed. |
| `ErrICE` | Connectivity could not be established. |
| `ErrProtocol` | The peer violated the signaling or stream protocol: bad envelope, bad frame. |
| `ErrPeerRejected` | The peer refused the session. Use `errors.As` with `*RejectedError` for the code. |
| `ErrPeerUnavailable` | Signaling could not reach the peer. |
| `ErrDuplicatePeer` | The peer ID is already registered with the signaling transport. |
| `ErrDisconnected` | An established connection was lost and could not be recovered within `Reconnect`. |
| `ErrConfig` | Invalid `Config`, or invalid arguments to `Dial`. |
| `ErrAlreadyListening` | Second `Listen` on one endpoint. |
| `ErrNotImplemented` | Reserved; never returned on a supported path. |

Handling a refused dial:

```go
conn, err := ep.Dial(ctx, "bob")
var rejected *pipe.RejectedError
switch {
case err == nil:
	// connected
case errors.As(err, &rejected):
	switch rejected.Code {
	case pipe.RejectNotListening:
		// bob is up but not accepting; retry later or tell the user
	case pipe.RejectBusy:
		// bob's backlog is full; back off
	case pipe.RejectUnauthorized:
		// bob's AllowPeer said no; do not retry
	default:
		log.Printf("rejected by %s: %s (%s)", rejected.Peer, rejected.Code, rejected.Reason)
	}
case errors.Is(err, pipe.ErrPeerUnavailable):
	// bob is not connected to signaling
case errors.Is(err, pipe.ErrTimeout):
	// no path found within DialTimeout: STUN/TURN problem, see 02-quickstart.md
case errors.Is(err, pipe.ErrSignaling):
	// our own signaling connection is broken; rebuild the endpoint
}
```

Reject codes defined by protocol version 1: `RejectNotListening`,
`RejectBusy`, `RejectUnauthorized`, `RejectUnsupportedVersion`,
`RejectInvalidOffer`, `RejectInternal`.

Reading with a deadline:

```go
conn.SetReadDeadline(time.Now().Add(5 * time.Second))
n, err := conn.Read(buf)
var ne net.Error
if errors.As(err, &ne) && ne.Timeout() {
	// idle; decide whether to keep waiting
}
```

## Connection state and statistics

```go
st := conn.Stats()
fmt.Printf("%s via %s/%s, %d B in, %d B out, %d restarts, rtt %v\n",
	st.State, st.LocalCandidate, st.RemoteCandidate,
	st.BytesRead, st.BytesWritten, st.ICERestarts, st.KeepAliveRTT)
```

| Field | Meaning |
| --- | --- |
| `State` | `StateConnected`, `StateRecovering`, `StateClosing`, or `StateClosed` for a connection you hold. |
| `BytesRead`, `BytesWritten` | Application bytes returned by `Read` and accepted by `Write`. |
| `FramesRead`, `FramesWritten` | Stream frames, including ping, pong, and close. |
| `EstablishedAt`, `ConnectDuration` | When the connection became usable, and how long negotiation took. |
| `ICERestarts` | Completed recoveries on this connection. |
| `LocalCandidate`, `RemoteCandidate` | `host`, `srflx`, `prflx`, `relay`, or `CandidateUnknown` (empty) before a pair is selected. Both `relay` means the bytes go through TURN. |
| `KeepAliveRTT` | Round trip of the last successful probe; zero when keepalive is off. |

Snapshots are consistent per field, never torn, and cheap. `conn.State()`
alone is cheaper still.

## Keepalive and recovery

Without keepalive, a connection whose peer silently disappeared is noticed
only when ICE's own consent checks fail, which takes tens of seconds, or when
you write and the SCTP retransmission budget runs out. Turn keepalive on when
your protocol has idle periods:

```go
KeepAlive: pipe.KeepAliveConfig{
	Interval: 15 * time.Second, // one ping per interval when idle
	Timeout:  5 * time.Second,  // no pong within this => connectivity lost
},
```

Probes are protocol frames inside the stream, so they measure the full path
your bytes take. A failed probe moves the connection to `StateRecovering` and
starts the reconnect policy.

Recovery means **ICE restart**: the dialing side sends a fresh offer through
signaling, both sides gather candidates again, and the same `Conn` continues
with no bytes lost, duplicated, or reordered. The answering side waits for the
restart offer; it never initiates one. Tune the budget:

```go
Reconnect: pipe.ReconnectPolicy{
	Enabled:        true,
	MaxAttempts:    5,
	AttemptTimeout: 15 * time.Second,
	Backoff: pipe.Backoff{
		Initial: time.Second,
		Maximum: 10 * time.Second,
		Factor:  2,
		Jitter:  0.2,
	},
},
```

Each attempt waits `AttemptTimeout` plus the backoff for that attempt. When
the budget is spent, the connection closes with `ErrDisconnected`. Pipe never
replaces the PeerConnection under a live `Conn`; if you want "reconnect
forever", loop around `Dial` in your own code, where you also know what to
resend.

`Reconnect: pipe.ReconnectPolicy{Enabled: false}` turns recovery off; a lost
connection then fails after `ICETimeout`.

## Metrics

```go
type Metrics interface {
	Count(name string, delta int64, labels ...Label)
	Gauge(name string, value int64, labels ...Label)
	Duration(name string, d time.Duration, labels ...Label)
}
```

Implementations must be safe for concurrent use and must not block. Labels
have bounded cardinality; peer and session IDs are never label values. The
names emitted:

| Name | Type | Labels |
| --- | --- | --- |
| `pipe.dial.attempts` | counter | |
| `pipe.dial.results` | counter | `result`: `success`, `timeout`, `rejected`, `error` |
| `pipe.accept.attempts` | counter | |
| `pipe.accept.results` | counter | `result`: `success`, `not-listening`, `unauthorized`, `busy`, `error` |
| `pipe.sessions.active` | gauge | |
| `pipe.sessions.pending` | gauge | |
| `pipe.signal.sent` | counter | `kind`, `result`: `success`, `timeout`, `error` |
| `pipe.signal.received` | counter | `kind`, `result`: `accepted`, `duplicate`, `invalid`, `misrouted` |
| `pipe.connect.duration` | duration | `role`: `offerer`, `answerer` |
| `pipe.restart.attempts` | counter | |
| `pipe.restart.results` | counter | `result` |
| `pipe.restart.duration` | duration | |
| `pipe.stream.bytes.read` | counter | added when a connection closes |
| `pipe.stream.bytes.written` | counter | added when a connection closes |
| `pipe.keepalive.rtt` | duration | |
| `pipe.keepalive.failures` | counter | `reason` |
| `pipe.protocol.failures` | counter | `scope`: `signal`, `routing`, `candidate` |

An adapter to a Prometheus-style registry has this shape (replace the
`registry` type with your client library's):

```go
type registry interface {
	CounterAdd(name string, labels map[string]string, delta float64)
	GaugeSet(name string, labels map[string]string, value float64)
	HistogramObserve(name string, labels map[string]string, value float64)
}

type promMetrics struct{ reg registry }

func (m promMetrics) Count(name string, delta int64, labels ...pipe.Label) {
	m.reg.CounterAdd(promName(name), toMap(labels), float64(delta))
}

func (m promMetrics) Gauge(name string, value int64, labels ...pipe.Label) {
	m.reg.GaugeSet(promName(name), toMap(labels), float64(value))
}

func (m promMetrics) Duration(name string, d time.Duration, labels ...pipe.Label) {
	m.reg.HistogramObserve(promName(name)+"_seconds", toMap(labels), d.Seconds())
}

func promName(name string) string { return strings.ReplaceAll(name, ".", "_") }

func toMap(labels []pipe.Label) map[string]string {
	out := make(map[string]string, len(labels))
	for _, l := range labels {
		out[l.Key] = l.Value
	}
	return out
}
```

## Logging

`Config.Logger` is a `*slog.Logger`. Pipe attaches `local`, and per session
`session`, `peer`, and `role`. At `Info` you see connections established,
connectivity lost and restored, and ICE restarts; at `Warn`, signaling
failures and keepalive timeouts; at `Debug`, every dropped or rejected signal
and every teardown reason.

Never logged, at any level: TURN credentials, SDP bodies, ICE candidates and
their credentials, application payloads, and signaling tokens. You can ship
debug logs to a shared system without leaking anything that lets someone else
connect.

## HTTP over pipe

A `Listener` is a `net.Listener` and a `Conn` is a `net.Conn`, so `net/http`
needs no adapter on either side. Server:

```go
ln, err := ep.Listen()
if err != nil {
	return err
}
srv := &http.Server{
	Handler:           mux,
	ReadHeaderTimeout: 10 * time.Second,
}
go srv.Serve(ln)
```

Client, with keep-alive across requests over one pipe connection:

```go
transport := &http.Transport{
	DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		// The address is ignored; the peer ID is the destination.
		return ep.Dial(ctx, "server")
	},
	MaxIdleConns:    2,
	IdleConnTimeout: 30 * time.Second,
}
client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

resp, err := client.Get("http://server/api/status") // host is decorative
```

The host in the URL is only used for the `Host` header and connection pooling
key; pick one per peer so pooled connections are not reused across peers.
`test/compat/compat_test.go` runs exactly this, plus TLS, JSON, gob, and
`bufio`, against live connections.

## TLS and mutual TLS over pipe

DTLS already encrypts the connection end to end, but it tells you only that
the party you negotiated with is the party sending bytes, not *who* that is.
A peer ID is authenticated by the signaling transport, if at all. For
cryptographic identity independent of signaling, run TLS over the pipe with
client certificates:

```go
// Listener side.
serverTLS := tls.Server(conn, &tls.Config{
	Certificates: []tls.Certificate{serverCert},
	ClientAuth:   tls.RequireAndVerifyClientCert,
	ClientCAs:    deviceCAPool,
	MinVersion:   tls.VersionTLS13,
})
if err := serverTLS.HandshakeContext(ctx); err != nil {
	return err
}
who := serverTLS.ConnectionState().PeerCertificates[0].Subject.CommonName

// Dialer side.
clientTLS := tls.Client(conn, &tls.Config{
	Certificates: []tls.Certificate{deviceCert},
	RootCAs:      serverCAPool,
	ServerName:   "bob.devices.example",
	MinVersion:   tls.VersionTLS13,
})
if err := clientTLS.HandshakeContext(ctx); err != nil {
	return err
}
```

Both `*tls.Conn` values are `net.Conn`s again. The TLS 1.3 handshake costs one
round trip on top of the pipe connection.

## ICE servers

```go
ICEServers: []pipe.ICEServer{
	// STUN: no credentials. Setting any is a configuration error.
	{URLs: []string{"stun:relay.example.net:3478"}},

	// TURN: username and credential are both required.
	{
		URLs:       []string{"turn:relay.example.net:3478?transport=udp"},
		Username:   user,
		Credential: pass,
	},

	// TURN over TCP for networks that block UDP; same credentials.
	{
		URLs:       []string{"turn:relay.example.net:3478?transport=tcp"},
		Username:   user,
		Credential: pass,
	},
},
```

Accepted schemes are `stun`, `stuns`, `turn`, and `turns`. Several URLs in one
entry describe one logical server. `CredentialType` defaults to
`ICECredentialPassword`; `ICECredentialOAuth` is reserved and rejected.
`ICETransportPolicyRelay` is rejected unless at least one `turn` or `turns`
URL is present, because it would otherwise gather nothing.

Ephemeral TURN credentials (the recommended way to hand out relay access; see
[07-multi-user-relay.md](07-multi-user-relay.md)) are ordinary username and
credential values with an expiry baked in. Fetch them from your service before
`New`, and rebuild the endpoint before they expire if it lives longer than
they do; ICE uses them at connection time and at ICE restart.

## The Pion escape hatch

`Config.Pion` is the one place where Pion types appear. Pipe enables detached
DataChannels and blocking writes itself and, for relay-only endpoints, sets
the relay acceptance wait to zero; do not undo those.

```go
Pion: pipe.PionOptions{
	ConfigureSettingEngine: func(se *webrtc.SettingEngine) {
		// How fast a dead path is noticed: disconnected after 5s, failed
		// after 15s, consent keepalive every 2s.
		se.SetICETimeouts(5*time.Second, 15*time.Second, 2*time.Second)

		// A server with a known public address and 1:1 NAT can advertise
		// it as a host candidate, which saves the STUN round trip.
		se.SetNAT1To1IPs([]string{"203.0.113.10"}, webrtc.ICECandidateTypeHost)

		// UDP only; skip TCP candidate gathering.
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeUDP6})
	},
	ConfigureConfiguration: func(c *webrtc.Configuration) {
		// Applied to every PeerConnection after ICEServers and the transport
		// policy from Config. Rarely needed.
	},
},
```

`ConfigureSettingEngine` runs once per endpoint, before the shared API is
built. Other useful settings there: interface filters, a fixed UDP port range
(`SetEphemeralUDPPortRange`), or a shared UDP mux for many endpoints on one
port.

## Common mistakes

- **Passing a request-scoped context to `New`** and expecting the endpoint to
  die with it. It will not; call `Close`.
- **One endpoint per connection.** Every endpoint holds a signaling connection.
  Create one per process (or per identity) and `Dial` many times.
- **Reading until `Read` returns fewer bytes than asked.** That is not a
  message boundary. Frame your messages.
- **Treating `Write` returning as delivery.** It means accepted. Delivery is
  assured by a clean `Close`, or by an application-level acknowledgement.
- **Relay-only without a TURN server**, or STUN entries with credentials: both
  fail `New` with `ErrConfig`.
- **Setting a `Timeout` on the `http.Client` given to `sse.Client`.** It kills
  the event stream. Contexts bound requests.
- **Reusing a peer ID on two devices.** The newer signaling stream replaces
  the older, which fails permanently.
- **Expecting `AllowPeer` to authenticate.** It filters names; the signaling
  transport is what proves them.
- **Ignoring `ErrDisconnected`.** Pipe recovers what it can transparently.
  When it gives up, the connection is gone; reconnect at your level with
  whatever resend logic your protocol needs.

## Related

- Concepts behind everything here: [01-concepts.md](01-concepts.md)
- Signaling transports: [03-signaling-server.md](03-signaling-server.md)
- What to watch in production and how to tune: [10-operations.md](10-operations.md)
- The envelope and frame formats: [11-protocol.md](11-protocol.md)
