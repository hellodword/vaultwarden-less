package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsAtomic0600State(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 23, 12, 34, 56, 789_999_999, time.UTC)
	if err := store.Update(func(current *State) error {
		current.Dirty = true
		current.DirtyGeneration = 1
		current.FirstDirtyAt = FormatTime(when)
		current.LastDirtyAt = FormatTime(when)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("state mode = %v", info.Mode())
	}
	reloaded, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Dirty || reloaded.FirstDirtyAt != "2026-08-23T12:34:56.789Z" {
		t.Fatalf("reloaded state = %#v", reloaded)
	}
}

func TestWorkerLockIsExclusive(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := AcquireLock(dir); err == nil {
		second.Close()
		t.Fatal("second lock acquisition succeeded")
	}
}

func TestWorkerLockRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "worker.lock")); err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireLock(dir); err == nil {
		lock.Close()
		t.Fatal("symlink worker lock was accepted")
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target was modified: %v/%v", info.Mode(), err)
	}
}

func TestTimestampRequiresMillisecondsUTC(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"2026-08-23T12:00:00Z", "2026-08-23T12:00:00.000+00:00", "2026-08-23T12:00:00.000001Z"} {
		if _, err := ParseTime(raw); err == nil {
			t.Fatalf("accepted timestamp %q", raw)
		}
	}
}

func TestOpenRecoversInterruptedBackupAsDirty(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(current *State) error {
		current.BackupInProgress = true
		current.RunningGeneration = current.DirtyGeneration
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	current := reopened.Snapshot()
	if current.BackupInProgress || !current.Dirty {
		t.Fatalf("interrupted state was not recovered for resampling: %#v", current)
	}
}

func TestOpenReconcilesTargetsToCurrentConfiguration(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	if _, err := Open(dir, []string{"removed"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, []string{"current"})
	if err != nil {
		t.Fatal(err)
	}
	targets := reopened.Snapshot().Targets
	if _, exists := targets["removed"]; exists {
		t.Fatal("removed target remained in active state")
	}
	if target, exists := targets["current"]; !exists || target.State != "unknown" {
		t.Fatalf("current target was not initialized: %#v", targets)
	}
}
