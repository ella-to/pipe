package turn

import (
	"net"
	"sync"
	"time"

	"github.com/pion/turn/v5"
	"golang.org/x/time/rate"
)

// UserQuota describes the limits applied to a single user.
type UserQuota struct {
	// MaxAllocations is the maximum number of concurrent allocations
	// (roughly: active client sessions). 0 means unlimited. When the limit
	// is reached, further Allocate requests receive a 486 (Allocation Quota
	// Reached) error.
	MaxAllocations int

	// MaxBytesPerSecond caps the user's total relay bandwidth (upload +
	// download combined, across all of the user's allocations).
	// 0 means unlimited. Packets over budget are dropped.
	MaxBytesPerSecond int
}

// Quota configures per-user limits. Users are identified by the user id
// returned from authentication: the username for static users, the userID
// part of "<expiry>:<userID>" for dynamic credentials.
type Quota struct {
	// Default applies to every user without a PerUser entry.
	Default UserQuota

	// PerUser overrides Default for specific user ids. An entry fully
	// replaces Default for that user (zero fields mean unlimited).
	// The map must not be mutated after Start; use Lookup for quotas that
	// change at runtime.
	PerUser map[string]UserQuota

	// Lookup, when set, resolves a user's quota dynamically and takes
	// precedence over PerUser and Default when it returns true. It is
	// called concurrently from the request path and must be safe for
	// concurrent use.
	Lookup func(userID string) (UserQuota, bool)
}

func (q *Quota) forUser(userID string) UserQuota {
	if q == nil {
		return UserQuota{}
	}
	if q.Lookup != nil {
		if uq, ok := q.Lookup(userID); ok {
			return uq
		}
	}
	if uq, ok := q.PerUser[userID]; ok {
		return uq
	}
	return q.Default
}

func (q *Quota) hasBandwidthLimit() bool {
	if q == nil {
		return false
	}
	// With a dynamic Lookup we can't know limits up front; always wrap.
	if q.Lookup != nil {
		return true
	}
	if q.Default.MaxBytesPerSecond > 0 {
		return true
	}
	for _, uq := range q.PerUser {
		if uq.MaxBytesPerSecond > 0 {
			return true
		}
	}
	return false
}

// quotaTracker tracks live allocations and rate limiters per user id.
type quotaTracker struct {
	mu       sync.Mutex
	sessions map[string]int
	limiters map[string]*rate.Limiter
}

func newQuotaTracker() *quotaTracker {
	return &quotaTracker{
		sessions: map[string]int{},
		limiters: map[string]*rate.Limiter{},
	}
}

func (t *quotaTracker) count(userID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[userID]
}

func (t *quotaTracker) total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, c := range t.sessions {
		n += c
	}
	return n
}

func (t *quotaTracker) inc(userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sessions[userID]++
}

func (t *quotaTracker) dec(userID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sessions[userID] <= 1 {
		delete(t.sessions, userID)
		delete(t.limiters, userID)
		return
	}
	t.sessions[userID]--
}

// limiter returns the user's shared rate limiter, creating it on first use.
// The burst allows at least one full-sized datagram so small limits don't
// starve completely.
func (t *quotaTracker) limiter(userID string, bytesPerSecond int) *rate.Limiter {
	burst := max(bytesPerSecond, 1500)
	t.mu.Lock()
	defer t.mu.Unlock()
	if l, ok := t.limiters[userID]; ok {
		// Pick up quota changes (e.g. a tier upgrade via Quota.Lookup) on
		// the next allocation.
		if l.Limit() != rate.Limit(bytesPerSecond) {
			l.SetLimit(rate.Limit(bytesPerSecond))
			l.SetBurst(burst)
		}
		return l
	}
	l := rate.NewLimiter(rate.Limit(bytesPerSecond), burst)
	t.limiters[userID] = l
	return l
}

// bandwidthLimitedGenerator wraps a RelayAddressGenerator so that UDP relay
// traffic of users with a bandwidth quota flows through a rate limiter.
type bandwidthLimitedGenerator struct {
	turn.RelayAddressGenerator
	quota   *Quota
	tracker *quotaTracker
}

func (g *bandwidthLimitedGenerator) AllocatePacketConn(
	conf turn.AllocateListenerConfig,
) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.RelayAddressGenerator.AllocatePacketConn(conf)
	if err != nil {
		return nil, nil, err
	}
	bps := g.quota.forUser(conf.UserID).MaxBytesPerSecond
	if bps <= 0 {
		return conn, addr, nil
	}
	return &rateLimitedConn{
		PacketConn: conn,
		limiter:    g.tracker.limiter(conf.UserID, bps),
	}, addr, nil
}

// rateLimitedConn wraps a relay net.PacketConn with rate limiting. ReadFrom
// and WriteTo share the same limiter, so the cap applies to the combined
// traffic. Packets over budget are dropped, mimicking a congested link.
type rateLimitedConn struct {
	net.PacketConn
	limiter *rate.Limiter
}

func (c *rateLimitedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if c.limiter.AllowN(time.Now(), n) {
			return n, addr, nil
		}
		// over budget: drop and read the next packet
	}
}

func (c *rateLimitedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.limiter.AllowN(time.Now(), len(p)) {
		// over budget: silently drop
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}
