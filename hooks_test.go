package edgekit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// requestReceivedFunc provides only the RequestReceived phase.
type requestReceivedFunc func(context.Context, *edgekit.RequestContext, *http.Request) error

// RequestReceived calls f.
func (f requestReceivedFunc) RequestReceived(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) error {
	return f(ctx, rc, r)
}

// beforeUpstreamFunc provides only the BeforeUpstream phase.
type beforeUpstreamFunc func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) error

// BeforeUpstream calls f.
func (f beforeUpstreamFunc) BeforeUpstream(ctx context.Context, rc *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
	return f(ctx, rc, out)
}

// upstreamResponseFunc provides only the UpstreamResponse phase.
type upstreamResponseFunc func(context.Context, *edgekit.RequestContext, *http.Response) error

// UpstreamResponse calls f.
func (f upstreamResponseFunc) UpstreamResponse(ctx context.Context, rc *edgekit.RequestContext, resp *http.Response) error {
	return f(ctx, rc, resp)
}

// requestCompletedFunc provides only the RequestCompleted phase.
type requestCompletedFunc func(context.Context, *edgekit.RequestContext, edgekit.Completion)

// RequestCompleted calls f.
func (f requestCompletedFunc) RequestCompleted(ctx context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
	f(ctx, rc, c)
}

// testHooks provides all phases for tests that need a complete lifecycle.
type testHooks struct {
	received  func(context.Context, *edgekit.RequestContext, *http.Request) error
	before    func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) error
	response  func(context.Context, *edgekit.RequestContext, *http.Response) error
	completed func(context.Context, *edgekit.RequestContext, edgekit.Completion)
}

// RequestReceived calls h.received when it is set.
func (h *testHooks) RequestReceived(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) error {
	if h.received == nil {
		return nil
	}

	return h.received(ctx, rc, r)
}

// BeforeUpstream calls h.before when it is set.
func (h *testHooks) BeforeUpstream(ctx context.Context, rc *edgekit.RequestContext, out *edgekit.OutboundRequest) error {
	if h.before == nil {
		return nil
	}

	return h.before(ctx, rc, out)
}

// UpstreamResponse calls h.response when it is set.
func (h *testHooks) UpstreamResponse(ctx context.Context, rc *edgekit.RequestContext, resp *http.Response) error {
	if h.response == nil {
		return nil
	}

	return h.response(ctx, rc, resp)
}

// RequestCompleted calls h.completed when it is set.
func (h *testHooks) RequestCompleted(ctx context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
	if h.completed != nil {
		h.completed(ctx, rc, c)
	}
}

// completions records completion without registering processing hooks.
func completions() (requestCompletedFunc, <-chan edgekit.Completion) {
	ch := make(chan edgekit.Completion, 16)
	hook := requestCompletedFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
		ch <- c
	})

	return hook, ch
}

func TestHookAdaptersExposeOnlyTheirPhase(t *testing.T) {
	collector, _ := completions()

	for _, tc := range []struct {
		name     string
		provider any
		phase    string
	}{
		{
			name:     "request received",
			provider: requestReceivedFunc(nil),
			phase:    "RequestReceived",
		},
		{
			name:     "before upstream",
			provider: beforeUpstreamFunc(nil),
			phase:    "BeforeUpstream",
		},
		{
			name:     "upstream response",
			provider: upstreamResponseFunc(nil),
			phase:    "UpstreamResponse",
		},
		{
			name:     "request completed",
			provider: requestCompletedFunc(nil),
			phase:    "RequestCompleted",
		},
		{
			name:     "completion collector",
			provider: collector,
			phase:    "RequestCompleted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var phases []string
			if _, ok := tc.provider.(edgekit.RequestReceivedHook); ok {
				phases = append(phases, "RequestReceived")
			}

			if _, ok := tc.provider.(edgekit.BeforeUpstreamHook); ok {
				phases = append(phases, "BeforeUpstream")
			}

			if _, ok := tc.provider.(edgekit.UpstreamResponseHook); ok {
				phases = append(phases, "UpstreamResponse")
			}

			if _, ok := tc.provider.(edgekit.RequestCompletedHook); ok {
				phases = append(phases, "RequestCompleted")
			}

			if !slices.Equal(phases, []string{tc.phase}) {
				t.Errorf("provider implements %v, want only %s", phases, tc.phase)
			}
		})
	}
}

func TestOutboundHeaderCheckDoesNotDependOnHooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		valid  bool
	}{
		{
			name: "valid header",
			change: func(r *http.Request) {
				r.Header.Set("X-Valid", "from middleware")
			},
			valid: true,
		},
		{
			name: "underscore alias",
			change: func(r *http.Request) {
				r.Header["X_alias"] = []string{"from middleware"}
			},
		},
		{
			name: "control character in value",
			change: func(r *http.Request) {
				r.Header.Set("X-Valid", "a\x01b")
			},
		},
		{
			name: "underscore alias in trailer name",
			change: func(r *http.Request) {
				r.Trailer = http.Header{
					"X_alias": nil,
				}
			},
		},
	} {
		for _, withBefore := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/before upstream hook %v", tc.name, withBefore), func(t *testing.T) {
				collector, completed := completions()
				providers := []any{collector}
				if withBefore {
					providers = append(providers, beforeUpstreamFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) error {
						return nil
					}))
				}

				var calls int
				transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
					calls++

					return &http.Response{
						StatusCode: http.StatusNoContent,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				})
				g := newGateway(t, edgekit.Config{
					Routes: []edgekit.Route{{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
						Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
							return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								r = r.Clone(r.Context())
								tc.change(r)
								next.ServeHTTP(w, r)
							})
						}},
					}},
					Hooks:     providers,
					Transport: transport,
				})

				w := httptest.NewRecorder()
				g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))
				c := receive(t, completed)

				if tc.valid {
					if w.Code != http.StatusNoContent || calls != 1 || c.Failure != nil {
						t.Errorf("status = %d, transport calls = %d, failure = %+v; want 204, 1, none", w.Code, calls, c.Failure)
					}

					return
				}

				if w.Code != http.StatusInternalServerError || w.Body.String() != "Internal Server Error\n" || calls != 0 {
					t.Errorf("response = %d %q, transport calls = %d; want 500 without details and no call", w.Code, w.Body.String(), calls)
				}

				if c.Failure == nil || c.Failure.Phase != edgekit.PhaseBeforeUpstream {
					t.Errorf("failure = %+v, want a failure in BeforeUpstream", c.Failure)
				}
			})
		}
	}
}
