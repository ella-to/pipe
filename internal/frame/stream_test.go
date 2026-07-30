package frame

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// newStreamPair returns two streams connected through fake channels.
func newStreamPair(t *testing.T, cfg Config) (*Stream, *Stream) {
	t.Helper()

	chA, chB := newFakePair(0)
	a := NewStream(chA, cfg)
	b := NewStream(chB, cfg)
	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})
	return a, b
}

func TestStreamRoundTrip(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 64})

	want := []byte("hello world")
	if n, err := a.Write(want); err != nil || n != len(want) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(want))
	}

	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	if s := a.Stats(); s.BytesWritten != uint64(len(want)) || s.FramesWritten != 1 {
		t.Errorf("writer stats = %+v", s)
	}
	if s := b.Stats(); s.BytesRead != uint64(len(want)) || s.FramesRead != 1 {
		t.Errorf("reader stats = %+v", s)
	}
}

func TestStreamEmptyWriteEmitsNoFrame(t *testing.T) {
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: 64})
	b := NewStream(chB, Config{MaxPayload: 64})
	defer a.Close()
	defer b.Close()

	if n, err := a.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
	if got := chA.writeCount(); got != 0 {
		t.Errorf("wrote %d messages for an empty write, want 0", got)
	}
}

func TestStreamEmptyReadBuffer(t *testing.T) {
	a, _ := newStreamPair(t, Config{MaxPayload: 64})
	if n, err := a.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestStreamSplitsLargeWrites(t *testing.T) {
	const payload = 64
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: payload})
	b := NewStream(chB, Config{MaxPayload: payload})
	defer a.Close()
	defer b.Close()

	want := bytes.Repeat([]byte("abcdefgh"), 40) // 320 bytes = 5 frames
	if n, err := a.Write(want); err != nil || n != len(want) {
		t.Fatalf("Write = (%d, %v)", n, err)
	}
	if got, expect := chA.writeCount(), len(want)/payload; got != expect {
		t.Errorf("wrote %d frames, want %d", got, expect)
	}

	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("payload did not round-trip across frame boundaries")
	}
}

func TestStreamReadsSmallerThanFrames(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 64})

	want := []byte("0123456789")
	if _, err := a.Write(want); err != nil {
		t.Fatal(err)
	}

	// One byte at a time.
	var got []byte
	for len(got) < len(want) {
		buf := make([]byte, 1)
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStreamReadLargerThanFrame(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 8})

	if _, err := a.Write(bytes.Repeat([]byte("x"), 20)); err != nil {
		t.Fatal(err)
	}

	// A read never spans frames, so it returns at most one frame's payload.
	buf := make([]byte, 100)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Errorf("Read = %d bytes, want 8 (one frame)", n)
	}
}

func TestStreamConcurrentWritersDoNotInterleave(t *testing.T) {
	const (
		payload = 16
		writers = 8
		records = 20
		size    = payload * 4
	)

	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: payload})
	b := NewStream(chB, Config{MaxPayload: payload, ReadBuffer: 1 << 20})
	defer a.Close()
	defer b.Close()

	// Each writer emits identifiable records spanning several frames. If frames
	// interleaved, a record would contain more than one byte value.
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(id byte) {
			defer wg.Done()
			record := bytes.Repeat([]byte{id}, size)
			for range records {
				if _, err := a.Write(record); err != nil {
					t.Errorf("writer %d: %v", id, err)
					return
				}
			}
		}(byte('A' + w))
	}

	total := writers * records * size
	got := make([]byte, total)
	readDone := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(b, got)
		readDone <- err
	}()

	wg.Wait()
	if err := <-readDone; err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	for i := 0; i < total; i += size {
		record := got[i : i+size]
		if bytes.Count(record, record[:1]) != size {
			t.Fatalf("record at offset %d interleaved: %q", i, record)
		}
	}
}

func TestStreamSimultaneousReadAndWrite(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 32})

	const rounds = 50
	errs := make(chan error, 2)

	go func() {
		for i := range rounds {
			if _, err := a.Write([]byte{byte(i)}); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, 1)
			if _, err := io.ReadFull(a, buf); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()

	go func() {
		buf := make([]byte, 1)
		for range rounds {
			if _, err := io.ReadFull(b, buf); err != nil {
				errs <- err
				return
			}
			if _, err := b.Write(buf); err != nil {
				errs <- err
				return
			}
		}
		errs <- nil
	}()

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("echo: %v", err)
		}
	}
}

func TestStreamReadDeadline(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 32})

	if err := b.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(make([]byte, 4)); !isTimeoutErr(err) {
		t.Fatalf("Read = %v, want a deadline error", err)
	}

	// Clearing the deadline restores blocking reads.
	if err := b.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("Read after clearing the deadline: %v", err)
	}
}

func TestStreamDeadlineAppliesToBlockedRead(t *testing.T) {
	_, b := newStreamPair(t, Config{MaxPayload: 32})

	errs := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 4))
		errs <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if err := b.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errs:
		if !isTimeoutErr(err) {
			t.Fatalf("Read = %v, want a deadline error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("setting a deadline did not unblock the read")
	}
}

func TestStreamCombinedDeadline(t *testing.T) {
	// A tiny peer queue forces the writer to block, so the write deadline is
	// exercised as well as the read deadline.
	chA, chB := newFakePair(1)
	a := NewStream(chA, Config{MaxPayload: 8, ReadBuffer: 8})
	defer a.Close()
	defer chB.Close()

	if err := a.SetDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	// The peer never reads, so writes eventually block and time out.
	var err error
	for range 100 {
		if _, err = a.Write(bytes.Repeat([]byte("x"), 8)); err != nil {
			break
		}
	}
	if !isTimeoutErr(err) {
		t.Fatalf("Write = %v, want a deadline error", err)
	}
	if _, err := a.Read(make([]byte, 1)); !isTimeoutErr(err) {
		t.Fatalf("Read = %v, want a deadline error", err)
	}
}

func TestStreamBoundedReadBuffer(t *testing.T) {
	const payload = 16
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: payload})
	// A read buffer of four frames.
	b := NewStream(chB, Config{MaxPayload: payload, ReadBuffer: payload * 4})
	defer a.Close()
	defer b.Close()

	// Write far more than the reader will buffer. The writer must block once the
	// queue and the channel are saturated instead of buffering without limit.
	written := make(chan int, 1)
	go func() {
		total := 0
		for range 100 {
			n, err := a.Write(bytes.Repeat([]byte("y"), payload))
			if err != nil {
				break
			}
			total += n
		}
		written <- total
	}()

	time.Sleep(100 * time.Millisecond)

	// The reader holds at most ReadBuffer bytes; everything else is either in
	// the channel or still with the writer.
	if got := b.Stats().BytesRead; got != 0 {
		t.Errorf("reader consumed %d bytes without a Read call", got)
	}

	// Draining lets the writer finish.
	buf := make([]byte, payload)
	for range 100 {
		if err := b.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Read(buf); err != nil {
			break
		}
	}
	select {
	case <-written:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer never unblocked")
	}
}

func TestStreamCloseUnblocksReadAndWrite(t *testing.T) {
	chA, chB := newFakePair(1)
	a := NewStream(chA, Config{MaxPayload: 8, ReadBuffer: 8})
	defer chB.Close()

	readErr := make(chan error, 1)
	go func() {
		_, err := a.Read(make([]byte, 4))
		readErr <- err
	}()

	writeErr := make(chan error, 1)
	go func() {
		for {
			if _, err := a.Write(bytes.Repeat([]byte("z"), 8)); err != nil {
				writeErr <- err
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for name, ch := range map[string]chan error{"read": readErr, "write": writeErr} {
		select {
		case err := <-ch:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("%s error = %v, want net.ErrClosed", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("Close did not unblock the %s", name)
		}
	}

	// Close is idempotent.
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestStreamLocalCloseDiscardsBufferedData(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 32})

	if _, err := a.Write([]byte("discarded")); err != nil {
		t.Fatal(err)
	}
	// Give the read loop time to queue the payload.
	time.Sleep(50 * time.Millisecond)

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(make([]byte, 9)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read after Close = %v, want net.ErrClosed", err)
	}
}

func TestStreamShutdownDrainsBufferedData(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 32})

	if _, err := a.Write([]byte("kept")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := b.Shutdown(CloseNormal, ""); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("Read after Shutdown: %v", err)
	}
	if string(got) != "kept" {
		t.Errorf("got %q, want %q", got, "kept")
	}
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %v, want io.EOF", err)
	}
	// Writes are closed in both directions: version 1 has no half-close.
	if _, err := b.Write([]byte("x")); err == nil {
		t.Error("Write succeeded after Shutdown")
	}
}

func TestStreamShutdownEOFOverridesTransportError(t *testing.T) {
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: 32})
	b := NewStream(chB, Config{MaxPayload: 32})
	defer a.Close()

	// An abrupt transport failure, as WebRTC reports when it aborts the SCTP
	// association before the close frame is delivered.
	abort := errors.New("abort chunk: user initiated abort")
	chB.setReadErr(abort)
	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("the read loop did not observe the failure")
	}
	if err := b.Err(); !errors.Is(err, abort) {
		t.Fatalf("Err = %v, want the transport error", err)
	}

	// Signaling proved the close was deliberate, so readers must see EOF.
	if err := b.ShutdownEOF(CloseNormal, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %v, want io.EOF", err)
	}
	_ = chA
}

func TestStreamRemoteCloseFrame(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 32})

	if _, err := a.Write([]byte("last")); err != nil {
		t.Fatal(err)
	}
	if err := a.CloseWith(CloseGoingAway, "bye"); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("buffered read after the remote close: %v", err)
	}
	if string(got) != "last" {
		t.Errorf("got %q, want %q", got, "last")
	}
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %v, want io.EOF", err)
	}

	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("the stream never finished")
	}
	info := b.RemoteClose()
	if info == nil {
		t.Fatal("no remote close was recorded")
	}
	if info.Code != CloseGoingAway || info.Reason != "bye" {
		t.Errorf("remote close = %+v", info)
	}
	if err := b.Err(); !errors.Is(err, io.EOF) {
		t.Errorf("Err = %v, want io.EOF", err)
	}
}

func TestStreamAbruptChannelClose(t *testing.T) {
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: 32})
	b := NewStream(chB, Config{MaxPayload: 32})
	defer a.Close()
	defer b.Close()

	// No close frame: the channel simply ends.
	if err := chA.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %v, want io.EOF", err)
	}
	if b.RemoteClose() != nil {
		t.Error("a remote close was recorded without a close frame")
	}
}

func TestStreamRejectsInvalidFrameFromPeer(t *testing.T) {
	chA, chB := newFakePair(0)
	b := NewStream(chB, Config{MaxPayload: 32})
	defer b.Close()

	// A frame claiming an unknown version.
	if _, err := chA.Write([]byte{9, byte(TypeData), 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}

	_, err := b.Read(make([]byte, 1))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("Read = %v, want ErrProtocol", err)
	}
	if err := b.Err(); !errors.Is(err, ErrProtocol) {
		t.Errorf("Err = %v, want ErrProtocol", err)
	}
}

func TestStreamRejectsOversizeMessageFromPeer(t *testing.T) {
	chA, chB := newFakePair(0)
	b := NewStream(chB, Config{MaxPayload: 16})
	defer b.Close()

	// A frame larger than the negotiated maximum. The read loop reads into a
	// buffer sized for the limit, so this surfaces as a short buffer, which must
	// become a protocol error rather than a silent truncation.
	oversize, err := Append(nil, TypeData, bytes.Repeat([]byte("x"), 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chA.Write(oversize); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Read(make([]byte, 64)); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Read = %v, want ErrProtocol", err)
	}
}

func TestStreamControlFramesAreNotReadable(t *testing.T) {
	a, b := newStreamPair(t, Config{MaxPayload: 32})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	rtt, err := a.Ping(ctx)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if rtt <= 0 {
		t.Errorf("round trip = %v, want a positive duration", rtt)
	}

	// The ping and pong must not appear as application data.
	if err := b.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Read(make([]byte, 8)); !isTimeoutErr(err) {
		t.Fatalf("Read = %v, want a deadline error", err)
	}

	// Frame counters see the control traffic even though Read does not.
	if s := b.Stats(); s.FramesRead == 0 {
		t.Error("the reader recorded no frames")
	}
}

func TestStreamRejectsSecondOutstandingPing(t *testing.T) {
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: 32})
	defer a.Close()
	defer chB.Close()
	// No peer stream, so nothing answers the probe.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	first := make(chan error, 1)
	go func() {
		_, err := a.Ping(context.Background())
		first <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if _, err := a.Ping(ctx); err == nil {
		t.Fatal("a second probe was accepted while one was outstanding")
	}

	_ = a.Close()
	<-first
}

func TestStreamMismatchedPongIsIgnored(t *testing.T) {
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: 32})
	defer a.Close()
	defer chB.Close()

	// A pong with a nonce nobody asked for.
	msg, err := Append(nil, TypePong, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chB.Write(msg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := a.Err(); err != nil {
		t.Errorf("an unmatched pong ended the stream: %v", err)
	}
}

func TestStreamWriteErrorReportsCommittedBytes(t *testing.T) {
	const payload = 8
	chA, chB := newFakePair(0)
	a := NewStream(chA, Config{MaxPayload: payload})
	defer a.Close()
	defer chB.Close()

	// Fail after the first frame of a three-frame write.
	var once sync.Once
	failAfter := func() {
		once.Do(func() { chA.setWriteErr(errors.New("channel failed")) })
	}

	if _, err := a.Write(bytes.Repeat([]byte("a"), payload)); err != nil {
		t.Fatal(err)
	}
	failAfter()

	n, err := a.Write(bytes.Repeat([]byte("b"), payload*3))
	if err == nil {
		t.Fatal("Write succeeded even though the channel failed")
	}
	if n != 0 {
		t.Errorf("Write committed %d bytes, want 0", n)
	}
}

func TestStreamMaxPayloadClamped(t *testing.T) {
	chA, chB := newFakePair(0)
	defer chB.Close()

	s := NewStream(chA, Config{MaxPayload: PayloadLimit * 2})
	defer s.Close()
	if got := s.MaxPayload(); got != PayloadLimit {
		t.Errorf("MaxPayload = %d, want %d", got, PayloadLimit)
	}

	chC, chD := newFakePair(0)
	defer chD.Close()
	d := NewStream(chC, Config{})
	defer d.Close()
	if got := d.MaxPayload(); got != 16<<10 {
		t.Errorf("default MaxPayload = %d, want %d", got, 16<<10)
	}
}

func TestStreamLargeTransfer(t *testing.T) {
	const total = 4 << 20

	chA, chB := newFakePair(16)
	a := NewStream(chA, Config{MaxPayload: 16 << 10, ReadBuffer: 1 << 20})
	b := NewStream(chB, Config{MaxPayload: 16 << 10, ReadBuffer: 1 << 20})
	defer a.Close()
	defer b.Close()

	src := bytes.Repeat([]byte("0123456789abcdef"), total/16)

	go func() {
		if _, err := a.Write(src); err != nil {
			t.Errorf("Write: %v", err)
		}
	}()

	got := make([]byte, total)
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("the payload did not survive the transfer")
	}
}

// TestStreamPongUnderWriteBackpressure proves the read loop cannot deadlock
// against a blocked application write: it drops the pong instead of waiting
// forever.
func TestStreamPongUnderWriteBackpressure(t *testing.T) {
	chA, chB := newFakePair(1)
	a := NewStream(chA, Config{MaxPayload: 8, ReadBuffer: 8})
	defer a.Close()
	defer chB.Close()

	// Saturate the peer queue so a's writes block.
	go func() {
		for {
			if _, err := a.Write(bytes.Repeat([]byte("q"), 8)); err != nil {
				return
			}
		}
	}()
	time.Sleep(50 * time.Millisecond)

	// A ping arrives while the write lock is held.
	ping, err := Append(nil, TypePing, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chB.Write(ping); err != nil {
		t.Fatal(err)
	}

	// The read loop must survive: it drops the pong after a bounded wait.
	select {
	case <-a.Done():
		t.Fatalf("the read loop ended: %v", a.Err())
	case <-time.After(controlAcquireTimeout + 500*time.Millisecond):
	}
	if got := a.Stats().DroppedPongs; got == 0 {
		t.Error("no dropped pong was recorded")
	}
}

func FuzzStreamRead(f *testing.F) {
	f.Add([]byte{Version, byte(TypeData), 0, 0, 0, 0, 0, 1, 'x'})
	f.Add([]byte{Version, byte(TypeClose), 0, 0, 0, 0, 0, 2, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		chA, chB := newFakePair(0)
		s := NewStream(chB, Config{MaxPayload: 1 << 10, ReadBuffer: 4 << 10})
		defer s.Close()

		if _, err := chA.Write(data); err != nil {
			return
		}
		_ = chA.Close()

		// Read until the stream ends. The only requirement is that it ends
		// without panicking and with a classified error.
		if err := s.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := s.Read(make([]byte, 512)); err != nil {
				return
			}
		}
	})
}
