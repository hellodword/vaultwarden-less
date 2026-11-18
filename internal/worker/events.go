package worker

import (
	"encoding/json"
	"net/http"
	"time"
)

const (
	ListenAddress = "0.0.0.0:9090"
	HealthPath    = "/healthz"
	EventsPath    = "/events"
	StatusPath    = "/status"
)

func Handler(worker *Worker) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == HealthPath:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == StatusPath:
			writeJSON(w, http.StatusOK, worker.Status())
		case r.Method == http.MethodPost && r.URL.Path == EventsPath:
			worker.Event()
			w.WriteHeader(http.StatusNoContent)
		default:
			writeError(w, http.StatusNotFound, "not_found")
		}
	})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": "Request could not be accepted"}})
}
func NewHTTPServer(worker *Worker) *http.Server {
	return &http.Server{Addr: ListenAddress, Handler: Handler(worker), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
}
