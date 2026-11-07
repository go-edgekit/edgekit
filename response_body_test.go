package edgekit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// failingResponseBody returns err instead of EOF after the data ends.
type failingResponseBody struct {
	io.Reader

	err    error
	closed atomic.Bool
}

// Read keeps the original error available to the response-body observer.
func (b *failingResponseBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if errors.Is(err, io.EOF) {
		err = b.err
	}

	return n, err
}

// Close records that the proxy released the body, also during an abort.
func (b *failingResponseBody) Close() error {
	b.closed.Store(true)

	return nil
}

// streamedResponse is what one request saw when the upstream response body
// ended with an error.
type streamedResponse struct {
	recorder   *httptest.ResponseRecorder
	body       *failingResponseBody
	renders    int32
	panicked   bool
	completion edgekit.Completion
}

// streamResponse sends one request through a gateway whose upstream returns
// a 200 response with the body "prefix", followed by readErr.
func streamResponse(t *testing.T, readErr error) streamedResponse {
	t.Helper()

	body := &failingResponseBody{
		Reader: strings.NewReader("prefix"),
		err:    readErr,
	}
	var rendered atomic.Int32
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          body,
				ContentLength: -1,
			}, nil
		}),
		Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
			rendered.Add(1)

			return "text/plain", nil
		},
		Hooks: []any{collector},
	})

	w := httptest.NewRecorder()
	r := serverRequest(httptest.NewRequest(http.MethodGet, "/api/stream", nil))
	panicked := panics(func() {
		g.ServeHTTP(w, r)
	})

	return streamedResponse{
		recorder:   w,
		body:       body,
		renders:    rendered.Load(),
		panicked:   panicked,
		completion: receive(t, completed),
	}
}

func TestResponseBodyEOFHasNoFailure(t *testing.T) {
	got := streamResponse(t, io.EOF)
	c := got.completion

	if got.panicked || c.Aborted {
		t.Errorf("ServeHTTP() panicked = %v, RequestCompleted() Aborted = %v; want false, false", got.panicked, c.Aborted)
	}

	if got.recorder.Code != http.StatusOK || got.recorder.Body.String() != "prefix" || got.renders != 0 {
		t.Errorf("ServeHTTP() response = %d %q, renderer calls = %d; want 200 prefix without rendering", got.recorder.Code, got.recorder.Body.String(), got.renders)
	}

	if !got.body.closed.Load() {
		t.Error("upstream response body was not closed")
	}

	if c.Failure != nil {
		t.Errorf("RequestCompleted() Failure = %+v, want nil", c.Failure)
	}
}

func TestResponseBodyReadFailureReachesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "wrapped EOF is not a successful end",
			err:  fmt.Errorf("read: %w", io.EOF),
		},
		{
			name: "truncated response",
			err:  io.ErrUnexpectedEOF,
		},
		{
			name: "original error",
			err:  errors.New("response body with secret data failed"),
		},
		{
			name: "unrelated body-limit sentinel",
			err:  edgekit.ErrRequestBodyTooLarge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := streamResponse(t, tc.err)
			c := got.completion

			if !got.panicked || !c.Aborted {
				t.Errorf("ServeHTTP() panicked = %v, RequestCompleted() Aborted = %v; want true, true", got.panicked, c.Aborted)
			}

			if got.recorder.Code != http.StatusOK || got.recorder.Body.String() != "prefix" || got.renders != 0 {
				t.Errorf("ServeHTTP() response = %d %q, renderer calls = %d; want the committed 200 prefix without rendering", got.recorder.Code, got.recorder.Body.String(), got.renders)
			}

			if !got.body.closed.Load() {
				t.Error("upstream response body was not closed")
			}

			if c.RejectionReason != "" {
				t.Errorf("RequestCompleted() RejectionReason = %q, want empty", c.RejectionReason)
			}

			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want a response-body failure")
			}

			if got := c.Failure.Phase; got != edgekit.PhaseResponseBody {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", got, edgekit.PhaseResponseBody)
			}

			// ADR-0028 requires the same error object, not an error that wraps it.
			if !reflect.ValueOf(c.Failure.Err).Equal(reflect.ValueOf(tc.err)) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want the original error %v", c.Failure.Err, tc.err)
			}

			if got := c.Failure.Phase.String(); got != "response_body" {
				t.Errorf("RequestCompleted() Failure.Phase.String() = %q, want response_body", got)
			}
		})
	}
}
