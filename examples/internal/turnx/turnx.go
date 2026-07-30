// Package turnx runs a self-contained STUN and TURN server for the examples,
// with a configurable throughput budget on the relayed path.
//
// One UDP listener serves both roles: it answers STUN Binding requests, and it
// answers TURN Allocate requests for the configured users. That is what a real
// deployment looks like, and it means an example needs one address rather than
// two.
//
// The throughput budget is what makes this useful beyond "it connects". Setting
// [Config.Rate] puts a token bucket on every relay socket the server allocates,
// so relayed traffic is limited the way a slow link limits it. Because the
// congestion is real — packets are delayed or dropped — SCTP inside the pipe
// reacts to it the way it would react to a real bottleneck.
package turnx

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/logging"
	"github.com/pion/turn/v5"
	"golang.org/x/time/rate"
)

// Defaults for [Config].
const (
	// DefaultRealm is the TURN realm used when none is configured. The realm is
	// part of the long-term credential digest, so both ends must agree on it.
	DefaultRealm = "pipe.example"

	// DefaultMaxDelay bounds how long a relayed packet may be held waiting for
	// throughput budget before it is dropped instead.
	DefaultMaxDelay = 20 * time.Millisecond

	// minBurst keeps a rate limit from blocking handshakes: a token bucket whose
	// burst is smaller than one datagram rejects every datagram.
	minBurst = 64 << 10
)

// Config configures a [Server].
type Config struct {
	// Listen is the UDP address to serve STUN and TURN on. Use a zero port to
	// let the kernel choose one, then read [Server.Addr].
	Listen string

	// Realm is the TURN realm. It defaults to [DefaultRealm].
	Realm string

	// Users maps TURN username to password. A server with no users answers STUN
	// but refuses every allocation.
	Users map[string]string

	// RelayIP is the address handed to clients as their relay address. It
	// defaults to the IP of the resolved listen address, which is what a local
	// example wants. A server behind NAT must set its public address here.
	RelayIP net.IP

	// Rate limits each allocated relay socket, in bytes per second per
	// direction. Zero means unlimited.
	Rate int64

	// Burst is the token bucket depth in bytes. It defaults to a tenth of a
	// second of Rate, and is never smaller than 64 KiB.
	Burst int64

	// MaxDelay bounds how long an inbound relayed packet may be held waiting for
	// budget. Packets that would wait longer are dropped, which is how a
	// congested link behaves. It defaults to [DefaultMaxDelay]; a zero-length
	// delay is expressed as a negative value.
	MaxDelay time.Duration

	// Logger receives lifecycle events. Credentials are never logged.
	Logger *slog.Logger
}

// Stats is a snapshot of relayed traffic.
type Stats struct {
	// Allocations counts relay sockets created since startup.
	Allocations int64

	// SentBytes and SentPackets count traffic from a client toward a peer.
	SentBytes, SentPackets int64

	// ReceivedBytes and ReceivedPackets count traffic from a peer toward a
	// client.
	ReceivedBytes, ReceivedPackets int64

	// DroppedBytes and DroppedPackets count traffic discarded because it
	// exceeded the throughput budget.
	DroppedBytes, DroppedPackets int64

	// DelayedPackets counts packets held to stay inside the budget.
	DelayedPackets int64
}

// String implements [fmt.Stringer].
func (s Stats) String() string {
	return fmt.Sprintf("allocations=%d sent=%s/%dpkt received=%s/%dpkt dropped=%s/%dpkt delayed=%dpkt",
		s.Allocations,
		FormatSize(s.SentBytes), s.SentPackets,
		FormatSize(s.ReceivedBytes), s.ReceivedPackets,
		FormatSize(s.DroppedBytes), s.DroppedPackets,
		s.DelayedPackets)
}

// Server is a running STUN and TURN server.
type Server struct {
	srv  *turn.Server
	conn net.PacketConn
	addr *net.UDPAddr
	gen  *shapedGenerator
	log  *slog.Logger
}

// Start binds the listener and starts serving. The caller owns the returned
// server and must Close it.
func Start(cfg Config) (*Server, error) {
	if cfg.Realm == "" {
		cfg.Realm = DefaultRealm
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	// Long-term credentials are stored as the RFC 5389 key digest, so the
	// password itself does not stay in memory after startup.
	keys := make(map[string][]byte, len(cfg.Users))
	for user, password := range cfg.Users {
		if user == "" {
			return nil, errors.New("turnx: a TURN username cannot be empty")
		}
		keys[user] = turn.GenerateAuthKey(user, cfg.Realm, password)
	}

	conn, err := net.ListenPacket("udp4", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("turnx: listen on %s: %w", cfg.Listen, err)
	}

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("turnx: listener reported a %T address", conn.LocalAddr())
	}

	relayIP := cfg.RelayIP
	if relayIP == nil {
		relayIP = addr.IP
	}
	if relayIP.IsUnspecified() {
		_ = conn.Close()
		return nil, errors.New("turnx: listening on a wildcard address requires an explicit RelayIP")
	}

	gen := &shapedGenerator{
		static: &turn.RelayAddressGeneratorStatic{
			RelayAddress: relayIP,
			// Relay sockets bind to the same interface the server listens on.
			Address: addr.IP.String(),
		},
		limits: limits{
			rate:     cfg.Rate,
			burst:    burstFor(cfg.Rate, cfg.Burst),
			maxDelay: maxDelayFor(cfg.MaxDelay),
		},
		log: log,
	}

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: cfg.Realm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			key, ok := keys[ra.Username]
			if !ok {
				log.Warn("turn: rejected an allocation",
					"user", ra.Username, "realm", ra.Realm, "from", addrString(ra.SrcAddr))
				return "", nil, false
			}
			return ra.Username, key, true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn:            conn,
			RelayAddressGenerator: gen,
		}},
		// Pion's own logging is noisy and can carry addresses; the example logs
		// what it needs through cfg.Logger instead.
		LoggerFactory: &logging.DefaultLoggerFactory{DefaultLogLevel: logging.LogLevelError, Writer: nil},
	})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("turnx: start the TURN server: %w", err)
	}

	s := &Server{srv: srv, conn: conn, addr: addr, gen: gen, log: log}
	log.Info("turn: serving",
		"stun", s.STUNURL(), "turn", s.TURNURL(), "realm", cfg.Realm,
		"users", len(keys), "rate", rateString(cfg.Rate))
	return s, nil
}

// Addr is the address the server is listening on.
func (s *Server) Addr() *net.UDPAddr { return s.addr }

// STUNURL is the URL to configure as a STUN server.
func (s *Server) STUNURL() string {
	return "stun:" + s.hostPort()
}

// TURNURL is the URL to configure as a TURN server.
func (s *Server) TURNURL() string {
	return "turn:" + s.hostPort() + "?transport=udp"
}

func (s *Server) hostPort() string {
	return net.JoinHostPort(s.addr.IP.String(), strconv.Itoa(s.addr.Port))
}

// Stats reports relayed traffic since startup.
func (s *Server) Stats() Stats { return s.gen.stats() }

// Close stops the server and releases the listener.
func (s *Server) Close() error {
	err := s.srv.Close()
	// turn.Server.Close closes the listener it was given.
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("turnx: close: %w", err)
	}
	return nil
}

// limits describes a relay socket's throughput budget.
type limits struct {
	rate     int64
	burst    int64
	maxDelay time.Duration
}

func (l limits) enabled() bool { return l.rate > 0 }

func (l limits) limiter() *rate.Limiter {
	return rate.NewLimiter(rate.Limit(l.rate), int(l.burst))
}

// shapedGenerator allocates relay sockets wrapped in a throughput budget.
type shapedGenerator struct {
	static *turn.RelayAddressGeneratorStatic
	limits limits
	log    *slog.Logger

	allocations atomic.Int64
	counters    counters
}

// Validate implements [turn.RelayAddressGenerator].
func (g *shapedGenerator) Validate() error { return g.static.Validate() }

// AllocatePacketConn implements [turn.RelayAddressGenerator].
func (g *shapedGenerator) AllocatePacketConn(conf turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.static.AllocatePacketConn(conf)
	if err != nil {
		return nil, nil, err
	}

	g.allocations.Add(1)
	g.log.Info("turn: allocated a relay",
		"user", conf.UserID, "relay", addrString(addr), "rate", rateString(g.limits.rate))

	if !g.limits.enabled() {
		return &countingConn{PacketConn: conn, counters: &g.counters}, addr, nil
	}
	return &shapedConn{
		PacketConn: conn,
		outbound:   g.limits.limiter(),
		inbound:    g.limits.limiter(),
		maxDelay:   g.limits.maxDelay,
		counters:   &g.counters,
	}, addr, nil
}

// AllocateListener implements [turn.RelayAddressGenerator]. TCP relaying is not
// shaped, because the examples use UDP.
func (g *shapedGenerator) AllocateListener(conf turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return g.static.AllocateListener(conf)
}

// AllocateConn implements [turn.RelayAddressGenerator].
func (g *shapedGenerator) AllocateConn(conf turn.AllocateConnConfig) (net.Conn, error) {
	return g.static.AllocateConn(conf)
}

func (g *shapedGenerator) stats() Stats {
	s := g.counters.snapshot()
	s.Allocations = g.allocations.Load()
	return s
}

// counters accumulates relayed traffic across every allocation.
type counters struct {
	sentBytes, sentPackets       atomic.Int64
	recvBytes, recvPackets       atomic.Int64
	droppedBytes, droppedPackets atomic.Int64
	delayedPackets               atomic.Int64
}

func (c *counters) snapshot() Stats {
	return Stats{
		SentBytes:       c.sentBytes.Load(),
		SentPackets:     c.sentPackets.Load(),
		ReceivedBytes:   c.recvBytes.Load(),
		ReceivedPackets: c.recvPackets.Load(),
		DroppedBytes:    c.droppedBytes.Load(),
		DroppedPackets:  c.droppedPackets.Load(),
		DelayedPackets:  c.delayedPackets.Load(),
	}
}

// countingConn measures a relay socket without limiting it.
type countingConn struct {
	net.PacketConn
	counters *counters
}

func (c *countingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if n > 0 {
		c.counters.recvBytes.Add(int64(n))
		c.counters.recvPackets.Add(1)
	}
	return n, addr, err
}

func (c *countingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	if n > 0 {
		c.counters.sentBytes.Add(int64(n))
		c.counters.sentPackets.Add(1)
	}
	return n, err
}

// shapedConn is a relay socket with a throughput budget in each direction.
//
// The two directions are treated differently on purpose:
//
//   - WriteTo carries client-to-peer traffic and runs on the goroutine that
//     serves every client of the listener, so it must never block. Traffic over
//     budget is dropped, which is policing.
//   - ReadFrom carries peer-to-client traffic and runs on this allocation's own
//     goroutine, so a packet may be held briefly to stay inside the budget
//     before being dropped, which is shaping.
type shapedConn struct {
	net.PacketConn
	outbound *rate.Limiter
	inbound  *rate.Limiter
	maxDelay time.Duration
	counters *counters
}

// ReadFrom returns the next packet that fits the budget, delaying or discarding
// the ones that do not.
func (c *shapedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil {
			return n, addr, err
		}
		if n == 0 {
			return n, addr, nil
		}

		res := c.inbound.ReserveN(time.Now(), n)
		delay := res.Delay()
		switch {
		case !res.OK() || delay > c.maxDelay:
			// Holding this packet would cost more latency than a congested link
			// would; a real bottleneck drops it instead.
			res.Cancel()
			c.counters.droppedBytes.Add(int64(n))
			c.counters.droppedPackets.Add(1)
			continue
		case delay > 0:
			c.counters.delayedPackets.Add(1)
			time.Sleep(delay)
		}

		c.counters.recvBytes.Add(int64(n))
		c.counters.recvPackets.Add(1)
		return n, addr, nil
	}
}

// WriteTo sends p if it fits the budget and reports success either way: a
// dropped datagram is indistinguishable from one lost in transit, which is
// exactly what is being simulated.
func (c *shapedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.outbound.AllowN(time.Now(), len(p)) {
		c.counters.droppedBytes.Add(int64(len(p)))
		c.counters.droppedPackets.Add(1)
		return len(p), nil
	}

	n, err := c.PacketConn.WriteTo(p, addr)
	if n > 0 {
		c.counters.sentBytes.Add(int64(n))
		c.counters.sentPackets.Add(1)
	}
	return n, err
}

func burstFor(rateBytes, burst int64) int64 {
	if burst > 0 {
		return max(burst, minBurst)
	}
	return max(rateBytes/10, minBurst)
}

func maxDelayFor(d time.Duration) time.Duration {
	switch {
	case d < 0:
		return 0
	case d == 0:
		return DefaultMaxDelay
	default:
		return d
	}
}

func rateString(rateBytes int64) string {
	if rateBytes <= 0 {
		return "unlimited"
	}
	return FormatSize(rateBytes) + "/s"
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}

// ParseUsers parses a "user=password,user2=password2" list.
func ParseUsers(spec string) (map[string]string, error) {
	users := make(map[string]string)
	for entry := range strings.SplitSeq(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		user, password, ok := strings.Cut(entry, "=")
		if !ok || user == "" || password == "" {
			return nil, fmt.Errorf("turnx: %q is not a user=password pair", entry)
		}
		users[user] = password
	}
	if len(users) == 0 {
		return nil, errors.New("turnx: no users were configured")
	}
	return users, nil
}

// ParseSize parses a byte count such as 512, 512KiB, 4MiB, or 1MB. Binary and
// decimal suffixes are both accepted and mean what they say.
func ParseSize(s string) (int64, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return 0, errors.New("turnx: an empty size")
	}

	multipliers := []struct {
		suffix string
		factor int64
	}{
		{"KiB", 1 << 10},
		{"MiB", 1 << 20},
		{"GiB", 1 << 30},
		{"KB", 1000},
		{"MB", 1000 * 1000},
		{"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10},
		{"M", 1 << 20},
		{"G", 1 << 30},
		{"B", 1},
	}

	factor := int64(1)
	for _, m := range multipliers {
		if len(text) > len(m.suffix) && strings.EqualFold(text[len(text)-len(m.suffix):], m.suffix) {
			factor = m.factor
			text = strings.TrimSpace(text[:len(text)-len(m.suffix)])
			break
		}
	}

	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("turnx: %q is not a size: %w", s, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("turnx: %q is negative", s)
	}
	return int64(value * float64(factor)), nil
}

// FormatSize renders a byte count with a binary suffix.
func FormatSize(n int64) string {
	const unit = 1 << 10

	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}

	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return strconv.FormatFloat(value, 'f', 1, 64) + suffix
		}
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + "PiB"
}
