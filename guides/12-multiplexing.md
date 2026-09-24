# Multiplexed signaling

One `pipe.Endpoint` already talks to any number of remote peers over a single
signaling stream: `ep.Dial(ctx, "bob")`, `ep.Dial(ctx, "carol")`, and every
inbound connection share the endpoint's one SSE connection. You do not need
anything from this guide to reach many peers.

This guide is for a process that hosts many **local** peer IDs, such as a
gateway with one endpoint per user or a hub with one per device. With
`sse.Client`, every endpoint holds its own event stream:

```
sse.Client                              sse.Mux
                                        
 ep "user-1" ── GET stream ──┐           ep "user-1" ──┐
 ep "user-2" ── GET stream ──┤ server    ep "user-2" ──┼── one GET stream ── server
 ...                         │           ...           │
 ep "user-N" ── GET stream ──┘           ep "user-N" ──┘
 N long-lived connections                1 long-lived connection
```

`sse.Mux` opens one stream for the whole process and routes each incoming
signal to the right endpoint by its `To` field.

## Client side

`sse.Mux` has the same fields as `sse.Client`, so switching is a rename:

```go
// Before: one stream per endpoint.
Signaler: &sse.Client{URL: conf.SignalURL, Token: token}

// After: one stream for every endpoint that shares this value.
Signaler: mux
```

Build one `Mux` and share it:

```go
// gateway/main.go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"ella.to/pipe"
	"ella.to/pipe/signaling/sse"
)

func main() {
	ctx := context.Background()
	tokens := map[pipe.PeerID]string{
		"alice": os.Getenv("ALICE_TOKEN"),
		"bob":   os.Getenv("BOB_TOKEN"),
	}

	mux := &sse.Mux{
		URL: "http://127.0.0.1:8080/pipe",
		// Each peer joins the stream with its own credential.
		Authorize: func(r *http.Request, local pipe.PeerID) {
			r.Header.Set("Authorization", "Bearer "+tokens[local])
		},
	}
	defer mux.Close() // closes every endpoint's signaling and the stream

	for id := range tokens {
		ep, err := pipe.New(ctx, pipe.Config{ID: id, Signaler: mux})
		if err != nil {
			log.Fatal(err)
		}
		defer ep.Close()
		log.Printf("%s is online", id)
	}
	select {}
}
```

What to expect:

- **Automatic lifecycle.** The first `pipe.New` opens the stream. Closing an
  endpoint detaches only that peer. Closing the last one closes the stream, and
  the next `pipe.New` opens it again. `mux.Close()` shuts everything down at
  once.
- **Errors per peer.** A bad credential fails only that peer's `pipe.New`,
  with `sse.ErrUnauthorized`. The other peers keep working.
- **Reconnects.** If the stream drops, the Mux reconnects with `Backoff`,
  rejoins every open peer, and replays what each one missed from its own
  `Last-Event-ID`. Endpoints see nothing; duplicates are discarded as usual.
- **Takeover.** If another process opens a peer ID that is on the Mux, that
  peer fails with `sse.ErrPermanent`, exactly as with `sse.Client`. The rest of
  the Mux is unaffected.
- **One ID per Mux.** Opening the same peer ID twice on one Mux fails with
  `pipe.ErrDuplicatePeer`.
- **Shared inbox.** `Inbox` bounds each peer's unread signals. A peer that
  stops reading holds up delivery to the others once its inbox is full.
  Endpoints read continuously, so this only matters for a custom `SignalConn`
  consumer.
- **Sending is unchanged.** Every signal is still its own `POST`, which reuses
  pooled keep-alive connections.

### Which one to use

| Situation | Use |
| --- | --- |
| One or a few peer IDs per process | `sse.Client` |
| Many peer IDs per process | `sse.Mux` |
| Server predates multiplexing | `sse.Client` (a Mux fails with `sse.ErrPermanent`: *does not support multiplexed streams*) |

## Server side

`sse.Server` supports multiplexed streams out of the box. There is nothing to
enable and nothing new to configure: upgrading the server is enough. Single-peer
`sse.Client`s and `sse.Mux`es talk to the same server and to each other.

### Authentication is unchanged

Every request is still authenticated by your `Authenticator` as exactly one
peer:

| Request | Authenticated as | Proves |
| --- | --- | --- |
| `GET` + `X-Pipe-Mux: 1` (open stream) | any peer the process holds | the caller holds some valid credential |
| `PUT` + `X-Pipe-Mux: <stream>` (join) | the joining peer | the caller may act as that peer |
| `DELETE` + `X-Pipe-Mux: <stream>` (leave) | the leaving peer | the caller may act as that peer |
| `POST` (send) | the sender | `From` is the caller, as before |

The stream opens empty, so it carries nothing until peers join it. Joining is
the step that registers a peer, and it needs that peer's own credential. This
keeps the guarantee from [06 Security](06-security.md): a signal from "alice"
was sent by whoever holds alice's credential, so `AllowPeer` still means what
it says.

A stream ID is a random 128-bit value that only the stream's owner sees.
Knowing one lets you join your own peers to that stream, which only sends your
own signals to someone else. It never lets you receive or remove another
peer's signals, because that takes the other peer's credential.

#### One credential for many peers

If a machine should hold a single token that covers a set of peers, you do not
need a token per peer. Write an `Authenticator` that checks the machine token
and accepts the peer named in `X-Pipe-Peer` when the token covers it:

```go
auth := sse.AuthenticatorFunc(func(r *http.Request) (pipe.PeerID, error) {
	token, ok := sse.BearerToken(r)
	if !ok {
		return "", sse.ErrUnauthorized
	}
	allowed, err := lookupMachine(r.Context(), token) // your store: token -> set of peer IDs
	if err != nil {
		return "", sse.ErrUnauthorized
	}
	peer := pipe.PeerID(r.Header.Get(sse.PeerHeader))
	if !allowed[peer] {
		return "", sse.ErrUnauthorized
	}
	return peer, nil
})
```

Then the client needs only `Token`:

```go
mux := &sse.Mux{URL: signalURL, Token: machineToken}
```

### Limits and state

- `MaxPeers`, `QueueSize`, `ReplayDepth`, and `OfflineGrace` apply per peer,
  exactly as for single-peer streams. A peer that leaves, or whose Mux stream
  drops, keeps its queue for `OfflineGrace`, so a reconnecting Mux loses
  nothing.
- Opening a Mux stream does not register a peer and does not count toward
  `MaxPeers`.
- Per stream, the server holds one goroutine and one connection, no matter how
  many peers have joined.

### Reverse proxies

The proxy settings from [03 Signaling server](03-signaling-server.md#step-6-behind-a-reverse-proxy)
still apply. In addition, the proxy must pass `PUT` and `DELETE` and the
`X-Pipe-Mux` header. Most proxies do this by default; check for method
allow-lists in WAF rules.

## Wire protocol

Implement this to write a multiplexing client in another language. Everything
in [11 Protocol](11-protocol.md) and the single-peer SSE protocol stays the
same. Every request carries the usual `X-Pipe-Peer` header and credential.

1. **Open.** `GET <url>` with `X-Pipe-Mux: 1` and
   `Accept: text/event-stream`. The first event is:

   ```
   event: mux
   data: {"stream":"3f9c...e1"}
   ```

   A server without multiplexing replies with a `hello` event instead. Treat
   that as unsupported.

2. **Join.** `PUT <url>` with `X-Pipe-Mux: <stream>` and, when resuming,
   `Last-Event-ID: <last id this peer received>`. The server answers:

   | Status | Meaning |
   | --- | --- |
   | `204` | joined; signals for this peer now arrive on the stream |
   | `404` | stream unknown; reopen it and join again |
   | `401` / `403` | this peer's credential was refused |
   | `503` | `MaxPeers` reached or shutting down; retry with backoff |

   Joining moves the peer from any stream it was on, including a single-peer
   stream elsewhere, which is then told `replaced`.

3. **Receive.** Events on the stream:

   | Event | Data | Client action |
   | --- | --- | --- |
   | `signal` | a `pipe.Signal`; `id:` is that peer's sequence number | route by `to`; remember `id` for that peer |
   | `detached` | `{"peer":"alice","reason":"replaced"}` | another stream took `alice`; fail her locally |
   | `shutdown` | none | reconnect with backoff |
   | comment | `ping` | keepalive, ignore |

   Sequence numbers are per peer. Keep one `Last-Event-ID` per peer, not one
   per stream.

4. **Leave.** `DELETE <url>` with `X-Pipe-Mux: <stream>`. Always `204`.

5. **Send.** Unchanged: `POST <url>` with the signal as JSON.

6. **Reconnect.** When the stream drops, open a new one (step 1). The new
   stream has a new ID and no members, so join every open peer again with its
   own `Last-Event-ID`. If the stream reconnects silently and a second `mux`
   event arrives, treat it the same way.
