// Package bodylimit provides an optional streaming request-body limit.
package bodylimit

import (
	"net/http"

	gateway "example.com/go-gateway-starter"
)

// New limits bytes read from the request body without buffering it. Negative
// limits panic; zero permits only empty bodies. Known oversized bodies are
// rejected before calling next. Unknown lengths are limited while reading, so
// an upstream may receive a partial body before the limit is exceeded.
// Register this middleware inside the gateway's managed chain.
func New(maxBytes int64) gateway.Middleware {
	if maxBytes < 0 {
		panic("bodylimit: negative limit")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > maxBytes {
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			r = r.Clone(r.Context())
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
