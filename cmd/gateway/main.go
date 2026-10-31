package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/headers"
	"example.com/go-gateway-starter/extensions/requesttag"
)

func main() {
	upstreamAddress := env("GATEWAY_UPSTREAM", "http://127.0.0.1:9090")
	upstream, err := url.Parse(upstreamAddress)
	if err != nil {
		slog.Error("invalid upstream", "error", err)
		os.Exit(1)
	}

	tag := requesttag.New("example", func(record requesttag.Record) {
		slog.Info("request completed", "tag", record.Tag, "route", record.RoutePrefix,
			"proxied", record.Proxied, "duration", record.Outcome.Duration,
			"error", record.Outcome.Err, "aborted", record.Outcome.Aborted)
	})
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{{
			Prefix:   "/api/",
			Upstream: upstream,
			Middlewares: []gateway.Middleware{
				headers.AddResponseHeader("X-Gateway", "starter"),
			},
		}},
		Middlewares: []gateway.Middleware{tag.Middleware, requestLogger},
		Extensions:  []any{tag},
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
		slog.Info("gateway listening", "address", server.Addr, "upstream", upstreamAddress)
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

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Info("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
