// Command gateway is a small gateway that uses only the public API of
// edgekit. It shows the hardened server, graceful shutdown, failure reports
// that contain no configuration values and no error text, the access-log
// extension with a completion log of approved fields, and the outbound-signing
// demonstration.
//
// Environment:
//
//	EDGEKIT_LISTEN_ADDR  listen address, default ":8080"
//	EDGEKIT_UPSTREAM     upstream URL for the route "/api/", default "http://127.0.0.1:9090"
//	EDGEKIT_SIGNING_KEY  key that signs the requests to the upstream, at least 32 bytes, required
//
// Make a random signing key with "openssl rand -hex 32". The program uses the
// 64 hexadecimal characters as the key bytes, so the key has 256 random bits.
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
	"github.com/go-edgekit/edgekit/extensions/accesslog"
	"github.com/go-edgekit/edgekit/extensions/outboundsign"
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
		signingKey: []byte(os.Getenv("EDGEKIT_SIGNING_KEY")),
	}, logger, nil)
}

// config is the configuration from the environment.
type config struct {
	listenAddr string
	upstream   string
	signingKey []byte
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

	signer := outboundsign.New()
	withKey, err := signer.WithKey(cfg.signingKey)
	if err != nil {
		return reportFailure(logger, "signing key is not valid", err)
	}

	recorder, err := accesslog.New(accesslog.Config{
		Sink: accesslog.SinkFunc(func(record accesslog.Record) {
			logCompletion(logger, record)
		}),
	})
	if err != nil {
		return reportFailure(logger, "access log configuration is not valid", err)
	}

	// The recorder is the first middleware so it observes inner responses.
	// The signer is last in BeforeUpstream so it signs all header changes.
	gateway, err := edgekit.New(edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				recorder,
				signer,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				recorder,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				recorder,
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			recorder,
			signer,
		},
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: selector,
				Middleware: []edgekit.Middleware{
					withKey,
				},
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
	logger.Error(event, slog.String("error_kind", errorKind(err)))

	return err
}

// errorKind classifies err without formatting it.
func errorKind(err error) string {
	var netErr net.Error
	var opErr *net.OpError

	switch {
	case err == nil:
		return "none"
	case errors.Is(err, edgekit.ErrInvalidConfiguration), errors.Is(err, outboundsign.ErrShortKey):
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

// logCompletion writes the access-log record of one request. Each value is a
// number, a boolean, or a label from an allowlist. The log contains no path,
// no origin, no route prefix, no trace ID, and no error text, because these
// can contain request data, configuration values, or credentials.
func logCompletion(logger *slog.Logger, record accesslog.Record) {
	c := record.Completion

	var failureErr error
	if c.Failure != nil {
		failureErr = c.Failure.Err
	}

	// A Sink gets no context (see accesslog.Sink). The record holds all values
	// of the log.
	logger.LogAttrs(context.Background(), slog.LevelInfo, "request completed",
		slog.Bool("observed", record.Observed),
		slog.String("request_id", safeRequestID(record.RequestID)),
		slog.String("method", safeMethod(record.Method)),
		slog.Int("status", record.Status),
		slog.Int64("bytes", record.BytesWritten),
		slog.Int("upstream_status", record.UpstreamStatus),
		slog.Duration("duration", c.Duration),
		slog.Bool("route_matched", c.RoutePrefix != ""),
		slog.Bool("upstream_selected", c.SelectedUpstream != nil),
		slog.String("rejection_reason", safeReason(c.RejectionReason)),
		slog.String("failure_phase", failurePhase(c.Failure)),
		slog.String("error_kind", errorKind(failureErr)),
		slog.String("write_error_kind", errorKind(record.WriteErr)),
		slog.Bool("aborted", c.Aborted),
		slog.Bool("hijacked", record.Hijacked),
		slog.Bool("connection_obtained", c.ConnectionObtained),
		slog.Bool("request_written", c.RequestWritten),
	)
}

// safeMethod gives the standard methods of net/http, or "OTHER".
func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

// safeReason gives the rejection reasons of the core, "none", or "other".
// The program registers no extension that rejects, so another reason is not
// expected. The core exports no list of its reasons: keep this list equal to
// the Reason constants, or a new core reason is logged as "other".
func safeReason(reason edgekit.RejectionReason) string {
	switch reason {
	case "":
		return "none"
	case edgekit.ReasonInvalidMethod, edgekit.ReasonInvalidRequestTarget, edgekit.ReasonInvalidPath,
		edgekit.ReasonInvalidQuery, edgekit.ReasonInvalidHeaderName, edgekit.ReasonInvalidUpgrade,
		edgekit.ReasonNoRoute, edgekit.ReasonBodyTooLarge, edgekit.ReasonAdmissionSaturated:
		return string(reason)
	default:
		return "other"
	}
}

// failurePhase gives the phase name of failure, "none", or "other".
func failurePhase(failure *edgekit.Failure) string {
	if failure == nil {
		return "none"
	}

	switch failure.Phase {
	case edgekit.PhaseUnknown, edgekit.PhaseRequestReceived, edgekit.PhaseUpstreamSelection,
		edgekit.PhaseBeforeUpstream, edgekit.PhaseRoundTrip, edgekit.PhaseUpstreamResponse,
		edgekit.PhaseProtocolSwitch, edgekit.PhaseResponseBody:
		return failure.Phase.String()
	default:
		return "other"
	}
}

// safeRequestID gives id when it has the form of a request ID of the access
// log, 32 lowercase hexadecimal digits, "none" when it is empty, and
// "invalid" otherwise.
func safeRequestID(id string) string {
	if id == "" {
		return "none"
	}

	if len(id) != 32 {
		return "invalid"
	}

	for i := range len(id) {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "invalid"
		}
	}

	return id
}

// env returns the value of the environment variable name, or fallback when
// it is empty.
func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	return fallback
}
