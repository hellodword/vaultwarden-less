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
	"strings"
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
	return r.commandWithCache(ctx, target, dir, filepath.Join(r.cacheDir, "restic", CacheKey(target)), args...)
}
func (r Runner) commandWithCache(ctx context.Context, target config.Target, dir, cache string, args ...string) ([]byte, error) {
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
	return r.run(ctx, r.binary, append(base, args...), environment, dir)
}

const FingerprintPrefix = "vwless-fingerprint:"

type Metadata struct {
	ID        string
	Snapshots []Snapshot
}

func (m Metadata) Latest(completeOnly bool) *Snapshot {
	var latest *Snapshot
	for _, snapshot := range m.Snapshots {
		if snapshot.Fingerprint() == "" {
			continue
		}
		if completeOnly && (!slices.Contains(snapshot.Tags, "complete") || slices.Contains(snapshot.Tags, "incomplete")) {
			continue
		}
		if latest == nil || snapshot.Time.After(latest.Time) || (snapshot.Time.Equal(latest.Time) && snapshot.ID > latest.ID) {
			value := snapshot
			latest = &value
		}
	}
	return latest
}
func (m Metadata) Has(id string) bool {
	return slices.ContainsFunc(m.Snapshots, func(s Snapshot) bool { return s.ID == id })
}
func (s Snapshot) Fingerprint() string {
	value := ""
	if slices.Contains(s.Tags, "complete") == slices.Contains(s.Tags, "incomplete") {
		return ""
	}
	for _, tag := range s.Tags {
		if strings.HasPrefix(tag, FingerprintPrefix) {
			if value != "" {
				return ""
			}
			value = strings.TrimPrefix(tag, FingerprintPrefix)
			if !validSnapshotID(value) {
				return ""
			}
		}
	}
	return value
}
func (r Runner) Inspect(ctx context.Context, target config.Target) (Metadata, error) {
	var metadata Metadata
	data, err := r.command(ctx, target, "", "cat", "config")
	if err != nil {
		return metadata, err
	}
	var configuration struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &configuration) != nil || !validSnapshotID(configuration.ID) {
		return metadata, &OperationError{Code: "invalid_repository_identity", ExitCode: -1}
	}
	metadata.ID = configuration.ID
	data, err = r.command(ctx, target, "", "snapshots", "--json", "--host", SnapshotHost)
	if err != nil {
		return metadata, err
	}
	if json.Unmarshal(data, &metadata.Snapshots) != nil {
		return metadata, &OperationError{Code: "invalid_snapshot_result", ExitCode: -1}
	}
	for _, s := range metadata.Snapshots {
		if !validSnapshotID(s.ID) || s.Hostname != SnapshotHost || s.Time.IsZero() {
			return metadata, &OperationError{Code: "invalid_snapshot_result", ExitCode: -1}
		}
	}
	return metadata, nil
}
func (r Runner) Latest(ctx context.Context, target config.Target) (*Snapshot, error) {
	metadata, err := r.Inspect(ctx, target)
	return metadata.Latest(false), err
}

// Each check gets a fresh cache, so cached bytes cannot hide remote damage.
func (r Runner) Check(ctx context.Context, target config.Target, readData bool) error {
	limit := target.Verify.Limit
	if limit <= 0 {
		limit = 2 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	root := filepath.Join(r.cacheDir, "work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return &OperationError{Code: "work_unavailable", ExitCode: -1}
	}
	cache, err := os.MkdirTemp(root, "verify-")
	if err != nil {
		return &OperationError{Code: "work_unavailable", ExitCode: -1}
	}
	defer os.RemoveAll(cache)
	args := []string{"check", "--json"}
	if readData {
		args = append(args, "--read-data")
	}
	_, err = r.commandWithCache(ctx, target, "", cache, args...)
	return err
}

func (r Runner) Backup(ctx context.Context, target config.Target, directory, tag, fingerprint string) (string, error) {
	if !slices.Contains([]string{"complete", "incomplete"}, tag) || !validSnapshotID(fingerprint) {
		return "", errors.New("invalid capture tag")
	}
	ctx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()
	data, err := r.command(ctx, target, directory, "backup", ".", "--json", "--host", SnapshotHost, "--group-by", "host", "--force", "--no-scan", "--tag", "vwless-pending")
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
	// Publish a comparison tag only after a successful native upload. Restic
	// can create a snapshot on exit 3; such a snapshot must never become a
	// confirmed baseline on restart simply because it carries our requested tag.
	data, err = r.command(ctx, target, "", "tag", "--json", "--remove", "vwless-pending", "--add", tag, "--add", FingerprintPrefix+fingerprint, id)
	if err != nil {
		return "", err
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	confirmed := ""
	summaries := 0
	for {
		var record struct {
			Type    string `json:"message_type"`
			Old     string `json:"old_snapshot_id"`
			New     string `json:"new_snapshot_id"`
			Changed int    `json:"changed_snapshots"`
		}
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", &OperationError{Code: "invalid_tag_result", ExitCode: -1}
		}
		switch record.Type {
		case "changed":
			if record.Old != id || !validSnapshotID(record.New) || confirmed != "" {
				return "", &OperationError{Code: "invalid_tag_result", ExitCode: -1}
			}
			confirmed = record.New
		case "summary":
			if record.Changed != 1 {
				return "", &OperationError{Code: "invalid_tag_result", ExitCode: -1}
			}
			summaries++
		default:
			return "", &OperationError{Code: "invalid_tag_result", ExitCode: -1}
		}
	}
	if confirmed == "" || summaries != 1 {
		return "", &OperationError{Code: "invalid_tag_result", ExitCode: -1}
	}
	return confirmed, nil
}

func validSnapshotID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
