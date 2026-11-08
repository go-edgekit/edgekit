package edgekit

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// countingReader records the bytes that the body wrapper takes from it.
type countingReader struct {
	io.Reader

	read int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n

	return n, err
}

// newLimitedBody wraps a body of size bytes with limit. The body ends after
// the test reads, so the trailer check does not run.
func newLimitedBody(size int, limit int64) (*requestBody, *countingReader) {
	source := &countingReader{
		Reader: strings.NewReader(strings.Repeat("x", size)),
	}
	b := &requestBody{
		ReadCloser: io.NopCloser(source),
		limit:      limit,
	}

	return b, source
}

func TestOverflowStaysFinal(t *testing.T) {
	b, source := newLimitedBody(100, 10)
	buf := make([]byte, 32)

	if _, err := b.Read(buf); !errors.Is(err, ErrRequestBodyTooLarge) {
		t.Fatalf("Read() over the limit: error = %v, want ErrRequestBodyTooLarge", err)
	}

	readBefore := source.read
	n, err := b.Read(buf)

	if n != 0 || !errors.Is(err, ErrRequestBodyTooLarge) {
		t.Errorf("Read() after overflow = %d, %v; want 0, ErrRequestBodyTooLarge", n, err)
	}

	if source.read != readBefore {
		t.Errorf("Read() after the overflow took %d bytes from the body, want 0", source.read-readBefore)
	}

	var maxBytes *http.MaxBytesError
	if !errors.As(err, &maxBytes) || maxBytes.Limit != 10 {
		t.Errorf("Read() after the overflow: error = %v, want a MaxBytesError with the limit 10", err)
	}
}

func TestEndOfBodyNeedsExactEOF(t *testing.T) {
	for _, tc := range []struct {
		name          string
		end           error
		wantTrailer   string
		wantReadError bool
	}{
		{
			name:        "io.EOF publishes the trailers",
			end:         io.EOF,
			wantTrailer: "abc",
		},
		{
			name:          "wrapped io.EOF publishes no trailers",
			end:           fmt.Errorf("upstream stream: %w", io.EOF),
			wantReadError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := http.NewRequest(http.MethodPost, "/", eofReader{
				err: tc.end,
			})
			if err != nil {
				t.Fatal(err)
			}

			// net/http fills the trailer map of its request at the end of
			// the body.
			source.Trailer = http.Header{
				"X-Checksum": {"abc"},
			}
			inner := *source
			inner.Trailer = make(http.Header)
			b := (&Gateway{}).newRequestBody(source, &inner, noBodyLimit)

			data, err := io.ReadAll(b)

			if string(data) != "data" {
				t.Errorf("io.ReadAll() = %q, want %q", data, "data")
			}

			if gotError := err != nil; gotError != tc.wantReadError {
				t.Errorf("io.ReadAll() error = %v, want an error: %t", err, tc.wantReadError)
			}

			if got := inner.Trailer.Get("X-Checksum"); got != tc.wantTrailer {
				t.Errorf("published trailer X-Checksum = %q, want %q", got, tc.wantTrailer)
			}
		})
	}
}

// eofReader returns its data and err in the same call.
type eofReader struct {
	err error
}

func (r eofReader) Read(p []byte) (int, error) {
	return copy(p, "data"), r.err
}

func TestBodyTooLargeError(t *testing.T) {
	err := tooLarge(5)

	if got, want := err.Error(), "request body too large"; got != want {
		t.Errorf("tooLarge(5).Error() = %q, want %q", got, want)
	}

	if !errors.Is(err, ErrRequestBodyTooLarge) {
		t.Error("errors.Is(tooLarge(5), ErrRequestBodyTooLarge) = false, want true")
	}

	var maxBytes *http.MaxBytesError
	if !errors.As(err, &maxBytes) || maxBytes.Limit != 5 {
		t.Errorf("errors.As(tooLarge(5), *http.MaxBytesError) gives %v, want a limit of 5", maxBytes)
	}
}
