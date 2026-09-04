package notify

import (
	"fmt"
	"strings"
)

type Event struct {
	Type      string
	ContentID string
	ErrorCode string
	Targets   []TargetStatus
}

type TargetStatus struct {
	Name      string
	State     string
	ErrorCode string
}

func RecoveryPointCreated(contentID string, targets []TargetStatus) Event {
	return Event{Type: "recovery_point_created", ContentID: contentID, Targets: cloneTargets(targets)}
}

func SourceDegraded(errorCode string) Event {
	return Event{Type: "source_degraded", ErrorCode: errorCode}
}

func SourceRecovered() Event {
	return Event{Type: "source_recovered"}
}

func TargetDegraded(name, errorCode string) Event {
	return Event{
		Type:    "target_degraded",
		Targets: []TargetStatus{{Name: name, State: "degraded", ErrorCode: errorCode}},
	}
}

func TargetRecovered(name string) Event {
	return Event{Type: "target_recovered", Targets: []TargetStatus{{Name: name, State: "healthy"}}}
}

func (event Event) clone() Event {
	event.Targets = cloneTargets(event.Targets)
	return event
}

func cloneTargets(targets []TargetStatus) []TargetStatus {
	if targets == nil {
		return nil
	}
	return append([]TargetStatus(nil), targets...)
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
		if len(event.Targets) != 1 {
			return "Vaultwarden backup status changed"
		}
		return fmt.Sprintf("Vaultwarden backup target %s is degraded (%s)", event.Targets[0].Name, event.Targets[0].ErrorCode)
	case "target_recovered":
		if len(event.Targets) != 1 {
			return "Vaultwarden backup status changed"
		}
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
