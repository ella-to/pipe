package pipe

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"time"

	"ella.to/pipe/internal/clock"
	"ella.to/pipe/internal/frame"
	"ella.to/pipe/internal/pionx"
)

// closeSignalTimeout bounds the best-effort close notification sent during
// teardown.
const closeSignalTimeout = 2 * time.Second

// closeReleaseTimeout bounds how long teardown waits for the stream to release
// its DataChannel before the PeerConnection is closed anyway. The stream waits
// for the peer to acknowledge written data first, because closing the
// PeerConnection aborts the SCTP association and discards anything in flight.
const closeReleaseTimeout = 3 * time.Second

// closeAckGrace is how long a session keeps its PeerConnection open after the
// stream has released the DataChannel, so that its final SCTP acknowledgements
// reach the peer. SCTP delays an acknowledgement by up to 200ms; a peer whose
// close frame is never acknowledged waits out its whole drain timeout instead.
// The grace applies to every orderly teardown, whichever side closed first,
// because the two sides' close paths race and either may be the one holding
// the last acknowledgement.
const closeAckGrace = 400 * time.Millisecond

// closeLingerTimeout bounds how long a session waits for its DataChannel to
// finish after the peer announced the close through signaling. Signaling can
// outrun the data path, so tearing the stream down immediately would discard
// bytes the peer already sent.
const closeLingerTimeout = 5 * time.Second

// sessionEvent is the single event type consumed by the session loop. Exactly
// one field is set.
type sessionEvent struct {
	signal    *Signal
	pion      *pionx.Event
	keepalive *keepaliveResult
	close     *closeIntent
}

type keepaliveResult struct {
	rtt time.Duration
	err error
}

// closeIntent describes why a session is being torn down.
type closeIntent struct {
	// code and reason are reported to the peer.
	code   CloseCode
	reason string

	// err is delivered to a pending Dial, if any.
	err error

	// notify sends a close signal to the peer. It is false when the peer
	// already knows, for example because the peer initiated the close.
	notify bool

	// graceful means the peer ended the connection, so data that has already
	// arrived must stay readable instead of being discarded.
	graceful bool
}

// dialResult is delivered once to a waiting Dial.
type dialResult struct {
	conn *Conn
	err  error
}

// session owns one negotiation and, once established, one connection. Exactly
// one goroutine, the session loop, mutates its negotiation state. Pion callbacks
// and the endpoint receive loop only post events.
type session struct {
	ep   *Endpoint
	id   string
	peer PeerID
	role role
	log  *slog.Logger

	box    *mailbox
	result chan dialResult

	// finished is closed once teardown has completed.
	finished chan struct{}

	// releaseSlot returns this session's inbound backlog slot. It is nil for
	// outbound sessions and runs at most once.
	releaseSlot func()

	// Fields below are owned by the session loop.
	peerConn       *pionx.Peer
	stream         *frame.Stream
	conn           *Conn
	state          ConnectionState
	pendingCands   []pionx.Candidate
	remoteComplete bool
	generation     uint32
	restarts       int
	restartAttempt int
	restartStarted time.Time
	startedAt      time.Time
	delivered      bool
	pinging        bool

	// peerClosed records that the peer announced an orderly close through
	// signaling, which is proof that a later transport abort is not data loss.
	peerClosed bool

	negotiateTimer clock.Timer
	recoveryTimer  clock.Timer
	keepaliveTimer clock.Timer
	lingerTimer    clock.Timer
}

func newSession(ep *Endpoint, id string, peer PeerID, r role) *session {
	return &session{
		ep:   ep,
		id:   id,
		peer: peer,
		role: r,
		log: ep.log.With(
			slog.String("session", id),
			slog.String("peer", string(peer)),
			slog.String("role", r.String()),
		),
		box:      newMailbox(),
		result:   make(chan dialResult, 1),
		finished: make(chan struct{}),
		state:    StateNew,
	}
}

// sink is the bounded Pion callback target. It never blocks and is safe to call
// after the session has shut down.
func (s *session) sink(ev pionx.Event) {
	s.box.post(sessionEvent{pion: &ev})
}

// postSignal hands a routed signal to the session loop.
func (s *session) postSignal(sig Signal) {
	s.box.post(sessionEvent{signal: &sig})
}

// requestClose asks the session to tear down. It is safe from any goroutine, is
// idempotent in effect, and never blocks.
func (s *session) requestClose() {
	s.closeWith(&closeIntent{code: CloseNormal, notify: true, err: ErrClosed})
}

func (s *session) closeWith(intent *closeIntent) {
	s.box.post(sessionEvent{close: intent})
}

// run drives the session until a terminal state, then releases every resource
// exactly once.
func (s *session) run(offer *Signal) {
	defer s.ep.wg.Done()

	intent := s.negotiate(offer)
	s.teardown(intent)
}

// negotiate runs the event loop and returns the intent that terminated it.
func (s *session) negotiate(offer *Signal) *closeIntent {
	s.startedAt = s.ep.cfg.clock.Now()

	if err := s.start(offer); err != nil {
		return intentFor(err)
	}
	s.negotiateTimer = s.ep.cfg.clock.NewTimer(s.ep.cfg.DialTimeout)

	for {
		select {
		case <-s.box.wait():
			events, overflow := s.box.drain()
			if overflow {
				return intentFor(errorf(ErrProtocol, "pipe: session event queue overflowed"))
			}
			for _, ev := range events {
				if intent := s.handle(ev); intent != nil {
					return intent
				}
			}

		case <-timerChan(s.negotiateTimer):
			s.negotiateTimer = nil
			if s.state == StateConnected || s.state == StateRecovering {
				continue
			}
			return &closeIntent{
				code:   CloseTimeout,
				reason: "negotiation timed out",
				notify: true,
				err:    errorf(ErrTimeout, "pipe: negotiation with %s timed out after %s", s.peer, s.ep.cfg.DialTimeout),
			}

		case <-timerChan(s.recoveryTimer):
			s.recoveryTimer = nil
			if intent := s.onRecoveryDeadline(); intent != nil {
				return intent
			}

		case <-timerChan(s.keepaliveTimer):
			s.keepaliveTimer = nil
			s.startProbe()

		case <-timerChan(s.lingerTimer):
			s.lingerTimer = nil
			return &closeIntent{code: CloseNormal, notify: false, graceful: true, err: ErrClosed}

		case <-s.streamDone():
			return s.onStreamDone()

		case <-s.ep.ctx.Done():
			return &closeIntent{
				code:   CloseGoingAway,
				reason: "endpoint is shutting down",
				notify: false,
				err:    ErrClosed,
			}
		}
	}
}

// start creates the PeerConnection and performs the first signaling step.
func (s *session) start(offer *Signal) error {
	if s.role == roleOfferer {
		p, err := s.ep.factory.NewOfferer(s.sink)
		if err != nil {
			return wrapErr(ErrNegotiation, err, "pipe: create peer connection")
		}
		s.peerConn = p

		sdp, err := p.CreateOffer(false)
		if err != nil {
			return wrapErr(ErrNegotiation, err, "pipe: create offer")
		}
		s.state = StateSignaling
		return s.sendSignal(KindOffer, sdpPayload{SDP: sdp})
	}

	p, err := s.ep.factory.NewAnswerer(s.sink)
	if err != nil {
		return wrapErr(ErrNegotiation, err, "pipe: create peer connection")
	}
	s.peerConn = p

	var body sdpPayload
	if err := decodePayload(offer.Payload, &body); err != nil {
		return err
	}
	if err := p.SetRemoteOffer(body.SDP); err != nil {
		return wrapErr(ErrNegotiation, err, "pipe: apply offer")
	}
	sdp, err := p.CreateAnswer()
	if err != nil {
		return wrapErr(ErrNegotiation, err, "pipe: create answer")
	}
	s.state = StateConnecting
	if err := s.sendSignal(KindAnswer, sdpPayload{SDP: sdp}); err != nil {
		return err
	}
	return s.flushCandidates()
}

// handle dispatches one event and returns a non-nil intent when the session must
// terminate.
func (s *session) handle(ev sessionEvent) *closeIntent {
	switch {
	case ev.close != nil:
		return ev.close
	case ev.signal != nil:
		return s.onSignal(*ev.signal)
	case ev.pion != nil:
		return s.onPionEvent(*ev.pion)
	case ev.keepalive != nil:
		return s.onKeepAlive(*ev.keepalive)
	}
	return nil
}

func (s *session) onSignal(sig Signal) *closeIntent {
	switch sig.Kind {
	case KindAnswer:
		if s.role != roleOfferer {
			s.dropSignal(sig, "answer for an answering session")
			return nil
		}
		if s.peerConn.HasRemoteDescription() && s.restartAttempt == 0 {
			s.dropSignal(sig, "duplicate answer")
			return nil
		}
		var body sdpPayload
		if err := decodePayload(sig.Payload, &body); err != nil {
			return intentFor(err)
		}
		if err := s.peerConn.SetRemoteAnswer(body.SDP); err != nil {
			return intentFor(wrapErr(ErrNegotiation, err, "pipe: apply answer"))
		}
		s.restartAttempt = 0
		if s.state == StateSignaling {
			s.state = StateConnecting
		}
		if err := s.flushCandidates(); err != nil {
			return intentFor(err)
		}

	case KindOffer:
		// A duplicate offer must never create a second PeerConnection.
		s.dropSignal(sig, "duplicate offer")

	case KindRestart:
		return s.onRestartSignal(sig)

	case KindCandidate:
		var body candidatePayload
		if err := decodePayload(sig.Payload, &body); err != nil {
			return intentFor(err)
		}
		cand := pionx.Candidate{
			Candidate:        body.Candidate,
			SDPMid:           body.SDPMid,
			SDPMLineIndex:    body.SDPMLineIndex,
			UsernameFragment: body.UsernameFragment,
		}
		if !s.peerConn.HasRemoteDescription() {
			if len(s.pendingCands) >= maxPendingCandidates {
				return intentFor(errorf(ErrProtocol,
					"pipe: peer sent more than %d candidates before its description", maxPendingCandidates))
			}
			s.pendingCands = append(s.pendingCands, cand)
			return nil
		}
		if err := s.peerConn.AddCandidate(cand); err != nil {
			// A single unusable candidate is not fatal; ICE continues with the
			// others.
			s.log.Debug("pipe: discarded remote candidate", slog.String("error", err.Error()))
			s.ep.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("candidate"))
		}

	case KindICEComplete:
		if !s.peerConn.HasRemoteDescription() {
			s.remoteComplete = true
			return nil
		}
		if err := s.peerConn.EndOfRemoteCandidates(); err != nil {
			s.log.Debug("pipe: end-of-candidates rejected", slog.String("error", err.Error()))
		}

	case KindReject:
		var body rejectPayload
		if err := decodePayload(sig.Payload, &body); err != nil {
			return intentFor(err)
		}
		return &closeIntent{
			code:   CloseNormal,
			notify: false,
			err:    &RejectedError{Code: body.Code, Reason: body.Reason, Peer: s.peer},
		}

	case KindClose:
		var body closePayload
		if err := decodePayload(sig.Payload, &body); err != nil {
			return intentFor(err)
		}
		if s.state != StateConnected || s.stream == nil {
			return &closeIntent{
				code:   CloseNormal,
				notify: false,
				err:    errorf(ErrPeerRejected, "pipe: peer closed the session during negotiation (%s)", body.Code),
			}
		}
		// The DataChannel is the authoritative in-order path: bytes the peer
		// wrote before closing may still be in flight. Wait for the stream to
		// end on its own, bounded by a linger timer.
		s.peerClosed = true
		if s.lingerTimer == nil {
			s.lingerTimer = s.ep.cfg.clock.NewTimer(closeLingerTimeout)
			s.log.Debug("pipe: peer closed the session, draining the stream")
		}
	}
	return nil
}

// onRestartSignal applies an ICE restart offer from the peer.
func (s *session) onRestartSignal(sig Signal) *closeIntent {
	var body restartPayload
	if err := decodePayload(sig.Payload, &body); err != nil {
		return intentFor(err)
	}
	if body.Generation <= s.generation {
		s.dropSignal(sig, "stale restart generation")
		return nil
	}
	s.generation = body.Generation

	if err := s.peerConn.SetRemoteOffer(body.SDP); err != nil {
		return intentFor(wrapErr(ErrNegotiation, err, "pipe: apply restart offer"))
	}
	sdp, err := s.peerConn.CreateAnswer()
	if err != nil {
		return intentFor(wrapErr(ErrNegotiation, err, "pipe: create restart answer"))
	}
	if err := s.sendSignal(KindAnswer, sdpPayload{SDP: sdp}); err != nil {
		return intentFor(err)
	}
	return nil
}

func (s *session) onPionEvent(ev pionx.Event) *closeIntent {
	switch ev.Kind {
	case pionx.EventLocalCandidate:
		if err := s.sendSignal(KindCandidate, candidatePayload{
			Candidate:        ev.Candidate.Candidate,
			SDPMid:           ev.Candidate.SDPMid,
			SDPMLineIndex:    ev.Candidate.SDPMLineIndex,
			UsernameFragment: ev.Candidate.UsernameFragment,
		}); err != nil {
			// Losing one candidate is survivable; losing signaling is not.
			if errors.Is(err, ErrSignaling) && s.state != StateConnected {
				return intentFor(err)
			}
			s.log.Debug("pipe: could not trickle candidate", slog.String("error", err.Error()))
		}

	case pionx.EventGatheringComplete:
		if err := s.sendSignal(KindICEComplete, nil); err != nil {
			s.log.Debug("pipe: could not send end-of-candidates", slog.String("error", err.Error()))
		}

	case pionx.EventConnected:
		s.onConnectivityUp()

	case pionx.EventDisconnected:
		s.onConnectivityLost("disconnected")

	case pionx.EventFailed:
		if intent := s.onConnectivityFailed(); intent != nil {
			return intent
		}

	case pionx.EventPeerClosed:
		if s.state == StateConnected {
			return &closeIntent{code: CloseNormal, notify: true, err: ErrDisconnected}
		}
		return intentFor(errorf(ErrNegotiation, "pipe: peer connection closed during negotiation"))

	case pionx.EventChannelOpen:
		if intent := s.onChannelOpen(); intent != nil {
			return intent
		}

	case pionx.EventChannelClosed:
		if s.stream != nil {
			// The stream adapter observes this through its own read loop.
			return nil
		}
		return intentFor(errorf(ErrNegotiation, "pipe: data channel closed before it opened"))

	case pionx.EventError:
		if errors.Is(ev.Err, pionx.ErrChannelRejected) {
			return &closeIntent{
				code:   CloseProtocolError,
				reason: "data channel does not match the protocol contract",
				notify: true,
				err:    wrapErr(ErrProtocol, ev.Err, "pipe: data channel rejected"),
			}
		}
		if s.state == StateConnected {
			s.log.Debug("pipe: peer connection error", slog.String("error", ev.Err.Error()))
			return nil
		}
		return intentFor(wrapErr(ErrNegotiation, ev.Err, "pipe: peer connection failed"))
	}
	return nil
}

// onChannelOpen detaches the DataChannel and publishes the connection.
func (s *session) onChannelOpen() *closeIntent {
	if s.stream != nil {
		return nil
	}

	if max := s.peerConn.MaxMessageSize(); max != 0 && uint64(frame.HeaderSize+s.ep.cfg.FramePayload) > uint64(max) {
		return intentFor(errorf(ErrNegotiation,
			"pipe: frame size %d exceeds the negotiated SCTP maximum message size %d",
			frame.HeaderSize+s.ep.cfg.FramePayload, max))
	}

	ch, err := s.peerConn.Detach()
	if err != nil {
		return intentFor(wrapErr(ErrNegotiation, err, "pipe: detach data channel"))
	}

	s.stream = frame.NewStream(ch, frame.Config{
		MaxPayload: s.ep.cfg.FramePayload,
		ReadBuffer: s.ep.cfg.ReadBuffer,
	})
	s.state = StateConnected
	if s.negotiateTimer != nil {
		s.negotiateTimer.Stop()
		s.negotiateTimer = nil
	}

	elapsed := s.ep.cfg.clock.Since(s.startedAt)
	s.conn = newConn(s, s.stream, elapsed)
	s.refreshCandidateStats()
	s.ep.cfg.Metrics.Duration(metricConnectDuration, elapsed, labelRole(s.role))

	s.armKeepAlive()
	s.log.Info("pipe: connection established", slog.Duration("elapsed", elapsed))

	if intent := s.deliver(s.conn); intent != nil {
		return intent
	}
	return nil
}

// deliver hands the connection to Dial or to the listener's accept queue.
func (s *session) deliver(conn *Conn) *closeIntent {
	s.delivered = true

	if s.role == roleOfferer {
		select {
		case s.result <- dialResult{conn: conn}:
			return nil
		default:
			// Dial already gave up; close the connection we just built.
			return &closeIntent{code: CloseNormal, notify: true, err: ErrClosed}
		}
	}

	if err := s.ep.enqueueInbound(conn, s.releaseSlot); err != nil {
		return &closeIntent{
			code:   CloseNormal,
			reason: "listener is unavailable",
			notify: true,
			err:    err,
		}
	}
	// The listener owns the backlog slot from here on.
	s.releaseSlot = nil
	return nil
}

func (s *session) onConnectivityUp() {
	s.refreshCandidateStats()
	if s.recoveryTimer != nil {
		s.recoveryTimer.Stop()
		s.recoveryTimer = nil
	}
	if s.state != StateRecovering {
		return
	}
	s.state = StateConnected
	if s.conn != nil {
		s.conn.setState(StateConnected)
	}
	if !s.restartStarted.IsZero() {
		s.restarts++
		s.ep.cfg.Metrics.Duration(metricRestartDuration, s.ep.cfg.clock.Since(s.restartStarted))
		s.ep.cfg.Metrics.Count(metricRestartResults, 1, labelResult("success"))
		s.restartStarted = time.Time{}
		if s.conn != nil {
			restarts := s.restarts
			s.conn.updateStats(func(st *sessionStats) { st.ICERestarts = restarts })
		}
	}
	s.restartAttempt = 0
	s.armKeepAlive()
	s.log.Info("pipe: connectivity restored")
}

// onConnectivityLost starts bounded recovery for an established connection.
func (s *session) onConnectivityLost(reason string) {
	if s.state != StateConnected {
		return
	}
	s.state = StateRecovering
	if s.conn != nil {
		s.conn.setState(StateRecovering)
	}
	s.log.Warn("pipe: connectivity lost", slog.String("reason", reason))

	budget := s.ep.cfg.Reconnect.AttemptTimeout
	if !s.ep.cfg.Reconnect.Enabled {
		budget = s.ep.cfg.ICETimeout
	}
	s.armRecovery(budget)
}

// onConnectivityFailed handles a PeerConnection that cannot recover on its own.
func (s *session) onConnectivityFailed() *closeIntent {
	if s.state == StateConnected || s.state == StateRecovering {
		if s.state == StateConnected {
			s.onConnectivityLost("failed")
		}
		return s.attemptRestart()
	}
	return intentFor(errorf(ErrICE, "pipe: connectivity establishment failed"))
}

// onRecoveryDeadline fires when connectivity has not returned in time.
func (s *session) onRecoveryDeadline() *closeIntent {
	if s.state != StateRecovering {
		return nil
	}
	return s.attemptRestart()
}

// attemptRestart issues an ICE restart within the configured policy. Only the
// offerer restarts; the answerer waits for the restart offer.
func (s *session) attemptRestart() *closeIntent {
	policy := s.ep.cfg.Reconnect
	switch {
	case !policy.Enabled,
		s.role != roleOfferer,
		s.restartAttempt >= policy.MaxAttempts:
		return &closeIntent{
			code:   CloseNormal,
			reason: "connectivity lost",
			notify: true,
			err:    errorf(ErrDisconnected, "pipe: connection to %s was lost and could not be recovered", s.peer),
		}
	}

	s.restartAttempt++
	s.generation++
	s.restartStarted = s.ep.cfg.clock.Now()
	s.ep.cfg.Metrics.Count(metricRestartAttempts, 1)

	sdp, err := s.peerConn.CreateOffer(true)
	if err != nil {
		s.ep.cfg.Metrics.Count(metricRestartResults, 1, labelResult("error"))
		return intentFor(wrapErr(ErrICE, err, "pipe: create restart offer"))
	}
	if err := s.sendSignal(KindRestart, restartPayload{Generation: s.generation, SDP: sdp}); err != nil {
		s.ep.cfg.Metrics.Count(metricRestartResults, 1, labelResult("error"))
		return intentFor(err)
	}

	s.log.Info("pipe: ICE restart requested", slog.Int("attempt", s.restartAttempt))
	s.armRecovery(policy.AttemptTimeout + s.backoffFor(s.restartAttempt))
	return nil
}

func (s *session) backoffFor(attempt int) time.Duration {
	b := s.ep.cfg.Reconnect.Backoff
	if b.Initial <= 0 {
		return 0
	}
	d := float64(b.Initial) * math.Pow(b.Factor, float64(attempt-1))
	if max := float64(b.Maximum); d > max {
		d = max
	}
	if b.Jitter > 0 {
		d *= 1 - b.Jitter + 2*b.Jitter*rand.Float64()
	}
	return time.Duration(d)
}

func (s *session) armRecovery(d time.Duration) {
	if s.recoveryTimer != nil {
		s.recoveryTimer.Stop()
	}
	if d <= 0 {
		d = time.Second
	}
	s.recoveryTimer = s.ep.cfg.clock.NewTimer(d)
}

func (s *session) armKeepAlive() {
	if s.ep.cfg.KeepAlive.Interval <= 0 || s.stream == nil || s.pinging {
		return
	}
	if s.keepaliveTimer != nil {
		s.keepaliveTimer.Stop()
	}
	s.keepaliveTimer = s.ep.cfg.clock.NewTimer(s.ep.cfg.KeepAlive.Interval)
}

// startProbe runs one keepalive probe. The probe blocks, so it runs in its own
// goroutine and reports back through the mailbox; at most one probe is
// outstanding.
func (s *session) startProbe() {
	if s.state != StateConnected || s.stream == nil || s.pinging {
		return
	}
	s.pinging = true

	stream := s.stream
	timeout := s.ep.cfg.KeepAlive.Timeout
	box := s.box

	s.ep.wg.Add(1)
	go func() {
		defer s.ep.wg.Done()

		ctx, cancel := context.WithTimeout(s.ep.ctx, timeout)
		defer cancel()

		rtt, err := stream.Ping(ctx)
		box.post(sessionEvent{keepalive: &keepaliveResult{rtt: rtt, err: err}})
	}()
}

func (s *session) onKeepAlive(res keepaliveResult) *closeIntent {
	s.pinging = false

	if res.err != nil {
		s.ep.cfg.Metrics.Count(metricKeepAliveFailures, 1, labelReason("no-pong"))
		s.log.Warn("pipe: keepalive probe failed", slog.String("error", res.err.Error()))
		if s.state == StateConnected {
			s.onConnectivityLost("keepalive timeout")
			return s.attemptRestart()
		}
		return nil
	}

	s.ep.cfg.Metrics.Duration(metricKeepAliveRTT, res.rtt)
	if s.conn != nil {
		rtt := res.rtt
		s.conn.updateStats(func(st *sessionStats) { st.KeepAliveRTT = rtt })
	}
	s.armKeepAlive()
	return nil
}

// streamDone reports the stream's terminal channel, or nil before the stream
// exists.
func (s *session) streamDone() <-chan struct{} {
	if s.stream == nil {
		return nil
	}
	return s.stream.Done()
}

// onStreamDone converts a terminal stream condition into a close intent.
func (s *session) onStreamDone() *closeIntent {
	err := s.stream.Err()
	switch {
	case err == nil, errors.Is(err, ErrClosed):
		return &closeIntent{code: CloseNormal, notify: true, err: ErrClosed}
	case errors.Is(err, frame.ErrProtocol):
		return &closeIntent{
			code:   CloseProtocolError,
			reason: "invalid stream frame",
			notify: true,
			err:    wrapErr(ErrProtocol, err, "pipe: peer sent an invalid frame"),
		}
	default:
		// A graceful peer close reports io.EOF here.
		return &closeIntent{code: CloseNormal, notify: false, graceful: true, err: ErrClosed}
	}
}

// flushCandidates applies the candidates that arrived before the remote
// description.
func (s *session) flushCandidates() error {
	if !s.peerConn.HasRemoteDescription() {
		return nil
	}
	for _, c := range s.pendingCands {
		if err := s.peerConn.AddCandidate(c); err != nil {
			s.log.Debug("pipe: discarded buffered candidate", slog.String("error", err.Error()))
			s.ep.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("candidate"))
		}
	}
	s.pendingCands = nil

	if s.remoteComplete {
		s.remoteComplete = false
		if err := s.peerConn.EndOfRemoteCandidates(); err != nil {
			s.log.Debug("pipe: end-of-candidates rejected", slog.String("error", err.Error()))
		}
	}
	return nil
}

func (s *session) refreshCandidateStats() {
	if s.conn == nil {
		return
	}
	local, remote := s.peerConn.SelectedCandidatePair()
	if local == "" && remote == "" {
		return
	}
	s.conn.updateStats(func(st *sessionStats) {
		st.LocalCandidate = CandidateType(local)
		st.RemoteCandidate = CandidateType(remote)
	})
}

// teardown releases every resource exactly once. It runs on the session
// goroutine after the loop exits.
func (s *session) teardown(intent *closeIntent) {
	if intent == nil {
		intent = &closeIntent{code: CloseInternal, notify: true, err: ErrClosed}
	}

	s.state = StateClosed
	s.box.close()
	stopTimer(s.negotiateTimer)
	stopTimer(s.recoveryTimer)
	stopTimer(s.keepaliveTimer)
	stopTimer(s.lingerTimer)

	if intent.notify {
		s.sendCloseSignal(intent)
	}

	if s.stream != nil {
		st := s.stream.Stats()
		s.ep.cfg.Metrics.Count(metricStreamBytesRead, int64(st.BytesRead))
		s.ep.cfg.Metrics.Count(metricStreamBytesWrite, int64(st.BytesWritten))

		code, reason := streamCloseCode(intent.code), intent.reason
		switch {
		case intent.graceful && s.peerClosed:
			// Signaling proved the peer closed on purpose, so report EOF even
			// if WebRTC aborted the association before the close frame landed.
			_ = s.stream.ShutdownEOF(code, reason)
		case intent.graceful:
			// The peer ended the connection: leave received bytes readable.
			_ = s.stream.Shutdown(code, reason)
		default:
			_ = s.stream.CloseWith(code, reason)
		}
	}
	if s.peerConn != nil {
		if s.stream != nil {
			s.awaitRelease(intent)
		}
		if err := s.peerConn.Close(); err != nil {
			s.log.Debug("pipe: closing peer connection", slog.String("error", err.Error()))
		}
	}
	if s.conn != nil {
		s.conn.setState(StateClosed)
	}

	s.ep.removeSession(s)
	if s.releaseSlot != nil {
		s.releaseSlot()
		s.releaseSlot = nil
	}

	// Unblock a Dial that is still waiting.
	if !s.delivered && s.role == roleOfferer {
		err := intent.err
		if err == nil {
			err = ErrClosed
		}
		select {
		case s.result <- dialResult{err: err}:
		default:
		}
	}

	s.log.Debug("pipe: session closed", slog.String("reason", closeReason(intent)))
	close(s.finished)
}

// awaitRelease keeps the PeerConnection alive until the stream has released
// its DataChannel, which after a local close means the peer acknowledged what
// was written, and then for closeAckGrace so that this side's own
// acknowledgements reach the peer. Neither wait happens when connectivity is
// already known to be gone, because nothing could be acknowledged anyway.
func (s *session) awaitRelease(intent *closeIntent) {
	if errors.Is(intent.err, ErrDisconnected) || errors.Is(intent.err, ErrICE) {
		return
	}
	started := s.ep.cfg.clock.Now()
	release := time.NewTimer(closeReleaseTimeout)
	defer release.Stop()
	select {
	case <-s.stream.Released():
	case <-release.C:
		s.log.Debug("pipe: stream did not release its channel in time")
	}
	grace := time.NewTimer(closeAckGrace)
	defer grace.Stop()
	select {
	case <-grace.C:
	case <-s.ep.ctx.Done():
	}
	s.log.Debug("pipe: transport released", slog.Duration("after", s.ep.cfg.clock.Since(started)))
}

// sendCloseSignal makes one bounded attempt to tell the peer why the session
// ended. Failure is expected during shutdown and is not reported upward.
func (s *session) sendCloseSignal(intent *closeIntent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ep.ctx), closeSignalTimeout)
	defer cancel()

	sig, err := newSignal(KindClose, s.ep.cfg.ID, s.peer, s.id, closePayload{
		Code:   intent.code,
		Reason: intent.reason,
	})
	if err != nil {
		return
	}
	if err := s.ep.send(ctx, sig); err != nil {
		s.log.Debug("pipe: could not send close signal", slog.String("error", err.Error()))
	}
}

// sendSignal sends one signal for this session, bounded by the endpoint's send
// timeout.
func (s *session) sendSignal(kind SignalKind, payload any) error {
	sig, err := newSignal(kind, s.ep.cfg.ID, s.peer, s.id, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ep.ctx, signalSendTimeout)
	defer cancel()
	return s.ep.send(ctx, sig)
}

func (s *session) dropSignal(sig Signal, why string) {
	s.log.Debug("pipe: dropped signal",
		slog.String("kind", string(sig.Kind)),
		slog.String("why", why))
	s.ep.cfg.Metrics.Count(metricProtocolFailures, 1, labelScope("routing"))
}

// intentFor builds the terminal intent for an internal failure.
func intentFor(err error) *closeIntent {
	code := CloseInternal
	switch {
	case errors.Is(err, ErrProtocol):
		code = CloseProtocolError
	case errors.Is(err, ErrTimeout):
		code = CloseTimeout
	}
	return &closeIntent{code: code, notify: true, err: err}
}

func closeReason(intent *closeIntent) string {
	if intent.err != nil {
		return intent.err.Error()
	}
	return string(intent.code)
}

// streamCloseCode maps a signaling close code to a stream close code.
func streamCloseCode(c CloseCode) uint16 {
	switch c {
	case CloseProtocolError:
		return frame.CloseProtocolError
	case CloseGoingAway:
		return frame.CloseGoingAway
	case CloseInternal, CloseTimeout:
		return frame.CloseInternal
	default:
		return frame.CloseNormal
	}
}

func timerChan(t clock.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C()
}

func stopTimer(t clock.Timer) {
	if t != nil {
		t.Stop()
	}
}

// RejectedError reports that a peer refused a session. It matches
// [ErrPeerRejected] through [errors.Is].
type RejectedError struct {
	// Code is the machine-readable reason.
	Code RejectCode

	// Reason is a human-readable diagnostic. It never carries secrets.
	Reason string

	// Peer is the peer that refused the session.
	Peer PeerID
}

// Error implements error.
func (e *RejectedError) Error() string {
	msg := "pipe: peer " + string(e.Peer) + " rejected the session: " + string(e.Code)
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	return msg
}

// Is reports that the error belongs to the [ErrPeerRejected] category.
func (e *RejectedError) Is(target error) bool { return target == ErrPeerRejected }
