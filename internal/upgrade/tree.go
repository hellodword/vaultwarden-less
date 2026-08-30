package upgrade

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

const rollbackMagic = "vaultwarden-less-rollback\x00"

type treeFile struct {
	path     string
	relative string
	info     os.FileInfo
}

type mutableTreeEntry struct {
	path      string
	info      os.FileInfo
	directory bool
}

func normalizePrivateTree(root string) error {
	if err := privatefs.ValidatePrivateDirectory(root); err != nil {
		return err
	}
	entries := make([]mutableTreeEntry, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) ||
			info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("rollback tree contains an unsafe owner or symlink")
		}
		if !info.IsDir() && (!info.Mode().IsRegular() || stat.Nlink != 1) {
			return fmt.Errorf("rollback tree contains a special or hard-linked file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
			return fmt.Errorf("rollback tree entry has an invalid path")
		}
		if relative != "." {
			relative = filepath.ToSlash(relative)
			if !utf8.ValidString(relative) || strings.ContainsRune(relative, 0) {
				return fmt.Errorf("rollback tree entry path is not valid UTF-8")
			}
		}
		entries = append(entries, mutableTreeEntry{path: path, info: info, directory: info.IsDir()})
		return nil
	})
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
	_, _, err = inspectPrivateTree(root)
	return err
}

func normalizeTreeEntry(entry mutableTreeEntry, mode os.FileMode) error {
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
		return fmt.Errorf("open rollback tree entry handle")
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(entry.info, opened) || opened.IsDir() != entry.directory {
		return fmt.Errorf("rollback tree entry changed while opening")
	}
	if !entry.directory && !privatefs.SameFileVersion(entry.info, opened) {
		return fmt.Errorf("rollback tree file changed while opening")
	}
	if err := validateMutableTreeEntry(opened, entry.directory); err != nil {
		return err
	}
	if opened.Mode().Perm() != mode.Perm() {
		if err := syscall.Fchmod(fd, uint32(mode.Perm())); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("rollback tree entry changed while securing permissions")
	}
	if err := validateMutableTreeEntry(after, entry.directory); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	latest, err := os.Lstat(entry.path)
	if err != nil || !os.SameFile(after, latest) || latest.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("rollback tree entry changed after securing permissions")
	}
	return validateMutableTreeEntry(latest, entry.directory)
}

func validateMutableTreeEntry(info os.FileInfo, directory bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) ||
		info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory {
		return fmt.Errorf("rollback tree entry has an unsafe owner or type")
	}
	if !directory && (!info.Mode().IsRegular() || stat.Nlink != 1) {
		return fmt.Errorf("rollback tree entry is special or hard-linked")
	}
	return nil
}

func rollbackTreeID(ctx context.Context, root string) (string, error) {
	files, _, err := inspectPrivateTree(root)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(rollbackMagic))
	if err := binary.Write(digest, binary.BigEndian, uint64(len(files))); err != nil {
		return "", err
	}
	for _, entry := range files {
		if err := writeFrame(digest, []byte(entry.relative)); err != nil {
			return "", err
		}
		if err := binary.Write(digest, binary.BigEndian, uint64(entry.info.Mode().Perm())); err != nil {
			return "", err
		}
		if err := hashStableFile(ctx, digest, entry); err != nil {
			return "", fmt.Errorf("hash rollback entry: %w", err)
		}
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func inspectPrivateTree(root string) ([]treeFile, []string, error) {
	if err := privatefs.ValidatePrivateDirectory(root); err != nil {
		return nil, nil, err
	}
	files := make([]treeFile, 0)
	directories := []string{root}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("rollback tree contains an unsafe owner or symlink")
		}
		if info.IsDir() {
			if info.Mode().Perm() != 0o700 {
				return fmt.Errorf("rollback tree directory is not 0700")
			}
			if path != root {
				directories = append(directories, path)
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || stat.Nlink != 1 {
			return fmt.Errorf("rollback tree contains a special, insecure, or hard-linked file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
			return fmt.Errorf("rollback tree entry has an invalid path")
		}
		relative = filepath.ToSlash(relative)
		if !utf8.ValidString(relative) || strings.ContainsRune(relative, 0) {
			return fmt.Errorf("rollback tree entry path is not valid UTF-8")
		}
		files = append(files, treeFile{path: path, relative: relative, info: info})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(files, func(left, right treeFile) int { return strings.Compare(left.relative, right.relative) })
	return files, directories, nil
}

func writeFrame(destination io.Writer, value []byte) error {
	if err := binary.Write(destination, binary.BigEndian, uint64(len(value))); err != nil {
		return err
	}
	_, err := destination.Write(value)
	return err
}

func hashStableFile(ctx context.Context, destination io.Writer, entry treeFile) error {
	fd, err := syscall.Open(entry.path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), entry.path)
	if file == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open rollback file handle")
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !privatefs.SameFileVersion(entry.info, opened) {
		return fmt.Errorf("rollback file changed while opening")
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
		return fmt.Errorf("rollback file changed while reading")
	}
	return nil
}

func copyPrivateTree(ctx context.Context, source, destination string) (err error) {
	files, directories, err := inspectPrivateTree(source)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return fmt.Errorf("rollback destination already exists")
		}
		return err
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()
	for _, directory := range directories[1:] {
		relative, relativeErr := filepath.Rel(source, directory)
		if relativeErr != nil {
			return relativeErr
		}
		if err := os.Mkdir(filepath.Join(destination, relative), 0o700); err != nil {
			return err
		}
	}
	for _, entry := range files {
		target := filepath.Join(destination, filepath.FromSlash(entry.relative))
		if err := copyStableTreeFile(ctx, entry, target); err != nil {
			return err
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		relative, relativeErr := filepath.Rel(source, directories[index])
		if relativeErr != nil {
			return relativeErr
		}
		target := destination
		if relative != "." {
			target = filepath.Join(destination, relative)
		}
		if err := privatefs.SyncDirectory(target); err != nil {
			return err
		}
	}
	complete = true
	return nil
}

func copyStableTreeFile(ctx context.Context, entry treeFile, destinationPath string) error {
	fd, err := syscall.Open(entry.path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	source := os.NewFile(uintptr(fd), entry.path)
	if source == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open rollback source handle")
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil || !privatefs.SameFileVersion(entry.info, opened) {
		return fmt.Errorf("rollback source changed while opening")
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
		return fmt.Errorf("rollback source changed while copying")
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

func deviceID(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("filesystem device is unavailable")
	}
	return uint64(stat.Dev), nil
}

func rejectLiveDatabaseSidecars(dataDir string) error {
	if _, err := os.Lstat(filepath.Join(dataDir, "db.sqlite3")); err != nil {
		return fmt.Errorf("active database is unavailable")
	}
	for _, name := range []string{"db.sqlite3-wal", "db.sqlite3-shm", "db.sqlite3-journal"} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); err == nil {
			return fmt.Errorf("active database has a live sidecar")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
