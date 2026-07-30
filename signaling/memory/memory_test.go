package memory_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/memory"
	"ella.to/pipe/signaling/signalertest"
)

func TestConformance(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		NewSignaler: func(t *testing.T) pipe.Signaler {
			return memory.New()
		},
		RejectsDuplicatePeers:  true,
		ReportsUnavailablePeer: true,
	})
}

// TestConformanceWithTinyQueue runs the same suite with a queue barely deeper
// than one message, which forces senders to block on the reader.
func TestConformanceWithTinyQueue(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		NewSignaler: func(t *testing.T) pipe.Signaler {
			return memory.New(memory.WithQueueSize(1))
		},
		RejectsDuplicatePeers:  true,
		ReportsUnavailablePeer: true,
	})
}

func TestOpenRequiresPeerID(t *testing.T) {
	h := memory.New()

	if conn, err := h.Open(context.Background(), ""); err == nil {
		_ = conn.Close()
		t.Fatal("Open accepted an empty peer ID")
	}
}

func TestRegisteredTracksLiveConnections(t *testing.T) {
	h := memory.New()

	if got := h.Registered(); len(got) != 0 {
		t.Fatalf("a fresh hub reports %v", got)
	}

	a := open(t, h, "alice")
	open(t, h, "bob")

	if got := len(h.Registered()); got != 2 {
		t.Errorf("Registered has %d peers, want 2", got)
	}

	_ = a.Close()
	live := h.Registered()
	if len(live) != 1 || live[0] != "bob" {
		t.Errorf("Registered = %v, want [bob]", live)
	}
}

func TestDisconnectClosesOnePeer(t *testing.T) {
	h := memory.New()
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	if h.Disconnect("nobody") {
		t.Error("Disconnect reported success for an unknown peer")
	}
	if !h.Disconnect("alice") {
		t.Fatal("Disconnect reported failure for a live peer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := a.Receive(ctx); !errors.Is(err, net.ErrClosed) {
		t.Errorf("alice's Receive = %v, want net.ErrClosed", err)
	}
	// Bob is untouched, but can no longer reach alice.
	err := b.Send(ctx, signalertest.Signal("bob", "alice", pipe.NewID(), pipe.KindICEComplete, nil))
	if !errors.Is(err, pipe.ErrPeerUnavailable) {
		t.Errorf("send to a disconnected peer = %v, want ErrPeerUnavailable", err)
	}
}

// TestSendRejectsSpoofedSender proves a connection cannot be used to send on
// behalf of another peer. The hub is not an authenticator, but it must not make
// impersonation trivially free either.
func TestSendRejectsSpoofedSender(t *testing.T) {
	h := memory.New()
	a := open(t, h, "alice")
	open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := a.Send(ctx, signalertest.Signal("carol", "bob", pipe.NewID(), pipe.KindICEComplete, nil))
	if !errors.Is(err, pipe.ErrProtocol) {
		t.Fatalf("send with a forged From = %v, want ErrProtocol", err)
	}
}

// TestQueueBackpressure proves the inbound queue is bounded: once it is full the
// sender blocks instead of buffering without limit.
func TestQueueBackpressure(t *testing.T) {
	const depth = 2

	h := memory.New(memory.WithQueueSize(depth))
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := range depth {
		if err := a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// The next send has nowhere to go until the reader drains one message.
	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelBlocked()

	err := a.Send(blockedCtx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send into a full queue = %v, want a deadline", err)
	}

	if _, err := b.Receive(ctx); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if err := a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)); err != nil {
		t.Fatalf("send after the queue drained: %v", err)
	}
}

func TestSendUnblocksOnLocalClose(t *testing.T) {
	h := memory.New(memory.WithQueueSize(1))
	a := open(t, h, "alice")
	open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		blocked <- a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil))
	}()

	time.Sleep(50 * time.Millisecond)
	_ = a.Close()

	select {
	case err := <-blocked:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("blocked send = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock a blocked Send")
	}
}

func TestSendUnblocksOnPeerDisconnect(t *testing.T) {
	h := memory.New(memory.WithQueueSize(1))
	a := open(t, h, "alice")
	open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		blocked <- a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil))
	}()

	time.Sleep(50 * time.Millisecond)
	h.Disconnect("bob")

	select {
	case err := <-blocked:
		if !errors.Is(err, pipe.ErrPeerUnavailable) {
			t.Errorf("blocked send = %v, want ErrPeerUnavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("disconnecting the peer did not unblock a blocked Send")
	}
}

func TestFaultDuplicateAll(t *testing.T) {
	h := memory.New(memory.WithFault(memory.DuplicateAll()))
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sig := signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)
	if err := a.Send(ctx, sig); err != nil {
		t.Fatal(err)
	}

	for i := range 2 {
		got, err := b.Receive(ctx)
		if err != nil {
			t.Fatalf("copy %d: %v", i, err)
		}
		if got.ID != sig.ID {
			t.Fatalf("copy %d has ID %q, want %q", i, got.ID, sig.ID)
		}
	}
}

func TestFaultDropKind(t *testing.T) {
	h := memory.New(memory.WithFault(memory.DropKind(pipe.KindCandidate)))
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	session := pipe.NewID()
	dropped := signalertest.Signal("alice", "bob", session, pipe.KindCandidate,
		[]byte(`{"candidate":"candidate:1 1 udp 1 203.0.113.1 9 typ host"}`))
	kept := signalertest.Signal("alice", "bob", session, pipe.KindICEComplete, nil)

	if err := a.Send(ctx, dropped); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, kept); err != nil {
		t.Fatal(err)
	}

	got, err := b.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != kept.ID {
		t.Fatalf("received %q (%s), want the ice-complete", got.ID, got.Kind)
	}
}

func TestFaultDropFirst(t *testing.T) {
	h := memory.New(memory.WithFault(memory.DropFirst(pipe.KindOffer, 2)))
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var sent []pipe.Signal
	for range 3 {
		sig := signalertest.Offer("alice", "bob", pipe.NewID(), "v=0\r\n")
		if err := a.Send(ctx, sig); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, sig)
	}

	got, err := b.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != sent[2].ID {
		t.Errorf("received the offer %q, want the third one", got.ID)
	}

	idle, cancelIdle := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelIdle()
	if _, err := b.Receive(idle); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a fourth signal arrived: %v", err)
	}
}

// TestFaultSwapAdjacent proves the reorder fault is deterministic, which is what
// lets the endpoint's out-of-order candidate handling be tested without timing
// luck.
func TestFaultSwapAdjacent(t *testing.T) {
	h := memory.New(memory.WithFault(memory.SwapAdjacent(pipe.KindCandidate)))
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	session := pipe.NewID()
	first := signalertest.Signal("alice", "bob", session, pipe.KindCandidate, []byte(`{"candidate":"a"}`))
	second := signalertest.Signal("alice", "bob", session, pipe.KindCandidate, []byte(`{"candidate":"b"}`))

	if err := a.Send(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := a.Send(ctx, second); err != nil {
		t.Fatal(err)
	}

	order := make([]string, 0, 2)
	for range 2 {
		got, err := b.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, got.ID)
	}
	if order[0] != second.ID || order[1] != first.ID {
		t.Errorf("delivery order = %v, want %v", order, []string{second.ID, first.ID})
	}
}

func TestConcurrentSendersAndDisconnects(t *testing.T) {
	h := memory.New(memory.WithQueueSize(4))
	a := open(t, h, "alice")
	b := open(t, h, "bob")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A drain loop keeps the bounded queues moving.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := b.Receive(ctx); err != nil {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				// Errors are expected once the connection closes; the point of
				// this test is that nothing races or panics.
				_ = a.Send(ctx, signalertest.Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil))
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.Registered()
		}()
	}
	wg.Wait()

	_ = b.Close()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain loop did not stop after Close")
	}
	_ = a.Close()
}

func open(t *testing.T, h *memory.Hub, id pipe.PeerID) pipe.SignalConn {
	t.Helper()

	conn, err := h.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open(%q): %v", id, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
