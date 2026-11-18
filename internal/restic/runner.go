package restic

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
)

const (
	maxCommandOutput = 64 << 20
	maxStderrOutput  = 64 << 10
	SnapshotHost     = "vaultwarden-less"
	UploadTimeout    = 2 * time.Hour
)

type OperationError struct {
	Code     string
	ExitCode int
}

func (e *OperationError) Error() string { return "restic operation failed: " + e.Code }

type Snapshot struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Paths []string  `json:"paths"`
}
type Runner struct {
	binary, cacheDir string
}

func NewRunner(binary, cacheDir string) Runner { return Runner{binary: binary, cacheDir: cacheDir} }

func (r Runner) command(ctx context.Context, target config.Target, args ...string) ([]byte, error) {
	cache := filepath.Join(r.cacheDir, "restic")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, &OperationError{Code: "cache_unavailable", ExitCode: -1}
	}
	base := []string{"--cache-dir", cache}
	for _, option := range target.Options {
		base = append(base, "--option", option)
	}
	workRoot := filepath.Join(r.cacheDir, "work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		return nil, &OperationError{Code: "work_unavailable", ExitCode: -1}
	}
	work, err := os.MkdirTemp(workRoot, "restic-")
	if err != nil {
		return nil, &OperationError{Code: "work_unavailable", ExitCode: -1}
	}
	defer os.RemoveAll(work)
	environment := append([]string{"PATH=" + os.Getenv("PATH")}, target.Environment...)
	environment = append(environment, "TMPDIR="+work)
	return r.run(ctx, append(base, args...), environment)
}

// Baseline identifies the native snapshot whose database is in the private cache.
type Baseline struct {
	Repository, ID string
	Time           time.Time
}

func (r Runner) latest(ctx context.Context, target config.Target) (Baseline, error) {
	var base Baseline
	data, err := r.command(ctx, target, "cat", "config")
	if err != nil {
		return base, err
	}
	var doc struct{ ID string }
	if json.Unmarshal(data, &doc) != nil || !validSnapshotID(doc.ID) {
		return base, errors.New("invalid_repository_identity")
	}
	base.Repository = doc.ID
	data, err = r.command(ctx, target, "snapshots", "--json", "--host", "", "--group-by", "", "--latest", "1")
	if err != nil {
		return base, err
	}
	var snapshots []Snapshot
	if json.Unmarshal(data, &snapshots) != nil || snapshots == nil || len(snapshots) > 1 {
		return base, errors.New("invalid_snapshot_list")
	}
	if len(snapshots) == 1 {
		s := snapshots[0]
		if !validSnapshotID(s.ID) || s.Time.IsZero() || !slices.Equal(s.Paths, []string{"/db.sqlite3"}) {
			return base, errors.New("unexpected_snapshot")
		}
		base.ID, base.Time = s.ID, s.Time
	}
	return base, nil
}

// Sync compares against this repository's current native snapshot on every pass.
// cacheFile belongs to this target and has only one concurrent user.
func (r Runner) Sync(ctx context.Context, target config.Target, rule capture.Rule, candidate, cacheFile string, cached Baseline) (Baseline, error) {
	base, err := r.latest(ctx, target)
	if err != nil {
		return Baseline{}, err
	}
	if base.ID != "" {
		_, statErr := os.Stat(cacheFile)
		if cached.Repository != base.Repository || cached.ID != base.ID || statErr != nil {
			// A failed dump must never be read as the previous cached baseline.
			part := cacheFile + ".part"
			defer os.Remove(part)
			if _, err = r.command(ctx, target, "dump", "--target", part, base.ID, "/db.sqlite3"); err != nil {
				return Baseline{}, err
			}
			if err = capture.IntegrityCheck(ctx, part); err != nil {
				return Baseline{}, errors.New("baseline_integrity_failed")
			}
			if err = os.Rename(part, cacheFile); err != nil {
				return Baseline{}, errors.New("baseline_cache_failed")
			}
		}
		changed, err := rule.Changed(ctx, cacheFile, candidate)
		if err != nil {
			return Baseline{}, errors.New("comparison_failed")
		}
		if !changed {
			return base, nil
		}
	}
	id, at, err := r.backup(ctx, target, candidate)
	if err != nil {
		return Baseline{}, err
	}
	// A confirmed upload can be used locally without downloading it again.
	if err = os.Remove(cacheFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Baseline{}, errors.New("baseline_cache_failed")
	}
	// If hard links are unavailable the next pass downloads the confirmed ID.
	_ = os.Link(candidate, cacheFile)
	return Baseline{Repository: base.Repository, ID: id, Time: at}, nil
}
func (r Runner) backup(ctx context.Context, target config.Target, path string) (string, time.Time, error) {
	data, err := r.command(ctx, target, "backup", "--json", "--host", SnapshotHost, "--stdin-filename", "db.sqlite3", "--stdin-from-command", "--", "cat", path)
	if err != nil {
		return "", time.Time{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	id := ""
	var at time.Time
	for {
		var record struct {
			Type string    `json:"message_type"`
			At   time.Time `json:"backup_start"`
			ID   string    `json:"snapshot_id"`
		}
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || id != "" || record.Type == "error" {
			return "", time.Time{}, &OperationError{Code: "invalid_backup_result", ExitCode: -1}
		}
		if record.Type == "summary" {
			if !validSnapshotID(record.ID) {
				return "", time.Time{}, &OperationError{Code: "invalid_backup_result", ExitCode: -1}
			}
			id, at = record.ID, record.At
		}
	}
	if id == "" || at.IsZero() {
		return "", time.Time{}, &OperationError{Code: "snapshot_id_missing", ExitCode: -1}
	}
	return id, at, nil
}

func validSnapshotID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
