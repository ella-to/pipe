package sse

import (
	"sync"
	"time"

	"ella.to/pipe/signal"
)

const (
	// idleEntryTTL is how long an inbox with no subscribers is kept around
	// before being swept. It must be long enough for a sender to post a
	// message before the receiver attaches.
	idleEntryTTL = 2 * time.Minute
	// sweepInterval bounds how often the sweep runs; it is amortized over
	// regular Get/Subscribe calls so no background goroutine is needed.
	sweepInterval = 30 * time.Second
)

type entry struct {
	ch       chan *signal.Msg
	subs     int
	lastSeen time.Time
}

type Mapper struct {
	mtx       sync.Mutex
	values    map[string]*entry
	lastSweep time.Time
}

func (m *Mapper) getEntryLocked(inbox string) *entry {
	m.maybeSweepLocked()

	e, ok := m.values[inbox]
	if !ok {
		e = &entry{ch: make(chan *signal.Msg, 16)}
		m.values[inbox] = e
	}
	e.lastSeen = time.Now()

	return e
}

func (m *Mapper) Get(inbox string) chan *signal.Msg {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	return m.getEntryLocked(inbox).ch
}

// Subscribe returns the channel for the inbox and marks it as having an
// active consumer. Each Subscribe must be paired with an Unsubscribe so the
// entry can be reclaimed once the consumer goes away.
func (m *Mapper) Subscribe(inbox string) chan *signal.Msg {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	e := m.getEntryLocked(inbox)
	e.subs++

	return e.ch
}

// Unsubscribe releases a subscription created by Subscribe. When the last
// subscriber goes away and no messages are buffered, the entry is removed
// immediately; otherwise it lingers until the idle sweep reclaims it. The
// channel is never closed here so concurrent senders stay safe.
func (m *Mapper) Unsubscribe(inbox string, ch chan *signal.Msg) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	e, ok := m.values[inbox]
	if !ok || e.ch != ch {
		return
	}

	e.subs--
	e.lastSeen = time.Now()
	if e.subs <= 0 && len(e.ch) == 0 {
		delete(m.values, inbox)
	}
}

func (m *Mapper) maybeSweepLocked() {
	now := time.Now()
	if now.Sub(m.lastSweep) < sweepInterval {
		return
	}
	m.lastSweep = now

	for inbox, e := range m.values {
		if e.subs <= 0 && now.Sub(e.lastSeen) > idleEntryTTL {
			delete(m.values, inbox)
		}
	}
}

// Len returns the number of tracked inboxes.
func (m *Mapper) Len() int {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	return len(m.values)
}

func (m *Mapper) Delete(inbox string) {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	e, ok := m.values[inbox]
	if !ok {
		return
	}

	close(e.ch)
	delete(m.values, inbox)
}

func NewMapper() *Mapper {
	return &Mapper{
		values: make(map[string]*entry),
	}
}
