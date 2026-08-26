package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const notifierQueueCapacity = 16

var defaultRetryOffsets = []time.Duration{0, time.Minute, 5 * time.Minute}

type senderWorker struct {
	sender Sender
	queue  chan Event
}

type Manager struct {
	workers      []senderWorker
	log          *slog.Logger
	retryOffsets []time.Duration
	wg           sync.WaitGroup
	startOnce    sync.Once
}

type lazySender struct {
	config config.Notifier
	mu     sync.Mutex
	sender Sender
}

func NewManager(configs []config.Notifier, logger *slog.Logger) *Manager {
	senders := make([]Sender, 0, len(configs))
	for _, notifier := range configs {
		senders = append(senders, &lazySender{config: notifier})
	}
	return newManagerWithSenders(senders, logger, defaultRetryOffsets)
}

func newManagerWithSenders(senders []Sender, logger *slog.Logger, retryOffsets []time.Duration) *Manager {
	workers := make([]senderWorker, 0, len(senders))
	for _, sender := range senders {
		workers = append(workers, senderWorker{sender: sender, queue: make(chan Event, notifierQueueCapacity)})
	}
	return &Manager{workers: workers, log: logger, retryOffsets: append([]time.Duration(nil), retryOffsets...)}
}

func ValidateConfigs(configs []config.Notifier) error {
	for _, notifier := range configs {
		if _, err := senderForConfig(notifier); err != nil {
			return fmt.Errorf("initialize notifier %s: %w", notifier.Name, err)
		}
	}
	return nil
}

func (manager *Manager) Start(ctx context.Context) {
	manager.startOnce.Do(func() {
		for index := range manager.workers {
			worker := &manager.workers[index]
			manager.wg.Add(1)
			go func() {
				defer manager.wg.Done()
				manager.runWorker(ctx, worker)
			}()
		}
	})
}

func (manager *Manager) Wait() { manager.wg.Wait() }

// Enqueue never blocks backup work. A full notifier queue drops this event and
// records only a safe structured error.
func (manager *Manager) Enqueue(event Event) {
	if manager == nil {
		return
	}
	if err := event.Validate(); err != nil {
		manager.logError("notification_invalid", "")
		return
	}
	for index := range manager.workers {
		worker := &manager.workers[index]
		copy := event
		copy.Targets = append([]TargetStatus{}, event.Targets...)
		select {
		case worker.queue <- copy:
		default:
			manager.logError("notification_queue_full", worker.sender.Name())
		}
	}
}

func (manager *Manager) runWorker(ctx context.Context, worker *senderWorker) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-worker.queue:
			manager.deliver(ctx, worker.sender, event)
		}
	}
}

func (manager *Manager) deliver(ctx context.Context, sender Sender, event Event) {
	offsets := manager.retryOffsets
	if len(offsets) == 0 {
		offsets = defaultRetryOffsets
	}
	var previousOffset time.Duration
	for attempt, offset := range offsets {
		delay := offset - previousOffset
		if delay < 0 {
			delay = 0
		}
		previousOffset = offset
		if delay != 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if err := sender.Send(ctx, event); err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		manager.logError("notification_delivery_failed", sender.Name())
		if attempt == len(offsets)-1 {
			return
		}
	}
}

func (manager *Manager) logError(code, notifier string) {
	if manager.log == nil {
		return
	}
	if notifier == "" {
		manager.log.Error("notification operation failed", "error_code", code)
		return
	}
	manager.log.Error("notification operation failed", "notifier", notifier, "error_code", code)
}

func (sender *lazySender) Name() string { return sender.config.Name }

func (sender *lazySender) Send(ctx context.Context, event Event) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.sender == nil {
		initialized, err := senderForConfig(sender.config)
		if err != nil {
			return err
		}
		sender.sender = initialized
	}
	return sender.sender.Send(ctx, event)
}

func senderForConfig(notifier config.Notifier) (Sender, error) {
	switch notifier.Type {
	case "wechat":
		return newWechatSender(notifier)
	case "bark":
		return newBarkSender(notifier)
	default:
		return nil, fmt.Errorf("unsupported notifier type")
	}
}
