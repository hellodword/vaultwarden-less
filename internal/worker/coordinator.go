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
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type Repository interface {
	Inspect(context.Context, config.Target) (restic.Metadata, error)
	Backup(context.Context, config.Target, string, string, string) (string, error)
	Check(context.Context, config.Target, bool) error
}
type Notifications interface {
	Enqueue(notify.Event) error
	Status() []notify.Status
}
type VerificationStatus struct {
	Scheduled   bool      `json:"scheduled"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	LastSuccess time.Time `json:"last_success,omitzero"`
	Result      string    `json:"result"`
	Error       string    `json:"error,omitempty"`
	ExitCode    int       `json:"exit_code"`
	Next        time.Time `json:"next,omitzero"`
	RetryAt     time.Time `json:"retry_at,omitzero"`
}
type TargetStatus struct {
	Name         string             `json:"name"`
	RepositoryID string             `json:"repository_id,omitempty"`
	Fingerprint  string             `json:"fingerprint,omitempty"`
	Covered      bool               `json:"covered"`
	SuccessID    string             `json:"success_id,omitempty"`
	SuccessAt    time.Time          `json:"success_at,omitzero"`
	CompleteID   string             `json:"complete_id,omitempty"`
	CompleteAt   time.Time          `json:"complete_at,omitzero"`
	Waiting      bool               `json:"waiting"`
	Uploading    bool               `json:"uploading"`
	Operation    string             `json:"operation,omitempty"`
	RetryAt      time.Time          `json:"retry_at,omitzero"`
	Error        string             `json:"error,omitempty"`
	ExitCode     int                `json:"exit_code"`
	State        string             `json:"state"`
	Failures     int                `json:"failures"`
	Structure    VerificationStatus `json:"structure"`
	ReadData     VerificationStatus `json:"read_data"`
	Snapshots    uint64             `json:"snapshots"`
}
type Status struct {
	Configured    bool             `json:"configured"`
	Checking      bool             `json:"checking"`
	LastCheck     time.Time        `json:"last_check,omitzero"`
	LastResult    string           `json:"last_result"`
	LastChange    time.Time        `json:"last_change,omitzero"`
	Fingerprint   string           `json:"fingerprint,omitempty"`
	Checks        uint64           `json:"checks"`
	Changes       uint64           `json:"changes"`
	NextOrdinary  time.Time        `json:"next_ordinary,omitzero"`
	SourceRetryAt time.Time        `json:"source_retry_at,omitzero"`
	Source        string           `json:"source"`
	Issues        map[string]int   `json:"issues"`
	Warnings      []string         `json:"warnings"`
	Targets       []TargetStatus   `json:"targets"`
	Notifiers     []notify.Status  `json:"notifiers"`
	Operations    []Operation      `json:"operations"`
	Faults        map[string]Fault `json:"faults"`
	StateError    string           `json:"state_error,omitempty"`
}
type targetState struct {
	config        config.Target
	status        TargetStatus
	initialized   bool
	running       *capture.Payload
	runningJobs   []string
	demands       []string
	verifyQueue   []string
	verifying     *verification
	verifyBarrier bool
	retryCapture  bool
	refreshAfter  uint64
}
type verification struct {
	ReadData, Retry, Manual bool
	Operation               string
}
type sourceResult struct {
	payload *capture.Payload
	err     error
}
type targetResult struct {
	index       int
	kind        string
	metadata    restic.Metadata
	id          string
	err         error
	metadataErr error
}
type Worker struct {
	mu                 sync.Mutex
	clock              timing.Clock
	build              func(context.Context, *capture.Payload, bool) (*capture.Payload, error)
	repo               Repository
	notifications      Notifications
	logger             *slog.Logger
	cache              string
	store              state.Store
	schedule           schedule
	current            task
	captureSeq         uint64
	checking           bool
	latest             *capture.Payload
	targets            []*targetState
	status             Status
	operations         map[string]*Operation
	order              []string
	reportState        reportState
	notificationConfig config.Notifications
	wake               chan struct{}
	sourceDone         chan sourceResult
	targetDone         chan targetResult
	wait               sync.WaitGroup
}

func New(cfg *config.Backup, builder capture.Builder, repo Repository, notifications Notifications, logger *slog.Logger) *Worker {
	w := &Worker{clock: timing.Real{}, build: builder.Build, repo: repo, notifications: notifications, logger: logger, cache: builder.CacheDir, operations: map[string]*Operation{}, notificationConfig: cfg.Notifications, wake: make(chan struct{}, 1), sourceDone: make(chan sourceResult, 1), targetDone: make(chan targetResult, len(cfg.Targets)+1)}
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
	w.reportState.Faults = map[string]Fault{}
	return w
}
func (w *Worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Request is the internal signal-only entry used by scheduled and gateway work.
func (w *Worker) Request() { w.mu.Lock(); w.schedule.manual(); w.mu.Unlock(); w.signal() }
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
	s.NextOrdinary = w.schedule.nextOrdinary
	s.SourceRetryAt = w.schedule.retryAt
	s.Issues = maps.Clone(s.Issues)
	s.Warnings = append([]string(nil), s.Warnings...)
	s.Targets = make([]TargetStatus, 0, len(w.targets))
	for _, t := range w.targets {
		status := t.status
		status.Covered = w.latest != nil && status.Fingerprint == w.latest.Fingerprint() && status.SuccessID != ""
		s.Targets = append(s.Targets, status)
	}
	s.Notifiers = []notify.Status{}
	if w.notifications != nil {
		s.Notifiers = w.notifications.Status()
	}
	s.Operations = []Operation{}
	for _, id := range w.order {
		s.Operations = append(s.Operations, w.operations[id].clone())
	}
	s.Faults = maps.Clone(w.reportState.Faults)
	return s
}
func (w *Worker) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); w.wait.Wait() }()
	w.mu.Lock()
	w.schedule.start(w.clock.Now())
	w.mu.Unlock()
	for {
		w.mu.Lock()
		now := w.clock.Now()
		w.schedule.tick(now)
		// A retry first refreshes the source. New writes never change its deadline.
		for _, t := range w.targets {
			if t.config.Error != "" || t.status.Operation != "" {
				continue
			}
			if !t.status.RetryAt.IsZero() && !t.status.RetryAt.After(now) && !t.retryCapture {
				t.retryCapture = true
				t.refreshAfter = w.captureSeq + 1
				w.schedule.urgent = true
				w.schedule.reason("target_retry")
			}
		}
		if due := w.schedule.sourceDue(now); !w.checking && !due.IsZero() && !due.After(now) {
			w.startCapture(ctx)
		}
		for index, t := range w.targets {
			if t.config.Error != "" || t.status.Operation != "" {
				continue
			}
			if !t.initialized {
				if t.status.RetryAt.After(now) && t.status.State != "initializing" && !w.forced(t) && len(t.verifyQueue) == 0 {
					w.deferDemands(t)
					continue
				}
				w.startInspect(ctx, index, t)
				continue
			}
			w.driveTarget(ctx, index, t, now)
		}
		if err := w.report(now); err != nil {
			w.mu.Unlock()
			return err
		}
		if err := w.persist(); err != nil {
			w.status.StateError = "state_write_failed"
			w.mu.Unlock()
			return err
		}
		due := earlier(w.schedule.nextOrdinary, w.schedule.retryAt)
		if !w.checking {
			due = earlier(due, w.schedule.sourceDue(now))
		}
		for _, t := range w.targets {
			if t.config.Error != "" || t.status.Operation != "" {
				continue
			}
			if t.status.RetryAt.After(now) {
				due = earlier(due, t.status.RetryAt)
			}
			if t.status.SuccessID != "" {
				for _, v := range []VerificationStatus{t.status.Structure, t.status.ReadData} {
					if v.Scheduled && v.Next.After(now) {
						due = earlier(due, v.Next)
					}
					if v.RetryAt.After(now) {
						due = earlier(due, v.RetryAt)
					}
				}
			}
		}
		due = earlier(due, w.reportDue(now))
		w.mu.Unlock()
		if due.IsZero() {
			due = now.Add(ordinaryInterval)
		}
		timer := w.clock.NewTimer(max(due.Sub(now), 0))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
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
func (w *Worker) startInspect(ctx context.Context, index int, t *targetState) {
	t.status.Operation = "initializing"
	w.wait.Go(func() {
		c, cancel := context.WithTimeout(ctx, restic.UploadTimeout)
		defer cancel()
		metadata, err := w.repo.Inspect(c, t.config)
		w.targetDone <- targetResult{index: index, kind: "inspect", metadata: metadata, err: err}
	})
}
func (w *Worker) startCapture(ctx context.Context) {
	w.current = w.schedule.take()
	w.captureSeq++
	w.current.Generation = w.captureSeq
	active := false
	for _, t := range w.targets {
		active = active || t.config.Error == ""
	}
	if !active {
		w.status.LastResult = "unconfigured"
		w.status.Source = "unconfigured"
		w.status.LastCheck = w.clock.Now()
		for _, id := range w.current.Operations {
			w.failOperation(id, "unconfigured")
		}
		return
	}
	w.checking = true
	for _, id := range w.current.Operations {
		op := w.operations[id]
		op.State = "running"
		op.Started = w.clock.Now()
	}
	w.wait.Go(func() { p, err := w.build(ctx, nil, true); w.sourceDone <- sourceResult{p, err} })
}
func (w *Worker) finishCapture(result sourceResult) {
	w.checking = false
	now := w.clock.Now()
	w.status.LastCheck = now
	w.status.Checks++
	if result.err != nil || result.payload == nil {
		w.status.Source = "failed"
		w.status.LastResult = "capture_failed"
		w.status.Issues = nil
		w.status.Warnings = nil
		w.schedule.complete(now, true)
		for _, t := range w.targets {
			if t.retryCapture {
				t.retryCapture = false
				t.status.RetryAt = now.Add(sourceRetry)
			}
		}
		for _, id := range w.current.Operations {
			w.failOperation(id, "capture_failed")
		}
		return
	}
	p := result.payload
	fingerprint := p.Fingerprint()
	w.latest = p
	w.status.Source = p.Tag()
	w.status.Issues = p.Issues
	w.status.Warnings = p.Warnings
	w.status.LastResult = "no_change"
	if fingerprint != w.status.Fingerprint {
		w.status.LastResult = "captured"
		w.status.LastChange = now
		w.status.Changes++
		w.status.Fingerprint = fingerprint
	}
	for _, t := range w.targets {
		if t.refreshAfter > 0 && w.current.Generation >= t.refreshAfter {
			t.retryCapture = false
			t.refreshAfter = 0
			t.status.RetryAt = time.Time{}
		}
		t.status.Waiting = fingerprint != t.status.Fingerprint || t.status.Error != ""
		for _, id := range w.current.Operations {
			op := w.operations[id]
			op.Source = p.Tag()
			if t.config.Error != "" {
				w.targetOperation(id, t, OperationTarget{State: "failed", Error: t.config.Error})
				continue
			}
			t.demands = append(t.demands, id)
		}
	}
	w.schedule.complete(now, !p.Complete() || !p.Reliable)
	w.collect()
}
func (w *Worker) driveTarget(ctx context.Context, index int, t *targetState, now time.Time) {
	// Once due, verification follows one due upload even under continuous writes.
	v := w.dueVerification(t, now)
	if t.refreshAfter > 0 {
		if v != nil {
			w.startVerification(ctx, index, t, *v)
		}
		return
	}
	if v != nil && t.verifyBarrier && w.startVerification(ctx, index, t, *v) {
		return
	}
	force := false
	for _, id := range t.demands {
		force = force || w.operations[id].Force
	}
	need := w.latest != nil && (t.status.Fingerprint != w.latest.Fingerprint() || t.status.Error != "" || force)
	t.status.Waiting = need
	if need && !t.status.RetryAt.After(now) && !t.retryCapture {
		if v != nil {
			t.verifyBarrier = true
		}
		w.startUpload(ctx, index, t)
		return
	}
	if need && force {
		if v != nil {
			t.verifyBarrier = true
		}
		w.startUpload(ctx, index, t)
		return
	}
	if need && t.status.RetryAt.After(now) {
		w.deferDemands(t)
	}
	if !need && w.latest != nil {
		for _, id := range t.demands {
			w.targetOperation(id, t, OperationTarget{State: "unchanged", SnapshotID: t.status.SuccessID})
		}
		t.demands = nil
		t.status.State = "unchanged"
	}
	if v != nil {
		w.startVerification(ctx, index, t, *v)
	}
}
func (w *Worker) startUpload(ctx context.Context, index int, t *targetState) {
	t.running = w.latest
	t.runningJobs = t.demands
	t.demands = nil
	t.status.Operation = "upload"
	t.status.Uploading = true
	t.status.State = "uploading"
	p := t.running
	w.wait.Go(func() {
		id, err := w.repo.Backup(ctx, t.config, p.Directory, p.Tag(), p.Fingerprint())
		w.targetDone <- targetResult{index: index, kind: "upload", id: id, err: err}
	})
}
func safeError(err error) (string, int) {
	var operation *restic.OperationError
	if errors.As(err, &operation) {
		return operation.Code, operation.ExitCode
	}
	return "repository_failed", -1
}
func (w *Worker) failTarget(t *targetState, err error) {
	t.status.Failures++
	t.status.Error, t.status.ExitCode = safeError(err)
	t.status.RetryAt = w.clock.Now().Add(targetBackoff(t.status.Failures))
	t.status.State = "failed"
}
func (w *Worker) finishTarget(result targetResult) {
	t := w.targets[result.index]
	now := w.clock.Now()
	t.status.Operation = ""
	switch result.kind {
	case "inspect":
		if result.err != nil {
			w.failTarget(t, result.err)
			for _, id := range t.demands {
				if w.operations[id].Force {
					w.targetOperation(id, t, OperationTarget{State: "failed", Error: t.status.Error, ExitCode: t.status.ExitCode, RetryAt: t.status.RetryAt})
				}
			}
			w.deferDemands(t)
			t.demands = nil
			for _, id := range t.verifyQueue {
				w.failOperation(id, t.status.Error)
			}
			t.verifyQueue = nil
			return
		}
		w.bindRepository(t, result.metadata, now)
		t.initialized = true
		t.status.State = "ready"
	case "upload":
		t.status.Uploading = false
		if result.err == nil && !nativeID(result.id) {
			result.err = &restic.OperationError{Code: "snapshot_id_missing", ExitCode: -1}
		}
		if result.err == nil {
			t.status.Fingerprint = t.running.Fingerprint()
			t.status.SuccessID = result.id
			t.status.SuccessAt = now
			t.status.Snapshots++
			if t.running.Complete() {
				t.status.CompleteID = result.id
				t.status.CompleteAt = now
			}
			t.status.Failures = 0
			t.status.Error = ""
			t.status.ExitCode = 0
			t.status.RetryAt = time.Time{}
			t.status.State = "uploaded"
			w.enableVerification(t, now)
		} else {
			w.failTarget(t, result.err)
		}
		for _, id := range t.runningJobs {
			outcome := OperationTarget{State: "uploaded", SnapshotID: result.id}
			if result.err != nil {
				outcome = OperationTarget{State: "failed", Error: t.status.Error, ExitCode: t.status.ExitCode, RetryAt: t.status.RetryAt}
			}
			w.targetOperation(id, t, outcome)
		}
		t.running = nil
		t.runningJobs = nil
	case "verify":
		w.finishVerification(t, result, now)
	}
	t.status.Waiting = w.latest != nil && (t.status.Fingerprint != w.latest.Fingerprint() || t.status.Error != "")
	w.collect()
}
func (w *Worker) collect() {
	if w.checking {
		return
	}
	keep := map[string]bool{}
	if w.latest != nil {
		keep[w.latest.Directory] = true
	}
	for _, t := range w.targets {
		if t.running != nil {
			keep[t.running.Directory] = true
		}
	}
	root := filepath.Join(w.cache, "payloads")
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() && payloadName(entry.Name()) && !keep[path] {
			if err := os.RemoveAll(path); err != nil && w.logger != nil {
				w.logger.Warn("payload_cleanup_failed")
			}
		}
	}
}

func (w *Worker) forced(t *targetState) bool {
	for _, id := range t.demands {
		if w.operations[id].Force {
			return true
		}
	}
	return false
}
