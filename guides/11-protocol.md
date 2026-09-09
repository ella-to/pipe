# The pipe wire protocol, version 1

This is the reference for what pipe puts on the wire: the signaling envelope
two endpoints exchange through a `Signaler`, the DataChannel they negotiate,
and the frames that turn that DataChannel into a byte stream. It is for
people writing a signaling transport, interoperating with pipe from another
language, or debugging a capture. Application programmers do not need it; see
[09-client-api.md](09-client-api.md).

## 1. Overview

A pipe connection is built in three layers:

| Layer | Carried by | Defined in |
| --- | --- | --- |
| Signaling envelope (`pipe.Signal`) | Your `Signaler` transport, as JSON | `signaling.go`, `signal_payload.go`, `signal_validate.go`; section 2 |
| WebRTC DataChannel | ICE, DTLS, SCTP, negotiated by SDP offer and answer | `internal/pionx`; section 3 |
| Stream frames | DataChannel messages, one frame per message | `internal/frame`; section 4 |

`ProtocolVersion` is 1 and applies to the signaling envelope. The frame format
has its own version byte, also 1, and the DataChannel label and protocol
string name the version too. All three change together.

## 2. Signaling envelope

### 2.1 Fields

`pipe.Signal` encodes as one JSON object:

```json
{
  "v": 1,
  "id": "3f9c1b2e7d4a4c0f9a1b2c3d4e5f6071",
  "session_id": "0c6a2b1e9f3d4e5f8a7b6c5d4e3f2a10",
  "kind": "offer",
  "from": "alice",
  "to": "bob",
  "payload": { "sdp": "v=0\r\n..." }
}
```

| JSON name | Go field | Type | Rules |
| --- | --- | --- | --- |
| `v` | `Version` | integer | Must equal 1. Anything else is rejected. |
| `id` | `ID` | string | Fresh random 128-bit value per signal, for duplicate suppression. Format in 2.2. |
| `session_id` | `SessionID` | string | Random 128-bit value chosen by the offerer; the same for every signal of one session. Format in 2.2. |
| `kind` | `Kind` | string | One of the seven kinds in 2.3. Unknown kinds are rejected. |
| `from` | `From` | string | Sending peer ID. Rules in 2.2. |
| `to` | `To` | string | Receiving peer ID. Must differ from `from`; an endpoint additionally requires it to be its own ID. |
| `payload` | `Payload` | object | Kind-specific body (2.3). Omitted for kinds that carry none. At most `MaxEnvelopeSize` bytes. |

Decoders ignore unknown object members, both at the envelope level and inside
payloads. A payload must be a single JSON value with no trailing content.

### 2.2 Identifiers and limits

An `id` or `session_id` is either 32 lowercase hexadecimal digits or a
canonical lowercase UUID (36 characters, hyphens at positions 8, 13, 18,
and 23). `pipe.NewID()` produces the 32-digit form from `crypto/rand`.

A peer ID is non-empty, at most `MaxPeerIDLength` = 128 bytes, valid UTF-8,
contains no control characters, and has no leading or trailing whitespace.

| Constant | Value | Applies to |
| --- | --- | --- |
| `MaxEnvelopeSize` | 256 KiB | `payload` bytes; transports should refuse larger envelopes |
| `MaxSDPSize` | 128 KiB | `sdp` in offer, answer, restart |
| `MaxCandidateSize` | 8 KiB | `candidate` string |
| (unexported) | 256 bytes | `username_fragment` |
| `MaxPeerIDLength` | 128 bytes | `from`, `to` |
| `MaxReasonLength` | 512 bytes | `reason` in reject and close |
| (unexported) | 256 | Remote candidates buffered before the remote description arrives; more ends the session |

Strings (`sdp`, `candidate`, `reason`) must be valid UTF-8.

### 2.3 Kinds and payloads

#### `offer`

Starts a session. Sent by the dialer (offerer). The `session_id` is chosen
here and reused by every later signal of the session.

```json
{"v":1,"id":"…","session_id":"…","kind":"offer","from":"alice","to":"bob",
 "payload":{"sdp":"v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\n…a=fingerprint:sha-256 …\r\n"}}
```

Payload: `{"sdp": string}`, non-empty, at most `MaxSDPSize`.

#### `answer`

The answerer's SDP for an offer, or for a `restart`.

```json
{"v":1,"id":"…","session_id":"…","kind":"answer","from":"bob","to":"alice",
 "payload":{"sdp":"v=0\r\n…"}}
```

Payload: `{"sdp": string}`, same rules as `offer`.

#### `candidate`

One trickled ICE candidate. Sent by either side, any number of times, after
its own description has been sent.

```json
{"v":1,"id":"…","session_id":"…","kind":"candidate","from":"alice","to":"bob",
 "payload":{"candidate":"candidate:1 1 udp 2130706431 192.0.2.10 51234 typ host",
            "sdp_mid":"0","sdp_mline_index":0,"username_fragment":"Ab3d"}}
```

Payload: `{"candidate": string, "sdp_mid"?: string, "sdp_mline_index"?: integer, "username_fragment"?: string}`.
`candidate` is non-empty (an empty candidate is not how end-of-candidates is
signaled; use `ice-complete`). The optional fields are omitted when absent so
that "absent" and "empty string" stay distinguishable.

#### `ice-complete`

The sender has no more candidates. No payload; an encoder may omit `payload`
or send `null` or `{}`. Anything else is rejected.

```json
{"v":1,"id":"…","session_id":"…","kind":"ice-complete","from":"alice","to":"bob"}
```

#### `restart`

A new offer for an ICE restart on an established session. Only the offerer
sends it. `generation` starts at 1 for the first restart and increases by one
each time; a receiver drops a restart whose generation is not greater than
the last one it applied, which makes duplicates and reordering harmless. The
receiver replies with `answer`.

```json
{"v":1,"id":"…","session_id":"…","kind":"restart","from":"alice","to":"bob",
 "payload":{"generation":1,"sdp":"v=0\r\n…a=ice-options:trickle\r\n…"}}
```

Payload: `{"generation": integer > 0, "sdp": string}`.

#### `reject`

Refuses a session that has not been established. Sent by the would-be
answerer in response to an `offer` it will not take; no session is created on
the rejecting side.

```json
{"v":1,"id":"…","session_id":"…","kind":"reject","from":"bob","to":"alice",
 "payload":{"code":"unauthorized","reason":"peer is not allowed to connect"}}
```

Payload: `{"code": RejectCode, "reason": string}`.

| `RejectCode` | Meaning |
| --- | --- |
| `not_listening` | The peer has no active listener. |
| `busy` | The peer's accept backlog is full. |
| `unauthorized` | The peer refused the caller (`Config.AllowPeer`). |
| `unsupported_version` | The peers share no protocol version. Defined; not emitted by this implementation, which rejects unknown versions at validation instead. |
| `invalid_offer` | The offer was malformed or unusable. Defined; this implementation reports such failures with `close` instead. |
| `internal` | A failure on the rejecting side. |

The dialer surfaces a reject as `*pipe.RejectedError` matching
`ErrPeerRejected`.

#### `close`

Ends a session, whether established or still negotiating. Sent by whichever
side closes, once, best-effort, bounded by a 2 s send timeout.

```json
{"v":1,"id":"…","session_id":"…","kind":"close","from":"alice","to":"bob",
 "payload":{"code":"normal","reason":""}}
```

Payload: `{"code": CloseCode, "reason": string}`.

| `CloseCode` | Meaning |
| --- | --- |
| `normal` | Ordinary close by the application, or connectivity lost beyond recovery (then `reason` says so). |
| `going_away` | The endpoint is shutting down. |
| `protocol_error` | The peer violated the signaling or stream protocol. |
| `timeout` | An operation exceeded its budget (negotiation timed out). |
| `internal` | A failure on the closing side. |

A `close` received during negotiation ends the dial with `ErrPeerRejected`.
A `close` received on an established session does not tear the stream down
immediately: the DataChannel is the authoritative in-order path and the
peer's last bytes may still be in flight, so the receiver drains the stream
until it ends, bounded by a 5 s linger. The `close` signal is also the proof
that a later SCTP abort was an orderly close, so the receiver reports
`io.EOF` rather than a failure.

### 2.4 Delivery: at-least-once, deduplicated

A transport must deliver each signal at least once, may deliver it more than
once, and may reorder. It must not claim exactly-once. Every endpoint keeps
the most recent 4096 signal `id`s it has accepted and drops repeats. Beyond
that:

- A repeated `offer` for a known session is dropped ("duplicate offer") and
  never creates a second PeerConnection.
- A repeated `answer` after the remote description is set is dropped unless a
  restart is in progress.
- A `candidate` arriving before the remote description is buffered (up to 256)
  and applied when the description arrives; `ice-complete` is buffered the
  same way.
- A `restart` with a stale generation is dropped.
- Any signal for an unknown session that is not an `offer` is dropped, so a
  late or replayed message cannot resurrect a closed session.
- A signal from a peer other than the one the session was created with is
  dropped and counted as a routing failure.

Transports move envelopes opaque and unmodified. Re-encoding JSON is fine;
changing a field is not. The conformance suite in `signaling/signalertest`
checks these properties against any `Signaler`.

### 2.5 Session state machine

```
                    offerer                        answerer
Dial ───────────► new
                  send offer ──────────────────►  (offer received)
                  signaling                        AllowPeer? backlog?  ── no ──► send reject
                                                   create PeerConnection
                                                   apply offer, send answer
                  ◄──────────────────────────────  connecting
(answer received)
                  apply answer, flush buffered candidates
                  connecting
                  ◄──── candidate / ice-complete, both directions ────►
                  ICE connected, DTLS, SCTP, DataChannel open
                  connected                        connected
                  ◄──── data frames, ping/pong, both directions ─────►
   (connectivity lost)
                  recovering
                  send restart(generation n) ───►  apply, send answer
                  ◄──────────────────────────────
                  connected (ICE restart complete)
   Close ───────► send close frame on the stream, then close signal
                  closing                          (close frame read → EOF)
                  closed                           closing → closed
```

Who may send what:

| Kind | Offerer | Answerer | When |
| --- | --- | --- | --- |
| `offer` | yes | no | Once, first |
| `answer` | no | yes | After `offer`; after each `restart` |
| `candidate` | yes | yes | After sending own description |
| `ice-complete` | yes | yes | After the last own candidate |
| `restart` | yes | no | On an established session, generation increasing |
| `reject` | no | yes | Instead of `answer`, before any session exists |
| `close` | yes | yes | Once, at teardown |

Timers on each side: negotiation is bounded by `DialTimeout` (default 30 s)
until the DataChannel opens; each signaling send is bounded by 10 s; a
`close` send by 2 s; recovery attempts by `Reconnect.AttemptTimeout` plus
backoff.

## 3. DataChannel contract

The offerer creates exactly one DataChannel before creating its offer, so the
offer describes it. The answerer accepts it only if every parameter matches:

| Parameter | Required value |
| --- | --- |
| Label | `pipe.stream.v1` |
| Protocol (subprotocol string) | `pipe/1` |
| Ordered | true |
| MaxRetransmits | unset (reliable) |
| MaxPacketLifeTime | unset (reliable) |

A DataChannel with any other label, protocol, or reliability setting is
closed and the session ends with a `close` of code `protocol_error` and the
reason "data channel does not match the protocol contract". A second
DataChannel on the same PeerConnection is likewise rejected. Negotiation is
in-band (DCEP); the channel is not pre-negotiated by ID.

Once open, the channel is detached from Pion's callback API and owned by the
stream layer. Before detaching, the endpoint checks that its configured frame
size fits the SCTP maximum message size the peer announced
(`8 + FramePayload <= MaxMessageSize`); if not, the session fails with an
`ErrNegotiation` naming both numbers.

## 4. Stream framing

Each DataChannel message carries exactly one frame. Frames are never split
across messages and messages never carry more than one frame.

```
 0               1               2               3
+---------------+---------------+---------------+---------------+
| version (1)   | type (1)      | flags (2)                     |
+---------------+---------------+---------------+---------------+
| payload length (4, unsigned, network byte order)              |
+---------------------------------------------------------------+
| payload ...                                                   |
+---------------------------------------------------------------+
```

| Field | Size | Rules |
| --- | --- | --- |
| version | 1 byte | Must be 1. |
| type | 1 byte | `0x01` data, `0x02` ping, `0x03` pong, `0x04` close. Others are rejected. |
| flags | 2 bytes | Must be zero in version 1. Reserved for later versions. |
| payload length | 4 bytes, big-endian | Must equal the number of bytes that follow, and must not exceed the receiver's negotiated maximum payload. |
| payload | length bytes | Type-specific. |

`HeaderSize` is 8. `PayloadLimit`, the hard ceiling regardless of
configuration, is 256 KiB. The negotiated maximum payload is
`Config.FramePayload` (default 16 KiB) on the receiving side; a frame whose
length exceeds it is a protocol violation, as is a message shorter than the
header, a length that does not match the message, an unknown type, or a
nonzero flags field. A protocol violation ends the stream; the session then
sends a close frame and a `close` signal with code `protocol_error`.

### 4.1 Data (`0x01`)

The payload is application bytes. A `Write` of more than the maximum payload
is split into consecutive data frames while the writer holds the stream's
write lock, so the frames of one `Write` are contiguous and never interleave
with another writer's. Control frames may only be inserted between
application frames, never inside a `Write`, and a control frame that cannot
obtain the lock within 1 s is dropped rather than waited for. A data frame
with an empty payload is legal and ignored by the receiver; an empty `Write`
emits no frame.

Example, the five bytes `hello`:

```
00000000  01 01 00 00 00 00 00 05  68 65 6c 6c 6f           |........hello|
          ^  ^  ^^^^^ ^^^^^^^^^^^  ^^^^^^^^^^^^^^
          |  |  flags length=5     payload
          |  type=data
          version=1
```

### 4.2 Ping (`0x02`) and pong (`0x03`)

The payload is an opaque 8-byte nonce (`PingSize`); any other length is a
protocol violation. A receiver answers a ping with a pong carrying the same
nonce. The sender matches the pong to its outstanding ping by nonce and
measures the round trip; a pong with an unknown nonce is ignored. At most one
ping is outstanding per stream. Pings are sent only when
`Config.KeepAlive.Interval` is positive, but every implementation must answer
them.

```
00000000  01 02 00 00 00 00 00 08  9f 3a 7c 11 d2 05 4e b0  |........ .:|...N.|
00000000  01 03 00 00 00 00 00 08  9f 3a 7c 11 d2 05 4e b0  |........ .:|...N.|
```

### 4.3 Close (`0x04`)

The payload is a 2-byte big-endian code followed by an optional UTF-8 reason
of at most `MaxCloseReason` = 256 bytes. A payload shorter than 2 bytes, a
longer reason, or invalid UTF-8 is a protocol violation. An encoder truncates
a longer reason on a rune boundary.

| Code | Meaning |
| --- | --- |
| 0 | normal |
| 1 | protocol error |
| 2 | internal failure |
| 3 | going away (endpoint shutting down) |

The signaling `CloseCode` maps onto these: `normal` to 0, `protocol_error`
to 1, `internal` and `timeout` to 2, `going_away` to 3.

Example, code 0 with reason `bye`:

```
00000000  01 04 00 00 00 00 00 05  00 00 62 79 65           |..........bye|
                                   ^^^^^ ^^^^^^^^
                                   code  reason
```

A receiver that reads a close frame stops reading, delivers any buffered
application bytes to `Read`, and then reports `io.EOF`. Because the stream is
ordered and reliable, every data frame sent before the close frame is
delivered before it.

### 4.4 No half-close

Version 1 has no way to say "finished sending, still receiving". A close
frame from either side ends both directions: the receiver's subsequent
`Write` fails with a closed-connection error. Protocols that need an
end-of-message marker carry it in the application payload.

### 4.5 The close sequence

Closing side:

1. Stop accepting new reads and writes; discard received-but-unread data
   (matching `net.Conn`).
2. Send a close frame, best-effort: skipped if an application `Write` holds
   the lock, bounded by a 5 s write timeout.
3. Off the caller's goroutine, wait until the peer has acknowledged every
   byte written (SCTP buffered amount reaches zero) or has closed its own
   side, bounded by 2 s. Then close the DataChannel, which sends an SCTP
   stream reset.
4. Send the `close` signal (bounded by 2 s).
5. Keep the PeerConnection open until the DataChannel is closed (bounded by
   3 s) and then 400 ms more so the closer's own SCTP acknowledgements reach
   the peer; then close the PeerConnection, which aborts the SCTP association.

Receiving side:

1. Read the close frame; deliver buffered data, then `io.EOF`.
2. Send its own close frame and close its DataChannel (stream reset).
3. Hold the PeerConnection for 400 ms so acknowledgements flush, then close
   it.

If the `close` signal arrives before the stream has ended, the receiver
lingers up to 5 s for the stream to drain rather than cutting it, and reports
`io.EOF` even if the transport ends with an abort, because the signal proved
the close was orderly.

## 5. Compatibility rules

Within version 1:

- New optional members may be added to the envelope or to any payload.
  Decoders ignore members they do not know. A sender must not rely on a peer
  understanding a new member.
- New values for `kind`, `RejectCode`, `CloseCode`, frame `type`, or frame
  `flags` are **not** additive: receivers reject unknown values. Introducing
  one requires a new protocol version.
- Changing the meaning of an existing field, the header layout, or any of the
  limits in 2.2 requires a new version.

A new version changes `ProtocolVersion`, the frame version byte, the
DataChannel label (`pipe.stream.vN`), and the DataChannel protocol string
(`pipe/N`) together, so that a version-1 peer rejects a version-2 session
cleanly at the first envelope rather than failing mid-stream.

For how the transport that carries these envelopes is expected to behave,
see [03-signaling-server.md](03-signaling-server.md); for the security
properties each field does and does not provide, see
[06-security.md](06-security.md).
