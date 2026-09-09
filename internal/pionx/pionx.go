// Package pionx contains every direct use of pion/webrtc. It creates and
// negotiates one reliable, ordered DataChannel per session, trickles ICE
// candidates, and reports Pion callbacks as small immutable events to the
// session that owns the peer.
//
// Callbacks registered here do bounded work: they translate a Pion event and
// hand it to the sink. They never perform signaling or network I/O, and they are
// safe to invoke after the session has shut down.
package pionx

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

// DataChannel contract from guides/11-protocol.md section 3.
const (
	// ChannelLabel is the only DataChannel label pipe accepts.
	ChannelLabel = "pipe.stream.v1"

	// ChannelProtocol is the DataChannel subprotocol field.
	ChannelProtocol = "pipe/1"
)

// Errors reported by this package. Callers classify them into pipe error
// categories.
var (
	// ErrNegotiation reports a failure to build or apply a description or
	// DataChannel.
	ErrNegotiation = errors.New("pionx: negotiation failed")

	// ErrChannelRejected reports a DataChannel whose parameters do not match
	// the protocol contract.
	ErrChannelRejected = errors.New("pionx: data channel rejected")

	// ErrClosed reports use of a closed peer.
	ErrClosed = errors.New("pionx: peer is closed")
)

// Channel is the detached DataChannel capability handed to the stream adapter.
type Channel interface {
	io.ReadWriteCloser
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// ICEServer is the adapter's view of one STUN or TURN server.
type ICEServer struct {
	URLs       []string
	Username   string
	Credential string
}

// Config configures a [Factory].
type Config struct {
	// ICEServers lists STUN and TURN servers.
	ICEServers []ICEServer

	// RelayOnly restricts gathering to relay candidates.
	RelayOnly bool

	// ConfigureSettingEngine may adjust the setting engine before the API is
	// built. Detached data channels and blocking writes are already enabled.
	ConfigureSettingEngine func(*webrtc.SettingEngine)

	// ConfigureConfiguration may adjust the per-PeerConnection configuration.
	ConfigureConfiguration func(*webrtc.Configuration)
}

// Factory builds PeerConnections from one shared [webrtc.API]. Detached data
// channels must be enabled before any PeerConnection exists, which is why the
// API is built once per endpoint.
type Factory struct {
	api  *webrtc.API
	base webrtc.Configuration
}

// NewFactory builds the WebRTC API and the shared PeerConnection configuration.
func NewFactory(cfg Config) (*Factory, error) {
	se := webrtc.SettingEngine{}
	// The stream adapter owns reads and writes, so data channels are detached.
	se.DetachDataChannels()
	// Blocking writes give the stream adapter real SCTP backpressure and make
	// write deadlines effective instead of dropping data into an unbounded
	// queue.
	se.EnableDataChannelBlockWrite(true)

	if cfg.RelayOnly {
		// ICE normally holds a working relay pair for two seconds in case a
		// direct pair shows up. With relay as the only permitted type there is
		// nothing to wait for, and the wait would be pure connect latency.
		se.SetRelayAcceptanceMinWait(0)
	}

	if cfg.ConfigureSettingEngine != nil {
		cfg.ConfigureSettingEngine(&se)
	}

	base := webrtc.Configuration{ICEServers: make([]webrtc.ICEServer, 0, len(cfg.ICEServers))}
	for _, s := range cfg.ICEServers {
		base.ICEServers = append(base.ICEServers, webrtc.ICEServer{
			URLs:           s.URLs,
			Username:       s.Username,
			Credential:     s.Credential,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	if cfg.RelayOnly {
		base.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	}
	if cfg.ConfigureConfiguration != nil {
		cfg.ConfigureConfiguration(&base)
	}

	return &Factory{
		api:  webrtc.NewAPI(webrtc.WithSettingEngine(se)),
		base: base,
	}, nil
}

// EventKind identifies a peer event.
type EventKind int

// Peer events.
const (
	// EventLocalCandidate carries one gathered local candidate to trickle.
	EventLocalCandidate EventKind = iota

	// EventGatheringComplete reports end-of-candidates for the local peer.
	EventGatheringComplete

	// EventConnected reports that DTLS and SCTP are up.
	EventConnected

	// EventDisconnected reports lost connectivity that may still recover.
	EventDisconnected

	// EventFailed reports connectivity that cannot recover without a restart.
	EventFailed

	// EventPeerClosed reports that the PeerConnection reached its terminal
	// state.
	EventPeerClosed

	// EventChannelOpen reports that the DataChannel is open and ready to
	// detach.
	EventChannelOpen

	// EventChannelClosed reports that the DataChannel closed before or after
	// opening.
	EventChannelClosed

	// EventError reports an adapter-level failure.
	EventError
)

// String implements [fmt.Stringer].
func (k EventKind) String() string {
	switch k {
	case EventLocalCandidate:
		return "local-candidate"
	case EventGatheringComplete:
		return "gathering-complete"
	case EventConnected:
		return "connected"
	case EventDisconnected:
		return "disconnected"
	case EventFailed:
		return "failed"
	case EventPeerClosed:
		return "peer-closed"
	case EventChannelOpen:
		return "channel-open"
	case EventChannelClosed:
		return "channel-closed"
	case EventError:
		return "error"
	default:
		return "unknown"
	}
}

// Candidate is a transport-neutral ICE candidate.
type Candidate struct {
	Candidate        string
	SDPMid           *string
	SDPMLineIndex    *uint16
	UsernameFragment *string
}

// Event is an immutable report from the adapter to the session.
type Event struct {
	Kind      EventKind
	Candidate Candidate
	Err       error
}

// Sink receives events. It must not block: implementations enqueue the event on
// a bounded mailbox and return.
type Sink func(Event)

// Peer wraps one PeerConnection and its single DataChannel.
type Peer struct {
	pc   *webrtc.PeerConnection
	sink Sink

	mu       sync.Mutex
	dc       *webrtc.DataChannel
	detached bool
	closed   bool
}

// NewOfferer creates a PeerConnection with the outbound DataChannel already
// created, so that the offer describes it.
func (f *Factory) NewOfferer(sink Sink) (*Peer, error) {
	p, err := f.newPeer(sink)
	if err != nil {
		return nil, err
	}

	ordered := true
	protocol := ChannelProtocol
	dc, err := p.pc.CreateDataChannel(ChannelLabel, &webrtc.DataChannelInit{
		Ordered:  &ordered,
		Protocol: &protocol,
		// MaxRetransmits and MaxPacketLifeTime stay unset, which selects
		// reliable delivery.
	})
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("%w: create data channel: %w", ErrNegotiation, err)
	}

	p.mu.Lock()
	p.dc = dc
	p.mu.Unlock()
	p.watchChannel(dc)
	return p, nil
}

// NewAnswerer creates a PeerConnection that waits for the remote DataChannel.
// OnDataChannel is registered before any description is applied.
func (f *Factory) NewAnswerer(sink Sink) (*Peer, error) {
	p, err := f.newPeer(sink)
	if err != nil {
		return nil, err
	}

	p.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if err := validateChannel(dc); err != nil {
			// Close the unexpected channel and report; the session decides how
			// to reject the peer.
			_ = dc.Close()
			p.sink(Event{Kind: EventError, Err: err})
			return
		}

		p.mu.Lock()
		duplicate := p.dc != nil
		if !duplicate {
			p.dc = dc
		}
		p.mu.Unlock()

		if duplicate {
			_ = dc.Close()
			p.sink(Event{Kind: EventError, Err: fmt.Errorf("%w: peer opened a second data channel",
				ErrChannelRejected)})
			return
		}
		p.watchChannel(dc)
	})
	return p, nil
}

func (f *Factory) newPeer(sink Sink) (*Peer, error) {
	if sink == nil {
		return nil, errors.New("pionx: a sink is required")
	}
	pc, err := f.api.NewPeerConnection(f.base)
	if err != nil {
		return nil, fmt.Errorf("%w: create peer connection: %w", ErrNegotiation, err)
	}

	p := &Peer{pc: pc, sink: sink}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			p.sink(Event{Kind: EventGatheringComplete})
			return
		}
		init := c.ToJSON()
		p.sink(Event{Kind: EventLocalCandidate, Candidate: Candidate{
			Candidate:        init.Candidate,
			SDPMid:           init.SDPMid,
			SDPMLineIndex:    init.SDPMLineIndex,
			UsernameFragment: init.UsernameFragment,
		}})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			p.sink(Event{Kind: EventConnected})
		case webrtc.PeerConnectionStateDisconnected:
			p.sink(Event{Kind: EventDisconnected})
		case webrtc.PeerConnectionStateFailed:
			p.sink(Event{Kind: EventFailed})
		case webrtc.PeerConnectionStateClosed:
			p.sink(Event{Kind: EventPeerClosed})
		}
	})

	return p, nil
}

// watchChannel reports the lifecycle of the session's DataChannel.
func (p *Peer) watchChannel(dc *webrtc.DataChannel) {
	dc.OnOpen(func() { p.sink(Event{Kind: EventChannelOpen}) })
	dc.OnClose(func() { p.sink(Event{Kind: EventChannelClosed}) })
	dc.OnError(func(err error) { p.sink(Event{Kind: EventError, Err: err}) })
}

// validateChannel enforces the DataChannel contract on the answering side.
func validateChannel(dc *webrtc.DataChannel) error {
	switch {
	case dc.Label() != ChannelLabel:
		return fmt.Errorf("%w: label %q is not %q", ErrChannelRejected, dc.Label(), ChannelLabel)
	case dc.Protocol() != ChannelProtocol:
		return fmt.Errorf("%w: protocol %q is not %q", ErrChannelRejected, dc.Protocol(), ChannelProtocol)
	case !dc.Ordered():
		return fmt.Errorf("%w: channel is unordered", ErrChannelRejected)
	case dc.MaxRetransmits() != nil:
		return fmt.Errorf("%w: channel limits retransmits", ErrChannelRejected)
	case dc.MaxPacketLifeTime() != nil:
		return fmt.Errorf("%w: channel limits packet lifetime", ErrChannelRejected)
	}
	return nil
}

// CreateOffer creates a local offer and applies it. Gathering continues in the
// background so that the caller can send the offer immediately and trickle
// candidates afterwards. Setting restart requests an ICE restart.
func (p *Peer) CreateOffer(restart bool) (string, error) {
	var opts *webrtc.OfferOptions
	if restart {
		opts = &webrtc.OfferOptions{ICERestart: true}
	}
	offer, err := p.pc.CreateOffer(opts)
	if err != nil {
		return "", fmt.Errorf("%w: create offer: %w", ErrNegotiation, err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("%w: set local offer: %w", ErrNegotiation, err)
	}
	return offer.SDP, nil
}

// SetRemoteOffer applies a remote offer.
func (p *Peer) SetRemoteOffer(sdp string) error {
	err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp})
	if err != nil {
		return fmt.Errorf("%w: set remote offer: %w", ErrNegotiation, err)
	}
	return nil
}

// CreateAnswer creates a local answer and applies it.
func (p *Peer) CreateAnswer() (string, error) {
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("%w: create answer: %w", ErrNegotiation, err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("%w: set local answer: %w", ErrNegotiation, err)
	}
	return answer.SDP, nil
}

// SetRemoteAnswer applies a remote answer.
func (p *Peer) SetRemoteAnswer(sdp string) error {
	err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
	if err != nil {
		return fmt.Errorf("%w: set remote answer: %w", ErrNegotiation, err)
	}
	return nil
}

// HasRemoteDescription reports whether remote candidates can be applied yet.
func (p *Peer) HasRemoteDescription() bool { return p.pc.RemoteDescription() != nil }

// AddCandidate applies one remote candidate. It requires a remote description,
// so callers buffer candidates that arrive earlier.
func (p *Peer) AddCandidate(c Candidate) error {
	err := p.pc.AddICECandidate(webrtc.ICECandidateInit{
		Candidate:        c.Candidate,
		SDPMid:           c.SDPMid,
		SDPMLineIndex:    c.SDPMLineIndex,
		UsernameFragment: c.UsernameFragment,
	})
	if err != nil {
		return fmt.Errorf("%w: add remote candidate: %w", ErrNegotiation, err)
	}
	return nil
}

// EndOfRemoteCandidates reports that the peer will send no further candidates.
func (p *Peer) EndOfRemoteCandidates() error {
	if err := p.pc.AddICECandidate(webrtc.ICECandidateInit{}); err != nil {
		return fmt.Errorf("%w: end of remote candidates: %w", ErrNegotiation, err)
	}
	return nil
}

// Detach hands the opened DataChannel to the stream adapter. It may be called
// at most once and only after [EventChannelOpen].
func (p *Peer) Detach() (Channel, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case p.closed:
		return nil, ErrClosed
	case p.dc == nil:
		return nil, fmt.Errorf("%w: data channel is not open", ErrNegotiation)
	case p.detached:
		return nil, fmt.Errorf("%w: data channel is already detached", ErrNegotiation)
	case p.dc.ReadyState() != webrtc.DataChannelStateOpen:
		return nil, fmt.Errorf("%w: data channel is %s, not open", ErrNegotiation, p.dc.ReadyState())
	}

	ch, err := p.dc.DetachWithDeadline()
	if err != nil {
		return nil, fmt.Errorf("%w: detach data channel: %w", ErrNegotiation, err)
	}
	p.detached = true
	return ch, nil
}

// MaxMessageSize reports the SCTP maximum message size once it is negotiated, or
// zero while it is unknown.
func (p *Peer) MaxMessageSize() uint32 {
	sctp := p.pc.SCTP()
	if sctp == nil {
		return 0
	}
	return sctp.GetCapabilities().MaxMessageSize
}

// SelectedCandidatePair reports the local and remote candidate types of the
// nominated pair, or empty strings while none is selected.
func (p *Peer) SelectedCandidatePair() (local, remote string) {
	sctp := p.pc.SCTP()
	if sctp == nil {
		return "", ""
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return "", ""
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return "", ""
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return "", ""
	}
	return pair.Local.Typ.String(), pair.Remote.Typ.String()
}

// Close closes the PeerConnection and its DataChannel. It is idempotent and is
// safe to call from any failure path.
func (p *Peer) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	dc, detached := p.dc, p.detached
	p.mu.Unlock()

	// A detached channel is owned by the stream adapter, which closes it.
	if dc != nil && !detached {
		_ = dc.Close()
	}
	return p.pc.Close()
}
