package edgekit

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"time"
)

// serverListener gives each accepted connection to the server as a
// serverConn.
type serverListener struct {
	net.Listener

	server *Server
}

// Accept wraps the accepted connection.
func (l serverListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &serverConn{
		Conn:   conn,
		server: l.server,
	}, nil
}

// serverConn is an accepted connection. It is below TLS, so its deadlines
// apply to network bytes and a TLS layer never sees a changed write. Until
// the gateway takes it for a protocol switch, it only records the deadlines
// that others set. After that, it applies the transfer idle deadlines.
type serverConn struct {
	net.Conn

	server *Server

	// upgraded is true after the gateway took the connection for a protocol
	// switch.
	upgraded atomic.Bool

	// readDeadline and writeDeadline are the deadlines that others set, in
	// Unix nanoseconds, or zero for none. An upgraded connection keeps them
	// as limits.
	readDeadline  atomic.Int64
	writeDeadline atomic.Int64
}

// Read reads with a read deadline on an upgraded connection. A write that
// sends data extends the deadline of a waiting read, so a quiet direction
// does not stop a connection that is active in the other direction.
func (c *serverConn) Read(p []byte) (int, error) {
	if idle := c.server.transferIdle; idle != 0 && c.upgraded.Load() {
		_ = c.Conn.SetReadDeadline(earliest(time.Now().Add(idle), c.readDeadline.Load()))
	}

	return c.Conn.Read(p)
}

// Write writes in parts with a write deadline for each part on an upgraded
// connection.
func (c *serverConn) Write(b []byte) (int, error) {
	idle := c.server.transferIdle
	if idle == 0 || !c.upgraded.Load() {
		return c.Conn.Write(b)
	}

	written := 0
	for {
		part := b[:min(len(b), transferChunk)]
		_ = c.Conn.SetWriteDeadline(earliest(time.Now().Add(idle), c.writeDeadline.Load()))
		n, err := c.Conn.Write(part)

		written += n
		if n > 0 {
			_ = c.Conn.SetReadDeadline(earliest(time.Now().Add(idle), c.readDeadline.Load()))
		}

		b = b[len(part):]
		if err != nil || len(b) == 0 {
			return written, err
		}
	}
}

// SetDeadline records and sets both deadlines.
func (c *serverConn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(unixNano(t))
	c.writeDeadline.Store(unixNano(t))

	return c.Conn.SetDeadline(t)
}

// SetReadDeadline records and sets the read deadline.
func (c *serverConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(unixNano(t))

	return c.Conn.SetReadDeadline(t)
}

// SetWriteDeadline records and sets the write deadline.
func (c *serverConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.Store(unixNano(t))

	return c.Conn.SetWriteDeadline(t)
}

// CloseWrite keeps the half-close of a TCP connection, which net/http uses
// to close a connection without losing the response. It returns
// [errors.ErrUnsupported] when the connection cannot half-close.
func (c *serverConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}

	return errors.ErrUnsupported
}

// Close closes the connection and removes it from the upgraded connections.
func (c *serverConn) Close() error {
	if c.upgraded.Load() {
		c.server.forget(c)
	}

	return c.Conn.Close()
}

// unixNano returns t in Unix nanoseconds, or zero for the zero time.
func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}

	return t.UnixNano()
}

// earliest returns the earlier of deadline and limit, a time in Unix
// nanoseconds where zero means no limit.
func earliest(deadline time.Time, limit int64) time.Time {
	if limit != 0 && limit < deadline.UnixNano() {
		return time.Unix(0, limit)
	}

	return deadline
}

// serverConnKey is the context key of the *serverConn of a request.
type serverConnKey struct{}

// connContext gives each request access to its server connection, so the
// gateway can hand over a connection that it took for a protocol switch.
func connContext(ctx context.Context, c net.Conn) context.Context {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}

	if sc, ok := c.(*serverConn); ok {
		return context.WithValue(ctx, serverConnKey{}, sc)
	}

	return ctx
}

// adoptUpgraded gives conn to the server that accepted it, after the gateway
// took it for a protocol switch. It does nothing when no Server accepted
// conn, such as for a gateway in another server, or when conn is not the
// connection of the request.
func adoptUpgraded(ctx context.Context, conn net.Conn) {
	sc, ok := ctx.Value(serverConnKey{}).(*serverConn)
	if !ok {
		return
	}

	if tc, ok := conn.(*tls.Conn); ok {
		conn = tc.NetConn()
	}

	if conn != net.Conn(sc) {
		return
	}

	sc.server.track(sc)
}
