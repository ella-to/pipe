package pipe

import "sync"

// dedupeCache remembers recently seen signal IDs so that at-least-once
// signaling transports cannot drive a state transition twice. It holds at most
// max IDs: once full, the older half is discarded, which bounds memory at the
// cost of forgetting the oldest IDs.
type dedupeCache struct {
	mu   sync.Mutex
	max  int
	seen map[string]uint64
	next uint64
}

func newDedupeCache(max int) *dedupeCache {
	if max < 2 {
		max = 2
	}
	return &dedupeCache{max: max, seen: make(map[string]uint64, max)}
}

// seenBefore records id and reports whether it was already present.
func (d *dedupeCache) seenBefore(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[id]; ok {
		return true
	}
	if len(d.seen) >= d.max {
		d.evictOldestHalfLocked()
	}
	d.seen[id] = d.next
	d.next++
	return false
}

// evictOldestHalfLocked drops the older half of the cache. Amortized cost is
// O(1) per insertion and no bookkeeping structure is needed beyond the counter.
func (d *dedupeCache) evictOldestHalfLocked() {
	cutoff := d.next - uint64(d.max/2)
	for id, seq := range d.seen {
		if seq < cutoff {
			delete(d.seen, id)
		}
	}
}

// len reports the number of remembered IDs. It exists for tests.
func (d *dedupeCache) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
