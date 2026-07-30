// Package integration exercises two endpoints against each other over
// in-process signaling. These tests use real Pion PeerConnections over host
// candidates only, so they need no external STUN or TURN server.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/internal/testutil"
	"ella.to/pipe/signaling/memory"
)

const testTimeout = 30 * time.Second

// pair builds two endpoints on a shared in-process hub. The second endpoint is
// listening.
type pair struct {
	hub    *memory.Hub
	dialer *pipe.Endpoint
	server *pipe.Endpoint
	ln     net.Listener
}

func newPair(t *testing.T, opts ...memory.Option) *pair {
	t.Helper()

	hub := memory.New(opts...)
	p := &pair{
		hub:    hub,
		dialer: newEndpoint(t, hub, "alice", nil),
		server: newEndpoint(t, hub, "bob", nil),
	}

	ln, err := p.server.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func newEndpoint(t *testing.T, hub *memory.Hub, id pipe.PeerID, mutate func(*pipe.Config)) *pipe.Endpoint {
	t.Helper()

	cfg := pipe.Config{
		ID:          id,
		Signaler:    hub,
		DialTimeout: testTimeout,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	ep, err := pipe.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new endpoint %s: %v", id, err)
	}
	t.Cleanup(func() { _ = ep.Close() })
	return ep
}

// connect dials the listening endpoint and returns both ends.
func connect(t *testing.T, p *pair) (client, server net.Conn) {
	t.Helper()

	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan accepted, 1)
	go func() {
		c, err := p.ln.Accept()
		acceptCh <- accepted{c, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	c, err := p.dialer.Dial(ctx, "bob")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	select {
	case a := <-acceptCh:
		if a.err != nil {
			t.Fatalf("accept: %v", a.err)
		}
		return c, a.conn
	case <-time.After(testTimeout):
		t.Fatal("accept timed out")
		return nil, nil
	}
}

func TestDialAcceptBidirectional(t *testing.T) {
	testutil.CheckLeaks(t)

	p := newPair(t)
	client, server := connect(t, p)
	defer client.Close()
	defer server.Close()

	if got := client.RemoteAddr().String(); got == "" {
		t.Error("client remote address is empty")
	}
	if got := client.LocalAddr().Network(); got != "webrtc" {
		t.Errorf("network = %q, want %q", got, "webrtc")
	}
	if tc, ok := client.(*pipe.Conn); ok {
		if tc.PeerID() != "bob" {
			t.Errorf("peer = %q, want bob", tc.PeerID())
		}
		if tc.State() != pipe.StateConnected {
			t.Errorf("state = %v, want connected", tc.State())
		}
		if tc.SessionID() == "" {
			t.Error("session ID is empty")
		}
	}

	// Client to server.
	want := []byte("hello from alice")
	if _, err := client.Write(want); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("server got %q, want %q", got, want)
	}

	// Server to client.
	want = []byte("hello from bob")
	if _, err := server.Write(want); err != nil {
		t.Fatalf("server write: %v", err)
	}
	got = make([]byte, len(want))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("client got %q, want %q", got, want)
	}
}

func TestStreamSizes(t *testing.T) {
	testutil.CheckLeaks(t)

	sizes := []int{0, 1, 16 << 10, 1 << 20}
	p := newPair(t)
	client, server := connect(t, p)
	defer client.Close()
	defer server.Close()

	for _, size := range sizes {
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := client.Write(payload)
			done <- err
		}()

		got := make([]byte, size)
		if _, err := io.ReadFull(server, got); err != nil {
			t.Fatalf("read %d bytes: %v", size, err)
		}
		if err := <-done; err != nil {
			t.Fatalf("write %d bytes: %v", size, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("payload of %d bytes did not round-trip", size)
		}
	}
}

func TestLargeCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 100 MiB transfer in short mode")
	}
	testutil.CheckLeaks(t)

	const total = 100 << 20

	p := newPair(t)
	client, server := connect(t, p)
	defer client.Close()
	defer server.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	var writeErr error
	go func() {
		defer wg.Done()
		_, writeErr = io.Copy(client, io.LimitReader(newPattern(), total))
	}()

	hash := newPattern()
	buf := make([]byte, 64<<10)
	expect := make([]byte, 64<<10)
	read := 0
	for read < total {
		want := len(buf)
		if remaining := total - read; remaining < want {
			want = remaining
		}
		n, err := io.ReadFull(server, buf[:want])
		if err != nil {
			t.Fatalf("read at offset %d: %v", read, err)
		}
		if _, err := io.ReadFull(hash, expect[:n]); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf[:n], expect[:n]) {
			t.Fatalf("payload mismatch at offset %d", read)
		}
		read += n
	}

	wg.Wait()
	if writeErr != nil {
		t.Fatalf("copy: %v", writeErr)
	}
}

// newPattern returns a deterministic, non-repeating byte source.
func newPattern() io.Reader { return &pattern{} }

type pattern struct{ n uint64 }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(p.n*31 + uint64(i))
		p.n++
	}
	return len(b), nil
}

func TestConcurrentDials(t *testing.T) {
	testutil.CheckLeaks(t)

	const count = 20

	p := newPair(t)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range count {
			conn, err := p.ln.Accept()
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	errs := make(chan error, count)
	for i := range count {
		go func(i int) {
			conn, err := p.dialer.Dial(ctx, "bob")
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()

			msg := []byte{byte(i), byte(i >> 8), 'x'}
			if _, err := conn.Write(msg); err != nil {
				errs <- err
				return
			}
			got := make([]byte, len(msg))
			if _, err := io.ReadFull(conn, got); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, msg) {
				errs <- errors.New("echo mismatch")
				return
			}
			errs <- nil
		}(i)
	}

	for range count {
		if err := <-errs; err != nil {
			t.Fatalf("dial: %v", err)
		}
	}
	wg.Wait()
}

func TestCrossDial(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	a := newEndpoint(t, hub, "alice", nil)
	b := newEndpoint(t, hub, "bob", nil)

	lnA, err := a.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer lnA.Close()
	lnB, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer lnB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	type result struct {
		conn *pipe.Conn
		err  error
	}
	dials := make(chan result, 2)
	go func() {
		c, err := a.Dial(ctx, "bob")
		dials <- result{c, err}
	}()
	go func() {
		c, err := b.Dial(ctx, "alice")
		dials <- result{c, err}
	}()

	accepts := make(chan error, 2)
	go func() {
		c, err := lnA.Accept()
		if err == nil {
			defer c.Close()
		}
		accepts <- err
	}()
	go func() {
		c, err := lnB.Accept()
		if err == nil {
			defer c.Close()
		}
		accepts <- err
	}()

	sessions := make(map[string]bool)
	for range 2 {
		r := <-dials
		if r.err != nil {
			t.Fatalf("dial: %v", r.err)
		}
		if sessions[r.conn.SessionID()] {
			t.Error("the two dials share a session ID")
		}
		sessions[r.conn.SessionID()] = true
		defer r.conn.Close()
	}
	for range 2 {
		if err := <-accepts; err != nil {
			t.Fatalf("accept: %v", err)
		}
	}
}

func TestDialWithoutListener(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	a := newEndpoint(t, hub, "alice", nil)
	newEndpoint(t, hub, "bob", nil)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	_, err := a.Dial(ctx, "bob")
	if !errors.Is(err, pipe.ErrPeerRejected) {
		t.Fatalf("error = %v, want ErrPeerRejected", err)
	}

	var rejected *pipe.RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("error %v is not a *RejectedError", err)
	}
	if rejected.Code != pipe.RejectNotListening {
		t.Errorf("code = %q, want %q", rejected.Code, pipe.RejectNotListening)
	}
}

func TestDialUnknownPeer(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	a := newEndpoint(t, hub, "alice", nil)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	_, err := a.Dial(ctx, "nobody")
	if !errors.Is(err, pipe.ErrPeerUnavailable) && !errors.Is(err, pipe.ErrSignaling) {
		t.Fatalf("error = %v, want a signaling or availability failure", err)
	}
}

func TestRemoteClose(t *testing.T) {
	testutil.CheckLeaks(t)

	p := newPair(t)
	client, server := connect(t, p)
	defer client.Close()

	if _, err := server.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("server close: %v", err)
	}

	// Buffered data is delivered before EOF.
	got := make([]byte, 3)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read after remote close: %v", err)
	}
	if string(got) != "bye" {
		t.Errorf("got %q, want %q", got, "bye")
	}

	if err := client.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read = %v, want io.EOF", err)
	}
}

func TestDeadlines(t *testing.T) {
	testutil.CheckLeaks(t)

	p := newPair(t)
	client, server := connect(t, p)
	defer client.Close()
	defer server.Close()

	if err := client.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := client.Read(make([]byte, 8))
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("read error = %v, want a timeout", err)
	}
	if !errors.Is(err, pipe.ErrTimeout) {
		t.Errorf("read error %v does not match ErrTimeout", err)
	}

	// Clearing the deadline restores normal reads.
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 2)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read after clearing the deadline: %v", err)
	}
}

func TestCloseUnblocksRead(t *testing.T) {
	testutil.CheckLeaks(t)

	p := newPair(t)
	client, server := connect(t, p)
	defer server.Close()

	errs := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 8))
		errs <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read error = %v, want net.ErrClosed", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("close did not unblock the read")
	}

	// Close is idempotent.
	if err := client.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestEndpointCloseClosesConns(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	a := newEndpoint(t, hub, "alice", nil)
	b := newEndpoint(t, hub, "bob", nil)

	ln, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	client, err := a.Dial(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted

	if err := a.Close(); err != nil {
		t.Fatalf("close endpoint: %v", err)
	}

	if err := client.SetReadDeadline(time.Now().Add(testTimeout)); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("read succeeded after the endpoint closed")
	}
	if err := server.SetReadDeadline(time.Now().Add(testTimeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer read succeeded after the remote endpoint closed")
	}

	// Dialing a closed endpoint fails immediately.
	if _, err := a.Dial(ctx, "bob"); !errors.Is(err, pipe.ErrClosed) {
		t.Fatalf("dial after close = %v, want ErrClosed", err)
	}

	_ = server.Close()
	_ = ln.Close()
}

func TestListenTwice(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	ep := newEndpoint(t, hub, "alice", nil)

	ln, err := ep.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if _, err := ep.Listen(); !errors.Is(err, pipe.ErrAlreadyListening) {
		t.Fatalf("second listen = %v, want ErrAlreadyListening", err)
	}
}

func TestListenerCloseUnblocksAccept(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	ep := newEndpoint(t, hub, "alice", nil)

	ln, err := ep.Listen()
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		errs <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errs:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("accept error = %v, want net.ErrClosed", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("listener close did not unblock accept")
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestDialCancellation(t *testing.T) {
	testutil.CheckLeaks(t)

	// Dropping every answer keeps the dial pending until the caller gives up.
	hub := memory.New(memory.WithFault(memory.DropKind(pipe.KindAnswer)))
	a := newEndpoint(t, hub, "alice", nil)
	b := newEndpoint(t, hub, "bob", nil)

	ln, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err = a.Dial(ctx, "bob")
	if err == nil {
		t.Fatal("dial succeeded even though answers were dropped")
	}
	if !errors.Is(err, pipe.ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

func TestDuplicateSignals(t *testing.T) {
	testutil.CheckLeaks(t)

	p := newPair(t, memory.WithFault(memory.DuplicateAll()))
	client, server := connect(t, p)
	defer client.Close()
	defer server.Close()

	if _, err := client.Write([]byte("dup")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "dup" {
		t.Errorf("got %q, want %q", got, "dup")
	}
}

func TestReorderedCandidates(t *testing.T) {
	testutil.CheckLeaks(t)

	p := newPair(t, memory.WithFault(memory.SwapAdjacent(pipe.KindCandidate)))
	client, server := connect(t, p)
	defer client.Close()
	defer server.Close()

	if _, err := client.Write([]byte("reordered")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 9)
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatal(err)
	}
}

func TestBacklogFull(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	a := newEndpoint(t, hub, "alice", nil)
	b := newEndpoint(t, hub, "bob", func(cfg *pipe.Config) {
		cfg.AcceptBacklog = 1
	})

	ln, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	// The first dial occupies the only slot and is never accepted.
	first, err := a.Dial(ctx, "bob")
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.Close()

	_, err = a.Dial(ctx, "bob")
	if err == nil {
		t.Fatal("second dial succeeded with a full backlog")
	}
	var rejected *pipe.RejectedError
	if !errors.As(err, &rejected) || rejected.Code != pipe.RejectBusy {
		t.Fatalf("error = %v, want a busy rejection", err)
	}

	// Accepting the queued connection frees the slot.
	accepted, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	defer accepted.Close()

	third, err := a.Dial(ctx, "bob")
	if err != nil {
		t.Fatalf("dial after accept: %v", err)
	}
	defer third.Close()
}

func TestPackageLevelWrappers(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	ln, err := pipe.Listen(ctx, pipe.Config{ID: "bob", Signaler: hub})
	if err != nil {
		t.Fatal(err)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	client, err := pipe.Dial(ctx, pipe.Config{ID: "alice", Signaler: hub}, "bob")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	server := <-accepted
	if _, err := client.Write([]byte("wrapped")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatal(err)
	}

	// Closing the connection closes its hidden endpoint, which frees the peer
	// ID for reuse.
	if err := client.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if got := hub.Registered(); len(got) != 0 {
		t.Errorf("hub still has %v registered", got)
	}
}

func TestKeepAlive(t *testing.T) {
	testutil.CheckLeaks(t)

	hub := memory.New()
	a := newEndpoint(t, hub, "alice", func(cfg *pipe.Config) {
		cfg.KeepAlive = pipe.KeepAliveConfig{Interval: 50 * time.Millisecond}
	})
	b := newEndpoint(t, hub, "bob", nil)

	ln, err := b.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	client, err := a.Dial(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()

	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if client.Stats().KeepAliveRTT > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no keepalive round trip was recorded")
}
