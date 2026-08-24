package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
)

const maxStateBytes = 8 << 20

type Store struct {
	mu    sync.Mutex
	dir   string
	path  string
	state State
}

func Open(dir string, targetNames []string) (*Store, error) {
	if err := safefs.ValidatePrivateDirectory(dir); err != nil {
		return nil, fmt.Errorf("validate state directory: %w", err)
	}
	store := &Store{dir: dir, path: filepath.Join(dir, "state.json")}
	data, err := readStateFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.state = New(targetNames)
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&store.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("state contains multiple JSON values")
	}
	if err := store.state.Validate(); err != nil {
		return nil, fmt.Errorf("validate state without overwriting it: %w", err)
	}
	changed := false
	configured := make(map[string]struct{}, len(targetNames))
	for _, name := range targetNames {
		configured[name] = struct{}{}
		if _, exists := store.state.Targets[name]; !exists {
			store.state.Targets[name] = Target{Name: name, State: "unknown"}
			changed = true
		}
	}
	for name := range store.state.Targets {
		if _, exists := configured[name]; !exists {
			delete(store.state.Targets, name)
			changed = true
		}
	}
	if store.state.BackupInProgress {
		store.state.BackupInProgress = false
		store.state.Dirty = true
		changed = true
	}
	if changed {
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func Read(dir string) (State, error) {
	if err := safefs.ValidatePrivateDirectory(dir); err != nil {
		return State{}, fmt.Errorf("validate state directory: %w", err)
	}
	path := filepath.Join(dir, "state.json")
	data, err := readStateFile(path)
	if err != nil {
		return State{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var current State
	if err := dec.Decode(&current); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return State{}, fmt.Errorf("state contains multiple JSON values")
	}
	if err := current.Validate(); err != nil {
		return State{}, err
	}
	return current, nil
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Clone()
}

func (s *Store) Update(update func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state.Clone()
	if err := update(&next); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("refuse invalid state update: %w", err)
	}
	previous := s.state
	s.state = next
	if err := s.saveLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(s.dir, ".state-")
	if err != nil {
		return fmt.Errorf("create state transaction: %w", err)
	}
	tempPath := temp.Name()
	keep := false
	defer func() {
		temp.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure state transaction: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write state transaction: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync state transaction: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close state transaction: %w", err)
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("commit state transaction: %w", err)
	}
	keep = true
	dir, err := os.Open(s.dir)
	if err != nil {
		return fmt.Errorf("open state directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func readStateFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxStateBytes {
		return nil, fmt.Errorf("state file must be a regular 0600 file no larger than %d bytes", maxStateBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxStateBytes+1))
}

type Status struct {
	SchemaVersion    int            `json:"schema_version"`
	Dirty            bool           `json:"dirty"`
	BackupInProgress bool           `json:"backup_in_progress"`
	LastSampleAt     string         `json:"last_sample_at,omitempty"`
	LastSnapshotAt   string         `json:"last_snapshot_at,omitempty"`
	LastContentID    string         `json:"last_content_id,omitempty"`
	Pending          *RecoveryPoint `json:"pending_recovery_point,omitempty"`
	Targets          []Target       `json:"targets"`
}

func (s State) PublicStatus(configuredTargets []string) Status {
	names := append([]string(nil), configuredTargets...)
	sort.Strings(names)
	targets := make([]Target, 0, len(names))
	for _, name := range names {
		if target, exists := s.Targets[name]; exists {
			targets = append(targets, target)
		}
	}
	return Status{
		SchemaVersion:    1,
		Dirty:            s.Dirty,
		BackupInProgress: s.BackupInProgress,
		LastSampleAt:     s.LastSampleAt,
		LastSnapshotAt:   s.LastSnapshotAt,
		LastContentID:    s.LastContentID,
		Pending:          s.Current,
		Targets:          targets,
	}
}
