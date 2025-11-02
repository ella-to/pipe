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
	defaultDataChannelChunkSize = 16 * 1024
	dataChannelDrainTimeout     = 5 * time.Second
	dataChannelDrainInterval    = 10 * time.Millisecond
)

// dataChannelStream adapts a WebRTC data channel to behave like an io.ReadWriteCloser.
// It converts the message-oriented semantics of the data channel into a streaming
// reader by using an in-memory pipe.
type dataChannelStream struct {
	dc        *webrtc.DataChannel
	reader    *io.PipeReader
	writer    *io.PipeWriter
	writeMu   sync.Mutex
	closeOnce sync.Once

	// pipeClosed ensures we only tear down the pipe once, either due to Close
	// or CloseWithError triggered from the remote side.
	pipeClosed atomic.Bool

	chunkSize int
}

func newDataChannelStream(dc *webrtc.DataChannel) *dataChannelStream {
	pr, pw := io.Pipe()

	stream := &dataChannelStream{
		dc:        dc,
		reader:    pr,
		writer:    pw,
		chunkSize: defaultDataChannelChunkSize,
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if len(msg.Data) == 0 {
			return
		}
		if _, err := pw.Write(msg.Data); err != nil {
			// If the reader side has been closed, propagate the error so that
			// subsequent reads observe it and stop the goroutine from blocking.
			stream.CloseWithError(err)
		}
	})

	return stream
}

func (s *dataChannelStream) Read(p []byte) (int, error) {
	return s.reader.Read(p)
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
	remaining := p
	for len(remaining) > 0 {
		chunk := len(remaining)
		if chunk > s.chunkSize {
			chunk = s.chunkSize
		}

		if err := s.dc.Send(remaining[:chunk]); err != nil {
			if total > 0 {
				return total, err
			}
			return 0, err
		}

		total += chunk
		remaining = remaining[chunk:]
	}

	return total, nil
}

func (s *dataChannelStream) Close() error {
	var closeErr error

	s.closeOnce.Do(func() {
		s.CloseWithError(io.EOF)

		ctx, cancel := context.WithTimeout(context.Background(), dataChannelDrainTimeout)
		defer cancel()

		if err := s.waitForDrain(ctx); err != nil && closeErr == nil {
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
		s.writer.CloseWithError(err)
	}
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
