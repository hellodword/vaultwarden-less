package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

var (
	firstSnapshotID  = strings.Repeat("a", 64)
	secondSnapshotID = strings.Repeat("b", 64)
	firstCommit      = strings.Repeat("1", 40)
	firstContentID   = "sha256:" + strings.Repeat("c", 64)
	secondContentID  = "sha256:" + strings.Repeat("d", 64)
	firstArchiveID   = "sha256:" + strings.Repeat("e", 64)
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
		Options: []config.Option{
			{Name: "s3.region", Value: "ap-singapore"},
			{Name: "s3.bucket-lookup", Value: "path"},
		},
	}
	credentials := &config.RoleCredentials{
		Targets: map[string]config.RoleTarget{
			"primary": {Environment: map[string]string{
				"AWS_ACCESS_KEY_ID": "synthetic-access", "AWS_SECRET_ACCESS_KEY": "synthetic-secret",
			}},
		},
	}
	calls := []recordedCall{}
	runner := NewRunner("/usr/local/bin/restic", cache, credentialsDir, config.RecoveryPointPath, credentials, nil)
	runner.execute = func(ctx context.Context, binary string, arguments, environment []string, directory string) ([]byte, error) {
		if binary != runner.binary {
			t.Fatalf("binary = %s", binary)
		}
		calls = append(calls, recordedCall{slices.Clone(arguments), slices.Clone(environment), directory})
		return execute(ctx, binary, arguments, environment, directory)
	}
	return runner, target, &calls
}

func manifestWithContent(t *testing.T, hash string) snapshot.Manifest {
	t.Helper()
	manifest, err := snapshot.NewManifest(
		firstCommit,
		"sha256:"+hash,
		firstArchiveID,
		[]snapshot.File{{Path: "db.sqlite3", Size: 1, SHA256: "sha256:" + strings.Repeat("0", 64)}},
	)
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
		Tags: []string{
			"project_commit=" + firstCommit,
			"content_id=" + content,
			"archive_id=" + firstArchiveID,
		},
		Paths: []string{config.RecoveryPointPath},
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
	if result.Created || result.SnapshotID != firstSnapshotID || result.ContentID != manifest.ContentID || result.ArchiveID != firstArchiveID {
		t.Fatalf("result = %#v", result)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d", len(*calls))
	}
}

func TestEnsureDeduplicatesByContentButRetainsTargetArchiveIdentity(t *testing.T) {
	manifest := manifestWithContent(t, strings.Repeat("c", 64))
	manifest.ArchiveID = "sha256:" + strings.Repeat("9", 64)
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
	if result.Created || result.ContentID != manifest.ContentID || result.ArchiveID != firstArchiveID || len(*calls) != 1 {
		t.Fatalf("deduplicated result/calls = %#v/%d", result, len(*calls))
	}
}

func TestEnsureBacksUpDifferentContentOnceWithFixedIdentity(t *testing.T) {
	manifest := manifestWithContent(t, strings.Repeat("d", 64))
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, environment []string, _ string) ([]byte, error) {
		switch operation(arguments) {
		case "snapshots":
			return encodedSnapshots(t, rawRecoverySnapshot(firstSnapshotID, firstContentID, "2026-08-25T12:00:00Z")), nil
		case "backup":
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
	if !result.Created || result.SnapshotID != secondSnapshotID || result.ContentID != manifest.ContentID || result.ArchiveID != manifest.ArchiveID {
		t.Fatalf("result = %#v", result)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %d", len(*calls))
	}
	wantArguments := []string{
		"--repo", target.Repository,
		"--password-file", filepath.Join(runner.passwordRoot, target.Name),
		"--cache-dir", filepath.Join(runner.cacheDir, target.Name),
		"--option", "s3.region=ap-singapore",
		"--option", "s3.bucket-lookup=path",
		"backup", "--json",
		"--host", SnapshotHost,
		"--tag", "project_commit=" + manifest.ProjectCommit,
		"--tag", "content_id=" + manifest.ContentID,
		"--tag", "archive_id=" + manifest.ArchiveID,
		"--", config.RecoveryPointPath,
	}
	if arguments := (*calls)[1].arguments; !slices.Equal(arguments, wantArguments) {
		t.Fatalf("backup arguments = %#v, want %#v", arguments, wantArguments)
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
		"HOME":                  "/nonexistent",
		"SSL_CERT_FILE":         "/etc/ssl/certs/ca-bundle.crt",
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
	if items[0].Time != "2026-08-25T12:00:00.000Z" || items[1].Time != "2026-08-25T12:00:00.000Z" {
		t.Fatalf("snapshot times were not normalized: %#v", items)
	}
	arguments := (*calls)[0].arguments
	latest := slices.Index(arguments, "--latest")
	if latest < 0 || latest+1 >= len(arguments) || arguments[latest+1] != "3" {
		t.Fatalf("bounded query arguments = %#v", arguments)
	}
}

func TestSnapshotsAcceptAdditionalUnrelatedTags(t *testing.T) {
	t.Parallel()
	raw := rawRecoverySnapshot(firstSnapshotID, firstContentID, "2026-08-25T12:00:00Z")
	raw.Tags = append(raw.Tags, "retention=gold", "operator-note")
	items, err := parseSnapshots(encodedSnapshots(t, raw), config.RecoveryPointPath)
	if err != nil || len(items) != 1 || items[0].ID != firstSnapshotID {
		t.Fatalf("snapshot with unrelated tags = %#v/%v", items, err)
	}
}

func TestResolveSnapshotUsesLatestOrOneExactFullID(t *testing.T) {
	for name, requested := range map[string]string{"latest": "", "explicit": secondSnapshotID} {
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
	if _, err := NewRunner("", "", "", "", nil, nil).ResolveSnapshot(context.Background(), config.Target{}, "short"); err == nil {
		t.Fatal("short snapshot ID was accepted")
	}
}

func TestPruneIsOneForgetCallWithFixedRetention(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "dry-run", true: "apply"}[apply], func(t *testing.T) {
			runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
				switch operation(arguments) {
				case "snapshots":
					return []byte("[]\n"), nil
				case "forget":
					return []byte(`[{"host":"` + SnapshotHost + `","paths":["` + config.RecoveryPointPath + `"],"keep":[{"id":"` + firstSnapshotID + `"}],"remove":[{"id":"` + secondSnapshotID + `"}]}]`), nil
				default:
					t.Fatalf("unexpected operation: %#v", arguments)
					return nil, nil
				}
			})
			result, err := runner.Prune(context.Background(), target, apply)
			if err != nil {
				t.Fatal(err)
			}
			if len(*calls) != 2 || result.Applied != apply || len(result.Groups) != 1 {
				t.Fatalf("calls/result = %d/%#v", len(*calls), result)
			}
			arguments := (*calls)[1].arguments
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

func TestPruneRejectsMalformedIdentityBeforeForget(t *testing.T) {
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, _ []string, _ string) ([]byte, error) {
		if operation(arguments) != "snapshots" {
			t.Fatalf("mutating operation ran after malformed preflight: %#v", arguments)
		}
		malformed := rawRecoverySnapshot(firstSnapshotID, firstContentID, "2026-08-27T12:00:00Z")
		malformed.Tags = malformed.Tags[:2]
		return encodedSnapshots(t, malformed), nil
	})
	if _, err := runner.Prune(context.Background(), target, true); err == nil {
		t.Fatal("snapshot missing a current identity tag was accepted")
	}
	if len(*calls) != 1 || operation((*calls)[0].arguments) != "snapshots" {
		t.Fatalf("preflight calls = %#v", *calls)
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
		return []byte(`{"message_type":"summary","total_files":1,"files_restored":1,"total_bytes":1,"bytes_restored":1}` + "\n"), nil
	})
	destination := privateResticDirectory(t, "restore")
	ctx := context.WithValue(context.Background(), key, "kept")
	if err := runner.Restore(ctx, target, firstSnapshotID, destination); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !slices.Contains((*calls)[0].arguments, firstSnapshotID+":"+config.RecoveryPointPath) {
		t.Fatalf("restore arguments = %#v", (*calls)[0].arguments)
	}
}

func TestInitUsesExactRepositoryVersionTwoArguments(t *testing.T) {
	runner, target, calls := testRunner(t, func(_ context.Context, _ string, arguments, environment []string, _ string) ([]byte, error) {
		if operation(arguments) != "init" {
			t.Fatalf("unexpected operation: %#v", arguments)
		}
		assertExactCredentialEnvironment(t, environment)
		return []byte(`{"message_type":"initialized","id":"` + firstSnapshotID + `","repository":"redacted"}` + "\n"), nil
	})
	if err := runner.Init(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--repo", target.Repository,
		"--password-file", filepath.Join(runner.passwordRoot, target.Name),
		"--cache-dir", filepath.Join(runner.cacheDir, target.Name),
		"--option", "s3.region=ap-singapore",
		"--option", "s3.bucket-lookup=path",
		"init", "--repository-version", "2", "--json",
	}
	if len(*calls) != 1 || !slices.Equal((*calls)[0].arguments, want) {
		t.Fatalf("init calls = %#v, want one call with %#v", *calls, want)
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
		return []byte(`{"message_type":"initialized","id":"` + firstSnapshotID + `","repository":"redacted"}` + "\n"), nil
	})
	var output bytes.Buffer
	runner.logger = slog.New(slog.NewJSONHandler(&output, nil))
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

func TestCommandParsersBoundFramesAndPreserveForwardCompatibility(t *testing.T) {
	t.Parallel()
	valid := []byte(
		`{"message_type":"future_progress","unknown":{"value":1}}` + "\n" +
			`{"message_type":"status","percent_done":0.5,"new_field":true}` + "\n" +
			`{"message_type":"summary","snapshot_id":"` + firstSnapshotID + `","unknown":"allowed"}` + "\n",
	)
	if id, err := parseBackupSnapshotID(valid); err != nil || id != firstSnapshotID {
		t.Fatalf("forward-compatible backup output = %q/%v", id, err)
	}
	invalid := map[string][]byte{
		"truncated":       []byte(`{"message_type":"summary"`),
		"duplicate field": []byte(`{"message_type":"summary","snapshot_id":"` + firstSnapshotID + `","snapshot_id":"` + secondSnapshotID + `"}`),
		"summary reordered": []byte(
			`{"message_type":"summary","snapshot_id":"` + firstSnapshotID + `"}` + "\n" + `{"message_type":"status"}` + "\n",
		),
		"duplicate summary": []byte(
			`{"message_type":"summary","snapshot_id":"` + firstSnapshotID + `"}` + "\n" +
				`{"message_type":"summary","snapshot_id":"` + secondSnapshotID + `"}` + "\n",
		),
		"invalid UTF-8": {0xff, '\n'},
		"oversized frame": append(
			append([]byte(`{"message_type":"status","padding":"`), bytes.Repeat([]byte{'x'}, maxJSONFrame)...),
			[]byte(`"}`)...,
		),
	}
	for name, data := range invalid {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseBackupSnapshotID(data); err == nil {
				t.Fatal("invalid framed output was accepted")
			}
		})
	}
}

func TestSummaryParsersRejectIncompleteOrErrorResults(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"missing":   `{"message_type":"status"}` + "\n",
		"error":     `{"message_type":"error","message":"unsafe raw diagnostic"}` + "\n",
		"check":     `{"message_type":"summary","num_errors":1}` + "\n",
		"restore":   `{"message_type":"summary","total_files":2,"files_restored":1,"total_bytes":2,"bytes_restored":1}` + "\n",
		"after":     `{"message_type":"summary","num_errors":0}` + "\n" + `{"message_type":"future"}` + "\n",
		"wrongtype": `{"message_type":"summary","num_errors":"0"}` + "\n",
	} {
		name, data := name, data
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			operation := "check"
			if name == "restore" {
				operation = "restore"
			}
			if err := parseFinalSummary([]byte(data), operation); err == nil {
				t.Fatal("invalid command summary was accepted")
			}
		})
	}
}

func TestSubprocessBoundsStderrTimeoutCancellationAndRedaction(t *testing.T) {
	runner := NewRunner("", "", "", "", nil, nil)
	runHelper := func(ctx context.Context, mode string) ([]byte, error) {
		return runner.run(ctx, os.Args[0], []string{"-test.run=TestResticHelperProcess", "--", mode}, []string{"GO_WANT_RESTIC_HELPER=1"}, "")
	}
	for range 100 {
		output, err := runHelper(context.Background(), "small-output")
		if err != nil || string(output) != "small deterministic output\n" {
			t.Fatalf("small subprocess output = %q/%v", output, err)
		}
	}

	_, err := runHelper(context.Background(), "stderr-secret")
	var operationError *OperationError
	if !errors.As(err, &operationError) || operationError.Code != "command_failed" || strings.Contains(err.Error(), "sensitive-diagnostic") {
		t.Fatalf("stderr failure was not safely classified: %#v", err)
	}

	_, err = runHelper(context.Background(), "stderr-limit")
	if !errors.As(err, &operationError) || operationError.Code != "output_limit" || strings.Contains(err.Error(), "sensitive-diagnostic") {
		t.Fatalf("stderr limit was not safely classified: %#v", err)
	}

	timeoutContext, timeoutCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer timeoutCancel()
	started := time.Now()
	_, err = runHelper(timeoutContext, "wait")
	if !errors.As(err, &operationError) || operationError.Code != "timeout" || time.Since(started) > 3*time.Second {
		t.Fatalf("timeout = %#v after %s", err, time.Since(started))
	}

	cancelContext, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = runHelper(cancelContext, "wait-with-child")
	if !errors.As(err, &operationError) || operationError.Code != "cancelled" {
		t.Fatalf("cancellation = %#v", err)
	}
}

func TestResticHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RESTIC_HELPER") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "small-output":
		_, _ = fmt.Fprintln(os.Stdout, "small deterministic output")
		os.Exit(0)
	case "stderr-secret":
		_, _ = fmt.Fprint(os.Stderr, "sensitive-diagnostic")
		os.Exit(7)
	case "stderr-limit":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("sensitive-diagnostic"), 8192))
		os.Exit(7)
	case "wait":
		time.Sleep(30 * time.Second)
	case "wait-with-child":
		child := exec.Command(os.Args[0], "-test.run=TestResticHelperProcess", "--", "wait")
		child.Env = []string{"GO_WANT_RESTIC_HELPER=1"}
		if err := child.Start(); err != nil {
			os.Exit(8)
		}
		_ = child.Wait()
	default:
		os.Exit(9)
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
