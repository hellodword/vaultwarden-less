package worker

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/routes"
	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type Repository interface {
	Latest(context.Context, config.Target) (*restic.Snapshot, error)
	Backup(context.Context, config.Target, string, string) (string, error)
}
type Notifications interface {
	Enqueue(notify.Event)
	Status() []notify.Status
}
type TargetStatus struct {
	Name      string    `json:"name"`
	SuccessID string    `json:"success_id,omitempty"`
	SuccessAt time.Time `json:"success_at,omitzero"`
	Waiting   bool      `json:"waiting"`
	Uploading bool      `json:"uploading"`
	RetryAt   time.Time `json:"retry_at,omitzero"`
	Error     string    `json:"error,omitempty"`
	State     string    `json:"state"`
}
type Status struct {
	Configured    bool            `json:"configured"`
	Checking      bool            `json:"checking"`
	Mode          string          `json:"mode"`
	FullPending   bool            `json:"full_pending"`
	LastCheck     time.Time       `json:"last_check,omitzero"`
	LastResult    string          `json:"last_result"`
	NextOrdinary  time.Time       `json:"next_ordinary,omitzero"`
	NextFull      time.Time       `json:"next_full,omitzero"`
	SourceRetryAt time.Time       `json:"source_retry_at,omitzero"`
	Source        string          `json:"source"`
	Issues        map[string]int  `json:"issues"`
	Warnings      []string        `json:"warnings"`
	Targets       []TargetStatus  `json:"targets"`
	Notifiers     []notify.Status `json:"notifiers"`
}
type targetState struct {
	config            config.Target
	status            TargetStatus
	initialized       bool
	baseline, running *capture.Payload
	full, runningFull bool
	failures          int
}
type sourceResult struct {
	payload *capture.Payload
	err     error
}
type targetResult struct {
	index    int
	initial  bool
	baseline *capture.Payload
	snapshot *restic.Snapshot
	id       string
	err      error
}
type Worker struct {
	mu            sync.Mutex
	clock         timing.Clock
	build         func(context.Context, *capture.Payload, bool) (*capture.Payload, error)
	repo          Repository
	notifications Notifications
	logger        *slog.Logger
	cache         string
	schedule      schedule
	current       task
	checking      bool
	latest        *capture.Payload
	targets       []*targetState
	status        Status
	wake          chan struct{}
	sourceDone    chan sourceResult
	targetDone    chan targetResult
	wait          sync.WaitGroup
}

func New(cfg *config.Backup, builder capture.Builder, repo Repository, notifications Notifications, logger *slog.Logger) *Worker {
	w := &Worker{clock: timing.Real{}, build: builder.Build, repo: repo, notifications: notifications, logger: logger, cache: builder.CacheDir, wake: make(chan struct{}, 1), sourceDone: make(chan sourceResult, 1), targetDone: make(chan targetResult, len(cfg.Targets))}
	for _, cfg := range cfg.Targets {
		t := &targetState{config: cfg, status: TargetStatus{Name: cfg.Name, State: "initializing"}}
		if cfg.Error != "" {
			t.initialized = true
			t.status.Error = cfg.Error
			t.status.State = "invalid"
		}
		w.targets = append(w.targets, t)
	}
	w.status.Source = "unknown"
	w.status.Mode = "full"
	return w
}
func (w *Worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *Worker) Request(full bool) { w.mu.Lock(); w.schedule.manual(full); w.mu.Unlock(); w.signal() }
func (w *Worker) Events(events routes.Summary) {
	if events.Empty() {
		return
	}
	w.mu.Lock()
	w.schedule.event(w.clock.Now(), events)
	w.mu.Unlock()
	w.signal()
}
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.status
	s.Checking = w.checking
	s.Configured = len(w.targets) > 0
	s.FullPending = w.schedule.pending.Full || w.schedule.retryFull
	s.NextOrdinary = w.schedule.nextOrdinary
	s.NextFull = w.schedule.nextFull
	s.SourceRetryAt = w.schedule.retryAt
	s.Issues = maps.Clone(s.Issues)
	s.Warnings = append([]string(nil), s.Warnings...)
	s.Targets = make([]TargetStatus, 0, len(w.targets))
	for _, t := range w.targets {
		s.Targets = append(s.Targets, t.status)
	}
	if w.notifications != nil {
		s.Notifiers = w.notifications.Status()
	} else {
		s.Notifiers = []notify.Status{}
	}
	return s
}
func (w *Worker) Run(ctx context.Context) {
	w.mu.Lock()
	w.schedule.start(w.clock.Now())
	for index, t := range w.targets {
		if t.initialized {
			continue
		}
		w.wait.Go(func() {
			initCtx, cancel := context.WithTimeout(ctx, restic.UploadTimeout)
			defer cancel()
			latest, err := w.repo.Latest(initCtx, t.config)
			var baseline *capture.Payload
			if err == nil {
				baseline, _ = loadBaseline(initCtx, w.cache, t.config, latest)
			}
			w.targetDone <- targetResult{index: index, initial: true, baseline: baseline, snapshot: latest, err: err}
		})
	}
	w.mu.Unlock()
	defer w.wait.Wait()
	for {
		w.mu.Lock()
		now := w.clock.Now()
		w.schedule.tick(now)
		if due := w.schedule.sourceDue(now); !w.checking && !due.IsZero() && !due.After(now) {
			w.startCapture(ctx)
		}
		for index, t := range w.targets {
			if !t.initialized || t.config.Error != "" || t.running != nil || w.latest == nil {
				continue
			}
			if w.latest.Same(t.baseline, t.full) {
				t.status.Waiting = false
				t.full = false
				if t.status.Error == "" {
					t.status.State = "unchanged"
				}
				continue
			}
			t.status.Waiting = true
			if t.status.RetryAt.After(now) {
				continue
			}
			t.running = w.latest
			t.runningFull = t.full
			t.full = false
			t.status.Uploading = true
			t.status.State = "uploading"
			payload := t.running
			w.wait.Go(func() {
				id, err := w.repo.Backup(ctx, t.config, payload.Directory, payload.Tag())
				w.targetDone <- targetResult{index: index, id: id, err: err}
			})
		}
		due := earlier(w.schedule.nextOrdinary, w.schedule.nextFull)
		due = earlier(due, w.schedule.retryAt)
		if !w.checking {
			due = earlier(due, w.schedule.sourceDue(now))
		}
		for _, t := range w.targets {
			if t.running == nil && t.status.Waiting {
				due = earlier(due, t.status.RetryAt)
			}
		}
		w.mu.Unlock()
		if due.IsZero() {
			due = now.Add(ordinaryInterval)
		}
		timer := w.clock.NewTimer(max(due.Sub(now), 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.wake:
			timer.Stop()
		case <-timer.C():
		case result := <-w.sourceDone:
			timer.Stop()
			w.mu.Lock()
			w.finishCapture(result)
			w.mu.Unlock()
		case result := <-w.targetDone:
			timer.Stop()
			w.mu.Lock()
			w.finishTarget(result)
			w.mu.Unlock()
		}
	}
}

func (w *Worker) startCapture(ctx context.Context) {
	w.current = w.schedule.take()
	w.status.Mode = "ordinary"
	if w.current.Full {
		w.status.Mode = "full"
	}
	active := false
	for _, t := range w.targets {
		active = active || t.config.Error == ""
	}
	if !active {
		w.status.LastCheck = w.clock.Now()
		w.status.LastResult = "unconfigured"
		w.status.Source = "unconfigured"
		w.report(w.current)
		return
	}
	w.checking = true
	previous := w.latest
	readAll := w.current.ReadAll
	w.wait.Go(func() {
		payload, err := w.build(ctx, previous, readAll)
		w.sourceDone <- sourceResult{payload: payload, err: err}
	})
}
func (w *Worker) finishCapture(result sourceResult) {
	w.checking = false
	now := w.clock.Now()
	w.status.LastCheck = now
	if result.err != nil || result.payload == nil {
		w.status.Source = "failed"
		w.status.LastResult = "capture_failed"
		w.status.Issues = nil
		w.status.Warnings = nil
		w.schedule.complete(now, w.current.Full, false, true)
		w.report(w.current)
		return
	}
	p := result.payload
	w.latest = p
	w.status.Source = p.Tag()
	w.status.Issues = p.Issues
	w.status.Warnings = p.Warnings
	w.status.LastResult = "no_change"
	for _, t := range w.targets {
		t.full = t.full || w.current.Full
		if t.config.Error != "" {
			continue
		}
		t.status.Waiting = !p.Same(t.baseline, t.full)
		if t.status.Waiting {
			w.status.LastResult = "captured"
			if t.running == nil && t.status.Error == "" {
				t.status.State = "waiting"
			}
		} else if t.running == nil && t.status.Error == "" {
			t.status.State = "unchanged"
		}
	}
	w.schedule.complete(now, w.current.Full, p.Reliable, !p.Complete() || !p.Reliable)
	w.report(w.current)
	w.collect()
}
func (w *Worker) finishTarget(result targetResult) {
	t := w.targets[result.index]
	now := w.clock.Now()
	if result.initial {
		t.initialized = true
		t.baseline = result.baseline
		if result.snapshot != nil {
			t.status.SuccessID = result.snapshot.ID
			t.status.SuccessAt = result.snapshot.Time
		}
		if result.err == nil {
			t.status.State = "ready"
			w.collect()
			return
		}
	} else {
		t.status.Uploading = false
		if result.err == nil && !nativeID(result.id) {
			result.err = errors.New("missing snapshot ID")
		}
		if result.err == nil {
			t.baseline = t.running
			t.status.SuccessID = result.id
			t.status.SuccessAt = now
			t.failures = 0
			t.status.Error = ""
			t.status.RetryAt = time.Time{}
			t.status.State = "uploaded"
			if err := saveBaseline(w.cache, t.config, result.id, t.baseline); err != nil {
				w.status.Warnings = append(w.status.Warnings, "baseline_cache_unavailable")
			}
		} else {
			t.full = t.full || t.runningFull
		}
		t.running = nil
	}
	if result.err != nil {
		t.failures++
		t.status.Error = "repository_failed"
		t.status.RetryAt = now.Add(targetBackoff(t.failures))
		t.status.State = "failed"
	}
	t.status.Waiting = w.latest != nil && !w.latest.Same(t.baseline, t.full)
	w.report(task{Reasons: map[string]uint64{"target_result": 1}})
	w.collect()
}
func (w *Worker) report(task task) {
	if w.notifications == nil {
		return
	}
	event := notify.Event{
		Count: 1, Reasons: maps.Clone(task.Reasons), Source: w.status.Source,
		Issues: maps.Clone(w.status.Issues), Warnings: append([]string(nil), w.status.Warnings...),
		Routes: maps.Clone(task.Events.Routes), Unknown: maps.Clone(task.Events.Unknown),
	}
	if task.Reasons["target_result"] == 0 {
		event.Checks = 1
	}
	for _, t := range w.targets {
		event.Targets = append(event.Targets, notify.TargetStatus{
			Name: t.config.Name, State: t.status.State, Waiting: t.status.Waiting,
			Uploading: t.status.Uploading, Failed: t.status.Error != "",
		})
	}
	w.notifications.Enqueue(event)
}
func (w *Worker) collect() {
	keep := map[string]bool{}
	if w.latest != nil {
		keep[w.latest.Directory] = true
	}
	for _, t := range w.targets {
		if !t.initialized {
			// A slow startup query must not retain every intervening capture.
			// Protect only the locally associated native baseline while that
			// target independently verifies its remote latest ID.
			directory := filepath.Join(w.cache, "baselines", restic.CacheKey(t.config))
			entries, _ := os.ReadDir(directory)
			for _, entry := range entries {
				if !nativeID(entry.Name()) {
					continue
				}
				link, err := os.Readlink(filepath.Join(directory, entry.Name()))
				name := filepath.Base(link)
				if err == nil && payloadName(name) && link == filepath.Join("..", "..", "payloads", name) {
					keep[filepath.Join(w.cache, "payloads", name)] = true
				}
			}
		}
		if t.baseline != nil {
			keep[t.baseline.Directory] = true
		}
		if t.running != nil {
			keep[t.running.Directory] = true
		}
	}
	if w.checking {
		return
	} // The current builder owns its directory until delivery.
	root := filepath.Join(w.cache, "payloads")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if payloadName(entry.Name()) && entry.IsDir() && !keep[path] {
			if err := os.RemoveAll(path); err != nil && w.logger != nil {
				w.logger.Warn("unused payload cleanup failed")
			}
		}
	}
}
