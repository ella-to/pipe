package frame

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// fakeChannel is a message-oriented, in-memory stand-in for a detached
// DataChannel. It mirrors the contract the real channel provides: one Read
// returns exactly one message, concurrent writes are atomic, a Close does not by
// itself interrupt a blocked Read, and a deadline in the past does.
type fakeChannel struct {
	mu     sync.Mutex
	queue  [][]byte
	notify chan struct{}

	// capacity bounds queued messages so that backpressure can be tested. Zero
	// means unbounded.
	capacity int
	drained  chan struct{}

	peer *fakeChannel

	closeOnce sync.Once
	closed    chan struct{}

	readDeadline  time.Time
	writeDeadline time.Time
	deadlineWake  chan struct{}

	// writeErr, when set, fails every write. readErr, when set, fails every
	// read once the queue is empty; it models an abrupt transport failure such
	// as an SCTP abort.
	writeErr error
	readErr  error
	writes   int
}

// newFakePair returns two connected fake channels.
func newFakePair(capacity int) (*fakeChannel, *fakeChannel) {
	a := newFakeChannel(capacity)
	b := newFakeChannel(capacity)
	a.peer, b.peer = b, a
	return a, b
}

func newFakeChannel(capacity int) *fakeChannel {
	return &fakeChannel{
		notify:       make(chan struct{}, 1),
		drained:      make(chan struct{}, 1),
		capacity:     capacity,
		closed:       make(chan struct{}),
		deadlineWake: make(chan struct{}, 1),
	}
}

// Read returns the next whole message. A buffer that is too small reports
// io.ErrShortBuffer instead of truncating. Queued messages are always delivered
// before a terminal condition, which is what the real SCTP stream does.
func (c *fakeChannel) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			msg := c.queue[0]
			if len(p) < len(msg) {
				c.mu.Unlock()
				return 0, io.ErrShortBuffer
			}
			c.queue = c.queue[1:]
			c.mu.Unlock()
			c.signalDrained()
			return copy(p, msg), nil
		}
		deadline, readErr := c.readDeadline, c.readErr
		c.mu.Unlock()

		if readErr != nil {
			return 0, readErr
		}
		if c.terminated() {
			return 0, io.EOF
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}

		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			timeout = timer.C
		}

		select {
		case <-c.notify:
		case <-c.deadlineWake:
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-c.closed:
		case <-c.peer.closed:
		}
	}
}

// terminated reports whether either end has closed, which resets the stream in
// both directions.
func (c *fakeChannel) terminated() bool {
	select {
	case <-c.closed:
		return true
	default:
	}
	select {
	case <-c.peer.closed:
		return true
	default:
		return false
	}
}

// Write delivers one message to the peer, blocking while the peer's queue is
// full so that backpressure is observable.
func (c *fakeChannel) Write(p []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}

	c.mu.Lock()
	err, deadline := c.writeErr, c.writeDeadline
	c.writes++
	c.mu.Unlock()

	if err != nil {
		return 0, err
	}

	msg := make([]byte, len(p))
	copy(msg, p)

	for {
		if c.peer.enqueue(msg) {
			return len(p), nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return 0, os.ErrDeadlineExceeded
		}

		var timeout <-chan time.Time
		if !deadline.IsZero() {
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			timeout = timer.C
		}

		select {
		case <-c.peer.drained:
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-c.closed:
			return 0, io.ErrClosedPipe
		case <-c.peer.closed:
			return 0, io.ErrClosedPipe
		}

		c.mu.Lock()
		deadline = c.writeDeadline
		c.mu.Unlock()
	}
}

// enqueue appends msg and reports whether there was room.
func (c *fakeChannel) enqueue(msg []byte) bool {
	c.mu.Lock()
	if c.capacity > 0 && len(c.queue) >= c.capacity {
		c.mu.Unlock()
		return false
	}
	c.queue = append(c.queue, msg)
	c.mu.Unlock()

	select {
	case c.notify <- struct{}{}:
	default:
	}
	return true
}

func (c *fakeChannel) signalDrained() {
	select {
	case c.drained <- struct{}{}:
	default:
	}
}

func (c *fakeChannel) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeChannel) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()

	select {
	case c.deadlineWake <- struct{}{}:
	default:
	}
	return nil
}

func (c *fakeChannel) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	return nil
}

func (c *fakeChannel) setWriteErr(err error) {
	c.mu.Lock()
	c.writeErr = err
	c.mu.Unlock()
}

// setReadErr makes subsequent reads fail once the queue drains, and wakes a
// reader that is already blocked.
func (c *fakeChannel) setReadErr(err error) {
	c.mu.Lock()
	c.readErr = err
	c.mu.Unlock()

	select {
	case c.deadlineWake <- struct{}{}:
	default:
	}
}

func (c *fakeChannel) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *fakeChannel) queueLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// isTimeoutErr reports whether err is a deadline error, matching what callers of
// a net.Conn expect.
func isTimeoutErr(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

var _ Channel = (*fakeChannel)(nil)
