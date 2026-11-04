package edgekit_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/go-edgekit/edgekit"
)

func TestDefaultProblemResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		method    string
		rejection *edgekit.Rejection
		err       error
		status    int
		body      string
	}{
		{
			name:      "rejection",
			method:    http.MethodGet,
			rejection: &limited,
			status:    http.StatusTooManyRequests,
			body:      `{"type":"about:blank","title":"Too Many Requests","status":429,"reason":"ratelimit.exceeded"}`,
		},
		{
			name:   "failure",
			method: http.MethodGet,
			err:    errors.New("database password=secret"),
			status: http.StatusInternalServerError,
			body:   `{"type":"about:blank","title":"Internal Server Error","status":500}`,
		},
		{
			name:   "status without status text",
			method: http.MethodGet,
			rejection: &edgekit.Rejection{
				Status: 499,
				Reason: "client.closed",
			},
			status: 499,
			body:   `{"type":"about:blank","status":499,"reason":"client.closed"}`,
		},
		{
			name:      "HEAD",
			method:    http.MethodHead,
			rejection: &limited,
			status:    http.StatusTooManyRequests,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hooks := requestReceivedFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
				return tc.rejection, tc.err
			})

			// The middleware sets framing and type fields for content that
			// is not sent, in two letter cases.
			middleware := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header()["content-length"] = []string{"5"}
					w.Header().Set("Content-Type", "text/html")
					w.Header()["content-type"] = []string{"text/html"}
					next.ServeHTTP(w, r)
				})
			}
			server := httptest.NewServer(newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://unused.test"),
				}},
				Middleware: []edgekit.Middleware{middleware},
				Hooks:      []any{hooks},
			}))
			defer server.Close()

			r, err := http.NewRequestWithContext(t.Context(), tc.method, server.URL+"/api/x", nil)
			if err != nil {
				t.Fatalf("NewRequestWithContext(%s, /api/x) error = %v, want nil", tc.method, err)
			}

			resp, err := server.Client().Do(r)
			if err != nil {
				t.Fatalf("Client.Do(%s /api/x) error = %v, want nil", tc.method, err)
			}
			defer func() { _ = resp.Body.Close() }()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("ReadAll(%s /api/x response body) error = %v, want nil", tc.method, err)
			}

			if resp.StatusCode != tc.status {
				t.Errorf("ServeHTTP() status = %d, want %d", resp.StatusCode, tc.status)
			}

			if string(body) != tc.body {
				t.Errorf("ServeHTTP() body = %q, want %q", body, tc.body)
			}

			if got := resp.Header.Values("Content-Type"); !slices.Equal(got, []string{"application/problem+json"}) {
				t.Errorf("Content-Type = %q, want only application/problem+json", got)
			}

			if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", resp.Header.Get("X-Content-Type-Options"))
			}

			if tc.method == http.MethodGet && resp.ContentLength != int64(len(tc.body)) {
				t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(tc.body))
			}
		})
	}
}

func TestProblemResponseOwnsContentTypeOptions(t *testing.T) {
	for _, protocol := range []struct {
		name  string
		http2 bool
	}{
		{
			name: "HTTP/1.1",
		},
		{
			name:  "HTTP/2",
			http2: true,
		},
	} {
		t.Run(protocol.name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				renderer edgekit.Renderer
			}{
				{
					name: "default renderer",
				},
				{
					name: "custom renderer",
					renderer: func(context.Context, edgekit.Problem) (string, []byte) {
						return "text/html", []byte("custom")
					},
				},
			} {
				t.Run(tc.name, func(t *testing.T) {
					middleware := func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("X-Content-Type-Options", "unsafe")
							w.Header()["X-CONTENT-TYPE-OPTIONS"] = []string{"unsafe"}
							w.Header()["x-content-type-options"] = []string{"", "unsafe"}
							next.ServeHTTP(w, r)
						})
					}
					g := newGateway(t, edgekit.Config{
						Routes: []edgekit.Route{{
							Prefix:   "/api/",
							Upstream: static(t, "http://unused.test"),
						}},
						Middleware: []edgekit.Middleware{middleware},
						Renderer:   tc.renderer,
					})
					server := httptest.NewUnstartedServer(g)
					server.EnableHTTP2 = protocol.http2
					server.StartTLS()
					defer server.Close()

					r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/missing", nil)
					if err != nil {
						t.Fatalf("NewRequestWithContext(GET, /missing) error = %v, want nil", err)
					}

					resp, err := server.Client().Do(r)
					if err != nil {
						t.Fatalf("Client.Do(GET /missing) error = %v, want nil", err)
					}
					defer func() { _ = resp.Body.Close() }()

					if protocol.http2 != (resp.ProtoMajor == 2) {
						t.Fatalf("protocol = %s, want HTTP/2 = %v", resp.Proto, protocol.http2)
					}

					if resp.StatusCode != http.StatusNotFound {
						t.Errorf("ServeHTTP() status = %d, want 404", resp.StatusCode)
					}

					if got := resp.Header.Values("X-Content-Type-Options"); !slices.Equal(got, []string{"nosniff"}) {
						t.Errorf("X-Content-Type-Options = %q, want only nosniff", got)
					}
				})
			}
		})
	}
}

// rendered is what a custom renderer got.
type rendered struct {
	problem    edgekit.Problem
	hasContext bool
	route      bool
}

func TestCustomRendererForEverySource(t *testing.T) {
	for _, tc := range []struct {
		name            string
		target          string
		hookPhase       edgekit.Phase
		rejection       *edgekit.Rejection
		hookErr         error
		selector        error
		transport       error
		invalidUpstream bool
		invalidOutbound bool
		upgradeProtocol string
		lose            bool
		problem         edgekit.Problem
		failurePhase    edgekit.Phase
		hasContext      bool
		route           bool
	}{
		{
			name:   "secure default",
			target: "/api/../x",
			problem: edgekit.Problem{
				Status: http.StatusBadRequest,
				Reason: edgekit.ReasonInvalidPath,
			},
			hasContext: true,
		},
		{
			name:   "routing miss",
			target: "/other",
			problem: edgekit.Problem{
				Status: http.StatusNotFound,
				Reason: edgekit.ReasonNoRoute,
			},
			hasContext: true,
		},
		{
			name:      "request received rejection",
			target:    "/api/x",
			hookPhase: edgekit.PhaseRequestReceived,
			rejection: &limited,
			problem: edgekit.Problem{
				Status: http.StatusTooManyRequests,
				Reason: "ratelimit.exceeded",
			},
			hasContext: true,
		},
		{
			name:      "request received failure",
			target:    "/api/x",
			hookPhase: edgekit.PhaseRequestReceived,
			hookErr:   errors.New("secret"),
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseRequestReceived,
			hasContext:   true,
		},
		{
			name:      "before upstream rejection",
			target:    "/api/x",
			hookPhase: edgekit.PhaseBeforeUpstream,
			rejection: &limited,
			problem: edgekit.Problem{
				Status: http.StatusTooManyRequests,
				Reason: "ratelimit.exceeded",
			},
			hasContext: true,
			route:      true,
		},
		{
			name:      "before upstream failure",
			target:    "/api/x",
			hookPhase: edgekit.PhaseBeforeUpstream,
			hookErr:   errors.New("secret"),
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseBeforeUpstream,
			hasContext:   true,
			route:        true,
		},
		{
			name:      "upstream response rejection",
			target:    "/api/x",
			hookPhase: edgekit.PhaseUpstreamResponse,
			rejection: &limited,
			problem: edgekit.Problem{
				Status: http.StatusTooManyRequests,
				Reason: "ratelimit.exceeded",
			},
			hasContext: true,
			route:      true,
		},
		{
			name:      "upstream response failure",
			target:    "/api/x",
			hookPhase: edgekit.PhaseUpstreamResponse,
			hookErr:   errors.New("secret"),
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseUpstreamResponse,
			hasContext:   true,
			route:        true,
		},
		{
			name:     "no upstream available",
			target:   "/api/x",
			selector: edgekit.ErrNoUpstreamAvailable,
			problem: edgekit.Problem{
				Status: http.StatusServiceUnavailable,
			},
			failurePhase: edgekit.PhaseUpstreamSelection,
			hasContext:   true,
			route:        true,
		},
		{
			name:     "selector failure",
			target:   "/api/x",
			selector: errors.New("secret"),
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseUpstreamSelection,
			hasContext:   true,
			route:        true,
		},
		{
			name:            "invalid upstream URL",
			target:          "/api/x",
			invalidUpstream: true,
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseUpstreamSelection,
			hasContext:   true,
			route:        true,
		},
		{
			name:            "invalid outbound header without hooks",
			target:          "/api/x",
			invalidOutbound: true,
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseBeforeUpstream,
			hasContext:   true,
			route:        true,
		},
		{
			name:      "round-trip failure",
			target:    "/api/x",
			transport: errors.New("secret"),
			problem: edgekit.Problem{
				Status: http.StatusBadGateway,
			},
			failurePhase: edgekit.PhaseRoundTrip,
			hasContext:   true,
			route:        true,
		},
		{
			name:            "protocol mismatch",
			target:          "/api/x",
			upgradeProtocol: "other",
			problem: edgekit.Problem{
				Status: http.StatusBadGateway,
			},
			failurePhase: edgekit.PhaseProtocolSwitch,
			hasContext:   true,
			route:        true,
		},
		{
			name:            "writer without hijack",
			target:          "/api/x",
			upgradeProtocol: "echo",
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
			failurePhase: edgekit.PhaseProtocolSwitch,
			hasContext:   true,
			route:        true,
		},
		{
			name:   "lost gateway context",
			target: "/api/x",
			lose:   true,
			problem: edgekit.Problem{
				Status: http.StatusInternalServerError,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := make(chan rendered, 2)
			renderer := func(ctx context.Context, p edgekit.Problem) (string, []byte) {
				rc, ok := edgekit.FromContext(ctx)
				route := false
				if ok {
					_, route = rc.RoutePrefix()
				}

				calls <- rendered{
					problem:    p,
					hasContext: ok,
					route:      route,
				}

				return "text/plain; charset=utf-8", []byte("custom")
			}
			completionHooks, completed := completions()
			providers := []any{completionHooks}
			switch tc.hookPhase {
			case edgekit.PhaseRequestReceived:
				providers = append(providers, requestReceivedFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					return tc.rejection, tc.hookErr
				}))
			case edgekit.PhaseBeforeUpstream:
				providers = append(providers, beforeUpstreamFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					return tc.rejection, tc.hookErr
				}))
			case edgekit.PhaseUpstreamResponse:
				providers = append(providers, upstreamResponseFunc(func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					return tc.rejection, tc.hookErr
				}))
			default:
				if tc.hookPhase != edgekit.PhaseUnknown {
					t.Fatalf("test setup: hook phase = %v, want a processing phase or no hook", tc.hookPhase)
				}
			}
			selector := edgekit.UpstreamSelectorFunc(func(*http.Request) (*url.URL, error) {
				if tc.selector != nil {
					return nil, tc.selector
				}

				if tc.invalidUpstream {
					return &url.URL{
						Scheme: "file",
						Host:   "upstream.test",
					}, nil
				}

				return parseURL(t, "http://upstream.test"), nil
			})
			transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if tc.transport != nil {
					return nil, tc.transport
				}

				if tc.upgradeProtocol != "" {
					body, peer := net.Pipe()
					t.Cleanup(func() { _ = body.Close() })
					t.Cleanup(func() { _ = peer.Close() })

					return &http.Response{
						StatusCode: http.StatusSwitchingProtocols,
						Header: http.Header{
							"Connection": {"Upgrade"},
							"Upgrade":    {"echo"},
						},
						Body:    body,
						Request: r,
					}, nil
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       http.NoBody,
					Request:    r,
				}, nil
			})
			lose := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.lose {
						r = r.WithContext(context.Background())
					}

					if tc.invalidOutbound {
						r = r.Clone(r.Context())
						r.Header["X_Invalid"] = []string{"secret"}
					}

					next.ServeHTTP(w, r)
				})
			}
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: selector,
				}},
				Middleware: []edgekit.Middleware{lose},
				Hooks:      providers,
				Transport:  transport,
				Renderer:   renderer,
			})

			r := httptest.NewRequest(http.MethodGet, tc.target, nil).WithContext(t.Context())
			if tc.upgradeProtocol != "" {
				r.Header.Set("Connection", "Upgrade")
				r.Header.Set("Upgrade", tc.upgradeProtocol)
			}

			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)

			if w.Code != tc.problem.Status {
				t.Errorf("ServeHTTP() status = %d, want %d", w.Code, tc.problem.Status)
			}

			if got := w.Body.String(); got != "custom" {
				t.Errorf("ServeHTTP() body = %q, want custom", got)
			}

			if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("ServeHTTP() Content-Type = %q, want text/plain; charset=utf-8", got)
			}

			c := receive(t, completed)
			if c.RejectionReason != tc.problem.Reason {
				t.Errorf("RequestCompleted() RejectionReason = %q, want %q", c.RejectionReason, tc.problem.Reason)
			}

			if tc.failurePhase == edgekit.PhaseUnknown {
				if c.Failure != nil {
					t.Errorf("RequestCompleted() Failure = %+v, want nil", c.Failure)
				}
			} else {
				if c.Failure == nil {
					t.Fatal("RequestCompleted() Failure = nil, want a recorded failure")
				}

				if c.Failure.Phase != tc.failurePhase {
					t.Errorf("RequestCompleted() Failure.Phase = %v, want %v", c.Failure.Phase, tc.failurePhase)
				}
			}

			got := receive(t, calls)
			want := rendered{
				problem:    tc.problem,
				hasContext: tc.hasContext,
				route:      tc.route,
			}
			if got != want {
				t.Errorf("renderer got %+v, want %+v", got, want)
			}

			if len(calls) != 0 {
				t.Error("renderer ran more than one time")
			}
		})
	}
}

func TestInvalidRendererContentTypeFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
	}{
		{
			name: "empty",
		},
		{
			name:        "header injection",
			contentType: "text/plain\r\nX-Injected: 1",
		},
		{
			name:        "NUL",
			contentType: "text/plain\x00",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{{
					Prefix:   "/api/",
					Upstream: static(t, "http://unused.test"),
				}},
				Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
					return tc.contentType, []byte("custom")
				},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/other", nil))

			if w.Code != http.StatusNotFound {
				t.Errorf("ServeHTTP() status = %d, want 404", w.Code)
			}

			if got, want := w.Body.String(), problem(http.StatusNotFound, edgekit.ReasonNoRoute); got != want {
				t.Errorf("ServeHTTP() body = %q, want %q", got, want)
			}

			if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("ServeHTTP() Content-Type = %q, want application/problem+json", got)
			}

			if got := w.Header().Get("X-Injected"); got != "" {
				t.Errorf("ServeHTTP() X-Injected = %q, want none", got)
			}
		})
	}
}

func TestRendererPanicContinuesAfterCompletion(t *testing.T) {
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: static(t, "http://unused.test"),
		}},
		Hooks: []any{completionHooks},
		Renderer: func(context.Context, edgekit.Problem) (string, []byte) {
			panic("renderer defect")
		},
	})

	recovered := func() (recovered any) {
		defer func() {
			recovered = recover()
		}()

		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/../x", nil))

		return nil
	}()

	if recovered != "renderer defect" {
		t.Errorf("recovered %v, want the renderer panic", recovered)
	}

	c := receive(t, completed)
	if !c.Aborted {
		t.Error("RequestCompleted() Aborted = false, want true")
	}

	if c.RejectionReason != edgekit.ReasonInvalidPath {
		t.Errorf("RequestCompleted() RejectionReason = %q, want %q", c.RejectionReason, edgekit.ReasonInvalidPath)
	}
}
