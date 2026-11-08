package edgekit_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-edgekit/edgekit"
)

func TestSharedReaderUsesEffectiveRouteLimit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gateway      edgekit.BodyLimit
		route        edgekit.BodyLimit
		size         int
		wantOverflow bool
	}{
		{
			name:    "route above gateway limit",
			gateway: edgekit.MaxBodyBytes(2),
			route:   edgekit.MaxBodyBytes(8),
			size:    8,
		},
		{
			name:  "route above default limit",
			route: edgekit.MaxBodyBytes(edgekit.DefaultBodyLimit + 1),
			size:  edgekit.DefaultBodyLimit + 1,
		},
		{
			name:         "smaller route",
			gateway:      edgekit.MaxBodyBytes(8),
			route:        edgekit.MaxBodyBytes(2),
			size:         3,
			wantOverflow: true,
		},
		{
			name:         "zero route",
			route:        edgekit.MaxBodyBytes(0),
			size:         1,
			wantOverflow: true,
		},
		{
			name:    "disabled route",
			gateway: edgekit.MaxBodyBytes(0),
			route:   edgekit.NoBodyLimit(),
			size:    8,
		},
		{
			name:    "inherited exact limit",
			gateway: edgekit.MaxBodyBytes(8),
			size:    8,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamBytes atomic.Int64
			var requests atomic.Int32
			upstream := readingUpstream(t, &upstreamBytes, &requests)
			var readErr error
			var matched bool
			g := newGateway(t, edgekit.Config{
				BodyLimit: tc.gateway,
				Routes: []edgekit.Route{
					{
						Prefix:    "/api/",
						Upstream:  static(t, upstream.URL),
						BodyLimit: tc.route,
					},
				},
				SharedMiddleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							rc, _ := edgekit.FromContext(r.Context())
							_, matched = rc.RoutePrefix()
							data, err := io.ReadAll(r.Body)
							readErr = err
							if err != nil {
								w.WriteHeader(http.StatusRequestEntityTooLarge)

								return
							}

							_ = r.Body.Close()
							out := r.Clone(r.Context())
							out.Body = io.NopCloser(strings.NewReader(string(data)))
							next.ServeHTTP(w, out)
						})
					},
				},
			})
			w := httptest.NewRecorder()
			g.ServeHTTP(w, bodyRequest(strings.Repeat("x", tc.size), false))

			if !matched {
				t.Errorf("ServeHTTP(POST /api/upload, %d bytes): shared middleware ran without a matched route", tc.size)
			}
			if got := errors.Is(readErr, edgekit.ErrRequestBodyTooLarge); got != tc.wantOverflow {
				t.Errorf("ServeHTTP(POST /api/upload, %d bytes): shared read error = %v, want overflow %v", tc.size, readErr, tc.wantOverflow)
			}
			wantStatus, wantRequests := http.StatusOK, int32(1)
			if tc.wantOverflow {
				wantStatus, wantRequests = http.StatusRequestEntityTooLarge, 0
			}
			if w.Code != wantStatus {
				t.Errorf("ServeHTTP(POST /api/upload, %d bytes) status = %d, want %d", tc.size, w.Code, wantStatus)
			}
			if got := requests.Load(); got != wantRequests {
				t.Errorf("ServeHTTP(POST /api/upload, %d bytes): upstream requests = %d, want %d", tc.size, got, wantRequests)
			}
			if got := upstreamBytes.Load(); !tc.wantOverflow && got != int64(tc.size) {
				t.Errorf("ServeHTTP(POST /api/upload, %d bytes): upstream bytes = %d, want %d", tc.size, got, tc.size)
			}
		})
	}
}

func TestStreamingDoesNotExposeUnprotectedCallerReplay(t *testing.T) {
	var readErr error
	var replayExposed bool
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, "http://unused.test"),
				BodyLimit: edgekit.MaxBodyBytes(4),
			},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					replayExposed = r.GetBody != nil
					_, readErr = io.ReadAll(r.Body)
					w.WriteHeader(http.StatusRequestEntityTooLarge)
				})
			},
		},
	})
	r := bodyRequest("abcde", false)
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("abcde")), nil
	}
	g.ServeHTTP(httptest.NewRecorder(), r)

	if replayExposed {
		t.Error("ServeHTTP(POST /api/upload with caller GetBody): middleware got a GetBody, want nil")
	}
	if !errors.Is(readErr, edgekit.ErrRequestBodyTooLarge) {
		t.Errorf("ServeHTTP(POST /api/upload with caller GetBody): middleware read error = %v, want %v", readErr, edgekit.ErrRequestBodyTooLarge)
	}
	if r.GetBody == nil {
		t.Error("ServeHTTP(POST /api/upload with caller GetBody): caller request GetBody = nil, want unchanged")
	}
}

func TestFixedRouteAndOutboundRewrites(t *testing.T) {
	for _, tc := range []struct {
		name       string
		change     func(*http.Request)
		wantStatus int
		wantPath   string
	}{
		{
			name:       "other route prefix",
			change:     func(r *http.Request) { r.URL.Path = "/strict/item" },
			wantStatus: http.StatusOK,
			wantPath:   "/base/strict/item",
		},
		{
			name:       "dot segments",
			change:     func(r *http.Request) { r.URL.Path = "/api/../secret" },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "rejected query",
			change:     func(r *http.Request) { r.URL.RawQuery = "x=%zz" },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "absolute URL",
			change:     func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", "other.test" },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "invalid method",
			change:     func(r *http.Request) { r.Method = "GET\nprivate" },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "empty method",
			change:     func(r *http.Request) { r.Method = "" },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "token method",
			change:     func(r *http.Request) { r.Method = "METHOD_!~" },
			wantStatus: http.StatusOK,
			wantPath:   "/base/api/upload",
		},
		{
			name:       "invalid host",
			change:     func(r *http.Request) { r.Host = "client\r\nprivate" },
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "nil URL",
			change:     func(r *http.Request) { r.URL = nil },
			wantStatus: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selected, forwarded int
			var path string
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:    "/api/",
						BodyLimit: edgekit.MaxBodyBytes(8),
						Upstream: edgekit.UpstreamSelectorFunc(func(*http.Request) (*url.URL, error) {
							selected++

							return url.Parse("http://upstream.test/base")
						}),
					},
					{
						Prefix:    "/strict/",
						BodyLimit: edgekit.MaxBodyBytes(0),
						Upstream:  static(t, "http://unused.test"),
					},
				},
				SharedMiddleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							out := r.Clone(r.Context())
							tc.change(out)
							next.ServeHTTP(w, out)
						})
					},
				},
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					forwarded++
					path = r.URL.Path
					_, err := io.Copy(io.Discard, r.Body)
					_ = r.Body.Close()
					if err != nil {
						return nil, err
					}

					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       http.NoBody,
					}, nil
				}),
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{collector},
				},
			})
			w := httptest.NewRecorder()
			g.ServeHTTP(w, bodyRequest("body", true))
			c := receive(t, completed)

			type outcome struct {
				status    int
				path      string
				route     string
				selected  int
				forwarded int
			}
			got := outcome{
				status:    w.Code,
				path:      path,
				route:     c.RoutePrefix,
				selected:  selected,
				forwarded: forwarded,
			}
			want := outcome{
				status:    tc.wantStatus,
				path:      tc.wantPath,
				route:     "/api/",
				selected:  1,
				forwarded: 1,
			}
			if tc.wantStatus == http.StatusInternalServerError {
				want.selected, want.forwarded = 0, 0
			}
			if got != want {
				t.Errorf("ServeHTTP(POST /api/upload) after rewrite = %+v, want %+v", got, want)
			}
			if tc.wantStatus == http.StatusInternalServerError {
				if c.Failure == nil || c.Failure.Phase != edgekit.PhaseBeforeUpstream {
					t.Errorf("ServeHTTP(POST /api/upload) after rewrite: Completion.Failure = %+v, want a failure in %v", c.Failure, edgekit.PhaseBeforeUpstream)
				}
				if c.RejectionReason != "" {
					t.Errorf("ServeHTTP(POST /api/upload) after rewrite: Completion.RejectionReason = %q, want none", c.RejectionReason)
				}
			}
		})
	}
}

func TestRouteMatchedSkipsDeclaredRejection(t *testing.T) {
	var matched, shared bool
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:    "/api/",
				Upstream:  static(t, "http://unused.test"),
				BodyLimit: edgekit.MaxBodyBytes(0),
			},
		},
		Hooks: edgekit.Hooks{
			RouteMatched: []edgekit.RouteMatchedHook{
				edgekit.RouteMatchedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					matched = true

					return nil, nil
				}),
			},
			RequestCompleted: []edgekit.RequestCompletedHook{collector},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { shared = true })
			},
		},
	})
	w := httptest.NewRecorder()
	g.ServeHTTP(w, bodyRequest("x", true))
	c := receive(t, completed)

	type outcome struct {
		hookRan   bool
		sharedRan bool
		status    int
		route     string
		reason    edgekit.RejectionReason
	}
	got := outcome{
		hookRan:   matched,
		sharedRan: shared,
		status:    w.Code,
		route:     c.RoutePrefix,
		reason:    c.RejectionReason,
	}
	want := outcome{
		status: http.StatusRequestEntityTooLarge,
		route:  "/api/",
		reason: edgekit.ReasonBodyTooLarge,
	}
	if got != want {
		t.Errorf("ServeHTTP(POST /api/upload, declared 1 byte over a 0-byte limit) = %+v, want %+v", got, want)
	}
}
