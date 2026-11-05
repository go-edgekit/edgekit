package edgekit_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// switchingUpstream accepts a protocol switch after release is closed, then
// writes ticks lines and echoes until the connection closes.
func switchingUpstream(t *testing.T, release <-chan struct{}, ticks int) (*httptest.Server, <-chan struct{}) {
	t.Helper()

	closed := make(chan struct{}, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		conn, buffered, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() {
			_ = conn.Close()
			closed <- struct{}{}
		}()

		_, _ = io.WriteString(buffered, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = buffered.Flush()

		if ticks != 0 {
			go func() {
				ticker := time.NewTicker(testIdle / 3)
				defer ticker.Stop()

				for range ticks {
					<-ticker.C
					if _, writeErr := io.WriteString(conn, "tick\n"); writeErr != nil {
						return
					}
				}
			}()
		}

		_, _ = io.Copy(conn, buffered)
	}))
	t.Cleanup(upstream.Close)

	return upstream, closed
}

// released is a closed channel.
var released = func() chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}()

// upgrade sends a protocol switch request on conn and returns the reader
// after the 101 response.
func upgrade(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()

	writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read 101 response: %v", err)
	}
	_ = response.Body.Close()

	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", response.StatusCode)
	}

	return reader
}

// dialUpgraded opens a connection to ts for a protocol switch, with TLS when
// ts is secure.
func dialUpgraded(t *testing.T, ts *testServer) net.Conn {
	t.Helper()

	conn := ts.dial(t)
	if !ts.secure {
		return conn
	}

	config := clientTLS(t, "http/1.1")
	config.ServerName = "127.0.0.1"

	tlsConn := tls.Client(conn, config)
	t.Cleanup(func() { _ = tlsConn.Close() })

	return tlsConn
}

// closedByServer reads reader to the end and reports whether the server
// closed the connection before the 5s test deadline of the connection.
func closedByServer(reader *bufio.Reader) bool {
	_, err := io.ReadAll(reader)

	var netErr net.Error

	return !errors.As(err, &netErr) || !netErr.Timeout()
}

func TestUpgradedConnectionIdleTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secure bool
	}{
		{
			name:   "plaintext",
			secure: false,
		},
		{
			name:   "TLS",
			secure: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream, upstreamClosed := switchingUpstream(t, released, 0)
			completionHook, completed := completions()
			ts := startServer(t, apiGateway(t, upstream.URL, nil, completionHook), edgekit.ServerConfig{
				TransferIdleTimeout: testIdle,
			}, serverOptions{tls: tc.secure})

			conn := dialUpgraded(t, ts)
			reader := upgrade(t, conn)

			writeClientData(t, conn, "ping\n")
			if line, err := reader.ReadString('\n'); line != "ping\n" || err != nil {
				t.Fatalf("echo = %q, %v", line, err)
			}

			// Then no data moves, so the server closes the upgraded connection.
			if !closedByServer(reader) {
				t.Fatal("server did not close the upgraded connection")
			}
			receive(t, completed)
			receive(t, upstreamClosed)
		})
	}
}

func TestUpgradedConnectionActiveInOneDirection(t *testing.T) {
	// The upstream writes a tick in each third of the timeout, for three
	// times the timeout. The client sends nothing.
	const ticks = 9

	upstream, _ := switchingUpstream(t, released, ticks)
	ts := startServer(t, apiGateway(t, upstream.URL, nil), edgekit.ServerConfig{
		TransferIdleTimeout: testIdle,
	}, serverOptions{})

	reader := upgrade(t, ts.dial(t))

	for i := range ticks {
		if line, err := reader.ReadString('\n'); line != "tick\n" || err != nil {
			t.Fatalf("tick %d = %q, %v", i, line, err)
		}
	}

	if !closedByServer(reader) {
		t.Fatal("server did not close the upgraded connection")
	}
}

func TestUpgradedConnectionWithoutIdleTimeout(t *testing.T) {
	upstream, _ := switchingUpstream(t, released, 0)
	ts := startServer(t, apiGateway(t, upstream.URL, nil), edgekit.ServerConfig{
		TransferIdleTimeout: edgekit.NoTimeout,
	}, serverOptions{})

	conn := ts.dial(t)
	reader := upgrade(t, conn)

	passIdleTimeouts()

	writeClientData(t, conn, "ping\n")
	if line, err := reader.ReadString('\n'); line != "ping\n" || err != nil {
		t.Errorf("echo after quiet time = %q, %v; want %q", line, err, "ping\n")
	}
}

func TestShutdownClosesUpgradedConnections(t *testing.T) {
	switching, upstreamClosed := switchingUpstream(t, released, 0)
	release := make(chan struct{})
	slow := quietUpstream(t, release)
	completionHook, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, switching.URL),
			},
			{
				Prefix:   "/slow/",
				Upstream: static(t, slow.URL),
			},
		},
		Hooks: []any{completionHook},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

	// An ordinary request that is active during the shutdown.
	slowReader := quietGet(t, ts.client(t, false), ts.url()+"/slow/")

	reader := upgrade(t, ts.dial(t))

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- ts.shutdown(t)
	}()

	// The upgraded connection closes before the ordinary request completes.
	if !closedByServer(reader) {
		t.Fatal("server did not close the upgraded connection")
	}
	receive(t, upstreamClosed)

	if c := receive(t, completed); c.RoutePrefix != "/api/" {
		t.Errorf("first completion route = %q, want the upgraded request", c.RoutePrefix)
	}

	select {
	case err := <-shutdown:
		t.Fatalf("Shutdown() returned %v before the active request completed", err)
	default:
	}

	close(release)

	if rest, err := io.ReadAll(slowReader); string(rest) != "last\n" || err != nil {
		t.Errorf("ordinary response rest = %q, %v; want %q", rest, err, "last\n")
	}

	if err := receive(t, shutdown); err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}

	// Completion of all requests ran before Shutdown returned.
	select {
	case <-completed:
	default:
		t.Error("completion of the ordinary request did not run before Shutdown returned")
	}
}

func TestUpgradeDuringShutdownIsClosed(t *testing.T) {
	release := make(chan struct{})
	upstream, upstreamClosed := switchingUpstream(t, release, 0)
	entered := make(chan struct{}, 1)
	completionHook, completed := completions()
	g := apiGateway(t, upstream.URL, []edgekit.Middleware{
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				next.ServeHTTP(w, r)
			})
		},
	}, completionHook)
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

	conn := ts.dial(t)
	writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	receive(t, entered)

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- ts.shutdown(t)
	}()

	// Shutdown closes the listener after it closed the upgraded connections,
	// so the protocol switch completes after that.
	receive(t, ts.listener.closed)
	close(release)

	if !closedByServer(bufio.NewReader(conn)) {
		t.Fatal("server did not close the connection")
	}
	receive(t, upstreamClosed)

	c := receive(t, completed)
	if c.Failure == nil || c.Failure.Phase != edgekit.PhaseProtocolSwitch {
		t.Errorf("completion Failure = %+v, want a failure in PhaseProtocolSwitch", c.Failure)
	}

	if err := receive(t, shutdown); err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}
}

func TestShutdownLeavesMiddlewareConnection(t *testing.T) {
	hijacked := make(chan net.Conn, 1)
	done := make(chan struct{})
	g := apiGateway(t, "http://upstream.test", []edgekit.Middleware{
		func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err != nil {
					t.Errorf("Hijack() error = %v", err)

					return
				}

				hijacked <- conn
				<-done
			})
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{
		TransferIdleTimeout: testIdle,
	}, serverOptions{})

	client := ts.dial(t)
	writeClientData(t, client, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\n\r\n")
	conn := receive(t, hijacked)

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- ts.shutdown(t)
	}()
	receive(t, ts.listener.closed)

	// The connection belongs to the middleware: no idle deadline and no
	// close at shutdown.
	passIdleTimeouts()

	_, writeErr := io.WriteString(conn, "open\n")
	line, readErr := bufio.NewReader(client).ReadString('\n')
	_ = conn.Close()
	close(done)

	if writeErr != nil || readErr != nil || line != "open\n" {
		t.Errorf("middleware connection = %q, %v, %v; want %q", line, writeErr, readErr, "open\n")
	}

	if err := receive(t, shutdown); err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}
}

func TestShutdownContextEndsBeforeRequests(t *testing.T) {
	entered := make(chan context.Context, 1)
	g := apiGateway(t, "http://upstream.test", []edgekit.Middleware{
		func(http.Handler) http.Handler {
			return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				entered <- r.Context()
				<-r.Context().Done()
			})
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

	conn := ts.dial(t)
	writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\n\r\n")
	requestCtx := receive(t, entered)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := ts.server.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Shutdown() error = %v, want context.Canceled", err)
	}

	// As http.Server.Shutdown, the active request continues.
	if requestCtx.Err() != nil {
		t.Error("Shutdown() canceled an active request")
	}

	if err := ts.server.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	select {
	case <-requestCtx.Done():
	case <-time.After(5 * time.Second):
		t.Error("Close() did not cancel the active request")
	}
}

func TestShutdownWaitsForUpgradedHandlers(t *testing.T) {
	upstream, _ := switchingUpstream(t, released, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	completionHook := requestCompletedFunc(func(context.Context, *edgekit.RequestContext, edgekit.Completion) {
		close(entered)
		<-release
	})
	ts := startServer(t, apiGateway(t, upstream.URL, nil, completionHook), edgekit.ServerConfig{}, serverOptions{})

	reader := upgrade(t, ts.dial(t))

	shutdown := make(chan error, 1)
	go func() {
		shutdown <- ts.shutdown(t)
	}()

	// net/http does not track the handler of an upgraded connection, so
	// only the server keeps Shutdown waiting for its completion.
	if !closedByServer(reader) {
		t.Fatal("server did not close the upgraded connection")
	}
	receive(t, entered)

	// Shutdown must not return while completion runs. No event shows that
	// it did not return, so the test gives it time to do so.
	select {
	case err := <-shutdown:
		t.Errorf("Shutdown() = %v before completion of the upgraded request returned", err)
	case <-time.After(3 * testIdle):
	}

	close(release)

	if err := receive(t, shutdown); err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}
}
