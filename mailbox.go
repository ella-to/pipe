package pipe

import "sync"

// maxMailbox bounds the events queued for one session. Pion emits a few dozen
// candidates plus a handful of state changes per session, so reaching this bound
// means the session loop is stuck; the session then fails instead of growing
// without limit.
const maxMailbox = 512

// mailbox is the bounded event queue of a session. Producers are Pion
// callbacks, the endpoint receive loop, and timers; the single consumer is the
// session loop. Posting never blocks, so no callback can stall Pion.
type mailbox struct {
	mu       sync.Mutex
	items    []sessionEvent
	overflow bool
	closed   bool
	notify   chan struct{}
}

func newMailbox() *mailbox {
	return &mailbox{notify: make(chan struct{}, 1)}
}

// post enqueues ev and reports whether it was accepted. It never blocks.
func (m *mailbox) post(ev sessionEvent) bool {
	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		return false
	case len(m.items) >= maxMailbox:
		m.overflow = true
		m.mu.Unlock()
		m.signal()
		return false
	}
	m.items = append(m.items, ev)
	m.mu.Unlock()

	m.signal()
	return true
}

func (m *mailbox) signal() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// wait returns the channel that reports newly posted events.
func (m *mailbox) wait() <-chan struct{} { return m.notify }

// drain removes and returns every queued event, plus whether the queue
// overflowed since the last drain.
func (m *mailbox) drain() ([]sessionEvent, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	items, overflow := m.items, m.overflow
	m.items, m.overflow = nil, false
	return items, overflow
}

// close stops accepting events and discards anything queued.
func (m *mailbox) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.items = nil
}
