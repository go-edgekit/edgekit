package edgekit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// countingUpstream returns a started upstream server that counts requests and
// answers "ok".
func countingUpstream(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("X-Upstream", "must-not-escape")
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(server.Close)

	return server
}

// unwinding returns middleware that sets *unwound when the next handler
// returns or unwinds.
func unwinding(unwound *bool) edgekit.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				*unwound = true
			}()

			next.ServeHTTP(w, r)
		})
	}
}

func TestLifecycleContextAndOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "original")
		w.Header().Set("X-Outbound-Seen", r.Header.Get("X-Outbound"))
		_, _ = io.WriteString(w, "body")
	}))
	defer upstream.Close()

	type derivedKey struct{}
	key := edgekit.NewStateKey[string]()

	type contextObservation struct {
		phase string
		ctx   context.Context
		rc    *edgekit.RequestContext
	}
	var (
		steps    []string
		shared   *edgekit.RequestContext
		contexts []contextObservation
	)

	first := &testHooks{
		received: func(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
			contexts = append(contexts, contextObservation{
				phase: "RouteMatched",
				ctx:   ctx,
				rc:    rc,
			})
			if ctx.Value(derivedKey{}) != nil || r.Context() != ctx {
				t.Error("RouteMatched got a later middleware context")
			}

			if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
				t.Error("RouteMatched has no fixed route")
			}

			if _, ok := edgekit.State(rc, key); ok {
				t.Error("RouteMatched got later middleware state")
			}
			edgekit.SetState(rc, key, "matched")

			steps = append(steps, "received:first")

			return nil, nil
		},
		before: func(ctx context.Context, rc *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			contexts = append(contexts, contextObservation{
				phase: "BeforeUpstream",
				ctx:   ctx,
				rc:    rc,
			})
			if ctx.Value(derivedKey{}) != "derived" {
				t.Error("BeforeUpstream lost the execution context")
			}

			if value, _ := edgekit.State(rc, key); value != "route" {
				t.Errorf("BeforeUpstream state = %q, want the state of route middleware", value)
			}

			selected, ok := rc.SelectedUpstream()
			if !ok || selected.String() != upstream.URL || out.URL().Host != selected.Host {
				t.Error("BeforeUpstream did not get the rewritten request to the selected upstream")
			}

			out.Header().Set("X-Outbound", "first")
			steps = append(steps, "before:first")

			return nil, nil
		},
		response: func(ctx context.Context, rc *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
			contexts = append(contexts, contextObservation{
				phase: "UpstreamResponse",
				ctx:   ctx,
				rc:    rc,
			})
			if ctx.Value(derivedKey{}) != "derived" {
				t.Error("UpstreamResponse lost the execution context")
			}

			if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
				t.Errorf("UpstreamResponse route = %q, %v; want /api/", prefix, ok)
			}

			selected, _ := rc.SelectedUpstream()
			selected.Host = "changed.test"
			if again, _ := rc.SelectedUpstream(); again.Host == "changed.test" {
				t.Error("SelectedUpstream returned the stored URL, not a copy")
			}

			if resp.Header.Get("X-Upstream") != "modified" {
				t.Error("UpstreamResponse hooks did not run in configured order")
			}

			steps = append(steps, "response:first")

			return nil, nil
		},
		completed: func(ctx context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
			contexts = append(contexts, contextObservation{
				phase: "RequestCompleted",
				ctx:   ctx,
				rc:    rc,
			})
			if ctx.Value(derivedKey{}) != nil {
				t.Error("RequestCompleted got a context that middleware derived")
			}

			if value, _ := edgekit.State(rc, key); value != "unwound" {
				t.Errorf("RequestCompleted state = %q, want the state after shared middleware returned", value)
			}

			if c.RoutePrefix != "/api/" || c.SelectedUpstream == nil || c.SelectedUpstream.String() != upstream.URL {
				t.Errorf("completion route = %q, upstream = %v; want /api/ and %s", c.RoutePrefix, c.SelectedUpstream, upstream.URL)
			}

			if c.Failure != nil || c.Aborted || c.RejectionReason != "" {
				t.Errorf("completion = %+v, want no failure, abort, or rejection", c)
			}

			if !c.Start.Equal(shared.StartTime()) || c.Duration <= 0 {
				t.Errorf("completion start = %v, duration = %v; want %v and > 0", c.Start, c.Duration, shared.StartTime())
			}

			steps = append(steps, "completed:first")
		},
	}
	second := &testHooks{
		received: func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
			steps = append(steps, "received:second")

			return nil, nil
		},
		before: func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			if out.Header().Get("X-Outbound") != "first" {
				t.Error("BeforeUpstream hooks ran out of order")
			}

			steps = append(steps, "before:second")

			return nil, nil
		},
		response: func(_ context.Context, _ *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
			resp.Header.Set("X-Upstream", "modified")
			steps = append(steps, "response:second")

			return nil, nil
		},
		completed: func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
			// Each hook gets its own snapshot, so the change must not reach
			// the next hook.
			c.SelectedUpstream.Host = "changed.test"
			steps = append(steps, "completed:second")
		},
	}
	sharedMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ok bool
			shared, ok = edgekit.FromContext(r.Context())
			if !ok || shared.StartTime().IsZero() || shared.StartTime().After(time.Now()) {
				t.Fatal("gateway context is not ready before shared middleware")
			}

			if value, _ := edgekit.State(shared, key); value != "matched" {
				t.Errorf("shared middleware state = %q, want state from RouteMatched", value)
			}
			edgekit.SetState(shared, key, "shared")
			steps = append(steps, "shared:before")
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), derivedKey{}, "derived")))
			edgekit.SetState(shared, key, "unwound")
			steps = append(steps, "shared:after")
		})
	}
	routeMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc, _ := edgekit.FromContext(r.Context())
			if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
				t.Errorf("route middleware route = %q, %v; want /api/", prefix, ok)
			}

			if _, ok := rc.SelectedUpstream(); ok {
				t.Error("upstream selected before route middleware")
			}

			edgekit.SetState(rc, key, "route")
			steps = append(steps, "route:before")
			next.ServeHTTP(w, r)
			steps = append(steps, "route:after")
		})
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:     "/api/",
				Upstream:   static(t, upstream.URL),
				Middleware: []edgekit.Middleware{routeMiddleware},
			},
		},
		SharedMiddleware: []edgekit.Middleware{sharedMiddleware},
		Hooks: edgekit.Hooks{
			RouteMatched: []edgekit.RouteMatchedHook{
				first,
				second,
			},
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				first,
				second,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				second,
				first,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				second,
				first,
			},
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/example", nil))

	if len(contexts) != 4 {
		t.Fatalf("recorded %d hook contexts, want 4", len(contexts))
	}

	for _, observed := range contexts {
		fromContext, ok := edgekit.FromContext(observed.ctx)
		if !ok || fromContext != shared || observed.rc != shared {
			t.Errorf("%s: gateway context is not the one of shared middleware", observed.phase)
		}
	}

	if w.Code != http.StatusOK || w.Body.String() != "body" {
		t.Errorf("response = %d %q, want 200 %q", w.Code, w.Body.String(), "body")
	}

	if w.Header().Get("X-Upstream") != "modified" || w.Header().Get("X-Outbound-Seen") != "first" {
		t.Errorf("response headers = %v, want the changes of the hooks", w.Header())
	}

	want := []string{
		"received:first",
		"received:second",
		"shared:before",
		"route:before",
		"before:first",
		"before:second",
		"response:second",
		"response:first",
		"route:after",
		"shared:after",
		"completed:second",
		"completed:first",
	}
	if !slices.Equal(steps, want) {
		t.Errorf("order = %v\nwant    %v", steps, want)
	}
}

func TestCompletionOnEarlyExits(t *testing.T) {
	var requests atomic.Int32
	upstream := countingUpstream(t, &requests)
	key := edgekit.NewStateKey[string]()

	for _, tc := range []struct {
		name           string
		path           string
		sharedDeny     bool
		routeDeny      bool
		hookErr        error
		status         int
		wantReceived   bool
		wantMiddleware bool
		wantRoute      string
		wantReason     edgekit.RejectionReason
		wantPhase      edgekit.Phase
	}{
		{
			name:           "shared middleware response",
			path:           "/api/x",
			sharedDeny:     true,
			status:         http.StatusUnauthorized,
			wantReceived:   true,
			wantMiddleware: true,
			wantRoute:      "/api/",
		},
		{
			name:           "route middleware response",
			path:           "/api/x",
			routeDeny:      true,
			status:         http.StatusForbidden,
			wantMiddleware: true,
			wantReceived:   true,
			wantRoute:      "/api/",
		},
		{
			name:         "RouteMatched error",
			path:         "/api/x",
			hookErr:      errors.New("denied"),
			status:       http.StatusInternalServerError,
			wantReceived: true,
			wantPhase:    edgekit.PhaseRouteMatched,
			wantRoute:    "/api/",
		},
		{
			name:         "routing miss",
			path:         "/missing",
			status:       http.StatusNotFound,
			wantReceived: false,
			wantReason:   edgekit.ReasonNoRoute,
		},
		{
			// The core does not redirect /api to /api/ (ADR-0020).
			name:         "missing trailing slash",
			path:         "/api",
			status:       http.StatusNotFound,
			wantReceived: false,
			wantReason:   edgekit.ReasonNoRoute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				received, later, unwound bool
				completed                []edgekit.Completion
				stateAtCompletion        string
			)
			hooks := &testHooks{
				received: func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					received = true

					return nil, tc.hookErr
				},
				before: func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					later = true

					return nil, nil
				},
				response: func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					later = true

					return nil, nil
				},
				completed: func(_ context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
					if unwound != tc.wantMiddleware {
						t.Errorf("middleware unwound = %v, want %v", unwound, tc.wantMiddleware)
					}

					stateAtCompletion, _ = edgekit.State(rc, key)
					completed = append(completed, c)
				},
			}
			sharedMiddleware := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer func() {
						unwound = true
					}()

					rc, _ := edgekit.FromContext(r.Context())
					edgekit.SetState(rc, key, "shared")
					if tc.sharedDeny {
						http.Error(w, "unauthorized", http.StatusUnauthorized)

						return
					}

					next.ServeHTTP(w, r)
				})
			}
			var routeMiddleware []edgekit.Middleware
			if tc.routeDeny {
				routeMiddleware = append(routeMiddleware, func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						http.Error(w, "forbidden", http.StatusForbidden)
					})
				})
			}
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:     "/api/",
						Upstream:   static(t, upstream.URL),
						Middleware: routeMiddleware,
					},
				},
				SharedMiddleware: []edgekit.Middleware{sharedMiddleware},
				Hooks:            allPhases(hooks),
			})

			before := requests.Load()
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if w.Code != tc.status || received != tc.wantReceived || later {
				t.Errorf("status = %d, RouteMatched ran = %v, later hooks ran = %v; want %d, %v, false", w.Code, received, later, tc.status, tc.wantReceived)
			}

			if got := requests.Load() - before; got != 0 {
				t.Errorf("upstream got %d requests, want 0", got)
			}

			if len(completed) != 1 {
				t.Fatalf("completion ran %d times, want 1", len(completed))
			}

			c := completed[0]
			if c.RoutePrefix != tc.wantRoute || c.SelectedUpstream != nil || c.RejectionReason != tc.wantReason || c.Aborted {
				t.Errorf("completion = %+v, want route %q, no upstream, reason %q, not aborted", c, tc.wantRoute, tc.wantReason)
			}

			switch {
			case tc.hookErr == nil && c.Failure != nil:
				t.Errorf("completion failure = %+v, want none", c.Failure)
			case tc.hookErr != nil && (c.Failure == nil || c.Failure.Phase != tc.wantPhase || !errors.Is(c.Failure.Err, tc.hookErr)):
				t.Errorf("completion failure = %+v, want the hook error in %v", c.Failure, tc.wantPhase)
			}

			wantState := ""
			if tc.wantMiddleware {
				wantState = "shared"
			}
			if stateAtCompletion != wantState {
				t.Errorf("state at completion = %q, want %q", stateAtCompletion, wantState)
			}
		})
	}
}

func TestSecureDefaultRejectionReachesCompletion(t *testing.T) {
	var requests, entered atomic.Int32
	upstream := countingUpstream(t, &requests)
	hooks, completed := completions()
	var processing atomic.Int32
	processingHooks := &testHooks{
		received: func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
			processing.Add(1)

			return nil, nil
		},
		before: func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			processing.Add(1)

			return nil, nil
		},
		response: func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
			processing.Add(1)

			return nil, nil
		},
	}
	var startTimes, routes, upstreams, states []bool
	key := edgekit.NewStateKey[string]()
	inspect := edgekit.RequestCompletedHookFunc(func(_ context.Context, rc *edgekit.RequestContext, _ edgekit.Completion) {
		_, routed := rc.RoutePrefix()
		_, selected := rc.SelectedUpstream()
		_, stored := edgekit.State(rc, key)
		startTimes = append(startTimes, !rc.StartTime().IsZero())
		routes = append(routes, routed)
		upstreams = append(upstreams, selected)
		states = append(states, stored)
	})
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		SharedMiddleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered.Add(1)
				next.ServeHTTP(w, r)
			})
		}},
		Hooks: edgekit.Hooks{
			RouteMatched: []edgekit.RouteMatchedHook{
				processingHooks,
			},
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				processingHooks,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				processingHooks,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				inspect,
				hooks,
				processingHooks,
			},
		},
	})

	for _, tc := range []struct {
		name    string
		method  string
		target  string
		header  http.Header
		trailer http.Header
		reason  edgekit.RejectionReason
	}{
		{
			name:   "CONNECT",
			method: http.MethodConnect,
			target: "/api/x",
			reason: edgekit.ReasonInvalidMethod,
		},
		{
			name:   "absolute form",
			target: "http://upstream.test/api/x",
			reason: edgekit.ReasonInvalidRequestTarget,
		},
		{
			name:   "dot-segment",
			target: "/api/../admin",
			reason: edgekit.ReasonInvalidPath,
		},
		{
			name:   "encoded slash",
			target: "/api/a%2Fb",
			reason: edgekit.ReasonInvalidPath,
		},
		{
			name:   "query semicolon",
			target: "/api/x?a=1;b=2",
			reason: edgekit.ReasonInvalidQuery,
		},
		{
			name:   "header name alias",
			target: "/api/x",
			header: http.Header{
				"X_User": {"admin"},
			},
			reason: edgekit.ReasonInvalidHeaderName,
		},
		{
			name:   "trailer name alias",
			target: "/api/x",
			trailer: http.Header{
				"X_User": nil,
			},
			reason: edgekit.ReasonInvalidHeaderName,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodGet
			}

			r := httptest.NewRequest(method, tc.target, nil)
			maps.Copy(r.Header, tc.header)
			r.Trailer = tc.trailer
			startTimes, routes, upstreams, states = nil, nil, nil, nil

			g.ServeHTTP(httptest.NewRecorder(), r)

			c := receive(t, completed)
			if c.RejectionReason != tc.reason || c.RoutePrefix != "" || c.SelectedUpstream != nil || c.Failure != nil || c.Aborted {
				t.Errorf("completion = %+v, want only reason %q", c, tc.reason)
			}

			if c.Start.IsZero() || c.Duration < 0 {
				t.Errorf("completion start = %v, duration = %v; want a start time", c.Start, c.Duration)
			}

			if !slices.Equal(startTimes, []bool{true}) || slices.Contains(routes, true) || slices.Contains(upstreams, true) || slices.Contains(states, true) {
				t.Errorf("gateway context: start time %v, route %v, upstream %v, state %v; want only a start time", startTimes, routes, upstreams, states)
			}
		})
	}

	if entered.Load() != 0 || processing.Load() != 0 || requests.Load() != 0 {
		t.Errorf("middleware ran %d times, processing hooks %d times, upstream got %d requests; want 0", entered.Load(), processing.Load(), requests.Load())
	}
}

func TestHookErrorsStopPhaseAndHideInternalDetails(t *testing.T) {
	secret := errors.New("secret database details")

	for _, tc := range []struct {
		phase        edgekit.Phase
		wantRequests int32
		wantLater    []string
	}{
		{
			phase:        edgekit.PhaseRouteMatched,
			wantRequests: 0,
		},
		{
			phase:        edgekit.PhaseBeforeUpstream,
			wantRequests: 0,
			wantLater:    []string{"received"},
		},
		{
			phase:        edgekit.PhaseUpstreamResponse,
			wantRequests: 1,
			wantLater: []string{
				"received",
				"before",
			},
		},
	} {
		t.Run(tc.phase.String(), func(t *testing.T) {
			var requests atomic.Int32
			upstream := countingUpstream(t, &requests)

			var laterRan []string
			first := &testHooks{}
			switch tc.phase {
			case edgekit.PhaseRouteMatched:
				first.received = func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
					return nil, fmt.Errorf("lookup: %w", secret)
				}
			case edgekit.PhaseBeforeUpstream:
				first.before = func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					return nil, fmt.Errorf("sign: %w", secret)
				}
			default:
				first.response = func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
					return nil, fmt.Errorf("inspect: %w", secret)
				}
			}
			second := &testHooks{}
			second.received = func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
				laterRan = append(laterRan, "received")

				return nil, nil
			}
			second.before = func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
				laterRan = append(laterRan, "before")

				return nil, nil
			}
			second.response = func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
				laterRan = append(laterRan, "response")

				return nil, nil
			}
			hooks, completed := completions()

			phaseHooks := edgekit.Hooks{
				RouteMatched: []edgekit.RouteMatchedHook{
					first,
					second,
				},
				BeforeUpstream: []edgekit.BeforeUpstreamHook{
					first,
					second,
				},
				UpstreamResponse: []edgekit.UpstreamResponseHook{
					first,
					second,
				},
				RequestCompleted: []edgekit.RequestCompletedHook{
					hooks,
				},
			}

			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
					},
				},
				Hooks: phaseHooks,
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusInternalServerError || w.Body.String() != problem(http.StatusInternalServerError, "") {
				t.Errorf("response = %d %q, want 500 without details", w.Code, w.Body.String())
			}

			if w.Header().Get("X-Upstream") != "" || strings.Contains(w.Body.String(), "secret") {
				t.Errorf("response leaks upstream or error data: %v %q", w.Header(), w.Body.String())
			}

			if !slices.Equal(laterRan, tc.wantLater) {
				t.Errorf("hooks of the second provider ran = %v, want %v", laterRan, tc.wantLater)
			}

			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("upstream got %d requests, want %d", got, tc.wantRequests)
			}

			c := receive(t, completed)
			if c.Failure == nil || c.Failure.Phase != tc.phase || !errors.Is(c.Failure.Err, secret) || c.Aborted {
				t.Errorf("completion = %+v, failure %+v; want the hook error in %v", c, c.Failure, tc.phase)
			}
		})
	}
}

func TestTransportErrorReachesCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstream.Close()

	hooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				hooks,
			},
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if w.Code != http.StatusBadGateway || w.Body.String() != problem(http.StatusBadGateway, "") {
		t.Errorf("response = %d %q, want 502 without details", w.Code, w.Body.String())
	}

	c := receive(t, completed)
	if c.Failure == nil || c.Failure.Phase != edgekit.PhaseRoundTrip || c.Failure.Err == nil || c.Aborted {
		t.Errorf("completion = %+v, failure %+v; want a round-trip failure", c, c.Failure)
	}

	if c.SelectedUpstream == nil || c.SelectedUpstream.String() != upstream.URL {
		t.Errorf("completion upstream = %v, want %s", c.SelectedUpstream, upstream.URL)
	}
}

func TestCompletionDuringPanicUnwinding(t *testing.T) {
	for _, panicValue := range []any{
		"middleware panic",
		http.ErrAbortHandler,
	} {
		t.Run(fmt.Sprint(panicValue), func(t *testing.T) {
			var unwound, unwoundAtCompletion bool
			completedCount := 0
			var completion edgekit.Completion
			hooks := edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
				completedCount++
				unwoundAtCompletion = unwound
				completion = c
			})
			panicking := func(http.Handler) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					panic(panicValue)
				})
			}
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://unused.test"),
					},
				},
				SharedMiddleware: []edgekit.Middleware{unwinding(&unwound), panicking},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						hooks,
					},
				},
			})

			recovered := func() (recovered any) {
				defer func() {
					recovered = recover()
				}()

				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

				return nil
			}()

			if recovered != panicValue {
				t.Errorf("recovered %v, want the original panic %v", recovered, panicValue)
			}

			if completedCount != 1 || !unwoundAtCompletion {
				t.Errorf("completion ran %d times, after unwinding = %v; want 1, true", completedCount, unwoundAtCompletion)
			}

			if !completion.Aborted || completion.Failure != nil {
				t.Errorf("completion = %+v, want aborted without failure", completion)
			}
		})
	}
}

func TestStreamingFailureAbortsAndCompletes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer upstream.Close()

	hooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				hooks,
			},
		},
	})

	// ReverseProxy aborts with http.ErrAbortHandler for a failed body copy
	// only when an http.Server runs it.
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
	}
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, server))
	recovered := func() (recovered any) {
		defer func() {
			recovered = recover()
		}()

		g.ServeHTTP(httptest.NewRecorder(), r)

		return nil
	}()

	if err, ok := recovered.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
		t.Errorf("recovered %v, want http.ErrAbortHandler", recovered)
	}

	c := receive(t, completed)
	if !c.Aborted || c.Failure == nil || c.Failure.Phase != edgekit.PhaseResponseBody || !errors.Is(c.Failure.Err, io.ErrUnexpectedEOF) {
		t.Errorf("completion = %+v, want an abort with io.ErrUnexpectedEOF in PhaseResponseBody", c)
	}
}

// tagExtension uses middleware and hooks with shared request state, as an
// extension does.
type tagExtension struct {
	tag     string
	records chan tagRecord
}

// tagRecord is what the completion hook of tagExtension saw.
type tagRecord struct {
	tag        string
	completion edgekit.Completion
}

// tagKey is the private state key of tagExtension.
var tagKey = edgekit.NewStateKey[string]()

// Middleware stores the tag in the request state.
func (e *tagExtension) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, ok := edgekit.FromContext(r.Context()); ok {
			edgekit.SetState(rc, tagKey, e.tag)
		}

		next.ServeHTTP(w, r)
	})
}

// UpstreamResponse adds the tag from the request state to the response.
func (e *tagExtension) UpstreamResponse(_ context.Context, rc *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
	if tag, ok := edgekit.State(rc, tagKey); ok {
		resp.Header.Set("X-Request-Tag", tag)
	}

	return nil, nil
}

// RequestCompleted records the tag and the completion.
func (e *tagExtension) RequestCompleted(_ context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
	tag, _ := edgekit.State(rc, tagKey)
	e.records <- tagRecord{
		tag:        tag,
		completion: c,
	}
}

func TestExtensionSharesStateBetweenMiddlewareAndHooks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	tag := &tagExtension{
		tag:     "example",
		records: make(chan tagRecord, 1),
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		SharedMiddleware: []edgekit.Middleware{tag.Middleware},
		Hooks: edgekit.Hooks{
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				tag,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				tag,
			},
		},
	})

	for _, tc := range []struct {
		path       string
		status     int
		wantHeader string
		wantRoute  string
		wantReason edgekit.RejectionReason
	}{
		{
			path:       "/api/x",
			status:     http.StatusServiceUnavailable,
			wantHeader: "example",
			wantRoute:  "/api/",
		},
		{
			path:       "/missing",
			status:     http.StatusNotFound,
			wantReason: edgekit.ReasonNoRoute,
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if w.Code != tc.status || w.Header().Get("X-Request-Tag") != tc.wantHeader {
				t.Errorf("response = %d, X-Request-Tag %q; want %d, %q", w.Code, w.Header().Get("X-Request-Tag"), tc.status, tc.wantHeader)
			}

			record := receive(t, tag.records)
			if record.tag != tc.wantHeader {
				t.Errorf("completion tag = %q, want %q", record.tag, tc.wantHeader)
			}

			// An upstream 503 is an HTTP response, not a gateway failure.
			c := record.completion
			if c.Failure != nil || c.Aborted || c.RoutePrefix != tc.wantRoute || c.RejectionReason != tc.wantReason {
				t.Errorf("completion = %+v, want route %q, reason %q, no failure", c, tc.wantRoute, tc.wantReason)
			}
		})
	}
}

// panics reports whether f panics.
func panics(f func()) (panicked bool) {
	defer func() {
		panicked = recover() != nil
	}()

	f()

	return false
}

func TestTypedStateIsolationAndNilValues(t *testing.T) {
	first, second := edgekit.NewStateKey[string](), edgekit.NewStateKey[string]()
	anyKey := edgekit.NewStateKey[any]()
	pointerKey := edgekit.NewStateKey[*int]()

	type observation struct {
		first, second       string
		firstOK, secondOK   bool
		missingOK, anyOK    bool
		anyValue            any
		pointerOK           bool
		pointerValue        *int
		replaced            string
		setPanics, getPanic bool
	}
	observed := make(chan observation, 2)
	leaked := make(chan bool, 2)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://unused.test"),
			},
		},
		SharedMiddleware: []edgekit.Middleware{func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rc, _ := edgekit.FromContext(r.Context())
				_, ok := edgekit.State(rc, first)
				leaked <- ok

				if r.URL.Path != "/api/first" {
					return
				}

				var o observation
				_, o.missingOK = edgekit.State(rc, anyKey)
				edgekit.SetState(rc, first, "first")
				edgekit.SetState(rc, second, "second")
				o.first, o.firstOK = edgekit.State(rc, first)
				o.second, o.secondOK = edgekit.State(rc, second)
				edgekit.SetState(rc, anyKey, nil)
				o.anyValue, o.anyOK = edgekit.State(rc, anyKey)
				edgekit.SetState(rc, pointerKey, nil)
				o.pointerValue, o.pointerOK = edgekit.State(rc, pointerKey)
				edgekit.SetState(rc, first, "replaced")
				o.replaced, _ = edgekit.State(rc, first)
				o.setPanics = panics(func() {
					edgekit.SetState[string](rc, nil, "x")
				})
				o.getPanic = panics(func() {
					edgekit.State[string](rc, nil)
				})
				observed <- o
				w.WriteHeader(http.StatusNoContent)
			})
		}},
	})

	for _, path := range []string{"/api/first", "/api/second"} {
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	o := receive(t, observed)
	if !o.firstOK || o.first != "first" || !o.secondOK || o.second != "second" {
		t.Errorf("keys of the same type: %q %v, %q %v; want isolated values", o.first, o.firstOK, o.second, o.secondOK)
	}

	if o.missingOK {
		t.Error("State reported a value that was not stored")
	}

	if !o.anyOK || o.anyValue != nil || !o.pointerOK || o.pointerValue != nil {
		t.Errorf("stored nil: any %v %v, pointer %v %v; want nil values that are present", o.anyValue, o.anyOK, o.pointerValue, o.pointerOK)
	}

	if o.replaced != "replaced" {
		t.Errorf("replaced value = %q, want %q", o.replaced, "replaced")
	}

	if !o.setPanics || !o.getPanic {
		t.Errorf("nil key panics: SetState %v, State %v; want true", o.setPanics, o.getPanic)
	}

	firstLeaked, secondLeaked := receive(t, leaked), receive(t, leaked)
	if firstLeaked || secondLeaked {
		t.Error("state of one request is visible in another request")
	}

	if _, ok := edgekit.FromContext(context.Background()); ok {
		t.Error("FromContext found a gateway context outside the gateway")
	}
}

func TestStateIsDroppedAfterCompletion(t *testing.T) {
	key := edgekit.NewStateKey[string]()
	var kept *edgekit.RequestContext
	hooks := &testHooks{
		received: func(_ context.Context, rc *edgekit.RequestContext, _ *http.Request) (*edgekit.Rejection, error) {
			edgekit.SetState(rc, key, "value")

			return nil, nil
		},
		completed: func(_ context.Context, rc *edgekit.RequestContext, _ edgekit.Completion) {
			kept = rc
		},
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://unused.test"),
			},
		},
		Hooks: allPhases(hooks),
	})

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))

	if _, ok := edgekit.State(kept, key); ok {
		t.Error("state is available after completion")
	}

	edgekit.SetState(kept, key, "late")
	if _, ok := edgekit.State(kept, key); ok {
		t.Error("SetState stored a value after completion")
	}
}

func TestStateIsDroppedWhenACompletionHookPanics(t *testing.T) {
	key := edgekit.NewStateKey[string]()
	var kept *edgekit.RequestContext
	hooks := &testHooks{
		received: func(_ context.Context, rc *edgekit.RequestContext, _ *http.Request) (*edgekit.Rejection, error) {
			edgekit.SetState(rc, key, "value")

			return nil, nil
		},
		completed: func(_ context.Context, rc *edgekit.RequestContext, _ edgekit.Completion) {
			kept = rc
			panic("completion hook")
		},
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://unused.test"),
			},
		},
		Hooks: allPhases(hooks),
	})

	panicked := panics(func() {
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	})

	if !panicked {
		t.Fatal("the panic of the completion hook did not continue")
	}

	if _, ok := edgekit.State(kept, key); ok {
		t.Error("state is available after a completion hook panicked")
	}
}

func TestConcurrentRequestStateIsolation(t *testing.T) {
	const requests = 50

	key := edgekit.NewStateKey[string]()
	var (
		mu   sync.Mutex
		seen = make(map[string]int)
	)
	hooks := &testHooks{
		received: func(_ context.Context, rc *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
			edgekit.SetState(rc, key, r.Header.Get("X-Test-Request"))

			return &edgekit.Rejection{
				Status: http.StatusForbidden,
				Reason: "test.done",
			}, nil
		},
		completed: func(_ context.Context, rc *edgekit.RequestContext, _ edgekit.Completion) {
			value, _ := edgekit.State(rc, key)
			mu.Lock()
			defer mu.Unlock()
			seen[value]++
		},
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://unused.test"),
			},
		},
		Hooks: allPhases(hooks),
	})

	var wg sync.WaitGroup
	for i := range requests {
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodGet, "/api/state", nil)
			r.Header.Set("X-Test-Request", fmt.Sprint(i))
			g.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
	wg.Wait()

	if len(seen) != requests {
		t.Fatalf("got %d distinct state values, want %d", len(seen), requests)
	}

	for value, count := range seen {
		if count != 1 || value == "" {
			t.Errorf("state value %q seen %d times, want once", value, count)
		}
	}
}

func TestCancellationReachesHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var completionErr error
	hooks := &testHooks{
		received: func(ctx context.Context, _ *edgekit.RequestContext, _ *http.Request) (*edgekit.Rejection, error) {
			return nil, ctx.Err()
		},
		completed: func(ctx context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
			completionErr = ctx.Err()
			if c.Failure == nil || c.Failure.Phase != edgekit.PhaseRouteMatched || !errors.Is(c.Failure.Err, context.Canceled) {
				t.Errorf("completion failure = %+v, want the cancellation in RouteMatched", c.Failure)
			}
		},
	}
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://unused.test"),
			},
		},
		Hooks: allPhases(hooks),
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(ctx))

	if !errors.Is(completionErr, context.Canceled) {
		t.Errorf("completion context error = %v, want context.Canceled", completionErr)
	}

	// The hook reports the cancellation and the client is gone, so the
	// failure is not an extension defect (ADR-0023).
	if w.Code != 499 {
		t.Errorf("status = %d, want 499", w.Code)
	}
}

func TestCompletionOnOutboundPanic(t *testing.T) {
	var (
		unwound, responseRan bool
		roundTrips           atomic.Int32
	)
	hooks := &testHooks{
		before: func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			panic("signing panic")
		},
		response: func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
			responseRan = true

			return nil, nil
		},
	}
	completionHooks, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:     "/api/",
				Upstream:   static(t, "http://unused.test"),
				Middleware: []edgekit.Middleware{unwinding(&unwound)},
			},
		},
		Hooks: edgekit.Hooks{
			RouteMatched: []edgekit.RouteMatchedHook{
				hooks,
			},
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				hooks,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				hooks,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				completionHooks,
				hooks,
			},
		},
		Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			roundTrips.Add(1)

			return nil, errors.New("transport must not run")
		}),
	})

	recovered := func() (recovered any) {
		defer func() {
			recovered = recover()
		}()

		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

		return nil
	}()

	if recovered != "signing panic" {
		t.Errorf("recovered %v, want the original panic", recovered)
	}

	if !unwound || responseRan || roundTrips.Load() != 0 {
		t.Errorf("route middleware unwound = %v, response hook ran = %v, round trips = %d; want true, false, 0", unwound, responseRan, roundTrips.Load())
	}

	c := receive(t, completed)
	if !c.Aborted || c.Failure != nil || c.SelectedUpstream == nil {
		t.Errorf("completion = %+v, want aborted, no failure, and a selected upstream", c)
	}

	if len(completed) != 0 {
		t.Errorf("completion ran %d more times, want once", len(completed))
	}
}

func TestMissingGatewayContextFailsClosed(t *testing.T) {
	// The middleware breaks the contract of Middleware: it does not derive the
	// context from the request context.
	dropContext := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.Background()))
		})
	}

	for _, tc := range []struct {
		name      string
		shared    []edgekit.Middleware
		route     []edgekit.Middleware
		wantRoute string
	}{
		{
			name:      "shared middleware",
			shared:    []edgekit.Middleware{dropContext},
			wantRoute: "/api/",
		},
		{
			name:      "route middleware",
			route:     []edgekit.Middleware{dropContext},
			wantRoute: "/api/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := countingUpstream(t, &requests)
			hooks, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:     "/api/",
						Upstream:   static(t, upstream.URL),
						Middleware: tc.route,
					},
				},
				SharedMiddleware: tc.shared,
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						hooks,
					},
				},
			})

			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusInternalServerError || w.Body.String() != problem(http.StatusInternalServerError, "") || requests.Load() != 0 {
				t.Errorf("response = %d %q, upstream requests = %d; want 500 without details and no request", w.Code, w.Body.String(), requests.Load())
			}

			c := receive(t, completed)
			if c.RoutePrefix != tc.wantRoute || c.SelectedUpstream != nil {
				t.Errorf("completion = %+v, want route %q and no upstream", c, tc.wantRoute)
			}
		})
	}
}

func TestClientCancellationReachesUpstreamAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		streaming bool
	}{
		{
			name: "waiting for headers",
		},
		{
			name:      "streaming response",
			streaming: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: first\n\n")
					_ = http.NewResponseController(w).Flush()
				}

				close(entered)
				<-r.Context().Done()
				close(canceled)
			}))
			defer upstream.Close()

			var (
				unwound     atomic.Bool
				completions atomic.Int32
			)
			type observed struct {
				completion edgekit.Completion
				unwound    bool
				ctxErr     error
			}
			results := make(chan observed, 2)
			hooks := edgekit.RequestCompletedHookFunc(func(ctx context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
				completions.Add(1)
				results <- observed{
					completion: c,
					unwound:    unwound.Load(),
					ctxErr:     ctx.Err(),
				}
			})
			server := httptest.NewServer(newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, upstream.URL),
					},
				},
				Transport: testTransport(t),
				SharedMiddleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						defer unwound.Store(true)

						next.ServeHTTP(w, r)
					})
				}},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						hooks,
					},
				},
			}))
			defer server.Close()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			r, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/slow", nil)
			if err != nil {
				t.Fatal(err)
			}

			firstRead := make(chan error, 1)
			clientDone := make(chan error, 1)
			go func() {
				resp, err := server.Client().Do(r)
				if err != nil {
					firstRead <- err
					clientDone <- err

					return
				}
				defer func() { _ = resp.Body.Close() }()

				if tc.streaming {
					_, err = io.ReadFull(resp.Body, make([]byte, len("data: first\n\n")))
					firstRead <- err
				}

				_, err = io.ReadAll(resp.Body)
				clientDone <- err
			}()

			receive(t, entered)
			if tc.streaming {
				if err := receive(t, firstRead); err != nil {
					t.Fatalf("first streamed event: %v", err)
				}
			}

			cancel()

			if err := receive(t, clientDone); !errors.Is(err, context.Canceled) {
				t.Errorf("client error = %v, want context.Canceled", err)
			}

			receive(t, canceled)
			got := receive(t, results)

			if completions.Load() != 1 || !got.unwound || got.ctxErr == nil {
				t.Errorf("completions = %d, after unwinding = %v, entry context error = %v; want 1, true, canceled", completions.Load(), got.unwound, got.ctxErr)
			}

			c := got.completion
			if !tc.streaming && (c.Failure == nil || c.Failure.Phase != edgekit.PhaseRoundTrip || !errors.Is(c.Failure.Err, context.Canceled) || c.Aborted) {
				t.Errorf("completion = %+v, failure %+v; want a canceled round trip", c, c.Failure)
			}

			if tc.streaming && (!c.Aborted || c.Failure == nil || c.Failure.Phase != edgekit.PhaseResponseBody || !errors.Is(c.Failure.Err, context.Canceled)) {
				t.Errorf("completion = %+v, want an abort with context.Canceled in PhaseResponseBody", c)
			}
		})
	}
}

func TestResponseHeaderTimeoutReachesCompletion(t *testing.T) {
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()

	transport := testTransport(t)
	transport.ResponseHeaderTimeout = 100 * time.Millisecond
	var unwound, unwoundAtCompletion bool
	var completion edgekit.Completion
	hooks := edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
		unwoundAtCompletion = unwound
		completion = c
	})
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, upstream.URL),
			},
		},
		Transport:        transport,
		SharedMiddleware: []edgekit.Middleware{unwinding(&unwound)},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{
				hooks,
			},
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/slow", nil))

	if w.Code != http.StatusGatewayTimeout || w.Body.String() != problem(http.StatusGatewayTimeout, "") {
		t.Errorf("response = %d %q, want 504 without details", w.Code, w.Body.String())
	}

	var timeout net.Error
	failure := completion.Failure
	if failure == nil || failure.Phase != edgekit.PhaseRoundTrip || !errors.As(failure.Err, &timeout) || !timeout.Timeout() || completion.Aborted {
		t.Errorf("completion = %+v, failure %+v; want a round-trip timeout", completion, failure)
	}

	if !unwoundAtCompletion {
		t.Error("completion ran before middleware unwound")
	}

	receive(t, canceled)
}
