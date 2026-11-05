package edgekit_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// testIdle is the transfer idle timeout of the tests that need one. It is
// short, so a stalled transfer ends fast, and long enough that a test
// client makes progress within it.
const testIdle = 200 * time.Millisecond

// testCertificate is a self-signed certificate for 127.0.0.1, made once.
var testCertificate = sync.OnceValues(func() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
})

// serverTLS returns a server TLS configuration with the test certificate.
func serverTLS(t *testing.T) *tls.Config {
	t.Helper()

	cert, err := testCertificate()
	if err != nil {
		t.Fatalf("make certificate: %v", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

// clientTLS returns a client TLS configuration that trusts the test
// certificate and offers the ALPN protocols nextProtos.
func clientTLS(t *testing.T, nextProtos ...string) *tls.Config {
	t.Helper()

	cert, err := testCertificate()
	if err != nil {
		t.Fatalf("make certificate: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)

	return &tls.Config{
		RootCAs:    roots,
		NextProtos: nextProtos,
		MinVersion: tls.VersionTLS12,
	}
}

// testListener records accepted connections and the close of the listener.
type testListener struct {
	net.Listener

	// writeBuffer, when not zero, is the send buffer of each accepted
	// connection, so a slow client blocks the server soon.
	writeBuffer int

	accepted  atomic.Int32
	closed    chan struct{}
	closeOnce sync.Once
}

// Accept counts the connection and sets its send buffer.
func (l *testListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	l.accepted.Add(1)
	if tcp, ok := conn.(*net.TCPConn); ok && l.writeBuffer != 0 {
		if err := tcp.SetWriteBuffer(l.writeBuffer); err != nil {
			// Close is best effort; return the buffer setup error.
			_ = conn.Close()

			return nil, err
		}
	}

	return conn, nil
}

// Close signals the close.
func (l *testListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })

	return l.Listener.Close()
}

// testServer is a started Server.
type testServer struct {
	server   *edgekit.Server
	listener *testListener
	secure   bool

	// served is closed when Serve returned, with its error in serveErr.
	served   chan struct{}
	serveErr error
}

// serverOptions are the test options of startServer. The zero value is a
// plaintext server.
type serverOptions struct {
	// tls serves with the test certificate.
	tls bool

	// writeBuffer, when not zero, is the send buffer of each accepted
	// connection, so a slow client blocks the server soon.
	writeBuffer int
}

// startServer serves g with cfg on a new local listener. It closes the
// server when the test ends.
func startServer(t *testing.T, g *edgekit.Gateway, cfg edgekit.ServerConfig, opts serverOptions) *testServer {
	t.Helper()

	if opts.tls {
		cfg.TLSConfig = serverTLS(t)
	}

	server, err := edgekit.NewServer(g, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ts := &testServer{
		server: server,
		listener: &testListener{
			Listener:    l,
			writeBuffer: opts.writeBuffer,
			closed:      make(chan struct{}),
		},
		secure: opts.tls,
		served: make(chan struct{}),
	}

	go func() {
		defer close(ts.served)
		ts.serveErr = server.Serve(ts.listener)
	}()

	t.Cleanup(func() {
		_ = server.Close()
		<-ts.served
	})

	return ts
}

// url returns the base URL of the server.
func (ts *testServer) url() string {
	if ts.secure {
		return "https://" + ts.listener.Addr().String()
	}

	return "http://" + ts.listener.Addr().String()
}

// client returns a client for the server. With TLS, it uses HTTP/2 when
// http2 is true, and HTTP/1.1 otherwise.
func (ts *testServer) client(t *testing.T, http2 bool) *http.Client {
	t.Helper()

	transport := &http.Transport{
		DisableCompression: true,
		Protocols:          new(http.Protocols),
	}
	if http2 {
		transport.Protocols.SetHTTP2(true)
	} else {
		transport.Protocols.SetHTTP1(true)
	}

	if ts.secure {
		transport.TLSClientConfig = clientTLS(t)
	}

	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
}

// dial opens a plaintext connection to the server with a 5s deadline for
// the complete test.
func (ts *testServer) dial(t *testing.T) net.Conn {
	t.Helper()

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", ts.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if deadlineErr := conn.SetDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
		t.Fatalf("set client deadline: %v", deadlineErr)
	}

	return conn
}

// writeClientData stops the test when client data cannot be sent.
func writeClientData(t *testing.T, conn net.Conn, data string) {
	t.Helper()

	if _, err := io.WriteString(conn, data); err != nil {
		t.Fatalf("write client data: %v", err)
	}
}

// shutdown calls Shutdown with a 5s limit.
func (ts *testServer) shutdown(t *testing.T) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	return ts.server.Shutdown(ctx)
}

// apiGateway returns a gateway with one route "/api/" to upstreamURL.
func apiGateway(t *testing.T, upstreamURL string, middleware []edgekit.Middleware, hooks ...any) *edgekit.Gateway {
	t.Helper()

	return newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstreamURL),
			},
		},
		Middleware: middleware,
		Hooks:      hooks,
	})
}

// passIdleTimeouts lets three transfer idle timeouts pass. A test that shows
// that a deadline does not stop a transfer has no event to wait for, so it
// must let the time pass. Tests use it only for that purpose.
func passIdleTimeouts() {
	<-time.After(3 * testIdle)
}

// protocols are the protocol cases of tests that run for both protocols.
var protocols = []struct {
	name  string
	http2 bool
}{
	{
		name:  "HTTP/1.1",
		http2: false,
	},
	{
		name:  "HTTP/2",
		http2: true,
	},
}

// maxWindowPlusOne is one more than the largest HTTP/2 window. With a
// 32-bit int, the value does not fit, and the sum wraps to a negative value,
// which is also not valid.
var maxWindowPlusOne = func() int {
	window := math.MaxInt32

	return window + 1
}()

func TestNewServerRejectsInvalidConfiguration(t *testing.T) {
	g := apiGateway(t, "http://upstream.test", nil)

	for _, tc := range []struct {
		name    string
		gateway *edgekit.Gateway
		cfg     edgekit.ServerConfig
	}{
		{
			name:    "nil gateway",
			gateway: nil,
			cfg:     edgekit.ServerConfig{},
		},
		{
			name:    "negative header read timeout",
			gateway: g,
			cfg: edgekit.ServerConfig{
				HeaderReadTimeout: -2 * time.Second,
			},
		},
		{
			name:    "negative keep-alive timeout",
			gateway: g,
			cfg: edgekit.ServerConfig{
				KeepAliveTimeout: -time.Second,
			},
		},
		{
			name:    "negative transfer idle timeout",
			gateway: g,
			cfg: edgekit.ServerConfig{
				TransferIdleTimeout: -2,
			},
		},
		{
			name:    "negative max header bytes",
			gateway: g,
			cfg: edgekit.ServerConfig{
				MaxHeaderBytes: -1,
			},
		},
		{
			name:    "negative stream count",
			gateway: g,
			cfg: edgekit.ServerConfig{
				HTTP2MaxConcurrentStreams: -1,
			},
		},
		{
			name:    "stream buffer too large",
			gateway: g,
			cfg: edgekit.ServerConfig{
				HTTP2MaxReceiveBufferPerStream: maxWindowPlusOne,
			},
		},
		{
			name:    "connection buffer below initial window",
			gateway: g,
			cfg: edgekit.ServerConfig{
				HTTP2MaxReceiveBufferPerConnection: 65534,
			},
		},
		{
			name:    "TLS without certificate",
			gateway: g,
			cfg: edgekit.ServerConfig{
				TLSConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, err := edgekit.NewServer(tc.gateway, tc.cfg)

			if server != nil {
				t.Error("NewServer() server is not nil")
			}

			if !errors.Is(err, edgekit.ErrInvalidConfiguration) {
				t.Fatalf("NewServer() error = %v, want ErrInvalidConfiguration", err)
			}

			for _, value := range []string{"-2s", "-1s", "2147483648", "65534"} {
				if strings.Contains(err.Error(), value) {
					t.Errorf("NewServer() error = %q, contains the value %q", err, value)
				}
			}
		})
	}
}

func TestNewServerAcceptsBoundaryValues(t *testing.T) {
	g := apiGateway(t, "http://upstream.test", nil)

	for _, tc := range []struct {
		name string
		cfg  edgekit.ServerConfig
	}{
		{
			name: "smallest values",
			cfg: edgekit.ServerConfig{
				HeaderReadTimeout:                  time.Nanosecond,
				MaxHeaderBytes:                     1,
				KeepAliveTimeout:                   time.Nanosecond,
				TransferIdleTimeout:                time.Nanosecond,
				HTTP2MaxConcurrentStreams:          1,
				HTTP2MaxReceiveBufferPerStream:     1,
				HTTP2MaxReceiveBufferPerConnection: 65535,
			},
		},
		{
			name: "largest windows",
			cfg: edgekit.ServerConfig{
				HTTP2MaxReceiveBufferPerStream:     math.MaxInt32,
				HTTP2MaxReceiveBufferPerConnection: math.MaxInt32,
			},
		},
		{
			name: "NoTimeout",
			cfg: edgekit.ServerConfig{
				HeaderReadTimeout:   edgekit.NoTimeout,
				KeepAliveTimeout:    edgekit.NoTimeout,
				TransferIdleTimeout: edgekit.NoTimeout,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := edgekit.NewServer(g, tc.cfg); err != nil {
				t.Errorf("NewServer() error = %v, want nil", err)
			}
		})
	}
}

// http2Settings returns the SETTINGS values and the connection window
// increment that the server sends first on a new HTTP/2 connection.
func http2Settings(t *testing.T, ts *testServer) (map[uint16]uint32, uint32) {
	t.Helper()

	dialer := tls.Dialer{
		Config: clientTLS(t, "h2"),
	}

	conn, err := dialer.DialContext(t.Context(), "tcp", ts.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if deadlineErr := conn.SetDeadline(time.Now().Add(5 * time.Second)); deadlineErr != nil {
		t.Fatalf("set HTTP/2 client deadline: %v", deadlineErr)
	}

	// The client preface and an empty SETTINGS frame (RFC 9113, 3.4).
	preface := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + "\x00\x00\x00\x04\x00\x00\x00\x00\x00"
	if _, err = io.WriteString(conn, preface); err != nil {
		t.Fatalf("write preface: %v", err)
	}

	settings := map[uint16]uint32{}
	var increment uint32

	reader := bufio.NewReader(conn)
	for range 10 {
		var header [9]byte
		if _, err = io.ReadFull(reader, header[:]); err != nil {
			t.Fatalf("read frame header: %v", err)
		}

		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		payload := make([]byte, length)
		if _, err = io.ReadFull(reader, payload); err != nil {
			t.Fatalf("read frame payload: %v", err)
		}

		frameType, flags, stream := header[3], header[4], binary.BigEndian.Uint32(header[5:])&0x7fffffff
		switch {
		case frameType == 0x4 && flags&0x1 == 0:
			for i := 0; i+6 <= len(payload); i += 6 {
				settings[binary.BigEndian.Uint16(payload[i:])] = binary.BigEndian.Uint32(payload[i+2:])
			}
		case frameType == 0x8 && stream == 0:
			increment = binary.BigEndian.Uint32(payload) & 0x7fffffff
		}

		if len(settings) != 0 && increment != 0 {
			break
		}
	}

	return settings, increment
}

func TestHTTP2Limits(t *testing.T) {
	const (
		settingMaxConcurrentStreams = 0x3
		settingInitialWindowSize    = 0x4
	)

	g := apiGateway(t, "http://upstream.test", nil)

	for _, tc := range []struct {
		name       string
		cfg        edgekit.ServerConfig
		streams    uint32
		window     uint32
		connWindow uint32
	}{
		{
			name:       "defaults",
			cfg:        edgekit.ServerConfig{},
			streams:    100,
			window:     64 << 10,
			connWindow: 1 << 20,
		},
		{
			name: "overrides",
			cfg: edgekit.ServerConfig{
				HTTP2MaxConcurrentStreams:          7,
				HTTP2MaxReceiveBufferPerStream:     128 << 10,
				HTTP2MaxReceiveBufferPerConnection: 4 << 20,
			},
			streams:    7,
			window:     128 << 10,
			connWindow: 4 << 20,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startServer(t, g, tc.cfg, serverOptions{tls: true})

			settings, increment := http2Settings(t, ts)

			if got := settings[settingMaxConcurrentStreams]; got != tc.streams {
				t.Errorf("SETTINGS_MAX_CONCURRENT_STREAMS = %d, want %d", got, tc.streams)
			}

			if got := settings[settingInitialWindowSize]; got != tc.window {
				t.Errorf("SETTINGS_INITIAL_WINDOW_SIZE = %d, want %d", got, tc.window)
			}

			// The connection window starts at 65535 (RFC 9113, 6.9.2).
			if got := increment + 65535; got != tc.connWindow {
				t.Errorf("connection window = %d, want %d", got, tc.connWindow)
			}
		})
	}
}

func TestHTTP2StreamLimitApplies(t *testing.T) {
	release := make(chan struct{})
	var wg sync.WaitGroup
	defer func() {
		// Release the handlers before waiting, also after a fatal test failure.
		close(release)
		wg.Wait()
	}()

	entered := make(chan struct{}, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/warm" {
			entered <- struct{}{}
			<-release
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	ts := startServer(t, apiGateway(t, upstream.URL, nil), edgekit.ServerConfig{
		HTTP2MaxConcurrentStreams: 2,
	}, serverOptions{tls: true})
	client := ts.client(t, true)

	// One request first, so the next requests find the connection and do
	// not dial in parallel.
	warmUp, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/warm", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(warmUp)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = response.Body.Close()

	for range 3 {
		wg.Go(func() {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/", nil)
			if err != nil {
				t.Errorf("NewRequest: %v", err)

				return
			}

			response, err := client.Do(request)
			if err != nil {
				t.Errorf("Do() error = %v", err)

				return
			}
			_ = response.Body.Close()
		})
	}

	for range 3 {
		receive(t, entered)
	}

	// The client opens a second connection when the server permits only
	// two streams on the first one.
	got := ts.listener.accepted.Load()
	if got != 2 {
		t.Errorf("connections = %d, want 2", got)
	}
}

func TestInboundHeaderLimitRejectsBeforeGatewayEntry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	for _, protocol := range protocols {
		t.Run(protocol.name, func(t *testing.T) {
			for _, tc := range []struct {
				name           string
				maxHeaderBytes int
				headerBytes    int
				bodyBytes      int
				rejected       bool
			}{
				{
					name:           "small headers",
					maxHeaderBytes: 0,
					headerBytes:    1 << 10,
					bodyBytes:      0,
					rejected:       false,
				},
				{
					// More than the default and the look-ahead of net/http.
					name:           "oversized headers",
					maxHeaderBytes: 0,
					headerBytes:    128 << 10,
					bodyBytes:      0,
					rejected:       true,
				},
				{
					name:           "body is not a header",
					maxHeaderBytes: 0,
					headerBytes:    1 << 10,
					bodyBytes:      128 << 10,
					rejected:       false,
				},
				{
					name:           "relaxed limit",
					maxHeaderBytes: 256 << 10,
					headerBytes:    128 << 10,
					bodyBytes:      0,
					rejected:       false,
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var entered atomic.Int32
					completionHook, completed := completions()
					g := apiGateway(t, upstream.URL, []edgekit.Middleware{
						func(next http.Handler) http.Handler {
							return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								entered.Add(1)
								next.ServeHTTP(w, r)
							})
						},
					}, completionHook)
					ts := startServer(t, g, edgekit.ServerConfig{
						MaxHeaderBytes: tc.maxHeaderBytes,
					}, serverOptions{tls: true})

					request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/", strings.NewReader(strings.Repeat("b", tc.bodyBytes)))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("X-Test", strings.Repeat("h", tc.headerBytes))

					response, err := ts.client(t, protocol.http2).Do(request)
					status := 0
					if err == nil {
						_, _ = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
						status = response.StatusCode
					}

					if tc.rejected {
						// The HTTP/2 client can refuse to send headers over the
						// limit that the server announced.
						if err == nil && status != http.StatusRequestHeaderFieldsTooLarge {
							t.Errorf("status = %d, want 431 or a client error", status)
						}

						if got := entered.Load(); got != 0 {
							t.Errorf("gateway entries = %d, want 0", got)
						}

						select {
						case <-completed:
							t.Error("completion ran for a request that net/http rejected")
						default:
						}

						return
					}

					if err != nil || status != http.StatusNoContent {
						t.Fatalf("Do() = %d, %v; want 204", status, err)
					}

					receive(t, completed)
				})
			}
		})
	}
}

func TestStalledTLSHandshakeTimesOut(t *testing.T) {
	output := captureDiagnostics(t)
	ts := startServer(t, apiGateway(t, "http://upstream.test", nil), edgekit.ServerConfig{
		HeaderReadTimeout: 100 * time.Millisecond,
	}, serverOptions{tls: true})

	// Open TCP, but send no ClientHello.
	conn := ts.dial(t)

	_, err := conn.Read(make([]byte, 1))
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("server did not close the stalled handshake")
	}

	if err == nil {
		t.Fatal("server sent data for a handshake without a ClientHello")
	}

	if err = ts.shutdown(t); err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}

	receive(t, ts.served)
	if !errors.Is(ts.serveErr, http.ErrServerClosed) {
		t.Errorf("Serve() error = %v, want http.ErrServerClosed", ts.serveErr)
	}

	receive(t, ts.listener.closed)

	unsafe, events, err := unsafeServerDiagnostics(output.String())
	if err != nil {
		t.Fatalf("decode diagnostics %q: %v", output.String(), err)
	}

	if events == 0 || len(unsafe) != 0 {
		t.Errorf("diagnostics: %d events, unsafe %v; want source-only http_server events", events, unsafe)
	}

	for _, secret := range []string{conn.LocalAddr().String(), "secret"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("diagnostics contain %q: %s", secret, output.String())
		}
	}
}

// unsafeServerDiagnostics returns the records in output that are not
// source-only http_server events, and the number of records. It returns an
// error when output is not a sequence of JSON records.
func unsafeServerDiagnostics(output string) ([]map[string]any, int, error) {
	var unsafe []map[string]any

	events := 0
	decoder := json.NewDecoder(strings.NewReader(output))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			return nil, 0, err
		}

		if record["source"] != "http_server" || record["msg"] != "diagnostic details omitted" || len(record) != 4 {
			unsafe = append(unsafe, record)
		}

		events++
	}

	return unsafe, events, nil
}

func TestServerDiagnosticsOmitDetails(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		aborted    bool
		diagnostic bool
	}{
		{
			name:       "panic",
			mode:       "panic",
			aborted:    true,
			diagnostic: true,
		},
		{
			name:       "abort",
			mode:       "abort",
			aborted:    true,
			diagnostic: false,
		},
		{
			name:       "invalid header",
			mode:       "invalid header",
			aborted:    false,
			diagnostic: true,
		},
		{
			name:       "normal",
			mode:       "normal",
			aborted:    false,
			diagnostic: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := captureDiagnostics(t)
			completionHook, completed := completions()
			g := apiGateway(t, "http://upstream.test", []edgekit.Middleware{
				func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						switch tc.mode {
						case "panic":
							panic("https://user:credential-secret@host/path-secret\nstack-secret")
						case "abort":
							panic(http.ErrAbortHandler)
						case "invalid header":
							w.Header().Set("Content-Length", "header-secret")
						}

						_, _ = io.WriteString(w, "public")
					})
				},
			}, completionHook)
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})

			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/path-secret?token=query-secret", nil)
			if err != nil {
				t.Fatal(err)
			}

			response, err := ts.client(t, false).Do(request)
			if tc.aborted {
				if err == nil {
					_ = response.Body.Close()
					t.Fatal("Do() error = nil, want an aborted response")
				}
			} else {
				if err != nil {
					t.Fatalf("Do() error = %v", err)
				}

				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()

				if response.StatusCode != http.StatusOK || readErr != nil || string(body) != "public" {
					t.Errorf("response = %d %q, %v; want 200 %q", response.StatusCode, body, readErr, "public")
				}
			}

			c := receive(t, completed)
			if c.Aborted != tc.aborted || c.Failure != nil {
				t.Errorf("completion Aborted = %t, Failure = %+v; want %t, nil", c.Aborted, c.Failure, tc.aborted)
			}

			// The server logs before it closes the connection, and the
			// shutdown waits for the handlers.
			if err = ts.shutdown(t); err != nil {
				t.Fatalf("Shutdown() error = %v", err)
			}

			if !tc.diagnostic {
				if output.Len() != 0 {
					t.Errorf("diagnostics = %q, want none", output.String())
				}

				return
			}

			unsafe, events, err := unsafeServerDiagnostics(output.String())
			if err != nil {
				t.Fatalf("decode diagnostics %q: %v", output.String(), err)
			}

			if events == 0 || len(unsafe) != 0 {
				t.Errorf("diagnostics: %d events, unsafe %v; want source-only http_server events", events, unsafe)
			}

			for _, secret := range []string{ts.listener.Addr().String(), "secret"} {
				if strings.Contains(output.String(), secret) {
					t.Errorf("diagnostics contain %q: %s", secret, output.String())
				}
			}
		})
	}
}

func TestServerLifecycleWritesNoDiagnostics(t *testing.T) {
	output := captureDiagnostics(t)
	server, err := edgekit.NewServer(apiGateway(t, "http://upstream.test", nil), edgekit.ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	// A listener that fails: Serve returns its error.
	_ = l.Close()
	if err = server.Serve(l); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve(closed listener) error = %v, want the listener error", err)
	}

	if err = server.Shutdown(t.Context()); err != nil {
		t.Errorf("Shutdown() error = %v", err)
	}

	l, err = lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	if err = server.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve() after Shutdown error = %v, want http.ErrServerClosed", err)
	}

	if err = server.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}

	if output.Len() != 0 {
		t.Errorf("diagnostics = %q, want none", output.String())
	}
}

func TestServeClosesListenerWhenTLSSetupFails(t *testing.T) {
	config := serverTLS(t)

	// HTTP/2 requires an AES-128-GCM cipher, so net/http refuses this list
	// before it accepts a connection.
	config.CipherSuites = []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384}

	server, err := edgekit.NewServer(apiGateway(t, "http://upstream.test", nil), edgekit.ServerConfig{
		TLSConfig: config,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	listener := &testListener{
		Listener: l,
		closed:   make(chan struct{}),
	}
	t.Cleanup(func() { _ = l.Close() })

	err = server.Serve(listener)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve() error = %v, want the TLS setup error", err)
	}

	select {
	case <-listener.closed:
	default:
		t.Error("Serve() returned without closing the listener")
	}
}
