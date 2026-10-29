package restic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

func TestForcedNativeBackupRestoreAcrossPaths(t *testing.T) {
	dir := t.TempDir()
	target := config.Target{Name: "primary", Environment: []string{"RESTIC_REPOSITORY=" + filepath.Join(dir, "repo"), "RESTIC_PASSWORD=synthetic-test"}}
	runner := NewRunner("restic", filepath.Join(dir, "cache"))
	if _, err := runner.command(context.Background(), target, "", "init"); err != nil {
		t.Fatal(err)
	}
	if latest, err := runner.Latest(context.Background(), target); err != nil || latest != nil {
		t.Fatalf("empty repo: %v %v", latest, err)
	}
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sqlite3", filepath.Join(first, "db.sqlite3"), "CREATE TABLE t(v); INSERT INTO t VALUES('preserved');")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(first, "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "db.sqlite3"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "rsa_key.pem"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "rsa_key.pem"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := runner.Backup(context.Background(), target, first, "complete", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := runner.Latest(context.Background(), target)
	if err != nil || latest == nil || latest.ID != id {
		t.Fatalf("latest mismatch: %v %v", latest, err)
	}
	secondID, err := runner.Backup(context.Background(), target, second, "incomplete", strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	output, err := runner.command(context.Background(), target, "", "cat", "snapshot", secondID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Parent string   `json:"parent"`
		Tags   []string `json:"tags"`
	}
	if json.Unmarshal(output, &snapshot) != nil || snapshot.Parent != "" || !slices.Contains(snapshot.Tags, "incomplete") || !slices.Contains(snapshot.Tags, FingerprintPrefix+strings.Repeat("b", 64)) {
		t.Fatal("forced read or native tags incorrect across staging paths")
	}
	restored := filepath.Join(dir, "restored")
	if _, err := runner.command(context.Background(), target, "", "restore", secondID, "--verify", "--target", restored); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(restored, "db.sqlite3")); err != nil {
		t.Fatal("restore did not yield root database")
	}
	result, err := exec.Command("sqlite3", filepath.Join(restored, "db.sqlite3"), "PRAGMA integrity_check; SELECT v FROM t;").CombinedOutput()
	if err != nil || string(result) != "ok\npreserved\n" {
		t.Fatalf("native restore unusable: %v %s", err, result)
	}
}

func TestExitStatusOptionsAndEnvironment(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "parent-secret")
	target := config.Target{Name: "target", Options: []string{"s3.connections=2", "raw='$(no shell)'"}, Environment: []string{"RESTIC_REPOSITORY=/target"}}
	runner := NewRunner("restic", t.TempDir())
	runner.execute = func(_ context.Context, _ string, args, env []string, dir string) ([]byte, error) {
		if dir != "/immutable" || !slices.Contains(args, "--force") || !slices.Contains(args, "raw='$(no shell)'") {
			t.Fatal("arguments changed")
		}
		if strings.Contains(strings.Join(env, "\n"), "parent-secret") {
			t.Fatal("inherited target credentials")
		}
		return []byte(`{"message_type":"summary","snapshot_id":"` + strings.Repeat("a", 64) + `"}`), errors.New("nonzero exit")
	}
	if id, err := runner.Backup(context.Background(), target, "/immutable", "complete", strings.Repeat("a", 64)); err == nil || id != "" {
		t.Fatal("nonzero exit advanced baseline")
	}
	for _, result := range []string{"", `{}`, `{"message_type":"summary"}`, `{"message_type":"error"}`} {
		runner.execute = func(context.Context, string, []string, []string, string) ([]byte, error) { return []byte(result), nil }
		if _, err := runner.Backup(context.Background(), target, "/immutable", "complete", strings.Repeat("a", 64)); err == nil {
			t.Fatalf("accepted %q", result)
		}
	}
}

func TestRepositoryVerificationReadsRemoteAndMetadata(t *testing.T) {
	root := t.TempDir()
	target := config.Target{Name: "test", Environment: []string{"RESTIC_REPOSITORY=" + filepath.Join(root, "repo"), "RESTIC_PASSWORD=synthetic"}}
	runner := NewRunner("restic", filepath.Join(root, "cache"))
	ctx := t.Context()
	if _, err := runner.command(ctx, target, "", "init"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	os.Mkdir(source, 0o700)
	os.WriteFile(filepath.Join(source, "file"), []byte(strings.Repeat("fixture", 1000)), 0o600)
	id, err := runner.Backup(ctx, target, source, "complete", strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range []bool{false, true} {
		if err := runner.Check(ctx, target, full); err != nil {
			t.Fatal(err)
		}
	}
	metadata, err := runner.Inspect(ctx, target)
	if err != nil || !metadata.Has(id) || metadata.Latest(true).Fingerprint() != strings.Repeat("c", 64) {
		t.Fatal("native metadata missing")
	}
	if err := os.Remove(filepath.Join(root, "repo", "snapshots", id)); err != nil {
		t.Fatal(err)
	}
	metadata, err = runner.Inspect(ctx, target)
	if err != nil || metadata.Has(id) {
		t.Fatal("deleted snapshot still claimed")
	}
	// A fresh independent cache must expose damaged repository packs even after
	// the ordinary backup cache has read the original content successfully.
	var pack string
	filepath.WalkDir(filepath.Join(root, "repo", "data"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && pack == "" {
			pack = path
		}
		return err
	})
	if pack == "" {
		t.Fatal("no pack created")
	}
	if err := os.Chmod(pack, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, []byte("damaged synthetic object"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.Check(ctx, target, true); err == nil {
		t.Fatal("remote corruption was hidden by cache")
	}
	work, _ := os.ReadDir(filepath.Join(root, "cache", "work"))
	if len(work) != 0 {
		t.Fatal("verification cache leaked")
	}
}
