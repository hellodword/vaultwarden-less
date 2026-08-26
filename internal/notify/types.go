package notify

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

type Event struct {
	SchemaVersion int            `json:"schema_version"`
	ID            string         `json:"id"`
	Type          string         `json:"type"`
	OccurredAt    string         `json:"occurred_at"`
	ContentID     string         `json:"content_id,omitempty"`
	State         string         `json:"state,omitempty"`
	ErrorCode     string         `json:"error_code,omitempty"`
	Targets       []TargetStatus `json:"targets"`
}

type TargetStatus struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	ErrorCode string `json:"error_code,omitempty"`
}

func NewRecoveryEvent(id, occurredAt, contentID string, targets []TargetStatus) Event {
	targets = slices.Clone(targets)
	if targets == nil {
		targets = []TargetStatus{}
	}
	return Event{
		SchemaVersion: version.SchemaVersion,
		ID:            id,
		Type:          "recovery_point_created",
		OccurredAt:    occurredAt,
		ContentID:     contentID,
		Targets:       targets,
	}
}

func NewSourceEvent(id, eventType, occurredAt, errorCode string) Event {
	state := "degraded"
	if eventType == "source_recovered" {
		state = "healthy"
		errorCode = ""
	}
	return Event{
		SchemaVersion: version.SchemaVersion,
		ID:            id,
		Type:          eventType,
		OccurredAt:    occurredAt,
		State:         state,
		ErrorCode:     errorCode,
		Targets:       []TargetStatus{},
	}
}

func NewTargetEvent(id, eventType, occurredAt string, target TargetStatus) Event {
	return Event{
		SchemaVersion: version.SchemaVersion,
		ID:            id,
		Type:          eventType,
		OccurredAt:    occurredAt,
		Targets:       []TargetStatus{target},
	}
}

func (event Event) Validate() error {
	if event.SchemaVersion != version.SchemaVersion || event.ID == "" || len(event.ID) > 256 || strings.ContainsAny(event.ID, "\r\n\x00") {
		return fmt.Errorf("invalid notification envelope")
	}
	when, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
	if err != nil || when.IsZero() || when.Location() != time.UTC || event.Targets == nil {
		return fmt.Errorf("invalid notification timestamp or targets")
	}
	seen := map[string]struct{}{}
	for _, target := range event.Targets {
		if err := target.validate(); err != nil {
			return err
		}
		if _, duplicate := seen[target.Name]; duplicate {
			return fmt.Errorf("duplicate notification target")
		}
		seen[target.Name] = struct{}{}
	}
	switch event.Type {
	case "recovery_point_created":
		if !snapshot.IsContentID(event.ContentID) || event.State != "" || event.ErrorCode != "" {
			return fmt.Errorf("invalid recovery notification")
		}
		created := false
		for _, target := range event.Targets {
			switch target.State {
			case "created":
				created = true
				if target.ErrorCode != "" {
					return fmt.Errorf("created target has an error")
				}
			case "synced":
				if target.ErrorCode != "" {
					return fmt.Errorf("synced target has an error")
				}
			case "failed":
				if target.ErrorCode == "" {
					return fmt.Errorf("failed target lacks an error")
				}
			default:
				return fmt.Errorf("invalid recovery target state")
			}
		}
		if !created {
			return fmt.Errorf("recovery notification lacks a created target")
		}
	case "source_degraded":
		if event.ContentID != "" || event.State != "degraded" || event.ErrorCode == "" || len(event.Targets) != 0 {
			return fmt.Errorf("invalid source degraded notification")
		}
	case "source_recovered":
		if event.ContentID != "" || event.State != "healthy" || event.ErrorCode != "" || len(event.Targets) != 0 {
			return fmt.Errorf("invalid source recovered notification")
		}
	case "target_degraded":
		if event.ContentID != "" || event.State != "" || event.ErrorCode != "" || len(event.Targets) != 1 || event.Targets[0].State != "degraded" || event.Targets[0].ErrorCode == "" {
			return fmt.Errorf("invalid target degraded notification")
		}
	case "target_recovered":
		if event.ContentID != "" || event.State != "" || event.ErrorCode != "" || len(event.Targets) != 1 || event.Targets[0].State != "healthy" || event.Targets[0].ErrorCode != "" {
			return fmt.Errorf("invalid target recovered notification")
		}
	default:
		return fmt.Errorf("invalid notification type")
	}
	return nil
}

func (target TargetStatus) validate() error {
	if len(target.Name) < 1 || len(target.Name) > 63 || target.Name[0] < 'a' || target.Name[0] > 'z' || !validErrorCode(target.ErrorCode) {
		return fmt.Errorf("invalid notification target")
	}
	for _, character := range target.Name[1:] {
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
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
	for _, character := range value {
		if character != '_' && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func (event Event) Message() string {
	switch event.Type {
	case "recovery_point_created":
		created, synced, failed := make([]string, 0), make([]string, 0), make([]string, 0)
		for _, target := range event.Targets {
			switch target.State {
			case "created":
				created = append(created, target.Name)
			case "synced":
				synced = append(synced, target.Name)
			case "failed":
				failed = append(failed, target.Name)
			}
		}
		return fmt.Sprintf(
			"Vaultwarden recovery point %s was created; created=[%s]; synced=[%s]; failed=[%s]",
			shortID(event.ContentID), strings.Join(created, ","), strings.Join(synced, ","), strings.Join(failed, ","),
		)
	case "source_degraded":
		return fmt.Sprintf("Vaultwarden backup source is degraded (%s)", event.ErrorCode)
	case "source_recovered":
		return "Vaultwarden backup source has recovered"
	case "target_degraded":
		return fmt.Sprintf("Vaultwarden backup target %s is degraded (%s)", event.Targets[0].Name, event.Targets[0].ErrorCode)
	case "target_recovered":
		return fmt.Sprintf("Vaultwarden backup target %s has recovered", event.Targets[0].Name)
	default:
		return "Vaultwarden backup status changed"
	}
}

func shortID(value string) string {
	if len(value) > 19 {
		return value[:19]
	}
	return value
}
