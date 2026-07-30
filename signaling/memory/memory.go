// Package memory provides an in-process signaling hub for tests and
// same-process examples. It routes envelopes between peers registered on the
// same [Hub] without sockets, and it can inject faults deterministically so
// that duplicate, dropped, reordered, and disconnected signaling can be tested
// without timing luck.
package memory

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"ella.to/pipe"
)

// DefaultQueueSize is the per-peer inbound queue depth.
const DefaultQueueSize = 64

// Fault transforms one outbound signal into the signals actually delivered.
// Returning an empty slice drops the signal; returning several copies duplicates
// it. Implementations must be safe for concurrent use.
type Fault interface {
	Apply(sig pipe.Signal) []pipe.Signal
}

// FaultFunc adapts a function to [Fault].
type FaultFunc func(sig pipe.Signal) []pipe.Signal

// Apply implements [Fault].
func (f FaultFunc) Apply(sig pipe.Signal) []pipe.Signal { return f(sig) }

// Option configures a [Hub].
type Option func(*Hub)

// WithQueueSize sets the per-peer inbound queue depth. A send to a full queue
// blocks until the peer reads or the context ends.
func WithQueueSize(n int) Option {
	return func(h *Hub) {
		if n > 0 {
			h.queueSize = n
		}
	}
}

// WithFault installs a fault injector applied to every delivered signal.
func WithFault(f Fault) Option {
	return func(h *Hub) { h.fault = f }
}

// Hub is an in-process signaling relay. It implements [pipe.Signaler] and may
// be shared by any number of endpoints with distinct peer IDs.
type Hub struct {
	queueSize int
	fault     Fault

	mu    sync.Mutex
	peers map[pipe.PeerID]*conn
}

var _ pipe.Signaler = (*Hub)(nil)

// New returns an empty hub.
func New(opts ...Option) *Hub {
	h := &Hub{
		queueSize: DefaultQueueSize,
		peers:     make(map[pipe.PeerID]*conn),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Open registers local and returns its signaling connection. Registering a peer
// ID that is already live fails with [pipe.ErrDuplicatePeer].
func (h *Hub) Open(ctx context.Context, local pipe.PeerID) (pipe.SignalConn, error) {
	if local == "" {
		return nil, errors.New("memory: a local peer ID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, exists := h.peers[local]; exists {
		return nil, fmt.Errorf("memory: peer %q is already registered: %w", local, pipe.ErrDuplicatePeer)
	}

	c := &conn{
		hub:    h,
		local:  local,
		inbox:  make(chan pipe.Signal, h.queueSize),
		closed: make(chan struct{}),
	}
	h.peers[local] = c
	return c, nil
}

// Disconnect closes the signaling connection of one peer as if its transport had
// failed. It reports whether the peer was registered.
func (h *Hub) Disconnect(id pipe.PeerID) bool {
	h.mu.Lock()
	c, ok := h.peers[id]
	h.mu.Unlock()

	if !ok {
		return false
	}
	_ = c.Close()
	return true
}

// Registered returns the peer IDs with a live connection.
func (h *Hub) Registered() []pipe.PeerID {
	h.mu.Lock()
	defer h.mu.Unlock()

	out := make([]pipe.PeerID, 0, len(h.peers))
	for id := range h.peers {
		out = append(out, id)
	}
	return out
}

func (h *Hub) lookup(id pipe.PeerID) (*conn, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.peers[id]
	return c, ok
}

func (h *Hub) unregister(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.peers[c.local]; ok && cur == c {
		delete(h.peers, c.local)
	}
}

// conn is one peer's connection to the hub.
type conn struct {
	hub   *Hub
	local pipe.PeerID
	inbox chan pipe.Signal

	closeOnce sync.Once
	closed    chan struct{}
}

var _ pipe.SignalConn = (*conn)(nil)

// Send routes msg to its destination. It reports [pipe.ErrPeerUnavailable] when
// the destination has no live connection, and blocks while the destination's
// queue is full until it drains or ctx ends.
func (c *conn) Send(ctx context.Context, msg pipe.Signal) error {
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	if msg.From != c.local {
		return fmt.Errorf("memory: signal from %q sent on the connection of %q: %w",
			msg.From, c.local, pipe.ErrProtocol)
	}

	dst, ok := c.hub.lookup(msg.To)
	if !ok {
		return fmt.Errorf("memory: peer %q is not registered: %w", msg.To, pipe.ErrPeerUnavailable)
	}

	deliveries := []pipe.Signal{msg}
	if c.hub.fault != nil {
		deliveries = c.hub.fault.Apply(msg)
	}

	for _, sig := range deliveries {
		select {
		case dst.inbox <- sig:
		case <-dst.closed:
			return fmt.Errorf("memory: peer %q disconnected: %w", msg.To, pipe.ErrPeerUnavailable)
		case <-c.closed:
			return net.ErrClosed
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Receive returns the next signal addressed to this peer.
func (c *conn) Receive(ctx context.Context) (pipe.Signal, error) {
	select {
	case sig := <-c.inbox:
		return sig, nil
	case <-c.closed:
		return pipe.Signal{}, net.ErrClosed
	case <-ctx.Done():
		return pipe.Signal{}, ctx.Err()
	}
}

// Close unregisters the peer and unblocks Send and Receive. It is idempotent.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.hub.unregister(c)
	})
	return nil
}

// DuplicateAll delivers every signal twice, which exercises deduplication.
func DuplicateAll() Fault {
	return FaultFunc(func(sig pipe.Signal) []pipe.Signal {
		return []pipe.Signal{sig, sig}
	})
}

// DropKind drops every signal of the given kinds.
func DropKind(kinds ...pipe.SignalKind) Fault {
	drop := make(map[pipe.SignalKind]bool, len(kinds))
	for _, k := range kinds {
		drop[k] = true
	}
	return FaultFunc(func(sig pipe.Signal) []pipe.Signal {
		if drop[sig.Kind] {
			return nil
		}
		return []pipe.Signal{sig}
	})
}

// DropFirst drops the first n signals of the given kind and delivers the rest.
func DropFirst(kind pipe.SignalKind, n int) Fault {
	var mu sync.Mutex
	remaining := n
	return FaultFunc(func(sig pipe.Signal) []pipe.Signal {
		if sig.Kind != kind {
			return []pipe.Signal{sig}
		}
		mu.Lock()
		defer mu.Unlock()
		if remaining > 0 {
			remaining--
			return nil
		}
		return []pipe.Signal{sig}
	})
}

// SwapAdjacent holds back every other signal of the given kind and releases it
// after the next one, which reorders delivery deterministically. An odd final
// signal stays held, so this fault also drops at most one signal per kind.
func SwapAdjacent(kind pipe.SignalKind) Fault {
	var (
		mu   sync.Mutex
		held *pipe.Signal
	)
	return FaultFunc(func(sig pipe.Signal) []pipe.Signal {
		if sig.Kind != kind {
			return []pipe.Signal{sig}
		}
		mu.Lock()
		defer mu.Unlock()

		if held == nil {
			copied := sig
			held = &copied
			return nil
		}
		out := []pipe.Signal{sig, *held}
		held = nil
		return out
	})
}
