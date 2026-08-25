package state

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

type State struct {
	SchemaVersion           int               `json:"schema_version"`
	Dirty                   bool              `json:"dirty"`
	DirtyGeneration         uint64            `json:"dirty_generation"`
	RunningGeneration       uint64            `json:"running_generation"`
	BackupInProgress        bool              `json:"backup_in_progress"`
	FirstDirtyAt            string            `json:"first_dirty_at,omitempty"`
	LastDirtyAt             string            `json:"last_dirty_at,omitempty"`
	LastSampleAt            string            `json:"last_sample_at,omitempty"`
	LastSnapshotAt          string            `json:"last_snapshot_at,omitempty"`
	LatestCompleteContentID string            `json:"latest_complete_content_id,omitempty"`
	Current                 *RecoveryPoint    `json:"current,omitempty"`
	Source                  Source            `json:"source"`
	Targets                 map[string]Target `json:"targets"`
	LastAnnouncedID         string            `json:"last_announced_id,omitempty"`
}

type Source struct {
	State               string `json:"state"`
	LastAttemptAt       string `json:"last_attempt_at,omitempty"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	NextRetryAt         string `json:"next_retry_at,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	ErrorCode           string `json:"error_code,omitempty"`
}

type RecoveryPoint struct {
	ContentID string          `json:"content_id"`
	Status    snapshot.Status `json:"status"`
}

type UploadIntent struct {
	ContentID string          `json:"content_id"`
	Status    snapshot.Status `json:"status"`
	StartedAt string          `json:"started_at"`
}

type Target struct {
	Name                      string         `json:"name"`
	State                     string         `json:"state"`
	UploadIntent              *UploadIntent  `json:"upload_intent,omitempty"`
	LastRecoveryPoint         *RecoveryPoint `json:"last_recovery_point,omitempty"`
	LastCompleteRecoveryPoint *RecoveryPoint `json:"last_complete_recovery_point,omitempty"`
	SnapshotID                string         `json:"snapshot_id,omitempty"`
	LastAttemptAt             string         `json:"last_attempt_at,omitempty"`
	LastSuccessAt             string         `json:"last_success_at,omitempty"`
	NextRetryAt               string         `json:"next_retry_at,omitempty"`
	ConsecutiveFailures       int            `json:"consecutive_failures"`
	ErrorCode                 string         `json:"error_code,omitempty"`
}

func New(targetNames []string) State {
	targets := make(map[string]Target, len(targetNames))
	for _, name := range targetNames {
		targets[name] = Target{Name: name, State: "unknown"}
	}
	return State{SchemaVersion: version.StateVersion, Source: Source{State: "unknown"}, Targets: targets}
}

func (s *State) Validate() error {
	if s.SchemaVersion != version.StateVersion {
		return fmt.Errorf("unsupported state schema_version %d", s.SchemaVersion)
	}
	if s.Targets == nil {
		return fmt.Errorf("state targets must not be null")
	}
	if err := validateSource(s.Source); err != nil {
		return err
	}
	for name, target := range s.Targets {
		if !validName(name) || target.Name != name || !validTargetState(target.State) {
			return fmt.Errorf("invalid target state")
		}
		if target.ConsecutiveFailures < 0 || target.ConsecutiveFailures > 1_000_000 || !validErrorCode(target.ErrorCode) {
			return fmt.Errorf("invalid target counters or error code")
		}
		for _, raw := range []string{target.LastAttemptAt, target.LastSuccessAt, target.NextRetryAt} {
			if raw != "" {
				if _, err := ParseTime(raw); err != nil {
					return fmt.Errorf("invalid target timestamp")
				}
			}
		}
		if err := validateRecoveryPoint(target.LastRecoveryPoint, false); err != nil {
			return fmt.Errorf("invalid target recovery point")
		}
		if err := validateRecoveryPoint(target.LastCompleteRecoveryPoint, true); err != nil {
			return fmt.Errorf("invalid target complete recovery point")
		}
		if target.UploadIntent != nil {
			if _, err := ContentID(target.UploadIntent.ContentID); err != nil || !validStatus(target.UploadIntent.Status) {
				return fmt.Errorf("invalid target upload intent")
			}
			if _, err := ParseTime(target.UploadIntent.StartedAt); err != nil {
				return fmt.Errorf("invalid target upload intent timestamp")
			}
		}
		if target.SnapshotID != "" && !validSnapshotID(target.SnapshotID) {
			return fmt.Errorf("invalid target snapshot ID")
		}
	}
	for _, raw := range []string{s.FirstDirtyAt, s.LastDirtyAt, s.LastSampleAt, s.LastSnapshotAt} {
		if raw != "" {
			if _, err := ParseTime(raw); err != nil {
				return fmt.Errorf("invalid state timestamp")
			}
		}
	}
	if err := validateRecoveryPoint(s.Current, false); err != nil {
		return fmt.Errorf("invalid current recovery point")
	}
	if s.LatestCompleteContentID != "" {
		if _, err := ContentID(s.LatestCompleteContentID); err != nil {
			return fmt.Errorf("invalid latest complete content ID")
		}
	}
	if s.LastAnnouncedID != "" {
		if _, err := ContentID(s.LastAnnouncedID); err != nil {
			return fmt.Errorf("invalid state content ID")
		}
	}
	if s.RunningGeneration > s.DirtyGeneration {
		return fmt.Errorf("invalid dirty generation")
	}
	return nil
}

func validateSource(source Source) error {
	if source.State != "unknown" && source.State != "healthy" && source.State != "degraded" {
		return fmt.Errorf("invalid source state")
	}
	if source.ConsecutiveFailures < 0 || source.ConsecutiveFailures > 1_000_000 || !validErrorCode(source.ErrorCode) {
		return fmt.Errorf("invalid source counters or error code")
	}
	for _, raw := range []string{source.LastAttemptAt, source.LastSuccessAt, source.NextRetryAt} {
		if raw != "" {
			if _, err := ParseTime(raw); err != nil {
				return fmt.Errorf("invalid source timestamp")
			}
		}
	}
	return nil
}

func validateRecoveryPoint(point *RecoveryPoint, completeOnly bool) error {
	if point == nil {
		return nil
	}
	if _, err := ContentID(point.ContentID); err != nil || !validStatus(point.Status) || completeOnly && point.Status != snapshot.Complete {
		return fmt.Errorf("invalid recovery point")
	}
	return nil
}

func validStatus(status snapshot.Status) bool {
	return status == snapshot.Complete || status == snapshot.Incomplete
}
func validTargetState(value string) bool {
	return value == "unknown" || value == "healthy" || value == "degraded" || value == "catching_up"
}

func (s State) Clone() State {
	clone := s
	clone.Source = s.Source
	clone.Current = cloneRecoveryPoint(s.Current)
	clone.Targets = make(map[string]Target, len(s.Targets))
	for name, target := range s.Targets {
		target.UploadIntent = cloneUploadIntent(target.UploadIntent)
		target.LastRecoveryPoint = cloneRecoveryPoint(target.LastRecoveryPoint)
		target.LastCompleteRecoveryPoint = cloneRecoveryPoint(target.LastCompleteRecoveryPoint)
		clone.Targets[name] = target
	}
	return clone
}

func cloneRecoveryPoint(point *RecoveryPoint) *RecoveryPoint {
	if point == nil {
		return nil
	}
	clone := *point
	return &clone
}

func cloneUploadIntent(intent *UploadIntent) *UploadIntent {
	if intent == nil {
		return nil
	}
	clone := *intent
	return &clone
}

func FormatTime(value time.Time) string {
	return value.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

func ParseTime(value string) (time.Time, error) {
	parsed, err := time.Parse("2006-01-02T15:04:05.000Z", value)
	if err != nil || FormatTime(parsed) != value {
		return time.Time{}, fmt.Errorf("timestamp must be UTC RFC 3339 with millisecond precision")
	}
	return parsed, nil
}

func validErrorCode(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 64 || strings.HasPrefix(value, "_") || strings.HasSuffix(value, "_") {
		return false
	}
	for _, char := range value {
		if char != '_' && (char < 'a' || char > 'z') {
			return false
		}
	}
	return true
}

func validName(value string) bool {
	if len(value) < 1 || len(value) > 63 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value[1:] {
		if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func validSnapshotID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ContentID(value string) (string, error) { return snapshot.ContentIDHex(value) }

type Status struct {
	SchemaVersion        int            `json:"schema_version"`
	Dirty                bool           `json:"dirty"`
	BackupInProgress     bool           `json:"backup_in_progress"`
	Source               StatusSource   `json:"source"`
	LastSampleAt         string         `json:"last_sample_at"`
	LastSnapshotAt       string         `json:"last_snapshot_at"`
	PendingRecoveryPoint *RecoveryPoint `json:"pending_recovery_point"`
	Protection           Protection     `json:"protection"`
	Targets              []Target       `json:"targets"`
}

// StatusSource is the stable public representation of source health. Unlike
// the on-disk Source type, empty timestamps and error codes remain present so
// status consumers do not need shape-dependent decoding.
type StatusSource struct {
	State               string `json:"state"`
	LastAttemptAt       string `json:"last_attempt_at"`
	LastSuccessAt       string `json:"last_success_at"`
	NextRetryAt         string `json:"next_retry_at"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	ErrorCode           string `json:"error_code"`
}

type Protection struct {
	State                     string   `json:"state"`
	LatestCompleteContentID   string   `json:"latest_complete_content_id"`
	ConfiguredTargets         int      `json:"configured_targets"`
	TargetsWithLatestComplete []string `json:"targets_with_latest_complete"`
}

func (s State) PublicStatus(configuredTargets []string) Status {
	names := append([]string(nil), configuredTargets...)
	sort.Strings(names)
	targets := make([]Target, 0, len(names))
	withLatest := make([]string, 0, len(names))
	for _, name := range names {
		if target, exists := s.Targets[name]; exists {
			targets = append(targets, target)
			if s.LatestCompleteContentID != "" && target.LastCompleteRecoveryPoint != nil && target.LastCompleteRecoveryPoint.ContentID == s.LatestCompleteContentID {
				withLatest = append(withLatest, name)
			}
		}
	}
	protectionState := "unprotected"
	if s.LatestCompleteContentID != "" && len(withLatest) > 0 {
		protectionState = "protected"
	}
	return Status{
		SchemaVersion:    version.SchemaVersion,
		Dirty:            s.Dirty,
		BackupInProgress: s.BackupInProgress,
		Source: StatusSource{
			State:               s.Source.State,
			LastAttemptAt:       s.Source.LastAttemptAt,
			LastSuccessAt:       s.Source.LastSuccessAt,
			NextRetryAt:         s.Source.NextRetryAt,
			ConsecutiveFailures: s.Source.ConsecutiveFailures,
			ErrorCode:           s.Source.ErrorCode,
		},
		LastSampleAt:         s.LastSampleAt,
		LastSnapshotAt:       s.LastSnapshotAt,
		PendingRecoveryPoint: cloneRecoveryPoint(s.Current),
		Protection: Protection{
			State: protectionState, LatestCompleteContentID: s.LatestCompleteContentID,
			ConfiguredTargets: len(names), TargetsWithLatestComplete: withLatest,
		},
		Targets: targets,
	}
}
