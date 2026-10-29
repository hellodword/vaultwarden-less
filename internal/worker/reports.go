package worker

import (
	"maps"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/notify"
)

type Fault = notify.Fault
type reportState struct {
	Faults          map[string]Fault
	Reminders       map[string]time.Time
	PeriodStart     time.Time
	NextDaily       time.Time
	Checks, Changes uint64
	Snapshots       map[string]uint64
}

func (w *Worker) reportDue(now time.Time) time.Time {
	due := w.reportState.NextDaily
	for _, at := range w.reportState.Reminders {
		due = earlier(due, at)
	}
	return due
}
func (w *Worker) nextDaily(now time.Time) time.Time {
	location := w.notificationConfig.Location
	if location == nil {
		location = time.UTC
	}
	at := w.notificationConfig.DailyAt
	if at == "" {
		at = "21:00"
	}
	parsed, _ := time.Parse("15:04", at)
	local := now.In(location)
	next := time.Date(local.Year(), local.Month(), local.Day(), parsed.Hour(), parsed.Minute(), 0, 0, location)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UTC()
}
func (w *Worker) faults(now time.Time) map[string]Fault {
	faults := map[string]Fault{}
	add := func(key, object, category, code string, complete, next time.Time, details map[string]int) {
		f := Fault{Object: object, Category: category, Code: code, First: now, Last: now, Occurrences: 1, CompleteAt: complete, NextAt: next, Details: maps.Clone(details)}
		if old, ok := w.reportState.Faults[key]; ok {
			f.First = old.First
			f.Occurrences = old.Occurrences
		}
		faults[key] = f
	}
	if w.status.Source == "failed" || w.status.Source == "incomplete" {
		add("source", "Source", "capture", w.status.Source, time.Time{}, w.schedule.retryAt, w.status.Issues)
	}
	for _, t := range w.targets {
		s := t.status
		if s.Error != "" {
			category := "upload"
			if t.config.Error != "" {
				category = "configuration"
			}
			if s.Error == "coverage_missing" {
				category = "coverage"
			}
			add("target:"+s.Name, s.Name, category, s.Error, s.CompleteAt, s.RetryAt, nil)
		}
		for _, entry := range []struct {
			category string
			v        VerificationStatus
		}{{"structure", s.Structure}, {"read_data", s.ReadData}} {
			if entry.v.Error != "" {
				add("verify:"+s.Name+":"+entry.category, s.Name, entry.category, entry.v.Error, s.CompleteAt, earlier(entry.v.Next, entry.v.RetryAt), nil)
			}
		}
	}
	return faults
}
func (w *Worker) event(kind string, now time.Time, faults map[string]Fault) notify.Event {
	timezone := w.notificationConfig.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	e := notify.Event{Kind: kind, At: now, Timezone: timezone, Count: 1, Source: w.status.Source, Issues: maps.Clone(w.status.Issues), Warnings: append([]string(nil), w.status.Warnings...), Faults: faults, ActiveFaults: maps.Clone(w.reportState.Faults)}
	for _, t := range w.targets {
		s := t.status
		e.Targets = append(e.Targets, notify.TargetStatus{Name: s.Name, State: s.State, Waiting: s.Waiting, Uploading: s.Uploading, Failed: s.Error != "", LastUpload: s.SuccessAt, LastComplete: s.CompleteAt, Next: s.RetryAt, Structure: s.Structure.Result, ReadData: s.ReadData.Result, StructureAt: s.Structure.LastAttempt, ReadDataAt: s.ReadData.LastAttempt, StructureNext: s.Structure.Next, ReadDataNext: s.ReadData.Next, StructureScheduled: s.Structure.Scheduled, ReadDataScheduled: s.ReadData.Scheduled})
	}
	return e
}
func (w *Worker) report(now time.Time) error {
	r := &w.reportState
	if r.Reminders == nil {
		r.Reminders = map[string]time.Time{}
	}
	if r.PeriodStart.IsZero() {
		r.PeriodStart = now
		r.NextDaily = w.nextDaily(now)
	}
	interval := w.notificationConfig.ReminderEvery
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	current := w.faults(now)
	changed, recovered, reminded := map[string]Fault{}, map[string]Fault{}, map[string]Fault{}
	for key, f := range current {
		old, exists := r.Faults[key]
		expanded := false
		for code, n := range f.Details {
			if n > old.Details[code] {
				expanded = true
			}
		}
		if !exists || old.Code != f.Code || expanded {
			changed[key] = f
			if !exists {
				r.Reminders[key] = now.Add(interval)
			}
		}
		if at := r.Reminders[key]; !at.IsZero() && !at.After(now) {
			reminded[key] = f
			r.Reminders[key] = now.Add(interval)
		}
	}
	for key, old := range r.Faults {
		if _, ok := current[key]; !ok {
			old.Recovered = now
			old.Last = now
			recovered[key] = old
			delete(r.Reminders, key)
		}
	}
	r.Faults = current
	send := func(e notify.Event) error {
		if w.notifications != nil {
			return w.notifications.Enqueue(e)
		}
		return nil
	}
	for _, item := range []struct {
		kind   string
		faults map[string]Fault
	}{{"Fault", changed}, {"Recovery", recovered}, {"Reminder", reminded}} {
		if len(item.faults) > 0 {
			if err := send(w.event(item.kind, now, item.faults)); err != nil {
				return err
			}
		}
	}
	if !r.NextDaily.After(now) {
		e := w.event("Daily", now, maps.Clone(current))
		e.PeriodStart = r.PeriodStart
		e.PeriodEnd = now
		e.Checks = w.status.Checks - r.Checks
		e.Changes = w.status.Changes - r.Changes
		e.Snapshots = map[string]uint64{}
		for _, t := range w.targets {
			// A changed repository identity starts a new target counter.
			count := t.status.Snapshots
			if previous := r.Snapshots[t.config.Name]; count >= previous {
				count -= previous
			}
			e.Snapshots[t.config.Name] = count
		}
		if err := send(e); err != nil {
			return err
		}
		r.PeriodStart = now
		r.NextDaily = w.nextDaily(now)
		r.Checks = w.status.Checks
		r.Changes = w.status.Changes
		r.Snapshots = map[string]uint64{}
		for _, t := range w.targets {
			r.Snapshots[t.config.Name] = t.status.Snapshots
		}
	}
	return nil
}
