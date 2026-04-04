package testsignal

import (
	"context"
	"errors"
	"sync"

	"ella.to/pipe/signal"
)

// Bus is a simple in-memory signaling bus for tests/examples.
// It implements signal.Signal by routing messages to in-memory inboxes.
// Not safe for unbounded use in production.

type Bus struct {
	mu    sync.Mutex
	inbox map[string]chan *signal.Msg
}

func New() *Bus {
	return &Bus{inbox: make(map[string]chan *signal.Msg)}
}

func (b *Bus) getInbox(inbox *signal.Inbox) chan *signal.Msg {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.inbox[inbox.String()]
	if !ok {
		ch = make(chan *signal.Msg, 32)
		b.inbox[inbox.String()] = ch
	}
	return ch
}

func (b *Bus) Send(ctx context.Context, inbox *signal.Inbox, msg *signal.Msg) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case b.getInbox(inbox) <- msg:
		return nil
	}
}

func (b *Bus) Receiver(inbox *signal.Inbox) (signal.Receiver, error) {
	ch := b.getInbox(inbox)
	return signal.ReceiverFunc(func(ctx context.Context) (*signal.Msg, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case m, ok := <-ch:
			if !ok {
				return nil, errors.New("inbox closed")
			}
			return m, nil
		}
	}), nil
}
