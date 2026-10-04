package worker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

type LastCheck struct {
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
	Result     string  `json:"result"`
	ContentID  *string `json:"content_id"`
	ArchiveID  *string `json:"archive_id"`
	ErrorCode  *string `json:"error_code"`
}

type TargetStatus struct {
	Name          string  `json:"name"`
	State         string  `json:"state"`
	LastAttemptAt *string `json:"last_attempt_at"`
	LastSuccessAt *string `json:"last_success_at"`
	SnapshotID    *string `json:"snapshot_id"`
	ContentID     *string `json:"content_id"`
	ArchiveID     *string `json:"archive_id"`
	ErrorCode     *string `json:"error_code"`
}

type Status struct {
	State       string         `json:"state"`
	StartedAt   string         `json:"started_at"`
	NextCheckAt *string        `json:"next_check_at"`
	LastCheck   *LastCheck     `json:"last_check"`
	Targets     []TargetStatus `json:"targets"`
}

type lastCheckState struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Result     string
	ContentID  *string
	ArchiveID  *string
	ErrorCode  *string
}

type targetStatusState struct {
	Name          string
	State         string
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	SnapshotID    *string
	ContentID     *string
	ArchiveID     *string
	ErrorCode     *string
}

type statusState struct {
	State       string
	StartedAt   time.Time
	NextCheckAt *time.Time
	LastCheck   *lastCheckState
	Targets     []targetStatusState
}

type failureState struct {
	since    time.Time
	degraded bool
}

type Worker struct {
	targets []config.Target
	build   func(context.Context) (snapshot.Manifest, error)
	ensure  func(context.Context, config.Target, snapshot.Manifest) (restic.SyncResult, error)
	enqueue func(notify.Event)
	logger  *slog.Logger
	clock   clock
	demand  chan struct{}

	mu             sync.RWMutex
	status         statusState
	sourceFailure  failureState
	targetFailures []failureState
}

func New(
	cfg *config.Backup,
	build func(context.Context) (snapshot.Manifest, error),
	ensure func(context.Context, config.Target, snapshot.Manifest) (restic.SyncResult, error),
	enqueue func(notify.Event),
	logger *slog.Logger,
) *Worker {
	return newWithClock(cfg, build, ensure, enqueue, logger, realClock{})
}

func newWithClock(
	cfg *config.Backup,
	build func(context.Context) (snapshot.Manifest, error),
	ensure func(context.Context, config.Target, snapshot.Manifest) (restic.SyncResult, error),
	enqueue func(notify.Event),
	logger *slog.Logger,
	currentClock clock,
) *Worker {
	if enqueue == nil {
		enqueue = func(notify.Event) {}
	}
	started := currentClock.Now().UTC()
	targets := copyTargets(cfg)
	targetStatuses := make([]targetStatusState, 0, len(targets))
	for _, target := range targets {
		targetStatuses = append(targetStatuses, targetStatusState{Name: target.Name, State: "unknown"})
	}
	return &Worker{
		targets: targets, build: build, ensure: ensure, enqueue: enqueue, logger: logger, clock: currentClock,
		demand: make(chan struct{}, 1),
		status: statusState{
			State:     "idle",
			StartedAt: started,
			Targets:   targetStatuses,
		},
		targetFailures: make([]failureState, len(targets)),
	}
}

func copyTargets(cfg *config.Backup) []config.Target {
	if cfg == nil || len(cfg.Targets) == 0 {
		return []config.Target{}
	}
	targets := make([]config.Target, len(cfg.Targets))
	for index, target := range cfg.Targets {
		target.Options = slices.Clone(target.Options)
		targets[index] = target
	}
	return targets
}

// Request coalesces all outstanding demand into one in-memory signal.
func (worker *Worker) Request() {
	if worker.targetless() {
		return
	}
	select {
	case worker.demand <- struct{}{}:
	default:
	}
}

func (worker *Worker) targetless() bool {
	return len(worker.targets) == 0
}

func (worker *Worker) Status() Status {
	worker.mu.RLock()
	defer worker.mu.RUnlock()
	result := Status{
		State:     worker.status.State,
		StartedAt: formatTime(worker.status.StartedAt),
		Targets:   make([]TargetStatus, len(worker.status.Targets)),
	}
	if worker.status.NextCheckAt != nil {
		value := formatTime(*worker.status.NextCheckAt)
		result.NextCheckAt = &value
	}
	if worker.status.LastCheck != nil {
		state := worker.status.LastCheck
		last := &LastCheck{
			StartedAt:  formatTime(state.StartedAt),
			FinishedAt: formatTime(state.FinishedAt),
			Result:     state.Result,
			ContentID:  cloneString(state.ContentID),
			ArchiveID:  cloneString(state.ArchiveID),
			ErrorCode:  cloneString(state.ErrorCode),
		}
		result.LastCheck = last
	}
	for index, state := range worker.status.Targets {
		target := TargetStatus{
			Name:       state.Name,
			State:      state.State,
			SnapshotID: cloneString(state.SnapshotID),
			ContentID:  cloneString(state.ContentID),
			ArchiveID:  cloneString(state.ArchiveID),
			ErrorCode:  cloneString(state.ErrorCode),
		}
		if state.LastAttemptAt != nil {
			value := formatTime(*state.LastAttemptAt)
			target.LastAttemptAt = &value
		}
		if state.LastSuccessAt != nil {
			value := formatTime(*state.LastSuccessAt)
			target.LastSuccessAt = &value
		}
		result.Targets[index] = target
	}
	return result
}

func (worker *Worker) Run(ctx context.Context) {
	if worker.targetless() {
		<-ctx.Done()
		return
	}
	var scheduling schedule
	scheduling.demand(worker.clock.Now())
	for {
		due, kind, exists := scheduling.next()
		if !exists {
			worker.setWaiting("idle", nil)
			select {
			case <-ctx.Done():
				return
			case <-worker.demand:
				scheduling.demand(worker.clock.Now())
				continue
			}
		}
		now := worker.clock.Now()
		if due.After(now) {
			state := "idle"
			if kind == triggerDemand {
				state = "debouncing"
			}
			worker.setWaiting(state, &due)
			timer := worker.clock.NewTimer(due.Sub(now))
			select {
			case <-ctx.Done():
				timer.Stop()
				worker.setWaiting("idle", nil)
				return
			case <-worker.demand:
				timer.Stop()
				scheduling.demand(worker.clock.Now())
				continue
			case <-timer.C():
			}
		}

		started := worker.clock.Now()
		scheduling.begin(started)
		worker.setChecking()
		outcome := worker.check(ctx)
		if outcome.cancelled || ctx.Err() != nil {
			worker.setWaiting("idle", nil)
			return
		}
		finished := worker.clock.Now()
		worker.finishCheck(started, finished, outcome)
		scheduling.complete(finished, outcome.retry)
		select {
		case <-worker.demand:
			scheduling.demand(worker.clock.Now())
		default:
		}
	}
}

type checkOutcome struct {
	result    string
	contentID string
	archiveID string
	errorCode string
	retry     bool
	cancelled bool
}

const targetConcurrency = 2

type targetAttempt struct {
	result     restic.SyncResult
	err        error
	finishedAt time.Time
}

func (worker *Worker) check(ctx context.Context) checkOutcome {
	var built snapshot.Manifest
	for {
		result, err := worker.build(ctx)
		if err == nil {
			built = result
			worker.sourceSucceeded()
			break
		}
		if ctx.Err() != nil {
			return checkOutcome{cancelled: true}
		}
		var consistency *snapshot.ConsistencyError
		if !errors.As(err, &consistency) {
			now := worker.clock.Now()
			worker.sourceFailed(now, "source_check_failed")
			worker.logFailure("source check failed", "source_check_failed", "")
			return checkOutcome{result: "failed", errorCode: "source_check_failed", retry: true}
		}
		now := worker.clock.Now()
		worker.sourceFailed(now, "source_inconsistent")
		worker.logFailure("source recovery point is not yet consistent", "source_inconsistent", "")
		timer := worker.clock.NewTimer(consistencyRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return checkOutcome{cancelled: true}
		case <-timer.C():
		}
	}

	attempts, completed := worker.synchronizeTargets(ctx, built)
	if !completed {
		return checkOutcome{cancelled: true}
	}
	targetNotifications := make([]notify.TargetStatus, len(worker.targets))
	created := 0
	succeeded := 0
	failed := 0
	for index, target := range worker.targets {
		attempt := attempts[index]
		result, err := attempt.result, attempt.err
		if err == nil && !validSyncResult(result, built) {
			err = fmt.Errorf("invalid target sync result")
		}
		if err != nil {
			code := targetErrorCode(err)
			worker.targetFailed(index, attempt.finishedAt, code)
			targetNotifications[index] = notify.TargetStatus{Name: target.Name, State: "failed", ErrorCode: code}
			worker.logFailure("target backup failed", code, target.Name)
			failed++
			continue
		}
		worker.targetSucceeded(index, attempt.finishedAt, result)
		state := "synced"
		if result.Created {
			state = "created"
			created++
		}
		targetNotifications[index] = notify.TargetStatus{Name: target.Name, State: state}
		succeeded++
	}

	if created > 0 {
		worker.enqueue(notify.RecoveryPointCreated(built.ContentID, targetNotifications))
	}
	if failed == 0 {
		result := "unchanged"
		if created > 0 {
			result = "created"
		}
		return checkOutcome{result: result, contentID: built.ContentID, archiveID: built.ArchiveID}
	}
	if succeeded > 0 {
		return checkOutcome{result: "partial", contentID: built.ContentID, archiveID: built.ArchiveID, errorCode: "target_failed", retry: true}
	}
	return checkOutcome{result: "failed", contentID: built.ContentID, archiveID: built.ArchiveID, errorCode: "all_targets_failed", retry: true}
}

func (worker *Worker) synchronizeTargets(ctx context.Context, built snapshot.Manifest) ([]targetAttempt, bool) {
	attempts := make([]targetAttempt, len(worker.targets))
	semaphore := make(chan struct{}, targetConcurrency)
	var wait sync.WaitGroup
	for index, target := range worker.targets {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wait.Wait()
			return nil, false
		}
		worker.recordTargetAttempt(index, worker.clock.Now())
		wait.Go(func() {
			defer func() { <-semaphore }()
			targetContext, cancel := context.WithTimeout(ctx, targetTimeout)
			result, err := worker.ensure(targetContext, target, built)
			cancel()
			attempts[index] = targetAttempt{result: result, err: err, finishedAt: worker.clock.Now()}
		})
	}
	wait.Wait()
	return attempts, ctx.Err() == nil
}

func (worker *Worker) sourceFailed(now time.Time, code string) {
	if worker.sourceFailure.since.IsZero() {
		worker.sourceFailure.since = now
	}
	if !worker.sourceFailure.degraded && !now.Before(worker.sourceFailure.since.Add(degradedAfter)) {
		worker.sourceFailure.degraded = true
		worker.enqueue(notify.SourceDegraded(code))
	}
}

func (worker *Worker) sourceSucceeded() {
	if worker.sourceFailure.degraded {
		worker.enqueue(notify.SourceRecovered())
	}
	worker.sourceFailure = failureState{}
}

func (worker *Worker) targetFailed(index int, now time.Time, code string) {
	failure := &worker.targetFailures[index]
	if failure.since.IsZero() {
		failure.since = now
	}
	state := "retrying"
	if !failure.degraded && !now.Before(failure.since.Add(degradedAfter)) {
		failure.degraded = true
		worker.enqueue(notify.TargetDegraded(worker.targets[index].Name, code))
	}
	if failure.degraded {
		state = "degraded"
	}
	worker.mu.Lock()
	worker.status.Targets[index].State = state
	worker.status.Targets[index].ErrorCode = new(code)
	worker.mu.Unlock()
}

func (worker *Worker) targetSucceeded(index int, now time.Time, result restic.SyncResult) {
	failure := worker.targetFailures[index]
	if failure.degraded {
		worker.enqueue(notify.TargetRecovered(worker.targets[index].Name))
	}
	worker.targetFailures[index] = failureState{}
	when := now.UTC()
	worker.mu.Lock()
	target := &worker.status.Targets[index]
	target.State = "healthy"
	target.LastSuccessAt = &when
	target.SnapshotID = new(result.SnapshotID)
	target.ContentID = new(result.ContentID)
	if result.ArchiveID == "" {
		target.ArchiveID = nil
	} else {
		target.ArchiveID = new(result.ArchiveID)
	}
	target.ErrorCode = nil
	worker.mu.Unlock()
}

func (worker *Worker) recordTargetAttempt(index int, now time.Time) {
	when := now.UTC()
	worker.mu.Lock()
	worker.status.Targets[index].LastAttemptAt = &when
	worker.mu.Unlock()
}

func (worker *Worker) setWaiting(state string, next *time.Time) {
	worker.mu.Lock()
	if worker.status.State == "partial" || worker.status.State == "degraded" {
		state = worker.status.State
	}
	worker.status.State = state
	if next == nil {
		worker.status.NextCheckAt = nil
	} else {
		value := next.UTC()
		worker.status.NextCheckAt = &value
	}
	worker.mu.Unlock()
}

func (worker *Worker) setChecking() {
	worker.mu.Lock()
	worker.status.State = "checking"
	worker.status.NextCheckAt = nil
	worker.mu.Unlock()
}

func (worker *Worker) finishCheck(started, finished time.Time, outcome checkOutcome) {
	last := &lastCheckState{
		StartedAt:  started.UTC(),
		FinishedAt: finished.UTC(),
		Result:     outcome.result,
	}
	if outcome.contentID != "" {
		last.ContentID = new(outcome.contentID)
	}
	if outcome.archiveID != "" {
		last.ArchiveID = new(outcome.archiveID)
	}
	if outcome.errorCode != "" {
		last.ErrorCode = new(outcome.errorCode)
	}
	worker.mu.Lock()
	worker.status.State = "idle"
	if outcome.result == "partial" {
		worker.status.State = "partial"
	}
	if worker.sourceFailure.degraded {
		worker.status.State = "degraded"
	}
	for _, failure := range worker.targetFailures {
		if failure.degraded {
			worker.status.State = "degraded"
			break
		}
	}
	worker.status.NextCheckAt = nil
	worker.status.LastCheck = last
	worker.mu.Unlock()
}

func (worker *Worker) logFailure(message, code, target string) {
	if worker.logger == nil {
		return
	}
	if target == "" {
		worker.logger.Error(message, "error_code", code)
		return
	}
	worker.logger.Error(message, "target", target, "error_code", code)
}

func targetErrorCode(err error) string {
	var operation *restic.OperationError
	if errors.As(err, &operation) && validErrorCode(operation.Code) {
		return operation.Code
	}
	return "target_backup_failed"
}

func validSyncResult(result restic.SyncResult, manifest snapshot.Manifest) bool {
	if !validHexID(result.SnapshotID) || result.ContentID != manifest.ContentID || !snapshot.IsContentID(result.ArchiveID) {
		return false
	}
	return !result.Created || result.ArchiveID == manifest.ArchiveID
}

func validHexID(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validErrorCode(value string) bool {
	if value == "" || len(value) > 64 || strings.HasPrefix(value, "_") || strings.HasSuffix(value, "_") {
		return false
	}
	for _, character := range value {
		if character != '_' && (character < 'a' || character > 'z') {
			return false
		}
	}
	return true
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
