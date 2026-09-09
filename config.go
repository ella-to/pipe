package pipe

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"ella.to/pipe/internal/clock"
)

// Documented defaults applied by [New] to zero-valued configuration.
const (
	// DefaultDialTimeout bounds a single outbound negotiation.
	DefaultDialTimeout = 30 * time.Second

	// DefaultICETimeout bounds connectivity establishment once descriptions
	// have been exchanged.
	DefaultICETimeout = 20 * time.Second

	// DefaultAcceptBacklog bounds inbound sessions that are negotiating or
	// waiting to be accepted.
	DefaultAcceptBacklog = 64

	// DefaultFramePayload is the largest application payload placed in one
	// stream frame.
	DefaultFramePayload = 16 << 10

	// DefaultReadBuffer is the largest amount of received but unread
	// application data held per connection.
	DefaultReadBuffer = 1 << 20

	// MaxFramePayload is the hard ceiling for [Config.FramePayload].
	MaxFramePayload = 256 << 10

	// signalSendTimeout bounds one signaling send when the caller has no
	// tighter deadline.
	signalSendTimeout = 10 * time.Second

	// dedupeCacheSize is the number of recent signal IDs remembered per
	// endpoint for duplicate suppression.
	dedupeCacheSize = 4096
)

// ICECredentialType selects how [ICEServer.Credential] is interpreted.
type ICECredentialType int

// Supported ICE credential types.
const (
	// ICECredentialPassword is a long-term credential password. It is the
	// default.
	ICECredentialPassword ICECredentialType = iota

	// ICECredentialOAuth is reserved for OAuth credentials and is not
	// supported yet.
	ICECredentialOAuth
)

// String implements [fmt.Stringer].
func (t ICECredentialType) String() string {
	switch t {
	case ICECredentialPassword:
		return "password"
	case ICECredentialOAuth:
		return "oauth"
	default:
		return "unknown"
	}
}

// ICEServer describes one STUN or TURN server. Pipe validates the URLs and
// credential combination and hands the result to Pion, which owns gathering,
// STUN transactions, and TURN allocations.
//
// Credentials are never logged, exported in metrics labels, or placed in
// signaling messages.
type ICEServer struct {
	// URLs lists stun:, stuns:, turn:, or turns: URLs for one logical server.
	URLs []string

	// Username is the TURN username. It must be empty for STUN-only servers.
	Username string

	// Credential is the TURN credential.
	Credential string

	// CredentialType selects how Credential is interpreted.
	CredentialType ICECredentialType
}

// ICETransportPolicy restricts which candidate types Pipe gathers.
type ICETransportPolicy int

// Supported ICE transport policies.
const (
	// ICETransportPolicyAll gathers host, server-reflexive, and relay
	// candidates. It is the default.
	ICETransportPolicyAll ICETransportPolicy = iota

	// ICETransportPolicyRelay gathers relay candidates only, which forces
	// traffic through TURN.
	ICETransportPolicyRelay
)

// String implements [fmt.Stringer].
func (p ICETransportPolicy) String() string {
	switch p {
	case ICETransportPolicyAll:
		return "all"
	case ICETransportPolicyRelay:
		return "relay"
	default:
		return "unknown"
	}
}

// KeepAliveConfig configures protocol-level ping and pong probes on an
// established connection. Keepalive is disabled unless Interval is positive.
type KeepAliveConfig struct {
	// Interval is the delay between probes.
	Interval time.Duration

	// Timeout bounds the wait for a matching pong. It defaults to Interval
	// when zero and must not exceed Interval.
	Timeout time.Duration
}

// ReconnectPolicy bounds recovery of an established connection. Pipe recovers
// by restarting ICE on the existing PeerConnection. It never replaces the
// PeerConnection underneath a live connection; see [ErrDisconnected].
type ReconnectPolicy struct {
	// Enabled turns ICE-restart recovery on.
	Enabled bool

	// MaxAttempts bounds consecutive recovery attempts.
	MaxAttempts int

	// AttemptTimeout bounds one recovery attempt.
	AttemptTimeout time.Duration

	// Backoff spaces successive attempts.
	Backoff Backoff
}

// Backoff describes exponential backoff with jitter.
type Backoff struct {
	// Initial is the first delay.
	Initial time.Duration

	// Maximum caps the delay.
	Maximum time.Duration

	// Factor multiplies the delay after each attempt. Values below 1 are
	// rejected.
	Factor float64

	// Jitter randomizes each delay by up to this fraction, in [0, 1].
	Jitter float64
}

// Config configures an [Endpoint]. Only ID and Signaler are required; every
// other field has a documented default. Config is copied by [New] and is not
// consulted again afterwards.
type Config struct {
	// ID is the local peer ID used for signaling.
	ID PeerID

	// Signaler opens the signaling connection.
	Signaler Signaler

	// ICEServers lists STUN and TURN servers. An empty list restricts
	// connectivity to host candidates.
	ICEServers []ICEServer

	// ICETransportPolicy restricts candidate types.
	ICETransportPolicy ICETransportPolicy

	// DialTimeout bounds one outbound negotiation. It defaults to
	// [DefaultDialTimeout].
	DialTimeout time.Duration

	// ICETimeout bounds connectivity establishment. It defaults to
	// [DefaultICETimeout].
	ICETimeout time.Duration

	// KeepAlive configures ping and pong probes. Probes are disabled by
	// default.
	KeepAlive KeepAliveConfig

	// Reconnect bounds ICE-restart recovery. It defaults to enabled with a
	// small number of attempts.
	Reconnect ReconnectPolicy

	// AcceptBacklog bounds inbound sessions that are negotiating or waiting to
	// be accepted. It defaults to [DefaultAcceptBacklog].
	AcceptBacklog int

	// AllowPeer, when set, is consulted for every inbound offer before any
	// resources are committed to it. Returning false refuses the session with
	// [RejectUnauthorized], and the dialer sees [ErrPeerRejected]. A nil
	// AllowPeer admits every peer.
	//
	// The peer ID is only as trustworthy as the signaling transport that
	// delivered it. AllowPeer is an access-control list on top of an
	// authenticated signaler, not a substitute for one. It must be safe for
	// concurrent use and should return quickly; it runs on the signaling
	// receive loop.
	AllowPeer func(peer PeerID) bool

	// FramePayload is the largest application payload per stream frame. It
	// defaults to [DefaultFramePayload] and may not exceed [MaxFramePayload].
	FramePayload int

	// ReadBuffer bounds received but unread application data per connection.
	// It defaults to [DefaultReadBuffer].
	ReadBuffer int

	// Logger receives structured logs. Logging is discarded when nil; Pipe
	// never falls back to the global logger.
	Logger *slog.Logger

	// Metrics receives counters, gauges, and durations. Measurement is
	// discarded when nil.
	Metrics Metrics

	// Pion is the advanced escape hatch for tuning the WebRTC engine. The
	// defaults are correct for ordinary use.
	Pion PionOptions

	// clock is the time source used by timers. Tests inject a fake clock.
	clock clock.Clock
}

// clone returns a deep copy with defaults applied, or an error describing the
// first invalid field. The copy shares no mutable state with cfg.
func (cfg Config) clone() (Config, error) {
	out := cfg

	if err := validPeerID(out.ID); err != nil {
		return Config{}, errorf(ErrConfig, "pipe: config ID: %w", err)
	}
	if out.Signaler == nil {
		return Config{}, errorf(ErrConfig, "pipe: config Signaler is required")
	}

	out.ICEServers = slices.Clone(cfg.ICEServers)
	for i := range out.ICEServers {
		out.ICEServers[i].URLs = slices.Clone(cfg.ICEServers[i].URLs)
		if err := validateICEServer(out.ICEServers[i]); err != nil {
			return Config{}, errorf(ErrConfig, "pipe: config ICEServers[%d]: %w", i, err)
		}
	}

	switch out.ICETransportPolicy {
	case ICETransportPolicyAll, ICETransportPolicyRelay:
	default:
		return Config{}, errorf(ErrConfig, "pipe: config ICETransportPolicy %d is unknown",
			out.ICETransportPolicy)
	}
	if out.ICETransportPolicy == ICETransportPolicyRelay && !hasTURN(out.ICEServers) {
		return Config{}, errorf(ErrConfig, "pipe: config ICETransportPolicy relay requires a TURN server")
	}

	if err := setTimeout(&out.DialTimeout, DefaultDialTimeout, "DialTimeout"); err != nil {
		return Config{}, err
	}
	if err := setTimeout(&out.ICETimeout, DefaultICETimeout, "ICETimeout"); err != nil {
		return Config{}, err
	}

	if out.AcceptBacklog == 0 {
		out.AcceptBacklog = DefaultAcceptBacklog
	}
	if out.AcceptBacklog < 0 {
		return Config{}, errorf(ErrConfig, "pipe: config AcceptBacklog must not be negative")
	}

	if out.FramePayload == 0 {
		out.FramePayload = DefaultFramePayload
	}
	if out.FramePayload < 0 || out.FramePayload > MaxFramePayload {
		return Config{}, errorf(ErrConfig, "pipe: config FramePayload must be in (0, %d]", MaxFramePayload)
	}

	if out.ReadBuffer == 0 {
		out.ReadBuffer = DefaultReadBuffer
	}
	if out.ReadBuffer < out.FramePayload {
		return Config{}, errorf(ErrConfig, "pipe: config ReadBuffer must be at least FramePayload (%d)",
			out.FramePayload)
	}

	if err := validateKeepAlive(&out.KeepAlive); err != nil {
		return Config{}, err
	}
	if err := validateReconnect(&out.Reconnect); err != nil {
		return Config{}, err
	}

	if out.Logger == nil {
		out.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if out.Metrics == nil {
		out.Metrics = nopMetrics{}
	}
	if out.clock == nil {
		out.clock = clock.System()
	}
	return out, nil
}

func setTimeout(field *time.Duration, def time.Duration, name string) error {
	switch {
	case *field == 0:
		*field = def
	case *field < 0:
		return errorf(ErrConfig, "pipe: config %s must not be negative", name)
	}
	return nil
}

func validateKeepAlive(ka *KeepAliveConfig) error {
	if ka.Interval < 0 || ka.Timeout < 0 {
		return errorf(ErrConfig, "pipe: config KeepAlive durations must not be negative")
	}
	if ka.Interval == 0 {
		// Keepalive is off; a stray timeout must not enable it.
		ka.Timeout = 0
		return nil
	}
	if ka.Timeout == 0 {
		ka.Timeout = ka.Interval
	}
	if ka.Timeout > ka.Interval {
		return errorf(ErrConfig, "pipe: config KeepAlive.Timeout must not exceed KeepAlive.Interval")
	}
	return nil
}

func validateReconnect(rp *ReconnectPolicy) error {
	if *rp == (ReconnectPolicy{}) {
		*rp = defaultReconnectPolicy()
		return nil
	}
	if !rp.Enabled {
		return nil
	}
	if rp.MaxAttempts <= 0 {
		return errorf(ErrConfig, "pipe: config Reconnect.MaxAttempts must be positive when enabled")
	}
	if rp.AttemptTimeout <= 0 {
		return errorf(ErrConfig, "pipe: config Reconnect.AttemptTimeout must be positive when enabled")
	}
	b := &rp.Backoff
	if b.Initial < 0 || b.Maximum < 0 {
		return errorf(ErrConfig, "pipe: config Reconnect.Backoff durations must not be negative")
	}
	if b.Initial == 0 {
		b.Initial = 500 * time.Millisecond
	}
	if b.Maximum == 0 {
		b.Maximum = 5 * time.Second
	}
	if b.Maximum < b.Initial {
		return errorf(ErrConfig, "pipe: config Reconnect.Backoff.Maximum must be at least Initial")
	}
	if b.Factor == 0 {
		b.Factor = 2
	}
	if b.Factor < 1 {
		return errorf(ErrConfig, "pipe: config Reconnect.Backoff.Factor must be at least 1")
	}
	if b.Jitter < 0 || b.Jitter > 1 {
		return errorf(ErrConfig, "pipe: config Reconnect.Backoff.Jitter must be in [0, 1]")
	}
	return nil
}

func defaultReconnectPolicy() ReconnectPolicy {
	return ReconnectPolicy{
		Enabled:        true,
		MaxAttempts:    3,
		AttemptTimeout: 10 * time.Second,
		Backoff: Backoff{
			Initial: 500 * time.Millisecond,
			Maximum: 5 * time.Second,
			Factor:  2,
			Jitter:  0.2,
		},
	}
}

func validateICEServer(s ICEServer) error {
	if len(s.URLs) == 0 {
		return errorf(ErrConfig, "at least one URL is required")
	}
	needsCredential := false
	for _, raw := range s.URLs {
		scheme, rest, ok := strings.Cut(raw, ":")
		if !ok || rest == "" {
			return errorf(ErrConfig, "URL %q is not a valid ICE URL", raw)
		}
		switch strings.ToLower(scheme) {
		case "stun", "stuns":
		case "turn", "turns":
			needsCredential = true
		default:
			return errorf(ErrConfig, "URL %q must use the stun, stuns, turn, or turns scheme", raw)
		}
	}
	switch s.CredentialType {
	case ICECredentialPassword:
	case ICECredentialOAuth:
		return errorf(ErrConfig, "OAuth ICE credentials are not supported")
	default:
		return errorf(ErrConfig, "unknown credential type %d", s.CredentialType)
	}
	switch {
	case needsCredential && (s.Username == "" || s.Credential == ""):
		return errorf(ErrConfig, "TURN URLs require a username and credential")
	case !needsCredential && (s.Username != "" || s.Credential != ""):
		return errorf(ErrConfig, "STUN URLs must not carry credentials")
	}
	return nil
}

func hasTURN(servers []ICEServer) bool {
	for _, s := range servers {
		for _, raw := range s.URLs {
			switch scheme, _, _ := strings.Cut(raw, ":"); strings.ToLower(scheme) {
			case "turn", "turns":
				return true
			}
		}
	}
	return false
}
