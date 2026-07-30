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
// Pipe does not ship a mandatory signaling service. Applications provide a
// [Signaler] that transports the versioned [Signal] envelope over WebSocket,
// SSE, NATS, Redis, MQTT, or anything else. Pipe owns negotiation semantics;
// the transport only moves envelopes.
//
// The signaling/memory package provides an in-process hub for tests and
// same-process examples.
//
// # Limitations
//
//   - A peer ID identifies a routing destination, not an authenticated party.
//     Authentication belongs to the signaling trust model.
//   - Protocol version 1 carries one reliable, ordered DataChannel per
//     connection and has no half-close.
//   - Recovery covers signaling reconnects and ICE restarts. If recovery would
//     require a brand-new PeerConnection, the existing connection closes with
//     [ErrDisconnected] rather than silently losing or reordering bytes.
package pipe
