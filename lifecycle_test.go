package edgekit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/requesttag"
)

type testExtension struct {
	request  func(context.Context, *edgekit.RequestContext, *http.Request) error
	outbound func(context.Context, *edgekit.RequestContext, *http.Request) error
	response func(context.Context, *edgekit.RequestContext, *http.Response) error
	complete func(context.Context, *edgekit.RequestContext, edgekit.Outcome)
}

func (e *testExtension) OnRequest(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) error {
	if e.request != nil {
		return e.request(ctx, rc, r)
	}
	return nil
}

func (e *testExtension) OnBeforeUpstream(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) error {
	if e.outbound != nil {
		return e.outbound(ctx, rc, r)
	}
	return nil
}

func (e *testExtension) OnUpstreamResponse(ctx context.Context, rc *edgekit.RequestContext, r *http.Response) error {
	if e.response != nil {
		return e.response(ctx, rc, r)
	}
	return nil
}

func (e *testExtension) OnComplete(ctx context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
	if e.complete != nil {
		e.complete(ctx, rc, outcome)
	}
}

func testRoute(t *testing.T, address string) edgekit.Route {
	t.Helper()
	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return edgekit.Route{Prefix: "/api/", Upstream: target}
}

func TestLifecycleContextAndOrder(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "original")
		_, _ = io.WriteString(w, "body")
	}))
	defer upstream.Close()

	type contextKey struct{}
	key := edgekit.NewStateKey[string]()
	var shared *edgekit.RequestContext
	var steps []string
	check := func(ctx context.Context, rc *edgekit.RequestContext) {
		t.Helper()
		fromContext, ok := edgekit.FromContext(ctx)
		if !ok || fromContext != shared || rc != shared {
			t.Fatal("gateway context identity changed")
		}
	}
	first := &testExtension{
		request: func(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) error {
			check(ctx, rc)
			if ctx.Value(contextKey{}) != "derived" || r.Context() != ctx {
				t.Fatal("request hook did not receive the middleware context")
			}
			if _, ok := rc.RoutePrefix(); ok {
				t.Fatal("route matched before request hook")
			}
			if value, ok := edgekit.GetState(rc, key); !ok || value != "entered" {
				t.Fatal("middleware state missing")
			}
			steps = append(steps, "request:first")
			return nil
		},
		outbound: func(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) error {
			check(ctx, rc)
			if ctx.Value(contextKey{}) != "derived" || r.Context() != ctx {
				t.Fatal("outbound hook lost the execution context")
			}
			if value, _ := edgekit.GetState(rc, key); value != "route entered" {
				t.Fatal("outbound hook ran before route middleware established state")
			}
			if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
				t.Fatal("outbound hook has no matched route")
			}
			if target, ok := rc.Upstream(); !ok || target.String() != upstream.URL || r.URL.Host != target.Host {
				t.Fatal("outbound hook did not receive the rewritten request")
			}
			r.Header.Set("X-Outbound", "first")
			steps = append(steps, "outbound:first")
			return nil
		},
		response: func(ctx context.Context, rc *edgekit.RequestContext, response *http.Response) error {
			check(ctx, rc)
			if ctx.Value(contextKey{}) != "derived" {
				t.Fatal("outbound context lost middleware values")
			}
			if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
				t.Fatal("response hook has no route")
			}
			target, ok := rc.Upstream()
			if !ok || target.String() != upstream.URL {
				t.Fatal("response hook has no upstream")
			}
			target.Host = "changed.example"
			if copy, _ := rc.Upstream(); copy.Host == target.Host {
				t.Fatal("upstream metadata is mutable")
			}
			response.Header.Set("X-Upstream", "modified")
			steps = append(steps, "response:first")
			return nil
		},
		complete: func(ctx context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
			check(ctx, rc)
			if ctx.Value(contextKey{}) != nil {
				t.Fatal("completion should receive gateway entry context")
			}
			if value, _ := edgekit.GetState(rc, key); value != "unwound" {
				t.Fatal("completion ran before middleware unwound")
			}
			if outcome.Err != nil || outcome.Aborted || outcome.Duration <= 0 {
				t.Fatalf("unexpected outcome: %+v", outcome)
			}
			steps = append(steps, "complete:first")
		},
	}
	second := &testExtension{
		request: func(context.Context, *edgekit.RequestContext, *http.Request) error {
			steps = append(steps, "request:second")
			return nil
		},
		outbound: func(_ context.Context, _ *edgekit.RequestContext, r *http.Request) error {
			if r.Header.Get("X-Outbound") != "first" {
				t.Fatal("outbound hooks ran out of order")
			}
			steps = append(steps, "outbound:second")
			return nil
		},
		response: func(_ context.Context, _ *edgekit.RequestContext, response *http.Response) error {
			if response.Header.Get("X-Upstream") != "modified" {
				t.Fatal("response hooks ran out of order")
			}
			steps = append(steps, "response:second")
			return nil
		},
		complete: func(context.Context, *edgekit.RequestContext, edgekit.Outcome) {
			steps = append(steps, "complete:second")
		},
	}
	global := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ok bool
			shared, ok = edgekit.FromContext(r.Context())
			if !ok || shared.StartedAt().IsZero() || shared.StartedAt().After(time.Now()) {
				t.Fatal("context not initialized before global middleware")
			}
			edgekit.SetState(shared, key, "entered")
			steps = append(steps, "global:before")
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, "derived")))
			edgekit.SetState(shared, key, "unwound")
			steps = append(steps, "global:after")
		})
	}
	route := testRoute(t, upstream.URL)
	route.Middlewares = []edgekit.Middleware{func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc, _ := edgekit.FromContext(r.Context())
			if prefix, ok := rc.RoutePrefix(); !ok || prefix != "/api/" {
				t.Fatal("route metadata missing before route middleware")
			}
			if _, ok := rc.Upstream(); ok {
				t.Fatal("proxy began before route middleware")
			}
			edgekit.SetState(rc, key, "route entered")
			steps = append(steps, "route:before")
			next.ServeHTTP(w, r)
			steps = append(steps, "route:after")
		})
	}}
	app, err := edgekit.NewWithConfig(edgekit.Config{
		Routes: []edgekit.Route{route}, Middlewares: []edgekit.Middleware{global}, Extensions: []any{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/example", nil))
	if w.Code != 200 || w.Header().Get("X-Upstream") != "modified" || w.Body.String() != "body" {
		t.Fatalf("unexpected response: %d %v %q", w.Code, w.Header(), w.Body.String())
	}
	want := []string{"global:before", "request:first", "request:second", "route:before", "outbound:first", "outbound:second", "response:first", "response:second", "route:after", "global:after", "complete:first", "complete:second"}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("order = %v, want %v", steps, want)
	}
}

func TestCompletionOnEarlyExits(t *testing.T) {
	// Use the current standard library's trailing-slash redirect status.
	mux := http.NewServeMux()
	mux.Handle("/api/", http.NotFoundHandler())
	redirect := httptest.NewRecorder()
	mux.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/api", nil))
	for _, tc := range []struct {
		name        string
		path        string
		globalDeny  bool
		routeDeny   bool
		hookDeny    bool
		wantStatus  int
		wantRequest bool
		wantRoute   bool
	}{
		{name: "global rejection", path: "/api/x", globalDeny: true, wantStatus: 401},
		{name: "route rejection", path: "/api/x", routeDeny: true, wantStatus: 403, wantRequest: true, wantRoute: true},
		{name: "hook rejection", path: "/api/x", hookDeny: true, wantStatus: 429, wantRequest: true},
		{name: "routing miss", path: "/missing", wantStatus: 404, wantRequest: true},
		{name: "routing redirect", path: "/api", wantStatus: redirect.Code, wantRequest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requestRan, unwound bool
			completions := 0
			denial := &edgekit.HTTPError{StatusCode: 429, Message: "slow down"}
			extension := &testExtension{
				request: func(context.Context, *edgekit.RequestContext, *http.Request) error {
					requestRan = true
					if tc.hookDeny {
						return fmt.Errorf("policy: %w", denial)
					}
					return nil
				},
				outbound: func(context.Context, *edgekit.RequestContext, *http.Request) error {
					t.Fatal("outbound hook ran for a local response")
					return nil
				},
				response: func(context.Context, *edgekit.RequestContext, *http.Response) error {
					t.Fatal("response hook ran for a local response")
					return nil
				},
				complete: func(_ context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
					completions++
					if !unwound || outcome.Aborted {
						t.Fatal("completion did not follow normal middleware exit")
					}
					if _, ok := rc.RoutePrefix(); ok != tc.wantRoute {
						t.Fatal("incorrect route metadata")
					}
					if _, ok := rc.Upstream(); ok {
						t.Fatal("upstream selected on early exit")
					}
					if errors.Is(outcome.Err, denial) != tc.hookDeny {
						t.Fatalf("incorrect outcome error: %v", outcome.Err)
					}
				},
			}
			global := func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer func() { unwound = true }()
					if tc.globalDeny {
						http.Error(w, "unauthorized", 401)
						return
					}
					next.ServeHTTP(w, r)
				})
			}
			route := testRoute(t, "http://unused.invalid")
			if tc.routeDeny {
				route.Middlewares = []edgekit.Middleware{func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", 403) })
				}}
			}
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes: []edgekit.Route{route}, Middlewares: []edgekit.Middleware{global}, Extensions: []any{extension},
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.wantStatus || requestRan != tc.wantRequest || completions != 1 {
				t.Fatalf("status=%d request=%v completions=%d", w.Code, requestRan, completions)
			}
		})
	}
}

func TestHookErrorsStopPhaseAndHideInternalDetails(t *testing.T) {
	for _, phase := range []string{"request", "outbound", "response"} {
		for _, tc := range []struct {
			name    string
			err     error
			status  int
			message string
		}{
			{name: "internal", err: errors.New("secret database details")},
			{name: "rejection", err: &edgekit.HTTPError{StatusCode: 403, Message: "denied"}, status: 403, message: "denied\n"},
			{name: "invalid rejection", err: &edgekit.HTTPError{StatusCode: 200, Message: "secret details"}, status: 500, message: "Internal Server Error\n"},
		} {
			t.Run(phase+"/"+tc.name, func(t *testing.T) {
				var received atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received.Add(1)
					w.Header().Set("X-Upstream", "must-not-escape")
					_, _ = io.WriteString(w, "upstream body")
				}))
				defer upstream.Close()
				first, second := &testExtension{}, &testExtension{}
				if phase == "request" {
					first.request = func(context.Context, *edgekit.RequestContext, *http.Request) error { return tc.err }
					second.request = func(context.Context, *edgekit.RequestContext, *http.Request) error {
						t.Fatal("later request hook ran after error")
						return nil
					}
				} else if phase == "outbound" {
					first.outbound = func(context.Context, *edgekit.RequestContext, *http.Request) error { return tc.err }
					second.outbound = func(context.Context, *edgekit.RequestContext, *http.Request) error {
						t.Fatal("later outbound hook ran after error")
						return nil
					}
					second.response = func(context.Context, *edgekit.RequestContext, *http.Response) error {
						t.Fatal("response hook ran after outbound failure")
						return nil
					}
				} else {
					first.response = func(context.Context, *edgekit.RequestContext, *http.Response) error { return tc.err }
					second.response = func(context.Context, *edgekit.RequestContext, *http.Response) error {
						t.Fatal("later response hook ran after error")
						return nil
					}
				}
				completed := false
				second.complete = func(_ context.Context, _ *edgekit.RequestContext, outcome edgekit.Outcome) {
					completed = true
					if !errors.Is(outcome.Err, tc.err) || outcome.Aborted {
						t.Fatalf("unexpected error outcome: %+v", outcome)
					}
				}
				app, err := edgekit.NewWithConfig(edgekit.Config{
					Routes: []edgekit.Route{testRoute(t, upstream.URL)}, Extensions: []any{first, second},
				})
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))
				status, message := tc.status, tc.message
				if status == 0 {
					status = 500
					if phase != "request" {
						status = 502
					}
					message = http.StatusText(status) + "\n"
				}
				if !completed || w.Code != status || w.Body.String() != message || w.Header().Get("X-Upstream") != "" {
					t.Fatalf("unexpected response: %d %v %q, completed=%v", w.Code, w.Header(), w.Body.String(), completed)
				}
				wantReceived := int32(0)
				if phase == "response" {
					wantReceived = 1
				}
				if got := received.Load(); got != wantReceived {
					t.Fatalf("upstream received %d requests, want %d", got, wantReceived)
				}
			})
		}
	}
}

func TestTransportErrorReachesCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	upstream.Close()
	completed := false
	extension := &testExtension{complete: func(_ context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
		completed = true
		if outcome.Err == nil || outcome.Aborted {
			t.Fatalf("unexpected transport outcome: %+v", outcome)
		}
		if _, ok := rc.Upstream(); !ok {
			t.Fatal("missing attempted upstream")
		}
	}}
	app, err := edgekit.NewWithConfig(edgekit.Config{Routes: []edgekit.Route{testRoute(t, upstream.URL)}, Extensions: []any{extension}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))
	if !completed || w.Code != 502 || w.Body.String() != "Bad Gateway\n" {
		t.Fatalf("unexpected transport response: %d %q", w.Code, w.Body.String())
	}
}

func TestCompletionDuringPanicUnwinding(t *testing.T) {
	for _, panicValue := range []any{"middleware panic", http.ErrAbortHandler} {
		t.Run(fmt.Sprint(panicValue), func(t *testing.T) {
			var unwound, completed bool
			extension := &testExtension{complete: func(_ context.Context, _ *edgekit.RequestContext, outcome edgekit.Outcome) {
				completed = true
				if !unwound || !outcome.Aborted {
					t.Fatal("completion did not observe panic unwinding")
				}
			}}
			middleware := func(http.Handler) http.Handler {
				return http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					defer func() { unwound = true }()
					panic(panicValue)
				})
			}
			app, err := edgekit.NewWithConfig(edgekit.Config{
				Routes:      []edgekit.Route{testRoute(t, "http://unused.invalid")},
				Middlewares: []edgekit.Middleware{middleware}, Extensions: []any{extension},
			})
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if recovered := recover(); recovered != panicValue {
						t.Fatalf("panic changed or was swallowed: %v", recovered)
					}
				}()
				app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
			}()
			if !completed {
				t.Fatal("completion did not run")
			}
		})
	}
}

func TestStreamingFailureAbortsAndCompletes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer upstream.Close()
	completed := false
	extension := &testExtension{complete: func(_ context.Context, _ *edgekit.RequestContext, outcome edgekit.Outcome) {
		completed = true
		if !outcome.Aborted {
			t.Fatal("streaming failure was reported as normal completion")
		}
	}}
	app, err := edgekit.NewWithConfig(edgekit.Config{Routes: []edgekit.Route{testRoute(t, upstream.URL)}, Extensions: []any{extension}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	// ReverseProxy aborts body-copy failures when running under an HTTP server.
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{}))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("expected streaming abort, got %v", recovered)
			}
		}()
		app.ServeHTTP(httptest.NewRecorder(), r)
	}()
	if !completed {
		t.Fatal("completion did not run after streaming failure")
	}
}

func TestRequestTagExtension(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	var records []requesttag.Record
	tag := requesttag.New("example", func(record requesttag.Record) { records = append(records, record) })
	app, err := edgekit.NewWithConfig(edgekit.Config{
		Routes: []edgekit.Route{testRoute(t, upstream.URL)}, Middlewares: []edgekit.Middleware{tag.Middleware}, Extensions: []any{tag},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"/api/x", "/missing"} {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if len(records) != i+1 || records[i].Tag != "example" || records[i].Proxied != (i == 0) {
			t.Fatalf("unexpected records: %+v", records)
		}
		if records[i].Outcome.Err != nil || records[i].Outcome.Aborted {
			t.Fatal("HTTP error status is not a gateway execution error")
		}
		if i == 0 && (w.Code != 503 || w.Header().Get("X-Request-Tag") != "example" || records[i].RoutePrefix != "/api/") {
			t.Fatalf("example response missing lifecycle header: %d %v", w.Code, w.Header())
		}
		if i == 1 && (w.Code != 404 || w.Header().Get("X-Request-Tag") != "") {
			t.Fatal("response hook ran for a local response")
		}
	}
}

func TestTypedStateIsolationAndNilValues(t *testing.T) {
	var rc edgekit.RequestContext
	first, second := edgekit.NewStateKey[string](), edgekit.NewStateKey[string]()
	if first == second {
		t.Fatal("distinct state keys have equal identities")
	}
	edgekit.SetState(&rc, first, "first")
	edgekit.SetState(&rc, second, "second")
	if value, ok := edgekit.GetState(&rc, first); !ok || value != "first" {
		t.Fatal("first key overwritten")
	}
	if value, ok := edgekit.GetState(&rc, second); !ok || value != "second" {
		t.Fatal("second key missing")
	}
	nilKey := edgekit.NewStateKey[any]()
	if _, ok := edgekit.GetState(&rc, nilKey); ok {
		t.Fatal("missing key reported present")
	}
	edgekit.SetState(&rc, nilKey, nil)
	if value, ok := edgekit.GetState(&rc, nilKey); !ok || value != nil {
		t.Fatal("stored nil was not preserved")
	}
	other := new(edgekit.RequestContext)
	if _, ok := edgekit.GetState(other, first); ok {
		t.Fatal("state leaked across request contexts")
	}
	if _, ok := edgekit.FromContext(context.Background()); ok {
		t.Fatal("unexpected context outside gateway")
	}
}

func TestConcurrentRequestStateIsolation(t *testing.T) {
	key := edgekit.NewStateKey[string]()
	var mu sync.Mutex
	seen := make(map[string]bool)
	extension := &testExtension{
		request: func(_ context.Context, rc *edgekit.RequestContext, r *http.Request) error {
			edgekit.SetState(rc, key, r.Header.Get("X-Test-Request"))
			return &edgekit.HTTPError{StatusCode: 403, Message: "local response"}
		},
		complete: func(_ context.Context, rc *edgekit.RequestContext, _ edgekit.Outcome) {
			value, ok := edgekit.GetState(rc, key)
			mu.Lock()
			defer mu.Unlock()
			if !ok || seen[value] {
				t.Error("missing or duplicated request state")
			}
			seen[value] = true
		},
	}
	app, err := edgekit.NewWithConfig(edgekit.Config{Routes: []edgekit.Route{testRoute(t, "http://unused.invalid")}, Extensions: []any{extension}})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
			r.Header.Set("X-Test-Request", fmt.Sprint(i))
			app.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
	if len(seen) != 20 {
		t.Fatalf("got %d distinct request values", len(seen))
	}
}

func TestCancellationReachesHooks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	completed := false
	extension := &testExtension{
		request: func(ctx context.Context, _ *edgekit.RequestContext, _ *http.Request) error {
			return ctx.Err()
		},
		complete: func(ctx context.Context, _ *edgekit.RequestContext, outcome edgekit.Outcome) {
			completed = true
			if ctx.Err() != context.Canceled || !errors.Is(outcome.Err, context.Canceled) {
				t.Fatal("cancellation was lost")
			}
		},
	}
	app, err := edgekit.NewWithConfig(edgekit.Config{Routes: []edgekit.Route{testRoute(t, "http://unused.invalid")}, Extensions: []any{extension}})
	if err != nil {
		t.Fatal(err)
	}
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(ctx))
	if !completed {
		t.Fatal("completion did not run for canceled context")
	}
}

func TestCompletionOnOutboundPanic(t *testing.T) {
	unwound := false
	completions := 0
	extension := &testExtension{
		outbound: func(context.Context, *edgekit.RequestContext, *http.Request) error {
			panic("signing panic")
		},
		response: func(context.Context, *edgekit.RequestContext, *http.Response) error {
			t.Fatal("response hook ran after outbound panic")
			return nil
		},
		complete: func(_ context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
			completions++
			if !unwound || !outcome.Aborted || outcome.Err != nil {
				t.Fatalf("unexpected panic outcome: unwound=%v outcome=%+v", unwound, outcome)
			}
			if _, ok := rc.Upstream(); !ok {
				t.Fatal("upstream was not selected before outbound panic")
			}
		},
	}
	route := testRoute(t, "http://unused.invalid")
	route.Middlewares = []edgekit.Middleware{func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { unwound = true }()
			next.ServeHTTP(w, r)
		})
	}}
	app, err := edgekit.NewWithConfig(edgekit.Config{Routes: []edgekit.Route{route}, Extensions: []any{extension}})
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != "signing panic" {
				t.Fatalf("panic changed or was swallowed: %v", recovered)
			}
		}()
		app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))
	}()
	if completions != 1 {
		t.Fatalf("completion calls=%d, want 1", completions)
	}
}

func TestInvalidExtensions(t *testing.T) {
	var typedNil *testExtension
	for _, extension := range []any{nil, typedNil, struct{}{}} {
		_, err := edgekit.NewWithConfig(edgekit.Config{
			Routes: []edgekit.Route{testRoute(t, "http://unused.invalid")}, Extensions: []any{extension},
		})
		if err == nil || !strings.Contains(err.Error(), "extension 0") {
			t.Fatalf("invalid extension accepted: %T, error: %v", extension, err)
		}
	}
}
