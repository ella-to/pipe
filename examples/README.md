# Examples

Every example is runnable from a clean checkout. `echo`, `turnserver`,
`turnclient`, and `turncred` need no network access, no Docker, and no
privileges; `signaling` and `pipecat` are the two-machine story and need a
port you can reach.

| Example | What it shows |
| --- | --- |
| [`echo`](echo) | The whole API: dial by peer name, `net.Conn` both ways, one process. |
| [`signaling`](signaling) | The HTTP signaling server that peers on different machines use. |
| [`pipecat`](pipecat) | netcat over pipe: stdin/stdout of two machines joined by a connection. |
| [`turnserver`](turnserver) | A STUN + TURN server with per-user throughput plans and quotas. |
| [`turnclient`](turnclient) | A pipe connection forced through TURN, measured. |
| [`turncred`](turncred) | Mints ephemeral TURN credentials from a shared secret. |
| [`docker`](docker) | Dockerfile, compose stack, and a coturn configuration. |

The [guides](../guides/README.md) explain the ideas behind these; this file is
the reference for the commands.

## echo

```sh
go run ./examples/echo
```

Two endpoints in one process, signaling through the in-process hub, exchanging
lines of text. No SDP, no ICE, no data channels appear in the code.

## signaling

The signaling server from [`signaling/sse`](../signaling/sse) as a command.
Peers receive signals over a Server-Sent Events stream and send them with POST;
the server authenticates every request and only routes envelopes, never
application data.

```sh
# Local development: believe whatever peer ID a client claims.
go run ./examples/signaling -insecure-trust-peer-header

# Anything else: one bearer token per peer ID.
export PIPE_SIGNAL_TOKENS="alice=$(openssl rand -hex 24),bob=$(openssl rand -hex 24)"
go run ./examples/signaling -listen :8080
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:8080` | TCP address to serve on. |
| `-path` | `/pipe` | Path the handler is mounted at; clients use `http://host:port/pipe`. |
| `-tokens` | | `peer=token` list, or `PIPE_SIGNAL_TOKENS`. Tokens need 16+ characters. |
| `-insecure-trust-peer-header` | off | Trust the `X-Pipe-Peer` header without a token. Local development only. |
| `-max-peers` | `0` (unlimited) | Bound on concurrently known peers. |
| `-offline-grace` | `30s` | How long a disconnected peer keeps its queue. |
| `-tls-cert`, `-tls-key` | | Serve HTTPS directly instead of behind a proxy. |
| `-v` | off | Debug logging. |

`GET /healthz` reports `ok peers=N`.

## pipecat

```sh
# Machine A
go run ./examples/pipecat -signal http://signal.example:8080/pipe -token "$BOB_TOKEN" -id bob listen > received.bin

# Machine B
go run ./examples/pipecat -signal http://signal.example:8080/pipe -token "$ALICE_TOKEN" -id alice dial bob < file.bin
```

Whatever one side writes, the other reads. The end of stdin closes the
connection and the peer reads EOF; `-hold` keeps the connection open after
stdin ends until the peer closes, for request-and-reply use.

| Flag | Env | Meaning |
| --- | --- | --- |
| `-signal` | `PIPE_SIGNAL_URL` | Signaling server URL. |
| `-token` | `PIPE_SIGNAL_TOKEN` | Bearer token. |
| `-id` | `PIPE_ID` | This peer's ID. |
| `-stun` | `PIPE_STUN` | STUN URL. |
| `-turn`, `-turn-user`, `-turn-pass` | `PIPE_TURN`, `PIPE_TURN_USERNAME`, `PIPE_TURN_PASSWORD` | TURN URL and credentials. |
| `-relay-only` | | Use relay candidates only; proves the relay works. |
| `-allow` | | Comma-separated peer IDs allowed to connect when listening. |
| `-hold` | | Keep reading after stdin ends until the peer closes. |
| `-keepalive` | | Probe interval, default 15s; `0` disables. |
| `-timeout` | | Dial timeout, default 60s. |
| `-v` | | Debug logging to stderr. |

It prints one line to stderr on connect with the candidate types used, for
example `via relay/relay` or `via host/host`.

## turnserver

A STUN and TURN server built on `pion/turn`. One UDP listener answers both:
STUN Binding requests need no credentials, TURN Allocate requests do. An
optional TCP listener serves clients whose networks block UDP.

```sh
# Local development server: admin/admin on 127.0.0.1:3478.
go run ./examples/turnserver

# Plans: free users get 512 KiB/s per relay socket per direction and 4 sockets,
# paid users 8 MiB/s and 32 sockets. Users pick their plan with :plan.
go run ./examples/turnserver -plans 'free=512KiB/4,paid=8MiB/32' \
    -users 'alice=alice-secret:free,bob=bob-secret:paid' -stats 5s

# Ephemeral credentials from a shared secret instead of a user list. The plan
# rides in the user ID as name@plan; see turncred.
export PIPE_TURN_SECRET=$(openssl rand -hex 32)
go run ./examples/turnserver -plans 'free=512KiB/4,paid=8MiB/32'

# A real deployment: public address, fixed relay port range, real realm.
go run ./examples/turnserver -listen 0.0.0.0:3478 -listen-tcp 0.0.0.0:3478 \
    -relay-ip 203.0.113.10 -relay-ports 49152-49352 -realm relay.example.net
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen` | `127.0.0.1:3478` | UDP address for STUN and TURN. |
| `-listen-tcp` | | Optional TCP address for TURN over TCP. |
| `-realm` | `pipe.example` | TURN realm; the client must match it. |
| `-users` | `admin=admin` | `user=password[:plan]` list, or `PIPE_TURN_USERS`. |
| `-auth-secret` | | Shared secret for ephemeral credentials, or `PIPE_TURN_SECRET`. |
| `-plans` | | `name=rate[/maxallocations]` list, e.g. `free=512KiB/4,paid=8MiB/32`. |
| `-relay-ip` | listen address | Relay address advertised to clients. Required behind NAT. |
| `-relay-ports` | kernel picks | Inclusive UDP range for relay sockets, e.g. `49152-49352`. |
| `-rate` | `0` (unlimited) | Default plan: bytes/second per relay socket per direction. |
| `-burst` | derived | Default plan: token bucket depth; 0.1 s of `-rate`, minimum 64 KiB. |
| `-max-delay` | `20ms` | How long a packet may be held for budget before it is dropped. |
| `-max-allocations` | `0` (unlimited) | Default plan: concurrent relay sockets per user. |
| `-stats` | off | Interval for logging traffic counters, total and per user. |
| `-v` | off | Debug logging. |

`admin/admin` is a convenience for a server bound to loopback. Anything
reachable from a network needs real credentials, a real realm, and a real relay
address. Passwords are converted to the RFC 5389 key digest at startup and are
never logged.

### How the throughput budget works

A plan's rate puts a token bucket on every relay socket the server allocates
for that user, in each direction:

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

# Compare two plans. The plan is chosen by the @plan suffix of the user ID.
go run ./examples/turnclient -plans 'free=512KiB,paid=8MiB' -user alice@free -bytes 1MiB
go run ./examples/turnclient -plans 'free=512KiB,paid=8MiB' -user alice@paid -bytes 1MiB

# Ephemeral credentials minted on the fly and verified by the embedded server.
go run ./examples/turnclient -auth-secret "$(openssl rand -hex 32)" -user bob@paid -plans 'free=512KiB,paid=8MiB'

# Use a relay you started separately (or coturn, or a cloud TURN service).
go run ./examples/turnclient -embedded=false \
	-turn 'turn:127.0.0.1:3478?transport=udp' -user admin -pass admin
```

Credentials fall back to `PIPE_TURN_USERNAME` and `PIPE_TURN_PASSWORD` when
the flags are empty.

Output from a free-plan run:

```
turn:        turn:127.0.0.1:54666?transport=udp
user:        alice@free (realm pipe.example)
policy:      relay
plan free:   512.0KiB/s
plan paid:   8.0MiB/s
transfer:    1.0MiB (echoed, so every byte crosses the relay four times)

connected in 9ms
transferred  1.0MiB round trip in 12.752s
throughput   80.3KiB/s each way
state        connected, candidates relay/relay, read 1.0MiB, written 1.0MiB

relay        allocations=2 active=2 sent=2.2MiB/2729pkt received=2.2MiB/2729pkt dropped=330.1KiB/292pkt delayed=7pkt
user alice@free plan=free allocations=2 active=2 sent=2.2MiB received=2.2MiB dropped=330.1KiB/292pkt rejected=0
```

`candidates relay/relay` is the part that matters: `-relay-only` (the default)
gathers relay candidates only, so ICE cannot quietly pick the direct host path
and report a success that never touched TURN.

### Reading the numbers honestly

Application throughput is **not** equal to the plan rate, and the example says so.
The transfer is an echo, so every byte crosses the relay four times — out through
the client's allocation, in through the server's, and again on the way back — and
each crossing is metered separately. On top of that, dropped datagrams make SCTP
back off. With a 512 KiB/s plan a 1 MiB round trip measures roughly 60 to
100 KiB/s each way on loopback; the relay counters in the same output show the
2.2 MiB that was actually metered and the few hundred KiB that were dropped.
A one-way transfer gets much closer to the cap.

Numbers from this example describe loopback on one machine. They are a
demonstration of the mechanism, not a capacity claim.

## turncred

```sh
export PIPE_TURN_SECRET=...
go run ./examples/turncred -user alice@free -ttl 12h
go run ./examples/turncred -user alice@paid -ttl 1h -json -turn 'turn:relay.example.net:3478?transport=udp'
```

Prints a username of the form `<unix expiry>:<user id>` and the matching
password, which any server configured with the same secret accepts until the
expiry: the example `turnserver -auth-secret`, or coturn with
`use-auth-secret`. `-json` prints an object shaped like a WebRTC ICE server
entry for handing to a client.

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
			Username:   os.Getenv("PIPE_TURN_USERNAME"),
			Credential: os.Getenv("PIPE_TURN_PASSWORD"),
		},
	},
	// Optional: refuse to connect except through TURN.
	ICETransportPolicy: pipe.ICETransportPolicyRelay,
})
```

Signaling is a separate concern from STUN and TURN. STUN and TURN find a network
path between two peers; signaling is how those peers exchange the descriptions
that make the path possible. `echo` and `turnclient` use `signaling/memory`
because both peers are in one process; `pipecat` uses `signaling/sse` because
they are not.
