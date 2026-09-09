// Package frame implements the pipe stream framing defined in
// guides/11-protocol.md section 4 and turns a message-oriented DataChannel into
// a byte stream.
//
// Each DataChannel message carries exactly one frame:
//
//	0               1               2               3
//	+---------------+---------------+---------------+---------------+
//	| version (1)   | type (1)      | flags (2)                     |
//	+---------------+---------------+---------------+---------------+
//	| payload length (4, unsigned, network byte order)              |
//	+---------------------------------------------------------------+
//	| payload ...                                                   |
//	+---------------------------------------------------------------+
package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// HeaderSize is the fixed frame header length in bytes.
const HeaderSize = 8

// Version is the frame version implemented by this package. A change to the
// header layout requires a new version and a new DataChannel protocol label.
const Version uint8 = 1

// PayloadLimit is the largest payload this package will encode or decode,
// independent of configuration.
const PayloadLimit = 256 << 10

// PingSize is the length of the opaque nonce carried by ping and pong frames.
const PingSize = 8

// MaxCloseReason bounds the diagnostic reason in a close frame.
const MaxCloseReason = 256

// Type identifies the meaning of a frame.
type Type uint8

// Frame types defined by version 1.
const (
	// TypeData carries application bytes.
	TypeData Type = 0x01
	// TypePing carries a keepalive probe.
	TypePing Type = 0x02
	// TypePong echoes a probe nonce.
	TypePong Type = 0x03
	// TypeClose reports orderly termination.
	TypeClose Type = 0x04
)

// String implements [fmt.Stringer].
func (t Type) String() string {
	switch t {
	case TypeData:
		return "data"
	case TypePing:
		return "ping"
	case TypePong:
		return "pong"
	case TypeClose:
		return "close"
	default:
		return fmt.Sprintf("unknown(0x%02x)", uint8(t))
	}
}

func (t Type) valid() bool {
	switch t {
	case TypeData, TypePing, TypePong, TypeClose:
		return true
	}
	return false
}

// ErrProtocol reports a frame that violates the protocol. Every decoding error
// produced by this package matches it through [errors.Is].
var ErrProtocol = errors.New("frame: protocol violation")

// Close codes carried by a close frame.
const (
	// CloseNormal reports an ordinary close.
	CloseNormal uint16 = 0
	// CloseProtocolError reports that the peer violated the protocol.
	CloseProtocolError uint16 = 1
	// CloseInternal reports a failure on the closing side.
	CloseInternal uint16 = 2
	// CloseGoingAway reports that the endpoint is shutting down.
	CloseGoingAway uint16 = 3
)

// Header is a decoded frame header.
type Header struct {
	Version uint8
	Type    Type
	Flags   uint16
	Length  uint32
}

// Encode writes a complete frame into dst and returns the encoded prefix. dst
// must have room for HeaderSize+len(payload) bytes. Flags are zero in version 1.
func Encode(dst []byte, t Type, payload []byte) ([]byte, error) {
	total := HeaderSize + len(payload)
	if len(payload) > PayloadLimit {
		return nil, fmt.Errorf("%w: payload of %d bytes exceeds the %d byte limit",
			ErrProtocol, len(payload), PayloadLimit)
	}
	if len(dst) < total {
		return nil, fmt.Errorf("frame: destination of %d bytes is too small for %d bytes", len(dst), total)
	}
	dst[0] = Version
	dst[1] = uint8(t)
	binary.BigEndian.PutUint16(dst[2:4], 0)
	binary.BigEndian.PutUint32(dst[4:8], uint32(len(payload)))
	copy(dst[HeaderSize:], payload)
	return dst[:total], nil
}

// Append appends a complete frame to dst and returns the extended slice.
func Append(dst []byte, t Type, payload []byte) ([]byte, error) {
	if len(payload) > PayloadLimit {
		return nil, fmt.Errorf("%w: payload of %d bytes exceeds the %d byte limit",
			ErrProtocol, len(payload), PayloadLimit)
	}
	var hdr [HeaderSize]byte
	hdr[0] = Version
	hdr[1] = uint8(t)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...), nil
}

// Parse decodes the single frame contained in msg, which must be exactly one
// DataChannel message. maxPayload bounds the accepted payload length; it is
// clamped to [PayloadLimit].
//
// The returned payload aliases msg and stays valid only until msg is reused.
func Parse(msg []byte, maxPayload int) (Header, []byte, error) {
	if maxPayload < 0 || maxPayload > PayloadLimit {
		maxPayload = PayloadLimit
	}
	if len(msg) < HeaderSize {
		return Header{}, nil, fmt.Errorf("%w: frame of %d bytes is shorter than the %d byte header",
			ErrProtocol, len(msg), HeaderSize)
	}

	hdr := Header{
		Version: msg[0],
		Type:    Type(msg[1]),
		Flags:   binary.BigEndian.Uint16(msg[2:4]),
		Length:  binary.BigEndian.Uint32(msg[4:8]),
	}
	switch {
	case hdr.Version != Version:
		return Header{}, nil, fmt.Errorf("%w: unsupported frame version %d", ErrProtocol, hdr.Version)
	case !hdr.Type.valid():
		return Header{}, nil, fmt.Errorf("%w: unknown frame type %s", ErrProtocol, hdr.Type)
	case hdr.Flags != 0:
		return Header{}, nil, fmt.Errorf("%w: unknown frame flags 0x%04x", ErrProtocol, hdr.Flags)
	case hdr.Length > uint32(maxPayload):
		return Header{}, nil, fmt.Errorf("%w: payload length %d exceeds the negotiated %d byte limit",
			ErrProtocol, hdr.Length, maxPayload)
	case uint64(hdr.Length) != uint64(len(msg)-HeaderSize):
		return Header{}, nil, fmt.Errorf("%w: payload length %d does not match the %d bytes present",
			ErrProtocol, hdr.Length, len(msg)-HeaderSize)
	}

	payload := msg[HeaderSize : HeaderSize+int(hdr.Length)]
	switch hdr.Type {
	case TypePing, TypePong:
		if len(payload) != PingSize {
			return Header{}, nil, fmt.Errorf("%w: %s payload must be %d bytes, got %d",
				ErrProtocol, hdr.Type, PingSize, len(payload))
		}
	case TypeClose:
		if len(payload) < 2 {
			return Header{}, nil, fmt.Errorf("%w: close payload must carry a 2 byte code", ErrProtocol)
		}
	}
	return hdr, payload, nil
}

// CloseInfo is the decoded body of a close frame.
type CloseInfo struct {
	Code   uint16
	Reason string
}

// String implements [fmt.Stringer].
func (c CloseInfo) String() string {
	if c.Reason == "" {
		return fmt.Sprintf("code %d", c.Code)
	}
	return fmt.Sprintf("code %d: %s", c.Code, c.Reason)
}

// EncodeClose returns the payload of a close frame. The reason is truncated on a
// rune boundary to [MaxCloseReason] bytes.
func EncodeClose(code uint16, reason string) []byte {
	if len(reason) > MaxCloseReason {
		reason = reason[:MaxCloseReason]
		for len(reason) > 0 && !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	out := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(out[:2], code)
	copy(out[2:], reason)
	return out
}

// ParseClose decodes the payload of a close frame.
func ParseClose(payload []byte) (CloseInfo, error) {
	if len(payload) < 2 {
		return CloseInfo{}, fmt.Errorf("%w: close payload must carry a 2 byte code", ErrProtocol)
	}
	reason := payload[2:]
	if len(reason) > MaxCloseReason {
		return CloseInfo{}, fmt.Errorf("%w: close reason of %d bytes exceeds the %d byte limit",
			ErrProtocol, len(reason), MaxCloseReason)
	}
	if !utf8.Valid(reason) {
		return CloseInfo{}, fmt.Errorf("%w: close reason is not valid UTF-8", ErrProtocol)
	}
	return CloseInfo{Code: binary.BigEndian.Uint16(payload[:2]), Reason: string(reason)}, nil
}
