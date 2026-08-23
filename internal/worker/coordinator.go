package worker

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/safefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/state"
)

type Coordinator struct {
	cfg       *config.Backup
	store     *state.Store
	builder   snapshot.Builder
	restic    backupRunner
	notifiers *notify.Manager
	log       *slog.Logger
	wake      chan struct{}
	dirtyMu   sync.Mutex
}

type backupRunner interface {
	Backup(context.Context, config.Target, snapshot.Manifest) (restic.BackupResult, error)
}

func NewCoordinator(cfg *config.Backup, store *state.Store, resticRunner backupRunner, notifiers *notify.Manager, log *slog.Logger) *Coordinator {
	return &Coordinator{
		cfg: cfg, store: store, builder: snapshot.Builder{DataDir: cfg.DataDir, StagingDir: cfg.StagingDir},
		restic: resticRunner, notifiers: notifiers, log: log, wake: make(chan struct{}, 1),
	}
}

func (c *Coordinator) MarkDirty(now time.Time) error {
	c.dirtyMu.Lock()
	defer c.dirtyMu.Unlock()
	current := c.store.Snapshot()
	if current.Dirty {
		if current.BackupInProgress && current.DirtyGeneration != current.RunningGeneration {
			c.signal()
			return nil
		}
		if !current.BackupInProgress {
			err := c.store.Update(func(next *state.State) error {
				next.LastDirtyAt = state.FormatTime(now)
				return nil
			})
			if err == nil {
				c.signal()
			}
			return err
		}
	}
	err := c.store.Update(func(next *state.State) error {
		next.DirtyGeneration++
		next.Dirty = true
		next.LastDirtyAt = state.FormatTime(now)
		if next.FirstDirtyAt == "" {
			next.FirstDirtyAt = state.FormatTime(now)
		}
		return nil
	})
	if err == nil {
		c.signal()
	}
	return err
}

func (c *Coordinator) Run(ctx context.Context) error {
	if err := c.Reconcile(ctx); err != nil {
		return err
	}
	if err := c.MarkDirty(time.Now()); err != nil {
		return err
	}
	if err := c.sample(ctx, true); err != nil && ctx.Err() == nil {
		c.log.Error("startup recovery-point sampling failed", "error_code", safeCoordinatorCode(err))
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		now := time.Now()
		current := c.store.Snapshot()
		snapshotAt := nextSnapshotAt(current, c.cfg, now)
		retryAt := nextTargetRetryAt(current, now)
		if !snapshotAt.After(now) {
			if err := c.sample(ctx, true); err != nil && ctx.Err() == nil {
				c.log.Error("recovery-point sampling failed", "error_code", safeCoordinatorCode(err))
			}
			continue
		}
		if !retryAt.IsZero() && !retryAt.After(now) {
			if err := c.catchUp(ctx, true); err != nil && ctx.Err() == nil {
				c.log.Error("target catch-up failed", "error_code", safeCoordinatorCode(err))
			}
			continue
		}
		next := snapshotAt
		if !retryAt.IsZero() && retryAt.Before(next) {
			next = retryAt
		}
		delay := time.Until(next)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-c.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *Coordinator) Sample(ctx context.Context) error {
	return c.sample(ctx, false)
}

func (c *Coordinator) sample(ctx context.Context, preemptibleUploads bool) (resultErr error) {
	current := c.store.Snapshot()
	runningGeneration := current.DirtyGeneration
	if err := c.beginWork(runningGeneration); err != nil {
		return err
	}
	defer func() {
		if err := c.endWork(); err != nil && resultErr == nil {
			resultErr = err
		}
	}()

	var result snapshot.Result
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		result, err = c.builder.Build(ctx, attempt == 3)
		if err == nil {
			break
		}
		var missing *snapshot.MissingFilesError
		if !errors.As(err, &missing) || attempt == 3 {
			break
		}
		timer := time.NewTimer(c.cfg.MissingRetry.Duration)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
			attempt = 3
		case <-timer.C:
		}
	}
	if err != nil {
		_ = c.store.Update(func(next *state.State) error {
			now := state.FormatTime(time.Now())
			next.LastSampleAt = now
			next.FirstDirtyAt = now
			next.LastDirtyAt = now
			if !next.Dirty {
				next.Dirty = true
				next.DirtyGeneration++
			}
			return nil
		})
		return err
	}
	if err := c.promote(ctx, result, runningGeneration); err != nil {
		_ = os.RemoveAll(result.Path)
		return err
	}
	return c.uploadCurrent(ctx, preemptibleUploads)
}

func (c *Coordinator) CatchUp(ctx context.Context) error {
	return c.catchUp(ctx, false)
}

func (c *Coordinator) catchUp(ctx context.Context, preemptible bool) (resultErr error) {
	if err := c.beginWork(c.store.Snapshot().DirtyGeneration); err != nil {
		return err
	}
	defer func() {
		if err := c.endWork(); err != nil && resultErr == nil {
			resultErr = err
		}
	}()
	return c.uploadCurrent(ctx, preemptible)
}

func (c *Coordinator) beginWork(runningGeneration uint64) error {
	return c.store.Update(func(next *state.State) error {
		next.BackupInProgress = true
		next.RunningGeneration = runningGeneration
		return nil
	})
}

func (c *Coordinator) endWork() error {
	return c.store.Update(func(next *state.State) error {
		next.BackupInProgress = false
		return nil
	})
}

func (c *Coordinator) promote(ctx context.Context, result snapshot.Result, runningGeneration uint64) error {
	currentPath := filepath.Join(c.cfg.StagingDir, "current")
	previousPath := filepath.Join(c.cfg.StagingDir, ".previous")
	if _, err := os.Lstat(previousPath); err == nil {
		if _, verifyErr := snapshot.Verify(ctx, currentPath, true); verifyErr != nil {
			return fmt.Errorf("stale previous snapshot exists while current is invalid")
		}
		if err := os.RemoveAll(previousPath); err != nil {
			return fmt.Errorf("remove stale previous snapshot: %w", err)
		}
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			return fmt.Errorf("sync stale snapshot removal: %w", err)
		}
	}
	hadCurrent := false
	if _, err := os.Lstat(currentPath); err == nil {
		if err := os.Rename(currentPath, previousPath); err != nil {
			return fmt.Errorf("retain previous recovery point: %w", err)
		}
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			_ = os.Rename(previousPath, currentPath)
			_ = syncCoordinatorDirectory(c.cfg.StagingDir)
			return fmt.Errorf("sync retained recovery point: %w", err)
		}
		hadCurrent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect current recovery point: %w", err)
	}
	if err := os.Rename(result.Path, currentPath); err != nil {
		if hadCurrent {
			_ = os.Rename(previousPath, currentPath)
			_ = syncCoordinatorDirectory(c.cfg.StagingDir)
		}
		return fmt.Errorf("promote recovery point: %w", err)
	}
	if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
		return fmt.Errorf("sync promoted recovery point: %w", err)
	}
	if _, err := snapshot.Verify(ctx, currentPath, true); err != nil {
		return fmt.Errorf("verify promoted recovery point: %w", err)
	}
	now := time.Now()
	if err := c.store.Update(func(next *state.State) error {
		next.Current = &state.RecoveryPoint{ContentID: result.Manifest.ContentID, Status: result.Manifest.Status}
		next.LastContentID = result.Manifest.ContentID
		next.LastSampleAt = state.FormatTime(now)
		next.LastSnapshotAt = state.FormatTime(now)
		if next.DirtyGeneration == runningGeneration {
			next.Dirty = false
			next.FirstDirtyAt = ""
			next.LastDirtyAt = ""
		}
		return nil
	}); err != nil {
		return fmt.Errorf("persist promoted recovery point: %w", err)
	}
	if hadCurrent {
		if err := os.RemoveAll(previousPath); err != nil {
			c.log.Error("old local recovery point cleanup failed", "error_code", "staging_cleanup_failed")
		} else if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			c.log.Error("old local recovery point sync failed", "error_code", "staging_cleanup_failed")
		}
	}
	return nil
}

type uploadAttempt struct {
	target        config.Target
	previousState string
	previousError string
}

type uploadResult struct {
	attempt uploadAttempt
	result  restic.BackupResult
	err     error
}

func (c *Coordinator) uploadCurrent(ctx context.Context, preemptible bool) error {
	current := c.store.Snapshot()
	if current.Current == nil {
		return nil
	}
	manifest, err := snapshot.Verify(ctx, filepath.Join(c.cfg.StagingDir, "current"), true)
	if err != nil {
		return fmt.Errorf("verify pending recovery point: %w", err)
	}
	now := time.Now()
	var attempts []uploadAttempt
	for _, target := range c.cfg.Targets {
		targetState := current.Targets[target.Name]
		if targetState.ContentID == current.Current.ContentID {
			continue
		}
		retryAt, _ := state.ParseTime(targetState.NextRetryAt)
		if targetState.State == "degraded" && !retryAt.IsZero() && retryAt.After(now) {
			continue
		}
		attempts = append(attempts, uploadAttempt{target: target, previousState: targetState.State, previousError: targetState.ErrorCode})
	}
	if len(attempts) == 0 {
		c.announceRecovery(manifest, now)
		return c.cleanupIfCaughtUp()
	}
	if err := c.store.Update(func(next *state.State) error {
		for _, attempt := range attempts {
			target := next.Targets[attempt.target.Name]
			target.State = "catching_up"
			target.LastAttemptAt = state.FormatTime(now)
			next.Targets[attempt.target.Name] = target
		}
		return nil
	}); err != nil {
		return err
	}
	uploadCtx, cancelUploads := context.WithCancel(ctx)
	defer cancelUploads()
	results := make(chan uploadResult, len(attempts))
	for _, attempt := range attempts {
		attempt := attempt
		go func() {
			targetCtx, cancel := context.WithTimeout(uploadCtx, c.cfg.TargetTimeout.Duration)
			defer cancel()
			result, err := c.restic.Backup(targetCtx, attempt.target, manifest)
			results <- uploadResult{attempt: attempt, result: result, err: err}
		}()
	}
	cancelledForSchedule := false
	waitOnly := false
	for remaining := len(attempts); remaining > 0; {
		var result uploadResult
		if waitOnly {
			result = <-results
		} else {
			var schedule <-chan time.Time
			var wake <-chan struct{}
			var timer *time.Timer
			if preemptible {
				now := time.Now()
				due := nextSnapshotAt(c.store.Snapshot(), c.cfg, now)
				if !due.After(now) {
					cancelledForSchedule = true
					waitOnly = true
					cancelUploads()
					continue
				}
				timer = time.NewTimer(time.Until(due))
				schedule = timer.C
				wake = c.wake
			}
			select {
			case result = <-results:
				stopTimer(timer)
			case <-wake:
				stopTimer(timer)
				continue
			case <-schedule:
				cancelledForSchedule = true
				waitOnly = true
				cancelUploads()
				continue
			case <-ctx.Done():
				stopTimer(timer)
				waitOnly = true
				cancelUploads()
				continue
			}
		}
		remaining--
		when := time.Now()
		if result.err == nil {
			if err := c.store.Update(func(next *state.State) error {
				target := next.Targets[result.attempt.target.Name]
				target.State = "healthy"
				target.ContentID = manifest.ContentID
				target.SnapshotID = result.result.SnapshotID
				target.LastSuccessAt = state.FormatTime(when)
				target.NextRetryAt = ""
				target.ConsecutiveFailures = 0
				target.ErrorCode = ""
				next.Targets[result.attempt.target.Name] = target
				return nil
			}); err != nil {
				return err
			}
			if result.attempt.previousState == "degraded" {
				c.enqueueTargetState("target_recovered", result.attempt.target.Name, "", when)
			}
			c.announceRecovery(manifest, when)
			continue
		}
		code := resticErrorCode(result.err)
		if (cancelledForSchedule || ctx.Err() != nil) &&
			(code == "operation_cancelled" || errors.Is(result.err, context.Canceled) || errors.Is(result.err, context.DeadlineExceeded)) {
			if err := c.store.Update(func(next *state.State) error {
				target := next.Targets[result.attempt.target.Name]
				target.State = result.attempt.previousState
				target.ErrorCode = result.attempt.previousError
				target.NextRetryAt = state.FormatTime(when)
				next.Targets[result.attempt.target.Name] = target
				return nil
			}); err != nil {
				return err
			}
			continue
		}
		failures := 1
		latest := c.store.Snapshot().Targets[result.attempt.target.Name]
		if latest.ConsecutiveFailures < 1_000_000 {
			failures = latest.ConsecutiveFailures + 1
		}
		retryAt := when.Add(backoff(failures))
		if errors.Is(ctx.Err(), context.Canceled) {
			retryAt = when
		}
		if err := c.store.Update(func(next *state.State) error {
			target := next.Targets[result.attempt.target.Name]
			target.State = "degraded"
			target.ErrorCode = code
			target.ConsecutiveFailures = failures
			target.NextRetryAt = state.FormatTime(retryAt)
			next.Targets[result.attempt.target.Name] = target
			return nil
		}); err != nil {
			return err
		}
		if result.attempt.previousState != "degraded" || result.attempt.previousError != code {
			c.enqueueTargetState("target_degraded", result.attempt.target.Name, code, when)
		}
	}
	return c.cleanupIfCaughtUp()
}

func stopTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (c *Coordinator) announceRecovery(manifest snapshot.Manifest, when time.Time) {
	current := c.store.Snapshot()
	if current.LastAnnouncedID == manifest.ContentID {
		return
	}
	available := false
	for _, configured := range c.cfg.Targets {
		target := current.Targets[configured.Name]
		if target.ContentID == manifest.ContentID {
			available = true
			break
		}
	}
	if !available || !c.enqueueRecovery(manifest, when) {
		return
	}
	if err := c.store.Update(func(next *state.State) error {
		next.LastAnnouncedID = manifest.ContentID
		return nil
	}); err != nil {
		c.log.Error("notification marker persistence failed", "error_code", "notification_state_failed")
	}
}

func (c *Coordinator) cleanupIfCaughtUp() error {
	current := c.store.Snapshot()
	if current.Current == nil {
		return nil
	}
	for _, target := range c.cfg.Targets {
		if current.Targets[target.Name].ContentID != current.Current.ContentID {
			return nil
		}
	}
	contentHex, err := snapshot.ContentIDHex(current.Current.ContentID)
	if err != nil {
		return err
	}
	currentPath := filepath.Join(c.cfg.StagingDir, "current")
	deletePath := filepath.Join(c.cfg.StagingDir, ".delete-"+contentHex)
	if err := os.Rename(currentPath, deletePath); err != nil {
		return fmt.Errorf("retire caught-up recovery point: %w", err)
	}
	if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
		_ = os.Rename(deletePath, currentPath)
		_ = syncCoordinatorDirectory(c.cfg.StagingDir)
		return fmt.Errorf("sync retired recovery point: %w", err)
	}
	if err := c.store.Update(func(next *state.State) error {
		next.Current = nil
		return nil
	}); err != nil {
		_ = os.Rename(deletePath, currentPath)
		_ = syncCoordinatorDirectory(c.cfg.StagingDir)
		return err
	}
	if err := os.RemoveAll(deletePath); err != nil {
		c.log.Error("caught-up local staging cleanup failed", "error_code", "staging_cleanup_failed")
	} else if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
		c.log.Error("caught-up local staging sync failed", "error_code", "staging_cleanup_failed")
	}
	return nil
}

func (c *Coordinator) Reconcile(ctx context.Context) error {
	if err := safefs.EnsurePrivateDirectory(c.cfg.StagingDir); err != nil {
		return err
	}
	currentPath := filepath.Join(c.cfg.StagingDir, "current")
	previousPath := filepath.Join(c.cfg.StagingDir, ".previous")
	if _, currentErr := os.Lstat(currentPath); errors.Is(currentErr, os.ErrNotExist) {
		if _, previousStatErr := os.Lstat(previousPath); previousStatErr == nil {
			previousManifest, previousErr := snapshot.Verify(ctx, previousPath, true)
			if previousErr != nil {
				return fmt.Errorf("retained staging recovery point is invalid and was preserved: %w", previousErr)
			}
			if err := os.Rename(previousPath, currentPath); err != nil {
				return fmt.Errorf("recover retained staging recovery point: %w", err)
			}
			if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
				return fmt.Errorf("sync recovered staging recovery point: %w", err)
			}
			if err := c.store.Update(func(next *state.State) error {
				next.Current = &state.RecoveryPoint{ContentID: previousManifest.ContentID, Status: previousManifest.Status}
				next.LastContentID = previousManifest.ContentID
				return nil
			}); err != nil {
				return err
			}
		} else if !errors.Is(previousStatErr, os.ErrNotExist) {
			return fmt.Errorf("inspect retained staging recovery point: %w", previousStatErr)
		}
	}
	manifest, verifyErr := snapshot.Verify(ctx, currentPath, true)
	if verifyErr == nil {
		if err := c.store.Update(func(next *state.State) error {
			next.Current = &state.RecoveryPoint{ContentID: manifest.ContentID, Status: manifest.Status}
			next.LastContentID = manifest.ContentID
			return nil
		}); err != nil {
			return err
		}
		if _, previousErr := os.Lstat(previousPath); previousErr == nil {
			if err := os.RemoveAll(previousPath); err != nil {
				return fmt.Errorf("remove reconciled previous recovery point: %w", err)
			}
			if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
				return fmt.Errorf("sync reconciled previous recovery point removal: %w", err)
			}
		} else if !errors.Is(previousErr, os.ErrNotExist) {
			return fmt.Errorf("inspect reconciled previous recovery point: %w", previousErr)
		}
	} else if _, statErr := os.Lstat(currentPath); statErr == nil {
		return fmt.Errorf("current staging recovery point is invalid and was preserved")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else if c.store.Snapshot().Current != nil {
		if err := c.store.Update(func(next *state.State) error {
			next.Current = nil
			next.Dirty = true
			next.DirtyGeneration++
			now := state.FormatTime(time.Now())
			next.FirstDirtyAt = now
			next.LastDirtyAt = now
			return nil
		}); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(c.cfg.StagingDir)
	if err != nil {
		return err
	}
	removedStale := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".build-") || strings.HasPrefix(entry.Name(), ".delete-") {
			path := filepath.Join(c.cfg.StagingDir, entry.Name())
			if err := os.RemoveAll(path); err != nil {
				c.log.Error("stale staging cleanup failed", "error_code", "staging_cleanup_failed")
			} else {
				removedStale = true
			}
		}
	}
	if removedStale {
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			return fmt.Errorf("sync stale staging cleanup: %w", err)
		}
	}
	return nil
}

func syncCoordinatorDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (c *Coordinator) enqueueRecovery(manifest snapshot.Manifest, when time.Time) bool {
	current := c.store.Snapshot()
	targets := make([]notify.TargetStatus, 0, len(c.cfg.Targets))
	for _, cfgTarget := range c.cfg.Targets {
		target := current.Targets[cfgTarget.Name]
		targets = append(targets, notify.TargetStatus{Name: target.Name, State: target.State, ErrorCode: target.ErrorCode})
	}
	event := notify.NewRecoveryEvent("recovery:"+manifest.ContentID, state.FormatTime(when), notify.RecoveryPoint{ContentID: manifest.ContentID, Status: manifest.Status}, targets)
	var err error
	if manifest.Status == snapshot.Incomplete {
		err = c.notifiers.EnqueueState("source-integrity", event)
	} else {
		err = c.notifiers.EnqueueSuccess(event)
	}
	if err != nil {
		c.log.Error("recovery notification persistence failed", "error_code", "notification_state_failed")
		return false
	}
	return true
}

func (c *Coordinator) enqueueTargetState(eventType, name, code string, when time.Time) {
	event := notify.NewTargetEvent(eventType+":"+name+":"+state.FormatTime(when), eventType, state.FormatTime(when), notify.TargetStatus{Name: name, State: map[string]string{"target_degraded": "degraded", "target_recovered": "healthy"}[eventType], ErrorCode: code})
	if err := c.notifiers.EnqueueState("target:"+name, event); err != nil {
		c.log.Error("target-state notification persistence failed", "error_code", "notification_state_failed")
	}
}

func (c *Coordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func backoff(failures int) time.Duration {
	base := time.Minute
	for i := 1; i < failures && base < 15*time.Minute; i++ {
		base *= 2
	}
	if base > 15*time.Minute {
		base = 15 * time.Minute
	}
	jitterRange := base / 5
	if jitterRange <= 0 || base >= 15*time.Minute {
		return base
	}
	random, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(jitterRange)+1))
	if err != nil {
		return base
	}
	result := base + time.Duration(random.Int64())
	if result > 15*time.Minute {
		return 15 * time.Minute
	}
	return result
}

func resticErrorCode(err error) string {
	var operation *restic.OperationError
	if errors.As(err, &operation) {
		return operation.Code
	}
	return "backup_failed"
}

func safeCoordinatorCode(err error) string {
	var missing *snapshot.MissingFilesError
	if errors.As(err, &missing) {
		return "source_files_unavailable"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "operation_cancelled"
	}
	return "snapshot_failed"
}
