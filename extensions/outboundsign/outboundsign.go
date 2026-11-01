// Package outboundsign demonstrates signing the rewritten outbound request.
// It is an extension experiment, not a production authentication protocol.
package outboundsign

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	gateway "example.com/go-gateway-starter"
)

// Extension signs requests using a key supplied by its route middleware.
// It is safe to share across concurrent requests. Register its outbound hook
// after any hooks that modify the signed fields.
type Extension struct {
	key *gateway.StateKey[[]byte]
}

// New creates an extension with private request-state ownership.
func New() *Extension {
	return &Extension{key: gateway.NewStateKey[[]byte]()}
}

// WithKey creates route middleware that installs a signing key in typed state.
// It copies secret at construction; callers must not modify it during this call.
// An empty key or missing middleware causes the outbound hook to fail closed.
// Outside a gateway, the middleware passes through without signing.
func (e *Extension) WithKey(secret []byte) gateway.Middleware {
	key := append([]byte(nil), secret...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rc, ok := gateway.FromContext(r.Context()); ok {
				gateway.SetState(rc, e.key, key)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OnBeforeUpstream signs the method, destination scheme and authority, effective
// Host, and escaped request URI (including query) using HMAC-SHA256. It replaces
// any client-supplied X-Gateway-Signature header. It does not read the body.
// There is no timestamp, replay protection, or body-integrity guarantee.
func (e *Extension) OnBeforeUpstream(ctx context.Context, rc *gateway.RequestContext, r *http.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key, ok := gateway.GetState(rc, e.key)
	if !ok || len(key) == 0 {
		return errors.New("outboundsign: route signing key is missing")
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	canonical := r.Method + "\n" + r.URL.Scheme + "\n" + r.URL.Host + "\n" + host + "\n" + r.URL.RequestURI()
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(canonical))
	r.Header.Set("X-Gateway-Signature", hex.EncodeToString(mac.Sum(nil)))
	return nil
}
