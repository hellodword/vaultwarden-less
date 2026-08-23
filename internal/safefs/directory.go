package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	return os.Chmod(path, 0o700)
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
