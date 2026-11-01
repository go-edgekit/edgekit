package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	gateway "example.com/go-gateway-starter"
)

func captureGatewayDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous, output, flags, prefix := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	var buffer bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	return &buffer
}

type diagnosticBody struct {
	sent, closed bool
	failure      error
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}
	return 0, b.failure
}

func (b *diagnosticBody) Close() error { b.closed = true; return nil }

func TestProxyStreamDiagnosticOmitsDetailsAndPreservesAbort(t *testing.T) {
	output := captureGatewayDiagnostics(t)
	failure := &url.Error{Op: "read", URL: "https://user:credential-secret@host/path-secret?token=query-secret", Err: errors.New("body-secret\nAuthorization: header-secret")}
	body := &diagnosticBody{failure: failure}
	unwound, completed := false, 0
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{testRoute(t, "http://upstream.test/base?token=upstream-secret")},
		Transport: safetyRoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: r}, nil
		}),
		Middlewares: []gateway.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { unwound = true }()
				next.ServeHTTP(w, r)
			})
		}},
		Extensions: []any{&testExtension{complete: func(_ context.Context, _ *gateway.RequestContext, outcome gateway.Outcome) {
			completed++
			if !unwound || !outcome.Aborted || outcome.Err != nil {
				t.Errorf("stream logging changed completion semantics: unwound=%v outcome=%+v", unwound, outcome)
			}
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/path-secret?token=inbound-secret", nil)
	r.Header.Set("Authorization", "Bearer authorization-secret")
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{}))
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("stream abort was changed: %v", got)
			}
		}()
		app.ServeHTTP(w, r)
	}()
	if completed != 1 || !body.closed || w.Code != 200 || w.Body.String() != "partial" {
		t.Fatalf("lost streaming behavior: completions=%d closed=%v response=%d %q", completed, body.closed, w.Code, w.Body.String())
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("missing safe proxy diagnostic: %v, output=%q", err, output.String())
	}
	if record["source"] != "reverse_proxy" || record["msg"] != "diagnostic details omitted" || strings.Contains(output.String(), "secret") {
		t.Fatalf("unsafe or unexpected diagnostic: %s", output.String())
	}
}

func TestProxyErrorRemainsAvailableWithoutAutomaticLogging(t *testing.T) {
	output := captureGatewayDiagnostics(t)
	failure := &url.Error{Op: "POST", URL: "https://user:credential-secret@host/path-secret?token=query-secret", Err: errors.New("authorization-secret")}
	var observed error
	app, err := gateway.NewWithConfig(gateway.Config{
		Routes: []gateway.Route{testRoute(t, "http://upstream.test")},
		Transport: safetyRoundTripper(func(*http.Request) (*http.Response, error) {
			return nil, failure
		}),
		Extensions: []any{&testExtension{complete: func(_ context.Context, _ *gateway.RequestContext, outcome gateway.Outcome) { observed = outcome.Err }}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/path-secret", strings.NewReader("request-secret")))
	if w.Code != 502 || w.Body.String() != "Bad Gateway\n" || !errors.Is(observed, failure) || observed.Error() != failure.Error() || output.Len() != 0 {
		t.Fatalf("error handling changed: response=%d %q original=%v output=%q", w.Code, w.Body.String(), errors.Is(observed, failure), output.String())
	}
}

func TestGatewayConstructionDoesNotReconfigureProcessLogging(t *testing.T) {
	output := captureGatewayDiagnostics(t)
	logger, writer := slog.Default(), log.Writer()
	_, err := gateway.New([]gateway.Route{testRoute(t, "http://upstream.test")})
	if err != nil {
		t.Fatal(err)
	}
	if slog.Default() != logger || log.Writer() != writer || output.Len() != 0 {
		t.Fatal("framework constructor changed process-global logging")
	}
}

var _ io.ReadCloser = (*diagnosticBody)(nil)
