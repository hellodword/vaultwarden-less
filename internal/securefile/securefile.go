package securefile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxSecretBytes = 64 << 10

// Read reads a small, regular, non-symlink file whose permissions are exactly
// 0600. A single trailing line ending is removed; other whitespace is data.
func Read(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("secret path must be a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect secret file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret path is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("secret file permissions must be 0600, got %04o", info.Mode().Perm())
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", fmt.Errorf("open secret file: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return "", fmt.Errorf("open secret file handle")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Mode().Perm() != 0o600 || !os.SameFile(info, opened) {
		return "", fmt.Errorf("secret file changed while opening")
	}

	data := make([]byte, maxSecretBytes+1)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	if n > maxSecretBytes {
		return "", fmt.Errorf("secret file exceeds %d bytes", maxSecretBytes)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(data[:n]), "\n"), "\r")
	if value == "" {
		return "", fmt.Errorf("secret file is empty")
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("secret file contains a NUL byte")
	}
	latest, err := os.Lstat(path)
	if err != nil || !latest.Mode().IsRegular() || !os.SameFile(opened, latest) {
		return "", fmt.Errorf("secret file changed while reading")
	}
	return value, nil
}
