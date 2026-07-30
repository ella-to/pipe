package pipe

import (
	"net"
	"sync"
)

// pendingConn is an established inbound connection waiting to be accepted,
// together with the function that returns its backlog slot.
type pendingConn struct {
	conn    *Conn
	release func()
}

// listener accepts inbound sessions for an endpoint. It satisfies
// [net.Listener].
type listener struct {
	ep     *Endpoint
	addr   Addr
	accept chan pendingConn

	closeOnce sync.Once
	closed    chan struct{}
}

var _ net.Listener = (*listener)(nil)

// Accept returns the next fully negotiated connection. Offers that are still
// negotiating never appear here.
func (l *listener) Accept() (net.Conn, error) {
	for {
		select {
		case p := <-l.accept:
			if p.release != nil {
				p.release()
			}
			l.ep.cfg.Metrics.Count(metricAcceptResults, 1, labelResult("success"))
			return p.conn, nil

		case <-l.closed:
			// Drain anything that arrived before the close so those
			// connections are not leaked.
			l.drain()
			return nil, l.opErr("accept", errorf(ErrClosed, "pipe: listener is closed"))

		case <-l.ep.ctx.Done():
			l.drain()
			return nil, l.opErr("accept", errorf(ErrClosed, "pipe: endpoint is closed"))
		}
	}
}

// Close stops accepting new sessions. Connections already returned by Accept stay
// open; closing the endpoint closes those too. Close is idempotent.
func (l *listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.ep.clearListener(l)
		l.drain()
	})
	return nil
}

// Addr returns the endpoint's logical address.
func (l *listener) Addr() net.Addr { return l.addr }

// push enqueues an established connection. The accept queue can never overflow
// because the backlog semaphore admits at most one inbound session per slot.
func (l *listener) push(p pendingConn) error {
	select {
	case <-l.closed:
		return errorf(ErrClosed, "pipe: listener is closed")
	default:
	}

	select {
	case l.accept <- p:
		return nil
	case <-l.closed:
		return errorf(ErrClosed, "pipe: listener is closed")
	default:
		return errorf(ErrClosed, "pipe: accept queue is full")
	}
}

func (l *listener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

// drain closes connections that were established but never accepted.
func (l *listener) drain() {
	for {
		select {
		case p := <-l.accept:
			if p.release != nil {
				p.release()
			}
			_ = p.conn.Close()
		default:
			return
		}
	}
}

func (l *listener) opErr(op string, err error) error {
	return opError(op, l.addr, nil, err)
}
