// Package accesslog combines HTTP accounting with gateway completion metadata.
// It does not depend on a logging backend or tracing SDK.
package accesslog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"

	gateway "example.com/go-gateway-starter"
)

// Result classifies the observations available to this extension. Proxy errors
// include outbound-hook, transport, and response-hook errors. They do not identify
// an exact failure phase or prove that a request reached the upstream.
type Result string

const (
	LocalResponse    Result = "local_response"
	UpstreamResponse Result = "upstream_response"
	Rejected         Result = "rejected"
	GatewayError     Result = "gateway_error"
	ProxyError       Result = "proxy_error"
	WriteError       Result = "write_error"
	Aborted          Result = "aborted"
)

// Record is a completion snapshot. Its request fields omit bodies, query strings,
// authorization headers, and upstream credentials. Path and errors can still
// contain sensitive data; the callback owns redaction and storage policy.
type Record struct {
	RequestID   string
	TraceID     string
	Method      string
	Path        string
	RoutePrefix string
	// Upstream is the selected origin, without credentials or query parameters.
	// Selection does not prove that the upstream received the request.
	Upstream string
	// UpstreamStatusCode is observed at this extension's response hook. Later
	// hooks can modify or reject the response before it reaches the client.
	UpstreamStatusCode int
	// StatusCode is the final status observed through ResponseWriter. Zero
	// means none was observed, such as an early panic or raw hijack.
	StatusCode int
	// BytesWritten counts body bytes accepted by ResponseWriter. It excludes
	// headers, framing, and raw hijacked traffic, and does not prove delivery.
	BytesWritten  int64
	ResponseError error
	Hijacked      bool
	Result        Result
	Outcome       gateway.Outcome
}

// Config configures an access-log extension. Both callbacks may run concurrently
// for different requests and must be safe for concurrent calls.
type Config struct {
	// Report receives one snapshot after gateway handling unwinds. A nil
	// callback disables reporting. The callback must not panic.
	Report func(Record)
	// TraceID extracts correlation from the middleware's standard context.
	// Put tracing middleware before this extension so its derived context is
	// visible. Only the ID is retained; this extension does not own the span.
	TraceID func(context.Context) string
}

// Extension observes requests that enter its registered Middleware. Place it
// before rejecting middleware and register the same instance as an extension.
// Like the gateway lifecycle, it requires synchronous downstream execution.
type Extension struct {
	config Config
	key    *gateway.StateKey[*requestState]
}

type requestState struct {
	record   Record
	response *responseCapture
}

type requestIDKey struct{}

// New creates an access-log extension. Configure it once before serving.
func New(config Config) *Extension {
	return &Extension{config: config, key: gateway.NewStateKey[*requestState]()}
}

// RequestIDFromContext returns the ID created by this extension's middleware.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok
}

// Middleware creates a random request ID and observes downstream HTTP writes.
// It replaces untrusted inbound X-Request-ID values and forwards the generated
// ID in both the request and response. Outside a gateway it passes through.
// If rand.Read returns an error, generating the request ID panics.
func (e *Extension) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc, ok := gateway.FromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		response := &responseCapture{ResponseWriter: w}
		state := &requestState{
			record:   Record{Method: r.Method, Path: r.URL.Path},
			response: response,
		}
		gateway.SetState(rc, e.key, state)

		id := newRequestID()
		state.record.RequestID = id
		response.requestID = id
		r = r.Clone(context.WithValue(r.Context(), requestIDKey{}, id))
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Request-ID", id)
		if e.config.TraceID != nil {
			state.record.TraceID = e.config.TraceID(r.Context())
		}

		next.ServeHTTP(response.wrap(), r)
		if response.status == 0 && !response.hijacked {
			// net/http sends an implicit 200 when a handler returns without writing.
			response.status = http.StatusOK
		}
	})
}

// OnUpstreamResponse records the upstream status and prevents an upstream from
// replacing this extension's response request ID. It also sees 101 upgrades.
func (e *Extension) OnUpstreamResponse(_ context.Context, rc *gateway.RequestContext, response *http.Response) error {
	if state, ok := gateway.GetState(rc, e.key); ok {
		state.record.UpstreamStatusCode = response.StatusCode
		response.Header.Del("X-Request-ID")
		// Forwarded informational responses may have cleared downstream headers.
		state.response.Header().Set("X-Request-ID", state.record.RequestID)
	}
	return nil
}

// OnComplete joins middleware observations with final gateway metadata. Trace
// correlation comes from the middleware snapshot, not this entry context.
func (e *Extension) OnComplete(_ context.Context, rc *gateway.RequestContext, outcome gateway.Outcome) {
	state, ok := gateway.GetState(rc, e.key)
	if !ok || e.config.Report == nil {
		return
	}
	record := state.record
	record.RoutePrefix, _ = rc.RoutePrefix()
	if upstream, ok := rc.Upstream(); ok {
		record.Upstream = upstream.Scheme + "://" + upstream.Host
	}
	record.StatusCode = state.response.status
	record.BytesWritten = state.response.bytes
	record.ResponseError = state.response.err
	record.Hijacked = state.response.hijacked
	record.Outcome = outcome
	record.Result = classify(record)
	e.config.Report(record)
}

func classify(record Record) Result {
	if record.Outcome.Aborted {
		return Aborted
	}
	if record.ResponseError != nil {
		return WriteError
	}
	if record.Outcome.Err != nil {
		var rejection *gateway.HTTPError
		if errors.As(record.Outcome.Err, &rejection) && rejection != nil && rejection.StatusCode >= 400 && rejection.StatusCode <= 599 {
			return Rejected
		}
		if record.Upstream != "" {
			return ProxyError
		}
		return GatewayError
	}
	if record.Upstream != "" {
		return UpstreamResponse
	}
	if record.StatusCode >= 400 && record.StatusCode < 500 {
		return Rejected
	}
	return LocalResponse
}

func newRequestID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Errorf("accesslog: generate request ID: %w", err))
	}
	return hex.EncodeToString(bytes[:])
}
