package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	SchemaVersion        int               `json:"schema_version"`
	Target               string            `json:"target"`
	SnapshotID           string            `json:"snapshot_id"`
	Output               string            `json:"output"`
	PublishState         string            `json:"publish_state"`
	ReusedExistingOutput bool              `json:"reused_existing_output"`
	Manifest             snapshot.Manifest `json:"manifest"`
}

func Run(ctx context.Context, cfg *config.Backup, runner restic.Runner, request Request) (_ Result, err error) {
	if err := validateOutput(cfg.DataDir, request.Output); err != nil {
		return Result{}, err
	}
	existing, err := outputExists(request.Output)
	if err != nil {
		return Result{}, err
	}
	if existing.exists && !existing.empty {
		return reuseExisting(ctx, runner, request, existing.manifest)
	}
	selected, err := selectSnapshot(ctx, runner, request.Target, request.SnapshotID, request.AllowIncomplete)
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
	if err := matchCandidate(selected, manifest); err != nil {
		return Result{}, err
	}
	if err := snapshot.SyncTree(temp); err != nil {
		return Result{}, fmt.Errorf("sync verified restore tree: %w", err)
	}
	if existing.empty {
		if err := os.Remove(request.Output); err != nil {
			return Result{}, fmt.Errorf("remove still-empty output directory: %w", err)
		}
	}
	if err := os.Rename(temp, request.Output); err != nil {
		return Result{}, fmt.Errorf("publish verified restore directory: %w", err)
	}
	keep = true
	result := Result{
		SchemaVersion: version.SchemaVersion, Target: request.Target.Name, SnapshotID: selected.ID,
		Output: request.Output, PublishState: string(safefs.Durable), ReusedExistingOutput: false, Manifest: manifest,
	}
	if err := syncDirectory(parent); err != nil {
		result.PublishState = string(safefs.PublishedUnconfirmed)
		return result, fmt.Errorf("restore_published_unconfirmed: %w", err)
	}
	return result, nil
}

type outputState struct {
	exists   bool
	empty    bool
	manifest snapshot.Manifest
}

func reuseExisting(ctx context.Context, runner restic.Runner, request Request, manifest snapshot.Manifest) (Result, error) {
	if manifest.Status == snapshot.Incomplete && !request.AllowIncomplete {
		return Result{}, fmt.Errorf("incomplete restore output requires --allow-incomplete")
	}
	report, err := runner.ListSnapshots(ctx, request.Target)
	if err != nil {
		return Result{}, err
	}
	var candidates []restic.SnapshotInfo
	for _, candidate := range report.Snapshots {
		if candidate.ContentID == manifest.ContentID && candidate.Status == manifest.Status &&
			(request.SnapshotID == "" || request.SnapshotID == candidate.ID) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		return Result{}, fmt.Errorf("existing restore output does not match a valid recovery point")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Time == candidates[j].Time {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].Time < candidates[j].Time
	})
	selected := candidates[len(candidates)-1]
	if request.SnapshotID != "" && selected.ID != request.SnapshotID {
		return Result{}, fmt.Errorf("existing restore output does not match requested snapshot")
	}
	if err := snapshot.SyncTree(request.Output); err != nil {
		return Result{}, fmt.Errorf("sync existing restore tree: %w", err)
	}
	if err := syncDirectory(filepath.Dir(request.Output)); err != nil {
		return Result{}, fmt.Errorf("sync restore parent directory: %w", err)
	}
	return Result{
		SchemaVersion: version.SchemaVersion, Target: request.Target.Name, SnapshotID: selected.ID,
		Output: request.Output, PublishState: string(safefs.Durable), ReusedExistingOutput: true, Manifest: manifest,
	}, nil
}

func selectSnapshot(ctx context.Context, runner restic.Runner, target config.Target, requested string, allowIncomplete bool) (restic.SnapshotInfo, error) {
	if requested != "" {
		selected, err := runner.FindSnapshot(ctx, target, requested)
		if err != nil {
			return restic.SnapshotInfo{}, err
		}
		if selected.Status == snapshot.Incomplete && !allowIncomplete {
			return restic.SnapshotInfo{}, fmt.Errorf("incomplete snapshot requires --allow-incomplete")
		}
		return selected, nil
	}
	report, err := runner.ListSnapshots(ctx, target)
	if err != nil {
		return restic.SnapshotInfo{}, err
	}
	for index := len(report.Snapshots) - 1; index >= 0; index-- {
		if report.Snapshots[index].Status == snapshot.Complete {
			return report.Snapshots[index], nil
		}
	}
	return restic.SnapshotInfo{}, fmt.Errorf("no matching valid complete snapshot exists")
}

func matchCandidate(candidate restic.SnapshotInfo, manifest snapshot.Manifest) error {
	if candidate.Status != manifest.Status {
		return fmt.Errorf("snapshot tag and manifest status disagree")
	}
	if candidate.ContentID != manifest.ContentID {
		return fmt.Errorf("snapshot content tag and manifest disagree")
	}
	return nil
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
	if err := safefs.ValidatePrivateDirectory(parent); err != nil || privateDirectoryMode(parent) != 0o700 {
		return fmt.Errorf("restore output parent must be an existing private 0700 directory")
	}
	_, err := outputExists(output)
	return err
}

func outputExists(path string) (outputState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return outputState{}, nil
	}
	if err != nil {
		return outputState{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return outputState{}, fmt.Errorf("restore output must be absent or a 0700 real directory")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return outputState{}, err
	}
	if len(entries) == 0 {
		return outputState{exists: true, empty: true}, nil
	}
	manifest, err := snapshot.Verify(context.Background(), path, true)
	if err != nil {
		return outputState{}, fmt.Errorf("validate existing restore output: %w", err)
	}
	return outputState{exists: true, manifest: manifest}, nil
}

func privateDirectoryMode(path string) os.FileMode {
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return info.Mode().Perm()
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
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
