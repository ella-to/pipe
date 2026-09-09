// package pipe exposes reliable WebRTC DataChannels through APIs that feel
// like standard Go networking.
//
// The library hides SDP, ICE, DTLS, SCTP, and Pion's callback-driven API behind
// an [Endpoint] that dials and accepts connections satisfying [net.Conn].
//
// An endpoint owns one signaling subscription and may serve many concurrent
// inbound and outbound sessions:
//
//	ep, err := pipe.New(ctx, pipe.Config{
//		ID:       "alice",
//		Signaler: signaler,
//		ICEServers: []pipe.ICEServer{
//			{URLs: []string{"stun:stun.example.net:3478"}},
//		},
//	})
//	if err != nil {
//		return err
//	}
//	defer ep.Close()
//
//	conn, err := ep.Dial(ctx, "bob")
//	if err != nil {
//		return err
//	}
//	defer conn.Close()
//
// Accepting connections mirrors [net.Listener]:
//
//	ln, err := ep.Listen()
//	if err != nil {
//		return err
//	}
//	for {
//		conn, err := ln.Accept()
//		if err != nil {
//			return err
//		}
//		go handle(conn)
//	}
//
// # Signaling
//
// Pipe does not mandate a signaling service. Applications provide a
// [Signaler] that transports the versioned [Signal] envelope; pipe owns
// negotiation semantics and the transport only moves envelopes.
//
// Two transports are included. The signaling/sse package is an HTTP transport
// (Server-Sent Events to receive, POST to send) with an authenticating server
// that binds every signal to the credential that sent it, which is what peers
// on different machines use. The signaling/memory package is an in-process hub
// for tests and same-process examples. The signaling/signalertest package is a
// conformance suite for transports of your own.
//
// # Guides
//
// The guides directory of the repository covers running the signaling server,
// STUN, a TURN relay with per-user budgets, security, Docker, and operations.
//
// # Limitations
//
//   - A peer ID identifies a routing destination. Whether it is also an
//     authenticated party depends on the signaling transport: signaling/sse
//     authenticates it, and [Config.AllowPeer] can then act as an access-control
//     list.
//   - Protocol version 1 carries one reliable, ordered DataChannel per
//     connection and has no half-close.
//   - Recovery covers signaling reconnects and ICE restarts. If recovery would
//     require a brand-new PeerConnection, the existing connection closes with
//     [ErrDisconnected] rather than silently losing or reordering bytes.
package pipe
