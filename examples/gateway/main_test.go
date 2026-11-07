package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
	"github.com/go-edgekit/edgekit/extensions/outboundsign"
)

// testKey is a signing key of the minimum length.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// completionKeys are the keys of a completion log record: the approved
// fields and the keys of slog.
var completionKeys = []string{
	"aborted",
	"bytes",
	"connection_obtained",
	"duration",
	"error_kind",
	"failure_phase",
	"hijacked",
	"level",
	"method",
	"msg",
	"observed",
	"rejection_reason",
	"request_id",
	"request_written",
	"route_matched",
	"status",
	"time",
	"upstream_selected",
	"upstream_status",
	"write_error_kind",
}

// records decodes the JSON log records in output.
func records(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()

	var result []map[string]any

	decoder := json.NewDecoder(output)
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}

		result = append(result, record)
	}

	return result
}

func TestFailureReportsGiveClassificationOnly(t *testing.T) {
	secretURL := &url.Error{
		Op:  "Get",
		URL: "https://host-secret/path-secret?token=query-secret",
		Err: errors.New("failure-secret"),
	}

	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{
			name: "deadline",
			err:  fmt.Errorf("private-secret: %w", context.DeadlineExceeded),
			kind: "deadline_exceeded",
		},
		{
			name: "invalid configuration",
			err:  fmt.Errorf("%w: route-secret", edgekit.ErrInvalidConfiguration),
			kind: "invalid_configuration",
		},
		{
			name: "network",
			err: &net.OpError{
				Op:  "listen",
				Net: "tcp",
				Err: errors.New("address-secret"),
			},
			kind: "network",
		},
		{
			name: "other",
			err:  secretURL,
			kind: "other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))

			if got := reportFailure(logger, "server failed", tc.err); !errors.Is(got, tc.err) {
				t.Errorf("reportFailure() = %v, want the original error", got)
			}

			if strings.Contains(output.String(), "secret") {
				t.Errorf("log contains error details: %s", output.String())
			}

			logged := records(t, &output)
			if len(logged) != 1 {
				t.Fatalf("records = %d, want 1", len(logged))
			}

			record := logged[0]
			if record["msg"] != "server failed" || record["error_kind"] != tc.kind || len(record) != 4 {
				t.Errorf("record = %v, want msg, error_kind %q, level, and time only", record, tc.kind)
			}
		})
	}
}

func TestStartupFailureIsReportedSafely(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
		msg  string
		kind string
	}{
		{
			name: "upstream URL",
			cfg: config{
				listenAddr: "127.0.0.1:0",
				upstream:   "https://host-secret/path-secret?token=query-secret",
			},
			msg:  "upstream URL is not valid",
			kind: "invalid_configuration",
		},
		{
			name: "listen address",
			cfg: config{
				listenAddr: "127.0.0.1:port-secret",
				upstream:   "http://127.0.0.1:9090",
				signingKey: testKey,
			},
			msg:  "listen failed",
			kind: "network",
		},
		{
			name: "no signing key",
			cfg: config{
				listenAddr: "127.0.0.1:0",
				upstream:   "http://127.0.0.1:9090",
			},
			msg:  "signing key is not valid",
			kind: "invalid_configuration",
		},
		{
			name: "short signing key",
			cfg: config{
				listenAddr: "127.0.0.1:0",
				upstream:   "http://127.0.0.1:9090",
				signingKey: []byte("key-secret-31-bytes-0123456789a"),
			},
			msg:  "signing key is not valid",
			kind: "invalid_configuration",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))

			if err := run(t.Context(), tc.cfg, logger, nil); err == nil {
				t.Fatal("run() error = nil, want a startup failure")
			}

			if strings.Contains(output.String(), "secret") {
				t.Errorf("log contains configuration values: %s", output.String())
			}

			logged := records(t, &output)
			if len(logged) != 1 || logged[0]["msg"] != tc.msg || logged[0]["error_kind"] != tc.kind {
				t.Errorf("records = %v, want one %q record with error_kind %q", logged, tc.msg, tc.kind)
			}
		})
	}
}

func TestRunShutsDownWhenContextEnds(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan net.Addr, 1)
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, config{
			listenAddr: "127.0.0.1:0",
			upstream:   "http://127.0.0.1:9090",
			signingKey: testKey,
		}, logger, ready)
	}()

	select {
	case <-ready:
	case err := <-result:
		t.Fatalf("run() = %v before it listened", err)
	}

	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Errorf("run() error = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run() did not return after the context ended")
	}

	var messages []any
	for _, record := range records(t, &output) {
		messages = append(messages, record["msg"])
	}

	if fmt.Sprint(messages) != "[gateway listening gateway stopped]" {
		t.Errorf("messages = %v, want listening and stopped", messages)
	}
}

func TestCompletionLogUsesOnlyApprovedFields(t *testing.T) {
	secretURL := url.URL{
		Scheme:   "https",
		User:     url.UserPassword("user", "credential-secret"),
		Host:     "host-secret",
		Path:     "/path-secret",
		RawQuery: "token=query-secret",
	}
	failure := &url.Error{
		Op:  "GET",
		URL: secretURL.String(),
		Err: errors.New("error-secret"),
	}
	writeErr := errors.New("write-secret")
	const id = "0123456789abcdef0123456789abcdef"

	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	logCompletion(logger, accesslog.Record{
		Observed:       true,
		RequestID:      id,
		Method:         "method-secret",
		Path:           "/api/path-secret",
		TraceID:        "trace-secret",
		Status:         http.StatusBadGateway,
		BytesWritten:   12,
		WriteErr:       writeErr,
		Hijacked:       true,
		UpstreamStatus: http.StatusCreated,
		Completion: edgekit.Completion{
			Duration:    time.Second,
			RoutePrefix: "/route-secret/",
			SelectedUpstream: &url.URL{
				Scheme:   "https",
				User:     url.UserPassword("user", "upstream-secret"),
				Host:     "origin-secret",
				Path:     "/base-secret",
				RawQuery: "q=query-secret",
			},
			RejectionReason:    "secret.reason",
			ConnectionObtained: true,
			Failure: &edgekit.Failure{
				Phase: edgekit.PhaseRoundTrip,
				Err:   failure,
			},
		},
	})

	if strings.Contains(output.String(), "secret") {
		t.Fatalf("completion log contains a sensitive value: %s", output.String())
	}

	logged := records(t, &output)
	if len(logged) != 1 {
		t.Fatalf("records = %d, want 1", len(logged))
	}

	record := logged[0]
	if keys := slices.Sorted(maps.Keys(record)); !slices.Equal(keys, completionKeys) {
		t.Errorf("keys = %v, want %v", keys, completionKeys)
	}

	for key, want := range map[string]any{
		"msg":                 "request completed",
		"observed":            true,
		"request_id":          id,
		"method":              "OTHER",
		"status":              float64(http.StatusBadGateway),
		"bytes":               float64(12),
		"upstream_status":     float64(http.StatusCreated),
		"duration":            float64(time.Second),
		"route_matched":       true,
		"upstream_selected":   true,
		"rejection_reason":    "other",
		"failure_phase":       "round_trip",
		"error_kind":          "other",
		"write_error_kind":    "other",
		"aborted":             false,
		"hijacked":            true,
		"connection_obtained": true,
		"request_written":     false,
	} {
		if record[key] != want {
			t.Errorf("%s = %v, want %v", key, record[key], want)
		}
	}

	// failure is the only value that logCompletion could change.
	if failure.URL != secretURL.String() {
		t.Errorf("failure.URL = %q, want the original URL %q", failure.URL, secretURL.String())
	}
}

func TestSafeMethod(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		want   string
	}{
		{
			name:   "GET",
			method: http.MethodGet,
			want:   http.MethodGet,
		},
		{
			name:   "HEAD",
			method: http.MethodHead,
			want:   http.MethodHead,
		},
		{
			name:   "POST",
			method: http.MethodPost,
			want:   http.MethodPost,
		},
		{
			name:   "PUT",
			method: http.MethodPut,
			want:   http.MethodPut,
		},
		{
			name:   "PATCH",
			method: http.MethodPatch,
			want:   http.MethodPatch,
		},
		{
			name:   "DELETE",
			method: http.MethodDelete,
			want:   http.MethodDelete,
		},
		{
			name:   "CONNECT",
			method: http.MethodConnect,
			want:   http.MethodConnect,
		},
		{
			name:   "OPTIONS",
			method: http.MethodOptions,
			want:   http.MethodOptions,
		},
		{
			name:   "TRACE",
			method: http.MethodTrace,
			want:   http.MethodTrace,
		},
		{
			name:   "empty",
			method: "",
			want:   "OTHER",
		},
		{
			name:   "lowercase",
			method: "get",
			want:   "OTHER",
		},
		{
			name:   "unlisted",
			method: "PROPFIND",
			want:   "OTHER",
		},
		{
			name:   "control_character",
			method: "GET\n",
			want:   "OTHER",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeMethod(tc.method); got != tc.want {
				t.Errorf("safeMethod(%q) = %q, want %q", tc.method, got, tc.want)
			}
		})
	}
}

func TestSafeReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason edgekit.RejectionReason
		want   string
	}{
		{
			name:   "invalid_method",
			reason: edgekit.ReasonInvalidMethod,
			want:   "invalid_method",
		},
		{
			name:   "invalid_request_target",
			reason: edgekit.ReasonInvalidRequestTarget,
			want:   "invalid_request_target",
		},
		{
			name:   "invalid_path",
			reason: edgekit.ReasonInvalidPath,
			want:   "invalid_path",
		},
		{
			name:   "invalid_query",
			reason: edgekit.ReasonInvalidQuery,
			want:   "invalid_query",
		},
		{
			name:   "invalid_header_name",
			reason: edgekit.ReasonInvalidHeaderName,
			want:   "invalid_header_name",
		},
		{
			name:   "invalid_upgrade",
			reason: edgekit.ReasonInvalidUpgrade,
			want:   "invalid_upgrade",
		},
		{
			name:   "no_route",
			reason: edgekit.ReasonNoRoute,
			want:   "no_route",
		},
		{
			name:   "body_too_large",
			reason: edgekit.ReasonBodyTooLarge,
			want:   "body_too_large",
		},
		{
			name:   "admission_saturated",
			reason: edgekit.ReasonAdmissionSaturated,
			want:   "admission_saturated",
		},
		{
			name:   "request_buffer_saturated",
			reason: edgekit.ReasonRequestBufferSaturated,
			want:   "request_buffer_saturated",
		},
		{
			name:   "empty",
			reason: "",
			want:   "none",
		},
		{
			name:   "extension",
			reason: "ext.check",
			want:   "other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeReason(tc.reason); got != tc.want {
				t.Errorf("safeReason(%q) = %q, want %q", tc.reason, got, tc.want)
			}
		})
	}
}

func TestFailurePhase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure *edgekit.Failure
		want    string
	}{
		{
			name:    "unknown",
			failure: &edgekit.Failure{Phase: edgekit.PhaseUnknown},
			want:    "unknown",
		},
		{
			name:    "route_matched",
			failure: &edgekit.Failure{Phase: edgekit.PhaseRouteMatched},
			want:    "route_matched",
		},
		{
			name:    "upstream_selection",
			failure: &edgekit.Failure{Phase: edgekit.PhaseUpstreamSelection},
			want:    "upstream_selection",
		},
		{
			name:    "before_upstream",
			failure: &edgekit.Failure{Phase: edgekit.PhaseBeforeUpstream},
			want:    "before_upstream",
		},
		{
			name:    "round_trip",
			failure: &edgekit.Failure{Phase: edgekit.PhaseRoundTrip},
			want:    "round_trip",
		},
		{
			name:    "upstream_response",
			failure: &edgekit.Failure{Phase: edgekit.PhaseUpstreamResponse},
			want:    "upstream_response",
		},
		{
			name:    "protocol_switch",
			failure: &edgekit.Failure{Phase: edgekit.PhaseProtocolSwitch},
			want:    "protocol_switch",
		},
		{
			name:    "response_body",
			failure: &edgekit.Failure{Phase: edgekit.PhaseResponseBody},
			want:    "response_body",
		},
		{
			name:    "request_buffering",
			failure: &edgekit.Failure{Phase: edgekit.PhaseRequestBuffering},
			want:    "request_buffering",
		},
		{
			name:    "no_failure",
			failure: nil,
			want:    "none",
		},
		{
			name:    "invalid",
			failure: &edgekit.Failure{Phase: edgekit.Phase(99)},
			want:    "other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failurePhase(tc.failure); got != tc.want {
				t.Errorf("failurePhase(%+v) = %q, want %q", tc.failure, got, tc.want)
			}
		})
	}
}

func TestSafeRequestID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{
			name: "generated",
			id:   "0123456789abcdef0123456789abcdef",
			want: "0123456789abcdef0123456789abcdef",
		},
		{
			name: "empty",
			id:   "",
			want: "none",
		},
		{
			name: "uppercase",
			id:   "0123456789ABCDEF0123456789ABCDEF",
			want: "invalid",
		},
		{
			name: "too_short",
			id:   "0123456789abcdef0123456789abcde",
			want: "invalid",
		},
		{
			name: "too_long",
			id:   "0123456789abcdef0123456789abcdef0",
			want: "invalid",
		},
		{
			name: "client_supplied",
			id:   "client-id-secret",
			want: "invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeRequestID(tc.id); got != tc.want {
				t.Errorf("safeRequestID(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestErrorKindNil(t *testing.T) {
	if got := errorKind(nil); got != "none" {
		t.Errorf("errorKind(nil) = %q, want none", got)
	}
}

// syncBuffer is a bytes.Buffer that the server goroutines and the test can
// use at the same time.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// snapshot returns a copy of the content.
func (b *syncBuffer) snapshot() *bytes.Buffer {
	b.mu.Lock()
	defer b.mu.Unlock()

	return bytes.NewBuffer(bytes.Clone(b.buf.Bytes()))
}

func TestRunSignsAndLogsRequests(t *testing.T) {
	signatures := make(chan bool, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mac := hmac.New(sha256.New, testKey)
		_, _ = io.WriteString(mac, r.Method+"\nhttp\n"+r.Host+"\n"+r.Host+"\n"+r.RequestURI)
		signatures <- r.Header.Get(outboundsign.SignatureHeader) == hex.EncodeToString(mac.Sum(nil))

		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(upstream.Close)

	var output syncBuffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan net.Addr, 1)
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)

		runErr = run(ctx, config{
			listenAddr: "127.0.0.1:0",
			upstream:   upstream.URL,
			signingKey: testKey,
		}, logger, ready)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	var addr net.Addr
	select {
	case addr = <-ready:
	case <-done:
		t.Fatalf("run() = %v before it listened", runErr)
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get("http://" + addr.String() + "/api/items?token=query-secret")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	select {
	case valid := <-signatures:
		if !valid {
			t.Error("upstream got a signature that is not valid")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream got no request")
	}

	cancel()
	<-done
	if runErr != nil {
		t.Fatalf("run() error = %v, want nil", runErr)
	}

	if strings.Contains(output.snapshot().String(), "secret") {
		t.Errorf("log contains request data: %s", output.snapshot().String())
	}

	var completed []map[string]any
	for _, record := range records(t, output.snapshot()) {
		if record["msg"] == "request completed" {
			completed = append(completed, record)
		}
	}

	if len(completed) != 1 {
		t.Fatalf("completion records = %d, want 1", len(completed))
	}

	record := completed[0]
	if keys := slices.Sorted(maps.Keys(record)); !slices.Equal(keys, completionKeys) {
		t.Errorf("keys = %v, want %v", keys, completionKeys)
	}

	if record["request_id"] != resp.Header.Get(accesslog.RequestIDHeader) || record["status"] != float64(http.StatusAccepted) {
		t.Errorf("record = %v, want the request ID of the response and status 202", record)
	}
}
