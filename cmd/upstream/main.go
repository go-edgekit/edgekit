// This tiny demo API lets you test the gateway without external services.
package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"

	"example.com/go-gateway-starter/internal/safelog"
)

func main() {
	safelog.ConfigureDefault(os.Stderr)
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
	server := &http.Server{Addr: ":9090", Handler: mux, ErrorLog: safelog.New("upstream_http_server")}
	if err := server.ListenAndServe(); err != nil {
		slog.Error("upstream failed", "error_kind", safelog.ErrorKind(err))
	}
}
