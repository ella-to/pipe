package pipe

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestRecvBufferReadAfterClose(t *testing.T) {
	b := newRecvBuffer(1024)

	if err := b.push([]byte("hello")); err != nil {
		t.Fatalf("push: %v", err)
	}
	b.closeWithError(io.EOF)

	// Buffered data must still be readable after close.
	buf := make([]byte, 16)
	n, err := b.read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("read %q, want %q", buf[:n], "hello")
	}

	// Once drained, the sticky error is returned.
	if _, err := b.read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("read after drain: %v, want io.EOF", err)
	}

	// Pushes after close fail.
	if err := b.push([]byte("x")); !errors.Is(err, io.EOF) {
		t.Fatalf("push after close: %v, want io.EOF", err)
	}
}

func TestRecvBufferPartialAndMultiMessageRead(t *testing.T) {
	b := newRecvBuffer(1024)

	if err := b.push([]byte("abcd")); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := b.push([]byte("efgh")); err != nil {
		t.Fatalf("push: %v", err)
	}

	small := make([]byte, 2)
	n, err := b.read(small)
	if err != nil || string(small[:n]) != "ab" {
		t.Fatalf("read = %q, %v; want %q", small[:n], err, "ab")
	}

	// A large read should coalesce the remainder of both messages.
	large := make([]byte, 16)
	n, err = b.read(large)
	if err != nil || string(large[:n]) != "cdefgh" {
		t.Fatalf("read = %q, %v; want %q", large[:n], err, "cdefgh")
	}
}

func TestRecvBufferBackpressureUnblocks(t *testing.T) {
	b := newRecvBuffer(4)

	if err := b.push([]byte("fill")); err != nil {
		t.Fatalf("push: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	pushed := make(chan error, 1)
	go func() {
		defer wg.Done()
		pushed <- b.push([]byte("more"))
	}()

	select {
	case err := <-pushed:
		t.Fatalf("push should have blocked on a full buffer, returned %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	buf := make([]byte, 4)
	if _, err := b.read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}

	select {
	case err := <-pushed:
		if err != nil {
			t.Fatalf("push: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("push did not unblock after read freed space")
	}
	wg.Wait()
}

func TestRecvBufferCloseUnblocksBlockedPush(t *testing.T) {
	b := newRecvBuffer(4)

	if err := b.push([]byte("full")); err != nil {
		t.Fatalf("push: %v", err)
	}
	pushErr := make(chan error, 1)
	go func() {
		pushErr <- b.push([]byte("blocked"))
	}()

	time.Sleep(20 * time.Millisecond)
	b.closeWithError(io.ErrClosedPipe)

	select {
	case err := <-pushErr:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("push: %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked push did not unblock on close")
	}
}

func TestRecvBufferCloseUnblocksBlockedRead(t *testing.T) {
	b := newRecvBuffer(4)

	readErr := make(chan error, 1)
	go func() {
		_, err := b.read(make([]byte, 4))
		readErr <- err
	}()

	time.Sleep(20 * time.Millisecond)
	b.closeWithError(io.ErrClosedPipe)

	select {
	case err := <-readErr:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("read: %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked read did not unblock on close")
	}
}

func BenchmarkRecvBuffer(b *testing.B) {
	rb := newRecvBuffer(dataChannelRecvBufferSize)
	msgSize := 16 * 1024
	readBuf := make([]byte, 32*1024)

	b.SetBytes(int64(msgSize))
	b.ResetTimer()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var total int64
		want := int64(msgSize) * int64(b.N)
		for total < want {
			n, err := rb.read(readBuf)
			if err != nil {
				return
			}
			total += int64(n)
		}
	}()

	for i := 0; i < b.N; i++ {
		// Allocate per push: the buffer takes ownership, mirroring how the
		// data channel hands over a fresh slice per message.
		if err := rb.push(make([]byte, msgSize)); err != nil {
			b.Fatal(err)
		}
	}
	<-done
}
