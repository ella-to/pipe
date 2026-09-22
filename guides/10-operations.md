# Operations

## Metrics

Export pipe's metrics with the standard library's `expvar`:

```go
// expvarmetrics/main.go
//
//	go run ./expvarmetrics & curl -s localhost:9090/debug/vars | grep pipe
package main

import (
	"context"
	"expvar"
	"log"
	"net/http"
	"strings"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
)

type expvarMetrics struct{ vars *expvar.Map }

func name(n string, labels []pipe.Label) string {
	var b strings.Builder
	b.WriteString(n)
	for _, l := range labels {
		b.WriteString("," + l.Key + "=" + l.Value)
	}
	return b.String()
}

func (m expvarMetrics) Count(n string, delta int64, labels ...pipe.Label) {
	m.vars.Add(name(n, labels), delta)
}

func (m expvarMetrics) Gauge(n string, value int64, labels ...pipe.Label) {
	v := new(expvar.Int)
	v.Set(value)
	m.vars.Set(name(n, labels), v)
}

func (m expvarMetrics) Duration(n string, d time.Duration, labels ...pipe.Label) {
	v := new(expvar.Float)
	v.Set(d.Seconds())
	m.vars.Set(name(n, labels)+",last_seconds", v)
}

func main() {
	metrics := expvarMetrics{vars: expvar.NewMap("pipe")}

	ep, err := pipe.New(context.Background(), pipe.Config{
		ID:       "alice",
		Signaler: memory.New(), // your real signaler
		Metrics:  metrics,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer ep.Close()

	log.Fatal(http.ListenAndServe("127.0.0.1:9090", nil)) // expvar registers /debug/vars
}
```

| Name | Kind | Labels |
| --- | --- | --- |
| `pipe.dial.attempts` | counter | |
| `pipe.dial.results` | counter | `result`: `success`, `timeout`, `rejected`, `error` |
| `pipe.accept.attempts` | counter | |
| `pipe.accept.results` | counter | `result`: `success`, `not-listening`, `busy`, `unauthorized`, `error` |
| `pipe.sessions.active` | gauge | |
| `pipe.sessions.pending` | gauge | |
| `pipe.signal.sent` | counter | `kind`, `result`: `success`, `error`, `timeout` |
| `pipe.signal.received` | counter | `kind`, `result`: `accepted`, `invalid`, `misrouted`, `duplicate` |
| `pipe.connect.duration` | duration | `role`: `offerer`, `answerer` |
| `pipe.restart.attempts` | counter | |
| `pipe.restart.results` | counter | `result` |
| `pipe.restart.duration` | duration | |
| `pipe.stream.bytes.read` | counter | added when a connection closes |
| `pipe.stream.bytes.written` | counter | added when a connection closes |
| `pipe.keepalive.rtt` | duration | |
| `pipe.keepalive.failures` | counter | `reason`: `no-pong` |
| `pipe.protocol.failures` | counter | `scope`: `signal`, `routing`, `candidate` |

Alert on:

| Signal | Likely cause |
| --- | --- |
| `dial.results{result=timeout}` rising | Signaling or STUN/TURN reachability |
| `dial.results{result=rejected}` rising | Peers not listening, full backlogs, allow-lists |
| `accept.results{result=busy}` | `AcceptBacklog` too small or `Accept` too slow |
| `accept.results{result=unauthorized}` | Misconfiguration, or someone probing |
| `signal.received{result=invalid\|misrouted}` | Broken or hostile signaling transport |
| `restart.attempts`, `keepalive.failures` | Flapping connectivity |
| `connect.duration` p99 step up | More connections falling back to the relay |

## Logging

```go
Logger: slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
```

| Level | Contents |
| --- | --- |
| Info | Connection established, connectivity lost and restored, ICE restart, endpoint closed |
| Warn | Signaling stopped, keepalive failed, slow shutdown |
| Debug | Every dropped or rejected signal, discarded candidates, close reasons, teardown timing |

Run at info; switch one endpoint to debug to diagnose a peer. Never logged:
credentials, SDP, candidates, payloads.

## Tuning

A roaming laptop that must survive network changes:

```go
cfg.KeepAlive = pipe.KeepAliveConfig{Interval: 10 * time.Second, Timeout: 5 * time.Second}
cfg.Reconnect = pipe.ReconnectPolicy{
	Enabled:        true,
	MaxAttempts:    5,
	AttemptTimeout: 15 * time.Second,
	Backoff:        pipe.Backoff{Initial: time.Second, Maximum: 10 * time.Second, Factor: 2, Jitter: 0.2},
}
```

High-bandwidth links where both ends are yours:

```go
cfg.FramePayload = 32 << 10 // same on both ends; at most 65527 against a default Pion peer
cfg.ReadBuffer = 4 << 20    // more unread data before backpressure
```

Many idle connections:

```go
cfg.ReadBuffer = 256 << 10 // less memory per connection
```

Bursty inbound dials:

```go
cfg.AcceptBacklog = 256
```

| Knob | Default | Effect |
| --- | --- | --- |
| `DialTimeout` | 30s | Whole dial, and inbound negotiation |
| `ICETimeout` | 20s | Recovery budget when `Reconnect` is off |
| `KeepAlive.Interval` | off | Ping period |
| `Reconnect.MaxAttempts` | 3 | Restarts before `ErrDisconnected` |
| `FramePayload` | 16 KiB | Per-message payload; ceiling 256 KiB |
| `ReadBuffer` | 1 MiB | Unread bytes per connection |
| `AcceptBacklog` | 64 | Pending inbound sessions |

## Timing

| Event | Time |
| --- | --- |
| Dial, host candidates, one machine | about 7 ms |
| Dial, relay-only through a local relay | about 6 ms |
| Dial with default policy that ends on a relay | plus about 2 s (Pion waits for a direct pair) |
| Dial across the internet with STUN | a few hundred ms |
| `Conn.Close` | returns immediately; drains up to 2 s in the background |
| PeerConnection release after close | about 0.5 s, up to about 3.4 s if the peer is gone |
| `Endpoint.Close` with open connections | about 0.5 s, bounded at 5 s |

## Capacity

No published numbers yet. Each connection is one Pion PeerConnection (ICE
agent, DTLS, SCTP), so cost per connection is Pion's. Measure your own:

```sh
go run ella.to/pipe/examples/turnclient@latest -bytes 64MiB   # throughput through a relay
go test ./test/integration -run TestConcurrentDials           # 20 concurrent dials
```

Then open as many connections as you plan to serve, at your expected traffic,
and watch `pipe.sessions.active`, memory, and `pipe.connect.duration`.

## Signaling server

- `srv.Peers()` lists attached peers and those inside `OfflineGrace`.
- A disconnected peer keeps its queue for `OfflineGrace` (30s), then senders
  get 404 (`pipe.ErrPeerUnavailable`).
- A second stream for the same peer ID replaces the first.
- State is in memory. After a restart, clients reconnect with backoff (500 ms
  to 10 s) and in-flight dials time out.
- Set `MaxPeers`.

## Relay

```go
st := srv.Stats()
log.Printf("relay %s", st)
for _, id := range relay.SortedUsers(st) {
	log.Printf("user %s %s", id, st.Users[id])
}
```

```
relay allocations=2 active=2 sent=2.2MiB/2729pkt received=2.2MiB/2729pkt dropped=330.1KiB/292pkt delayed=7pkt
user alice@free plan=free allocations=2 active=2 sent=2.2MiB received=2.2MiB dropped=330.1KiB/292pkt rejected=0
```

| Field | Meaning |
| --- | --- |
| `allocations` / `active` | Relay sockets since start / open now |
| `sent` / `received` | Toward peers / toward the user |
| `dropped` | Over the plan's rate (expected on capped plans; a bug on unlimited ones) |
| `delayed` | Held briefly to fit the budget |
| `rejected` | Refused by `MaxAllocations` |

Abandoned allocations expire after 10 minutes, so `active` lags a crashed
client by up to that long.

## Troubleshooting

| Symptom | Look at |
| --- | --- |
| `Dial` fails with `ErrTimeout` | Is the peer listening? Did both sides gather candidates (debug logs)? Add STUN, then TURN |
| Rejected `not_listening` | The peer must call `Listen` |
| Rejected `busy` | Peer's `AcceptBacklog` full, or it is not calling `Accept` |
| Rejected `unauthorized` | Peer's `AllowPeer` |
| `ErrPeerUnavailable` | Peer not connected to signaling, wrong ID, or past `OfflineGrace` |
| `ErrSignaling` | Signaling server down, TLS or proxy problem |
| `sse: permanent failure` from `pipe.New` | Token, token-to-ID mapping, or URL path |
| `ErrDisconnected` | Network change; enable keepalive on the dialer, raise `Reconnect.MaxAttempts` |
| `stream protocol violation` | `FramePayload` differs between the ends |
| Read error mentioning `abort chunk` | Peer crashed or was killed |
| LAN works, internet does not | No `ICEServers`: [04-stun.md](04-stun.md), [05-turn-relay.md](05-turn-relay.md) |
| Relay-only works locally, not remotely | `RelayIP` or relay port range |
| Relayed throughput far below the plan rate | Echo tests cross the relay four times; measure one direction |
| Keepalive failures on a healthy link | `KeepAlive.Timeout` below worst-case RTT under load |
| `sessions did not shut down within the grace period` | A peer was unreachable during close; harmless |

Still stuck: debug logs on both endpoints, side by side. Every dropped signal,
discarded candidate, and close reason is there.
