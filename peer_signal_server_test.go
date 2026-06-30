package pipe_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ella.to/pipe"
)

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s: %s", timeout, msg)
}

// openSubscription opens a long-lived SSE GET on the given URL and keeps the
// connection open (draining the body) until the returned cancel func is called.
// This mirrors what a Peer's Listener does, so the server registration stays
// active for the lifetime of the subscription.
func openSubscription(t *testing.T, url string) (cancel context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	go func() {
		close(started)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body) // blocks until ctx cancel closes the stream
	}()
	<-started
	return cancel
}

func TestPeerSignalServer_RejectsDuplicateRegistration(t *testing.T) {
	srv, err := pipe.NewPeerSignalServer()
	if err != nil {
		t.Fatalf("NewPeerSignalServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	mainInboxURL := ts.URL + "?inbox=alice.inbox"

	// First registration: a long-lived subscription on the main inbox.
	cancel1 := openSubscription(t, mainInboxURL)
	defer cancel1()

	waitFor(t, func() bool { return srv.IsRegistered("alice") }, 5*time.Second, "alice should be registered")

	// Second registration for the same id must be rejected with 409.
	resp2, err := http.DefaultClient.Get(mainInboxURL)
	if err != nil {
		t.Fatalf("duplicate GET: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate registration status = %d, want %d", resp2.StatusCode, http.StatusConflict)
	}

	// Releasing the first subscription must free the id for re-registration.
	cancel1()
	waitFor(t, func() bool { return !srv.IsRegistered("alice") }, 5*time.Second, "alice should be unregistered after disconnect")
}

func TestPeerSignalServer_DifferentIdsCoexist(t *testing.T) {
	srv, err := pipe.NewPeerSignalServer()
	if err != nil {
		t.Fatalf("NewPeerSignalServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	defer openSubscription(t, ts.URL+"?inbox=alice.inbox")()
	defer openSubscription(t, ts.URL+"?inbox=bob.inbox")()

	waitFor(t, func() bool {
		return srv.IsRegistered("alice") && srv.IsRegistered("bob")
	}, 5*time.Second, "both alice and bob should register")
}

// A non-main (random) inbox subscription is for dialing and must never be
// gated by the registry.
func TestPeerSignalServer_RandomInboxNotRegistered(t *testing.T) {
	srv, err := pipe.NewPeerSignalServer()
	if err != nil {
		t.Fatalf("NewPeerSignalServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	defer openSubscription(t, ts.URL+"?inbox=alice.abc123")()

	// "alice" must NOT be considered registered from a random-inbox subscription.
	time.Sleep(200 * time.Millisecond)
	if srv.IsRegistered("alice") {
		t.Fatal("random inbox subscription must not register the peer id")
	}
}
