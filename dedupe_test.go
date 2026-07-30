package pipe

import (
	"fmt"
	"sync"
	"testing"
)

func TestDedupeCacheDetectsRepeats(t *testing.T) {
	c := newDedupeCache(16)

	if c.seenBefore("a") {
		t.Error("a fresh ID was reported as seen")
	}
	if !c.seenBefore("a") {
		t.Error("a repeated ID was not detected")
	}
	if c.seenBefore("b") {
		t.Error("a different ID was reported as seen")
	}
}

// TestDedupeCacheIsBounded proves the cache never grows past its limit, which is
// what keeps an at-least-once transport from exhausting memory.
func TestDedupeCacheIsBounded(t *testing.T) {
	const max = 64
	c := newDedupeCache(max)

	for i := range 10 * max {
		c.seenBefore(fmt.Sprintf("id-%d", i))
		if got := c.len(); got > max {
			t.Fatalf("cache holds %d IDs, above the %d limit", got, max)
		}
	}

	// The most recent IDs are still remembered.
	if !c.seenBefore(fmt.Sprintf("id-%d", 10*max-1)) {
		t.Error("the most recent ID was forgotten")
	}
}

func TestDedupeCacheMinimumSize(t *testing.T) {
	c := newDedupeCache(0)
	c.seenBefore("a")
	if !c.seenBefore("a") {
		t.Error("a cache created with size 0 remembers nothing")
	}
}

func TestDedupeCacheConcurrent(t *testing.T) {
	c := newDedupeCache(1024)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 500 {
				c.seenBefore(fmt.Sprintf("w%d-%d", w, i))
			}
		}(w)
	}
	wg.Wait()

	if got := c.len(); got > 1024 {
		t.Errorf("cache holds %d IDs, above the limit", got)
	}
}

func TestMailboxBoundedAndDrains(t *testing.T) {
	box := newMailbox()

	sig := signal(KindICEComplete, "")
	for range maxMailbox {
		if !box.post(sessionEvent{signal: &sig}) {
			t.Fatal("post rejected an event below the bound")
		}
	}
	if box.post(sessionEvent{signal: &sig}) {
		t.Fatal("post accepted an event above the bound")
	}

	events, overflow := box.drain()
	if len(events) != maxMailbox {
		t.Errorf("drained %d events, want %d", len(events), maxMailbox)
	}
	if !overflow {
		t.Error("the overflow was not reported")
	}

	events, overflow = box.drain()
	if len(events) != 0 || overflow {
		t.Error("draining twice returned stale state")
	}

	// A closed mailbox accepts nothing and never blocks a producer.
	box.close()
	if box.post(sessionEvent{signal: &sig}) {
		t.Error("a closed mailbox accepted an event")
	}
}

func TestMailboxSignalsWaiters(t *testing.T) {
	box := newMailbox()
	sig := signal(KindICEComplete, "")

	select {
	case <-box.wait():
		t.Fatal("an empty mailbox signalled a waiter")
	default:
	}

	box.post(sessionEvent{signal: &sig})
	select {
	case <-box.wait():
	default:
		t.Fatal("post did not signal the waiter")
	}
}
