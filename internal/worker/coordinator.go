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
	"slices"
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
	randomInt func(int64) (int64, error)
}

type backupRunner interface {
	Backup(context.Context, config.Target, snapshot.Manifest) (restic.BackupResult, error)
}

type candidateRunner interface {
	backupRunner
	ListSnapshots(context.Context, config.Target) (restic.SnapshotReport, error)
}

func NewCoordinator(cfg *config.Backup, store *state.Store, resticRunner backupRunner, notifiers *notify.Manager, log *slog.Logger) *Coordinator {
	return &Coordinator{
		cfg: cfg, store: store, builder: snapshot.Builder{DataDir: cfg.DataDir, StagingDir: cfg.StagingDir},
		restic: resticRunner, notifiers: notifiers, log: log, wake: make(chan struct{}, 1),
		randomInt: func(max int64) (int64, error) {
			value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(max))
			if err != nil {
				return 0, err
			}
			return value.Int64(), nil
		},
	}
}

func (c *Coordinator) MarkDirty(now time.Time) error {
	if len(c.cfg.Targets) == 0 {
		return nil
	}
	c.dirtyMu.Lock()
	defer c.dirtyMu.Unlock()
	formatted := state.FormatTime(now)
	err := c.store.Update(func(next *state.State) error {
		if next.Dirty {
			if next.BackupInProgress && next.DirtyGeneration != next.RunningGeneration {
				return errDirtyAlreadyQueued
			}
			if !next.BackupInProgress {
				next.LastDirtyAt = formatted
				return nil
			}
		}
		next.DirtyGeneration++
		next.Dirty = true
		next.LastDirtyAt = formatted
		if next.FirstDirtyAt == "" {
			next.FirstDirtyAt = formatted
		}
		return nil
	})
	if err == nil || errors.Is(err, errDirtyAlreadyQueued) {
		c.signal()
		return nil
	}
	return err
}

var errDirtyAlreadyQueued = errors.New("dirty generation already queued")

func (c *Coordinator) Run(ctx context.Context) error {
	if len(c.cfg.Targets) == 0 {
		<-ctx.Done()
		return nil
	}
	if err := c.Reconcile(ctx); err != nil {
		return err
	}
	if err := c.MarkDirty(time.Now()); err != nil {
		return err
	}
	if err := c.sample(ctx); err != nil && ctx.Err() == nil {
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
			if err := c.sample(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("recovery-point sampling failed", "error_code", safeCoordinatorCode(err))
			}
			continue
		}
		if !retryAt.IsZero() && !retryAt.After(now) {
			if err := c.catchUp(ctx); err != nil && ctx.Err() == nil {
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
	if len(c.cfg.Targets) == 0 {
		return nil
	}
	return c.sample(ctx)
}

func (c *Coordinator) sample(ctx context.Context) (resultErr error) {
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

	localCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	attemptedAt := time.Now()
	if err := c.store.Update(func(next *state.State) error {
		next.Source.LastAttemptAt = state.FormatTime(attemptedAt)
		return nil
	}); err != nil {
		return err
	}
	var result snapshot.Result
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		result, err = c.builder.Build(localCtx, attempt == 3)
		if err == nil {
			break
		}
		var missing *snapshot.MissingFilesError
		if !errors.As(err, &missing) || attempt == 3 {
			break
		}
		timer := time.NewTimer(c.cfg.MissingRetry.Duration)
		select {
		case <-localCtx.Done():
			timer.Stop()
			err = localCtx.Err()
			attempt = 3
		case <-timer.C:
		}
	}
	if err != nil {
		_ = c.markSourceFailure(err, time.Now())
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
	if err := c.markSourceHealthy(time.Now()); err != nil {
		return err
	}
	if err := c.promote(localCtx, result, runningGeneration); err != nil {
		_ = os.RemoveAll(result.Path)
		return err
	}
	return c.uploadCurrentWithManifest(ctx, &result.Manifest)
}

func (c *Coordinator) catchUp(ctx context.Context) (resultErr error) {
	if err := c.beginWork(c.store.Snapshot().DirtyGeneration); err != nil {
		return err
	}
	defer func() {
		if err := c.endWork(); err != nil && resultErr == nil {
			resultErr = err
		}
	}()
	return c.uploadCurrentWithManifest(ctx, nil)
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
		return fmt.Errorf("staging contains a previous recovery point; reconcile before promotion")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect previous recovery point: %w", err)
	}
	hadCurrent := false
	if _, err := os.Lstat(currentPath); err == nil {
		if err := os.Rename(currentPath, previousPath); err != nil {
			return fmt.Errorf("retain previous recovery point: %w", err)
		}
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			return fmt.Errorf("sync retained recovery point: %w", err)
		}
		hadCurrent = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect current recovery point: %w", err)
	}
	if err := os.Rename(result.Path, currentPath); err != nil {
		return fmt.Errorf("promote recovery point: %w", err)
	}
	if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
		return fmt.Errorf("sync promoted recovery point: %w", err)
	}
	now := time.Now()
	if err := c.store.Update(func(next *state.State) error {
		next.Current = &state.RecoveryPoint{ContentID: result.Manifest.ContentID, Status: result.Manifest.Status}
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

func (c *Coordinator) uploadCurrentWithManifest(ctx context.Context, supplied *snapshot.Manifest) error {
	current := c.store.Snapshot()
	if current.Current == nil {
		return nil
	}
	manifest := snapshot.Manifest{}
	if supplied != nil {
		manifest = *supplied
	} else {
		var err error
		manifest, err = snapshot.Verify(ctx, filepath.Join(c.cfg.StagingDir, "current"), true)
		if err != nil {
			return fmt.Errorf("verify pending recovery point: %w", err)
		}
	}
	now := time.Now()
	attempts := make([]uploadAttempt, 0, len(c.cfg.Targets))
	for _, target := range c.cfg.Targets {
		targetState := current.Targets[target.Name]
		if targetContentID(targetState) == current.Current.ContentID {
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
			target.UploadIntent = &state.UploadIntent{ContentID: manifest.ContentID, Status: manifest.Status, StartedAt: state.FormatTime(now)}
			next.Targets[attempt.target.Name] = target
		}
		return nil
	}); err != nil {
		return err
	}
	results := make(chan uploadResult, len(attempts))
	for _, attempt := range attempts {
		attempt := attempt
		go func() {
			targetCtx, cancel := context.WithTimeout(ctx, c.cfg.TargetTimeout.Duration)
			defer cancel()
			result, err := c.restic.Backup(targetCtx, attempt.target, manifest)
			results <- uploadResult{attempt: attempt, result: result, err: err}
		}()
	}
	for remaining := len(attempts); remaining > 0; {
		result := <-results
		remaining--
		when := time.Now()
		if result.err != nil && remoteLookupAllowed(result.err) {
			if recovered, snapshotID := c.findRemoteContent(ctx, result.attempt.target, manifest); recovered {
				result.err = nil
				result.result.SnapshotID = snapshotID
			}
		}
		if result.err == nil {
			if err := c.persistTargetSuccess(result.attempt.target.Name, manifest, result.result.SnapshotID, when); err != nil {
				c.log.Error("target success state persistence failed", "target", result.attempt.target.Name, "error_code", "runtime_state_failed")
				if recovered, snapshotID := c.findRemoteContent(ctx, result.attempt.target, manifest); recovered {
					if retryErr := c.persistTargetSuccess(result.attempt.target.Name, manifest, snapshotID, when); retryErr == nil {
						c.announceRecovery(manifest, when)
						continue
					}
				}
				continue
			}
			if result.attempt.previousState == "degraded" {
				c.enqueueTargetState("target_recovered", result.attempt.target.Name, "", when)
			}
			c.announceRecovery(manifest, when)
			continue
		}
		code := resticErrorCode(result.err)
		latest := c.store.Snapshot().Targets[result.attempt.target.Name]
		failures := latest.ConsecutiveFailures + 1
		if failures > 1_000_000 {
			failures = 1_000_000
		}
		retryAt := when.Add(c.targetBackoff(failures))
		if err := c.store.Update(func(next *state.State) error {
			target := next.Targets[result.attempt.target.Name]
			target.State = "degraded"
			target.ErrorCode = code
			target.ConsecutiveFailures = failures
			target.NextRetryAt = state.FormatTime(retryAt)
			target.UploadIntent = nil
			next.Targets[result.attempt.target.Name] = target
			return nil
		}); err != nil {
			return err
		}
		if result.attempt.previousState != "degraded" {
			c.enqueueTargetState("target_degraded", result.attempt.target.Name, code, when)
		}
	}
	if err := c.cleanupIfCaughtUp(); err != nil {
		return err
	}
	return nil
}

func (c *Coordinator) persistTargetSuccess(name string, manifest snapshot.Manifest, snapshotID string, when time.Time) error {
	return c.store.Update(func(next *state.State) error {
		target := next.Targets[name]
		target.State = "healthy"
		target.LastRecoveryPoint = &state.RecoveryPoint{ContentID: manifest.ContentID, Status: manifest.Status}
		if manifest.Status == snapshot.Complete {
			target.LastCompleteRecoveryPoint = &state.RecoveryPoint{ContentID: manifest.ContentID, Status: manifest.Status}
			next.LatestCompleteContentID = manifest.ContentID
		}
		target.SnapshotID = snapshotID
		target.LastSuccessAt = state.FormatTime(when)
		target.NextRetryAt = ""
		target.ConsecutiveFailures = 0
		target.ErrorCode = ""
		target.UploadIntent = nil
		next.Targets[name] = target
		return nil
	})
}

func remoteLookupAllowed(err error) bool {
	var operation *restic.OperationError
	if !errors.As(err, &operation) {
		return true
	}
	switch operation.Code {
	case "repository_backend_mismatch", "role_credentials_missing", "backend_credentials_invalid", "password_directory_invalid", "password_file_invalid", "cache_path_invalid", "cache_create_failed", "invalid_content_id", "invalid_manifest":
		return false
	default:
		return true
	}
}

func (c *Coordinator) findRemoteContent(ctx context.Context, target config.Target, manifest snapshot.Manifest) (bool, string) {
	runner, ok := c.restic.(candidateRunner)
	if !ok {
		return false, ""
	}
	targetCtx, cancel := c.targetContext(ctx)
	defer cancel()
	report, err := runner.ListSnapshots(targetCtx, target)
	if err != nil {
		return false, ""
	}
	var matches []restic.SnapshotInfo
	for _, candidate := range report.Snapshots {
		if candidate.ContentID == manifest.ContentID && candidate.Status == manifest.Status {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return false, ""
	}
	slices.SortFunc(matches, func(left, right restic.SnapshotInfo) int {
		if left.Time == right.Time {
			return strings.Compare(left.ID, right.ID)
		}
		return strings.Compare(left.Time, right.Time)
	})
	return true, matches[len(matches)-1].ID
}

func (c *Coordinator) targetContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.cfg == nil || c.cfg.TargetTimeout.Duration <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.cfg.TargetTimeout.Duration)
}

func (c *Coordinator) targetBackoff(failures int) time.Duration {
	base := time.Minute
	for index := 1; index < failures && base < 15*time.Minute; index++ {
		base *= 2
	}
	if base >= 15*time.Minute {
		return 15 * time.Minute
	}
	jitter := base / 5
	if jitter <= 0 || c.randomInt == nil {
		return base
	}
	extra, err := c.randomInt(int64(jitter) + 1)
	if err != nil {
		return base
	}
	return base + time.Duration(extra)
}

func (c *Coordinator) announceRecovery(manifest snapshot.Manifest, when time.Time) {
	current := c.store.Snapshot()
	if current.LastAnnouncedID == manifest.ContentID {
		return
	}
	available := false
	for _, configured := range c.cfg.Targets {
		target := current.Targets[configured.Name]
		if targetContentID(target) == manifest.ContentID {
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
		if targetContentID(current.Targets[target.Name]) != current.Current.ContentID {
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
	publishErr := syncCoordinatorDirectory(c.cfg.StagingDir)
	if err := c.store.Update(func(next *state.State) error {
		next.Current = nil
		return nil
	}); err != nil {
		var persistErr *state.PersistError
		if errors.As(err, &persistErr) && persistErr.PublishState == safefs.Unpublished {
			if rollbackErr := os.Rename(deletePath, currentPath); rollbackErr != nil {
				return fmt.Errorf("persist retirement state: %w; restore current recovery point: %v", err, rollbackErr)
			}
			if rollbackErr := syncCoordinatorDirectory(c.cfg.StagingDir); rollbackErr != nil {
				return fmt.Errorf("persist retirement state: %w; sync restored recovery point: %v", err, rollbackErr)
			}
		}
		// A published_unconfirmed state rename is authoritative. Leave the
		// identified .delete-* entry for Reconcile to clean up instead of
		// manufacturing a second, contradictory rollback.
		return err
	}
	if publishErr != nil {
		return fmt.Errorf("retire_published_unconfirmed: %w", publishErr)
	}
	if err := os.RemoveAll(deletePath); err != nil {
		c.log.Error("caught-up local staging cleanup failed", "error_code", "staging_cleanup_failed")
	} else if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
		c.log.Error("caught-up local staging sync failed", "error_code", "staging_cleanup_failed")
	}
	return nil
}

func (c *Coordinator) Reconcile(ctx context.Context) error {
	if err := safefs.ValidatePrivateDirectory(c.cfg.StagingDir); err != nil {
		return err
	}
	entries, err := os.ReadDir(c.cfg.StagingDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "current" && name != ".previous" && !strings.HasPrefix(name, ".build-") && !strings.HasPrefix(name, ".delete-") {
			return fmt.Errorf("unknown staging entry %q", name)
		}
	}
	currentPath := filepath.Join(c.cfg.StagingDir, "current")
	previousPath := filepath.Join(c.cfg.StagingDir, ".previous")
	_, currentErr := os.Lstat(currentPath)
	if currentErr != nil && !errors.Is(currentErr, os.ErrNotExist) {
		return fmt.Errorf("inspect current recovery point: %w", currentErr)
	}
	_, previousErr := os.Lstat(previousPath)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return fmt.Errorf("inspect previous recovery point: %w", previousErr)
	}
	currentPresent := currentErr == nil
	previousPresent := previousErr == nil
	recoveredPrevious := false
	var currentManifest snapshot.Manifest
	currentValid := currentPresent
	if currentValid {
		currentManifest, currentErr = snapshot.Verify(ctx, currentPath, true)
		currentValid = currentErr == nil
	}
	initialCurrentValid := currentValid
	var previousManifest snapshot.Manifest
	previousValid := previousPresent
	if previousValid {
		previousManifest, previousErr = snapshot.Verify(ctx, previousPath, true)
		previousValid = previousErr == nil
	}
	if !currentValid && previousValid {
		if currentPresent {
			if err := os.RemoveAll(currentPath); err != nil {
				return fmt.Errorf("remove invalid current recovery point: %w", err)
			}
			if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
				return err
			}
		}
		if err := os.Rename(previousPath, currentPath); err != nil {
			return fmt.Errorf("recover previous recovery point: %w", err)
		}
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			return fmt.Errorf("sync recovered recovery point: %w", err)
		}
		currentManifest = previousManifest
		currentValid = true
		recoveredPrevious = true
		previousPresent = false
	}
	if !currentValid && (currentPresent || previousPresent) {
		if currentPresent {
			if err := os.RemoveAll(currentPath); err != nil {
				return fmt.Errorf("remove invalid current recovery point: %w", err)
			}
		}
		if previousPresent {
			if err := os.RemoveAll(previousPath); err != nil {
				return fmt.Errorf("remove invalid previous recovery point: %w", err)
			}
		}
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			return err
		}
		if err := c.store.Update(func(next *state.State) error {
			next.Current = nil
			next.Dirty = true
			next.DirtyGeneration++
			now := state.FormatTime(time.Now())
			next.FirstDirtyAt = now
			next.LastDirtyAt = now
			next.Source.State = "degraded"
			next.Source.ErrorCode = "staging_recovery_invalid"
			return nil
		}); err != nil {
			return err
		}
	} else if !currentValid && c.store.Snapshot().Current != nil {
		if err := c.store.Update(func(next *state.State) error {
			next.Current = nil
			next.Dirty = true
			next.DirtyGeneration++
			now := state.FormatTime(time.Now())
			next.FirstDirtyAt = now
			next.LastDirtyAt = now
			next.Source.State = "degraded"
			next.Source.ErrorCode = "staging_recovery_missing"
			return nil
		}); err != nil {
			return err
		}
	}
	if currentValid {
		if err := c.store.Update(func(next *state.State) error {
			next.Current = &state.RecoveryPoint{ContentID: currentManifest.ContentID, Status: currentManifest.Status}
			return nil
		}); err != nil {
			return err
		}
		if previousPresent && !recoveredPrevious {
			if err := os.RemoveAll(previousPath); err != nil {
				return fmt.Errorf("remove superseded previous recovery point: %w", err)
			}
			if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
				return err
			}
		}
	}
	removed := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".build-") || strings.HasPrefix(entry.Name(), ".delete-") {
			if err := os.RemoveAll(filepath.Join(c.cfg.StagingDir, entry.Name())); err != nil {
				return fmt.Errorf("remove stale staging entry: %w", err)
			}
			removed = true
		}
	}
	if removed {
		if err := syncCoordinatorDirectory(c.cfg.StagingDir); err != nil {
			return fmt.Errorf("sync stale staging cleanup: %w", err)
		}
	}
	if err := c.reconcileUploadIntents(ctx); err != nil {
		return err
	}
	if c.store.Rebuilt() && !initialCurrentValid {
		if err := c.rebuildProtection(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) reconcileUploadIntents(ctx context.Context) error {
	runner, ok := c.restic.(candidateRunner)
	if !ok {
		return nil
	}
	current := c.store.Snapshot()
	for _, configured := range c.cfg.Targets {
		targetState := current.Targets[configured.Name]
		if targetState.UploadIntent == nil {
			continue
		}
		targetCtx, cancel := c.targetContext(ctx)
		report, err := runner.ListSnapshots(targetCtx, configured)
		cancel()
		if err != nil {
			continue
		}
		var matches []restic.SnapshotInfo
		for _, candidate := range report.Snapshots {
			if candidate.ContentID == targetState.UploadIntent.ContentID && candidate.Status == targetState.UploadIntent.Status {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 0 {
			continue
		}
		slices.SortFunc(matches, func(left, right restic.SnapshotInfo) int {
			if left.Time == right.Time {
				return strings.Compare(left.ID, right.ID)
			}
			return strings.Compare(left.Time, right.Time)
		})
		selected := matches[len(matches)-1]
		when := time.Now()
		if err := c.store.Update(func(next *state.State) error {
			target := next.Targets[configured.Name]
			target.State = "healthy"
			target.LastRecoveryPoint = &state.RecoveryPoint{ContentID: selected.ContentID, Status: selected.Status}
			if selected.Status == snapshot.Complete {
				target.LastCompleteRecoveryPoint = &state.RecoveryPoint{ContentID: selected.ContentID, Status: selected.Status}
				next.LatestCompleteContentID = selected.ContentID
			}
			target.SnapshotID = selected.ID
			target.LastSuccessAt = state.FormatTime(when)
			target.UploadIntent = nil
			target.NextRetryAt = ""
			target.ConsecutiveFailures = 0
			target.ErrorCode = ""
			next.Targets[configured.Name] = target
			return nil
		}); err != nil {
			return err
		}
		current = c.store.Snapshot()
	}
	return nil
}

func (c *Coordinator) rebuildProtection(ctx context.Context) error {
	runner, ok := c.restic.(candidateRunner)
	if !ok {
		return nil
	}
	type targetProgress struct {
		latestAny      *restic.SnapshotInfo
		latestComplete *restic.SnapshotInfo
	}
	progress := make(map[string]targetProgress, len(c.cfg.Targets))
	var globalLatest *restic.SnapshotInfo
	for _, configured := range c.cfg.Targets {
		targetCtx, cancel := c.targetContext(ctx)
		report, err := runner.ListSnapshots(targetCtx, configured)
		cancel()
		if err != nil {
			continue
		}
		var latest targetProgress
		for index := range report.Snapshots {
			candidate := &report.Snapshots[index]
			if latest.latestAny == nil || newerSnapshot(*candidate, *latest.latestAny) {
				latest.latestAny = candidate
			}
			if candidate.Status == snapshot.Complete && (latest.latestComplete == nil || newerSnapshot(*candidate, *latest.latestComplete)) {
				latest.latestComplete = candidate
			}
		}
		progress[configured.Name] = latest
		if latest.latestComplete != nil && (globalLatest == nil || newerSnapshot(*latest.latestComplete, *globalLatest)) {
			globalLatest = latest.latestComplete
		}
	}
	return c.store.Update(func(next *state.State) error {
		for _, configured := range c.cfg.Targets {
			latest, exists := progress[configured.Name]
			if !exists {
				continue
			}
			target := next.Targets[configured.Name]
			if latest.latestAny != nil {
				target.LastRecoveryPoint = &state.RecoveryPoint{ContentID: latest.latestAny.ContentID, Status: latest.latestAny.Status}
				target.SnapshotID = latest.latestAny.ID
			}
			if latest.latestComplete != nil {
				candidate := latest.latestComplete
				target.LastCompleteRecoveryPoint = &state.RecoveryPoint{ContentID: candidate.ContentID, Status: candidate.Status}
			}
			if latest.latestAny != nil || latest.latestComplete != nil {
				next.Targets[configured.Name] = target
			}
		}
		if globalLatest != nil {
			next.LatestCompleteContentID = globalLatest.ContentID
		}
		return nil
	})
}

func newerSnapshot(left, right restic.SnapshotInfo) bool {
	if left.Time == right.Time {
		return left.ID > right.ID
	}
	return left.Time > right.Time
}

func syncCoordinatorDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func (c *Coordinator) enqueueRecovery(manifest snapshot.Manifest, when time.Time) bool {
	if c.notifiers == nil {
		return true
	}
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
	if c.notifiers == nil {
		return
	}
	event := notify.NewTargetEvent(eventType+":"+name+":"+state.FormatTime(when), eventType, state.FormatTime(when), notify.TargetStatus{Name: name, State: map[string]string{"target_degraded": "degraded", "target_recovered": "healthy"}[eventType], ErrorCode: code})
	if err := c.notifiers.EnqueueState("target:"+name, event); err != nil {
		c.log.Error("target-state notification persistence failed", "error_code", "notification_state_failed")
	}
}

func (c *Coordinator) markSourceFailure(err error, when time.Time) error {
	code := safeCoordinatorCode(err)
	current := c.store.Snapshot()
	failures := current.Source.ConsecutiveFailures + 1
	if failures > 1_000_000 {
		failures = 1_000_000
	}
	retryAt := when.Add(sourceBackoff(failures))
	wasDegraded := current.Source.State == "degraded"
	if err := c.store.Update(func(next *state.State) error {
		next.Source.State = "degraded"
		next.Source.LastAttemptAt = state.FormatTime(when)
		next.Source.ConsecutiveFailures = failures
		next.Source.ErrorCode = code
		next.Source.NextRetryAt = state.FormatTime(retryAt)
		return nil
	}); err != nil {
		return err
	}
	if !wasDegraded && c.notifiers != nil {
		event := notify.NewSourceEvent("source-degraded:"+state.FormatTime(when), "source_degraded", state.FormatTime(when), code)
		if err := c.notifiers.EnqueueState("source", event); err != nil {
			c.log.Error("source-state notification persistence failed", "error_code", "notification_state_failed")
		}
	}
	return nil
}

func (c *Coordinator) markSourceHealthy(when time.Time) error {
	previous := c.store.Snapshot().Source
	if err := c.store.Update(func(next *state.State) error {
		next.Source.State = "healthy"
		next.Source.LastSuccessAt = state.FormatTime(when)
		next.Source.ConsecutiveFailures = 0
		next.Source.ErrorCode = ""
		next.Source.NextRetryAt = ""
		return nil
	}); err != nil {
		return err
	}
	if previous.State == "degraded" && c.notifiers != nil {
		event := notify.NewSourceEvent("source-recovered:"+state.FormatTime(when), "source_recovered", state.FormatTime(when), "")
		if err := c.notifiers.EnqueueState("source", event); err != nil {
			c.log.Error("source-state notification persistence failed", "error_code", "notification_state_failed")
		}
	}
	return nil
}

func (c *Coordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func sourceBackoff(failures int) time.Duration {
	switch {
	case failures <= 1:
		return time.Minute
	case failures == 2:
		return 2 * time.Minute
	case failures == 3:
		return 4 * time.Minute
	case failures == 4:
		return 8 * time.Minute
	case failures <= 7:
		return 15 * time.Minute
	default:
		return 6 * time.Hour
	}
}

func resticErrorCode(err error) string {
	var operation *restic.OperationError
	if errors.As(err, &operation) {
		return operation.Code
	}
	return "backup_failed"
}

func targetContentID(target state.Target) string {
	if target.LastRecoveryPoint == nil {
		return ""
	}
	return target.LastRecoveryPoint.ContentID
}

func safeCoordinatorCode(err error) string {
	var missing *snapshot.MissingFilesError
	if errors.As(err, &missing) {
		return "source_files_unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "operation_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "operation_cancelled"
	}
	return "snapshot_failed"
}
