package edgekit_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// buffering sets explicit test resource values, not deployment defaults.
func buffering(t *testing.T, bytes int64) *edgekit.RequestBuffering {
	t.Helper()

	budget, err := edgekit.NewRequestBufferBudget(bytes)
	if err != nil {
		t.Fatalf("NewRequestBufferBudget: %v", err)
	}

	return &edgekit.RequestBuffering{
		Budget:       budget,
		IdleTimeout:  time.Minute,
		TotalTimeout: time.Minute,
	}
}

// requestOutcome holds the comparable result fields of one request, so a
// test can compare the complete result with one expected value.
type requestOutcome struct {
	status int
	failed bool
	phase  edgekit.Phase
	reason edgekit.RejectionReason
}

// outcomeOf extracts the comparable fields of a response status and its
// completion. It makes no assertion.
func outcomeOf(status int, c edgekit.Completion) requestOutcome {
	out := requestOutcome{
		status: status,
		reason: c.RejectionReason,
	}
	if c.Failure != nil {
		out.failed = true
		out.phase = c.Failure.Phase
	}

	return out
}

func TestRequestBufferingCompletesBeforeMiddlewareAndPreservesUnmanagedTrailers(t *testing.T) {
	for _, tc := range []struct {
		name          string
		inboundHTTP2  bool
		upstreamHTTP2 bool
	}{
		{
			name: "http1 to http1",
		},
		{
			name:          "http1 to http2",
			upstreamHTTP2: true,
		},
		{
			name:         "http2 to http1",
			inboundHTTP2: true,
		},
		{
			name:          "http2 to http2",
			inboundHTTP2:  true,
			upstreamHTTP2: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type observation struct {
				body     string
				trailers string
			}
			upstreamSaw := make(chan observation, 1)
			upstream, transport := protocolUpstream(t, tc.upstreamHTTP2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.ProtoMajor == 2; got != tc.upstreamHTTP2 {
					t.Errorf("upstream HTTP/2 = %t, want %t", got, tc.upstreamHTTP2)
				}

				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("upstream ReadAll(body) error = %v, want nil", err)
				}
				upstreamSaw <- observation{
					body:     string(body),
					trailers: trailerValues(r.Trailer),
				}
				w.WriteHeader(http.StatusNoContent)
			}))

			var replayCalls atomic.Int32
			var transportCalls atomic.Int32
			g := newGateway(t, edgekit.Config{
				BodyLimit:      edgekit.MaxBodyBytes(2),
				ManagedHeaders: []string{"X-Managed"},
				Routes: []edgekit.Route{
					{
						Prefix:           "/api/",
						Upstream:         static(t, upstream.URL),
						BodyLimit:        edgekit.MaxBodyBytes(6),
						RequestBuffering: buffering(t, 6),
					},
				},
				SharedMiddleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if got := r.ProtoMajor == 2; got != tc.inboundHTTP2 {
								t.Errorf("inbound HTTP/2 = %t, want %t", got, tc.inboundHTTP2)
							}
							if got := trailerValues(r.Trailer); got != "X-End=checked" {
								t.Errorf("middleware entry: trailers = %q, want %q", got, "X-End=checked")
							}
							if r.GetBody == nil {
								t.Error("middleware entry: GetBody = nil, want a replay function")
							}
							var readers sync.WaitGroup
							for range 8 {
								readers.Go(func() {
									replay, err := r.GetBody()
									if err != nil {
										t.Errorf("GetBody() error = %v, want nil", err)

										return
									}
									data, err := io.ReadAll(replay)
									_ = replay.Close()
									if string(data) != "abcdef" || err != nil {
										t.Errorf("ReadAll(replay) = %q, %v; want %q, nil", data, err, "abcdef")
									}
									replayCalls.Add(1)
								})
							}
							readers.Wait()

							data, err := io.ReadAll(r.Body)
							if string(data) != "abcdef" || err != nil {
								t.Errorf("ReadAll(r.Body) = %q, %v; want %q, nil", data, err, "abcdef")
							}
							_ = r.Body.Close()
							out := r.Clone(r.Context())
							out.Body, err = r.GetBody()
							if err != nil {
								t.Errorf("GetBody() for forwarding error = %v, want nil", err)

								return
							}
							next.ServeHTTP(w, out)
						})
					},
				},
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					if r.GetBody != nil {
						t.Error("transport request GetBody is set, want nil")
					}

					return transport.RoundTrip(r)
				}),
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: tc.inboundHTTP2})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", unknownLength{strings.NewReader("abcdef")})
			if err != nil {
				t.Fatal(err)
			}
			req.Trailer = http.Header{
				"X-End":           {"checked"},
				"X-Managed":       {"spoofed"},
				"X-Forwarded-For": {"203.0.113.9"},
			}
			resp, err := ts.client(t, tc.inboundHTTP2).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			seen := receive(t, upstreamSaw)

			type result struct {
				status    int
				upstream  observation
				replays   int32
				forwarded int32
			}
			got := result{
				status:    resp.StatusCode,
				upstream:  seen,
				replays:   replayCalls.Load(),
				forwarded: transportCalls.Load(),
			}
			want := result{
				status: http.StatusNoContent,
				upstream: observation{
					body:     "abcdef",
					trailers: "X-End=checked",
				},
				replays:   8,
				forwarded: 1,
			}
			if got != want {
				t.Errorf("POST /api/upload with trailers X-End, X-Managed, and X-Forwarded-For = %+v, want %+v", got, want)
			}
		})
	}
}

func TestBufferedExpectContinue(t *testing.T) {
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:           "/api/",
				Upstream:         static(t, "http://unused.test"),
				BodyLimit:        edgekit.MaxBodyBytes(4),
				RequestBuffering: buffering(t, 8),
			},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if n, err := io.Copy(io.Discard, r.Body); n != 4 || err != nil {
						t.Errorf("io.Copy(buffered body) = %d, %v; want 4, nil", n, err)
					}
					w.WriteHeader(http.StatusNoContent)
				})
			},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	client := ts.client(t, false)
	client.Transport.(*http.Transport).ExpectContinueTimeout = time.Second
	for _, expect := range []string{
		"100-continue",
		"100-continue, foo",
		"foo, 100-CoNtInUe",
	} {
		t.Run(expect, func(t *testing.T) {
			var continued atomic.Int32
			trace := &httptrace.ClientTrace{
				Got100Continue: func() { continued.Add(1) },
			}
			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodPost, ts.url()+"/api/upload", strings.NewReader("body"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Expect", expect)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Errorf("POST /api/upload with Expect %q: status = %d, want %d", expect, resp.StatusCode, http.StatusNoContent)
			}
			if got := continued.Load(); got != 1 {
				t.Errorf("POST /api/upload with Expect %q: 100 Continue responses = %d, want 1", expect, got)
			}
		})
	}
}

func TestBufferedEmptyBodyWithZeroLimit(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:           "/api/",
						Upstream:         static(t, "http://unused.test"),
						BodyLimit:        edgekit.MaxBodyBytes(0),
						RequestBuffering: buffering(t, 8),
					},
				},
				SharedMiddleware: []edgekit.Middleware{
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							data, err := io.ReadAll(r.Body)
							if len(data) != 0 || err != nil {
								t.Errorf("ReadAll(empty buffered body) = %q, %v; want empty, nil", data, err)
							}
							if r.GetBody == nil {
								t.Error("empty buffered body: GetBody = nil, want a replay function")
							}
							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{TransferIdleTimeout: edgekit.NoTimeout}, serverOptions{tls: http2})
			for _, known := range []bool{false, true} {
				t.Run(fmt.Sprintf("declared=%t", known), func(t *testing.T) {
					var body io.Reader = strings.NewReader("")
					if !known {
						body = unknownLength{body}
					}
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/empty", body)
					if err != nil {
						t.Fatal(err)
					}
					resp, err := ts.client(t, http2).Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
					if resp.StatusCode != http.StatusNoContent {
						t.Errorf("POST /api/empty (declared %t): status = %d, want %d", known, resp.StatusCode, http.StatusNoContent)
					}
				})
			}
		})
	}
}

func TestBufferedOversizeNeverReachesProcessingOrUpstream(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
			for _, known := range []bool{false, true} {
				t.Run(fmt.Sprintf("declared=%t", known), func(t *testing.T) {
					var forwarded, processed, matched atomic.Int32
					collector, completed := completions()
					g := newGateway(t, edgekit.Config{
						Routes: []edgekit.Route{
							{
								Prefix:           "/api/",
								Upstream:         static(t, "http://unused.test"),
								BodyLimit:        edgekit.MaxBodyBytes(4),
								RequestBuffering: buffering(t, 4),
							},
						},
						SharedMiddleware: []edgekit.Middleware{
							func(next http.Handler) http.Handler {
								return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
									processed.Add(1)
									next.ServeHTTP(w, r)
								})
							},
						},
						Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
							forwarded.Add(1)

							return nil, errors.New("unexpected forwarding")
						}),
						Hooks: edgekit.Hooks{
							RouteMatched: []edgekit.RouteMatchedHook{
								edgekit.RouteMatchedHookFunc(func(context.Context, *edgekit.RequestContext, *http.Request) (*edgekit.Rejection, error) {
									matched.Add(1)

									return nil, nil
								}),
							},
							RequestCompleted: []edgekit.RequestCompletedHook{collector},
						},
					})
					ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: http2})
					var reader io.Reader = strings.NewReader("abcde")
					if !known {
						reader = unknownLength{reader}
					}
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", reader)
					if err != nil {
						t.Fatal(err)
					}
					resp, err := ts.client(t, http2).Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
					c := receive(t, completed)

					type result struct {
						outcome   requestOutcome
						matched   int32
						processed int32
						forwarded int32
					}
					got := result{
						outcome:   outcomeOf(resp.StatusCode, c),
						matched:   matched.Load(),
						processed: processed.Load(),
						forwarded: forwarded.Load(),
					}
					// A declared length is rejected before RouteMatched; an unknown length
					// overflows while the gateway buffers it.
					want := result{
						outcome: requestOutcome{
							status: http.StatusRequestEntityTooLarge,
							reason: edgekit.ReasonBodyTooLarge,
						},
					}
					if !known {
						want.outcome = requestOutcome{
							status: http.StatusRequestEntityTooLarge,
							failed: true,
							phase:  edgekit.PhaseRequestBuffering,
						}
						want.matched = 1
					}
					if got != want {
						t.Errorf("POST /api/upload (declared %t) with 5 bytes over a 4-byte limit = %+v, want %+v", known, got, want)
					}
					if !known && (c.Failure == nil || !errors.Is(c.Failure.Err, edgekit.ErrRequestBodyTooLarge)) {
						t.Errorf("POST /api/upload (declared %t) with 5 bytes over a 4-byte limit: Completion.Failure = %+v, want an error wrapping %v", known, c.Failure, edgekit.ErrRequestBodyTooLarge)
					}
				})
			}
		})
	}
}

func TestBufferReservationLastsThroughResponseAndRevokesHandles(t *testing.T) {
	release, finish := releaser(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "end")
	}))
	t.Cleanup(func() {
		finish()
		upstream.Close()
	})

	type handles struct {
		body   io.ReadCloser
		replay func() (io.ReadCloser, error)
	}
	kept := make(chan handles, 2)
	collector, completed := completions()
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:           "/api/",
				Upstream:         static(t, upstream.URL),
				BodyLimit:        edgekit.MaxBodyBytes(8),
				RequestBuffering: buffering(t, 8),
			},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					replay, err := r.GetBody()
					if err != nil {
						t.Errorf("GetBody() error = %v, want nil", err)

						return
					}
					kept <- handles{
						body:   replay,
						replay: r.GetBody,
					}
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{collector},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	client := ts.client(t, false)
	// An unknown length reserves the complete route limit, so one live
	// request fills the budget.
	post := func() *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", unknownLength{strings.NewReader("x")})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		return resp
	}

	first := post()
	h := receive(t, kept)
	second := post()
	_ = second.Body.Close()
	c := receive(t, completed)
	wantSaturated := requestOutcome{
		status: http.StatusServiceUnavailable,
		reason: edgekit.ReasonRequestBufferSaturated,
	}
	if got := outcomeOf(second.StatusCode, c); got != wantSaturated {
		t.Errorf("second POST /api/upload during the first response = %+v, want %+v", got, wantSaturated)
	}
	if data, err := io.ReadAll(h.body); string(data) != "x" || err != nil {
		t.Errorf("ReadAll(retained replay) during the response = %q, %v; want %q, nil", data, err, "x")
	}

	finish()
	_, _ = io.Copy(io.Discard, first.Body)
	_ = first.Body.Close()
	c = receive(t, completed)
	if c.Failure != nil {
		t.Errorf("first POST /api/upload: Completion.Failure = %+v, want nil", c.Failure)
	}
	if _, err := h.body.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("retained replay Read() after release error = %v, want %v", err, io.ErrClosedPipe)
	}
	if _, err := h.replay(); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("retained GetBody() after release error = %v, want %v", err, io.ErrClosedPipe)
	}

	third := post()
	_, _ = io.Copy(io.Discard, third.Body)
	_ = third.Body.Close()
	if third.StatusCode != http.StatusOK {
		t.Errorf("third POST /api/upload after release: status = %d, want %d", third.StatusCode, http.StatusOK)
	}
}

func TestBufferedInboundTimeouts(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		for _, totalFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("http2=%t/total=%t", http2, totalFirst), func(t *testing.T) {
				cfg := buffering(t, 8)
				if totalFirst {
					cfg.TotalTimeout = 30 * time.Millisecond
				} else {
					cfg.IdleTimeout = 30 * time.Millisecond
				}
				collector, completed := completions()
				g := newGateway(t, edgekit.Config{
					Routes: []edgekit.Route{
						{
							Prefix:           "/api/",
							Upstream:         static(t, "http://unused.test"),
							BodyLimit:        edgekit.MaxBodyBytes(8),
							RequestBuffering: cfg,
						},
					},
					Hooks: edgekit.Hooks{
						RequestCompleted: []edgekit.RequestCompletedHook{collector},
					},
				})
				ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: http2})
				reader, writer := io.Pipe()
				t.Cleanup(func() { _ = writer.Close() })
				req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", reader)
				if err != nil {
					t.Fatal(err)
				}
				req.ContentLength = 4
				type response struct {
					value *http.Response
					err   error
				}
				done := make(chan response, 1)
				client := ts.client(t, http2)
				go func() {
					resp, err := client.Do(req)
					if err == nil {
						_, _ = io.Copy(io.Discard, resp.Body)
						_ = resp.Body.Close()
					}
					done <- response{
						value: resp,
						err:   err,
					}
				}()
				if _, err := writer.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
				got := receive(t, done)
				if got.err != nil {
					t.Fatal(got.err)
				}
				_, _ = io.Copy(io.Discard, got.value.Body)
				_ = got.value.Body.Close()
				c := receive(t, completed)
				want := requestOutcome{
					status: http.StatusRequestTimeout,
					failed: true,
					phase:  edgekit.PhaseRequestBuffering,
				}
				if outcome := outcomeOf(got.value.StatusCode, c); outcome != want {
					t.Errorf("POST /api/upload that stops after 1 of 4 bytes (total timeout %t) = %+v, want %+v", totalFirst, outcome, want)
				}
			})
		}
	}
}

func TestBufferedTruncatedAndMalformedHTTP1Bodies(t *testing.T) {
	for _, tc := range []struct {
		name       string
		framing    string
		body       string
		wantStatus int
	}{
		{
			name:       "truncated declared body",
			framing:    "Content-Length: 4\r\n",
			body:       "x",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "malformed chunk",
			framing:    "Transfer-Encoding: chunked\r\n",
			body:       "ZZ\r\nx\r\n",
			wantStatus: http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:           "/api/",
						Upstream:         static(t, "http://unused.test"),
						BodyLimit:        edgekit.MaxBodyBytes(8),
						RequestBuffering: buffering(t, 8),
					},
				},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{collector},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp", ts.listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			if _, err = io.WriteString(conn, "POST /api/upload HTTP/1.1\r\nHost: client.test\r\n"+tc.framing+"\r\n"+tc.body); err != nil {
				t.Fatal(err)
			}
			if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			c := receive(t, completed)
			want := requestOutcome{
				status: tc.wantStatus,
				failed: true,
				phase:  edgekit.PhaseRequestBuffering,
			}
			if got := outcomeOf(resp.StatusCode, c); got != want {
				t.Errorf("POST /api/upload with %q and body %q = %+v, want %+v", tc.framing, tc.body, got, want)
			}
		})
	}
}

func TestBufferedHTTP2TruncationWithoutTypedEvidenceIsInternal(t *testing.T) {
	collector, completed := completions()
	var processed atomic.Bool
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:           "/api/",
				Upstream:         static(t, "http://unused.test"),
				BodyLimit:        edgekit.MaxBodyBytes(8),
				RequestBuffering: buffering(t, 8),
			},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					processed.Store(true)
					next.ServeHTTP(w, r)
				})
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{collector},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: true})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	// Go sends END_STREAM with one byte. Its server reports the length
	// mismatch with an untyped error, not io.ErrUnexpectedEOF.
	req.ContentLength = 4
	resp, err := ts.client(t, true).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	c := receive(t, completed)
	type result struct {
		proto     int
		outcome   requestOutcome
		processed bool
		selected  bool
	}
	got := result{
		proto:     resp.ProtoMajor,
		outcome:   outcomeOf(resp.StatusCode, c),
		processed: processed.Load(),
		selected:  c.SelectedUpstream != nil,
	}
	want := result{
		proto: 2,
		outcome: requestOutcome{
			status: http.StatusInternalServerError,
			failed: true,
			phase:  edgekit.PhaseRequestBuffering,
		},
	}
	if got != want {
		t.Errorf("HTTP/2 POST /api/upload with 1 of 4 declared bytes = %+v, want %+v", got, want)
	}
	if strings.Contains(string(data), "Content-Length") || strings.Contains(string(data), "wrote") {
		t.Errorf("HTTP/2 POST /api/upload with 1 of 4 declared bytes: response body = %q, want no reader failure details", data)
	}
}

func TestBufferedUpgradeReservationEndsAtShutdown(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	upstream, upstreamClosed := switchingUpstream(t, ready, 0)
	collector, completed := completions()
	kept := make(chan func() (io.ReadCloser, error), 1)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:           "/api/",
				Upstream:         static(t, upstream.URL),
				BodyLimit:        edgekit.MaxBodyBytes(8),
				RequestBuffering: buffering(t, 8),
				Middleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if r.URL.Path == "/api/probe" {
								w.WriteHeader(http.StatusNoContent)

								return
							}
							kept <- r.GetBody
							next.ServeHTTP(w, r)
						})
					},
				},
			},
		},
		Hooks: edgekit.Hooks{
			RequestCompleted: []edgekit.RequestCompletedHook{collector},
		},
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	conn := ts.dial(t)
	// A request without a body reserves nothing, so the upgrade request has
	// a body of unknown length, which reserves the complete route limit.
	writeClientData(t, conn, "GET /api/ HTTP/1.1\r\nHost: gateway.test\r\nConnection: Upgrade\r\nUpgrade: echo\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n")
	switched, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read 101 response: %v", err)
	}
	_ = switched.Body.Close()
	if switched.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", switched.StatusCode)
	}
	replay := receive(t, kept)
	newProbe := func(url string) *http.Request {
		probe, probeErr := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/api/probe", unknownLength{strings.NewReader("x")})
		if probeErr != nil {
			t.Fatal(probeErr)
		}

		return probe
	}
	resp, err := ts.client(t, false).Do(newProbe(ts.url()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	c := receive(t, completed)
	wantSaturated := requestOutcome{
		status: http.StatusServiceUnavailable,
		reason: edgekit.ReasonRequestBufferSaturated,
	}
	if got := outcomeOf(resp.StatusCode, c); got != wantSaturated {
		t.Errorf("POST /api/probe during the upgraded connection = %+v, want %+v", got, wantSaturated)
	}
	if err = ts.server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	receive(t, upstreamClosed)
	c = receive(t, completed)
	if c.Aborted {
		t.Error("upgrade request after Shutdown: Completion.Aborted = true, want false")
	}
	if _, err = replay(); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("retained upgrade GetBody() after Shutdown error = %v, want %v", err, io.ErrClosedPipe)
	}

	other := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	resp, err = other.client(t, false).Do(newProbe(other.url()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST /api/probe after Shutdown: status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	receive(t, completed)
}

func TestBufferedBodyReplacementOwnsBoundsAndTrailers(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replacement bool
		change      func(*http.Request)
		want        string
		invalid     bool
	}{
		{
			name: "unchanged",
			want: "body:client",
		},
		{
			name:        "replacement",
			replacement: true,
			want:        "generated:",
		},
		{
			name:        "replacement new value",
			replacement: true,
			change:      func(r *http.Request) { r.Trailer.Set("X-End", "extension") },
			want:        "generated:extension",
		},
		{
			name:   "case variant value",
			change: func(r *http.Request) { r.Trailer["x-end"] = []string{"extension"} },
			want:   "body:extension",
		},
		{
			name:   "case variant equal value",
			change: func(r *http.Request) { r.Trailer["x-end"] = []string{"client"} },
			want:   "body:client",
		},
		{
			name:        "replacement case variant",
			replacement: true,
			change:      func(r *http.Request) { r.Trailer["x-end"] = []string{"client"} },
			want:        "generated:client",
		},
		{
			name:   "deleted trailer",
			change: func(r *http.Request) { delete(r.Trailer, "X-End") },
			want:   "body:",
		},
		{
			name:   "nil trailer map",
			change: func(r *http.Request) { r.Trailer = nil },
			want:   "body:",
		},
		{
			name:    "invalid trailer value",
			change:  func(r *http.Request) { r.Trailer["x-end"] = []string{"private\r\nvalue"} },
			invalid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type received struct {
				body     string
				trailers []string
			}
			got := make(chan received, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("upstream ReadAll(body) error = %v, want nil", err)
				}
				got <- received{
					body:     string(data),
					trailers: slices.Clone(r.Trailer.Values("X-End")),
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(upstream.Close)
			collector, completed := completions()
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{
					{
						Prefix:           "/api/",
						Upstream:         static(t, upstream.URL),
						BodyLimit:        edgekit.MaxBodyBytes(4),
						RequestBuffering: buffering(t, 4),
					},
				},
				SharedMiddleware: []edgekit.Middleware{
					func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							out := r.Clone(r.Context())
							if tc.replacement {
								_ = r.Body.Close()
								out.Body = io.NopCloser(strings.NewReader("generated"))
								out.GetBody = nil
								out.ContentLength = -1
							}
							if tc.change != nil {
								tc.change(out)
							}
							next.ServeHTTP(w, out)
						})
					},
				},
				Hooks: edgekit.Hooks{
					RequestCompleted: []edgekit.RequestCompletedHook{collector},
				},
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", unknownLength{strings.NewReader("body")})
			if err != nil {
				t.Fatal(err)
			}
			req.Trailer = http.Header{
				"X-End": {"client"},
			}
			resp, err := ts.client(t, false).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			c := receive(t, completed)
			if tc.invalid {
				want := requestOutcome{
					status: http.StatusInternalServerError,
					failed: true,
					phase:  edgekit.PhaseBeforeUpstream,
				}
				if outcome := outcomeOf(resp.StatusCode, c); outcome != want {
					t.Errorf("POST /api/upload with an invalid middleware trailer = %+v, want %+v", outcome, want)
				}
				if strings.Contains(string(data), "private") {
					t.Errorf("POST /api/upload with an invalid middleware trailer: response body = %q, want no trailer value", data)
				}
				select {
				case value := <-got:
					t.Errorf("upstream got %+v, want no request", value)
				default:
				}

				return
			}
			wantBody, wantTrailer, _ := strings.Cut(tc.want, ":")
			var wantTrailers []string
			if wantTrailer != "" {
				wantTrailers = []string{wantTrailer}
			}
			seen := receive(t, got)
			want := requestOutcome{
				status: http.StatusNoContent,
			}
			if got := outcomeOf(resp.StatusCode, c); got != want {
				t.Errorf("POST /api/upload with trailer X-End = %+v, want %+v", got, want)
			}
			if seen.body != wantBody {
				t.Errorf("upstream body = %q, want %q", seen.body, wantBody)
			}
			if !slices.Equal(seen.trailers, wantTrailers) {
				t.Errorf("upstream X-End trailer values = %q, want %q", seen.trailers, wantTrailers)
			}
		})
	}
}

func TestBufferingDoesNotEnableTransportReplayRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		buffered bool
		framing  string
		content  string
	}{
		{
			name:     "streaming declared length",
			buffered: false,
			framing:  "Content-Length: 4\r\n\r\nbody",
		},
		{
			name:     "buffered declared length",
			buffered: true,
			framing:  "Content-Length: 4\r\n\r\nbody",
		},
		{
			name:     "buffered unknown length",
			buffered: true,
			framing:  "Transfer-Encoding: chunked\r\n\r\n4\r\nbody\r\n0\r\n\r\n",
		},
		{
			// A buffered empty body of unknown length keeps its body. A
			// declared length of zero would let ReverseProxy drop it, and
			// net/http retries a GET without a body.
			name:     "buffered empty unknown length",
			buffered: true,
			framing:  "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(r.URL.Path, "/warm") {
					w.WriteHeader(http.StatusNoContent)

					return
				}
				if attempts.Add(1) == 1 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("upstream Hijack() error = %v, want nil", err)

						return
					}
					_ = conn.Close()

					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)

			var reused atomic.Bool
			transport := edgekit.NewTransport()
			t.Cleanup(transport.CloseIdleConnections)
			route := edgekit.Route{
				Prefix:    "/api/",
				Upstream:  static(t, upstream.URL),
				BodyLimit: edgekit.MaxBodyBytes(8),
			}
			if tc.buffered {
				route.RequestBuffering = buffering(t, 8)
			}
			g := newGateway(t, edgekit.Config{
				Routes: []edgekit.Route{route},
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					if r.GetBody != nil {
						t.Error("transport request GetBody is set, want nil")
					}
					trace := &httptrace.ClientTrace{
						GotConn: func(info httptrace.GotConnInfo) {
							if strings.HasSuffix(r.URL.Path, "/retry") {
								reused.Store(info.Reused)
							}
						},
					}

					return transport.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), trace)))
				}),
			})
			ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
			warm := sendRaw(t, ts, "GET /api/warm HTTP/1.1\r\nHost: gateway.test\r\n"+tc.framing)
			retry := sendRaw(t, ts, "GET /api/retry HTTP/1.1\r\nHost: gateway.test\r\n"+tc.framing)

			if warm != http.StatusNoContent {
				t.Errorf("GET /api/warm with %q: status = %d, want %d", tc.framing, warm, http.StatusNoContent)
			}
			if retry != http.StatusBadGateway {
				t.Errorf("GET /api/retry with %q: status = %d, want %d", tc.framing, retry, http.StatusBadGateway)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("GET /api/retry with %q: upstream attempts = %d, want 1", tc.framing, got)
			}
			if !reused.Load() {
				t.Errorf("GET /api/retry with %q used a new upstream connection, want a reused one", tc.framing)
			}
		})
	}
}

// holdingGateway serves a buffered route with an 8-byte limit and an 8-byte
// budget. A request to /api/hold keeps its reservation until release closes.
// Other requests get 204 from shared middleware, which calls inspect first.
func holdingGateway(t *testing.T, inspect func(*http.Request)) (*edgekit.Gateway, <-chan struct{}, func()) {
	t.Helper()

	release, finish := releaser(t)
	held := make(chan struct{}, 1)
	g := newGateway(t, edgekit.Config{
		Routes: []edgekit.Route{
			{
				Prefix:           "/api/",
				Upstream:         static(t, "http://unused.test"),
				BodyLimit:        edgekit.MaxBodyBytes(8),
				RequestBuffering: buffering(t, 8),
			},
		},
		SharedMiddleware: []edgekit.Middleware{
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/hold" {
						held <- struct{}{}
						<-release
					} else if inspect != nil {
						inspect(r)
					}
					w.WriteHeader(http.StatusNoContent)
				})
			},
		},
	})

	return g, held, finish
}

// heldResult is the final result of the request that holdReservation sends.
type heldResult struct {
	status int
	err    error
}

// holdReservation sends body to /api/hold and returns its final result on
// the channel.
func holdReservation(t *testing.T, ts *testServer, body io.Reader) <-chan heldResult {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/hold", body)
	if err != nil {
		t.Fatal(err)
	}
	client := ts.client(t, false)
	result := make(chan heldResult, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			result <- heldResult{
				err: err,
			}

			return
		}
		_ = resp.Body.Close()
		result <- heldResult{
			status: resp.StatusCode,
		}
	}()

	return result
}

func TestBufferedRequestWithoutBodySkipsBuffering(t *testing.T) {
	type replayed struct {
		data []byte
		err  error
	}
	replays := make(chan replayed, 2)
	g, held, finish := holdingGateway(t, func(r *http.Request) {
		if r.GetBody == nil {
			replays <- replayed{
				err: errors.New("no GetBody"),
			}

			return
		}
		replay, err := r.GetBody()
		if err != nil {
			replays <- replayed{
				err: err,
			}

			return
		}
		data, err := io.ReadAll(replay)
		replays <- replayed{
			data: data,
			err:  err,
		}
	})
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	// An unknown length reserves the complete budget.
	holder := holdReservation(t, ts, unknownLength{strings.NewReader("x")})
	receive(t, held)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.url()+"/api/empty", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.client(t, false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	served := receive(t, replays)
	// A recorder has no scoped deadline support, so only skipped
	// buffering can serve this request.
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/empty", nil))
	recorded := receive(t, replays)
	finish()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("bodyless request with a full budget: status = %d, want 204", resp.StatusCode)
	}
	if served.err != nil || len(served.data) != 0 {
		t.Errorf("bodyless request with a full budget: replay = %q, %v; want empty, nil", served.data, served.err)
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("bodyless request without deadline support: status = %d, want 204", w.Code)
	}
	if recorded.err != nil || len(recorded.data) != 0 {
		t.Errorf("bodyless request without deadline support: replay = %q, %v; want empty, nil", recorded.data, recorded.err)
	}
	if held := receive(t, holder); held.err != nil || held.status != http.StatusNoContent {
		t.Errorf("holding request = %d, %v; want 204, nil", held.status, held.err)
	}
}

func TestBufferedDeclaredLengthReservesDeclaredBytes(t *testing.T) {
	g, held, finish := holdingGateway(t, nil)
	ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{})
	// The holder declares 3 of the 8 budget bytes.
	holder := holdReservation(t, ts, strings.NewReader("abc"))
	receive(t, held)

	for _, tc := range []struct {
		name       string
		body       io.Reader
		wantStatus int
	}{
		{
			name:       "declared fits",
			body:       strings.NewReader("abcde"),
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "declared over",
			body:       strings.NewReader("abcdef"),
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:       "unknown length",
			body:       unknownLength{strings.NewReader("a")},
			wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", tc.body)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := ts.client(t, false).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("POST /api/upload (%s) while 3 of 8 budget bytes are held: status = %d, want %d", tc.name, resp.StatusCode, tc.wantStatus)
			}
		})
	}

	finish()
	if held := receive(t, holder); held.err != nil || held.status != http.StatusNoContent {
		t.Errorf("holding request = %d, %v; want 204, nil", held.status, held.err)
	}
}

func TestBufferedForwardDeclaresStoredSize(t *testing.T) {
	type observation struct {
		length   int64
		chunked  bool
		body     string
		trailers string
	}
	for _, tc := range []struct {
		name     string
		content  string
		declared bool
		trailer  bool
		raw      string
		forward  func(*http.Request) (*http.Request, error)
		want     observation
	}{
		{
			name:    "unread cursor",
			content: "hello",
			want: observation{
				length: 5,
				body:   "hello",
			},
		},
		{
			name:    "replay after read",
			content: "hello",
			forward: func(r *http.Request) (*http.Request, error) {
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				out := r.Clone(r.Context())
				replay, err := r.GetBody()
				out.Body = replay

				return out, err
			},
			want: observation{
				length: 5,
				body:   "hello",
			},
		},
		{
			name:    "partly read cursor",
			content: "hello",
			forward: func(r *http.Request) (*http.Request, error) {
				_, err := io.ReadFull(r.Body, make([]byte, 2))

				return r, err
			},
			want: observation{
				length:  -1,
				chunked: true,
				body:    "llo",
			},
		},
		{
			name:    "wrapped cursor",
			content: "hello",
			forward: func(r *http.Request) (*http.Request, error) {
				out := r.Clone(r.Context())
				out.Body = io.NopCloser(r.Body)

				return out, nil
			},
			want: observation{
				length:  -1,
				chunked: true,
				body:    "hello",
			},
		},
		{
			name:    "client trailers",
			content: "hello",
			trailer: true,
			want: observation{
				length:   -1,
				chunked:  true,
				body:     "hello",
				trailers: "checked",
			},
		},
		{
			name:     "declared length",
			content:  "hello",
			declared: true,
			want: observation{
				length: 5,
				body:   "hello",
			},
		},
		{
			name: "empty unknown length",
			raw:  "POST /api/upload HTTP/1.1\r\nHost: gateway.test\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n",
			want: observation{
				length:  -1,
				chunked: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, http2 := range []bool{false, true} {
				// The raw request is HTTP/1.1 chunked framing, which HTTP/2
				// does not have.
				if http2 && tc.raw != "" {
					continue
				}
				t.Run(fmt.Sprintf("http2=%t", http2), func(t *testing.T) {
					got := make(chan observation, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Errorf("upstream ReadAll(body) error = %v, want nil", err)
						}
						got <- observation{
							length:   r.ContentLength,
							chunked:  slices.Equal(r.TransferEncoding, []string{"chunked"}),
							body:     string(body),
							trailers: r.Trailer.Get("X-End"),
						}
						w.WriteHeader(http.StatusNoContent)
					}))
					t.Cleanup(upstream.Close)

					var middleware []edgekit.Middleware
					if tc.forward != nil {
						middleware = append(middleware, func(next http.Handler) http.Handler {
							return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								out, err := tc.forward(r)
								if err != nil {
									t.Errorf("forward(%s) error = %v, want nil", tc.name, err)
									w.WriteHeader(http.StatusInternalServerError)

									return
								}
								next.ServeHTTP(w, out)
							})
						})
					}
					g := newGateway(t, edgekit.Config{
						Routes: []edgekit.Route{
							{
								Prefix:           "/api/",
								Upstream:         static(t, upstream.URL),
								BodyLimit:        edgekit.MaxBodyBytes(8),
								RequestBuffering: buffering(t, 8),
								Middleware:       middleware,
							},
						},
					})
					ts := startServer(t, g, edgekit.ServerConfig{}, serverOptions{tls: http2})
					status := 0
					if tc.raw != "" {
						status = sendRaw(t, ts, tc.raw)
					} else {
						var body io.Reader = strings.NewReader(tc.content)
						if !tc.declared {
							body = unknownLength{body}
						}
						req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.url()+"/api/upload", body)
						if err != nil {
							t.Fatal(err)
						}
						if tc.trailer {
							req.Trailer = http.Header{
								"X-End": {"checked"},
							}
						}
						resp, err := ts.client(t, http2).Do(req)
						if err != nil {
							t.Fatal(err)
						}
						_ = resp.Body.Close()
						status = resp.StatusCode
					}
					seen := receive(t, got)

					if status != http.StatusNoContent {
						t.Errorf("POST /api/upload (%s, http2=%t): status = %d, want %d", tc.name, http2, status, http.StatusNoContent)
					}
					if seen != tc.want {
						t.Errorf("POST /api/upload (%s, http2=%t): upstream got %+v, want %+v", tc.name, http2, seen, tc.want)
					}
				})
			}
		})
	}
}
