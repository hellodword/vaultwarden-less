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

var defaultRetryDelays = []time.Duration{0, time.Minute, 4 * time.Minute}

type senderWorker struct {
	sender Sender
	queue  chan Event
}

type Manager struct {
	workers     []senderWorker
	log         *slog.Logger
	retryDelays []time.Duration
}

func NewManager(configs []config.Notifier, logger *slog.Logger) (*Manager, error) {
	senders := make([]Sender, 0, len(configs))
	for _, notifier := range configs {
		sender, err := senderForConfig(notifier)
		if err != nil {
			return nil, fmt.Errorf("initialize notifier %s: %w", notifier.Name, err)
		}
		senders = append(senders, sender)
	}
	return newManagerWithSenders(senders, logger, defaultRetryDelays), nil
}

func newManagerWithSenders(senders []Sender, logger *slog.Logger, retryDelays []time.Duration) *Manager {
	workers := make([]senderWorker, 0, len(senders))
	for _, sender := range senders {
		workers = append(workers, senderWorker{sender: sender, queue: make(chan Event, notifierQueueCapacity)})
	}
	return &Manager{workers: workers, log: logger, retryDelays: append([]time.Duration(nil), retryDelays...)}
}

func (manager *Manager) Run(ctx context.Context) {
	var wait sync.WaitGroup
	for index := range manager.workers {
		worker := &manager.workers[index]
		wait.Go(func() { manager.runWorker(ctx, worker) })
	}
	wait.Wait()
}

// Enqueue never blocks backup work. A full notifier queue drops this event and
// records only a safe structured error.
func (manager *Manager) Enqueue(event Event) {
	for index := range manager.workers {
		worker := &manager.workers[index]
		select {
		case worker.queue <- event.clone():
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
	for _, delay := range manager.retryDelays {
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
