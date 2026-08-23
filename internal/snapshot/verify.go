package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxManifestBytes = 64 << 20

func Verify(ctx context.Context, root string, allowIncomplete bool) (Manifest, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0o700 {
		return Manifest{}, fmt.Errorf("snapshot root is not a real 0700 directory")
	}
	manifestPath := filepath.Join(root, "manifest.json")
	data, err := readBoundedRegular(manifestPath, maxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var manifest Manifest
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("manifest contains multiple JSON values")
	}
	if len(manifest.Files) > 1_000_000 || len(manifest.Missing) > 1_000_000 {
		return Manifest{}, fmt.Errorf("manifest contains too many entries")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	if manifest.Status == Incomplete && !allowIncomplete {
		return Manifest{}, fmt.Errorf("incomplete snapshot requires explicit authorization")
	}

	expected := map[string]File{"manifest.json": {Path: "manifest.json"}}
	for _, file := range manifest.Files {
		expected[file.Path] = file
		path, err := secureJoin(root, file.Path)
		if err != nil {
			return Manifest{}, err
		}
		actual, err := hashRegularFile(path)
		if err != nil {
			return Manifest{}, fmt.Errorf("verify %s: %w", file.Path, err)
		}
		if actual.Size != file.Size || actual.SHA256 != file.SHA256 {
			return Manifest{}, fmt.Errorf("file hash or size mismatch for %s", file.Path)
		}
	}
	if err := walkSnapshot(root, expected); err != nil {
		return Manifest{}, err
	}
	databasePath := filepath.Join(root, "data", "db.sqlite3")
	migration, err := validateDatabase(ctx, databasePath)
	if err != nil || migration != manifest.DatabaseMigration {
		return Manifest{}, fmt.Errorf("validate restored database: %w", err)
	}
	refs, err := referencedFiles(ctx, databasePath)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestReferences(manifest, refs); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateManifestReferences(manifest Manifest, refs []referencedFile) error {
	filePaths := make(map[string]struct{}, len(manifest.Files))
	for _, file := range manifest.Files {
		filePaths[file.Path] = struct{}{}
	}
	missingPaths := make(map[string]struct{}, len(manifest.Missing))
	for _, missing := range manifest.Missing {
		missingPaths[missing.Path] = struct{}{}
	}
	referencePaths := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		referencePaths[ref.Path] = struct{}{}
		_, present := filePaths[ref.Path]
		_, absent := missingPaths[ref.Path]
		if present == absent {
			return fmt.Errorf("database reference must appear exactly once in manifest")
		}
	}
	for path := range filePaths {
		if isDatabaseManagedPath(path) {
			if _, ok := referencePaths[path]; !ok {
				return fmt.Errorf("manifest contains a file not referenced by the database")
			}
		}
	}
	for path := range missingPaths {
		if isDatabaseManagedPath(path) {
			if _, ok := referencePaths[path]; !ok {
				return fmt.Errorf("manifest contains a missing file not referenced by the database")
			}
		}
	}
	return nil
}

func isDatabaseManagedPath(path string) bool {
	return strings.HasPrefix(path, "data/attachments/") || strings.HasPrefix(path, "data/sends/")
}

func walkSnapshot(root string, expected map[string]File) error {
	seen := make(map[string]struct{}, len(expected))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot contains a symlink")
		}
		if info.IsDir() {
			if path != root && info.Mode().Perm() != 0o700 {
				return fmt.Errorf("snapshot directory permissions are not 0700")
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("snapshot contains a special or insecure file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, ok := expected[relative]; !ok {
			return fmt.Errorf("snapshot contains an unlisted file")
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("snapshot is missing a listed file")
	}
	return nil
}

func readBoundedRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > limit {
		return nil, fmt.Errorf("file is not a bounded secure regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds size limit")
	}
	return data, nil
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ContentIDHex(contentID string) (string, error) {
	if !strings.HasPrefix(contentID, "sha256:") || !validHash(strings.TrimPrefix(contentID, "sha256:")) {
		return "", fmt.Errorf("invalid content ID")
	}
	return strings.TrimPrefix(contentID, "sha256:"), nil
}
