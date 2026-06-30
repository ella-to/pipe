package pipe

import (
	"log/slog"
	"net/http"
	"sync"

	"ella.to/pipe/signal/sse"
)

// PeerSignalServer is an SSE signaling server that enforces unique peer ids.
//
// A Peer registers itself by opening a long-lived GET subscription on its main
// inbox (the "<id>.inbox" inbox its Listener watches). PeerSignalServer treats
// that subscription as the peer's identity claim: while one is active, any
// other attempt to subscribe to the same main inbox is rejected with HTTP 409
// Conflict. The claim is released when the subscription disconnects (which is
// exactly what Peer.Close triggers by canceling its signal receiver).
//
// All other traffic (POST sends, random-inbox subscriptions used for dialing)
// is delegated unchanged to the embedded sse.Server.
type PeerSignalServer struct {
	inner *sse.Server

	mu         sync.Mutex
	registered map[string]struct{}
}

var _ http.Handler = (*PeerSignalServer)(nil)

// NewPeerSignalServer creates a PeerSignalServer. Any sse.ServerOption is passed
// through to the embedded sse.Server.
func NewPeerSignalServer(opts ...sse.ServerOption) (*PeerSignalServer, error) {
	inner, err := sse.NewServer(opts...)
	if err != nil {
		return nil, err
	}
	return &PeerSignalServer{
		inner:      inner,
		registered: make(map[string]struct{}),
	}, nil
}

func (s *PeerSignalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	inbox, err := sse.ParseRequest(r)
	if err != nil {
		slog.WarnContext(r.Context(), "signal server received bad request", "method", r.Method, "raw_query", r.URL.RawQuery, "err", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// A GET on the main inbox is a peer registering its identity. Enforce
	// uniqueness for the lifetime of that subscription.
	if r.Method == http.MethodGet && inbox.IsMain() {
		if !s.tryRegister(inbox.Id) {
			// A second peer is trying to claim an id that is already online.
			// Its Listener will never receive anything, so it appears to "never
			// connect" — log it explicitly since it is an easy mistake to make.
			slog.WarnContext(r.Context(), "rejected duplicate peer id registration", "peer_id", inbox.Id)
			http.Error(w, "peer id already registered", http.StatusConflict)
			return
		}
		slog.InfoContext(r.Context(), "peer registered on signal server", "peer_id", inbox.Id)
		defer func() {
			s.unregister(inbox.Id)
			slog.InfoContext(r.Context(), "peer unregistered from signal server", "peer_id", inbox.Id)
		}()
	}

	s.inner.ServeHTTP(w, r)
}

// tryRegister claims id, returning false if it is already claimed.
func (s *PeerSignalServer) tryRegister(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registered[id]; ok {
		return false
	}
	s.registered[id] = struct{}{}
	return true
}

func (s *PeerSignalServer) unregister(id string) {
	s.mu.Lock()
	delete(s.registered, id)
	s.mu.Unlock()
}

// IsRegistered reports whether a peer id currently holds an active registration.
// Primarily useful for tests and observability.
func (s *PeerSignalServer) IsRegistered(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.registered[id]
	return ok
}
