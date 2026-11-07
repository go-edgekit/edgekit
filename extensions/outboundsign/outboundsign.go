// Package outboundsign is a demonstration extension that signs the outbound
// request of a route with HMAC-SHA256 (RFC 2104). It shows that a
// [edgekit.BeforeUpstreamHook] gets the outbound request after the upstream
// selection and the rewrite, and that route middleware can give the hook
// its route-specific settings through request state.
//
// It is a demonstration, not an authentication protocol. The signature has
// no timestamp, no nonce, no replay protection, no body integrity, and no
// key identifier, and there is no key rotation. A party that sees one signed
// request can send it again, with any body, as long as the key is valid.
// Use TLS to the upstream, and do not use the signature as the only
// authentication of the gateway.
//
// # Registration
//
// Register the [Signer] last in [edgekit.Hooks.BeforeUpstream] so it signs
// the header changes of earlier hooks. Register it separately as a header
// owner, and register the middleware of [Signer.WithKey] on each route that
// it signs:
//
//	signer := outboundsign.New()
//	withKey, err := signer.WithKey(key)
//	// ...
//	gateway, err := edgekit.New(edgekit.Config{
//		Hooks: edgekit.Hooks{
//			BeforeUpstream: []edgekit.BeforeUpstreamHook{
//				signer,
//			},
//		},
//		HeaderOwners: []edgekit.HeaderOwner{
//			signer,
//		},
//		Routes: []edgekit.Route{
//			{
//				Prefix:     "/api/",
//				Upstream:   selector,
//				Middleware: []edgekit.Middleware{withKey},
//			},
//		},
//	})
//
// A request on a route without the middleware fails closed: the hook
// returns [ErrNoKey], so the gateway does not call the transport and sends
// 500 Internal Server Error (ADR-0017).
//
// # Signature
//
// The hook sets [SignatureHeader] to the lowercase hexadecimal HMAC-SHA256
// of these five fields, each followed by a newline except the last:
//
//	method
//	scheme of the outbound URL
//	authority of the outbound URL
//	Host value that the transport sends
//	escaped request URI of the outbound URL, with the query
//
// The fields come from the [edgekit.OutboundRequest], so they are the
// values that the upstream gets, not the values of the inbound request. The
// hook does not read, buffer, or change the body or the trailers.
//
// The newlines make the signed string unambiguous only when the first four
// fields contain no newline: otherwise two different requests can have the
// same signed string. The gateway does not check that the method is a token,
// so the hook checks these fields and fails with [ErrInvalidField]. A newline
// in the last field cannot move a field boundary.
//
// The route middleware gives the key to the hook through request state. This
// shows one way to give route-specific settings to a hook. It does not decide
// between route bindings and route middleware (roadmap, section 3).
package outboundsign

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-edgekit/edgekit"
)

// SignatureHeader is the outbound header that carries the signature.
const SignatureHeader = "X-Gateway-Signature"

// MinKeySize is the minimum length of a signing key in bytes: the output
// size of SHA-256, which RFC 2104, section 3, gives as the minimum key length
// for HMAC-SHA256.
const MinKeySize = sha256.Size

// ErrShortKey is the error of [Signer.WithKey] for a key that is shorter than
// [MinKeySize].
var ErrShortKey = errors.New("outboundsign: key is too short")

// ErrNoKey is the error of the BeforeUpstream hook when the request has no
// signing key, because the route has no middleware of [Signer.WithKey].
var ErrNoKey = errors.New("outboundsign: route has no signing key")

// ErrInvalidField is the error of the BeforeUpstream hook when a signed field
// other than the request URI contains a newline, so the signed string would
// be ambiguous. The error contains no field value.
var ErrInvalidField = errors.New("outboundsign: signed field contains a newline")

// Signer signs outbound requests with the key of their route. Make it with
// [New]. Register it in [edgekit.Hooks.BeforeUpstream] and
// [edgekit.Config.HeaderOwners].
//
// A Signer is safe for concurrent use. The keys are copies that it does not
// change, and the key of each request is in the request state of that
// request, so concurrent requests on different routes use their own keys.
type Signer struct {
	key *edgekit.StateKey[[]byte]
}

// The package documentation promises these interfaces to users who register
// a Signer.
var (
	_ edgekit.BeforeUpstreamHook = (*Signer)(nil)
	_ edgekit.HeaderOwner        = (*Signer)(nil)
)

// New returns a Signer.
func New() *Signer {
	return &Signer{
		key: edgekit.NewStateKey[[]byte](),
	}
}

// WithKey returns route middleware that gives key to the hook of s for each
// request of the route. It copies key, so later changes to the slice have no
// effect. It returns [ErrShortKey] for a key that is shorter than
// [MinKeySize]. The error contains no key data.
//
// The key is secret. The Signer does not put it in errors, logs, records,
// or responses. Outside a gateway, the middleware calls the next handler
// without changes.
func (s *Signer) WithKey(key []byte) (edgekit.Middleware, error) {
	if len(key) < MinKeySize {
		return nil, ErrShortKey
	}

	owned := bytes.Clone(key)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rc, ok := edgekit.FromContext(r.Context()); ok {
				edgekit.SetState(rc, s.key, owned)
			}

			next.ServeHTTP(w, r)
		})
	}, nil
}

// ManagedHeaders declares [SignatureHeader], so the gateway removes the
// values that the client sent. It implements [edgekit.HeaderOwner].
func (s *Signer) ManagedHeaders() []string {
	return []string{SignatureHeader}
}

// BeforeUpstream signs out and sets [SignatureHeader]. It implements
// [edgekit.BeforeUpstreamHook].
//
// It fails, without a signature, when ctx is done, with the error of ctx,
// when the request has no key, with [ErrNoKey], and when a signed field
// other than the request URI contains a newline, with [ErrInvalidField]. The
// gateway then does not call the transport. It never rejects.
func (s *Signer) BeforeUpstream(ctx context.Context, rc *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key, ok := edgekit.State(rc, s.key)
	if !ok {
		return nil, ErrNoKey
	}

	u := out.URL()
	fields := []string{
		out.Method(),
		u.Scheme,
		u.Host,
		out.Host(),
		u.RequestURI(),
	}

	for _, field := range fields[:len(fields)-1] {
		if strings.ContainsRune(field, '\n') {
			return nil, ErrInvalidField
		}
	}

	mac := hmac.New(sha256.New, key)
	// A hash.Hash never returns an error from Write.
	_, _ = io.WriteString(mac, strings.Join(fields, "\n"))

	out.Header().Set(SignatureHeader, hex.EncodeToString(mac.Sum(nil)))

	return nil, nil
}
