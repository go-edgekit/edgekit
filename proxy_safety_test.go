package gateway_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/admission"
	"example.com/go-gateway-starter/extensions/bodylimit"
)

func safetyTransport(t *testing.T) *http.Transport {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	return transport
}

func waitSafety[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for proxy activity")
		var zero T
		return zero
	}
}

func TestForwardingTrustAndHopByHopHeaders(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			seen := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				w.Header().Add("Connection", "X-Response-Hop")
				w.Header().Add("Connection", "Keep-Alive")
				w.Header().Set("X-Response-Hop", "private")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.Header().Set("Proxy-Authenticate", "private")
				w.Header().Set("X-End-To-End", "response")
				_, _ = io.WriteString(w, "ok")
			}))
			defer upstream.Close()
			app, err := gateway.NewWithConfig(gateway.Config{
				Routes: []gateway.Route{testRoute(t, upstream.URL)}, Transport: safetyTransport(t),
			})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "http://client.example/api/x", nil)
			r.RemoteAddr = "192.0.2.10:12345"
			if secure {
				r.TLS = &tls.ConnectionState{}
			}
			for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"} {
				r.Header.Add(name, "spoof-one")
				r.Header.Add(name, "spoof-two")
			}
			r.Header.Add("Connection", "X-Request-Hop, X-Forwarded-For")
			r.Header.Add("Connection", "Keep-Alive")
			r.Header.Set("X-Request-Hop", "private")
			r.Header.Set("Keep-Alive", "timeout=5")
			r.Header.Set("Proxy-Authorization", "private")
			r.Header.Set("Proxy-Connection", "keep-alive")
			r.Header.Set("Te", "gzip, trailers")
			r.Header.Set("X-End-To-End", "request")
			original := r.Header.Clone()
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			got := waitSafety(t, seen)
			for _, name := range []string{"Forwarded", "X-Real-IP", "Connection", "X-Request-Hop", "Keep-Alive", "Proxy-Authorization", "Proxy-Connection"} {
				if got.Get(name) != "" {
					t.Errorf("upstream received %s: %q", name, got.Values(name))
				}
			}
			proto := "http"
			if secure {
				proto = "https"
			}
			for name, want := range map[string]string{
				"X-Forwarded-For": "192.0.2.10", "X-Forwarded-Host": "client.example",
				"X-Forwarded-Proto": proto, "Te": "trailers", "X-End-To-End": "request",
			} {
				if !reflect.DeepEqual(got.Values(name), []string{want}) {
					t.Errorf("%s = %q, want only %q", name, got.Values(name), want)
				}
			}
			for _, name := range []string{"Connection", "X-Response-Hop", "Keep-Alive", "Proxy-Authenticate"} {
				if w.Header().Get(name) != "" {
					t.Errorf("client received %s", name)
				}
			}
			if w.Code != 200 || w.Body.String() != "ok" || w.Header().Get("X-End-To-End") != "response" || !reflect.DeepEqual(r.Header, original) {
				t.Fatal("lost end-to-end response or mutated inbound headers")
			}
		})
	}
}

type safetyRoundTripper func(*http.Request) (*http.Response, error)

func (f safetyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfiguredTransportWithAndWithoutOutboundHooks(t *testing.T) {
	for _, withHooks := range []bool{false, true} {
		for _, fail := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "plain", true: "hooks"}[withHooks], map[bool]string{false: "success", true: "failure"}[fail]}, "/"), func(t *testing.T) {
				calls := 0
				failure := errors.New("private transport detail")
				var observed gateway.Outcome
				extension := &testExtension{
					outbound: func(_ context.Context, _ *gateway.RequestContext, r *http.Request) error {
						r.Header.Set("X-Hook", "ran")
						return nil
					},
					complete: func(_ context.Context, _ *gateway.RequestContext, outcome gateway.Outcome) { observed = outcome },
				}
				config := gateway.Config{
					Routes: []gateway.Route{testRoute(t, "http://unused.invalid")},
					Transport: safetyRoundTripper(func(r *http.Request) (*http.Response, error) {
						calls++
						if (r.Header.Get("X-Hook") == "ran") != withHooks {
							t.Error("configured transport lost outbound hook processing")
						}
						if fail {
							return nil, failure
						}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
					}),
				}
				if withHooks {
					config.Extensions = []any{extension}
				}
				app, err := gateway.NewWithConfig(config)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))
				wantStatus, wantBody := 200, "ok"
				if fail {
					wantStatus, wantBody = 502, "Bad Gateway\n"
				}
				if calls != 1 || w.Code != wantStatus || w.Body.String() != wantBody || (withHooks && fail && !errors.Is(observed.Err, failure)) {
					t.Fatalf("calls=%d response=%d %q outcome=%+v", calls, w.Code, w.Body.String(), observed)
				}
			})
		}
	}
}

func TestTransportReusesConnectionsAcrossRoutes(t *testing.T) {
	var connections atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	upstream.Start()
	defer upstream.Close()
	first, second := testRoute(t, upstream.URL), testRoute(t, upstream.URL)
	second.Prefix = "/other/"
	app, err := gateway.NewWithConfig(gateway.Config{Routes: []gateway.Route{first, second}, Transport: safetyTransport(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/one", "/other/two", "/api/three"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 200 || w.Body.String() != "ok" {
			t.Fatalf("response=%d %q", w.Code, w.Body.String())
		}
	}
	if connections.Load() != 1 {
		t.Fatalf("opened %d upstream connections, want one reused connection", connections.Load())
	}
}

func TestResponseHeaderTimeoutCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	transport := safetyTransport(t)
	transport.ResponseHeaderTimeout = 100 * time.Millisecond
	var outcome gateway.Outcome
	unwound := false
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{testRoute(t, upstream.URL)}, Transport: transport,
		Middlewares: []gateway.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { unwound = true }()
				next.ServeHTTP(w, r)
			})
		}},
		Extensions: []any{&testExtension{complete: func(_ context.Context, _ *gateway.RequestContext, got gateway.Outcome) {
			if !unwound {
				t.Error("completion preceded middleware unwind")
			}
			outcome = got
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/slow", nil).WithContext(ctx))
	var timeout net.Error
	if w.Code != 502 || w.Body.String() != "Bad Gateway\n" || !errors.As(outcome.Err, &timeout) || !timeout.Timeout() || outcome.Aborted {
		t.Fatalf("response=%d %q outcome=%+v", w.Code, w.Body.String(), outcome)
	}
	waitSafety(t, canceled)
}

func TestClientCancellationReachesUpstreamAndCompletion(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting for headers", true: "streaming response"}[streaming], func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: first\n\n")
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()
			outcomes := make(chan gateway.Outcome, 2)
			var completions atomic.Int32
			unwound := false
			app, err := gateway.NewWithConfig(gateway.Config{
				Routes: []gateway.Route{testRoute(t, upstream.URL)}, Transport: safetyTransport(t),
				Middlewares: []gateway.Middleware{func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { unwound = true }()
						next.ServeHTTP(w, r)
					})
				}},
				Extensions: []any{&testExtension{complete: func(ctx context.Context, _ *gateway.RequestContext, outcome gateway.Outcome) {
					if !unwound || ctx.Err() == nil {
						t.Error("completion lost middleware unwind or entry cancellation")
					}
					completions.Add(1)
					outcomes <- outcome
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(app)
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/slow", nil)
			clientDone := make(chan error, 1)
			firstRead := make(chan error, 1)
			client := &http.Client{Timeout: 5 * time.Second}
			go func() {
				response, err := client.Do(r)
				if err == nil {
					if streaming {
						_, err = io.ReadFull(response.Body, make([]byte, len("data: first\n\n")))
						firstRead <- err
					}
					if err == nil {
						_, err = io.ReadAll(response.Body)
					}
					_ = response.Body.Close()
				} else if streaming {
					firstRead <- err
				}
				clientDone <- err
			}()
			waitSafety(t, entered)
			if streaming {
				if err := waitSafety(t, firstRead); err != nil {
					t.Fatalf("first streamed event: %v", err)
				}
			}
			cancel()
			if err := waitSafety(t, clientDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("client error = %v", err)
			}
			waitSafety(t, canceled)
			outcome := waitSafety(t, outcomes)
			if completions.Load() != 1 {
				t.Fatal("completion did not run exactly once")
			}
			if !streaming && (!errors.Is(outcome.Err, context.Canceled) || outcome.Aborted) {
				t.Fatalf("unexpected header-wait cancellation outcome: %+v", outcome)
			}
			if streaming && (!outcome.Aborted || outcome.Err != nil) {
				t.Fatalf("stream abort was hidden or invented a proxy callback error: %+v", outcome)
			}
		})
	}
}

func TestCancellationWhileWaitingForConnectionPool(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	defer close(release)
	transport := safetyTransport(t)
	transport.MaxConnsPerHost = 1
	app, err := gateway.NewWithConfig(gateway.Config{Routes: []gateway.Route{testRoute(t, upstream.URL)}, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	firstDone := make(chan int, 1)
	go func() {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/first", nil).WithContext(ctx))
		firstDone <- w.Code
	}()
	waitSafety(t, entered)
	queuedCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/queued", nil).WithContext(queuedCtx))
	if w.Code != 502 || w.Body.String() != "Bad Gateway\n" || calls.Load() != 1 || queuedCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("queued request escaped pool cap/cancellation: response=%d %q calls=%d", w.Code, w.Body.String(), calls.Load())
	}
	// Cancel the first request too, then join its synchronous handling.
	cancel()
	if status := waitSafety(t, firstDone); status != 502 {
		t.Fatalf("first request status = %d", status)
	}
}

func TestRequestBodyStreamsBeforeClientEOF(t *testing.T) {
	first := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 3)
		_, err := io.ReadFull(r.Body, buf)
		if err != nil {
			first <- err.Error()
			return
		}
		first <- string(buf)
		rest, err := io.ReadAll(r.Body)
		if err != nil || string(rest) != "def" {
			t.Errorf("remaining body = %q, error = %v", rest, err)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	route := testRoute(t, upstream.URL)
	route.Middlewares = []gateway.Middleware{bodylimit.New(6)}
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{route}, Transport: safetyTransport(t),
		Middlewares: []gateway.Middleware{admission.New(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	result := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Post(server.URL+"/api/upload", "application/octet-stream", reader)
		if err == nil {
			_, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != 200 {
				err = errors.New(response.Status)
			}
		}
		result <- err
	}()
	if _, err := io.WriteString(writer, "abc"); err != nil {
		t.Fatal(err)
	}
	if got := waitSafety(t, first); got != "abc" {
		t.Fatalf("upstream first bytes = %q", got)
	}
	// The upload remains incremental and holds its slot while waiting for more
	// client bytes. Admission must not buffer it or release the slot early.
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/api/overflow")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 503 || readErr != nil || string(body) != "Service Unavailable\n" {
		t.Fatalf("slow upload lost admission slot: status=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
	if _, err := io.WriteString(writer, "def"); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := waitSafety(t, result); err != nil {
		t.Fatal(err)
	}
}
