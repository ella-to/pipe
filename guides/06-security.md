# Securing a pipe deployment

This guide is for anyone about to expose a signaling server or a TURN relay to
a network they do not fully control. It walks through what each component can
see, what it can do if it turns hostile, and the specific settings and code
that close each gap. Read [01-concepts.md](01-concepts.md) first if the words
signaling, ICE, and TURN are new to you.

## The threat model, in layers

A pipe deployment has four kinds of participant. Each one can be honest,
compromised, or simply misconfigured, and the defenses are different for each.

| Participant | Sees | Can do if hostile | Defense |
| --- | --- | --- | --- |
| Signaling server | Peer IDs, who talks to whom, SDP (including DTLS fingerprints and ICE credentials), candidates (IP addresses) | Route offers to the wrong peer, substitute its own SDP to man-in-the-middle, deny service, watch the social graph | Run it yourself over TLS with authenticated peers; run mTLS over the pipe for identities that must not depend on the server |
| TURN relay | Source and relay IP addresses, packet sizes and timing, ciphertext | Drop or delay traffic, burn your bandwidth for others, be used as a pivot into your network | Credentials with short lifetimes, quotas, `denied-peer-ip`, a firewalled port range |
| A peer | Everything the application sends it | Dial anyone whose ID it knows, flood a listener with offers, send malformed frames | Authenticated peer IDs, `Config.AllowPeer`, protocol limits, `AcceptBacklog` |
| A network observer | Encrypted packets, addresses, timing | Correlate who talks to whom by timing and size | Relay-only policy hides peer addresses from each other; nothing hides them from the relay |

Two things are true regardless of configuration. First, the data path is
encrypted end to end with DTLS, so neither the signaling server nor the relay
reads your bytes. Second, a *malicious* signaling server can still swap the
DTLS fingerprints in the SDP it forwards and terminate DTLS itself on both
sides. Authentication of the far end therefore has two tiers: peer IDs bound
to credentials by an honest signaling server, and, when the server itself
must not be trusted, TLS over the pipe with certificates you control.

## What is encrypted, and by whom

- **ICE connectivity checks** are STUN messages authenticated with the ICE
  credentials exchanged in the SDP. They carry no application data.
- **DTLS** runs over the nominated candidate pair. Each side's SDP carries the
  fingerprint of its certificate, and each side verifies the far end's
  certificate against the fingerprint it received through signaling. If the
  signaling server delivered the fingerprints unmodified, the key exchange is
  authenticated and the session key is known only to the two peers.
- **SCTP** runs inside DTLS. The relay forwards DTLS records; it cannot see
  SCTP, the DataChannel, or pipe's frames.
- **Signaling** itself is JSON over whatever transport you chose. With
  `signaling/sse` that is HTTP, so put TLS on it (see below). SDP contains no
  secrets that let an observer decrypt data, but it does contain ICE
  credentials that let an observer *inject connectivity checks*, and it maps
  peer IDs to IP addresses.

A DTLS fingerprint tells you "the party I am encrypting to is the party whose
SDP the signaling server showed me". It does not tell you *who that party is*.
Identity comes from one of two places: the signaling server's authentication
of the peer that submitted the SDP, or a certificate the peer presents over
the pipe.

## Peer IDs: routing identity versus authenticated identity

A `pipe.PeerID` is a routing name. The library validates its shape (non-empty,
at most 128 bytes, valid UTF-8, no control characters, no surrounding
whitespace) and nothing else. Whether `"alice"` really is alice depends on the
signaling transport.

`signaling/sse` makes peer IDs authenticated:

1. Every request, stream or send, passes through the configured
   `Authenticator`, which returns the peer ID the caller is allowed to act as.
   Failure is a 401 with no detail.
2. A client states the peer ID it intends to act as in the `X-Pipe-Peer`
   header. If it does not match the authenticated identity, the request is
   refused with 403, so a client holding bob's token cannot open a stream as
   alice.
3. A posted signal whose `from` field is not the authenticated peer is refused
   with 403. Nobody can forge the sender of an offer.
4. Signals are delivered only to the stream of the peer named in `to`.

The result is that when an endpoint receives an offer from `"alice"`, it knows
the holder of alice's credential sent it. That is what makes an allow-list
meaningful.

### Bearer tokens with `StaticTokens`

```go
package main

import (
	"log/slog"
	"net/http"
	"os"
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
		MaxPeers: 100,
		Logger:   slog.Default(),
	})
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/pipe", srv)

	s := &http.Server{
		Addr:              ":8443",
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut every event stream.
	}
	panic(s.ListenAndServeTLS("cert.pem", "key.pem"))
}
```

`StaticTokens` compares tokens in constant time and binds each token to one
peer ID. Empty tokens and empty peer IDs are dropped from the table.

### A custom `Authenticator`

Anything that can turn an `*http.Request` into a peer ID works: a session
cookie looked up in your user database, a JWT whose subject is the peer ID, a
client certificate's common name. Return `sse.ErrUnauthorized` (or any error)
to refuse; the error text never reaches the client.

```go
package main

import (
	"net/http"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

// clientCertAuth accepts requests that presented a client certificate and
// uses its common name as the peer ID. Configure the http.Server with
// tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}.
func clientCertAuth() sse.Authenticator {
	return sse.AuthenticatorFunc(func(r *http.Request) (pipe.PeerID, error) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			return "", sse.ErrUnauthorized
		}
		cn := r.TLS.PeerCertificates[0].Subject.CommonName
		if cn == "" {
			return "", sse.ErrUnauthorized
		}
		return pipe.PeerID(cn), nil
	})
}
```

### `TrustPeerHeader` is for localhost only

`sse.TrustPeerHeader()` believes whatever `X-Pipe-Peer` says. It exists so
that two processes on one machine can be wired together in a minute
(`examples/signaling -insecure-trust-peer-header`). On any interface other
than loopback it lets anyone act as any peer, dial your listeners under a
trusted name, and hijack signals meant for someone else. Do not deploy it.

## Authorization: `Config.AllowPeer`

Authentication says who is calling. Authorization says whether they may. Pipe
consults `Config.AllowPeer` for every inbound offer before it spends a
PeerConnection on it. Returning `false` refuses the session with
`RejectUnauthorized`; the dialer sees `ErrPeerRejected` and can read the code
with `errors.As` into a `*pipe.RejectedError`.

```go
package main

import (
	"context"
	"sync"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

type allowList struct {
	mu    sync.RWMutex
	peers map[pipe.PeerID]bool
}

func (a *allowList) Allow(peer pipe.PeerID) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.peers[peer]
}

func (a *allowList) Set(peers ...pipe.PeerID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.peers = make(map[pipe.PeerID]bool, len(peers))
	for _, p := range peers {
		a.peers[p] = true
	}
}

func listen(ctx context.Context, signaler *sse.Client) (*pipe.Endpoint, error) {
	acl := &allowList{}
	acl.Set("alice", "carol")

	return pipe.New(ctx, pipe.Config{
		ID:        "bob",
		Signaler:  signaler,
		AllowPeer: acl.Allow, // runs on the signaling receive loop: keep it fast
	})
}
```

Rules of thumb:

- `AllowPeer` is only as good as the signaler's authentication. With
  `TrustPeerHeader` it is decoration.
- It runs on the endpoint's signaling receive loop. Do a map lookup, not a
  database query; if you need remote checks, cache them.
- A refused peer costs you one signaling round trip and nothing else: no
  PeerConnection, no ICE gathering, no backlog slot.
- Combine it with `AcceptBacklog` so that even an allowed peer cannot hold
  more than a bounded number of sessions in negotiation.

## TLS for signaling

Bearer tokens travel in the `Authorization` header and SDP travels in request
bodies and event streams. Without TLS, anyone on the path can read the tokens
and replay them. The moment the signaling server leaves your machine, it needs
TLS.

Two ways to get it:

- **A reverse proxy** (Caddy, nginx, Traefik) that terminates TLS and forwards
  to the signaling server on loopback or a private network. Disable response
  buffering and raise the read timeout on the proxy so that Server-Sent Events
  streams survive; [08-docker.md](08-docker.md) has snippets.
- **Directly**, with `examples/signaling -tls-cert cert.pem -tls-key key.pem`,
  or `http.Server.ListenAndServeTLS` in your own program.

Either way, the `http.Server` must not set `WriteTimeout`. The `sse.Server`
handler sets a per-write deadline itself through `http.ResponseController`, so
a stalled client cannot pin a goroutine, but a global write timeout would kill
every healthy stream too.

On the client, `sse.Client.HTTPClient` must not set `Timeout` for the same
reason; contexts bound individual requests.

## Handling tokens

- **Generate** them with a real random source: `openssl rand -hex 24` gives
  192 bits. `examples/signaling` refuses tokens shorter than 16 characters and
  refuses a token shared by two peers.
- **One token per device.** A peer ID belongs to one live endpoint at a time
  (a second stream for the same peer replaces the first), so a shared token
  means devices kick each other off.
- **Least privilege.** A token authenticates one peer ID and nothing else. Do
  not reuse a token that also unlocks your API or your TURN secret.
- **Rotation.** `StaticTokens` copies its map at construction, so rotating a
  token means building a new `sse.Server` or writing an `Authenticator` that
  consults a store you can update. Clients reconnect automatically with
  backoff when a stream drops; a client whose token was revoked sees a 401
  and fails permanently (`sse.ErrPermanent`), which is the intended outcome.
- **Never log them.** `sse.Server` logs peer IDs, never tokens or
  `Authorization` headers. Keep your reverse proxy's access log from recording
  headers too.

## Securing the TURN relay

A TURN server relays bytes for anyone who can authenticate. The security
questions are who may allocate, how much they may use, and where the relay is
allowed to send.

### Credentials

- **Long-term (static) credentials** in `examples/turnserver -users` are
  converted to the RFC 5389 key digest (`MD5(user:realm:password)`) at
  startup; the password itself is not kept. They still never expire and must
  be rotated by hand. Suitable for a handful of devices you own.
- **Ephemeral credentials** (`-auth-secret`, the TURN REST API scheme, coturn's
  `use-auth-secret`) are derived from a shared secret and an expiry embedded
  in the username. Issue them with `examples/turncred` or
  `turnx.IssueCredentials` when a signed-in user asks; they stop working at
  the expiry with no revocation list. Use a TTL that matches your session
  length (hours, not weeks). This is the right default for anything with more
  than a few users. See [07-multi-user-relay.md](07-multi-user-relay.md).
- The **realm** is part of the credential digest. Pick one and make clients
  match it; a mismatch fails authentication, which is a common first
  deployment error.
- The default `admin=admin` in `examples/turnserver` is for a server bound to
  `127.0.0.1`. The command prints a warning about a relay with no users at all
  but cannot know your network; do not run the defaults on a public address.

### Where the relay may send

A TURN allocation lets an authenticated client send to any peer address it
names. Without a filter, a client can reach hosts on the relay's private
network, including the relay itself and your cloud metadata endpoint. The
bundled coturn configuration (`examples/docker/coturn/turnserver.conf`) denies
RFC 1918 ranges, loopback, link-local, and their IPv6 equivalents with
`denied-peer-ip`. When you run the relay on a host that has private
neighbors, apply the same policy at the firewall: allow the relay port range
to reach the internet only.

### Ports and firewall

Confine relay sockets to a range (`-relay-ports 49152-49352`) and open exactly
that range plus 3478 (UDP, and TCP if you serve `transport=tcp`). A relay with
ephemeral ports needs the whole high range open, which is what makes it a
pivot.

### Quotas

Each pipe connection holds one relay allocation per side. A plan's
`MaxAllocations` (the `/n` suffix in `-plans free=512KiB/4`) bounds how many
a user may hold at once; excess Allocate requests are refused with a quota
error and counted in the per-user statistics. Set it a little above the
number of concurrent connections a legitimate user needs. Rate limits bound
bandwidth per allocation; see [07-multi-user-relay.md](07-multi-user-relay.md)
for tiers.

### Transport

`turn:` over UDP and TCP is authenticated but not encrypted. Everything it
carries is already DTLS ciphertext, so confidentiality does not depend on it.
`turns:` (TLS) hides the TURN protocol itself from a network observer and
passes firewalls that allow only TLS; use it when clients are on hostile
networks. The example server does not terminate TLS; coturn does.

## Cryptographic peer identity: mTLS over the pipe

When the signaling server is run by someone else, or when you want identity
that survives a compromised signaling server, put TLS inside the pipe. A
`pipe.Conn` is a `net.Conn`, so `crypto/tls` runs over it unchanged. Each
device holds a certificate issued by a CA you control, with the peer ID as its
common name (or a SAN). Both sides verify the other's certificate and check
that the name matches the peer they intended to talk to.

```go
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"ella.to/pipe"
)

// tlsConfig builds a mutual-TLS configuration for one device. certFile and
// keyFile are the device's certificate and key; caFile is the CA that issues
// every device certificate.
func tlsConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificates found")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// dialSecure connects to peer and proves, with a certificate rather than a
// signaling server's word, that the far end is peer.
func dialSecure(ctx context.Context, ep *pipe.Endpoint, peer pipe.PeerID, base *tls.Config) (*tls.Conn, error) {
	raw, err := ep.Dial(ctx, peer)
	if err != nil {
		return nil, err
	}
	cfg := base.Clone()
	cfg.ServerName = string(peer) // verified against the certificate's names
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", peer, err)
	}
	return conn, nil
}

// acceptSecure completes the server side of the handshake and checks that the
// client certificate names the peer ID the connection claims to come from.
func acceptSecure(ctx context.Context, ln *pipe.Listener, base *tls.Config) (*tls.Conn, error) {
	raw, err := ln.AcceptConn()
	if err != nil {
		return nil, err
	}
	conn := tls.Server(raw, base)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		_ = conn.Close()
		return nil, errors.New("client presented no certificate")
	}
	if got := state.PeerCertificates[0].Subject.CommonName; got != string(raw.PeerID()) {
		_ = conn.Close()
		return nil, fmt.Errorf("certificate is for %q but the peer claims to be %q", got, raw.PeerID())
	}
	return conn, nil
}
```

The compatibility suite in `test/compat` runs exactly
this shape, a TLS 1.3 handshake and transfer over live pipe connections, so
the behavior is tested rather than assumed.

With mTLS in place, the signaling server could substitute fingerprints and
terminate DTLS, but it would then have to present a device certificate it
does not have; the TLS handshake fails and the connection is refused. The
server's remaining power is denial of service and traffic analysis.

## What pipe never logs

The library's own logs, at any level, exclude TURN credentials, SDP, ICE
usernames and passwords, and application payloads. Metrics labels never carry
peer IDs or session IDs. `sse.Server` never logs tokens, SDP, or candidates.
`turnx` logs usernames and relay addresses, never passwords.

Keep that property in your own code:

- Log `conn.RemoteAddr()` (which is the peer ID and session ID), not the
  contents of what you read.
- Do not log `pipe.Signal` values from a custom `Signaler`; the payload is SDP.
- Do not log `*http.Request` headers on the signaling path.
- Treat an `error` from `pipe.New` with care: it wraps your configuration
  errors, and a badly written custom signaler could include a URL with
  credentials in it. Pipe's own `ErrConfig` messages name the field, not the
  value.

## Denial-of-service surfaces and their bounds

Everything an unauthenticated or hostile party can make pipe allocate is
bounded. The table lists the knob or constant and where it lives.

| Surface | Bound | Where |
| --- | --- | --- |
| Inbound sessions negotiating or waiting to be accepted | `Config.AcceptBacklog`, default 64; excess offers are rejected with `busy` | `config.go` |
| Offers from peers you do not want | `Config.AllowPeer`, refused before any allocation | `config.go`, `endpoint.go` |
| One signaling envelope | `MaxEnvelopeSize` = 256 KiB payload; the HTTP server accepts at most that plus 4 KiB of body | `signal_validate.go`, `signaling/sse/server.go` |
| SDP in an offer, answer, or restart | `MaxSDPSize` = 128 KiB | `signal_validate.go` |
| One ICE candidate string | `MaxCandidateSize` = 8 KiB; username fragment at most 256 bytes | `signal_validate.go` |
| Peer ID | `MaxPeerIDLength` = 128 bytes | `signal_validate.go` |
| Reject or close reason | `MaxReasonLength` = 512 bytes | `signal_validate.go` |
| Candidates buffered before the remote description arrives | 256 per session; more is a protocol violation that ends the session | `signal_validate.go` |
| Replayed signals | Dedupe cache of 4096 recent signal IDs per endpoint | `config.go`, `dedupe.go` |
| Events queued for one session | 512; overflow fails that session, never the endpoint | `mailbox.go` |
| A stream frame | Negotiated `FramePayload` (default 16 KiB, hard ceiling 256 KiB); larger frames are a protocol violation | `internal/frame` |
| Unread received data per connection | `Config.ReadBuffer`, default 1 MiB; beyond that SCTP backpressure applies to the sender | `config.go` |
| Signals queued for a peer on the signaling server | `sse.Config.QueueSize`, default 256; senders get 503 | `signaling/sse/server.go` |
| Replay window per peer on the signaling server | `sse.Config.ReplayDepth`, default 256 | `signaling/sse/server.go` |
| Known peers on the signaling server | `sse.Config.MaxPeers`, default unlimited; set it | `signaling/sse/server.go` |
| How long a disconnected peer's queue is kept | `sse.Config.OfflineGrace`, default 30 s | `signaling/sse/server.go` |
| A stalled event-stream write | 10 s per write | `signaling/sse/server.go` |
| Relay allocations per user | plan `MaxAllocations` | `examples/internal/turnx` |
| Relay bandwidth per allocation | plan `Rate` and `Burst` | `examples/internal/turnx` |

Things that are deliberately *not* rate limited inside the library, because
the right policy is yours: how many endpoints one token may create (one peer
ID at a time, but tokens are not counted), how often a peer may dial, and how
many signals per second one peer may post. Enforce those at the reverse proxy
or in a custom `Authenticator`.

## Privacy

ICE candidates carry IP addresses. With the default `ICETransportPolicyAll`,
each peer learns the other's host and server-reflexive addresses through
signaling, and the signaling server sees both. If your users should not learn
each other's addresses, set `ICETransportPolicyRelay` on both sides: only
relay candidates are gathered and exchanged, the peers see only the relay's
address, and all traffic costs relay bandwidth. The relay operator still sees
everyone's addresses; that is inherent.

Peer IDs are visible to the signaling server and to every peer that talks to
you. Do not put personal data in them; a random per-device identifier mapped
to a user in your own database is the safer design.

## Deployment checklist

Before a signaling server or relay is reachable from the internet:

- [ ] Signaling uses `StaticTokens` or a custom `Authenticator`, never
      `TrustPeerHeader`.
- [ ] Tokens are at least 16 random characters, one per device, kept out of
      source control and logs.
- [ ] TLS terminates in front of (or in) the signaling server; the proxy does
      not buffer responses and has a long read timeout.
- [ ] `http.Server.WriteTimeout` is zero; `ReadHeaderTimeout` is set.
- [ ] `sse.Config.MaxPeers` is set.
- [ ] Listeners that should not accept everyone set `Config.AllowPeer`.
- [ ] The relay uses ephemeral credentials with a TTL of hours, or static
      credentials that are not `admin=admin`.
- [ ] The relay has a fixed `-relay-ports` range, and the firewall opens only
      3478 and that range.
- [ ] The relay cannot send into private address space (`denied-peer-ip` on
      coturn, or firewall rules for the example server).
- [ ] Plans set `MaxAllocations` and a rate for untrusted users.
- [ ] `-relay-ip` is the public address clients will actually reach.
- [ ] Connections that need cryptographic identity run mTLS over the pipe.
- [ ] Your application logs peer IDs, not payloads, headers, or SDP.

The operational side of the same components, metrics, logs, and what to do
when a connection does not come up, is in [10-operations.md](10-operations.md).
