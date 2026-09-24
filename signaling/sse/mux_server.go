package sse

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ella.to/pipe"
	"ella.to/sse"
)

// MuxHeader marks multiplexed-stream requests. On a GET its presence opens a
// multiplexed stream; on PUT and DELETE it carries the stream ID that the
// authenticated peer joins or leaves.
const MuxHeader = "X-Pipe-Mux"

// Event names used only on multiplexed streams.
const (
	// eventMux is the first event on a multiplexed stream; its data is a
	// [muxHello].
	eventMux = "mux"
	// eventDetached tells a multiplexed stream that one of its peers was
	// taken over by another stream; its data is a [muxDetached].
	eventDetached = "detached"
)

// muxHello is the data of the eventMux event.
type muxHello struct {
	Stream string `json:"stream"`
}

// muxDetached is the data of the eventDetached event.
type muxDetached struct {
	Peer   pipe.PeerID `json:"peer"`
	Reason string      `json:"reason"`
}

// muxStream is one multiplexed event stream carrying the signals of every
// peer that joined it.
type muxStream struct {
	id    string
	owner pipe.PeerID

	// wake is shared by the attachments of every member.
	wake chan struct{}
	// down is closed when the server shuts down.
	down     chan struct{}
	downOnce sync.Once

	mu      sync.Mutex
	members map[pipe.PeerID]muxMember
	// detached lists members taken over by another stream, not yet reported
	// to the client.
	detached []muxDetached
	closed   bool
}

type muxMember struct {
	p   *peer
	att *attachment
}

// serveMuxStream opens a multiplexed stream. The request needs a valid
// credential for some peer, but that peer is not registered: the stream starts
// empty, and peers join it with PUT requests carrying their own credentials.
func (s *Server) serveMuxStream(w http.ResponseWriter, r *http.Request, owner pipe.PeerID) {
	m, err := s.openMux(owner)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer s.closeMux(m)

	push, ok := openPusher(w)
	if !ok {
		return
	}

	hello, _ := json.Marshal(muxHello{Stream: m.id})
	if err := push(&sse.Message{Event: eventMux, Data: string(hello)}); err != nil {
		return
	}
	s.log.Debug("sse: multiplexed stream opened", slog.String("owner", string(owner)))

	ticker := time.NewTicker(s.cfg.KeepAlive)
	defer ticker.Stop()

	for {
		for _, mem := range m.snapshot() {
			for _, e := range mem.p.takeUnsent(mem.att) {
				msg := &sse.Message{Id: strconv.FormatUint(e.id, 10), Event: eventSignal, Data: string(e.data)}
				if err := push(msg); err != nil {
					s.log.Debug("sse: multiplexed stream write failed",
						slog.String("owner", string(owner)), slog.String("error", err.Error()))
					return
				}
			}
		}
		for _, d := range m.takeDetached() {
			data, _ := json.Marshal(d)
			if err := push(&sse.Message{Event: eventDetached, Data: string(data)}); err != nil {
				return
			}
		}

		select {
		case <-m.wake:
		case <-ticker.C:
			if err := push(sse.NewComment("ping")); err != nil {
				return
			}
		case <-m.down:
			_ = push(&sse.Message{Event: eventShutdown})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// serveJoin attaches the authenticated peer to the multiplexed stream named
// by [MuxHeader], replacing any stream the peer had. Last-Event-ID resumes
// delivery after that sequence number, exactly as on a single-peer stream.
func (s *Server) serveJoin(w http.ResponseWriter, r *http.Request, id pipe.PeerID) {
	m, ok := s.lookupMux(r.Header.Get(MuxHeader))
	if !ok {
		http.Error(w, "multiplexed stream not found", http.StatusNotFound)
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
	p, err := s.admit(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if !m.join(p, lastID) {
		http.Error(w, "multiplexed stream not found", http.StatusNotFound)
		return
	}
	s.log.Debug("sse: peer joined multiplexed stream", slog.String("peer", string(id)),
		slog.String("owner", string(m.owner)), slog.Uint64("last_event_id", lastID))
	w.WriteHeader(http.StatusNoContent)
}

// serveLeave detaches the authenticated peer from the multiplexed stream named
// by [MuxHeader]. The peer then keeps its queue for the offline grace period,
// as if its own stream had dropped. Leaving a stream the peer is not on
// succeeds.
func (s *Server) serveLeave(w http.ResponseWriter, r *http.Request, id pipe.PeerID) {
	if m, ok := s.lookupMux(r.Header.Get(MuxHeader)); ok {
		m.leave(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) openMux(owner pipe.PeerID) (*muxStream, error) {
	m := &muxStream{
		id:      pipe.NewID(),
		owner:   owner,
		wake:    make(chan struct{}, 1),
		down:    make(chan struct{}),
		members: make(map[pipe.PeerID]muxMember),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errServerClosing
	}
	s.muxes[m.id] = m
	return m, nil
}

func (s *Server) lookupMux(id string) (*muxStream, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.muxes[id]
	return m, ok
}

// closeMux unregisters m and detaches its members, which start their offline
// grace period.
func (s *Server) closeMux(m *muxStream) {
	s.mu.Lock()
	if cur, ok := s.muxes[m.id]; ok && cur == m {
		delete(s.muxes, m.id)
	}
	s.mu.Unlock()

	m.mu.Lock()
	m.closed = true
	members := m.members
	m.members = nil
	m.mu.Unlock()

	for _, mem := range members {
		mem.p.detach(mem.att)
	}
}

// join makes p a member. It reports false when the stream has closed.
func (m *muxStream) join(p *peer, lastID uint64) bool {
	att := &attachment{wake: m.wake}
	att.end = func(reason string) { m.ended(p.id, att, reason) }

	// Record the member before attaching, so that a replacement racing with
	// this join finds it; the lock order is peer, then stream.
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	prev, had := m.members[p.id]
	m.members[p.id] = muxMember{p: p, att: att}
	m.mu.Unlock()

	if had && prev.p != p {
		// The peer was forgotten and registered again since it joined.
		prev.p.detach(prev.att)
	}
	p.attach(att, lastID)

	// The stream may have closed while attaching; closeMux may then have run
	// before the attach and missed it.
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		p.detach(att)
		return false
	}
	return true
}

func (m *muxStream) leave(id pipe.PeerID) {
	m.mu.Lock()
	mem, ok := m.members[id]
	if ok {
		delete(m.members, id)
	}
	m.mu.Unlock()
	if ok {
		mem.p.detach(mem.att)
	}
}

// ended is the attachment callback for member id. It runs with the peer mutex
// held.
func (m *muxStream) ended(id pipe.PeerID, att *attachment, reason string) {
	m.mu.Lock()
	mem, ok := m.members[id]
	if !ok || mem.att != att {
		m.mu.Unlock()
		return
	}
	delete(m.members, id)
	// A server shutdown ends the whole stream through down; only a takeover
	// concerns this one peer.
	if reason == eventReplaced {
		m.detached = append(m.detached, muxDetached{Peer: id, Reason: reason})
	}
	m.mu.Unlock()

	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *muxStream) snapshot() []muxMember {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]muxMember, 0, len(m.members))
	for _, mem := range m.members {
		out = append(out, mem)
	}
	return out
}

func (m *muxStream) takeDetached() []muxDetached {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.detached
	m.detached = nil
	return out
}

func (m *muxStream) shutdown() {
	m.downOnce.Do(func() { close(m.down) })
}
