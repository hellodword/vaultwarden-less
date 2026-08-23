package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

type Manager struct {
	store   *QueueStore
	senders []Sender
	names   []string
	log     *slog.Logger
	wg      sync.WaitGroup
}

type lazySender struct {
	cfg    config.Notifier
	mu     sync.Mutex
	sender Sender
}

func NewManager(stateDir string, configs []config.Notifier, log *slog.Logger) (*Manager, error) {
	senders := make([]Sender, 0, len(configs))
	names := make([]string, 0, len(configs))
	for _, cfg := range configs {
		sender := &lazySender{cfg: cfg}
		senders = append(senders, sender)
		names = append(names, cfg.Name)
	}
	store, err := OpenQueueStore(stateDir, names)
	if err != nil {
		return nil, err
	}
	return &Manager{store: store, senders: senders, names: names, log: log}, nil
}

func NewUnavailableManager(log *slog.Logger) *Manager {
	return &Manager{log: log}
}

func ValidateConfigs(configs []config.Notifier) error {
	for _, cfg := range configs {
		_, err := senderForConfig(cfg)
		if err != nil {
			return fmt.Errorf("initialize notifier %s: %w", cfg.Name, err)
		}
	}
	return nil
}

func (m *Manager) Start(ctx context.Context) {
	for _, sender := range m.senders {
		sender := sender
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runSender(ctx, sender)
		}()
	}
}

func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) EnqueueSuccess(event Event) error {
	if m.store == nil {
		return fmt.Errorf("notification persistence is unavailable")
	}
	return m.store.EnqueueSuccessReplacingState(m.names, event, "source-integrity")
}

func (m *Manager) EnqueueState(key string, event Event) error {
	if m.store == nil {
		return fmt.Errorf("notification persistence is unavailable")
	}
	return m.store.EnqueueAll(m.names, event, "state", key)
}

func (s *lazySender) Name() string { return s.cfg.Name }

func (s *lazySender) Send(ctx context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sender == nil {
		sender, err := senderForConfig(s.cfg)
		if err != nil {
			return err
		}
		s.sender = sender
	}
	return s.sender.Send(ctx, event)
}

func senderForConfig(cfg config.Notifier) (Sender, error) {
	switch cfg.Type {
	case "webhook":
		return newWebhookSender(cfg)
	case "wechat":
		return newWechatSender(cfg)
	case "bark":
		return newBarkSender(cfg)
	default:
		return nil, fmt.Errorf("unsupported notifier type")
	}
}

func (m *Manager) runSender(ctx context.Context, sender Sender) {
	failures := 0
	for {
		item, exists := m.store.Peek(sender.Name())
		if !exists {
			select {
			case <-ctx.Done():
				return
			case <-m.store.Wake(sender.Name()):
				continue
			}
		}
		if err := sender.Send(ctx, item.Event); err != nil {
			if ctx.Err() != nil {
				return
			}
			failures++
			m.log.Error("notification delivery failed", "notifier", sender.Name(), "error_code", "notification_delivery_failed")
			delay := retryDelay(failures)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				continue
			}
		}
		failures = 0
		if err := m.store.Ack(sender.Name(), item.Event.ID); err != nil {
			m.log.Error("notification acknowledgement persistence failed", "notifier", sender.Name(), "error_code", "notification_state_failed")
			timer := time.NewTimer(time.Minute)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func retryDelay(failures int) time.Duration {
	delay := time.Minute
	for i := 1; i < failures && delay < 15*time.Minute; i++ {
		delay *= 2
	}
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}
