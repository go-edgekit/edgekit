package edgekit

import (
	"strings"
	"testing"
)

func BenchmarkQueryAllowed(b *testing.B) {
	for _, bc := range []struct {
		name  string
		query string
	}{
		{
			name:  "empty",
			query: "",
		},
		{
			name:  "two parameters",
			query: "id=42&sort=desc",
		},
		{
			name:  "escaped parameters",
			query: "q=caf%C3%A9&filter=a%20b&page=2&size=50",
		},
		{
			name:  "100 parameters",
			query: strings.Repeat("key=value&", 99) + "key=value",
		},
	} {
		b.Run(bc.name, func(b *testing.B) {
			for b.Loop() {
				queryAllowed(bc.query)
			}
		})
	}
}
