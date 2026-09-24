package sse_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/signalertest"
	"ella.to/pipe/signaling/sse"
)

// streamCounter wraps a handler and tracks event-stream requests.
type streamCounter struct {
	next   http.Handler
	opened atomic.Int64 // streams ever opened
	live   atomic.Int64 // streams currently open
}

func (sc *streamCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		sc.opened.Add(1)
		sc.live.Add(1)
		defer sc.live.Add(-1)
	}
	sc.next.ServeHTTP(w, r)
}

// startCounted is start with a streamCounter in front of the server.
func startCounted(t *testing.T, cfg sse.Config) (*harness, *streamCounter) {
	t.Helper()
	if cfg.Authenticator == nil {
		cfg.Authenticator = sse.TrustPeerHeader()
	}
	srv, err := sse.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	sc := &streamCounter{next: srv}
	ts := httptest.NewServer(sc)
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})
	return &harness{srv: srv, ts: ts}, sc
}

func (h *harness) mux(t *testing.T) *sse.Mux {
	m := &sse.Mux{
		URL:        h.ts.URL,
		HTTPClient: h.ts.Client(),
		Backoff:    pipe.Backoff{Initial: 20 * time.Millisecond, Maximum: 100 * time.Millisecond, Factor: 2},
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func openOn(t *testing.T, ctx context.Context, s pipe.Signaler, id pipe.PeerID) pipe.SignalConn {
	t.Helper()
	c, err := s.Open(ctx, id)
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMuxConformance(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		NewSignaler: func(t *testing.T) pipe.Signaler {
			return start(t, sse.Config{}).mux(t)
		},
		RejectsDuplicatePeers:  true,
		ReportsUnavailablePeer: true,
	})
}

// TestMuxSharesOneStream opens many peers on one Mux and checks that they all
// ride a single event stream and receive only their own signals.
func TestMuxSharesOneStream(t *testing.T) {
	h, sc := startCounted(t, sse.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	mux := h.mux(t)
	const peers = 50
	conns := make([]pipe.SignalConn, peers)
	for i := range conns {
		conns[i] = openOn(t, ctx, mux, pipe.PeerID(fmt.Sprintf("peer-%d", i)))
	}
	if got := sc.opened.Load(); got != 1 {
		t.Fatalf("%d peers opened %d streams, want 1", peers, got)
	}

	// A plain Client talks to every multiplexed peer.
	sender := openOn(t, ctx, h.client(), "sender")
	for i := range conns {
		sig := signalertest.Offer("sender", pipe.PeerID(fmt.Sprintf("peer-%d", i)), pipe.NewID(), "v=0\r\n")
		if err := sender.Send(ctx, sig); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range conns {
		sig, err := c.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := pipe.PeerID(fmt.Sprintf("peer-%d", i)); sig.To != want {
			t.Fatalf("peer-%d received a signal for %s", i, sig.To)
		}
	}

	// And multiplexed peers reach the plain one.
	want := signalertest.Offer("peer-7", "sender", pipe.NewID(), "v=0\r\n")
	if err := conns[7].Send(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, err := sender.Receive(ctx); err != nil || got.ID != want.ID {
		t.Fatalf("sender received %v, %v; want %s", got.ID, err, want.ID)
	}
}

// TestMuxPerPeerCredentials checks that every peer joins with its own
// credential, and that one bad credential fails only its own Open.
func TestMuxPerPeerCredentials(t *testing.T) {
	h := start(t, sse.Config{
		Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
			"alice-secret": "alice",
			"bob-secret":   "bob",
		}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	tokens := map[pipe.PeerID]string{
		"alice":   "alice-secret",
		"bob":     "bob-secret",
		"mallory": "guess",
	}
	mux := h.mux(t)
	mux.Authorize = func(r *http.Request, local pipe.PeerID) {
		r.Header.Set("Authorization", "Bearer "+tokens[local])
	}

	// The first Open also authenticates the stream itself.
	if _, err := mux.Open(ctx, "mallory"); !errors.Is(err, sse.ErrUnauthorized) {
		t.Fatalf("Open mallory = %v, want ErrUnauthorized", err)
	}
	alice := openOn(t, ctx, mux, "alice")
	if _, err := mux.Open(ctx, "mallory"); !errors.Is(err, sse.ErrUnauthorized) {
		t.Fatalf("Open mallory on a live stream = %v, want ErrUnauthorized", err)
	}
	bob := openOn(t, ctx, mux, "bob")

	// A signal that lies about its sender is refused.
	forged := signalertest.Offer("alice", "bob", pipe.NewID(), "v=0\r\n")
	if err := bob.Send(ctx, forged); !errors.Is(err, sse.ErrUnauthorized) {
		t.Fatalf("forged Send = %v, want ErrUnauthorized", err)
	}

	want := signalertest.Offer("bob", "alice", pipe.NewID(), "v=0\r\n")
	if err := bob.Send(ctx, want); err != nil {
		t.Fatal(err)
	}
	if got, err := alice.Receive(ctx); err != nil || got.ID != want.ID {
		t.Fatalf("alice received %v, %v; want %s", got.ID, err, want.ID)
	}
}

// TestMuxReconnectReplays drops every connection mid-stream and checks that
// every multiplexed peer gets everything sent to it.
func TestMuxReconnectReplays(t *testing.T) {
	h, sc := startCounted(t, sse.Config{KeepAlive: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	mux := h.mux(t)
	a := openOn(t, ctx, mux, "alice")
	b := openOn(t, ctx, mux, "bob")
	sender := openOn(t, ctx, h.client(), "sender")

	const total = 40
	sent := map[pipe.PeerID]map[string]bool{"alice": {}, "bob": {}}
	for i := 0; i < total; i++ {
		if i == total/2 {
			h.ts.CloseClientConnections()
		}
		for to := range sent {
			sig := signalertest.Offer("sender", to, pipe.NewID(), "v=0\r\n")
			if err := sender.Send(ctx, sig); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
			sent[to][sig.ID] = true
		}
	}

	for to, c := range map[pipe.PeerID]pipe.SignalConn{"alice": a, "bob": b} {
		got := make(map[string]bool, total)
		for len(got) < total {
			sig, err := c.Receive(ctx)
			if err != nil {
				t.Fatalf("%s received %d of %d: %v", to, len(got), total, err)
			}
			if !sent[to][sig.ID] {
				t.Fatalf("%s received unknown signal %s", to, sig.ID)
			}
			got[sig.ID] = true
		}
	}
	if sc.opened.Load() < 2 {
		t.Fatal("the stream never reconnected")
	}
}

// TestMuxStreamFollowsPeers checks that the stream closes with the last peer,
// that a closed peer stops receiving, and that a later Open reconnects.
func TestMuxStreamFollowsPeers(t *testing.T) {
	h, sc := startCounted(t, sse.Config{OfflineGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	mux := h.mux(t)
	a, err := mux.Open(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := mux.Open(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}

	// Closing alice leaves the stream; the server forgets her after the
	// grace period while bob stays.
	_ = a.Close()
	waitUntil(t, "alice to be forgotten", func() bool {
		peers := h.srv.Peers()
		return len(peers) == 1 && peers[0] == "bob"
	})
	if sc.live.Load() != 1 {
		t.Fatalf("live streams = %d, want 1", sc.live.Load())
	}

	_ = b.Close()
	waitUntil(t, "the stream to close", func() bool { return sc.live.Load() == 0 })

	c := openOn(t, ctx, mux, "carol")
	d := openOn(t, ctx, mux, "dave")
	sig := signalertest.Offer("dave", "carol", pipe.NewID(), "v=0\r\n")
	if err := d.Send(ctx, sig); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Receive(ctx); err != nil || got.ID != sig.ID {
		t.Fatalf("carol received %v, %v; want %s", got.ID, err, sig.ID)
	}
	if got := sc.opened.Load(); got != 2 {
		t.Fatalf("streams opened = %d, want 2", got)
	}
}

// TestMuxPeerTakenOver checks that a peer claimed by another stream fails on
// its own without disturbing the rest of the Mux.
func TestMuxPeerTakenOver(t *testing.T) {
	h := start(t, sse.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	mux := h.mux(t)
	a := openOn(t, ctx, mux, "alice")
	b := openOn(t, ctx, mux, "bob")

	// Another process signals as alice.
	openOn(t, ctx, h.client(), "alice")

	if _, err := a.Receive(ctx); !errors.Is(err, sse.ErrPermanent) {
		t.Fatalf("taken-over Receive = %v, want ErrPermanent", err)
	}

	sender := openOn(t, ctx, h.client(), "sender")
	sig := signalertest.Offer("sender", "bob", pipe.NewID(), "v=0\r\n")
	if err := sender.Send(ctx, sig); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Receive(ctx); err != nil || got.ID != sig.ID {
		t.Fatalf("bob received %v, %v; want %s", got.ID, err, sig.ID)
	}

	// The ID is free on the Mux again once the failed connection is gone.
	if _, err := mux.Open(ctx, "alice"); err != nil {
		t.Fatalf("reopen alice: %v", err)
	}
}

func TestMuxClose(t *testing.T) {
	h, sc := startCounted(t, sse.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	mux := h.mux(t)
	a := openOn(t, ctx, mux, "alice")

	errc := make(chan error, 1)
	go func() {
		_, err := a.Receive(ctx)
		errc <- err
	}()
	if err := mux.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Receive after Mux.Close = %v, want net.ErrClosed", err)
	}
	if _, err := mux.Open(ctx, "bob"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Open after Mux.Close = %v, want net.ErrClosed", err)
	}
	waitUntil(t, "the stream to close", func() bool { return sc.live.Load() == 0 })
}

// TestMuxUnsupportedServer points a Mux at a server that ignores the
// multiplexing header, as one predating it would.
func TestMuxUnsupportedServer(t *testing.T) {
	srv, err := sse.NewServer(sse.Config{Authenticator: sse.TrustPeerHeader()})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(sse.MuxHeader)
		srv.ServeHTTP(w, r)
	}))
	defer ts.Close()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	mux := &sse.Mux{URL: ts.URL, HTTPClient: ts.Client()}
	defer mux.Close()
	if _, err := mux.Open(ctx, "alice"); !errors.Is(err, sse.ErrPermanent) {
		t.Fatalf("Open = %v, want ErrPermanent", err)
	}
}

// TestMuxPipeEndToEnd runs real endpoints that share one Mux.
func TestMuxPipeEndToEnd(t *testing.T) {
	h, sc := startCounted(t, sse.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mux := h.mux(t)
	newEndpoint := func(id pipe.PeerID) *pipe.Endpoint {
		ep, err := pipe.New(ctx, pipe.Config{ID: id, Signaler: mux})
		if err != nil {
			t.Fatalf("new %s: %v", id, err)
		}
		t.Cleanup(func() { _ = ep.Close() })
		return ep
	}
	bob := newEndpoint("bob")
	alice := newEndpoint("alice")

	ln, err := bob.Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	conn, err := alice.Dial(ctx, "bob")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := []byte("over one multiplexed stream")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Fatalf("echo = %q, want %q", got, msg)
	}
	if n := sc.opened.Load(); n != 1 {
		t.Fatalf("two endpoints opened %d streams, want 1", n)
	}
}
