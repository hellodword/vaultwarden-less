package notify

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type Status struct {
	Name        string    `json:"name"`
	Pending     uint64    `json:"pending"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	Error       string    `json:"error,omitempty"`
	Permanent   bool      `json:"stopped"`
	Exhausted   bool      `json:"exhausted"`
}
type channel struct {
	sender     Sender
	pending    Event
	wake       chan struct{}
	status     Status
	generation uint64
}
type Manager struct {
	mu       sync.Mutex
	channels []*channel
	clock    timing.Clock
}

func NewManager(configs []config.Notifier) *Manager {
	m := &Manager{clock: timing.Real{}}
	for _, cfg := range configs {
		sender, err := senderForConfig(cfg)
		c := &channel{sender: sender, wake: make(chan struct{}, 1), status: Status{Name: cfg.Name}}
		if err != nil {
			c.status.Error = "invalid_notifier_config"
			c.status.Permanent = true
		}
		m.channels = append(m.channels, c)
	}
	return m
}
func (m *Manager) Enqueue(event Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Count == 0 {
		event.Count = 1
	}
	for _, c := range m.channels {
		c.pending.Merge(event)
		c.status.Pending = c.pending.Count
		c.generation++
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}
func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Status, 0, len(m.channels))
	for _, c := range m.channels {
		result = append(result, c.status)
	}
	return result
}
func (m *Manager) Run(ctx context.Context) {
	var wait sync.WaitGroup
	for _, c := range m.channels {
		if c.sender != nil {
			wait.Go(func() { m.run(ctx, c) })
		}
	}
	wait.Wait()
}

func (m *Manager) run(ctx context.Context, c *channel) {
	var firstFailure, lastAttempt time.Time
	attempts := 0
	var exhaustedAt uint64
	for {
		m.mu.Lock()
		now := m.clock.Now()
		ready := !c.status.Permanent && c.pending.Count > 0
		if c.status.Exhausted {
			if c.generation == exhaustedAt {
				ready = false
			} else {
				c.status.Exhausted = false
				attempts = 0
				firstFailure = time.Time{}
			}
		}
		due := c.status.NextAttempt
		if minimum := lastAttempt.Add(10 * time.Second); !lastAttempt.IsZero() && due.Before(minimum) {
			due = minimum
		}
		if ready && !due.After(now) {
			event := c.pending.clone()
			c.pending = Event{}
			if c.status.Error != "" {
				event.Undelivered = event.Count
			}
			m.mu.Unlock()
			lastAttempt = now
			sendCtx, cancel := context.WithTimeout(ctx, notifierTimeout)
			err := c.sender.Send(sendCtx, event)
			cancel()
			m.mu.Lock()
			if err == nil {
				c.status.LastSuccess = m.clock.Now()
				c.status.Error = ""
				c.status.NextAttempt = lastAttempt.Add(10 * time.Second)
				c.status.Exhausted = false
				attempts = 0
				firstFailure = time.Time{}
			} else {
				// Newer state wins when failed delivery is merged back.
				event.Merge(c.pending)
				c.pending = event
				attempts++
				if firstFailure.IsZero() {
					firstFailure = m.clock.Now()
				}
				c.status.Error = "notification_failed"
				var failure *DeliveryError
				if errors.As(err, &failure) {
					c.status.Error = failure.Code
					c.status.Permanent = failure.Permanent
				}
				if attempts >= 3 {
					c.status.Exhausted = true
					exhaustedAt = c.generation
					c.status.NextAttempt = time.Time{}
				} else {
					delay := time.Minute
					if attempts == 2 {
						delay = 5 * time.Minute
					}
					c.status.NextAttempt = firstFailure.Add(delay)
				}
				if failure != nil && failure.After > 0 {
					minimum := m.clock.Now().Add(failure.After)
					if minimum.After(c.status.NextAttempt) {
						c.status.NextAttempt = minimum
					}
				}
			}
			c.status.Pending = c.pending.Count
			m.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			continue
		}
		m.mu.Unlock()
		if !ready {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}
		timer := m.clock.NewTimer(due.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.wake:
			timer.Stop()
		case <-timer.C():
		}
	}
}
