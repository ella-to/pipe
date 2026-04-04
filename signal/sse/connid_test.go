package sse_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signal/sse"
)

// TestServerProvidedConnectionID demonstrates how the server can generate
// connection IDs based on the HTTP request context (e.g., user permissions)
func TestServerProvidedConnectionID(t *testing.T) {
	// Create a server with a custom connection ID generator
	generatedIDs := make(map[string]bool)
	var mu sync.Mutex

	server, err := sse.NewServer(
		sse.WithConnIDGenerator(func(ctx context.Context, r *http.Request) (string, error) {
			// In a real scenario, you would extract user info from context
			// and generate an ID based on permissions, session, etc.
			connID := "server-generated-" + r.Header.Get("X-User-ID")

			mu.Lock()
			generatedIDs[connID] = true
			mu.Unlock()

			return connID, nil
		}),
	)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Wrap the server to inject custom headers based on the inbox
	// This simulates different users connecting to the same server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbox := r.URL.Query().Get("inbox")
		// Set user ID based on inbox to simulate different authenticated users
		if inbox != "" {
			// Extract the device/user from the inbox (format: "alice.inbox" or "alice.random")
			if len(inbox) > 0 {
				// Simple heuristic: if inbox starts with 'a', it's alice; if 'b', it's bob
				if inbox[0] == 'a' {
					r.Header.Set("X-User-ID", "alice-user-id")
				} else if inbox[0] == 'b' {
					r.Header.Set("X-User-ID", "bob-user-id")
				}
			}
		}
		server.ServeHTTP(w, r)
	})

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	// Both Alice and Bob use the same server
	clientAlice, err := sse.NewClient(ts.URL)
	if err != nil {
		t.Fatalf("new client alice: %v", err)
	}

	clientBob, err := sse.NewClient(ts.URL)
	if err != nil {
		t.Fatalf("new client bob: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Track the connection IDs
	var aliceConnID, bobConnID string
	var connMu sync.Mutex

	// Alice listens
	go func() {
		listener, err := pipe.CreateListener("alice", clientAlice)
		if err != nil {
			t.Errorf("create listener: %v", err)
			return
		}

		conn, err := listener.Listen(ctx)
		if err != nil {
			t.Errorf("listener listen: %v", err)
			return
		}
		defer conn.Close()

		if err := conn.WaitReady(ctx); err != nil {
			t.Errorf("listener wait ready: %v", err)
			return
		}

		connMu.Lock()
		aliceConnID = conn.ID()
		connMu.Unlock()
	}()

	// Give listener time to start
	time.Sleep(100 * time.Millisecond)

	// Bob dials Alice
	dialer, err := pipe.CreateDialer("bob", clientBob)
	if err != nil {
		t.Fatalf("create dialer: %v", err)
	}

	conn, err := dialer.Dial(ctx, "alice")
	if err != nil {
		t.Fatalf("dialer dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WaitReady(ctx); err != nil {
		t.Fatalf("dialer wait ready: %v", err)
	}

	connMu.Lock()
	bobConnID = conn.ID()
	connMu.Unlock()

	// Wait for listener to finish
	time.Sleep(500 * time.Millisecond)

	// Verify that both connections received the same server-generated ID
	mu.Lock()
	defer mu.Unlock()

	t.Logf("Alice connection ID: %s", aliceConnID)
	t.Logf("Bob connection ID: %s", bobConnID)
	t.Logf("Generated IDs: %v", generatedIDs)

	// The server generates ONE ID based on the initial exchange to the main inbox
	// Both Alice and Bob should receive the same connection ID
	expectedID := "server-generated-alice-user-id"

	if aliceConnID != expectedID {
		t.Errorf("Alice connection ID = %q, want %q", aliceConnID, expectedID)
	}

	if bobConnID != expectedID {
		t.Errorf("Bob connection ID = %q, want %q", bobConnID, expectedID)
	}

	// Verify that both connections have the same ID
	if aliceConnID != bobConnID {
		t.Errorf("Connection IDs don't match: Alice=%q, Bob=%q", aliceConnID, bobConnID)
	}

	// Verify that the server generated exactly one ID
	if len(generatedIDs) != 1 {
		t.Errorf("Expected 1 generated ID, got %d", len(generatedIDs))
	}
}
