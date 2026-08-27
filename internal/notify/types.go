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
