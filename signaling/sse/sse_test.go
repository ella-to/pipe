package sse_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signaling/signalertest"
	"ella.to/pipe/signaling/sse"
)

const waitFor = 10 * time.Second

// harness is one signaling server behind an httptest listener.
type harness struct {
	srv *sse.Server
	ts  *httptest.Server
}

func start(t *testing.T, cfg sse.Config) *harness {
	t.Helper()
	if cfg.Authenticator == nil {
		cfg.Authenticator = sse.TrustPeerHeader()
	}
	srv, err := sse.NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(func() {
		_ = srv.Close()
		ts.Close()
	})
	return &harness{srv: srv, ts: ts}
}

func (h *harness) client() *sse.Client {
	return &sse.Client{
		URL:        h.ts.URL,
		HTTPClient: h.ts.Client(),
		Backoff:    pipe.Backoff{Initial: 20 * time.Millisecond, Maximum: 100 * time.Millisecond, Factor: 2},
	}
}

func TestConformance(t *testing.T) {
	signalertest.Run(t, signalertest.Config{
		NewSignaler: func(t *testing.T) pipe.Signaler {
			return start(t, sse.Config{}).client()
		},
		// A second stream for the same peer replaces the first rather than
		// being refused, so duplicate registration is not an error here.
		RejectsDuplicatePeers:  false,
		ReportsUnavailablePeer: true,
	})
}

func TestStaticTokens(t *testing.T) {
	h := start(t, sse.Config{
		Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
			"alice-secret": "alice",
			"bob-secret":   "bob",
		}),
	})

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	t.Run("wrong token", func(t *testing.T) {
		c := h.client()
		c.Token = "nope"
		_, err := c.Open(ctx, "alice")
		if !errors.Is(err, sse.ErrUnauthorized) {
			t.Fatalf("Open = %v, want ErrUnauthorized", err)
		}
	})

	t.Run("token for another peer", func(t *testing.T) {
		c := h.client()
		c.Token = "bob-secret"
		_, err := c.Open(ctx, "alice")
		if !errors.Is(err, sse.ErrUnauthorized) {
			t.Fatalf("Open = %v, want ErrUnauthorized", err)
		}
	})

	t.Run("right token", func(t *testing.T) {
		alice := h.client()
		alice.Token = "alice-secret"
		a, err := alice.Open(ctx, "alice")
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()

		bob := h.client()
		bob.Token = "bob-secret"
		b, err := bob.Open(ctx, "bob")
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()

		// A signal that lies about its sender is refused even with a valid
		// token.
		forged := signalertest.Offer("alice", "bob", pipe.NewID(), "v=0\r\n")
		if err := b.Send(ctx, forged); !errors.Is(err, sse.ErrUnauthorized) {
			t.Fatalf("forged Send = %v, want ErrUnauthorized", err)
		}

		want := signalertest.Offer("bob", "alice", pipe.NewID(), "v=0\r\n")
		if err := b.Send(ctx, want); err != nil {
			t.Fatal(err)
		}
		got, err := a.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want.ID {
			t.Fatalf("received %s, want %s", got.ID, want.ID)
		}
	})
}

// TestReconnectReplays drops every client connection mid-stream and checks
// that nothing sent around the drop is lost.
func TestReconnectReplays(t *testing.T) {
	h := start(t, sse.Config{KeepAlive: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	a, err := h.client().Open(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := h.client().Open(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	const total = 40
	sent := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		if i == total/2 {
			// Sever every connection, including alice's stream. Signals sent
			// before the server notices go to a dead socket and must be
			// replayed from Last-Event-ID.
			h.ts.CloseClientConnections()
		}
		sig := signalertest.Offer("bob", "alice", pipe.NewID(), "v=0\r\n")
		if err := b.Send(ctx, sig); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		sent[sig.ID] = true
	}

	got := make(map[string]bool, total)
	for len(got) < total {
		sig, err := a.Receive(ctx)
		if err != nil {
			t.Fatalf("received %d of %d: %v", len(got), total, err)
		}
		if !sent[sig.ID] {
			t.Fatalf("received unknown signal %s", sig.ID)
		}
		got[sig.ID] = true
	}
}

func TestOfflineGraceForgetsPeer(t *testing.T) {
	h := start(t, sse.Config{OfflineGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	a, err := h.client().Open(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.client().Open(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if got := h.srv.Peers(); len(got) != 2 {
		t.Fatalf("peers = %v, want alice and bob", got)
	}

	_ = a.Close()
	deadline := time.Now().Add(waitFor)
	for len(h.srv.Peers()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("peers = %v, alice was not forgotten", h.srv.Peers())
		}
		time.Sleep(10 * time.Millisecond)
	}

	err = b.Send(ctx, signalertest.Offer("bob", "alice", pipe.NewID(), "v=0\r\n"))
	if !errors.Is(err, pipe.ErrPeerUnavailable) {
		t.Fatalf("Send to a forgotten peer = %v, want ErrPeerUnavailable", err)
	}
}

func TestMaxPeers(t *testing.T) {
	h := start(t, sse.Config{MaxPeers: 1})
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	a, err := h.client().Open(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// The limit is a transient condition from the client's point of view, so
	// Open keeps retrying until the context ends rather than failing fast.
	short, cancelShort := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancelShort()
	if _, err := h.client().Open(short, "bob"); err == nil {
		t.Fatal("Open beyond MaxPeers succeeded")
	}
}

func TestWrongURLIsPermanent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not signaling")
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	c := &sse.Client{URL: ts.URL + "/nothing-here", HTTPClient: ts.Client()}
	_, err := c.Open(ctx, "alice")
	if !errors.Is(err, sse.ErrPermanent) {
		t.Fatalf("Open = %v, want ErrPermanent", err)
	}
}

// TestPipeEndToEnd runs two real endpoints through the HTTP signaler.
func TestPipeEndToEnd(t *testing.T) {
	h := start(t, sse.Config{
		Authenticator: sse.StaticTokens(map[string]pipe.PeerID{
			"alice-secret": "alice",
			"bob-secret":   "bob",
		}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	newEndpoint := func(id pipe.PeerID, token string) *pipe.Endpoint {
		c := h.client()
		c.Token = token
		ep, err := pipe.New(ctx, pipe.Config{ID: id, Signaler: c})
		if err != nil {
			t.Fatalf("new %s: %v", id, err)
		}
		t.Cleanup(func() { _ = ep.Close() })
		return ep
	}
	bob := newEndpoint("bob", "bob-secret")
	alice := newEndpoint("alice", "alice-secret")

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

	msg := []byte("over http signaling")
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
}
