// Package signalertest is a black-box conformance suite for
// [pipe.Signaler] implementations.
//
// A transport author wires the suite into a test and gets the rules that the
// endpoint relies on checked for free:
//
//	func TestConformance(t *testing.T) {
//		signalertest.Run(t, signalertest.Config{
//			NewSignaler: func(t *testing.T) pipe.Signaler { return memory.New() },
//		})
//	}
//
// The suite only exercises behavior that every transport must provide. Anything
// a transport may reasonably decide for itself — whether a duplicate peer
// registration is refused, whether sending to an absent peer is reported — is
// opt-in through [Config].
package signalertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ella.to/pipe"
)

// waitFor bounds every blocking operation in the suite. It is deliberately
// generous: the suite asserts outcomes, never latency.
const waitFor = 10 * time.Second

// Config describes the transport under test.
type Config struct {
	// NewSignaler returns a signaler whose peers can reach one another. It is
	// called once per subtest, so a transport that needs a server can start one
	// per call and register its shutdown with t.Cleanup.
	NewSignaler func(t *testing.T) pipe.Signaler

	// RejectsDuplicatePeers declares that opening a connection for a peer ID
	// that is already live fails with [pipe.ErrDuplicatePeer]. Transports that
	// allow several connections per ID leave this false.
	RejectsDuplicatePeers bool

	// ReportsUnavailablePeer declares that sending to a peer with no live
	// connection fails with [pipe.ErrPeerUnavailable]. Transports that queue
	// for absent peers, or that only learn of the failure later, leave this
	// false.
	ReportsUnavailablePeer bool

	// MaxPayload caps the payload size the suite will round-trip. It defaults to
	// [pipe.MaxSDPSize], which every transport must carry.
	MaxPayload int
}

// Run executes the conformance suite against cfg.
func Run(t *testing.T, cfg Config) {
	t.Helper()

	if cfg.NewSignaler == nil {
		t.Fatal("signalertest: Config.NewSignaler is required")
	}
	if cfg.MaxPayload <= 0 {
		cfg.MaxPayload = pipe.MaxSDPSize
	}

	tests := []struct {
		name string
		fn   func(*testing.T, Config)
	}{
		{"RoundTrip", testRoundTrip},
		{"EnvelopeSurvivesTransport", testEnvelopeSurvives},
		{"LargePayload", testLargePayload},
		{"Bidirectional", testBidirectional},
		{"DeliversOnlyToRecipient", testIsolation},
		{"AtLeastOnceDeliversEverything", testNoLoss},
		{"ConcurrentSenders", testConcurrentSenders},
		{"ReceiveHonorsContext", testReceiveHonorsContext},
		{"OpenHonorsCancelledContext", testOpenHonorsCancelledContext},
		{"CloseUnblocksReceive", testCloseUnblocksReceive},
		{"CloseIsIdempotent", testCloseIsIdempotent},
		{"UseAfterCloseFails", testUseAfterCloseFails},
		{"DuplicatePeerRejected", testDuplicatePeer},
		{"UnavailablePeerReported", testUnavailablePeer},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, cfg) })
	}
}

func testRoundTrip(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")

	sent := Offer("alice", "bob", pipe.NewID(), "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\n")
	send(t, a, sent)

	got := receive(t, b)
	if got.ID != sent.ID || got.Kind != sent.Kind {
		t.Fatalf("received %+v, want %+v", got, sent)
	}
}

// testEnvelopeSurvives proves the transport moves envelopes without editing
// them. The endpoint validates every field it reads, so silent normalization by
// a transport would surface as an unexplained protocol error.
func testEnvelopeSurvives(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")

	for _, sent := range []pipe.Signal{
		Offer("alice", "bob", pipe.NewID(), "v=0\r\ns=-\r\n"),
		Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil),
		Signal("alice", "bob", pipe.NewID(), pipe.KindCandidate,
			json.RawMessage(`{"candidate":"candidate:1 1 udp 2130706431 203.0.113.1 8000 typ host","sdp_mid":"0","sdp_mline_index":0}`)),
	} {
		send(t, a, sent)

		got := receive(t, b)
		if got.Version != sent.Version {
			t.Errorf("version = %d, want %d", got.Version, sent.Version)
		}
		if got.ID != sent.ID {
			t.Errorf("id = %q, want %q", got.ID, sent.ID)
		}
		if got.SessionID != sent.SessionID {
			t.Errorf("session = %q, want %q", got.SessionID, sent.SessionID)
		}
		if got.Kind != sent.Kind {
			t.Errorf("kind = %q, want %q", got.Kind, sent.Kind)
		}
		if got.From != sent.From || got.To != sent.To {
			t.Errorf("routing = %q->%q, want %q->%q", got.From, got.To, sent.From, sent.To)
		}
		if !sameJSON(got.Payload, sent.Payload) {
			t.Errorf("payload = %s, want %s", got.Payload, sent.Payload)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("the delivered signal no longer validates: %v", err)
		}
	}
}

func testLargePayload(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")

	// An SDP at the protocol limit is the largest payload a real session can
	// produce, and a transport that silently truncates it must fail here.
	sdp := "v=0\r\n" + strings.Repeat("a=x\r\n", (cfg.MaxPayload-64)/5)
	sent := Offer("alice", "bob", pipe.NewID(), sdp)
	send(t, a, sent)

	got := receive(t, b)
	if !sameJSON(got.Payload, sent.Payload) {
		t.Fatalf("payload of %d bytes did not survive (received %d bytes)", len(sent.Payload), len(got.Payload))
	}
}

func testBidirectional(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")
	session := pipe.NewID()

	send(t, a, Offer("alice", "bob", session, "v=0\r\noffer\r\n"))
	if got := receive(t, b); got.Kind != pipe.KindOffer {
		t.Fatalf("bob received %q, want an offer", got.Kind)
	}

	send(t, b, Answer("bob", "alice", session, "v=0\r\nanswer\r\n"))
	if got := receive(t, a); got.Kind != pipe.KindAnswer {
		t.Fatalf("alice received %q, want an answer", got.Kind)
	}
}

// testIsolation proves a signal reaches its addressee and nobody else. Routing
// leaks would let one peer observe another's session.
func testIsolation(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")
	c := open(t, s, "carol")

	send(t, a, Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil))
	receive(t, b)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	sig, err := c.Receive(ctx)
	if err == nil {
		t.Fatalf("carol received a signal addressed to bob: %+v", sig)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("carol's Receive failed with %v, want a deadline", err)
	}
}

// testNoLoss holds the transport to at-least-once delivery: every signal sent
// arrives at least once. Duplicates and reordering are permitted, so the
// assertion is on the set of IDs, not the sequence.
func testNoLoss(t *testing.T, cfg Config) {
	const count = 200

	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")

	want := make(map[string]bool, count)
	ids := make([]string, count)
	for i := range ids {
		ids[i] = pipe.NewID()
		want[ids[i]] = true
	}

	// A reader runs concurrently because a bounded transport queue makes the
	// sender block until the peer drains it.
	done := make(chan map[string]int, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()

		seen := make(map[string]int, count)
		for len(seen) < count {
			sig, err := b.Receive(ctx)
			if err != nil {
				break
			}
			seen[sig.ID]++
		}
		done <- seen
	}()

	for _, id := range ids {
		send(t, a, pipe.Signal{
			Version:   pipe.ProtocolVersion,
			ID:        id,
			SessionID: pipe.NewID(),
			Kind:      pipe.KindICEComplete,
			From:      "alice",
			To:        "bob",
		})
	}

	seen := <-done
	for id := range want {
		if seen[id] == 0 {
			t.Fatalf("signal %s was never delivered (%d of %d arrived)", id, len(seen), count)
		}
	}
}

// testConcurrentSenders exercises the rule that Send is safe to call
// concurrently with itself and with Receive. Run the suite under -race for this
// to mean anything.
func testConcurrentSenders(t *testing.T, cfg Config) {
	const (
		senders = 8
		each    = 25
	)

	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")

	received := make(chan int, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()

		seen := make(map[string]bool, senders*each)
		for len(seen) < senders*each {
			sig, err := b.Receive(ctx)
			if err != nil {
				break
			}
			seen[sig.ID] = true
		}
		received <- len(seen)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, senders)
	for w := range senders {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				sig := Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)
				if err := a.Send(ctx, sig); err != nil {
					errs <- fmt.Errorf("sender %d message %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if got := <-received; got != senders*each {
		t.Errorf("received %d distinct signals, want %d", got, senders*each)
	}
}

func testReceiveHonorsContext(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := a.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive returned %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > waitFor {
		t.Fatalf("Receive took %v to notice cancellation", elapsed)
	}
}

func testOpenHonorsCancelledContext(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conn, err := s.Open(ctx, "alice")
	if err == nil {
		_ = conn.Close()
		t.Fatal("Open succeeded with a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Open failed with %v, want context.Canceled", err)
	}
}

func testCloseUnblocksReceive(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")

	blocked := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitFor)
		defer cancel()
		_, err := a.Receive(ctx)
		blocked <- err
	}()

	// Give Receive a moment to actually block before closing under it.
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("Receive returned a signal after Close")
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Close did not unblock Receive: %v", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Receive stayed blocked after Close")
	}
}

func testCloseIsIdempotent(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")

	if err := a.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Errorf("third Close: %v", err)
	}
}

func testUseAfterCloseFails(t *testing.T, cfg Config) {
	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")
	b := open(t, s, "bob")

	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	sig := Signal("alice", "bob", pipe.NewID(), pipe.KindICEComplete, nil)
	if err := a.Send(ctx, sig); err == nil {
		t.Error("Send succeeded on a closed connection")
	} else if !errors.Is(err, net.ErrClosed) {
		t.Errorf("Send failed with %v, want net.ErrClosed", err)
	}

	if _, err := a.Receive(ctx); err == nil {
		t.Error("Receive succeeded on a closed connection")
	} else if !errors.Is(err, net.ErrClosed) {
		t.Errorf("Receive failed with %v, want net.ErrClosed", err)
	}

	// The peer that stayed open is unaffected.
	if err := b.Send(ctx, Signal("bob", "alice", pipe.NewID(), pipe.KindICEComplete, nil)); err != nil &&
		!errors.Is(err, pipe.ErrPeerUnavailable) {
		t.Errorf("closing alice broke bob's connection: %v", err)
	}
}

func testDuplicatePeer(t *testing.T, cfg Config) {
	if !cfg.RejectsDuplicatePeers {
		t.Skip("the transport allows several connections per peer ID")
	}

	s := cfg.NewSignaler(t)
	first := open(t, s, "alice")

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	second, err := s.Open(ctx, "alice")
	if err == nil {
		_ = second.Close()
		t.Fatal("Open accepted a duplicate peer ID")
	}
	if !errors.Is(err, pipe.ErrDuplicatePeer) {
		t.Errorf("Open failed with %v, want ErrDuplicatePeer", err)
	}

	// Closing the first registration must free the ID again.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	again, err := s.Open(ctx, "alice")
	if err != nil {
		t.Fatalf("the peer ID stayed taken after Close: %v", err)
	}
	_ = again.Close()
}

func testUnavailablePeer(t *testing.T, cfg Config) {
	if !cfg.ReportsUnavailablePeer {
		t.Skip("the transport does not report unavailable peers at send time")
	}

	s := cfg.NewSignaler(t)
	a := open(t, s, "alice")

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	err := a.Send(ctx, Signal("alice", "nobody", pipe.NewID(), pipe.KindICEComplete, nil))
	if err == nil {
		t.Fatal("Send to an unregistered peer succeeded")
	}
	if !errors.Is(err, pipe.ErrPeerUnavailable) {
		t.Errorf("Send failed with %v, want ErrPeerUnavailable", err)
	}
}

// Signal builds a valid envelope for kind with the given payload. Transport
// tests use it so that a delivered signal can be re-validated on arrival.
func Signal(from, to pipe.PeerID, session string, kind pipe.SignalKind, payload json.RawMessage) pipe.Signal {
	return pipe.Signal{
		Version:   pipe.ProtocolVersion,
		ID:        pipe.NewID(),
		SessionID: session,
		Kind:      kind,
		From:      from,
		To:        to,
		Payload:   payload,
	}
}

// Offer builds an offer envelope carrying sdp.
func Offer(from, to pipe.PeerID, session, sdp string) pipe.Signal {
	return Signal(from, to, session, pipe.KindOffer, sdpPayload(sdp))
}

// Answer builds an answer envelope carrying sdp.
func Answer(from, to pipe.PeerID, session, sdp string) pipe.Signal {
	return Signal(from, to, session, pipe.KindAnswer, sdpPayload(sdp))
}

func sdpPayload(sdp string) json.RawMessage {
	body, err := json.Marshal(struct {
		SDP string `json:"sdp"`
	}{SDP: sdp})
	if err != nil {
		// A string always marshals; this cannot happen.
		panic("signalertest: " + err.Error())
	}
	return body
}

func open(t *testing.T, s pipe.Signaler, id pipe.PeerID) pipe.SignalConn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	conn, err := s.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open(%q): %v", id, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, c pipe.SignalConn, sig pipe.Signal) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	if err := c.Send(ctx, sig); err != nil {
		t.Fatalf("Send(%s from %q to %q): %v", sig.Kind, sig.From, sig.To, err)
	}
}

func receive(t *testing.T, c pipe.SignalConn) pipe.Signal {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	sig, err := c.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	return sig
}

// sameJSON compares payloads semantically so that a transport is free to
// re-encode an envelope.
func sameJSON(got, want json.RawMessage) bool {
	if len(got) == 0 || len(want) == 0 {
		return len(got) == 0 && len(want) == 0
	}

	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		return false
	}
	if err := json.Unmarshal(want, &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
