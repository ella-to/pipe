# The signaling server

This guide is for whoever runs the piece that lets two pipe peers find each
other: what the bundled HTTP signaling server (`signaling/sse`) does, how to run
and embed it, how it authenticates peers, how to put it behind TLS and a
reverse proxy, and how to write a different signaling transport if HTTP is not
what you want. If you have not read [01-concepts.md](01-concepts.md), the short
version is: signaling carries offers, answers, and ICE candidates between two
named peers; it never carries your data.

## What the server carries, and what it never sees

Per connection between two peers, the signaling server moves:

| Kind | Count per connection | Size |
| --- | --- | --- |
| `offer` | 1 | a few KiB of SDP |
| `answer` | 1 | a few KiB of SDP |
| `candidate` | 2 to 10 each way | under 1 KiB each |
| `ice-complete` | 1 each way | empty |
| `restart` and its `answer` | per ICE restart | a few KiB |
| `reject` or `close` | 1 | under 1 KiB |

Everything is JSON in the `pipe.Signal` envelope
([11-protocol.md](11-protocol.md)), carried on a Server-Sent Events stream that
is written and parsed by [`ella.to/sse`](https://pkg.go.dev/ella.to/sse); this
package adds the routing, authentication, queues, and replay around it. The
server validates the envelope, checks
that `from` names the authenticated caller, and queues it for `to`. It does not
parse SDP or candidates. Application bytes flow peer to peer, or through a TURN
relay, encrypted with DTLS using key fingerprints that were carried in the SDP,
so a signaling server that behaves honestly cannot read the connection, and a
dishonest one can at most substitute itself for a peer, which is the same
trust you place in any introducer. Mutual TLS over the pipe
([09-client-api.md](09-client-api.md)) removes even that.

## Running the example server

```sh
export PIPE_SIGNAL_TOKENS="alice=$(openssl rand -hex 24),bob=$(openssl rand -hex 24)"
go run ./examples/signaling -listen :8080
# signaling: http://localhost:8080/pipe
# health:    http://localhost:8080/healthz
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to serve HTTP on. |
| `-path` | `/pipe` | Path the signaling handler is mounted at; clients use `scheme://host:port/pipe`. |
| `-tokens` | | Comma-separated `peer=token` list. `PIPE_SIGNAL_TOKENS` takes precedence when set. |
| `-insecure-trust-peer-header` | `false` | Believe `X-Pipe-Peer` without any credential. Local development only. |
| `-max-peers` | `0` | Bound on concurrently known peers; 0 is unlimited. |
| `-offline-grace` | `30s` | How long a disconnected peer keeps its queue before it is forgotten. |
| `-tls-cert` | | PEM certificate; with `-tls-key`, serve HTTPS directly. |
| `-tls-key` | | PEM private key. |
| `-v` | `false` | Debug logging. |

Rules the example enforces on tokens: at least 16 characters, unique across
peers, and no empty peer or token. It refuses to start without a token source
or the insecure flag.

`GET /healthz` answers `ok peers=N`, where N is the number of peers known to
the server (attached or inside their offline grace). Point a load balancer
health check or an uptime monitor at it.

The example's `http.Server` is set up the way this handler needs:

```go
httpServer := &http.Server{
	Addr:    ":8080",
	Handler: mux,
	// No WriteTimeout: it would cut every event stream. The handler sets a
	// per-write deadline itself.
	ReadHeaderTimeout: 10 * time.Second,
	IdleTimeout:       120 * time.Second,
}
```

## Embedding the server in your own program

`sse.Server` is an `http.Handler`. Mount it next to whatever else you serve:

```go
package main

import (
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	signal, err := sse.NewServer(sse.Config{
		Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
			os.Getenv("ALICE_TOKEN"): "alice",
			os.Getenv("BOB_TOKEN"):   "bob",
		}),
		MaxPeers: 500,
		Logger:   slog.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer signal.Close()

	mux := http.NewServeMux()
	mux.Handle("/pipe", signal)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("my app\n"))
	})

	srv := &http.Server{
		Addr:              ":8443",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout deliberately unset.
	}
	log.Fatal(srv.ListenAndServeTLS("cert.pem", "key.pem"))
}
```

`Server.Peers()` lists known peer IDs, sorted. `Server.Close()` detaches every
stream and makes the handler answer 503 afterwards; call it before
`http.Server.Shutdown` so that streaming handlers return promptly.

## Authentication

Every request is bound to a peer identity by the configured `Authenticator`:

```go
type Authenticator interface {
	Authenticate(r *http.Request) (pipe.PeerID, error)
}
```

The server then enforces two things on every request. If the client sent an
`X-Pipe-Peer` header (the bundled client always does), it must equal the
authenticated ID, or the request gets 403; that makes a client configured with
the wrong token fail at `Open` with a clear message instead of signaling under
a name it does not own. And a POSTed signal whose `from` is not the
authenticated ID gets 403. The consequence is that peer IDs delivered through
this transport are authenticated, which is what makes `pipe.Config.AllowPeer`
a real access-control list rather than a suggestion.

### `StaticTokens`

Bearer tokens, one per peer, compared in constant time:

```go
auth := sse.StaticTokens(map[string]pipe.PeerID{
	"7d3a…": "alice",
	"c91f…": "bob",
})
```

Right for a personal deployment with a handful of devices. Rotate a token by
restarting with a new map.

### `TrustPeerHeader`

```go
auth := sse.TrustPeerHeader()
```

Believes the `X-Pipe-Peer` header. Anyone who can reach the server can be
anyone. Use it on `127.0.0.1` while developing and nowhere else.

### Your own: cookies, JWTs, a database

Anything that maps a request to a peer ID is an `AuthenticatorFunc`. A
session-cookie example:

```go
package main

import (
	"errors"
	"net/http"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

type sessions interface {
	Lookup(sessionID string) (userID string, ok bool)
}

func cookieAuth(store sessions) sse.Authenticator {
	return sse.AuthenticatorFunc(func(r *http.Request) (pipe.PeerID, error) {
		c, err := r.Cookie("session")
		if err != nil {
			return "", sse.ErrUnauthorized
		}
		user, ok := store.Lookup(c.Value)
		if !ok {
			return "", sse.ErrUnauthorized
		}
		// One user may have several devices; put the device in the peer ID
		// and check it belongs to the user.
		device := r.Header.Get(sse.PeerHeader)
		if device == "" || !ownsDevice(user, device) {
			return "", errors.New("device does not belong to user")
		}
		return pipe.PeerID(device), nil
	})
}

func ownsDevice(user, device string) bool { /* your lookup */ return true }
```

A JWT variant verifies the token from `sse.BearerToken(r)` and returns the
subject (or a device claim) as the peer ID. Whatever you return, the server
compares it with `X-Pipe-Peer`, so the client's configured ID must match the
identity your credential proves. The error you return is never sent to the
client; it is logged at debug level only.

## The wire protocol, briefly

One URL, two methods.

**`GET`** opens the event stream for the authenticated peer. The response is
`text/event-stream` with these events:

| Event | Data | Meaning |
| --- | --- | --- |
| `hello` | JSON string, the peer ID | Sent first. The client checks it against its own ID and fails `Open` on a mismatch. |
| `signal` | JSON `pipe.Signal` | One signal. Carries an `id:` line with a per-peer sequence number. |
| `replaced` | empty | A newer stream attached for the same peer; this stream is done. The client treats it as permanent. |
| `shutdown` | empty | The server is closing. The client reconnects with backoff. |
| `: ping` (comment) | | Written every `KeepAlive` on an idle stream so proxies keep the connection and clients notice a dead one. |

A client reconnecting sends `Last-Event-ID: <n>`; the server replays every
signal for that peer with a higher sequence number that it still holds (up to
`ReplayDepth` of them). Pipe drops duplicates by signal ID, so replaying too
much is harmless and replaying too little is the only real loss.

**`POST`** sends one signal; the body is the JSON envelope.

| Status | Meaning | What the client does |
| --- | --- | --- |
| 202 | Queued for the recipient. | `Send` returns nil. |
| 400 | Not a valid envelope, or `Last-Event-ID` was not a number. | `Send` returns an error with the server's message. |
| 401 | No or bad credential. | `Send` returns `sse.ErrUnauthorized`; a stream gets `sse.ErrPermanent`. |
| 403 | Credential does not match `X-Pipe-Peer`, or `from` is not the caller. | Same as 401. |
| 404 | Recipient is not connected and not within its offline grace. | `Send` returns `pipe.ErrPeerUnavailable`; the dial fails fast. |
| 413 | Body larger than `pipe.MaxEnvelopeSize` plus 4 KiB. | Error. Pipe never produces such envelopes. |
| 503 | Recipient's queue is full, `MaxPeers` reached, or the server is shutting down. | `Send` returns an error; a stream reconnects with backoff. |

## Server configuration

| Field | Default | Protects against |
| --- | --- | --- |
| `Authenticator` | required | Anyone signaling as anyone. |
| `QueueSize` | 256 | A peer that stops reading holding unbounded memory. Signals for it beyond this are refused with 503. A negotiation is a few dozen signals, so 256 covers several concurrent sessions per peer. |
| `ReplayDepth` | 256 | Memory for reconnect replay. Delivered signals kept per peer for a client that reconnects with `Last-Event-ID`. |
| `OfflineGrace` | 30s | Forgetting a peer that blinked. A peer with no stream keeps its queue this long, then is removed and senders get 404. Longer means slower "peer unavailable" errors; shorter means a laptop waking from sleep loses its queue. |
| `KeepAlive` | 15s | Proxies and NATs dropping idle streams; clients not noticing a dead one. |
| `MaxPeers` | 0 (unlimited) | Memory exhaustion from many registrations. New peers beyond the limit get 503. |
| `Logger` | discard | Nothing. Tokens, SDP, and candidates are never logged at any level. |

Memory per known peer is roughly `(QueueSize + ReplayDepth)` envelopes at
worst, a few KiB each, plus one goroutine per attached stream. A single small
process handles thousands of peers.

## The client

`sse.Client` implements `pipe.Signaler`:

```go
signaler := &sse.Client{
	URL:   "https://signal.example.net/pipe",
	Token: os.Getenv("PIPE_SIGNAL_TOKEN"),
}
ep, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: signaler})
```

| Field | Default | Notes |
| --- | --- | --- |
| `URL` | required | Where the server is mounted. |
| `Token` | | Sent as `Authorization: Bearer …` on every request. |
| `Authorize` | | `func(r *http.Request, local pipe.PeerID)` called on every request after default headers. For credentials that are not one static token, or one client acting as several peers. |
| `HTTPClient` | `http.DefaultClient` | Its `Timeout` must be zero; a timeout would cut the event stream. Contexts bound individual requests. |
| `Backoff` | 500ms initial, 10s max, factor 2, jitter 0.2 | Reconnection spacing for the stream. |
| `Inbox` | 64 | Signals received but not yet consumed by `Receive`. |
| `Logger` | discard | Debug lines about reconnects; never the signal contents. |

`Open` returns only after the server's `hello` confirms the identity, so a bad
token, a wrong URL, or a token that belongs to another peer fails at
`pipe.New` rather than at the first dial. Those failures wrap
`sse.ErrPermanent` (and, for credentials, `sse.ErrUnauthorized`). Network
errors and 5xx responses are transient: the stream goroutine reconnects with
backoff forever until `Close`, replaying from its last event ID. When the
stream fails permanently after `Open`, the endpoint's receive loop stops and
every later `Dial` on that endpoint fails; rebuild the endpoint with a fixed
configuration.

One `Client` acting as several peers (a test, or one process hosting several
identities) picks the credential per peer:

```go
tokens := map[pipe.PeerID]string{"alice": aliceToken, "bob": bobToken}

signaler := &sse.Client{
	URL: "https://signal.example.net/pipe",
	Authorize: func(r *http.Request, local pipe.PeerID) {
		r.Header.Set("Authorization", "Bearer "+tokens[local])
	},
}
```

Two processes opening the same peer ID do not both stay attached. The newer
stream replaces the older one, which receives `replaced` and reports
`sse.ErrPermanent`. Give every device its own ID.

## TLS and reverse proxies

Terminate TLS in front of the server or hand it a certificate. Whichever proxy
you use, it must not buffer the event stream and must not cut long-lived
responses.

Caddy:

```
signal.example.net {
	reverse_proxy /pipe* 127.0.0.1:8080 {
		flush_interval -1
		transport http {
			read_timeout 0
			write_timeout 0
		}
	}
	reverse_proxy /healthz 127.0.0.1:8080
}
```

nginx:

```nginx
location /pipe {
	proxy_pass http://127.0.0.1:8080;
	proxy_http_version 1.1;
	proxy_set_header Connection "";
	proxy_buffering off;
	proxy_cache off;
	proxy_read_timeout 1h;
	proxy_send_timeout 1h;
	chunked_transfer_encoding on;
}
```

The server sets `X-Accel-Buffering: no` and `Cache-Control: no-cache` on
streams for proxies that honor them, and writes a keepalive comment every
15 seconds so that idle streams do not look dead to a proxy with a shorter
idle timeout. If your proxy has a hard maximum response duration (some cloud
load balancers do), the client simply reconnects when it fires; nothing is
lost within `ReplayDepth`, but set the maximum as high as you can.

Inside your own `http.Server`, never set `WriteTimeout`. The handler applies a
10-second deadline to each individual write through `http.ResponseController`,
which bounds a stalled client without bounding a healthy stream's lifetime.

## Scaling and running more than one instance

State lives in memory: per-peer queues, replay logs, and the attached stream.
That is what makes the server small, and it means all peers that need to talk
to each other must be on the same instance. For a personal project or a
product with thousands of peers, one instance behind TLS is the right size.

If you do run several, route by peer so that every peer of a group lands on
the same instance (a consistent hash on the peer ID in the load balancer, or a
separate hostname per group), and remember that `Server.Peers()` and
`/healthz` are per instance. A shared backing store is not something this
package tries to provide; a transport over NATS, Redis Streams, or a message
broker is a better fit for that shape, and the next section is how to build
one.

## Writing your own Signaler

Pipe owns the negotiation protocol; a transport only moves envelopes. Two
interfaces:

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

The rules from `signaling.go` that the endpoint relies on:

- `Receive` has exactly one caller at a time (the endpoint's receive loop).
- `Send` may be called concurrently with `Receive`. The endpoint serializes
  its own sends, so `Send` does not have to be safe against other `Send`s.
- `Send` and `Receive` honor their contexts.
- `Close` is idempotent and unblocks both `Send` and `Receive`.
- Delivery is at-least-once, ordered when practical. Duplicates and
  reordering are tolerated; silent loss is what breaks dials.
- Implementations may reconnect internally.
- Report a missing recipient by wrapping `pipe.ErrPeerUnavailable`, and a
  peer ID already registered by wrapping `pipe.ErrDuplicatePeer`, when the
  transport can know those things.

The envelope must survive the trip byte for byte in meaning: encode with
`encoding/json` and do not rewrite fields. A transport that can, should also
refuse a signal whose `From` is not the authenticated sender, as the HTTP
server does; that is what turns peer IDs into identities.

Run the conformance suite against your transport. It checks every rule above
and opts into the two transport-dependent behaviors through `Config`:

```go
package mytransport_test

import (
	"testing"

	"ella.to/pipe"
	"ella.to/pipe/signaling/signalertest"

	"example.com/mytransport"
)

func TestConformance(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		// Called once per subtest. Start a broker here and register its
		// shutdown with t.Cleanup.
		NewSignaler: func(t *testing.T) pipe.Signaler {
			return mytransport.Start(t)
		},
		// Set to true if Open fails with pipe.ErrDuplicatePeer for a peer ID
		// that is already live.
		RejectsDuplicatePeers: true,
		// Set to true if Send fails with pipe.ErrPeerUnavailable for a peer
		// that has no live connection.
		ReportsUnavailablePeer: true,
		// Largest payload the suite round-trips; defaults to pipe.MaxSDPSize.
		MaxPayload: 0,
	})
}
```

`signalertest.Offer`, `signalertest.Answer`, and `signalertest.Signal` build
valid envelopes for your own tests. The HTTP transport's own conformance test
is in `signaling/sse/sse_test.go` and is a good template: it starts an
`httptest.Server` per subtest and passes `ReportsUnavailablePeer: true`.

### The memory hub

`signaling/memory` is the in-process transport used by the tests and the
single-binary examples. Both peers must share the same `*memory.Hub`. It can
inject faults deterministically, which is how pipe's own tolerance for
imperfect transports is tested and how you can test yours:

```go
hub := memory.New(
	memory.WithQueueSize(128),
	memory.WithFault(memory.DuplicateAll()),      // every signal twice
)
// other faults:
//   memory.DropKind(pipe.KindICEComplete)        drop a kind entirely
//   memory.DropFirst(pipe.KindCandidate, 2)      drop the first n of a kind
//   memory.SwapAdjacent(pipe.KindCandidate)      reorder pairs

hub.Disconnect("alice")   // as if alice's transport failed
hub.Registered()          // live peer IDs
```

A `Fault` is any `func(pipe.Signal) []pipe.Signal`: return nothing to drop,
several copies to duplicate, or hold and release later to reorder.

## Related

- Hardening the whole deployment: [06-security.md](06-security.md)
- Running it in Docker with TLS in front: [08-docker.md](08-docker.md)
- The envelope format and validation limits: [11-protocol.md](11-protocol.md)
