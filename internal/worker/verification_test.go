package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/state"
)

func checkCount(r *fakeRepo, name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.checks[name])
}
func TestVerificationOptInAndSerialization(t *testing.T) {
	w, repo, source, _ := setupWorker(t, "one", "two", "manual")
	for _, target := range w.targets[:2] {
		target.config.Verify = config.Verification{StructureEvery: 24 * time.Hour, ReadDataEvery: 720 * time.Hour}
	}
	block := make(chan struct{})
	repo.checkBlocks = map[string]<-chan struct{}{"one": block, "two": block}
	runWorker(t, w)
	waitFor(t, func() bool { return checkCount(repo, "one")+checkCount(repo, "two") == 1 })
	s := w.Status()
	active := 0
	other := 1
	if s.Targets[1].Operation == "verify_read_data" {
		active, other = 1, 0
	}
	source.set("new", "new")
	op, err := w.Backup(false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, n := repo.count(w.targets[other].config.Name); return n == 2 })
	if _, n := repo.count(w.targets[active].config.Name); n != 1 {
		t.Fatal("upload overlapped same target verification")
	}
	if checkCount(repo, "manual") != 0 {
		t.Fatal("implicit verification schedule")
	}
	close(block)
	waitFor(t, func() bool {
		result, _ := w.Operation(op.ID)
		return result.Done() && checkCount(repo, "one") == 1 && checkCount(repo, "two") == 1 && settled(w)
	})
	if op, err = w.Verify("manual", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { result, _ := w.Operation(op.ID); return result.Done() })
	if checkCount(repo, "manual") != 1 {
		t.Fatal("manual verification unavailable")
	}
}
func TestFullReadRetryRegularAndManualDeadlines(t *testing.T) {
	w, _, _, clock := setupWorker(t, "one")
	target := w.targets[0]
	target.config.Verify = config.Verification{StructureEvery: 24 * time.Hour, ReadDataEvery: 720 * time.Hour}
	target.status = TargetStatus{Name: "one", RepositoryID: strings.Repeat("a", 64), SuccessID: strings.Repeat("b", 64)}
	w.enableVerification(target, clock.Now())
	fail := targetResult{err: errors.New("failure"), metadataErr: errors.New("unavailable")}
	target.verifying = &verification{ReadData: true}
	w.finishVerification(target, fail, clock.Now())
	regular := clock.Now().Add(720 * time.Hour)
	if !target.status.ReadData.Next.Equal(regular) || !target.status.ReadData.RetryAt.Equal(clock.Now().Add(24*time.Hour)) {
		t.Fatal("initial failure deadlines")
	}
	clock.Advance(24 * time.Hour)
	due := w.dueVerification(target, clock.Now())
	if due == nil || !due.Retry || !due.ReadData {
		t.Fatal("single full retry missing")
	}
	target.verifying = due
	target.status.ReadData.RetryAt = time.Time{}
	w.finishVerification(target, fail, clock.Now())
	if !target.status.ReadData.RetryAt.IsZero() || !target.status.ReadData.Next.Equal(regular) {
		t.Fatal("retry generated another retry or postponed regular period")
	}
	// Manual failure never opens an automatic retry round.
	op, _ := w.Verify("one", true)
	target.verifying = &verification{ReadData: true, Manual: true, Operation: op.ID}
	w.finishVerification(target, fail, clock.Now())
	if !target.status.ReadData.RetryAt.IsZero() || !target.status.ReadData.Next.Equal(regular) {
		t.Fatal("manual failure changed schedule")
	}
	metadata := restic.Metadata{ID: target.status.RepositoryID, Snapshots: []restic.Snapshot{{ID: target.status.SuccessID}}}
	target.verifying = &verification{ReadData: false, Manual: true, Operation: op.ID}
	w.finishVerification(target, targetResult{metadata: metadata}, clock.Now())
	if target.status.ReadData.Error == "" {
		t.Fatal("structure success cleared full-read fault")
	}
	target.verifying = &verification{ReadData: true, Manual: true, Operation: op.ID}
	w.finishVerification(target, targetResult{metadata: metadata}, clock.Now())
	if target.status.ReadData.Error != "" || !target.status.ReadData.Next.Equal(clock.Now().Add(720*time.Hour)) {
		t.Fatal("successful full read did not reset period")
	}
}
func TestMissingSnapshotSchedulesCaptureAndPreservesVerificationFault(t *testing.T) {
	w, repo, source, _ := setupWorker(t, "one")
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	before := w.Status().Targets[0].SuccessID
	repo.mu.Lock()
	repo.snapshots["one"] = nil
	repo.checkFail = map[string]bool{"one": true}
	repo.mu.Unlock()
	op, _ := w.Verify("one", true)
	waitFor(t, func() bool {
		result, _ := w.Operation(op.ID)
		return result.Done() && settled(w) && w.Status().Targets[0].SuccessID != before
	})
	if source.count() != 2 || w.Status().Targets[0].ReadData.Error == "" {
		t.Fatal("coverage loss or check fault lost")
	}
	source.set("later", "later")
	w.Request()
	waitFor(t, func() bool { return source.count() == 3 && settled(w) })
	if w.Status().Targets[0].ReadData.Error == "" {
		t.Fatal("upload cleared full-read failure")
	}
	repo.mu.Lock()
	repo.checkFail["one"] = false
	repo.mu.Unlock()
	op, _ = w.Verify("one", true)
	waitFor(t, func() bool { result, _ := w.Operation(op.ID); return result.Done() })
	if w.Status().Targets[0].ReadData.Error != "" {
		t.Fatal("full read did not recover")
	}
}
func TestStateSurvivesCacheLossAndBindsRepositoryIdentity(t *testing.T) {
	w, repo, source, clock := setupWorker(t, "one")
	store := state.Store{Directory: t.TempDir()}
	if err := w.Restore(store); err != nil {
		t.Fatal(err)
	}
	w.targets[0].config.Verify = config.Verification{ReadDataEvery: 720 * time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	waitFor(t, func() bool { return settled(w) && w.Status().Targets[0].ReadData.Result == "succeeded" })
	next := w.Status().Targets[0].ReadData.Next
	id := w.Status().Targets[0].SuccessID
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// This is only test-owned rebuildable cache, not persistent state.
	if err := os.RemoveAll(w.cache); err != nil {
		t.Fatal(err)
	}
	w2 := New(&config.Backup{Targets: []config.Target{w.targets[0].config}}, capture.Builder{CacheDir: w.cache}, repo, nil, nil)
	w2.clock = clock
	w2.build = source.build
	if err := w2.Restore(store); err != nil {
		t.Fatal(err)
	}
	runWorker(t, w2)
	waitFor(t, func() bool { return settled(w2) })
	if got := w2.Status().Targets[0]; got.SuccessID != id || !got.ReadData.Next.Equal(next) || checkCount(repo, "one") != 1 {
		t.Fatal("cache loss reset baseline or verification period")
	}
	w2.mu.Lock()
	w2.bindRepository(w2.targets[0], restic.Metadata{ID: strings.Repeat("f", 64)}, clock.Now())
	got := w2.targets[0].status
	w2.mu.Unlock()
	if !got.ReadData.LastSuccess.IsZero() || got.SuccessID != "" {
		t.Fatal("new repository inherited recovery evidence")
	}
}
func TestForceBypassesOnceWithoutResettingFailureCount(t *testing.T) {
	w, repo, source, clock := setupWorker(t, "one")
	repo.fail["one"] = true
	runWorker(t, w)
	waitFor(t, func() bool { return w.Status().Targets[0].Failures == 1 })
	normal, _ := w.Backup(false)
	waitFor(t, func() bool { op, _ := w.Operation(normal.ID); return op.Done() })
	op, _ := w.Operation(normal.ID)
	if op.Targets[0].State != "deferred" || op.State != "failed" {
		t.Fatal("backoff not reported")
	}
	force, _ := w.Backup(true)
	waitFor(t, func() bool { op, _ := w.Operation(force.ID); return op.Done() })
	s := w.Status().Targets[0]
	if s.Failures != 2 || !s.RetryAt.Equal(clock.Now().Add(10*time.Minute)) {
		t.Fatal("force reset backoff")
	}
	repo.mu.Lock()
	repo.fail["one"] = false
	repo.mu.Unlock()
	source.set("fresh-without-event", "fresh-without-event")
	clock.Advance(10 * time.Minute)
	w.signal()
	waitFor(t, func() bool { return settled(w) })
	if got := w.Status().Fingerprint; got == "" || w.targets[0].status.Fingerprint != got {
		t.Fatal("retry used old waiting payload")
	}
	entries, err := os.ReadDir(filepath.Join(w.cache, "payloads"))
	if err != nil || len(entries) != 1 {
		t.Fatal("unused staging retained")
	}
}
