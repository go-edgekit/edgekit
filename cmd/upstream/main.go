// This tiny demo API lets you test the gateway without external services.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "hello from upstream",
			"method":  r.Method,
			"path":    r.URL.Path,
			"query":   r.URL.RawQuery,
		})
	})
	slog.Info("sample upstream listening", "address", ":9090")
	if err := http.ListenAndServe(":9090", mux); err != nil {
		slog.Error("upstream failed", "error", err)
	}
}
