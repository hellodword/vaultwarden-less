package state

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

type State struct {
	SchemaVersion     int               `json:"schema_version"`
	Dirty             bool              `json:"dirty"`
	DirtyGeneration   uint64            `json:"dirty_generation"`
	RunningGeneration uint64            `json:"running_generation"`
	BackupInProgress  bool              `json:"backup_in_progress"`
	FirstDirtyAt      string            `json:"first_dirty_at,omitempty"`
	LastDirtyAt       string            `json:"last_dirty_at,omitempty"`
	LastSampleAt      string            `json:"last_sample_at,omitempty"`
	LastSnapshotAt    string            `json:"last_snapshot_at,omitempty"`
	LastContentID     string            `json:"last_content_id,omitempty"`
	Current           *RecoveryPoint    `json:"current,omitempty"`
	Targets           map[string]Target `json:"targets"`
	LastAnnouncedID   string            `json:"last_announced_id,omitempty"`
}

type RecoveryPoint struct {
	ContentID string          `json:"content_id"`
	Status    snapshot.Status `json:"status"`
}

type Target struct {
	Name                string `json:"name"`
	State               string `json:"state"`
	ContentID           string `json:"content_id,omitempty"`
	SnapshotID          string `json:"snapshot_id,omitempty"`
	LastAttemptAt       string `json:"last_attempt_at,omitempty"`
	LastSuccessAt       string `json:"last_success_at,omitempty"`
	NextRetryAt         string `json:"next_retry_at,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	ErrorCode           string `json:"error_code,omitempty"`
}

func New(targetNames []string) State {
	targets := make(map[string]Target, len(targetNames))
	for _, name := range targetNames {
		targets[name] = Target{Name: name, State: "unknown"}
	}
	return State{SchemaVersion: version.StateVersion, Targets: targets}
}

func (s *State) Validate() error {
	if s.SchemaVersion != version.StateVersion {
		return fmt.Errorf("unsupported state schema_version %d", s.SchemaVersion)
	}
	if s.Targets == nil {
		return fmt.Errorf("state targets must not be null")
	}
	for name, target := range s.Targets {
		if !validName(name) || target.Name != name || !slices.Contains([]string{"unknown", "healthy", "degraded", "catching_up"}, target.State) {
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
		if target.ContentID != "" {
			if _, err := snapshot.ContentIDHex(target.ContentID); err != nil {
				return fmt.Errorf("invalid target content ID")
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
	if s.Current != nil {
		if _, err := snapshot.ContentIDHex(s.Current.ContentID); err != nil || s.Current.Status != snapshot.Complete && s.Current.Status != snapshot.Incomplete {
			return fmt.Errorf("invalid current recovery point")
		}
	}
	for _, contentID := range []string{s.LastContentID, s.LastAnnouncedID} {
		if contentID != "" {
			if _, err := snapshot.ContentIDHex(contentID); err != nil {
				return fmt.Errorf("invalid state content ID")
			}
		}
	}
	if s.RunningGeneration > s.DirtyGeneration {
		return fmt.Errorf("invalid dirty generation")
	}
	return nil
}

func (s State) Clone() State {
	data, _ := json.Marshal(s)
	var clone State
	_ = json.Unmarshal(data, &clone)
	return clone
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
