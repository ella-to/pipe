package sse

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"ella.to/pipe"
	"ella.to/sse"
)

// Defaults for [Config].
const (
	// DefaultQueueSize bounds signals queued for a peer that has not read them
	// yet. A negotiation produces a few dozen signals, so this covers several
	// concurrent sessions per peer with room to spare.
	DefaultQueueSize = 256

	// DefaultReplayDepth is how many already-delivered signals the server keeps
	// per peer so that a reconnecting stream can catch up.
	DefaultReplayDepth = 256

	// DefaultOfflineGrace is how long a peer with no attached stream keeps its
	// queue before it is forgotten.
	DefaultOfflineGrace = 30 * time.Second

	// DefaultKeepAlive is the interval between comment lines on an idle
	// stream. It keeps proxies from closing the connection and lets clients
	// detect a dead one.
	DefaultKeepAlive = 15 * time.Second

	// writeTimeout bounds one write to a stream, so that a stalled client cannot
	// hold the goroutine forever.
	writeTimeout = 10 * time.Second

	// maxBodySlack is the extra room allowed over pipe.MaxEnvelopeSize for the
	// envelope fields around the payload.
	maxBodySlack = 4 << 10
)

// Event names used on the stream.
const (
	// eventHello is the first event on every stream; its data is the
	// JSON-quoted peer ID the server authenticated.
	eventHello = "hello"
	// eventSignal carries one JSON-encoded [pipe.Signal].
	eventSignal = "signal"
	// eventReplaced tells a stream that a newer stream took over its peer.
	eventReplaced = "replaced"
	// eventShutdown tells a stream that the server is going away.
	eventShutdown = "shutdown"
)

// Config configures a [Server]. Only Authenticator is required.
type Config struct {
	// Authenticator establishes the identity behind every request.
	Authenticator Authenticator

	// QueueSize bounds undelivered signals per peer. It defaults to
	// [DefaultQueueSize].
	QueueSize int

	// ReplayDepth bounds delivered signals kept for reconnecting streams. It
	// defaults to [DefaultReplayDepth].
	ReplayDepth int

	// OfflineGrace is how long a detached peer keeps its queue. It defaults to
	// [DefaultOfflineGrace].
	OfflineGrace time.Duration

	// KeepAlive is the interval between keepalive comments on an idle stream.
	// It defaults to [DefaultKeepAlive].
	KeepAlive time.Duration

	// MaxPeers bounds concurrently known peers. Zero means unlimited.
	MaxPeers int

	// Logger receives structured logs. Nothing is logged when nil. Tokens,
	// SDP, and candidates are never logged at any level.
	Logger *slog.Logger
}

// Server routes signals between authenticated peers. It implements
// [http.Handler]; mount it at one path and hand that URL to clients.
type Server struct {
	cfg Config
	log *slog.Logger

	mu     sync.Mutex
	peers  map[pipe.PeerID]*peer
	closed bool
}

// NewServer validates cfg and returns a server ready to be mounted.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Authenticator == nil {
		return nil, errors.New("sse: Config.Authenticator is required")
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.ReplayDepth < 0 {
		return nil, errors.New("sse: Config.ReplayDepth must not be negative")
	}
	if cfg.ReplayDepth == 0 {
		cfg.ReplayDepth = DefaultReplayDepth
	}
	if cfg.OfflineGrace <= 0 {
		cfg.OfflineGrace = DefaultOfflineGrace
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = DefaultKeepAlive
	}
	if cfg.MaxPeers < 0 {
		return nil, errors.New("sse: Config.MaxPeers must not be negative")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg:   cfg,
		log:   cfg.Logger,
		peers: make(map[pipe.PeerID]*peer),
	}, nil
}

// Peers returns the IDs of peers currently known to the server, attached or
// within their offline grace period, in sorted order.
func (s *Server) Peers() []pipe.PeerID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]pipe.PeerID, 0, len(s.peers))
	for id := range s.peers {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Close detaches every stream and forgets every peer. In-flight handlers
// return; the server refuses requests afterwards with 503.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	peers := make([]*peer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	s.peers = make(map[pipe.PeerID]*peer)
	s.mu.Unlock()

	for _, p := range peers {
		p.shutdown()
	}
	return nil
}

// ServeHTTP implements [http.Handler].
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.isClosed() {
		http.Error(w, "signaling server is shutting down", http.StatusServiceUnavailable)
		return
	}

	id, err := s.cfg.Authenticator.Authenticate(r)
	if err != nil {
		s.log.Debug("sse: rejected request", slog.String("error", err.Error()))
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if claimed := r.Header.Get(PeerHeader); claimed != "" && pipe.PeerID(claimed) != id {
		s.log.Debug("sse: peer header does not match credential",
			slog.String("authenticated", string(id)), slog.String("claimed", claimed))
		http.Error(w, "credential does not belong to the claimed peer", http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.serveStream(w, r, id)
	case http.MethodPost:
		s.serveSend(w, r, id)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveSend validates one signal and queues it for its recipient.
func (s *Server) serveSend(w http.ResponseWriter, r *http.Request, from pipe.PeerID) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, pipe.MaxEnvelopeSize+maxBodySlack))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "signal is too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return
	}

	var sig pipe.Signal
	if err := json.Unmarshal(body, &sig); err != nil {
		http.Error(w, "body is not a signal envelope", http.StatusBadRequest)
		return
	}
	if sig.From != from {
		http.Error(w, "signal From does not match the authenticated peer", http.StatusForbidden)
		return
	}
	if err := sig.Validate(); err != nil {
		http.Error(w, "invalid signal: "+err.Error(), http.StatusBadRequest)
		return
	}

	dst, ok := s.lookup(sig.To)
	if !ok {
		http.Error(w, "recipient is not connected", http.StatusNotFound)
		return
	}
	// Re-encode canonically so that the stream carries exactly what a pipe
	// decoder expects, regardless of how the client formatted it.
	data, err := json.Marshal(sig)
	if err != nil {
		http.Error(w, "could not encode the signal", http.StatusInternalServerError)
		return
	}
	if !dst.enqueue(data) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "recipient queue is full", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// serveStream attaches the request as the peer's event stream and writes
// signals until the client leaves or another stream replaces this one.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, id pipe.PeerID) {
	p, err := s.admit(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	var lastID uint64
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			http.Error(w, "Last-Event-ID is not a number", http.StatusBadRequest)
			return
		}
		lastID = n
	}

	rc := http.NewResponseController(w)
	// A server with WriteTimeout set would otherwise cut the stream; a
	// per-write deadline keeps stalled clients from pinning the goroutine
	// without limiting how long a healthy stream may live. It is refreshed
	// before every push, keepalives included, so the pusher's own idle ping
	// is left off.
	arm := func() error {
		err := rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	if err := arm(); err != nil {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Accel-Buffering", "no")
	pusher, err := sse.CreateHttpPusher(w)
	if err != nil {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	push := func(msg *sse.Message) error {
		if err := arm(); err != nil {
			return err
		}
		return pusher.Push(msg)
	}

	att := p.attach(lastID)
	defer p.detach(att)

	// Tell the client who it is, so that a misconfigured one fails loudly.
	if err := push(&sse.Message{Event: eventHello, Data: strconv.Quote(string(id))}); err != nil {
		return
	}
	s.log.Debug("sse: stream attached", slog.String("peer", string(id)),
		slog.Uint64("last_event_id", lastID))

	ticker := time.NewTicker(s.cfg.KeepAlive)
	defer ticker.Stop()

	for {
		for _, e := range p.takeUnsent() {
			msg := &sse.Message{Id: strconv.FormatUint(e.id, 10), Event: eventSignal, Data: string(e.data)}
			if err := push(msg); err != nil {
				s.log.Debug("sse: stream write failed", slog.String("peer", string(id)),
					slog.String("error", err.Error()))
				return
			}
		}

		select {
		case <-p.notify:
		case <-ticker.C:
			if err := push(sse.NewComment("ping")); err != nil {
				return
			}
		case <-att.done:
			// Replaced by a newer stream, or the server is shutting down.
			_ = push(&sse.Message{Event: att.reason})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// admit returns the peer record for id, creating it within the configured
// limits.
func (s *Server) admit(id pipe.PeerID) (*peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("signaling server is shutting down")
	}
	if p, ok := s.peers[id]; ok {
		return p, nil
	}
	if s.cfg.MaxPeers > 0 && len(s.peers) >= s.cfg.MaxPeers {
		return nil, errors.New("peer limit reached")
	}
	p := newPeer(s, id)
	s.peers[id] = p
	s.log.Info("sse: peer registered", slog.String("peer", string(id)))
	return p, nil
}

func (s *Server) lookup(id pipe.PeerID) (*peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	return p, ok
}

// forget removes p if it is still the registered record for its ID.
func (s *Server) forget(p *peer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.peers[p.id]; ok && cur == p {
		delete(s.peers, p.id)
		s.log.Info("sse: peer forgotten", slog.String("peer", string(p.id)))
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// entry is one queued signal with its per-peer sequence number.
type entry struct {
	id   uint64
	data []byte
}

// attachment identifies one stream so that a newer stream can replace it.
type attachment struct {
	done chan struct{}
	// reason names the event sent to the client when done is closed. It is
	// written under the peer mutex before done is closed.
	reason string
}

func (a *attachment) end(reason string) {
	a.reason = reason
	close(a.done)
}

// peer holds one peer's queue, replay log, and current stream.
type peer struct {
	srv *Server
	id  pipe.PeerID

	// notify wakes the stream when new entries are queued.
	notify chan struct{}

	mu sync.Mutex
	// seq is the last sequence number assigned.
	seq uint64
	// sentUpTo is the highest sequence number written to a stream.
	sentUpTo uint64
	// log holds entries not yet delivered plus up to ReplayDepth delivered
	// ones, oldest first.
	log []entry
	// stream is the attached stream, or nil.
	stream *attachment
	// reaper forgets the peer when it stays detached for too long.
	reaper *time.Timer
	// down is set once the peer has been shut down or forgotten.
	down bool
}

func newPeer(s *Server, id pipe.PeerID) *peer {
	p := &peer{srv: s, id: id, notify: make(chan struct{}, 1)}
	p.reaper = time.AfterFunc(s.cfg.OfflineGrace, p.expire)
	return p
}

// enqueue appends a signal and reports whether there was room.
func (p *peer) enqueue(data []byte) bool {
	p.mu.Lock()
	if p.down {
		p.mu.Unlock()
		return false
	}
	if p.seq-p.sentUpTo >= uint64(p.srv.cfg.QueueSize) {
		p.mu.Unlock()
		return false
	}
	p.seq++
	p.log = append(p.log, entry{id: p.seq, data: data})
	p.mu.Unlock()

	select {
	case p.notify <- struct{}{}:
	default:
	}
	return true
}

// attach registers a new stream, replacing any current one, and rewinds the
// delivery cursor to lastID so that the new stream receives what the old one
// may have lost.
func (p *peer) attach(lastID uint64) *attachment {
	att := &attachment{done: make(chan struct{})}

	p.mu.Lock()
	if p.stream != nil {
		p.stream.end(eventReplaced)
	}
	p.stream = att
	if p.reaper != nil {
		p.reaper.Stop()
	}
	if lastID < p.sentUpTo {
		// Rewind no further than the oldest entry still held; anything older
		// is gone and counting it as unsent would only eat queue capacity.
		floor := p.seq
		if len(p.log) > 0 {
			floor = p.log[0].id - 1
		}
		p.sentUpTo = max(lastID, floor)
	}
	p.mu.Unlock()

	select {
	case p.notify <- struct{}{}:
	default:
	}
	return att
}

// detach clears the stream if att is still current and starts the offline
// grace timer.
func (p *peer) detach(att *attachment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stream != att {
		return
	}
	p.stream = nil
	if !p.down {
		p.reaper = time.AfterFunc(p.srv.cfg.OfflineGrace, p.expire)
	}
}

// takeUnsent returns the entries not yet written to a stream, marks them sent,
// and trims delivered entries beyond the replay depth.
func (p *peer) takeUnsent() []entry {
	p.mu.Lock()
	defer p.mu.Unlock()

	start := len(p.log)
	for i := range p.log {
		if p.log[i].id > p.sentUpTo {
			start = i
			break
		}
	}
	out := slices.Clone(p.log[start:])
	if len(out) > 0 {
		p.sentUpTo = out[len(out)-1].id
	}

	// Everything is now delivered; keep only the replay window.
	if extra := len(p.log) - p.srv.cfg.ReplayDepth; extra > 0 {
		p.log = slices.Clone(p.log[extra:])
	}
	return out
}

// expire runs when the offline grace period ends with no stream attached.
func (p *peer) expire() {
	p.mu.Lock()
	if p.stream != nil || p.down {
		p.mu.Unlock()
		return
	}
	p.down = true
	p.log = nil
	p.mu.Unlock()
	p.srv.forget(p)
}

// shutdown detaches the stream and discards the queue.
func (p *peer) shutdown() {
	p.mu.Lock()
	p.down = true
	p.log = nil
	if p.reaper != nil {
		p.reaper.Stop()
	}
	if p.stream != nil {
		p.stream.end(eventShutdown)
		p.stream = nil
	}
	p.mu.Unlock()
}
