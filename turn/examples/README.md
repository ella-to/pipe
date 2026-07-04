# TURN Server Examples

Runnable examples for `ella.to/pipe/turn`, ordered from simple to advanced.

| Example | What it shows |
|---|---|
| [01-simple](01-simple/main.go) | Minimal TURN server with static users over UDP |
| [02-tcp-tls](02-tcp-tls/main.go) | TURN over UDP + TCP + TLS (TURNS) |
| [03-dynamic-credentials](03-dynamic-credentials/main.go) | Time-limited credentials with an HTTP endpoint that issues them (TURN REST API) |
| [04-quota](04-quota/main.go) | Per-user allocation and bandwidth quotas, with per-user overrides |
| [05-advanced](05-advanced/main.go) | Production-style: dynamic credentials, quotas, permission filtering, lifecycle events/metrics, relay port range, graceful shutdown |

## Running

Each example is a standalone `main` package:

```bash
go run ./turn/examples/01-simple -public-ip 127.0.0.1 -users alice=secret
```

For local experiments `-public-ip 127.0.0.1` works fine. On a real server,
set it to the machine's public IP address and open the listening port (UDP
3478 by default) plus the relay port range in your firewall.

## Testing with a TURN client

You can verify any of these servers with the `pion/turn` client examples, or
with `ella.to/pipe` itself using relay-only mode:

```go
turnServer := pipe.NewTURNServer("turn:127.0.0.1:3478?transport=udp", "alice", "secret")
p, err := pipe.NewPeer("my-id", sig,
    pipe.WithPeerTURN(turnServer),
    pipe.WithPeerForceRelay(), // force traffic through the relay
)
```
