// Package statefile owns canonical private JSON state-file I/O.
package statefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/strictjson"
)

const (
	temporaryPrefix = ".vaultwarden-less-state-"
	temporarySuffix = ".tmp"
)

var (
	createTemp      = os.CreateTemp
	chmodFile       = func(file *os.File, mode os.FileMode) error { return file.Chmod(mode) }
	writeFile       = func(file *os.File, data []byte) (int, error) { return file.Write(data) }
	syncFile        = func(file *os.File) error { return file.Sync() }
	closeFile       = func(file *os.File) error { return file.Close() }
	renameNoReplace = privatefs.RenameNoReplace
	renameReplace   = os.Rename
	syncDirectory   = privatefs.SyncDirectory
)

// ReadJSON reads one canonical, bounded, private JSON document into destination.
func ReadJSON(path string, limit int64, destination any) error {
	if destination == nil {
		return fmt.Errorf("state destination is required")
	}
	data, err := readBytes(path, limit)
	if err != nil {
		return err
	}
	if len(data) == 0 || !utf8.Valid(data) {
		return fmt.Errorf("state JSON is empty or invalid UTF-8")
	}
	if err := strictjson.RejectDuplicateKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("state JSON contains trailing values")
	}
	canonical, err := canonicalJSON(destination, limit)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, canonical) {
		return fmt.Errorf("state JSON is not canonical")
	}
	return nil
}

// WriteNewJSON atomically creates a previously absent canonical JSON file.
func WriteNewJSON(path string, limit int64, value any) error {
	data, err := canonicalJSON(value, limit)
	if err != nil {
		return err
	}
	parent, target, err := validateTarget(path)
	if err != nil {
		return err
	}
	temporary, err := writeTemporary(parent, target, data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := renameNoReplace(temporary, path); err != nil {
		return err
	}
	return syncDirectory(parent)
}

// ReplaceJSON atomically replaces an existing canonical private JSON file.
func ReplaceJSON(path string, limit int64, value any) error {
	data, err := canonicalJSON(value, limit)
	if err != nil {
		return err
	}
	parent, target, err := validateTarget(path)
	if err != nil {
		return err
	}
	before, err := validateFile(path, limit)
	if err != nil {
		return fmt.Errorf("validate replaced state file: %w", err)
	}
	temporary, err := writeTemporary(parent, target, data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(temporary) }()
	after, err := validateFile(path, limit)
	if err != nil || !privatefs.SameFileVersion(before, after) {
		return fmt.Errorf("state file changed before replacement")
	}
	if err := renameReplace(temporary, path); err != nil {
		return err
	}
	return syncDirectory(parent)
}

// IsOwnedTemporary reports whether candidateName belongs to targetName.
func IsOwnedTemporary(targetName, candidateName string) bool {
	if !validTargetName(targetName) || filepath.Base(candidateName) != candidateName ||
		!strings.HasSuffix(candidateName, temporarySuffix) {
		return false
	}
	prefix := temporaryPrefix + targetName + "-"
	return strings.HasPrefix(candidateName, prefix) &&
		len(candidateName) > len(prefix)+len(temporarySuffix)
}

func canonicalJSON(value any, limit int64) ([]byte, error) {
	if value == nil || limit < 1 {
		return nil, fmt.Errorf("state value and positive limit are required")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("state file exceeds %d bytes", limit)
	}
	return data, nil
}

func validateTarget(path string) (string, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", fmt.Errorf("state path must be clean and absolute")
	}
	parent := filepath.Dir(path)
	target := filepath.Base(path)
	if !validTargetName(target) {
		return "", "", fmt.Errorf("state target name is invalid")
	}
	if err := privatefs.ValidatePrivateDirectory(parent); err != nil {
		return "", "", err
	}
	return parent, target, nil
}

func validTargetName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name &&
		!strings.ContainsAny(name, "/\\\x00")
}

func readBytes(path string, limit int64) ([]byte, error) {
	before, err := validateFile(path, limit)
	if err != nil {
		return nil, err
	}
	data, err := privatefs.ReadBytes(path, limit)
	if err != nil {
		return nil, err
	}
	after, err := validateFile(path, limit)
	if err != nil || !privatefs.SameFileVersion(before, after) {
		return nil, fmt.Errorf("state file changed while reading")
	}
	return data, nil
}

func validateFile(path string, limit int64) (os.FileInfo, error) {
	if limit < 1 {
		return nil, fmt.Errorf("state file limit must be positive")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ok ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) ||
		info.Mode().Perm() != 0o600 || info.Size() > limit {
		return nil, fmt.Errorf("state path is not one bounded non-linked regular file")
	}
	return info, nil
}

func writeTemporary(parent, target string, data []byte) (string, error) {
	file, err := createTemp(parent, temporaryPrefix+target+"-*"+temporarySuffix)
	if err != nil {
		return "", err
	}
	path := file.Name()
	open := true
	defer func() {
		if open {
			_ = closeFile(file)
		}
	}()
	if err := chmodFile(file, 0o600); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if written, err := writeFile(file, data); err != nil || written != len(data) {
		_ = os.Remove(path)
		if err == nil {
			err = io.ErrShortWrite
		}
		return "", err
	}
	if err := syncFile(file); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	if err := closeFile(file); err != nil {
		open = false
		_ = os.Remove(path)
		return "", err
	}
	open = false
	return path, nil
}
