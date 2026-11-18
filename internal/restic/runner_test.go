package restic

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
)

func TestNativeSnapshotIsTheBaseline(t *testing.T) {
	root := t.TempDir()
	runner := NewRunner("restic", root)
	t.Setenv("RESTIC_PASSWORD", "synthetic")
	t.Setenv("RESTIC_REPOSITORY", filepath.Join(root, "repository"))
	t.Setenv("RESTIC_CACHE_DIR", filepath.Join(root, "restic"))
	target := config.Target{Name: "one", Environment: []string{"RESTIC_PASSWORD=synthetic", "RESTIC_REPOSITORY=" + os.Getenv("RESTIC_REPOSITORY")}}
	native := func(args ...string) []byte {
		t.Helper()
		data, err := exec.Command("restic", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("restic: %v: %s", err, data)
		}
		return data
	}
	native("init")
	candidate := filepath.Join(root, "candidate")
	db := func(query string) {
		t.Helper()
		if data, err := exec.Command("sqlite3", candidate, query).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, data)
		}
	}
	db("CREATE TABLE personal(value);INSERT INTO personal VALUES('first');CREATE TABLE devices(id);")
	rule := capture.Rule("SELECT (SELECT value FROM previous.personal) IS NOT (SELECT value FROM current.personal);")
	cacheFile := filepath.Join(root, "cached")
	sync := func(cached Baseline) Baseline {
		t.Helper()
		base, err := runner.Sync(t.Context(), target, rule, candidate, cacheFile, cached)
		if err != nil {
			t.Fatal(err)
		}
		return base
	}
	first := sync(Baseline{})
	// Build a fresh candidate for every request, as the worker does.
	next := filepath.Join(root, "next")
	if err := capture.OnlineBackup(t.Context(), candidate, next); err != nil {
		t.Fatal(err)
	}
	candidate = next
	db("INSERT INTO devices VALUES('new login')")
	if err := os.Remove(cacheFile); err != nil {
		t.Fatal(err)
	}
	if got := sync(Baseline{}); got.ID != first.ID {
		t.Fatal("cache loss or login created a snapshot")
	}
	db("UPDATE personal SET value='edited'")
	latest := sync(first)
	if latest.ID == first.ID {
		t.Fatal("personal edit missed")
	}
	if _, _, err := runner.backup(context.Background(), target, filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing command source accepted")
	}
	var snapshots []Snapshot
	if err := json.Unmarshal(native("snapshots", "--json"), &snapshots); err != nil || len(snapshots) != 2 {
		t.Fatal("unexpected snapshot count")
	}
	dir := filepath.Join(root, "restore")
	native("restore", latest.ID, "--verify", "--target", dir)
	data, err := exec.Command("sqlite3", filepath.Join(dir, "db.sqlite3"), "SELECT value FROM personal;SELECT id FROM devices;").Output()
	if err != nil || string(data) != "edited\nnew login\n" {
		t.Fatal("standalone restore lost native data")
	}
}
