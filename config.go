package edgekit

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"slices"
	"strings"
)

// invalid wraps a configuration problem without adding configuration values.
func invalid(format string, args ...any) error {
	problem := fmt.Errorf(format, args...)

	return fmt.Errorf("%w: %w", ErrInvalidConfiguration, problem)
}

// checkConfig validates cfg and compiles hook lists without calling middleware.
func checkConfig(cfg Config) (compiledHooks, map[string]struct{}, []error) {
	compiled, problems := compileHooks(cfg.Hooks)

	managedHeaders := make(map[string]struct{}, len(cfg.ManagedHeaders))
	problems = append(problems, addManagedHeaders(managedHeaders, cfg.ManagedHeaders, "managed header")...)
	problems = append(problems, addHeaderOwners(managedHeaders, cfg.HeaderOwners)...)

	if err := checkTransport(cfg.Transport); err != nil {
		problems = append(problems, invalid("transport: %w", err))
	}

	problems = append(problems, checkRoutes(cfg.Routes, cfg.Admission, cfg.BodyLimit.resolve(DefaultBodyLimit))...)
	problems = append(problems, checkProtections(cfg)...)

	if i := slices.IndexFunc(cfg.SharedMiddleware, isNil); i >= 0 {
		problems = append(problems, invalid("shared middleware %d: is nil", i))
	}

	return compiled, managedHeaders, problems
}

// addHeaderOwners calls ManagedHeaders once for each owner that is not nil.
// It is the only step of the validation that calls extension code.
func addHeaderOwners(set map[string]struct{}, owners []HeaderOwner) []error {
	var problems []error
	for i, owner := range owners {
		if isNilValue(owner) {
			problems = append(problems, invalid("header owners %d: is nil", i))

			continue
		}

		location := fmt.Sprintf("header owners %d: managed header", i)
		problems = append(problems, addManagedHeaders(set, owner.ManagedHeaders(), location)...)
	}

	return problems
}

// addManagedHeaders permits overlap between lists, but not duplicates within a
// list; location must not contain configuration values.
func addManagedHeaders(set map[string]struct{}, names []string, location string) []error {
	var problems []error

	seen := make(map[string]struct{}, len(names))
	for i, name := range names {
		if err := checkManagedHeader(name); err != nil {
			problems = append(problems, invalid("%s %d: %w", location, i, err))

			continue
		}

		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if _, ok := seen[canonical]; ok {
			problems = append(problems, invalid("%s %d: is a duplicate", location, i))
		}

		seen[canonical] = struct{}{}
		set[canonical] = struct{}{}
	}

	return problems
}

// checkTransport accepts nil for the gateway-owned transport.
func checkTransport(transport http.RoundTripper) error {
	if transport == nil {
		return nil
	}

	if isNilValue(transport) {
		return errors.New("is a typed nil value")
	}

	if t, ok := transport.(*http.Transport); ok && !t.DisableCompression {
		return errors.New("does transparent compression")
	}

	return nil
}

// checkProtections validates the gateway-wide protections and relaxations.
func checkProtections(cfg Config) []error {
	var problems []error

	if err := cfg.BodyLimit.check(); err != nil {
		problems = append(problems, invalid("body limit: %w", err))
	}

	if err := checkBudget(cfg.Admission); err != nil {
		problems = append(problems, invalid("admission budget: %w", err))
	}

	if !cfg.QueryPolicy.valid() {
		problems = append(problems, invalid("query policy: is not a defined value"))
	}

	if !cfg.HeaderNamePolicy.valid() {
		problems = append(problems, invalid("header-name policy: is not a defined value"))
	}

	return problems
}

// checkBudget accepts nil, which disables admission.
func checkBudget(b *AdmissionBudget) error {
	if b != nil && b.limit <= 0 {
		return errors.New("is not made with NewAdmissionBudget")
	}

	return nil
}

// checkRoutes validates routes without calling middleware. A route must not
// use gatewayBudget, because then one request takes two slots of it.
// gatewayLimit is the resolved gateway body limit that routes inherit.
func checkRoutes(routes []Route, gatewayBudget *AdmissionBudget, gatewayLimit int64) []error {
	var problems []error

	if len(routes) == 0 {
		problems = append(problems, invalid("routes: no route is configured"))
	}

	seen := make(map[string]struct{}, len(routes))
	for i, r := range routes {
		if err := checkPrefix(r.Prefix); err != nil {
			problems = append(problems, invalid("route %d: prefix: %w", i, err))
		} else if _, ok := seen[r.Prefix]; ok {
			problems = append(problems, invalid("route %d: prefix: is a duplicate", i))
		}

		seen[r.Prefix] = struct{}{}

		if isNilValue(r.Upstream) {
			problems = append(problems, invalid("route %d: upstream selector is nil", i))
		}

		if j := slices.IndexFunc(r.Middleware, isNil); j >= 0 {
			problems = append(problems, invalid("route %d: middleware %d: is nil", i, j))
		}

		if err := r.BodyLimit.check(); err != nil {
			problems = append(problems, invalid("route %d: body limit: %w", i, err))
		}

		if err := checkBudget(r.Admission); err != nil {
			problems = append(problems, invalid("route %d: admission budget: %w", i, err))
		} else if r.Admission != nil && r.Admission == gatewayBudget {
			problems = append(problems, invalid("route %d: admission budget: is also the gateway budget", i))
		}

		if err := checkBuffering(r.RequestBuffering, r.BodyLimit.resolve(gatewayLimit)); err != nil {
			problems = append(problems, invalid("route %d: request buffering: %w", i, err))
		}
	}

	return problems
}

// isNilValue also detects a typed nil, such as a nil pointer or a nil function
// in an interface value, because its methods get a nil receiver.
func isNilValue(v any) bool {
	if v == nil {
		return true
	}

	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// checkManagedHeader rejects names reserved for HTTP handling.
func checkManagedHeader(name string) error {
	if !headerNameAllowed(name) {
		return errors.New("name contains characters other than letters, digits, and -")
	}

	if _, ok := reservedHeaders[textproto.CanonicalMIMEHeaderKey(name)]; ok {
		return errors.New("is a hop-by-hop, framing, or Host header")
	}

	return nil
}

// checkPrefix requires an escaped path that obeys the path policy.
func checkPrefix(prefix string) error {
	if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
		return errors.New("does not start and end with /")
	}

	// The router compares the prefix with EscapedPath, so the prefix must be
	// the escaped form that EscapedPath gives for it.
	decoded, err := url.PathUnescape(prefix)
	if err != nil {
		return errors.New("is not in escaped form")
	}

	u := url.URL{
		Path:    decoded,
		RawPath: prefix,
	}
	if u.EscapedPath() != prefix {
		return errors.New("is not in escaped form")
	}

	if !pathAllowed(prefix) {
		return errors.New("does not obey the path policy")
	}

	return nil
}
