package turn

import (
	"net"
	"testing"
	"time"

	pionturn "github.com/pion/turn/v5"
	"golang.org/x/time/rate"
)

func TestQuota_ForUser(t *testing.T) {
	q := &Quota{
		Default: UserQuota{MaxAllocations: 2, MaxBytesPerSecond: 1000},
		PerUser: map[string]UserQuota{
			"premium": {MaxAllocations: 10, MaxBytesPerSecond: 100000},
			"blocked": {}, // fully overrides Default: unlimited
		},
	}

	if got := q.forUser("anyone"); got != q.Default {
		t.Fatalf("expected Default quota, got %+v", got)
	}
	if got := q.forUser("premium"); got.MaxAllocations != 10 || got.MaxBytesPerSecond != 100000 {
		t.Fatalf("expected premium override, got %+v", got)
	}
	if got := q.forUser("blocked"); got.MaxAllocations != 0 || got.MaxBytesPerSecond != 0 {
		t.Fatalf("expected zero (unlimited) override, got %+v", got)
	}

	var nilQuota *Quota
	if got := nilQuota.forUser("anyone"); got != (UserQuota{}) {
		t.Fatalf("expected zero quota for nil Quota, got %+v", got)
	}
}

func TestQuota_ForUser_Lookup(t *testing.T) {
	q := &Quota{
		Default: UserQuota{MaxAllocations: 1},
		PerUser: map[string]UserQuota{"alice": {MaxAllocations: 2}},
		Lookup: func(userID string) (UserQuota, bool) {
			if userID == "alice" {
				return UserQuota{MaxAllocations: 5}, true
			}
			return UserQuota{}, false
		},
	}

	// Lookup takes precedence over PerUser.
	if got := q.forUser("alice"); got.MaxAllocations != 5 {
		t.Fatalf("expected Lookup to win, got %+v", got)
	}
	// Lookup miss falls back to Default.
	if got := q.forUser("bob"); got.MaxAllocations != 1 {
		t.Fatalf("expected Default fallback, got %+v", got)
	}
	// A quota with only Lookup must enable the bandwidth wrapper.
	if !(&Quota{Lookup: q.Lookup}).hasBandwidthLimit() {
		t.Fatal("expected Lookup quota to enable bandwidth limiting")
	}
}

func TestQuota_HasBandwidthLimit(t *testing.T) {
	var nilQuota *Quota
	if nilQuota.hasBandwidthLimit() {
		t.Fatal("nil quota should not have bandwidth limit")
	}
	if (&Quota{Default: UserQuota{MaxAllocations: 5}}).hasBandwidthLimit() {
		t.Fatal("allocation-only quota should not have bandwidth limit")
	}
	if !(&Quota{Default: UserQuota{MaxBytesPerSecond: 1}}).hasBandwidthLimit() {
		t.Fatal("expected default bandwidth limit to be detected")
	}
	if !(&Quota{PerUser: map[string]UserQuota{"u": {MaxBytesPerSecond: 1}}}).hasBandwidthLimit() {
		t.Fatal("expected per-user bandwidth limit to be detected")
	}
}

func TestQuotaTracker(t *testing.T) {
	tr := newQuotaTracker()

	tr.inc("alice")
	tr.inc("alice")
	tr.inc("bob")

	if tr.count("alice") != 2 || tr.count("bob") != 1 || tr.total() != 3 {
		t.Fatalf("unexpected counts: alice=%d bob=%d total=%d",
			tr.count("alice"), tr.count("bob"), tr.total())
	}

	l := tr.limiter("alice", 1000)
	if tr.limiter("alice", 1000) != l {
		t.Fatal("expected same limiter for same user")
	}

	tr.dec("alice")
	if tr.count("alice") != 1 {
		t.Fatalf("expected 1, got %d", tr.count("alice"))
	}
	// limiter survives while allocations remain
	if tr.limiter("alice", 1000) != l {
		t.Fatal("limiter should survive while allocations remain")
	}

	tr.dec("alice")
	if tr.count("alice") != 0 {
		t.Fatalf("expected 0, got %d", tr.count("alice"))
	}
	// limiter is dropped with the last allocation
	if tr.limiter("alice", 1000) == l {
		t.Fatal("expected fresh limiter after last allocation closed")
	}
}

type recordingPacketConn struct {
	net.PacketConn
	writes int
}

func (c *recordingPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.writes++
	return len(p), nil
}

func TestRateLimitedConn_DropsOverBudget(t *testing.T) {
	inner := &recordingPacketConn{}
	conn := &rateLimitedConn{
		PacketConn: inner,
		limiter:    rate.NewLimiter(rate.Limit(1000), 1500),
	}

	packet := make([]byte, 1500)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}

	// First packet fits in the burst.
	if n, err := conn.WriteTo(packet, addr); err != nil || n != len(packet) {
		t.Fatalf("WriteTo: n=%d err=%v", n, err)
	}
	// Second packet immediately after exceeds the budget: silently dropped.
	if n, err := conn.WriteTo(packet, addr); err != nil || n != len(packet) {
		t.Fatalf("WriteTo (dropped): n=%d err=%v", n, err)
	}

	if inner.writes != 1 {
		t.Fatalf("expected 1 packet to reach the wire, got %d", inner.writes)
	}
}

func newTestClient(t *testing.T, serverAddr, username, password, realm string) (*pionturn.Client, net.PacketConn) {
	t.Helper()

	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("client listen: %v", err)
	}

	client, err := pionturn.NewClient(&pionturn.ClientConfig{
		STUNServerAddr: serverAddr,
		TURNServerAddr: serverAddr,
		Conn:           conn,
		Username:       username,
		Password:       password,
		Realm:          realm,
	})
	if err != nil {
		_ = conn.Close()
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Listen(); err != nil {
		client.Close()
		_ = conn.Close()
		t.Fatalf("client Listen: %v", err)
	}
	t.Cleanup(func() {
		client.Close()
		_ = conn.Close()
	})
	return client, conn
}

func TestServer_AllocationQuota(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Realm:      "test.local",
		Users:      []User{{Username: "alice", Password: "secret"}},
		Quota:      &Quota{Default: UserQuota{MaxAllocations: 1}},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	c1, _ := newTestClient(t, s.cfg.ListenAddr, "alice", "secret", "test.local")
	relay1, err := c1.Allocate()
	if err != nil {
		t.Fatalf("first Allocate: %v", err)
	}
	defer relay1.Close()

	if got := s.ActiveAllocations("alice"); got != 1 {
		t.Fatalf("expected 1 active allocation, got %d", got)
	}
	if got := s.TotalAllocations(); got != 1 {
		t.Fatalf("expected 1 total allocation, got %d", got)
	}

	c2, _ := newTestClient(t, s.cfg.ListenAddr, "alice", "secret", "test.local")
	if _, err := c2.Allocate(); err == nil {
		t.Fatal("expected second Allocate to be rejected by quota")
	}
}

func TestServer_DynamicCredentials_E2E(t *testing.T) {
	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Realm:      "test.local",
		Dynamic:    &DynamicAuth{Secret: "test-secret", MaxTTL: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	username, credential, err := s.GenerateCredentials("alice", 30*time.Minute)
	if err != nil {
		t.Fatalf("GenerateCredentials: %v", err)
	}

	client, _ := newTestClient(t, s.cfg.ListenAddr, username, credential, "test.local")
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("Allocate with dynamic credentials: %v", err)
	}
	defer relay.Close()

	// Quota accounting is keyed on the userID part, not the raw username.
	if got := s.ActiveAllocations("alice"); got != 1 {
		t.Fatalf("expected 1 active allocation for 'alice', got %d", got)
	}
}

func TestServer_EventCallbacks(t *testing.T) {
	created := make(chan string, 1)

	s := &Server{}
	err := s.Start(Config{
		ListenAddr: "127.0.0.1:0",
		PublicIP:   "127.0.0.1",
		Realm:      "test.local",
		Users:      []User{{Username: "alice", Password: "secret"}},
		Events: EventHandler{
			OnAllocationCreated: func(_, _ net.Addr, _, userID, _ string, _ net.Addr, _ int) {
				select {
				case created <- userID:
				default:
				}
			},
		},
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Close()

	client, _ := newTestClient(t, s.cfg.ListenAddr, "alice", "secret", "test.local")
	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	defer relay.Close()

	select {
	case userID := <-created:
		if userID != "alice" {
			t.Fatalf("expected userID 'alice', got %q", userID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnAllocationCreated was not called")
	}
}
