# Wire protocol, version 1

For people writing a signaling transport, implementing pipe in another
language, or reading a capture. Three layers:

| Layer | Carried by |
| --- | --- |
| Signaling envelope (`pipe.Signal`, JSON) | Your `Signaler` |
| One WebRTC DataChannel | ICE, DTLS, SCTP |
| Stream frames, one per DataChannel message | The DataChannel |

`ProtocolVersion`, the frame version byte, the DataChannel label
(`pipe.stream.v1`), and its protocol string (`pipe/1`) change together.

## 1. Signaling envelope

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

Build and validate one in Go:

```go
// envelope/main.go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"ella.to/pipe"
)

func main() {
	sig := pipe.Signal{
		Version:   pipe.ProtocolVersion,
		ID:        pipe.NewID(),
		SessionID: pipe.NewID(),
		Kind:      pipe.KindOffer,
		From:      "alice",
		To:        "bob",
		Payload:   json.RawMessage(`{"sdp":"v=0\r\n"}`),
	}
	if err := sig.Validate(); err != nil {
		log.Fatal(err)
	}
	out, _ := json.Marshal(sig)
	fmt.Println(string(out))

	sig.From = "bob" // from == to
	fmt.Println(sig.Validate())
}
```

| Field | Rules |
| --- | --- |
| `v` | Must be 1 |
| `id` | Fresh random 128-bit ID per signal, for deduplication |
| `session_id` | Chosen by the offerer; same for every signal of a session |
| `kind` | One of the seven kinds below; unknown kinds are rejected |
| `from`, `to` | Peer IDs; must differ; the receiver requires `to` to be itself |
| `payload` | Kind-specific; at most `MaxEnvelopeSize` |

IDs are 32 lowercase hex digits (`pipe.NewID()`) or a lowercase canonical
UUID. Peer IDs are non-empty, at most 128 bytes, valid UTF-8, with no control
characters and no surrounding whitespace. Decoders ignore unknown members.

| Limit | Value |
| --- | --- |
| `MaxEnvelopeSize` | 256 KiB of payload |
| `MaxSDPSize` | 128 KiB |
| `MaxCandidateSize` | 8 KiB; `username_fragment` at most 256 bytes |
| `MaxPeerIDLength` | 128 bytes |
| `MaxReasonLength` | 512 bytes |
| Candidates buffered before the remote description | 256 |

### Kinds

| Kind | Sender | Payload |
| --- | --- | --- |
| `offer` | offerer, once, first | `{"sdp": string}` |
| `answer` | answerer, after `offer` and each `restart` | `{"sdp": string}` |
| `candidate` | both, after their own description | `{"candidate": string, "sdp_mid"?: string, "sdp_mline_index"?: int, "username_fragment"?: string}` |
| `ice-complete` | both, after the last candidate | none (`null` or `{}` accepted) |
| `restart` | offerer, on an established session | `{"generation": int > 0, "sdp": string}`; generation strictly increases |
| `reject` | answerer, instead of `answer` | `{"code": RejectCode, "reason": string}` |
| `close` | both, once, best effort (2 s) | `{"code": CloseCode, "reason": string}` |

An empty `candidate` string is invalid; end of candidates is `ice-complete`.

| `RejectCode` | Meaning |
| --- | --- |
| `not_listening` | No listener |
| `busy` | Accept backlog full |
| `unauthorized` | `AllowPeer` refused |
| `unsupported_version` | Defined, not emitted |
| `invalid_offer` | Defined; this implementation sends `close` instead |
| `internal` | Failure on the rejecting side |

| `CloseCode` | Meaning |
| --- | --- |
| `normal` | Application close, or connectivity lost beyond recovery |
| `going_away` | Endpoint shutting down |
| `protocol_error` | Peer violated the protocol |
| `timeout` | Negotiation timed out |
| `internal` | Failure on the closing side |

A `close` during negotiation fails the dial with `ErrPeerRejected`. On an
established session the receiver drains the stream (up to 5 s) and reports
`io.EOF`.

### Delivery

Transports deliver at least once and may duplicate or reorder. Each endpoint
drops repeats of its last 4096 signal IDs, and additionally:

- a repeated `offer` for a known session is dropped;
- a repeated `answer` is dropped unless a restart is in progress;
- `candidate` and `ice-complete` before the remote description are buffered;
- a `restart` with a stale generation is dropped;
- a non-`offer` signal for an unknown session is dropped;
- a signal from the wrong peer for a session is dropped.

Transports must not modify fields. `signaling/signalertest` checks this.

### Session flow

```
offerer                                   answerer
Dial
  offer  ─────────────────────────────►   AllowPeer? backlog? ── no ──► reject
                                          create PeerConnection
         ◄─────────────────────────────   answer
  candidate / ice-complete  ◄────────►    candidate / ice-complete
  ICE, DTLS, SCTP, DataChannel open
  data, ping, pong          ◄────────►
(connectivity lost)
  restart(n) ──────────────────────────►
         ◄─────────────────────────────   answer
Close
  close frame on the stream, then close signal
```

Timers: negotiation `DialTimeout` (30 s), each signaling send 10 s, `close`
2 s, recovery `Reconnect.AttemptTimeout` plus backoff.

## 2. DataChannel

The offerer creates exactly one channel before its offer. The answerer accepts
only this:

| Parameter | Value |
| --- | --- |
| Label | `pipe.stream.v1` |
| Protocol | `pipe/1` |
| Ordered | true |
| MaxRetransmits, MaxPacketLifeTime | unset (reliable) |
| Negotiation | in band (DCEP) |

Anything else, or a second channel, ends the session with `close`
`protocol_error`. Each side checks `8 + FramePayload` fits the peer's SCTP
max message size (65535 by default in Pion).

## 3. Stream frames

```
 0               1               2               3
+---------------+---------------+---------------+---------------+
| version = 1   | type          | flags = 0                     |
+---------------+---------------+---------------+---------------+
| payload length, uint32 big-endian                             |
+---------------------------------------------------------------+
| payload                                                       |
+---------------------------------------------------------------+
```

| Type | Payload |
| --- | --- |
| `0x01` data | Application bytes; empty is legal and ignored |
| `0x02` ping | 8-byte nonce |
| `0x03` pong | The ping's nonce |
| `0x04` close | uint16 big-endian code, then an optional UTF-8 reason of at most 256 bytes |

Close codes: 0 normal, 1 protocol error, 2 internal, 3 going away. Signaling
`CloseCode` maps `normal` to 0, `protocol_error` to 1, `internal` and
`timeout` to 2, `going_away` to 3.

A frame shorter than 8 bytes, a length that does not match, a payload over the
receiver's `FramePayload` (default 16 KiB, ceiling 256 KiB), an unknown type,
or nonzero flags is a protocol violation.

Encode and decode in Go, for an implementation of your own:

```go
// frames/main.go
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const headerSize = 8

func encode(typ byte, payload []byte) []byte {
	msg := make([]byte, headerSize+len(payload))
	msg[0] = 1 // version
	msg[1] = typ
	binary.BigEndian.PutUint32(msg[4:8], uint32(len(payload)))
	copy(msg[headerSize:], payload)
	return msg
}

func decode(msg []byte, maxPayload int) (typ byte, payload []byte, err error) {
	if len(msg) < headerSize {
		return 0, nil, errors.New("short frame")
	}
	if msg[0] != 1 {
		return 0, nil, fmt.Errorf("version %d", msg[0])
	}
	if msg[2] != 0 || msg[3] != 0 {
		return 0, nil, errors.New("nonzero flags")
	}
	n := binary.BigEndian.Uint32(msg[4:8])
	if int(n) != len(msg)-headerSize || int(n) > maxPayload {
		return 0, nil, errors.New("bad length")
	}
	if msg[1] < 0x01 || msg[1] > 0x04 {
		return 0, nil, fmt.Errorf("type %#x", msg[1])
	}
	return msg[1], msg[headerSize:], nil
}

func main() {
	data := encode(0x01, []byte("hello"))
	fmt.Printf("% x\n", data) // 01 01 00 00 00 00 00 05 68 65 6c 6c 6f

	closeFrame := encode(0x04, append([]byte{0, 0}, "bye"...))
	typ, payload, err := decode(closeFrame, 16<<10)
	fmt.Println(typ, binary.BigEndian.Uint16(payload), string(payload[2:]), err) // 4 0 bye <nil>
}
```

### Rules

- One `Write` larger than the payload limit becomes consecutive data frames
  that never interleave with another writer's.
- Control frames go between application frames only; one that cannot get the
  write lock within 1 s is dropped.
- At most one ping outstanding. Every implementation must answer pings.
- No half-close: a close frame ends both directions.

### Close sequence

Closing side: stop reads and writes (discard unread data); send a close frame;
in the background wait until the peer acknowledged everything or closed (up to
2 s); close the DataChannel; send the `close` signal; keep the PeerConnection
up until the channel closed (up to 3 s) plus 400 ms; close it.

Receiving side: read the close frame; deliver buffered data, then `io.EOF`;
send its own close frame and close the channel; hold the PeerConnection
400 ms; close it.

## 4. Compatibility

- New optional members in envelopes and payloads: allowed within v1.
- New `kind`, `RejectCode`, `CloseCode`, frame type, or flags: not additive;
  receivers reject unknown values. New version required.
- A new version changes `ProtocolVersion`, the frame version byte, the label
  (`pipe.stream.vN`), and the protocol string (`pipe/N`) together.
