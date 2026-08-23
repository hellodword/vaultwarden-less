package worker

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/version"
	_ "github.com/mattn/go-sqlite3"
)

type partialTargetRunner struct {
	mu       sync.Mutex
	failing  map[string]bool
	calls    map[string]int
	snapshot map[string]string
}

type blockingTargetRunner struct {
	started  chan struct{}
	canceled chan struct{}
}

func (r *blockingTargetRunner) Backup(ctx context.Context, _ config.Target, _ snapshot.Manifest) (restic.BackupResult, error) {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	return restic.BackupResult{}, &restic.OperationError{Code: "operation_cancelled"}
}

func (r *partialTargetRunner) Backup(_ context.Context, target config.Target, _ snapshot.Manifest) (restic.BackupResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[target.Name]++
	if r.failing[target.Name] {
		return restic.BackupResult{}, &restic.OperationError{Code: "backup_failed"}
	}
	return restic.BackupResult{SnapshotID: r.snapshot[target.Name]}, nil
}

func (r *partialTargetRunner) recover(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing[name] = false
}

func (r *partialTargetRunner) callCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[name]
}

func TestPartialTargetFailureDoesNotBlockHealthyTargetAndCatchUpUsesLatest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dataDir := createCoordinatorData(t, root)
	cfg := &config.Backup{
		DataDir: dataDir, StateDir: filepath.Join(root, "state"), StagingDir: filepath.Join(root, "staging"),
		CacheDir: filepath.Join(root, "cache"), MissingRetry: config.Duration{Duration: time.Millisecond},
		TargetTimeout: config.Duration{Duration: time.Second},
		Targets:       []config.Target{{Name: "primary"}, {Name: "secondary"}},
	}
	store, err := state.Open(cfg.StateDir, []string{"primary", "secondary"})
	if err != nil {
		t.Fatal(err)
	}
	notifiers, err := notify.NewManager(cfg.StateDir, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runner := &partialTargetRunner{
		failing: map[string]bool{"secondary": true}, calls: map[string]int{},
		snapshot: map[string]string{"primary": strings.Repeat("a", 64), "secondary": strings.Repeat("b", 64)},
	}
	coordinator := NewCoordinator(cfg, store, runner, notifiers, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := coordinator.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := store.Snapshot()
	if current.Current == nil || current.Targets["primary"].State != "healthy" || current.Targets["secondary"].State != "degraded" {
		t.Fatalf("partial failure state = %#v", current)
	}
	if current.Targets["primary"].ContentID != current.Current.ContentID || current.Targets["secondary"].ContentID == current.Current.ContentID {
		t.Fatalf("target progress was not isolated: %#v", current.Targets)
	}

	runner.recover("secondary")
	if err := store.Update(func(next *state.State) error {
		target := next.Targets["secondary"]
		target.NextRetryAt = state.FormatTime(time.Now().Add(-time.Second))
		next.Targets["secondary"] = target
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.CatchUp(context.Background()); err != nil {
		t.Fatal(err)
	}
	current = store.Snapshot()
	if current.Current != nil || current.Targets["secondary"].State != "healthy" || current.Targets["secondary"].ContentID == "" {
		t.Fatalf("catch-up state = %#v", current)
	}
	if runner.callCount("primary") != 1 || runner.callCount("secondary") != 2 {
		t.Fatalf("target calls primary=%d secondary=%d", runner.callCount("primary"), runner.callCount("secondary"))
	}
	if _, err := os.Lstat(filepath.Join(cfg.StagingDir, "current")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("caught-up staging point was not retired: %v", err)
	}
}

func TestScheduledSamplePreemptsSlowTargetWithoutMarkingFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Backup{
		DataDir: createCoordinatorData(t, root), StateDir: filepath.Join(root, "state"),
		StagingDir: filepath.Join(root, "staging"), CacheDir: filepath.Join(root, "cache"),
		MissingRetry: config.Duration{Duration: time.Millisecond}, TargetTimeout: config.Duration{Duration: 5 * time.Second},
		SampleInterval: config.Duration{Duration: 50 * time.Millisecond}, Targets: []config.Target{{Name: "slow"}},
	}
	store, err := state.Open(cfg.StateDir, []string{"slow"})
	if err != nil {
		t.Fatal(err)
	}
	notifiers, err := notify.NewManager(cfg.StateDir, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runner := &blockingTargetRunner{started: make(chan struct{}), canceled: make(chan struct{})}
	coordinator := NewCoordinator(cfg, store, runner, notifiers, slog.New(slog.NewTextHandler(io.Discard, nil)))

	startedAt := time.Now()
	if err := coordinator.sample(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if time.Since(startedAt) > time.Second {
		t.Fatal("scheduled sample waited for the slow target timeout")
	}
	select {
	case <-runner.started:
	default:
		t.Fatal("slow target was not attempted")
	}
	select {
	case <-runner.canceled:
	default:
		t.Fatal("slow target was not canceled when sampling became due")
	}
	current := store.Snapshot()
	if current.Current == nil || current.Targets["slow"].State == "degraded" || current.Targets["slow"].ConsecutiveFailures != 0 {
		t.Fatalf("scheduled preemption was recorded as a remote failure: %#v", current)
	}
}

func TestUnavailableNotificationPersistenceDoesNotChangeBackupResult(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := &config.Backup{
		DataDir: createCoordinatorData(t, root), StateDir: filepath.Join(root, "state"),
		StagingDir: filepath.Join(root, "staging"), CacheDir: filepath.Join(root, "cache"),
		MissingRetry: config.Duration{Duration: time.Millisecond}, TargetTimeout: config.Duration{Duration: time.Second},
		Targets: []config.Target{{Name: "primary"}},
	}
	store, err := state.Open(cfg.StateDir, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &partialTargetRunner{
		failing: map[string]bool{}, calls: map[string]int{}, snapshot: map[string]string{"primary": strings.Repeat("a", 64)},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	coordinator := NewCoordinator(cfg, store, runner, notify.NewUnavailableManager(log), log)
	if err := coordinator.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := store.Snapshot()
	if current.Targets["primary"].State != "healthy" || current.Targets["primary"].ContentID == "" {
		t.Fatalf("notification persistence changed backup state: %#v", current)
	}
}

func createCoordinatorData(t *testing.T, root string) string {
	t.Helper()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite3", filepath.Join(dataDir, "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	statements := []string{
		`CREATE TABLE attachments (id TEXT PRIMARY KEY, cipher_uuid TEXT NOT NULL, file_size INTEGER NOT NULL)`,
		`CREATE TABLE ciphers (uuid TEXT PRIMARY KEY)`,
		`CREATE TABLE sends (uuid TEXT PRIMARY KEY, atype INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE users (uuid TEXT PRIMARY KEY)`,
		`CREATE TABLE __diesel_schema_migrations (version TEXT PRIMARY KEY)`,
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`INSERT INTO __diesel_schema_migrations(version) VALUES (?)`, version.VaultwardenSchema); err != nil {
		t.Fatal(err)
	}
	return dataDir
}
