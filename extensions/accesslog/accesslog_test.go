package accesslog_test

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gateway "example.com/go-gateway-starter"
	"example.com/go-gateway-starter/extensions/accesslog"
)

type requestHook func(context.Context, *gateway.RequestContext, *http.Request) error

func (f requestHook) OnRequest(ctx context.Context, rc *gateway.RequestContext, r *http.Request) error {
	return f(ctx, rc, r)
}

type responseHook func(context.Context, *gateway.RequestContext, *http.Response) error

func (f responseHook) OnUpstreamResponse(ctx context.Context, rc *gateway.RequestContext, r *http.Response) error {
	return f(ctx, rc, r)
}

type completionHook func(context.Context, *gateway.RequestContext, gateway.Outcome)

func (f completionHook) OnComplete(ctx context.Context, rc *gateway.RequestContext, outcome gateway.Outcome) {
	f(ctx, rc, outcome)
}

func route(t *testing.T, address string) gateway.Route {
	t.Helper()
	target, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	return gateway.Route{Prefix: "/api/", Upstream: target}
}

func newGateway(t *testing.T, config gateway.Config) *gateway.Gateway {
	t.Helper()
	app, err := gateway.NewWithConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func checkRequestID(t *testing.T, id string) {
	t.Helper()
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("invalid generated request ID %q", id)
	}
}

func TestRecordJoinsHTTPGatewayAndTraceObservations(t *testing.T) {
	upstreamID := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamID <- r.Header.Get("X-Request-ID")
		w.Header().Set("X-Request-ID", "upstream-supplied")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	// This small tracer models an SDK-owned span in a derived standard context.
	// Completion runs after that middleware ends the span, using the entry context.
	type traceKey struct{}
	type span struct {
		id    string
		ended bool
	}
	current := &span{id: "trace-from-middleware"}
	tracing := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() { current.ended = true }()
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceKey{}, current)))
		})
	}
	var records []accesslog.Record
	observer := accesslog.New(accesslog.Config{
		TraceID: func(ctx context.Context) string {
			if current.ended {
				t.Fatal("trace ID should be captured while the span is active")
			}
			return ctx.Value(traceKey{}).(*span).id
		},
		Report: func(record accesslog.Record) {
			if !current.ended {
				t.Fatal("report ran before tracing middleware unwound")
			}
			records = append(records, record)
		},
	})
	checkContext := requestHook(func(ctx context.Context, _ *gateway.RequestContext, r *http.Request) error {
		id, ok := accesslog.RequestIDFromContext(ctx)
		if !ok || id != r.Header.Get("X-Request-ID") || ctx.Value(traceKey{}) != current {
			t.Fatal("request ID or middleware trace context missing in hook")
		}
		return nil
	})
	checkCompletion := completionHook(func(ctx context.Context, _ *gateway.RequestContext, _ gateway.Outcome) {
		if ctx.Value(traceKey{}) != nil {
			t.Fatal("completion unexpectedly received the derived trace context")
		}
		if _, ok := accesslog.RequestIDFromContext(ctx); ok {
			t.Fatal("completion entry context unexpectedly contains the middleware ID")
		}
	})
	configured := route(t, upstream.URL)
	configured.Upstream.User = url.UserPassword("service", "upstream-secret")
	configured.Upstream.RawQuery = "token=upstream-query-secret"
	app := newGateway(t, gateway.Config{
		Routes: []gateway.Route{configured}, Middlewares: []gateway.Middleware{tracing, observer.Middleware},
		Extensions: []any{observer, checkContext, checkCompletion},
	})
	r := httptest.NewRequest(http.MethodPost, "/api/data?token=request-query-secret", strings.NewReader("request-body-secret"))
	r.Header.Set("X-Request-ID", "untrusted-client-id")
	r.Header.Set("Authorization", "Bearer authorization-secret")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	record := records[0]
	checkRequestID(t, record.RequestID)
	if record.RequestID == "untrusted-client-id" || <-upstreamID != record.RequestID {
		t.Fatal("request ID was not generated and propagated to the upstream")
	}
	if values := w.Header().Values("X-Request-ID"); len(values) != 1 || values[0] != record.RequestID {
		t.Fatalf("upstream replaced or duplicated the response ID: %v", values)
	}
	if record.TraceID != current.id || record.Method != http.MethodPost || record.Path != "/api/data" || record.RoutePrefix != "/api/" {
		t.Fatalf("missing request correlation: %+v", record)
	}
	if record.Upstream != upstream.URL || record.UpstreamStatusCode != 201 || record.StatusCode != w.Code || record.BytesWritten != 2 || record.Result != accesslog.UpstreamResponse {
		t.Fatalf("incorrect HTTP or upstream observations: %+v", record)
	}
	if record.Outcome.Duration <= 0 || record.Outcome.Err != nil || record.Outcome.Aborted || record.ResponseError != nil || record.Hijacked {
		t.Fatalf("unexpected execution outcome: %+v", record)
	}
	text := fmt.Sprintf("%+v", record)
	for _, secret := range []string{"upstream-secret", "upstream-query-secret", "request-query-secret", "request-body-secret", "authorization-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("record contains sensitive input %q", secret)
		}
	}
}

func TestLocalResponsesAndGatewayFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "unavailable")
	}))
	defer upstream.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	denied := &gateway.HTTPError{StatusCode: http.StatusForbidden, Message: "denied"}
	internal := errors.New("private failure details")

	for _, tc := range []struct {
		name          string
		path          string
		address       string
		localStatus   int
		routeDeny     bool
		requestError  error
		responseError error
		wantStatus    int
		wantUpstream  bool
		wantRoute     bool
		wantResponse  int
		wantResult    accesslog.Result
	}{
		{name: "middleware rejection", localStatus: 401, wantStatus: 401, wantResult: accesslog.Rejected},
		{name: "route rejection", routeDeny: true, wantStatus: 403, wantRoute: true, wantResult: accesslog.Rejected},
		{name: "request hook rejection", requestError: denied, wantStatus: 403, wantResult: accesslog.Rejected},
		{name: "request hook error", requestError: internal, wantStatus: 500, wantResult: accesslog.GatewayError},
		{name: "routing miss", path: "/missing", wantStatus: 404, wantResult: accesslog.Rejected},
		{name: "empty local response", localStatus: -1, wantStatus: 200, wantResult: accesslog.LocalResponse},
		{name: "upstream HTTP failure", wantStatus: 503, wantRoute: true, wantUpstream: true, wantResponse: 503, wantResult: accesslog.UpstreamResponse},
		{name: "proxy connection failure", address: closed.URL, wantStatus: 502, wantRoute: true, wantUpstream: true, wantResult: accesslog.ProxyError},
		{name: "response hook rejection", responseError: denied, wantStatus: 403, wantRoute: true, wantUpstream: true, wantResponse: 503, wantResult: accesslog.Rejected},
		{name: "response hook error", responseError: internal, wantStatus: 502, wantRoute: true, wantUpstream: true, wantResponse: 503, wantResult: accesslog.ProxyError},
		{name: "invalid rejection", requestError: &gateway.HTTPError{StatusCode: 200, Message: "private"}, wantStatus: 500, wantResult: accesslog.GatewayError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var records []accesslog.Record
			observer := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { records = append(records, record) }})
			middleware := []gateway.Middleware{observer.Middleware}
			if tc.localStatus != 0 {
				middleware = append(middleware, func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if tc.localStatus > 0 {
							http.Error(w, "local response", tc.localStatus)
						}
					})
				})
			}
			extensions := []any{observer}
			if tc.requestError != nil {
				extensions = append(extensions, requestHook(func(context.Context, *gateway.RequestContext, *http.Request) error { return tc.requestError }))
			}
			if tc.responseError != nil {
				extensions = append(extensions, responseHook(func(context.Context, *gateway.RequestContext, *http.Response) error { return tc.responseError }))
			}
			address := tc.address
			if address == "" {
				address = upstream.URL
			}
			configured := route(t, address)
			if tc.routeDeny {
				configured.Middlewares = []gateway.Middleware{func(http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", 403) })
				}}
			}
			app := newGateway(t, gateway.Config{Routes: []gateway.Route{configured}, Middlewares: middleware, Extensions: extensions})
			path := tc.path
			if path == "" {
				path = "/api/data"
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			if len(records) != 1 {
				t.Fatalf("got %d completion records", len(records))
			}
			record := records[0]
			checkRequestID(t, record.RequestID)
			if record.StatusCode != tc.wantStatus || record.StatusCode != w.Code || record.Result != tc.wantResult || record.BytesWritten != int64(w.Body.Len()) {
				t.Fatalf("incorrect classification or accounting: %+v", record)
			}
			if (record.Upstream != "") != tc.wantUpstream || (record.RoutePrefix != "") != tc.wantRoute || record.UpstreamStatusCode != tc.wantResponse {
				t.Fatalf("incorrect gateway metadata: %+v", record)
			}
			if record.Outcome.Aborted || record.ResponseError != nil {
				t.Fatalf("unexpected abort or writer error: %+v", record)
			}
			cause := tc.requestError
			if cause == nil {
				cause = tc.responseError
			}
			if cause != nil && !errors.Is(record.Outcome.Err, cause) {
				t.Fatalf("lost hook error: %v", record.Outcome.Err)
			}
			if tc.address == closed.URL && record.Outcome.Err == nil {
				t.Fatal("proxy failure lost its error")
			}
			if cause == nil && tc.address != closed.URL && record.Outcome.Err != nil {
				t.Fatalf("HTTP status was mistaken for an execution error: %v", record.Outcome.Err)
			}
		})
	}
}

func TestPanicAccountingDoesNotInventSuccessfulStatus(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			var records []accesslog.Record
			observer := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { records = append(records, record) }})
			panicking := func(http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if committed {
						w.WriteHeader(http.StatusAccepted)
						_, _ = io.WriteString(w, "partial")
					}
					panic(http.ErrAbortHandler)
				})
			}
			app := newGateway(t, gateway.Config{
				Routes:      []gateway.Route{route(t, "http://unused.invalid")},
				Middlewares: []gateway.Middleware{observer.Middleware, panicking}, Extensions: []any{observer},
			})
			func() {
				defer func() {
					if recovered := recover(); recovered != http.ErrAbortHandler {
						t.Fatalf("panic changed or was swallowed: %v", recovered)
					}
				}()
				app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/data", nil))
			}()
			if len(records) != 1 || !records[0].Outcome.Aborted || records[0].Result != accesslog.Aborted {
				t.Fatalf("missing abort record: %+v", records)
			}
			wantStatus, wantBytes := 0, int64(0)
			if committed {
				wantStatus, wantBytes = http.StatusAccepted, int64(len("partial"))
			}
			if records[0].StatusCode != wantStatus || records[0].BytesWritten != wantBytes {
				t.Fatalf("incorrect accounting on panic: %+v", records[0])
			}
		})
	}
}

func TestStreamingReadFailureKeepsPartialHTTPObservations(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "short")
	}))
	defer upstream.Close()
	var records []accesslog.Record
	observer := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { records = append(records, record) }})
	app := newGateway(t, gateway.Config{
		Routes: []gateway.Route{route(t, upstream.URL)}, Middlewares: []gateway.Middleware{observer.Middleware}, Extensions: []any{observer},
	})
	r := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	// Enable ReverseProxy's production behavior for response-copy failures.
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{}))
	func() {
		defer func() {
			if recovered := recover(); recovered != http.ErrAbortHandler {
				t.Fatalf("expected streaming abort, got %v", recovered)
			}
		}()
		app.ServeHTTP(httptest.NewRecorder(), r)
	}()
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	record := records[0]
	if record.StatusCode != 200 || record.BytesWritten != 5 || record.UpstreamStatusCode != 200 || record.Result != accesslog.Aborted || !record.Outcome.Aborted {
		t.Fatalf("partial stream observations lost: %+v", record)
	}
	if record.Outcome.Err != nil || record.ResponseError != nil {
		t.Fatal("upstream body-copy failure should not be invented as a proxy callback or downstream write error")
	}
}

type failingWriter struct{ header http.Header }

func (w *failingWriter) Header() http.Header { return w.header }
func (w *failingWriter) WriteHeader(int)     {}
func (w *failingWriter) Write([]byte) (int, error) {
	return 2, io.ErrClosedPipe
}

func TestPartialWriteErrorIsSeparateFromGatewayError(t *testing.T) {
	var got accesslog.Record
	observer := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { got = record }})
	local := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "hello") })
	}
	app := newGateway(t, gateway.Config{
		Routes:      []gateway.Route{route(t, "http://unused.invalid")},
		Middlewares: []gateway.Middleware{observer.Middleware, local}, Extensions: []any{observer},
	})
	app.ServeHTTP(&failingWriter{header: make(http.Header)}, httptest.NewRequest(http.MethodGet, "/api/data", nil))
	if got.StatusCode != 200 || got.BytesWritten != 2 || !errors.Is(got.ResponseError, io.ErrClosedPipe) || got.Result != accesslog.WriteError || got.Outcome.Err != nil {
		t.Fatalf("incorrect write-failure accounting: %+v", got)
	}
}

func waitRecord(t *testing.T, records <-chan accesslog.Record) accesslog.Record {
	t.Helper()
	select {
	case record := <-records:
		return record
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for completion")
		return accesslog.Record{}
	}
}

func TestStreamingResponseFlushesBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer upstream.Close()
	defer finish()
	records := make(chan accesslog.Record, 1)
	observer := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { records <- record }})
	app := newGateway(t, gateway.Config{
		Routes: []gateway.Route{route(t, upstream.URL)}, Middlewares: []gateway.Middleware{observer.Middleware}, Extensions: []any{observer},
	})
	server := httptest.NewServer(app)
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("stream did not flush the first event: %q, %v", line, err)
	}
	select {
	case <-records:
		t.Fatal("completion ran while the stream was still open")
	default:
	}
	finish()
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	record := waitRecord(t, records)
	if record.StatusCode != 200 || record.BytesWritten != int64(len("data: first\n\ndata: last\n\n")) || record.Result != accesslog.UpstreamResponse {
		t.Fatalf("incorrect streaming record: %+v", record)
	}
}

func TestProtocolUpgradePreservesHijacking(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(buffer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: example\r\nX-Request-ID: upstream-id\r\n\r\n")
		if err := buffer.Flush(); err != nil {
			t.Error(err)
			return
		}
		message := make([]byte, 4)
		if _, err := io.ReadFull(buffer, message); err != nil {
			t.Error(err)
			return
		}
		_, _ = buffer.Write(message)
		_ = buffer.Flush()
	}))
	defer upstream.Close()
	records := make(chan accesslog.Record, 1)
	observer := accesslog.New(accesslog.Config{Report: func(record accesslog.Record) { records <- record }})
	app := newGateway(t, gateway.Config{
		Routes: []gateway.Route{route(t, upstream.URL)}, Middlewares: []gateway.Middleware{observer.Middleware}, Extensions: []any{observer},
	})
	server := httptest.NewServer(app)
	defer server.Close()
	target, _ := url.Parse(server.URL)
	conn, err := net.DialTimeout("tcp", target.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /api/socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: example\r\n\r\n", target.Host)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusEarlyHints {
		t.Fatalf("informational response was not forwarded: %s", response.Status)
	}
	response, err = http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 {
		t.Fatalf("upgrade failed: %s", response.Status)
	}
	checkRequestID(t, response.Header.Get("X-Request-ID"))
	if len(response.Header.Values("X-Request-ID")) != 1 {
		t.Fatal("upstream duplicated the request ID during the upgrade")
	}
	_, _ = io.WriteString(conn, "ping")
	message := make([]byte, 4)
	if _, err := io.ReadFull(reader, message); err != nil || string(message) != "ping" {
		t.Fatalf("upgraded stream failed: %q, %v", message, err)
	}
	_ = conn.Close()
	record := waitRecord(t, records)
	// ReverseProxy writes the upgrade handshake directly to the hijacked stream.
	// It does not call ResponseWriter.WriteHeader or Write for those bytes.
	if !record.Hijacked || record.StatusCode != 0 || record.BytesWritten != 0 || record.UpstreamStatusCode != 101 || record.Result != accesslog.UpstreamResponse {
		t.Fatalf("incorrect upgrade accounting: %+v", record)
	}
}

func TestConcurrentRecordsKeepRequestAndTraceIDsIsolated(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer upstream.Close()
	type traceKey struct{}
	tracing := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), traceKey{}, "trace:"+r.URL.Path)))
		})
	}
	var mu sync.Mutex
	var records []accesslog.Record
	observer := accesslog.New(accesslog.Config{
		TraceID: func(ctx context.Context) string { return ctx.Value(traceKey{}).(string) },
		Report: func(record accesslog.Record) {
			mu.Lock()
			defer mu.Unlock()
			records = append(records, record)
		},
	})
	app := newGateway(t, gateway.Config{
		Routes: []gateway.Route{route(t, upstream.URL)}, Middlewares: []gateway.Middleware{tracing, observer.Middleware}, Extensions: []any{observer},
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/%d", i), nil))
		}()
	}
	wg.Wait()
	if len(records) != 20 {
		t.Fatalf("got %d records", len(records))
	}
	seen := make(map[string]bool)
	for _, record := range records {
		checkRequestID(t, record.RequestID)
		if seen[record.RequestID] || record.TraceID != "trace:"+record.Path || record.BytesWritten != 2 || record.StatusCode != 200 {
			t.Fatalf("request state leaked or accounting failed: %+v", record)
		}
		seen[record.RequestID] = true
	}
}

func TestMissingMiddlewareAndStandaloneUse(t *testing.T) {
	calls := 0
	observer := accesslog.New(accesslog.Config{Report: func(accesslog.Record) { calls++ }})
	app := newGateway(t, gateway.Config{Routes: []gateway.Route{route(t, "http://unused.invalid")}, Extensions: []any{observer}})
	app.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	standalone := observer.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	standalone.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if calls != 0 || w.Code != 204 || w.Header().Get("X-Request-ID") != "" {
		t.Fatal("extension should skip requests that have no middleware state")
	}
	if _, ok := accesslog.RequestIDFromContext(context.Background()); ok {
		t.Fatal("request ID leaked outside the middleware context")
	}
}
