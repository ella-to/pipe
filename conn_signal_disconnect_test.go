package pipe_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ella.to/pipe"
	"ella.to/pipe/signal"
)

type trackingSignal struct {
	mu            sync.Mutex
	inboxes       map[string]chan *signal.Msg
	nonMainClosed atomic.Int32
}

func newTrackingSignal() *trackingSignal {
	return &trackingSignal{
		inboxes: make(map[string]chan *signal.Msg),
	}
}

func (s *trackingSignal) getInbox(inbox *signal.Inbox) chan *signal.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch, ok := s.inboxes[inbox.String()]
	if !ok {
		ch = make(chan *signal.Msg, 32)
		s.inboxes[inbox.String()] = ch
	}

	return ch
}

func (s *trackingSignal) Send(ctx context.Context, inbox *signal.Inbox, msg *signal.Msg) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.getInbox(inbox) <- msg:
		return nil
	}
}

func (s *trackingSignal) Receiver(inbox *signal.Inbox) (signal.Receiver, error) {
	ch := s.getInbox(inbox)

	r := &trackingReceiver{
		ch:   ch,
		done: make(chan struct{}),
		onClose: func() {
			if !inbox.IsMain() {
				s.nonMainClosed.Add(1)
			}
		},
	}

	return r, nil
}

type trackingReceiver struct {
	ch        <-chan *signal.Msg
	done      chan struct{}
	closeOnce sync.Once
	onClose   func()
}

func (r *trackingReceiver) Receive(ctx context.Context) (*signal.Msg, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, context.Canceled
	case msg := <-r.ch:
		return msg, nil
	}
}

func (r *trackingReceiver) Close() error {
	r.closeOnce.Do(func() {
		close(r.done)
		if r.onClose != nil {
			r.onClose()
		}
	})
	return nil
}

var _ signal.ReceiverCloser = (*trackingReceiver)(nil)

func TestDisconnectSignalOnConnected(t *testing.T) {
	sig := newTrackingSignal()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opts := &pipe.WebRTCOptions{
		DisconnectSignalOnConnected: true,
	}

	listener, err := pipe.CreateListenerWithOptions("alice", sig, opts)
	if err != nil {
		t.Fatalf("create listener: %v", err)
	}

	listenerConnCh := make(chan *pipe.Conn, 1)
	listenerErrCh := make(chan error, 1)
	go func() {
		conn, listenErr := listener.Listen(ctx)
		if listenErr != nil {
			listenerErrCh <- listenErr
			return
		}

		if waitErr := conn.WaitReady(ctx); waitErr != nil {
			listenerErrCh <- waitErr
			return
		}

		listenerConnCh <- conn
		listenerErrCh <- nil
	}()

	time.Sleep(100 * time.Millisecond)

	dialer, err := pipe.CreateDialerWithOptions("bob", sig, opts)
	if err != nil {
		t.Fatalf("create dialer: %v", err)
	}

	dialConn, err := dialer.Dial(ctx, "alice")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer dialConn.Close()

	if err := dialConn.WaitReady(ctx); err != nil {
		t.Fatalf("dialer wait ready: %v", err)
	}

	var listenerConn *pipe.Conn
	select {
	case err := <-listenerErrCh:
		if err != nil {
			t.Fatalf("listener error: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for listener readiness: %v", ctx.Err())
	}

	select {
	case listenerConn = <-listenerConnCh:
		defer listenerConn.Close()
	case <-ctx.Done():
		t.Fatalf("timeout waiting for listener conn: %v", ctx.Err())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sig.nonMainClosed.Load() >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("expected at least 2 non-main signal receivers to be closed, got %d", sig.nonMainClosed.Load())
}
