package frame

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/transport/v4/deadline"
)

// controlWriteTimeout bounds one control-frame write once the write lock is
// held.
const controlWriteTimeout = 5 * time.Second

// controlAcquireTimeout bounds how long the read loop waits for the write lock
// before dropping a pong. The bound exists so that two peers writing to each
// other under backpressure cannot deadlock on each other's control frames.
const controlAcquireTimeout = time.Second

// Channel is the detached DataChannel capability the stream needs. Pion's
// detached channel satisfies it once blocking writes are enabled.
//
// A Channel is message-oriented: one Read returns exactly one message and one
// Write sends exactly one message. Concurrent writes are safe and each message
// is written atomically.
type Channel interface {
	io.ReadWriteCloser

	// SetReadDeadline bounds a blocked Read. Setting a deadline in the past
	// must unblock a Read that is already in progress.
	SetReadDeadline(time.Time) error

	// SetWriteDeadline bounds a blocked Write.
	SetWriteDeadline(time.Time) error
}

// Config configures a [Stream].
type Config struct {
	// MaxPayload is the largest payload placed in one frame and the largest
	// payload accepted from the peer. It defaults to 16 KiB.
	MaxPayload int

	// ReadBuffer bounds received but unread application bytes. It defaults to
	// 1 MiB and is rounded up to hold at least one frame.
	ReadBuffer int
}

// Stats is a snapshot of stream counters.
type Stats struct {
	BytesRead     uint64
	BytesWritten  uint64
	FramesRead    uint64
	FramesWritten uint64
	DroppedPongs  uint64
}

// Stream converts a message-oriented [Channel] into a concurrency-safe byte
// stream with deadlines, backpressure, and internal control frames.
//
// A Stream is safe for concurrent use. One reader and one writer may run at the
// same time; multiple writers are serialized so that the frames of one Write
// never interleave with another's.
type Stream struct {
	ch         Channel
	maxPayload int

	// rsem and wsem serialize readers and writers. They are channels rather
	// than mutexes so that a blocked caller can honor its deadline.
	rsem chan struct{}
	wsem chan struct{}

	readDeadline  *deadline.Deadline
	writeDeadline *deadline.Deadline

	// wall is the absolute write deadline requested by the caller. It is
	// re-applied to the channel after a control frame borrows the lock.
	wallMu sync.Mutex
	wall   time.Time

	// wbuf is the encoding buffer for application writes, owned by whoever
	// holds wsem.
	wbuf []byte

	// dataCh carries decoded application payloads from the read loop. Its
	// capacity is the bounded unread-data budget.
	dataCh chan []byte
	// cur is the unconsumed remainder of the current payload, owned by
	// whoever holds rsem.
	cur []byte

	// closed reports a local close, which discards buffered data. term reports
	// that the stream is finished for any reason and stops the read loop. done
	// is closed when the read loop has exited.
	closed    chan struct{}
	term      chan struct{}
	done      chan struct{}
	localOnce sync.Once
	termOnce  sync.Once
	termErr   error

	mu          sync.Mutex
	readErr     error
	remoteClose *CloseInfo

	pingMu  sync.Mutex
	pending *pendingPing

	bytesRead     atomic.Uint64
	bytesWritten  atomic.Uint64
	framesRead    atomic.Uint64
	framesWritten atomic.Uint64
	droppedPongs  atomic.Uint64
}

type pendingPing struct {
	nonce [PingSize]byte
	sent  time.Time
	reply chan time.Duration
}

// NewStream wraps ch and starts its read loop. The caller must call
// [Stream.Close] to release the channel and stop the loop.
func NewStream(ch Channel, cfg Config) *Stream {
	maxPayload := cfg.MaxPayload
	if maxPayload <= 0 {
		maxPayload = 16 << 10
	}
	if maxPayload > PayloadLimit {
		maxPayload = PayloadLimit
	}
	readBuffer := cfg.ReadBuffer
	if readBuffer <= 0 {
		readBuffer = 1 << 20
	}
	depth := readBuffer / maxPayload
	if depth < 1 {
		depth = 1
	}

	s := &Stream{
		ch:            ch,
		maxPayload:    maxPayload,
		rsem:          make(chan struct{}, 1),
		wsem:          make(chan struct{}, 1),
		readDeadline:  deadline.New(),
		writeDeadline: deadline.New(),
		wbuf:          make([]byte, HeaderSize+maxPayload),
		dataCh:        make(chan []byte, depth),
		closed:        make(chan struct{}),
		term:          make(chan struct{}),
		done:          make(chan struct{}),
	}
	go s.readLoop()
	return s
}

// MaxPayload returns the negotiated maximum frame payload.
func (s *Stream) MaxPayload() int { return s.maxPayload }

// Done returns a channel that is closed when the stream is terminally finished,
// either because the peer closed it, the channel failed, or [Stream.Close] ran.
func (s *Stream) Done() <-chan struct{} { return s.done }

// Err returns the terminal error once [Stream.Done] is closed. A graceful remote
// close reports [io.EOF].
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readErr
}

// RemoteClose returns the close frame sent by the peer, if any.
func (s *Stream) RemoteClose() *CloseInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.remoteClose
}

// Stats returns a counter snapshot.
func (s *Stream) Stats() Stats {
	return Stats{
		BytesRead:     s.bytesRead.Load(),
		BytesWritten:  s.bytesWritten.Load(),
		FramesRead:    s.framesRead.Load(),
		FramesWritten: s.framesWritten.Load(),
		DroppedPongs:  s.droppedPongs.Load(),
	}
}

// Read implements [io.Reader]. It returns buffered application bytes before
// reporting a terminal condition, and never returns control-frame content.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	select {
	case s.rsem <- struct{}{}:
	case <-s.closed:
		return 0, net.ErrClosed
	case <-s.readDeadline.Done():
		return 0, os.ErrDeadlineExceeded
	}
	defer func() { <-s.rsem }()

	for len(s.cur) == 0 {
		// A local close discards buffered data, matching net.Conn.
		select {
		case <-s.closed:
			return 0, net.ErrClosed
		default:
		}
		// Prefer already-queued data over an expired deadline.
		select {
		case b, ok := <-s.dataCh:
			if !ok {
				return 0, s.terminalErr()
			}
			s.cur = b
			continue
		default:
		}
		select {
		case b, ok := <-s.dataCh:
			if !ok {
				return 0, s.terminalErr()
			}
			s.cur = b
		case <-s.closed:
			return 0, net.ErrClosed
		case <-s.readDeadline.Done():
			return 0, os.ErrDeadlineExceeded
		}
	}

	n := copy(p, s.cur)
	s.cur = s.cur[n:]
	s.bytesRead.Add(uint64(n))
	return n, nil
}

// Write implements [io.Writer]. Writes larger than the maximum frame payload are
// split into several frames while the write lock is held, so the frames of one
// call are always contiguous.
//
// On failure after some frames were sent, Write returns the number of
// application bytes committed together with the underlying error.
func (s *Stream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		// An empty write emits no frame.
		if err := s.writable(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	select {
	case s.wsem <- struct{}{}:
	case <-s.closed:
		return 0, net.ErrClosed
	case <-s.term:
		return 0, s.writeTerminalErr()
	case <-s.writeDeadline.Done():
		return 0, os.ErrDeadlineExceeded
	}
	defer func() { <-s.wsem }()

	if err := s.writable(); err != nil {
		return 0, err
	}

	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > s.maxPayload {
			chunk = chunk[:s.maxPayload]
		}
		msg, err := Encode(s.wbuf, TypeData, chunk)
		if err != nil {
			return written, err
		}
		if err := s.writeMessage(msg, s.callerDeadline()); err != nil {
			return written, err
		}
		written += len(chunk)
		s.bytesWritten.Add(uint64(len(chunk)))
		p = p[len(chunk):]
	}
	return written, nil
}

// writable reports the terminal condition that prevents further writes.
func (s *Stream) writable() error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	select {
	case <-s.term:
		return s.writeTerminalErr()
	case <-s.done:
		return s.writeTerminalErr()
	default:
	}
	return nil
}

// writeTerminalErr converts the read side's terminal error into a write error.
// Version 1 has no half-close, so a peer close ends both directions.
func (s *Stream) writeTerminalErr() error {
	err := s.terminalErr()
	if errors.Is(err, io.EOF) {
		return net.ErrClosed
	}
	return err
}

// writeMessage sends one complete frame under the supplied absolute deadline.
func (s *Stream) writeMessage(msg []byte, dl time.Time) error {
	if err := s.ch.SetWriteDeadline(dl); err != nil {
		return err
	}
	n, err := s.ch.Write(msg)
	if err != nil {
		return s.mapWriteErr(err)
	}
	if n != len(msg) {
		return fmt.Errorf("frame: channel wrote %d of %d bytes: %w", n, len(msg), io.ErrShortWrite)
	}
	s.framesWritten.Add(1)
	return nil
}

// mapWriteErr classifies a channel write failure. Our own teardown takes
// precedence, because a write that was interrupted by Close should report a
// close rather than whatever the transport happened to say.
func (s *Stream) mapWriteErr(err error) error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	select {
	case <-s.term:
		return s.writeTerminalErr()
	default:
	}
	if isTimeout(err) {
		return os.ErrDeadlineExceeded
	}
	return err
}

// callerDeadline returns the absolute write deadline requested through
// SetWriteDeadline.
func (s *Stream) callerDeadline() time.Time {
	s.wallMu.Lock()
	defer s.wallMu.Unlock()
	return s.wall
}

// writeControl sends a control frame. It borrows the write lock so that it
// cannot split a multi-frame application write, but only for a bounded time: if
// an application write holds the lock for longer, the control frame is dropped.
func (s *Stream) writeControl(t Type, payload []byte) error {
	timer := time.NewTimer(controlAcquireTimeout)
	defer timer.Stop()

	select {
	case s.wsem <- struct{}{}:
	case <-s.closed:
		return net.ErrClosed
	case <-s.term:
		return net.ErrClosed
	case <-timer.C:
		return errControlBusy
	}
	defer func() {
		// Restore the caller's deadline before releasing the lock.
		_ = s.ch.SetWriteDeadline(s.callerDeadline())
		<-s.wsem
	}()

	buf := make([]byte, HeaderSize+len(payload))
	msg, err := Encode(buf, t, payload)
	if err != nil {
		return err
	}
	return s.writeMessage(msg, time.Now().Add(controlWriteTimeout))
}

var errControlBusy = errors.New("frame: write lock busy, control frame dropped")

// Ping sends a keepalive probe and waits for the matching pong. Only one probe
// may be outstanding per stream.
func (s *Stream) Ping(ctx context.Context) (time.Duration, error) {
	var nonce [PingSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return 0, err
	}

	p := &pendingPing{nonce: nonce, sent: time.Now(), reply: make(chan time.Duration, 1)}

	s.pingMu.Lock()
	if s.pending != nil {
		s.pingMu.Unlock()
		return 0, errors.New("frame: a keepalive probe is already outstanding")
	}
	s.pending = p
	s.pingMu.Unlock()

	defer func() {
		s.pingMu.Lock()
		if s.pending == p {
			s.pending = nil
		}
		s.pingMu.Unlock()
	}()

	if err := s.writeControl(TypePing, nonce[:]); err != nil {
		return 0, err
	}

	select {
	case rtt := <-p.reply:
		return rtt, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.closed:
		return 0, net.ErrClosed
	case <-s.term:
		return 0, s.writeTerminalErr()
	}
}

// SetDeadline implements [net.Conn].
func (s *Stream) SetDeadline(t time.Time) error {
	if err := s.SetReadDeadline(t); err != nil {
		return err
	}
	return s.SetWriteDeadline(t)
}

// SetReadDeadline implements [net.Conn]. A zero time clears the deadline.
func (s *Stream) SetReadDeadline(t time.Time) error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	s.readDeadline.Set(t)
	return nil
}

// SetWriteDeadline implements [net.Conn]. A zero time clears the deadline. The
// new deadline applies to a write that is already blocked.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	s.wallMu.Lock()
	s.wall = t
	s.wallMu.Unlock()
	s.writeDeadline.Set(t)
	return s.ch.SetWriteDeadline(t)
}

// Close closes the stream locally. It is idempotent, unblocks pending reads and
// writes, discards data that has been received but not read, and makes a bounded
// best-effort attempt to send a close frame first.
func (s *Stream) Close() error { return s.CloseWith(CloseNormal, "") }

// CloseWith closes the stream locally and reports code and reason to the peer.
func (s *Stream) CloseWith(code uint16, reason string) error {
	return s.terminate(code, reason, true)
}

// Shutdown ends the stream without discarding data that has already been
// received. Pending and future reads drain the buffer and then report [io.EOF].
// Use it when the peer ended the connection, so that in-flight bytes are not
// thrown away.
func (s *Stream) Shutdown(code uint16, reason string) error {
	return s.terminate(code, reason, false)
}

// ShutdownEOF behaves like [Stream.Shutdown] but reports [io.EOF] even when the
// underlying transport already failed abruptly.
//
// The owner uses it when it has out-of-band proof that the peer closed on
// purpose, for example a close signal. WebRTC tears the SCTP association down
// with an abort, which can outrun the close frame, so without this the two peers
// could not tell an orderly close from a lost connection.
func (s *Stream) ShutdownEOF(code uint16, reason string) error {
	s.mu.Lock()
	s.readErr = io.EOF
	s.mu.Unlock()
	return s.terminate(code, reason, false)
}

func (s *Stream) terminate(code uint16, reason string, local bool) error {
	if local {
		// Unblock readers immediately and make the terminal error explicit
		// before anything else can classify it.
		s.localOnce.Do(func() {
			s.setReadErr(net.ErrClosed)
			close(s.closed)
		})
	}

	s.termOnce.Do(func() {
		s.sendCloseFrame(code, reason)
		if !local {
			s.setReadErr(io.EOF)
		}
		close(s.term)

		// Closing the underlying channel does not interrupt a read that is
		// already blocked, so expire its read deadline first.
		_ = s.ch.SetReadDeadline(time.Now().Add(-time.Second))
		s.termErr = s.ch.Close()
	})
	return s.termErr
}

// sendCloseFrame makes one best-effort attempt to tell the peer why the stream
// ended. If an application write holds the lock, the frame is skipped rather
// than waited for: the peer still observes the stream reset.
func (s *Stream) sendCloseFrame(code uint16, reason string) {
	select {
	case s.wsem <- struct{}{}:
	default:
		return
	}
	defer func() { <-s.wsem }()

	body := EncodeClose(code, reason)
	msg, err := Encode(make([]byte, HeaderSize+len(body)), TypeClose, body)
	if err != nil {
		return
	}
	_ = s.writeMessage(msg, time.Now().Add(controlWriteTimeout))
}

// readLoop is the only reader of the channel. It decodes frames, answers pings,
// records the peer's close, and hands application payloads to Read.
func (s *Stream) readLoop() {
	defer close(s.done)
	defer close(s.dataCh)

	buf := make([]byte, HeaderSize+s.maxPayload)
	for {
		n, err := s.ch.Read(buf)
		if err != nil {
			s.setReadErr(s.classifyReadErr(err))
			return
		}

		hdr, payload, err := Parse(buf[:n], s.maxPayload)
		if err != nil {
			// A protocol violation is not recoverable. The owner observes Done
			// and tears the stream down with a close frame.
			s.setReadErr(err)
			return
		}
		s.framesRead.Add(1)

		switch hdr.Type {
		case TypeData:
			if len(payload) == 0 {
				continue
			}
			cp := make([]byte, len(payload))
			copy(cp, payload)
			select {
			case s.dataCh <- cp:
			case <-s.term:
				s.setReadErr(net.ErrClosed)
				return
			}

		case TypePing:
			if err := s.writeControl(TypePong, payload); err != nil {
				if errors.Is(err, errControlBusy) {
					s.droppedPongs.Add(1)
					continue
				}
				if errors.Is(err, net.ErrClosed) {
					s.setReadErr(net.ErrClosed)
					return
				}
				s.setReadErr(err)
				return
			}

		case TypePong:
			s.deliverPong(payload)

		case TypeClose:
			info, err := ParseClose(payload)
			if err != nil {
				s.setReadErr(err)
				return
			}
			s.mu.Lock()
			s.remoteClose = &info
			s.mu.Unlock()
			s.setReadErr(io.EOF)
			return
		}
	}
}

// classifyReadErr normalizes the channel's terminal read error.
func (s *Stream) classifyReadErr(err error) error {
	select {
	case <-s.term:
		// Our own teardown forced this error; terminate already recorded the
		// terminal error, so this value is only a fallback.
		return net.ErrClosed
	default:
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return io.EOF
	}
	if errors.Is(err, io.ErrShortBuffer) {
		return fmt.Errorf("%w: peer sent a message larger than the %d byte frame limit",
			ErrProtocol, HeaderSize+s.maxPayload)
	}
	return err
}

func (s *Stream) setReadErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr == nil {
		s.readErr = err
	}
}

func (s *Stream) terminalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return s.readErr
	}
	return io.EOF
}

func (s *Stream) deliverPong(payload []byte) {
	s.pingMu.Lock()
	p := s.pending
	if p == nil || string(p.nonce[:]) != string(payload) {
		s.pingMu.Unlock()
		return
	}
	s.pending = nil
	s.pingMu.Unlock()

	select {
	case p.reply <- time.Since(p.sent):
	default:
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
