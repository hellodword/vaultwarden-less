package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

const ResultSchemaVersion = 1

type Repository interface {
	ResolveSnapshot(context.Context, config.Target, string) (restic.Snapshot, error)
	Restore(context.Context, config.Target, string, string) error
}

type Request struct {
	Target      config.Target
	SnapshotID  string
	Output      string
	RestoreRoot string
}

type Result struct {
	SchemaVersion int    `json:"schema_version"`
	Target        string `json:"target"`
	SnapshotID    string `json:"snapshot_id"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
	Output        string `json:"output"`
}

func Run(ctx context.Context, repository Repository, request Request) (_ Result, err error) {
	if err := validateOutput(request.RestoreRoot, request.Output); err != nil {
		return Result{}, err
	}
	selected, err := repository.ResolveSnapshot(ctx, request.Target, request.SnapshotID)
	if err != nil {
		return Result{}, err
	}
	parent := filepath.Dir(request.Output)
	temporary, err := os.MkdirTemp(parent, ".vaultwarden-less-restore-")
	if err != nil {
		return Result{}, fmt.Errorf("create restore temporary directory: %w", err)
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		_ = os.RemoveAll(temporary)
		return Result{}, fmt.Errorf("secure restore temporary directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := repository.Restore(ctx, request.Target, selected.ID, temporary); err != nil {
		return Result{}, err
	}
	manifest, err := snapshot.Verify(ctx, temporary)
	if err != nil {
		return Result{}, fmt.Errorf("verify restored recovery point: %w", err)
	}
	if manifest.ProjectCommit != selected.ProjectCommit || manifest.ContentID != selected.ContentID || manifest.ArchiveID != selected.ArchiveID {
		return Result{}, fmt.Errorf("restored manifest does not match snapshot identity tags")
	}
	if err := privatefs.RenameNoReplace(temporary, request.Output); err != nil {
		return Result{}, fmt.Errorf("publish verified restore directory: %w", err)
	}
	published = true
	if err := privatefs.SyncDirectory(parent); err != nil {
		return Result{}, fmt.Errorf("sync verified restore publication: %w", err)
	}
	return Result{
		SchemaVersion: ResultSchemaVersion,
		Target:        request.Target.Name,
		SnapshotID:    selected.ID,
		ProjectCommit: manifest.ProjectCommit,
		ContentID:     manifest.ContentID,
		ArchiveID:     manifest.ArchiveID,
		Output:        request.Output,
	}, nil
}

func validateOutput(restoreRoot, output string) error {
	if restoreRoot == "" || !filepath.IsAbs(restoreRoot) || filepath.Clean(restoreRoot) != restoreRoot || restoreRoot == string(filepath.Separator) {
		return fmt.Errorf("restore root must be a clean absolute non-root path")
	}
	if err := exactPrivateDirectory(restoreRoot); err != nil {
		return fmt.Errorf("restore root: %w", err)
	}
	if output == "" || !filepath.IsAbs(output) || filepath.Clean(output) != output || output == string(filepath.Separator) {
		return fmt.Errorf("output must be a clean absolute non-root path")
	}
	relative, err := filepath.Rel(restoreRoot, output)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("output must be a child of the restore root")
	}
	parent := filepath.Dir(output)
	if err := realDirectoryChain(restoreRoot, parent); err != nil {
		return fmt.Errorf("restore output parent: %w", err)
	}
	if err := exactPrivateDirectory(parent); err != nil {
		return fmt.Errorf("restore output parent: %w", err)
	}
	if _, err := os.Lstat(output); err == nil {
		return fmt.Errorf("restore output already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect restore output: %w", err)
	}
	return nil
}

func exactPrivateDirectory(path string) error {
	if err := privatefs.ValidatePrivateDirectory(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("directory mode must be 0700")
	}
	return nil
}

func realDirectoryChain(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("parent escapes restore root")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent chain contains a symlink or non-directory")
		}
	}
	return nil
}
