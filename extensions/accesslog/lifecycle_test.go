package accesslog_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

func TestRepeatedMiddlewareKeepsOneObservation(t *testing.T) {
	sink := newCollector(t, 1)
	traceCalls := 0
	recorder := newRecorder(t, accesslog.Config{
		Sink: sink,
		TraceID: func(context.Context) string {
			traceCalls++

			return "trace"
		},
	})

	type requestID struct {
		id string
		ok bool
	}

	var ids []requestID
	observeID := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var observed requestID
			if rc, ok := edgekit.FromContext(r.Context()); ok {
				observed.id, observed.ok = recorder.RequestID(rc)
			}

			ids = append(ids, observed)
			next.ServeHTTP(w, r)
		})
	}

	g := newGateway(t, edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
			observeID,
			recorder.Middleware,
			observeID,
			func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w, "ok")
				})
			},
		},
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				recorder,
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				recorder,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				recorder,
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			recorder,
		},
		Routes: []edgekit.Route{
			apiRoute(t, "http://upstream.test"),
		},
	})

	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/data", nil))

	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}

	record := records[0]
	if !validRequestID(record.RequestID) {
		t.Errorf("RequestID = %q, want 32 lowercase hexadecimal digits", record.RequestID)
	}

	if len(ids) != 2 {
		t.Fatalf("request ID observations = %d, want 2", len(ids))
	}

	for i, observed := range ids {
		if !observed.ok {
			t.Errorf("RequestID() at middleware position %d reports no ID, want an ID", i)
		}

		if observed.id != record.RequestID {
			t.Errorf("RequestID() at middleware position %d = %q, want %q", i, observed.id, record.RequestID)
		}
	}

	if traceCalls != 1 {
		t.Errorf("TraceID calls = %d, want 1", traceCalls)
	}

	if record.Status != http.StatusCreated {
		t.Errorf("Status = %d, want 201", record.Status)
	}

	if record.BytesWritten != 2 {
		t.Errorf("BytesWritten = %d, want 2", record.BytesWritten)
	}

	if got := w.Body.String(); got != "ok" {
		t.Errorf("response body = %q, want %q", got, "ok")
	}

	if got := w.Header().Get(accesslog.RequestIDHeader); got != record.RequestID {
		t.Errorf("response request ID = %q, want %q", got, record.RequestID)
	}
}

func TestRequestIDEndsAtCompletion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		panicking bool
	}{
		{
			name: "normal",
		},
		{
			name:      "panic",
			panicking: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newCollector(t, 1)
			recorder := newRecorder(t, accesslog.Config{
				Sink: sink,
			})

			var saved *edgekit.RequestContext
			var duringID, completionID string
			var duringOK, completionOK bool
			g := newGateway(t, edgekit.Config{
				Middleware: []edgekit.Middleware{
					recorder.Middleware,
					func(http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if rc, ok := edgekit.FromContext(r.Context()); ok {
								saved = rc
								duringID, duringOK = recorder.RequestID(rc)
							}

							if tc.panicking {
								panic(http.ErrAbortHandler)
							}

							w.WriteHeader(http.StatusNoContent)
						})
					},
				},
				Hooks: edgekit.Hooks{
					BeforeUpstream: []edgekit.BeforeUpstreamHook{
						recorder,
					},
					UpstreamResponse: []edgekit.UpstreamResponseHook{
						recorder,
					},
					RequestCompleted: []edgekit.RequestCompletedHook{
						edgekit.RequestCompletedHookFunc(func(_ context.Context, rc *edgekit.RequestContext, _ edgekit.Completion) {
							completionID, completionOK = recorder.RequestID(rc)
						}),
						recorder,
					},
				},
				HeaderOwners: []edgekit.HeaderOwner{
					recorder,
				},
				Routes: []edgekit.Route{
					apiRoute(t, "http://upstream.test"),
				},
			})

			recovered := func() (value any) {
				defer func() { value = recover() }()

				g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/data", nil))

				return nil
			}()

			var wantPanic any
			if tc.panicking {
				wantPanic = http.ErrAbortHandler
			}

			if recovered != wantPanic {
				t.Errorf("panic = %v, want %v", recovered, wantPanic)
			}

			if saved == nil {
				t.Fatal("middleware got no RequestContext, want a context")
			}

			records := sink.all()
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}

			if !duringOK || duringID != records[0].RequestID {
				t.Errorf("RequestID() during handling = %q, %t; want %q, true", duringID, duringOK, records[0].RequestID)
			}

			if !completionOK || completionID != records[0].RequestID {
				t.Errorf("RequestID() during completion = %q, %t; want %q, true", completionID, completionOK, records[0].RequestID)
			}

			if id, ok := recorder.RequestID(saved); ok || id != "" {
				t.Errorf("RequestID() after completion = %q, %t; want an empty ID and false", id, ok)
			}

			if records[0].Completion.Aborted != tc.panicking {
				t.Errorf("Aborted = %t, want %t", records[0].Completion.Aborted, tc.panicking)
			}
		})
	}
}

func TestRetainedRecordDoesNotChange(t *testing.T) {
	firstErr := errors.New("first failure")
	secondErr := errors.New("second failure")
	sink := newCollector(t, 2)
	recorder := newRecorder(t, accesslog.Config{
		Sink: sink,
	})
	g := newGateway(t, edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: edgekit.Hooks{
			BeforeUpstream: []edgekit.BeforeUpstreamHook{
				recorder,
				edgekit.BeforeUpstreamHookFunc(func(_ context.Context, _ *edgekit.RequestContext, out *edgekit.OutboundRequest) (*edgekit.Rejection, error) {
					if out.URL().Host == "first.test" {
						return nil, firstErr
					}

					return nil, secondErr
				}),
			},
			UpstreamResponse: []edgekit.UpstreamResponseHook{
				recorder,
			},
			RequestCompleted: []edgekit.RequestCompletedHook{
				recorder,
			},
		},
		HeaderOwners: []edgekit.HeaderOwner{
			recorder,
		},
		Routes: []edgekit.Route{
			{
				Prefix:   "/first/",
				Upstream: static(t, "http://first.test/base"),
			},
			{
				Prefix:   "/second/",
				Upstream: static(t, "http://second.test/other"),
			},
		},
	})

	first := httptest.NewRecorder()
	g.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/first/data", nil))

	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("records after first request = %d, want 1", len(records))
	}

	want := records[0]
	if want.Completion.SelectedUpstream == nil || want.Completion.Failure == nil {
		t.Fatalf("first Completion = %+v, want a selected upstream and a failure", want.Completion)
	}

	if !errors.Is(want.Completion.Failure.Err, firstErr) {
		t.Errorf("first Failure.Err = %v, want %v", want.Completion.Failure.Err, firstErr)
	}

	// Copy the pointer fields, so a later mutation cannot also change want.
	upstream := *want.Completion.SelectedUpstream
	want.Completion.SelectedUpstream = &upstream
	failure := *want.Completion.Failure
	want.Completion.Failure = &failure

	second := httptest.NewRecorder()
	g.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/second/items", nil))

	records = sink.all()
	if len(records) != 2 {
		t.Fatalf("records after second request = %d, want 2", len(records))
	}

	got := records[0]
	if !reflect.DeepEqual(got.Completion.SelectedUpstream, want.Completion.SelectedUpstream) {
		t.Errorf("retained SelectedUpstream = %v, want %v", got.Completion.SelectedUpstream, want.Completion.SelectedUpstream)
	}

	// The error is an immutable sentinel: equality checks its identity too.
	if got.Completion.Failure == nil {
		t.Error("retained Failure = nil, want the first failure")
	} else if *got.Completion.Failure != *want.Completion.Failure {
		t.Errorf("retained Failure = %+v, want %+v", got.Completion.Failure, want.Completion.Failure)
	}

	// The pointer contents were checked above. Compare the other value fields
	// without requiring the copied pointers to have the same addresses.
	got.Completion.SelectedUpstream = want.Completion.SelectedUpstream
	got.Completion.Failure = want.Completion.Failure
	if got != want {
		t.Errorf("retained Record = %+v, want %+v", got, want)
	}

	if records[1].RequestID == want.RequestID {
		t.Errorf("second RequestID = %q, want an ID different from the first request", records[1].RequestID)
	}

	if records[1].Completion.Failure == nil || !errors.Is(records[1].Completion.Failure.Err, secondErr) {
		t.Errorf("second Failure = %+v, want %v", records[1].Completion.Failure, secondErr)
	}
}
