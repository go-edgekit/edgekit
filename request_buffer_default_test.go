package edgekit_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// changedStreaming returns explicit streaming after change modifies it.
func changedStreaming(change func(*edgekit.RequestBuffering)) *edgekit.RequestBuffering {
	cfg := edgekit.NoRequestBuffering()
	change(cfg)

	return cfg
}

// configurationProblems returns the joined problems of New, or nil when err
// is not a joined error.
func configurationProblems(err error) []error {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return nil
	}

	return joined.Unwrap()
}

// bufferingChoice selects the request-buffering setting of a gateway or route.
type bufferingChoice int

const (
	omittedBuffering bufferingChoice = iota
	explicitStreaming
	completeBuffering
)

// bufferingSetting returns a new setting for choice. A complete setting has
// its own budget of the given bytes.
func bufferingSetting(t *testing.T, choice bufferingChoice, bytes int64) *edgekit.RequestBuffering {
	t.Helper()

	switch choice {
	case omittedBuffering:
		return nil
	case explicitStreaming:
		return edgekit.NoRequestBuffering()
	case completeBuffering:
		return buffering(t, bytes)
	}

	t.Fatalf("bufferingSetting: undefined choice %d", choice)

	return nil
}

func TestInheritedRequestBufferConfiguration(t *testing.T) {
	budget, err := edgekit.NewRequestBufferBudget(8)
	if err != nil {
		t.Fatal(err)
	}
	largeBudget, err := edgekit.NewRequestBufferBudget(edgekit.DefaultBodyLimit)
	if err != nil {
		t.Fatal(err)
	}
	complete := &edgekit.RequestBuffering{
		Budget:       budget,
		IdleTimeout:  time.Second,
		TotalTimeout: time.Minute,
	}
	for _, tc := range []struct {
		name         string
		gatewayLimit edgekit.BodyLimit
		routeLimit   edgekit.BodyLimit
		gateway      *edgekit.RequestBuffering
		route        *edgekit.RequestBuffering
		wantError    string
	}{
		{
			name: "omitted settings stream",
		},
		{
			name:    "explicit gateway streaming",
			gateway: edgekit.NoRequestBuffering(),
		},
		{
			name:         "inherited buffering",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			gateway:      complete,
		},
		{
			name: "inherited default body limit",
			gateway: &edgekit.RequestBuffering{
				Budget:       largeBudget,
				IdleTimeout:  time.Second,
				TotalTimeout: time.Minute,
			},
		},
		{
			name:         "route reduces inherited limit",
			gatewayLimit: edgekit.MaxBodyBytes(16),
			routeLimit:   edgekit.MaxBodyBytes(8),
			gateway:      complete,
		},
		{
			name:         "route replaces disabled limit",
			gatewayLimit: edgekit.NoBodyLimit(),
			routeLimit:   edgekit.MaxBodyBytes(8),
			gateway:      complete,
		},
		{
			name:         "inherited zero limit",
			gatewayLimit: edgekit.MaxBodyBytes(0),
			gateway:      complete,
		},
		{
			name:         "streaming overrides default with disabled limit",
			gatewayLimit: edgekit.NoBodyLimit(),
			gateway:      complete,
			route:        edgekit.NoRequestBuffering(),
		},
		{
			name:         "complete route replaces default",
			gatewayLimit: edgekit.MaxBodyBytes(16),
			gateway:      complete,
			route: &edgekit.RequestBuffering{
				Budget:       largeBudget,
				IdleTimeout:  time.Minute,
				TotalTimeout: time.Hour,
			},
		},
		{
			name:         "buffering overrides gateway streaming",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			gateway:      edgekit.NoRequestBuffering(),
			route:        complete,
		},
		{
			name:      "inherited default body limit exceeds budget",
			gateway:   complete,
			wantError: "route 0: request buffering: byte budget is smaller than the effective body limit",
		},
		{
			name:         "route increases limit beyond inherited budget",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			routeLimit:   edgekit.MaxBodyBytes(9),
			gateway:      complete,
			wantError:    "route 0: request buffering: byte budget is smaller than the effective body limit",
		},
		{
			name:         "route disables limit with inherited buffering",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			routeLimit:   edgekit.NoBodyLimit(),
			gateway:      complete,
			wantError:    "route 0: request buffering: requires a finite effective body limit",
		},
		{
			name:         "inherited disabled limit",
			gatewayLimit: edgekit.NoBodyLimit(),
			gateway:      complete,
			wantError:    "route 0: request buffering: requires a finite effective body limit",
		},
		{
			name:         "empty route configuration does not inherit fields",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			gateway:      complete,
			route:        &edgekit.RequestBuffering{},
			wantError:    "route 0: request buffering: requires a constructed byte budget",
		},
		{
			name:         "route budget is not inherited",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			gateway:      complete,
			route: &edgekit.RequestBuffering{
				IdleTimeout:  time.Second,
				TotalTimeout: time.Minute,
			},
			wantError: "route 0: request buffering: requires a constructed byte budget",
		},
		{
			name:         "route idle timeout is not inherited",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			gateway:      complete,
			route: &edgekit.RequestBuffering{
				Budget:       budget,
				TotalTimeout: time.Minute,
			},
			wantError: "route 0: request buffering: requires positive idle and total timeouts",
		},
		{
			name:         "route total timeout is not inherited",
			gatewayLimit: edgekit.MaxBodyBytes(8),
			gateway:      complete,
			route: &edgekit.RequestBuffering{
				Budget:      budget,
				IdleTimeout: time.Second,
			},
			wantError: "route 0: request buffering: requires positive idle and total timeouts",
		},
		{
			name:      "invalid default rejected even when route streams",
			gateway:   &edgekit.RequestBuffering{},
			route:     edgekit.NoRequestBuffering(),
			wantError: "request buffering: requires a constructed byte budget",
		},
		{
			name: "invalid default timeouts rejected even when route streams",
			gateway: &edgekit.RequestBuffering{
				Budget: budget,
			},
			route:     edgekit.NoRequestBuffering(),
			wantError: "request buffering: requires positive idle and total timeouts",
		},
		{
			name: "gateway streaming with settings rejected",
			gateway: changedStreaming(func(cfg *edgekit.RequestBuffering) {
				cfg.Budget = budget
			}),
			route:     edgekit.NoRequestBuffering(),
			wantError: "request buffering: streaming configuration contains buffering settings",
		},
		{
			name:    "route streaming with budget rejected",
			gateway: complete,
			route: changedStreaming(func(cfg *edgekit.RequestBuffering) {
				cfg.Budget = budget
			}),
			wantError: "route 0: request buffering: streaming configuration contains buffering settings",
		},
		{
			name: "route streaming with idle timeout rejected",
			route: changedStreaming(func(cfg *edgekit.RequestBuffering) {
				cfg.IdleTimeout = time.Second
			}),
			wantError: "route 0: request buffering: streaming configuration contains buffering settings",
		},
		{
			name: "route streaming with total timeout rejected",
			route: changedStreaming(func(cfg *edgekit.RequestBuffering) {
				cfg.TotalTimeout = time.Minute
			}),
			wantError: "route 0: request buffering: streaming configuration contains buffering settings",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := edgekit.New(edgekit.Config{
				BodyLimit:        tc.gatewayLimit,
				RequestBuffering: tc.gateway,
				Routes: []edgekit.Route{
					{
						Prefix:           "/",
						Upstream:         static(t, "http://unused.test"),
						BodyLimit:        tc.routeLimit,
						RequestBuffering: tc.route,
					},
				},
			})
			if g != nil {
				t.Cleanup(g.CloseIdleConnections)
			}
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("New() error = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, edgekit.ErrInvalidConfiguration) {
				t.Fatalf("New() error = %v, want ErrInvalidConfiguration", err)
			}
			var got []string
			for _, problem := range configurationProblems(err) {
				if !errors.Is(problem, edgekit.ErrInvalidConfiguration) {
					t.Errorf("problem %v does not wrap ErrInvalidConfiguration", problem)
				}
				got = append(got, problem.Error())
			}
			want := []string{
				edgekit.ErrInvalidConfiguration.Error() + ": " + tc.wantError,
			}
			if !slices.Equal(got, want) {
				t.Errorf("New() problems = %q, want %q", got, want)
			}
			if g != nil {
				t.Error("New() gateway is not nil after invalid configuration")
			}
		})
	}
}

func TestInvalidGatewayRequestBufferingIsReportedOnce(t *testing.T) {
	// The third route has its own settings, so its limit is still checked.
	g, err := edgekit.New(edgekit.Config{
		BodyLimit:        edgekit.NoBodyLimit(),
		RequestBuffering: &edgekit.RequestBuffering{},
		Routes: []edgekit.Route{
			{
				Prefix:    "/first/",
				Upstream:  static(t, "http://unused.test"),
				BodyLimit: edgekit.MaxBodyBytes(8),
			},
			{
				Prefix:    "/second/",
				Upstream:  static(t, "http://unused.test"),
				BodyLimit: edgekit.MaxBodyBytes(8),
			},
			{
				Prefix:           "/third/",
				Upstream:         static(t, "http://unused.test"),
				RequestBuffering: buffering(t, 8),
			},
		},
	})
	if g != nil {
		t.Cleanup(g.CloseIdleConnections)
		t.Error("New() gateway is not nil after invalid configuration")
	}

	var got []string
	for _, problem := range configurationProblems(err) {
		if !errors.Is(problem, edgekit.ErrInvalidConfiguration) {
			t.Errorf("problem %v does not wrap ErrInvalidConfiguration", problem)
		}
		got = append(got, problem.Error())
	}
	prefix := edgekit.ErrInvalidConfiguration.Error() + ": "
	want := []string{
		prefix + "request buffering: requires a constructed byte budget",
		prefix + "route 2: request buffering: requires a finite effective body limit",
	}
	if !slices.Equal(got, want) {
		t.Errorf("New() problems = %q, want %q", got, want)
	}
}

func TestGatewayRequestBufferingSelection(t *testing.T) {
	// The gateway limit and buffering budget are 8 bytes. A route with its own
	// buffering has a budget of 16 bytes. Such cases set a route limit of 16
	// bytes and send 12 bytes. An unknown length reserves the full route limit,
	// so the gateway settings cannot handle these cases.
	for _, tc := range []struct {
		name         string
		gateway      bufferingChoice
		route        bufferingChoice
		routeLimit   edgekit.BodyLimit
		content      string
		wantBuffered bool
	}{
		{
			name:    "omitted settings stream",
			content: "hello",
		},
		{
			name:    "explicit gateway streaming",
			gateway: explicitStreaming,
			content: "hello",
		},
		{
			name:         "inherited buffering",
			gateway:      completeBuffering,
			content:      "hello",
			wantBuffered: true,
		},
		{
			name:    "route streaming overrides buffering",
			gateway: completeBuffering,
			route:   explicitStreaming,
			content: "hello",
		},
		{
			name:         "route buffering overrides gateway streaming",
			gateway:      explicitStreaming,
			route:        completeBuffering,
			routeLimit:   edgekit.MaxBodyBytes(16),
			content:      "hello, world",
			wantBuffered: true,
		},
		{
			name:         "route buffering without gateway default",
			route:        completeBuffering,
			routeLimit:   edgekit.MaxBodyBytes(16),
			content:      "hello, world",
			wantBuffered: true,
		},
		{
			name:         "route buffering replaces gateway buffering",
			gateway:      completeBuffering,
			route:        completeBuffering,
			routeLimit:   edgekit.MaxBodyBytes(16),
			content:      "hello, world",
			wantBuffered: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, http2 := range []bool{false, true} {
				t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
					collector, completed := completions()
					g := newGateway(t, edgekit.Config{
						BodyLimit:        edgekit.MaxBodyBytes(8),
						RequestBuffering: bufferingSetting(t, tc.gateway, 8),
						Routes: []edgekit.Route{
							{
								Prefix:           "/api/",
								Upstream:         static(t, "http://unused.test"),
								BodyLimit:        tc.routeLimit,
								RequestBuffering: bufferingSetting(t, tc.route, 16),
							},
						},
						SharedMiddleware: []edgekit.Middleware{
							func(http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									defer w.WriteHeader(http.StatusNoContent)

									if got := r.GetBody != nil; got != tc.wantBuffered {
										t.Errorf("r.GetBody != nil = %v, want %v", got, tc.wantBuffered)
									}
									data, err := io.ReadAll(r.Body)
									_ = r.Body.Close()
									if string(data) != tc.content || err != nil {
										t.Errorf("ReadAll(Body) = %q, %v; want %q, nil", data, err, tc.content)
									}
									if r.GetBody == nil {
										return
									}

									replay, err := r.GetBody()
									if err != nil {
										t.Errorf("GetBody() error = %v, want nil", err)

										return
									}

									data, err = io.ReadAll(replay)
									_ = replay.Close()
									if string(data) != tc.content || err != nil {
										t.Errorf("ReadAll(replay) = %q, %v; want %q, nil", data, err, tc.content)
									}
								})
							},
						},
						Hooks: edgekit.Hooks{
							RequestCompleted: []edgekit.RequestCompletedHook{collector},
						},
					})
					ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: http2})
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", unknownLength{strings.NewReader(tc.content)})
					if err != nil {
						t.Fatal(err)
					}
					resp, err := ts.client(t, http2).Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
					c := receive(t, completed)
					if resp.StatusCode != http.StatusNoContent {
						t.Errorf("POST /api/upload status = %d, want %d", resp.StatusCode, http.StatusNoContent)
					}
					if c.Failure != nil {
						t.Errorf("Completion.Failure = %+v, want nil", c.Failure)
					}
				})
			}
		})
	}
}

func TestRequestBufferingSettingsAreCopied(t *testing.T) {
	gatewaySetting := buffering(t, 8)
	streamSetting := edgekit.NoRequestBuffering()
	bufferSetting := buffering(t, 8)
	replayed := make(chan bool, 1)
	g := newGateway(t, edgekit.Config{
		BodyLimit:        edgekit.MaxBodyBytes(8),
		RequestBuffering: gatewaySetting,
		Routes: []edgekit.Route{
			{
				Prefix:   "/inherit/",
				Upstream: static(t, "http://unused.test"),
			},
			{
				Prefix:           "/stream/",
				Upstream:         static(t, "http://unused.test"),
				RequestBuffering: streamSetting,
			},
			{
				Prefix:           "/buffer/",
				Upstream:         static(t, "http://unused.test"),
				RequestBuffering: bufferSetting,
			},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					replayed <- r.GetBody != nil
					_, _ = io.Copy(io.Discard, r.Body)
					_ = r.Body.Close()
					w.WriteHeader(http.StatusNoContent)
				})
			},
		},
	})

	// Each changed setting selects the opposite handling. New must ignore it.
	*gatewaySetting = *edgekit.NoRequestBuffering()
	*streamSetting = *buffering(t, 8)
	*bufferSetting = *edgekit.NoRequestBuffering()

	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	client := ts.client(t, false)
	for _, tc := range []struct {
		name         string
		path         string
		wantBuffered bool
	}{
		{
			name:         "inherited gateway buffering",
			path:         "/inherit/upload",
			wantBuffered: true,
		},
		{
			name: "explicit route streaming",
			path: "/stream/upload",
		},
		{
			name:         "route buffering",
			path:         "/buffer/upload",
			wantBuffered: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+tc.path, unknownLength{strings.NewReader("hello")})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			// Only the middleware sends 204, so a different status means that
			// it did not run and sent no value.
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("POST %s status = %d, want %d", tc.path, resp.StatusCode, http.StatusNoContent)
			}

			if got := receive(t, replayed); got != tc.wantBuffered {
				t.Errorf("POST %s: r.GetBody != nil = %v, want %v", tc.path, got, tc.wantBuffered)
			}
		})
	}
}

func TestExplicitStreamingKeepsBodyLimitWithoutBufferDeadlineSupport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		wantStatus int
	}{
		{
			name:       "empty body",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "exact limit",
			content:    "12345678",
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "overflow",
			content:    "123456789",
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t, edgekit.Config{
				BodyLimit:        edgekit.MaxBodyBytes(8),
				RequestBuffering: buffering(t, 8),
				Routes: []edgekit.Route{
					{
						Prefix:           "/api/",
						Upstream:         static(t, "http://unused.test"),
						RequestBuffering: edgekit.NoRequestBuffering(),
					},
				},
				SharedMiddleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.GetBody != nil {
								t.Error("streaming middleware got GetBody, want nil")
							}
							_, err := io.Copy(io.Discard, r.Body)
							_ = r.Body.Close()
							if errors.Is(err, edgekit.ErrRequestBodyTooLarge) {
								w.WriteHeader(http.StatusRequestEntityTooLarge)

								return
							}
							if err != nil {
								t.Errorf("streaming Read() error = %v, want nil", err)
							}
							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			})
			w := httptest.NewRecorder()
			g.ServeHTTP(w, bodyRequest(tc.content, false))

			if w.Code != tc.wantStatus {
				t.Errorf("POST /api/upload status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}

func TestGatewayRequestBufferBudgetIsSharedAcrossRoutes(t *testing.T) {
	release, finish := releaser(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		if r.URL.Path == "/first/upload" {
			<-release
		}
	}))
	t.Cleanup(func() {
		finish()
		upstream.Close()
	})
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		BodyLimit:        edgekit.MaxBodyBytes(8),
		RequestBuffering: buffering(t, 8),
		Routes: []edgekit.Route{
			{
				Prefix:   "/first/",
				Upstream: static(t, upstream.URL),
			},
			{
				Prefix:   "/second/",
				Upstream: static(t, upstream.URL),
			},
			{
				Prefix:           "/stream/",
				Upstream:         static(t, upstream.URL),
				RequestBuffering: edgekit.NoRequestBuffering(),
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{collector},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	client := ts.client(t, false)
	post := func(path string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+path, unknownLength{strings.NewReader("x")})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		return resp
	}

	first := post("/first/upload")
	t.Cleanup(func() { _ = first.Body.Close() })
	second := post("/second/upload")
	_ = second.Body.Close()
	c := receive(t, completed)
	wantSaturated := requestOutcome{
		status: http.StatusServiceUnavailable,
		reason: edgekit.ReasonRequestBufferSaturated,
	}
	if got := outcomeOf(second.StatusCode, c); got != wantSaturated {
		t.Errorf("POST /second/upload while first holds the shared budget = %+v, want %+v", got, wantSaturated)
	}

	stream := post("/stream/upload")
	_, _ = io.Copy(io.Discard, stream.Body)
	_ = stream.Body.Close()
	c = receive(t, completed)
	if stream.StatusCode != http.StatusOK || c.Failure != nil {
		t.Errorf("POST /stream/upload while buffer budget is full: status/failure = %d/%+v, want 200/nil", stream.StatusCode, c.Failure)
	}

	finish()
	_, _ = io.Copy(io.Discard, first.Body)
	_ = first.Body.Close()
	c = receive(t, completed)
	if first.StatusCode != http.StatusOK || c.Failure != nil {
		t.Errorf("POST /first/upload status/failure = %d/%+v, want 200/nil", first.StatusCode, c.Failure)
	}

	third := post("/second/upload")
	_, _ = io.Copy(io.Discard, third.Body)
	_ = third.Body.Close()
	c = receive(t, completed)
	if third.StatusCode != http.StatusOK || c.Failure != nil {
		t.Errorf("POST /second/upload after release: status/failure = %d/%+v, want 200/nil", third.StatusCode, c.Failure)
	}
}
