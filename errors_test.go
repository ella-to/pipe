package pipe

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func TestErrorCategories(t *testing.T) {
	cause := errors.New("underlying failure")

	tests := []struct {
		name     string
		err      error
		category error
	}{
		{"timeout", errorf(ErrTimeout, "pipe: too slow"), ErrTimeout},
		{"signaling", wrapErr(ErrSignaling, cause, "pipe: send"), ErrSignaling},
		{"negotiation", wrapErr(ErrNegotiation, cause, "pipe: offer"), ErrNegotiation},
		{"ice", wrapErr(ErrICE, cause, "pipe: connectivity"), ErrICE},
		{"protocol", errorf(ErrProtocol, "pipe: bad frame"), ErrProtocol},
		{"config", errorf(ErrConfig, "pipe: bad config"), ErrConfig},
		{"disconnected", wrapErr(ErrDisconnected, cause, "pipe: lost"), ErrDisconnected},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, tc.category) {
				t.Errorf("%v does not match its category", tc.err)
			}
			for _, other := range tests {
				if other.category == tc.category {
					continue
				}
				if errors.Is(tc.err, other.category) {
					t.Errorf("%v also matched %v", tc.err, other.category)
				}
			}
		})
	}
}

func TestErrorPreservesCause(t *testing.T) {
	cause := errors.New("root cause")

	wrapped := wrapErr(ErrSignaling, cause, "pipe: send offer")
	if !errors.Is(wrapped, cause) {
		t.Error("wrapErr lost the cause")
	}
	if !errors.Is(wrapped, ErrSignaling) {
		t.Error("wrapErr lost the category")
	}
	if !strings.Contains(wrapped.Error(), "root cause") {
		t.Errorf("message = %q, want it to include the cause", wrapped)
	}

	formatted := errorf(ErrProtocol, "pipe: rejected: %w", cause)
	if !errors.Is(formatted, cause) {
		t.Error("errorf lost the cause")
	}
	if !errors.Is(formatted, ErrProtocol) {
		t.Error("errorf lost the category")
	}
}

func TestWrapErrOnNilCause(t *testing.T) {
	if err := wrapErr(ErrSignaling, nil, "pipe: nothing"); err != nil {
		t.Errorf("wrapErr(nil) = %v, want nil", err)
	}
}

func TestErrorMessages(t *testing.T) {
	if got := (&categorized{category: ErrTimeout}).Error(); got != ErrTimeout.Error() {
		t.Errorf("bare category message = %q", got)
	}
	cause := errors.New("boom")
	if got := (&categorized{category: ErrICE, cause: cause}).Error(); !strings.HasSuffix(got, "boom") {
		t.Errorf("message = %q, want it to end with the cause", got)
	}
	if got := (&categorized{category: ErrICE, msg: "only a message"}).Error(); got != "only a message" {
		t.Errorf("message = %q", got)
	}
}

func TestTimeoutErrorsSatisfyNetError(t *testing.T) {
	err := deadlineError()

	var ne net.Error
	if !errors.As(err, &ne) {
		t.Fatalf("%v does not satisfy net.Error", err)
	}
	if !ne.Timeout() {
		t.Error("Timeout() is false for a deadline error")
	}
	if ne.Temporary() {
		t.Error("Temporary() should always be false")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("a deadline error should match os.ErrDeadlineExceeded")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Error("a deadline error should match ErrTimeout")
	}
}

// TestTimeoutFromCause proves a wrapped transport timeout still reports Timeout.
func TestTimeoutFromCause(t *testing.T) {
	err := wrapErr(ErrDisconnected, &net.OpError{Op: "write", Err: os.ErrDeadlineExceeded}, "pipe: write")

	var ne net.Error
	if !errors.As(err, &ne) {
		t.Fatal("the error does not satisfy net.Error")
	}
	if !ne.Timeout() {
		t.Error("Timeout() is false for a wrapped deadline error")
	}
}

func TestClosedIsNetErrClosed(t *testing.T) {
	if !errors.Is(ErrClosed, net.ErrClosed) {
		t.Error("ErrClosed must be net.ErrClosed so that generic code keeps working")
	}
}

func TestOpError(t *testing.T) {
	local := Addr{Peer: "alice", Session: testSessionID}
	remote := Addr{Peer: "bob", Session: testSessionID}

	err := opError("read", local, remote, errorf(ErrProtocol, "pipe: bad frame"))

	var oe *net.OpError
	if !errors.As(err, &oe) {
		t.Fatalf("%v is not a *net.OpError", err)
	}
	if oe.Op != "read" || oe.Net != networkName {
		t.Errorf("op error = %+v", oe)
	}
	if !errors.Is(err, ErrProtocol) {
		t.Error("the category did not survive the op error")
	}
	if got := oe.Source.String(); got != local.String() {
		t.Errorf("source = %q, want %q", got, local)
	}

	if err := opError("read", local, remote, nil); err != nil {
		t.Errorf("opError(nil) = %v, want nil", err)
	}
}

func TestRejectedError(t *testing.T) {
	err := &RejectedError{Code: RejectBusy, Reason: "backlog is full", Peer: "bob"}

	if !errors.Is(err, ErrPeerRejected) {
		t.Error("a rejection should match ErrPeerRejected")
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a rejection should not match ErrTimeout")
	}
	msg := err.Error()
	for _, want := range []string{"bob", "busy", "backlog is full"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q is missing %q", msg, want)
		}
	}

	bare := &RejectedError{Code: RejectInternal, Peer: "bob"}
	if strings.Contains(bare.Error(), "()") {
		t.Errorf("message %q has an empty reason group", bare)
	}

	var target *RejectedError
	wrapped := fmt.Errorf("dial failed: %w", err)
	if !errors.As(wrapped, &target) || target.Code != RejectBusy {
		t.Error("errors.As could not recover the rejection")
	}
}

func TestAddr(t *testing.T) {
	if got := (Addr{Peer: "alice"}).Network(); got != "webrtc" {
		t.Errorf("Network = %q, want webrtc", got)
	}
	if got := (Addr{Peer: "alice"}).String(); got != "alice" {
		t.Errorf("String = %q, want alice", got)
	}
	if got := (Addr{Peer: "alice", Session: "s1"}).String(); got != "alice#s1" {
		t.Errorf("String = %q, want alice#s1", got)
	}

	var _ net.Addr = Addr{}
}
