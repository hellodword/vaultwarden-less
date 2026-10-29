package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/routes"
	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type EventSender struct {
	mu       sync.Mutex
	pending  routes.Summary
	endpoint string
	client   *http.Client
	clock    timing.Clock
	log      *slog.Logger
}

func NewEventSender(log *slog.Logger) *EventSender {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &EventSender{endpoint: EventsURL, client: &http.Client{Transport: transport, Timeout: EventTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, clock: timing.Real{}, log: log}
}
func (s *EventSender) Record(method, path string, start bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending.Record(method, path, start)
}
func (s *EventSender) Run(ctx context.Context) {
	var lastFailure time.Time
	for {
		timer := s.clock.NewTimer(EventRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		s.mu.Lock()
		event := s.pending
		s.pending = routes.Summary{}
		s.mu.Unlock()
		if event.Empty() {
			continue
		}
		if err := s.send(ctx, event); err != nil {
			s.mu.Lock()
			s.pending.Merge(event)
			s.mu.Unlock()
			if s.log != nil && (lastFailure.IsZero() || s.clock.Now().Sub(lastFailure) >= 5*time.Minute) {
				s.log.Warn("backup event delivery failed; merged signal retained")
				lastFailure = s.clock.Now()
			}
		}
	}
}
func (s *EventSender) send(ctx context.Context, event routes.Summary) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("event request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return errors.New("event delivery failed")
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusNoContent {
		return errors.New("event not accepted")
	}
	return nil
}

// Flush runs after request draining and after the periodic sender has stopped.
// Its caller supplies the separate, bounded shutdown delivery deadline.
func (s *EventSender) Flush(ctx context.Context) error {
	for {
		s.mu.Lock()
		event := s.pending
		s.pending = routes.Summary{}
		s.mu.Unlock()
		if event.Empty() {
			return nil
		}
		if err := s.send(ctx, event); err == nil {
			continue
		}
		s.mu.Lock()
		s.pending.Merge(event)
		s.mu.Unlock()
		timer := time.NewTimer(EventRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
