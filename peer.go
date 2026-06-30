package pipe

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"ella.to/pipe/signal"
)

// ErrEmptyPeerID is returned when a peer id (local or remote) is empty.
var ErrEmptyPeerID = errors.New("peer id cannot be empty")

// ErrSelfPeer is returned when WriteTo targets the peer's own id.
var ErrSelfPeer = errors.New("cannot send a datagram to self")

// ErrDatagramTooLarge is returned when a datagram exceeds maxDatagramSize.
var ErrDatagramTooLarge = errors.New("datagram exceeds maximum size")

// maxDatagramSize bounds a single framed datagram (and the handshake frame) to
// keep per-message allocations bounded.
const maxDatagramSize = 1 << 20 // 1 MiB

const (
	defaultPeerIdleTimeout = 30 * time.Second
	defaultPeerDialTimeout = 30 * time.Second
	defaultPeerRecvBuffer  = 256
)

// Peer is a string-addressed, datagram-oriented overlay built on top of the
// WebRTC Conn/signal machinery. It behaves like a net.PacketConn keyed by peer
// id rather than by net.Addr:
//
//	WriteTo(peerID, b) sends datagram b to peerID, dialing it on demand.
//	ReadFrom(b)        returns the next datagram from any peer plus its sender.
//	Close()            tears everything down and detaches from the signal server.
//
// Internally a Peer is both a Dialer and a Listener using the same id. Each
// remote peer is reached over a single bidirectional Conn; the initiator sends
// a one-frame handshake carrying its id so the acceptor learns who connected.
// Sessions idle for longer than the configured idle timeout are evicted, and a
// later WriteTo transparently re-establishes the connection through the signal
// channel.
//
// Once Close has been called, WriteTo and ReadFrom return io.ErrClosedPipe.
type Peer struct {
	id  string
	cfg peerConfig

	dialer   Dialer
	listener Listener

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*peerSession  // remote id -> live session
	conns    map[*Conn]struct{}       // every open conn, for guaranteed cleanup
	pending  map[string]chan struct{} // remote id -> in-flight dial barrier

	recv   chan inboundDatagram
	done   chan struct{}
	closed atomic.Bool

	wg sync.WaitGroup
}

type peerConfig struct {
	idleTimeout    time.Duration
	dialTimeout    time.Duration
	recvBufferSize int
	webrtc         *WebRTCOptions
}

func defaultPeerConfig() peerConfig {
	return peerConfig{
		idleTimeout:    defaultPeerIdleTimeout,
		dialTimeout:    defaultPeerDialTimeout,
		recvBufferSize: defaultPeerRecvBuffer,
		// Keep only the main (listening) inbox alive once a data channel is up;
		// per-conn signaling inboxes are closed after connection.
		webrtc: &WebRTCOptions{DisconnectSignalOnConnected: true},
	}
}

// PeerOption configures a Peer at construction time.
type PeerOption func(*peerConfig)

// WithPeerIdleTimeout sets how long a session may be idle (no successful read
// or write) before it is evicted. Defaults to 30s.
func WithPeerIdleTimeout(d time.Duration) PeerOption {
	return func(c *peerConfig) {
		if d > 0 {
			c.idleTimeout = d
		}
	}
}

// WithPeerDialTimeout bounds how long an on-demand dial may take. Defaults to 30s.
func WithPeerDialTimeout(d time.Duration) PeerOption {
	return func(c *peerConfig) {
		if d > 0 {
			c.dialTimeout = d
		}
	}
}

// WithPeerRecvBuffer sets the number of inbound datagrams that may be buffered
// before read backpressure is applied to senders. Defaults to 256.
func WithPeerRecvBuffer(n int) PeerOption {
	return func(c *peerConfig) {
		if n > 0 {
			c.recvBufferSize = n
		}
	}
}

// WithPeerWebRTCOptions overrides the WebRTC options used for both dialing and
// listening.
func WithPeerWebRTCOptions(opts *WebRTCOptions) PeerOption {
	return func(c *peerConfig) {
		if opts != nil {
			c.webrtc = opts
		}
	}
}

type peerSession struct {
	remoteID   string
	conn       *Conn
	writeMu    sync.Mutex
	lastActive atomic.Int64 // unix nanos of last successful read/write
}

func (s *peerSession) touch() {
	s.lastActive.Store(time.Now().UnixNano())
}

type inboundDatagram struct {
	from string
	data []byte
}

// NewPeer creates a Peer with the given id over the provided signal channel.
// The same signal channel is used both to accept inbound connections (on the
// main inbox derived from id) and to dial peers on demand.
func NewPeer(id string, sig signal.Signal, opts ...PeerOption) (*Peer, error) {
	if id == "" {
		return nil, ErrEmptyPeerID
	}
	if sig == nil {
		return nil, errors.New("signal channel cannot be nil")
	}

	cfg := defaultPeerConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	dialer, err := CreateDialerWithOptions(id, sig, cfg.webrtc)
	if err != nil {
		return nil, err
	}

	listener, err := CreateListenerWithOptions(id, sig, cfg.webrtc)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	p := &Peer{
		id:       id,
		cfg:      cfg,
		dialer:   dialer,
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
		sessions: make(map[string]*peerSession),
		conns:    make(map[*Conn]struct{}),
		pending:  make(map[string]chan struct{}),
		recv:     make(chan inboundDatagram, cfg.recvBufferSize),
		done:     make(chan struct{}),
	}

	p.wg.Add(2)
	go p.acceptLoop()
	go p.janitor()

	return p, nil
}

// ID returns the peer's own id.
func (p *Peer) ID() string {
	return p.id
}

// ConnectedPeers returns the ids of peers with a currently live session.
func (p *Peer) ConnectedPeers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.sessions))
	for id := range p.sessions {
		out = append(out, id)
	}
	return out
}

// WriteTo sends datagram b to peerID, establishing (or re-establishing) a
// connection through the signal channel if one is not already live. It returns
// len(b) on success. After Close, it returns io.ErrClosedPipe.
func (p *Peer) WriteTo(peerID string, b []byte) (int, error) {
	if p.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if peerID == "" {
		return 0, ErrEmptyPeerID
	}
	if peerID == p.id {
		return 0, ErrSelfPeer
	}
	if len(b) > maxDatagramSize {
		return 0, ErrDatagramTooLarge
	}

	var lastErr error
	// Two attempts: the first may reuse a cached (possibly dead) session; on
	// failure we evict it and the second attempt re-dials from scratch.
	for range 2 {
		s, err := p.getOrDial(peerID)
		if err != nil {
			return 0, err
		}

		s.writeMu.Lock()
		werr := writeFrame(s.conn, b)
		s.writeMu.Unlock()

		if werr == nil {
			s.touch()
			return len(b), nil
		}

		lastErr = werr
		p.evict(peerID, s)
	}

	return 0, lastErr
}

// ReadFrom returns the next datagram received from any peer, along with the
// sender's id. If the supplied buffer is smaller than the datagram, the excess
// is discarded (net.PacketConn semantics). After Close, it returns
// io.ErrClosedPipe.
func (p *Peer) ReadFrom(b []byte) (n int, from string, err error) {
	if p.closed.Load() {
		return 0, "", io.ErrClosedPipe
	}

	select {
	case <-p.done:
		return 0, "", io.ErrClosedPipe
	case dg := <-p.recv:
		return copy(b, dg.data), dg.from, nil
	}
}

// Close detaches the peer from the signal server and tears down every session.
// It is safe to call multiple times; subsequent calls are no-ops.
func (p *Peer) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}

	close(p.done)
	p.cancel()

	p.mu.Lock()
	conns := make([]*Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	p.conns = make(map[*Conn]struct{})
	p.sessions = make(map[string]*peerSession)
	p.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}

	p.wg.Wait()
	return nil
}

// acceptLoop continuously accepts inbound connections on the peer's main inbox.
func (p *Peer) acceptLoop() {
	defer p.wg.Done()

	for {
		conn, err := p.listener.Listen(p.ctx)
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			slog.Debug("peer accept failed; retrying", "id", p.id, "err", err)
			select {
			case <-time.After(200 * time.Millisecond):
			case <-p.done:
				return
			}
			continue
		}

		if !p.trackConn(conn) {
			_ = conn.Close()
			return
		}

		p.wg.Add(1)
		go p.handleInbound(conn)
	}
}

// handleInbound waits for an accepted conn to become ready, reads the initiator
// handshake to learn the remote id, registers the session, then reads datagrams.
func (p *Peer) handleInbound(conn *Conn) {
	defer p.wg.Done()

	if err := conn.WaitReady(p.ctx); err != nil {
		p.closeConn(conn)
		return
	}

	raw, err := readFrame(conn)
	if err != nil || len(raw) == 0 {
		p.closeConn(conn)
		return
	}
	remoteID := string(raw)

	s := &peerSession{remoteID: remoteID, conn: conn}
	s.touch()

	// replace=true: a re-dial after eviction must supersede any stale session.
	chosen, ok := p.register(remoteID, s, true)
	if !ok || chosen != s {
		p.closeConn(conn)
		return
	}

	p.readLoop(s)
}

// readLoop reads framed datagrams from a session and delivers them to ReadFrom.
func (p *Peer) readLoop(s *peerSession) {
	for {
		data, err := readFrame(s.conn)
		if err != nil {
			p.evict(s.remoteID, s)
			return
		}
		s.touch()

		select {
		case p.recv <- inboundDatagram{from: s.remoteID, data: data}:
		case <-p.done:
			return
		}
	}
}

// getOrDial returns a live session for peerID, dialing on demand. Concurrent
// callers for the same peer share a single in-flight dial.
func (p *Peer) getOrDial(peerID string) (*peerSession, error) {
	for {
		p.mu.Lock()
		if p.closed.Load() {
			p.mu.Unlock()
			return nil, io.ErrClosedPipe
		}
		if s := p.sessions[peerID]; s != nil {
			p.mu.Unlock()
			return s, nil
		}
		if ch := p.pending[peerID]; ch != nil {
			p.mu.Unlock()
			select {
			case <-ch:
				continue // re-check; the in-flight dial finished
			case <-p.done:
				return nil, io.ErrClosedPipe
			}
		}

		ch := make(chan struct{})
		p.pending[peerID] = ch
		p.mu.Unlock()

		s, err := p.dial(peerID)

		p.mu.Lock()
		delete(p.pending, peerID)
		p.mu.Unlock()
		close(ch)

		return s, err
	}
}

// dial establishes a new outbound session to peerID and starts its read loop.
func (p *Peer) dial(peerID string) (*peerSession, error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.cfg.dialTimeout)
	defer cancel()

	conn, err := p.dialer.Dial(ctx, peerID)
	if err != nil {
		return nil, err
	}

	if !p.trackConn(conn) {
		_ = conn.Close()
		return nil, io.ErrClosedPipe
	}

	// Handshake: announce our id so the remote's acceptor can tag datagrams.
	if err := writeFrame(conn, []byte(p.id)); err != nil {
		p.closeConn(conn)
		return nil, err
	}

	s := &peerSession{remoteID: peerID, conn: conn}
	s.touch()

	// replace=false: if another path established a session for peerID while we
	// were dialing, use theirs and discard ours.
	chosen, ok := p.register(peerID, s, false)
	if !ok {
		p.closeConn(conn)
		return nil, io.ErrClosedPipe
	}
	if chosen != s {
		p.closeConn(conn)
		return chosen, nil
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.readLoop(s)
	}()

	return s, nil
}

// register installs s as the session for remoteID. When replace is false and a
// session already exists, the existing one is returned unchanged. When replace
// is true, any existing session's conn is closed and s takes its place. The
// bool is false only when the peer has been closed.
func (p *Peer) register(remoteID string, s *peerSession, replace bool) (*peerSession, bool) {
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return nil, false
	}
	old := p.sessions[remoteID]
	if old != nil && !replace {
		p.mu.Unlock()
		return old, true
	}
	p.sessions[remoteID] = s
	p.mu.Unlock()

	if old != nil && old != s {
		p.closeConn(old.conn)
	}
	return s, true
}

// evict removes s as the session for remoteID (only if still current) and
// closes its conn.
func (p *Peer) evict(remoteID string, s *peerSession) {
	p.mu.Lock()
	if cur, ok := p.sessions[remoteID]; ok && cur == s {
		delete(p.sessions, remoteID)
	}
	delete(p.conns, s.conn)
	p.mu.Unlock()

	_ = s.conn.Close()
}

// trackConn records conn for guaranteed cleanup on Close. It returns false if
// the peer is already closed, in which case the caller must close conn.
func (p *Peer) trackConn(conn *Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed.Load() {
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}

func (p *Peer) closeConn(conn *Conn) {
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
	_ = conn.Close()
}

// janitor evicts sessions that have been idle longer than the idle timeout.
func (p *Peer) janitor() {
	defer p.wg.Done()

	interval := max(p.cfg.idleTimeout/2, 20*time.Millisecond)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-p.cfg.idleTimeout).UnixNano()

			var stale []*peerSession
			p.mu.Lock()
			for _, s := range p.sessions {
				if s.lastActive.Load() < cutoff {
					stale = append(stale, s)
				}
			}
			p.mu.Unlock()

			for _, s := range stale {
				slog.Debug("evicting idle peer session", "id", p.id, "remote", s.remoteID)
				p.evict(s.remoteID, s)
			}
		}
	}
}

// writeFrame writes a length-prefixed frame (4-byte big-endian length + body).
func writeFrame(w io.Writer, b []byte) error {
	if len(b) > maxDatagramSize {
		return ErrDatagramTooLarge
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(b)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return writeAll(w, b)
}

// readFrame reads a single length-prefixed frame. The returned slice is freshly
// allocated and owned by the caller.
func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > maxDatagramSize {
		return nil, ErrDatagramTooLarge
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, int(size))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
