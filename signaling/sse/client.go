package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ella.to/pipe"
	"ella.to/sse"
)

// Client-side defaults.
const (
	// DefaultInbox bounds signals received from the stream but not yet
	// returned by Receive.
	DefaultInbox = 64

	// closeGrace bounds how long Close waits for the stream goroutine.
	closeGrace = 5 * time.Second

	// errorBodyLimit bounds how much of an error response is read into the
	// returned error message.
	errorBodyLimit = 1 << 10
)

// ErrPermanent marks a stream failure that reconnecting cannot fix, such as a
// rejected credential or a wrong URL. The [pipe.Endpoint] receive loop stops
// on it and every later Dial fails until the endpoint is rebuilt with a
// corrected configuration.
var ErrPermanent = errors.New("sse: permanent failure")

// Client is a [pipe.Signaler] that talks to a [Server]. The zero value is not
// usable; URL is required and Token or Authorize is needed unless the server
// uses [TrustPeerHeader].
//
// A Client is safe for concurrent use and may be shared by several endpoints,
// each of which calls Open with its own peer ID. Authentication is per request,
// so a shared Client that must act as several peers sets Authorize.
type Client struct {
	// URL is the address the Server is mounted at, for example
	// https://signal.example.net/pipe.
	URL string

	// Token, when set, is sent as a bearer token on every request.
	Token string

	// Authorize, when set, is called on every outgoing request after the
	// default headers are applied. Use it for credentials that are not a
	// single static bearer token, or to select a token per peer.
	Authorize func(r *http.Request, local pipe.PeerID)

	// HTTPClient issues requests. It defaults to [http.DefaultClient]. Its
	// Timeout must be zero; a timeout would cut the event stream.
	HTTPClient *http.Client

	// Backoff spaces stream reconnection attempts. Zero fields take the
	// defaults of 500ms initial, 10s maximum, factor 2, jitter 0.2.
	Backoff pipe.Backoff

	// Inbox bounds signals received but not yet consumed. It defaults to
	// [DefaultInbox].
	Inbox int

	// Logger receives structured logs. Nothing is logged when nil.
	Logger *slog.Logger
}

var _ pipe.Signaler = (*Client)(nil)

// Open connects the event stream for local and returns once the server has
// confirmed the identity, so that a bad token or a mismatched peer ID fails
// here rather than at the first Dial.
func (c *Client) Open(ctx context.Context, local pipe.PeerID) (pipe.SignalConn, error) {
	if c.URL == "" {
		return nil, errors.New("sse: Client.URL is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	conn := &conn{
		client: c,
		local:  local,
		http:   c.HTTPClient,
		log:    c.logger().With(slog.String("local", string(local))),
		inbox:  make(chan pipe.Signal, c.inbox()),
		ready:  make(chan struct{}),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	if conn.http == nil {
		conn.http = http.DefaultClient
	}
	conn.ctx, conn.cancel = context.WithCancel(context.Background())

	go conn.run()

	select {
	case <-conn.ready:
		return conn, nil
	case <-conn.done:
		return nil, conn.terminal()
	case <-ctx.Done():
		_ = conn.Close()
		return nil, ctx.Err()
	}
}

func (c *Client) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (c *Client) inbox() int {
	if c.Inbox > 0 {
		return c.Inbox
	}
	return DefaultInbox
}

func (c *Client) backoff() pipe.Backoff {
	b := c.Backoff
	if b.Initial <= 0 {
		b.Initial = 500 * time.Millisecond
	}
	if b.Maximum <= 0 {
		b.Maximum = 10 * time.Second
	}
	if b.Maximum < b.Initial {
		b.Maximum = b.Initial
	}
	if b.Factor < 1 {
		b.Factor = 2
	}
	if b.Jitter < 0 || b.Jitter > 1 {
		b.Jitter = 0.2
	}
	return b
}

// conn is one peer's signaling connection.
type conn struct {
	client *Client
	local  pipe.PeerID
	http   *http.Client
	log    *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	inbox chan pipe.Signal

	// ready is closed after the first successful hello.
	ready     chan struct{}
	readyOnce sync.Once

	// closed is closed by Close; done is closed when the stream goroutine
	// exits.
	closed    chan struct{}
	closeOnce sync.Once
	done      chan struct{}

	mu sync.Mutex
	// lastID is the highest event ID received, used to seed the replay
	// cursor of the next receiver.
	lastID   uint64
	termErr  error
	receiver *sse.HttpReceiver
}

var _ pipe.SignalConn = (*conn)(nil)

// Send posts one signal.
func (c *conn) Send(ctx context.Context, msg pipe.Signal) error {
	if err := c.checkOpen(); err != nil {
		return err
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("sse: encode signal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.client.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sse: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Signals are idempotent: receivers discard duplicates by ID. Saying so
	// lets the HTTP client retry the POST on a fresh connection when a pooled
	// keep-alive connection turns out to have been closed by the server or a
	// proxy, which would otherwise surface as an EOF.
	req.Header.Set("Idempotency-Key", msg.ID)
	c.decorate(req)

	resp, err := c.http.Do(req)
	if err != nil {
		if cerr := c.checkOpen(); cerr != nil {
			return cerr
		}
		return fmt.Errorf("sse: send %s: %w", msg.Kind, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusAccepted, http.StatusOK, http.StatusNoContent:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("sse: send %s to %s: %w", msg.Kind, msg.To, pipe.ErrPeerUnavailable)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("sse: send %s: %s: %w", msg.Kind, statusMessage(resp), ErrUnauthorized)
	default:
		return fmt.Errorf("sse: send %s: %s", msg.Kind, statusMessage(resp))
	}
}

// Receive returns the next signal from the stream.
func (c *conn) Receive(ctx context.Context) (pipe.Signal, error) {
	// Deliver what has already arrived before reporting any failure.
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
	case <-c.done:
		return pipe.Signal{}, c.terminal()
	case <-ctx.Done():
		return pipe.Signal{}, ctx.Err()
	}
}

// Close ends the stream. It is idempotent and unblocks Send and Receive.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.cancel()
		// The receiver blocks in a body read that the context does not
		// interrupt; closing it does.
		c.mu.Lock()
		rcv := c.receiver
		c.mu.Unlock()
		if rcv != nil {
			_ = rcv.Close()
		}
	})
	select {
	case <-c.done:
	case <-time.After(closeGrace):
		c.log.Warn("sse: stream goroutine did not exit in time")
	}
	return nil
}

func (c *conn) checkOpen() error {
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	select {
	case <-c.done:
		return c.terminal()
	default:
		return nil
	}
}

// terminal returns the error that ended the stream goroutine.
func (c *conn) terminal() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.termErr != nil {
		return c.termErr
	}
	return net.ErrClosed
}

func (c *conn) setTerminal(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.termErr == nil {
		c.termErr = err
	}
}

// decorate applies identity headers to a request.
func (c *conn) decorate(req *http.Request) {
	req.Header.Set(PeerHeader, string(c.local))
	if c.client.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.client.Token)
	}
	if c.client.Authorize != nil {
		c.client.Authorize(req, c.local)
	}
}

// run keeps the event stream attached until Close or a permanent failure.
func (c *conn) run() {
	defer close(c.done)

	backoff := c.client.backoff()
	attempt := 0
	for {
		err := c.streamOnce()
		if c.ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrPermanent) {
			c.setTerminal(err)
			c.log.Warn("sse: stream failed permanently", slog.String("error", err.Error()))
			return
		}

		attempt++
		delay := delayFor(backoff, attempt)
		c.log.Debug("sse: stream lost, reconnecting",
			slog.String("error", errString(err)), slog.Duration("in", delay))

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-c.ctx.Done():
			timer.Stop()
			return
		}
	}
}

// streamOnce runs one receiver until it fails. The receiver is created with a
// single connection attempt so that every failure surfaces here, where it is
// classified and, unless permanent, retried with backoff by run. The replay
// cursor is carried across receivers explicitly.
func (c *conn) streamOnce() error {
	c.mu.Lock()
	lastID := c.lastID
	c.mu.Unlock()

	var wrongType bool
	opts := []sse.HttpReceiverOption{
		sse.WithHttpReceiverClient(c.http),
		sse.WithHttpReceiverRetry(1, 0),
		sse.WithHttpReceiverRequest(c.decorate),
		sse.WithHttpReceiverRespHeader(func(h http.Header) {
			wrongType = !strings.HasPrefix(h.Get("Content-Type"), "text/event-stream")
		}),
	}
	if lastID > 0 {
		opts = append(opts, sse.WithHttpReceiverLastEventID(strconv.FormatUint(lastID, 10)))
	}

	rcv, err := sse.CreateHttpReceiver(c.client.URL, opts...)
	if err != nil {
		return c.classify(err)
	}

	c.mu.Lock()
	closed := c.ctx.Err() != nil
	if !closed {
		c.receiver = rcv
	}
	c.mu.Unlock()
	if closed {
		_ = rcv.Close()
		return nil
	}
	defer func() {
		c.mu.Lock()
		c.receiver = nil
		c.mu.Unlock()
		_ = rcv.Close()
	}()

	for {
		if wrongType {
			return fmt.Errorf("%w: %s did not return an event stream", ErrPermanent, c.client.URL)
		}
		msg, err := rcv.Receive()
		if err != nil {
			return c.classify(err)
		}
		if msg.Event == "" {
			// A keepalive comment.
			continue
		}
		if err := c.handleEvent(msg.Event, msg.Id, []byte(msg.Data)); err != nil {
			return err
		}
	}
}

// classify separates failures that reconnecting cannot fix from those it can,
// using the status the receiver reports for a refused stream request.
func (c *conn) classify(err error) error {
	if c.ctx.Err() != nil {
		return nil
	}
	var status *sse.StatusError
	if !errors.As(err, &status) {
		return fmt.Errorf("sse: stream: %w", err)
	}
	text := statusText(strconv.Itoa(status.Code)+" "+http.StatusText(status.Code), []byte(status.Body))
	switch status.Code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s: %w", ErrPermanent, text, ErrUnauthorized)
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: %s is not a pipe signaling server (%s)", ErrPermanent, c.client.URL, text)
	default:
		return fmt.Errorf("sse: stream: %s", text)
	}
}

// handleEvent applies one complete event.
func (c *conn) handleEvent(event, id string, data []byte) error {
	switch event {
	case eventHello:
		var peer string
		if err := json.Unmarshal(data, &peer); err != nil {
			return fmt.Errorf("%w: malformed hello event", ErrPermanent)
		}
		if pipe.PeerID(peer) != c.local {
			return fmt.Errorf("%w: credential belongs to peer %q, not %q", ErrPermanent, peer, c.local)
		}
		c.readyOnce.Do(func() { close(c.ready) })
		return nil

	case eventSignal:
		var sig pipe.Signal
		if err := json.Unmarshal(data, &sig); err != nil {
			c.log.Debug("sse: dropped undecodable signal", slog.String("error", err.Error()))
			return nil
		}
		if id != "" {
			if n, err := strconv.ParseUint(id, 10, 64); err == nil {
				c.mu.Lock()
				c.lastID = n
				c.mu.Unlock()
			}
		}
		select {
		case c.inbox <- sig:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
		return nil

	case eventReplaced:
		// The server attached a newer stream for this peer, which happens when
		// a second process opens the same peer ID. This one is done.
		return fmt.Errorf("%w: another client is signaling as %q", ErrPermanent, c.local)

	case eventShutdown:
		// The server is going away; reconnect with backoff.
		return errors.New("sse: server is shutting down")

	default:
		return nil
	}
}

func delayFor(b pipe.Backoff, attempt int) time.Duration {
	d := float64(b.Initial) * math.Pow(b.Factor, float64(attempt-1))
	if max := float64(b.Maximum); d > max {
		d = max
	}
	if b.Jitter > 0 {
		d *= 1 - b.Jitter + 2*b.Jitter*rand.Float64()
	}
	return time.Duration(d)
}

// statusMessage renders a response status with the start of its body.
func statusMessage(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	return statusText(resp.Status, body)
}

func statusText(status string, body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return status
	}
	return status + ": " + text
}

func errString(err error) string {
	if err == nil {
		return "stream ended"
	}
	return err.Error()
}
