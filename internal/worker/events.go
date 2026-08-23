package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

const (
	EventsPath = "/v1/events"
	HealthPath = "/.vaultwarden-less/healthz"
)

type DirtySink interface {
	MarkDirty(time.Time) error
}

type EventServer struct {
	cfg     *config.Backup
	sink    DirtySink
	http    *http.Server
	limiter tokenBucket
}

type tokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	rate     float64
	capacity float64
}

func NewEventServer(cfg *config.Backup, sink DirtySink) *EventServer {
	server := &EventServer{
		cfg:     cfg,
		sink:    sink,
		limiter: tokenBucket{tokens: float64(cfg.EventBurst), last: time.Now(), rate: cfg.EventRatePerSecond, capacity: float64(cfg.EventBurst)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(EventsPath, server.handleEvent)
	mux.HandleFunc(HealthPath, server.handleHealth)
	server.http = &http.Server{
		Addr: cfg.ListenAddress, Handler: mux,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 64 << 10,
	}
	return server
}

func (s *EventServer) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *EventServer) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *EventServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *EventServer) handleEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeHTTPError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	if !s.limiter.allow(time.Now()) {
		writeHTTPError(w, http.StatusTooManyRequests, "rate_limited", "Event rate limit exceeded")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeHTTPError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	if r.ContentLength > s.cfg.EventMaxBytes {
		writeHTTPError(w, http.StatusRequestEntityTooLarge, "event_too_large", "Event body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.EventMaxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var event struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := dec.Decode(&event); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeHTTPError(w, http.StatusRequestEntityTooLarge, "event_too_large", "Event body too large")
			return
		}
		writeHTTPError(w, http.StatusBadRequest, "invalid_event", "Invalid event body")
		return
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) || event.SchemaVersion != version.SchemaVersion {
		writeHTTPError(w, http.StatusBadRequest, "invalid_event", "Invalid event body")
		return
	}
	if err := s.sink.MarkDirty(time.Now()); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, "state_unavailable", "Event state unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (b *tokenBucket) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func writeHTTPError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\"error\":{\"code\":%s,\"message\":%s}}\n", strconv.Quote(code), strconv.Quote(message))
}
