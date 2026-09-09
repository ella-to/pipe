// Package sse is a networked [pipe.Signaler] built on plain HTTP: peers receive
// signals over a Server-Sent Events stream and send them with POST requests.
// The event stream itself is produced and consumed with [ella.to/sse]; this
// package adds routing, authentication, per-peer queues, and replay. It passes
// through every proxy and load balancer that understands HTTP/1.1.
//
// # Wire protocol
//
// The [Server] is an [http.Handler] mounted at one URL. The method selects the
// operation:
//
//   - GET opens the stream for the authenticated peer. The response is
//     text/event-stream. Every signal is one event named "signal" whose data is
//     the JSON encoding of [pipe.Signal], with an "id" field carrying a
//     per-peer sequence number. A comment line is written periodically as a
//     keepalive. A client reconnecting with a Last-Event-ID header receives
//     the signals it missed, bounded by [Config.ReplayDepth].
//   - POST sends one signal. The body is the JSON encoding of [pipe.Signal].
//     The server checks that the From field names the authenticated peer,
//     validates the envelope, and queues it for the To peer. It answers 202
//     when queued, 404 when the recipient is unknown, 403 when From is not the
//     caller, 400 when the envelope is invalid, and 503 when the recipient's
//     queue is full.
//
// The server never reads signal payloads. SDP and ICE candidates pass through
// as opaque JSON.
//
// # Authentication
//
// Every request carries an identity that the server establishes through the
// configured [Authenticator]. [StaticTokens] maps bearer tokens to peer IDs and
// is enough for a personal deployment; [TrustPeerHeader] believes whatever the
// client claims and exists for local development only. Anything else, for
// example a session cookie or a JWT, is a small function.
//
// Because the server binds each request to an authenticated peer and refuses
// signals whose From field says otherwise, peer IDs delivered through this
// transport are authenticated: an endpoint that receives an offer from "alice"
// knows that whoever holds alice's credential sent it. That is what makes
// [pipe.Config.AllowPeer] a meaningful access-control list.
//
// # Delivery
//
// Delivery is at-least-once. A client that loses its stream reconnects with
// backoff and replays from its last event ID, so a signal may arrive twice;
// pipe discards duplicates by signal ID. Signals for a peer with no stream
// attached are queued for [Config.OfflineGrace] and then discarded together
// with the peer, after which senders see 404 and pipe reports
// [pipe.ErrPeerUnavailable].
//
// # Deployment notes
//
// Run the server behind TLS. The [http.Server] must not set WriteTimeout,
// because it would cut every stream; use ReadHeaderTimeout and rely on the
// per-write deadline this package sets through [http.ResponseController]. The
// client's [http.Client] must not set Timeout for the same reason; contexts
// bound individual requests.
package sse
