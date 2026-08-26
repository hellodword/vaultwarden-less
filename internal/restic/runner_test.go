package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

var (
	firstSnapshotID  = strings.Repeat("a", 64)
	secondSnapshotID = strings.Repeat("b", 64)
	firstContentID   = "sha256:" + strings.Repeat("c", 64)
	secondContentID  = "sha256:" + strings.Repeat("d", 64)
)

type recordedCall struct {
	arguments   []string
	environment []string
	directory   string
}

func testRunner(t *testing.T, execute executeFunc) (Runner, config.Target, *[]recordedCall) {
	t.Helper()
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	credentialsDir := filepath.Join(root, "credentials")
	if err := os.Mkdir(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	password := filepath.Join(credentialsDir, "primary")
	if err := os.WriteFile(password, []byte("synthetic-restic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := config.Target{
		Name:       "primary",
		Repository: "s3:https://objects.example.test/bucket/repository",
		ResticOptions: map[string]string{
			"s3.bucket-lookup": "path",
			"s3.region":        "ap-singapore",
		},
	}
	credentials := &config.RoleCredentials{
		SchemaVersion: config.SchemaVersion,
		Targets: map[string]config.S3Credentials{
			"primary": {AccessKeyID: "synthetic-access", SecretAccessKey: "synthetic-secret"},
		},
	}
	calls := []recordedCall{}
	runner := Runner{
		Binary:       "/usr/local/bin/restic",
		CacheDir:     cache,
		PasswordRoot: credentialsDir,
		Credentials:  credentials,
	}
	runner.execute = func(ctx context.Context, binary string, arguments, environment []string, directory string) ([]byte, error) {
		if binary != runner.Binary {
			t.Fatalf("binary = %s", binary)
		}
		calls = append(calls, recordedCall{slices.Clone(arguments), slices.Clone(environment), directory})
		return execute(ctx, binary, arguments, environment, directory)
	}
	return runner, target, &calls
}

func manifestWithContent(t *testing.T, hash string) snapshot.Manifest {
	t.Helper()
	manifest, err := snapshot.NewManifest([]snapshot.File{{Path: "data/db.sqlite3", Size: 1, SHA256: hash}})
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func encodedSnapshots(t *testing.T, values ...rawSnapshot) []byte {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func rawRecoverySnapshot(id, content, when string) rawSnapshot {
	return rawSnapshot{
		ID:       id,
		Time:     when,
		Hostname: SnapshotHost,
		Tags:     []string{SnapshotTag, "content-id=" + strings.TrimPrefix(content, "sha256:")},
		Paths:    []string{SnapshotPath},
	}
}

func operation(arguments []string) string {
	for _, candidate := range []string{"snapshots", "backup", "forget", "restore", "init", "unlock", "check", "version"} {
		if slices.Contains(arguments, candidate) {
			return candidate
		}
	}
	return ""
}

func TestEnsureSkipsBackupWhenLatestContentMatches(t *testing.T) {
	manifest := manifestWithContent(t, strings.Repeat("c", 64))
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
		if operation(arguments) != "snapshots" {
			t.Fatalf("unexpected operation: %#v", arguments)
		}
		return encodedSnapshots(t, rawRecoverySnapshot(firstSnapshotID, manifest.ContentID, "2026-08-25T12:00:00Z")), nil
	})
	result, err := runner.Ensure(context.Background(), target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.SnapshotID != firstSnapshotID || result.ContentID != manifest.ContentID {
		t.Fatalf("result = %#v", result)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
}

func TestEnsureBacksUpDifferentContentOnceWithFixedIdentity(t *testing.T) {
	manifest := manifestWithContent(t, strings.Repeat("d", 64))
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, environment []string, _ string) ([]byte, error) {
		switch operation(arguments) {
		case "snapshots":
			return encodedSnapshots(t, rawRecoverySnapshot(firstSnapshotID, firstContentID, "2026-08-25T12:00:00Z")), nil
		case "backup":
			for _, forbidden := range []string{"--skip-if-unchanged", "complete", "incomplete"} {
				if slices.Contains(arguments, forbidden) {
					t.Fatalf("forbidden backup argument: %s", forbidden)
				}
			}
			for _, required := range []string{"--host", SnapshotHost, "--tag", SnapshotTag, "content-id=" + strings.TrimPrefix(manifest.ContentID, "sha256:"), "--", SnapshotPath} {
				if !slices.Contains(arguments, required) {
					t.Fatalf("missing backup argument %q in %#v", required, arguments)
				}
			}
			assertExactCredentialEnvironment(t, environment)
			return []byte(`{"message_type":"status"}` + "\n" + `{"message_type":"summary","snapshot_id":"` + secondSnapshotID + `"}` + "\n"), nil
		default:
			t.Fatalf("unexpected operation: %#v", arguments)
			return nil, nil
		}
	})
	t.Setenv("AWS_SESSION_TOKEN", "ambient-must-not-pass")
	t.Setenv("RESTIC_PASSWORD", "ambient-must-not-pass")
	result, err := runner.Ensure(context.Background(), target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || result.SnapshotID != secondSnapshotID || result.ContentID != manifest.ContentID {
		t.Fatalf("result = %#v", result)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d", len(*calls))
	}
}

func assertExactCredentialEnvironment(t *testing.T, environment []string) {
	t.Helper()
	found := map[string]string{}
	for _, value := range environment {
		name, content, ok := strings.Cut(value, "=")
		if ok {
			found[name] = content
		}
	}
	want := map[string]string{
		"TMPDIR":                "/tmp",
		"AWS_ACCESS_KEY_ID":     "synthetic-access",
		"AWS_SECRET_ACCESS_KEY": "synthetic-secret",
	}
	if len(found) != len(want) {
		t.Fatalf("credential environment = %#v", found)
	}
	for name, value := range want {
		if found[name] != value {
			t.Fatalf("credential environment = %#v", found)
		}
	}
}

func TestSnapshotsUsesOneMetadataQueryAndReturnsNewestFirst(t *testing.T) {
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
		if operation(arguments) != "snapshots" {
			t.Fatalf("unexpected operation: %#v", arguments)
		}
		return encodedSnapshots(t,
			rawRecoverySnapshot(firstSnapshotID, firstContentID, "2026-08-24T12:00:00Z"),
			rawRecoverySnapshot(secondSnapshotID, secondContentID, "2026-08-25T12:00:00Z"),
		), nil
	})
	items, truncated, err := runner.Snapshots(context.Background(), target, 100)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(*calls) != 1 || len(items) != 2 || items[0].ID != secondSnapshotID || items[1].ID != firstSnapshotID {
		t.Fatalf("calls/items = %d/%#v", len(*calls), items)
	}
	if slices.Contains((*calls)[0].arguments, "dump") {
		t.Fatal("snapshots invoked manifest dump")
	}
}

func TestSnapshotsBoundNormalizesTimesAndReportsTruncation(t *testing.T) {
	thirdSnapshotID := strings.Repeat("e", 64)
	thirdContentID := "sha256:" + strings.Repeat("f", 64)
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
		if operation(arguments) != "snapshots" {
			t.Fatalf("unexpected operation: %#v", arguments)
		}
		return encodedSnapshots(t,
			rawRecoverySnapshot(firstSnapshotID, firstContentID, "2026-08-25T11:59:59Z"),
			rawRecoverySnapshot(secondSnapshotID, secondContentID, "2026-08-25T13:00:00+01:00"),
			rawRecoverySnapshot(thirdSnapshotID, thirdContentID, "2026-08-25T12:00:00.000000000Z"),
		), nil
	})
	items, truncated, err := runner.Snapshots(context.Background(), target, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(items) != 2 || items[0].ID != thirdSnapshotID || items[1].ID != secondSnapshotID {
		t.Fatalf("bounded snapshots = %#v, truncated=%t", items, truncated)
	}
	if items[0].Time != "2026-08-25T12:00:00Z" || items[1].Time != "2026-08-25T12:00:00Z" {
		t.Fatalf("snapshot times were not normalized: %#v", items)
	}
	arguments := (*calls)[0].arguments
	latest := slices.Index(arguments, "--latest")
	if latest < 0 || latest+1 >= len(arguments) || arguments[latest+1] != "3" {
		t.Fatalf("bounded query arguments = %#v", arguments)
	}
}

func TestResolveSnapshotUsesLatestOrOneExactFullID(t *testing.T) {
	for name, requested := range map[string]string{"latest": "", "explicit": secondSnapshotID} {
		requested := requested
		t.Run(name, func(t *testing.T) {
			runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
				if operation(arguments) != "snapshots" {
					t.Fatalf("unexpected operation: %#v", arguments)
				}
				return encodedSnapshots(t, rawRecoverySnapshot(secondSnapshotID, secondContentID, "2026-08-25T12:00:00Z")), nil
			})
			selected, err := runner.ResolveSnapshot(context.Background(), target, requested)
			if err != nil {
				t.Fatal(err)
			}
			if selected.ID != secondSnapshotID || len(*calls) != 1 {
				t.Fatalf("selected/calls = %#v/%d", selected, len(*calls))
			}
			if requested == "" && !slices.Contains((*calls)[0].arguments, "--latest") {
				t.Fatal("latest query omitted --latest")
			}
			if requested != "" && !slices.Contains((*calls)[0].arguments, requested) {
				t.Fatal("explicit query omitted snapshot ID")
			}
		})
	}
	if _, err := (Runner{}).ResolveSnapshot(context.Background(), config.Target{}, "short"); err == nil {
		t.Fatal("short snapshot ID was accepted")
	}
}

func TestPruneIsOneForgetCallWithFixedRetention(t *testing.T) {
	for _, apply := range []bool{false, true} {
		apply := apply
		t.Run(map[bool]string{false: "dry-run", true: "apply"}[apply], func(t *testing.T) {
			runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
				if operation(arguments) != "forget" {
					t.Fatalf("unexpected operation: %#v", arguments)
				}
				return []byte(`[{"host":"` + SnapshotHost + `","paths":["` + SnapshotPath + `"],"keep":[{"id":"` + firstSnapshotID + `"}],"remove":[{"id":"` + secondSnapshotID + `"}]}]`), nil
			})
			result, err := runner.Prune(context.Background(), target, apply)
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 1 || result.Applied != apply || len(result.Groups) != 1 {
				t.Fatalf("calls/result = %d/%#v", len(*calls), result)
			}
			arguments := (*calls)[0].arguments
			for _, required := range []string{"--prune", "--group-by", "host,paths", "--keep-within", "24h", "--keep-hourly", "48", "--keep-daily", "14", "--keep-weekly", "8", "--keep-monthly", "12", "--keep-yearly", "3"} {
				if !slices.Contains(arguments, required) {
					t.Fatalf("missing retention argument %q", required)
				}
			}
			if slices.Contains(arguments, "--dry-run") == apply {
				t.Fatalf("dry-run mismatch: %#v", arguments)
			}
		})
	}
}

func TestRestoreUsesExactSnapshotSubfolderAndCallerContext(t *testing.T) {
	type contextKey string
	const key contextKey = "request"
	runner, target, calls := testRunner(t, func(ctx context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
		if ctx.Value(key) != "kept" {
			t.Fatal("caller context value was lost")
		}
		if operation(arguments) != "restore" {
			t.Fatalf("unexpected operation: %#v", arguments)
		}
		return []byte(`{}`), nil
	})
	destination := privateResticDirectory(t, "restore")
	ctx := context.WithValue(context.Background(), key, "kept")
	if err := runner.Restore(ctx, target, firstSnapshotID, destination); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !slices.Contains((*calls)[0].arguments, firstSnapshotID+":"+SnapshotPath) {
		t.Fatalf("restore arguments = %#v", (*calls)[0].arguments)
	}
}

func TestMalformedMetadataAndNonzeroExitFailWithoutGuessingStderr(t *testing.T) {
	runner, target, _ := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
		if operation(arguments) == "snapshots" {
			return []byte(`[{"id":"short"}]`), nil
		}
		return nil, &OperationError{Code: "command_failed", ExitCode: 12}
	})
	if _, _, err := runner.Snapshots(context.Background(), target, 100); err == nil {
		t.Fatal("malformed metadata was accepted")
	}
	runner.execute = func(context.Context, string, []string, []string, string) ([]byte, error) {
		return []byte("English text must not be classified"), &OperationError{Code: "command_failed", ExitCode: 1}
	}
	err := runner.Init(context.Background(), target)
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "command_failed" || operationError.ExitCode != 1 {
		t.Fatalf("error = %#v", err)
	}
}

func TestStructuredResticLogsExcludeArgumentsAndCredentials(t *testing.T) {
	runner, target, _ := testRunner(t, func(_ context.Context, _ string, _ []string, _ []string, _ string) ([]byte, error) {
		return []byte(`{}`), nil
	})
	var output bytes.Buffer
	runner.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	if err := runner.Init(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	log := output.String()
	for _, required := range []string{`"operation":"init"`, `"target":"primary"`, `"exit_code":0`} {
		if !strings.Contains(log, required) {
			t.Fatalf("structured log is missing %s: %s", required, log)
		}
	}
	for _, forbidden := range []string{target.Repository, "synthetic-access", "synthetic-secret", "synthetic-restic-password", "--password-file"} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("structured log exposed %q: %s", forbidden, log)
		}
	}
}

func privateResticDirectory(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
