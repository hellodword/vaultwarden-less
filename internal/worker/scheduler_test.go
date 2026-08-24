package worker

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/state"
)

func schedulerConfig() *config.Backup {
	return &config.Backup{
		Debounce: config.Duration{Duration: 30 * time.Second}, MaxWait: config.Duration{Duration: 2 * time.Minute},
		MinInterval: config.Duration{Duration: 5 * time.Minute}, SampleInterval: config.Duration{Duration: 15 * time.Minute},
		EventMaxBytes: 1024, EventRatePerSecond: 10, EventBurst: 20, ListenAddress: "127.0.0.1:9090",
	}
}

func TestSchedulerDebounceMaxWaitMinimumAndPeriodicSample(t *testing.T) {
	t.Parallel()
	cfg := schedulerConfig()
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if got := nextSnapshotAt(state.State{}, cfg, base); !got.Equal(base) {
		t.Fatalf("startup = %s", got)
	}
	current := state.State{
		Dirty: true, FirstDirtyAt: state.FormatTime(base), LastDirtyAt: state.FormatTime(base.Add(110 * time.Second)),
		LastSampleAt: state.FormatTime(base),
	}
	if got, want := nextSnapshotAt(current, cfg, base), base.Add(2*time.Minute); !got.Equal(want) {
		t.Fatalf("max wait = %s, want %s", got, want)
	}
	current.FirstDirtyAt = state.FormatTime(base.Add(4 * time.Minute))
	current.LastDirtyAt = current.FirstDirtyAt
	current.LastSnapshotAt = state.FormatTime(base)
	if got, want := nextSnapshotAt(current, cfg, base), base.Add(5*time.Minute); !got.Equal(want) {
		t.Fatalf("minimum interval = %s, want %s", got, want)
	}
	current.Dirty = false
	if got, want := nextSnapshotAt(current, cfg, base), base.Add(15*time.Minute); !got.Equal(want) {
		t.Fatalf("periodic sample = %s, want %s", got, want)
	}
}

func TestBackoffIsBoundedAndJitteredWithinContract(t *testing.T) {
	t.Parallel()
	for failures := 1; failures <= 20; failures++ {
		delay := backoff(failures)
		if delay < time.Minute || delay > 15*time.Minute {
			t.Fatalf("failures %d delay %s outside contract", failures, delay)
		}
	}
}

func TestMarkDirtyRefreshesDebounceButCoalescesDuringBackup(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		cfg:   &config.Backup{Targets: []config.Target{{Name: "primary"}}},
		store: store, wake: make(chan struct{}, 1),
	}
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if err := coordinator.MarkDirty(base); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.MarkDirty(base.Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	current := store.Snapshot()
	if current.DirtyGeneration != 1 || current.FirstDirtyAt != state.FormatTime(base) || current.LastDirtyAt != state.FormatTime(base.Add(20*time.Second)) {
		t.Fatalf("waiting dirty state = %#v", current)
	}
	if err := store.Update(func(next *state.State) error {
		next.BackupInProgress = true
		next.RunningGeneration = next.DirtyGeneration
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.MarkDirty(base.Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.MarkDirty(base.Add(40 * time.Second)); err != nil {
		t.Fatal(err)
	}
	current = store.Snapshot()
	if current.DirtyGeneration != 2 || current.LastDirtyAt != state.FormatTime(base.Add(30*time.Second)) {
		t.Fatalf("in-progress dirty state did not coalesce = %#v", current)
	}
}

type eventSink struct{ count atomic.Int64 }

func (s *eventSink) MarkDirty(time.Time) error { s.count.Add(1); return nil }

func TestEventEndpointStrictBodyAndContentType(t *testing.T) {
	t.Parallel()
	sink := &eventSink{}
	server := NewEventServer(schedulerConfig(), sink)
	request := httptest.NewRequest(http.MethodPost, EventsPath, strings.NewReader(`{"schema_version":1}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || sink.count.Load() != 1 {
		t.Fatalf("valid event status/count = %d/%d", response.Code, sink.count.Load())
	}
	request = httptest.NewRequest(http.MethodPost, EventsPath, strings.NewReader(`{"schema_version":1,"extra":true}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, EventsPath, strings.NewReader(`{"schema_version":1}`))
	response = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type status = %d", response.Code)
	}
}
