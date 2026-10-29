package worker

import (
	"context"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/restic"
)

func (w *Worker) bindRepository(t *targetState, m restic.Metadata, now time.Time) {
	if t.status.RepositoryID != m.ID {
		// Neither verification successes nor backoff from a different repository
		// prove anything about the newly configured repository.
		t.status = TargetStatus{Name: t.config.Name, RepositoryID: m.ID, State: "ready"}
	}
	if t.status.SuccessID != "" && !m.Has(t.status.SuccessID) {
		t.status.Error = "coverage_missing"
		t.status.ExitCode = 0
		t.refreshAfter = w.captureSeq + 1
		w.schedule.urgent = true
		w.schedule.reason("coverage_missing")
	}
	latest, complete := m.Latest(false), m.Latest(true)
	if latest != nil {
		t.status.SuccessID = latest.ID
		t.status.SuccessAt = latest.Time
		t.status.Fingerprint = latest.Fingerprint()
	} else {
		t.status.SuccessID = ""
		t.status.Fingerprint = ""
	}
	if complete != nil {
		t.status.CompleteID = complete.ID
		t.status.CompleteAt = complete.Time
	} else {
		t.status.CompleteID = ""
		t.status.CompleteAt = time.Time{}
	}
	w.enableVerification(t, now)
}
func (w *Worker) enableVerification(t *targetState, now time.Time) {
	for _, entry := range []struct {
		state *VerificationStatus
		every time.Duration
	}{{&t.status.Structure, t.config.Verify.StructureEvery}, {&t.status.ReadData, t.config.Verify.ReadDataEvery}} {
		v := entry.state
		v.Scheduled = entry.every > 0
		if !v.Scheduled {
			v.Next = time.Time{}
			v.RetryAt = time.Time{}
		} else if v.Next.IsZero() && t.status.SuccessID != "" {
			v.Next = now
		}
	}
}
func (w *Worker) dueVerification(t *targetState, now time.Time) *verification {
	// Manual jobs remain available even when the repository has no snapshot.
	if len(t.verifyQueue) > 0 {
		op := w.operations[t.verifyQueue[0]]
		return &verification{ReadData: op.ReadData, Manual: true, Operation: op.ID}
	}
	if t.status.SuccessID == "" {
		return nil
	}
	read, structure := t.status.ReadData, t.status.Structure
	if read.Scheduled && !read.Next.IsZero() && !read.Next.After(now) {
		return &verification{ReadData: true}
	}
	if read.Scheduled && !read.RetryAt.IsZero() && !read.RetryAt.After(now) {
		return &verification{ReadData: true, Retry: true}
	}
	if structure.Scheduled && !structure.Next.IsZero() && !structure.Next.After(now) {
		return &verification{}
	}
	return nil
}
func (w *Worker) startVerification(ctx context.Context, index int, t *targetState, v verification) bool {
	if v.ReadData {
		for _, other := range w.targets {
			if other.verifying != nil && other.verifying.ReadData {
				return false
			}
		}
	}
	now := w.clock.Now()
	t.verifying = &v
	t.verifyBarrier = false
	t.status.Operation = "verify_structure"
	t.status.Structure.LastAttempt = now
	if v.ReadData {
		t.status.Operation = "verify_read_data"
		t.status.ReadData.LastAttempt = now
	}
	if v.Retry {
		t.status.ReadData.RetryAt = time.Time{}
	}
	if v.Manual {
		t.verifyQueue = t.verifyQueue[1:]
		op := w.operations[v.Operation]
		op.State = "running"
		op.Started = now
	}
	w.wait.Go(func() {
		limit := t.config.Verify.Limit
		if limit <= 0 {
			limit = 2 * time.Hour
		}
		c, cancel := context.WithTimeout(ctx, limit)
		defer cancel()
		err := w.repo.Check(c, t.config, v.ReadData)
		// Snapshot presence is independent of the check result. Even a failed
		// check may still allow metadata to expose a deleted recovery point.
		metadata, metadataErr := w.repo.Inspect(c, t.config)
		w.targetDone <- targetResult{index: index, kind: "verify", err: err, metadata: metadata, metadataErr: metadataErr}
	})
	return true
}
func (w *Worker) finishVerification(t *targetState, result targetResult, now time.Time) {
	v := *t.verifying
	t.verifying = nil
	err := result.err
	if err == nil {
		err = result.metadataErr
	}
	success := err == nil
	update := func(s *VerificationStatus, every time.Duration) {
		s.Result = "succeeded"
		s.Error = ""
		s.ExitCode = 0
		if success {
			s.LastSuccess = now
		} else {
			s.Result = "failed"
			s.Error, s.ExitCode = safeError(err)
		}
		// Manual failures and the single retry never move a regular deadline.
		if s.Scheduled && (success || (!v.Manual && !v.Retry)) {
			s.Next = now.Add(every)
		}
	}
	// A failed full read may have failed only while reading data; retain a
	// previous structure fault unless a structure/full check actually succeeds.
	if success || !v.ReadData {
		update(&t.status.Structure, t.config.Verify.StructureEvery)
	}
	if v.ReadData {
		update(&t.status.ReadData, t.config.Verify.ReadDataEvery)
		if success {
			t.status.ReadData.RetryAt = time.Time{}
		} else if !v.Manual && !v.Retry && t.status.ReadData.Scheduled {
			t.status.ReadData.RetryAt = now.Add(24 * time.Hour)
		}
		// Failed regular full reads still replace the simultaneously due structure
		// attempt; avoid an immediate duplicate repository check.
		if !success && !v.Manual && !v.Retry && t.status.Structure.Scheduled {
			t.status.Structure.Next = now.Add(t.config.Verify.StructureEvery)
		}
	}
	if result.metadataErr == nil {
		if result.metadata.ID != t.status.RepositoryID {
			w.bindRepository(t, result.metadata, now)
		}
		if t.status.SuccessID != "" && !result.metadata.Has(t.status.SuccessID) {
			t.status.SuccessID = ""
			t.status.Fingerprint = ""
			t.status.Error = "coverage_missing"
			t.status.ExitCode = 0
			t.refreshAfter = w.captureSeq + 1
			w.schedule.urgent = true
			w.schedule.reason("coverage_missing")
		}
		if t.status.CompleteID != "" && !result.metadata.Has(t.status.CompleteID) {
			t.status.CompleteID = ""
			t.status.CompleteAt = time.Time{}
			if complete := result.metadata.Latest(true); complete != nil {
				t.status.CompleteID = complete.ID
				t.status.CompleteAt = complete.Time
			}
		}
	}
	if v.Manual {
		outcome := OperationTarget{State: "verified"}
		if !success {
			outcome.State = "failed"
			outcome.Error, outcome.ExitCode = safeError(err)
		}
		w.targetOperation(v.Operation, t, outcome)
	}
}
