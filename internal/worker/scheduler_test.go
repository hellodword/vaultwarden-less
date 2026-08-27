package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

func TestSchedulerQuietMaximumMinimumFallbackAndRetry(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var state schedule
	state.demand(base)
	assertSchedule(t, &state, base.Add(30*time.Second), triggerDemand)

	state.demand(base.Add(20 * time.Second))
	assertSchedule(t, &state, base.Add(50*time.Second), triggerDemand)
	state.demand(base.Add(110 * time.Second))
	assertSchedule(t, &state, base.Add(2*time.Minute), triggerDemand)

	state.begin(base)
	state.demand(base.Add(time.Second))
	assertSchedule(t, &state, base.Add(5*time.Minute), triggerDemand)

	state.begin(base.Add(5 * time.Minute))
	state.complete(base.Add(5*time.Minute+10*time.Second), false)
	assertSchedule(t, &state, base.Add(12*time.Hour+5*time.Minute+10*time.Second), triggerFallback)

	state.begin(base.Add(13 * time.Hour))
	state.complete(base.Add(13*time.Hour+10*time.Second), true)
	assertSchedule(t, &state, base.Add(13*time.Hour+5*time.Minute+10*time.Second), triggerRetry)
}

func TestSchedulerDemandDuringCheckIsRetained(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	state := schedule{}
	state.demand(base)
	state.begin(base.Add(config.QuietWindow))
	state.demand(base.Add(config.QuietWindow + time.Second))
	state.complete(base.Add(config.QuietWindow+2*time.Second), false)
	assertSchedule(t, &state, base.Add(config.QuietWindow+config.MinimumCheckInterval), triggerDemand)
}

func TestWaitingPreservesPartialAndDegradedStatus(t *testing.T) {
	t.Parallel()
	next := time.Date(2026, 8, 27, 12, 5, 0, 0, time.UTC)
	worker := New(workerConfig("primary"), nil, nil, nil, discardLogger())
	for _, state := range []string{"partial", "degraded"} {
		worker.status.State = state
		worker.setWaiting("idle", &next)
		status := worker.Status()
		if status.State != state || status.NextCheckAt == nil || *status.NextCheckAt != formatTime(next) {
			t.Fatalf("waiting %s status = %#v", state, status)
		}
	}
	worker.status.State = "idle"
	worker.setWaiting("debouncing", &next)
	if status := worker.Status(); status.State != "debouncing" {
		t.Fatalf("ordinary waiting status = %#v", status)
	}
}

func assertSchedule(t *testing.T, state *schedule, want time.Time, wantKind triggerKind) {
	t.Helper()
	got, kind, ok := state.next()
	if !ok || !got.Equal(want) || kind != wantKind {
		t.Fatalf("next schedule = %s/%s/%t, want %s/%s/true", got, kind, ok, want, wantKind)
	}
}

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	created chan time.Duration
}

type fakeTimer struct {
	clock   *fakeClock
	due     time.Time
	channel chan time.Time
	stopped bool
	fired   bool
}

func newFakeClock(now time.Time) *fakeClock {
	return &fakeClock{now: now.UTC(), created: make(chan time.Duration, 32)}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) NewTimer(duration time.Duration) timer {
	clock.mu.Lock()
	timer := &fakeTimer{clock: clock, due: clock.now.Add(duration), channel: make(chan time.Time, 1)}
	clock.timers = append(clock.timers, timer)
	clock.mu.Unlock()
	clock.created <- duration
	return timer
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	now := clock.now
	ready := make([]*fakeTimer, 0)
	for _, timer := range clock.timers {
		if !timer.stopped && !timer.fired && !timer.due.After(now) {
			timer.fired = true
			ready = append(ready, timer)
		}
	}
	clock.mu.Unlock()
	for _, timer := range ready {
		timer.channel <- now
	}
}

func (timer *fakeTimer) C() <-chan time.Time { return timer.channel }

func (timer *fakeTimer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	if timer.stopped || timer.fired {
		return false
	}
	timer.stopped = true
	return true
}

type fixedSource struct {
	manifest snapshot.Manifest
	err      error
	calls    int
	called   chan struct{}
}

func (source *fixedSource) Build(context.Context) (snapshot.Manifest, error) {
	source.calls++
	if source.called != nil {
		select {
		case source.called <- struct{}{}:
		default:
		}
	}
	if source.err != nil {
		return snapshot.Manifest{}, source.err
	}
	return source.manifest, nil
}

type orderedRepository struct {
	mu      sync.Mutex
	order   []string
	results map[string]restic.SyncResult
	errors  map[string]error
	started chan struct{}
	release chan struct{}
}

func (repository *orderedRepository) Ensure(ctx context.Context, target config.Target, manifest snapshot.Manifest) (restic.SyncResult, error) {
	repository.mu.Lock()
	repository.order = append(repository.order, target.Name)
	repository.mu.Unlock()
	if repository.started != nil {
		select {
		case repository.started <- struct{}{}:
		default:
		}
	}
	if repository.release != nil {
		select {
		case <-repository.release:
		case <-ctx.Done():
			return restic.SyncResult{}, ctx.Err()
		}
	}
	if err := repository.errors[target.Name]; err != nil {
		return restic.SyncResult{}, err
	}
	result := repository.results[target.Name]
	if result.ContentID == "" {
		result.ContentID = manifest.ContentID
	}
	return result, nil
}

type eventCollector struct {
	mu     sync.Mutex
	events []notify.Event
}

func (collector *eventCollector) Enqueue(event notify.Event) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.events = append(collector.events, event)
}

func (collector *eventCollector) snapshot() []notify.Event {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return append([]notify.Event(nil), collector.events...)
}

func TestSchedulerRunRetainsDemandArrivingDuringCheck(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	manifest := workerManifest(t)
	source := &fixedSource{manifest: manifest, called: make(chan struct{}, 1)}
	repository := &orderedRepository{
		results: map[string]restic.SyncResult{"primary": syncResult('a', manifest.ContentID, true)},
		errors:  map[string]error{}, started: make(chan struct{}, 1), release: make(chan struct{}),
	}
	worker := newWithClock(workerConfig("primary"), source.Build, repository.Ensure, nil, discardLogger(), clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()

	if got := <-clock.created; got != config.QuietWindow {
		t.Fatalf("startup delay = %s", got)
	}
	clock.Advance(config.QuietWindow)
	<-repository.started
	worker.Request()
	worker.Request()
	close(repository.release)
	if got := <-clock.created; got != config.MinimumCheckInterval {
		t.Fatalf("retained demand delay = %s, want %s", got, config.MinimumCheckInterval)
	}
	status := worker.Status()
	if status.State != "debouncing" || status.LastCheck == nil || status.LastCheck.Result != "created" {
		t.Fatalf("status after first check = %#v", status)
	}
	cancel()
	<-done
}

func TestWorkerConsistencyFailureRetriesEveryThirtySeconds(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	manifest := workerManifest(t)
	source := &fixedSource{manifest: manifest, err: &snapshot.ConsistencyError{Path: "attachments/x/y", Reason: "not_found"}}
	repository := &orderedRepository{results: map[string]restic.SyncResult{"primary": syncResult('a', manifest.ContentID, false)}, errors: map[string]error{}}
	worker := newWithClock(workerConfig("primary"), source.Build, repository.Ensure, nil, discardLogger(), clock)
	done := make(chan checkOutcome, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- worker.check(ctx) }()
	if got := <-clock.created; got != config.ConsistencyRetry {
		t.Fatalf("consistency retry = %s", got)
	}
	source.err = nil
	clock.Advance(config.ConsistencyRetry)
	outcome := <-done
	if outcome.result != "unchanged" || outcome.retry || source.calls != 2 {
		t.Fatalf("outcome/calls = %#v/%d", outcome, source.calls)
	}
}

func TestWorkerTargetsAreSerialAndPartialResultRetries(t *testing.T) {
	t.Parallel()
	manifest := workerManifest(t)
	repository := &orderedRepository{
		results: map[string]restic.SyncResult{"primary": syncResult('a', manifest.ContentID, true)},
		errors:  map[string]error{"secondary": &restic.OperationError{Code: "backup_failed"}},
	}
	collector := &eventCollector{}
	source := &fixedSource{manifest: manifest}
	worker := New(workerConfig("primary", "secondary"), source.Build, repository.Ensure, collector.Enqueue, discardLogger())
	outcome := worker.check(context.Background())
	if outcome.result != "partial" || !outcome.retry || outcome.errorCode != "target_failed" {
		t.Fatalf("partial outcome = %#v", outcome)
	}
	if got := strings.Join(repository.order, ","); got != "primary,secondary" {
		t.Fatalf("target order = %s", got)
	}
	events := collector.snapshot()
	if len(events) != 1 || events[0].Type != "recovery_point_created" || len(events[0].Targets) != 2 || events[0].Targets[1].State != "failed" {
		t.Fatalf("recovery event = %#v", events)
	}
}

func TestWorkerReportsObservedSourceArchiveWhenTargetContentIsUnchanged(t *testing.T) {
	t.Parallel()
	manifest := workerManifest(t)
	targetArchive := "sha256:" + strings.Repeat("4", 64)
	result := syncResult('a', manifest.ContentID, false)
	result.ArchiveID = targetArchive
	repository := &orderedRepository{
		results: map[string]restic.SyncResult{"primary": result}, errors: map[string]error{},
	}
	worker := New(workerConfig("primary"), (&fixedSource{manifest: manifest}).Build, repository.Ensure, nil, discardLogger())
	outcome := worker.check(context.Background())
	if outcome.result != "unchanged" || outcome.archiveID != manifest.ArchiveID {
		t.Fatalf("unchanged outcome = %#v", outcome)
	}
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	worker.finishCheck(now, now, outcome)
	status := worker.Status()
	if status.LastCheck == nil || status.LastCheck.ArchiveID == nil || *status.LastCheck.ArchiveID != manifest.ArchiveID {
		t.Fatalf("last check = %#v", status.LastCheck)
	}
	if status.Targets[0].ArchiveID == nil || *status.Targets[0].ArchiveID != targetArchive {
		t.Fatalf("target status = %#v", status.Targets[0])
	}
}

func TestWorkerNotifiesWhenASecondTargetCatchesUpToKnownContent(t *testing.T) {
	t.Parallel()
	manifest := workerManifest(t)
	repository := &orderedRepository{
		results: map[string]restic.SyncResult{
			"primary":   syncResult('a', manifest.ContentID, true),
			"secondary": syncResult('b', manifest.ContentID, false),
		},
		errors: map[string]error{
			"secondary": &restic.OperationError{Code: "command_failed", ExitCode: 1},
		},
	}
	collector := &eventCollector{}
	source := &fixedSource{manifest: manifest}
	worker := New(workerConfig("primary", "secondary"), source.Build, repository.Ensure, collector.Enqueue, discardLogger())
	first := worker.check(context.Background())
	if first.result != "partial" {
		t.Fatalf("first outcome = %#v", first)
	}
	repository.results["primary"] = syncResult('a', manifest.ContentID, false)
	repository.results["secondary"] = syncResult('b', manifest.ContentID, true)
	delete(repository.errors, "secondary")
	second := worker.check(context.Background())
	if second.result != "created" {
		t.Fatalf("catch-up outcome = %#v", second)
	}
	events := collector.snapshot()
	if len(events) != 2 || events[0].ContentID != manifest.ContentID || events[1].ContentID != manifest.ContentID {
		t.Fatalf("catch-up recovery events = %#v", events)
	}
	if len(events[1].Targets) != 2 || events[1].Targets[0].State != "synced" || events[1].Targets[1].State != "created" {
		t.Fatalf("catch-up event target states = %#v", events[1].Targets)
	}
}

func TestWorkerDegradedTransitionsOccurOnceAndRecoverOnce(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	collector := &eventCollector{}
	source, repository := &fixedSource{}, &orderedRepository{}
	worker := New(workerConfig("primary"), source.Build, repository.Ensure, collector.Enqueue, discardLogger())
	worker.targetFailed(0, base, "backup_failed")
	worker.targetFailed(0, base.Add(config.DegradedAfter-time.Second), "backup_failed")
	if got := len(collector.snapshot()); got != 0 {
		t.Fatalf("events before degraded threshold = %d", got)
	}
	worker.targetFailed(0, base.Add(config.DegradedAfter), "backup_failed")
	worker.targetFailed(0, base.Add(config.DegradedAfter+time.Minute), "backup_failed")
	worker.targetSucceeded(0, base.Add(config.DegradedAfter+2*time.Minute), syncResult('a', strings.Repeat("b", 71), false))
	worker.targetSucceeded(0, base.Add(config.DegradedAfter+3*time.Minute), syncResult('a', strings.Repeat("b", 71), false))
	events := collector.snapshot()
	if len(events) != 2 || events[0].Type != "target_degraded" || events[1].Type != "target_recovered" {
		t.Fatalf("transition events = %#v", events)
	}
}

func TestWorkerInitialStatusHasNullHistoryAndArrayTargets(t *testing.T) {
	t.Parallel()
	source, repository := &fixedSource{}, &orderedRepository{}
	worker := New(workerConfig("primary"), source.Build, repository.Ensure, nil, discardLogger())
	status := worker.Status()
	if status.State != "idle" || status.NextCheckAt != nil || status.LastCheck != nil || status.Targets == nil || len(status.Targets) != 1 {
		t.Fatalf("initial status = %#v", status)
	}
	if status.Targets[0].State != "unknown" || status.Targets[0].LastAttemptAt != nil || status.Targets[0].SnapshotID != nil {
		t.Fatalf("initial target = %#v", status.Targets[0])
	}
}

func TestWorkerHTTPContract(t *testing.T) {
	t.Parallel()
	source, repository := &fixedSource{}, &orderedRepository{}
	worker := New(workerConfig("primary"), source.Build, repository.Ensure, nil, discardLogger())
	handler := Handler(worker)

	assertHTTP(t, handler, http.MethodGet, HealthPath, "", "", http.StatusNoContent, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", `{"schema_version":1}`, http.StatusNoContent, "")
	assertHTTP(t, handler, http.MethodPost, CheckPath, "application/json", `{"schema_version":1}`, http.StatusAccepted, "{\"schema_version\":1,\"accepted\":true}\n")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", `{"schema_version":1,"extra":true}`, http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", `{"schema_version":1,"schema_version":1}`, http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", `{"schema_version":"1"}`, http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", `{"schema_version":2}`, http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "", `{"schema_version":1}`, http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json; charset=utf-8", `{"schema_version":1}`, http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", strings.Repeat(" ", config.EventMaxBytes+1), http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodPost, EventsPath, "application/json", `{"schema_version":1}`+strings.Repeat(" ", config.EventMaxBytes), http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodGet, HealthPath, "", "unexpected", http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodGet, StatusPath, "", "unexpected", http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodGet, EventsPath, "", "", http.StatusMethodNotAllowed, "")
	assertHTTP(t, handler, http.MethodGet, StatusPath+"?extra=1", "", "", http.StatusBadRequest, "")
	assertHTTP(t, handler, http.MethodGet, "/missing", "", "", http.StatusNotFound, "")
}

func assertHTTP(t *testing.T, handler http.Handler, method, path, contentType, body string, wantStatus int, wantBody string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status = %d, body = %s", method, path, response.Code, response.Body.String())
	}
	if wantBody != "" && response.Body.String() != wantBody {
		t.Fatalf("%s %s body = %q, want %q", method, path, response.Body.String(), wantBody)
	}
	if wantStatus >= 400 {
		var document struct {
			SchemaVersion int `json:"schema_version"`
			Error         struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil || document.SchemaVersion != 1 || document.Error.Code == "" || document.Error.Message == "" {
			t.Fatalf("%s %s error envelope = %#v/%v", method, path, document, err)
		}
	}
}

func workerManifest(t *testing.T) snapshot.Manifest {
	t.Helper()
	manifest, err := snapshot.NewManifest(
		strings.Repeat("1", 40),
		"sha256:"+strings.Repeat("2", 64),
		"sha256:"+strings.Repeat("3", 64),
		[]snapshot.File{{Path: "db.sqlite3", Size: 1, SHA256: "sha256:" + strings.Repeat("0", 64)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func workerConfig(names ...string) *config.Backup {
	targets := make([]config.Target, 0, len(names))
	for _, name := range names {
		targets = append(targets, config.Target{Name: name})
	}
	return &config.Backup{SchemaVersion: config.SchemaVersion, Targets: targets}
}

func syncResult(character byte, contentID string, created bool) restic.SyncResult {
	return restic.SyncResult{
		SnapshotID: strings.Repeat(string(character), 64), ContentID: contentID,
		ArchiveID: "sha256:" + strings.Repeat("3", 64), Created: created,
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }
