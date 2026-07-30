package clock

import (
	"testing"
	"time"
)

func TestSystemClock(t *testing.T) {
	c := System()

	start := c.Now()
	timer := c.NewTimer(10 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C():
	case <-time.After(time.Second):
		t.Fatal("the timer never fired")
	}
	if c.Since(start) <= 0 {
		t.Error("Since returned a non-positive duration")
	}
	if timer.Stop() {
		t.Error("Stop reported success for a timer that already fired")
	}
}

func TestFakeClockAdvance(t *testing.T) {
	c := NewFake(time.Time{})
	start := c.Now()

	timer := c.NewTimer(time.Minute)
	if c.Pending() != 1 {
		t.Fatalf("Pending = %d, want 1", c.Pending())
	}

	select {
	case <-timer.C():
		t.Fatal("the timer fired before the clock advanced")
	default:
	}

	c.Advance(30 * time.Second)
	select {
	case <-timer.C():
		t.Fatal("the timer fired early")
	default:
	}

	c.Advance(30 * time.Second)
	select {
	case fired := <-timer.C():
		if !fired.Equal(start.Add(time.Minute)) {
			t.Errorf("fired at %v, want %v", fired, start.Add(time.Minute))
		}
	default:
		t.Fatal("the timer did not fire after the clock passed its deadline")
	}

	if c.Pending() != 0 {
		t.Errorf("Pending = %d, want 0", c.Pending())
	}
	if c.Since(start) != time.Minute {
		t.Errorf("Since = %v, want a minute", c.Since(start))
	}
}

func TestFakeClockZeroDurationFiresImmediately(t *testing.T) {
	c := NewFake(time.Now())

	timer := c.NewTimer(0)
	select {
	case <-timer.C():
	default:
		t.Fatal("a zero-duration timer did not fire")
	}
	if c.Pending() != 0 {
		t.Error("a fired timer is still pending")
	}
}

func TestFakeClockStop(t *testing.T) {
	c := NewFake(time.Now())

	timer := c.NewTimer(time.Second)
	if !timer.Stop() {
		t.Error("Stop reported failure for an active timer")
	}
	if c.Pending() != 0 {
		t.Error("a stopped timer is still pending")
	}

	c.Advance(time.Hour)
	select {
	case <-timer.C():
		t.Fatal("a stopped timer fired")
	default:
	}
	if timer.Stop() {
		t.Error("Stop reported success twice")
	}
}

func TestFakeClockReset(t *testing.T) {
	c := NewFake(time.Now())

	timer := c.NewTimer(time.Second)
	c.Advance(2 * time.Second)
	<-timer.C()

	if timer.Reset(time.Second) {
		t.Error("Reset reported an active timer after it fired")
	}
	c.Advance(time.Second)
	select {
	case <-timer.C():
	default:
		t.Fatal("the reset timer did not fire")
	}
}

func TestFakeClockDefaultsToNonZeroStart(t *testing.T) {
	if NewFake(time.Time{}).Now().IsZero() {
		t.Error("a fake clock started at the zero time")
	}
}
