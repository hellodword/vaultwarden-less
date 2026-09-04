package restic

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/strictjson"
)

const (
	maxJSONFrame     = 8 << 20
	maxCommandOutput = 64 << 20
	maxJSONFrames    = 100_000
	maxStderrOutput  = 64 << 10

	SnapshotHost          = "vaultwarden-less"
	DefaultSnapshotsLimit = 100
	MaxSnapshotsLimit     = 1000
)

var resticVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

type OperationError struct {
	Code     string
	ExitCode int
}

func (e *OperationError) Error() string { return "restic operation failed: " + e.Code }

type Snapshot struct {
	ID            string `json:"id"`
	Time          string `json:"time"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
}

type SyncResult struct {
	SnapshotID string
	ContentID  string
	ArchiveID  string
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

func NewRunner(binary, cacheDir, passwordRoot, recoveryPath string, credentials *config.RoleCredentials, logger *slog.Logger) Runner {
	return Runner{
		Binary: binary, CacheDir: cacheDir, PasswordRoot: passwordRoot,
		Credentials: credentials, Logger: logger, recoveryPath: recoveryPath,
	}
}

func (r Runner) VerifyVersion(ctx context.Context) error {
	output, err := r.runLogged(ctx, "version", "", r.Binary, []string{"version", "--json"}, sanitizedEnvironment(nil), "")
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
	return r.ensure(ctx, target, manifest)
}

func (r Runner) ensure(ctx context.Context, target config.Target, manifest snapshot.Manifest) (SyncResult, error) {
	if err := manifest.Validate(); err != nil {
		return SyncResult{}, &OperationError{Code: "invalid_manifest", ExitCode: -1}
	}
	latest, err := r.Latest(ctx, target)
	if err != nil {
		return SyncResult{}, err
	}
	reusable := latest != nil && latest.ContentID == manifest.ContentID
	if reusable {
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
	if r.Logger != nil {
		r.Logger.Info(
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
	if _, err := config.ReadPassword(r.PasswordRoot, target.Name); err != nil {
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
	for _, option := range target.Options {
		base = append(base, "--option", option.Name+"="+option.Value)
	}
	environment := sanitizedEnvironment(credentialEnvironment)
	return base, environment, nil
}

func sanitizedEnvironment(credentials map[string]string) []string {
	environment := []string{
		"HOME=/nonexistent",
		"SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt",
		"TMPDIR=/tmp",
	}
	names := make([]string, 0, len(credentials))
	for name := range credentials {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+credentials[name])
	}
	return environment
}

func (r Runner) runTargetCommand(ctx context.Context, target config.Target, operation string, arguments, environment []string, directory string) ([]byte, error) {
	return r.runLogged(ctx, operation, target.Name, r.Binary, arguments, environment, directory)
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

func (r Runner) run(ctx context.Context, binary string, arguments, environment []string, directory string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, &OperationError{Code: "timeout", ExitCode: -1}
		}
		return nil, &OperationError{Code: "cancelled", ExitCode: -1}
	}
	if r.execute != nil {
		return r.execute(ctx, binary, slices.Clone(arguments), slices.Clone(environment), directory)
	}
	command := exec.Command(binary, arguments...)
	command.Dir = directory
	command.Env = slices.Clone(environment)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout := newBoundedWriter(maxCommandOutput)
	stderr := newBoundedWriter(maxStderrOutput)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, &OperationError{Code: "command_failed", ExitCode: -1}
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	cancelled := false
	var waitError error
	select {
	case waitError = <-waited:
	case <-ctx.Done():
		cancelled = true
		terminateProcessGroup(command.Process, syscall.SIGTERM)
		timer := time.NewTimer(2 * time.Second)
		select {
		case waitError = <-waited:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
			terminateProcessGroup(command.Process, syscall.SIGKILL)
			waitError = <-waited
		}
	}
	if cancelled {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &OperationError{Code: "timeout", ExitCode: -1}
		}
		return nil, &OperationError{Code: "cancelled", ExitCode: -1}
	}
	exitCode := -1
	var exitError *exec.ExitError
	if errors.As(waitError, &exitError) {
		exitCode = exitError.ExitCode()
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, &OperationError{Code: "output_limit", ExitCode: exitCode}
	}
	if waitError != nil {
		return nil, &OperationError{Code: "command_failed", ExitCode: exitCode}
	}
	return stdout.data, nil
}

func terminateProcessGroup(process *os.Process, signal syscall.Signal) {
	if process == nil {
		return
	}
	if err := syscall.Kill(-process.Pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = process.Signal(signal)
	}
}

type boundedWriter struct {
	data     []byte
	limit    int
	exceeded bool
}

func newBoundedWriter(limit int) boundedWriter {
	return boundedWriter{data: make([]byte, 0, min(limit, 64<<10)), limit: limit}
}

func (writer *boundedWriter) Write(data []byte) (int, error) {
	remaining := writer.limit - len(writer.data)
	if remaining > 0 {
		writer.data = append(writer.data, data[:min(remaining, len(data))]...)
	}
	if len(data) > remaining {
		writer.exceeded = true
	}
	return len(data), nil
}

type rawSnapshot struct {
	ID       string   `json:"id"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
	Paths    []string `json:"paths"`
}

func parseSnapshots(data []byte, expectedPath string) ([]Snapshot, error) {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) != 1 {
		return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
	}
	var raw []rawSnapshot
	if err := strictUnmarshal(frames[0], &raw); err != nil {
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
		identity, ok := snapshotIdentity(item.Tags)
		if !ok {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return nil, &OperationError{Code: "snapshots_result_invalid", ExitCode: -1}
		}
		seen[item.ID] = struct{}{}
		result = append(result, Snapshot{
			ID:            item.ID,
			Time:          formatTime(when),
			ProjectCommit: identity.projectCommit,
			ContentID:     identity.contentID,
			ArchiveID:     identity.archiveID,
		})
	}
	if result == nil {
		result = []Snapshot{}
	}
	return result, nil
}

type identityTags struct {
	projectCommit string
	contentID     string
	archiveID     string
}

func snapshotIdentity(tags []string) (identityTags, bool) {
	var result identityTags
	for _, tag := range tags {
		switch {
		case strings.HasPrefix(tag, "project_commit="):
			if result.projectCommit != "" {
				return identityTags{}, false
			}
			result.projectCommit = strings.TrimPrefix(tag, "project_commit=")
		case strings.HasPrefix(tag, "content_id="):
			if result.contentID != "" {
				return identityTags{}, false
			}
			result.contentID = strings.TrimPrefix(tag, "content_id=")
		case strings.HasPrefix(tag, "archive_id="):
			if result.archiveID != "" {
				return identityTags{}, false
			}
			result.archiveID = strings.TrimPrefix(tag, "archive_id=")
		}
	}
	return result, validCommit(result.projectCommit) && validDigest(result.contentID) && validDigest(result.archiveID)
}

func parseBackupSnapshotID(data []byte) (string, error) {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) == 0 {
		return "", fmt.Errorf("invalid backup output")
	}
	snapshotID := ""
	for index, frame := range frames {
		var message struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := strictUnmarshal(frame, &message); err != nil {
			return "", err
		}
		if message.MessageType == "summary" {
			if snapshotID != "" || index != len(frames)-1 {
				return "", fmt.Errorf("multiple backup summaries")
			}
			snapshotID = message.SnapshotID
		} else if snapshotID != "" || message.MessageType == "error" {
			return "", fmt.Errorf("invalid backup record order")
		}
	}
	if !validSnapshotID(snapshotID) {
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
	if len(bytes.TrimSpace(data)) == 0 {
		return []PruneGroup{}, nil
	}
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) != 1 {
		return nil, fmt.Errorf("invalid forget output")
	}
	var raw []forgetGroup
	if err := strictUnmarshal(frames[0], &raw); err != nil {
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
		groups = append(groups, parsed)
	}
	if groups == nil {
		groups = []PruneGroup{}
	}
	return groups, nil
}

func parseInitialized(data []byte) error {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) != 1 {
		return fmt.Errorf("invalid init output")
	}
	var result struct {
		MessageType string `json:"message_type"`
		ID          string `json:"id"`
	}
	if err := strictUnmarshal(frames[0], &result); err != nil || result.MessageType != "initialized" || !validSnapshotID(result.ID) {
		return fmt.Errorf("invalid init result")
	}
	return nil
}

func parseFinalSummary(data []byte, operation string) error {
	frames, err := decodeJSONFrames(data)
	if err != nil || len(frames) == 0 {
		return fmt.Errorf("missing final summary")
	}
	seen := false
	for index, frame := range frames {
		var header struct {
			MessageType string `json:"message_type"`
		}
		if err := strictUnmarshal(frame, &header); err != nil {
			return err
		}
		if header.MessageType == "error" || seen {
			return fmt.Errorf("invalid result record")
		}
		if header.MessageType != "summary" {
			continue
		}
		if index != len(frames)-1 {
			return fmt.Errorf("output follows final summary")
		}
		seen = true
		switch operation {
		case "check":
			var summary struct {
				MessageType string `json:"message_type"`
				NumErrors   int    `json:"num_errors"`
			}
			if err := strictUnmarshal(frame, &summary); err != nil || summary.NumErrors != 0 {
				return fmt.Errorf("check did not report success")
			}
		case "restore":
			var summary struct {
				MessageType   string `json:"message_type"`
				TotalFiles    uint64 `json:"total_files"`
				FilesRestored uint64 `json:"files_restored"`
				FilesSkipped  uint64 `json:"files_skipped"`
				TotalBytes    uint64 `json:"total_bytes"`
				BytesRestored uint64 `json:"bytes_restored"`
				BytesSkipped  uint64 `json:"bytes_skipped"`
			}
			if err := strictUnmarshal(frame, &summary); err != nil || summary.TotalFiles == 0 ||
				summary.FilesRestored != summary.TotalFiles || summary.FilesSkipped != 0 ||
				summary.BytesRestored != summary.TotalBytes || summary.BytesSkipped != 0 {
				return fmt.Errorf("restore did not report complete success")
			}
		default:
			return fmt.Errorf("unknown summary operation")
		}
	}
	if !seen {
		return fmt.Errorf("missing final summary")
	}
	return nil
}

func decodeJSONFrames(data []byte) ([]json.RawMessage, error) {
	if len(data) > maxCommandOutput || !utf8.Valid(data) {
		return nil, fmt.Errorf("JSON output exceeds bounds or is not UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	frames := make([]json.RawMessage, 0, 1)
	for {
		var frame json.RawMessage
		before := decoder.InputOffset()
		err := decoder.Decode(&frame)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(frame) == 0 || len(frame) > maxJSONFrame || decoder.InputOffset()-before > int64(maxJSONFrame) {
			return nil, fmt.Errorf("invalid or oversized JSON frame")
		}
		frames = append(frames, slices.Clone(frame))
		if len(frames) > maxJSONFrames {
			return nil, fmt.Errorf("too many JSON frames")
		}
	}
	return frames, nil
}

func strictUnmarshal(data []byte, destination any) error {
	if err := strictjson.RejectDuplicateKeys(data); err != nil {
		return err
	}
	return json.Unmarshal(data, destination)
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func validCommit(value string) bool {
	return len(value) == 40 && validHex(value)
}

func validDigest(value string) bool {
	return len(value) == len("sha256:")+64 && strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"))
}

func validHex(value string) bool {
	if value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
