package notify

import (
	"fmt"
	"slices"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

type Event struct {
	SchemaVersion   int            `json:"schema_version"`
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	OccurredAt      string         `json:"occurred_at"`
	RecoveryPoint   *RecoveryPoint `json:"recovery_point,omitempty"`
	Target          *TargetStatus  `json:"target,omitempty"`
	Targets         []TargetStatus `json:"targets,omitempty"`
	MergedSuccesses uint64         `json:"merged_successes,omitempty"`
}

type RecoveryPoint struct {
	ContentID string          `json:"content_id"`
	Status    snapshot.Status `json:"status"`
}

type TargetStatus struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`
}

func NewRecoveryEvent(id, occurredAt string, point RecoveryPoint, targets []TargetStatus) Event {
	targets = slices.Clone(targets)
	if targets == nil {
		targets = []TargetStatus{}
	}
	slices.SortFunc(targets, func(a, b TargetStatus) int { return strings.Compare(a.Name, b.Name) })
	return Event{
		SchemaVersion: version.SchemaVersion,
		ID:            id,
		Type:          "recovery_point_available",
		OccurredAt:    occurredAt,
		RecoveryPoint: &point,
		Targets:       targets,
	}
}

func NewTargetEvent(id, eventType, occurredAt string, target TargetStatus) Event {
	return Event{SchemaVersion: version.SchemaVersion, ID: id, Type: eventType, OccurredAt: occurredAt, Target: &target}
}

func (e Event) Validate() error {
	if e.SchemaVersion != version.SchemaVersion || e.ID == "" || len(e.ID) > 256 || strings.ContainsAny(e.ID, "\r\n\x00") {
		return fmt.Errorf("invalid notification envelope")
	}
	if _, err := state.ParseTime(e.OccurredAt); err != nil {
		return fmt.Errorf("invalid notification timestamp")
	}
	switch e.Type {
	case "recovery_point_available":
		if e.RecoveryPoint == nil || e.Target != nil || e.RecoveryPoint.Status != snapshot.Complete && e.RecoveryPoint.Status != snapshot.Incomplete {
			return fmt.Errorf("invalid recovery-point notification")
		}
		if _, err := snapshot.ContentIDHex(e.RecoveryPoint.ContentID); err != nil {
			return fmt.Errorf("invalid recovery-point content ID")
		}
		previous := ""
		for _, target := range e.Targets {
			if err := target.validate(); err != nil || target.Name <= previous {
				return fmt.Errorf("invalid or unsorted recovery-point targets")
			}
			previous = target.Name
		}
	case "target_degraded", "target_recovered":
		if e.Target == nil || e.RecoveryPoint != nil || len(e.Targets) != 0 || e.MergedSuccesses != 0 || e.Target.validate() != nil {
			return fmt.Errorf("invalid target-state notification")
		}
		if e.Type == "target_degraded" && (e.Target.State != "degraded" || e.Target.ErrorCode == "") ||
			e.Type == "target_recovered" && (e.Target.State != "healthy" || e.Target.ErrorCode != "") {
			return fmt.Errorf("target event type and state disagree")
		}
	case "source_degraded", "source_recovered":
		if e.Target == nil || e.RecoveryPoint != nil || len(e.Targets) != 0 || e.MergedSuccesses != 0 || e.Target.Name != "source" {
			return fmt.Errorf("invalid source-state notification")
		}
		if e.Type == "source_degraded" && (e.Target.State != "degraded" || e.Target.ErrorCode == "") ||
			e.Type == "source_recovered" && (e.Target.State != "healthy" || e.Target.ErrorCode != "") {
			return fmt.Errorf("source event type and state disagree")
		}
	default:
		return fmt.Errorf("invalid notification type")
	}
	return nil
}

func (t TargetStatus) validate() error {
	if len(t.Name) < 1 || len(t.Name) > 63 || t.Name[0] < 'a' || t.Name[0] > 'z' ||
		!slices.Contains([]string{"unknown", "catching_up", "healthy", "degraded"}, t.State) || !validErrorCode(t.ErrorCode) {
		return fmt.Errorf("invalid notification target")
	}
	for _, char := range t.Name[1:] {
		if char != '-' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return fmt.Errorf("invalid notification target name")
		}
	}
	return nil
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

func (e Event) Message() string {
	switch e.Type {
	case "recovery_point_available":
		message := fmt.Sprintf("Vaultwarden %s recovery point %s is available on at least one target", e.RecoveryPoint.Status, shortID(e.RecoveryPoint.ContentID))
		if e.MergedSuccesses > 0 {
			message += fmt.Sprintf(" (%d earlier success notifications were merged)", e.MergedSuccesses)
		}
		return message
	case "target_degraded":
		return fmt.Sprintf("Vaultwarden backup target %s is degraded (%s)", e.Target.Name, e.Target.ErrorCode)
	case "target_recovered":
		return fmt.Sprintf("Vaultwarden backup target %s has caught up", e.Target.Name)
	case "source_degraded":
		return fmt.Sprintf("Vaultwarden source is degraded (%s)", e.Target.ErrorCode)
	case "source_recovered":
		return "Vaultwarden backup source has recovered"
	default:
		return "Vaultwarden backup status changed"
	}
}

func NewSourceEvent(id, eventType, occurredAt, errorCode string) Event {
	state := "degraded"
	if eventType == "source_recovered" {
		state = "healthy"
		errorCode = ""
	}
	return NewTargetEvent(id, eventType, occurredAt, TargetStatus{Name: "source", State: state, ErrorCode: errorCode})
}

func shortID(id string) string {
	if len(id) > 19 {
		return id[:19]
	}
	return id
}
