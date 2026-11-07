package edgekit_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// countingServer returns a started upstream server that counts its new
// connections.
func countingServer(t *testing.T, handler http.Handler, connections *atomic.Int32) *httptest.Server {
	t.Helper()

	server := httptest.NewUnstartedServer(handler)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	return server
}

// testTransport returns a transport with the secure defaults and closes its
// idle connections when the test ends.
func testTransport(t *testing.T) *http.Transport {
	t.Helper()

	transport := edgekit.NewTransport()
	t.Cleanup(transport.CloseIdleConnections)

	return transport
}

func TestUpstreamTransportPolicy(t *testing.T) {
	transport := testTransport(t)

	if transport.Proxy != nil {
		t.Error("NewTransport().Proxy is set, want nil (no proxy from the environment)")
	}

	if transport.DialContext == nil {
		t.Error("NewTransport().DialContext = nil, want a dialer with a timeout")
	}

	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{
			name: "TLSHandshakeTimeout",
			got:  transport.TLSHandshakeTimeout,
			want: 5 * time.Second,
		},
		{
			name: "ResponseHeaderTimeout",
			got:  transport.ResponseHeaderTimeout,
			want: 10 * time.Second,
		},
		{
			name: "ExpectContinueTimeout",
			got:  transport.ExpectContinueTimeout,
			want: time.Second,
		},
		{
			name: "IdleConnTimeout",
			got:  transport.IdleConnTimeout,
			want: 90 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}

	if transport.MaxResponseHeaderBytes != 64<<10 {
		t.Errorf("MaxResponseHeaderBytes = %d, want %d", transport.MaxResponseHeaderBytes, 64<<10)
	}

	if transport.MaxIdleConns != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", transport.MaxIdleConns)
	}

	if transport.MaxIdleConnsPerHost != 16 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 16", transport.MaxIdleConnsPerHost)
	}

	if transport.MaxConnsPerHost != 0 {
		t.Errorf("MaxConnsPerHost = %d, want 0 (no limit)", transport.MaxConnsPerHost)
	}

	if transport.DisableKeepAlives {
		t.Error("DisableKeepAlives = true, want false")
	}

	if !transport.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}

	if !transport.DisableCompression {
		t.Error("DisableCompression = false, want true")
	}

	if transport == testTransport(t) {
		t.Error("NewTransport() returned the same transport twice, want a new one for each call")
	}
}

func TestTransportTLSHandshakeTimeout(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		defer close(accepted)

		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	t.Cleanup(func() {
		// Best-effort closes release the accept goroutine on every return path.
		_ = listener.Close()
		if conn, ok := <-accepted; ok {
			_ = conn.Close()
		}
	})

	transport := testTransport(t)
	transport.TLSHandshakeTimeout = 100 * time.Millisecond

	// The upstream accepts TCP, but never answers the ClientHello.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+listener.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := (&http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}).Do(request)
	if err == nil {
		_ = response.Body.Close()
	}

	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Do() error = %v, want a TLS handshake timeout before the client timeout", err)
	}
}

func TestUpstreamDialRespectsCanceledContext(t *testing.T) {
	transport := testTransport(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	conn, err := transport.DialContext(ctx, "tcp", "127.0.0.1:1")
	if conn != nil {
		_ = conn.Close()
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("dial error = %v, want context.Canceled", err)
	}
}

func TestTransportReusesConnectionsAcrossRoutes(t *testing.T) {
	var connections atomic.Int32
	upstream := countingServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}), &connections)

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
			{
				Prefix:   "/other/",
				Upstream: static(t, upstream.URL),
			},
		},
	})

	for _, path := range []string{"/api/one", "/other/two", "/api/three"} {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))

		if w.Code != http.StatusOK || w.Body.String() != "ok" {
			t.Fatalf("GET %s = %d %q, want 200 %q", path, w.Code, w.Body.String(), "ok")
		}
	}

	if connections.Load() != 1 {
		t.Errorf("gateway opened %d upstream connections, want 1", connections.Load())
	}
}

func TestCloseIdleConnections(t *testing.T) {
	for _, tc := range []struct {
		name            string
		configured      bool
		wantConnections int32
	}{
		{
			name:            "gateway transport",
			configured:      false,
			wantConnections: 2,
		},
		{
			name:            "configured transport",
			configured:      true,
			wantConnections: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var connections atomic.Int32
			upstream := countingServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}), &connections)

			cfg := edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
			}
			if tc.configured {
				cfg.Transport = testTransport(t)
			}

			g := newGateway(t, cfg)

			g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/one", nil))
			g.CloseIdleConnections()
			g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/two", nil))

			if connections.Load() != tc.wantConnections {
				t.Errorf("gateway opened %d upstream connections, want %d", connections.Load(), tc.wantConnections)
			}
		})
	}
}

func TestResponseHeaderTimeoutCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()

	transport := testTransport(t)
	transport.ResponseHeaderTimeout = 100 * time.Millisecond
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Transport: transport,
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/slow", nil))

	if w.Code != http.StatusGatewayTimeout || w.Body.String() != problem(http.StatusGatewayTimeout, "") {
		t.Errorf("response = %d %q, want 504 without details", w.Code, w.Body.String())
	}

	receive(t, canceled)
}

func TestHeaderTimeoutDoesNotLimitResponseStream(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/hang" {
			<-r.Context().Done()

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush: %v", err)

			return
		}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}

		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer upstream.Close()

	transport := testTransport(t)
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Transport: transport,
	})
	server := httptest.NewServer(g)
	defer server.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	stream, err := client.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()

	reader := bufio.NewReader(stream.Body)
	if line, readErr := reader.ReadString('\n'); readErr != nil || line != "data: first\n" {
		t.Fatalf("first event = %q, %v; want %q, <nil>", line, readErr, "data: first\n")
	}

	// The control request starts after the first event and times out. So the
	// stream is open for longer than the response-header timeout.
	control, err := client.Get(server.URL + "/api/hang")
	if err != nil {
		t.Fatal(err)
	}
	_ = control.Body.Close()

	if control.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("control request status = %d, want 504 from the timeout", control.StatusCode)
	}

	close(release)

	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "\ndata: last\n\n" {
		t.Errorf("rest of stream = %q, %v; want %q, <nil>", rest, err, "\ndata: last\n\n")
	}
}

func TestCancellationWhileWaitingForConnectionPool(t *testing.T) {
	entered := make(chan struct{})
	var calls, connections atomic.Int32
	upstream := countingServer(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}

		<-r.Context().Done()
	}), &connections)

	transport := testTransport(t)
	transport.MaxConnsPerHost = 1
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
		Transport: transport,
	})

	firstCtx, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()

	firstDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/first", nil).WithContext(firstCtx))
		firstDone <- w.Code
	}()
	receive(t, entered)

	// The only connection is in use, so the queued request waits for the pool
	// after GetConn.
	waiting := make(chan struct{})
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { close(waiting) },
	}
	queuedCtx, cancelQueued := context.WithCancel(httptrace.WithClientTrace(t.Context(), trace))
	defer cancelQueued()

	queuedDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/queued", nil).WithContext(queuedCtx))
		queuedDone <- w.Code
	}()
	receive(t, waiting)
	cancelQueued()

	// Both clients are gone when their requests fail (ADR-0023).
	if status := receive(t, queuedDone); status != 499 {
		t.Errorf("queued request status = %d, want 499", status)
	}

	if calls.Load() != 1 || connections.Load() != 1 {
		t.Errorf("upstream got %d requests on %d connections, want 1 on 1", calls.Load(), connections.Load())
	}

	cancelFirst()

	if status := receive(t, firstDone); status != 499 {
		t.Errorf("first request status = %d, want 499", status)
	}
}

func TestRequestBodyStreamsBeforeClientEOF(t *testing.T) {
	type read struct {
		data string
		err  error
	}

	first, rest := make(chan read, 1), make(chan read, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 3)
		if _, err := io.ReadFull(r.Body, buf); err != nil {
			first <- read{err: err}

			return
		}
		first <- read{data: string(buf)}

		remaining, err := io.ReadAll(r.Body)
		rest <- read{data: string(remaining), err: err}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
	})
	server := httptest.NewServer(g)
	defer server.Close()

	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()

	result := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Post(server.URL+"/api/upload", "application/octet-stream", reader)
		if err == nil {
			_, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
		}

		if err == nil && response.StatusCode != http.StatusOK {
			err = errors.New(response.Status)
		}
		result <- err
	}()

	if _, err := io.WriteString(writer, "abc"); err != nil {
		t.Fatal(err)
	}

	// The client has not finished the body, so the upstream can only get these
	// bytes if the gateway streams them.
	if got := receive(t, first); got.data != "abc" || got.err != nil {
		t.Fatalf("upstream first bytes = %q, read error %v; want %q", got.data, got.err, "abc")
	}

	if _, err := io.WriteString(writer, "def"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()

	if got := receive(t, rest); got.data != "def" || got.err != nil {
		t.Errorf("upstream rest of body = %q, read error %v; want %q", got.data, got.err, "def")
	}

	if err := receive(t, result); err != nil {
		t.Errorf("POST /api/upload error = %v, want <nil>", err)
	}
}

func TestNoTransparentCompression(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(zw, "payload")
	_ = zw.Close()

	received := make(chan []string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Values("Accept-Encoding")
		if r.Header.Get("Accept-Encoding") == "gzip" {
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(compressed.Bytes())

			return
		}

		_, _ = io.WriteString(w, "payload")
	}))
	defer upstream.Close()

	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, upstream.URL),
		}},
	})

	for _, tc := range []struct {
		name           string
		acceptEncoding string
		wantUpstream   []string
		wantEncoding   string
		wantBody       []byte
	}{
		{
			name:     "client without Accept-Encoding",
			wantBody: []byte("payload"),
		},
		{
			name:           "client accepts gzip",
			acceptEncoding: "gzip",
			wantUpstream:   []string{"gzip"},
			wantEncoding:   "gzip",
			wantBody:       compressed.Bytes(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			if tc.acceptEncoding != "" {
				r.Header.Set("Accept-Encoding", tc.acceptEncoding)
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if got := receive(t, received); !slices.Equal(got, tc.wantUpstream) {
				t.Errorf("upstream Accept-Encoding = %q, want %q", got, tc.wantUpstream)
			}

			if got := w.Header().Get("Content-Encoding"); got != tc.wantEncoding {
				t.Errorf("client Content-Encoding = %q, want %q", got, tc.wantEncoding)
			}

			if !bytes.Equal(w.Body.Bytes(), tc.wantBody) {
				t.Errorf("client body = %q, want %q", w.Body.Bytes(), tc.wantBody)
			}
		})
	}
}

func TestUpstreamHeaderLimitBeforeResponseHooks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http2 bool
	}{
		{
			name: "http1",
		},
		{
			name:  "http2",
			http2: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wrongProto atomic.Bool
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.http2 != (r.ProtoMajor == 2) {
					wrongProto.Store(true)
				}

				// The default limit of NewTransport is 64 KiB.
				size := 1 << 10
				if strings.Contains(r.URL.Path, "large") {
					size = 128 << 10
				}

				w.Header().Set("X-Large", strings.Repeat("h", size))
				_, _ = io.WriteString(w, "ok")
			}))
			transport := testTransport(t)
			if tc.http2 {
				upstream.EnableHTTP2 = true
				upstream.StartTLS()

				roots := x509.NewCertPool()
				roots.AddCert(upstream.Certificate())
				transport.TLSClientConfig = &tls.Config{
					RootCAs:    roots,
					MinVersion: tls.VersionTLS12,
				}
			} else {
				upstream.Start()
			}
			defer upstream.Close()

			var responses int
			hooks := edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
				responses++

				return nil, nil
			})
			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Transport: transport,
				Hooks:     []any{hooks, completionHooks},
			})

			for _, path := range []string{"/api/small", "/api/large", "/api/small-again"} {
				before := responses
				w := httptest.NewRecorder()
				g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				c := receive(t, completed)

				if path == "/api/large" {
					if w.Code != http.StatusBadGateway || w.Body.String() != problem(http.StatusBadGateway, "") || w.Header().Get("X-Large") != "" {
						t.Errorf("%s: response = %d %q, X-Large %d bytes; want 502 without the header", path, w.Code, w.Body.String(), len(w.Header().Get("X-Large")))
					}

					if responses != before || c.Failure == nil || c.Failure.Phase != edgekit.PhaseRoundTrip || c.Aborted {
						t.Errorf("%s: response hooks ran %d times, completion = %+v; want 0 and a round-trip failure", path, responses-before, c)
					}

					continue
				}

				if w.Code != http.StatusOK || w.Body.String() != "ok" || len(w.Header().Get("X-Large")) != 1<<10 {
					t.Errorf("%s: response = %d %q, X-Large %d bytes; want 200 with 1 KiB", path, w.Code, w.Body.String(), len(w.Header().Get("X-Large")))
				}

				if responses != before+1 || c.Failure != nil || c.Aborted {
					t.Errorf("%s: response hooks ran %d times, completion = %+v; want 1 and no failure", path, responses-before, c)
				}
			}

			if wrongProto.Load() {
				t.Errorf("upstream got a protocol other than %s", tc.name)
			}
		})
	}
}
