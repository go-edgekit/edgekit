package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
	"github.com/go-edgekit/edgekit/internal/safelog"
)

func TestUpstreamTransportPolicy(t *testing.T) {
	transport := newUpstreamTransport()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || transport.DialContext == nil ||
		transport.TLSHandshakeTimeout != 5*time.Second || transport.ResponseHeaderTimeout != 10*time.Second ||
		transport.ExpectContinueTimeout != time.Second || transport.IdleConnTimeout != 90*time.Second || transport.MaxResponseHeaderBytes != 64<<10 ||
		transport.MaxIdleConns != 100 || transport.MaxIdleConnsPerHost != 16 || transport.MaxConnsPerHost != 64 ||
		transport.DisableKeepAlives || !transport.ForceAttemptHTTP2 {
		t.Fatalf("unexpected example transport policy: %+v", transport)
	}
}

func TestHeaderTimeoutDoesNotLimitResponseStream(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer upstream.Close()
	transport := newUpstreamTransport()
	transport.ResponseHeaderTimeout = 50 * time.Millisecond
	defer transport.CloseIdleConnections()
	target, _ := url.Parse(upstream.URL)
	app, err := edgekit.NewWithConfig(edgekit.Config{Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}}, Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	defer finish()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "data: first\n" {
		t.Fatalf("first event was buffered: %q, %v", line, err)
	}
	// Keep the body open beyond the header budget. There is no body deadline.
	time.Sleep(3 * transport.ResponseHeaderTimeout)
	finish()
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "\ndata: last\n\n" {
		t.Fatalf("stream did not survive header timeout: %q, %v", rest, err)
	}
}

func TestTLSHandshakeTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Accept TCP without replying to the TLS ClientHello.
		<-release
	}()
	transport := newUpstreamTransport()
	transport.TLSHandshakeTimeout = 50 * time.Millisecond
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("https://" + listener.Addr().String())
	if response != nil {
		_ = response.Body.Close()
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() || !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Fatalf("expected TLS timeout, got %v", err)
	}
	// Unblock and join the listener goroutine before leaving the test.
	finish()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not exit")
	}
}

func captureExampleLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous, writer, flags, prefix := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	var output bytes.Buffer
	safelog.ConfigureDefault(&output)
	return &output
}

func TestCompletionLogUsesOnlyApprovedFields(t *testing.T) {
	output := captureExampleLogs(t)
	failure := &url.Error{Op: "GET", URL: "https://user:credential-secret@host/path-secret?token=query-secret", Err: errors.New("error-secret")}
	const id = "0123456789abcdef0123456789abcdef"
	logCompletion(accesslog.Record{
		RequestID: id, TraceID: "trace-secret", Method: "method-secret", Path: "/api/path-secret",
		RoutePrefix: "/route-secret/", Upstream: "https://origin-secret", StatusCode: 502, Result: "result-secret",
		Outcome: edgekit.Outcome{Err: failure}, ResponseError: errors.New("write-secret"),
	})
	if strings.Contains(output.String(), "secret") {
		t.Fatalf("completion log exposed a sensitive field: %s", output.String())
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"request_id": id, "method": "OTHER", "result": "unknown", "route_matched": true,
		"upstream_selected": true, "has_error": true, "has_response_error": true,
		"error_kind": "other", "response_error_kind": "other", "status": float64(502),
	} {
		if record[key] != want {
			t.Errorf("%s=%v, want %v", key, record[key], want)
		}
	}
	if !strings.Contains(failure.Error(), "credential-secret") {
		t.Fatal("logging changed the original error")
	}
}

func TestCompletionLogValueAllowlists(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch} {
		if safeMethod(method) != method {
			t.Errorf("lost standard method %s", method)
		}
	}
	for _, result := range []accesslog.Result{accesslog.LocalResponse, accesslog.UpstreamResponse, accesslog.Rejected, accesslog.GatewayError, accesslog.ProxyError, accesslog.WriteError, accesslog.Aborted} {
		if safeResult(result) != string(result) {
			t.Errorf("lost known result %s", result)
		}
	}
	for _, id := range []string{"", "secret", strings.Repeat("g", 32), strings.Repeat("A", 32)} {
		if safeRequestID(id) != "" {
			t.Errorf("accepted non-generated ID %q", id)
		}
	}
}

func TestFailureLogsDoNotEchoConfigurationOrErrors(t *testing.T) {
	output := captureExampleLogs(t)
	for _, event := range []string{"invalid upstream", "gateway configuration failed", "gateway server failed", "gateway shutdown failed"} {
		logFailure(event, &url.Error{Op: "GET", URL: "https://user:credential-secret@host/path-secret?token=query-secret", Err: errors.New("failure-secret")})
	}
	logFailure("gateway shutdown failed", fmt.Errorf("private-secret: %w", context.DeadlineExceeded))
	if strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), `"error_kind":"deadline_exceeded"`) || !strings.Contains(output.String(), `"error_kind":"other"`) {
		t.Fatalf("unsafe or missing failure classification: %s", output.String())
	}
}

type completionObserver func(edgekit.Outcome)

func (f completionObserver) OnComplete(_ context.Context, _ *edgekit.RequestContext, outcome edgekit.Outcome) {
	f(outcome)
}

func TestServerDiagnosticsDoNotChangeCompletionOrEchoDetails(t *testing.T) {
	for _, mode := range []string{"panic", "abort", "invalid header", "normal"} {
		t.Run(mode, func(t *testing.T) {
			output := captureExampleLogs(t)
			unwound := false
			outcomes := make(chan edgekit.Outcome, 2)
			target, _ := url.Parse("http://unused.test")
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}},
				Middlewares: []edgekit.Middleware{func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer func() { unwound = true }()
						switch mode {
						case "panic":
							panic("https://user:credential-secret@host/path-secret?token=query-secret\nstack-secret")
						case "abort":
							panic(http.ErrAbortHandler)
						case "invalid header":
							w.Header().Set("Content-Length", "header-secret")
						}
						_, _ = io.WriteString(w, "public")
					})
				}},
				Extensions: []any{completionObserver(func(outcome edgekit.Outcome) {
					if !unwound {
						t.Error("completion preceded middleware unwind")
					}
					outcomes <- outcome
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(app)
			server.Config = newHTTPServer(app)
			if server.Config.ErrorLog == nil || server.Config.ReadHeaderTimeout != 5*time.Second || server.Config.IdleTimeout != 60*time.Second || server.Config.MaxHeaderBytes != 64<<10 || server.Config.WriteTimeout != 0 || server.Config.ReadTimeout != 0 {
				t.Fatal("server diagnostics changed timeout policy or were not installed")
			}
			server.Start()
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Get(server.URL + "/api/path-secret?token=query-secret")
			aborted := mode == "panic" || mode == "abort"
			if aborted {
				if response != nil {
					_ = response.Body.Close()
				}
				if err == nil {
					t.Fatal("server panic was swallowed or converted into a response")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.StatusCode != 200 || readErr != nil || string(body) != "public" {
					t.Fatalf("response changed: status=%d body=%q err=%v", response.StatusCode, body, readErr)
				}
			}
			server.Close()
			select {
			case outcome := <-outcomes:
				if outcome.Aborted != aborted || outcome.Err != nil || len(outcomes) != 0 {
					t.Fatalf("server diagnostics changed completion: %+v", outcome)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("completion did not run")
			}
			if mode == "normal" || mode == "abort" {
				if output.Len() != 0 {
					t.Fatalf("invented a diagnostic: %s", output.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatalf("missing server diagnostic: %v output=%q", err, output.String())
			}
			if record["source"] != "http_server" || record["msg"] != "diagnostic details omitted" || len(record) != 4 || strings.Contains(output.String(), "secret") {
				t.Fatalf("unsafe server diagnostic: %s", output.String())
			}
		})
	}
}

// Verify the dial function preserves caller cancellation, including when no
// connection has been established. It must not detach from the request context.
func TestUpstreamDialRespectsCanceledContext(t *testing.T) {
	transport := newUpstreamTransport()
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := transport.DialContext(ctx, "tcp", "127.0.0.1:1")
	if conn != nil {
		_ = conn.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error = %v", err)
	}
}
