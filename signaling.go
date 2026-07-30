package pipe

import (
	"context"
	"encoding/json"
)

// PeerID names a signaling destination. It is a routing identity, not an
// authenticated one: proving that a peer owns its ID is the responsibility of
// the signaling transport and its authentication hooks.
type PeerID string

// ProtocolVersion is the signaling envelope version implemented by this
// package. Peers that do not share a version reject the session.
const ProtocolVersion uint16 = 1

// SignalKind identifies the meaning of a [Signal]. Unknown kinds are rejected
// rather than guessed.
type SignalKind string

// Signal kinds defined by protocol version 1.
const (
	// KindOffer carries an SDP offer and starts a session.
	KindOffer SignalKind = "offer"
	// KindAnswer carries the SDP answer for an offer or a restart.
	KindAnswer SignalKind = "answer"
	// KindCandidate carries one trickled ICE candidate.
	KindCandidate SignalKind = "candidate"
	// KindICEComplete reports end-of-candidates for the sender.
	KindICEComplete SignalKind = "ice-complete"
	// KindRestart carries a new offer for an ICE restart on an existing
	// session.
	KindRestart SignalKind = "restart"
	// KindReject refuses a session that has not been established.
	KindReject SignalKind = "reject"
	// KindClose terminates a session.
	KindClose SignalKind = "close"
)

// Signal is the versioned signaling envelope exchanged by two endpoints.
// Signaling transports move envelopes without interpreting their payloads.
//
// Additive fields are permitted within version 1: decoders ignore unknown
// object members. Any change to the meaning of an existing field requires a new
// kind or a new version.
type Signal struct {
	// Version must equal [ProtocolVersion].
	Version uint16 `json:"v"`
	// ID is a fresh random 128-bit value in lowercase hex. It exists so that
	// receivers can discard duplicates.
	ID string `json:"id"`
	// SessionID is a random 128-bit value in lowercase hex chosen by the
	// offerer. Negotiation is correlated by session, never by peer.
	SessionID string `json:"session_id"`
	// Kind identifies the payload.
	Kind SignalKind `json:"kind"`
	// From is the sending peer.
	From PeerID `json:"from"`
	// To is the receiving peer.
	To PeerID `json:"to"`
	// Payload is the kind-specific body, absent for kinds that carry none.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Signaler opens a signaling connection for a local peer. A Signaler may be
// shared by several endpoints as long as each endpoint uses a distinct peer ID.
type Signaler interface {
	// Open establishes a signaling connection for local. It must honor ctx
	// while connecting. The returned connection is owned by the caller.
	Open(ctx context.Context, local PeerID) (SignalConn, error)
}

// SignalConn is a bidirectional signaling connection.
//
// Implementations may reconnect internally, but must preserve these rules:
//
//   - Receive has exactly one caller at a time; the endpoint receive loop is
//     that caller.
//   - Send may be called concurrently with Receive.
//   - Send and Receive honor the supplied context.
//   - Close is idempotent and unblocks Send and Receive.
//
// Delivery is at-least-once and ordered when practical. Pipe tolerates
// duplicates and reordering, so implementations must not claim exactly-once
// delivery.
type SignalConn interface {
	// Send transmits msg. A nil error means the transport accepted the
	// message, not that the peer processed it.
	Send(ctx context.Context, msg Signal) error

	// Receive returns the next signal addressed to the local peer.
	Receive(ctx context.Context) (Signal, error)

	// Close releases the connection.
	Close() error
}
