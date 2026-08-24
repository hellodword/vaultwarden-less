package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/safefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

type Request struct {
	Target          config.Target
	SnapshotID      string
	AllowIncomplete bool
	Output          string
}

type Result struct {
	SchemaVersion int               `json:"schema_version"`
	Target        string            `json:"target"`
	SnapshotID    string            `json:"snapshot_id"`
	Output        string            `json:"output"`
	Manifest      snapshot.Manifest `json:"manifest"`
}

func Run(ctx context.Context, cfg *config.Backup, runner restic.Runner, request Request) (_ Result, err error) {
	if err := validateOutput(cfg.DataDir, request.Output); err != nil {
		return Result{}, err
	}
	selected, status, err := selectSnapshot(ctx, runner, request.Target, request.SnapshotID, request.AllowIncomplete)
	if err != nil {
		return Result{}, err
	}
	parent := filepath.Dir(request.Output)
	temp, err := os.MkdirTemp(parent, ".vaultwarden-less-restore-")
	if err != nil {
		return Result{}, fmt.Errorf("create restore transaction: %w", err)
	}
	if err := os.Chmod(temp, 0o700); err != nil {
		_ = os.RemoveAll(temp)
		return Result{}, fmt.Errorf("secure restore transaction: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(temp)
		}
	}()
	if err := runner.Restore(ctx, request.Target, selected.ID, temp); err != nil {
		return Result{}, err
	}
	manifest, err := snapshot.Verify(ctx, temp, request.AllowIncomplete)
	if err != nil {
		return Result{}, fmt.Errorf("verify restored recovery point: %w", err)
	}
	if manifest.Status != status {
		return Result{}, fmt.Errorf("snapshot tag and manifest status disagree")
	}
	contentHex, err := snapshot.ContentIDHex(manifest.ContentID)
	if err != nil || !slices.Contains(selected.Tags, "content-id="+contentHex) {
		return Result{}, fmt.Errorf("snapshot content tag and manifest disagree")
	}
	if err := snapshot.SyncTree(temp); err != nil {
		return Result{}, fmt.Errorf("sync verified restore tree: %w", err)
	}
	outputExisted, err := emptyOutputState(request.Output)
	if err != nil {
		return Result{}, err
	}
	if outputExisted {
		if err := os.Remove(request.Output); err != nil {
			return Result{}, fmt.Errorf("remove still-empty output directory: %w", err)
		}
	}
	if err := os.Rename(temp, request.Output); err != nil {
		if outputExisted {
			_ = os.Mkdir(request.Output, 0o700)
		}
		return Result{}, fmt.Errorf("publish verified restore directory: %w", err)
	}
	keep = true
	if err := syncDirectory(parent); err != nil {
		return Result{}, fmt.Errorf("sync restore parent directory: %w", err)
	}
	return Result{SchemaVersion: version.SchemaVersion, Target: request.Target.Name, SnapshotID: selected.ID, Output: request.Output, Manifest: manifest}, nil
}

func selectSnapshot(ctx context.Context, runner restic.Runner, target config.Target, requested string, allowIncomplete bool) (restic.SnapshotInfo, snapshot.Status, error) {
	filter := snapshot.Complete
	if requested != "" {
		filter = ""
	}
	snapshots, err := runner.Snapshots(ctx, target, filter)
	if err != nil {
		return restic.SnapshotInfo{}, "", err
	}
	if len(snapshots) == 0 {
		return restic.SnapshotInfo{}, "", fmt.Errorf("no matching v1 snapshot exists")
	}
	selected := snapshots[len(snapshots)-1]
	if requested != "" {
		found := false
		for _, candidate := range snapshots {
			if candidate.ID == requested {
				selected = candidate
				found = true
				break
			}
		}
		if !found {
			return restic.SnapshotInfo{}, "", fmt.Errorf("requested snapshot is not a v1 Vaultwarden recovery point")
		}
	}
	status := snapshot.Status("")
	if slices.Contains(selected.Tags, string(snapshot.Complete)) {
		status = snapshot.Complete
	}
	if slices.Contains(selected.Tags, string(snapshot.Incomplete)) {
		if status != "" {
			return restic.SnapshotInfo{}, "", fmt.Errorf("snapshot has conflicting integrity tags")
		}
		status = snapshot.Incomplete
	}
	if status == "" {
		return restic.SnapshotInfo{}, "", fmt.Errorf("snapshot lacks a v1 integrity tag")
	}
	if status == snapshot.Incomplete && !allowIncomplete {
		return restic.SnapshotInfo{}, "", fmt.Errorf("incomplete snapshot requires --allow-incomplete")
	}
	return selected, status, nil
}

func validateOutput(dataDir, output string) error {
	if output == "" || !filepath.IsAbs(output) || filepath.Clean(output) != output || output == string(filepath.Separator) {
		return fmt.Errorf("output must be a clean absolute non-root path")
	}
	if overlap(dataDir, output) {
		return fmt.Errorf("restore output must not overlap online data_dir")
	}
	parent := filepath.Dir(output)
	if err := realDirectoryChain(parent); err != nil {
		return fmt.Errorf("restore output parent: %w", err)
	}
	if err := safefs.ValidatePrivateDirectory(parent); err != nil {
		return fmt.Errorf("restore output parent must be an existing private directory: %w", err)
	}
	_, err := emptyOutputState(output)
	return err
}

func emptyOutputState(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("restore output must be absent or an empty real directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	if len(entries) != 0 {
		return false, fmt.Errorf("restore output directory is not empty")
	}
	return true, nil
}

func realDirectoryChain(path string) error {
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(strings.TrimPrefix(path, volume), string(filepath.Separator))
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" {
			continue
		}
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

func overlap(left, right string) bool {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
