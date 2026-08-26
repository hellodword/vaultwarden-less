package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/version"
	"github.com/hellodword/vaultwarden-less/internal/worker"
)

type fakeRepository struct {
	error     error
	snapshots []restic.Snapshot
	prune     restic.PruneResult
	calls     []string
}

func (repository *fakeRepository) record(name string) error {
	repository.calls = append(repository.calls, name)
	return repository.error
}

func (repository *fakeRepository) VerifyVersion(context.Context) error {
	return repository.record("version")
}
func (repository *fakeRepository) Ensure(context.Context, config.Target, snapshot.Manifest) (restic.SyncResult, error) {
	return restic.SyncResult{}, repository.record("ensure")
}
func (repository *fakeRepository) Snapshots(context.Context, config.Target) ([]restic.Snapshot, error) {
	return repository.snapshots, repository.record("snapshots")
}
func (repository *fakeRepository) Init(context.Context, config.Target) error {
	return repository.record("init")
}
func (repository *fakeRepository) Unlock(context.Context, config.Target) error {
	return repository.record("unlock")
}
func (repository *fakeRepository) Check(context.Context, config.Target, string) error {
	return repository.record("check")
}
func (repository *fakeRepository) Prune(context.Context, config.Target, bool) (restic.PruneResult, error) {
	return repository.prune, repository.record("prune")
}
func (repository *fakeRepository) ResolveSnapshot(context.Context, config.Target, string) (restic.Snapshot, error) {
	return restic.Snapshot{}, repository.record("resolve")
}
func (repository *fakeRepository) Restore(context.Context, config.Target, string, string) error {
	return repository.record("restore")
}

func TestCLIHelpVersionAndRemovedCommands(t *testing.T) {
	t.Parallel()
	environment := testEnvironment(nil, nil)
	stdout, stderr, code := executeCommand(environment, "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "request-backup") || !strings.Contains(stdout, "config") {
		t.Fatalf("help = %d/%q/%q", code, stdout, stderr)
	}
	for _, removed := range []string{"rebuild", "inspect"} {
		if strings.Contains(stdout, removed) {
			t.Errorf("help still exposes %q", removed)
		}
	}
	stdout, stderr, code = executeCommand(environment, "--version")
	if code != 0 || stderr != "" || stdout != "vaultwarden-less-backup "+version.Product+"\n" {
		t.Fatalf("version = %d/%q/%q", code, stdout, stderr)
	}
}

func TestCLIUsageAndRuntimeFailuresKeepStdoutEmpty(t *testing.T) {
	t.Parallel()
	for _, arguments := range [][]string{
		nil,
		{"unknown"},
		{"serve", "extra"},
		{"config"},
		{"repo"},
		{"repo", "snapshots"},
		{"repo", "check", "--target", "primary", "--read-data-subset", "0/12"},
		{"restore"},
		{"restore", "--target", "primary", "--output", "/restore/drill", "--snapshot", "short"},
	} {
		stdout, stderr, code := executeCommand(testEnvironment(nil, nil), arguments...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("arguments %#v = %d/%q/%q", arguments, code, stdout, stderr)
		}
	}
	repository := &fakeRepository{error: errors.New("repository failed")}
	stdout, stderr, code := executeCommand(testEnvironment(repository, nil), "repo", "snapshots", "--target", "primary")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "repository failed") {
		t.Fatalf("runtime failure = %d/%q/%q", code, stdout, stderr)
	}
}

func TestConfigValidateOutputsOneSecretFreeDocument(t *testing.T) {
	t.Parallel()
	stdout, stderr, code := executeCommand(testEnvironment(nil, nil), "config", "validate")
	if code != 0 || stderr != "" {
		t.Fatalf("config validate = %d/%q", code, stderr)
	}
	assertOneJSON(t, stdout)
	if strings.Contains(stdout, "access") || strings.Contains(stdout, "secret") || !strings.Contains(stdout, `"valid":true`) || !strings.Contains(stdout, `"warnings":[]`) {
		t.Fatalf("config output is not secret-free or complete: %s", stdout)
	}
}

func TestWorkerClientCommandsHaveStrictJSONAndNoSecondDocument(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case worker.CheckPath:
			body, _ := io.ReadAll(request.Body)
			if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" || string(body) != "{\"schema_version\":1}\n" {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte("{\"schema_version\":1,\"accepted\":true}\n"))
		case worker.StatusPath:
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte("{\"schema_version\":1,\"state\":\"idle\",\"started_at\":\"2026-08-25T12:00:00Z\",\"next_check_at\":null,\"last_check\":null,\"targets\":[]}"))
		case worker.HealthPath:
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	environment := testEnvironment(nil, server)
	for _, command := range []string{"request-backup", "status"} {
		stdout, stderr, code := executeCommand(environment, command)
		if code != 0 || stderr != "" {
			t.Fatalf("%s = %d/%q", command, code, stderr)
		}
		assertOneJSON(t, stdout)
	}
	stdout, stderr, code := executeCommand(environment, "healthcheck")
	if code != 0 || stdout != "" || stderr != "" {
		t.Fatalf("healthcheck = %d/%q/%q", code, stdout, stderr)
	}
}

func TestRepositoryCommandsOutputOneDocument(t *testing.T) {
	t.Parallel()
	contentID := "sha256:" + strings.Repeat("b", 64)
	for _, test := range []struct {
		arguments []string
		contains  string
	}{
		{[]string{"repo", "init", "--target", "primary"}, `"operation":"init"`},
		{[]string{"repo", "snapshots", "--target", "primary"}, `"snapshots":[`},
		{[]string{"repo", "check", "--target", "primary", "--read-data-subset", "2/12"}, `"operation":"check"`},
		{[]string{"repo", "unlock", "--target", "primary"}, `"operation":"unlock"`},
		{[]string{"repo", "prune", "--target", "primary"}, `"applied":false`},
	} {
		repository := &fakeRepository{
			snapshots: []restic.Snapshot{{ID: strings.Repeat("a", 64), Time: "2026-08-25T12:00:00Z", ContentID: contentID}},
			prune:     restic.PruneResult{Applied: false, Groups: []restic.PruneGroup{{Host: "vaultwarden-less", Paths: []string{"/staging/point"}, KeepSnapshotIDs: []string{}, RemoveSnapshotIDs: []string{}}}},
		}
		stdout, stderr, code := executeCommand(testEnvironment(repository, nil), test.arguments...)
		if code != 0 || stderr != "" || !strings.Contains(stdout, test.contains) {
			t.Fatalf("%#v = %d/%q/%q", test.arguments, code, stdout, stderr)
		}
		assertOneJSON(t, stdout)
	}
}

func TestRestoreCommandOutputsExactResultShape(t *testing.T) {
	t.Parallel()
	contentID := "sha256:" + strings.Repeat("b", 64)
	snapshotID := strings.Repeat("a", 64)
	restore := func(_ context.Context, _ restoretool.Repository, request restoretool.Request) (restoretool.Result, error) {
		return restoretool.Result{
			SchemaVersion: version.SchemaVersion, Target: request.Target.Name, SnapshotID: snapshotID,
			ContentID: contentID, Output: request.Output,
		}, nil
	}
	stdout, stderr, code := executeCommand(testEnvironment(&fakeRepository{}, nil, restore), "restore", "--target", "primary", "--output", "/restore/drill")
	if code != 0 || stderr != "" {
		t.Fatalf("restore = %d/%q", code, stderr)
	}
	assertOneJSON(t, stdout)
	if !strings.Contains(stdout, `"snapshot_id":"`+snapshotID+`"`) || !strings.Contains(stdout, `"output":"/restore/drill"`) {
		t.Fatalf("restore output = %s", stdout)
	}
}

func testEnvironment(repositoryClient repository, server *httptest.Server, restoreOverrides ...restoreFunc) commandEnvironment {
	if repositoryClient == nil {
		repositoryClient = &fakeRepository{}
	}
	environment := commandEnvironment{
		loadBackup: func(string) (*config.Backup, error) {
			return &config.Backup{SchemaVersion: 1, Targets: []config.Target{{Name: "primary", Repository: "s3:https://objects.example.test/bucket"}}, Notifiers: []config.Notifier{}}, nil
		},
		loadCredentials: func(string, *config.Backup) (*config.RoleCredentials, error) {
			return &config.RoleCredentials{SchemaVersion: 1, Targets: map[string]config.S3Credentials{"primary": {AccessKeyID: "test", SecretAccessKey: "test"}}}, nil
		},
		readSecret:     func(string) (string, error) { return "test-password", nil },
		validateNotify: func([]config.Notifier) error { return nil },
		repository:     func(*config.Backup) (repository, error) { return repositoryClient, nil },
		restore: func(_ context.Context, _ restoretool.Repository, _ restoretool.Request) (restoretool.Result, error) {
			return restoretool.Result{}, errors.New("restore should not be called")
		},
		httpClient: &http.Client{Timeout: time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		workerURL:  "http://127.0.0.1:1", credentialsPath: "/credentials/role.yaml", passwordPath: "/credentials/password",
		resticBinary: "restic", dataDir: "/data", stagingDir: "/staging", cacheDir: "/cache", restoreRoot: "/restore",
	}
	if server != nil {
		environment.workerURL = server.URL
		environment.httpClient = server.Client()
	}
	if len(restoreOverrides) == 1 {
		environment.restore = restoreOverrides[0]
	}
	return environment
}

func executeCommand(environment commandEnvironment, arguments ...string) (string, string, int) {
	var stdout, stderr bytes.Buffer
	command := newCommand(&stdout, &stderr, environment)
	command.SetArgs(arguments)
	err := command.Execute()
	if err != nil {
		_, _ = fmt.Fprintln(&stderr, err)
	}
	return stdout.String(), stderr.String(), ExitCode(err)
}

func assertOneJSON(t *testing.T, output string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var document any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode output %q: %v", output, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("output contains a second document: %q", output)
	}
}
