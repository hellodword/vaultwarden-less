package proxy

import (
	"context"

	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type EventSender struct {
	mu       sync.Mutex
	pending  bool
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
func (s *EventSender) Record() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = true
}
func (s *EventSender) Run(ctx context.Context) {
	var lastFailure time.Time
	for {
		timer := s.clock.NewTimerAt(s.clock.Now().Add(EventRetry))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		}
		s.mu.Lock()
		event := s.pending
		s.pending = false
		s.mu.Unlock()
		if !event {
			continue
		}
		if err := s.send(ctx); err != nil {
			s.mu.Lock()
			s.pending = true
			s.mu.Unlock()
			if s.log != nil && (lastFailure.IsZero() || s.clock.Now().Sub(lastFailure) >= 5*time.Minute) {
				s.log.Warn("backup event delivery failed; merged signal retained")
				lastFailure = s.clock.Now()
			}
		}
	}
}
func (s *EventSender) send(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, nil)
	if err != nil {
		return errors.New("event request failed")
	}

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
		s.pending = false
		s.mu.Unlock()
		if !event {
			return nil
		}
		if err := s.send(ctx); err == nil {
			continue
		}
		s.mu.Lock()
		s.pending = true
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
