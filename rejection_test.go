package edgekit_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// limited is a rejection that a rate limit returns.
var limited = edgekit.Rejection{
	Status: http.StatusTooManyRequests,
	Reason: "ratelimit.exceeded",
	Header: http.Header{
		"Retry-After": {"30"},
	},
}

func TestHookRejectionStopsPhase(t *testing.T) {
	for _, phase := range []string{
		"request_received",
		"before_upstream",
		"upstream_response",
	} {
		t.Run(phase, func(t *testing.T) {
			var requests atomic.Int32
			upstream := countingUpstream(t, &requests)

			laterRan := false
			later := func() (*edgekit.Rejection, error) {
				laterRan = true

				return nil, nil
			}

			// The response phase runs from the last provider to the
			// first, so the rejecting hook is the second provider there.
			var first, second any
			switch phase {
			case "request_received":
				first = edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					return &limited, nil
				})
				second = edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					return later()
				})
			case "before_upstream":
				first = edgekit.BeforeUpstreamHookFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					return &limited, nil
				})
				second = edgekit.BeforeUpstreamHookFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					return later()
				})
			case "upstream_response":
				first = edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					return later()
				})
				second = edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					return &limited, nil
				})
			}

			var unwound, unwoundAtCompletion bool
			var completion edgekit.Completion
			completed := edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
				unwoundAtCompletion = unwound
				completion = c
			})
			middleware := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "1")
					w.Header().Set("Vary", "Origin")
					next.ServeHTTP(w, r)
				})
			}
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, upstream.URL),
				}},
				Middleware: []edgekit.Middleware{unwinding(&unwound), middleware},
				Hooks:      []any{completed, first, second},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusTooManyRequests {
				t.Errorf("ServeHTTP() status = %d, want 429", w.Code)
			}

			if got, want := w.Body.String(), problem(http.StatusTooManyRequests, "ratelimit.exceeded"); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			if got := w.Header().Values("Retry-After"); !slices.Equal(got, []string{"30"}) {
				t.Errorf("Retry-After = %q, want the rejection value only", got)
			}

			if got := w.Header().Get("Vary"); got != "Origin" {
				t.Errorf("ServeHTTP() Vary = %q, want Origin", got)
			}

			if got := w.Header().Get("X-Upstream"); got != "" {
				t.Errorf("ServeHTTP() X-Upstream = %q, want none", got)
			}

			if laterRan {
				t.Error("a later hook of the phase ran after the rejection")
			}

			wantRequests := int32(0)
			if phase == "upstream_response" {
				wantRequests = 1
			}

			if got := requests.Load(); got != wantRequests {
				t.Errorf("upstream got %d requests, want %d", got, wantRequests)
			}

			if completion.RejectionReason != "ratelimit.exceeded" {
				t.Errorf("RequestCompleted() RejectionReason = %q, want ratelimit.exceeded", completion.RejectionReason)
			}

			if completion.Failure != nil {
				t.Errorf("RequestCompleted() Failure = %+v, want nil", completion.Failure)
			}

			if completion.Aborted {
				t.Error("RequestCompleted() Aborted = true, want false")
			}

			if !unwoundAtCompletion {
				t.Error("RequestCompleted() middleware unwound = false, want true")
			}
		})
	}
}

func TestRejectionHeadersAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http2 bool
	}{
		{
			name: "http1",
		},
		{
			name:  "http2",
			http2: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejection := edgekit.Rejection{
				Status: http.StatusUnauthorized,
				Reason: "auth.missing_key",
				Header: http.Header{
					"www-authenticate":  {`ApiKey realm="api"`},
					"Connection":        {"X-Hop"},
					"X-Hop":             {"private"},
					"Keep-Alive":        {"timeout=5"},
					"Transfer-Encoding": {"chunked"},
					"Content-Length":    {"999"},
					"Content-Type":      {"text/html"},
					"Alt-Svc":           {`h3=":443"`},
					"Upgrade":           {"echo"},
					"Trailer":           {"X-Fake"},
					"X-Allowed":         {"a\tb"},
				},
			}
			hooks := edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
				return &rejection, nil
			})
			server := httptest.NewUnstartedServer(newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://unused.test"),
				}},
				Hooks: []any{hooks},
			}))
			client := &http.Client{}
			if tc.http2 {
				server.EnableHTTP2 = true
				server.StartTLS()

				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				client.Transport = &http.Transport{
					ForceAttemptHTTP2: true,
					TLSClientConfig: &tls.Config{
						RootCAs:    roots,
						MinVersion: tls.VersionTLS12,
					},
				}
			} else {
				server.Start()
			}
			defer server.Close()

			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/x", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext(GET, /api/x) error = %v, want nil", err)
			}

			resp, err := client.Do(r)
			if err != nil {
				t.Fatalf("Client.Do(GET /api/x) error = %v, want nil", err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll(GET /api/x response body) error = %v, want nil", err)
			}

			if tc.http2 != (resp.ProtoMajor == 2) {
				t.Fatalf("protocol = %s, want HTTP/2 = %v", resp.Proto, tc.http2)
			}

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("ServeHTTP() status = %d, want 401", resp.StatusCode)
			}

			if want := problem(http.StatusUnauthorized, "auth.missing_key"); string(body) != want {
				t.Errorf("ServeHTTP() body = %q, want %q", body, want)
			}

			if got := resp.Header.Get("WWW-Authenticate"); got != `ApiKey realm="api"` {
				t.Errorf("ServeHTTP() WWW-Authenticate = %q, want %q", got, `ApiKey realm="api"`)
			}

			if got := resp.Header.Get("X-Allowed"); got != "a\tb" {
				t.Errorf("ServeHTTP() X-Allowed = %q, want %q", got, "a\tb")
			}

			if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("ServeHTTP() Content-Type = %q, want application/problem+json", got)
			}

			if want := int64(len(body)); resp.ContentLength != want {
				t.Errorf("ServeHTTP() Content-Length = %d, want %d", resp.ContentLength, want)
			}

			for _, name := range []string{"X-Hop", "Keep-Alive", "Alt-Svc", "Upgrade", "Trailer"} {
				if resp.Header.Get(name) != "" {
					t.Errorf("%s = %q, want none", name, resp.Header.Get(name))
				}
			}

			if len(resp.TransferEncoding) != 0 {
				t.Errorf("ServeHTTP() Transfer-Encoding = %q, want none", resp.TransferEncoding)
			}

			if resp.Close {
				t.Error("ServeHTTP() response Close = true, want false")
			}
		})
	}
}

func TestHookResultCombinations(t *testing.T) {
	failure := errors.New("database down")
	invalid := &edgekit.Rejection{
		Status: http.StatusOK,
		Reason: "not valid",
	}

	for _, stage := range []string{
		"request_received",
		"before_upstream",
		"upstream_response",
	} {
		for _, tc := range []struct {
			name      string
			rejection *edgekit.Rejection
			err       error
			status    int
			reason    edgekit.RejectionReason
		}{
			{
				name:   "continue",
				status: http.StatusOK,
			},
			{
				name:      "rejection",
				rejection: &limited,
				status:    http.StatusTooManyRequests,
				reason:    "ratelimit.exceeded",
			},
			{
				name:   "failure",
				err:    failure,
				status: http.StatusInternalServerError,
			},
			{
				name:      "rejection and failure",
				rejection: &limited,
				err:       failure,
				status:    http.StatusInternalServerError,
			},
			{
				name:      "invalid rejection and failure",
				rejection: invalid,
				err:       failure,
				status:    http.StatusInternalServerError,
			},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				var requests atomic.Int32
				upstream := countingUpstream(t, &requests)

				hooks := &testHooks{}
				result := func() (*edgekit.Rejection, error) {
					return tc.rejection, tc.err
				}
				switch stage {
				case "request_received":
					hooks.received = func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
						return result()
					}
				case "before_upstream":
					hooks.before = func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
						return result()
					}
				case "upstream_response":
					hooks.response = func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
						return result()
					}
				}

				completionHooks, completed := completions()
				g := newGateway(t, edgekit.Config{
					Routes: []edgekit.Route{{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
					}},
					Hooks: []any{completionHooks, hooks},
				})

				w := httptest.NewRecorder()
				g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

				if w.Code != tc.status {
					t.Errorf("status = %d, want %d", w.Code, tc.status)
				}

				if tc.status != http.StatusTooManyRequests && w.Header().Get("Retry-After") != "" {
					t.Errorf("Retry-After = %q, want none for an ignored rejection", w.Header().Get("Retry-After"))
				}

				// The original failure stays unchanged, also when the
				// ignored rejection is not valid.
				c := receive(t, completed)
				wantFailure := tc.err != nil
				if c.RejectionReason != tc.reason {
					t.Errorf("RequestCompleted() RejectionReason = %q, want %q", c.RejectionReason, tc.reason)
				}

				if got := c.Failure != nil; got != wantFailure {
					t.Fatalf("RequestCompleted() Failure present = %v, want %v", got, wantFailure)
				}

				if wantFailure {
					if c.Failure.Err == nil {
						t.Fatal("RequestCompleted() Failure.Err = nil, want the original error")
					}

					if !errors.Is(c.Failure.Err, failure) {
						t.Errorf("RequestCompleted() Failure.Err = %v, want an error that matches %v", c.Failure.Err, failure)
					}

					if got, want := c.Failure.Err.Error(), failure.Error(); got != want {
						t.Errorf("RequestCompleted() Failure.Err.Error() = %q, want %q", got, want)
					}

					if errors.Is(c.Failure.Err, edgekit.ErrInvalidRejection) {
						t.Errorf("RequestCompleted() Failure.Err = %v, want no ErrInvalidRejection", c.Failure.Err)
					}
				}
			})
		}
	}
}

func TestInvalidRejectionFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rejection edgekit.Rejection
	}{
		{
			name: "zero value",
		},
		{
			name: "success status",
			rejection: edgekit.Rejection{
				Status: http.StatusOK,
				Reason: "acl.denied",
			},
		},
		{
			name: "status below 400",
			rejection: edgekit.Rejection{
				Status: 399,
				Reason: "acl.denied",
			},
		},
		{
			name: "status above 599",
			rejection: edgekit.Rejection{
				Status: 600,
				Reason: "acl.denied",
			},
		},
		{
			name: "empty reason",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
			},
		},
		{
			name: "core reason",
			rejection: edgekit.Rejection{
				Status: http.StatusNotFound,
				Reason: edgekit.ReasonNoRoute,
			},
		},
		{
			name: "reason without extension",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "denied",
			},
		},
		{
			name: "reason with three parts",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.rule.denied",
			},
		},
		{
			name: "empty check",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.",
			},
		},
		{
			name: "empty extension",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: ".denied",
			},
		},
		{
			name: "upper-case reason",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.Denied",
			},
		},
		{
			name: "part starts with a digit",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.1denied",
			},
		},
		{
			name: "dash in reason",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.access-denied",
			},
		},
		{
			name: "request value in reason",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.denied /admin?token=secret",
			},
		},
		{
			name: "reason of 65 bytes",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: edgekit.RejectionReason("acl." + strings.Repeat("d", 61)),
			},
		},
		{
			name: "underscore header name",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.denied",
				Header: http.Header{"X_Reason": {"secret"}},
			},
		},
		{
			name: "empty header name",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.denied",
				Header: http.Header{"": {"secret"}},
			},
		},
		{
			name: "line feed in header value",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.denied",
				Header: http.Header{"X-Reason": {"a\nSet-Cookie: secret"}},
			},
		},
		{
			name: "DEL in header value",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.denied",
				Header: http.Header{"X-Reason": {"a\x7f"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := captureDiagnostics(t)
			later := false
			hooks := edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
				return &tc.rejection, nil
			})
			laterHooks := edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
				later = true

				return nil, nil
			})
			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://unused.test"),
				}},
				Hooks: []any{completionHooks, hooks, laterHooks},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusInternalServerError {
				t.Errorf("ServeHTTP() status = %d, want 500", w.Code)
			}

			if got, want := w.Body.String(), problem(http.StatusInternalServerError, ""); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			for _, name := range []string{"X-Reason", "Set-Cookie"} {
				if got := w.Header().Get(name); got != "" {
					t.Errorf("ServeHTTP() Header.Get(%q) = %q, want none", name, got)
				}
			}

			if later {
				t.Error("a later hook of the phase ran after the invalid rejection")
			}

			c := receive(t, completed)
			if c.RejectionReason != "" {
				t.Errorf("RequestCompleted() RejectionReason = %q, want none", c.RejectionReason)
			}

			if output.Len() != 0 {
				t.Errorf("ServeHTTP() diagnostics = %q, want none", output.String())
			}

			if c.Failure == nil {
				t.Fatal("RequestCompleted() Failure = nil, want an invalid rejection")
			}

			if c.Failure.Phase != edgekit.PhaseRequestReceived {
				t.Errorf("RequestCompleted() Failure.Phase = %v, want RequestReceived", c.Failure.Phase)
			}

			if !errors.Is(c.Failure.Err, edgekit.ErrInvalidRejection) {
				t.Errorf("RequestCompleted() Failure.Err = %v, want ErrInvalidRejection", c.Failure.Err)
			}

			if c.Failure.Err == nil {
				t.Fatal("RequestCompleted() Failure.Err = nil, want an invalid rejection")
			}

			if text := c.Failure.Err.Error(); strings.Contains(text, "secret") || strings.Contains(text, "acl") || strings.Contains(text, "denied") {
				t.Errorf("RequestCompleted() Failure.Err.Error() = %q, want no value of the rejection", text)
			}
		})
	}
}

// rejectionResponseBody records whether the gateway closes a refused response.
type rejectionResponseBody struct {
	io.Reader

	closed bool
}

// Close records the release of the response body.
func (b *rejectionResponseBody) Close() error {
	b.closed = true

	return nil
}

func TestInvalidRejectionInLaterPhases(t *testing.T) {
	for _, phase := range []struct {
		name           string
		phase          edgekit.Phase
		roundTrips     int
		responseClosed bool
	}{
		{
			name:  "before upstream",
			phase: edgekit.PhaseBeforeUpstream,
		},
		{
			name:           "upstream response",
			phase:          edgekit.PhaseUpstreamResponse,
			roundTrips:     1,
			responseClosed: true,
		},
	} {
		for _, tc := range []struct {
			name      string
			rejection edgekit.Rejection
		}{
			{
				name: "invalid status",
				rejection: edgekit.Rejection{
					Status: http.StatusOK,
					Reason: "acl.denied",
					Header: http.Header{
						"Retry-After": {"30"},
					},
				},
			},
			{
				name: "invalid reason",
				rejection: edgekit.Rejection{
					Status: http.StatusForbidden,
					Reason: "secret",
					Header: http.Header{
						"Retry-After": {"30"},
					},
				},
			},
			{
				name: "invalid header",
				rejection: edgekit.Rejection{
					Status: http.StatusForbidden,
					Reason: "acl.denied",
					Header: http.Header{
						"Retry-After": {"30"},
						"X_Reason":    {"secret"},
					},
				},
			},
		} {
			t.Run(phase.name+"/"+tc.name, func(t *testing.T) {
				output := captureDiagnostics(t)
				laterRan := false
				var rejecting, later any
				if phase.phase == edgekit.PhaseBeforeUpstream {
					rejecting = edgekit.BeforeUpstreamHookFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
						return &tc.rejection, nil
					})
					later = edgekit.BeforeUpstreamHookFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
						laterRan = true

						return nil, nil
					})
				} else {
					rejecting = edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
						return &tc.rejection, nil
					})
					later = edgekit.UpstreamResponseHookFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
						laterRan = true

						return nil, nil
					})
				}

				completionHooks, completed := completions()
				providers := []any{rejecting, later, completionHooks}
				if phase.phase == edgekit.PhaseUpstreamResponse {
					// Response hooks run in reverse provider order.
					providers[0], providers[1] = providers[1], providers[0]
				}

				body := &rejectionResponseBody{
					Reader: strings.NewReader("upstream response"),
				}
				roundTrips := 0
				g := newGateway(t, edgekit.Config{
					Routes: []edgekit.Route{{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					}},
					Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
						roundTrips++

						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       body,
							Request:    r,
						}, nil
					}),
					Hooks: providers,
				})

				w := httptest.NewRecorder()
				g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(t.Context()))

				if w.Code != http.StatusInternalServerError {
					t.Errorf("ServeHTTP() status = %d, want 500", w.Code)
				}

				if got, want := w.Body.String(), problem(http.StatusInternalServerError, ""); got != want {
					t.Errorf("ServeHTTP() body = %q, want %q", got, want)
				}

				if got := w.Header().Get("Retry-After"); got != "" {
					t.Errorf("ServeHTTP() Retry-After = %q, want none", got)
				}

				if laterRan {
					t.Error("ServeHTTP() later hook ran = true, want false")
				}

				if roundTrips != phase.roundTrips {
					t.Errorf("ServeHTTP() round trips = %d, want %d", roundTrips, phase.roundTrips)
				}

				if body.closed != phase.responseClosed {
					t.Errorf("ServeHTTP() upstream body closed = %v, want %v", body.closed, phase.responseClosed)
				}

				if output.Len() != 0 {
					t.Errorf("ServeHTTP() diagnostics = %q, want none", output.String())
				}

				c := receive(t, completed)
				if c.RejectionReason != "" {
					t.Errorf("RequestCompleted() RejectionReason = %q, want none", c.RejectionReason)
				}

				if c.Aborted {
					t.Error("RequestCompleted() Aborted = true, want false")
				}

				if c.Failure == nil {
					t.Fatal("RequestCompleted() Failure = nil, want an invalid rejection")
				}

				if c.Failure.Phase != phase.phase {
					t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", c.Failure.Phase, phase.phase)
				}

				if !errors.Is(c.Failure.Err, edgekit.ErrInvalidRejection) {
					t.Errorf("RequestCompleted() Failure.Err = %v, want ErrInvalidRejection", c.Failure.Err)
				}

				if c.Failure.Err == nil {
					t.Fatal("RequestCompleted() Failure.Err = nil, want an invalid rejection")
				}

				for _, value := range []string{"acl", "denied", "secret"} {
					if text := c.Failure.Err.Error(); strings.Contains(text, value) {
						t.Errorf("RequestCompleted() Failure.Err.Error() = %q, want no rejection value %q", text, value)
					}
				}
			})
		}
	}
}

func TestValidRejectionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rejection edgekit.Rejection
	}{
		{
			name: "status 400",
			rejection: edgekit.Rejection{
				Status: http.StatusBadRequest,
				Reason: "a.b",
			},
		},
		{
			name: "status 599",
			rejection: edgekit.Rejection{
				Status: 599,
				Reason: "a.b",
			},
		},
		{
			name: "reason of 64 bytes",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: edgekit.RejectionReason("acl." + strings.Repeat("d", 60)),
			},
		},
		{
			name: "digits and underscores",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl_2.denied_by_rule_7",
			},
		},
		{
			name: "tab in header value",
			rejection: edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "acl.denied",
				Header: http.Header{"X-Reason": {"a\tb"}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
				return &tc.rejection, nil
			})
			completionHooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://unused.test"),
				}},
				Hooks: []any{completionHooks, hooks},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != tc.rejection.Status {
				t.Errorf("ServeHTTP() status = %d, want %d", w.Code, tc.rejection.Status)
			}

			if got, want := w.Body.String(), problem(tc.rejection.Status, tc.rejection.Reason); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			c := receive(t, completed)
			if c.RejectionReason != tc.rejection.Reason {
				t.Errorf("RequestCompleted() RejectionReason = %q, want %q", c.RejectionReason, tc.rejection.Reason)
			}

			if c.Failure != nil {
				t.Errorf("RequestCompleted() Failure = %+v, want nil", c.Failure)
			}
		})
	}
}

func TestSharedRejectionIsNotChanged(t *testing.T) {
	shared := edgekit.Rejection{
		Status: http.StatusTooManyRequests,
		Reason: "ratelimit.exceeded",
		Header: http.Header{
			"Retry-After": {"30"},
			"Connection":  {"X-Hop"},
			"X-Hop":       {"private"},
		},
	}
	want := shared.Header.Clone()
	hooks := edgekit.RequestReceivedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
		return &shared, nil
	})
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://unused.test"),
		}},
		Hooks: []any{hooks},
	})

	// The race detector reports a write to the shared map.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))
			w.Header().Set("Retry-After", "changed")
		})
	}
	wg.Wait()

	if !maps.EqualFunc(shared.Header, want, slices.Equal[[]string]) {
		t.Errorf("shared header = %v, want %v", shared.Header, want)
	}
}
