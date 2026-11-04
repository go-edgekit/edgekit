package edgekit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// upgradeTestBody records each close without requiring idempotent behavior.
type upgradeTestBody struct {
	closes atomic.Int32
	err    error
}

// Read ends the test stream.
func (*upgradeTestBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

// Write accepts the test stream.
func (*upgradeTestBody) Write(b []byte) (int, error) {
	return len(b), nil
}

// Close records each call and returns the configured error.
func (b *upgradeTestBody) Close() error {
	b.closes.Add(1)

	return b.err
}

// halfCloseUpgradeBody records a half-close separately from a full close.
type halfCloseUpgradeBody struct {
	*upgradeTestBody

	writeClosed bool
}

// CloseWrite records the half-close and returns the configured error.
func (b *halfCloseUpgradeBody) CloseWrite() error {
	b.writeClosed = true

	return b.err
}

// upgradeTestResponse provides the gateway context for an accepted 101 body.
func upgradeTestResponse(ctx context.Context, body io.ReadWriteCloser) (*RequestContext, *http.Response) {
	rc := &RequestContext{}
	r := httptest.NewRequest(http.MethodGet, "/api/echo", nil)
	r = r.WithContext(context.WithValue(ctx, requestContextKey{}, rc))
	resp := &http.Response{
		StatusCode: http.StatusSwitchingProtocols,
		Header: http.Header{
			"Connection": {"Upgrade"},
			"Upgrade":    {"echo"},
		},
		Body:    body,
		Request: r,
	}

	return rc, resp
}

func TestUpgradeBodyClosesOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "successful close",
		},
		{
			name: "failed close",
			err:  errors.New("close failed"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &upgradeTestBody{
				err: tc.err,
			}
			rc, resp := upgradeTestResponse(t.Context(), body)
			defer func() { _ = resp.Body.Close() }()

			g := &Gateway{}
			if err := g.modifyResponse(resp); err != nil {
				t.Fatalf("modifyResponse(101) error = %v, want nil", err)
			}

			// The gateway and ReverseProxy can close the same body from
			// different goroutines during a failed protocol switch.
			const attempts = 8
			errs := make(chan error, 2*attempts)
			var wg sync.WaitGroup
			for _, closer := range []io.Closer{rc.upgradeBody, resp.Body} {
				for range attempts {
					wg.Go(func() {
						errs <- closer.Close()
					})
				}
			}
			wg.Wait()
			close(errs)

			if got := body.closes.Load(); got != 1 {
				t.Errorf("modifyResponse(101) body Close() calls = %d, want 1", got)
			}

			for err := range errs {
				if !errors.Is(err, tc.err) {
					t.Errorf("body.Close() error = %v, want the original error %v", err, tc.err)
				}
			}
		})
	}
}

func TestUpgradeBodyPreservesHalfClose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		halfClose bool
	}{
		{
			name: "without half-close",
		},
		{
			name:      "with half-close",
			halfClose: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &halfCloseUpgradeBody{
				upgradeTestBody: &upgradeTestBody{
					err: errors.New("half-close failed"),
				},
			}
			var body io.ReadWriteCloser = original.upgradeTestBody
			if tc.halfClose {
				body = original
			}

			_, resp := upgradeTestResponse(t.Context(), body)
			defer func() { _ = resp.Body.Close() }()

			g := &Gateway{}
			if err := g.modifyResponse(resp); err != nil {
				t.Fatalf("modifyResponse(101) error = %v, want nil", err)
			}

			closer, ok := resp.Body.(interface{ CloseWrite() error })
			if ok != tc.halfClose {
				t.Fatalf("modifyResponse(101) body supports CloseWrite() = %v, want %v", ok, tc.halfClose)
			}

			if ok {
				if err := closer.CloseWrite(); !errors.Is(err, original.err) {
					t.Errorf("body.CloseWrite() error = %v, want the original error %v", err, original.err)
				}
			}

			if original.writeClosed != tc.halfClose {
				t.Errorf("body.CloseWrite() reached the original body = %v, want %v", original.writeClosed, tc.halfClose)
			}

			if got := original.closes.Load(); got != 0 {
				t.Errorf("body.CloseWrite() body Close() calls = %d, want 0", got)
			}
		})
	}
}
