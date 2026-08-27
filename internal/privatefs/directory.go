package privatefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ValidatePrivateDirectory validates an existing exact 0700 mount root without
// creating it or changing its permissions.
func ValidatePrivateDirectory(path string) error {
	if path == "" || path == string(filepath.Separator) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("private directory path must be clean, absolute, and non-root")
	}
	if err := validateDirectoryChain(path, false); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	permissions := info.Mode().Perm()
	stat, ownerKnown := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownerKnown || stat.Uid != uint32(os.Geteuid()) ||
		stat.Gid != uint32(os.Getegid()) || permissions != 0o700 {
		return fmt.Errorf("path must be a real 0700 directory owned by effective identity %d:%d", os.Geteuid(), os.Getegid())
	}
	return nil
}

// EnsurePrivateDirectory creates an absolute directory and then rejects any
// symlink or non-directory in its path before setting the leaf to 0700.
func EnsurePrivateDirectory(path string) error {
	if path == "" || path == string(filepath.Separator) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("private directory path must be clean and absolute")
	}
	if err := validateDirectoryChain(path, true); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := validateDirectoryChain(path, false); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return ValidatePrivateDirectory(path)
}

func validateDirectoryChain(path string, allowMissing bool) error {
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if allowMissing && os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("private directory path contains a symlink or non-directory")
		}
	}
	return nil
}

// SameFileVersion identifies the same regular-file contents boundary used by
// stable-copy and private secret readers. Ctime catches in-place replacement
// on filesystems where size and mtime are preserved.
func SameFileVersion(left, right os.FileInfo) bool {
	if left == nil || right == nil || !left.Mode().IsRegular() || !right.Mode().IsRegular() ||
		!os.SameFile(left, right) || left.Size() != right.Size() || left.ModTime() != right.ModTime() {
		return false
	}
	leftStat, leftOK := left.Sys().(*syscall.Stat_t)
	rightStat, rightOK := right.Sys().(*syscall.Stat_t)
	return leftOK && rightOK && leftStat.Ctim == rightStat.Ctim
}
