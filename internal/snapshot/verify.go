package snapshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/strictjson"
)

const maxManifestBytes = 64 << 20

func DecodeManifest(data []byte) (Manifest, error) {
	if int64(len(data)) > maxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest exceeds size limit")
	}
	if !utf8.Valid(data) {
		return Manifest{}, fmt.Errorf("manifest is not valid UTF-8")
	}
	if err := strictjson.RejectDuplicateKeys(data); err != nil {
		return Manifest{}, fmt.Errorf("manifest JSON is invalid: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("manifest contains multiple JSON values")
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Verify(ctx context.Context, root string) (Manifest, error) {
	return verifyOwned(ctx, root, uint32(os.Geteuid()), uint32(os.Getegid()))
}

func verifyOwned(ctx context.Context, root string, uid, gid uint32) (Manifest, error) {
	if err := validateDirectoryOwnership(root, uid, gid); err != nil {
		return Manifest{}, fmt.Errorf("recovery root is invalid: %w", err)
	}
	manifestData, err := readBoundedRegular(filepath.Join(root, "manifest.json"), maxManifestBytes)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	manifest, err := DecodeManifest(manifestData)
	if err != nil {
		return Manifest{}, err
	}

	expectedFiles := map[string]File{"manifest.json": {Path: "manifest.json"}}
	allowedDirectories := map[string]struct{}{root: {}}
	for _, file := range manifest.Files {
		expectedFiles[file.Path] = file
		path, err := secureJoin(root, file.Path)
		if err != nil {
			return Manifest{}, err
		}
		actual, err := hashRegularFile(ctx, path)
		if err != nil {
			return Manifest{}, fmt.Errorf("verify %s: %w", file.Path, err)
		}
		if actual.Size != file.Size || actual.SHA256 != file.SHA256 {
			return Manifest{}, fmt.Errorf("file hash or size mismatch for %s", file.Path)
		}
		for directory := filepath.Dir(path); directory != root; directory = filepath.Dir(directory) {
			allowedDirectories[directory] = struct{}{}
		}
	}
	if err := walkRecoveryTree(root, expectedFiles, allowedDirectories, uid, gid); err != nil {
		return Manifest{}, err
	}

	databasePath := filepath.Join(root, "db.sqlite3")
	if _, err := validateDatabase(ctx, databasePath); err != nil {
		return Manifest{}, fmt.Errorf("validate restored database: %w", err)
	}
	refs, err := referencedFiles(ctx, databasePath)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifestReferences(manifest, refs); err != nil {
		return Manifest{}, err
	}
	contentID, err := calculateContentID(ctx, databasePath, manifest.ProjectCommit, manifest.Files)
	if err != nil {
		return Manifest{}, fmt.Errorf("recalculate logical recovery identity: %w", err)
	}
	if contentID != manifest.ContentID {
		return Manifest{}, fmt.Errorf("manifest content_id mismatch")
	}
	archiveID, err := calculateArchiveID(ctx, root, manifest.Files)
	if err != nil {
		return Manifest{}, fmt.Errorf("recalculate archive recovery identity: %w", err)
	}
	if archiveID != manifest.ArchiveID {
		return Manifest{}, fmt.Errorf("manifest archive_id mismatch")
	}
	return manifest, nil
}

func validateManifestReferences(manifest Manifest, refs []referencedFile) error {
	files := make(map[string]File, len(manifest.Files))
	for _, file := range manifest.Files {
		if file.Path != "db.sqlite3" {
			files[file.Path] = file
		}
	}
	if len(files) != len(refs) {
		return fmt.Errorf("manifest path set does not match database references")
	}
	for _, ref := range refs {
		file, exists := files[ref.Path]
		if !exists || file.Size != ref.ExpectedSize {
			return fmt.Errorf("manifest path set does not match database references")
		}
	}
	return nil
}

func walkRecoveryTree(
	root string,
	expectedFiles map[string]File,
	allowedDirectories map[string]struct{},
	uid, gid uint32,
) error {
	seen := make(map[string]struct{}, len(expectedFiles))
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ownerKnown := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSymlink != 0 || !ownerKnown || stat.Uid != uid || stat.Gid != gid {
			return fmt.Errorf("recovery tree contains an unsafe entry")
		}
		if info.IsDir() {
			if info.Mode().Perm() != 0o700 {
				return fmt.Errorf("recovery directory permissions are not 0700")
			}
			if _, allowed := allowedDirectories[path]; !allowed {
				return fmt.Errorf("recovery tree contains an unlisted directory")
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("recovery tree contains a special or insecure file")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if _, expected := expectedFiles[relative]; !expected {
			return fmt.Errorf("recovery tree contains an unlisted file")
		}
		seen[relative] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(expectedFiles) {
		return fmt.Errorf("recovery tree is missing a listed file")
	}
	return nil
}

func readBoundedRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > limit {
		return nil, fmt.Errorf("file is not a bounded 0600 regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds size limit")
	}
	return data, nil
}

func IsContentID(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHash(strings.TrimPrefix(value, "sha256:"))
}
