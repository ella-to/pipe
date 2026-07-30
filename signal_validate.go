package pipe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Protocol limits. They are hard bounds, not tunables: every field is checked
// against them before Pipe allocates or routes anything.
const (
	// MaxEnvelopeSize is the largest encoded signaling envelope a transport
	// should accept, in bytes.
	MaxEnvelopeSize = 256 << 10

	// MaxSDPSize is the largest SDP body accepted in an offer, answer, or
	// restart payload, in bytes.
	MaxSDPSize = 128 << 10

	// MaxCandidateSize is the largest ICE candidate string accepted in a
	// candidate payload, in bytes.
	MaxCandidateSize = 8 << 10

	// MaxPeerIDLength is the largest accepted peer ID, in bytes.
	MaxPeerIDLength = 128

	// MaxReasonLength is the largest accepted diagnostic reason string in a
	// reject or close payload, in bytes.
	MaxReasonLength = 512

	// maxUsernameFragmentLength bounds the optional ICE username fragment.
	maxUsernameFragmentLength = 256

	// maxPendingCandidates is the number of remote candidates a session
	// buffers before its remote description is applied.
	maxPendingCandidates = 256
)

// Validate reports whether the envelope is structurally valid for protocol
// version 1. It checks the version, identifiers, kind, peer names, and the
// kind-specific payload against the documented limits.
//
// Validate does not know which peer is local; endpoints additionally require
// that To names the local peer.
func (s Signal) Validate() error {
	if s.Version != ProtocolVersion {
		return errorf(ErrProtocol, "pipe: unsupported signal version %d", s.Version)
	}
	if !validID(s.ID) {
		return errorf(ErrProtocol, "pipe: invalid signal id")
	}
	if !validID(s.SessionID) {
		return errorf(ErrProtocol, "pipe: invalid session id")
	}
	if err := validPeerID(s.From); err != nil {
		return errorf(ErrProtocol, "pipe: invalid from: %w", err)
	}
	if err := validPeerID(s.To); err != nil {
		return errorf(ErrProtocol, "pipe: invalid to: %w", err)
	}
	if s.From == s.To {
		return errorf(ErrProtocol, "pipe: signal addressed to its own sender")
	}
	if len(s.Payload) > MaxEnvelopeSize {
		return errorf(ErrProtocol, "pipe: signal payload of %d bytes exceeds the %d byte limit",
			len(s.Payload), MaxEnvelopeSize)
	}
	return s.validatePayload()
}

func (s Signal) validatePayload() error {
	switch s.Kind {
	case KindOffer, KindAnswer:
		var p sdpPayload
		if err := decodePayload(s.Payload, &p); err != nil {
			return err
		}
		return validSDP(p.SDP)

	case KindCandidate:
		var p candidatePayload
		if err := decodePayload(s.Payload, &p); err != nil {
			return err
		}
		switch {
		case p.Candidate == "":
			return errorf(ErrProtocol, "pipe: candidate is empty; use %q for end-of-candidates", KindICEComplete)
		case len(p.Candidate) > MaxCandidateSize:
			return errorf(ErrProtocol, "pipe: candidate of %d bytes exceeds the %d byte limit",
				len(p.Candidate), MaxCandidateSize)
		case !utf8.ValidString(p.Candidate):
			return errorf(ErrProtocol, "pipe: candidate is not valid UTF-8")
		}
		if p.UsernameFragment != nil && len(*p.UsernameFragment) > maxUsernameFragmentLength {
			return errorf(ErrProtocol, "pipe: username fragment exceeds the %d byte limit",
				maxUsernameFragmentLength)
		}
		return nil

	case KindICEComplete:
		if len(s.Payload) != 0 && string(s.Payload) != "null" && string(s.Payload) != "{}" {
			return errorf(ErrProtocol, "pipe: %q must not carry a payload", KindICEComplete)
		}
		return nil

	case KindRestart:
		var p restartPayload
		if err := decodePayload(s.Payload, &p); err != nil {
			return err
		}
		if p.Generation == 0 {
			return errorf(ErrProtocol, "pipe: restart generation must be greater than zero")
		}
		return validSDP(p.SDP)

	case KindReject:
		var p rejectPayload
		if err := decodePayload(s.Payload, &p); err != nil {
			return err
		}
		if !p.Code.valid() {
			return errorf(ErrProtocol, "pipe: unknown reject code %q", p.Code)
		}
		return validReason(p.Reason)

	case KindClose:
		var p closePayload
		if err := decodePayload(s.Payload, &p); err != nil {
			return err
		}
		if !p.Code.valid() {
			return errorf(ErrProtocol, "pipe: unknown close code %q", p.Code)
		}
		return validReason(p.Reason)

	default:
		return errorf(ErrProtocol, "pipe: unknown signal kind %q", s.Kind)
	}
}

func validSDP(sdp string) error {
	switch {
	case sdp == "":
		return errorf(ErrProtocol, "pipe: sdp is empty")
	case len(sdp) > MaxSDPSize:
		return errorf(ErrProtocol, "pipe: sdp of %d bytes exceeds the %d byte limit", len(sdp), MaxSDPSize)
	case !utf8.ValidString(sdp):
		return errorf(ErrProtocol, "pipe: sdp is not valid UTF-8")
	}
	return nil
}

func validReason(reason string) error {
	switch {
	case len(reason) > MaxReasonLength:
		return errorf(ErrProtocol, "pipe: reason of %d bytes exceeds the %d byte limit",
			len(reason), MaxReasonLength)
	case !utf8.ValidString(reason):
		return errorf(ErrProtocol, "pipe: reason is not valid UTF-8")
	}
	return nil
}

// validID accepts a 128-bit identifier written as 32 lowercase hex digits or as
// a canonical lowercase UUID.
func validID(id string) bool {
	switch len(id) {
	case 32:
		return allHexLower(id)
	case 36:
		for i, r := range id {
			switch i {
			case 8, 13, 18, 23:
				if r != '-' {
					return false
				}
			default:
				if !isHexLower(byte(r)) {
					return false
				}
			}
		}
		return true
	default:
		return false
	}
}

func allHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexLower(s[i]) {
			return false
		}
	}
	return true
}

func isHexLower(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// validPeerID accepts a non-empty, bounded, printable UTF-8 name. Control
// characters and surrounding whitespace are rejected so that peer IDs stay safe
// to embed in logs and routing tables.
func validPeerID(id PeerID) error {
	switch {
	case id == "":
		return errorf(ErrProtocol, "peer id is empty")
	case len(id) > MaxPeerIDLength:
		return errorf(ErrProtocol, "peer id of %d bytes exceeds the %d byte limit", len(id), MaxPeerIDLength)
	case !utf8.ValidString(string(id)):
		return errorf(ErrProtocol, "peer id is not valid UTF-8")
	}
	for _, r := range string(id) {
		if unicode.IsControl(r) {
			return errorf(ErrProtocol, "peer id contains a control character")
		}
	}
	if strings.TrimSpace(string(id)) != string(id) {
		return errorf(ErrProtocol, "peer id has leading or trailing whitespace")
	}
	return nil
}
