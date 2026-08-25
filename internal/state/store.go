package state

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
)

const maxStateBytes = 8 << 20

type Store struct {
	mu      sync.Mutex
	dir     string
	path    string
	state   State
	fileOps safefs.FileOps
	rebuilt bool
}

// PersistError reports whether a failed state write crossed the rename
// publication boundary. Callers that have already published a related
// filesystem change must not roll it back when this is
// PublishedUnconfirmed.
type PersistError struct {
	PublishState safefs.PublishState
	Err          error
}

func (e *PersistError) Error() string { return e.Err.Error() }
func (e *PersistError) Unwrap() error { return e.Err }

func Open(dir string, targetNames []string) (*Store, error) {
	return OpenWithFileOps(dir, targetNames, safefs.FileOps{})
}

func OpenWithFileOps(dir string, targetNames []string, fileOps safefs.FileOps) (*Store, error) {
	if err := safefs.ValidatePrivateDirectory(dir); err != nil {
		return nil, fmt.Errorf("validate state directory: %w", err)
	}
	store := &Store{dir: dir, path: filepath.Join(dir, "state.json"), fileOps: fileOps}
	data, err := readStateFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.state = New(targetNames)
		if err := store.saveLocked(); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		// Unsafe files are never overwritten. A malformed but safe regular
		// file is handled below and can be rebuilt in place.
		return nil, err
	}
	parsed, parseErr := parseState(data)
	if parseErr != nil {
		store.state = New(targetNames)
		store.rebuilt = true
		if err := store.saveLocked(); err != nil {
			return nil, fmt.Errorf("rebuild runtime state: %w", err)
		}
		return store, nil
	}
	store.state = parsed
	changed := reconcile(&store.state, targetNames)
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

func (s *Store) Rebuilt() bool { return s.rebuilt }

func parseState(data []byte) (State, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var current State
	if err := dec.Decode(&current); err != nil {
		return State{}, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return State{}, fmt.Errorf("state contains multiple JSON values")
		}
		return State{}, fmt.Errorf("state contains trailing data")
	}
	if err := current.Validate(); err != nil {
		return State{}, err
	}
	return current, nil
}

func reconcile(current *State, targetNames []string) bool {
	changed := false
	configured := make(map[string]struct{}, len(targetNames))
	for _, name := range targetNames {
		configured[name] = struct{}{}
		if _, exists := current.Targets[name]; !exists {
			current.Targets[name] = Target{Name: name, State: "unknown"}
			changed = true
		}
	}
	for name := range current.Targets {
		if _, exists := configured[name]; !exists {
			delete(current.Targets, name)
			changed = true
		}
	}
	return changed
}

func Read(dir string) (State, error) {
	if err := safefs.ValidatePrivateDirectory(dir); err != nil {
		return State{}, fmt.Errorf("validate state directory: %w", err)
	}
	data, err := readStateFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return State{}, err
	}
	return parseState(data)
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
	result, err := s.saveLockedResult()
	if err != nil && result == safefs.Unpublished {
		// The old value is still the durable value before publication.
		s.state = previous
	}
	if err != nil {
		// A published_unconfirmed rename is authoritative in memory even
		// though callers must surface the fsync uncertainty.
		return &PersistError{PublishState: result, Err: err}
	}
	return nil
}

func (s *Store) saveLocked() error {
	_, err := s.saveLockedResult()
	return err
}

func (s *Store) saveLockedResult() (safefs.PublishState, error) {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return safefs.Unpublished, fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')
	result, err := safefs.AtomicReplace(s.path, data, s.fileOps)
	if err != nil {
		return result, fmt.Errorf("persist state: %w", err)
	}
	return result, nil
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
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxStateBytes {
		return nil, fmt.Errorf("state file exceeds %d bytes", maxStateBytes)
	}
	return data, nil
}
