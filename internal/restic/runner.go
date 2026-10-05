package restic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

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
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Tags     []string  `json:"tags"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
}

type executeFunc func(context.Context, string, []string, []string, string) ([]byte, error)
type Runner struct {
	binary, cacheDir string
	execute          executeFunc
}

func NewRunner(binary, cacheDir string) Runner { return Runner{binary: binary, cacheDir: cacheDir} }

// CacheKey avoids interpreting operator names as paths and separates changed
// target configuration. It is local cache organization, not snapshot identity.
func CacheKey(target config.Target) string {
	encoded, _ := json.Marshal(struct {
		Name, EnvFile string
		Options       []string
	}{target.Name, target.EnvFile, target.Options})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (r Runner) command(ctx context.Context, target config.Target, dir string, args ...string) ([]byte, error) {
	if target.Error != "" {
		return nil, &OperationError{Code: "target_config_invalid", ExitCode: -1}
	}
	cache := filepath.Join(r.cacheDir, "restic", CacheKey(target))
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, &OperationError{Code: "cache_unavailable", ExitCode: -1}
	}
	base := []string{"--cache-dir", cache}
	for _, option := range target.Options {
		base = append(base, "--option", option)
	}
	environment := append([]string{"PATH=" + os.Getenv("PATH")}, target.Environment...)
	return r.run(ctx, r.binary, append(base, args...), environment, dir)
}

func (r Runner) Latest(ctx context.Context, target config.Target) (*Snapshot, error) {
	data, err := r.command(ctx, target, "", "snapshots", "--json", "--host", SnapshotHost, "--group-by", "host", "--latest", "1")
	if err != nil {
		return nil, err
	}
	// Restic's grouped JSON carries snapshots under their native group keys.
	var groups []struct {
		Snapshots []Snapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, &OperationError{Code: "invalid_snapshot_result", ExitCode: -1}
	}
	var items []Snapshot
	for _, group := range groups {
		items = append(items, group.Snapshots...)
	}
	if len(groups) > 0 && len(items) == 0 {
		if json.Unmarshal(data, &items) != nil {
			return nil, &OperationError{Code: "invalid_snapshot_result", ExitCode: -1}
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) != 1 || !validSnapshotID(items[0].ID) || items[0].Hostname != SnapshotHost {
		return nil, &OperationError{Code: "invalid_snapshot_result", ExitCode: -1}
	}
	return &items[0], nil
}

func (r Runner) Backup(ctx context.Context, target config.Target, directory, tag string) (string, error) {
	if !slices.Contains([]string{"complete", "incomplete"}, tag) {
		return "", errors.New("invalid capture tag")
	}
	ctx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()
	data, err := r.command(ctx, target, directory, "backup", ".", "--json", "--host", SnapshotHost, "--group-by", "host", "--ignore-ctime", "--no-scan", "--tag", tag)
	// A nonzero exit may still have produced a snapshot. It is never a confirmed
	// successful baseline, regardless of the JSON emitted before failure.
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	id := ""
	for {
		var record struct {
			Type string `json:"message_type"`
			ID   string `json:"snapshot_id"`
		}
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || id != "" || record.Type == "error" {
			return "", &OperationError{Code: "invalid_backup_result", ExitCode: -1}
		}
		if record.Type == "summary" {
			if !validSnapshotID(record.ID) {
				return "", &OperationError{Code: "invalid_backup_result", ExitCode: -1}
			}
			id = record.ID
		}
	}
	if id == "" {
		return "", &OperationError{Code: "snapshot_id_missing", ExitCode: -1}
	}
	return id, nil
}

func validSnapshotID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
