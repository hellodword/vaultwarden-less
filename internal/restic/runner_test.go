package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

func fakeRunner(t *testing.T) (Runner, config.Target, string) {
	t.Helper()
	root := t.TempDir()
	logPath := filepath.Join(root, "arguments.log")
	script := filepath.Join(root, "restic")
	scriptBody := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$ARG_LOG"
while [ "${1:-}" = -o ] || [ "${1:-}" = --cacert ]; do
  shift 2
done
if [ "$1" = version ]; then
  printf '%s\n' 'restic 0.19.1 compiled with go1.26.7 on linux/amd64'
elif [ "$1" = backup ]; then
  printf '%s\n' '{"message_type":"summary","snapshot_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}'
else
  printf '%s\n' '[]'
fi
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARG_LOG", logPath)
	secretsDir := filepath.Join(root, "secrets")
	if err := os.Mkdir(secretsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(secretsDir, "password")
	if err := os.WriteFile(password, []byte("synthetic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := config.Target{
		Name: "primary", Repository: "s3:https://objects.example.test/bucket/repository",
	}
	credentials := &config.RoleCredentials{SchemaVersion: 1, Targets: map[string]map[string]string{
		"primary": {"AWS_ACCESS_KEY_ID": "synthetic-access", "AWS_SECRET_ACCESS_KEY": "synthetic-backend-value"},
	}}
	staging := filepath.Join(root, "staging")
	for _, path := range []string{filepath.Join(staging, "current", "data"), filepath.Join(root, "cache")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(staging, "current", "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Runner{
		Binary: script, CacheDir: filepath.Join(root, "cache"), StagingDir: staging,
		PasswordFile: password, Credentials: credentials,
	}, target, logPath
}

func TestBackupRunsOnlyBackupWithSecretFileEnvironment(t *testing.T) {
	runner, target, logPath := fakeRunner(t)
	if err := runner.VerifyVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	manifest, err := testManifest()
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Backup(context.Background(), target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.SnapshotID) != 64 {
		t.Fatalf("snapshot ID = %q", result.SnapshotID)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 2 || lines[0] != "version" || !strings.HasPrefix(lines[1], "backup --json") {
		t.Fatalf("commands = %q", lines)
	}
	for _, forbidden := range []string{"synthetic-password", "synthetic-backend-value", " init", " unlock", " forget", " prune", " check"} {
		if strings.Contains(lines[1], forbidden) {
			t.Fatalf("backup command contains forbidden value %q: %s", forbidden, lines[1])
		}
	}
}

func TestPruneIsDryRunAndStrictlyFilteredByDefault(t *testing.T) {
	runner, target, logPath := fakeRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reports, err := runner.Prune(ctx, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %d", len(reports))
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	allLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var lines []string
	for _, line := range allLines {
		if strings.Contains(line, "forget --json") {
			lines = append(lines, line)
		}
	}
	if len(allLines) != 4 || len(lines) != 2 {
		t.Fatalf("prune inspection/command count = %d/%d (%q)", len(allLines), len(lines), allLines)
	}
	for i, line := range lines {
		for _, required := range []string{
			"forget --json", "--dry-run", "--host " + version.SnapshotHost,
			"--path " + filepath.Join(runner.StagingDir, "current", "data"),
			"--path " + filepath.Join(runner.StagingDir, "current", "manifest.json"),
			"--tag " + version.SnapshotTag + ",", "--group-by host,paths", "--keep-within 24h", "--keep-hourly 48", "--keep-yearly 3",
		} {
			if !strings.Contains(line, required) {
				t.Errorf("prune line %d lacks %q: %s", i, required, line)
			}
		}
		if strings.Contains(line, "--prune") {
			t.Errorf("dry-run unexpectedly applies prune: %s", line)
		}
	}
}

func TestSnapshotsUsesConjunctiveStatusTagFilter(t *testing.T) {
	runner, target, logPath := fakeRunner(t)
	if _, err := runner.Snapshots(context.Background(), target, snapshot.Complete); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	want := "--tag " + version.SnapshotTag + "," + string(snapshot.Complete)
	if !strings.Contains(line, want) {
		t.Fatalf("snapshots command lacks conjunctive tag filter %q: %s", want, line)
	}
}

func TestCredentialPermissionFailureIsGeneric(t *testing.T) {
	runner, target, _ := fakeRunner(t)
	if err := os.Chmod(runner.PasswordFile, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := testManifest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Backup(context.Background(), target, manifest)
	if err == nil || strings.Contains(err.Error(), runner.PasswordFile) {
		t.Fatalf("error was absent or exposed path: %v", err)
	}
}

func TestPasswordFileRejectsAdditionalLines(t *testing.T) {
	runner, target, _ := fakeRunner(t)
	if err := os.WriteFile(runner.PasswordFile, []byte("first\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.ValidateTarget(target); err == nil {
		t.Fatal("multi-line Restic password was accepted")
	}
}

func TestS3OptionsArePassedAsStructuredGlobalArguments(t *testing.T) {
	runner, target, logPath := fakeRunner(t)
	target.ResticOptions = map[string]string{"s3.region": "ap-singapore", "s3.bucket-lookup": "dns"}
	manifest, err := testManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Backup(context.Background(), target, manifest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "-o s3.bucket-lookup=dns -o s3.region=ap-singapore backup ") {
		t.Fatalf("structured S3 options were not deterministic: %s", line)
	}
}

func testManifest() (snapshot.Manifest, error) {
	return snapshot.NewManifest(snapshot.Complete, version.VaultwardenSchema, []snapshot.File{{
		Path: "data/db.sqlite3", Size: 0, SHA256: strings.Repeat("0", 64),
	}}, nil)
}

func TestSamePathsRequiresExactSet(t *testing.T) {
	want := []string{"/staging/current/data", "/staging/current/manifest.json"}
	for _, test := range []struct {
		name string
		got  []string
		want bool
	}{
		{"same order", append([]string(nil), want...), true},
		{"reverse order", []string{want[1], want[0]}, true},
		{"missing", []string{want[0]}, false},
		{"additional", []string{want[0], want[1], "/other"}, false},
		{"duplicate", []string{want[0], want[0]}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := samePaths(test.got, want); got != test.want {
				t.Fatalf("samePaths() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSnapshotTagsRequireExactV1Identity(t *testing.T) {
	t.Parallel()
	content := "content-id=" + strings.Repeat("a", 64)
	if !validSnapshotTags([]string{version.SnapshotTag, string(snapshot.Complete), content}) {
		t.Fatal("valid v1 tags were rejected")
	}
	for _, tags := range [][]string{
		{version.SnapshotTag, string(snapshot.Complete)},
		{version.SnapshotTag, string(snapshot.Complete), string(snapshot.Incomplete), content},
		{version.SnapshotTag, string(snapshot.Complete), "content-id=short"},
		{version.SnapshotTag, string(snapshot.Complete), content, "unrelated"},
	} {
		if validSnapshotTags(tags) {
			t.Fatalf("invalid v1 tags were accepted: %q", tags)
		}
	}
}

func TestScrubEnvironmentRemovesUnconfiguredBackendFamilies(t *testing.T) {
	t.Parallel()
	input := []string{"PATH=/usr/bin", "AWS_PROFILE=host", "AZURE_CLIENT_SECRET=host", "OS_AUTH_TOKEN=host", "RESTIC_PASSWORD=host", "UNRELATED=value"}
	got := scrubEnvironment(input)
	if strings.Join(got, ",") != "PATH=/usr/bin,UNRELATED=value" {
		t.Fatalf("scrubbed environment = %q", got)
	}
}

func TestBackupCancellationTerminatesRestic(t *testing.T) {
	runner, target, _ := fakeRunner(t)
	script := filepath.Join(t.TempDir(), "restic-blocking")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner.Binary = script
	manifest, err := testManifest()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = runner.Backup(ctx, target, manifest)
	if time.Since(started) > 2*time.Second {
		t.Fatal("cancelled Restic command did not terminate promptly")
	}
	var operation *OperationError
	if !errors.As(err, &operation) || operation.Code != "operation_cancelled" {
		t.Fatalf("cancelled backup error = %v", err)
	}
}
