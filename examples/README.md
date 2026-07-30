# Examples

Every example is runnable from a clean checkout with no network access, no
Docker, and no privileges.

| Example | What it shows |
| --- | --- |
| [`echo`](echo) | The whole API: dial by peer name, `net.Conn` both ways. |
| [`turnserver`](turnserver) | A STUN + TURN server with a throughput budget. |
| [`turnclient`](turnclient) | A pipe connection forced through TURN, measured. |

## echo

```sh
go run ./examples/echo
```

Two endpoints in one process, signaling through the in-process hub, exchanging
lines of text. No SDP, no ICE, no data channels appear in the code.

## turnserver

A STUN and TURN server built on `pion/turn`. One UDP listener answers both: STUN
Binding requests need no credentials, TURN Allocate requests use long-term
credentials.

```sh
# Local development server: admin/admin on 127.0.0.1:3478.
go run ./examples/turnserver

# Meter the relay to 512 KiB/s per socket per direction and log counters.
go run ./examples/turnserver -rate 512KiB -stats 2s

# Real credentials come from the environment, not from source control.
TUNNEL_TURN_USERS="alice=$(openssl rand -hex 16)" go run ./examples/turnserver
```

It prints the URLs to paste into a client:

```
stun: stun:127.0.0.1:3478
turn: turn:127.0.0.1:3478?transport=udp
realm: pipe.example
users: admin
```

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:3478` | UDP address for STUN and TURN. |
| `-realm` | `pipe.example` | TURN realm; the client must match it. |
| `-users` | `admin=admin` | `user=password` list, or `TUNNEL_TURN_USERS`. |
| `-relay-ip` | listen address | Relay address advertised to clients. Required behind NAT. |
| `-rate` | `0` (unlimited) | Bytes/second per relay socket per direction. |
| `-burst` | derived | Token bucket depth; defaults to 0.1 s of `-rate`, minimum 64 KiB. |
| `-max-delay` | `20ms` | How long a packet may be held for budget before it is dropped. |
| `-stats` | off | Interval for logging traffic counters. |

`admin/admin` is a convenience for a server bound to loopback. Anything
reachable from a network needs real credentials, a real realm, and a real relay
address. Passwords are converted to the RFC 5389 key digest at startup and are
never logged.

### How the throughput budget works

`-rate` puts a token bucket on every relay socket the server allocates, in each
direction:

- **Client → peer** is policed. This path runs on the goroutine that serves every
  client of the listener, so it must never block: datagrams over budget are
  dropped.
- **Peer → client** is shaped. This path has its own goroutine per allocation, so
  a datagram may be held up to `-max-delay` to stay inside the budget, and is
  dropped only if it would need longer.

The congestion is therefore real. SCTP inside the pipe sees delay and loss and
backs off the way it would on a genuinely slow link — which is the point. A knob
that only reported a number would not tell you anything about how your
application behaves.

## turnclient

Moves data through a pipe connection that cannot avoid the relay, and reports
what it measured.

```sh
# Self-contained: starts its own relay, transfers 4 MiB, prints the result.
go run ./examples/turnclient

# Squeeze the relay.
go run ./examples/turnclient -rate 512KiB -bytes 1MiB

# Use a relay you started separately (or coturn, or a cloud TURN service).
go run ./examples/turnclient -embedded=false \
	-turn 'turn:127.0.0.1:3478?transport=udp' -user admin -pass admin
```

Credentials fall back to `TUNNEL_TURN_USERNAME` and `TUNNEL_TURN_PASSWORD` when
the flags are empty.

Output from an unmetered run:

```
turn:        turn:127.0.0.1:63134?transport=udp
policy:      relay
relay rate:  unlimited
transfer:    2.0MiB (echoed, so every byte crosses the relay four times)

connected in 2.013s
transferred  2.0MiB round trip in 63ms
throughput   31.7MiB/s each way
state        connected, candidates relay/relay, read 2.0MiB, written 2.0MiB
relay        allocations=2 sent=4.3MiB/5210pkt received=4.3MiB/5210pkt dropped=0B/0pkt
```

`candidates relay/relay` is the part that matters: `-relay-only` (the default)
gathers relay candidates only, so ICE cannot quietly pick the direct host path
and report a success that never touched TURN.

### Reading the numbers honestly

Application throughput is **not** equal to `-rate`, and the example says so.
The transfer is an echo, so every byte crosses the relay four times — out through
the client's allocation, in through the server's, and again on the way back — and
each crossing is metered separately. On top of that, dropped datagrams make SCTP
back off. With `-rate 512KiB` a 1 MiB round trip measures roughly 60–100 KiB/s
each way on loopback; the relay counters in the same output show the ~2.2 MiB
that was actually metered and the ~0.4 MiB that was dropped.

Numbers from this example describe loopback on one machine. They are a
demonstration of the mechanism, not a capacity claim.

## Configuring a real STUN/TURN service

The client code is the same whichever server you use:

```go
ep, err := pipe.New(ctx, pipe.Config{
	ID:       "alice",
	Signaler: mySignaler,
	ICEServers: []pipe.ICEServer{
		{URLs: []string{"stun:stun.example.net:3478"}},
		{
			URLs:       []string{"turn:turn.example.net:3478?transport=udp"},
			Username:   os.Getenv("TUNNEL_TURN_USERNAME"),
			Credential: os.Getenv("TUNNEL_TURN_PASSWORD"),
		},
	},
	// Optional: refuse to connect except through TURN.
	ICETransportPolicy: pipe.ICETransportPolicyRelay,
})
```

Signaling is a separate concern from STUN and TURN. STUN and TURN find a network
path between two peers; signaling is how those peers exchange the descriptions
that make the path possible. These examples use `signaling/memory` because both
peers are in one process.
