package privatefs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// ReadBytes reads a bounded, regular, non-symlink 0600 file owned by the
// effective user.
func ReadBytes(path string, limit int64) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("private file path must be clean and absolute")
	}
	if err := validateDirectoryChain(filepath.Dir(path), false); err != nil {
		return nil, fmt.Errorf("inspect private file ancestors: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect private file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("private path is not a regular file")
	}
	if !privateReadableMode(info) {
		return nil, fmt.Errorf("private file must be owned by effective identity %d:%d with mode 0600", os.Geteuid(), os.Getegid())
	}
	if limit < 1 || info.Size() > limit {
		return nil, fmt.Errorf("private file exceeds %d bytes", limit)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open private file: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("open private file handle")
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !privateReadableMode(opened) || !SameFileVersion(info, opened) {
		return nil, fmt.Errorf("private file changed while opening")
	}

	data := make([]byte, limit+1)
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("read private file: %w", err)
	}
	if int64(n) > limit {
		return nil, fmt.Errorf("private file exceeds %d bytes", limit)
	}
	if int64(n) != opened.Size() {
		return nil, fmt.Errorf("private file changed while reading")
	}
	latest, err := os.Lstat(path)
	if err != nil || !latest.Mode().IsRegular() || !privateReadableMode(latest) || !SameFileVersion(opened, latest) {
		return nil, fmt.Errorf("private file changed while reading")
	}
	return data[:n], nil
}

func privateReadableMode(info os.FileInfo) bool {
	permissions := info.Mode().Perm()
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Gid == uint32(os.Getegid()) && permissions == 0o600
}
