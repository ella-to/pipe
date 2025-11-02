package pipe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
	"github.com/rs/xid"

	"ella.to/pipe/signal"
)

var statusFunc atomic.Value

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

func newWebrtcAPI() (*webrtc.API, error) {
	s := webrtc.SettingEngine{}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))

	return api, nil
}

type Conn struct {
	id  string
	sig signal.Signal

	sendInbox    string
	recvInbox    string
	sendInboxSet chan struct{}

	pc      *webrtc.PeerConnection
	raw     io.ReadWriteCloser
	isReady chan struct{}

	closed   chan struct{}
	isClosed atomic.Bool

	candidateMu       sync.Mutex
	pendingCandidates []webrtc.ICECandidateInit
}

var _ io.ReadWriteCloser = (*Conn)(nil)

func (c *Conn) ID() string {
	return c.id
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
	return c.raw.Read(p)
}

func (c *Conn) Write(p []byte) (n int, err error) {
	return c.raw.Write(p)
}

func (c *Conn) Close() error {
	if !c.isClosed.CompareAndSwap(false, true) {
		return nil
	}

	close(c.closed)

	var rawErr error
	if c.raw != nil {
		if err := c.raw.Close(); err != nil {
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
	c.pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		if sigErr := sendMsg(
			context.WithoutCancel(ctx),
			c.sig,
			c.sendInbox,
			signal.CreateMsg(
				signal.TypeCandidate,
				candidate.ToJSON(),
			),
		); sigErr != nil {
			slog.Error("failed to send ice candidate", "id", c.id, "inbox", c.sendInbox, "err", sigErr)
		}
	})

	c.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		callStatusFunc(c, s)

		switch s {
		case webrtc.PeerConnectionStateConnecting:
		case webrtc.PeerConnectionStateConnected:
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateFailed:
			c.Close()
		}
	})
}

func getRandomInbox(id string) string {
	return id + "." + xid.New().String()
}

func getMainInbox(id string) string {
	return id + ".inbox"
}

func sendMsg(ctx context.Context, sig signal.Signal, inbox string, msg *signal.Msg) error {
	// slog.Info("sending signaling message", "inbox", inbox, "type", msg.Type)
	return sig.Send(ctx, inbox, msg)
}

func receiveMsg(ctx context.Context, receiver signal.Receiver) (*signal.Msg, error) {
	msg, err := receiver.Receive(ctx)
	if err != nil {
		return nil, err
	}
	// slog.Info("received signaling message", "type", msg.Type)
	return msg, nil
}

func (c *Conn) incoming() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	receiver, err := c.sig.Receiver(c.recvInbox)
	if err != nil {
		slog.Error("failed to create signaling receiver", "id", c.id, "err", err)
		return
	}

	go func() {
		<-c.closed
		cancel()
	}()

	for {
		msg, err := receiveMsg(ctx, receiver)
		if errors.Is(err, context.Canceled) {
			// it is expected that context is canceled when connection is closed
			// so just return to exit the loop and end the goroutine
			return
		} else if err != nil {
			slog.Error("failed to receive signaling message", "id", c.id, "err", err)
			return
		}

		switch msg.Type {
		case signal.TypeExchange:
			if c.sendInbox != "" {
				// already set
				continue
			}

			if err = msg.DecodeBody(&c.sendInbox); err != nil {
				slog.Error("failed to decode exchange message body", "id", c.id, "err", err)
				continue
			}

			close(c.sendInboxSet)

		case signal.TypeOffer:
			var offer webrtc.SessionDescription
			if err = json.Unmarshal(msg.Body, &offer); err != nil {
				slog.Error("failed to unmarshal offer", "id", c.id, "err", err)
				continue
			}

			if err = c.pc.SetRemoteDescription(offer); err != nil {
				slog.Error("failed to set remote description", "id", c.id, "err", err)
				continue
			}

			c.flushPendingICECandidates()

			answer, answerErr := c.pc.CreateAnswer(nil)
			if answerErr != nil {
				slog.Error("failed to create answer", "id", c.id, "err", answerErr)
				continue
			}

			if err = c.pc.SetLocalDescription(answer); err != nil {
				slog.Error("failed to set local description", "id", c.id, "err", err)
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
				slog.Error("failed to send answer", "id", c.id, "err", err)
				continue
			}

		case signal.TypeAnswer:
			var answer webrtc.SessionDescription
			if err = json.Unmarshal(msg.Body, &answer); err != nil {
				slog.Error("failed to unmarshal answer", "id", c.id, "err", err)
				continue
			}

			if err = c.pc.SetRemoteDescription(answer); err != nil {
				slog.Error("failed to set remote description", "id", c.id, "err", err)
				continue
			}

			c.flushPendingICECandidates()

		case signal.TypeCandidate:
			var candidate webrtc.ICECandidateInit
			if err = json.Unmarshal(msg.Body, &candidate); err != nil {
				slog.Error("failed to unmarshal candidate", "id", c.id, "err", err)
				continue
			}

			if err = c.addICECandidate(candidate); err != nil {
				slog.Error("failed to add ice candidate", "id", c.id, "err", err)
				continue
			}
		default:
			slog.Warn("received unexpected signaling message type", "id", c.id, "type", msg.Type)
		}
	}
}

func (c *Conn) addICECandidate(candidate webrtc.ICECandidateInit) error {
	c.candidateMu.Lock()
	defer c.candidateMu.Unlock()

	if c.pc == nil {
		return errors.New("peer connection is nil")
	}

	if c.pc.CurrentRemoteDescription() == nil {
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

func newConn(sig signal.Signal, id string) *Conn {
	return &Conn{
		id:        id,
		sig:       sig,
		recvInbox: getRandomInbox(id),

		sendInboxSet: make(chan struct{}),
		isReady:      make(chan struct{}),
		closed:       make(chan struct{}),
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
	api, err := newWebrtcAPI()
	if err != nil {
		return nil, err
	}

	// TODO: validate config

	return DialerFunc(func(ctx context.Context, to string) (conn *Conn, err error) {
		conn = newConn(sig, id)

		defer func() {
			if err != nil {
				if cerr := conn.Close(); cerr != nil {
					slog.Error("failed to close connection after dial error", "err", cerr)
				}
			}
		}()

		conn.pc, err = api.NewPeerConnection(defaultConfig)
		if err != nil {
			return nil, err
		}

		{
			var (
				ordered bool = true
				// maxRetransmits uint16              = 3
				dataCh *webrtc.DataChannel = nil
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
				conn.raw = stream
				close(conn.isReady)
			})

			dataCh.OnClose(func() {
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

		// This is completed when the connection is established or fails
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-conn.isReady:
			// connection is ready
		}

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
	api, err := newWebrtcAPI()
	if err != nil {
		return nil, err
	}

	receiver, err := sig.Receiver(getMainInbox(id))
	if err != nil {
		return nil, err
	}

	return ListenerFunc(func(ctx context.Context) (*Conn, error) {
		for {
			msg, err := receiveMsg(ctx, receiver)
			if err != nil {
				return nil, err
			}

			// Expecting an exchange message to start the connection
			if msg.Type != signal.TypeExchange {
				continue
			}

			conn := newConn(sig, id)

			if err = json.Unmarshal(msg.Body, &conn.sendInbox); err != nil {
				slog.Error("failed to unmarshal inbox from exchange message", "err", err)
				continue
			}

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
				slog.Error("failed to send exchange message", "err", err)
				continue
			}

			conn.pc, err = api.NewPeerConnection(defaultConfig)
			if err != nil {
				slog.Error("failed to create peer connection", "err", err)
				continue
			}

			conn.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
				stream := newDataChannelStream(dc)

				dc.OnClose(func() {
					stream.CloseWithError(io.EOF)
					conn.Close()
				})

				dc.OnOpen(func() {
					conn.raw = stream
					close(conn.isReady)
				})
			})

			conn.setupHandlers(ctx)

			go conn.incoming()

			return conn, nil
		}
	}), nil
}
