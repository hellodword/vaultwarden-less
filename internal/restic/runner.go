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
	"log/slog"
	"net/url"
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

const (
	maxCommandOutput  = 16 << 20
	maxManifestOutput = 64 << 20
	maxStderrCapture  = 1 << 20
	maxStderrLog      = 64 << 10
)

type Runner struct {
	Binary       string
	CacheDir     string
	StagingDir   string
	PasswordFile string
	Credentials  *config.RoleCredentials
	Logger       *slog.Logger
	Disabled     bool
}

type OperationError struct {
	Code     string
	ExitCode int
	Stderr   string
}

func (e *OperationError) Error() string { return "restic operation failed: " + e.Code }

type BackupResult struct{ SnapshotID string }

type SnapshotInfo struct {
	ID        string            `json:"id"`
	Time      string            `json:"time"`
	Host      string            `json:"host"`
	Tags      []string          `json:"tags"`
	Paths     []string          `json:"paths"`
	Status    snapshot.Status   `json:"status"`
	ContentID string            `json:"content_id"`
	Manifest  snapshot.Manifest `json:"-"`
}

type SnapshotAnomaly struct {
	SnapshotID string `json:"snapshot_id"`
	ErrorCode  string `json:"error_code"`
}

type SnapshotReport struct {
	Snapshots []SnapshotInfo
	Anomalies []SnapshotAnomaly
}

type PruneResult struct {
	Status             snapshot.Status `json:"status"`
	KeptCount          int             `json:"kept_count"`
	RemovedCount       int             `json:"removed_count"`
	KeptSnapshotIDs    []string        `json:"kept_snapshot_ids"`
	RemovedSnapshotIDs []string        `json:"removed_snapshot_ids"`
}

func (r Runner) ValidateTarget(target config.Target) error {
	if r.Disabled {
		return nil
	}
	_, _, err := r.environment(target)
	if err != nil {
		r.logTargetError(target, "validate", err)
	}
	return err
}

func (r Runner) VerifyVersion(ctx context.Context) error {
	if r.Disabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stdout, err := r.run(ctx, "version", "", []string{"TMPDIR=/tmp"}, []string{"version"}, maxCommandOutput)
	if err != nil {
		operationErr := r.operationError(ctx, err, "version")
		r.logOperationError(config.Target{}, "version", operationErr)
		return operationErr
	}
	value := strings.TrimSpace(string(stdout))
	if value != "restic "+version.Restic && !strings.HasPrefix(value, "restic "+version.Restic+" ") {
		return &OperationError{Code: "restic_version_mismatch"}
	}
	return nil
}

func (r Runner) Backup(ctx context.Context, target config.Target, manifest snapshot.Manifest) (BackupResult, error) {
	if r.Disabled {
		return BackupResult{}, &OperationError{Code: "restic_disabled"}
	}
	if err := manifest.Validate(); err != nil {
		return BackupResult{}, &OperationError{Code: "invalid_manifest"}
	}
	env, globals, err := r.environment(target)
	if err != nil {
		r.logTargetError(target, "backup", err)
		return BackupResult{}, err
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		r.logTargetError(target, "backup", err)
		return BackupResult{}, err
	}
	contentHex, err := snapshot.ContentIDHex(manifest.ContentID)
	if err != nil {
		return BackupResult{}, &OperationError{Code: "invalid_content_id"}
	}
	args := append(globals,
		"backup", "--json", "--cache-dir", cache,
		"--host", version.SnapshotHost,
		"--tag", version.SnapshotTag,
		"--tag", string(manifest.Status),
		"--tag", "content-id="+contentHex,
		"data", "manifest.json",
	)
	stdout, err := r.run(ctx, "backup", filepath.Join(r.StagingDir, "current"), env, args, maxCommandOutput)
	if err != nil {
		return BackupResult{}, r.operationErrorForTarget(ctx, err, "backup", target)
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
	_, err := r.maintenance(ctx, target, "init", []string{"init"})
	return err
}

func (r Runner) Unlock(ctx context.Context, target config.Target) error {
	cache, err := r.targetCache(target.Name)
	if err != nil {
		r.logTargetError(target, "unlock", err)
		return err
	}
	_, err = r.maintenance(ctx, target, "unlock", []string{"unlock", "--cache-dir", cache})
	return err
}

func (r Runner) Check(ctx context.Context, target config.Target, readDataSubset string) error {
	cache, err := r.targetCache(target.Name)
	if err != nil {
		r.logTargetError(target, "check", err)
		return err
	}
	args := []string{"check", "--cache-dir", cache}
	if readDataSubset != "" {
		args = append(args, "--read-data-subset", readDataSubset)
	}
	_, err = r.maintenance(ctx, target, "check", args)
	return err
}

// Snapshots returns valid candidates only. Call ListSnapshots when the caller
// must expose anomalies or decide whether an explicit ID is unsafe.
func (r Runner) Snapshots(ctx context.Context, target config.Target, status snapshot.Status) ([]SnapshotInfo, error) {
	report, err := r.ListSnapshots(ctx, target)
	if err != nil {
		return nil, err
	}
	result := make([]SnapshotInfo, 0, len(report.Snapshots))
	for _, item := range report.Snapshots {
		if status == "" || item.Status == status {
			result = append(result, item)
		}
	}
	if status != "" && status != snapshot.Complete && status != snapshot.Incomplete {
		return nil, &OperationError{Code: "snapshot_status_invalid"}
	}
	return result, nil
}

func (r Runner) ListSnapshots(ctx context.Context, target config.Target) (SnapshotReport, error) {
	if r.Disabled {
		return SnapshotReport{Snapshots: []SnapshotInfo{}, Anomalies: []SnapshotAnomaly{}}, nil
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		r.logTargetError(target, "snapshots", err)
		return SnapshotReport{}, err
	}
	args := []string{"snapshots", "--json", "--cache-dir", cache, "--host", version.SnapshotHost, "--tag", version.SnapshotTag}
	stdout, err := r.maintenance(ctx, target, "snapshots", args)
	if err != nil {
		return SnapshotReport{}, err
	}
	var raw []rawSnapshot
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	if err := decoder.Decode(&raw); err != nil {
		return SnapshotReport{}, &OperationError{Code: "snapshots_result_invalid"}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return SnapshotReport{}, &OperationError{Code: "snapshots_result_invalid"}
	}
	report := SnapshotReport{Snapshots: []SnapshotInfo{}, Anomalies: []SnapshotAnomaly{}}
	for _, item := range raw {
		if !slices.Contains(item.Tags, version.SnapshotTag) {
			continue
		}
		candidate, code, hardErr := r.classifyCandidate(ctx, target, item, cache)
		if hardErr != nil {
			return SnapshotReport{}, hardErr
		}
		if code != "" {
			report.Anomalies = append(report.Anomalies, SnapshotAnomaly{SnapshotID: item.ID, ErrorCode: code})
			continue
		}
		report.Snapshots = append(report.Snapshots, candidate)
	}
	sort.Slice(report.Snapshots, func(i, j int) bool {
		if report.Snapshots[i].Time == report.Snapshots[j].Time {
			return report.Snapshots[i].ID < report.Snapshots[j].ID
		}
		return report.Snapshots[i].Time < report.Snapshots[j].Time
	})
	sort.Slice(report.Anomalies, func(i, j int) bool { return report.Anomalies[i].SnapshotID < report.Anomalies[j].SnapshotID })
	return report, nil
}

type rawSnapshot struct {
	ID       string          `json:"id"`
	Time     json.RawMessage `json:"time"`
	Hostname string          `json:"hostname"`
	Tags     []string        `json:"tags"`
	Paths    []string        `json:"paths"`
}

func (r Runner) classifyCandidate(ctx context.Context, target config.Target, raw rawSnapshot, cache string) (SnapshotInfo, string, error) {
	if !validSnapshotID(raw.ID) {
		return SnapshotInfo{}, "snapshot_id_invalid", nil
	}
	if raw.Hostname != version.SnapshotHost {
		return SnapshotInfo{}, "snapshot_host_invalid", nil
	}
	when, ok := parseSnapshotTime(raw.Time)
	if !ok {
		return SnapshotInfo{}, "snapshot_time_invalid", nil
	}
	status, contentHex, ok := identityTags(raw.Tags)
	if !ok {
		return SnapshotInfo{}, "snapshot_tag_invalid", nil
	}
	data, err := r.dumpManifest(ctx, target, cache, raw.ID)
	if err != nil {
		var operation *OperationError
		if errors.As(err, &operation) {
			switch operation.Code {
			case "operation_timeout", "operation_cancelled", "repository_missing", "repository_locked", "repository_password_invalid", "backend_credentials_invalid", "role_credentials_missing", "password_directory_invalid", "password_file_invalid", "cache_path_invalid", "cache_create_failed", "restic_runtime_failed", "restic_process_failed", "restic_exit_unknown":
				return SnapshotInfo{}, "", err
			default:
				return SnapshotInfo{}, "snapshot_manifest_invalid", nil
			}
		}
		return SnapshotInfo{}, "snapshot_manifest_invalid", nil
	}
	manifest, err := snapshot.DecodeManifest(data)
	if err != nil {
		return SnapshotInfo{}, "snapshot_manifest_invalid", nil
	}
	manifestHex, err := snapshot.ContentIDHex(manifest.ContentID)
	if err != nil || string(manifest.Status) != status || manifestHex != contentHex {
		return SnapshotInfo{}, "snapshot_manifest_tag_mismatch", nil
	}
	return SnapshotInfo{
		ID: raw.ID, Time: state.FormatTime(when), Host: raw.Hostname, Tags: slices.Clone(raw.Tags), Paths: slices.Clone(raw.Paths),
		Status: manifest.Status, ContentID: manifest.ContentID, Manifest: manifest,
	}, "", nil
}

func parseSnapshotTime(raw json.RawMessage) (time.Time, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil && !parsed.IsZero()
}

func identityTags(tags []string) (string, string, bool) {
	if countTag(tags, version.SnapshotTag) != 1 {
		return "", "", false
	}
	status := ""
	for _, candidate := range []snapshot.Status{snapshot.Complete, snapshot.Incomplete} {
		if countTag(tags, string(candidate)) > 1 {
			return "", "", false
		}
		if slices.Contains(tags, string(candidate)) {
			if status != "" {
				return "", "", false
			}
			status = string(candidate)
		}
	}
	if status == "" {
		return "", "", false
	}
	content := ""
	for _, tag := range tags {
		if strings.HasPrefix(tag, "content-id=") {
			if content != "" {
				return "", "", false
			}
			content = strings.TrimPrefix(tag, "content-id=")
		}
	}
	if !validSnapshotID(content) {
		return "", "", false
	}
	return status, content, true
}

func countTag(tags []string, want string) int {
	count := 0
	for _, tag := range tags {
		if tag == want {
			count++
		}
	}
	return count
}

func (r Runner) FindSnapshot(ctx context.Context, target config.Target, requested string) (SnapshotInfo, error) {
	if !validSnapshotID(requested) {
		return SnapshotInfo{}, &OperationError{Code: "snapshot_candidate_invalid"}
	}
	report, err := r.ListSnapshots(ctx, target)
	if err != nil {
		return SnapshotInfo{}, err
	}
	for _, item := range report.Snapshots {
		if item.ID == requested {
			return item, nil
		}
	}
	for _, anomaly := range report.Anomalies {
		if anomaly.SnapshotID == requested {
			return SnapshotInfo{}, &OperationError{Code: "snapshot_candidate_invalid"}
		}
	}
	return SnapshotInfo{}, &OperationError{Code: "snapshot_candidate_invalid"}
}

func (r Runner) dumpManifest(ctx context.Context, target config.Target, cache, snapshotID string) ([]byte, error) {
	// dump is intentionally a separate command so every candidate is validated
	// against the durable manifest instead of trusting snapshot metadata.
	env, globals, err := r.environment(target)
	if err != nil {
		r.logTargetError(target, "dump", err)
		return nil, err
	}
	stdout, err := r.run(ctx, "dump", "", env, append(globals, "dump", "--cache-dir", cache, snapshotID, "/manifest.json"), maxManifestOutput)
	if err != nil {
		return nil, r.operationErrorForTarget(ctx, err, "dump", target)
	}
	return stdout, nil
}

func (r Runner) Restore(ctx context.Context, target config.Target, snapshotID, destination string) error {
	if !validSnapshotID(snapshotID) {
		return &OperationError{Code: "snapshot_id_invalid"}
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		r.logTargetError(target, "restore", err)
		return err
	}
	_, err = r.maintenance(ctx, target, "restore", []string{"restore", "--cache-dir", cache, "--verify", "--target", destination, snapshotID})
	return err
}

func (r Runner) Prune(ctx context.Context, target config.Target, apply bool) ([]PruneResult, error) {
	report, err := r.ListSnapshots(ctx, target)
	if err != nil {
		return nil, err
	}
	if len(report.Anomalies) != 0 {
		return nil, &OperationError{Code: "snapshot_anomaly_blocks_prune"}
	}
	cache, err := r.targetCache(target.Name)
	if err != nil {
		r.logTargetError(target, "prune", err)
		return nil, err
	}
	// Both dry-runs must be parsed before an apply can remove anything.
	reports := make([]PruneResult, 0, 2)
	for _, status := range []snapshot.Status{snapshot.Complete, snapshot.Incomplete} {
		output, err := r.forget(ctx, target, cache, status, false)
		if err != nil {
			return nil, err
		}
		parsed, err := parseForgetOutput(output, status, false)
		if err != nil {
			return nil, &OperationError{Code: "prune_result_invalid"}
		}
		reports = append(reports, parsed)
	}
	if !apply {
		return reports, nil
	}
	for index, status := range []snapshot.Status{snapshot.Complete, snapshot.Incomplete} {
		output, err := r.forget(ctx, target, cache, status, true)
		if err != nil {
			return nil, err
		}
		parsed, err := parseForgetOutput(output, status, true)
		if err != nil {
			if index > 0 {
				return nil, &OperationError{Code: "prune_partial"}
			}
			return nil, &OperationError{Code: "prune_result_invalid"}
		}
		reports[index] = parsed
	}
	return reports, nil
}

func (r Runner) forget(ctx context.Context, target config.Target, cache string, status snapshot.Status, apply bool) ([]byte, error) {
	args := []string{"forget", "--json", "--cache-dir", cache, "--host", version.SnapshotHost, "--tag", version.SnapshotTag + "," + string(status), "--group-by", "host"}
	if status == snapshot.Complete {
		args = append(args, "--keep-within", "24h", "--keep-hourly", "48", "--keep-daily", "14", "--keep-weekly", "8", "--keep-monthly", "12", "--keep-yearly", "3")
	} else {
		args = append(args, "--keep-within", "24h", "--keep-daily", "7")
	}
	if apply {
		args = append(args, "--prune")
	} else {
		args = append(args, "--dry-run")
	}
	return r.maintenance(ctx, target, "forget", args)
}

type forgetGroup struct {
	Tags    []string         `json:"tags"`
	Host    string           `json:"host"`
	Paths   []string         `json:"paths"`
	Keep    []forgetSnapshot `json:"keep"`
	Remove  []forgetSnapshot `json:"remove"`
	Reasons []keepReason     `json:"reasons"`
}

type keepReason struct {
	Snapshot forgetSnapshot `json:"snapshot"`
	Matches  []string       `json:"matches"`
}

type forgetSnapshot struct {
	Time           time.Time       `json:"time"`
	Parent         string          `json:"parent"`
	Tree           string          `json:"tree"`
	Paths          []string        `json:"paths"`
	Hostname       string          `json:"hostname"`
	Username       string          `json:"username"`
	UID            uint32          `json:"uid"`
	GID            uint32          `json:"gid"`
	Excludes       []string        `json:"excludes"`
	Tags           []string        `json:"tags"`
	ProgramVersion string          `json:"program_version"`
	Summary        json.RawMessage `json:"summary"`
	ID             string          `json:"id"`
	ShortID        string          `json:"short_id"`
}

type pruneSummary struct {
	MessageType string          `json:"message_type"`
	Blobs       *pruneBlobs     `json:"blobs"`
	Bytes       *pruneSizes     `json:"bytes"`
	Packfiles   *prunePackfiles `json:"packfiles"`
}

type pruneBlobs struct {
	Used         uint64 `json:"used"`
	Duplicate    uint64 `json:"duplicate"`
	Unused       uint64 `json:"unused"`
	Total        uint64 `json:"total"`
	Repack       uint64 `json:"repack"`
	RepackRemove uint64 `json:"repack_remove"`
	Remove       uint64 `json:"remove"`
	RemoveTotal  uint64 `json:"remove_total"`
	Remaining    uint64 `json:"remaining"`
}

type pruneSizes struct {
	Used            uint64 `json:"used"`
	Duplicate       uint64 `json:"duplicate"`
	Unused          uint64 `json:"unused"`
	Unreferenced    uint64 `json:"unreferenced"`
	Uncompressed    uint64 `json:"uncompressed"`
	Total           uint64 `json:"total"`
	Repack          uint64 `json:"repack"`
	RepackRemove    uint64 `json:"repack_remove"`
	Remove          uint64 `json:"remove"`
	RemoveTotal     uint64 `json:"remove_total"`
	Remaining       uint64 `json:"remaining"`
	RemainingUnused uint64 `json:"remaining_unused"`
}

type prunePackfiles struct {
	Used         uint64 `json:"used"`
	Unused       uint64 `json:"unused"`
	PartlyUsed   uint64 `json:"partly_used"`
	Unreferenced uint64 `json:"unreferenced"`
	Total        uint64 `json:"total"`
	Keep         uint64 `json:"keep"`
	Repack       uint64 `json:"repack"`
	Remove       uint64 `json:"remove"`
	RemoveTotal  uint64 `json:"remove_total"`
}

func parseForgetOutput(data []byte, status snapshot.Status, applied bool) (PruneResult, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return PruneResult{}, fmt.Errorf("forget output must start with an array")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var groups []forgetGroup
	if err := decoder.Decode(&groups); err != nil {
		return PruneResult{}, err
	}
	result := PruneResult{Status: status, KeptSnapshotIDs: []string{}, RemovedSnapshotIDs: []string{}}
	seen := map[string]struct{}{}
	for _, group := range groups {
		for _, item := range group.Keep {
			if !validSnapshotID(item.ID) {
				return PruneResult{}, fmt.Errorf("invalid kept snapshot ID")
			}
			if _, exists := seen[item.ID]; exists {
				return PruneResult{}, fmt.Errorf("duplicate snapshot ID")
			}
			seen[item.ID] = struct{}{}
			result.KeptSnapshotIDs = append(result.KeptSnapshotIDs, item.ID)
		}
		for _, item := range group.Remove {
			if !validSnapshotID(item.ID) {
				return PruneResult{}, fmt.Errorf("invalid removed snapshot ID")
			}
			if _, exists := seen[item.ID]; exists {
				return PruneResult{}, fmt.Errorf("duplicate snapshot ID")
			}
			seen[item.ID] = struct{}{}
			result.RemovedSnapshotIDs = append(result.RemovedSnapshotIDs, item.ID)
		}
	}
	if !applied {
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return PruneResult{}, fmt.Errorf("extra forget output")
		}
	} else {
		// Restic 0.19.1 emits the forget groups as JSON and, after a
		// successful --prune, no second JSON value. Accept the documented
		// optional summary shape as well so a future pinned Restic release
		// cannot silently add an unvalidated value.
		var summary pruneSummary
		if err := decoder.Decode(&summary); err != nil {
			if !errors.Is(err, io.EOF) {
				return PruneResult{}, fmt.Errorf("invalid prune summary")
			}
		} else {
			if summary.MessageType != "summary" || summary.Blobs == nil || summary.Bytes == nil || summary.Packfiles == nil {
				return PruneResult{}, fmt.Errorf("unknown prune summary")
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return PruneResult{}, fmt.Errorf("extra prune output")
			}
		}
	}
	sort.Strings(result.KeptSnapshotIDs)
	sort.Strings(result.RemovedSnapshotIDs)
	result.KeptCount = len(result.KeptSnapshotIDs)
	result.RemovedCount = len(result.RemovedSnapshotIDs)
	return result, nil
}

func (r Runner) maintenance(ctx context.Context, target config.Target, operation string, command []string) ([]byte, error) {
	env, globals, err := r.environment(target)
	if err != nil {
		r.logTargetError(target, operation, err)
		return nil, err
	}
	stdout, err := r.run(ctx, operation, "", env, append(globals, command...), maxCommandOutput)
	if err != nil {
		return nil, r.operationErrorForTarget(ctx, err, operation, target)
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
	_ = password // Reading validates the file; Restic receives only its path.
	env := []string{"RESTIC_REPOSITORY=" + target.Repository, "RESTIC_PASSWORD_FILE=" + r.PasswordFile, "TMPDIR=/tmp"}
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

func validSnapshotTags(tags []string) bool {
	_, _, ok := identityTags(tags)
	return ok
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type commandError struct {
	err         error
	contextErr  error
	exitCode    int
	stderr      string
	stdoutLimit bool
	stderrLimit bool
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }

func (r Runner) run(ctx context.Context, operation, dir string, env, args []string, stdoutLimit int) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.Binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	stdout := &limitedBuffer{limit: stdoutLimit}
	stderr := &limitedBuffer{limit: maxStderrCapture}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil || stdout.exceeded || stderr.exceeded {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return nil, &commandError{err: errOrOutputError(err, stdout.exceeded || stderr.exceeded), contextErr: ctx.Err(), exitCode: exitCode, stderr: sanitizeStderr(stderr.Bytes(), r.secretValues(env)), stdoutLimit: stdout.exceeded, stderrLimit: stderr.exceeded}
	}
	return stdout.Bytes(), nil
}

func errOrOutputError(err error, exceeded bool) error {
	if err != nil {
		return err
	}
	if exceeded {
		return errors.New("restic output exceeded safe limit")
	}
	return errors.New("restic command failed")
}

func (r Runner) secretValues(env []string) []string {
	values := []string{}
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if ok && (name == "AWS_ACCESS_KEY_ID" || name == "AWS_SECRET_ACCESS_KEY") {
			values = append(values, value)
		}
	}
	if r.PasswordFile != "" {
		if value, err := securefile.Read(r.PasswordFile); err == nil {
			values = append(values, value)
		}
	}
	return values
}

func sanitizeStderr(data []byte, secrets []string) string {
	value := string(data)
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	if len(value) > maxStderrLog {
		value = value[:maxStderrLog] + "...[truncated]"
	}
	return value
}

func (r Runner) operationError(ctx context.Context, err error, operation string) *OperationError {
	var operationError *OperationError
	if errors.As(err, &operationError) {
		return operationError
	}
	var commandErr *commandError
	if !errors.As(err, &commandErr) {
		return &OperationError{Code: "restic_process_failed"}
	}
	code := classifyRunError(ctx, commandErr, operation)
	return &OperationError{Code: code, ExitCode: commandErr.exitCode, Stderr: commandErr.stderr}
}

func (r Runner) operationErrorForTarget(ctx context.Context, err error, operation string, target config.Target) *OperationError {
	operationErr := r.operationError(ctx, err, operation)
	r.logOperationError(target, operation, operationErr)
	return operationErr
}

func (r Runner) logTargetError(target config.Target, operation string, err error) {
	var operationErr *OperationError
	if errors.As(err, &operationErr) {
		r.logOperationError(target, operation, operationErr)
	}
}

func (r Runner) logOperationError(target config.Target, operation string, operationErr *OperationError) {
	if r.Logger == nil || operationErr == nil {
		return
	}
	attrs := []any{
		"operation", operation,
		"backend", target.Backend(),
		"error_code", operationErr.Code,
		"exit_code", operationErr.ExitCode,
	}
	if target.Name != "" {
		attrs = append(attrs, "target", target.Name)
	}
	if target.Repository != "" {
		if parsed, err := url.Parse(strings.TrimPrefix(target.Repository, "s3:")); err == nil {
			attrs = append(attrs, "s3_scheme", parsed.Scheme, "s3_host", parsed.Host, "s3_path", parsed.Path)
		}
	}
	if operationErr.Stderr != "" {
		attrs = append(attrs, "stderr", operationErr.Stderr)
	}
	r.Logger.Error("restic operation failed", attrs...)
}

func classifyRunError(ctx context.Context, err error, operation string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "operation_timeout"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "operation_cancelled"
	}
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		if errors.Is(commandErr.contextErr, context.DeadlineExceeded) {
			return "operation_timeout"
		}
		if errors.Is(commandErr.contextErr, context.Canceled) {
			return "operation_cancelled"
		}
		if errors.Is(commandErr.err, context.DeadlineExceeded) {
			return "operation_timeout"
		}
		if errors.Is(commandErr.err, context.Canceled) {
			return "operation_cancelled"
		}
		if commandErr.stdoutLimit || commandErr.stderrLimit {
			return "restic_output_exceeded"
		}
		switch commandErr.exitCode {
		case 1:
			return "repository_operation_failed"
		case 2:
			return "restic_runtime_failed"
		case 3:
			if operation == "backup" {
				return "source_data_unreadable"
			}
			if operation == "forget" || operation == "prune" {
				return "snapshot_remove_partial"
			}
			return "repository_operation_failed"
		case 10:
			return "repository_missing"
		case 11:
			return "repository_locked"
		case 12:
			return "repository_password_invalid"
		case 130:
			return "operation_cancelled"
		default:
			return "restic_exit_unknown"
		}
	}
	return "restic_process_failed"
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
