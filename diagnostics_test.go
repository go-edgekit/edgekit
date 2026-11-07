package edgekit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// captureDiagnostics sends the default slog output to the returned buffer
// until the test ends. It changes process-wide state, so tests that use it
// must not run in parallel.
func captureDiagnostics(t *testing.T) *bytes.Buffer {
	t.Helper()

	previousLogger, previousWriter := slog.Default(), log.Writer()
	previousFlags, previousPrefix := log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))

	return &output
}

// failingBody returns "partial" and then fails with err.
type failingBody struct {
	sent   bool
	closed bool
	err    error
}

// Read returns "partial" once, and then the error.
func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true

		return copy(p, "partial"), nil
	}

	return 0, b.err
}

// Close records the call.
func (b *failingBody) Close() error {
	b.closed = true

	return nil
}

// errSecret is a failure whose text contains secrets.
var errSecret = &url.Error{
	Op:  "read",
	URL: "https://host/path-secret?token=query-secret",
	Err: errors.New("body-secret\nAuthorization: header-secret credential-secret"),
}

func TestStreamFailureDiagnosticOmitsDetails(t *testing.T) {
	output := captureDiagnostics(t)
	body := &failingBody{
		err: errSecret,
	}
	rendered := false
	var unwound bool
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://upstream.test/base"),
		}},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
				Request:    r,
			}, nil
		}),
		Middleware: []edgekit.Middleware{unwinding(&unwound)},
		Hooks:      []any{completionHooks},
		Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
			rendered = true

			return "text/plain", nil
		},
	})

	// ReverseProxy aborts a failed body copy only when an http.Server runs
	// it.
	r := httptest.NewRequest(http.MethodGet, "/api/path-secret?token=inbound-secret", nil)
	r.Header.Set("Authorization", "Bearer authorization-secret")
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{
		ReadHeaderTimeout: time.Second,
	}))
	w := httptest.NewRecorder()
	recovered := func() (recovered any) {
		defer func() {
			recovered = recover()
		}()

		g.ServeHTTP(w, r)

		return nil
	}()

	if err, ok := recovered.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Errorf("recovered %v, want http.ErrAbortHandler", recovered)
	}

	if w.Code != http.StatusOK {
		t.Errorf("ServeHTTP() status = %d, want 200", w.Code)
	}

	if got := w.Body.String(); got != "partial" {
		t.Errorf("ServeHTTP() body = %q, want partial", got)
	}

	if rendered {
		t.Error("ServeHTTP() renderer called = true, want false after the body fails")
	}

	if !body.closed {
		t.Error("ServeHTTP() upstream body closed = false, want true")
	}

	c := receive(t, completed)
	if !c.Aborted {
		t.Error("RequestCompleted() Aborted = false, want true")
	}

	if c.Failure == nil || c.Failure.Phase != edgekit.PhaseResponseBody || !errors.Is(c.Failure.Err, errSecret) {
		t.Errorf("RequestCompleted() Failure = %+v, want the original error in PhaseResponseBody", c.Failure)
	}

	if !unwound {
		t.Error("RequestCompleted() middleware unwound = false, want true")
	}

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("diagnostic = %q, %v; want one JSON record", output.String(), err)
	}

	if got := record["source"]; got != "reverse_proxy" {
		t.Errorf("ServeHTTP() diagnostic source = %v, want reverse_proxy", got)
	}

	if got := record["msg"]; got != "diagnostic details omitted" {
		t.Errorf("ServeHTTP() diagnostic message = %v, want diagnostic details omitted", got)
	}

	if strings.Contains(output.String(), "secret") {
		t.Errorf("ServeHTTP() diagnostic = %q, want no secret values", output.String())
	}
}

func TestRoundTripErrorStaysAvailableWithoutLogging(t *testing.T) {
	output := captureDiagnostics(t)
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://upstream.test"),
		}},
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, errSecret
		}),
		Hooks: []any{completionHooks},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/path-secret", strings.NewReader("request-secret")))

	if w.Code != http.StatusBadGateway {
		t.Errorf("ServeHTTP() status = %d, want 502", w.Code)
	}

	if got, want := w.Body.String(), problem(http.StatusBadGateway, ""); got != want {
		t.Errorf("ServeHTTP() body = %q, want %q", got, want)
	}

	if output.Len() != 0 {
		t.Errorf("ServeHTTP() diagnostics = %q, want none", output.String())
	}

	c := receive(t, completed)
	if c.Failure == nil {
		t.Fatal("RequestCompleted() Failure = nil, want the original failure")
	}

	if !errors.Is(c.Failure.Err, errSecret) {
		t.Errorf("RequestCompleted() Failure.Err = %v, want the original error", c.Failure.Err)
	}

	if c.Failure.Err == nil {
		t.Fatal("RequestCompleted() Failure.Err = nil, want the original error")
	}

	if got, want := c.Failure.Err.Error(), errSecret.Error(); got != want {
		t.Errorf("RequestCompleted() Failure.Err.Error() = %q, want %q", got, want)
	}
}
