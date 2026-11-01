// Package admission provides optional fail-fast concurrency admission.
package admission

import (
	"net/http"

	gateway "example.com/go-gateway-starter"
)

// New limits concurrent calls to downstream handlers with no waiting queue.
// maxActive must be positive or New panics. Reusing the returned middleware
// shares one budget across its wrapped handlers; separate calls create separate
// budgets. This is concurrency control, not a request-rate or identity policy.
//
// An admitted call runs synchronously with its request and writer unchanged.
// Its slot is held until downstream returns or unwinds, including during body
// streaming. Cancellation does not release a slot before downstream ends.
// Saturated requests receive a generic local 503 without reading their body.
//
// Register inside the gateway, after response-observing middleware. The budget
// does not cover outer middleware, completion callbacks, rejected-response
// writes, or server work outside the handler chain.
func New(maxActive int) gateway.Middleware {
	if maxActive <= 0 {
		panic("admission: max active calls must be positive")
	}
	slots := make(chan struct{}, maxActive)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
