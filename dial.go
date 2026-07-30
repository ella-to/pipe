package pipe

import (
	"context"
	"net"
)

// Dial creates a single-use endpoint, connects to peer, and returns the
// connection. Closing the returned connection also closes the hidden endpoint.
//
// Use [New] and [Endpoint.Dial] when an application makes more than one
// connection: one endpoint can serve many sessions over a single signaling
// subscription.
func Dial(ctx context.Context, cfg Config, peer PeerID) (net.Conn, error) {
	ep, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}

	conn, err := ep.Dial(ctx, peer)
	if err != nil {
		_ = ep.Close()
		return nil, err
	}
	return &ownedConn{Conn: conn, ep: ep}, nil
}

// Listen creates a single-use endpoint and returns its listener. Closing the
// returned listener also closes the hidden endpoint, which closes every
// connection that came from it.
func Listen(ctx context.Context, cfg Config) (net.Listener, error) {
	ep, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}

	ln, err := ep.Listen()
	if err != nil {
		_ = ep.Close()
		return nil, err
	}
	return &ownedListener{Listener: ln, ep: ep}, nil
}

// ownedConn ties the lifetime of a hidden endpoint to one connection.
type ownedConn struct {
	*Conn
	ep *Endpoint
}

// Close closes the connection and then the endpoint that owns it.
func (c *ownedConn) Close() error {
	err := c.Conn.Close()
	if cerr := c.ep.Close(); err == nil {
		err = cerr
	}
	return err
}

// ownedListener ties the lifetime of a hidden endpoint to one listener.
type ownedListener struct {
	net.Listener
	ep *Endpoint
}

// Close closes the listener and then the endpoint that owns it, which closes
// every connection accepted from this listener.
func (l *ownedListener) Close() error {
	err := l.Listener.Close()
	if cerr := l.ep.Close(); err == nil {
		err = cerr
	}
	return err
}
