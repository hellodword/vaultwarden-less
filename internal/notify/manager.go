package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type Status struct {
	Name        string    `json:"name"`
	Pending     uint64    `json:"pending"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	Attempts    int       `json:"attempts"`
	Error       string    `json:"error,omitempty"`
	Permanent   bool      `json:"stopped"`
	Exhausted   bool      `json:"exhausted"`
}
type channel struct {
	sender   Sender
	pending  Event
	inflight Event
	wake     chan struct{}
	status   Status
	identity string
}
type savedChannel struct {
	Identity          string
	Pending, Inflight Event
	Status            Status
}
type Manager struct {
	mu       sync.Mutex
	channels []*channel
	clock    timing.Clock
	store    state.Store
}

func NewManager(configs []config.Notifier) *Manager {
	m := &Manager{clock: timing.Real{}}
	for _, cfg := range configs {
		sender, err := senderForConfig(cfg)
		// Hash routing/configuration, never persist secrets. A corrected permanent
		// rejection is revalidated on restart; unchanged exhausted budgets persist.
		encoded, _ := json.Marshal([]string{cfg.Name, cfg.Type, cfg.WebhookURL, cfg.Secret, cfg.DeviceToken, cfg.AuthKeyFile, cfg.KeyID, cfg.TeamID, cfg.Topic, cfg.Sound})
		if cfg.Key != nil {
			encoded = append(encoded, cfg.Key.PublicKey.X.Bytes()...)
			encoded = append(encoded, cfg.Key.PublicKey.Y.Bytes()...)
		}
		hash := sha256.Sum256(encoded)
		c := &channel{sender: sender, wake: make(chan struct{}, 1), status: Status{Name: cfg.Name}, identity: hex.EncodeToString(hash[:])}
		if err != nil {
			c.status.Error = "invalid_notifier_config"
			c.status.Permanent = true
		}
		m.channels = append(m.channels, c)
	}
	return m
}
func (m *Manager) Restore(store state.Store) error {
	m.store = store
	var saved map[string]savedChannel
	if err := store.Load("notifications.json", &saved); err != nil {
		return err
	}
	for _, c := range m.channels {
		previous, ok := saved[c.status.Name]
		if !ok {
			continue
		}
		previous.Inflight.Merge(previous.Pending)
		c.pending = previous.Inflight
		if c.status.Permanent {
			c.status.Pending = c.pending.Count
			continue
		}
		if previous.Identity == c.identity {
			c.status = previous.Status
			// An interrupted last attempt has already spent its budget. Its outcome
			// is unknown, so retain the content and allow only the remaining attempts.
			if previous.Inflight.Count > 0 && c.status.Attempts >= 3 {
				c.status.Exhausted = true
			}
		} else {
			c.status.LastSuccess = previous.Status.LastSuccess
		}
		c.status.Pending = c.pending.Count
	}
	return nil
}
func (m *Manager) persist() error {
	saved := map[string]savedChannel{}
	for _, c := range m.channels {
		saved[c.status.Name] = savedChannel{c.identity, c.pending, c.inflight, c.status}
	}
	return m.store.Save("notifications.json", saved)
}
func (m *Manager) Enqueue(event Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if event.Count == 0 {
		event.Count = 1
	}
	for _, c := range m.channels {
		c.pending.Merge(event)
		c.status.Pending = Add(c.pending.Count, c.inflight.Count)
		// Only a new report opens a new round. Merging into a running round never
		// resets attempts or the platform's retry deadline.
		if c.status.Exhausted {
			c.status.Exhausted = false
			c.status.Attempts = 0
		}
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	return m.persist()
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
func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(m.channels))
	var wait sync.WaitGroup
	for _, c := range m.channels {
		if c.sender != nil {
			wait.Go(func() { done <- m.run(ctx, c) })
		}
	}
	allDone := make(chan struct{})
	go func() { wait.Wait(); close(allDone) }()
	select {
	case <-ctx.Done():
		cancel()
		<-allDone
		return nil
	case err := <-done:
		cancel()
		<-allDone
		return err
	case <-allDone:
		return nil
	}
}
func (m *Manager) run(ctx context.Context, c *channel) error {
	for {
		m.mu.Lock()
		now := m.clock.Now()
		ready := !c.status.Permanent && !c.status.Exhausted && c.pending.Count > 0
		due := c.status.NextAttempt
		if minimum := c.status.LastAttempt.Add(10 * time.Second); !c.status.LastAttempt.IsZero() && due.Before(minimum) {
			due = minimum
		}
		if ready && !due.After(now) {
			c.inflight = c.pending.clone()
			c.pending = Event{}
			event := c.inflight.clone()
			if c.status.Error != "" {
				event.Undelivered = Add(event.Undelivered, event.Count)
			}
			c.status.LastAttempt = now
			c.status.Attempts++
			// Write the attempt BEFORE HTTP. A crash cannot grant another full budget.
			delay := time.Minute
			if c.status.Attempts >= 2 {
				delay = 5 * time.Minute
			}
			c.status.NextAttempt = now.Add(delay)
			if err := m.persist(); err != nil {
				m.mu.Unlock()
				return err
			}
			m.mu.Unlock()
			sendCtx, cancel := context.WithTimeout(ctx, notifierTimeout)
			err := c.sender.Send(sendCtx, event)
			cancel()
			m.mu.Lock()
			if err == nil {
				c.inflight = Event{}
				c.status.LastSuccess = m.clock.Now()
				c.status.Error = ""
				c.status.Attempts = 0
				c.status.Exhausted = false
				c.status.NextAttempt = c.status.LastAttempt.Add(10 * time.Second)
			} else {
				c.inflight.Merge(c.pending)
				c.pending = c.inflight
				c.inflight = Event{}
				c.status.Error = "notification_failed"
				var failure *DeliveryError
				if errors.As(err, &failure) {
					c.status.Error = failure.Code
					c.status.Permanent = failure.Permanent
				}
				// Retry 1 minute after the first failure and 5 minutes after the second.
				delay := time.Minute
				if c.status.Attempts >= 2 {
					delay = 5 * time.Minute
				}
				if c.status.Attempts >= 3 {
					delay = 0
				}
				if failure != nil {
					delay = max(delay, failure.After)
				}
				c.status.NextAttempt = m.clock.Now().Add(delay)
				if c.status.Attempts >= 3 {
					c.status.Exhausted = true
				}
			}
			c.status.Pending = c.pending.Count
			saveErr := m.persist()
			m.mu.Unlock()
			if saveErr != nil {
				return saveErr
			}
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		m.mu.Unlock()
		if !ready {
			select {
			case <-ctx.Done():
				return nil
			case <-c.wake:
			}
			continue
		}
		timer := m.clock.NewTimer(due.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-c.wake:
			timer.Stop()
		case <-timer.C():
		}
	}
}
