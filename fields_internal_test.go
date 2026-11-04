package edgekit

import (
	"maps"
	"slices"
	"testing"
)

// fieldSet returns the reserved headers with the added names and without the
// excepted names.
func fieldSet(added []string, excepted ...string) map[string]struct{} {
	set := maps.Clone(reservedHeaders)
	for _, name := range added {
		set[name] = struct{}{}
	}

	for _, name := range excepted {
		delete(set, name)
	}

	return set
}

// TestRemovedFieldSetsFollowReservedHeaders makes sure that a name added to
// reservedHeaders also reaches each list of removed fields, or is an explicit
// exception here.
func TestRemovedFieldSetsFollowReservedHeaders(t *testing.T) {
	outbound := maps.Clone(generatedRequestFields)
	for _, name := range connectionFields {
		if _, ok := outbound[name]; ok {
			t.Errorf("%s is in connectionFields and in generatedRequestFields, want one list", name)
		}

		outbound[name] = struct{}{}
	}

	for _, tc := range []struct {
		name string
		got  map[string]struct{}
		want map[string]struct{}
	}{
		{
			// Content-Length has its own rule, and Host has no meaning in a
			// response.
			name: "final response",
			got:  finalResponseFields,
			want: fieldSet([]string{altSvc}, "Content-Length", "Host"),
		},
		{
			// Connection and Upgrade are restored after the removal.
			name: "101 response",
			got:  upgradeResponseFields,
			want: fieldSet([]string{altSvc}, "Host"),
		},
		{
			// Proxy-Authenticate is a response field.
			name: "outbound request",
			got:  outbound,
			want: fieldSet(nil, "Proxy-Authenticate"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(tc.got))
			want := slices.Sorted(maps.Keys(tc.want))

			if !slices.Equal(got, want) {
				t.Errorf("fields = %v, want %v", got, want)
			}
		})
	}
}
