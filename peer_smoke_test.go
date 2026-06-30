package pipe_test

import (
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signal/sse"
)

// TestPeerSmoke_OverSSE exercises the whole stack end to end: the
// PeerSignalServer (HTTP/SSE) for signaling, two real Peers built on the SSE
// client, an actual WebRTC data channel underneath, bidirectional datagram
// exchange with sender attribution, and clean shutdown semantics.
func TestPeerSmoke_OverSSE(t *testing.T) {
	if testing.Short() {
		t.Skip("smoke test establishes a real WebRTC connection; skipped in -short")
	}

	// 1. Signaling server that enforces unique peer ids.
	srv, err := pipe.NewPeerSignalServer()
	if err != nil {
		t.Fatalf("NewPeerSignalServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 2. Each peer gets its own SSE client pointed at the server.
	aliceClient, err := sse.NewClient(ts.URL)
	if err != nil {
		t.Fatalf("alice client: %v", err)
	}
	bobClient, err := sse.NewClient(ts.URL)
	if err != nil {
		t.Fatalf("bob client: %v", err)
	}

	alice, err := pipe.NewPeer("alice", aliceClient)
	if err != nil {
		t.Fatalf("NewPeer alice: %v", err)
	}
	defer alice.Close()

	bob, err := pipe.NewPeer("bob", bobClient)
	if err != nil {
		t.Fatalf("NewPeer bob: %v", err)
	}
	defer bob.Close()

	// Both peers should claim their ids on the signaling server.
	waitFor(t, func() bool {
		return srv.IsRegistered("alice") && srv.IsRegistered("bob")
	}, 10*time.Second, "both peers should register on the signal server")

	// 3. alice -> bob over a freshly dialed WebRTC connection.
	if _, err := alice.WriteTo("bob", []byte("ping from alice")); err != nil {
		t.Fatalf("alice.WriteTo: %v", err)
	}
	r := mustRead(t, bob, 30*time.Second)
	if string(r.data) != "ping from alice" || r.from != "alice" {
		t.Fatalf("bob received (%q, from=%q), want (ping from alice, alice)", r.data, r.from)
	}

	// 4. bob -> alice reusing the same bidirectional data channel.
	if _, err := bob.WriteTo("alice", []byte("pong from bob")); err != nil {
		t.Fatalf("bob.WriteTo: %v", err)
	}
	r = mustRead(t, alice, 30*time.Second)
	if string(r.data) != "pong from bob" || r.from != "bob" {
		t.Fatalf("alice received (%q, from=%q), want (pong from bob, bob)", r.data, r.from)
	}

	// 5. Sessions are tracked on both sides.
	waitForPeers(t, alice, 1, 5*time.Second)
	waitForPeers(t, bob, 1, 5*time.Second)

	// 6. Closing alice releases its registration and closes the signal client.
	if err := alice.Close(); err != nil {
		t.Fatalf("alice.Close: %v", err)
	}
	waitFor(t, func() bool { return !srv.IsRegistered("alice") }, 10*time.Second, "alice should unregister after Close")

	// 7. Post-close operations report io.ErrClosedPipe.
	if _, err := alice.WriteTo("bob", []byte("nope")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("WriteTo after Close: got %v, want io.ErrClosedPipe", err)
	}
	if _, _, err := alice.ReadFrom(make([]byte, 16)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("ReadFrom after Close: got %v, want io.ErrClosedPipe", err)
	}
}
