package pipe

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"ella.to/pipe/internal/pionx"
)

// sessionShutdownGrace bounds how long Close waits for sessions to release their
// resources before it tears down signaling anyway.
const sessionShutdownGrace = 5 * time.Second

// Endpoint is the primary API. It owns one signaling connection and may serve
// many concurrent inbound and outbound sessions.
//
// An Endpoint is safe for concurrent use. Its lifetime is controlled by
// [Endpoint.Close], not by the context passed to [New].
type Endpoint struct {
	cfg     Config
	log     *slog.Logger
	signal  SignalConn
	factory *pionx.Factory

	// ctx is cancelled by Close and governs every internal goroutine.
	ctx    context.Context
	cancel context.CancelFunc

	// sendSem serializes signaling sends, because the SPI only promises that
	// Send is safe alongside Receive.
	sendSem chan struct{}

	// inboundSlots bounds inbound sessions that are negotiating or waiting to
	// be accepted.
	inboundSlots chan struct{}

	dedupe *dedupeCache

	// wg tracks the receive loop, every session goroutine, and keepalive
	// probes.
	wg sync.WaitGroup

	mu        sync.Mutex
	sessions  map[string]*session
	listener  *Listener
	listened  bool
	signalErr error
	closed    bool

	closeOnce sync.Once
	closeErr  error
}

// New validates the configuration, opens the signaling connection, and starts the
// endpoint's receive loop.
//
// ctx governs construction only. Once New returns successfully, cancelling ctx
// has no effect on the endpoint; call [Endpoint.Close] to release it.
func New(ctx context.Context, cfg Config) (*Endpoint, error) {
	c, err := cfg.clone()
	if err != nil {
		return nil, err
	}

	factory, err := pionx.NewFactory(pionx.Config{
		ICEServers:             toPionICEServers(c.ICEServers),
		RelayOnly:              c.ICETransportPolicy == ICETransportPolicyRelay,
		ConfigureSettingEngine: c.Pion.ConfigureSettingEngine,
		ConfigureConfiguration: c.Pion.ConfigureConfiguration,
	})
	if err != nil {
		return nil, wrapErr(ErrConfig, err, "pipe: build WebRTC engine")
	}

	signal, err := c.Signaler.Open(ctx, c.ID)
	if err != nil {
		return nil, wrapErr(ErrSignaling, err, "pipe: open signaling")
	}

	ep := &Endpoint{
		cfg:          c,
		log:          c.Logger.With(slog.String("local", string(c.ID))),
		signal:       signal,
		factory:      factory,
		sendSem:      make(chan struct{}, 1),
		inboundSlots: make(chan struct{}, c.AcceptBacklog),
		dedupe:       newDedupeCache(dedupeCacheSize),
		sessions:     make(map[string]*session),
	}
	// The endpoint owns an independent lifetime from here on.
	ep.ctx, ep.cancel = context.WithCancel(context.WithoutCancel(ctx))

	ep.wg.Add(1)
	go ep.receiveLoop()

	return ep, nil
}

// LocalID returns the endpoint's peer ID.
func (e *Endpoint) LocalID() PeerID { return e.cfg.ID }

// Addr returns the endpoint's logical address.
func (e *Endpoint) Addr() net.Addr { return Addr{Peer: e.cfg.ID} }

// Dial establishes a connection to peer. It returns only after the connection is
// fully negotiated, detached, and ready for I/O.
//
// Cancelling ctx removes the pending session and releases its resources. The
// call is also bounded by [Config.DialTimeout].
func (e *Endpoint) Dial(ctx context.Context, peer PeerID) (*Conn, error) {
	e.cfg.Metrics.Count(metricDialAttempts, 1)

	conn, err := e.dial(ctx, peer)
	switch {
	case err == nil:
		e.cfg.Metrics.Count(metricDialResults, 1, labelResult("success"))
		return conn, nil
	case errors.Is(err, ErrTimeout):
		e.cfg.Metrics.Count(metricDialResults, 1, labelResult("timeout"))
	case errors.Is(err, ErrPeerRejected):
		e.cfg.Metrics.Count(metricDialResults, 1, labelResult("rejected"))
	default:
		e.cfg.Metrics.Count(metricDialResults, 1, labelResult("error"))
	}
	return nil, err
}

func (e *Endpoint) dial(ctx context.Context, peer PeerID) (*Conn, error) {
	if err := validPeerID(peer); err != nil {
		return nil, errorf(ErrConfig, "pipe: dial: %w", err)
	}
	if peer == e.cfg.ID {
		return nil, errorf(ErrConfig, "pipe: dial: an endpoint cannot dial itself")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s := newSession(e, NewID(), peer, roleOfferer)
	if err := e.addSession(s); err != nil {
		return nil, err
	}

	e.wg.Add(1)
	go s.run(nil)

	select {
	case res := <-s.result:
		if res.err != nil {
			return nil, res.err
		}
		return res.conn, nil

	case <-ctx.Done():
		s.closeWith(&closeIntent{
			code:   CloseNormal,
			reason: "caller cancelled the dial",
			notify: true,
			err:    ctx.Err(),
		})
		// Wait for teardown so that no resource outlives the call.
		<-s.finished
		select {
		case res := <-s.result:
			if res.conn != nil {
				// The connection was established just as the caller gave up.
				_ = res.conn.Close()
			}
		default:
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errorf(ErrTimeout, "pipe: dial %s: %w", peer, ctx.Err())
		}
		return nil, errorf(ErrClosed, "pipe: dial %s: %w", peer, ctx.Err())

	case <-e.ctx.Done():
		return nil, errorf(ErrClosed, "pipe: dial %s: endpoint is closed", peer)
	}
}

// Listen returns a listener that accepts inbound sessions. An endpoint has at
// most one listener; later calls return [ErrAlreadyListening]. Closing the
// listener and calling Listen again is not supported either.
func (e *Endpoint) Listen() (*Listener, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil, errorf(ErrClosed, "pipe: listen: endpoint is closed")
	}
	if e.listened {
		return nil, ErrAlreadyListening
	}

	ln := &Listener{
		ep:     e,
		accept: make(chan pendingConn, e.cfg.AcceptBacklog),
		closed: make(chan struct{}),
		addr:   Addr{Peer: e.cfg.ID},
	}
	e.listener = ln
	e.listened = true
	return ln, nil
}

// Close releases the endpoint: every session, its PeerConnections, the listener,
// and the signaling connection. It is idempotent and safe from concurrent
// callers.
func (e *Endpoint) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		sessions := make([]*session, 0, len(e.sessions))
		for _, s := range e.sessions {
			sessions = append(sessions, s)
		}
		ln := e.listener
		e.mu.Unlock()

		if ln != nil {
			_ = ln.Close()
		}

		// Ask sessions to close while signaling is still up, so peers learn
		// why.
		for _, s := range sessions {
			s.closeWith(&closeIntent{
				code:   CloseGoingAway,
				reason: "endpoint is shutting down",
				notify: true,
				err:    ErrClosed,
			})
		}

		e.awaitSessions(sessions)

		// Cancel first so the receive loop stops before signaling closes.
		e.cancel()
		e.closeErr = e.signal.Close()
		e.wg.Wait()
		e.log.Debug("pipe: endpoint closed")
	})
	return e.closeErr
}

// awaitSessions waits for every session to release its resources, bounded by the
// shutdown grace period.
func (e *Endpoint) awaitSessions(sessions []*session) {
	grace := time.NewTimer(sessionShutdownGrace)
	defer grace.Stop()

	for _, s := range sessions {
		select {
		case <-s.finished:
		case <-grace.C:
			e.log.Warn("pipe: sessions did not shut down within the grace period")
			return
		}
	}
}

// receiveLoop is the only caller of SignalConn.Receive.
func (e *Endpoint) receiveLoop() {
	defer e.wg.Done()

	for {
		sig, err := e.signal.Receive(e.ctx)
		if err != nil {
			if e.ctx.Err() == nil {
				e.setSignalErr(wrapErr(ErrSignaling, err, "pipe: signaling receive failed"))
				e.log.Warn("pipe: signaling stopped", slog.String("error", err.Error()))
			}
			return
		}
		e.route(sig)
	}
}

// route validates, deduplicates, and delivers one inbound signal.
func (e *Endpoint) route(sig Signal) {
	if err := sig.Validate(); err != nil {
		e.log.Debug("pipe: rejected invalid signal", slog.String("error", err.Error()))
		e.cfg.Metrics.Count(metricSignalReceived, 1, labelKind(sig.Kind), labelResult("invalid"))
		e.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("signal"))
		return
	}
	if sig.To != e.cfg.ID {
		e.log.Debug("pipe: rejected misrouted signal", slog.String("to", string(sig.To)))
		e.cfg.Metrics.Count(metricSignalReceived, 1, labelKind(sig.Kind), labelResult("misrouted"))
		e.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("signal"))
		return
	}
	if e.dedupe.seenBefore(sig.ID) {
		e.cfg.Metrics.Count(metricSignalReceived, 1, labelKind(sig.Kind), labelResult("duplicate"))
		return
	}
	e.cfg.Metrics.Count(metricSignalReceived, 1, labelKind(sig.Kind), labelResult("accepted"))

	e.mu.Lock()
	s, known := e.sessions[sig.SessionID]
	closed := e.closed
	e.mu.Unlock()

	if closed {
		return
	}
	if known {
		if s.peer != sig.From {
			e.log.Debug("pipe: rejected signal from an unexpected peer",
				slog.String("session", sig.SessionID),
				slog.String("from", string(sig.From)))
			e.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("routing"))
			return
		}
		s.postSignal(sig)
		return
	}

	if sig.Kind == KindOffer {
		e.acceptOffer(sig)
		return
	}

	// Only a valid offer may create a session. Everything else for an unknown
	// session is dropped so that late or replayed messages cannot resurrect it.
	e.log.Debug("pipe: dropped signal for an unknown session",
		slog.String("kind", string(sig.Kind)),
		slog.String("session", sig.SessionID))
	e.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("routing"))
}

// acceptOffer admits an inbound session if a listener has capacity.
func (e *Endpoint) acceptOffer(sig Signal) {
	e.cfg.Metrics.Count(metricAcceptAttempts, 1)

	e.mu.Lock()
	ln := e.listener
	e.mu.Unlock()

	if ln == nil || ln.isClosed() {
		e.cfg.Metrics.Count(metricAcceptResults, 1, labelResult("not-listening"))
		e.reject(sig, RejectNotListening, "peer is not accepting connections")
		return
	}

	if allow := e.cfg.AllowPeer; allow != nil && !allow(sig.From) {
		e.log.Debug("pipe: refused an offer from an unauthorized peer",
			slog.String("from", string(sig.From)))
		e.cfg.Metrics.Count(metricAcceptResults, 1, labelResult("unauthorized"))
		e.reject(sig, RejectUnauthorized, "peer is not allowed to connect")
		return
	}

	select {
	case e.inboundSlots <- struct{}{}:
	default:
		e.cfg.Metrics.Count(metricAcceptResults, 1, labelResult("busy"))
		e.reject(sig, RejectBusy, "accept backlog is full")
		return
	}

	s := newSession(e, sig.SessionID, sig.From, roleAnswerer)
	var once sync.Once
	s.releaseSlot = func() {
		once.Do(func() {
			select {
			case <-e.inboundSlots:
			default:
			}
			e.cfg.Metrics.Gauge(metricSessionsPending, int64(len(e.inboundSlots)))
		})
	}

	if err := e.addSession(s); err != nil {
		s.releaseSlot()
		e.cfg.Metrics.Count(metricAcceptResults, 1, labelResult("error"))
		e.reject(sig, RejectInternal, "endpoint is not accepting sessions")
		return
	}
	e.cfg.Metrics.Gauge(metricSessionsPending, int64(len(e.inboundSlots)))

	e.wg.Add(1)
	offer := sig
	go s.run(&offer)
}

// reject refuses a session that was never established.
func (e *Endpoint) reject(sig Signal, code RejectCode, reason string) {
	out, err := newSignal(KindReject, e.cfg.ID, sig.From, sig.SessionID, rejectPayload{
		Code:   code,
		Reason: reason,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, signalSendTimeout)
	defer cancel()
	if err := e.send(ctx, out); err != nil {
		e.log.Debug("pipe: could not send reject", slog.String("error", err.Error()))
	}
}

// enqueueInbound hands an established connection to the listener.
func (e *Endpoint) enqueueInbound(conn *Conn, release func()) error {
	e.mu.Lock()
	ln := e.listener
	e.mu.Unlock()

	if ln == nil {
		return errorf(ErrClosed, "pipe: no listener is accepting connections")
	}
	return ln.push(pendingConn{conn: conn, release: release})
}

// send transmits one signal. Sends are serialized because the signaling SPI only
// promises that Send is safe alongside Receive.
func (e *Endpoint) send(ctx context.Context, sig Signal) error {
	select {
	case e.sendSem <- struct{}{}:
	case <-ctx.Done():
		e.cfg.Metrics.Count(metricSignalSent, 1, labelKind(sig.Kind), labelResult("timeout"))
		return errorf(ErrSignaling, "pipe: send %s: %w", sig.Kind, ctx.Err())
	}
	defer func() { <-e.sendSem }()

	if err := e.signal.Send(ctx, sig); err != nil {
		e.cfg.Metrics.Count(metricSignalSent, 1, labelKind(sig.Kind), labelResult("error"))
		return wrapErr(ErrSignaling, err, "pipe: send "+string(sig.Kind))
	}
	e.cfg.Metrics.Count(metricSignalSent, 1, labelKind(sig.Kind), labelResult("success"))
	return nil
}

func (e *Endpoint) addSession(s *session) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return errorf(ErrClosed, "pipe: endpoint is closed")
	}
	if err := e.signalErr; err != nil {
		return err
	}
	if _, exists := e.sessions[s.id]; exists {
		return errorf(ErrProtocol, "pipe: session %s already exists", s.id)
	}
	e.sessions[s.id] = s
	e.cfg.Metrics.Gauge(metricSessionsActive, int64(len(e.sessions)))
	return nil
}

func (e *Endpoint) removeSession(s *session) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if cur, ok := e.sessions[s.id]; ok && cur == s {
		delete(e.sessions, s.id)
		e.cfg.Metrics.Gauge(metricSessionsActive, int64(len(e.sessions)))
	}
}

func (e *Endpoint) setSignalErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.signalErr == nil {
		e.signalErr = err
	}
}

// clearListener detaches a closed listener so that new offers are rejected.
func (e *Endpoint) clearListener(ln *Listener) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.listener == ln {
		e.listener = nil
	}
}

func toPionICEServers(servers []ICEServer) []pionx.ICEServer {
	out := make([]pionx.ICEServer, 0, len(servers))
	for _, s := range servers {
		out = append(out, pionx.ICEServer{
			URLs:       s.URLs,
			Username:   s.Username,
			Credential: s.Credential,
		})
	}
	return out
}
