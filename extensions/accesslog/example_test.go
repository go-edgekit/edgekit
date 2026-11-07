package accesslog_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"

	"github.com/go-edgekit/edgekit"
	"github.com/go-edgekit/edgekit/extensions/accesslog"
)

func ExampleNew() {
	// The upstream is a test server here. In a deployment, it is an API.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "created")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(target)
	if err != nil {
		fmt.Println(err)

		return
	}

	// The sink gets one record for each request. It must not block.
	recorder, err := accesslog.New(accesslog.Config{
		Sink: accesslog.SinkFunc(func(r accesslog.Record) {
			fmt.Println("status:", r.Status, "upstream status:", r.UpstreamStatus, "bytes:", r.BytesWritten)
			fmt.Printf("route matched: %t, reason: %q\n", r.Completion.RoutePrefix != "", r.Completion.RejectionReason)
		}),
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	// The recorder is the first middleware and the first hook provider.
	gateway, err := edgekit.New(edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	for _, path := range []string{"/api/items", "/admin"} {
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Println("request ID sent:", len(w.Header().Get(accesslog.RequestIDHeader)) == 32)
	}

	// Output:
	// status: 201 upstream status: 201 bytes: 7
	// route matched: true, reason: ""
	// request ID sent: true
	// status: 404 upstream status: 0 bytes: 75
	// route matched: false, reason: "no_route"
	// request ID sent: true
}

func ExampleRecord_LogValue() {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	recorder, err := accesslog.New(accesslog.Config{
		// LogValue writes only the fields that are safe by default, not the
		// path, the method, the trace ID, or errors.
		Sink: accesslog.SinkFunc(func(r accesslog.Record) {
			logger.Info("request", slog.Any("record", r))
		}),
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	selector, err := edgekit.StaticUpstream(&url.URL{
		Scheme: "http",
		Host:   "upstream.test",
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	defer gateway.CloseIdleConnections()

	gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/secret-path?token=secret", nil))

	var logged struct {
		Record map[string]any `json:"record"`
	}
	if err := json.Unmarshal(output.Bytes(), &logged); err != nil {
		fmt.Println(err)

		return
	}

	fmt.Println("status:", logged.Record["status"])
	fmt.Println("rejection reason:", logged.Record["rejection_reason"])
	_, pathLogged := logged.Record["path"]
	fmt.Println("path logged:", pathLogged)

	// Output:
	// status: 404
	// rejection reason: no_route
	// path logged: false
}

// queueSink gives records to a separate consumer through a bounded queue.
// Report never blocks: it drops the record when the queue is full.
type queueSink struct {
	queue   chan accesslog.Record
	dropped atomic.Int64
}

// Report queues r, or counts a drop when the queue is full.
func (s *queueSink) Report(r accesslog.Record) {
	select {
	case s.queue <- r:
	default:
		s.dropped.Add(1)
	}
}

func ExampleSink() {
	selector, err := edgekit.StaticUpstream(&url.URL{
		Scheme: "http",
		Host:   "upstream.test",
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	// The queue holds one record, so the second record is dropped. In a
	// deployment, the queue is larger and the consumer runs all the time.
	sink := &queueSink{
		queue: make(chan accesslog.Record, 1),
	}
	recorder, err := accesslog.New(accesslog.Config{
		Sink: sink,
	})
	if err != nil {
		fmt.Println(err)

		return
	}

	gateway, err := edgekit.New(edgekit.Config{
		Middleware: []edgekit.Middleware{
			recorder.Middleware,
		},
		Hooks: []any{
			recorder,
		},
		Routes: []edgekit.Route{{
			Prefix:   "/api/",
			Upstream: selector,
		}},
	})
	if err != nil {
		fmt.Println(err)

		return
	}
	defer gateway.CloseIdleConnections()

	for _, path := range []string{"/first", "/second"} {
		gateway.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	// The consumer does the slow work, such as an export, outside the
	// request. It ends when the queue is closed and empty.
	done := make(chan struct{})
	go func() {
		defer close(done)

		for record := range sink.queue {
			fmt.Println("exported:", record.Status, record.Completion.RejectionReason)
		}
	}()

	close(sink.queue)
	<-done
	fmt.Println("dropped:", sink.dropped.Load())

	// Output:
	// exported: 404 no_route
	// dropped: 1
}
