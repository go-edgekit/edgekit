package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"
)

// RequestHook runs after global middleware enters and before routing.
// An error stops normal processing and is rendered by the gateway.
type RequestHook interface {
	OnRequest(context.Context, *RequestContext, *http.Request) error
}

// UpstreamResponseHook inspects or modifies the upstream response before its
// final headers are forwarded. It does not run for local responses or forwarded
// informational (1xx) responses, except protocol upgrades (101).
// An error closes the upstream body and enters gateway error handling.
// A hook replacing Body must close the original body and update framing headers.
type UpstreamResponseHook interface {
	OnUpstreamResponse(context.Context, *RequestContext, *http.Response) error
}

// CompletionHook observes the outcome after registered middleware unwinds,
// including local rejection, routing misses, and panic unwinding. It must not
// panic or write a response. The context is the gateway entry context, not a
// derived context created inside middleware, and may already be canceled.
type CompletionHook interface {
	OnComplete(context.Context, *RequestContext, Outcome)
}

// Outcome describes gateway execution, not guaranteed delivery to the client.
// HTTP status and byte accounting belong in response-observing middleware;
// the gateway does not wrap ResponseWriter or change its optional interfaces.
type Outcome struct {
	Duration time.Duration
	// Err is an error seen by a lifecycle hook or proxy error handler.
	// Local middleware responses and HTTP error statuses are not Go errors.
	Err error
	// Aborted means the middleware chain did not return normally. Streaming
	// failures can abort after headers are committed without setting Err.
	Aborted bool
}

// HTTPError rejects a request with a public message and a 4xx or 5xx status.
// Other errors receive a generic response; their details are not sent to clients.
// Invalid status codes produce a generic 500 response.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string { return e.Message }

type hooks struct {
	requests    []RequestHook
	responses   []UpstreamResponseHook
	completions []CompletionHook
}

func collectHooks(extensions []any) (hooks, error) {
	var result hooks
	for i, extension := range extensions {
		if extension == nil || isNilExtension(extension) {
			return hooks{}, fmt.Errorf("gateway: extension %d is nil", i)
		}
		found := false
		if hook, ok := extension.(RequestHook); ok {
			result.requests = append(result.requests, hook)
			found = true
		}
		if hook, ok := extension.(UpstreamResponseHook); ok {
			result.responses = append(result.responses, hook)
			found = true
		}
		if hook, ok := extension.(CompletionHook); ok {
			result.completions = append(result.completions, hook)
			found = true
		}
		if !found {
			return hooks{}, fmt.Errorf("gateway: extension %d implements no lifecycle hooks", i)
		}
	}
	return result, nil
}

func isNilExtension(extension any) bool {
	value := reflect.ValueOf(extension)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func renderError(w http.ResponseWriter, rc *RequestContext, err error, fallback int) {
	rc.err = err
	status := fallback
	message := http.StatusText(status)
	var rejection *HTTPError
	if errors.As(err, &rejection) && rejection != nil {
		status = rejection.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
			message = http.StatusText(status)
		} else {
			message = rejection.Message
			if message == "" {
				message = http.StatusText(status)
			}
		}
	}
	http.Error(w, message, status)
}
