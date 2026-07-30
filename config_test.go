package pipe

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// stubSignaler satisfies the SPI without doing anything, for configuration
// tests that never negotiate.
type stubSignaler struct{}

func (stubSignaler) Open(context.Context, PeerID) (SignalConn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Send(context.Context, Signal) error { return nil }
func (stubConn) Receive(ctx context.Context) (Signal, error) {
	<-ctx.Done()
	return Signal{}, ctx.Err()
}
func (stubConn) Close() error { return nil }

func TestConfigDefaults(t *testing.T) {
	got, err := Config{ID: "alice", Signaler: stubSignaler{}}.clone()
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	if got.DialTimeout != DefaultDialTimeout {
		t.Errorf("DialTimeout = %v, want %v", got.DialTimeout, DefaultDialTimeout)
	}
	if got.ICETimeout != DefaultICETimeout {
		t.Errorf("ICETimeout = %v, want %v", got.ICETimeout, DefaultICETimeout)
	}
	if got.AcceptBacklog != DefaultAcceptBacklog {
		t.Errorf("AcceptBacklog = %d, want %d", got.AcceptBacklog, DefaultAcceptBacklog)
	}
	if got.FramePayload != DefaultFramePayload {
		t.Errorf("FramePayload = %d, want %d", got.FramePayload, DefaultFramePayload)
	}
	if got.ReadBuffer != DefaultReadBuffer {
		t.Errorf("ReadBuffer = %d, want %d", got.ReadBuffer, DefaultReadBuffer)
	}
	if got.Logger == nil {
		t.Error("Logger is nil; it should be a discard logger")
	}
	if got.Metrics == nil {
		t.Error("Metrics is nil; it should discard measurements")
	}
	if got.clock == nil {
		t.Error("clock is nil; it should be the system clock")
	}
	if got.KeepAlive.Interval != 0 {
		t.Error("keepalive should be disabled by default")
	}
	if !got.Reconnect.Enabled || got.Reconnect.MaxAttempts <= 0 {
		t.Errorf("Reconnect = %+v, want a bounded enabled policy", got.Reconnect)
	}
	if got.Reconnect.Backoff.Factor < 1 {
		t.Errorf("Backoff.Factor = %v, want at least 1", got.Reconnect.Backoff.Factor)
	}
}

func TestConfigValidation(t *testing.T) {
	base := func() Config {
		return Config{ID: "alice", Signaler: stubSignaler{}}
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"empty ID", func(c *Config) { c.ID = "" }, "peer id is empty"},
		{"oversize ID", func(c *Config) { c.ID = PeerID(strings.Repeat("a", MaxPeerIDLength+1)) }, "exceeds"},
		{"ID with a control character", func(c *Config) { c.ID = "a\nb" }, "control character"},
		{"padded ID", func(c *Config) { c.ID = " alice " }, "whitespace"},
		{"missing signaler", func(c *Config) { c.Signaler = nil }, "Signaler is required"},
		{"negative dial timeout", func(c *Config) { c.DialTimeout = -1 }, "DialTimeout"},
		{"negative ICE timeout", func(c *Config) { c.ICETimeout = -1 }, "ICETimeout"},
		{"negative backlog", func(c *Config) { c.AcceptBacklog = -1 }, "AcceptBacklog"},
		{"oversize frame payload", func(c *Config) { c.FramePayload = MaxFramePayload + 1 }, "FramePayload"},
		{"negative frame payload", func(c *Config) { c.FramePayload = -1 }, "FramePayload"},
		{"read buffer below a frame", func(c *Config) { c.ReadBuffer = 8 }, "ReadBuffer"},
		{
			"keepalive timeout above the interval",
			func(c *Config) { c.KeepAlive = KeepAliveConfig{Interval: time.Second, Timeout: 2 * time.Second} },
			"KeepAlive.Timeout",
		},
		{
			"negative keepalive",
			func(c *Config) { c.KeepAlive = KeepAliveConfig{Interval: -1} },
			"KeepAlive durations",
		},
		{
			"reconnect without attempts",
			func(c *Config) { c.Reconnect = ReconnectPolicy{Enabled: true} },
			"MaxAttempts",
		},
		{
			"reconnect without an attempt timeout",
			func(c *Config) { c.Reconnect = ReconnectPolicy{Enabled: true, MaxAttempts: 2} },
			"AttemptTimeout",
		},
		{
			"backoff factor below 1",
			func(c *Config) {
				c.Reconnect = ReconnectPolicy{
					Enabled: true, MaxAttempts: 2, AttemptTimeout: time.Second,
					Backoff: Backoff{Factor: 0.5},
				}
			},
			"Factor",
		},
		{
			"jitter out of range",
			func(c *Config) {
				c.Reconnect = ReconnectPolicy{
					Enabled: true, MaxAttempts: 2, AttemptTimeout: time.Second,
					Backoff: Backoff{Jitter: 2},
				}
			},
			"Jitter",
		},
		{
			"backoff maximum below initial",
			func(c *Config) {
				c.Reconnect = ReconnectPolicy{
					Enabled: true, MaxAttempts: 2, AttemptTimeout: time.Second,
					Backoff: Backoff{Initial: time.Minute, Maximum: time.Second},
				}
			},
			"Maximum",
		},
		{"unknown transport policy", func(c *Config) { c.ICETransportPolicy = 42 }, "ICETransportPolicy"},
		{
			"relay policy without TURN",
			func(c *Config) {
				c.ICETransportPolicy = ICETransportPolicyRelay
				c.ICEServers = []ICEServer{{URLs: []string{"stun:stun.example.net:3478"}}}
			},
			"requires a TURN server",
		},
		{"ICE server without URLs", func(c *Config) { c.ICEServers = []ICEServer{{}} }, "at least one URL"},
		{
			"unknown ICE scheme",
			func(c *Config) { c.ICEServers = []ICEServer{{URLs: []string{"http://example.net"}}} },
			"scheme",
		},
		{
			"malformed ICE URL",
			func(c *Config) { c.ICEServers = []ICEServer{{URLs: []string{"stun"}}} },
			"not a valid ICE URL",
		},
		{
			"TURN without credentials",
			func(c *Config) { c.ICEServers = []ICEServer{{URLs: []string{"turn:turn.example.net:3478"}}} },
			"require a username and credential",
		},
		{
			"STUN with credentials",
			func(c *Config) {
				c.ICEServers = []ICEServer{{
					URLs:     []string{"stun:stun.example.net:3478"},
					Username: "u", Credential: "p",
				}}
			},
			"must not carry credentials",
		},
		{
			"OAuth credentials",
			func(c *Config) {
				c.ICEServers = []ICEServer{{
					URLs:           []string{"turn:turn.example.net:3478"},
					Username:       "u",
					Credential:     "p",
					CredentialType: ICECredentialOAuth,
				}}
			},
			"OAuth",
		},
		{
			"unknown credential type",
			func(c *Config) {
				c.ICEServers = []ICEServer{{
					URLs:           []string{"turn:turn.example.net:3478"},
					Username:       "u",
					Credential:     "p",
					CredentialType: ICECredentialType(9),
				}}
			},
			"unknown credential type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)

			_, err := cfg.clone()
			if err == nil {
				t.Fatal("clone accepted an invalid configuration")
			}
			if !errors.Is(err, ErrConfig) {
				t.Errorf("error %v does not match ErrConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestConfigAcceptsValidICEServers(t *testing.T) {
	cfg := Config{
		ID:       "alice",
		Signaler: stubSignaler{},
		ICEServers: []ICEServer{
			{URLs: []string{"stun:stun.example.net:3478", "stuns:stun.example.net:5349"}},
			{
				URLs:     []string{"turn:turn.example.net:3478", "turns:turn.example.net:5349"},
				Username: "user", Credential: "secret",
			},
		},
		ICETransportPolicy: ICETransportPolicyRelay,
	}
	if _, err := cfg.clone(); err != nil {
		t.Fatalf("clone rejected a valid configuration: %v", err)
	}
}

// TestConfigCopiesSlices proves configuration is immutable after construction.
func TestConfigCopiesSlices(t *testing.T) {
	urls := []string{"stun:stun.example.net:3478"}
	servers := []ICEServer{{URLs: urls}}

	cfg := Config{ID: "alice", Signaler: stubSignaler{}, ICEServers: servers}
	got, err := cfg.clone()
	if err != nil {
		t.Fatal(err)
	}

	// Mutating the caller's slices must not reach the copy.
	urls[0] = "stun:evil.example.net:3478"
	servers[0].Username = "injected"
	servers = append(servers, ICEServer{URLs: []string{"stun:other:3478"}})

	if len(got.ICEServers) != 1 {
		t.Fatalf("copy has %d ICE servers, want 1", len(got.ICEServers))
	}
	if got.ICEServers[0].URLs[0] != "stun:stun.example.net:3478" {
		t.Errorf("URL = %q, want the original", got.ICEServers[0].URLs[0])
	}
	if got.ICEServers[0].Username != "" {
		t.Errorf("Username = %q, want it unchanged", got.ICEServers[0].Username)
	}
}

func TestConfigKeepAliveTimeoutDefaultsToInterval(t *testing.T) {
	cfg := Config{
		ID:        "alice",
		Signaler:  stubSignaler{},
		KeepAlive: KeepAliveConfig{Interval: 3 * time.Second},
	}
	got, err := cfg.clone()
	if err != nil {
		t.Fatal(err)
	}
	if got.KeepAlive.Timeout != 3*time.Second {
		t.Errorf("Timeout = %v, want the interval", got.KeepAlive.Timeout)
	}
}

func TestConfigDisabledKeepAliveClearsTimeout(t *testing.T) {
	cfg := Config{
		ID:        "alice",
		Signaler:  stubSignaler{},
		KeepAlive: KeepAliveConfig{Timeout: time.Second},
	}
	got, err := cfg.clone()
	if err != nil {
		t.Fatal(err)
	}
	if got.KeepAlive.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 while keepalive is off", got.KeepAlive.Timeout)
	}
}

func TestConfigKeepsSuppliedLoggerAndMetrics(t *testing.T) {
	logger := slog.Default()
	metrics := &recordingMetrics{}

	got, err := Config{ID: "alice", Signaler: stubSignaler{}, Logger: logger, Metrics: metrics}.clone()
	if err != nil {
		t.Fatal(err)
	}
	if got.Logger != logger {
		t.Error("the supplied logger was replaced")
	}
	if got.Metrics != metrics {
		t.Error("the supplied metrics sink was replaced")
	}
}

func TestStringers(t *testing.T) {
	if got := ICECredentialPassword.String(); got != "password" {
		t.Errorf("credential type = %q", got)
	}
	if got := ICECredentialType(9).String(); got != "unknown" {
		t.Errorf("credential type = %q", got)
	}
	if got := ICETransportPolicyRelay.String(); got != "relay" {
		t.Errorf("policy = %q", got)
	}
	if got := ICETransportPolicy(9).String(); got != "unknown" {
		t.Errorf("policy = %q", got)
	}

	states := map[ConnectionState]string{
		StateNew:            "new",
		StateSignaling:      "signaling",
		StateConnecting:     "connecting",
		StateConnected:      "connected",
		StateRecovering:     "recovering",
		StateClosing:        "closing",
		StateClosed:         "closed",
		ConnectionState(99): "unknown",
	}
	for state, want := range states {
		if got := state.String(); got != want {
			t.Errorf("ConnectionState(%d) = %q, want %q", state, got, want)
		}
	}

	if got := roleOfferer.String(); got != "offerer" {
		t.Errorf("role = %q", got)
	}
	if got := roleAnswerer.String(); got != "answerer" {
		t.Errorf("role = %q", got)
	}
}

// recordingMetrics captures measurements so tests can assert on them.
type recordingMetrics struct {
	counts    map[string]int64
	gauges    map[string]int64
	durations map[string]int
}

func newRecordingMetrics() *recordingMetrics {
	return &recordingMetrics{
		counts:    make(map[string]int64),
		gauges:    make(map[string]int64),
		durations: make(map[string]int),
	}
}

func (m *recordingMetrics) key(name string, labels []Label) string {
	var b strings.Builder
	b.WriteString(name)
	for _, l := range labels {
		b.WriteString("|")
		b.WriteString(l.Key)
		b.WriteString("=")
		b.WriteString(l.Value)
	}
	return b.String()
}

func (m *recordingMetrics) Count(name string, delta int64, labels ...Label) {
	if m.counts == nil {
		return
	}
	m.counts[m.key(name, labels)] += delta
}

func (m *recordingMetrics) Gauge(name string, value int64, labels ...Label) {
	if m.gauges == nil {
		return
	}
	m.gauges[m.key(name, labels)] = value
}

func (m *recordingMetrics) Duration(name string, _ time.Duration, labels ...Label) {
	if m.durations == nil {
		return
	}
	m.durations[m.key(name, labels)]++
}
