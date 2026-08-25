package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
)

type changedFileError struct{ reason string }

func (e changedFileError) Error() string { return e.reason }

func copyStableFile(ctx context.Context, dataDir, destinationRoot string, ref referencedFile) (File, error) {
	source, err := openSourceFile(dataDir, ref.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, changedFileError{reason: "not_found"}
		}
		return File{}, fmt.Errorf("open contained source file: %w", err)
	}
	defer func() { _ = source.Close() }()
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return File{}, fmt.Errorf("inspect source file")
	}
	if ref.ExpectedSize >= 0 && before.Size() != ref.ExpectedSize {
		return File{}, changedFileError{reason: "size_mismatch"}
	}

	destinationPath, err := secureJoin(destinationRoot, ref.Path)
	if err != nil {
		return File{}, err
	}
	if err := mkdirAllSecure(filepath.Dir(destinationPath)); err != nil {
		return File{}, fmt.Errorf("create destination directory: %w", err)
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return File{}, fmt.Errorf("create destination file: %w", err)
	}
	keep := false
	defer func() {
		_ = destination.Close()
		if !keep {
			_ = os.Remove(destinationPath)
		}
	}()
	hash := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(destination, hash), source)
	if err != nil {
		return File{}, fmt.Errorf("copy source file: %w", err)
	}
	if err := destination.Sync(); err != nil {
		return File{}, fmt.Errorf("sync destination file: %w", err)
	}
	if err := destination.Chmod(0o600); err != nil {
		return File{}, fmt.Errorf("secure destination file: %w", err)
	}
	after, err := source.Stat()
	if err != nil || !safefs.SameFileVersion(before, after) || written != before.Size() {
		return File{}, changedFileError{reason: "changed_during_copy"}
	}
	latest, err := openSourceFile(dataDir, ref.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, changedFileError{reason: "not_found"}
		}
		return File{}, fmt.Errorf("reopen contained source file: %w", err)
	}
	latestInfo, statErr := latest.Stat()
	closeErr := latest.Close()
	if statErr != nil || closeErr != nil || !safefs.SameFileVersion(before, latestInfo) {
		return File{}, changedFileError{reason: "changed_during_copy"}
	}
	if ref.ExpectedSize >= 0 && written != ref.ExpectedSize {
		return File{}, changedFileError{reason: "size_mismatch"}
	}
	if err := destination.Close(); err != nil {
		return File{}, fmt.Errorf("close destination file: %w", err)
	}
	keep = true
	return File{Path: ref.Path, Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func openSourceFile(dataDir, relative string) (*os.File, error) {
	if !validManifestPath(relative) || !strings.HasPrefix(relative, "data/") {
		return nil, fmt.Errorf("invalid source path")
	}
	cleanRelative := strings.TrimPrefix(relative, "data/")
	if cleanRelative == "" || filepath.IsAbs(cleanRelative) || filepath.Clean(cleanRelative) != cleanRelative || filepath.ToSlash(cleanRelative) != cleanRelative {
		return nil, fmt.Errorf("invalid source path")
	}
	parts := strings.Split(cleanRelative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("invalid source path component")
		}
	}
	directoryFD, err := syscall.Open(dataDir, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range parts[:len(parts)-1] {
		nextFD, openErr := syscall.Openat(directoryFD, part, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(directoryFD)
		if openErr != nil {
			return nil, openErr
		}
		directoryFD = nextFD
	}
	fileFD, err := syscall.Openat(directoryFD, parts[len(parts)-1], syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	_ = syscall.Close(directoryFD)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fileFD), relative)
	if file == nil {
		_ = syscall.Close(fileFD)
		return nil, fmt.Errorf("open source file handle")
	}
	return file, nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 256<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, readErr
		}
	}
}

func hashRegularFile(path string) (File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return File{}, fmt.Errorf("snapshot file is not a secure regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return File{}, err
	}
	return File{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
