package edgekit

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type outboundHookFunc func(context.Context, *RequestContext, *http.Request) error

func (f outboundHookFunc) OnBeforeUpstream(ctx context.Context, rc *RequestContext, r *http.Request) error {
	return f(ctx, rc, r)
}

type outboundRoundTripFunc func(*http.Request) (*http.Response, error)

func (f outboundRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type outboundBody struct {
	io.Reader
	closes int
}

func (b *outboundBody) Close() error {
	b.closes++
	return nil
}

func TestOutboundTransportClonesMetadataAndPassesTheHookRequest(t *testing.T) {
	rc := &RequestContext{}
	ctx := context.WithValue(context.Background(), requestContextKey{}, rc)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.example/base/api/x?q=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := &outboundBody{Reader: strings.NewReader("payload")}
	r.Body = body
	r.Header.Set("X-Test", "original")
	var hookRequest *http.Request
	transport := &outboundTransport{
		hooks: []BeforeUpstreamHook{outboundHookFunc(func(gotCtx context.Context, gotRC *RequestContext, out *http.Request) error {
			if gotCtx != ctx || gotRC != rc || out == r || out.Body != body {
				t.Fatal("outbound hook lost context, isolation, or streaming body")
			}
			out.Header.Set("X-Test", "signed")
			hookRequest = out
			return nil
		})},
		base: outboundRoundTripFunc(func(out *http.Request) (*http.Response, error) {
			if out != hookRequest || out.Header.Get("X-Test") != "signed" {
				t.Fatal("transport did not receive the hook's request")
			}
			data, err := io.ReadAll(out.Body)
			if err != nil || string(data) != "payload" {
				t.Fatalf("body = %q, error = %v", data, err)
			}
			_ = out.Body.Close()
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: out}, nil
		}),
	}
	response, err := transport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if r.Header.Get("X-Test") != "original" || body.closes != 1 {
		t.Fatalf("caller metadata changed or body ownership lost: header=%q closes=%d", r.Header.Get("X-Test"), body.closes)
	}
}

func TestOutboundTransportFailureClosesBodyWithoutDelegating(t *testing.T) {
	for _, withBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "no body", true: "body"}[withBody], func(t *testing.T) {
			r, err := http.NewRequest(http.MethodPost, "http://unused.invalid/api/x", nil)
			if err != nil {
				t.Fatal(err)
			}
			body := &outboundBody{Reader: strings.NewReader("unread")}
			if withBody {
				r.Body = body
			}
			failure := errors.New("signing failed")
			transport := &outboundTransport{
				hooks: []BeforeUpstreamHook{
					outboundHookFunc(func(context.Context, *RequestContext, *http.Request) error { return failure }),
					outboundHookFunc(func(context.Context, *RequestContext, *http.Request) error {
						t.Fatal("later hook ran after failure")
						return nil
					}),
				},
				base: outboundRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Fatal("transport ran after failure")
					return nil, nil
				}),
			}
			response, err := transport.RoundTrip(r)
			if response != nil || !errors.Is(err, failure) || (withBody && body.closes != 1) {
				t.Fatalf("response=%v error=%v body closes=%d", response, err, body.closes)
			}
		})
	}
}
