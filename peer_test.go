package pipe_test

import (
	"errors"
	"io"
	"sort"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signal/testsignal"
)

// readResult carries the outcome of an asynchronous ReadFrom.
type readResult struct {
	n    int
	from string
	data []byte
	err  error
}

// readFromAsync runs ReadFrom in a goroutine so tests can bound it with a
// timeout instead of blocking forever on failure.
func readFromAsync(p *pipe.Peer) <-chan readResult {
	ch := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 64*1024)
		n, from, err := p.ReadFrom(buf)
		ch <- readResult{n: n, from: from, data: append([]byte(nil), buf[:n]...), err: err}
	}()
	return ch
}

func mustRead(t *testing.T, p *pipe.Peer, timeout time.Duration) readResult {
	t.Helper()
	select {
	case r := <-readFromAsync(p):
		if r.err != nil {
			t.Fatalf("ReadFrom: %v", r.err)
		}
		return r
	case <-time.After(timeout):
		t.Fatalf("ReadFrom timed out after %s", timeout)
		return readResult{}
	}
}

func waitForPeers(t *testing.T, p *pipe.Peer, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(p.ConnectedPeers()) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d connected peers, got %d (%v)", want, len(p.ConnectedPeers()), p.ConnectedPeers())
}

func TestPeer_ExchangeOverBus(t *testing.T) {
	bus := testsignal.New()

	alice, err := pipe.NewPeer("alice", bus)
	if err != nil {
		t.Fatalf("NewPeer alice: %v", err)
	}
	defer alice.Close()

	bob, err := pipe.NewPeer("bob", bus)
	if err != nil {
		t.Fatalf("NewPeer bob: %v", err)
	}
	defer bob.Close()

	// alice -> bob
	if n, err := alice.WriteTo("bob", []byte("ping")); err != nil || n != 4 {
		t.Fatalf("alice.WriteTo: n=%d err=%v", n, err)
	}

	r := mustRead(t, bob, 10*time.Second)
	if string(r.data) != "ping" || r.from != "alice" {
		t.Fatalf("bob received (%q, from=%q), want (ping, alice)", r.data, r.from)
	}

	// bob -> alice should reuse the session established by the accept side.
	if n, err := bob.WriteTo("alice", []byte("pong")); err != nil || n != 4 {
		t.Fatalf("bob.WriteTo: n=%d err=%v", n, err)
	}

	r = mustRead(t, alice, 10*time.Second)
	if string(r.data) != "pong" || r.from != "bob" {
		t.Fatalf("alice received (%q, from=%q), want (pong, bob)", r.data, r.from)
	}

	waitForPeers(t, alice, 1, 5*time.Second)
	waitForPeers(t, bob, 1, 5*time.Second)

	if got := alice.ConnectedPeers(); len(got) != 1 || got[0] != "bob" {
		t.Fatalf("alice.ConnectedPeers = %v, want [bob]", got)
	}
	if got := bob.ConnectedPeers(); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("bob.ConnectedPeers = %v, want [alice]", got)
	}
}

func TestPeer_MultipleDatagramsPreserveBoundaries(t *testing.T) {
	bus := testsignal.New()

	alice, err := pipe.NewPeer("alice", bus)
	if err != nil {
		t.Fatalf("NewPeer alice: %v", err)
	}
	defer alice.Close()

	bob, err := pipe.NewPeer("bob", bus)
	if err != nil {
		t.Fatalf("NewPeer bob: %v", err)
	}
	defer bob.Close()

	msgs := []string{"first", "second", "third", "fourth"}
	for _, m := range msgs {
		if _, err := alice.WriteTo("bob", []byte(m)); err != nil {
			t.Fatalf("WriteTo %q: %v", m, err)
		}
	}

	for i, want := range msgs {
		r := mustRead(t, bob, 10*time.Second)
		if string(r.data) != want {
			t.Fatalf("datagram %d: got %q, want %q", i, r.data, want)
		}
		if r.from != "alice" {
			t.Fatalf("datagram %d: from=%q, want alice", i, r.from)
		}
	}
}

func TestPeer_ReadFromTruncates(t *testing.T) {
	bus := testsignal.New()

	alice, err := pipe.NewPeer("alice", bus)
	if err != nil {
		t.Fatalf("NewPeer alice: %v", err)
	}
	defer alice.Close()

	bob, err := pipe.NewPeer("bob", bus)
	if err != nil {
		t.Fatalf("NewPeer bob: %v", err)
	}
	defer bob.Close()

	if _, err := alice.WriteTo("bob", []byte("0123456789")); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}

	ch := make(chan readResult, 1)
	go func() {
		small := make([]byte, 4) // smaller than the 10-byte datagram
		n, from, err := bob.ReadFrom(small)
		ch <- readResult{n: n, from: from, data: append([]byte(nil), small[:n]...), err: err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("ReadFrom: %v", r.err)
		}
		if r.n != 4 || string(r.data) != "0123" {
			t.Fatalf("truncated read = (%d, %q), want (4, 0123)", r.n, r.data)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ReadFrom timed out")
	}
}

func TestPeer_CloseReturnsClosedPipe(t *testing.T) {
	bus := testsignal.New()

	p, err := pipe.NewPeer("solo", bus)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second close is a no-op.
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if _, err := p.WriteTo("other", []byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("WriteTo after Close: got %v, want io.ErrClosedPipe", err)
	}

	if _, _, err := p.ReadFrom(make([]byte, 16)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("ReadFrom after Close: got %v, want io.ErrClosedPipe", err)
	}
}

func TestPeer_ReadFromUnblocksOnClose(t *testing.T) {
	bus := testsignal.New()

	p, err := pipe.NewPeer("waiter", bus)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}

	ch := readFromAsync(p)

	// Give the blocking ReadFrom a moment to park.
	time.Sleep(50 * time.Millisecond)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case r := <-ch:
		if !errors.Is(r.err, io.ErrClosedPipe) {
			t.Fatalf("blocked ReadFrom unblocked with %v, want io.ErrClosedPipe", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadFrom did not unblock after Close")
	}
}

func TestPeer_Validation(t *testing.T) {
	bus := testsignal.New()

	if _, err := pipe.NewPeer("", bus); !errors.Is(err, pipe.ErrEmptyPeerID) {
		t.Fatalf("NewPeer empty id: got %v, want ErrEmptyPeerID", err)
	}

	p, err := pipe.NewPeer("self", bus)
	if err != nil {
		t.Fatalf("NewPeer: %v", err)
	}
	defer p.Close()

	if _, err := p.WriteTo("self", []byte("x")); !errors.Is(err, pipe.ErrSelfPeer) {
		t.Fatalf("WriteTo self: got %v, want ErrSelfPeer", err)
	}
	if _, err := p.WriteTo("", []byte("x")); !errors.Is(err, pipe.ErrEmptyPeerID) {
		t.Fatalf("WriteTo empty: got %v, want ErrEmptyPeerID", err)
	}
	if _, err := p.WriteTo("peer", make([]byte, 1<<20+1)); !errors.Is(err, pipe.ErrDatagramTooLarge) {
		t.Fatalf("WriteTo oversized: got %v, want ErrDatagramTooLarge", err)
	}
}

func TestPeer_IdleEvictionAndReconnect(t *testing.T) {
	bus := testsignal.New()

	idle := 300 * time.Millisecond

	alice, err := pipe.NewPeer("alice", bus, pipe.WithPeerIdleTimeout(idle))
	if err != nil {
		t.Fatalf("NewPeer alice: %v", err)
	}
	defer alice.Close()

	bob, err := pipe.NewPeer("bob", bus, pipe.WithPeerIdleTimeout(idle))
	if err != nil {
		t.Fatalf("NewPeer bob: %v", err)
	}
	defer bob.Close()

	// Establish a session.
	if _, err := alice.WriteTo("bob", []byte("hello")); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	r := mustRead(t, bob, 10*time.Second)
	if string(r.data) != "hello" {
		t.Fatalf("got %q, want hello", r.data)
	}
	waitForPeers(t, alice, 1, 5*time.Second)

	// Stay idle past the timeout; the janitor should evict the session.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(alice.ConnectedPeers()) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := alice.ConnectedPeers(); len(got) != 0 {
		t.Fatalf("session not evicted after idle timeout: %v", got)
	}

	// A new WriteTo must transparently re-establish the connection.
	if _, err := alice.WriteTo("bob", []byte("again")); err != nil {
		t.Fatalf("WriteTo after eviction: %v", err)
	}
	r = mustRead(t, bob, 10*time.Second)
	if string(r.data) != "again" || r.from != "alice" {
		t.Fatalf("after reconnect got (%q, %q), want (again, alice)", r.data, r.from)
	}
	waitForPeers(t, alice, 1, 5*time.Second)
}

func TestPeer_ThreePeersFanOut(t *testing.T) {
	bus := testsignal.New()

	hub, err := pipe.NewPeer("hub", bus)
	if err != nil {
		t.Fatalf("NewPeer hub: %v", err)
	}
	defer hub.Close()

	leafA, err := pipe.NewPeer("leafA", bus)
	if err != nil {
		t.Fatalf("NewPeer leafA: %v", err)
	}
	defer leafA.Close()

	leafB, err := pipe.NewPeer("leafB", bus)
	if err != nil {
		t.Fatalf("NewPeer leafB: %v", err)
	}
	defer leafB.Close()

	if _, err := hub.WriteTo("leafA", []byte("to-a")); err != nil {
		t.Fatalf("WriteTo leafA: %v", err)
	}
	if _, err := hub.WriteTo("leafB", []byte("to-b")); err != nil {
		t.Fatalf("WriteTo leafB: %v", err)
	}

	ra := mustRead(t, leafA, 10*time.Second)
	if string(ra.data) != "to-a" || ra.from != "hub" {
		t.Fatalf("leafA got (%q, %q), want (to-a, hub)", ra.data, ra.from)
	}
	rb := mustRead(t, leafB, 10*time.Second)
	if string(rb.data) != "to-b" || rb.from != "hub" {
		t.Fatalf("leafB got (%q, %q), want (to-b, hub)", rb.data, rb.from)
	}

	waitForPeers(t, hub, 2, 5*time.Second)
	got := hub.ConnectedPeers()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "leafA" || got[1] != "leafB" {
		t.Fatalf("hub.ConnectedPeers = %v, want [leafA leafB]", got)
	}
}
