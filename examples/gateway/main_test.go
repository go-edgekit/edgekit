package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-edgekit/edgekit"
)

// records decodes the JSON log records in output.
func records(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()

	var result []map[string]any

	decoder := json.NewDecoder(output)
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}

		result = append(result, record)
	}

	return result
}

func TestFailureReportsGiveClassificationOnly(t *testing.T) {
	secretURL := &url.Error{
		Op:  "Get",
		URL: "https://host-secret/path-secret?token=query-secret",
		Err: errors.New("failure-secret"),
	}

	for _, tc := range []struct {
		name string
		err  error
		kind string
	}{
		{
			name: "deadline",
			err:  fmt.Errorf("private-secret: %w", context.DeadlineExceeded),
			kind: "deadline_exceeded",
		},
		{
			name: "invalid configuration",
			err:  fmt.Errorf("%w: route-secret", edgekit.ErrInvalidConfiguration),
			kind: "invalid_configuration",
		},
		{
			name: "network",
			err: &net.OpError{
				Op:  "listen",
				Net: "tcp",
				Err: errors.New("address-secret"),
			},
			kind: "network",
		},
		{
			name: "other",
			err:  secretURL,
			kind: "other",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))

			if got := reportFailure(logger, "server failed", tc.err); !errors.Is(got, tc.err) {
				t.Errorf("reportFailure() = %v, want the original error", got)
			}

			if strings.Contains(output.String(), "secret") {
				t.Errorf("log contains error details: %s", output.String())
			}

			logged := records(t, &output)
			if len(logged) != 1 {
				t.Fatalf("records = %d, want 1", len(logged))
			}

			record := logged[0]
			if record["msg"] != "server failed" || record["error_kind"] != tc.kind || len(record) != 4 {
				t.Errorf("record = %v, want msg, error_kind %q, level, and time only", record, tc.kind)
			}
		})
	}
}

func TestStartupFailureIsReportedSafely(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config
		msg  string
		kind string
	}{
		{
			name: "upstream URL",
			cfg: config{
				listenAddr: "127.0.0.1:0",
				upstream:   "https://host-secret/path-secret?token=query-secret",
			},
			msg:  "upstream URL is not valid",
			kind: "invalid_configuration",
		},
		{
			name: "listen address",
			cfg: config{
				listenAddr: "127.0.0.1:port-secret",
				upstream:   "http://127.0.0.1:9090",
			},
			msg:  "listen failed",
			kind: "network",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))

			if err := run(t.Context(), tc.cfg, logger, nil); err == nil {
				t.Fatal("run() error = nil, want a startup failure")
			}

			if strings.Contains(output.String(), "secret") {
				t.Errorf("log contains configuration values: %s", output.String())
			}

			logged := records(t, &output)
			if len(logged) != 1 || logged[0]["msg"] != tc.msg || logged[0]["error_kind"] != tc.kind {
				t.Errorf("records = %v, want one %q record with error_kind %q", logged, tc.msg, tc.kind)
			}
		})
	}
}

func TestRunShutsDownWhenContextEnds(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))

	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan net.Addr, 1)
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, config{
			listenAddr: "127.0.0.1:0",
			upstream:   "http://127.0.0.1:9090",
		}, logger, ready)
	}()

	select {
	case <-ready:
	case err := <-result:
		t.Fatalf("run() = %v before it listened", err)
	}

	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Errorf("run() error = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run() did not return after the context ended")
	}

	var messages []any
	for _, record := range records(t, &output) {
		messages = append(messages, record["msg"])
	}

	if fmt.Sprint(messages) != "[gateway listening gateway stopped]" {
		t.Errorf("messages = %v, want listening and stopped", messages)
	}
}
