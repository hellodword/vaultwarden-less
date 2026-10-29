package worker

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/state"
)

func nativeID(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func payloadName(value string) bool {
	suffix, ok := strings.CutPrefix(value, "payload-")
	if !ok || suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type durable struct {
	Source       Status                  `json:"source"`
	Targets      map[string]TargetStatus `json:"targets"`
	NextOrdinary time.Time               `json:"next_ordinary"`
	SourceRetry  time.Time               `json:"source_retry"`
	Reports      reportState             `json:"reports"`
}

func (w *Worker) Restore(store state.Store) error {
	w.store = store
	var saved durable
	if err := store.Load("worker.json", &saved); err != nil {
		return err
	}
	if saved.Source.Source != "" {
		w.status = saved.Source
	}
	w.schedule.nextOrdinary = saved.NextOrdinary
	w.schedule.retryAt = saved.SourceRetry
	if saved.Reports.Faults != nil {
		w.reportState = saved.Reports
	}
	for _, t := range w.targets {
		if t.config.Error != "" {
			continue
		}
		if previous, ok := saved.Targets[t.config.Name]; ok {
			t.status = previous
			t.status.Operation = ""
			t.status.Uploading = false
			t.status.State = "initializing"
		}
	}
	return nil
}
func (w *Worker) persist() error {
	saved := durable{Source: w.status, Targets: map[string]TargetStatus{}, NextOrdinary: w.schedule.nextOrdinary, SourceRetry: w.schedule.retryAt, Reports: w.reportState}
	for _, t := range w.targets {
		saved.Targets[t.config.Name] = t.status
	}
	return w.store.Save("worker.json", saved)
}
