// Package state stores small worker records independently of disposable caches.
package state

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type Store struct{ Directory string }

func (s Store) Load(name string, out any) error {
	if s.Directory == "" {
		return nil
	}
	file, err := os.Open(filepath.Join(s.Directory, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("state_read_failed")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16<<20+1))
	if err != nil || len(data) > 16<<20 {
		return errors.New("state_read_failed")
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("state_invalid")
	}
	return nil
}
func (s Store) Save(name string, value any) error {
	if s.Directory == "" {
		return nil
	}
	if err := os.MkdirAll(s.Directory, 0o700); err != nil {
		return errors.New("state_write_failed")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("state_write_failed")
	}
	file, err := os.CreateTemp(s.Directory, ".replace-")
	if err != nil {
		return errors.New("state_write_failed")
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err = file.Write(data); err != nil {
		return errors.New("state_write_failed")
	}
	if err = file.Sync(); err != nil {
		return errors.New("state_write_failed")
	}
	if err = file.Close(); err != nil {
		return errors.New("state_write_failed")
	}
	if err = os.Rename(file.Name(), filepath.Join(s.Directory, name)); err != nil {
		return errors.New("state_write_failed")
	}
	dir, err := os.Open(s.Directory)
	if err != nil {
		return errors.New("state_write_failed")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errors.New("state_write_failed")
	}
	return nil
}

// Lock must be held until all capture, repository and delivery goroutines exit.
// Only after taking it may the process reclaim its own disk work directory.
func (s Store) Lock() (*os.File, error) {
	if err := os.MkdirAll(s.Directory, 0o700); err != nil {
		return nil, errors.New("state_unavailable")
	}
	file, err := os.OpenFile(filepath.Join(s.Directory, "worker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("state_unavailable")
	}
	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		file.Close()
		return nil, errors.New("worker_already_running")
	}
	return file, nil
}
