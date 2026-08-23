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
	Items []queueItem `json:"items"`
}

type queueItem struct {
	Kind  string `json:"kind"`
	Key   string `json:"key"`
	Event Event  `json:"event"`
}

type QueueStore struct {
	mu    sync.Mutex
	dir   string
	path  string
	state queueFile
	wake  map[string]chan struct{}
}

func OpenQueueStore(dir string, notifierNames []string) (*QueueStore, error) {
	if err := safefs.EnsurePrivateDirectory(dir); err != nil {
		return nil, fmt.Errorf("create notification state directory: %w", err)
	}
	store := &QueueStore{
		dir:  dir,
		path: filepath.Join(dir, "notifications.json"),
		wake: make(map[string]chan struct{}, len(notifierNames)),
	}
	data, err := readQueueFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.state = queueFile{SchemaVersion: version.StateVersion, Notifiers: map[string]notifierQueue{}}
	} else if err != nil {
		return nil, err
	} else {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&store.state); err != nil {
			return nil, fmt.Errorf("decode notification state: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("notification state contains multiple JSON values")
		}
		if err := store.validate(); err != nil {
			return nil, fmt.Errorf("validate notification state without overwriting it: %w", err)
		}
	}
	changed := false
	configured := make(map[string]struct{}, len(notifierNames))
	for _, name := range notifierNames {
		configured[name] = struct{}{}
		store.wake[name] = make(chan struct{}, 1)
		if _, exists := store.state.Notifiers[name]; !exists {
			store.state.Notifiers[name] = notifierQueue{}
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

func (s *QueueStore) EnqueueAll(notifierNames []string, event Event, kind, key string) error {
	return s.enqueueAll(notifierNames, event, kind, key, "")
}

func (s *QueueStore) EnqueueSuccessReplacingState(notifierNames []string, event Event, stateKey string) error {
	if stateKey == "" {
		return fmt.Errorf("replacement state key must not be empty")
	}
	return s.enqueueAll(notifierNames, event, "success", event.ID, stateKey)
}

func (s *QueueStore) enqueueAll(notifierNames []string, event Event, kind, key, supersedeStateKey string) error {
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
		if supersedeStateKey != "" {
			filtered := queue.Items[:0]
			for _, item := range queue.Items {
				if item.Kind != "state" || item.Key != supersedeStateKey {
					filtered = append(filtered, item)
				}
			}
			queue.Items = filtered
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
			if successCount >= maxSuccessItems {
				for i := len(queue.Items) - 1; i >= 0; i-- {
					if queue.Items[i].Kind == "success" {
						merged := queue.Items[i].Event.MergedSuccesses
						if merged < ^uint64(0) {
							merged++
						}
						queuedEvent.MergedSuccesses = merged
						queue.Items[i] = queueItem{Kind: kind, Key: key, Event: queuedEvent}
						break
					}
				}
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
	if err := s.saveLocked(); err != nil {
		s.state = previous
		return err
	}
	for _, name := range notifierNames {
		s.signal(name)
	}
	return nil
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
	if err := s.saveLocked(); err != nil {
		s.state = previous
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
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(s.dir, ".notifications-")
	if err != nil {
		return err
	}
	path := temp.Name()
	keep := false
	defer func() {
		temp.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, s.path); err != nil {
		return err
	}
	keep = true
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cloneQueueFile(input queueFile) queueFile {
	data, _ := json.Marshal(input)
	var clone queueFile
	_ = json.Unmarshal(data, &clone)
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
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxQueueFileBytes+1))
}
