# Signaling server

`sse.Server` is an `http.Handler`. Peers hold one Server-Sent Events stream
open to receive signals, and POST to send them. It carries a few KiB per
connection and never sees application data.

| Step | Adds |
| --- | --- |
| [1](#step-1-minimal) | A local server with no authentication |
| [2](#step-2-bearer-tokens) | One token per peer |
| [3](#step-3-production-http-server) | TLS, timeouts, health check, limits, graceful shutdown |
| [4](#step-4-your-own-authentication) | Stateless tokens minted by your API |
| [5](#step-5-client-options) | Client knobs: one client for many peers, custom HTTP client |
| [6](#step-6-behind-a-reverse-proxy) | Caddy and nginx settings |
| [7](#step-7-your-own-transport) | Replace HTTP with your own signaler, and test it |

## Step 1: minimal

```go
// signal/main.go
package main

import (
	"log"
	"net/http"

	"ella.to/pipe/signaling/sse"
)

func main() {
	srv, err := sse.NewServer(sse.Config{Authenticator: sse.TrustPeerHeader()})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	http.Handle("/pipe", srv)
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
```

Client side:

```go
ep, err := pipe.New(ctx, pipe.Config{
	ID:       "alice",
	Signaler: &sse.Client{URL: "http://127.0.0.1:8080/pipe"},
})
```

`TrustPeerHeader` lets anyone act as anyone. Loopback only.

## Step 2: bearer tokens

```go
// signal/main.go
package main

import (
	"log"
	"net/http"
	"os"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	srv, err := sse.NewServer(sse.Config{
		Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
			os.Getenv("ALICE_TOKEN"): "alice",
			os.Getenv("BOB_TOKEN"):   "bob",
		}),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	http.Handle("/pipe", srv)
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
```

```go
Signaler: &sse.Client{
	URL:   "http://127.0.0.1:8080/pipe",
	Token: os.Getenv("ALICE_TOKEN"),
},
```

The server rejects a request whose token does not match the peer ID the client
claims, and a signal whose `from` is not the caller. Peer IDs delivered through
`sse` are therefore authenticated, which is what makes `Config.AllowPeer` a
real access-control list.

## Step 3: production HTTP server

```go
// signal/main.go
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	srv, err := sse.NewServer(sse.Config{
		Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
			os.Getenv("ALICE_TOKEN"): "alice",
			os.Getenv("BOB_TOKEN"):   "bob",
		}),
		MaxPeers:     1000,
		OfflineGrace: 30 * time.Second,
		Logger:       slog.Default(),
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/pipe", srv)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok peers=%d\n", len(srv.Peers()))
	})

	httpServer := &http.Server{
		Addr:              ":8443",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: it would cut every event stream. The handler bounds
		// each write itself.
	}

	go func() {
		err := httpServer.ListenAndServeTLS("cert.pem", "key.pem")
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()

	// Close the handler first so streaming requests return, then shut down.
	_ = srv.Close()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
}
```

```go
Signaler: &sse.Client{URL: "https://signal.example.net:8443/pipe", Token: os.Getenv("ALICE_TOKEN")},
```

`sse.Config`:

| Field | Default | Bounds |
| --- | --- | --- |
| `Authenticator` | required | Who may act as which peer |
| `QueueSize` | 256 | Undelivered signals per peer; beyond it senders get 503 |
| `ReplayDepth` | 256 | Delivered signals kept for a reconnecting stream |
| `OfflineGrace` | 30s | How long a disconnected peer keeps its queue; after it, senders get 404 (`pipe.ErrPeerUnavailable`) |
| `KeepAlive` | 15s | Comment line on idle streams, for proxies and dead-stream detection |
| `MaxPeers` | 0 (unlimited) | Concurrently known peers; new peers beyond it get 503 |
| `Logger` | discard | Tokens, SDP, and candidates are never logged |

## Step 4: your own authentication

Any `func(*http.Request) (pipe.PeerID, error)` works. This one verifies
stateless tokens of the form `peer.signature` that your API mints with a shared
secret, so the signaling server needs no user list.

```go
// signal/main.go
//
//	SIGNAL_SECRET=$(openssl rand -hex 32) go run ./signal
//	SIGNAL_SECRET=... go run ./signal token alice   # prints a token for alice
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func sign(secret []byte, peer string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(peer))
	return peer + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func hmacAuth(secret []byte) sse.Authenticator {
	return sse.AuthenticatorFunc(func(r *http.Request) (pipe.PeerID, error) {
		token, ok := sse.BearerToken(r)
		if !ok {
			return "", sse.ErrUnauthorized
		}
		peer, _, ok := strings.Cut(token, ".")
		if !ok || !hmac.Equal([]byte(token), []byte(sign(secret, peer))) {
			return "", sse.ErrUnauthorized
		}
		return pipe.PeerID(peer), nil
	})
}

func main() {
	secret := []byte(os.Getenv("SIGNAL_SECRET"))
	if len(secret) == 0 {
		log.Fatal("set SIGNAL_SECRET")
	}

	if len(os.Args) == 3 && os.Args[1] == "token" {
		fmt.Println(sign(secret, os.Args[2]))
		return
	}

	srv, err := sse.NewServer(sse.Config{Authenticator: hmacAuth(secret)})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	http.Handle("/pipe", srv)
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
```

The same shape covers session cookies (`r.Cookie`), JWTs (verify
`sse.BearerToken(r)` and return a claim), or client certificates
(`r.TLS.PeerCertificates`). The returned error is never sent to the client.

## Step 5: client options

Every `sse.Client` field:

```go
signaler := &sse.Client{
	URL:   "https://signal.example.net/pipe",
	Token: os.Getenv("PIPE_SIGNAL_TOKEN"),

	// Optional. Timeout must stay zero: it would cut the event stream.
	HTTPClient: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
	Backoff:    pipe.Backoff{Initial: time.Second, Maximum: 30 * time.Second, Factor: 2, Jitter: 0.2},
	Inbox:      64,
	Logger:     slog.Default(),
}
```

One client for several peer IDs in one process, picking the token per peer:

```go
// multipeer/main.go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	ctx := context.Background()
	tokens := map[pipe.PeerID]string{
		"alice": os.Getenv("ALICE_TOKEN"),
		"bob":   os.Getenv("BOB_TOKEN"),
	}

	signaler := &sse.Client{
		URL: "http://127.0.0.1:8080/pipe",
		Authorize: func(r *http.Request, local pipe.PeerID) {
			r.Header.Set("Authorization", "Bearer "+tokens[local])
		},
	}

	for id := range tokens {
		ep, err := pipe.New(ctx, pipe.Config{ID: id, Signaler: signaler})
		if err != nil {
			log.Fatal(err)
		}
		defer ep.Close()
		log.Printf("%s is online", id)
	}
}
```

Behavior worth knowing:

- `pipe.New` returns only after the server confirmed the identity, so a bad
  token fails there with `sse.ErrPermanent` and `sse.ErrUnauthorized`.
- Network errors and 5xx responses reconnect with backoff and replay missed
  signals.
- Opening the same peer ID twice: the newer stream wins and the older fails
  permanently. Give every device its own ID.

## Step 6: behind a reverse proxy

The proxy must not buffer responses and must not time out long-lived ones.

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
}
```

State lives in memory, so peers that talk to each other must reach the same
instance. One instance handles thousands of peers; to run several, route by
peer ID.

## Step 7: your own transport

Implement `pipe.Signaler`, then run the conformance suite.

```go
type Signaler interface {
	Open(ctx context.Context, local PeerID) (SignalConn, error)
}

type SignalConn interface {
	Send(ctx context.Context, msg Signal) error    // may run concurrently with Receive
	Receive(ctx context.Context) (Signal, error)   // one caller at a time
	Close() error                                  // idempotent; unblocks Send and Receive
}
```

Rules: honor contexts, deliver at least once (duplicates and reordering are
fine, silent loss is not), move the JSON envelope unmodified, and wrap
`pipe.ErrPeerUnavailable` / `pipe.ErrDuplicatePeer` when the transport can tell.

```go
// mytransport/conformance_test.go
package mytransport_test

import (
	"testing"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
	"ella.to/pipe/signaling/signalertest"
)

func TestConformance(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		// Replace memory.New() with your transport. Called once per subtest.
		NewSignaler: func(t *testing.T) pipe.Signaler {
			return memory.New()
		},
		RejectsDuplicatePeers:  true, // Open fails with pipe.ErrDuplicatePeer for a live ID
		ReportsUnavailablePeer: true, // Send fails with pipe.ErrPeerUnavailable for an absent peer
	})
}
```

```sh
go test ./mytransport
```

For your own tests, `signaling/memory` injects faults:

```go
hub := memory.New(
	memory.WithQueueSize(128),
	memory.WithFault(memory.DuplicateAll()), // or DropKind, DropFirst, SwapAdjacent
)
hub.Disconnect("alice") // as if alice's transport failed
```

## Reference: HTTP protocol

`GET <url>` opens the stream. Events: `hello` (the authenticated peer ID),
`signal` (a JSON `pipe.Signal`, with an `id:` sequence number), `replaced`,
`shutdown`, and `: ping` comments. Reconnects send `Last-Event-ID`.

`POST <url>` with a JSON `pipe.Signal` body:

| Status | Meaning |
| --- | --- |
| 202 | Queued |
| 400 | Invalid envelope |
| 401 | Missing or bad credential (`sse.ErrUnauthorized`) |
| 403 | Credential does not match `X-Pipe-Peer`, or `from` is not the caller |
| 404 | Recipient unknown (`pipe.ErrPeerUnavailable`) |
| 413 | Body larger than `pipe.MaxEnvelopeSize` plus 4 KiB |
| 503 | Recipient queue full, `MaxPeers` reached, or shutting down |

The envelope itself is in [11-protocol.md](11-protocol.md).
