package pipe

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
	"github.com/xtaci/smux"

	"ella.to/pipe/signal"
)

var statusFunc atomic.Value

type PeerConnectionState = webrtc.PeerConnectionState

type StatusFunc func(conn *Conn, status webrtc.PeerConnectionState)

func SetStatusFunc(f StatusFunc) {
	statusFunc.Store(f)
}

func callStatusFunc(conn *Conn, status webrtc.PeerConnectionState) {
	if f, ok := statusFunc.Load().(StatusFunc); ok && f != nil {
		f(conn, status)
	}
}

var defaultConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{
			URLs: []string{
				"stun:stun.l.google.com:19302",
				"stun:stun1.l.google.com:19302",
				"stun:stun2.l.google.com:19302",
				"stun:stun3.l.google.com:19302",
				"stun:stun4.l.google.com:19302",
			},
		},
	},
}

// TURNServer describes a single TURN/TURNS relay server and its credentials.
// It is a convenience wrapper over webrtc.ICEServer for the common case of
// adding a password-authenticated TURN server.
//
// URLs example values:
//
//	"turn:turn.example.com:3478?transport=udp"
//	"turn:turn.example.com:3478?transport=tcp"
//	"turns:turn.example.com:5349?transport=tcp"
type TURNServer struct {
	URLs       []string
	Username   string
	Credential string
}

// NewTURNServer builds a TURNServer for a single URL with username/password
// credentials.
func NewTURNServer(url, username, credential string) TURNServer {
	return TURNServer{
		URLs:       []string{url},
		Username:   username,
		Credential: credential,
	}
}

func (t TURNServer) iceServer() webrtc.ICEServer {
	return webrtc.ICEServer{
		URLs:           t.URLs,
		Username:       t.Username,
		Credential:     t.Credential,
		CredentialType: webrtc.ICECredentialTypePassword,
	}
}

// WebRTCOptions allows configuring the underlying WebRTC API and ICE servers.
//
// ICEServers: full STUN/TURN server list. When non-empty it REPLACES the
// default STUN servers; when empty the built-in Google STUN servers are used.
//
// TURNServers: TURN relays to ADD on top of ICEServers (or the default STUN
// list). This is the easy way to enable relay support without having to
// re-specify the default STUN servers.
//
// ForceRelay: when true, only relay (TURN) candidates are used — the connection
// will fail rather than fall back to a direct path. Useful for testing that
// your TURN server actually works.
//
// NAT1To1IPs: if running behind NAT, set your public IPs so candidates are rewritten.
type WebRTCOptions struct {
	ICEServers  []webrtc.ICEServer
	TURNServers []TURNServer
	ForceRelay  bool
	// NAT1To1IPs         []string
	ICETransportPolicy          webrtc.ICETransportPolicy // optional (default: all)
	DisconnectSignalOnConnected bool
}

func newWebrtcAPI(opts *WebRTCOptions) (*webrtc.API, error) {
	s := webrtc.SettingEngine{}
	// if opts != nil && len(opts.NAT1To1IPs) > 0 {
	// 	// Advertise public IPs for host candidates when behind NAT
	// 	s.SetNAT1To1IPs(opts.NAT1To1IPs, webrtc.ICECandidateTypeHost)
	// }
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	return api, nil
}

func buildConfiguration(opts *WebRTCOptions) webrtc.Configuration {
	if opts == nil {
		return defaultConfig
	}

	cfg := webrtc.Configuration{}

	// Base list: explicit ICEServers if provided, otherwise the default STUN
	// servers. Append rather than alias so we never mutate defaultConfig.
	if len(opts.ICEServers) > 0 {
		cfg.ICEServers = append(cfg.ICEServers, opts.ICEServers...)
	} else {
		cfg.ICEServers = append(cfg.ICEServers, defaultConfig.ICEServers...)
	}

	// TURN relays are additive on top of the base list.
	for _, t := range opts.TURNServers {
		cfg.ICEServers = append(cfg.ICEServers, t.iceServer())
	}

	switch {
	case opts.ForceRelay:
		cfg.ICETransportPolicy = webrtc.ICETransportPolicyRelay
	case opts.ICETransportPolicy != 0:
		cfg.ICETransportPolicy = opts.ICETransportPolicy
	}

	logICEConfiguration(cfg)

	return cfg
}

// logICEConfiguration emits a one-line summary of the ICE servers in use, which
// is helpful when diagnosing why a connection cannot be established (e.g. no
// TURN server configured, or relay-only with a broken TURN server).
func logICEConfiguration(cfg webrtc.Configuration) {
	urls := make([]string, 0, len(cfg.ICEServers))
	hasTURN := false
	for _, s := range cfg.ICEServers {
		urls = append(urls, s.URLs...)
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
				hasTURN = true
			}
		}
	}

	policy := "all"
	if cfg.ICETransportPolicy == webrtc.ICETransportPolicyRelay {
		policy = "relay-only"
	}

	slog.Debug("ice configuration", "servers", urls, "has_turn", hasTURN, "policy", policy)
}

// ErrMultiplexingActive is returned when Read/Write is called after Stream() has been used.
var ErrMultiplexingActive = errors.New("cannot use Read/Write after Stream() has been called; use the returned streams instead")

// ErrInitialValueTooLarge is returned when stream initial value exceeds max supported size.
var ErrInitialValueTooLarge = errors.New("stream initial value is too large")

// ErrInitialValueOnListener is returned when a listener tries to set an initial value.
var ErrInitialValueOnListener = errors.New("initial value can only be set by stream opener")

// ErrInvalidStreamInitialValueArgs is returned when Stream receives more than one initial value.
var ErrInvalidStreamInitialValueArgs = errors.New("stream accepts at most one initial value argument")

const maxStreamInitialValueSize = 64 * 1024

type Conn struct {
	id                          string
	sig                         signal.Signal
	serverProvidedID            atomic.Value // stores string, set by server's conn_id message
	disconnectSignalOnConnected bool
	signalReceiverMu            sync.Mutex
	signalReceiverCloser        signal.ReceiverCloser

	sendInbox    *signal.Inbox
	recvInbox    *signal.Inbox
	sendInboxSet chan struct{}

	pc      *webrtc.PeerConnection
	raw     io.ReadWriteCloser
	rawMu   sync.RWMutex
	isReady chan struct{}

	closed   chan struct{}
	isClosed atomic.Bool

	candidateMu       sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit

	// multiplexing support
	isDialer      bool          // true if created via Dial, false if via Listen
	sessionMu     sync.Mutex    // guards session creation
	session       *smux.Session // lazily created on first Stream() call
	sessionActive atomic.Bool   // set once session is created; checked by Read/Write
}

var _ io.ReadWriteCloser = (*Conn)(nil)

func (c *Conn) ID() string {
	// If server provided an ID, use that; otherwise use the original ID
	if serverID, ok := c.serverProvidedID.Load().(string); ok && serverID != "" {
		return serverID
	}
	return c.id
}

// role returns "dialer" or "listener" for logging/diagnostics.
func (c *Conn) role() string {
	if c.isDialer {
		return "dialer"
	}
	return "listener"
}

func (c *Conn) IsReady() bool {
	select {
	case <-c.isReady:
		return true
	default:
		return false
	}
}

func (c *Conn) IsClosed() bool {
	return c.isClosed.Load()
}

func (c *Conn) WaitReady(ctx context.Context) error {
	select {
	case <-c.isReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitClosed blocks until the underlying connection has been closed or the
// provided context is canceled.
func (c *Conn) WaitClosed(ctx context.Context) error {
	select {
	case <-c.closed:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Conn) Read(p []byte) (n int, err error) {
	if c.sessionActive.Load() {
		return 0, ErrMultiplexingActive
	}
	raw := c.getRaw()
	if raw == nil {
		return 0, io.ErrClosedPipe
	}
	return raw.Read(p)
}

func (c *Conn) Write(p []byte) (n int, err error) {
	if c.sessionActive.Load() {
		return 0, ErrMultiplexingActive
	}
	raw := c.getRaw()
	if raw == nil {
		return 0, io.ErrClosedPipe
	}
	return raw.Write(p)
}

// Sync blocks until all buffered data has been transmitted to the network layer.
// This is useful after large writes (e.g., io.Copy) to ensure data has left the
// local buffer before proceeding.
//
// Note: This only guarantees the data was sent to the network, not that the remote
// peer received it. For end-to-end confirmation, use application-level acknowledgment.
func (c *Conn) Sync(ctx context.Context) error {
	raw := c.getRaw()
	if raw == nil {
		return nil
	}
	if syncer, ok := raw.(interface{ Sync(context.Context) error }); ok {
		return syncer.Sync(ctx)
	}
	return nil
}

func (c *Conn) setRaw(raw io.ReadWriteCloser) {
	c.rawMu.Lock()
	c.raw = raw
	c.rawMu.Unlock()
}

func (c *Conn) getRaw() io.ReadWriteCloser {
	c.rawMu.RLock()
	raw := c.raw
	c.rawMu.RUnlock()
	return raw
}

func (c *Conn) takeRaw() io.ReadWriteCloser {
	c.rawMu.Lock()
	raw := c.raw
	c.raw = nil
	c.rawMu.Unlock()
	return raw
}

// Stream returns a multiplexed stream over this connection.
//
// On the dialer side (connection created via Dial), this opens a new stream.
// On the listener side (connection created via Listen), this accepts an incoming stream.
//
// The first call to Stream() initializes the multiplexing session. After that,
// direct Read/Write on the Conn will return ErrMultiplexingActive.
//
// Each returned stream is an io.ReadWriteCloser that can be used independently.
// Multiple streams can be created over a single connection.
//
// Optional initial metadata can be passed by the stream opener as the first
// argument. The stream receiver gets that value as the second return value.
//
// Dialer:
//
//	stream, _, _ := conn.Stream([]byte("token:abc"))
//
// Listener:
//
//	stream, init, _ := conn.Stream()
//
// Example:
//
//	// Dialer side
//	conn, _ := dialer.Dial(ctx, "peer")
//	stream1, _, _ := conn.Stream(nil) // opens stream
//	stream2, _, _ := conn.Stream(nil) // opens another stream
//
//	// Listener side
//	conn, _ := listener.Listen(ctx)
//	stream1, init1, _ := conn.Stream() // accepts stream
//	stream2, init2, _ := conn.Stream() // accepts another stream
func (c *Conn) Stream(initialValue ...byte) (io.ReadWriteCloser, []byte, error) {
	if len(initialValue) > maxStreamInitialValueSize {
		return nil, nil, ErrInitialValueTooLarge
	}

	session, err := c.getOrCreateSession()
	if err != nil {
		return nil, nil, err
	}

	if c.isDialer {
		rawStream, err := session.OpenStream()
		if err != nil {
			return nil, nil, err
		}
		if err := writeStreamInitialValue(rawStream, initialValue); err != nil {
			_ = rawStream.Close()
			return nil, nil, err
		}
		return rawStream, nil, nil
	}

	if len(initialValue) > 0 {
		return nil, nil, ErrInitialValueOnListener
	}

	rawStream, err := session.AcceptStream()
	if err != nil {
		return nil, nil, err
	}
	receivedInitialValue, err := readStreamInitialValue(rawStream)
	if err != nil {
		_ = rawStream.Close()
		return nil, nil, err
	}

	return rawStream, receivedInitialValue, nil
}

func writeStreamInitialValue(w io.Writer, initialValue []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(initialValue)))

	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	if len(initialValue) == 0 {
		return nil
	}

	return writeAll(w, initialValue)
}

func readStreamInitialValue(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	size := binary.BigEndian.Uint32(header[:])
	if size > maxStreamInitialValueSize {
		return nil, ErrInitialValueTooLarge
	}
	if size == 0 {
		return nil, nil
	}

	buf := make([]byte, int(size))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}

	return buf, nil
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// getOrCreateSession lazily creates the smux session on first call.
func (c *Conn) getOrCreateSession() (*smux.Session, error) {
	select {
	case <-c.isReady:
	case <-c.closed:
		return nil, io.EOF
	}

	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()

	if c.session != nil {
		return c.session, nil
	}

	raw := c.getRaw()
	if raw == nil {
		return nil, io.ErrClosedPipe
	}

	var err error
	if c.isDialer {
		// Dialer acts as smux client
		c.session, err = smux.Client(raw, nil)
	} else {
		// Listener acts as smux server
		c.session, err = smux.Server(raw, nil)
	}
	if err != nil {
		return nil, err
	}
	c.sessionActive.Store(true)

	return c.session, nil
}

// NumStreams returns the number of currently open streams.
// Returns 0 if multiplexing has not been initialized.
func (c *Conn) NumStreams() int {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.session == nil {
		return 0
	}
	return c.session.NumStreams()
}

func (c *Conn) Close() (retErr error) {
	if c == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			// Guard against rare races around partial initialization or double closes
			slog.Error("panic during Close; ignored", "id", c.id, "panic", r)
			retErr = nil
		}
	}()

	if !c.isClosed.CompareAndSwap(false, true) {
		return nil
	}

	if c.closed != nil {
		close(c.closed)
	}

	// Close smux session first if it exists
	c.sessionMu.Lock()
	if c.session != nil {
		if err := c.session.Close(); err != nil && !isClosedError(err) {
			slog.Error("failed to close smux session", "id", c.id, "err", err)
		}
	}
	c.sessionMu.Unlock()

	var rawErr error
	if raw := c.takeRaw(); raw != nil {
		if err := raw.Close(); err != nil {
			// Suppress error if it's due to the connection already being closed
			// This happens when the peer connection closes before we close the data channel
			if !isClosedError(err) {
				rawErr = err
				slog.Error("failed to close raw data channel", "id", c.id, "err", err)
			}
		}
	}

	var pcErr error
	if c.pc != nil {
		pcErr = c.pc.Close()
	}

	// Return the first non-nil error
	if rawErr != nil {
		return rawErr
	}
	return pcErr
}

// isClosedError checks if the error is due to the connection already being closed
func isClosedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "non-established state") ||
		strings.Contains(errStr, "closed") ||
		strings.Contains(errStr, "Closed")
}

func (c *Conn) setupHandlers(ctx context.Context) {
	role := c.role()

	c.pc.OnICEGatheringStateChange(func(s webrtc.ICEGatheringState) {
		slog.Debug("ice gathering state changed", "id", c.id, "role", role, "state", s.String())
	})

	c.pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		// ICE connection state is the clearest signal of whether the two peers
		// can actually reach each other (e.g. stuck in "checking" usually means
		// no working candidate pair / NAT or STUN/TURN problem).
		slog.Info("ice connection state changed", "id", c.id, "role", role, "state", s.String())
	})

	c.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			slog.Debug("ice candidate gathering finished", "id", c.id, "role", role)
			return
		}

		slog.Debug("sending local ice candidate", "id", c.id, "role", role, "candidate", candidate.String())

		if sigErr := sendMsg(
			context.WithoutCancel(ctx),
			c.sig,
			c.sendInbox,
			signal.CreateMsg(
				signal.TypeCandidate,
				candidate.ToJSON(),
			),
		); sigErr != nil {
			slog.ErrorContext(ctx, "failed to send ice candidate", "id", c.id, "role", role, "inbox", c.sendInbox, "err", sigErr)
		}
	})

	c.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		slog.Info("peer connection state changed", "id", c.id, "role", role, "state", s.String())

		callStatusFunc(c, s)

		switch s {
		case webrtc.PeerConnectionStateConnecting:
		case webrtc.PeerConnectionStateConnected:
			c.logConnectionPath()
			if c.disconnectSignalOnConnected {
				slog.Debug("disconnecting signal receiver after connect", "id", c.id, "role", role)
				c.disconnectSignal()
			}
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateFailed:
			slog.Warn("peer connection lost; closing", "id", c.id, "role", role, "state", s.String())
			c.Close()
		}
	})
}

// logConnectionPath inspects the negotiated ICE candidate pair once the
// connection is established and logs how the data path was formed: a direct
// host path, a NAT-traversed path (server-reflexive via STUN), or a relayed
// path through a TURN server. This answers the common question of whether a
// connection succeeded and whether it required TURN.
func (c *Conn) logConnectionPath() {
	role := c.role()

	sctp := c.pc.SCTP()
	if sctp == nil {
		slog.Debug("connected but path unknown: no SCTP transport", "id", c.id, "role", role)
		return
	}
	dtls := sctp.Transport()
	if dtls == nil {
		slog.Debug("connected but path unknown: no DTLS transport", "id", c.id, "role", role)
		return
	}
	iceT := dtls.ICETransport()
	if iceT == nil {
		slog.Debug("connected but path unknown: no ICE transport", "id", c.id, "role", role)
		return
	}

	pair, err := iceT.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		slog.Warn("connected but could not determine selected candidate pair", "id", c.id, "role", role, "err", err)
		return
	}

	localType := pair.Local.Typ
	remoteType := pair.Remote.Typ

	// A relay candidate on either end means traffic flows through a TURN server.
	requiredTURN := localType == webrtc.ICECandidateTypeRelay || remoteType == webrtc.ICECandidateTypeRelay

	var pathType string
	switch {
	case requiredTURN:
		pathType = "relay (TURN)"
	case localType == webrtc.ICECandidateTypeHost && remoteType == webrtc.ICECandidateTypeHost:
		pathType = "direct (host)"
	default:
		pathType = "direct (NAT traversal)"
	}

	slog.Info("connection established",
		"id", c.id,
		"role", role,
		"path", pathType,
		"required_turn", requiredTURN,
		"protocol", pair.Local.Protocol.String(),
		"local_candidate", localType.String(),
		"local_addr", net.JoinHostPort(pair.Local.Address, strconv.Itoa(int(pair.Local.Port))),
		"remote_candidate", remoteType.String(),
		"remote_addr", net.JoinHostPort(pair.Remote.Address, strconv.Itoa(int(pair.Remote.Port))),
	)
}

func (c *Conn) setSignalReceiverCloser(closer signal.ReceiverCloser) {
	c.signalReceiverMu.Lock()
	c.signalReceiverCloser = closer
	c.signalReceiverMu.Unlock()
}

func (c *Conn) takeSignalReceiverCloser() signal.ReceiverCloser {
	c.signalReceiverMu.Lock()
	closer := c.signalReceiverCloser
	c.signalReceiverCloser = nil
	c.signalReceiverMu.Unlock()
	return closer
}

func (c *Conn) disconnectSignal() {
	closer := c.takeSignalReceiverCloser()
	if closer != nil {
		_ = closer.Close()
	}
}

func getRandomInbox(id string) *signal.Inbox {
	return signal.NewInboxRandom(id)
}

func getMainInbox(id string) *signal.Inbox {
	return signal.NewInboxMain(id)
}

func sendMsg(ctx context.Context, sig signal.Signal, inbox *signal.Inbox, msg *signal.Msg) error {
	return sig.Send(ctx, inbox, msg)
}

func receiveMsg(ctx context.Context, receiver signal.Receiver) (*signal.Msg, error) {
	msg, err := receiver.Receive(ctx)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *Conn) incoming() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	role := c.role()

	receiver, err := c.sig.Receiver(c.recvInbox)
	if err != nil {
		slog.ErrorContext(ctx, "failed to create signaling receiver", "id", c.id, "role", role, "recv_inbox", c.recvInbox, "err", err)
		return
	}
	slog.Debug("signaling receiver started", "id", c.id, "role", role, "recv_inbox", c.recvInbox)
	if closer, ok := receiver.(signal.ReceiverCloser); ok {
		c.setSignalReceiverCloser(closer)
		defer func() {
			if closeErr := c.takeSignalReceiverCloser(); closeErr != nil {
				if err := closeErr.Close(); err != nil && !errors.Is(err, context.Canceled) {
					slog.DebugContext(ctx, "failed to close signaling receiver", "id", c.id, "err", err)
				}
			}
		}()
	}

	go func() {
		<-c.closed
		cancel()
		c.disconnectSignal()
	}()

	for {
		msg, err := receiveMsg(ctx, receiver)
		if errors.Is(err, context.Canceled) {
			// it is expected that context is canceled when connection is closed
			// so just return to exit the loop and end the goroutine
			return
		} else if err != nil {
			slog.ErrorContext(ctx, "failed to receive signaling message", "id", c.id, "role", role, "err", err)
			return
		}

		slog.Debug("received signaling message", "id", c.id, "role", role, "type", msg.Type.String())

		switch msg.Type {
		case 0: // Custom message type - could be connection ID from server
			var payload map[string]string
			if err := msg.DecodeBody(&payload); err == nil {
				if connID, ok := payload["conn_id"]; ok && connID != "" {
					c.serverProvidedID.Store(connID)
					slog.InfoContext(ctx, "received server-provided connection ID", "conn_id", connID, "original_id", c.id)
				}
			}
			continue

		case signal.TypeExchange:
			if c.sendInbox != nil {
				// already set
				continue
			}

			if err = msg.DecodeBody(&c.sendInbox); err != nil {
				slog.ErrorContext(ctx, "failed to decode exchange message body", "id", c.id, "role", role, "err", err)
				continue
			}

			slog.Debug("received remote send inbox via exchange", "id", c.id, "role", role, "send_inbox", c.sendInbox)
			close(c.sendInboxSet)

		case signal.TypeOffer:
			var offer webrtc.SessionDescription
			if err = json.Unmarshal(msg.Body, &offer); err != nil {
				slog.ErrorContext(ctx, "failed to unmarshal offer", "id", c.id, "role", role, "err", err)
				continue
			}

			if err = c.pc.SetRemoteDescription(offer); err != nil {
				slog.ErrorContext(ctx, "failed to set remote description", "id", c.id, "role", role, "err", err)
				continue
			}

			c.flushPendingICECandidates()

			answer, answerErr := c.pc.CreateAnswer(nil)
			if answerErr != nil {
				slog.ErrorContext(ctx, "failed to create answer", "id", c.id, "role", role, "err", answerErr)
				continue
			}

			if err = c.pc.SetLocalDescription(answer); err != nil {
				slog.ErrorContext(ctx, "failed to set local description", "id", c.id, "role", role, "err", err)
				continue
			}

			if err = sendMsg(
				context.WithoutCancel(ctx),
				c.sig,
				c.sendInbox,
				signal.CreateMsg(
					signal.TypeAnswer,
					answer,
				),
			); err != nil {
				slog.ErrorContext(ctx, "failed to send answer", "id", c.id, "role", role, "err", err)
				continue
			}

			slog.Debug("processed offer and sent answer", "id", c.id, "role", role)

		case signal.TypeAnswer:
			var answer webrtc.SessionDescription
			if err = json.Unmarshal(msg.Body, &answer); err != nil {
				slog.ErrorContext(ctx, "failed to unmarshal answer", "id", c.id, "role", role, "err", err)
				continue
			}

			if err = c.pc.SetRemoteDescription(answer); err != nil {
				slog.ErrorContext(ctx, "failed to set remote description", "id", c.id, "role", role, "err", err)
				continue
			}

			c.flushPendingICECandidates()
			slog.Debug("processed answer and set remote description", "id", c.id, "role", role)

		case signal.TypeCandidate:
			var candidate webrtc.ICECandidateInit
			if err = json.Unmarshal(msg.Body, &candidate); err != nil {
				slog.ErrorContext(ctx, "failed to unmarshal candidate", "id", c.id, "role", role, "err", err)
				continue
			}

			if err = c.addICECandidate(candidate); err != nil {
				slog.ErrorContext(ctx, "failed to add ice candidate", "id", c.id, "role", role, "err", err)
				continue
			}
			slog.Debug("added remote ice candidate", "id", c.id, "role", role)
		default:
			slog.Warn("received unexpected signaling message type", "id", c.id, "role", role, "type", msg.Type)
		}
	}
}

func (c *Conn) addICECandidate(candidate webrtc.ICECandidateInit) error {
	c.candidateMu.Lock()
	defer c.candidateMu.Unlock()

	if c.pc == nil {
		return errors.New("peer connection is nil")
	}

	if c.pc.RemoteDescription() == nil {
		c.pendingCandidates = append(c.pendingCandidates, candidate)
		return nil
	}

	return c.pc.AddICECandidate(candidate)
}

func (c *Conn) flushPendingICECandidates() {
	c.candidateMu.Lock()
	defer c.candidateMu.Unlock()

	if c.pc == nil || len(c.pendingCandidates) == 0 {
		return
	}

	for _, candidate := range c.pendingCandidates {
		if err := c.pc.AddICECandidate(candidate); err != nil {
			slog.Error("failed to add pending ice candidate", "id", c.id, "err", err)
		}
	}

	c.pendingCandidates = nil
}

func newConn(sig signal.Signal, id string, isDialer bool) *Conn {
	return &Conn{
		id:        id,
		sig:       sig,
		recvInbox: getRandomInbox(id),

		sendInboxSet: make(chan struct{}),
		isReady:      make(chan struct{}),
		closed:       make(chan struct{}),

		isDialer: isDialer,
	}
}

type Dialer interface {
	Dial(ctx context.Context, to string) (*Conn, error)
}

type DialerFunc func(ctx context.Context, to string) (*Conn, error)

func (f DialerFunc) Dial(ctx context.Context, to string) (*Conn, error) {
	return f(ctx, to)
}

func CreateDialer(id string, sig signal.Signal) (Dialer, error) {
	return CreateDialerWithOptions(id, sig, nil)
}

// CreateDialerWithOptions allows specifying STUN/TURN and NAT options.
func CreateDialerWithOptions(id string, sig signal.Signal, opts *WebRTCOptions) (Dialer, error) {
	api, err := newWebrtcAPI(opts)
	if err != nil {
		return nil, err
	}

	// TODO: validate config

	return DialerFunc(func(ctx context.Context, to string) (conn *Conn, err error) {
		conn = newConn(sig, id, true) // true = dialer
		if opts != nil {
			conn.disconnectSignalOnConnected = opts.DisconnectSignalOnConnected
		}

		slog.Info("dialing peer", "id", conn.id, "to", to, "recv_inbox", conn.recvInbox)

		defer func() {
			if err != nil {
				slog.ErrorContext(ctx, "dial failed", "id", conn.id, "to", to, "err", err)
				if cerr := conn.Close(); cerr != nil {
					slog.ErrorContext(ctx, "failed to close connection after dial error", "id", conn.id, "err", cerr)
				}
			}
		}()

		conn.pc, err = api.NewPeerConnection(buildConfiguration(opts))
		if err != nil {
			return nil, err
		}

		{
			var (
				ordered = true
				// maxRetransmits uint16              = 3
				dataCh *webrtc.DataChannel
			)

			// client side rpc
			dataCh, err = conn.pc.CreateDataChannel("data", &webrtc.DataChannelInit{
				Ordered: &ordered,
				// MaxRetransmits: &maxRetransmits,
			})
			if err != nil {
				return nil, err
			}

			stream := newDataChannelStream(dataCh)

			dataCh.OnOpen(func() {
				slog.Info("data channel open; connection ready", "id", conn.id, "role", "dialer", "to", to)
				conn.setRaw(stream)
				close(conn.isReady)
			})

			dataCh.OnClose(func() {
				slog.Debug("data channel closed", "id", conn.id, "role", "dialer", "to", to)
				stream.CloseWithError(io.EOF)
				conn.Close()
			})
		}

		err = sendMsg(
			ctx,
			conn.sig,
			getMainInbox(to),
			signal.CreateMsg(
				signal.TypeExchange,
				conn.recvInbox,
			),
		)
		if err != nil {
			return nil, err
		}
		slog.Debug("sent exchange; waiting for remote inbox", "id", conn.id, "to", to, "main_inbox", getMainInbox(to))

		conn.setupHandlers(ctx)

		go conn.incoming()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-conn.sendInboxSet:
		}

		offer, err := conn.pc.CreateOffer(nil)
		if err != nil {
			return nil, err
		}

		if err = conn.pc.SetLocalDescription(offer); err != nil {
			return nil, err
		}

		err = sendMsg(
			ctx,
			conn.sig,
			conn.sendInbox,
			signal.CreateMsg(
				signal.TypeOffer,
				offer,
			),
		)
		if err != nil {
			return nil, err
		}
		slog.Debug("sent offer; waiting for connection to become ready", "id", conn.id, "to", to)

		// This is completed when the connection is established or fails
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "dial timed out / canceled before connection ready", "id", conn.id, "to", to, "err", ctx.Err())
			return nil, ctx.Err()
		case <-conn.isReady:
			// connection is ready
		}

		slog.Info("dial completed", "id", conn.ID(), "to", to)
		return conn, nil
	}), nil
}

type Listener interface {
	Listen(ctx context.Context) (*Conn, error)
}

type ListenerFunc func(ctx context.Context) (*Conn, error)

func (f ListenerFunc) Listen(ctx context.Context) (*Conn, error) {
	return f(ctx)
}

func CreateListener(id string, sig signal.Signal) (Listener, error) {
	return CreateListenerWithOptions(id, sig, nil)
}

// CreateListenerWithOptions allows specifying STUN/TURN and NAT options.
func CreateListenerWithOptions(id string, sig signal.Signal, opts *WebRTCOptions) (Listener, error) {
	api, err := newWebrtcAPI(opts)
	if err != nil {
		return nil, err
	}

	var (
		listenMu       sync.Mutex
		receiverMu     sync.Mutex
		mainReceiver   signal.Receiver
		receiverCloser signal.ReceiverCloser
	)

	getOrCreateReceiver := func() (signal.Receiver, error) {
		receiverMu.Lock()
		defer receiverMu.Unlock()

		if mainReceiver != nil {
			return mainReceiver, nil
		}

		receiver, err := sig.Receiver(getMainInbox(id))
		if err != nil {
			return nil, err
		}

		mainReceiver = receiver
		receiverCloser = nil
		if closer, ok := receiver.(signal.ReceiverCloser); ok {
			receiverCloser = closer
		}

		return mainReceiver, nil
	}

	resetReceiver := func(closeReceiver bool) {
		receiverMu.Lock()
		closer := receiverCloser
		mainReceiver = nil
		receiverCloser = nil
		receiverMu.Unlock()

		if closeReceiver && closer != nil {
			if err := closer.Close(); err != nil && !errors.Is(err, context.Canceled) {
				slog.Debug("failed to close listener signaling receiver", "id", id, "err", err)
			}
		}
	}

	return ListenerFunc(func(ctx context.Context) (*Conn, error) {
		listenMu.Lock()
		defer listenMu.Unlock()

		receiver, err := getOrCreateReceiver()
		if err != nil {
			return nil, err
		}

		// Reset the shared receiver when the context is canceled so that an
		// in-flight receiveMsg unblocks. AfterFunc avoids parking a goroutine
		// per Listen call, which previously accumulated one stack per
		// accepted connection under a long-lived context.
		context.AfterFunc(ctx, func() {
			resetReceiver(true)
		})

		var serverProvidedConnID string

		for {
			msg, err := receiveMsg(ctx, receiver)
			if err != nil {
				if ctx.Err() != nil {
					return nil, err
				}

				slog.WarnContext(ctx, "listener signaling receiver failed; recreating", "id", id, "err", err)
				resetReceiver(true)

				receiver, err = getOrCreateReceiver()
				if err != nil {
					return nil, err
				}

				continue
			}

			// Check for server-provided connection ID (type 0 message)
			if msg.Type == 0 {
				var payload map[string]string
				if err := msg.DecodeBody(&payload); err == nil {
					if connID, ok := payload["conn_id"]; ok && connID != "" {
						serverProvidedConnID = connID
						slog.DebugContext(ctx, "listener received server-provided connection ID", "conn_id", connID)
					}
				}
				continue
			}

			// Expecting an exchange message to start the connection
			if msg.Type != signal.TypeExchange {
				continue
			}

			conn := newConn(sig, id, false) // false = listener
			if opts != nil {
				conn.disconnectSignalOnConnected = opts.DisconnectSignalOnConnected
			}

			// Set the server-provided ID if we received one
			if serverProvidedConnID != "" {
				conn.serverProvidedID.Store(serverProvidedConnID)
			}

			if err = json.Unmarshal(msg.Body, &conn.sendInbox); err != nil {
				slog.ErrorContext(ctx, "failed to unmarshal inbox from exchange message", "id", id, "err", err)
				continue
			}

			slog.Info("accepted incoming exchange from peer", "id", conn.id, "send_inbox", conn.sendInbox, "recv_inbox", conn.recvInbox)

			err = sendMsg(
				ctx,
				conn.sig,
				conn.sendInbox,
				signal.CreateMsg(
					signal.TypeExchange,
					conn.recvInbox,
				),
			)
			if err != nil {
				slog.ErrorContext(ctx, "failed to send exchange message", "id", conn.id, "err", err)
				continue
			}

			conn.pc, err = api.NewPeerConnection(buildConfiguration(opts))
			if err != nil {
				slog.ErrorContext(ctx, "failed to create peer connection", "id", conn.id, "err", err)
				continue
			}

			conn.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
				stream := newDataChannelStream(dc)

				dc.OnClose(func() {
					slog.Debug("data channel closed", "id", conn.id, "role", "listener")
					stream.CloseWithError(io.EOF)
					conn.Close()
				})

				dc.OnOpen(func() {
					slog.Info("data channel open; connection ready", "id", conn.id, "role", "listener")
					conn.setRaw(stream)
					close(conn.isReady)
				})
			})

			conn.setupHandlers(ctx)

			go conn.incoming()

			return conn, nil
		}
	}), nil
}
