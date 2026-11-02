package admission_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
	"github.com/go-edgekit/edgekit/extensions/admission"
)

type completionFunc func(*edgekit.RequestContext, edgekit.Outcome)

func (f completionFunc) OnComplete(_ context.Context, rc *edgekit.RequestContext, outcome edgekit.Outcome) {
	f(rc, outcome)
}

type requestFunc func(*edgekit.RequestContext, *http.Request) error

func (f requestFunc) OnRequest(_ context.Context, rc *edgekit.RequestContext, r *http.Request) error {
	return f(rc, r)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGatewaySaturationIsLocalAndCompletionRunsAfterUnwind(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	unwoundKey := edgekit.NewStateKey[bool]()
	type observed struct {
		outcome                  edgekit.Outcome
		route, selected, unwound bool
	}
	completed := make(chan observed, 2)
	records := make(chan accesslog.Record, 2)
	access := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { records <- record }})
	var requests atomic.Int32
	target, _ := url.Parse("http://upstream.test")
	app, err := edgekit.NewWithConfig(edgekit.Config{
		Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}},
		Middlewares: []edgekit.Middleware{access.Middleware, func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rc, _ := edgekit.FromContext(r.Context())
				defer edgekit.SetState(rc, unwoundKey, true)
				next.ServeHTTP(w, r)
			})
		}, admission.New(1)},
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			close(entered)
			<-release
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
		}),
		Extensions: []any{
			access, requestFunc(func(*edgekit.RequestContext, *http.Request) error { requests.Add(1); return nil }),
			completionFunc(func(rc *edgekit.RequestContext, outcome edgekit.Outcome) {
				_, route := rc.RoutePrefix()
				_, selected := rc.Upstream()
				unwound, _ := edgekit.GetState(rc, unwoundKey)
				completed <- observed{outcome: outcome, route: route, selected: selected, unwound: unwound}
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := serve(app, httptest.NewRequest(http.MethodGet, "/api/active", nil))
	wait(t, entered)
	body := &unreadBody{}
	w := wait(t, serve(app, httptest.NewRequest(http.MethodPost, "/api/overflow", body)))
	result, record := wait(t, completed), wait(t, records)
	if w.Code != 503 || w.Body.String() != "Service Unavailable\n" || body.reads != 0 || requests.Load() != 1 {
		t.Fatalf("overflow delegated or response changed: status=%d reads=%d request hooks=%d", w.Code, body.reads, requests.Load())
	}
	if !result.unwound || result.route || result.selected || result.outcome.Err != nil || result.outcome.Aborted || record.StatusCode != 503 || record.Result != accesslog.LocalResponse || record.RequestID == "" {
		t.Fatalf("local response invented proxy facts or lost completion/accounting: outcome=%+v record=%+v", result, record)
	}
	finish()
	if w := wait(t, first); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("active request changed: %d %q", w.Code, w.Body.String())
	}
	result, record = wait(t, completed), wait(t, records)
	if !result.unwound || !result.route || !result.selected || result.outcome.Err != nil || result.outcome.Aborted || record.Result != accesslog.UpstreamResponse || len(completed) != 0 || len(records) != 0 {
		t.Fatalf("active request lost facts or completed twice: outcome=%+v record=%+v", result, record)
	}
}

func TestHTTP2StreamsShareBudgetAndKeepSlotsWhileStreaming(t *testing.T) {
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	var connections atomic.Int32
	handler := admission.New(2)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	defer finish()
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.Transport.(*http.Transport).MaxConnsPerHost = 1
	var streams []*http.Response
	for range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.ProtoMajor != 2 || response.StatusCode != 200 {
			t.Fatalf("unexpected stream: %s", response.Status)
		}
		// Observe actual bytes before admitting the next stream.
		first := make([]byte, len("data: first\n\n"))
		if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "data: first\n\n" {
			t.Fatalf("first event was buffered: %q, %v", first, err)
		}
		wait(t, entered)
		streams = append(streams, response)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 503 || response.ProtoMajor != 2 || err != nil || string(body) != "Service Unavailable\n" || connections.Load() != 1 {
		t.Fatalf("stream cap bypassed: status=%d protocol=%s body=%q connections=%d err=%v", response.StatusCode, response.Proto, body, connections.Load(), err)
	}
	finish()
	for _, stream := range streams {
		if rest, err := io.ReadAll(stream.Body); err != nil || string(rest) != "data: last\n\n" {
			t.Fatalf("stream changed: %q, %v", rest, err)
		}
	}
}

func TestCancellationDuringPoolAcquisitionReleasesAdmissionSlot(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxConnsPerHost = 1
	defer transport.CloseIdleConnections()
	target, _ := url.Parse(upstream.URL)
	completed := make(chan edgekit.Outcome, 4)
	app, err := edgekit.NewWithConfig(edgekit.Config{
		Routes: []edgekit.Route{{Prefix: "/api/", Upstream: target}}, Transport: transport,
		Middlewares: []edgekit.Middleware{admission.New(2)},
		Extensions:  []any{completionFunc(func(_ *edgekit.RequestContext, outcome edgekit.Outcome) { completed <- outcome })},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	first := serve(app, httptest.NewRequest(http.MethodGet, "/api/active", nil).WithContext(ctx))
	wait(t, entered)
	for range 2 {
		acquiring := make(chan struct{}, 1)
		queuedCtx, cancel := context.WithCancel(ctx)
		queuedCtx = httptrace.WithClientTrace(queuedCtx, &httptrace.ClientTrace{GetConn: func(string) {
			select {
			case acquiring <- struct{}{}:
			default:
			}
		}})
		pending := serve(app, httptest.NewRequest(http.MethodGet, "/api/queued", nil).WithContext(queuedCtx))
		wait(t, acquiring)
		if w := wait(t, serve(app, httptest.NewRequest(http.MethodGet, "/api/overflow", nil))); w.Code != 503 {
			cancel()
			t.Fatal("pool wait did not hold an admission slot")
		}
		if outcome := wait(t, completed); outcome.Err != nil || outcome.Aborted {
			cancel()
			t.Fatalf("overflow invented a gateway error: %+v", outcome)
		}
		cancel()
		if w := wait(t, pending); w.Code != 502 || w.Body.String() != "Bad Gateway\n" {
			t.Fatalf("pool cancellation response=%d %q", w.Code, w.Body.String())
		}
		if outcome := wait(t, completed); !errors.Is(outcome.Err, context.Canceled) || outcome.Aborted {
			t.Fatalf("pool cancellation outcome=%+v", outcome)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("queued/overflow requests reached upstream: calls=%d", calls.Load())
	}
	finish()
	if w := wait(t, first); w.Code != 200 || w.Body.String() != "ok" {
		t.Fatalf("active response changed: %d %q", w.Code, w.Body.String())
	}
	if outcome := wait(t, completed); outcome.Err != nil || outcome.Aborted || len(completed) != 0 {
		t.Fatalf("active completion changed: %+v", outcome)
	}
}
