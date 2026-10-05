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

func TestNativeBackupRestoreAndParentAcrossPaths(t *testing.T) {
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
	id, err := runner.Backup(context.Background(), target, first, "complete")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := runner.Latest(context.Background(), target)
	if err != nil || latest == nil || latest.ID != id {
		t.Fatalf("latest mismatch: %v %v", latest, err)
	}
	secondID, err := runner.Backup(context.Background(), target, second, "incomplete")
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
	if json.Unmarshal(output, &snapshot) != nil || snapshot.Parent != id || !slices.Equal(snapshot.Tags, []string{"incomplete"}) {
		t.Fatal("parent or native tags changed across staging paths")
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
		if dir != "/immutable" || !slices.Contains(args, "raw='$(no shell)'") {
			t.Fatal("arguments changed")
		}
		if strings.Contains(strings.Join(env, "\n"), "parent-secret") {
			t.Fatal("inherited target credentials")
		}
		return []byte(`{"message_type":"summary","snapshot_id":"` + strings.Repeat("a", 64) + `"}`), errors.New("nonzero exit")
	}
	if id, err := runner.Backup(context.Background(), target, "/immutable", "complete"); err == nil || id != "" {
		t.Fatal("nonzero exit advanced baseline")
	}
	for _, result := range []string{"", `{}`, `{"message_type":"summary"}`, `{"message_type":"error"}`} {
		runner.execute = func(context.Context, string, []string, []string, string) ([]byte, error) { return []byte(result), nil }
		if _, err := runner.Backup(context.Background(), target, "/immutable", "complete"); err == nil {
			t.Fatalf("accepted %q", result)
		}
	}
}
