package sse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ella.to/pipe"
	"ella.to/sse"
)

// leaveTimeout bounds the request that detaches a closed peer from the
// multiplexed stream.
const leaveTimeout = 2 * time.Second

// errStreamGone reports that the server no longer knows the multiplexed
// stream a join named, so the stream must be reopened.
var errStreamGone = errors.New("sse: multiplexed stream is gone")

// Mux is a [pipe.Signaler] that carries every local peer opened on it over one
// shared event stream, instead of one stream per peer as [Client] does. Use it
// when one process hosts many peer IDs, for example one endpoint per user or
// per device:
//
//	mux := &sse.Mux{URL: "https://signal.example.net/pipe", Authorize: tokenFor}
//	defer mux.Close()
//	alice, _ := pipe.New(ctx, pipe.Config{ID: "alice", Signaler: mux})
//	bob, _ := pipe.New(ctx, pipe.Config{ID: "bob", Signaler: mux})
//
// The fields mean what they mean on [Client], and the server needs no extra
// configuration: every peer still authenticates with its own credential when
// it joins the stream, so [Config.Authenticator] and [pipe.Config.AllowPeer]
// keep their guarantees.
//
// The stream is opened by the first Open and closed when the last connection
// opened on the Mux is closed. Sending still uses one POST per signal. A Mux
// is safe for concurrent use and must not be copied after first use. Opening
// the same peer ID twice on one Mux fails with [pipe.ErrDuplicatePeer].
type Mux struct {
	// URL is the address the Server is mounted at.
	URL string

	// Token, when set, is sent as a bearer token on every request.
	Token string

	// Authorize, when set, is called on every outgoing request after the
	// default headers are applied, with the peer the request acts for. Use it
	// to select each peer's credential.
	Authorize func(r *http.Request, local pipe.PeerID)

	// HTTPClient issues requests. It defaults to [http.DefaultClient]. Its
	// Timeout must be zero; a timeout would cut the event stream.
	HTTPClient *http.Client

	// Backoff spaces reconnection attempts. Zero fields take the same
	// defaults as on [Client].
	Backoff pipe.Backoff

	// Inbox bounds signals received for one peer but not yet consumed. It
	// defaults to [DefaultInbox]. A peer whose inbox is full holds up delivery
	// to every other peer on the stream until it drains; an [pipe.Endpoint]
	// drains its inbox continuously.
	Inbox int

	// Logger receives structured logs. Nothing is logged when nil.
	Logger *slog.Logger

	initOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
	// kick wakes the stream goroutine when peers are added or removed.
	kick chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	conns   map[pipe.PeerID]*muxConn
	running bool
	closed  bool
	// stream is the ID of the attached stream, empty while disconnected.
	stream string
	// rcv is the attached stream's receiver, closed to interrupt its read.
	rcv *sse.HttpReceiver
}

var _ pipe.Signaler = (*Mux)(nil)

func (m *Mux) init() {
	m.initOnce.Do(func() {
		m.ctx, m.cancel = context.WithCancel(context.Background())
		m.kick = make(chan struct{}, 1)
		m.conns = make(map[pipe.PeerID]*muxConn)
	})
}

// settings returns the Client with the same configuration, whose defaults and
// request decoration the Mux shares.
func (m *Mux) settings() *Client {
	return &Client{
		URL:        m.URL,
		Token:      m.Token,
		Authorize:  m.Authorize,
		HTTPClient: m.HTTPClient,
		Backoff:    m.Backoff,
		Inbox:      m.Inbox,
		Logger:     m.Logger,
	}
}

func (m *Mux) http() *http.Client {
	if m.HTTPClient != nil {
		return m.HTTPClient
	}
	return http.DefaultClient
}

func (m *Mux) poke() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Open joins local to the shared stream, connecting the stream first if this
// is the only open peer. It returns once the server has accepted local's
// credential.
func (m *Mux) Open(ctx context.Context, local pipe.PeerID) (pipe.SignalConn, error) {
	if m.URL == "" {
		return nil, errors.New("sse: Mux.URL is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.init()

	s := m.settings()
	c := &muxConn{
		mux:    m,
		local:  local,
		log:    s.logger().With(slog.String("local", string(local))),
		inbox:  make(chan pipe.Signal, s.inbox()),
		ready:  make(chan struct{}),
		closed: make(chan struct{}),
		failed: make(chan struct{}),
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, net.ErrClosed
	}
	if _, dup := m.conns[local]; dup {
		m.mu.Unlock()
		return nil, fmt.Errorf("sse: %q is already open on this Mux: %w", local, pipe.ErrDuplicatePeer)
	}
	m.conns[local] = c
	if !m.running {
		m.running = true
		m.wg.Add(1)
		go m.run()
	}
	m.mu.Unlock()
	m.poke()

	select {
	case <-c.ready:
		return c, nil
	case <-c.failed:
		m.remove(c)
		return nil, c.err
	case <-c.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		_ = c.Close()
		return nil, ctx.Err()
	}
}

// Close closes every connection opened on the Mux and the shared stream. It is
// idempotent; Open fails afterwards.
func (m *Mux) Close() error {
	m.init()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	conns := m.conns
	m.conns = make(map[pipe.PeerID]*muxConn)
	rcv := m.rcv
	m.mu.Unlock()

	m.cancel()
	if rcv != nil {
		_ = rcv.Close()
	}
	// Closing the stream detaches every peer on the server, so no
	// per-peer leave request is needed.
	for _, c := range conns {
		c.closeOnce.Do(func() { close(c.closed) })
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeGrace):
		m.settings().logger().Warn("sse: mux stream goroutine did not exit in time")
	}
	return nil
}

// remove forgets c if it is still the connection registered for its peer, and
// reports the stream it was joined to.
func (m *Mux) remove(c *muxConn) (stream string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.conns[c.local]; !ok || cur != c {
		return ""
	}
	delete(m.conns, c.local)
	if c.stream == m.stream {
		stream = c.stream
	}
	m.poke()
	return stream
}

// keepRunning reports whether the stream is still needed, and otherwise marks
// the goroutine stopped in the same critical section that Open checks.
func (m *Mux) keepRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.conns) == 0 {
		m.running = false
		return false
	}
	return true
}

// run keeps the shared stream attached while any peer is open.
func (m *Mux) run() {
	defer m.wg.Done()

	backoff := m.settings().backoff()
	attempt := 0
	for m.keepRunning() {
		established, err := m.session()
		if m.ctx.Err() != nil {
			m.mu.Lock()
			m.running = false
			m.mu.Unlock()
			return
		}
		if established {
			attempt = 0
		}
		if err == nil {
			continue
		}

		attempt++
		delay := delayFor(backoff, attempt)
		m.settings().logger().Debug("sse: mux stream lost, reconnecting",
			slog.String("error", err.Error()), slog.Duration("in", delay))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			timer.Stop()
		}
	}
}

// pickAuth returns an open peer whose credential authenticates the stream
// request. Which one does not matter: the stream carries nothing until peers
// join it with their own credentials.
func (m *Mux) pickAuth() *muxConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		return c
	}
	return nil
}

// session connects one stream, joins every open peer to it, and serves it until
// it fails or no peer is left. A nil error with established false asks run to
// retry immediately.
func (m *Mux) session() (established bool, err error) {
	auth := m.pickAuth()
	if auth == nil {
		return false, nil
	}
	rcv, streamID, err := m.connect(auth)
	if err != nil {
		switch {
		case m.ctx.Err() != nil:
			return false, nil
		case errors.Is(err, ErrUnauthorized):
			// The credential belongs to one peer; the others may still
			// connect.
			auth.fail(err)
			m.remove(auth)
			return false, nil
		case errors.Is(err, ErrPermanent):
			m.failAll(err)
			return false, nil
		default:
			return false, err
		}
	}

	stop := make(chan struct{})
	readErr := make(chan error, 1)
	go func() { readErr <- m.read(rcv, streamID, stop) }()

	teardown := func() {
		m.mu.Lock()
		m.stream = ""
		m.rcv = nil
		m.mu.Unlock()
		close(stop)
		_ = rcv.Close()
	}

	backoff := m.settings().backoff()
	joinAttempt := 0
	var retryTimer *time.Timer
	defer func() {
		if retryTimer != nil {
			retryTimer.Stop()
		}
	}()
	for {
		retry, gone := m.joinPending(streamID)
		if gone {
			teardown()
			<-readErr
			return true, errStreamGone
		}

		if retryTimer != nil {
			retryTimer.Stop()
		}
		var retryC <-chan time.Time
		if retry {
			joinAttempt++
			retryTimer = time.NewTimer(delayFor(backoff, joinAttempt))
			retryC = retryTimer.C
		} else {
			joinAttempt = 0
		}

		m.mu.Lock()
		idle := len(m.conns) == 0
		m.mu.Unlock()
		if idle {
			teardown()
			<-readErr
			return true, nil
		}

		select {
		case <-m.kick:
		case <-retryC:
		case err := <-readErr:
			teardown()
			if err == nil {
				err = errors.New("sse: stream ended")
			}
			return true, err
		case <-m.ctx.Done():
			teardown()
			<-readErr
			return true, nil
		}
	}
}

// connect opens a multiplexed stream authenticated as auth and waits for the
// server to name it.
func (m *Mux) connect(auth *muxConn) (*sse.HttpReceiver, string, error) {
	s := m.settings()
	var wrongType bool
	rcv, err := sse.CreateHttpReceiver(m.URL,
		sse.WithHttpReceiverClient(m.http()),
		sse.WithHttpReceiverRetry(1, 0),
		sse.WithHttpReceiverRequest(func(req *http.Request) {
			req.Header.Set(MuxHeader, "1")
			s.decorate(req, auth.local)
		}),
		sse.WithHttpReceiverRespHeader(func(h http.Header) {
			wrongType = !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
		}),
	)
	if err != nil {
		return nil, "", classifyStream(err, m.URL)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = rcv.Close()
		return nil, "", net.ErrClosed
	}
	m.rcv = rcv
	m.mu.Unlock()

	fail := func(err error) (*sse.HttpReceiver, string, error) {
		m.mu.Lock()
		m.rcv = nil
		m.mu.Unlock()
		_ = rcv.Close()
		return nil, "", err
	}

	if wrongType {
		return fail(fmt.Errorf("%w: %s did not return an event stream", ErrPermanent, m.URL))
	}
	for {
		msg, err := rcv.Receive()
		if err != nil {
			return fail(classifyStream(err, m.URL))
		}
		switch msg.Event {
		case "":
			continue
		case eventMux:
			var hello muxHello
			if err := json.Unmarshal([]byte(msg.Data), &hello); err != nil || hello.Stream == "" {
				return fail(fmt.Errorf("%w: malformed mux event", ErrPermanent))
			}
			m.mu.Lock()
			m.stream = hello.Stream
			m.mu.Unlock()
			return rcv, hello.Stream, nil
		case eventHello:
			return fail(fmt.Errorf("%w: %s does not support multiplexed streams", ErrPermanent, m.URL))
		default:
			return fail(fmt.Errorf("%w: unexpected %q event opening a multiplexed stream", ErrPermanent, msg.Event))
		}
	}
}

// joinPending joins every open peer that is not yet on streamID. It reports
// whether a join should be retried later, and whether the server no longer
// knows the stream.
func (m *Mux) joinPending(streamID string) (retry, gone bool) {
	type pending struct {
		c      *muxConn
		lastID uint64
	}
	m.mu.Lock()
	var todo []pending
	for _, c := range m.conns {
		if c.stream != streamID {
			todo = append(todo, pending{c, c.lastID})
		}
	}
	m.mu.Unlock()

	for _, p := range todo {
		err := m.join(p.c, streamID, p.lastID)
		switch {
		case err == nil:
			m.mu.Lock()
			cur, open := m.conns[p.c.local]
			open = open && cur == p.c
			if open {
				p.c.stream = streamID
			}
			m.mu.Unlock()
			if !open {
				// Closed while joining; undo the join it could not.
				m.leave(p.c.local, streamID)
				continue
			}
			p.c.readyOnce.Do(func() { close(p.c.ready) })
		case errors.Is(err, errStreamGone):
			return false, true
		case m.ctx.Err() != nil:
			return false, false
		case errors.Is(err, ErrUnauthorized):
			p.c.fail(err)
			m.remove(p.c)
		default:
			p.c.log.Debug("sse: join failed, retrying", slog.String("error", err.Error()))
			retry = true
		}
	}
	return retry, false
}

// join attaches c's peer to the stream on the server.
func (m *Mux) join(c *muxConn, streamID string, lastID uint64) error {
	req, err := http.NewRequestWithContext(m.ctx, http.MethodPut, m.URL, nil)
	if err != nil {
		return fmt.Errorf("sse: build request: %w", err)
	}
	req.Header.Set(MuxHeader, streamID)
	if lastID > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	m.settings().decorate(req, c.local)

	resp, err := m.http().Do(req)
	if err != nil {
		return fmt.Errorf("sse: join %s: %w", c.local, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return errStreamGone
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s: %w", ErrPermanent, statusMessage(resp), ErrUnauthorized)
	case http.StatusMethodNotAllowed:
		// A server that predates multiplexing would not have opened the
		// stream, so this is a proxy refusing the method.
		return fmt.Errorf("%w: join %s: %s", ErrPermanent, c.local, statusMessage(resp))
	default:
		return fmt.Errorf("sse: join %s: %s", c.local, statusMessage(resp))
	}
}

// leave detaches local from the stream on the server, best effort. Without it
// the server would keep routing local's signals to the stream until it closes.
func (m *Mux) leave(local pipe.PeerID, streamID string) {
	if m.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, leaveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set(MuxHeader, streamID)
	m.settings().decorate(req, local)
	resp, err := m.http().Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// read dispatches events from one stream until it fails or stop is closed.
func (m *Mux) read(rcv *sse.HttpReceiver, streamID string, stop <-chan struct{}) error {
	log := m.settings().logger()
	for {
		msg, err := rcv.Receive()
		if err != nil {
			select {
			case <-stop:
				return nil
			default:
			}
			return classifyStream(err, m.URL)
		}

		switch msg.Event {
		case "":
			// A keepalive comment.

		case eventSignal:
			var sig pipe.Signal
			if err := json.Unmarshal([]byte(msg.Data), &sig); err != nil {
				log.Debug("sse: dropped undecodable signal", slog.String("error", err.Error()))
				continue
			}
			m.mu.Lock()
			c := m.conns[sig.To]
			if c != nil && msg.Id != "" {
				if n, err := strconv.ParseUint(msg.Id, 10, 64); err == nil {
					c.lastID = n
				}
			}
			m.mu.Unlock()
			if c == nil {
				// Closed locally before the server learned of it.
				continue
			}
			select {
			case c.inbox <- sig:
			case <-c.closed:
			case <-c.failed:
			case <-stop:
				return nil
			}

		case eventDetached:
			var d muxDetached
			if err := json.Unmarshal([]byte(msg.Data), &d); err != nil {
				continue
			}
			m.mu.Lock()
			c := m.conns[d.Peer]
			ours := c != nil && c.stream == streamID
			m.mu.Unlock()
			if ours {
				c.fail(fmt.Errorf("%w: another client is signaling as %q", ErrPermanent, d.Peer))
				m.remove(c)
			}

		case eventShutdown:
			return errors.New("sse: server is shutting down")

		case eventMux:
			// The receiver reconnected on its own and the server opened a
			// fresh, empty stream. Start over so that every peer rejoins.
			return errors.New("sse: multiplexed stream restarted")
		}
	}
}

// failAll ends every open connection with err.
func (m *Mux) failAll(err error) {
	m.mu.Lock()
	conns := m.conns
	m.conns = make(map[pipe.PeerID]*muxConn)
	m.mu.Unlock()
	for _, c := range conns {
		c.fail(err)
	}
}

// muxConn is one peer's view of the shared stream.
type muxConn struct {
	mux   *Mux
	local pipe.PeerID
	log   *slog.Logger

	inbox chan pipe.Signal

	// ready is closed after the first successful join.
	ready     chan struct{}
	readyOnce sync.Once

	closed    chan struct{}
	closeOnce sync.Once

	// failed is closed, after err is set, when the connection can no longer
	// be used.
	failed   chan struct{}
	failOnce sync.Once
	err      error

	// Guarded by mux.mu.
	stream string
	lastID uint64
}

var _ pipe.SignalConn = (*muxConn)(nil)

// Send posts one signal.
func (c *muxConn) Send(ctx context.Context, msg pipe.Signal) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	m := c.mux
	return postSignal(ctx, m.http(), m.URL, msg, func(req *http.Request) {
		m.settings().decorate(req, c.local)
	})
}

// Receive returns the next signal addressed to this peer.
func (c *muxConn) Receive(ctx context.Context) (pipe.Signal, error) {
	select {
	case sig := <-c.inbox:
		return sig, nil
	default:
	}
	select {
	case sig := <-c.inbox:
		return sig, nil
	case <-c.closed:
		return pipe.Signal{}, net.ErrClosed
	case <-c.failed:
		return pipe.Signal{}, c.err
	case <-ctx.Done():
		return pipe.Signal{}, ctx.Err()
	}
}

// Close detaches this peer from the shared stream and closes the stream if no
// other peer is left. It is idempotent and unblocks Send and Receive.
func (c *muxConn) Close() error {
	first := false
	c.closeOnce.Do(func() {
		close(c.closed)
		first = true
	})
	if !first {
		return nil
	}
	if stream := c.mux.remove(c); stream != "" {
		c.mux.leave(c.local, stream)
	}
	return nil
}

func (c *muxConn) checkOpen() error {
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	select {
	case <-c.failed:
		return c.err
	default:
		return nil
	}
}

func (c *muxConn) fail(err error) {
	c.failOnce.Do(func() {
		c.err = err
		close(c.failed)
	})
}
