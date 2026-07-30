package pipe

import (
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"

	"ella.to/pipe/internal/frame"
)

// Conn is an established pipe connection. It satisfies [net.Conn] over a
// reliable, ordered WebRTC DataChannel.
//
// Read and Write present a byte stream even though the underlying DataChannel is
// message-oriented, so a Read may return fewer bytes than a matching Write sent.
// Multiple goroutines may use a Conn concurrently: one reader and one writer run
// in parallel, and concurrent writers are serialized so that the frames of a
// single Write stay contiguous.
//
// Protocol version 1 has no half-close. Closing either side ends both
// directions.
type Conn struct {
	stream *frame.Stream

	peer      PeerID
	local     PeerID
	sessionID string
	role      role

	localAddr  Addr
	remoteAddr Addr

	// state is the coarse public state, published by the owning session.
	state atomic.Int32
	// closed guards the close path against duplicate teardown.
	closed atomic.Bool

	establishedAt   time.Time
	connectDuration time.Duration

	// snapshot is the session-owned part of the statistics, replaced whole so
	// that readers never observe a torn value.
	snapshot atomic.Pointer[sessionStats]

	// closeFn tears down the owning session. It is idempotent.
	closeFn func()
}

// sessionStats is the session-maintained portion of [ConnStats].
type sessionStats struct {
	ICERestarts     int
	LocalCandidate  CandidateType
	RemoteCandidate CandidateType
	KeepAliveRTT    time.Duration
}

var _ net.Conn = (*Conn)(nil)

// newConn builds the public connection around an open stream.
func newConn(s *session, stream *frame.Stream, connectDuration time.Duration) *Conn {
	c := &Conn{
		stream:          stream,
		peer:            s.peer,
		local:           s.ep.cfg.ID,
		sessionID:       s.id,
		role:            s.role,
		localAddr:       Addr{Peer: s.ep.cfg.ID, Session: s.id},
		remoteAddr:      Addr{Peer: s.peer, Session: s.id},
		establishedAt:   s.ep.cfg.clock.Now(),
		connectDuration: connectDuration,
		closeFn:         s.requestClose,
	}
	c.state.Store(int32(StateConnected))
	c.snapshot.Store(&sessionStats{})
	return c
}

// PeerID returns the remote peer ID.
func (c *Conn) PeerID() PeerID { return c.peer }

// SessionID returns the session identifier that correlates negotiation for this
// connection.
func (c *Conn) SessionID() string { return c.sessionID }

// State returns the coarse connection state.
func (c *Conn) State() ConnectionState { return ConnectionState(c.state.Load()) }

// Stats returns a snapshot of the connection's counters. Counters may advance
// between fields; a snapshot is never torn.
func (c *Conn) Stats() ConnStats {
	fs := c.stream.Stats()
	ss := c.snapshot.Load()
	return ConnStats{
		State:           c.State(),
		BytesRead:       fs.BytesRead,
		BytesWritten:    fs.BytesWritten,
		FramesRead:      fs.FramesRead,
		FramesWritten:   fs.FramesWritten,
		EstablishedAt:   c.establishedAt,
		ConnectDuration: c.connectDuration,
		ICERestarts:     ss.ICERestarts,
		LocalCandidate:  ss.LocalCandidate,
		RemoteCandidate: ss.RemoteCandidate,
		KeepAliveRTT:    ss.KeepAliveRTT,
	}
}

// Read implements [net.Conn].
func (c *Conn) Read(p []byte) (int, error) {
	n, err := c.stream.Read(p)
	if err != nil {
		return n, c.wrap("read", err)
	}
	return n, nil
}

// Write implements [net.Conn]. A successful call reports len(p). If some frames
// reached the peer before a failure, the returned count is the number of
// application bytes committed.
func (c *Conn) Write(p []byte) (int, error) {
	n, err := c.stream.Write(p)
	if err != nil {
		return n, c.wrap("write", err)
	}
	return n, nil
}

// Close implements [net.Conn]. It is idempotent, unblocks pending I/O, and
// releases the session, PeerConnection, and DataChannel that back the
// connection.
func (c *Conn) Close() error {
	first := c.closed.CompareAndSwap(false, true)
	c.state.Store(int32(StateClosing))
	err := c.stream.Close()
	if c.closeFn != nil {
		c.closeFn()
	}
	c.state.Store(int32(StateClosed))
	if !first || err == nil {
		return nil
	}
	return c.wrap("close", err)
}

// LocalAddr implements [net.Conn]. The address is logical: pipe has no
// IP-level identity of its own.
func (c *Conn) LocalAddr() net.Addr { return c.localAddr }

// RemoteAddr implements [net.Conn].
func (c *Conn) RemoteAddr() net.Addr { return c.remoteAddr }

// SetDeadline implements [net.Conn].
func (c *Conn) SetDeadline(t time.Time) error {
	return c.wrap("set", c.stream.SetDeadline(t))
}

// SetReadDeadline implements [net.Conn].
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.wrap("set", c.stream.SetReadDeadline(t))
}

// SetWriteDeadline implements [net.Conn].
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.wrap("set", c.stream.SetWriteDeadline(t))
}

// wrap turns a stream error into a categorized [net.OpError]. io.EOF passes
// through unchanged because callers rely on comparing it directly.
func (c *Conn) wrap(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		return io.EOF
	}

	var categorized error
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		categorized = deadlineError()
	case errors.Is(err, net.ErrClosed):
		categorized = err
	case errors.Is(err, frame.ErrProtocol):
		categorized = wrapErr(ErrProtocol, err, "pipe: stream protocol violation")
	default:
		categorized = wrapErr(ErrDisconnected, err, "pipe: stream failed")
	}
	return opError(op, c.localAddr, c.remoteAddr, categorized)
}

// setState publishes a new public state. Only the owning session calls it.
func (c *Conn) setState(s ConnectionState) {
	// A terminal state is never overwritten.
	for {
		cur := ConnectionState(c.state.Load())
		if cur == StateClosed {
			return
		}
		if c.state.CompareAndSwap(int32(cur), int32(s)) {
			return
		}
	}
}

// updateStats replaces the session-owned statistics. Only the owning session
// calls it.
func (c *Conn) updateStats(fn func(*sessionStats)) {
	cur := c.snapshot.Load()
	next := *cur
	fn(&next)
	c.snapshot.Store(&next)
}
