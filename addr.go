package pipe

import "net"

// networkName is reported by every [net.Addr] and [net.OpError] produced by
// this package.
const networkName = "webrtc"

// Addr is the logical address of one side of a pipe connection. Pipe has no
// IP-level identity of its own: a peer is named by its [PeerID] and a session
// is named by its identifier.
type Addr struct {
	// Peer is the peer ID of the addressed side.
	Peer PeerID

	// Session is the session identifier, or the empty string for an address
	// that is not bound to a session (for example a listener address).
	Session string
}

var _ net.Addr = Addr{}

// Network returns "webrtc".
func (a Addr) Network() string { return networkName }

// String returns the peer ID, suffixed with "#" and the session ID when the
// address belongs to a session.
func (a Addr) String() string {
	if a.Session == "" {
		return string(a.Peer)
	}
	return string(a.Peer) + "#" + a.Session
}
