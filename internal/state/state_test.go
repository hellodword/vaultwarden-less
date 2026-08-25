package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/safefs"
)

func TestStorePersistsAtomic0600State(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	mustPrivateDirectory(t, dir)
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
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("status read accepted a group-accessible state directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestStoreUsesPublishedStateAfterParentSyncUncertainty(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	mustPrivateDirectory(t, dir)
	store, err := Open(dir, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	store.fileOps.SyncDir = func(*os.File) error { return errors.New("parent sync failed") }
	err = store.Update(func(current *State) error {
		current.Dirty = true
		current.DirtyGeneration = 1
		return nil
	})
	if err == nil {
		t.Fatal("published parent-sync failure was not returned")
	}
	var persistErr *PersistError
	if !errors.As(err, &persistErr) || persistErr.PublishState != safefs.PublishedUnconfirmed {
		t.Fatalf("persistence error = %v", err)
	}
	if !store.Snapshot().Dirty {
		t.Fatal("in-memory state rolled back after publication")
	}
	reloaded, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Dirty {
		t.Fatal("published state was not present on disk")
	}
}

func TestWorkerLockIsExclusive(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	mustPrivateDirectory(t, dir)
	first, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if second, err := AcquireLock(dir); err == nil {
		_ = second.Close()
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
		_ = lock.Close()
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
	mustPrivateDirectory(t, dir)
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
	mustPrivateDirectory(t, dir)
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

func TestOpenRebuildsSafeInvalidStateButNeverUnsafeFiles(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	mustPrivateDirectory(t, dir)
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{\"schema_version\":1,\"unknown\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := Open(dir, []string{"primary"})
	if err != nil {
		t.Fatal(err)
	}
	if !rebuilt.Rebuilt() || rebuilt.Snapshot().Targets["primary"].State != "unknown" {
		t.Fatalf("safe invalid state was not rebuilt: %#v", rebuilt.Snapshot())
	}
	if err := os.WriteFile(path, []byte("keep\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, []string{"primary"}); err == nil {
		t.Fatal("insecure state file was rebuilt")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "keep\n" {
		t.Fatalf("insecure state file changed: %q/%v", contents, err)
	}
}

func TestOpenRefusesStateSymlink(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "state")
	mustPrivateDirectory(t, dir)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, []string{"primary"}); err == nil {
		t.Fatal("state symlink was accepted")
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != "keep\n" {
		t.Fatalf("state symlink target changed: %q/%v", contents, err)
	}
}

func mustPrivateDirectory(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
}
