package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

const (
	maxQueueFileBytes = 16 << 20
	maxSuccessItems   = 100
)

type queueFile struct {
	SchemaVersion int                      `json:"schema_version"`
	Notifiers     map[string]notifierQueue `json:"notifiers"`
}

type notifierQueue struct {
	Availability string      `json:"availability"`
	Items        []queueItem `json:"items"`
}

type queueItem struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Event Event  `json:"event"`
}

type QueueStore struct {
	mu      sync.Mutex
	dir     string
	path    string
	state   queueFile
	wake    map[string]chan struct{}
	fileOps safefs.FileOps
	rebuilt bool
}

func OpenQueueStore(dir string, notifierNames []string) (*QueueStore, error) {
	return OpenQueueStoreWithFileOps(dir, notifierNames, safefs.FileOps{})
}

func OpenQueueStoreWithFileOps(dir string, notifierNames []string, fileOps safefs.FileOps) (*QueueStore, error) {
	if err := safefs.ValidatePrivateDirectory(dir); err != nil {
		return nil, fmt.Errorf("validate notification state directory: %w", err)
	}
	store := &QueueStore{
		dir:     dir,
		path:    filepath.Join(dir, "notifications.json"),
		wake:    make(map[string]chan struct{}, len(notifierNames)),
		fileOps: fileOps,
	}
	data, err := readQueueFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.state = queueFile{SchemaVersion: version.StateVersion, Notifiers: map[string]notifierQueue{}}
	} else if err != nil {
		return nil, err
	} else {
		if err := store.decode(data); err != nil {
			store.state = queueFile{SchemaVersion: version.StateVersion, Notifiers: map[string]notifierQueue{}}
			store.rebuilt = true
			if saveErr := store.saveLocked(); saveErr != nil {
				return nil, fmt.Errorf("rebuild notification state: %w", saveErr)
			}
		}
	}
	changed := false
	configured := make(map[string]struct{}, len(notifierNames))
	for _, name := range notifierNames {
		configured[name] = struct{}{}
		store.wake[name] = make(chan struct{}, 1)
		if _, exists := store.state.Notifiers[name]; !exists {
			store.state.Notifiers[name] = notifierQueue{Availability: "available", Items: []queueItem{}}
			changed = true
		}
	}
	for name := range store.state.Notifiers {
		if _, exists := configured[name]; !exists {
			delete(store.state.Notifiers, name)
			changed = true
		}
	}
	if changed || data == nil {
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (s *QueueStore) Rebuilt() bool { return s.rebuilt }

func (s *QueueStore) decode(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s.state); err != nil {
		return fmt.Errorf("decode notification state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("notification state contains multiple JSON values")
		}
		return fmt.Errorf("notification state contains trailing data")
	}
	return s.validate()
}

func (s *QueueStore) EnqueueAll(notifierNames []string, event Event, kind, key string) error {
	return s.enqueueAll(notifierNames, event, kind, key)
}

func (s *QueueStore) enqueueAll(notifierNames []string, event Event, kind, key string) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if kind != "success" && kind != "state" {
		return fmt.Errorf("invalid notification queue kind")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := cloneQueueFile(s.state)
	for _, name := range notifierNames {
		queue, exists := s.state.Notifiers[name]
		if !exists {
			continue
		}
		queuedEvent := event
		if queue.Availability == "" {
			queue.Availability = "available"
		}
		if kind == "state" {
			replaced := false
			for i := range queue.Items {
				if queue.Items[i].Kind == "state" && queue.Items[i].Key == key {
					queue.Items[i] = queueItem{Kind: kind, Key: key, Event: queuedEvent}
					replaced = true
					break
				}
			}
			if !replaced {
				insertAt := len(queue.Items)
				for i := range queue.Items {
					if queue.Items[i].Kind == "success" {
						insertAt = i
						break
					}
				}
				queue.Items = append(queue.Items, queueItem{})
				copy(queue.Items[insertAt+1:], queue.Items[insertAt:])
				queue.Items[insertAt] = queueItem{Kind: kind, Key: key, Event: queuedEvent}
			}
		} else {
			duplicate := false
			for _, item := range queue.Items {
				if item.Kind == "success" && item.Event.ID == event.ID {
					duplicate = true
					break
				}
			}
			if duplicate {
				s.state.Notifiers[name] = queue
				continue
			}
			successCount := 0
			for _, item := range queue.Items {
				if item.Kind == "success" {
					successCount++
				}
			}
			if queue.Availability == "unavailable" || successCount >= maxSuccessItems {
				if queue.Availability != "unavailable" {
					queue.Availability = "unavailable"
					compactSuccesses(&queue)
				}
				replacePendingSuccess(&queue, queuedEvent)
			} else {
				queue.Items = append(queue.Items, queueItem{Kind: kind, Key: key, Event: queuedEvent})
			}
		}
		s.state.Notifiers[name] = queue
	}
	if err := s.validate(); err != nil {
		s.state = previous
		return err
	}
	result, err := s.saveLockedResult()
	if err != nil {
		if result == safefs.Unpublished {
			s.state = previous
		}
		return err
	}
	for _, name := range notifierNames {
		s.signal(name)
	}
	return nil
}

func (s *QueueStore) MarkUnavailable(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, ok := s.state.Notifiers[name]
	if !ok {
		return nil
	}
	if queue.Availability == "unavailable" {
		return nil
	}
	previous := cloneQueueFile(s.state)
	queue.Availability = "unavailable"
	compactSuccesses(&queue)
	s.state.Notifiers[name] = queue
	if err := s.validate(); err != nil {
		s.state = previous
		return err
	}
	result, err := s.saveLockedResult()
	if err != nil {
		if result == safefs.Unpublished {
			s.state = previous
		}
		return err
	}
	return nil
}

func (s *QueueStore) Availability(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Notifiers[name].Availability
}

func compactSuccesses(queue *notifierQueue) {
	var successes []queueItem
	for _, item := range queue.Items {
		if item.Kind == "success" {
			successes = append(successes, item)
		}
	}
	if len(successes) <= 1 {
		return
	}
	latest := successes[len(successes)-1]
	merged := latest.Event.MergedSuccesses
	for _, item := range successes[:len(successes)-1] {
		if ^uint64(0)-merged < item.Event.MergedSuccesses+1 {
			merged = ^uint64(0)
		} else {
			merged += item.Event.MergedSuccesses + 1
		}
	}
	latest.Event.MergedSuccesses = merged
	items := make([]queueItem, 0, len(queue.Items)-len(successes)+1)
	for _, item := range queue.Items {
		if item.Kind != "success" {
			items = append(items, item)
		}
	}
	items = append(items, latest)
	queue.Items = items
}

func replacePendingSuccess(queue *notifierQueue, event Event) {
	for index := range queue.Items {
		if queue.Items[index].Kind != "success" {
			continue
		}
		previous := queue.Items[index].Event
		merged := previous.MergedSuccesses
		if merged < ^uint64(0) {
			merged++
		}
		event.MergedSuccesses = merged
		queue.Items[index] = queueItem{Kind: "success", Key: event.ID, Event: event}
		return
	}
	queue.Items = append(queue.Items, queueItem{Kind: "success", Key: event.ID, Event: event})
}

func (s *QueueStore) Peek(name string) (queueItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, exists := s.state.Notifiers[name]
	if !exists || len(queue.Items) == 0 {
		return queueItem{}, false
	}
	return queue.Items[0], true
}

func (s *QueueStore) Ack(name, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, exists := s.state.Notifiers[name]
	if !exists || len(queue.Items) == 0 || queue.Items[0].Event.ID != id {
		return nil
	}
	previous := cloneQueueFile(s.state)
	queue.Items = append([]queueItem(nil), queue.Items[1:]...)
	s.state.Notifiers[name] = queue
	queue.Availability = "available"
	s.state.Notifiers[name] = queue
	result, err := s.saveLockedResult()
	if err != nil {
		if result == safefs.Unpublished {
			s.state = previous
		}
		return err
	}
	if len(queue.Items) > 0 {
		s.signal(name)
	}
	return nil
}

func (s *QueueStore) Wake(name string) <-chan struct{} { return s.wake[name] }

func (s *QueueStore) signal(name string) {
	if wake := s.wake[name]; wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func (s *QueueStore) validate() error {
	if s.state.SchemaVersion != version.StateVersion || s.state.Notifiers == nil {
		return fmt.Errorf("unsupported notification state")
	}
	for _, queue := range s.state.Notifiers {
		if queue.Availability != "available" && queue.Availability != "unavailable" {
			return fmt.Errorf("invalid notification availability")
		}
		if queue.Items == nil {
			return fmt.Errorf("notification queue items must not be null")
		}
		if len(queue.Items) > 2048 {
			return fmt.Errorf("notification queue exceeds bound")
		}
		successCount := 0
		stateKeys := map[string]struct{}{}
		for _, item := range queue.Items {
			if err := item.Event.Validate(); err != nil {
				return err
			}
			switch item.Kind {
			case "success":
				if item.Key == "" || item.Key != item.Event.ID {
					return fmt.Errorf("invalid success notification key")
				}
				successCount++
			case "state":
				if item.Key == "" {
					return fmt.Errorf("notification state item lacks key")
				}
				if _, duplicate := stateKeys[item.Key]; duplicate {
					return fmt.Errorf("duplicate notification state key")
				}
				stateKeys[item.Key] = struct{}{}
			default:
				return fmt.Errorf("invalid notification queue item")
			}
		}
		if successCount > maxSuccessItems {
			return fmt.Errorf("success notification queue exceeds bound")
		}
	}
	return nil
}

func (s *QueueStore) saveLocked() error {
	_, err := s.saveLockedResult()
	return err
}

func (s *QueueStore) saveLockedResult() (safefs.PublishState, error) {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return safefs.Unpublished, err
	}
	data = append(data, '\n')
	return safefs.AtomicReplace(s.path, data, s.fileOps)
}

func cloneQueueFile(input queueFile) queueFile {
	clone := queueFile{SchemaVersion: input.SchemaVersion, Notifiers: make(map[string]notifierQueue, len(input.Notifiers))}
	for name, queue := range input.Notifiers {
		cloneQueue := notifierQueue{Availability: queue.Availability, Items: make([]queueItem, len(queue.Items))}
		for index, item := range queue.Items {
			cloneQueue.Items[index] = item
			cloneQueue.Items[index].Event = cloneEvent(item.Event)
		}
		clone.Notifiers[name] = cloneQueue
	}
	return clone
}

func cloneEvent(input Event) Event {
	clone := input
	if input.RecoveryPoint != nil {
		point := *input.RecoveryPoint
		clone.RecoveryPoint = &point
	}
	if input.Target != nil {
		target := *input.Target
		clone.Target = &target
	}
	clone.Targets = append([]TargetStatus(nil), input.Targets...)
	return clone
}

func readQueueFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxQueueFileBytes {
		return nil, fmt.Errorf("notification state file is not a bounded regular 0600 file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxQueueFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxQueueFileBytes {
		return nil, fmt.Errorf("notification state file exceeds %d bytes", maxQueueFileBytes)
	}
	return data, nil
}
