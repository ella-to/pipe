# Operating pipe

This guide is for the person who keeps a pipe deployment running: what to
measure, what to log, which knobs change behavior under load or on bad
networks, how fast things are and are not, and what a symptom means when a
connection does not come up. It assumes the concepts in
[01-concepts.md](01-concepts.md) and refers to the API in
[09-client-api.md](09-client-api.md).

## Metrics

Pipe reports measurements through the `Metrics` interface in `Config`
(`Count`, `Gauge`, `Duration`, each with bounded-cardinality labels). Nothing
is recorded when it is nil. Peer IDs and session IDs are never label values,
so cardinality is fixed.

| Name | Kind | Labels | Meaning |
| --- | --- | --- | --- |
| `pipe.dial.attempts` | counter | | `Dial` calls |
| `pipe.dial.results` | counter | `result` = `success`, `timeout`, `rejected`, `error` | Outcome of each dial |
| `pipe.accept.attempts` | counter | | Inbound offers seen by a listening endpoint |
| `pipe.accept.results` | counter | `result` = `success`, `not-listening`, `busy`, `unauthorized`, `error` | Outcome of each inbound offer; `success` is counted when `Accept` returns the connection |
| `pipe.sessions.active` | gauge | | Sessions negotiating or established |
| `pipe.sessions.pending` | gauge | | Inbound backlog slots in use |
| `pipe.signal.sent` | counter | `kind`, `result` = `success`, `error`, `timeout` | Signals handed to the signaler |
| `pipe.signal.received` | counter | `kind`, `result` = `accepted`, `invalid`, `misrouted`, `duplicate` | Signals received from the signaler |
| `pipe.connect.duration` | duration | `role` = `offerer`, `answerer` | Time from session start to an open DataChannel |
| `pipe.restart.attempts` | counter | | ICE restarts requested |
| `pipe.restart.results` | counter | `result` = `success`, `error` | ICE restart outcomes |
| `pipe.restart.duration` | duration | | Time from a restart request to restored connectivity |
| `pipe.stream.bytes.read` | counter | | Application bytes read, added when a connection closes |
| `pipe.stream.bytes.written` | counter | | Application bytes written, added when a connection closes |
| `pipe.keepalive.rtt` | duration | | Round trip of each successful keepalive probe |
| `pipe.keepalive.failures` | counter | `reason` = `no-pong` | Probes that got no pong within `KeepAlive.Timeout` |
| `pipe.protocol.failures` | counter | `scope` = `signal`, `routing`, `candidate` | Invalid or unexpected input that was dropped |

Signal kinds are `offer`, `answer`, `candidate`, `ice-complete`, `restart`,
`reject`, `close`.

What to alert on:

- `pipe.dial.results{result="timeout"}` rising as a share of attempts: the
  signaling path or ICE is failing. Check the signaling server and STUN/TURN
  reachability.
- `pipe.dial.results{result="rejected"}` rising: peers are not listening,
  their backlog is full, or your allow-lists are wrong.
- `pipe.accept.results{result="busy"}`: `AcceptBacklog` is too small for the
  offered load, or `Accept` is not being called fast enough.
- `pipe.accept.results{result="unauthorized"}`: someone is dialing peers that
  refuse them. A few are misconfiguration; many are a probe.
- `pipe.signal.received{result="invalid"}` or `misrouted`: a broken or
  hostile signaling transport. Both should be zero with `signaling/sse`.
- `pipe.restart.attempts` and `pipe.keepalive.failures`: connectivity is
  flapping. Compare `pipe.restart.results{result="success"}` to see whether
  recovery is working.
- `pipe.connect.duration` p99: with STUN only this should be well under a
  second on healthy networks; a step up usually means relay fallbacks.

An adapter to your metrics library is a few lines; the interface has three
methods and every name and label is listed above.

## Logging

`Config.Logger` is a `*slog.Logger`. When nil, logging is discarded; pipe
never writes to the global logger. Every line carries `local` (this endpoint's
peer ID), and session-scoped lines add `session`, `peer`, and `role`.

| Level | Contents |
| --- | --- |
| Info | Connection established (with elapsed time), connectivity lost and restored, ICE restart requested, endpoint closed |
| Warn | Signaling stopped, keepalive probe failed, sessions not finishing within the shutdown grace |
| Debug | Every dropped or rejected signal with the reason, discarded candidates, close reasons, teardown timing (`transport released after=`), send failures during shutdown |

Nothing at any level includes credentials, SDP, ICE usernames or passwords, or
payload bytes. The `signaling/sse` server logs peer registrations and
forgettings at info and rejected requests at debug; it never logs tokens or
signal contents. The example relay logs allocations and releases at info and
never logs passwords.

Run production at info. Turn on debug for one endpoint when diagnosing a
specific peer; the volume is a handful of lines per negotiation.

## Keepalive and reconnect

Defaults come from `config.go`.

| Setting | Default | Effect |
| --- | --- | --- |
| `DialTimeout` | 30 s | Bound on one whole dial, from offer to open DataChannel. Also the negotiation timer for inbound sessions. |
| `ICETimeout` | 20 s | Recovery budget when `Reconnect` is disabled: how long a lost connection may stay lost before it closes. |
| `KeepAlive.Interval` | 0 (off) | Period between ping frames on an idle or busy connection. |
| `KeepAlive.Timeout` | `Interval` | How long to wait for the pong. Must not exceed `Interval`. |
| `Reconnect.Enabled` | true | Recover lost connectivity with an ICE restart on the same PeerConnection. |
| `Reconnect.MaxAttempts` | 3 | Consecutive restart attempts before the connection closes with `ErrDisconnected`. |
| `Reconnect.AttemptTimeout` | 10 s | How long each attempt may take before the next one (or failure). |
| `Reconnect.Backoff` | 500 ms initial, 5 s maximum, factor 2, jitter 0.2 | Extra delay between attempts. |

How they interact:

- Without keepalive, connectivity loss is noticed when ICE or DTLS notices
  it, which can take tens of seconds on a silently dead path. With
  `KeepAlive.Interval` of 10 to 30 s, a missing pong marks the connection
  `StateRecovering` and starts a restart immediately. Enable keepalive on
  long-lived idle connections; leave it off for short transfers.
- Only the dialing side (offerer) issues ICE restarts. The listening side
  waits for the restart offer. If your listeners sit behind stable addresses
  and your dialers roam, this is the natural direction. If the *listener*
  roams, its connectivity loss is detected but recovery still depends on the
  dialer noticing and restarting, which is another argument for keepalive on
  the dialer.
- A restart keeps the same `Conn`, byte order, and buffers. Anything that
  would need a new PeerConnection ends the connection with `ErrDisconnected`
  instead; reconnect at the application level, where you know what a resume
  means.
- `Reconnect: pipe.ReconnectPolicy{Enabled: false}` turns recovery off; a
  lost connection then waits `ICETimeout` and closes.

For a connection that must survive a laptop changing Wi-Fi networks, a
reasonable set is:

```go
cfg.KeepAlive = pipe.KeepAliveConfig{Interval: 10 * time.Second, Timeout: 5 * time.Second}
cfg.Reconnect = pipe.ReconnectPolicy{
	Enabled:        true,
	MaxAttempts:    5,
	AttemptTimeout: 15 * time.Second,
	Backoff:        pipe.Backoff{Initial: time.Second, Maximum: 10 * time.Second, Factor: 2, Jitter: 0.2},
}
```

## Performance tuning

### Frame size

`Config.FramePayload` is the largest payload per DataChannel message (default
16 KiB, ceiling `MaxFramePayload` = 256 KiB). Larger frames mean fewer SCTP
messages per byte and less per-frame overhead. Two constraints:

- The peer's SCTP announces a maximum message size; when a channel opens, pipe
  checks that `8 + FramePayload` fits and otherwise fails the session with an
  `ErrNegotiation` naming both numbers. Pion announces 65535 bytes when nothing
  else is configured, so `FramePayload` above 65527 fails against a default
  Pion peer.
- Both sides must accept each other's frames. A receiver rejects a frame
  larger than its own `FramePayload` as a protocol violation. Configure the
  same value on both ends.

32 KiB is a safe step up from the default when both ends are yours.

### Read buffer

`Config.ReadBuffer` (default 1 MiB, at least one frame) bounds received bytes
that the application has not read. Beyond it, the read loop stops taking
messages and SCTP flow control pushes back on the sender. Raise it for
high-bandwidth, high-latency links where the application reads in bursts;
lower it to cap memory per connection when you hold many idle ones.

### Backlog

`Config.AcceptBacklog` (default 64) bounds inbound sessions that are
negotiating or waiting in `Accept`. Offers beyond it are refused with `busy`
and cost the refuser nothing but a signal. Size it to the burst of concurrent
connection attempts you expect, not to total connections.

### The read path

Received frames used to be copied out of the read buffer into a fresh
allocation per frame. The read loop now reads each message straight into a
pooled buffer whose ownership passes to `Read`, and the buffer returns to a
free list once consumed. In the in-memory stream benchmark
(`internal/frame/bench_test.go`, 16 KiB frames, 1 MiB writes), throughput
rose from about 7.2 GB/s to about 11 GB/s and allocations fell from 129 to 73
per megabyte; the 73 that remain belong to the test's fake channel, which
copies on write. Over a real network SCTP and DTLS dominate, but the change
removes a garbage-collection cost that scaled with bytes received.

### Connection setup latency

Measured on one machine with the in-process signaler:

| Path | Time to an open DataChannel |
| --- | --- |
| Host candidates | about 7 ms |
| Relay-only through a local TURN server | about 6 ms |

Relay-only used to take a flat 2 seconds. Pion's ICE agent holds a working
relay pair for two seconds in case a direct pair appears; with
`ICETransportPolicyRelay` there is nothing to wait for, and pipe now sets that
wait to zero for relay-only endpoints. Endpoints with the default policy that
end up on a relay still pay it, by design: the two seconds buy a direct path
when one exists.

Across the internet, add one signaling round trip per message and the ICE
check round trips; a few hundred milliseconds is typical with STUN, more when
TURN allocation is needed.

### Close semantics and the drain

`Conn.Close` returns immediately. Behind it:

1. The stream sends a close frame and stops accepting reads and writes.
2. Off the caller's goroutine, the stream waits for the peer to acknowledge
   every byte written, for up to 2 s (`drainTimeout` in `internal/frame`).
   The wait also ends as soon as the peer closes its side, because a peer
   that has closed has read everything it ever will. Only then is the
   DataChannel closed.
3. The session keeps the PeerConnection open until the stream has released
   the channel (bounded at 3 s) and then for a further 400 ms so that its own
   SCTP acknowledgements reach the peer. Closing the PeerConnection aborts
   the SCTP association, which would otherwise discard anything in flight.

Consequences:

- Write-then-Close delivers the whole write; the peer reads it and then gets
  `io.EOF`. `TestCloseAfterWriteDeliversEverything` in `test/integration`
  covers 8 MiB.
- A closed connection's PeerConnection lingers for roughly half a second in
  the common case, and for up to about 3.4 s when the peer is unreachable.
  During that time it counts in `pipe.sessions.active`.
- `Endpoint.Close` waits for every session to finish, bounded by a 5 s grace,
  so it typically takes about half a second when connections are open and
  returns at once when none are.

### Capacity

There are no published scale numbers, and the maintainers do not claim any
until they are measured. What is known: one endpoint holds one signaling
stream and one PeerConnection per connection; each PeerConnection carries its
own ICE agent, DTLS session, and SCTP association, so memory and CPU per
connection are those of Pion, not of pipe. The signaling server holds a few
hundred bytes per queued signal and one goroutine per attached stream.

Measure before you promise. `examples/turnclient -bytes 64MiB` reports
throughput through a relay with and without a rate budget; the integration
test `TestConcurrentDials` opens 20 connections at once; and your own load
generator should open as many `pipe.Conn`s as you intend to serve and hold
them at your expected traffic while you watch `pipe.sessions.active`, memory,
and `pipe.connect.duration`.

## Operating the signaling server

- `Server.Peers()` lists known peers, attached or within their offline grace.
  The example command exposes the count on `/healthz`.
- A peer whose stream drops keeps its queue for `OfflineGrace` (default 30 s)
  and receives what it missed on reconnect, up to `ReplayDepth` (default 256)
  already-delivered signals via `Last-Event-ID`. After the grace it is
  forgotten and senders get 404, which pipe reports as `ErrPeerUnavailable`.
- A second stream for the same peer replaces the first; the first receives a
  `replaced` event and its client fails permanently. This is how a device
  that restarts takes over its own ID, and how you find out that two devices
  share one token.
- State is in memory. A restart forgets every peer; clients reconnect with
  backoff (500 ms doubling to 10 s by default) and re-register on their first
  successful stream. Dials in flight during the restart time out.
- Set `MaxPeers`. Without it, anyone with a valid token can register any
  number of peer IDs only if your authenticator lets one token map to many
  IDs, but a leaked `TrustPeerHeader` deployment can be filled up by anyone.
- The server does not need to be reachable by anyone but your peers. Put it
  behind TLS; see [06-security.md](06-security.md) and
  [08-docker.md](08-docker.md).

## Operating the relay

`examples/turnserver -stats 60s` prints two kinds of lines:

```
turn: traffic stats="allocations=2 active=2 sent=2.2MiB/2729pkt received=2.2MiB/2729pkt dropped=330.1KiB/292pkt delayed=7pkt"
turn: user user=alice@free stats="plan=free allocations=2 active=2 sent=2.2MiB received=2.2MiB dropped=330.1KiB/292pkt rejected=0"
```

| Field | Meaning |
| --- | --- |
| `allocations` | Relay sockets created since startup |
| `active` | Relay sockets open now |
| `sent` | Client-to-peer traffic through relay sockets (bytes and packets) |
| `received` | Peer-to-client traffic |
| `dropped` | Traffic discarded because it exceeded the plan's budget |
| `delayed` | Packets held briefly to stay inside the budget (peer-to-client direction only) |
| `plan` | The plan the user resolved to |
| `rejected` | Allocate requests refused by the plan's `MaxAllocations` quota |

`dropped` on a rate-limited plan is expected; it is how the budget is
enforced, and SCTP inside the pipe backs off in response. `dropped` on an
unlimited plan means a bug or a misconfigured burst. `rejected` climbing for
one user means that user runs more concurrent connections than its plan
allows, or leaks them.

Allocations expire on their own when a client stops refreshing them (Pion's
default lifetime is 10 minutes); a crashed client's allocation disappears
without operator action. `active` therefore lags a crash by up to that long.

## Troubleshooting

| Symptom | Meaning | Look at |
| --- | --- | --- |
| `Dial` returns `ErrTimeout` after `DialTimeout` | Signaling never delivered the answer, or ICE found no path | Is the peer online and listening? Did both sides gather candidates (debug logs)? Is UDP blocked? Add STUN, then TURN |
| `ErrPeerRejected` with code `not_listening` | The peer exists but has no `Listener` | The peer must call `Listen` before others dial |
| `ErrPeerRejected` with code `busy` | The peer's `AcceptBacklog` is full | The peer is not calling `Accept` fast enough or is being flooded |
| `ErrPeerRejected` with code `unauthorized` | The peer's `AllowPeer` refused you | The listener's allow-list |
| `ErrPeerUnavailable` | The signaling server has no stream for that peer ID (404 from `sse`) | Peer not started, wrong ID, or forgotten after `OfflineGrace` |
| `ErrSignaling` on dial | The signaling transport failed to send | Signaling server down or unreachable; TLS or proxy problem |
| `sse: permanent failure` from `pipe.New` | 401, 403, or a URL that is not a signaling server | Token, token-to-ID mapping, `URL` path |
| Connection closes with `ErrDisconnected` | Connectivity was lost and the restart budget ran out | Network change on one side; raise `Reconnect.MaxAttempts`, enable keepalive on the dialer |
| Read error `pipe: stream protocol violation` | The peer sent a frame that violates the framing rules, usually a `FramePayload` mismatch | Configure the same `FramePayload` on both ends; check for a non-pipe peer |
| Read error mentioning `abort chunk` | The peer's PeerConnection was torn down without an orderly close | Peer crashed or was killed; if it happens on normal `Close`, the two sides run different pipe versions |
| Connects on the LAN, not across the internet | Only host candidates; no STUN or TURN configured | Add `ICEServers`; see [04-stun.md](04-stun.md) and [05-turn-relay.md](05-turn-relay.md) |
| Connects with `-relay-only` locally but not from outside | `-relay-ip` or relay port range wrong on the TURN server | [08-docker.md](08-docker.md) troubleshooting table |
| Throughput through the relay is far below the plan's rate | Every byte of an echo crosses the relay four times, and drops make SCTP back off | Measure one direction; expect application throughput well below the token bucket rate |
| `Stats().LocalCandidate` is `relay` on a LAN | Relay-only policy, or host and reflexive checks failed | Expected with `ICETransportPolicyRelay`; otherwise check firewalls between the hosts |
| Keepalive failures with the link up | `KeepAlive.Timeout` shorter than the path's worst-case RTT under load, or the peer's write lock held by a long write | Raise the timeout; the receiver drops a pong only if it cannot get the write lock within 1 s |
| `pipe: sessions did not shut down within the grace period` at close | A session's teardown exceeded 5 s | Usually an unreachable peer during drain; harmless, logged at warn |

When the tables do not cover it, enable debug logging on both endpoints and
read the two logs side by side; every dropped signal, discarded candidate, and
close reason is there.
