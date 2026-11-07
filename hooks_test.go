package edgekit_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/go-edgekit/edgekit"
)

// testHooks provides all phases for tests that need a complete lifecycle.
type testHooks struct {
	received  func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error)
	before    func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error)
	response  func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error)
	completed func(context.Context, *edgekit.RequestContext, edgekit.Completion)
}

// RequestReceived calls h.received when it is set.
func (h *testHooks) RequestReceived(ctx context.Context, rc *edgekit.RequestContext, r *http.Request) (*edgekit.Rejection, error) {
	if h.received == nil {
		return nil, nil
	}

	return h.received(ctx, rc, r)
}

// BeforeUpstream calls h.before when it is set.
func (h *testHooks) BeforeUpstream(ctx context.Context, rc *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
	if h.before == nil {
		return nil, nil
	}

	return h.before(ctx, rc, out)
}

// UpstreamResponse calls h.response when it is set.
func (h *testHooks) UpstreamResponse(ctx context.Context, rc *edgekit.RequestContext, resp *http.Response) (*edgekit.Rejection, error) {
	if h.response == nil {
		return nil, nil
	}

	return h.response(ctx, rc, resp)
}

// RequestCompleted calls h.completed when it is set.
func (h *testHooks) RequestCompleted(ctx context.Context, rc *edgekit.RequestContext, c edgekit.Completion) {
	if h.completed != nil {
		h.completed(ctx, rc, c)
	}
}

// allPhaseHook implements the hook interfaces of all four phases.
type allPhaseHook interface {
	edgekit.RequestReceivedHook
	edgekit.BeforeUpstreamHook
	edgekit.UpstreamResponseHook
	edgekit.RequestCompletedHook
}

// allPhases registers h once in each phase, for tests where the position does
// not matter.
func allPhases(h allPhaseHook) edgekit.Hooks {
	return edgekit.Hooks{
		RequestReceived: []edgekit.RequestReceivedHook{
			h,
		},
		BeforeUpstream: []edgekit.BeforeUpstreamHook{
			h,
		},
		UpstreamResponse: []edgekit.UpstreamResponseHook{
			h,
		},
		RequestCompleted: []edgekit.RequestCompletedHook{
			h,
		},
	}
}

// completions records completion without registering processing hooks.
func completions() (edgekit.RequestCompletedHookFunc, <-chan edgekit.Completion) {
	ch := make(chan edgekit.Completion, 16)
	hook := edgekit.RequestCompletedHookFunc(func(_ context.Context, _ *edgekit.RequestContext, c edgekit.Completion) {
		ch <- c
	})

	return hook, ch
}

// recordingHooks records only the phase and the name of each call.
func recordingHooks(steps *[]string, name string) *testHooks {
	return &testHooks{
		received: func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
			*steps = append(*steps, "received:"+name)

			return nil, nil
		},
		before: func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
			*steps = append(*steps, "before:"+name)

			return nil, nil
		},
		response: func(context.Context, *edgekit.RequestContext, *http.Response) (*edgekit.Rejection, error) {
			*steps = append(*steps, "response:"+name)

			return nil, nil
		},
		completed: func(context.Context, *edgekit.RequestContext, edgekit.Completion) {
			*steps = append(*steps, "completed:"+name)
		},
	}
}

// hookTransport returns an empty successful response without network I/O.
func hookTransport() http.RoundTripper {
	return roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     make(http.Header),
			Body:       http.NoBody,
		}, nil
	})
}

// declaringHook is a hook that also implements HeaderOwner and counts its
// ManagedHeaders calls.
type declaringHook struct {
	*testHooks
	names []string
	calls int
}

// ManagedHeaders returns the declaration without a copy.
func (h *declaringHook) ManagedHeaders() []string {
	h.calls++

	return h.names
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
			provider: edgekit.RequestReceivedHookFunc(nil),
			phase:    "RequestReceived",
		},
		{
			name:     "before upstream",
			provider: edgekit.BeforeUpstreamHookFunc(nil),
			phase:    "BeforeUpstream",
		},
		{
			name:     "upstream response",
			provider: edgekit.UpstreamResponseHookFunc(nil),
			phase:    "UpstreamResponse",
		},
		{
			name:     "request completed",
			provider: edgekit.RequestCompletedHookFunc(nil),
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
				phaseHooks := edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{
						collector,
					},
				}
				if withBefore {
					phaseHooks.BeforeUpstream = append(phaseHooks.BeforeUpstream, edgekit.BeforeUpstreamHookFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
						return nil, nil
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
					Routes: []edgekit.Route{
						{
							Prefix:   "/api/",
							Upstream: static(t, "http://upstream.test"),
							Middleware: []edgekit.Middleware{func(next http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									r = r.Clone(r.Context())
									tc.change(r)
									next.ServeHTTP(w, r)
								})
							}},
						},
					},
					Hooks:     phaseHooks,
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

				if w.Code != http.StatusInternalServerError || w.Body.String() != problem(http.StatusInternalServerError, "") || calls != 0 {
					t.Errorf("response = %d %q, transport calls = %d; want 500 without details and no call", w.Code, w.Body.String(), calls)
				}

				if c.Failure == nil || c.Failure.Phase != edgekit.PhaseBeforeUpstream {
					t.Errorf("failure = %+v, want a failure in BeforeUpstream", c.Failure)
				}
			})
		}
	}
}

func TestHooksRunInWrittenOrderPerPhase(t *testing.T) {
	var steps []string
	a, b := recordingHooks(&steps, "a"), recordingHooks(&steps, "b")
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: hookTransport(),
		Hooks: edgekit.Hooks{
			RequestReceived: []edgekit.RequestReceivedHook{
				a,
				b,
				b,
			},
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				b,
				a,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				a,
				b,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				b,
				a,
				a,
			},
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}

	want := []string{
		"received:a",
		"received:b",
		"received:b",
		"before:b",
		"before:a",
		"response:a",
		"response:b",
		"completed:b",
		"completed:a",
		"completed:a",
	}
	if !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
}

func TestHooksRegisterOnlyListedPhases(t *testing.T) {
	var steps []string
	hook := recordingHooks(&steps, "registered")
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: hookTransport(),
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				hook,
			},
		},
	})

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if !slices.Equal(steps, []string{"before:registered"}) {
		t.Errorf("steps = %v, want only the registered phase", steps)
	}
}

func TestHookListsAreCopied(t *testing.T) {
	var steps []string
	hook, replacement := recordingHooks(&steps, "original"), recordingHooks(&steps, "replacement")
	cfg := edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: hookTransport(),
		Hooks: edgekit.Hooks{
			RequestReceived: []edgekit.RequestReceivedHook{
				hook,
			},
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				hook,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				hook,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				hook,
			},
		},
	}
	g := newGateway(t, cfg)
	cfg.Hooks.RequestReceived[0] = replacement
	cfg.Hooks.BeforeUpstream[0] = replacement
	cfg.Hooks.UpstreamResponse[0] = replacement
	cfg.Hooks.RequestCompleted[0] = replacement

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	want := []string{
		"received:original",
		"before:original",
		"response:original",
		"completed:original",
	}
	if !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v after configuration changes", steps, want)
	}
}

func TestHookValuesAreShared(t *testing.T) {
	var steps []string
	hook := recordingHooks(&steps, "original")
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Transport: hookTransport(),
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				hook,
			},
		},
	})
	hook.before = func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
		steps = append(steps, "before:updated")

		return nil, nil
	}

	g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	if !slices.Equal(steps, []string{"before:updated"}) {
		t.Errorf("steps = %v, want the shared hook value", steps)
	}
}

func TestInvalidHooks(t *testing.T) {
	var typedNil *testHooks
	for _, tc := range []struct {
		name      string
		received  edgekit.RequestReceivedHook
		before    edgekit.BeforeUpstreamHook
		response  edgekit.UpstreamResponseHook
		completed edgekit.RequestCompletedHook
	}{
		{
			name: "nil entries",
		},
		{
			name:      "typed nil receivers",
			received:  typedNil,
			before:    typedNil,
			response:  typedNil,
			completed: typedNil,
		},
		{
			name:      "nil function adapters",
			received:  edgekit.RequestReceivedHookFunc(nil),
			before:    edgekit.BeforeUpstreamHookFunc(nil),
			response:  edgekit.UpstreamResponseHookFunc(nil),
			completed: edgekit.RequestCompletedHookFunc(nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := edgekit.New(edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				Hooks: edgekit.Hooks{
					RequestReceived: []edgekit.RequestReceivedHook{
						&testHooks{},
						tc.received,
					},
					BeforeUpstream: []edgekit.BeforeUpstreamHook{
						&testHooks{},
						tc.before,
					},
					UpstreamResponse: []edgekit.UpstreamResponseHook{
						&testHooks{},
						tc.response,
					},
					RequestCompleted: []edgekit.RequestCompletedHook{
						&testHooks{},
						tc.completed,
					},
				},
			})
			if !errors.Is(err, edgekit.ErrInvalidConfiguration) {
				t.Fatalf("New error = %v, want ErrInvalidConfiguration", err)
			}

			for _, phase := range []string{
				"request received",
				"before upstream",
				"upstream response",
				"request completed",
			} {
				location := "hooks: " + phase + " 1: is nil"
				if !strings.Contains(err.Error(), location) {
					t.Errorf("New error = %v, want %q", err, location)
				}
			}
		})
	}
}

func TestEmptyHookListsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks edgekit.Hooks
	}{
		{
			name: "zero value",
		},
		{
			name: "empty lists",
			hooks: edgekit.Hooks{
				RequestReceived:  []edgekit.RequestReceivedHook{},
				BeforeUpstream:   []edgekit.BeforeUpstreamHook{},
				UpstreamResponse: []edgekit.UpstreamResponseHook{},
				RequestCompleted: []edgekit.RequestCompletedHook{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:   "/api/",
						Upstream: static(t, "http://upstream.test"),
					},
				},
				Transport: hookTransport(),
				Hooks:     tc.hooks,
			})
			w := httptest.NewRecorder()
			g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/x", nil))

			if w.Code != http.StatusNoContent {
				t.Errorf("status = %d, want 204", w.Code)
			}
		})
	}
}

func TestHookRegistrationDoesNotDeclareHeaders(t *testing.T) {
	var steps []string
	hook := &declaringHook{
		testHooks: recordingHooks(&steps, "registered"),
		names: []string{
			"X-Declared",
		},
	}
	var observed string
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				hook,
			},
		},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			observed = r.Header.Get("X-Declared")

			return hookTransport().RoundTrip(r)
		}),
	})
	r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	r.Header.Set("X-Declared", "client value")
	g.ServeHTTP(httptest.NewRecorder(), r)

	if hook.calls != 0 {
		t.Errorf("ManagedHeaders calls = %d, want 0", hook.calls)
	}

	if observed != "client value" {
		t.Errorf("upstream X-Declared = %q, want the unchanged client value", observed)
	}

	if !slices.Equal(steps, []string{"before:registered"}) {
		t.Errorf("steps = %v, want only the registered phase", steps)
	}
}

func TestHeaderOwnerDeclarationsAreCopiedOnce(t *testing.T) {
	var steps []string
	owner := &declaringHook{
		testHooks: recordingHooks(&steps, "unregistered"),
		names: []string{
			"X-Declared",
		},
	}
	replacement := &declaringHook{
		names: []string{
			"X-Changed",
		},
	}
	var headers []http.Header
	cfg := edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: static(t, "http://upstream.test"),
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			owner,
		},
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			headers = append(headers, r.Header.Clone())

			return hookTransport().RoundTrip(r)
		}),
	}
	g := newGateway(t, cfg)
	owner.names[0] = "X-Changed"
	cfg.HeaderOwners[0] = replacement

	for range 2 {
		r := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		r.Header.Set("X-Declared", "spoofed")
		r.Header.Set("X-Changed", "kept")
		g.ServeHTTP(httptest.NewRecorder(), r)
	}

	if owner.calls != 1 {
		t.Errorf("owner ManagedHeaders calls = %d, want 1", owner.calls)
	}

	if replacement.calls != 0 {
		t.Errorf("replacement ManagedHeaders calls = %d, want 0", replacement.calls)
	}

	if len(steps) != 0 {
		t.Errorf("unregistered lifecycle hooks ran: %v", steps)
	}

	if len(headers) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(headers))
	}

	for i, header := range headers {
		if got := header.Get("X-Declared"); got != "" {
			t.Errorf("request %d: upstream X-Declared = %q, want it removed by the original declaration", i, got)
		}

		if got := header.Get("X-Changed"); got != "kept" {
			t.Errorf("request %d: upstream X-Changed = %q, want %q because the changed declaration is not used", i, got, "kept")
		}
	}
}

func ExampleHooks() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	upstream, err := url.Parse(server.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(upstream)
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:   "/api/",
				Upstream: selector,
			},
		},
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				edgekit.BeforeUpstreamHookFunc(func(context.Context, *edgekit.RequestContext, *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					fmt.Println("before upstream")

					return nil, nil
				}),
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				edgekit.RequestCompletedHookFunc(func(context.Context, *edgekit.RequestContext, edgekit.Completion) {
					fmt.Println("completed first")
				}),
				edgekit.RequestCompletedHookFunc(func(context.Context, *edgekit.RequestContext, edgekit.Completion) {
					fmt.Println("completed second")
				}),
			},
		},
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	defer gateway.CloseIdleConnections()
	gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/x", nil))

	// Output:
	// before upstream
	// completed first
	// completed second
}
