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
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
)

const (
	maxCommandOutput = 16 << 20
	maxStderrOutput  = 1 << 20

	SnapshotHost          = "vaultwarden-less"
	SnapshotPath          = "/staging/point"
	SnapshotTag           = "vaultwarden-less-recovery-v2"
	DefaultSnapshotsLimit = 100
	MaxSnapshotsLimit     = 1000
)

type OperationError struct {
	Code     string
	ExitCode int
}

func (e *OperationError) Error() string { return "restic operation failed: " + e.Code }

type Snapshot struct {
	ID        string `json:"id"`
	Time      string `json:"time"`
	ContentID string `json:"content_id"`
}

type SyncResult struct {
	SnapshotID string
	ContentID  string
	Created    bool
}

type PruneGroup struct {
	Host              string   `json:"host"`
	Paths             []string `json:"paths"`
	KeepSnapshotIDs   []string `json:"keep_snapshot_ids"`
	RemoveSnapshotIDs []string `json:"remove_snapshot_ids"`
}

type PruneResult struct {
	Applied bool         `json:"applied"`
	Groups  []PruneGroup `json:"groups"`
}

type executeFunc func(context.Context, string, []string, []string, string) ([]byte, error)

type Runner struct {
	Binary       string
	CacheDir     string
	PasswordRoot string
	Credentials  *config.RoleCredentials
	Logger       *slog.Logger

	execute      executeFunc
	recoveryPath string
}

func (r Runner) VerifyVersion(ctx context.Context) error {
	binary := r.Binary
	if binary == "" {
		binary = config.ResticBinary
	}
	output, err := r.runLogged(ctx, "version", "", binary, []string{"version"}, sanitizedEnvironment(nil), "")
	if err != nil {
		return err
	}
	value := strings.TrimSpace(string(output))
	if value != "restic "+upstream.ResticVersion && !strings.HasPrefix(value, "restic "+upstream.ResticVersion+" ") {
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
		return SyncResult{SnapshotID: latest.ID, ContentID: manifest.ContentID, Created: false}, nil
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
		"--tag", SnapshotTag,
		"--tag", "content-id="+contentHex,
		"--", r.path(),
	)
	output, err := r.runTargetCommand(ctx, target, "backup", arguments, environment, "")
	if err != nil {
		return SyncResult{}, err
	}
	snapshotID, err := parseBackupSnapshotID(output)
	if err != nil {
		return SyncResult{}, &OperationError{Code: "backup_result_invalid", ExitCode: -1}
	}
	result := SyncResult{SnapshotID: snapshotID, ContentID: manifest.ContentID, Created: true}
	r.logResult("restic recovery snapshot synchronized", target.Name, result)
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
		"--path", r.path(),
		"--tag", SnapshotTag,
	)
	arguments = append(arguments, additional...)
	output, err := r.runTargetCommand(ctx, target, "snapshots", arguments, environment, "")
	if err != nil {
		return nil, err
	}
	return parseSnapshots(output, r.path())
}

func (r Runner) Init(ctx context.Context, target config.Target) error {
	_, err := r.maintenance(ctx, target, []string{"init", "--json"})
	return err
}

func (r Runner) Unlock(ctx context.Context, target config.Target) error {
	_, err := r.maintenance(ctx, target, []string{"unlock", "--json"})
	return err
}

func (r Runner) Check(ctx context.Context, target config.Target, readDataSubset string) error {
	arguments := []string{"check", "--json"}
	if readDataSubset != "" {
		parts := strings.Split(readDataSubset, "/")
		if len(parts) != 2 || parts[1] != "12" {
			return &OperationError{Code: "read_subset_invalid", ExitCode: -1}
		}
		part, err := strconv.Atoi(parts[0])
		if err != nil || part < 1 || part > 12 {
			return &OperationError{Code: "read_subset_invalid", ExitCode: -1}
		}
		arguments = append(arguments, "--read-data-subset", readDataSubset)
	}
	_, err := r.maintenance(ctx, target, arguments)
	return err
}

func (r Runner) Prune(ctx context.Context, target config.Target, apply bool) (PruneResult, error) {
	arguments := []string{
		"forget", "--json", "--prune",
		"--host", SnapshotHost,
		"--path", r.path(),
		"--tag", SnapshotTag,
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
	groups, err := parseForgetGroups(output, r.path())
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
	_, err := r.maintenance(ctx, target, []string{
		"restore", "--verify", "--target", destination,
		snapshotID + ":" + r.path(),
	})
	return err
}

func (r Runner) path() string {
	if r.recoveryPath != "" {
		return r.recoveryPath
	}
	return SnapshotPath
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

func (r Runner) targetCommand(target config.Target) ([]string, []string, error) {
	if err := target.Validate(); err != nil {
		return nil, nil, &OperationError{Code: "target_invalid", ExitCode: -1}
	}
	if r.Credentials == nil {
		return nil, nil, &OperationError{Code: "credentials_missing", ExitCode: -1}
	}
	credentialEnvironment, err := r.Credentials.Environment(target)
	if err != nil {
		return nil, nil, &OperationError{Code: "credentials_invalid", ExitCode: -1}
	}
	if err := privatefs.ValidatePrivateDirectory(r.PasswordRoot); err != nil {
		return nil, nil, &OperationError{Code: "password_path_invalid", ExitCode: -1}
	}
	passwordFile := filepath.Join(r.PasswordRoot, target.Name)
	if filepath.Dir(passwordFile) != filepath.Clean(r.PasswordRoot) {
		return nil, nil, &OperationError{Code: "password_path_invalid", ExitCode: -1}
	}
	password, err := privatefs.Read(passwordFile)
	if err != nil || strings.ContainsAny(password, "\r\n\x00") {
		return nil, nil, &OperationError{Code: "password_file_invalid", ExitCode: -1}
	}
	if err := privatefs.ValidatePrivateDirectory(r.CacheDir); err != nil {
		return nil, nil, &OperationError{Code: "cache_path_invalid", ExitCode: -1}
	}
	cache := filepath.Join(r.CacheDir, target.Name)
	if filepath.Dir(cache) != filepath.Clean(r.CacheDir) {
		return nil, nil, &OperationError{Code: "cache_path_invalid", ExitCode: -1}
	}
	if err := privatefs.EnsurePrivateDirectory(cache); err != nil {
		return nil, nil, &OperationError{Code: "cache_create_failed", ExitCode: -1}
	}

	base := []string{
		"--repo", target.Repository,
		"--password-file", passwordFile,
		"--cache-dir", cache,
	}
	optionNames := make([]string, 0, len(target.ResticOptions))
	for name := range target.ResticOptions {
		optionNames = append(optionNames, name)
	}
	sort.Strings(optionNames)
	for _, name := range optionNames {
		base = append(base, "--option", name+"="+target.ResticOptions[name])
	}
	environment := sanitizedEnvironment(credentialEnvironment)
	return base, environment, nil
}

func sanitizedEnvironment(credentials map[string]string) []string {
	environment := []string{"TMPDIR=/tmp"}
	for _, name := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if value, ok := credentials[name]; ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func (r Runner) runTargetCommand(ctx context.Context, target config.Target, operation string, arguments, environment []string, directory string) ([]byte, error) {
	binary := r.Binary
	if binary == "" {
		binary = config.ResticBinary
	}
	return r.runLogged(ctx, operation, target.Name, binary, arguments, environment, directory)
}

func (r Runner) runLogged(ctx context.Context, operation, target, binary string, arguments, environment []string, directory string) ([]byte, error) {
	started := time.Now()
	output, err := r.run(ctx, binary, arguments, environment, directory)
	if r.Logger == nil {
		return output, err
	}
	attributes := []any{
		"operation", operation,
		"duration_ms", time.Since(started).Milliseconds(),
		"exit_code", operationExitCode(err),
	}
	if target != "" {
		attributes = append(attributes, "target", target)
	}
	if err == nil {
		r.Logger.Info("restic operation completed", attributes...)
	} else {
		var operationError *OperationError
		if errors.As(err, &operationError) {
			attributes = append(attributes, "error_code", operationError.Code)
		}
		r.Logger.Error("restic operation failed", attributes...)
	}
	return output, err
}

func operationExitCode(err error) int {
	if err == nil {
		return 0
	}
	var operationError *OperationError
	if errors.As(err, &operationError) {
		return operationError.ExitCode
	}
	return -1
}

func (r Runner) logResult(message, target string, result SyncResult) {
	if r.Logger == nil {
		return
	}
	r.Logger.Info(
		message,
		"target", target,
		"snapshot_id", result.SnapshotID,
		"content_id", result.ContentID,
		"created", result.Created,
	)
}

func (r Runner) run(ctx context.Context, binary string, arguments, environment []string, directory string) ([]byte, error) {
	if r.execute != nil {
		return r.execute(ctx, binary, slices.Clone(arguments), slices.Clone(environment), directory)
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = directory
	command.Env = environment
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Signal(syscall.SIGTERM)
	}
	command.WaitDelay = 10 * time.Second
	stdout := &limitedBuffer{limit: maxCommandOutput}
	stderr := &limitedBuffer{limit: maxStderrOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if err != nil || stdout.exceeded || stderr.exceeded {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, &OperationError{Code: "cancelled", ExitCode: -1}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &OperationError{Code: "timeout", ExitCode: -1}
		}
		exitCode := -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
		if stdout.exceeded || stderr.exceeded {
			return nil, &OperationError{Code: "output_limit", ExitCode: exitCode}
		}
		return nil, &OperationError{Code: "command_failed", ExitCode: exitCode}
	}
	return stdout.Bytes(), nil
}

type rawSnapshot struct {
	ID       string   `json:"id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
}

func parseSnapshots(data []byte, expectedPath string) ([]Snapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw []rawSnapshot
	if err := decoder.Decode(&raw); err != nil {
		return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
	}
	result := make([]Snapshot, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		if !validSnapshotID(item.ID) || item.Hostname != SnapshotHost || !slices.Equal(item.Paths, []string{expectedPath}) {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		when, err := time.Parse(time.RFC3339Nano, item.Time)
		if err != nil || when.IsZero() {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		contentHex, ok := contentTag(item.Tags)
		if !ok {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		seen[item.ID] = struct{}{}
		result = append(result, Snapshot{
			ID:        item.ID,
			Time:      when.UTC().Format(time.RFC3339Nano),
			ContentID: "sha256:" + contentHex,
		})
	}
	if result == nil {
		result = []Snapshot{}
	}
	return result, nil
}

func contentTag(tags []string) (string, bool) {
	if countTag(tags, SnapshotTag) != 1 {
		return "", false
	}
	content := ""
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "content-id=") {
			continue
		}
		if content != "" {
			return "", false
		}
		content = strings.TrimPrefix(tag, "content-id=")
	}
	return content, validSnapshotID(content)
}

func countTag(tags []string, expected string) int {
	count := 0
	for _, tag := range tags {
		if tag == expected {
			count++
		}
	}
	return count
}

func parseBackupSnapshotID(data []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), maxCommandOutput)
	snapshotID := ""
	for scanner.Scan() {
		var message struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return "", err
		}
		if message.MessageType == "summary" {
			if snapshotID != "" {
				return "", fmt.Errorf("multiple backup summaries")
			}
			snapshotID = message.SnapshotID
		}
	}
	if err := scanner.Err(); err != nil || !validSnapshotID(snapshotID) {
		return "", fmt.Errorf("invalid backup summary")
	}
	return snapshotID, nil
}

type forgetGroup struct {
	Host   string           `json:"host"`
	Paths  []string         `json:"paths"`
	Keep   []forgetSnapshot `json:"keep"`
	Remove []forgetSnapshot `json:"remove"`
}

type forgetSnapshot struct {
	ID string `json:"id"`
}

func parseForgetGroups(data []byte, expectedPath string) ([]PruneGroup, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw []forgetGroup
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	groups := make([]PruneGroup, 0, len(raw))
	for _, group := range raw {
		if group.Host != SnapshotHost || !slices.Equal(group.Paths, []string{expectedPath}) {
			return nil, fmt.Errorf("unexpected forget group")
		}
		parsed := PruneGroup{
			Host:              group.Host,
			Paths:             slices.Clone(group.Paths),
			KeepSnapshotIDs:   []string{},
			RemoveSnapshotIDs: []string{},
		}
		seen := map[string]struct{}{}
		for _, entry := range group.Keep {
			if !validSnapshotID(entry.ID) {
				return nil, fmt.Errorf("invalid kept snapshot ID")
			}
			if _, duplicate := seen[entry.ID]; duplicate {
				return nil, fmt.Errorf("duplicate snapshot ID")
			}
			seen[entry.ID] = struct{}{}
			parsed.KeepSnapshotIDs = append(parsed.KeepSnapshotIDs, entry.ID)
		}
		for _, entry := range group.Remove {
			if !validSnapshotID(entry.ID) {
				return nil, fmt.Errorf("invalid removed snapshot ID")
			}
			if _, duplicate := seen[entry.ID]; duplicate {
				return nil, fmt.Errorf("duplicate snapshot ID")
			}
			seen[entry.ID] = struct{}{}
			parsed.RemoveSnapshotIDs = append(parsed.RemoveSnapshotIDs, entry.ID)
		}
		sort.Strings(parsed.KeepSnapshotIDs)
		sort.Strings(parsed.RemoveSnapshotIDs)
		groups = append(groups, parsed)
	}
	for {
		var trailing map[string]any
		err := decoder.Decode(&trailing)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || trailing == nil {
			return nil, fmt.Errorf("invalid trailing prune output")
		}
	}
	if groups == nil {
		groups = []PruneGroup{}
	}
	return groups, nil
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := buffer.limit - buffer.Len()
	if remaining <= 0 {
		buffer.exceeded = true
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		buffer.exceeded = true
	}
	_, _ = buffer.Buffer.Write(data)
	return original, nil
}
