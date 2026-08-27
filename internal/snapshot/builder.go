package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

const spaceReserve = int64(64 << 20)

type Builder struct {
	DataDir    string
	StagingDir string

	beforeCopy func(string)
	afterCopy  func(string)
}

// Build constructs and verifies a complete recovery point, then publishes it
// as StagingDir/point. A failed build leaves any prior point untouched.
func (b Builder) Build(ctx context.Context) (_ Manifest, err error) {
	if err := validateOwnedDirectory(b.DataDir); err != nil {
		return Manifest{}, fmt.Errorf("validate data directory: %w", err)
	}
	if err := validateOwnedDirectory(b.StagingDir); err != nil {
		return Manifest{}, fmt.Errorf("validate staging directory: %w", err)
	}
	if err := cleanupTransientBuilds(b.StagingDir); err != nil {
		return Manifest{}, fmt.Errorf("clean transient recovery builds: %w", err)
	}
	buildDir, err := os.MkdirTemp(b.StagingDir, ".build-")
	if err != nil {
		return Manifest{}, fmt.Errorf("create recovery build directory: %w", err)
	}
	if err := os.Chmod(buildDir, 0o700); err != nil {
		_ = os.RemoveAll(buildDir)
		return Manifest{}, fmt.Errorf("secure recovery build directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(buildDir)
		}
	}()

	databasePath := filepath.Join(buildDir, "data", "db.sqlite3")
	databaseRequired, err := checkedSizeSum(estimatedDatabaseBytes(b.DataDir), spaceReserve)
	if err != nil {
		return Manifest{}, err
	}
	if err := preflightSpace(b.StagingDir, databaseRequired); err != nil {
		return Manifest{}, err
	}
	if err := onlineBackup(ctx, filepath.Join(b.DataDir, "db.sqlite3"), databasePath); err != nil {
		return Manifest{}, err
	}
	if _, err := validateDatabase(ctx, databasePath); err != nil {
		return Manifest{}, fmt.Errorf("validate recovery database: %w", err)
	}
	refs, err := referencedFiles(ctx, databasePath)
	if err != nil {
		return Manifest{}, err
	}
	var referencedBytes int64
	for _, ref := range refs {
		referencedBytes, err = checkedSizeSum(referencedBytes, ref.ExpectedSize)
		if err != nil {
			return Manifest{}, fmt.Errorf("referenced file size overflow")
		}
	}
	// The online backup is already present on the staging filesystem, so the
	// second free-space reading must account only for files still to be copied.
	recoveryRequired, err := checkedSizeSum(referencedBytes, spaceReserve)
	if err != nil {
		return Manifest{}, err
	}
	if err := preflightSpace(b.StagingDir, recoveryRequired); err != nil {
		return Manifest{}, err
	}

	databaseFile, err := hashRegularFile(ctx, databasePath)
	if err != nil {
		return Manifest{}, fmt.Errorf("hash recovery database: %w", err)
	}
	databaseFile.Path = "data/db.sqlite3"
	files := []File{databaseFile}
	for _, ref := range refs {
		if b.beforeCopy != nil {
			b.beforeCopy(ref.Path)
		}
		var afterRead func()
		if b.afterCopy != nil {
			afterRead = func() { b.afterCopy(ref.Path) }
		}
		file, err := copyStableFile(ctx, b.DataDir, buildDir, ref, afterRead)
		if err != nil {
			return Manifest{}, fmt.Errorf("copy %s: %w", ref.Path, err)
		}
		files = append(files, file)
	}

	manifest, err := NewManifest(files)
	if err != nil {
		return Manifest{}, err
	}
	if err := writeManifest(filepath.Join(buildDir, "manifest.json"), manifest); err != nil {
		return Manifest{}, err
	}
	if _, err := Verify(ctx, buildDir); err != nil {
		return Manifest{}, fmt.Errorf("verify recovery build: %w", err)
	}

	point := filepath.Join(b.StagingDir, "point")
	newPointPublished, err := publishRecoveryPoint(buildDir, point)
	if newPointPublished {
		published = true
	}
	if err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func publishRecoveryPoint(buildDir, point string) (bool, error) {
	return publishRecoveryPointUsing(
		buildDir,
		point,
		privatefs.RenameNoReplace,
		privatefs.RenameExchange,
		privatefs.SyncDirectory,
	)
}

func publishRecoveryPointUsing(
	buildDir, point string,
	noReplace func(string, string) error,
	exchange func(string, string) error,
	syncDirectory func(string) error,
) (bool, error) {
	parent := filepath.Dir(point)
	if _, err := os.Lstat(point); errors.Is(err, os.ErrNotExist) {
		if err := noReplace(buildDir, point); err != nil {
			return false, fmt.Errorf("publish first recovery point: %w", err)
		}
		if err := syncDirectory(parent); err != nil {
			return true, fmt.Errorf("sync first recovery point publication: %w", err)
		}
		return true, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect existing recovery point: %w", err)
	}
	if err := validateOwnedDirectory(point); err != nil {
		return false, fmt.Errorf("validate existing recovery point: %w", err)
	}
	if err := exchange(buildDir, point); err != nil {
		return false, fmt.Errorf("atomically replace recovery point: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return true, fmt.Errorf("sync recovery point replacement: %w", err)
	}
	// The new verified point is already authoritative. The old point now has
	// the recognized build name and is safe for this or the next build to clean.
	if err := os.RemoveAll(buildDir); err != nil {
		return true, fmt.Errorf("clean replaced recovery point: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return true, fmt.Errorf("sync replaced recovery point cleanup: %w", err)
	}
	return true, nil
}

func writeManifest(path string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create manifest: %w", err)
	}
	open := true
	defer func() {
		if open {
			_ = file.Close()
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync manifest: %w", err)
	}
	if err := file.Close(); err != nil {
		open = false
		return fmt.Errorf("close manifest: %w", err)
	}
	open = false
	return nil
}

func cleanupTransientBuilds(stagingDir string) error {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".build-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(stagingDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func validateOwnedDirectory(path string) error {
	if path == "" || path == string(filepath.Separator) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path must be clean, absolute, and non-root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ownerKnown := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownerKnown || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("path must be a real 0700 directory owned by effective UID %d", os.Geteuid())
	}
	return nil
}

func estimatedDatabaseBytes(dataDir string) int64 {
	var total int64
	for _, name := range []string{"db.sqlite3", "db.sqlite3-wal"} {
		if info, err := os.Lstat(filepath.Join(dataDir, name)); err == nil && info.Mode().IsRegular() {
			if info.Size() > math.MaxInt64-total {
				return math.MaxInt64
			}
			total += info.Size()
		}
	}
	return total
}

func checkedSizeSum(values ...int64) (int64, error) {
	var total int64
	for _, value := range values {
		if value < 0 || value > math.MaxInt64-total {
			return 0, fmt.Errorf("required staging space overflow")
		}
		total += value
	}
	return total, nil
}

func preflightSpace(path string, required int64) error {
	if required < 0 {
		return fmt.Errorf("required staging space overflow")
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return fmt.Errorf("inspect staging free space: %w", err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available < 0 || available < required {
		return fmt.Errorf("insufficient staging space: need at least %d bytes", required)
	}
	return nil
}
