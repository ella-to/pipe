# Security

| Participant | Sees | Defense |
| --- | --- | --- |
| Signaling server | Peer IDs, who talks to whom, SDP, IP addresses | Run your own, TLS, authenticated peers; mTLS over the pipe if the server must not be trusted |
| TURN relay | Addresses, sizes, timing, ciphertext | Expiring credentials, plans, a firewalled port range, no relaying into private networks |
| A peer | What you send it | Authenticated peer IDs, `AllowPeer`, protocol limits |
| Network observer | Encrypted packets, addresses | Relay-only policy hides peer addresses from each other |

The data path is DTLS end to end. A **malicious** signaling server can still
swap DTLS fingerprints and sit in the middle. Two tiers of identity cover
that: peer IDs authenticated by an honest signaling server, and TLS inside the
pipe with certificates you control.

## 1. Authenticate signaling

Never deploy `sse.TrustPeerHeader()`. Use tokens or your own authenticator
([03-signaling-server.md](03-signaling-server.md)):

```go
srv, err := sse.NewServer(sse.Config{
	Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
		os.Getenv("ALICE_TOKEN"): "alice",
		os.Getenv("BOB_TOKEN"):   "bob",
	}),
	MaxPeers: 1000,
})
```

With `sse`, a signal's `from` must match the caller's credential, so the peer
ID an endpoint sees is authenticated.

Tokens: `openssl rand -hex 24`, one per device, never logged, never reused for
anything else. Revoking one means building a new `StaticTokens` map, or an
authenticator backed by a store you can update.

## 2. TLS on signaling

```go
httpServer := &http.Server{
	Addr:              ":8443",
	Handler:           mux,
	ReadHeaderTimeout: 10 * time.Second,
	// No WriteTimeout, and no Timeout on the client's http.Client:
	// both would cut the event streams.
}
log.Fatal(httpServer.ListenAndServeTLS("cert.pem", "key.pem"))
```

Or a reverse proxy; settings in [03-signaling-server.md](03-signaling-server.md#step-6-behind-a-reverse-proxy).

## 3. Authorize callers with `AllowPeer`

`AllowPeer` runs for every inbound offer, before any PeerConnection is
created. It runs on the signaling receive loop: keep it to a map lookup.

```go
// allowpeer/main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
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

func main() {
	ctx := context.Background()
	hub := memory.New()
	acl := &allowList{peers: map[pipe.PeerID]bool{"alice": true}}

	bob, err := pipe.New(ctx, pipe.Config{ID: "bob", Signaler: hub, AllowPeer: acl.Allow})
	if err != nil {
		log.Fatal(err)
	}
	defer bob.Close()
	ln, err := bob.Listen()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	for _, id := range []pipe.PeerID{"alice", "carol"} {
		ep, err := pipe.New(ctx, pipe.Config{ID: id, Signaler: hub})
		if err != nil {
			log.Fatal(err)
		}
		defer ep.Close()

		conn, err := ep.Dial(ctx, "bob")
		var rejected *pipe.RejectedError
		switch {
		case err == nil:
			fmt.Printf("%s: connected\n", id)
			conn.Close()
		case errors.As(err, &rejected):
			fmt.Printf("%s: rejected: %s\n", id, rejected.Code)
		default:
			log.Fatal(err)
		}
	}
}
```

```sh
go run ./allowpeer
# alice: connected
# carol: rejected: unauthorized
```

## 4. Cryptographic identity: mTLS over the pipe

A `pipe.Conn` is a `net.Conn`, so `crypto/tls` runs over it. Each device
holds a certificate from your CA with its peer ID as the name; both sides
verify. A signaling server that swaps fingerprints cannot produce a device
certificate, so the handshake fails.

This program creates a CA and two device certificates in memory. In
production, load them from files with `tls.LoadX509KeyPair`.

```go
// mtls/main.go
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log"
	"math/big"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAuthority() (*authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "devices CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &authority{cert: cert, key: key, pool: pool}, nil
}

// issue returns a device certificate whose name is the peer ID.
func (a *authority) issue(peer pipe.PeerID) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: string(peer)},
		DNSNames:     []string{string(peer)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func tlsConfig(cert tls.Certificate, pool *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// dialTLS connects to peer and verifies its certificate names peer.
func dialTLS(ctx context.Context, ep *pipe.Endpoint, peer pipe.PeerID, cfg *tls.Config) (*tls.Conn, error) {
	raw, err := ep.Dial(ctx, peer)
	if err != nil {
		return nil, err
	}
	cfg = cfg.Clone()
	cfg.ServerName = string(peer)
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// acceptTLS accepts a connection and verifies the client certificate names
// the peer ID the connection came from.
func acceptTLS(ctx context.Context, ln *pipe.Listener, cfg *tls.Config) (*tls.Conn, error) {
	raw, err := ln.AcceptConn()
	if err != nil {
		return nil, err
	}
	conn := tls.Server(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	if cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != string(raw.PeerID()) {
		conn.Close()
		return nil, fmt.Errorf("certificate is for %q, connection is from %q", cn, raw.PeerID())
	}
	return conn, nil
}

func main() {
	ctx := context.Background()
	ca, err := newAuthority()
	if err != nil {
		log.Fatal(err)
	}
	aliceCert, err := ca.issue("alice")
	if err != nil {
		log.Fatal(err)
	}
	bobCert, err := ca.issue("bob")
	if err != nil {
		log.Fatal(err)
	}

	hub := memory.New()
	bob, err := pipe.New(ctx, pipe.Config{ID: "bob", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer bob.Close()
	alice, err := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: hub})
	if err != nil {
		log.Fatal(err)
	}
	defer alice.Close()

	ln, err := bob.Listen()
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		conn, err := acceptTLS(ctx, ln, tlsConfig(bobCert, ca.pool))
		if err != nil {
			log.Println("accept:", err)
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		fmt.Fprintf(conn, "bob verified you, you said %s", line)
	}()

	conn, err := dialTLS(ctx, alice, "bob", tlsConfig(aliceCert, ca.pool))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintln(conn, "hello")
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(reply)
}
```

```sh
go run ./mtls
# bob verified you, you said hello
```

## 5. Harden the relay

Expiring credentials, a short TTL, and plans with allocation quotas
([05-turn-relay.md](05-turn-relay.md#step-6-ephemeral-credentials)):

```go
srv, err := relay.Start(relay.Config{
	Listen:      "0.0.0.0:3478",
	RelayIP:     net.ParseIP("203.0.113.10"),
	MinPort:     49152,
	MaxPort:     49252,
	Realm:       "relay.example.net",
	AuthSecret:  os.Getenv("PIPE_TURN_SECRET"),
	Plans:       map[string]relay.Plan{"free": {Rate: 512 << 10, MaxAllocations: 4}},
	DefaultPlan: relay.Plan{Rate: 256 << 10, MaxAllocations: 2},
})
```

```go
user, pass, err := relay.IssueCredentials(secret, "alice@free", time.Hour)
```

`relay` forwards to any address a client names. Stop it from reaching your
private network at the host firewall:

```sh
for net in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 127.0.0.0/8; do
  iptables -A OUTPUT -p udp --sport 49152:49252 -d $net -j DROP
done
```

coturn does the same with `denied-peer-ip` in
`examples/docker/coturn/turnserver.conf`.

Rotate `PIPE_TURN_SECRET` to revoke every outstanding credential at once.

## 6. Hide peer addresses from each other

With the default policy, peers learn each other's IP addresses. Relay-only
exposes only the relay's address, at the cost of relaying everything:

```go
ICETransportPolicy: pipe.ICETransportPolicyRelay,
```

## What pipe never logs

TURN credentials, SDP, ICE credentials, candidates, payloads, and signaling
tokens, at any level. Metric labels never carry peer or session IDs. Keep your
own code the same: log `conn.RemoteAddr()`, not what you read, and never log
`pipe.Signal` payloads or signaling request headers.

## Built-in bounds

| Surface | Bound |
| --- | --- |
| Inbound sessions negotiating or waiting in `Accept` | `Config.AcceptBacklog` (64); excess rejected as `busy` |
| Unwanted callers | `Config.AllowPeer`, before any allocation |
| Signaling envelope | `pipe.MaxEnvelopeSize` (256 KiB) |
| SDP | `pipe.MaxSDPSize` (128 KiB) |
| One ICE candidate | `pipe.MaxCandidateSize` (8 KiB) |
| Peer ID | `pipe.MaxPeerIDLength` (128 bytes) |
| Candidates buffered before the remote description | 256 per session |
| Duplicate suppression | 4096 recent signal IDs per endpoint |
| Stream frame | `Config.FramePayload` (16 KiB, ceiling 256 KiB) |
| Unread data per connection | `Config.ReadBuffer` (1 MiB), then backpressure |
| Signals queued per peer on `sse` | `sse.Config.QueueSize` (256) |
| Known peers on `sse` | `sse.Config.MaxPeers` (unlimited: set it) |
| Relay sockets per user | `relay.Plan.MaxAllocations` |
| Relay bandwidth per socket | `relay.Plan.Rate` |

Not limited inside the library: dials per second per peer and signals per
second per peer. Enforce those at your proxy or in your authenticator.

## Checklist

- [ ] Signaling uses `StaticTokens` or your own `Authenticator`, never `TrustPeerHeader`
- [ ] Tokens are random, one per device, out of source control and logs
- [ ] Signaling is behind TLS; no `WriteTimeout`; `ReadHeaderTimeout` set
- [ ] `sse.Config.MaxPeers` is set
- [ ] Listeners that should not accept everyone set `AllowPeer`
- [ ] Relay uses `AuthSecret` with a TTL of hours, not `admin`/`admin`
- [ ] Relay has `MinPort`/`MaxPort`, and the firewall opens only 3478 and that range
- [ ] Relay cannot send into private address space
- [ ] Every plan sets `MaxAllocations`, and untrusted plans set a `Rate`
- [ ] `RelayIP` is the public address
- [ ] Connections that need cryptographic identity run mTLS over the pipe
