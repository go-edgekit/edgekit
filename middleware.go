package gateway

import "net/http"

// Middleware wraps an HTTP handler to inspect, modify, or reject requests.
// This uses the standard net/http model to keep extensions interoperable.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware in the order provided: the first is outermost
// and runs first on the request path (and last on the response path).
func Chain(handler http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		handler = middlewares[i](handler)
	}
	return handler
}
