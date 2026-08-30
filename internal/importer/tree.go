package importer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

const importTreeMagic = "vaultwarden-less-import-tree\x00"
const candidateTransientName = ".candidate-initial"

var excludedSourceFiles = map[string]struct{}{
	"db.sqlite3":         {},
	"db.sqlite3-wal":     {},
	"db.sqlite3-shm":     {},
	"db.sqlite3-journal": {},
	"rsa_key.pem":        {},
	"rsa_key.pub.pem":    {},
	"rsa_key.der":        {},
}

type treeEntry struct {
	path      string
	relative  string
	info      os.FileInfo
	directory bool
}

func inspectPrivateTree(root string, exactModes bool) ([]treeEntry, uint64, error) {
	if err := privatefs.ValidatePrivateDirectory(root); err != nil {
		return nil, 0, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, 0, err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok || rootStat.Dev == 0 {
		return nil, 0, fmt.Errorf("import tree filesystem identity is unavailable")
	}
	device := uint64(rootStat.Dev)
	entries := make([]treeEntry, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) ||
			uint64(stat.Dev) != device || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("import tree contains an unsafe owner, device, or symlink")
		}
		directory := info.IsDir()
		if !directory && (!info.Mode().IsRegular() || stat.Nlink != 1) {
			return fmt.Errorf("import tree contains a special or hard-linked file")
		}
		wantMode := os.FileMode(0o600)
		if directory {
			wantMode = 0o700
		}
		if exactModes && info.Mode().Perm() != wantMode {
			return fmt.Errorf("import tree entry has an unsafe permission mode")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
			return fmt.Errorf("import tree entry has an invalid path")
		}
		relative = filepath.ToSlash(relative)
		if !utf8.ValidString(relative) || strings.ContainsRune(relative, 0) {
			return fmt.Errorf("import tree entry path is not valid UTF-8")
		}
		entries = append(entries, treeEntry{path: path, relative: relative, info: info, directory: directory})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	slices.SortFunc(entries, func(left, right treeEntry) int { return strings.Compare(left.relative, right.relative) })
	return entries, device, nil
}

func normalizePrivateTree(root string) error {
	entries, _, err := inspectPrivateTree(root, false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.directory {
			continue
		}
		if err := normalizeTreeEntry(entry, 0o600); err != nil {
			return err
		}
	}
	for index := len(entries) - 1; index >= 0; index-- {
		if !entries[index].directory {
			continue
		}
		if err := normalizeTreeEntry(entries[index], 0o700); err != nil {
			return err
		}
	}
	_, _, err = inspectPrivateTree(root, true)
	return err
}

func normalizeTreeEntry(entry treeEntry, mode os.FileMode) error {
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
	if entry.directory {
		flags |= syscall.O_DIRECTORY
	}
	fd, err := syscall.Open(entry.path, flags, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), entry.path)
	if file == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open import tree entry handle")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(entry.info, opened) || opened.IsDir() != entry.directory {
		return fmt.Errorf("import tree entry changed while opening")
	}
	if !entry.directory && !privatefs.SameFileVersion(entry.info, opened) {
		return fmt.Errorf("import tree file changed while opening")
	}
	if err := validateTreeEntry(opened, entry.directory); err != nil {
		return err
	}
	if opened.Mode().Perm() != mode {
		if err := syscall.Fchmod(fd, uint32(mode)); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Mode().Perm() != mode {
		return fmt.Errorf("import tree entry changed while securing permissions")
	}
	return validateTreeEntry(after, entry.directory)
}

func validateTreeEntry(info os.FileInfo, directory bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) ||
		info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory {
		return fmt.Errorf("import tree entry has an unsafe owner or type")
	}
	if !directory && (!info.Mode().IsRegular() || stat.Nlink != 1) {
		return fmt.Errorf("import tree entry is special or hard-linked")
	}
	return nil
}

func treeID(ctx context.Context, root string) (string, error) {
	entries, _, err := inspectPrivateTree(root, true)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(importTreeMagic))
	if err := binary.Write(digest, binary.BigEndian, uint64(len(entries))); err != nil {
		return "", err
	}
	for _, entry := range entries {
		if err := writeFrame(digest, []byte(entry.relative)); err != nil {
			return "", err
		}
		kind := byte('f')
		if entry.directory {
			kind = 'd'
		}
		if _, err := digest.Write([]byte{kind}); err != nil {
			return "", err
		}
		if err := binary.Write(digest, binary.BigEndian, uint64(entry.info.Mode().Perm())); err != nil {
			return "", err
		}
		if entry.directory {
			continue
		}
		if err := hashStableFile(ctx, digest, entry); err != nil {
			return "", fmt.Errorf("hash import tree entry: %w", err)
		}
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func fileID(ctx context.Context, root, relative string) (string, error) {
	entries, _, err := inspectPrivateTree(root, true)
	if err != nil {
		return "", err
	}
	index := slices.IndexFunc(entries, func(entry treeEntry) bool { return entry.relative == relative })
	if index < 0 || entries[index].directory {
		return "", fmt.Errorf("required import tree file is missing")
	}
	digest := sha256.New()
	if err := hashStableFile(ctx, digest, entries[index]); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func hashStableFile(ctx context.Context, destination io.Writer, entry treeEntry) error {
	fd, err := syscall.Open(entry.path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), entry.path)
	if file == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open import file handle")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !privatefs.SameFileVersion(entry.info, opened) {
		return fmt.Errorf("import file changed while opening")
	}
	if err := binary.Write(destination, binary.BigEndian, uint64(opened.Size())); err != nil {
		return err
	}
	buffer := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != count {
				return io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	after, err := file.Stat()
	if err != nil || total != opened.Size() || !privatefs.SameFileVersion(opened, after) {
		return fmt.Errorf("import file changed while reading")
	}
	return nil
}

func buildCandidate(
	ctx context.Context,
	source, destination string,
	backup func(context.Context, string, string) error,
) error {
	entries, _, err := inspectPrivateTree(source, true)
	if err != nil {
		return err
	}
	if backup == nil {
		return fmt.Errorf("SQLite online backup is unavailable")
	}
	hasDatabase, hasRSA := false, false
	for _, entry := range entries {
		switch entry.relative {
		case "db.sqlite3":
			hasDatabase = !entry.directory
		case "rsa_key.pem":
			hasRSA = !entry.directory
		}
		if _, excluded := excludedSourceFiles[entry.relative]; excluded {
			continue
		}
		target := filepath.Join(destination, filepath.FromSlash(entry.relative))
		if entry.directory {
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := copyStableFile(ctx, entry, target); err != nil {
			return err
		}
	}
	if !hasDatabase || !hasRSA {
		return fmt.Errorf("import source lacks a database or RSA identity")
	}
	scratch := filepath.Join(destination, ".sqlite-source")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.directory {
			continue
		}
		switch entry.relative {
		case "db.sqlite3", "db.sqlite3-wal", "db.sqlite3-shm", "db.sqlite3-journal":
			if err := copyStableFile(ctx, entry, filepath.Join(scratch, entry.relative)); err != nil {
				return err
			}
		}
	}
	if err := privatefs.SyncDirectory(scratch); err != nil {
		return err
	}
	if err := backup(ctx, filepath.Join(scratch, "db.sqlite3"), filepath.Join(destination, "db.sqlite3")); err != nil {
		return err
	}
	if err := safeRemoveTree(scratch); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(destination, "db.sqlite3"), 0o600); err != nil {
		return err
	}
	for excluded := range excludedSourceFiles {
		if excluded == "db.sqlite3" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(destination, excluded)); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("candidate retained an excluded source identity or SQLite sidecar")
			}
			return err
		}
	}
	if err := syncTreeDirectories(destination); err != nil {
		return err
	}
	_, _, err = inspectPrivateTree(destination, true)
	return err
}

func copyStableFile(ctx context.Context, entry treeEntry, destinationPath string) error {
	fd, err := syscall.Open(entry.path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	source := os.NewFile(uintptr(fd), entry.path)
	if source == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open import source handle")
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil || !privatefs.SameFileVersion(entry.info, opened) {
		return fmt.Errorf("import source changed while opening")
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		_ = destination.Close()
		if !keep {
			_ = os.Remove(destinationPath)
		}
	}()
	written, err := copyContext(ctx, destination, source)
	if err != nil {
		return err
	}
	after, err := source.Stat()
	if err != nil || written != opened.Size() || !privatefs.SameFileVersion(opened, after) {
		return fmt.Errorf("import source changed while copying")
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	if err := destination.Close(); err != nil {
		return err
	}
	keep = true
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != count {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func syncTreeDirectories(root string) error {
	entries, _, err := inspectPrivateTree(root, true)
	if err != nil {
		return err
	}
	directories := []string{root}
	for _, entry := range entries {
		if entry.directory {
			directories = append(directories, entry.path)
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err := privatefs.SyncDirectory(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

func safeRemoveTree(root string) error {
	if _, _, err := inspectPrivateTree(root, false); err != nil {
		return err
	}
	return os.RemoveAll(root)
}

func emptyPrivateDirectory(path string) (bool, error) {
	if err := privatefs.ValidatePrivateDirectory(path); err != nil {
		return false, err
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0, err
}

func deviceID(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 {
		return 0, fmt.Errorf("filesystem device is unavailable")
	}
	return uint64(stat.Dev), nil
}

func writeFrame(destination io.Writer, value []byte) error {
	if err := binary.Write(destination, binary.BigEndian, uint64(len(value))); err != nil {
		return err
	}
	_, err := destination.Write(value)
	return err
}
