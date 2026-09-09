// Package turnx runs a self-contained STUN and TURN server for the examples,
// with per-user throughput plans on the relayed path.
//
// One UDP listener serves both roles: it answers STUN Binding requests, and it
// answers TURN Allocate requests for authenticated users. That is what a real
// deployment looks like, and it means an example needs one address rather than
// two. An optional TCP listener serves clients whose networks block UDP.
//
// Users are authenticated either from a static table ([Config.Users]) or with
// ephemeral credentials derived from a shared secret ([Config.AuthSecret]),
// the mechanism coturn calls use-auth-secret. Every authenticated user maps to
// a [Plan] that says how fast its relay sockets may go and how many it may
// hold at once. That is how a service gives free users a 512 KiB/s relay and
// paying users a faster one from the same server.
//
// The throughput budget is real congestion, not a reported number: packets are
// delayed or dropped, so SCTP inside a pipe connection backs off the way it
// would on a slow link.
package turnx

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
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

	// DefaultCredentialTTL is how long ephemeral credentials issued by
	// [Server.IssueCredentials] stay valid.
	DefaultCredentialTTL = 24 * time.Hour

	// minBurst keeps a rate limit from blocking handshakes: a token bucket whose
	// burst is smaller than one datagram rejects every datagram.
	minBurst = 64 << 10
)

// Plan is the relay budget granted to a class of users.
type Plan struct {
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

	// MaxAllocations bounds the relay sockets one user may hold at a time. A
	// pipe connection uses one allocation per side. Zero means unlimited.
	MaxAllocations int
}

// String renders the plan for logs and banners.
func (p Plan) String() string {
	s := rateString(p.Rate)
	if p.MaxAllocations > 0 {
		s += fmt.Sprintf(", %d allocations", p.MaxAllocations)
	}
	return s
}

// User is one entry in the static credential table.
type User struct {
	// Password is the long-term credential. It is digested at startup and
	// never kept in memory afterwards.
	Password string

	// Plan names an entry in [Config.Plans]. Empty selects
	// [Config.DefaultPlan].
	Plan string
}

// Config configures a [Server].
type Config struct {
	// Listen is the UDP address to serve STUN and TURN on. Use a zero port to
	// let the kernel choose one, then read [Server.Addr].
	Listen string

	// ListenTCP, when set, is a TCP address on which TURN is also served for
	// clients behind UDP-hostile firewalls. STUN over TCP is answered too.
	ListenTCP string

	// Realm is the TURN realm. It defaults to [DefaultRealm].
	Realm string

	// Users maps TURN username to credential and plan for static
	// authentication.
	Users map[string]User

	// AuthSecret, when set, enables ephemeral credentials in the TURN REST API
	// format: the username is "<unix expiry>:<user id>" and the password is
	// the base64 HMAC-SHA1 of the username under the secret. Issue them with
	// [Server.IssueCredentials] or [IssueCredentials]. Static Users still work
	// alongside.
	AuthSecret string

	// Plans are the named budgets users may be assigned to.
	Plans map[string]Plan

	// DefaultPlan applies to users without a plan. Its zero value means an
	// unlimited relay.
	DefaultPlan Plan

	// PlanFor, when set, chooses the plan name for an authenticated user ID.
	// The default looks the ID up in Users, then treats a "name@plan" ID as
	// selecting plan, and otherwise uses DefaultPlan. Ephemeral credentials
	// carry the plan in the user ID that way: "alice@paid".
	PlanFor func(userID string) string

	// RelayIP is the address handed to clients as their relay address. It
	// defaults to the IP of the resolved listen address, which is what a local
	// example wants. A server behind NAT must set its public address here.
	RelayIP net.IP

	// MinPort and MaxPort, when both set, restrict relay sockets to that
	// inclusive range, which is what a firewall or a Docker port mapping needs.
	// When zero, the kernel picks ephemeral ports.
	MinPort, MaxPort uint16

	// Logger receives lifecycle events. Credentials are never logged.
	Logger *slog.Logger
}

// Stats is a snapshot of relayed traffic.
type Stats struct {
	// Allocations counts relay sockets created since startup.
	Allocations int64

	// Active counts relay sockets open right now.
	Active int64

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

	// Users breaks the counters down by authenticated user ID.
	Users map[string]UserStats
}

// UserStats is the traffic of one user.
type UserStats struct {
	// Plan is the plan name the user resolved to.
	Plan string

	// Allocations counts relay sockets created for the user; Active counts
	// the ones open right now.
	Allocations, Active int64

	// SentBytes and ReceivedBytes count relayed traffic in each direction.
	SentBytes, ReceivedBytes int64

	// DroppedBytes and DroppedPackets count traffic over the plan's budget.
	DroppedBytes, DroppedPackets int64

	// RejectedAllocations counts Allocate requests refused by the plan's
	// MaxAllocations quota.
	RejectedAllocations int64
}

// String renders one line of per-user statistics.
func (u UserStats) String() string {
	return fmt.Sprintf("plan=%s allocations=%d active=%d sent=%s received=%s dropped=%s/%dpkt rejected=%d",
		u.Plan, u.Allocations, u.Active,
		FormatSize(u.SentBytes), FormatSize(u.ReceivedBytes),
		FormatSize(u.DroppedBytes), u.DroppedPackets, u.RejectedAllocations)
}

// String implements [fmt.Stringer].
func (s Stats) String() string {
	return fmt.Sprintf("allocations=%d active=%d sent=%s/%dpkt received=%s/%dpkt dropped=%s/%dpkt delayed=%dpkt",
		s.Allocations, s.Active,
		FormatSize(s.SentBytes), s.SentPackets,
		FormatSize(s.ReceivedBytes), s.ReceivedPackets,
		FormatSize(s.DroppedBytes), s.DroppedPackets,
		s.DelayedPackets)
}

// Server is a running STUN and TURN server.
type Server struct {
	cfg  Config
	srv  *turn.Server
	addr *net.UDPAddr
	tcp  net.Addr
	gen  *shapedGenerator
	log  *slog.Logger
}

// Start binds the listeners and starts serving. The caller owns the returned
// server and must Close it.
func Start(cfg Config) (*Server, error) {
	if cfg.Realm == "" {
		cfg.Realm = DefaultRealm
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if (cfg.MinPort == 0) != (cfg.MaxPort == 0) || cfg.MinPort > cfg.MaxPort {
		return nil, errors.New("turnx: MinPort and MaxPort must both be set, with MinPort <= MaxPort")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	for name := range cfg.Plans {
		if name == "" {
			return nil, errors.New("turnx: a plan name cannot be empty")
		}
	}
	for user, u := range cfg.Users {
		if user == "" {
			return nil, errors.New("turnx: a TURN username cannot be empty")
		}
		if u.Plan != "" {
			if _, ok := cfg.Plans[u.Plan]; !ok {
				return nil, fmt.Errorf("turnx: user %q refers to unknown plan %q", user, u.Plan)
			}
		}
	}
	if len(cfg.Users) == 0 && cfg.AuthSecret == "" {
		log.Warn("turn: no users and no auth secret configured; every allocation will be refused")
	}

	// Long-term credentials are stored as the RFC 5389 key digest, so the
	// password itself does not stay in memory after startup.
	keys := make(map[string][]byte, len(cfg.Users))
	for user, u := range cfg.Users {
		keys[user] = turn.GenerateAuthKey(user, cfg.Realm, u.Password)
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

	// Relay sockets bind to the same interface the server listens on, except
	// that a wildcard listener relays on every interface.
	bindIP := addr.IP.String()
	if addr.IP.IsUnspecified() {
		bindIP = "0.0.0.0"
	}
	var inner turn.RelayAddressGenerator
	if cfg.MinPort != 0 {
		inner = &turn.RelayAddressGeneratorPortRange{
			RelayAddress: relayIP,
			MinPort:      cfg.MinPort,
			MaxPort:      cfg.MaxPort,
			Address:      bindIP,
		}
	} else {
		inner = &turn.RelayAddressGeneratorStatic{RelayAddress: relayIP, Address: bindIP}
	}

	gen := &shapedGenerator{
		inner: inner,
		cfg:   &cfg,
		log:   log,
		users: make(map[string]*userCounters),
	}

	var tcpListener net.Listener
	var listenerConfigs []turn.ListenerConfig
	if cfg.ListenTCP != "" {
		tcpListener, err = net.Listen("tcp4", cfg.ListenTCP)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("turnx: listen on tcp %s: %w", cfg.ListenTCP, err)
		}
		listenerConfigs = []turn.ListenerConfig{{Listener: tcpListener, RelayAddressGenerator: gen}}
	}

	secretHandler := turn.LongTermTURNRESTAuthHandler(cfg.AuthSecret,
		logging.NewDefaultLoggerFactory().NewLogger("turn-auth"))

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: cfg.Realm,
		AuthHandler: func(ra *turn.RequestAttributes) (string, []byte, bool) {
			if key, ok := keys[ra.Username]; ok {
				return ra.Username, key, true
			}
			if cfg.AuthSecret != "" {
				if id, key, ok := secretHandler(ra); ok {
					return id, key, true
				}
			}
			log.Warn("turn: rejected an allocation",
				"user", ra.Username, "realm", ra.Realm, "from", addrString(ra.SrcAddr))
			return "", nil, false
		},
		QuotaHandler:      gen.quota,
		EventHandler:      gen.events(),
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: conn, RelayAddressGenerator: gen}},
		ListenerConfigs:   listenerConfigs,
		// Pion's own logging is noisy and can carry addresses; the example logs
		// what it needs through cfg.Logger instead.
		LoggerFactory: &logging.DefaultLoggerFactory{DefaultLogLevel: logging.LogLevelError, Writer: nil},
	})
	if err != nil {
		_ = conn.Close()
		if tcpListener != nil {
			_ = tcpListener.Close()
		}
		return nil, fmt.Errorf("turnx: start the TURN server: %w", err)
	}

	s := &Server{cfg: cfg, srv: srv, addr: addr, gen: gen, log: log}
	if tcpListener != nil {
		s.tcp = tcpListener.Addr()
	}
	attrs := []any{
		"stun", s.STUNURL(), "turn", s.TURNURL(), "realm", cfg.Realm,
		"users", len(keys), "ephemeral", cfg.AuthSecret != "",
		"default_plan", cfg.DefaultPlan.String(),
	}
	if s.tcp != nil {
		attrs = append(attrs, "turn_tcp", s.TURNTCPURL())
	}
	if cfg.MinPort != 0 {
		attrs = append(attrs, "relay_ports", fmt.Sprintf("%d-%d", cfg.MinPort, cfg.MaxPort))
	}
	for name, plan := range cfg.Plans {
		attrs = append(attrs, "plan."+name, plan.String())
	}
	log.Info("turn: serving", attrs...)
	return s, nil
}

// Addr is the UDP address the server is listening on.
func (s *Server) Addr() *net.UDPAddr { return s.addr }

// STUNURL is the URL to configure as a STUN server.
func (s *Server) STUNURL() string { return "stun:" + s.hostPort() }

// TURNURL is the URL to configure as a TURN server over UDP.
func (s *Server) TURNURL() string { return "turn:" + s.hostPort() + "?transport=udp" }

// TURNTCPURL is the URL of the TCP listener, or empty when there is none.
func (s *Server) TURNTCPURL() string {
	if s.tcp == nil {
		return ""
	}
	return "turn:" + s.tcp.String() + "?transport=tcp"
}

func (s *Server) hostPort() string {
	return net.JoinHostPort(s.addr.IP.String(), strconv.Itoa(s.addr.Port))
}

// Realm returns the configured realm.
func (s *Server) Realm() string { return s.cfg.Realm }

// IssueCredentials returns ephemeral credentials for userID that expire after
// ttl, signed with the server's auth secret. It fails when no secret is set.
func (s *Server) IssueCredentials(userID string, ttl time.Duration) (username, password string, err error) {
	return IssueCredentials(s.cfg.AuthSecret, userID, ttl)
}

// IssueCredentials returns TURN REST API credentials for userID under secret.
// The username embeds the expiry, so the server needs no database: it
// recomputes the HMAC and checks the clock. Put the plan in the user ID, for
// example "alice@paid", and [Config.PlanFor]'s default maps it.
func IssueCredentials(secret, userID string, ttl time.Duration) (username, password string, err error) {
	if secret == "" {
		return "", "", errors.New("turnx: an auth secret is required to issue credentials")
	}
	if userID == "" || strings.Contains(userID, ":") {
		return "", "", errors.New("turnx: the user ID must be non-empty and must not contain ':'")
	}
	if ttl <= 0 {
		ttl = DefaultCredentialTTL
	}
	return turn.GenerateLongTermTURNRESTCredentials(secret, userID, ttl)
}

// Stats reports relayed traffic since startup.
func (s *Server) Stats() Stats { return s.gen.stats() }

// Close stops the server and releases the listeners.
func (s *Server) Close() error {
	err := s.srv.Close()
	// turn.Server.Close closes the listeners it was given.
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("turnx: close: %w", err)
	}
	return nil
}

// planName resolves the plan for an authenticated user ID.
func (c *Config) planName(userID string) string {
	if c.PlanFor != nil {
		return c.PlanFor(userID)
	}
	if u, ok := c.Users[userID]; ok && u.Plan != "" {
		return u.Plan
	}
	if _, plan, ok := strings.Cut(userID, "@"); ok {
		if _, known := c.Plans[plan]; known {
			return plan
		}
	}
	return ""
}

// plan resolves the effective plan for a user ID.
func (c *Config) plan(userID string) (string, Plan) {
	name := c.planName(userID)
	if p, ok := c.Plans[name]; ok && name != "" {
		return name, p
	}
	return "default", c.DefaultPlan
}

// userIDOf recovers the user ID that the auth handler reports for a raw TURN
// username. Ephemeral usernames carry an expiry prefix.
func (c *Config) userIDOf(username string) string {
	if c.AuthSecret == "" {
		return username
	}
	if _, ok := c.Users[username]; ok {
		return username
	}
	if _, id, ok := strings.Cut(username, ":"); ok {
		return id
	}
	return username
}

// shapedGenerator allocates relay sockets wrapped in their user's budget and
// keeps the counters behind [Stats].
type shapedGenerator struct {
	inner turn.RelayAddressGenerator
	cfg   *Config
	log   *slog.Logger

	allocations atomic.Int64
	active      atomic.Int64
	counters    counters

	mu    sync.Mutex
	users map[string]*userCounters
}

// Validate implements [turn.RelayAddressGenerator].
func (g *shapedGenerator) Validate() error { return g.inner.Validate() }

// AllocatePacketConn implements [turn.RelayAddressGenerator].
func (g *shapedGenerator) AllocatePacketConn(conf turn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.inner.AllocatePacketConn(conf)
	if err != nil {
		return nil, nil, err
	}

	name, plan := g.cfg.plan(conf.UserID)
	uc := g.user(conf.UserID, name)
	g.allocations.Add(1)
	uc.allocations.Add(1)
	g.log.Info("turn: allocated a relay",
		"user", conf.UserID, "plan", name, "relay", addrString(addr), "rate", rateString(plan.Rate))

	sinks := []*counters{&g.counters, &uc.counters}
	if plan.Rate <= 0 {
		return &countingConn{PacketConn: conn, sinks: sinks}, addr, nil
	}
	burst := burstFor(plan.Rate, plan.Burst)
	return &shapedConn{
		PacketConn: conn,
		outbound:   rate.NewLimiter(rate.Limit(plan.Rate), int(burst)),
		inbound:    rate.NewLimiter(rate.Limit(plan.Rate), int(burst)),
		maxDelay:   maxDelayFor(plan.MaxDelay),
		sinks:      sinks,
	}, addr, nil
}

// AllocateListener implements [turn.RelayAddressGenerator]. TCP relaying is not
// shaped, because pipe relays UDP.
func (g *shapedGenerator) AllocateListener(conf turn.AllocateListenerConfig) (net.Listener, net.Addr, error) {
	return g.inner.AllocateListener(conf)
}

// AllocateConn implements [turn.RelayAddressGenerator].
func (g *shapedGenerator) AllocateConn(conf turn.AllocateConnConfig) (net.Conn, error) {
	return g.inner.AllocateConn(conf)
}

// quota implements [turn.QuotaHandler]: it refuses an allocation when the user
// already holds as many as its plan allows.
func (g *shapedGenerator) quota(username, _ string, srcAddr net.Addr) bool {
	id := g.cfg.userIDOf(username)
	name, plan := g.cfg.plan(id)
	if plan.MaxAllocations <= 0 {
		return true
	}
	uc := g.user(id, name)
	if uc.active.Load() < int64(plan.MaxAllocations) {
		return true
	}
	uc.rejected.Add(1)
	g.log.Warn("turn: allocation quota reached",
		"user", id, "plan", name, "limit", plan.MaxAllocations, "from", addrString(srcAddr))
	return false
}

// events tracks live allocations per user.
func (g *shapedGenerator) events() turn.EventHandler {
	return turn.EventHandler{
		OnAllocationCreated: func(_, _ net.Addr, _, userID, _ string, _ net.Addr, _ int) {
			g.active.Add(1)
			g.userExisting(userID).active.Add(1)
		},
		OnAllocationDeleted: func(_, _ net.Addr, _, userID, _ string) {
			g.active.Add(-1)
			g.userExisting(userID).active.Add(-1)
			g.log.Info("turn: released a relay", "user", userID)
		},
	}
}

func (g *shapedGenerator) user(id, plan string) *userCounters {
	g.mu.Lock()
	defer g.mu.Unlock()
	uc, ok := g.users[id]
	if !ok {
		uc = &userCounters{plan: plan}
		g.users[id] = uc
	}
	return uc
}

func (g *shapedGenerator) userExisting(id string) *userCounters {
	name, _ := g.cfg.plan(id)
	return g.user(id, name)
}

func (g *shapedGenerator) stats() Stats {
	s := g.counters.snapshot()
	s.Allocations = g.allocations.Load()
	s.Active = g.active.Load()

	g.mu.Lock()
	defer g.mu.Unlock()
	s.Users = make(map[string]UserStats, len(g.users))
	for id, uc := range g.users {
		c := uc.counters.snapshot()
		s.Users[id] = UserStats{
			Plan:                uc.plan,
			Allocations:         uc.allocations.Load(),
			Active:              uc.active.Load(),
			SentBytes:           c.SentBytes,
			ReceivedBytes:       c.ReceivedBytes,
			DroppedBytes:        c.DroppedBytes,
			DroppedPackets:      c.DroppedPackets,
			RejectedAllocations: uc.rejected.Load(),
		}
	}
	return s
}

// userCounters is the per-user slice of the statistics.
type userCounters struct {
	plan        string
	allocations atomic.Int64
	active      atomic.Int64
	rejected    atomic.Int64
	counters    counters
}

// counters accumulates relayed traffic.
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

func addSent(sinks []*counters, n int) {
	for _, c := range sinks {
		c.sentBytes.Add(int64(n))
		c.sentPackets.Add(1)
	}
}

func addReceived(sinks []*counters, n int) {
	for _, c := range sinks {
		c.recvBytes.Add(int64(n))
		c.recvPackets.Add(1)
	}
}

func addDropped(sinks []*counters, n int) {
	for _, c := range sinks {
		c.droppedBytes.Add(int64(n))
		c.droppedPackets.Add(1)
	}
}

func addDelayed(sinks []*counters) {
	for _, c := range sinks {
		c.delayedPackets.Add(1)
	}
}

// countingConn measures a relay socket without limiting it.
type countingConn struct {
	net.PacketConn
	sinks []*counters
}

func (c *countingConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(p)
	if n > 0 {
		addReceived(c.sinks, n)
	}
	return n, addr, err
}

func (c *countingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	if n > 0 {
		addSent(c.sinks, n)
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
	sinks    []*counters
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
			addDropped(c.sinks, n)
			continue
		case delay > 0:
			addDelayed(c.sinks)
			time.Sleep(delay)
		}

		addReceived(c.sinks, n)
		return n, addr, nil
	}
}

// WriteTo sends p if it fits the budget and reports success either way: a
// dropped datagram is indistinguishable from one lost in transit, which is
// exactly what is being simulated.
func (c *shapedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if !c.outbound.AllowN(time.Now(), len(p)) {
		addDropped(c.sinks, len(p))
		return len(p), nil
	}

	n, err := c.PacketConn.WriteTo(p, addr)
	if n > 0 {
		addSent(c.sinks, n)
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

// ParseUsers parses a "user=password[:plan],user2=password2" list.
func ParseUsers(spec string) (map[string]User, error) {
	users := make(map[string]User)
	for entry := range strings.SplitSeq(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		user, rest, ok := strings.Cut(entry, "=")
		if !ok || user == "" || rest == "" {
			return nil, fmt.Errorf("turnx: %q is not a user=password[:plan] entry", entry)
		}
		password, plan, _ := strings.Cut(rest, ":")
		if password == "" {
			return nil, fmt.Errorf("turnx: %q has an empty password", entry)
		}
		users[user] = User{Password: password, Plan: plan}
	}
	return users, nil
}

// ParsePlans parses a "name=rate[/maxallocations],..." list, for example
// "free=512KiB/4,paid=8MiB". A rate of 0 means unlimited.
func ParsePlans(spec string) (map[string]Plan, error) {
	plans := make(map[string]Plan)
	for entry := range strings.SplitSeq(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rest, ok := strings.Cut(entry, "=")
		if !ok || name == "" || rest == "" {
			return nil, fmt.Errorf("turnx: %q is not a name=rate[/maxallocations] entry", entry)
		}
		rateSpec, allocSpec, hasAlloc := strings.Cut(rest, "/")
		r, err := ParseSize(rateSpec)
		if err != nil {
			return nil, fmt.Errorf("turnx: plan %q: %w", name, err)
		}
		p := Plan{Rate: r}
		if hasAlloc {
			n, err := strconv.Atoi(strings.TrimSpace(allocSpec))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("turnx: plan %q: %q is not an allocation count", name, allocSpec)
			}
			p.MaxAllocations = n
		}
		plans[name] = p
	}
	return plans, nil
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

// ParsePortRange parses "min-max" into an inclusive port range. An empty
// string means no restriction.
func ParsePortRange(spec string) (minPort, maxPort uint16, err error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return 0, 0, nil
	}
	lo, hi, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("turnx: %q is not a min-max port range", spec)
	}
	a, err := strconv.ParseUint(strings.TrimSpace(lo), 10, 16)
	if err != nil || a == 0 {
		return 0, 0, fmt.Errorf("turnx: %q is not a valid port", lo)
	}
	b, err := strconv.ParseUint(strings.TrimSpace(hi), 10, 16)
	if err != nil || b == 0 || b < a {
		return 0, 0, fmt.Errorf("turnx: %q is not a valid upper port", hi)
	}
	return uint16(a), uint16(b), nil
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

// SortedUsers returns the user IDs in stats in a stable order.
func SortedUsers(s Stats) []string {
	out := make([]string, 0, len(s.Users))
	for id := range s.Users {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
