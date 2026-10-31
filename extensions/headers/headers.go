// Package headers is an example of an optional gateway extension.
package headers

import (
	"net/http"

	gateway "example.com/go-gateway-starter"
)

// AddResponseHeader demonstrates how downstream middleware can be written
// without changing the core gateway package.
func AddResponseHeader(name, value string) gateway.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(name, value)
			next.ServeHTTP(w, r)
		})
	}
}
