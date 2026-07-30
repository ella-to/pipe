package pionx

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// collector is a bounded sink that records events for assertions and forwards
// candidates to the other peer, which is what a session loop does.
type collector struct {
	mu     sync.Mutex
	events []Event
	notify chan struct{}
}

func newCollector() *collector {
	return &collector{notify: make(chan struct{}, 1)}
}

func (c *collector) sink(ev Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()

	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *collector) drain() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

// waitFor collects events until pred is satisfied or the deadline passes. It
// reports errors instead of failing the test so that it is safe to call from a
// goroutine other than the test's own.
func (c *collector) waitFor(forward func(Event), pred func(Event) bool) (Event, error) {
	deadline := time.After(15 * time.Second)
	for {
		for _, ev := range c.drain() {
			if forward != nil {
				forward(ev)
			}
			if pred(ev) {
				return ev, nil
			}
		}
		select {
		case <-c.notify:
		case <-deadline:
			return Event{}, errors.New("timed out waiting for an event")
		}
	}
}

// mustWaitFor is waitFor for use on the test's own goroutine.
func (c *collector) mustWaitFor(t *testing.T, forward func(Event), pred func(Event) bool) Event {
	t.Helper()

	ev, err := c.waitFor(forward, pred)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func newTestFactory(t *testing.T) *Factory {
	t.Helper()

	f, err := NewFactory(Config{})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	return f
}

// negotiate connects an offerer and an answerer, forwarding trickled candidates
// in both directions exactly as a session would.
func negotiate(t *testing.T) (offerer, answerer *Peer, offCol, ansCol *collector) {
	t.Helper()

	f := newTestFactory(t)
	offCol, ansCol = newCollector(), newCollector()

	offerer, err := f.NewOfferer(offCol.sink)
	if err != nil {
		t.Fatalf("NewOfferer: %v", err)
	}
	answerer, err = f.NewAnswerer(ansCol.sink)
	if err != nil {
		offerer.Close()
		t.Fatalf("NewAnswerer: %v", err)
	}
	t.Cleanup(func() {
		_ = offerer.Close()
		_ = answerer.Close()
	})

	offer, err := offerer.CreateOffer(false)
	if err != nil {
		t.Fatalf("CreateOffer: %v", err)
	}
	if err := answerer.SetRemoteOffer(offer); err != nil {
		t.Fatalf("SetRemoteOffer: %v", err)
	}
	answer, err := answerer.CreateAnswer()
	if err != nil {
		t.Fatalf("CreateAnswer: %v", err)
	}
	if err := offerer.SetRemoteAnswer(answer); err != nil {
		t.Fatalf("SetRemoteAnswer: %v", err)
	}

	return offerer, answerer, offCol, ansCol
}

// forwarder returns a function that applies the peer's local candidates to dst.
func forwarder(t *testing.T, dst *Peer) func(Event) {
	t.Helper()

	return func(ev Event) {
		switch ev.Kind {
		case EventLocalCandidate:
			if err := dst.AddCandidate(ev.Candidate); err != nil {
				t.Logf("AddCandidate: %v", err)
			}
		case EventGatheringComplete:
			if err := dst.EndOfRemoteCandidates(); err != nil {
				t.Logf("EndOfRemoteCandidates: %v", err)
			}
		}
	}
}

func TestNegotiateAndDetach(t *testing.T) {
	offerer, answerer, offCol, ansCol := negotiate(t)

	var wg sync.WaitGroup
	wg.Add(2)

	var offCh, ansCh Channel
	var offErr, ansErr error

	go func() {
		defer wg.Done()
		if _, err := offCol.waitFor(forwarder(t, answerer), func(ev Event) bool {
			return ev.Kind == EventChannelOpen
		}); err != nil {
			offErr = err
			return
		}
		offCh, offErr = offerer.Detach()
	}()
	go func() {
		defer wg.Done()
		if _, err := ansCol.waitFor(forwarder(t, offerer), func(ev Event) bool {
			return ev.Kind == EventChannelOpen
		}); err != nil {
			ansErr = err
			return
		}
		ansCh, ansErr = answerer.Detach()
	}()
	wg.Wait()

	if offErr != nil {
		t.Fatalf("offerer Detach: %v", offErr)
	}
	if ansErr != nil {
		t.Fatalf("answerer Detach: %v", ansErr)
	}

	// The detached channels carry messages in both directions.
	if _, err := offCh.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 32)
	if err := ansCh.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := ansCh.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(buf[:n], []byte("ping")) {
		t.Errorf("got %q, want %q", buf[:n], "ping")
	}

	// Detaching twice must fail rather than hand out the channel again.
	if _, err := offerer.Detach(); err == nil {
		t.Error("Detach succeeded twice")
	}

	if got := offerer.MaxMessageSize(); got == 0 {
		t.Error("MaxMessageSize is unknown after SCTP came up")
	}
	if local, remote := offerer.SelectedCandidatePair(); local == "" || remote == "" {
		t.Errorf("selected pair = (%q, %q), want both to be reported", local, remote)
	}
}

func TestDetachBeforeOpen(t *testing.T) {
	f := newTestFactory(t)

	offerer, err := f.NewOfferer(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer offerer.Close()

	if _, err := offerer.Detach(); err == nil {
		t.Fatal("Detach succeeded before the channel opened")
	} else if !errors.Is(err, ErrNegotiation) {
		t.Errorf("error = %v, want ErrNegotiation", err)
	}

	answerer, err := f.NewAnswerer(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer answerer.Close()

	if _, err := answerer.Detach(); err == nil {
		t.Fatal("Detach succeeded with no channel at all")
	}
}

func TestDetachAfterClose(t *testing.T) {
	f := newTestFactory(t)

	p, err := f.NewOfferer(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := p.Detach(); !errors.Is(err, ErrClosed) {
		t.Errorf("error = %v, want ErrClosed", err)
	}
	// Close is idempotent.
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestOffererCreatesTheContractChannel(t *testing.T) {
	// The answerer validates the label, protocol, and reliability, so a
	// successful negotiation proves the offerer honored the contract.
	offerer, answerer, offCol, ansCol := negotiate(t)

	go func() {
		_, _ = offCol.waitFor(forwarder(t, answerer), func(ev Event) bool {
			return ev.Kind == EventChannelOpen
		})
	}()
	ev := ansCol.mustWaitFor(t, forwarder(t, offerer), func(ev Event) bool {
		return ev.Kind == EventChannelOpen || ev.Kind == EventError
	})
	if ev.Kind == EventError {
		t.Fatalf("the answerer rejected the channel: %v", ev.Err)
	}
	_ = offerer
}

func TestAnswererRejectsUnexpectedChannel(t *testing.T) {
	f := newTestFactory(t)

	// A peer built outside the adapter opens a channel with the wrong label.
	rogue, err := f.api.NewPeerConnection(f.base)
	if err != nil {
		t.Fatal(err)
	}
	defer rogue.Close()

	col := newCollector()
	answerer, err := f.NewAnswerer(col.sink)
	if err != nil {
		t.Fatal(err)
	}
	defer answerer.Close()

	if _, err := rogue.CreateDataChannel("not-pipe", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := rogue.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rogue.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if err := answerer.SetRemoteOffer(offer.SDP); err != nil {
		t.Fatal(err)
	}
	answer, err := answerer.CreateAnswer()
	if err != nil {
		t.Fatal(err)
	}
	if err := rogue.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer,
	}); err != nil {
		t.Fatal(err)
	}

	// Both sides are host-only in-process, so candidates flow through the
	// standard callbacks; forward them so connectivity completes.
	rogue.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		init := c.ToJSON()
		_ = answerer.AddCandidate(Candidate{
			Candidate:        init.Candidate,
			SDPMid:           init.SDPMid,
			SDPMLineIndex:    init.SDPMLineIndex,
			UsernameFragment: init.UsernameFragment,
		})
	})

	ev := col.mustWaitFor(t, func(ev Event) {
		if ev.Kind == EventLocalCandidate {
			_ = rogue.AddICECandidate(webrtc.ICECandidateInit{
				Candidate:        ev.Candidate.Candidate,
				SDPMid:           ev.Candidate.SDPMid,
				SDPMLineIndex:    ev.Candidate.SDPMLineIndex,
				UsernameFragment: ev.Candidate.UsernameFragment,
			})
		}
	}, func(ev Event) bool {
		return ev.Kind == EventError || ev.Kind == EventChannelOpen
	})

	if ev.Kind != EventError {
		t.Fatal("the answerer accepted a channel that violates the contract")
	}
	if !errors.Is(ev.Err, ErrChannelRejected) {
		t.Errorf("error = %v, want ErrChannelRejected", ev.Err)
	}
	if !strings.Contains(ev.Err.Error(), "label") {
		t.Errorf("error = %q, want it to name the label", ev.Err)
	}
}

func TestCandidateBeforeRemoteDescription(t *testing.T) {
	f := newTestFactory(t)

	p, err := f.NewAnswerer(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if p.HasRemoteDescription() {
		t.Fatal("HasRemoteDescription is true before any description was applied")
	}
	// Pion requires a remote description, which is exactly why sessions buffer
	// candidates that arrive early.
	if err := p.AddCandidate(Candidate{Candidate: "candidate:1 1 udp 1 10.0.0.1 1 typ host"}); err == nil {
		t.Error("AddCandidate succeeded without a remote description")
	}
}

func TestDuplicateCandidateTolerated(t *testing.T) {
	offerer, answerer, _, _ := negotiate(t)

	cand := Candidate{Candidate: "candidate:1 1 udp 2130706431 127.0.0.1 9999 typ host"}
	if err := answerer.AddCandidate(cand); err != nil {
		t.Fatalf("first AddCandidate: %v", err)
	}
	if err := answerer.AddCandidate(cand); err != nil {
		t.Errorf("duplicate AddCandidate: %v", err)
	}
	if err := answerer.EndOfRemoteCandidates(); err != nil {
		t.Errorf("EndOfRemoteCandidates: %v", err)
	}
	// A second end-of-candidates must also be tolerated.
	if err := answerer.EndOfRemoteCandidates(); err != nil {
		t.Errorf("duplicate EndOfRemoteCandidates: %v", err)
	}
	_ = offerer
}

func TestICERestartOfferPrimitive(t *testing.T) {
	offerer, answerer, offCol, ansCol := negotiate(t)

	go func() {
		_, _ = offCol.waitFor(forwarder(t, answerer), func(ev Event) bool {
			return ev.Kind == EventChannelOpen
		})
	}()
	ansCol.mustWaitFor(t, forwarder(t, offerer), func(ev Event) bool {
		return ev.Kind == EventChannelOpen
	})

	restart, err := offerer.CreateOffer(true)
	if err != nil {
		t.Fatalf("CreateOffer(restart): %v", err)
	}
	if restart == "" {
		t.Fatal("the restart offer is empty")
	}
	if err := answerer.SetRemoteOffer(restart); err != nil {
		t.Fatalf("apply restart offer: %v", err)
	}
	if _, err := answerer.CreateAnswer(); err != nil {
		t.Fatalf("restart answer: %v", err)
	}
}

func TestApplyMalformedDescriptions(t *testing.T) {
	f := newTestFactory(t)

	p, err := f.NewAnswerer(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	if err := p.SetRemoteOffer("not an sdp"); !errors.Is(err, ErrNegotiation) {
		t.Errorf("SetRemoteOffer error = %v, want ErrNegotiation", err)
	}
	if err := p.SetRemoteAnswer("not an sdp"); !errors.Is(err, ErrNegotiation) {
		t.Errorf("SetRemoteAnswer error = %v, want ErrNegotiation", err)
	}
	if _, err := p.CreateAnswer(); !errors.Is(err, ErrNegotiation) {
		t.Errorf("CreateAnswer without an offer = %v, want ErrNegotiation", err)
	}
}

func TestCloseDuringNegotiation(t *testing.T) {
	stages := []string{"after-create", "after-offer", "after-answer"}

	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			f := newTestFactory(t)

			offerer, err := f.NewOfferer(func(Event) {})
			if err != nil {
				t.Fatal(err)
			}
			answerer, err := f.NewAnswerer(func(Event) {})
			if err != nil {
				t.Fatal(err)
			}

			if stage != "after-create" {
				offer, err := offerer.CreateOffer(false)
				if err != nil {
					t.Fatal(err)
				}
				if err := answerer.SetRemoteOffer(offer); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "after-answer" {
				answer, err := answerer.CreateAnswer()
				if err != nil {
					t.Fatal(err)
				}
				if err := offerer.SetRemoteAnswer(answer); err != nil {
					t.Fatal(err)
				}
			}

			if err := offerer.Close(); err != nil {
				t.Errorf("close offerer: %v", err)
			}
			if err := answerer.Close(); err != nil {
				t.Errorf("close answerer: %v", err)
			}
		})
	}
}

// TestSinkAfterCloseDoesNotPanic proves callbacks that arrive during or after
// shutdown are safe, which is what lets a session drop its mailbox first.
func TestSinkAfterCloseDoesNotPanic(t *testing.T) {
	f := newTestFactory(t)

	var closed sync.WaitGroup
	closed.Add(1)

	p, err := f.NewOfferer(func(ev Event) {
		if ev.Kind == EventPeerClosed {
			closed.Done()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateOffer(false); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		closed.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no close event was reported")
	}
}

func TestFactoryRequiresSink(t *testing.T) {
	f := newTestFactory(t)

	if _, err := f.NewOfferer(nil); err == nil {
		t.Error("NewOfferer accepted a nil sink")
	}
	if _, err := f.NewAnswerer(nil); err == nil {
		t.Error("NewAnswerer accepted a nil sink")
	}
}

func TestFactoryConfiguration(t *testing.T) {
	var sawSettingEngine, sawConfiguration bool

	f, err := NewFactory(Config{
		ICEServers: []ICEServer{
			{URLs: []string{"turn:turn.example.net:3478"}, Username: "u", Credential: "p"},
		},
		RelayOnly:              true,
		ConfigureSettingEngine: func(*webrtc.SettingEngine) { sawSettingEngine = true },
		ConfigureConfiguration: func(*webrtc.Configuration) { sawConfiguration = true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawSettingEngine || !sawConfiguration {
		t.Error("the configuration hooks were not called")
	}
	if f.base.ICETransportPolicy != webrtc.ICETransportPolicyRelay {
		t.Errorf("policy = %v, want relay", f.base.ICETransportPolicy)
	}
	if len(f.base.ICEServers) != 1 || f.base.ICEServers[0].Username != "u" {
		t.Errorf("ICE servers = %+v", f.base.ICEServers)
	}
}

func TestEventKindString(t *testing.T) {
	kinds := map[EventKind]string{
		EventLocalCandidate:    "local-candidate",
		EventGatheringComplete: "gathering-complete",
		EventConnected:         "connected",
		EventDisconnected:      "disconnected",
		EventFailed:            "failed",
		EventPeerClosed:        "peer-closed",
		EventChannelOpen:       "channel-open",
		EventChannelClosed:     "channel-closed",
		EventError:             "error",
		EventKind(99):          "unknown",
	}
	for kind, want := range kinds {
		if got := kind.String(); got != want {
			t.Errorf("EventKind(%d) = %q, want %q", kind, got, want)
		}
	}
}

func TestMaxMessageSizeUnknownBeforeConnect(t *testing.T) {
	f := newTestFactory(t)

	p, err := f.NewOfferer(func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// SCTP does not exist yet, so nothing is reported rather than a wrong guess.
	if got := p.MaxMessageSize(); got != 0 {
		t.Errorf("MaxMessageSize = %d, want 0 before SCTP exists", got)
	}
	if local, remote := p.SelectedCandidatePair(); local != "" || remote != "" {
		t.Errorf("selected pair = (%q, %q), want empty", local, remote)
	}
}
