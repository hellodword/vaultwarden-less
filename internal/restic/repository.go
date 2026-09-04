package restic

import (
	"bytes"
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

func (r Runner) VerifyVersion(ctx context.Context) error {
	output, err := r.runLogged(ctx, "version", "", r.binary, []string{"version", "--json"}, sanitizedEnvironment(nil), "")
	if err != nil {
		return err
	}
	frames, err := decodeJSONFrames(output)
	if err != nil || len(frames) != 1 {
		return &OperationError{Code: "version_mismatch", ExitCode: -1}
	}
	var value struct {
		MessageType string `json:"message_type"`
		Version     string `json:"version"`
	}
	if err := strictUnmarshal(frames[0], &value); err != nil || value.MessageType != "version" || !resticVersionPattern.MatchString(value.Version) {
		return &OperationError{Code: "version_mismatch", ExitCode: -1}
	}
	return nil
}

func (r Runner) Ensure(ctx context.Context, target config.Target, manifest snapshot.Manifest) (SyncResult, error) {
	if err := manifest.Validate(); err != nil {
		return SyncResult{}, &OperationError{Code: "invalid_manifest", ExitCode: -1}
	}
	latest, err := r.Latest(ctx, target)
	if err != nil {
		return SyncResult{}, err
	}
	if latest != nil && latest.ContentID == manifest.ContentID {
		return SyncResult{SnapshotID: latest.ID, ContentID: manifest.ContentID, ArchiveID: latest.ArchiveID, Created: false}, nil
	}
	base, environment, err := r.targetCommand(target)
	if err != nil {
		return SyncResult{}, err
	}
	contentHex, err := snapshot.ContentIDHex(manifest.ContentID)
	if err != nil {
		return SyncResult{}, &OperationError{Code: "invalid_content_id", ExitCode: -1}
	}
	arguments := append(base,
		"backup", "--json",
		"--host", SnapshotHost,
		"--tag", "project_commit="+manifest.ProjectCommit,
		"--tag", "content_id=sha256:"+contentHex,
		"--tag", "archive_id="+manifest.ArchiveID,
		"--", r.recoveryPath,
	)
	output, err := r.runTargetCommand(ctx, target, "backup", arguments, environment, "")
	if err != nil {
		return SyncResult{}, err
	}
	snapshotID, err := parseBackupSnapshotID(output)
	if err != nil {
		return SyncResult{}, &OperationError{Code: "backup_result_invalid", ExitCode: -1}
	}
	result := SyncResult{
		SnapshotID: snapshotID, ContentID: manifest.ContentID,
		ArchiveID: manifest.ArchiveID, Created: true,
	}
	if r.logger != nil {
		r.logger.Info(
			"restic recovery snapshot synchronized",
			"target", target.Name,
			"snapshot_id", result.SnapshotID,
			"content_id", result.ContentID,
			"created", result.Created,
		)
	}
	return result, nil
}

func (r Runner) Latest(ctx context.Context, target config.Target) (*Snapshot, error) {
	items, err := r.snapshotQuery(ctx, target, []string{"--latest", "1"})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) != 1 {
		return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
	}
	return &items[0], nil
}

func (r Runner) Snapshots(ctx context.Context, target config.Target, limit int) ([]Snapshot, bool, error) {
	if limit < 1 || limit > MaxSnapshotsLimit {
		return nil, false, &OperationError{Code: "snapshots_limit_invalid", ExitCode: -1}
	}
	items, err := r.snapshotQuery(ctx, target, []string{"--latest", strconv.Itoa(limit + 1)})
	if err != nil {
		return nil, false, err
	}
	sort.Slice(items, func(left, right int) bool {
		leftTime, _ := time.Parse(time.RFC3339Nano, items[left].Time)
		rightTime, _ := time.Parse(time.RFC3339Nano, items[right].Time)
		if leftTime.Equal(rightTime) {
			return items[left].ID > items[right].ID
		}
		return leftTime.After(rightTime)
	})
	truncated := len(items) > limit
	if truncated {
		items = items[:limit]
	}
	return items, truncated, nil
}

func (r Runner) ResolveSnapshot(ctx context.Context, target config.Target, requested string) (Snapshot, error) {
	if requested == "" {
		latest, err := r.Latest(ctx, target)
		if err != nil {
			return Snapshot{}, err
		}
		if latest == nil {
			return Snapshot{}, &OperationError{Code: "snapshot_not_found", ExitCode: -1}
		}
		return *latest, nil
	}
	if !validSnapshotID(requested) {
		return Snapshot{}, &OperationError{Code: "snapshot_id_invalid", ExitCode: -1}
	}
	items, err := r.snapshotQuery(ctx, target, []string{requested})
	if err != nil {
		return Snapshot{}, err
	}
	if len(items) != 1 || items[0].ID != requested {
		return Snapshot{}, &OperationError{Code: "snapshot_not_found", ExitCode: -1}
	}
	return items[0], nil
}

func (r Runner) snapshotQuery(ctx context.Context, target config.Target, additional []string) ([]Snapshot, error) {
	base, environment, err := r.targetCommand(target)
	if err != nil {
		return nil, err
	}
	arguments := append(base,
		"snapshots", "--json",
		"--host", SnapshotHost,
		"--path", r.recoveryPath,
	)
	arguments = append(arguments, additional...)
	output, err := r.runTargetCommand(ctx, target, "snapshots", arguments, environment, "")
	if err != nil {
		return nil, err
	}
	return parseSnapshots(output, r.recoveryPath)
}

func (r Runner) Init(ctx context.Context, target config.Target) error {
	output, err := r.maintenance(ctx, target, []string{"init", "--repository-version", "2", "--json"})
	if err != nil {
		return err
	}
	if err := parseInitialized(output); err != nil {
		return &OperationError{Code: "init_result_invalid", ExitCode: -1}
	}
	return nil
}

func (r Runner) Unlock(ctx context.Context, target config.Target) error {
	output, err := r.maintenance(ctx, target, []string{"unlock", "--json"})
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(output)) != 0 {
		return &OperationError{Code: "unlock_result_invalid", ExitCode: -1}
	}
	return nil
}

func (r Runner) Check(ctx context.Context, target config.Target) error {
	output, err := r.maintenance(ctx, target, []string{"check", "--json"})
	if err != nil {
		return err
	}
	if err := parseFinalSummary(output, "check"); err != nil {
		return &OperationError{Code: "check_result_invalid", ExitCode: -1}
	}
	return nil
}

func (r Runner) Prune(ctx context.Context, target config.Target, apply bool) (PruneResult, error) {
	// Restic cannot express prefix-presence tag filters. Validate every
	// candidate before the mutating forget command so malformed identities are
	// never silently included in retention decisions.
	if _, err := r.snapshotQuery(ctx, target, nil); err != nil {
		return PruneResult{}, err
	}
	arguments := []string{
		"forget", "--json", "--prune",
		"--host", SnapshotHost,
		"--path", r.recoveryPath,
		"--group-by", "host,paths",
		"--keep-within", "24h",
		"--keep-hourly", "48",
		"--keep-daily", "14",
		"--keep-weekly", "8",
		"--keep-monthly", "12",
		"--keep-yearly", "3",
	}
	if !apply {
		arguments = append(arguments, "--dry-run")
	}
	output, err := r.maintenance(ctx, target, arguments)
	if err != nil {
		return PruneResult{}, err
	}
	groups, err := parseForgetGroups(output, r.recoveryPath)
	if err != nil {
		return PruneResult{}, &OperationError{Code: "prune_result_invalid", ExitCode: -1}
	}
	return PruneResult{Applied: apply, Groups: groups}, nil
}

func (r Runner) Restore(ctx context.Context, target config.Target, snapshotID, destination string) error {
	if !validSnapshotID(snapshotID) {
		return &OperationError{Code: "snapshot_id_invalid", ExitCode: -1}
	}
	if err := validateRestoreDestination(destination); err != nil {
		return err
	}
	output, err := r.maintenance(ctx, target, []string{
		"restore", "--json", "--verify", "--target", destination,
		snapshotID + ":" + r.recoveryPath,
	})
	if err != nil {
		return err
	}
	if err := parseFinalSummary(output, "restore"); err != nil {
		return &OperationError{Code: "restore_result_invalid", ExitCode: -1}
	}
	return nil
}

func validateRestoreDestination(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return &OperationError{Code: "restore_target_invalid", ExitCode: -1}
	}
	if err := privatefs.ValidatePrivateDirectory(path); err != nil {
		return &OperationError{Code: "restore_target_invalid", ExitCode: -1}
	}
	return nil
}

func (r Runner) maintenance(ctx context.Context, target config.Target, command []string) ([]byte, error) {
	base, environment, err := r.targetCommand(target)
	if err != nil {
		return nil, err
	}
	return r.runTargetCommand(ctx, target, command[0], append(base, command...), environment, "")
}
