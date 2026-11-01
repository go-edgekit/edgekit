package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/accesslog"
	"example.com/go-gateway-starter/extensions/bodylimit"
	"example.com/go-gateway-starter/extensions/headers"
)

func main() {
	upstreamAddress := env("GATEWAY_UPSTREAM", "http://127.0.0.1:9090")
	upstream, err := url.Parse(upstreamAddress)
	if err != nil {
		slog.Error("invalid upstream")
		os.Exit(1)
	}

	transport := newUpstreamTransport()
	defer transport.CloseIdleConnections()
	access := accesslog.New(accesslog.Config{Report: logCompletion})
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{{
			Prefix:   "/api/",
			Upstream: upstream,
			Middlewares: []gateway.Middleware{
				bodylimit.New(8 << 20), // Example policy: 8 MiB per request body.
				headers.AddResponseHeader("X-Gateway", "starter"),
			},
		}},
		Middlewares: []gateway.Middleware{access.Middleware},
		Extensions:  []any{access},
		Transport:   transport,
	})
	if err != nil {
		slog.Error("gateway configuration failed", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              env("GATEWAY_LISTEN_ADDR", ":8080"),
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		slog.Info("gateway listening", "address", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("gateway server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("gateway shutdown failed", "error", err)
	}
}

// newUpstreamTransport defines example deployment policy, not framework defaults.
func newUpstreamTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Static upstreams do not use ambient proxy variables.
	transport.DialContext = (&net.Dialer{
		Timeout: 3 * time.Second, KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 10 * time.Second
	transport.ExpectContinueTimeout = time.Second
	transport.IdleConnTimeout = 90 * time.Second
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 16
	transport.MaxConnsPerHost = 64
	return transport
}

func logCompletion(record accesslog.Record) {
	// Paths and arbitrary errors may contain secrets. Keep them out of this sink.
	slog.Info("request completed", "request_id", record.RequestID, "trace_id", record.TraceID,
		"method", record.Method, "route", record.RoutePrefix,
		"upstream", record.Upstream, "upstream_status", record.UpstreamStatusCode,
		"status", record.StatusCode, "bytes", record.BytesWritten, "result", record.Result,
		"duration", record.Outcome.Duration, "has_error", record.Outcome.Err != nil,
		"has_response_error", record.ResponseError != nil, "aborted", record.Outcome.Aborted,
		"hijacked", record.Hijacked)
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
