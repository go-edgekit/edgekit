package edgekit

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-edgekit/edgekit/internal/safelog"
)

// NoTimeout disables a timeout in [ServerConfig]. It is the only negative
// duration that ServerConfig accepts, so a disabled timeout is always an
// explicit choice that is visible in the code.
const NoTimeout time.Duration = -1

// The secure defaults of the server (ADR-0012, R8 and R9).
const (
	defaultHeaderReadTimeout                  = 10 * time.Second
	defaultMaxHeaderBytes                     = 64 << 10
	defaultKeepAliveTimeout                   = 60 * time.Second
	defaultTransferIdleTimeout                = 300 * time.Second
	defaultHTTP2MaxConcurrentStreams          = 100
	defaultHTTP2MaxReceiveBufferPerStream     = 64 << 10
	defaultHTTP2MaxReceiveBufferPerConnection = 1 << 20
)

// http2MinConnectionWindow is the initial connection window of RFC 9113,
// section 6.9.2. net/http ignores a smaller connection buffer.
const http2MinConnectionWindow = 65535

// ServerConfig is the configuration of a [Server]. The zero value is valid
// and applies all secure defaults.
//
// For each field, zero means the secure default and a positive value
// replaces it. A timeout field also accepts [NoTimeout], which disables the
// timeout. [NewServer] rejects all other negative values, so an accidental
// negative calculation cannot disable a protection.
type ServerConfig struct {
	// TLSConfig enables TLS when it is not nil. The server then serves
	// HTTP/1.1 and HTTP/2 and selects the protocol with ALPN. Without TLS, the
	// server serves HTTP/1.1 only. The configuration must give a certificate:
	// Certificates, GetCertificate, or GetConfigForClient.
	//
	// [NewServer] copies the configuration with [tls.Config.Clone], and the
	// server adds the ALPN protocols that it needs to the copy. Later changes
	// to the original have no effect. The copy shares what Clone shares, such
	// as the certificates and the callbacks: they must stay valid, and the
	// callbacks must be safe for concurrent use, for the life of the server.
	// The caller owns the certificates and the other TLS settings, such as
	// the minimum version.
	TLSConfig *tls.Config

	// HeaderReadTimeout limits the time to read the request headers of an
	// HTTP/1.1 request. The default is 10s. It also limits the TLS handshake,
	// for HTTP/1.1 and HTTP/2, because net/http uses the same value for both.
	// It does not limit the headers of each HTTP/2 stream.
	HeaderReadTimeout time.Duration

	// MaxHeaderBytes limits the size of the request headers. The default is
	// 64 KiB. net/http measures the size with the accounting of each
	// protocol, so it is not an exact limit of wire bytes: HTTP/1.1 reads up
	// to 4096 bytes more than the limit before it rejects a request, and
	// HTTP/2 measures the decoded header list (RFC 9113, section 6.5.2),
	// with 32 bytes for each field. A request over the limit gets 431
	// Request Header Fields Too Large before the gateway runs, so no
	// middleware, hook, or completion runs for it.
	MaxHeaderBytes int

	// KeepAliveTimeout limits the time that an idle connection waits for its
	// next request. The default is 60s.
	KeepAliveTimeout time.Duration

	// TransferIdleTimeout limits the time that a body transfer can make no
	// progress. The default is 300s. See [Server] for the rules. It applies
	// to each request and to each upgraded connection.
	//
	// The server also uses it as the HTTP/2 connection write timeout
	// ([http.HTTP2Config.WriteByteTimeout]): an HTTP/2 connection that can
	// write no data for this time is closed, with all its streams. This
	// protection is for the connection, not for one request, so a request
	// that takes over its write deadline does not change it. NoTimeout
	// disables both protections.
	TransferIdleTimeout time.Duration

	// HTTP2MaxConcurrentStreams limits the open streams of one HTTP/2
	// connection. The default is 100.
	HTTP2MaxConcurrentStreams int

	// HTTP2MaxReceiveBufferPerStream is the HTTP/2 flow-control window of
	// one stream, in bytes. It limits the request body data that a client can
	// send before the gateway reads it. The default is 64 KiB. The maximum is
	// 2^31-1. A small window limits the upload speed of one stream to about
	// one window for each round trip.
	HTTP2MaxReceiveBufferPerStream int

	// HTTP2MaxReceiveBufferPerConnection is the HTTP/2 flow-control window of
	// one connection, for all its streams together, in bytes. The default is
	// 1 MiB. It must be 65535 or more, and at most 2^31-1.
	HTTP2MaxReceiveBufferPerConnection int
}

// Server serves a [Gateway] with the server-level secure defaults
// (ADR-0016). It is the supported way to serve a gateway. Make it with
// [NewServer]. Its methods are safe for concurrent use.
//
// A gateway that another [http.Server] serves gets none of these
// protections. Then the header limits, the timeouts, and the handling of
// upgraded connections are the responsibility of that server.
//
// # Transfer idle deadlines
//
// The server applies [ServerConfig.TransferIdleTimeout] to each request with
// [http.ResponseController] deadlines:
//
//   - Before each read and close of the request body, it sets a read
//     deadline of the timeout from now. When the call returns, it removes
//     the deadline, so a request that waits for the upstream is not limited.
//   - It writes the response in parts of at most 16 KiB, and sets a write
//     deadline of the timeout from now before each part. So a large write
//     that makes progress continues, and a transfer that sends less than
//     16 KiB in the timeout stops. On HTTP/1.1 without full duplex, net/http
//     can read the unread request body before it writes the response
//     headers. So during a write, the server also sets a read deadline when
//     no read of the body is in progress, and gives the write one more
//     timeout. A write never changes the deadline of a read in progress, so
//     response progress cannot hide a stalled upload.
//   - When the handler returns, it sets a write deadline again, so the final
//     write of the response is limited. On HTTP/1.1, it also sets a read
//     deadline, so net/http cannot wait without limit when it reads the
//     unread request body to reuse the connection.
//
// On HTTP/2, these deadlines apply to one stream: a stalled stream does not
// stop the other streams of its connection. The connection itself has a
// separate write timeout (see [ServerConfig.TransferIdleTimeout]).
//
// A handler that calls SetReadDeadline or SetWriteDeadline of
// [http.ResponseController] takes over that direction for the rest of the
// request, when the call succeeds. Then the server does not set or remove
// deadlines for that direction, also not when the handler returns. The zero
// time means no deadline. A route middleware can use this to relax the
// deadline for a streaming route, or to make it stricter. A call that fails
// does not change the protection.
//
// The response writer keeps the optional interfaces of the writer of
// net/http that the server can support with deadlines: [http.Flusher],
// [http.Hijacker] (HTTP/1.1 only), and [http.Pusher] (HTTP/2 only), and the
// methods that [http.ResponseController] uses. It does not have ReadFrom,
// so io.Copy writes through the deadlines. Set deadlines only with
// [http.ResponseController] on this writer: a deadline that code sets on
// the writer that Unwrap returns is not a takeover, and the server can
// remove it.
//
// The server does not use total read or write timeouts, because they stop
// long uploads and streams that make progress. It does not buffer bodies and
// starts no goroutines and no timers of its own for a request.
//
// # Upgraded connections
//
// When the gateway accepts a protocol switch (101 Switching Protocols), the
// proxy takes the connection from net/http. [http.Server] then does not
// apply its timeouts to the connection, and it does not close the connection
// at shutdown. This server does both for each connection that the gateway
// itself took for a protocol switch. A read times out when the connection
// moved no data in either direction for TransferIdleTimeout. A write sends
// at most 16 KiB for each deadline, as for responses. The server never
// manages a connection that middleware or another component took with
// Hijack, because that component owns it.
//
// # Shutdown
//
// [Server.Shutdown] stops the listeners, closes the upgraded connections
// immediately, and waits for the active requests. A protocol switch that
// completes during the shutdown is closed immediately. [Server.Close] stops
// everything immediately.
//
// # Diagnostics
//
// Messages of net/http, such as TLS handshake failures, request parse
// failures, and recovered panics, go to the default [log/slog] logger as
// events without details: no request data, no addresses, and no error text.
// A request that net/http rejects before the gateway runs, such as for
// headers that are too large, makes no gateway context and runs no
// completion.
type Server struct {
	http         *http.Server
	secure       bool
	gateway      *Gateway
	transferIdle time.Duration // Zero when disabled.
	base         context.Context
	cancelBase   context.CancelFunc

	mu sync.Mutex

	// closing is true after Shutdown or Close started.
	closing bool

	// upgraded are the connections that the gateway took for a protocol
	// switch and that are not closed yet.
	upgraded map[*serverConn]struct{}

	// active counts the handler calls that did not return.
	active int

	// drained is closed when active becomes zero. It is nil when no caller
	// waits.
	drained chan struct{}
}

// NewServer makes a server for g with the configuration cfg. It does not
// listen; call [Server.Serve] to accept connections.
//
// If cfg is not valid, NewServer returns an error that wraps
// [ErrInvalidConfiguration], with one wrapped error for each problem. The
// errors identify the field, but do not contain its value.
func NewServer(g *Gateway, cfg ServerConfig) (*Server, error) {
	problems := checkServerConfig(cfg)
	if g == nil {
		problems = append(problems, invalid("server: gateway is nil"))
	}

	if len(problems) != 0 {
		return nil, errors.Join(problems...)
	}

	s := &Server{
		gateway:      g,
		secure:       cfg.TLSConfig != nil,
		transferIdle: timeoutValue(cfg.TransferIdleTimeout, defaultTransferIdleTimeout),
		upgraded:     make(map[*serverConn]struct{}),
	}
	s.base, s.cancelBase = context.WithCancel(context.Background())

	s.http = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		TLSConfig:         cfg.TLSConfig.Clone(),
		ReadHeaderTimeout: timeoutValue(cfg.HeaderReadTimeout, defaultHeaderReadTimeout),
		IdleTimeout:       timeoutValue(cfg.KeepAliveTimeout, defaultKeepAliveTimeout),
		MaxHeaderBytes:    cmp.Or(cfg.MaxHeaderBytes, defaultMaxHeaderBytes),
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams:          cmp.Or(cfg.HTTP2MaxConcurrentStreams, defaultHTTP2MaxConcurrentStreams),
			MaxReceiveBufferPerStream:     cmp.Or(cfg.HTTP2MaxReceiveBufferPerStream, defaultHTTP2MaxReceiveBufferPerStream),
			MaxReceiveBufferPerConnection: cmp.Or(cfg.HTTP2MaxReceiveBufferPerConnection, defaultHTTP2MaxReceiveBufferPerConnection),
			WriteByteTimeout:              s.transferIdle,
		},
		ErrorLog: safelog.New("http_server"),
		BaseContext: func(net.Listener) context.Context {
			return s.base
		},
		ConnContext: connContext,
		ConnState:   s.connState,
	}

	return s, nil
}

// checkServerConfig reports each field that is not valid, without its value.
func checkServerConfig(cfg ServerConfig) []error {
	var problems []error

	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{
			name:  "header read timeout",
			value: cfg.HeaderReadTimeout,
		},
		{
			name:  "keep-alive timeout",
			value: cfg.KeepAliveTimeout,
		},
		{
			name:  "transfer idle timeout",
			value: cfg.TransferIdleTimeout,
		},
	} {
		if field.value < 0 && field.value != NoTimeout {
			problems = append(problems, invalid("server: %s: is negative and is not NoTimeout", field.name))
		}
	}

	for _, field := range []struct {
		name     string
		value    int
		min, max int
	}{
		{
			name:  "max header bytes",
			value: cfg.MaxHeaderBytes,
			min:   1,
			max:   math.MaxInt,
		},
		{
			name:  "HTTP/2 max concurrent streams",
			value: cfg.HTTP2MaxConcurrentStreams,
			min:   1,
			max:   min(math.MaxInt, math.MaxUint32),
		},
		{
			name:  "HTTP/2 max receive buffer per stream",
			value: cfg.HTTP2MaxReceiveBufferPerStream,
			min:   1,
			max:   math.MaxInt32,
		},
		{
			name:  "HTTP/2 max receive buffer per connection",
			value: cfg.HTTP2MaxReceiveBufferPerConnection,
			min:   http2MinConnectionWindow,
			max:   math.MaxInt32,
		},
	} {
		// Zero selects the default. net/http silently replaces a value out
		// of range with its own default, so such a value is an error here.
		if field.value != 0 && (field.value < field.min || field.value > field.max) {
			problems = append(problems, invalid("server: %s: is out of range", field.name))
		}
	}

	if c := cfg.TLSConfig; c != nil && len(c.Certificates) == 0 && c.GetCertificate == nil && c.GetConfigForClient == nil {
		problems = append(problems, invalid("server: TLS config: has no certificate"))
	}

	return problems
}

// timeoutValue maps a valid configured timeout to the value for net/http,
// where zero means no timeout.
func timeoutValue(configured, fallback time.Duration) time.Duration {
	switch configured {
	case 0:
		return fallback
	case NoTimeout:
		return 0
	default:
		return configured
	}
}

// Serve accepts connections on l and serves the gateway on them. It blocks
// until the listener fails or [Server.Shutdown] or [Server.Close] is called.
// Serve takes ownership of l and closes it when it returns. After Shutdown
// or Close, it returns [http.ErrServerClosed]. Otherwise it returns the error
// of the listener, without changes.
//
// Serve can run more than one time, also concurrently, with different
// listeners. Shutdown and Close stop all of them.
//
// With TLS, Serve returns the error of the TLS setup of net/http before it
// accepts a connection, such as when [ServerConfig.TLSConfig] sets
// CipherSuites without a cipher that HTTP/2 requires. Serve also closes l
// then.
func (s *Server) Serve(l net.Listener) error {
	// net/http does not close l when its TLS setup fails.
	defer func() { _ = l.Close() }()

	sl := serverListener{
		Listener: l,
		server:   s,
	}

	// Not s.http.TLSConfig: net/http sets it also for a server without TLS.
	if s.secure {
		// NewServer checked that the TLS configuration has a certificate,
		// so ServeTLS needs no files.
		return s.http.ServeTLS(sl, "", "")
	}

	return s.http.Serve(sl)
}

// Shutdown stops the server gracefully. It closes the listeners, closes the
// upgraded connections before it waits, and closes idle connections. Then it
// waits for the active requests to complete, also for the handlers of
// upgraded connections, so completion ran for all requests when Shutdown
// returns nil. A protocol switch that completes during Shutdown is closed
// immediately.
//
// If ctx ends first, Shutdown returns the error of ctx, as
// [http.Server.Shutdown] does, and the active requests continue. Call
// [Server.Close] to stop them. A connection that middleware or another
// component took with Hijack is not closed, but Shutdown waits for its
// handler.
//
// After Shutdown, [Server.Serve] returns [http.ErrServerClosed]. Do not use
// the server again.
func (s *Server) Shutdown(ctx context.Context) error {
	// Close the upgraded connections first: http.Server.Shutdown blocks
	// until the other requests complete.
	s.closeUpgraded()

	if err := s.http.Shutdown(ctx); err != nil {
		return err
	}

	if err := s.waitHandlers(ctx); err != nil {
		return err
	}

	s.cancelBase()

	return nil
}

// Close stops the server immediately. It closes the listeners and all
// connections, also the upgraded connections, and cancels the contexts of
// the active requests. It does not wait for the handlers to return. It
// returns the error of closing the listeners. Do not use the server again.
func (s *Server) Close() error {
	s.closeUpgraded()
	err := s.http.Close()
	s.cancelBase()

	return err
}

// serveHTTP applies the transfer idle deadlines and counts the handler
// calls, so Shutdown can wait for handlers that net/http no longer tracks.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.enter()
	defer s.leave()

	http1 := r.ProtoMajor == 1
	noBody := r.Body == nil || r.Body == http.NoBody
	t := &transfer{
		controller:     http.NewResponseController(w),
		idle:           s.transferIdle,
		http1:          http1,
		bodyDone:       noBody,
		expectContinue: http1 && !noBody && strings.EqualFold(r.Header.Get("Expect"), "100-continue"),
	}
	defer t.finish()

	inner := r
	if !noBody {
		// net/http fills this map at the end of an HTTP/1.1 body, also with
		// trailers that the request did not declare. The copy shares it, so
		// the gateway sees them (ADR-0027).
		if r.Trailer == nil {
			r.Trailer = make(http.Header)
		}

		// A shallow copy, so the gateway can change the body without a change
		// to the request of net/http. The two requests share the trailer map
		// above.
		inner = new(http.Request)
		*inner = *r
		inner.Body = &transferBody{
			ReadCloser: r.Body,
			transfer:   t,
		}
	}

	s.gateway.ServeHTTP(newTransferWriter(w, t), inner)
}

// enter counts a handler call.
func (s *Server) enter() {
	s.mu.Lock()
	s.active++
	s.mu.Unlock()
}

// leave ends a handler call and wakes a waiting Shutdown.
func (s *Server) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.active--
	if s.active == 0 && s.drained != nil {
		close(s.drained)
		s.drained = nil
	}
}

// waitHandlers waits until no handler call is active, or until ctx ends.
func (s *Server) waitHandlers(ctx context.Context) error {
	s.mu.Lock()
	if s.active == 0 {
		s.mu.Unlock()

		return nil
	}

	if s.drained == nil {
		s.drained = make(chan struct{})
	}

	drained := s.drained
	s.mu.Unlock()

	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeUpgraded closes the upgraded connections and makes later protocol
// switches close immediately.
func (s *Server) closeUpgraded() {
	s.mu.Lock()
	s.closing = true
	upgraded := s.upgraded
	s.upgraded = make(map[*serverConn]struct{})
	s.mu.Unlock()

	for c := range upgraded {
		// Close the network connection, not a TLS layer above it: a TLS
		// close sends an alert, which can block on a stalled client.
		_ = c.Conn.Close()
	}
}

// connState removes the write deadline that the last HTTP/1 response left on
// an idle connection, so it cannot stop a later response that net/http
// writes itself.
func (s *Server) connState(c net.Conn, state http.ConnState) {
	if state != http.StateIdle || s.transferIdle == 0 {
		return
	}

	// HTTP/2 manages the deadlines of its connection itself.
	if tc, ok := c.(*tls.Conn); ok && tc.ConnectionState().NegotiatedProtocol == "h2" {
		return
	}

	_ = c.SetWriteDeadline(time.Time{})
}

// track starts the upgraded mode of c, or closes c when the server is
// closing.
func (s *Server) track(c *serverConn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		_ = c.Conn.Close()

		return
	}

	s.upgraded[c] = struct{}{}
	c.upgraded.Store(true)
}

// forget removes c from the upgraded connections.
func (s *Server) forget(c *serverConn) {
	s.mu.Lock()
	delete(s.upgraded, c)
	s.mu.Unlock()
}
