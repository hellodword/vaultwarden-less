package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/safefs"
	"github.com/hellodword/vaultwarden-less/internal/securefile"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

const maxCommandOutput = 16 << 20

type Runner struct {
	Binary       string
	CacheDir     string
	StagingDir   string
	PasswordFile string
	Credentials  *config.RoleCredentials
}

type OperationError struct {
	Code string
}

func (e *OperationError) Error() string { return "restic operation failed: " + e.Code }

type BackupResult struct {
	SnapshotID string
}

type SnapshotInfo struct {
	ID    string   `json:"id"`
	Time  string   `json:"time"`
	Host  string   `json:"host"`
	Tags  []string `json:"tags"`
	Paths []string `json:"paths"`
}

type PruneResult struct {
	Status    snapshot.Status   `json:"status"`
	Documents []json.RawMessage `json:"documents"`
}

func (r Runner) ValidateTarget(target config.Target) error {
	_, _, err := r.environment(target)
	return err
}

func (r Runner) VerifyVersion(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stdout, err := r.run(ctx, "", scrubEnvironment(os.Environ()), []string{"version"})
	if err != nil {
		return &OperationError{Code: "restic_version_failed"}
	}
	if !strings.HasPrefix(strings.TrimSpace(string(stdout)), "restic "+version.Restic+" ") && strings.TrimSpace(string(stdout)) != "restic "+version.Restic {
		return &OperationError{Code: "restic_version_mismatch"}
	}
	return nil
}

func (r Runner) Backup(ctx context.Context, target config.Target, manifest snapshot.Manifest) (BackupResult, error) {
	if err := manifest.Validate(); err != nil {
		return BackupResult{}, &OperationError{Code: "invalid_manifest"}
	}
	env, globals, err := r.environment(target)
	if err != nil {
		return BackupResult{}, err
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		return BackupResult{}, err
	}
	contentHex, err := snapshot.ContentIDHex(manifest.ContentID)
	if err != nil {
		return BackupResult{}, &OperationError{Code: "invalid_content_id"}
	}
	current := filepath.Join(r.StagingDir, "current")
	args := append(globals,
		"backup", "--json", "--cache-dir", cache,
		"--host", version.SnapshotHost,
		"--tag", version.SnapshotTag,
		"--tag", string(manifest.Status),
		"--tag", "content-id="+contentHex,
		"data", "manifest.json",
	)
	stdout, err := r.run(ctx, current, env, args)
	if err != nil {
		return BackupResult{}, &OperationError{Code: classifyRunError(ctx, err, "backup_failed")}
	}
	var result BackupResult
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64<<10), maxCommandOutput)
	for scanner.Scan() {
		var message struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal(scanner.Bytes(), &message) == nil && message.MessageType == "summary" {
			result.SnapshotID = message.SnapshotID
		}
	}
	if scanner.Err() != nil || !validSnapshotID(result.SnapshotID) {
		return BackupResult{}, &OperationError{Code: "backup_result_invalid"}
	}
	return result, nil
}

func (r Runner) Init(ctx context.Context, target config.Target) error {
	_, err := r.maintenance(ctx, target, []string{"init"})
	return err
}

func (r Runner) Unlock(ctx context.Context, target config.Target) error {
	cache, err := r.targetCache(target.Name)
	if err != nil {
		return err
	}
	_, err = r.maintenance(ctx, target, []string{"unlock", "--cache-dir", cache})
	return err
}

func (r Runner) Check(ctx context.Context, target config.Target, readDataSubset string) error {
	cache, err := r.targetCache(target.Name)
	if err != nil {
		return err
	}
	args := []string{"check", "--cache-dir", cache}
	if readDataSubset != "" {
		args = append(args, "--read-data-subset", readDataSubset)
	}
	_, err = r.maintenance(ctx, target, args)
	return err
}

func (r Runner) Snapshots(ctx context.Context, target config.Target, status snapshot.Status) ([]SnapshotInfo, error) {
	if status != "" && status != snapshot.Complete && status != snapshot.Incomplete {
		return nil, &OperationError{Code: "snapshot_status_invalid"}
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		return nil, err
	}
	args := []string{
		"snapshots", "--json", "--cache-dir", cache,
		"--host", version.SnapshotHost,
		"--path", r.snapshotDataPath(),
		"--path", r.snapshotManifestPath(),
	}
	if status != "" {
		args = append(args, "--tag", version.SnapshotTag+","+string(status))
	} else {
		args = append(args, "--tag", version.SnapshotTag)
	}
	stdout, err := r.maintenance(ctx, target, args)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID    string    `json:"id"`
		Time  time.Time `json:"time"`
		Host  string    `json:"hostname"`
		Tags  []string  `json:"tags"`
		Paths []string  `json:"paths"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, &OperationError{Code: "snapshots_result_invalid"}
	}
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].Time.Equal(raw[j].Time) {
			return raw[i].ID < raw[j].ID
		}
		return raw[i].Time.Before(raw[j].Time)
	})
	result := make([]SnapshotInfo, 0, len(raw))
	for _, item := range raw {
		if item.Host != version.SnapshotHost || !slices.Contains(item.Tags, version.SnapshotTag) ||
			(status != "" && !slices.Contains(item.Tags, string(status))) || !validSnapshotTags(item.Tags) ||
			!validSnapshotID(item.ID) || item.Time.IsZero() || !samePaths(item.Paths, r.SnapshotPaths()) {
			return nil, &OperationError{Code: "snapshots_filter_violation"}
		}
		result = append(result, SnapshotInfo{ID: item.ID, Time: state.FormatTime(item.Time), Host: item.Host, Tags: item.Tags, Paths: item.Paths})
	}
	return result, nil
}

func (r Runner) Restore(ctx context.Context, target config.Target, snapshotID, destination string) error {
	if !validSnapshotID(snapshotID) {
		return &OperationError{Code: "snapshot_id_invalid"}
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		return err
	}
	_, err = r.maintenance(ctx, target, []string{
		"restore", "--cache-dir", cache, "--verify", "--target", destination, snapshotID,
	})
	return err
}

func (r Runner) Prune(ctx context.Context, target config.Target, apply bool) ([]PruneResult, error) {
	cache, err := r.targetCache(target.Name)
	if err != nil {
		return nil, err
	}
	reports := make([]PruneResult, 0, 2)
	for _, status := range []snapshot.Status{snapshot.Complete, snapshot.Incomplete} {
		if _, err := r.Snapshots(ctx, target, status); err != nil {
			return nil, err
		}
		args := []string{
			"forget", "--json", "--cache-dir", cache,
			"--host", version.SnapshotHost,
			"--path", r.snapshotDataPath(),
			"--path", r.snapshotManifestPath(),
			"--tag", version.SnapshotTag + "," + string(status),
			"--group-by", "host,paths",
			"--keep-within", "24h",
			"--keep-hourly", "48",
			"--keep-daily", "14",
			"--keep-weekly", "8",
			"--keep-monthly", "12",
			"--keep-yearly", "3",
		}
		if apply {
			args = append(args, "--prune")
		} else {
			args = append(args, "--dry-run")
		}
		output, err := r.maintenance(ctx, target, args)
		if err != nil {
			return nil, err
		}
		documents, err := decodeJSONStream(output)
		if err != nil {
			return nil, &OperationError{Code: "prune_result_invalid"}
		}
		reports = append(reports, PruneResult{Status: status, Documents: documents})
	}
	return reports, nil
}

func decodeJSONStream(data []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var documents []json.RawMessage
	for {
		var document json.RawMessage
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, err
		}
		documents = append(documents, document)
	}
}

func (r Runner) maintenance(ctx context.Context, target config.Target, command []string) ([]byte, error) {
	env, globals, err := r.environment(target)
	if err != nil {
		return nil, err
	}
	stdout, err := r.run(ctx, "", env, append(globals, command...))
	if err != nil {
		return nil, &OperationError{Code: classifyRunError(ctx, err, "maintenance_failed")}
	}
	return stdout, nil
}

func (r Runner) environment(target config.Target) ([]string, []string, error) {
	if err := target.Validate(); err != nil {
		return nil, nil, &OperationError{Code: "repository_backend_mismatch"}
	}
	if r.Credentials == nil {
		return nil, nil, &OperationError{Code: "role_credentials_missing"}
	}
	credentialEnvironment, err := r.Credentials.Environment(target)
	if err != nil {
		return nil, nil, &OperationError{Code: "backend_credentials_invalid"}
	}
	if err := safefs.ValidatePrivateDirectory(filepath.Dir(r.PasswordFile)); err != nil {
		return nil, nil, &OperationError{Code: "password_directory_invalid"}
	}
	password, err := securefile.Read(r.PasswordFile)
	if err != nil || strings.ContainsAny(password, "\r\n") {
		return nil, nil, &OperationError{Code: "password_file_invalid"}
	}
	env := scrubEnvironment(os.Environ())
	env = append(env,
		"RESTIC_REPOSITORY="+target.Repository,
		"RESTIC_PASSWORD_FILE="+r.PasswordFile,
	)
	names := make([]string, 0, len(credentialEnvironment))
	for name := range credentialEnvironment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+credentialEnvironment[name])
	}
	globals := []string{}
	optionNames := make([]string, 0, len(target.ResticOptions))
	for name := range target.ResticOptions {
		optionNames = append(optionNames, name)
	}
	sort.Strings(optionNames)
	for _, name := range optionNames {
		globals = append(globals, "-o", name+"="+target.ResticOptions[name])
	}
	return env, globals, nil
}

func (r Runner) targetCache(name string) (string, error) {
	if err := safefs.ValidatePrivateDirectory(r.CacheDir); err != nil {
		return "", &OperationError{Code: "cache_path_invalid"}
	}
	path := filepath.Join(r.CacheDir, name)
	if filepath.Dir(path) != filepath.Clean(r.CacheDir) {
		return "", &OperationError{Code: "cache_path_invalid"}
	}
	if err := safefs.EnsurePrivateDirectory(path); err != nil {
		return "", &OperationError{Code: "cache_create_failed"}
	}
	return path, nil
}

func (r Runner) snapshotDataPath() string {
	return filepath.Join(r.StagingDir, "current", "data")
}

func (r Runner) snapshotManifestPath() string {
	return filepath.Join(r.StagingDir, "current", "manifest.json")
}

// SnapshotPaths is the durable v1 Restic path identity. Both paths must be
// present, and no additional path may be accepted by restore or maintenance.
func (r Runner) SnapshotPaths() []string {
	return []string{r.snapshotDataPath(), r.snapshotManifestPath()}
}

func samePaths(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}

func validSnapshotTags(tags []string) bool {
	if len(tags) != 3 || !slices.Contains(tags, version.SnapshotTag) {
		return false
	}
	complete := slices.Contains(tags, string(snapshot.Complete))
	incomplete := slices.Contains(tags, string(snapshot.Incomplete))
	if complete == incomplete {
		return false
	}
	contentTags := 0
	for _, tag := range tags {
		if strings.HasPrefix(tag, "content-id=") {
			contentTags++
			if !validSnapshotID(strings.TrimPrefix(tag, "content-id=")) {
				return false
			}
		}
	}
	return contentTags == 1
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (r Runner) run(ctx context.Context, dir string, env, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	stdout := &limitedBuffer{limit: maxCommandOutput}
	stderr := &limitedBuffer{limit: 1 << 20}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, fmt.Errorf("restic output exceeded safe limit")
	}
	return stdout.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.exceeded = true
	}
	_, _ = b.Buffer.Write(data)
	return original, nil
}

func classifyRunError(ctx context.Context, err error, fallback string) string {
	if ctx.Err() != nil {
		return "operation_cancelled"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fallback
	}
	return "restic_process_failed"
}

func scrubEnvironment(input []string) []string {
	blockedPrefixes := []string{"RESTIC_", "AWS_", "B2_", "AZURE_", "GOOGLE_", "OS_", "ST_", "SWIFT_"}
	result := make([]string, 0, len(input))
	for _, item := range input {
		name, _, _ := strings.Cut(item, "=")
		blocked := false
		for _, prefix := range blockedPrefixes {
			if strings.HasPrefix(name, prefix) {
				blocked = true
				break
			}
		}
		if !blocked {
			result = append(result, item)
		}
	}
	return result
}
