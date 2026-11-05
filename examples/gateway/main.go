// Command gateway is a small gateway that uses only the public API of
// edgekit. It shows the hardened server, graceful shutdown, and failure
// reports that contain no configuration values and no error text.
//
// Environment:
//
//	EDGEKIT_LISTEN_ADDR  listen address, default ":8080"
//	EDGEKIT_UPSTREAM     upstream URL for the route "/api/", default "http://127.0.0.1:9090"
//
// The server serves plaintext HTTP/1.1. Stop it with SIGINT or SIGTERM.
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

	"github.com/go-edgekit/edgekit"
)

// shutdownTimeout limits the graceful shutdown. After it, the server stops
// the remaining requests.
const shutdownTimeout = 10 * time.Second

// main exits with status 1 when the gateway fails. The failure is in the
// log already.
func main() {
	if err := serve(); err != nil {
		os.Exit(1)
	}
}

// serve configures the logger and runs the gateway until a signal arrives.
func serve() error {
	// The server sends its diagnostics to the default logger.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return run(ctx, config{
		listenAddr: env("EDGEKIT_LISTEN_ADDR", ":8080"),
		upstream:   env("EDGEKIT_UPSTREAM", "http://127.0.0.1:9090"),
	}, logger, nil)
}

// config is the configuration from the environment.
type config struct {
	listenAddr string
	upstream   string
}

// run serves until ctx ends, and then shuts down. It reports each failure
// with reportFailure and returns it. When ready is not nil, run sends the
// listen address to it.
func run(ctx context.Context, cfg config, logger *slog.Logger, ready chan<- net.Addr) error {
	target, err := url.Parse(cfg.upstream)
	if err != nil {
		return reportFailure(logger, "upstream URL is not valid", err)
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		return reportFailure(logger, "upstream URL is not valid", err)
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: selector,
			},
		},
	})
	if err != nil {
		return reportFailure(logger, "gateway configuration is not valid", err)
	}
	defer gateway.CloseIdleConnections()

	// The zero ServerConfig applies all secure defaults.
	server, err := edgekit.NewServer(gateway, edgekit.ServerConfig{})
	if err != nil {
		return reportFailure(logger, "server configuration is not valid", err)
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", cfg.listenAddr)
	if err != nil {
		return reportFailure(logger, "listen failed", err)
	}

	served := make(chan error, 1)
	go func() {
		served <- server.Serve(listener)
	}()

	logger.Info("gateway listening")
	if ready != nil {
		ready <- listener.Addr()
	}

	select {
	case err = <-served:
		return reportFailure(logger, "server failed", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		// Stop the requests that did not complete in time.
		_ = server.Close()
	}

	if err = <-served; !errors.Is(err, http.ErrServerClosed) {
		return reportFailure(logger, "server failed", err)
	}

	if shutdownErr != nil {
		return reportFailure(logger, "graceful shutdown did not complete", shutdownErr)
	}

	logger.Info("gateway stopped")

	return nil
}

// reportFailure logs event with a classification of err, and returns err.
// Events are static text. The log never contains the error text, because
// errors can contain configuration values, addresses, or credentials.
func reportFailure(logger *slog.Logger, event string, err error) error {
	logger.Error(event, "error_kind", errorKind(err))

	return err
}

// errorKind classifies err without formatting it.
func errorKind(err error) string {
	var netErr net.Error
	var opErr *net.OpError

	switch {
	case errors.Is(err, edgekit.ErrInvalidConfiguration):
		return "invalid_configuration"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, http.ErrServerClosed):
		return "server_closed"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.As(err, &opErr):
		return "network"
	default:
		return "other"
	}
}

// env returns the value of the environment variable name, or fallback when
// it is empty.
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
