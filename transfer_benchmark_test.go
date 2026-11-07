package edgekit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// zeroReader is an endless body without allocation or I/O.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

func (zeroReader) Close() error {
	return nil
}

// deadlineSink accepts deadlines without recording them, so the benchmark
// measures the transfer bookkeeping only.
type deadlineSink struct {
	*httptest.ResponseRecorder
}

func (deadlineSink) SetReadDeadline(time.Time) error {
	return nil
}

func (deadlineSink) SetWriteDeadline(time.Time) error {
	return nil
}

// BenchmarkTransferBodyRead measures one streaming body read through the
// transfer deadline manager, with and without a transfer idle timeout.
func BenchmarkTransferBodyRead(b *testing.B) {
	for _, tc := range []struct {
		name string
		idle time.Duration
	}{
		{
			name: "idle",
			idle: time.Minute,
		},
		{
			name: "no_idle",
			idle: 0,
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			t := &transfer{
				controller: http.NewResponseController(deadlineSink{
					ResponseRecorder: httptest.NewRecorder(),
				}),
				idle: tc.idle,
			}
			body := &transferBody{
				ReadCloser: zeroReader{},
				transfer:   t,
			}
			p := make([]byte, 512)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := body.Read(p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
