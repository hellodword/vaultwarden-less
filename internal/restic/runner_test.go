package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	environmentPath := filepath.Join(root, "environment.log")
	script := filepath.Join(root, "restic")
	scriptBody := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> '` + logPath + `'
/usr/bin/env > '` + environmentPath + `'
while [ "${1:-}" = -o ]; do
  shift 2
done
if [ "${1:-}" = version ]; then
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
	t.Setenv("SENTINEL", "host-only")
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
	environment, err := os.ReadFile(filepath.Join(filepath.Dir(logPath), "environment.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(environment), "ARG_LOG=") || strings.Contains(string(environment), "SENTINEL=") {
		t.Fatalf("host-only environment leaked: %s", environment)
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
	if len(allLines) != 3 || len(lines) != 2 {
		t.Fatalf("prune inspection/command count = %d/%d (%q)", len(allLines), len(lines), allLines)
	}
	for i, line := range lines {
		required := []string{
			"forget --json", "--dry-run", "--host " + version.SnapshotHost,
			"--tag " + version.SnapshotTag + ",", "--group-by host", "--keep-within 24h",
		}
		if i == 0 {
			required = append(required, "--keep-hourly 48", "--keep-yearly 3")
		} else {
			required = append(required, "--keep-daily 7")
		}
		for _, required := range required {
			if !strings.Contains(line, required) {
				t.Errorf("prune line %d lacks %q: %s", i, required, line)
			}
		}
		if strings.Contains(line, "--prune") {
			t.Errorf("dry-run unexpectedly applies prune: %s", line)
		}
	}
}

func TestAppliedForgetAcceptsRestic0191EndOfJSON(t *testing.T) {
	id := strings.Repeat("a", 64)
	data := []byte(`[{"tags":null,"host":"vaultwarden-less-v1","paths":null,"keep":[{"id":"` + id + `"}],"remove":null,"reasons":null}]`)
	result, err := parseForgetOutput(data, snapshot.Complete, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.KeptCount != 1 || result.KeptSnapshotIDs[0] != id {
		t.Fatalf("result = %#v", result)
	}
}

func TestAppliedForgetRejectsUnknownTrailingJSON(t *testing.T) {
	id := strings.Repeat("a", 64)
	data := []byte(`[{"keep":[{"id":"` + id + `"}]}]` + "\n" + `{"message_type":"unknown"}`)
	if _, err := parseForgetOutput(data, snapshot.Complete, true); err == nil {
		t.Fatal("unknown trailing prune output was accepted")
	}
}

func TestAppliedForgetAcceptsDocumentedPruneSummary(t *testing.T) {
	id := strings.Repeat("a", 64)
	data := []byte(`[{
  "tags": null, "host": "vaultwarden-less-v1", "paths": null,
  "keep": [{"id": "` + id + `"}], "remove": null, "reasons": null
}]
{"message_type":"summary","blobs":{"used":0,"duplicate":0,"unused":0,"total":0,"repack":0,"repack_remove":0,"remove":0,"remove_total":0,"remaining":0},"bytes":{"used":0,"duplicate":0,"unused":0,"unreferenced":0,"uncompressed":0,"total":0,"repack":0,"repack_remove":0,"remove":0,"remove_total":0,"remaining":0,"remaining_unused":0},"packfiles":{"used":0,"unused":0,"partly_used":0,"unreferenced":0,"total":0,"keep":0,"repack":0,"remove":0,"remove_total":0}}`)
	if _, err := parseForgetOutput(data, snapshot.Complete, true); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotsUsesProductHostFilter(t *testing.T) {
	runner, target, logPath := fakeRunner(t)
	if _, err := runner.Snapshots(context.Background(), target, snapshot.Complete); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	want := "snapshots --json"
	if !strings.Contains(line, want) {
		t.Fatalf("snapshots command lacks list operation %q: %s", want, line)
	}
	if !strings.Contains(line, "--host "+version.SnapshotHost) || !strings.Contains(line, "--tag "+version.SnapshotTag) {
		t.Fatalf("snapshots command lacks product identity filter: %s", line)
	}
	if strings.Contains(line, string(snapshot.Complete)) {
		t.Fatalf("snapshots command applied a status filter remotely: %s", line)
	}
}

func TestListSnapshotsSeparatesInvalidCandidatesAndAllowsExtraMetadata(t *testing.T) {
	runner, target, logPath := fakeRunner(t)
	manifest, err := testManifest()
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(logPath)
	manifestPath := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(manifestPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	firstID := strings.Repeat("a", 64)
	secondID := strings.Repeat("b", 64)
	listing := fmt.Sprintf(`[{"id":%q,"time":"2026-08-23T12:00:00.000Z","hostname":%q,"tags":[%q,%q,"content-id=%s"],"paths":["data","manifest.json"]},{"id":"bad","time":"2026-08-23T12:01:00.000Z","hostname":%q,"tags":[%q,%q,"content-id=%s"],"paths":[]},{"id":%q,"time":"2026-08-23T12:02:00.000Z","hostname":%q,"tags":[%q,%q,"content-id=%s","operator-tag"],"paths":["a-different-path"]}]`, firstID, version.SnapshotHost, version.SnapshotTag, snapshot.Complete, strings.TrimPrefix(manifest.ContentID, "sha256:"), version.SnapshotHost, version.SnapshotTag, snapshot.Complete, strings.TrimPrefix(manifest.ContentID, "sha256:"), secondID, version.SnapshotHost, version.SnapshotTag, snapshot.Complete, strings.TrimPrefix(manifest.ContentID, "sha256:"))
	script := filepath.Join(root, "restic-list")
	scriptBody := "#!/bin/sh\nset -eu\nwhile [ \"${1:-}\" = -o ]; do shift 2; done\nif [ \"${1:-}\" = snapshots ]; then printf '%s\\n' '" + listing + "'\nelif [ \"${1:-}\" = dump ]; then /bin/cat '" + manifestPath + "'\nfi\n"
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil {
		t.Fatal(err)
	}
	runner.Binary = script
	report, err := runner.ListSnapshots(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Snapshots) != 2 || report.Snapshots[0].ID != firstID || report.Snapshots[1].ID != secondID {
		t.Fatalf("valid candidates = %#v", report.Snapshots)
	}
	if len(report.Anomalies) != 1 || report.Anomalies[0].SnapshotID != "bad" || report.Anomalies[0].ErrorCode != "snapshot_id_invalid" {
		t.Fatalf("candidate anomalies = %#v", report.Anomalies)
	}
	if _, err := runner.FindSnapshot(context.Background(), target, "bad"); err == nil {
		t.Fatal("explicit invalid candidate was accepted")
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
	} {
		if validSnapshotTags(tags) {
			t.Fatalf("invalid v1 tags were accepted: %q", tags)
		}
	}
	if !validSnapshotTags([]string{version.SnapshotTag, string(snapshot.Complete), content, "unrelated"}) {
		t.Fatal("extra safety tag was rejected")
	}
}

func TestBackupCancellationTerminatesRestic(t *testing.T) {
	runner, target, _ := fakeRunner(t)
	script := filepath.Join(t.TempDir(), "restic-blocking")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /usr/bin/sleep 30\n"), 0o700); err != nil {
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
	if !errors.As(err, &operation) || operation.Code != "operation_timeout" {
		t.Fatalf("cancelled backup error = %v", err)
	}
}

func TestResticFailureRedactsSecretsBeforeLogging(t *testing.T) {
	runner, target, _ := fakeRunner(t)
	script := filepath.Join(t.TempDir(), "restic-failure")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s %s %s\\n' 'synthetic-access' 'synthetic-backend-value' 'synthetic-password' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner.Binary = script
	var logs bytes.Buffer
	runner.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	manifest, err := testManifest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Backup(context.Background(), target, manifest)
	var operation *OperationError
	if !errors.As(err, &operation) || operation.Code != "repository_operation_failed" {
		t.Fatalf("failure = %v", err)
	}
	for _, secret := range []string{"synthetic-access", "synthetic-backend-value", "synthetic-password"} {
		if strings.Contains(operation.Stderr, secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("secret %q escaped redaction: stderr=%q logs=%q", secret, operation.Stderr, logs.String())
		}
	}
	if !strings.Contains(operation.Stderr, "[REDACTED]") || !strings.Contains(logs.String(), `"target":"primary"`) || !strings.Contains(logs.String(), `"s3_host":"objects.example.test"`) {
		t.Fatalf("redacted diagnostic fields missing: stderr=%q logs=%q", operation.Stderr, logs.String())
	}
}
