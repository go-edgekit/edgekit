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
	"example.com/go-gateway-starter/internal/safelog"
)

func main() {
	safelog.ConfigureDefault(os.Stderr)
	upstreamAddress := env("GATEWAY_UPSTREAM", "http://127.0.0.1:9090")
	upstream, err := url.Parse(upstreamAddress)
	if err != nil {
		logFailure("invalid upstream", err)
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
		logFailure("gateway configuration failed", err)
		os.Exit(1)
	}

	server := newHTTPServer(app)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		slog.Info("gateway listening")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logFailure("gateway server failed", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logFailure("gateway shutdown failed", err)
	}
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              env("GATEWAY_LISTEN_ADDR", ":8080"),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          safelog.New("http_server"),
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
	// Retain observations without echoing paths, origins, route labels, trace
	// values, or arbitrary errors. Only this extension's generated ID is logged.
	slog.Info("request completed", "request_id", safeRequestID(record.RequestID),
		"method", safeMethod(record.Method), "route_matched", record.RoutePrefix != "",
		"upstream_selected", record.Upstream != "", "upstream_status", record.UpstreamStatusCode,
		"status", record.StatusCode, "bytes", record.BytesWritten, "result", safeResult(record.Result),
		"duration", record.Outcome.Duration, "has_error", record.Outcome.Err != nil,
		"error_kind", safelog.ErrorKind(record.Outcome.Err),
		"has_response_error", record.ResponseError != nil, "response_error_kind", safelog.ErrorKind(record.ResponseError),
		"aborted", record.Outcome.Aborted, "hijacked", record.Hijacked)
}

func logFailure(event string, err error) {
	// Callers use static event labels, never arbitrary error messages.
	slog.Error(event, "error_kind", safelog.ErrorKind(err))
}

func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return method
	default:
		return "OTHER"
	}
}

func safeResult(result accesslog.Result) string {
	switch result {
	case accesslog.LocalResponse, accesslog.UpstreamResponse, accesslog.Rejected, accesslog.GatewayError,
		accesslog.ProxyError, accesslog.WriteError, accesslog.Aborted:
		return string(result)
	default:
		return "unknown"
	}
}

func safeRequestID(id string) string {
	if len(id) != 32 {
		return ""
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return ""
		}
	}
	return id
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
