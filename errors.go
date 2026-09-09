package pipe

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// Sentinel error categories. Every error returned by this package matches at
// least one of them through [errors.Is], so callers never need to compare
// strings.
var (
	// ErrClosed reports use of an endpoint, listener, or connection that has
	// been closed. It is [net.ErrClosed] so that generic networking code keeps
	// working.
	ErrClosed = net.ErrClosed

	// ErrTimeout reports that an operation exceeded a deadline or a configured
	// timeout. Errors in this category also satisfy [net.Error] with
	// Timeout reporting true.
	ErrTimeout = errors.New("pipe: timeout")

	// ErrSignaling reports a failure of the signaling transport, such as a
	// closed signaling connection or a rejected send.
	ErrSignaling = errors.New("pipe: signaling failure")

	// ErrNegotiation reports a failure while establishing the session:
	// offer/answer exchange, DataChannel setup, or detach.
	ErrNegotiation = errors.New("pipe: negotiation failure")

	// ErrICE reports that connectivity establishment failed or was lost.
	ErrICE = errors.New("pipe: ICE failure")

	// ErrProtocol reports that a peer violated the signaling or stream
	// protocol, including invalid envelopes and malformed frames.
	ErrProtocol = errors.New("pipe: protocol violation")

	// ErrPeerRejected reports that the remote peer explicitly refused the
	// session, for example because it is not listening, its backlog is full,
	// or its [Config.AllowPeer] returned false. Use [errors.As] with
	// [*RejectedError] to read the [RejectCode].
	ErrPeerRejected = errors.New("pipe: peer rejected the session")

	// ErrPeerUnavailable reports that signaling could not reach the peer.
	ErrPeerUnavailable = errors.New("pipe: peer unavailable")

	// ErrDuplicatePeer reports that the peer ID is already registered with the
	// signaling transport.
	ErrDuplicatePeer = errors.New("pipe: duplicate peer registration")

	// ErrDisconnected reports that an established connection was lost and
	// could not be recovered within the configured policy.
	ErrDisconnected = errors.New("pipe: connection lost")

	// ErrConfig reports invalid configuration passed to [New].
	ErrConfig = errors.New("pipe: invalid configuration")

	// ErrAlreadyListening is returned by the second and later calls to
	// [Endpoint.Listen] on the same endpoint.
	ErrAlreadyListening = errors.New("pipe: endpoint is already listening")

	// ErrNotImplemented reports functionality that this build does not
	// provide yet. It never appears on a supported code path.
	ErrNotImplemented = errors.New("pipe: not implemented")
)

// categorized carries an error category alongside the underlying cause so that
// both are visible to errors.Is and errors.As.
type categorized struct {
	category error
	cause    error
	msg      string
}

func (e *categorized) Error() string {
	switch {
	case e.msg != "" && e.cause != nil:
		return e.msg + ": " + e.cause.Error()
	case e.msg != "":
		return e.msg
	case e.cause != nil:
		return e.category.Error() + ": " + e.cause.Error()
	default:
		return e.category.Error()
	}
}

// Unwrap exposes both the category and the cause, so errors.Is matches either.
func (e *categorized) Unwrap() []error {
	if e.cause == nil {
		return []error{e.category}
	}
	return []error{e.category, e.cause}
}

// Timeout implements the timeout half of [net.Error].
func (e *categorized) Timeout() bool {
	if errors.Is(e.category, ErrTimeout) {
		return true
	}
	var ne net.Error
	return errors.As(e.cause, &ne) && ne.Timeout()
}

// Temporary implements the deprecated half of [net.Error]. Pipe never
// reports temporary errors; callers should inspect the category instead.
func (e *categorized) Temporary() bool { return false }

// errorf builds a categorized error with a formatted message. A %w verb in the
// format string sets the cause.
func errorf(category error, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	return &categorized{category: category, cause: errors.Unwrap(err), msg: err.Error()}
}

// wrapErr attaches a category to an existing cause.
func wrapErr(category error, cause error, msg string) error {
	if cause == nil {
		return nil
	}
	return &categorized{category: category, cause: cause, msg: msg}
}

// deadlineError is the error reported when a read or write deadline expires.
// It satisfies both [os.ErrDeadlineExceeded] and [ErrTimeout].
func deadlineError() error {
	return &categorized{category: ErrTimeout, cause: os.ErrDeadlineExceeded, msg: "pipe: deadline exceeded"}
}

// opError decorates err with the operation, network, and logical addresses so
// that it behaves like an error from the net package.
func opError(op string, local, remote net.Addr, err error) error {
	if err == nil {
		return nil
	}
	return &net.OpError{Op: op, Net: networkName, Source: local, Addr: remote, Err: err}
}
