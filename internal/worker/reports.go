package worker

import (
	"maps"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/state"
)

type reportState struct {
	Faults    map[string]notify.Fault
	NextDaily time.Time
}

func (w *Worker) Restore(store state.Store) error {
	w.store = store
	if w.notifications != nil {
		return store.Load("reports.json", &w.reports)
	}
	return nil
}
func (w *Worker) reportDue() time.Time {
	due := w.reports.NextDaily
	for _, f := range w.reports.Faults {
		due = earlier(due, f.NextAt)
	}
	return due
}
func (w *Worker) nextDaily(now time.Time) time.Time {
	n := w.notificationConfig
	local := now.In(n.Location)
	clock, _ := time.Parse("15:04", n.DailyAt)
	next := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, n.Location)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UTC()
}
func (w *Worker) report(now time.Time) error {
	r := &w.reports
	dirty := false
	if r.NextDaily.IsZero() {
		r.NextDaily = w.nextDaily(now)
		dirty = true
	}
	if r.Faults == nil {
		r.Faults = map[string]notify.Fault{}
	}
	events := map[string]map[string]notify.Fault{}
	emit := func(kind, key string, f notify.Fault) {
		if events[kind] == nil {
			events[kind] = map[string]notify.Fault{}
		}
		events[kind][key] = f
		dirty = true
	}
	observe := func(key, name, code string, known bool) {
		if !known {
			return
		} // Startup alone cannot clear a persisted incident.
		old, exists := r.Faults[key]
		if code == "" {
			if exists {
				old.Recovered = now
				emit("Recovery", key, old)
				delete(r.Faults, key)
			}
			return
		}
		if !exists || old.Code != code {
			f := notify.Fault{Object: name, Code: code, First: now, NextAt: now.Add(w.notificationConfig.ReminderEvery)}
			r.Faults[key] = f
			emit("Fault", key, f)
		}
	}
	sourceError := w.status.SourceError
	if w.status.Source == "unconfigured" {
		sourceError = "no_usable_targets"
	}
	observe("source", "SQLite", sourceError, w.status.Source != "initializing")
	names := map[string]bool{"source": true}
	var targets []notify.TargetStatus
	for _, t := range w.targets {
		s := t.status
		key := "target:" + s.Name
		names[key] = true
		observe(key, s.Name, s.Error, s.Error != "" || s.State == "protected")
		targets = append(targets, notify.TargetStatus{Name: s.Name, State: s.State, Checked: s.CheckedAt, Snapshot: s.SnapshotAt, Next: s.RetryAt})
	}
	for key, f := range r.Faults {
		if !names[key] {
			delete(r.Faults, key)
			dirty = true
			continue
		}
		if !f.NextAt.After(now) {
			f.NextAt = now.Add(w.notificationConfig.ReminderEvery)
			r.Faults[key] = f
			emit("Reminder", key, f)
		}
	}
	if !r.NextDaily.After(now) {
		events["Daily"] = maps.Clone(r.Faults)
		r.NextDaily = w.nextDaily(now)
		dirty = true
	}
	for _, kind := range []string{"Fault", "Recovery", "Reminder", "Daily"} {
		if faults, ok := events[kind]; ok {
			if err := w.notifications.Enqueue(notify.Event{Kind: kind, At: now, Timezone: w.notificationConfig.Timezone, Source: w.status.Source, Targets: targets, Faults: faults, ActiveFaults: maps.Clone(r.Faults), Count: 1}); err != nil {
				return err
			}
		}
	}
	if dirty {
		return w.store.Save("reports.json", r)
	}
	return nil
}
