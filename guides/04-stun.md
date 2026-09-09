# STUN: letting peers discover their public address

This guide is for anyone whose two pipe peers are on different networks and
who wants them to connect directly whenever the NATs in between allow it. It
explains what STUN contributes, when it is enough on its own, how to run or
choose a STUN server, how pipe is configured to use one, and how to verify
that it actually did something. It assumes you have read
[01-concepts.md](01-concepts.md).

## What STUN does

A peer behind a home router has a private address such as `192.168.1.20`.
That address is useless to a peer on another network. STUN (Session
Traversal Utilities for NAT, RFC 8489) is a tiny request and response
protocol: the peer sends a Binding request to a server on the public
internet, and the server replies with the source address and port it saw the
request come from. That is the peer's public `ip:port` as the rest of the
internet sees it, and ICE advertises it as a **server-reflexive** (`srflx`)
candidate.

If both peers do this and exchange the results through signaling, each can
send packets to the other's public address. Most consumer NATs then let the
reply traffic through because the peer already sent something outward from
that port. This is hole punching. Nothing is relayed; the STUN server is not
in the data path and hears nothing after the Binding response.

The STUN server sees only a couple of small UDP packets per connection
attempt. A single tiny instance handles an enormous number of peers.

## When STUN is enough, and when it is not

Whether hole punching works depends on how the NATs on both sides map ports.

| Situation | Direct path with STUN |
| --- | --- |
| Both peers on the same LAN | Not needed; `host` candidates connect. |
| Home routers, typical mobile hotspots (endpoint-independent mapping) | Usually works. |
| One side behind a symmetric NAT (a fresh external port per destination) | Often works, because the other side's mapping is stable. |
| Both sides behind symmetric NATs | Almost never works. |
| Corporate or carrier-grade NAT with strict filtering | Frequently fails. |
| Network blocks outbound UDP to arbitrary hosts (hotels, some offices) | Fails; even the STUN request may be dropped. |

In practice, for two arbitrary consumers on the internet, STUN alone
succeeds somewhere around 80 to 90 percent of the time. The remainder needs
a TURN relay, which is the subject of [05-turn-relay.md](05-turn-relay.md).
STUN is not an alternative to TURN; it is the cheap first attempt that makes
the relay rarely necessary. Configure both, and ICE picks the direct path
whenever it exists.

## How pipe uses STUN

A STUN server is one entry in `Config.ICEServers` with no credentials:

```go
package main

import (
	"context"
	"log"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	ctx := context.Background()

	ep, err := pipe.New(ctx, pipe.Config{
		ID:       "alice",
		Signaler: &sse.Client{URL: "https://signal.example.net/pipe", Token: "..."},
		ICEServers: []pipe.ICEServer{
			{URLs: []string{"stun:stun.example.net:3478"}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ep.Close()

	// ep.Dial / ep.Listen as usual; ICE gathers host and srflx candidates.
}
```

Pipe validates every `ICEServer` in `pipe.New` and rejects a configuration
with `pipe.ErrConfig` when:

- a URL does not use the `stun`, `stuns`, `turn`, or `turns` scheme, or has
  nothing after the scheme;
- a server whose URLs are all STUN carries a `Username` or `Credential`
  ("STUN URLs must not carry credentials");
- a server with a TURN URL lacks a `Username` or `Credential`;
- `ICETransportPolicyRelay` is set but no TURN URL is configured.

A STUN-only entry therefore looks exactly like the example above: just
`URLs`. If you want to hand the same host out as both STUN and TURN, use two
entries, one without credentials for `stun:` and one with credentials for
`turn:`, as in the `examples/pipecat` command:

```go
servers := []pipe.ICEServer{
	{URLs: []string{"stun:relay.example.net:3478"}},
	{
		URLs:       []string{"turn:relay.example.net:3478?transport=udp"},
		Username:   os.Getenv("PIPE_TURN_USERNAME"),
		Credential: os.Getenv("PIPE_TURN_PASSWORD"),
	},
}
```

With an empty `ICEServers` list only `host` candidates are gathered, which is
fine for tests and for peers that share a network.

### URL syntax

```
stun:host[:port]
stuns:host[:port]
```

- `port` defaults to 3478 for `stun:` and 5349 for `stuns:`.
- `stuns:` is STUN over TLS over TCP. Pipe accepts the scheme and hands it to
  Pion. It is rarely useful for pipe: the point of STUN is to learn the UDP
  mapping that the data path will use, and a TCP-based query reports a
  different mapping. Prefer plain `stun:` over UDP, and use TURN over TCP or
  TLS for UDP-hostile networks.
- Several URLs may share one `ICEServer` entry; ICE tries each of them.

## Running your own STUN server

### The example TURN server answers STUN

Any TURN server is also a STUN server: the TURN protocol is an extension of
STUN and both listen on the same port. The `examples/turnserver` command in
this repository serves STUN Binding requests on its UDP listener without
requiring credentials, so if you run a relay you already have STUN:

```sh
go run ./examples/turnserver -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 \
    -relay-ports 49152-49252 -users "alice=$(openssl rand -hex 16)"
```

It prints the URL to paste into clients:

```
stun:  stun:0.0.0.0:3478
turn:  turn:0.0.0.0:3478?transport=udp
realm: pipe.example
users: alice
```

Replace `0.0.0.0` with the server's public name or address when you hand the
URL to clients. Every other flag is documented in
[05-turn-relay.md](05-turn-relay.md). The example listens on IPv4 UDP only.

For a STUN-only deployment you still need a user list or a secret, because
the command refuses to start with neither. Give it a throwaway user; STUN
Binding requests never authenticate, and nobody can allocate a relay without
the password:

```sh
go run ./examples/turnserver -listen 0.0.0.0:3478 -relay-ip 203.0.113.10 \
    -users "nobody=$(openssl rand -hex 32)"
```

### coturn

coturn is the widely deployed production TURN server, and it answers STUN
with the same binary. A minimal STUN-only configuration:

```
# /etc/turnserver.conf
listening-port=3478
external-ip=203.0.113.10
stun-only
no-cli
fingerprint
log-file=stdout
```

`stun-only` disables relaying entirely, so no credentials are needed. Drop it
and add authentication when you want TURN as well; see the annotated
configuration in [05-turn-relay.md](05-turn-relay.md) and
[08-docker.md](08-docker.md).

### stunserver and others

Any RFC 5389 or 8489 compliant server works. `stunserver` from the Stuntman
project is a small C++ daemon that does nothing else; `pion/turn` can be
embedded in your own Go program if you want STUN inside an existing service.
Pipe does not care which one you use.

### Firewall

The server needs one inbound rule:

| Direction | Protocol | Port | Purpose |
| --- | --- | --- | --- |
| Inbound to server | UDP | 3478 | STUN Binding requests (and TURN, if enabled) |

For `stuns:` add TCP 5349 and a certificate. Clients need no inbound rules;
that is the whole point.

## Public STUN servers

Several organizations run STUN servers that anyone may use, for example
`stun:stun.l.google.com:19302`. They are convenient for development and for
personal projects with a handful of peers.

Tradeoffs:

- **Privacy.** Every peer sends its public address to the operator at every
  connection attempt. Nothing else is disclosed; STUN carries no payload, no
  peer IDs, and no information about who the peer is trying to reach. If that
  is unacceptable for your users, run your own; it costs almost nothing.
- **Availability.** A public server can change address, rate-limit you, or
  disappear. There is no contract. If ICE cannot reach the STUN server it
  simply gathers no `srflx` candidate and connections that needed one fail or
  fall back to TURN.
- **Latency.** Gathering waits for the STUN response. A server on another
  continent adds a round trip to every connection setup. Pion also holds a
  working reflexive pair for 500 ms before nominating it, in case a host pair
  shows up, so the server's distance is not the whole story.

A common arrangement: list your own TURN server's `stun:` URL first and a
public STUN server second, so that peers still get a reflexive candidate when
your relay is down.

## Verifying that STUN worked

The proof is in the selected candidate pair. `pipe.Conn.Stats()` reports the
local and remote candidate types of the pair ICE nominated:

```go
st := conn.Stats()
fmt.Println(st.LocalCandidate, st.RemoteCandidate) // e.g. srflx srflx, or host srflx
```

The values are `host`, `srflx`, `prflx`, `relay`, or the empty string
(`pipe.CandidateUnknown`) before a pair is selected.

### With pipecat

`examples/pipecat` prints the pair on stderr when the connection comes up.
Start a signaling server (see [03-signaling-server.md](03-signaling-server.md)),
then on two machines on different networks:

```sh
# machine A
pipecat -signal https://signal.example.net/pipe -token "$BOB_TOKEN" -id bob \
    -stun stun:stun.example.net:3478 listen

# machine B
echo hello | pipecat -signal https://signal.example.net/pipe -token "$ALICE_TOKEN" -id alice \
    -stun stun:stun.example.net:3478 dial bob
```

On success each side prints a line such as:

```
pipecat: connected to bob in 412ms via srflx/srflx
```

`srflx` on either side means STUN did its job. `host/host` means the two
machines could reach each other directly (same LAN, or one has a public
address). `prflx` means a working address was learned from the connectivity
checks themselves, which also implies the NATs cooperated.

If instead you see a dial timeout, the NATs did not cooperate and you need a
relay; try the same command with `-turn`, `-turn-user`, and `-turn-pass`.

### With turnclient

`examples/turnclient` runs both peers in one process, so it cannot cross a
NAT, but it is a quick way to confirm that a STUN server answers at all:

```sh
go run ./examples/turnclient -embedded=false -relay-only=false \
    -stun stun:stun.example.net:3478 \
    -turn 'turn:relay.example.net:3478?transport=udp' -user alice -pass "$PASS" -bytes 64KiB
```

With `-relay-only=false` ICE is allowed every candidate type. On one machine
the two peers will nominate `host/host`, which is expected; the interesting
part is the debug log with `-v`, where gathered candidates are trickled
through signaling. A `srflx` candidate appearing means the STUN query
succeeded. A TURN URL is still required by the command because it is a relay
test tool at heart.

### With a generic client

Any STUN client can query the server directly, which separates "the server
is down" from "pipe is misconfigured". For example, with the `stunclient`
tool from Stuntman:

```sh
stunclient stun.example.net 3478
```

It prints the mapped address the server observed, or an error.

## Troubleshooting

**No `srflx` candidate, connections only work on the LAN.**
The STUN server is unreachable or not answering. Check that UDP 3478 is open
on the server and that the URL has no typo. Pipe does not fail `New` or
`Dial` when a STUN server is down; ICE simply gathers fewer candidates.
Enable a `Logger` at debug level to see the candidates being trickled.

**`pipe.New` fails with "STUN URLs must not carry credentials".**
You put a `Username` or `Credential` on an entry whose URLs are all `stun:`.
Split the entry: one for STUN without credentials, one for TURN with them.

**`srflx` candidates exist but the connection still times out.**
Both peers learned their public addresses, but at least one NAT does not let
the other's packets in. This is the case STUN cannot solve. Add a TURN server;
ICE will fall back to it automatically when the direct pairs fail their
checks.

**The reflexive address is a private address.**
The STUN server is on the same private network as the peer, or a VPN
intercepts the route. Use a server on the public internet.

**Connections take half a second longer than expected.**
Pion waits 500 ms before nominating a reflexive pair so that a host pair can
win if one appears. This is deliberate and usually right. If your peers never
share a LAN and you want to trade that for latency, lower the wait through
`Config.Pion.ConfigureSettingEngine` with `SetSrflxAcceptanceMinWait`; see
[09-client-api.md](09-client-api.md).

**A public STUN server stopped answering.**
Public servers change without notice. Run your own; the example server or
coturn takes minutes to set up, as shown above.

## Summary

- STUN tells a peer its public address so that ICE can try a direct path.
- It works for most home networks and fails for symmetric NATs and
  UDP-blocking networks; TURN covers the rest.
- Configure it as an `ICEServer` with a `stun:` URL and no credentials.
- Any TURN server, including `examples/turnserver` and coturn, is also a STUN
  server on the same port.
- Verify with the candidate types in `Conn.Stats()` or pipecat's `via` line.
