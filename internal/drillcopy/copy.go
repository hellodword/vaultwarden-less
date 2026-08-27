package drillcopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

const (
	SourceRoot      = "/source"
	DestinationRoot = "/drill-data"
)

// ParseRuntimeIdentity parses the canonical non-root identity that will own the
// disposable Vaultwarden data directory.
func ParseRuntimeIdentity(rawUID, rawGID string) (uint32, uint32, error) {
	uid, err := parseRuntimeID(rawUID)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid drill runtime UID")
	}
	gid, err := parseRuntimeID(rawGID)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid drill runtime GID")
	}
	return uid, gid, nil
}

func parseRuntimeID(raw string) (uint32, error) {
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 || value == math.MaxUint32 || strconv.FormatUint(value, 10) != raw {
		return 0, fmt.Errorf("runtime ID must be canonical non-root uint32")
	}
	return uint32(value), nil
}

// Run verifies one complete recovery point and copies only its manifest files
// into an initially empty disposable Vaultwarden data directory owned by the
// explicitly configured runtime identity.
func Run(ctx context.Context, sourceRoot, destinationRoot string, uid, gid uint32) error {
	if os.Geteuid() != 0 || os.Getegid() != 0 {
		return fmt.Errorf("drill copy helper must run as root")
	}
	if uid == 0 || gid == 0 || uid == math.MaxUint32 || gid == math.MaxUint32 {
		return fmt.Errorf("drill runtime identity is invalid")
	}
	manifest, err := snapshot.VerifyOwned(ctx, sourceRoot, uid, gid)
	if err != nil {
		return fmt.Errorf("verify drill source: %w", err)
	}
	return copyManifestFiles(ctx, sourceRoot, destinationRoot, manifest.Files, uid, gid)
}

func copyManifestFiles(
	ctx context.Context,
	sourceRoot, destinationRoot string,
	files []snapshot.File,
	uid, gid uint32,
) error {
	if err := validateEmptyDestination(destinationRoot); err != nil {
		return err
	}
	if err := os.Chown(destinationRoot, int(uid), int(gid)); err != nil {
		return fmt.Errorf("own drill destination: %w", err)
	}
	if err := os.Chmod(destinationRoot, 0o700); err != nil {
		return fmt.Errorf("secure drill destination: %w", err)
	}
	for _, listed := range files {
		source, err := containedPath(sourceRoot, listed.Path)
		if err != nil {
			return err
		}
		destination, err := containedPath(destinationRoot, listed.Path)
		if err != nil {
			return err
		}
		if err := makeOwnedParents(destinationRoot, filepath.Dir(destination), uid, gid); err != nil {
			return err
		}
		if err := copyListedFile(ctx, source, destination, listed, uid, gid); err != nil {
			return fmt.Errorf("copy %s: %w", listed.Path, err)
		}
	}
	directory, err := os.Open(destinationRoot)
	if err != nil {
		return fmt.Errorf("open drill destination: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync drill destination: %w", err)
	}
	return nil
}

func validateEmptyDestination(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return fmt.Errorf("drill destination must be a clean absolute non-root path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect drill destination: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("drill destination must be a real directory")
	}
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open drill destination: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if _, err := directory.Readdirnames(1); !errors.Is(err, io.EOF) {
		return fmt.Errorf("drill destination must be empty")
	}
	return nil
}

func containedPath(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || filepath.ToSlash(relative) != relative {
		return "", fmt.Errorf("invalid drill recovery path")
	}
	joined := filepath.Join(root, filepath.FromSlash(relative))
	relativeToRoot, err := filepath.Rel(root, joined)
	if err != nil || relativeToRoot == ".." || strings.HasPrefix(relativeToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("drill recovery path escapes root")
	}
	return joined, nil
}

func makeOwnedParents(root, parent string, uid, gid uint32) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("drill destination parent escapes root")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create drill directory: %w", err)
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("drill destination contains an unsafe directory")
		}
		if err := os.Chown(current, int(uid), int(gid)); err != nil {
			return fmt.Errorf("own drill directory: %w", err)
		}
		if err := os.Chmod(current, 0o700); err != nil {
			return fmt.Errorf("secure drill directory: %w", err)
		}
	}
	return nil
}

func copyListedFile(
	ctx context.Context,
	sourcePath, destinationPath string,
	listed snapshot.File,
	uid, gid uint32,
) error {
	info, err := os.Lstat(sourcePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != listed.Size {
		return fmt.Errorf("source is not the listed 0600 regular file")
	}
	fd, err := syscall.Open(sourcePath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open drill source: %w", err)
	}
	source := os.NewFile(uintptr(fd), sourcePath)
	if source == nil {
		_ = syscall.Close(fd)
		return fmt.Errorf("open drill source handle")
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil || !privatefs.SameFileVersion(info, opened) {
		return fmt.Errorf("source changed while opening")
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create drill destination file: %w", err)
	}
	keep := false
	defer func() {
		_ = destination.Close()
		if !keep {
			_ = os.Remove(destinationPath)
		}
	}()
	digest := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(destination, digest), source)
	if err != nil {
		return err
	}
	after, statErr := source.Stat()
	if statErr != nil || !privatefs.SameFileVersion(opened, after) || written != listed.Size ||
		"sha256:"+hex.EncodeToString(digest.Sum(nil)) != listed.SHA256 {
		return fmt.Errorf("source changed or did not match the manifest")
	}
	if err := destination.Chown(int(uid), int(gid)); err != nil {
		return fmt.Errorf("own drill destination file: %w", err)
	}
	if err := destination.Chmod(0o600); err != nil {
		return fmt.Errorf("secure drill destination file: %w", err)
	}
	if err := destination.Sync(); err != nil {
		return fmt.Errorf("sync drill destination file: %w", err)
	}
	if err := destination.Close(); err != nil {
		return fmt.Errorf("close drill destination file: %w", err)
	}
	keep = true
	return nil
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
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
