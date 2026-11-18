// Package worker coordinates a single SQLite capture and one reconciliation per target.
package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type Repository interface {
	Sync(context.Context, config.Target, capture.Rule, string, string, restic.Baseline) (restic.Baseline, error)
}
type Notifications interface {
	Enqueue(notify.Event) error
	Status() []notify.Status
	Paused() bool
	Pause(string, error)
}
type TargetStatus struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Snapshot   string    `json:"snapshot,omitempty"`
	SnapshotAt time.Time `json:"snapshot_at,omitzero"`
	CheckedAt  time.Time `json:"checked_at,omitzero"`
	Error      string    `json:"error,omitempty"`
	RetryAt    time.Time `json:"retry_at,omitzero"`
}
type Status struct {
	Source             string            `json:"source"`
	SourceError        string            `json:"source_error,omitempty"`
	CheckedAt          time.Time         `json:"checked_at,omitzero"`
	Targets            []TargetStatus    `json:"targets"`
	NotificationErrors map[string]string `json:"notification_errors,omitempty"`
}
type target struct {
	cfg                config.Target
	status             TargetStatus
	baseline           restic.Baseline
	running, processed string
	failures           int
	fresh              bool
}
type result struct {
	target   *target
	path     string
	baseline restic.Baseline
	err      error
}

type Worker struct {
	mu                 sync.Mutex
	clock              timing.Clock
	source             func(context.Context) (string, error)
	rule               capture.Rule
	work               string
	repo               Repository
	targets            []*target
	status             Status
	schedule           schedule
	latest             string
	checking           bool
	wake               chan struct{}
	results            chan result
	wait               sync.WaitGroup
	notifications      Notifications
	notificationConfig config.Notifications
	reports            reportState
	store              state.Store
}

func New(cfg *config.Backup, b capture.Builder, repo Repository, notifications Notifications) *Worker {
	w := &Worker{
		clock: timing.Real{}, source: b.Build, rule: b.Rule, work: b.CacheDir, repo: repo,
		notifications: notifications, notificationConfig: cfg.Notifications,
		wake: make(chan struct{}, 1), results: make(chan result, len(cfg.Targets)+1),
	}
	w.status.Source = "initializing"
	for _, c := range cfg.Targets {
		s := TargetStatus{Name: c.Name, State: "initializing", Error: c.Error}
		if c.Error != "" {
			s.State = "failed"
		}
		w.targets = append(w.targets, &target{cfg: c, status: s})
	}
	return w
}
func (w *Worker) Event() {
	w.mu.Lock()
	w.schedule.event(w.clock.Now())
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.status
	s.Targets = make([]TargetStatus, 0, len(w.targets))
	for _, t := range w.targets {
		s.Targets = append(s.Targets, t.status)
	}
	if w.notifications != nil {
		for _, channel := range w.notifications.Status() {
			if channel.Error != "" {
				if s.NotificationErrors == nil {
					s.NotificationErrors = map[string]string{}
				}
				s.NotificationErrors[channel.Name] = channel.Error
			}
		}
	}
	return s
}

func (w *Worker) Run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); w.wait.Wait() }()
	w.mu.Lock()
	w.schedule.ordinary = w.clock.Now()
	w.mu.Unlock()
	for {
		w.mu.Lock()
		now := w.clock.Now()
		due := w.advance(ctx, now)
		if w.notifications != nil && !w.notifications.Paused() {
			if err := w.report(now); err != nil {
				w.notifications.Pause("reports.json", err)
			} else {
				due = earlier(due, w.reportDue())
			}
		}
		w.mu.Unlock()
		if due.IsZero() {
			due = now.Add(ordinaryInterval)
		}
		timer := w.clock.NewTimerAt(due)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.wake:
			timer.Stop()
		case <-timer.C():
		case r := <-w.results:
			timer.Stop()
			w.mu.Lock()
			w.finish(r, w.clock.Now())
			w.mu.Unlock()
		}
	}
}
func (w *Worker) advance(ctx context.Context, now time.Time) time.Time {
	var due time.Time
	needed := false
	for i, t := range w.targets {
		if t.cfg.Error != "" {
			continue
		}
		needed = true
		if !t.status.RetryAt.IsZero() {
			if t.status.RetryAt.After(now) {
				due = earlier(due, t.status.RetryAt)
				continue
			}
			t.status.RetryAt = time.Time{}
			t.fresh = true
			w.schedule.retry = now
		}
		if t.running != "" || t.fresh || w.latest == "" || t.processed == w.latest {
			continue
		}
		t.running = w.latest
		t.status.State = "checking"
		path, base := t.running, t.baseline
		cacheFile := filepath.Join(w.work, "baseline-"+strconv.Itoa(i))
		w.wait.Go(func() {
			c, cancel := context.WithTimeout(ctx, restic.UploadTimeout)
			defer cancel()
			base, err := w.repo.Sync(c, t.cfg, w.rule, path, cacheFile, base)
			w.results <- result{target: t, path: path, baseline: base, err: err}
		})
	}
	if !needed {
		w.status.Source = "unconfigured"
		return due
	}
	if w.checking {
		return due
	}
	next := w.schedule.due()
	if next.IsZero() || next.After(now) {
		return earlier(due, next)
	}
	w.schedule.take(now)
	for _, t := range w.targets {
		if t.fresh {
			t.processed = w.latest
			t.fresh = false
		}
	}
	w.checking = true
	w.wait.Go(func() {
		c, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		path, err := w.source(c)
		w.results <- result{path: path, err: err}
	})
	return due
}
func (w *Worker) finish(r result, now time.Time) {
	if r.target == nil {
		w.checking = false
		w.status.CheckedAt = now
		if r.err != nil {
			w.status.Source = "failed"
			w.status.SourceError = r.err.Error()
			w.schedule.retry = now.Add(sourceRetry)
			return
		}
		previous := w.latest
		w.latest = r.path
		w.status.Source = "ready"
		w.status.SourceError = ""
		w.release(previous)
		return
	}
	t := r.target
	t.running = ""
	t.processed = r.path
	t.status.CheckedAt = now
	if r.err != nil {
		t.failures++
		t.status.State = "failed"
		t.status.Error = r.err.Error()
		var e *restic.OperationError
		if errors.As(r.err, &e) {
			t.status.Error = e.Code
		}
		t.status.RetryAt = now.Add(targetBackoff(t.failures))
		// A failed reload may have replaced the cached file.
		t.baseline = restic.Baseline{}
	} else {
		t.failures = 0
		t.baseline = r.baseline
		t.status.State = "protected"
		t.status.Error = ""
		t.status.Snapshot = r.baseline.ID
		t.status.SnapshotAt = r.baseline.Time
	}
	w.release(r.path)
}

// Keep one latest candidate plus the candidates still in use by slow targets.
func (w *Worker) release(path string) {
	if path == "" || path == w.latest {
		return
	}
	for _, t := range w.targets {
		if t.running == path {
			return
		}
	}
	os.RemoveAll(filepath.Dir(path))
}
