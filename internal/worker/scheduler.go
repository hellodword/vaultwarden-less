package worker

import (
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/state"
)

func nextSnapshotAt(current state.State, cfg *config.Backup, now time.Time) time.Time {
	var next time.Time
	lastSample, _ := state.ParseTime(current.LastSampleAt)
	if lastSample.IsZero() {
		next = now
	} else {
		next = lastSample.Add(cfg.SampleInterval.Duration)
	}
	if current.Dirty {
		firstDirty, _ := state.ParseTime(current.FirstDirtyAt)
		lastDirty, _ := state.ParseTime(current.LastDirtyAt)
		if firstDirty.IsZero() {
			firstDirty = now
		}
		if lastDirty.IsZero() {
			lastDirty = firstDirty
		}
		dirtyAt := lastDirty.Add(cfg.Debounce.Duration)
		maxAt := firstDirty.Add(cfg.MaxWait.Duration)
		if maxAt.Before(dirtyAt) {
			dirtyAt = maxAt
		}
		lastSnapshot, _ := state.ParseTime(current.LastSnapshotAt)
		minimumAt := lastSnapshot.Add(cfg.MinInterval.Duration)
		if !lastSnapshot.IsZero() && minimumAt.After(dirtyAt) {
			dirtyAt = minimumAt
		}
		if next.IsZero() || dirtyAt.Before(next) {
			next = dirtyAt
		}
	}
	if current.Source.State == "degraded" {
		if retryAt, err := state.ParseTime(current.Source.NextRetryAt); err == nil && !retryAt.IsZero() {
			// While the source is degraded, the retry schedule is the next
			// sampling opportunity. A later periodic fallback must not delay it.
			if retryAt.Before(now) {
				retryAt = now
			}
			next = retryAt
		}
	}
	return next
}

func nextTargetRetryAt(current state.State, now time.Time) time.Time {
	if current.Current == nil {
		return time.Time{}
	}
	var next time.Time
	for _, target := range current.Targets {
		if targetContentID(target) == current.Current.ContentID {
			continue
		}
		retryAt, _ := state.ParseTime(target.NextRetryAt)
		if retryAt.IsZero() {
			retryAt = now
		}
		if next.IsZero() || retryAt.Before(next) {
			next = retryAt
		}
	}
	return next
}
