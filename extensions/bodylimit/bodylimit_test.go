package bodylimit_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-edgekit/edgekit/extensions/bodylimit"
)

func TestMiddlewareDoesNotReadAheadOrWrapResponseWriter(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := httptest.NewRequest(http.MethodPost, "/", reader)
	w := httptest.NewRecorder()
	called := false
	bodylimit.New(4)(http.HandlerFunc(func(got http.ResponseWriter, request *http.Request) {
		called = true
		if got != w || request == r || request.Body == r.Body || request.Context() != r.Context() {
			t.Error("writer capabilities, request isolation, or context changed")
		}
		// No bytes have been supplied. Entering next must not wait for EOF.
	})).ServeHTTP(w, r)
	if !called {
		t.Fatal("next was not called")
	}
}

func TestNegativeLimitPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative configuration did not panic")
		}
	}()
	bodylimit.New(-1)
}
