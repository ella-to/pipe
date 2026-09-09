# Quickstart: two machines talking in ten minutes

This guide is for someone who has read nothing else and wants two programs on
two different machines to exchange bytes through pipe. It uses the example
commands shipped in this repository, then shows the same thing with the Go API,
and ends with what to do when the connection does not come up. The background
for every step is in [01-concepts.md](01-concepts.md).

## What you will run

```
        machine A (home)                  a VM with a public address                machine B (office)
   +----------------------+           +-----------------------------+           +----------------------+
   | pipecat listen       |  HTTPS    | examples/signaling  :8080   |   HTTPS   | pipecat dial alice   |
   |   -id alice          |<--------->|  routes offers, answers,    |<--------->|   -id bob            |
   |                      |           |  candidates between peers   |           |                      |
   +----------+-----------+           +-----------------------------+           +-----------+----------+
              |                                                                             |
              |                       +-----------------------------+                       |
              |     UDP               | examples/turnserver :3478   |          UDP          |
              +======================>|  STUN (find my address)     |<======================+
              |                       |  TURN (relay when direct    |                       |
              |                       |        fails)               |                       |
              |                       +-----------------------------+                       |
              |                                                                             |
              +============================ your bytes (direct, when possible) =============+
```

Three pieces:

1. **A signaling server.** Small HTTP service, carries a few KiB per
   connection. Runs on any machine both peers can reach.
2. **The two peers.** Start with `pipecat`, a netcat-like example. Later, your
   own Go program.
3. **STUN and TURN**, only when a direct path does not exist. On one LAN you
   need neither. Across the internet you almost always need STUN, and you need
   TURN for the networks where hole punching fails.

## Step 0: build the examples

From the repository root:

```sh
go build -o bin/ ./examples/...
ls bin/
# echo  pipecat  signaling  turnclient  turncred  turnserver
```

Or run each one in place with `go run ./examples/<name>`. The rest of this
guide uses `bin/…`.

Copy `bin/pipecat` to both machines (build it there, or cross-compile with
`GOOS`/`GOARCH`). Copy `bin/signaling` to the machine that will host
signaling.

## Step 1: run the signaling server

### First try: everything on one machine, no authentication

To see it work before dealing with tokens, run the signaling server in a mode
that believes whatever peer ID a client claims. This is for one machine and a
few minutes, never for a network you do not control:

```sh
bin/signaling -listen 127.0.0.1:8080 -insecure-trust-peer-header
```

Output:

```
signaling: http://127.0.0.1:8080/pipe
health:    http://127.0.0.1:8080/healthz
```

In two other terminals:

```sh
# terminal 2
bin/pipecat -signal http://127.0.0.1:8080/pipe -id alice listen

# terminal 3
echo hello from bob | bin/pipecat -signal http://127.0.0.1:8080/pipe -id bob dial alice
```

Terminal 2 prints `hello from bob` and both commands exit. Both peers found
each other over host candidates on loopback; a dial like that completes in
about 7 ms.

### For real: bearer tokens

Each peer ID gets its own token. Generate them, then start the server on the
machine with a public address:

```sh
export PIPE_SIGNAL_TOKENS="alice=$(openssl rand -hex 24),bob=$(openssl rand -hex 24)"
echo "$PIPE_SIGNAL_TOKENS"       # copy each token to its peer, out of band
bin/signaling -listen :8080
```

Tokens must be at least 16 characters, and no two peers may share one. The
server refuses to start without either `-tokens`, `PIPE_SIGNAL_TOKENS`, or
`-insecure-trust-peer-header`.

Flags of `examples/signaling`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to serve HTTP on. |
| `-path` | `/pipe` | URL path the signaling handler is mounted at. |
| `-tokens` | | Comma-separated `peer=token` list; `PIPE_SIGNAL_TOKENS` overrides it. |
| `-insecure-trust-peer-header` | `false` | Believe the `X-Pipe-Peer` header without a token. Local development only. |
| `-max-peers` | `0` | Bound on concurrently known peers; 0 is unlimited. |
| `-offline-grace` | `30s` | How long a disconnected peer keeps its queue before it is forgotten. |
| `-tls-cert`, `-tls-key` | | PEM files; with both set, the server speaks HTTPS itself. |
| `-v` | `false` | Debug logging. |

Put TLS in front of it before it leaves your network. Either hand it a
certificate with `-tls-cert`/`-tls-key`, or run it behind Caddy or nginx; the
proxy settings that matter are in [03-signaling-server.md](03-signaling-server.md).
Bearer tokens are only as secret as the transport that carries them.

Check it is up from another machine:

```sh
curl https://signal.example.net/healthz
# ok peers=0
```

## Step 2: pipecat on two machines

On machine A (alice listens):

```sh
export PIPE_SIGNAL_URL=https://signal.example.net/pipe
export PIPE_SIGNAL_TOKEN=<alice's token>
bin/pipecat -id alice listen
# pipecat: listening as alice
```

On machine B (bob dials alice):

```sh
export PIPE_SIGNAL_URL=https://signal.example.net/pipe
export PIPE_SIGNAL_TOKEN=<bob's token>
echo hello from bob | bin/pipecat -id bob dial alice
```

Machine A prints the line, and both sides report how they connected:

```
pipecat: connected to bob in 41ms via host/host
```

The time is illustrative; on a LAN expect tens of milliseconds.

`host/host` means a direct path between local interface addresses, which works
on one LAN. Across two home networks you will see `srflx` (found with STUN) or
`relay` (through TURN) instead; those need Step 4.

### Send a file

pipecat behaves like `nc -N`: when stdin ends it closes the connection, and
pipe's close waits for the peer to acknowledge everything written, so the
whole file arrives before the receiver sees EOF.

```sh
# machine A
bin/pipecat -id alice listen > backup.tar.zst

# machine B
bin/pipecat -id bob dial alice < backup.tar.zst
```

Compare checksums on both sides when you are done. For a request and reply
exchange, where the dialer must keep reading after its own input ends, add
`-hold` on the dialing side.

### Only let known peers in

Anyone with a valid signaling token can dial `alice`. To accept only `bob`:

```sh
bin/pipecat -id alice -allow bob listen
```

Another authenticated peer dialing alice sees:

```
pipecat: pipe: peer alice rejected the session: unauthorized (peer is not allowed to connect)
```

Flags of `examples/pipecat`:

| Flag | Env var | Default | Meaning |
| --- | --- | --- | --- |
| `-signal` | `PIPE_SIGNAL_URL` | | Signaling server URL. Required. |
| `-token` | `PIPE_SIGNAL_TOKEN` | | Bearer token for the signaling server. |
| `-id` | `PIPE_ID` | | This peer's ID. Required. |
| `-stun` | `PIPE_STUN` | | STUN URL, e.g. `stun:stun.example.net:3478`. |
| `-turn` | `PIPE_TURN` | | TURN URL, e.g. `turn:relay.example.net:3478?transport=udp`. |
| `-turn-user` | `PIPE_TURN_USERNAME` | | TURN username. |
| `-turn-pass` | `PIPE_TURN_PASSWORD` | | TURN password. |
| `-relay-only` | | `false` | Gather relay candidates only; the connection must go through TURN. |
| `-allow` | | | Comma-separated peer IDs allowed to connect when listening; empty allows all. |
| `-hold` | | `false` | After stdin ends, keep reading from the peer until it closes. |
| `-keepalive` | | `15s` | Keepalive probe interval; 0 disables. |
| `-timeout` | | `60s` | Dial timeout. |
| `-v` | | `false` | Debug logging to stderr. |

Modes: `pipecat [flags] listen` accepts one connection; `pipecat [flags] dial
<peer-id>` connects to it.

## Step 3: the same thing from Go

The signaling server stays as it is. Replace pipecat with two small programs.

### The listener

```go
// listener/main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	ctx := context.Background()

	ep, err := pipe.New(ctx, pipe.Config{
		ID: "alice",
		Signaler: &sse.Client{
			URL:   os.Getenv("PIPE_SIGNAL_URL"),
			Token: os.Getenv("PIPE_SIGNAL_TOKEN"),
		},
		// Only bob may connect. Nil would admit every authenticated peer.
		AllowPeer: func(peer pipe.PeerID) bool { return peer == "bob" },
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ep.Close()

	ln, err := ep.Listen()
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	log.Println("listening as", ep.LocalID())

	for {
		// AcceptConn returns *pipe.Conn; Accept returns the same connection
		// as a net.Conn for code that only knows net.Listener.
		conn, err := ln.AcceptConn()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Fatal(err)
		}
		go func() {
			defer conn.Close()
			log.Printf("connection from %s (%s/%s)", conn.PeerID(),
				conn.Stats().LocalCandidate, conn.Stats().RemoteCandidate)
			n, err := io.Copy(os.Stdout, conn)
			if err != nil && !errors.Is(err, io.EOF) {
				log.Println("read:", err)
			}
			fmt.Fprintf(os.Stderr, "\n%d bytes from %s\n", n, conn.PeerID())
		}()
	}
}
```

### The dialer

```go
// dialer/main.go
package main

import (
	"context"
	"io"
	"log"
	"os"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ep, err := pipe.New(ctx, pipe.Config{
		ID: "bob",
		Signaler: &sse.Client{
			URL:   os.Getenv("PIPE_SIGNAL_URL"),
			Token: os.Getenv("PIPE_SIGNAL_TOKEN"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ep.Close()

	conn, err := ep.Dial(ctx, "alice")
	if err != nil {
		log.Fatal(err)
	}
	// Close sends a close frame and, off this goroutine, waits for alice to
	// acknowledge everything written before the transport goes away.
	defer conn.Close()

	if _, err := io.Copy(conn, os.Stdin); err != nil {
		log.Fatal(err)
	}
}
```

Run them exactly like pipecat:

```sh
# machine A
PIPE_SIGNAL_URL=https://signal.example.net/pipe PIPE_SIGNAL_TOKEN=<alice> go run ./listener

# machine B
PIPE_SIGNAL_URL=https://signal.example.net/pipe PIPE_SIGNAL_TOKEN=<bob> \
  go run ./dialer < backup.tar.zst
```

`conn` is a `net.Conn`, so `bufio`, `encoding/json`, `net/http`, and
`crypto/tls` all work on it unchanged. [09-client-api.md](09-client-api.md)
goes through the API in detail.

## Step 4: when it does not connect

The symptom is always the same: the dial waits and then fails with
`pipe: negotiation with alice timed out after 30s` (or your `-timeout`). Both
peers exchanged offers and candidates through signaling fine; ICE found no pair
of addresses that could reach each other. Work through these in order.

### Add a STUN server

STUN tells each peer its public address. With it, peers behind ordinary home
NATs connect directly. Run your own (the TURN server below answers STUN too),
or use a public one for a first test:

```sh
# both machines
bin/pipecat ... -stun stun:stun.l.google.com:19302 listen
bin/pipecat ... -stun stun:stun.l.google.com:19302 dial alice
```

Success shows `srflx/srflx` or `srflx/host` in the connected line. Read
[04-stun.md](04-stun.md) for what STUN can and cannot do.

### Add a TURN server

When STUN is not enough (symmetric NAT, a firewall that blocks UDP to unknown
hosts, mobile carriers), a relay is the only way. Start one on a machine with a
public IP; it serves STUN and TURN from one UDP port:

```sh
# on the relay machine
bin/turnserver -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 \
    -relay-ports 49152-49352 -realm relay.example.net \
    -users "alice=$(openssl rand -hex 16),bob=$(openssl rand -hex 16)"
```

Open UDP 3478 and UDP 49152-49352 inbound on that machine. Then give both
peers the relay and their credentials:

```sh
bin/pipecat ... -stun stun:203.0.113.10:3478 \
    -turn 'turn:203.0.113.10:3478?transport=udp' -turn-user alice -turn-pass <pw> listen
```

ICE prefers direct paths and falls back to `relay` only when they fail, so a
working relay is invisible when it is not needed. To prove it works, force it:

```sh
bin/pipecat ... -turn 'turn:203.0.113.10:3478?transport=udp' -turn-user bob -turn-pass <pw> \
    -relay-only dial alice
# pipecat: connected to alice in 62ms via relay/relay   (time will vary)
```

`relay/relay` is the proof. With only relay candidates permitted, pipe skips
ICE's two-second wait for a better path, so a relayed dial on a fast link is
not noticeably slower than a direct one (about 6 ms on loopback).

Flags of `examples/turnserver`:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:3478` | UDP address to serve STUN and TURN on. |
| `-listen-tcp` | | Optional TCP address to serve TURN on as well. |
| `-realm` | `pipe.example` | TURN realm; clients must use the same. |
| `-users` | `admin=admin` | `user=password[:plan]` list; `PIPE_TURN_USERS` overrides it. |
| `-auth-secret` | | Shared secret for ephemeral credentials; `PIPE_TURN_SECRET` overrides it. |
| `-plans` | | `name=rate[/maxallocations]` list, e.g. `free=512KiB/4,paid=8MiB/32`. |
| `-relay-ip` | listen address | Address advertised to clients as their relay address. Required behind NAT. |
| `-relay-ports` | | Inclusive UDP port range for relay sockets, e.g. `49152-49352`. |
| `-rate` | `0` | Default plan: bytes per second per relay socket per direction; 0 is unlimited. |
| `-burst` | `0` | Default plan: token bucket depth; 0 derives it from `-rate`. |
| `-max-delay` | `20ms` | How long a relayed packet may be held for budget before it is dropped. |
| `-max-allocations` | `0` | Default plan: concurrent relay sockets per user; 0 is unlimited. |
| `-stats` | `0` | Interval for logging traffic counters; 0 disables. |
| `-v` | `false` | Debug logging. |

The relay is the part of the setup that costs bandwidth. Plans, quotas,
ephemeral credentials, and how to hand out relay access to free and paying
users are in [05-turn-relay.md](05-turn-relay.md) and
[07-multi-user-relay.md](07-multi-user-relay.md).

### Still stuck

- **`open signaling: sse: permanent failure: 401 Unauthorized`.** The token is
  wrong, or it belongs to a different peer ID than `-id`. Tokens and IDs are
  bound one to one.
- **`peer alice rejected the session: not_listening`.** Alice's endpoint is
  up but has no listener (pipecat exits after one connection; restart it).
- **`peer alice rejected the session: unauthorized`.** Alice's `-allow` list
  or `AllowPeer` does not include you.
- **`sse: send offer to alice: pipe: peer unavailable`.** Alice is not
  connected to the signaling server, or was forgotten after
  `-offline-grace`. Check `/healthz`; it reports how many peers are attached.
- **Connects on a LAN, times out across the internet.** You need STUN, and
  possibly TURN, as above. Run both peers with `-v` and look for candidate
  types in the debug log; if you only ever see `host`, STUN is not reachable.
- **TURN configured, `-relay-only` still times out.** UDP 3478 or the relay
  port range is not open, `-relay-ip` is not the public address, or the realm
  or credentials differ. `bin/turnclient -embedded=false -turn <url> -user
  <u> -pass <p>` tests a relay from one machine.

More diagnostics are in [10-operations.md](10-operations.md).

## Step 5: run it as a service

Once the pieces work by hand, [08-docker.md](08-docker.md) has a Dockerfile
and a compose file that run the signaling server and the relay together, and
[06-security.md](06-security.md) is the checklist to go through before the
setup faces the internet.
