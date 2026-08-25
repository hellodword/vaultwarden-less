package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var eventBody = []byte("{\"schema_version\":1}\n")

// EventSender coalesces any number of dirty marks into a single bounded signal.
// Delivery never runs in a request goroutine and never delays the proxy response.
type EventSender struct {
	url    *url.URL
	client *http.Client
	log    *slog.Logger
	signal chan struct{}
}

func NewEventSender(log *slog.Logger) *EventSender {
	return newEventSender(mustURL(EventsURL), EventTimeout, log)
}

func newEventSender(target *url.URL, timeout time.Duration, log *slog.Logger) *EventSender {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &EventSender{
		url: target,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log:    log,
		signal: make(chan struct{}, 1),
	}
}

func (s *EventSender) MarkDirty() {
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *EventSender) Run(ctx context.Context, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.signal:
				if err := s.send(ctx); err != nil {
					s.log.Error("backup event delivery failed; periodic sampling remains active", "error_code", "event_delivery_failed")
				}
			}
		}
	}()
}

func (s *EventSender) send(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url.String(), bytes.NewReader(eventBody))
	if err != nil {
		return fmt.Errorf("create event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("send event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("event endpoint returned status %d", resp.StatusCode)
	}
	return nil
}
