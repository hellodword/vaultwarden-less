package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/routes"
	"github.com/hellodword/vaultwarden-less/internal/testutil"
)

func TestScheduleQuietMaximumAndIndependentTimers(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := schedule{}
	s.start(now)
	if got := s.take(); got.Reasons["startup"] != 1 {
		t.Fatal("startup must capture")
	}
	s.event(now, routes.Summary{})
	if due := s.sourceDue(now); !due.Equal(now.Add(30 * time.Second)) {
		t.Fatal("quiet window")
	}
	for i := 1; i <= 7; i++ {
		s.event(now.Add(time.Duration(i)*15*time.Second), routes.Summary{})
	}
	if due := s.sourceDue(now); !due.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("maximum wait")
	}
	s.take()
	s.complete(now.Add(2*time.Minute), false)
	if !s.nextOrdinary.Equal(now.Add(6 * time.Hour)) {
		t.Fatal("traffic reset independent timers")
	}
	s.tick(now.Add(6 * time.Hour))
	if got := s.take(); got.Reasons["scheduled"] != 1 {
		t.Fatal("ordinary timer lost")
	}
	s.complete(now, true)
	if !s.retryAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatal("source retry")
	}
	s.event(now, routes.Summary{})
	s.manual()
	if !s.sourceDue(now).Equal(now) {
		t.Fatal("manual capture waited")
	}
	for i, want := range []time.Duration{5, 10, 20, 40, 60, 60, 60} {
		if targetBackoff(i+1) != want*time.Minute {
			t.Fatal("target backoff")
		}
	}
}

type fakeRepo struct {
	mu            sync.Mutex
	latest        map[string]int
	uploads       map[string]int
	fail          map[string]bool
	blocks        map[string]<-chan struct{}
	initialBlocks map[string]<-chan struct{}
	tags          map[string][]string
	snapshots     map[string][]restic.Snapshot
	checks        map[string][]bool
	checkFail     map[string]bool
	checkBlocks   map[string]<-chan struct{}
}

func (r *fakeRepo) Inspect(ctx context.Context, t config.Target) (restic.Metadata, error) {
	r.mu.Lock()
	r.latest[t.Name]++
	block := r.initialBlocks[t.Name]
	r.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return restic.Metadata{}, ctx.Err()
		case <-block:
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	digest := sha256.Sum256([]byte(t.Name))
	return restic.Metadata{ID: hex.EncodeToString(digest[:]), Snapshots: append([]restic.Snapshot(nil), r.snapshots[t.Name]...)}, nil
}
func (r *fakeRepo) Backup(ctx context.Context, t config.Target, path, tag, fingerprint string) (string, error) {
	r.mu.Lock()
	r.uploads[t.Name]++
	if r.tags == nil {
		r.tags = map[string][]string{}
	}
	r.tags[t.Name] = append(r.tags[t.Name], tag)
	fail := r.fail[t.Name]
	block := r.blocks[t.Name]
	r.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-block:
		}
	}
	if fail {
		return "", errors.New("synthetic repository failure")
	}
	digest := sha256.Sum256([]byte(path + tag + t.Name))
	id := hex.EncodeToString(digest[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snapshots == nil {
		r.snapshots = map[string][]restic.Snapshot{}
	}
	r.snapshots[t.Name] = append(r.snapshots[t.Name], restic.Snapshot{ID: id, Time: time.Now().UTC(), Tags: []string{tag, restic.FingerprintPrefix + fingerprint}, Hostname: restic.SnapshotHost})
	return id, nil
}
func (r *fakeRepo) Check(ctx context.Context, t config.Target, readData bool) error {
	r.mu.Lock()
	if r.checks == nil {
		r.checks = map[string][]bool{}
	}
	r.checks[t.Name] = append(r.checks[t.Name], readData)
	block := r.checkBlocks[t.Name]
	fail := r.checkFail[t.Name]
	r.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-block:
		}
	}
	if fail {
		return errors.New("synthetic check failure")
	}
	return nil
}
func (r *fakeRepo) count(name string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest[name], r.uploads[name]
}

type fakeSource struct {
	mu             sync.Mutex
	business, full string
	calls          int
	reads          []bool
	fail           bool
	issues         map[string]int
	cache          string
}

func (s *fakeSource) build(_ context.Context, _ *capture.Payload, readAll bool) (*capture.Payload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.reads = append(s.reads, readAll)
	if s.fail {
		return nil, errors.New("synthetic source failure")
	}
	if err := os.MkdirAll(filepath.Join(s.cache, "payloads"), 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(s.cache, "payloads"), "payload-")
	if err != nil {
		return nil, err
	}
	return &capture.Payload{Directory: dir, Database: capture.Digest{Business: s.business, Full: s.full}, Reliable: true, Files: map[string]capture.Entry{}, Issues: maps.Clone(s.issues)}, nil
}
func (s *fakeSource) set(business, full string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.business = business
	s.full = full
}
func (s *fakeSource) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func setupWorker(t *testing.T, names ...string) (*Worker, *fakeRepo, *fakeSource, *testutil.Clock) {
	t.Helper()
	cache := t.TempDir()
	cfg := &config.Backup{}
	for _, name := range names {
		cfg.Targets = append(cfg.Targets, config.Target{Name: name})
	}
	repo := &fakeRepo{latest: map[string]int{}, uploads: map[string]int{}, fail: map[string]bool{}, blocks: map[string]<-chan struct{}{}}
	source := &fakeSource{cache: cache, business: "a", full: "a"}
	clock := testutil.NewClock()
	w := New(cfg, capture.Builder{CacheDir: cache}, repo, nil, nil)
	w.build = source.build
	w.clock = clock
	return w, repo, source, clock
}
func runWorker(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}
func settled(w *Worker) bool {
	s := w.Status()
	if s.Checking || s.LastCheck.IsZero() {
		return false
	}
	for _, t := range s.Targets {
		if t.Uploading || t.Waiting || t.SuccessID == "" {
			return false
		}
	}
	return true
}

func TestUnifiedComparisonAndForcedBackup(t *testing.T) {
	w, repo, source, clock := setupWorker(t, "one")
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	source.set("a", "auth-changed")
	events := routes.Summary{}
	events.Record("POST", "/identity/connect/token", true)
	events.Record("POST", "/identity/connect/token", false)
	w.Events(events)
	clock.Advance(30 * time.Second)
	w.signal()
	waitFor(t, func() bool { return source.count() == 2 && settled(w) })
	queries, uploads := repo.count("one")
	if queries != 1 || uploads != 1 {
		t.Fatalf("no-change contacted storage: %d %d", queries, uploads)
	}
	op, err := w.Backup(true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { result, _ := w.Operation(op.ID); return result.Done() && settled(w) })
	if result, _ := w.Operation(op.ID); result.State != "succeeded" {
		t.Fatalf("force: %+v", result)
	}
	w.Request()
	waitFor(t, func() bool { return source.count() == 4 && settled(w) })
	_, uploads = repo.count("one")
	if uploads != 2 {
		t.Fatal("ordinary backup repeated unchanged content")
	}
}

type recordedNotifications struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *recordedNotifications) Enqueue(event notify.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, event)
	return nil
}
func (n *recordedNotifications) Status() []notify.Status { return nil }
func (n *recordedNotifications) summary() notify.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	var summary notify.Event
	for _, event := range n.events {
		summary.Merge(event)
	}
	return summary
}

func TestHealthyChecksOnlyEnterDaily(t *testing.T) {
	w, _, source, clock := setupWorker(t, "one")
	reports := &recordedNotifications{}
	w.notifications = reports
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	w.Request()
	waitFor(t, func() bool { return source.count() == 2 && settled(w) })
	if reports.summary().Count != 0 {
		t.Fatal("healthy activity sent a notification")
	}
	clock.Advance(21 * time.Hour)
	w.signal()
	waitFor(t, func() bool { return reports.summary().Count == 1 })
	if reports.summary().Kind != "Daily" || reports.summary().Checks < 2 {
		t.Fatal("daily checks missing")
	}
}

func TestIncompleteCaptureUploadsAndRetriesAfterFiveMinutes(t *testing.T) {
	w, repo, source, clock := setupWorker(t, "one")
	source.issues = map[string]int{"capture_timeout": 1}
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	status := w.Status()
	if status.Source != "incomplete" || !status.SourceRetryAt.Equal(clock.Now().Add(5*time.Minute)) {
		t.Fatal("timeout payload did not schedule source retry")
	}
	source.mu.Lock()
	source.issues = nil
	source.mu.Unlock()
	clock.Advance(5*time.Minute - time.Second)
	w.signal()
	if source.count() != 1 {
		t.Fatal("source retried early")
	}
	clock.Advance(time.Second)
	w.signal()
	waitFor(t, func() bool { return source.count() == 2 && settled(w) })
	status = w.Status()
	if status.Source != "complete" || !status.SourceRetryAt.IsZero() {
		t.Fatal("successful source retry did not clear incomplete state")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if tags := repo.tags["one"]; len(tags) != 2 || tags[0] != "incomplete" || tags[1] != "complete" {
		t.Fatalf("wrong native upload tags: %v", tags)
	}
}

func TestStatusOmitsUnsetTimes(t *testing.T) {
	w, _, _, _ := setupWorker(t, "one")
	data, err := json.Marshal(w.Status())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"success_at", "retry_at", "last_check", "next_ordinary", "next_full", "source_retry_at"} {
		if strings.Contains(string(data), `"`+field+`"`) {
			t.Fatalf("unset time %s present in status: %s", field, data)
		}
	}
	now := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	w.status.LastCheck = now
	w.schedule.nextOrdinary, w.schedule.retryAt = now, now
	w.targets[0].status.SuccessAt, w.targets[0].status.RetryAt = now, now
	data, err = json.Marshal(w.Status())
	var decoded Status
	if err != nil || json.Unmarshal(data, &decoded) != nil || !decoded.LastCheck.Equal(now) || !decoded.NextOrdinary.Equal(now) || !decoded.SourceRetryAt.Equal(now) || !decoded.Targets[0].SuccessAt.Equal(now) || !decoded.Targets[0].RetryAt.Equal(now) {
		t.Fatalf("populated status timestamps lost: %s (%v)", data, err)
	}
}

func TestIndependentTargetsLatestReplacementAndBackoff(t *testing.T) {
	w, repo, source, clock := setupWorker(t, "good", "bad")
	repo.fail["bad"] = true
	runWorker(t, w)
	waitFor(t, func() bool { s := w.Status(); return s.Targets[0].SuccessID != "" && s.Targets[1].State == "failed" })
	retry := w.Status().Targets[1].RetryAt
	for _, value := range []string{"b", "c"} {
		source.set(value, value)
		want := source.count() + 1
		w.Request()
		waitFor(t, func() bool {
			return source.count() >= want && !w.Status().Targets[0].Uploading && !w.Status().Targets[0].Waiting
		})
	}
	if !w.Status().Targets[1].RetryAt.Equal(retry) {
		t.Fatal("new request reset failed target backoff")
	}
	if _, n := repo.count("bad"); n != 1 {
		t.Fatal("failed target retried early")
	}
	repo.mu.Lock()
	repo.fail["bad"] = false
	repo.mu.Unlock()
	clock.Advance(5 * time.Minute)
	w.signal()
	waitFor(t, func() bool { return settled(w) })
	if _, n := repo.count("good"); n != 3 {
		t.Fatalf("healthy target blocked: %d", n)
	}
	if _, n := repo.count("bad"); n != 2 {
		t.Fatalf("queued every intermediate version: %d", n)
	}
	entries, err := os.ReadDir(filepath.Join(w.cache, "payloads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2*len(w.targets)+2 {
		t.Fatal("unbounded retained payloads")
	}
}

func TestTriggersDuringCaptureAndFailureRecovery(t *testing.T) {
	w, _, source, _ := setupWorker(t, "one")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	original := w.build
	w.build = func(ctx context.Context, previous *capture.Payload, readAll bool) (*capture.Payload, error) {
		once.Do(func() {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		return original(ctx, previous, readAll)
	}
	runWorker(t, w)
	<-started
	w.Request()
	close(release)
	waitFor(t, func() bool { return source.count() == 2 && settled(w) })
	source.mu.Lock()
	source.fail = true
	source.mu.Unlock()
	w.Request()
	waitFor(t, func() bool { return w.Status().LastResult == "capture_failed" })
	if w.Status().SourceRetryAt.IsZero() {
		t.Fatal("failed capture lost retry")
	}
	source.mu.Lock()
	source.fail = false
	source.mu.Unlock()
	w.Request()
	waitFor(t, func() bool { return source.count() == 4 && settled(w) })
	if !w.Status().SourceRetryAt.IsZero() {
		t.Fatal("recovery did not clear source retry")
	}
}

func TestRevertDuringUploadIsComparedAgain(t *testing.T) {
	w, repo, source, _ := setupWorker(t, "one")
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	blocked := make(chan struct{})
	repo.mu.Lock()
	repo.blocks["one"] = blocked
	repo.mu.Unlock()
	source.set("b", "b")
	w.Request()
	waitFor(t, func() bool { return w.Status().Targets[0].Uploading })
	source.set("a", "a")
	w.Request()
	waitFor(t, func() bool { return source.count() == 3 && !w.Status().Checking })
	close(blocked)
	waitFor(t, func() bool { _, n := repo.count("one"); return n == 3 && settled(w) })
}

func TestTargetlessDoesNotCapture(t *testing.T) {
	w, _, source, _ := setupWorker(t)
	runWorker(t, w)
	waitFor(t, func() bool { return w.Status().LastResult == "unconfigured" })
	if source.count() != 0 || w.Status().Configured {
		t.Fatal("targetless deployment captured source")
	}
}

func TestSlowStartupTargetDoesNotRetainEveryCapture(t *testing.T) {
	w, repo, source, _ := setupWorker(t, "good", "slow")
	block := make(chan struct{})
	repo.initialBlocks = map[string]<-chan struct{}{"slow": block}
	runWorker(t, w)
	waitFor(t, func() bool { return w.Status().Targets[0].SuccessID != "" })
	for index := 0; index < 10; index++ {
		value := time.Duration(index).String()
		source.set(value, value)
		want := source.count() + 1
		w.Request()
		waitFor(t, func() bool {
			s := w.Status()
			return source.count() >= want && !s.Checking && !s.Targets[0].Waiting && !s.Targets[0].Uploading
		})
	}
	entries, err := os.ReadDir(filepath.Join(w.cache, "payloads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 2*len(w.targets)+2 {
		t.Fatalf("slow startup retained %d payloads", len(entries))
	}
	close(block)
	waitFor(t, func() bool { return settled(w) })
}
