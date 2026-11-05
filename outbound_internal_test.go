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
type beforeUpstreamFunc func(context.Context, *RequestContext, *OutboundRequest) (*Rejection, error)

// BeforeUpstream calls f.
func (f beforeUpstreamFunc) BeforeUpstream(ctx context.Context, rc *RequestContext, out *OutboundRequest) (*Rejection, error) {
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
	failure := errors.New("signing failed")

	for _, tc := range []struct {
		name      string
		withBody  bool
		rejection *Rejection
		hookErr   error
		wantErr   error
	}{
		{
			name:    "no body",
			hookErr: failure,
			wantErr: failure,
		},
		{
			name:     "body",
			withBody: true,
			hookErr:  failure,
			wantErr:  failure,
		},
		{
			name: "invalid rejection without body",
			rejection: &Rejection{
				Status: http.StatusOK,
				Reason: "acl.denied",
			},
			wantErr: ErrInvalidRejection,
		},
		{
			name:     "invalid rejection with body",
			withBody: true,
			rejection: &Rejection{
				Status: http.StatusOK,
				Reason: "acl.denied",
			},
			wantErr: ErrInvalidRejection,
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

			laterRan, delegated := false, false
			transport := &outboundTransport{
				hooks: []BeforeUpstreamHook{
					beforeUpstreamFunc(func(context.Context, *RequestContext, *OutboundRequest) (*Rejection, error) {
						return tc.rejection, tc.hookErr
					}),
					beforeUpstreamFunc(func(context.Context, *RequestContext, *OutboundRequest) (*Rejection, error) {
						laterRan = true

						return nil, nil
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

			if resp != nil {
				t.Errorf("RoundTrip() response = %v, want nil", resp)
			}

			if !errors.Is(err, tc.wantErr) {
				t.Errorf("RoundTrip() error = %v, want an error that matches %v", err, tc.wantErr)
			}

			if laterRan {
				t.Error("RoundTrip() later hook ran = true, want false")
			}

			if delegated {
				t.Error("RoundTrip() base transport called = true, want false")
			}

			if tc.withBody && body.closes != 1 {
				t.Errorf("body closed %d times, want 1", body.closes)
			}

			if rc.failure == nil {
				t.Fatal("RoundTrip() failure = nil, want a recorded failure")
			}

			if rc.failure.Phase != PhaseBeforeUpstream {
				t.Errorf("RoundTrip() failure phase = %v, want BeforeUpstream", rc.failure.Phase)
			}

			if !errors.Is(rc.failure.Err, tc.wantErr) {
				t.Errorf("RoundTrip() failure error = %v, want an error that matches %v", rc.failure.Err, tc.wantErr)
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
