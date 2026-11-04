package edgekit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProxyWriterCommitStopsRendering is a white-box test: ReverseProxy calls
// its error handler only before a commit or after a hijack, so only a direct
// call can show that a flush or a body write also stops the rendering.
func TestProxyWriterCommitStopsRendering(t *testing.T) {
	for _, tc := range []struct {
		name     string
		action   func(*proxyWriter)
		rendered bool
	}{
		{
			name:     "nothing written",
			action:   func(*proxyWriter) {},
			rendered: true,
		},
		{
			name: "informational response",
			action: func(w *proxyWriter) {
				w.WriteHeader(http.StatusEarlyHints)
			},
			rendered: true,
		},
		{
			name: "final status",
			action: func(w *proxyWriter) {
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "empty body write",
			action: func(w *proxyWriter) {
				_, _ = w.Write(nil)
			},
		},
		{
			name: "flush",
			action: func(w *proxyWriter) {
				_ = http.NewResponseController(w).Flush()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			renders := 0
			g := &Gateway{
				renderer: func(context.Context, Problem) (string, []byte) {
					renders++

					return problemContentType, nil
				},
			}
			rc := &RequestContext{}
			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			r = r.WithContext(context.WithValue(r.Context(), requestContextKey{}, rc))

			recorder := httptest.NewRecorder()
			w := newProxyWriter(recorder, rc)
			tc.action(w)
			g.proxyFailed(w, r, errors.New("failure"))

			wantRenders := 0
			if tc.rendered {
				wantRenders = 1
			}

			if renders != wantRenders {
				t.Errorf("proxyFailed() renderer calls = %d, want %d", renders, wantRenders)
			}

			if rc.failure == nil {
				t.Fatal("proxyFailed() failure = nil, want a recorded failure")
			}

			if rc.failure.Phase != PhaseUnknown {
				t.Errorf("proxyFailed() failure phase = %v, want Unknown", rc.failure.Phase)
			}
		})
	}
}
