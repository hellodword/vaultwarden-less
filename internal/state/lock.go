package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
)

type Lock struct {
	file *os.File
}

func AcquireLock(dir string) (*Lock, error) {
	if err := safefs.ValidatePrivateDirectory(dir); err != nil {
		return nil, fmt.Errorf("validate lock directory: %w", err)
	}
	path := filepath.Join(dir, "worker.lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open worker lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open worker lock handle")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("worker lock is not a regular file")
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure worker lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another backup worker owns the state directory")
		}
		return nil, fmt.Errorf("acquire worker lock: %w", err)
	}
	return &Lock{file: file}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
