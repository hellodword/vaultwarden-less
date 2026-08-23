package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

const spaceReserve = int64(64 << 20)

type Builder struct {
	DataDir    string
	StagingDir string
}

type Result struct {
	Path     string
	Manifest Manifest
}

type MissingFilesError struct {
	Count int
}

func (e *MissingFilesError) Error() string {
	return fmt.Sprintf("%d database-referenced or changing files were unavailable", e.Count)
}

// Build creates a verified snapshot in a new private staging directory. The
// caller owns Result.Path. If allowIncomplete is false, any unavailable or
// concurrently changing whitelisted file aborts the attempt.
func (b Builder) Build(ctx context.Context, allowIncomplete bool) (_ Result, err error) {
	if err := validateOwnedDirectory(b.DataDir); err != nil {
		return Result{}, fmt.Errorf("validate data directory: %w", err)
	}
	if err := mkdirAllSecure(b.StagingDir); err != nil {
		return Result{}, fmt.Errorf("prepare staging directory: %w", err)
	}
	buildDir, err := os.MkdirTemp(b.StagingDir, ".build-")
	if err != nil {
		return Result{}, fmt.Errorf("create snapshot staging directory: %w", err)
	}
	if err := os.Chmod(buildDir, 0o700); err != nil {
		_ = os.RemoveAll(buildDir)
		return Result{}, fmt.Errorf("secure snapshot staging directory: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(buildDir)
		}
	}()

	databasePath := filepath.Join(buildDir, "data", "db.sqlite3")
	databaseRequired, err := checkedSizeSum(estimatedDatabaseBytes(b.DataDir), spaceReserve)
	if err != nil {
		return Result{}, err
	}
	if err := preflightSpace(b.StagingDir, databaseRequired); err != nil {
		return Result{}, err
	}
	if err := onlineBackup(ctx, filepath.Join(b.DataDir, "db.sqlite3"), databasePath); err != nil {
		return Result{}, err
	}
	migration, err := validateDatabase(ctx, databasePath)
	if err != nil {
		return Result{}, fmt.Errorf("validate snapshot database: %w", err)
	}
	refs, err := referencedFiles(ctx, databasePath)
	if err != nil {
		return Result{}, err
	}
	optional, optionalBytes, err := b.optionalFiles()
	if err != nil {
		return Result{}, err
	}
	var referencedBytes int64
	for _, ref := range refs {
		referencedBytes, err = checkedSizeSum(referencedBytes, ref.ExpectedSize)
		if err != nil {
			return Result{}, fmt.Errorf("referenced file size overflow")
		}
	}
	recoveryRequired, err := checkedSizeSum(referencedBytes, optionalBytes, spaceReserve)
	if err != nil {
		return Result{}, err
	}
	if err := preflightSpace(b.StagingDir, recoveryRequired); err != nil {
		return Result{}, err
	}
	refs = append(refs, optional...)
	slices.SortFunc(refs, func(a, c referencedFile) int { return strings.Compare(a.Path, c.Path) })
	for i := 1; i < len(refs); i++ {
		if refs[i-1].Path == refs[i].Path {
			return Result{}, fmt.Errorf("duplicate source path in snapshot set")
		}
	}

	databaseFile, err := hashRegularFile(databasePath)
	if err != nil {
		return Result{}, fmt.Errorf("hash snapshot database: %w", err)
	}
	databaseFile.Path = "data/db.sqlite3"
	files := []File{databaseFile}
	missing := make([]Missing, 0)
	for _, ref := range refs {
		file, err := copyStableFile(ctx, b.DataDir, buildDir, ref)
		if err == nil {
			files = append(files, file)
			continue
		}
		var changed changedFileError
		if errors.As(err, &changed) {
			missing = append(missing, Missing{Path: ref.Path, Reason: changed.reason})
			continue
		}
		return Result{}, fmt.Errorf("copy %s: %w", ref.Path, err)
	}
	if len(missing) != 0 && !allowIncomplete {
		return Result{}, &MissingFilesError{Count: len(missing)}
	}
	status := Complete
	if len(missing) != 0 {
		status = Incomplete
	}
	manifest, err := NewManifest(status, migration, files, missing)
	if err != nil {
		return Result{}, err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("encode manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	manifestPath := filepath.Join(buildDir, "manifest.json")
	manifestFile, err := os.OpenFile(manifestPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("create manifest: %w", err)
	}
	if _, err := manifestFile.Write(manifestBytes); err != nil {
		manifestFile.Close()
		return Result{}, fmt.Errorf("write manifest: %w", err)
	}
	if err := manifestFile.Sync(); err != nil {
		manifestFile.Close()
		return Result{}, fmt.Errorf("sync manifest: %w", err)
	}
	if err := manifestFile.Close(); err != nil {
		return Result{}, fmt.Errorf("close manifest: %w", err)
	}
	if _, err := Verify(ctx, buildDir, true); err != nil {
		return Result{}, fmt.Errorf("verify completed staging set: %w", err)
	}
	if err := SyncTree(buildDir); err != nil {
		return Result{}, fmt.Errorf("sync snapshot directory tree: %w", err)
	}
	keep = true
	return Result{Path: buildDir, Manifest: manifest}, nil
}

func (b Builder) optionalFiles() ([]referencedFile, int64, error) {
	entries, err := os.ReadDir(b.DataDir)
	if err != nil {
		return nil, 0, fmt.Errorf("list data directory whitelist: %w", err)
	}
	var refs []referencedFile
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if name != "config.json" && !strings.HasPrefix(name, "rsa_key") {
			continue
		}
		if strings.Contains(name, "/") || name == "rsa_key" && entry.IsDir() {
			return nil, 0, fmt.Errorf("invalid whitelisted data entry")
		}
		info, err := entry.Info()
		if err != nil {
			return nil, 0, fmt.Errorf("inspect whitelisted data entry: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("whitelisted data entry is not a regular file")
		}
		refs = append(refs, referencedFile{Path: "data/" + name, ExpectedSize: info.Size()})
		total, err = checkedSizeSum(total, info.Size())
		if err != nil {
			return nil, 0, fmt.Errorf("optional file size overflow")
		}
	}
	templatesRoot := filepath.Join(b.DataDir, "templates")
	info, err := os.Lstat(templatesRoot)
	if err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("templates is not a real directory")
		}
		err = filepath.WalkDir(templatesRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == templatesRoot {
				return nil
			}
			entryInfo, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if entryInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("templates contains a symlink")
			}
			if entryInfo.IsDir() {
				return nil
			}
			if !entryInfo.Mode().IsRegular() {
				return fmt.Errorf("templates contains a special file")
			}
			relative, err := filepath.Rel(b.DataDir, path)
			if err != nil {
				return err
			}
			refs = append(refs, referencedFile{Path: "data/" + filepath.ToSlash(relative), ExpectedSize: entryInfo.Size()})
			total, err = checkedSizeSum(total, entryInfo.Size())
			if err != nil {
				return fmt.Errorf("optional file size overflow")
			}
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("walk templates whitelist: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, fmt.Errorf("inspect templates whitelist: %w", err)
	}
	return refs, total, nil
}

func validateOwnedDirectory(path string) error {
	if path == string(filepath.Separator) {
		return fmt.Errorf("filesystem root is not a supported data directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("path is not a real 0700 directory")
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

// SyncTree flushes every regular file and then every directory from leaves to
// root so a subsequently synced rename cannot publish only part of a verified
// recovery set after a crash.
func SyncTree(root string) error {
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("snapshot tree contains a non-regular file")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}); err != nil {
		return err
	}
	slices.SortFunc(directories, func(left, right string) int {
		return len(right) - len(left)
	})
	for _, path := range directories {
		if err := syncDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
