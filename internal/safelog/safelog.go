// Package safelog provides deliberately coarse diagnostic sinks for this repo.
// It is not a general-purpose string redactor or a public logging API.
package safelog

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
)

// New replaces each standard-log write with a source-only structured event.
// source must be a static label, not request or configuration data. Input text
// is neither retained nor parsed: unknown future diagnostics are also omitted.
// These events do not establish a request outcome or an exact failure phase.
func New(source string) *log.Logger {
	return log.New(diagnosticWriter{source: source}, "", 0)
}

type diagnosticWriter struct{ source string }

func (w diagnosticWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	slog.Warn("diagnostic details omitted", "source", w.source)
	return len(p), nil
}

// ConfigureDefault installs the example executables' process-wide log policy.
// The structured sink writes directly to output, not through log.Default, so
// routing standard logs back to it cannot recurse. Libraries must not call this.
// Configure it once at startup, before serving or starting background work.
func ConfigureDefault(output io.Writer) {
	slog.SetDefault(slog.New(slog.NewJSONHandler(output, nil)))
	log.SetOutput(New("stdlib").Writer())
	log.SetFlags(0)
	log.SetPrefix("")
}

// ErrorKind returns an allowlisted diagnostic interpretation without formatting
// the error. It is not core failure provenance or proof of a failure's cause.
func ErrorKind(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return "other"
}
