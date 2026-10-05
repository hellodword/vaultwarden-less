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
	if got := s.take(); !got.Full || !got.ReadAll || got.Reasons["startup"] != 1 {
		t.Fatal("startup must be full")
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
	s.complete(now.Add(2*time.Minute), false, true, false)
	if !s.nextOrdinary.Equal(now.Add(6*time.Hour)) || !s.nextFull.Equal(now.Add(7*24*time.Hour)) {
		t.Fatal("traffic reset independent timers")
	}
	s.tick(now.Add(6 * time.Hour))
	if got := s.take(); !got.ReadAll || got.Full {
		t.Fatal("ordinary scheduled read scope")
	}
	s.tick(now.Add(7 * 24 * time.Hour))
	if got := s.take(); !got.Full || !got.ReadAll {
		t.Fatal("full timer lost")
	}
	s.complete(now, true, false, true)
	s.manual(false)
	if got := s.take(); !got.Full || !got.ReadAll {
		t.Fatal("failed full requirement consumed")
	}
	s.complete(now, true, true, true)
	if s.retryFull || !s.retryAt.Equal(now.Add(5*time.Minute)) {
		t.Fatal("incomplete file capture retry policy")
	}
	s.event(now, routes.Summary{})
	s.manual(true)
	if !s.sourceDue(now).Equal(now) {
		t.Fatal("manual check waited for quiet window")
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
}

func (r *fakeRepo) Latest(ctx context.Context, t config.Target) (*restic.Snapshot, error) {
	r.mu.Lock()
	r.latest[t.Name]++
	block := r.initialBlocks[t.Name]
	r.mu.Unlock()
	if block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
		}
	}
	return nil, nil
}
func (r *fakeRepo) Backup(ctx context.Context, t config.Target, path, tag string) (string, error) {
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
	return hex.EncodeToString(digest[:]), nil
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

func TestNoChangeAuthenticationAndFullCheck(t *testing.T) {
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
	w.Request(true)
	waitFor(t, func() bool { _, uploads := repo.count("one"); return uploads == 2 && settled(w) })
	w.Request(true)
	waitFor(t, func() bool { return source.count() == 4 && settled(w) })
	_, uploads = repo.count("one")
	if uploads != 2 {
		t.Fatal("full check forced unchanged upload")
	}
}

type recordedNotifications struct {
	mu     sync.Mutex
	events []notify.Event
}

func (n *recordedNotifications) Enqueue(event notify.Event) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, event)
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

func TestReportsPreserveRequestPhasesAcrossChecks(t *testing.T) {
	w, _, source, clock := setupWorker(t, "one")
	reports := &recordedNotifications{}
	w.notifications = reports
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	events := routes.Summary{}
	events.Record("POST", "/api/ciphers", true)
	events.Record("POST", "/api/ciphers", false)
	events.Record("GET", "/private-unknown-request", true)
	w.Events(events)
	clock.Advance(30 * time.Second)
	w.signal()
	waitFor(t, func() bool { return source.count() == 2 && settled(w) && reports.summary().Checks == 2 })
	summary := reports.summary()
	if summary.Routes["POST /api/ciphers"] != (routes.Counts{Started: 1, Finished: 1}) || summary.Unknown["GET"] != (routes.Counts{Started: 1}) {
		t.Fatal("worker double-counted a completed request or invented its finish")
	}
	events = routes.Summary{}
	events.Record("GET", "/private-unknown-request", false)
	w.Events(events)
	clock.Advance(30 * time.Second)
	w.signal()
	waitFor(t, func() bool { return source.count() == 3 && settled(w) && reports.summary().Checks == 3 })
	summary = reports.summary()
	if summary.Unknown["GET"] != (routes.Counts{Started: 1, Finished: 1}) || strings.Contains(summary.Message(4096), "private-unknown-request") {
		t.Fatal("cross-check request phases lost or raw request path disclosed")
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
	w.schedule.nextOrdinary, w.schedule.nextFull, w.schedule.retryAt = now, now, now
	w.targets[0].status.SuccessAt, w.targets[0].status.RetryAt = now, now
	data, err = json.Marshal(w.Status())
	var decoded Status
	if err != nil || json.Unmarshal(data, &decoded) != nil || !decoded.LastCheck.Equal(now) || !decoded.NextOrdinary.Equal(now) || !decoded.NextFull.Equal(now) || !decoded.SourceRetryAt.Equal(now) || !decoded.Targets[0].SuccessAt.Equal(now) || !decoded.Targets[0].RetryAt.Equal(now) {
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
		w.Request(false)
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

func TestTriggersDuringCaptureAndFailedFullRequirement(t *testing.T) {
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
	w.Request(true)
	close(release)
	waitFor(t, func() bool { return source.count() == 2 && settled(w) })
	source.mu.Lock()
	source.fail = true
	source.mu.Unlock()
	w.Request(true)
	waitFor(t, func() bool { return w.Status().LastResult == "capture_failed" })
	if !w.Status().FullPending {
		t.Fatal("failed full check lost its requirement")
	}
	source.mu.Lock()
	source.fail = false
	source.mu.Unlock()
	w.Request(false)
	waitFor(t, func() bool { return source.count() == 4 && settled(w) })
	if w.Status().Mode != "full" || w.Status().FullPending {
		t.Fatal("retry did not complete full comparison")
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
	w.Request(false)
	waitFor(t, func() bool { return w.Status().Targets[0].Uploading })
	source.set("a", "a")
	w.Request(false)
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
		w.Request(false)
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
