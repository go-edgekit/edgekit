package safelog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous, output, flags, prefix := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	var buffer bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buffer, nil)))
	return &buffer
}

func TestDiagnosticTextIsNeverForwarded(t *testing.T) {
	output := captureLogs(t)
	logger := New("test_source")
	input := []byte("panic from https://user:password-secret@host/path-secret?token=query-secret\nAuthorization: Bearer header-secret\nstack-secret\xff")
	n, err := logger.Writer().Write(input)
	if n != len(input) || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "diagnostic details omitted" || record["source"] != "test_source" || record["level"] != "WARN" || len(record) != 4 {
		t.Fatalf("unexpected diagnostic fields: %v", record)
	}
	for _, secret := range []string{"password-secret", "path-secret", "query-secret", "header-secret", "stack-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("diagnostic exposed %q", secret)
		}
	}
}

func TestEmptyWritesDoNotInventDiagnostics(t *testing.T) {
	output := captureLogs(t)
	if n, err := New("test_source").Writer().Write(nil); n != 0 || err != nil || output.Len() != 0 {
		t.Fatalf("empty write = %d, %v, output=%q", n, err, output.String())
	}
}

func TestConcurrentDiagnostics(t *testing.T) {
	output := captureLogs(t)
	logger := New("test_source")
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Print("secret")
		}()
	}
	wg.Wait()
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	count := 0
	for {
		var record map[string]any
		if err := decoder.Decode(&record); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record["source"] != "test_source" || record["msg"] != "diagnostic details omitted" {
			t.Fatalf("unexpected event: %v", record)
		}
		count++
	}
	if count != 32 || strings.Contains(output.String(), "secret") {
		t.Fatalf("diagnostic count=%d, output=%s", count, output.String())
	}
}

func TestExecutablePolicyCoversGlobalStandardLogsWithoutRecursion(t *testing.T) {
	output := captureLogs(t)
	ConfigureDefault(output)
	log.Printf("Unsolicited response starting with %q: %s", "body-secret", "error-secret")
	New("http_server").Print("panic-secret\nstack-secret")
	slog.Info("application event", "status", 502)
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for _, source := range []string{"stdlib", "http_server", ""} {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		if source != "" && record["source"] != source {
			t.Fatalf("unexpected diagnostic: %v", record)
		}
		if source == "" && (record["msg"] != "application event" || record["status"] != float64(502)) {
			t.Fatalf("structured logging was intercepted: %v", record)
		}
	}
	if strings.Contains(output.String(), "secret") {
		t.Fatalf("standard logger leaked input: %s", output.String())
	}
}

type unprintableError struct{ timeout bool }

func (e unprintableError) Error() string   { panic("must not format errors") }
func (e unprintableError) Timeout() bool   { return e.timeout }
func (e unprintableError) Temporary() bool { return false }

func TestErrorKindDoesNotFormatErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		kind string
	}{
		{nil, "none"},
		{context.Canceled, "canceled"},
		{fmt.Errorf("secret: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{&url.Error{Op: "GET", URL: "https://user:secret@host/path?token=secret", Err: context.Canceled}, "canceled"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "secret", IsTimeout: true}}, "timeout"},
		{errors.New("secret"), "other"},
		{unprintableError{}, "other"},
		{unprintableError{timeout: true}, "timeout"},
	} {
		if got := ErrorKind(tc.err); got != tc.kind {
			t.Errorf("kind=%s, want %s", got, tc.kind)
		}
	}
}
