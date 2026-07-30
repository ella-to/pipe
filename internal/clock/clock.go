// Package clock provides the time source used by pipe timers so that
// recovery, keepalive, and timeout behavior can be tested without sleeping.
package clock

import (
	"sync"
	"time"
)

// Timer is the subset of [time.Timer] that pipe relies on.
type Timer interface {
	// C returns the channel that receives the firing time.
	C() <-chan time.Time

	// Stop prevents the timer from firing and reports whether it had not yet
	// fired or been stopped.
	Stop() bool

	// Reset restarts the timer with a new duration. Callers must stop and
	// drain a timer before reusing it.
	Reset(d time.Duration) bool
}

// Clock produces the current time and timers.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// NewTimer returns a timer that fires once after d.
	NewTimer(d time.Duration) Timer

	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
}

// System returns the real clock.
func System() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time                  { return time.Now() }
func (systemClock) Since(t time.Time) time.Duration { return time.Since(t) }
func (systemClock) NewTimer(d time.Duration) Timer  { return &systemTimer{t: time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (s *systemTimer) C() <-chan time.Time        { return s.t.C }
func (s *systemTimer) Stop() bool                 { return s.t.Stop() }
func (s *systemTimer) Reset(d time.Duration) bool { return s.t.Reset(d) }

// Fake is a manually advanced clock for deterministic tests. It is safe for
// concurrent use.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// NewFake returns a fake clock positioned at start. A zero start is replaced by
// a fixed, non-zero instant so that elapsed-time arithmetic stays sensible.
func NewFake(start time.Time) *Fake {
	if start.IsZero() {
		start = time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return &Fake{now: start}
}

// Now returns the fake clock's current time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the fake elapsed time since t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// NewTimer returns a timer that fires when the fake clock passes d.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()

	t := &fakeTimer{
		clock:    f,
		deadline: f.now.Add(d),
		ch:       make(chan time.Time, 1),
		active:   true,
	}
	if d <= 0 {
		t.fireLocked()
	} else {
		f.timers = append(f.timers, t)
	}
	return t
}

// Advance moves the clock forward by d and fires every timer whose deadline has
// passed, in deadline order.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.now = f.now.Add(d)
	remaining := f.timers[:0]
	for _, t := range f.timers {
		if !t.deadline.After(f.now) {
			t.fireLocked()
			continue
		}
		remaining = append(remaining, t)
	}
	f.timers = remaining
}

// Pending reports how many timers are waiting to fire.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

type fakeTimer struct {
	clock    *Fake
	deadline time.Time
	ch       chan time.Time
	active   bool
}

// fireLocked delivers the firing time. The caller holds the clock's mutex.
func (t *fakeTimer) fireLocked() {
	if !t.active {
		return
	}
	t.active = false
	select {
	case t.ch <- t.clock.now:
	default:
	}
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	was := t.active
	t.active = false
	t.clock.removeLocked(t)
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	was := t.active
	t.clock.removeLocked(t)
	t.active = true
	t.deadline = t.clock.now.Add(d)
	if d <= 0 {
		t.fireLocked()
	} else {
		t.clock.timers = append(t.clock.timers, t)
	}
	return was
}

// removeLocked drops t from the pending list. The caller holds the mutex.
func (f *Fake) removeLocked(target *fakeTimer) {
	for i, t := range f.timers {
		if t == target {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			return
		}
	}
}
