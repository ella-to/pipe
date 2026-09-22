# pipe guides

`pipe` gives you a `net.Conn` between two programs identified by name, over
WebRTC. Peers find each other through a signaling server, connect directly
when they can, and fall back to a TURN relay when they cannot.

Every Go snippet is a complete program or a function you drop into the program
shown just before it. Set up a module once and run them from there:

```sh
mkdir hello-pipe && cd hello-pipe
go mod init hello-pipe
go get ella.to/pipe@latest
```

## Read in order

| Guide | You end up with |
| --- | --- |
| [01 Concepts](01-concepts.md) | The mental model in one page |
| [02 Quickstart](02-quickstart.md) | Two processes, then two machines, talking over pipe |
| [03 Signaling server](03-signaling-server.md) | Your own signaling server with real authentication |
| [04 STUN](04-stun.md) | Direct connections across home NATs |
| [05 TURN relay](05-turn-relay.md) | Your own relay: auth, UDP and TCP, free, pro, and unlimited tiers |

## Reference

| Guide | Covers |
| --- | --- |
| [06 Security](06-security.md) | Authentication, `AllowPeer`, mTLS over pipe, relay hardening, a checklist |
| [07 Multi-user relay](07-multi-user-relay.md) | Relay plus credentials API plus stats in one program, upgrades, revocation |
| [08 Docker](08-docker.md) | Containers for signaling and the relay |
| [09 Client API](09-client-api.md) | `Endpoint`, `Conn`, errors, deadlines, HTTP over pipe, metrics |
| [10 Operations](10-operations.md) | Metrics, logs, tuning, troubleshooting |
| [11 Protocol](11-protocol.md) | Signaling envelope and stream frames, for other implementations |

## Packages

| Import | Use |
| --- | --- |
| `ella.to/pipe` | Endpoints, dialing, listening, `Conn` |
| `ella.to/pipe/signaling/sse` | HTTP signaling server and client |
| `ella.to/pipe/signaling/memory` | In-process signaling for tests and single-binary demos |
| `ella.to/pipe/signaling/signalertest` | Conformance suite for your own signaling transport |
| `ella.to/pipe/relay` | STUN and TURN server with per-user plans |

## Ready-made commands

Run any of them without cloning, for example
`go run ella.to/pipe/examples/turnserver@latest`.

| Command | Purpose |
| --- | --- |
| `examples/echo` | Two peers in one process |
| `examples/signaling` | HTTP signaling server |
| `examples/pipecat` | netcat over pipe |
| `examples/turnserver` | STUN and TURN with per-user plans |
| `examples/turnclient` | Measures a connection forced through a relay |
| `examples/turncred` | Mints ephemeral TURN credentials |
| `examples/docker` | Dockerfile, compose stack, coturn configuration |
