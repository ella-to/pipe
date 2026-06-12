package pipe

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	// Outbound writes are split into chunks of at most this size. 64 KiB is
	// the safe ceiling from RFC 8831 when the peer announces no
	// max-message-size, and matches what pion itself announces.
	defaultDataChannelChunkSize = 64 * 1024
	dataChannelDrainTimeout     = 5 * time.Second
	dataChannelDrainInterval    = 10 * time.Millisecond
	dataChannelWriteTimeout     = 15 * time.Second
	// Writes pause once this much data is queued in the SCTP send buffer and
	// resume when it drains below dataChannelLowWatermark. The gap between the
	// two provides hysteresis so the writer is not woken for every message.
	dataChannelHighWatermark = 1024 * 1024
	dataChannelLowWatermark  = 512 * 1024
	// Bound on inbound data buffered between the data channel and Read calls.
	// When full, message delivery blocks, which propagates backpressure to the
	// sender through SCTP flow control.
	dataChannelRecvBufferSize = 1024 * 1024
)

// recvBuffer is a bounded FIFO of inbound message payloads. It takes
// ownership of pushed slices (the data channel allocates a fresh slice per
// message), so the read path adds no extra copy beyond Read itself.
type recvBuffer struct {
	mu       sync.Mutex
	readable sync.Cond
	writable sync.Cond
	queue    [][]byte
	offset   int // read offset into queue[0]
	size     int // total unread bytes
	maxSize  int
	err      error // sticky; reads drain buffered data first, then return it
}

func newRecvBuffer(maxSize int) *recvBuffer {
	b := &recvBuffer{maxSize: maxSize}
	b.readable.L = &b.mu
	b.writable.L = &b.mu
	return b
}

// push appends p to the buffer, taking ownership of it. It blocks while the
// buffer is full and returns the sticky error once the buffer is closed.
func (b *recvBuffer) push(p []byte) error {
	if len(p) == 0 {
		return nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for b.size >= b.maxSize && b.err == nil {
		b.writable.Wait()
	}
	if b.err != nil {
		return b.err
	}

	b.queue = append(b.queue, p)
	b.size += len(p)
	b.readable.Signal()
	return nil
}

func (b *recvBuffer) read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	for b.size == 0 {
		if b.err != nil {
			return 0, b.err
		}
		b.readable.Wait()
	}

	n := 0
	for n < len(p) && len(b.queue) > 0 {
		head := b.queue[0][b.offset:]
		c := copy(p[n:], head)
		n += c
		if c == len(head) {
			b.queue[0] = nil
			b.queue = b.queue[1:]
			b.offset = 0
		} else {
			b.offset += c
		}
	}
	b.size -= n
	b.writable.Broadcast()
	return n, nil
}

// closeWithError sets the sticky error (first caller wins) and wakes all
// waiting readers and writers.
func (b *recvBuffer) closeWithError(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
	b.readable.Broadcast()
	b.writable.Broadcast()
}

// dataChannelStream adapts a WebRTC data channel to behave like an
// io.ReadWriteCloser. Inbound messages are queued in a bounded buffer so the
// data channel's read loop is only blocked when the application falls more
// than dataChannelRecvBufferSize behind. Outbound writes are paced by the
// SCTP buffered amount using OnBufferedAmountLow signaling.
type dataChannelStream struct {
	dc        *webrtc.DataChannel
	rb        *recvBuffer
	writeMu   sync.Mutex
	closeOnce sync.Once

	// pipeClosed ensures we only tear down the stream once, either due to
	// Close or CloseWithError triggered from the remote side.
	pipeClosed atomic.Bool
	// closed is closed alongside pipeClosed to unblock pending writers.
	closed chan struct{}
	// writable receives a token whenever the SCTP send buffer drains below
	// dataChannelLowWatermark.
	writable chan struct{}

	chunkSize int
}

func newDataChannelStream(dc *webrtc.DataChannel) *dataChannelStream {
	stream := &dataChannelStream{
		dc:        dc,
		rb:        newRecvBuffer(dataChannelRecvBufferSize),
		closed:    make(chan struct{}),
		writable:  make(chan struct{}, 1),
		chunkSize: defaultDataChannelChunkSize,
	}

	dc.SetBufferedAmountLowThreshold(dataChannelLowWatermark)
	dc.OnBufferedAmountLow(func() {
		select {
		case stream.writable <- struct{}{}:
		default:
		}
	})

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// msg.Data is freshly allocated per message by the data channel, so
		// the buffer can take ownership of it. push only fails once the
		// stream is closed, at which point the data is discarded.
		_ = stream.rb.push(msg.Data)
	})

	return stream
}

func (s *dataChannelStream) Read(p []byte) (int, error) {
	return s.rb.read(p)
}

func (s *dataChannelStream) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if s.pipeClosed.Load() {
		return 0, io.ErrClosedPipe
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	total := 0
	for total < len(p) {
		chunk := min(len(p)-total, s.chunkSize)

		if err := s.waitForWritable(); err != nil {
			return total, err
		}

		if err := s.dc.Send(p[total : total+chunk]); err != nil {
			return total, err
		}

		total += chunk
	}

	return total, nil
}

func (s *dataChannelStream) Close() error {
	var closeErr error

	s.closeOnce.Do(func() {
		s.CloseWithError(io.EOF)

		ctx, cancel := context.WithTimeout(context.Background(), dataChannelDrainTimeout)
		defer cancel()

		if err := s.waitForDrain(ctx); err != nil {
			closeErr = err
		}

		if err := s.dc.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	})

	return closeErr
}

func (s *dataChannelStream) CloseWithError(err error) {
	if !s.pipeClosed.Swap(true) {
		close(s.closed)
		s.rb.closeWithError(err)
	}
}

// Sync blocks until all buffered data has been transmitted to the network layer.
// Note: This only guarantees the data left the local buffer, not that the remote
// peer received it. For end-to-end confirmation, use application-level acknowledgment.
func (s *dataChannelStream) Sync(ctx context.Context) error {
	return s.waitForDrain(ctx)
}

func (s *dataChannelStream) waitForDrain(ctx context.Context) error {
	if s.dc == nil {
		return nil
	}

	ticker := time.NewTicker(dataChannelDrainInterval)
	defer ticker.Stop()

	for {
		state := s.dc.ReadyState()
		if state != webrtc.DataChannelStateOpen && state != webrtc.DataChannelStateClosing {
			return nil
		}

		if s.dc.BufferedAmount() == 0 {
			return nil
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForWritable returns once the SCTP send buffer has room for another
// chunk. The fast path is a single atomic load; the slow path waits for the
// buffered-amount-low callback, with a coarse ticker as a safety net in case
// the callback is missed (e.g., the channel dies without firing OnClose).
func (s *dataChannelStream) waitForWritable() error {
	if s.dc == nil {
		return io.ErrClosedPipe
	}

	var (
		timer  *time.Timer
		ticker *time.Ticker
	)
	defer func() {
		if timer != nil {
			timer.Stop()
			ticker.Stop()
		}
	}()

	for {
		state := s.dc.ReadyState()
		if state != webrtc.DataChannelStateOpen && state != webrtc.DataChannelStateClosing {
			return io.ErrClosedPipe
		}

		if s.dc.BufferedAmount() <= dataChannelHighWatermark {
			return nil
		}

		if timer == nil {
			timer = time.NewTimer(dataChannelWriteTimeout)
			ticker = time.NewTicker(dataChannelDrainInterval)
		}

		select {
		case <-s.writable:
		case <-ticker.C:
		case <-s.closed:
			return io.ErrClosedPipe
		case <-timer.C:
			return context.DeadlineExceeded
		}
	}
}
