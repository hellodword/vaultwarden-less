package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/routes"
)

const (
	ListenAddress = "0.0.0.0:9090"
	HealthPath    = "/healthz"
	EventsPath    = "/events"
	CheckPath     = "/checks"
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
			var events routes.Summary
			if !decode(w, r, &events) || !events.Valid() {
				writeError(w, http.StatusBadRequest, "invalid_events")
				return
			}
			worker.Events(events)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == CheckPath:
			var check struct {
				Full *bool `json:"full"`
			}
			if !decode(w, r, &check) || check.Full == nil {
				writeError(w, http.StatusBadRequest, "invalid_check")
				return
			}
			worker.Request(*check.Full)
			writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
		default:
			writeError(w, http.StatusNotFound, "not_found")
		}
	})
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
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
