package pipe

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// NewID returns a fresh random 128-bit identifier in lowercase hexadecimal,
// suitable for [Signal.ID] and [Signal.SessionID]. It panics only if the system
// random source fails, which Go treats as unrecoverable.
func NewID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("pipe: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}

// RejectCode is the closed set of reasons for refusing a session.
type RejectCode string

// Reject codes defined by protocol version 1.
const (
	// RejectNotListening reports that the peer has no active listener.
	RejectNotListening RejectCode = "not_listening"
	// RejectBusy reports that the peer's accept backlog is full.
	RejectBusy RejectCode = "busy"
	// RejectUnauthorized reports that the peer refused the caller.
	RejectUnauthorized RejectCode = "unauthorized"
	// RejectUnsupportedVersion reports that the peers share no protocol
	// version.
	RejectUnsupportedVersion RejectCode = "unsupported_version"
	// RejectInvalidOffer reports that the offer was malformed or unusable.
	RejectInvalidOffer RejectCode = "invalid_offer"
	// RejectInternal reports a failure on the rejecting side.
	RejectInternal RejectCode = "internal"
)

func (c RejectCode) valid() bool {
	switch c {
	case RejectNotListening, RejectBusy, RejectUnauthorized,
		RejectUnsupportedVersion, RejectInvalidOffer, RejectInternal:
		return true
	}
	return false
}

// CloseCode is the closed set of reasons for terminating a session through
// signaling.
type CloseCode string

// Close codes defined by protocol version 1.
const (
	// CloseNormal reports an ordinary close initiated by the application.
	CloseNormal CloseCode = "normal"
	// CloseGoingAway reports that the endpoint is shutting down.
	CloseGoingAway CloseCode = "going_away"
	// CloseProtocolError reports that the peer violated the protocol.
	CloseProtocolError CloseCode = "protocol_error"
	// CloseTimeout reports that an operation exceeded its budget.
	CloseTimeout CloseCode = "timeout"
	// CloseInternal reports a failure on the closing side.
	CloseInternal CloseCode = "internal"
)

func (c CloseCode) valid() bool {
	switch c {
	case CloseNormal, CloseGoingAway, CloseProtocolError, CloseTimeout, CloseInternal:
		return true
	}
	return false
}

// sdpPayload is the body of an offer or answer signal.
type sdpPayload struct {
	SDP string `json:"sdp"`
}

// candidatePayload is the body of a candidate signal. The optional fields use
// pointers so that "absent" and "empty" stay distinguishable on the wire.
type candidatePayload struct {
	Candidate        string  `json:"candidate"`
	SDPMid           *string `json:"sdp_mid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdp_mline_index,omitempty"`
	UsernameFragment *string `json:"username_fragment,omitempty"`
}

// restartPayload is the body of a restart signal. It carries a new offer.
type restartPayload struct {
	Generation uint32 `json:"generation"`
	SDP        string `json:"sdp"`
}

// rejectPayload is the body of a reject signal.
type rejectPayload struct {
	Code   RejectCode `json:"code"`
	Reason string     `json:"reason"`
}

// closePayload is the body of a close signal.
type closePayload struct {
	Code   CloseCode `json:"code"`
	Reason string    `json:"reason"`
}

// encodePayload marshals a payload body. Payload encoding never fails for the
// types defined here, so a failure is reported as an internal protocol error.
func encodePayload(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, errorf(ErrProtocol, "pipe: encode signal payload: %w", err)
	}
	return raw, nil
}

// decodePayload unmarshals a payload body, tolerating unknown members so that
// additive version 1 fields do not break older peers.
func decodePayload(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return errorf(ErrProtocol, "pipe: signal payload is missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return errorf(ErrProtocol, "pipe: decode signal payload: %w", err)
	}
	// Reject trailing content so that a payload cannot smuggle a second value.
	if dec.More() {
		return errorf(ErrProtocol, "pipe: signal payload has trailing data")
	}
	return nil
}

// newSignal builds a validated envelope with a fresh message ID.
func newSignal(kind SignalKind, from, to PeerID, sessionID string, payload any) (Signal, error) {
	sig := Signal{
		Version:   ProtocolVersion,
		ID:        NewID(),
		SessionID: sessionID,
		Kind:      kind,
		From:      from,
		To:        to,
	}
	if payload != nil {
		raw, err := encodePayload(payload)
		if err != nil {
			return Signal{}, err
		}
		sig.Payload = raw
	}
	if err := sig.Validate(); err != nil {
		return Signal{}, fmt.Errorf("pipe: built invalid %s signal: %w", kind, err)
	}
	return sig, nil
}
