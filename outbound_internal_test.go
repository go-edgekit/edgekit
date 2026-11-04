package edgekit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// beforeUpstreamFunc is a BeforeUpstreamHook made from a function.
type beforeUpstreamFunc func(context.Context, *RequestContext, *OutboundRequest) error

// BeforeUpstream calls f.
func (f beforeUpstreamFunc) BeforeUpstream(ctx context.Context, rc *RequestContext, out *OutboundRequest) error {
	return f(ctx, rc, out)
}

// roundTripperFunc is an http.RoundTripper made from a function.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// RoundTrip calls f.
func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// countingBody counts the calls of Close.
type countingBody struct {
	io.Reader

	closes int
}

// Close counts the call.
func (b *countingBody) Close() error {
	b.closes++

	return nil
}

// TestOutboundFailureClosesBodyWithoutDelegating is a white-box test: through
// the gateway, ReverseProxy gives the transport a body wrapper whose Close has
// no visible effect, so only a direct call can show that the transport closes
// the body that it gets.
func TestOutboundFailureClosesBodyWithoutDelegating(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withBody bool
	}{
		{
			name: "no body",
		},
		{
			name:     "body",
			withBody: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &RequestContext{}
			ctx := context.WithValue(t.Context(), requestContextKey{}, rc)
			r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.test/api/x", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}

			body := &countingBody{
				Reader: strings.NewReader("unread"),
			}
			if tc.withBody {
				r.Body = body
			}

			failure := errors.New("signing failed")
			laterRan, delegated := false, false
			transport := &outboundTransport{
				hooks: []BeforeUpstreamHook{
					beforeUpstreamFunc(func(context.Context, *RequestContext, *OutboundRequest) error {
						return failure
					}),
					beforeUpstreamFunc(func(context.Context, *RequestContext, *OutboundRequest) error {
						laterRan = true

						return nil
					}),
				},
				base: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					delegated = true

					return nil, errors.New("must not run")
				}),
			}

			resp, err := transport.RoundTrip(r)
			if resp != nil {
				_ = resp.Body.Close()
			}

			if resp != nil || !errors.Is(err, failure) {
				t.Errorf("RoundTrip = %v, %v; want nil and the hook error", resp, err)
			}

			if laterRan || delegated {
				t.Errorf("later hook ran = %v, transport ran = %v; want false, false", laterRan, delegated)
			}

			if tc.withBody && body.closes != 1 {
				t.Errorf("body closed %d times, want 1", body.closes)
			}

			if rc.failure == nil || rc.failure.Phase != PhaseBeforeUpstream {
				t.Errorf("recorded failure = %+v, want the hook error in BeforeUpstream", rc.failure)
			}
		})
	}
}

func TestOutboundCheckNamesTheFailedCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		header  http.Header
		trailer http.Header
		want    error
	}{
		{
			name: "header name",
			header: http.Header{
				"X_User": {"admin"},
			},
			want: errInvalidOutboundName,
		},
		{
			name: "trailer name",
			header: http.Header{
				"X-User": {"admin"},
			},
			trailer: http.Header{
				"X_User": nil,
			},
			want: errInvalidOutboundName,
		},
		{
			name: "header value",
			header: http.Header{
				"X-User": {"a\nb"},
			},
			want: errInvalidOutboundValue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{
				Header:  tc.header,
				Trailer: tc.trailer,
			}
			var saved [len(connectionFields)][]string

			err := sanitizeOutbound(r, &saved)

			if !errors.Is(err, tc.want) {
				t.Errorf("sanitizeOutbound = %v, want %v", err, tc.want)
			}
		})
	}
}
