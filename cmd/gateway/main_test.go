package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/accesslog"
)

func TestUpstreamTransportPolicy(t *testing.T) {
	transport := newUpstreamTransport()
	defer transport.CloseIdleConnections()
	if transport.Proxy != nil || transport.DialContext == nil ||
		transport.TLSHandshakeTimeout != 5*time.Second || transport.ResponseHeaderTimeout != 10*time.Second ||
		transport.ExpectContinueTimeout != time.Second || transport.IdleConnTimeout != 90*time.Second ||
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
	app, err := gateway.NewWithConfig(gateway.Config{Routes: []gateway.Route{{Prefix: "/api/", Upstream: target}}, Transport: transport})
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

func TestCompletionLogOmitsPathsAndRawErrors(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	logCompletion(accesslog.Record{
		RequestID: "generated-id", Path: "/api/path-secret", RoutePrefix: "/api/", StatusCode: 502,
		Outcome: gateway.Outcome{Err: errors.New("error-secret")}, ResponseError: errors.New("write-secret"),
	})
	for _, secret := range []string{"path-secret", "error-secret", "write-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, output.String())
		}
	}
	if !strings.Contains(output.String(), `"has_error":true`) || !strings.Contains(output.String(), `"has_response_error":true`) {
		t.Fatalf("lost failure observation: %s", output.String())
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
