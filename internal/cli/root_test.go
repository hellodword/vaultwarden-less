package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
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
func (repository *fakeRepository) Snapshots(context.Context, config.Target, int) ([]restic.Snapshot, bool, error) {
	return repository.snapshots, false, repository.record("snapshots")
}
func (repository *fakeRepository) Init(context.Context, config.Target) error {
	return repository.record("init")
}
func (repository *fakeRepository) Unlock(context.Context, config.Target) error {
	return repository.record("unlock")
}
func (repository *fakeRepository) Check(context.Context, config.Target) error {
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

func TestCLIExposesSupportedCommandSetAndDevelopmentVersion(t *testing.T) {
	setBuildIdentity(t, "", "development", "false")
	command := newCommand(io.Discard, io.Discard, testEnvironment(nil, nil))
	names := make([]string, 0, len(command.Commands()))
	for _, child := range command.Commands() {
		if child.Name() != "help" {
			names = append(names, child.Name())
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"config", "healthcheck", "repo", "request-backup", "restore", "serve", "status", "version"}) {
		t.Fatalf("command set = %#v", names)
	}
	stdout, stderr, code := executeCommand(testEnvironment(nil, nil), "version")
	if code != 0 || stderr != "" {
		t.Fatalf("development version = %d/%q/%q", code, stdout, stderr)
	}
	var version versionResult
	decodeSingleJSON(t, stdout, &version)
	if version.ProjectCommit != nil || version.Release != "development" || version.Clean {
		t.Fatalf("development version = %#v", version)
	}
}

func TestCLICleanVersionIsSemanticJSON(t *testing.T) {
	commit := strings.Repeat("a", 40)
	setBuildIdentity(t, commit, "git-"+commit, "true")
	stdout, stderr, code := executeCommand(testEnvironment(nil, nil), "version")
	if code != 0 || stderr != "" {
		t.Fatalf("clean version = %d/%q/%q", code, stdout, stderr)
	}
	var version versionResult
	decodeSingleJSON(t, stdout, &version)
	if version.ProjectCommit == nil || *version.ProjectCommit != commit || version.Release != "git-"+commit || !version.Clean {
		t.Fatalf("clean version = %#v", version)
	}
}

func TestCLIClassifiesUsageAndRepositoryFailures(t *testing.T) {
	for _, arguments := range [][]string{
		nil,
		{"unknown"},
		{"serve", "extra"},
		{"config"},
		{"config", "validate"},
		{"repo"},
		{"repo", "snapshots"},
		{"restore"},
		{"restore", "--target", "primary", "--output", "/restore/drill"},
	} {
		stdout, stderr, code := executeCommand(testEnvironment(nil, nil), arguments...)
		if code != 2 || stdout != "" || errorCode(t, stderr) != codeUsageError {
			t.Errorf("arguments %#v = %d/%q/%q", arguments, code, stdout, stderr)
		}
	}

	repository := &fakeRepository{error: errors.New("repository and secret diagnostics must not escape")}
	stdout, stderr, code := executeCommand(testEnvironment(repository, nil), "repo", "snapshots", "--target", "primary")
	if code != 1 || stdout != "" || strings.Contains(stderr, "secret") || errorCode(t, stderr) != codeRepositoryFailure {
		t.Fatalf("repository failure = %d/%q/%q", code, stdout, stderr)
	}
}

func TestConfigValidateRolesReturnsOrderedTargets(t *testing.T) {
	for _, role := range []string{"backup", "maintenance"} {
		environment := testEnvironment(nil, nil)
		var directories []string
		environment.validateDirectory = func(path string) error {
			directories = append(directories, path)
			return nil
		}
		stdout, stderr, code := executeCommand(environment, "config", "validate", "--role", role)
		if code != 0 || stderr != "" {
			t.Fatalf("config validate %s = %d/%q/%q", role, code, stdout, stderr)
		}
		var result configValidationResult
		decodeSingleJSON(t, stdout, &result)
		if !result.Valid || result.Role != role || !slices.Equal(result.Targets, []string{"primary"}) {
			t.Fatalf("config validate result = %#v", result)
		}
		wantDirectories := []string{"/cache", "/restore"}
		if role == "backup" {
			wantDirectories = []string{"/data", "/staging", "/cache"}
		}
		if !slices.Equal(directories, wantDirectories) {
			t.Fatalf("config validate %s directories = %#v", role, directories)
		}
	}

	environment := testEnvironment(nil, nil)
	environment.loadBackup = func(string) (*config.Backup, error) {
		return &config.Backup{
			Targets:   []config.Target{},
			Notifiers: []config.Notifier{},
		}, nil
	}
	environment.loadCredentials = func(string, *config.Backup) (*config.RoleCredentials, error) {
		return &config.RoleCredentials{Targets: map[string]config.RoleTarget{}}, nil
	}
	for _, role := range []string{"backup", "maintenance"} {
		stdout, stderr, code := executeCommand(environment, "config", "validate", "--role", role)
		if code != 0 || stderr != "" {
			t.Fatalf("targetless config validate %s = %d/%q/%q", role, code, stdout, stderr)
		}
		var result configValidationResult
		decodeSingleJSON(t, stdout, &result)
		if result.Targets == nil || len(result.Targets) != 0 {
			t.Fatalf("targetless result = %#v", result)
		}
	}
}

func TestTargetlessReturnsBeforeRepositoryFactory(t *testing.T) {
	called := false
	environment := testEnvironment(nil, nil)
	environment.repository = func(*config.Backup) (repository, error) {
		called = true
		return nil, errors.New("must not construct repository")
	}
	if _, err := environment.repositoryFor(&config.Backup{Targets: []config.Target{}}); err == nil || called {
		t.Fatalf("targetless repository factory = err:%v called:%t", err, called)
	}

	environment.loadBackup = func(string) (*config.Backup, error) {
		return &config.Backup{Targets: []config.Target{}, Notifiers: []config.Notifier{}}, nil
	}
	stdout, stderr, code := executeCommand(environment, "repo", "snapshots", "--target", "primary")
	if code != 2 || stdout != "" || errorCode(t, stderr) != codeUsageError || called {
		t.Fatalf("targetless repo command = %d/%q/%q/called=%t", code, stdout, stderr, called)
	}
}

func TestWorkerClientCommandsValidateSemanticResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case worker.CheckPath:
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte("{\"accepted\":true}\n"))
		case worker.StatusPath:
			_, _ = writer.Write([]byte("{\"targets\":[],\"last_check\":null,\"next_check_at\":null,\"started_at\":\"2026-08-27T12:00:00.000Z\",\"state\":\"idle\",\"future_field\":true}"))
		case worker.HealthPath:
			writer.Header().Del("Content-Type")
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	environment := testEnvironment(nil, server)
	for _, command := range []string{"request-backup", "status", "healthcheck"} {
		stdout, stderr, code := executeCommand(environment, command)
		if code != 0 || stderr != "" || stdout == "" {
			t.Fatalf("%s = %d/%q/%q", command, code, stdout, stderr)
		}
		var result map[string]any
		decodeSingleJSON(t, stdout, &result)
	}
}

func TestRequestBackupPreservesTargetlessNotReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte(`{"error":{"code":"not_ready","message":"` + worker.NoBackupTargetsMessage + `"}}` + "\n"))
	}))
	defer server.Close()
	stdout, stderr, code := executeCommand(testEnvironment(nil, server), "request-backup")
	if code != 1 || stdout != "" || errorCode(t, stderr) != codeNotReady {
		t.Fatalf("targetless request-backup = %d/%q/%q", code, stdout, stderr)
	}
}

func TestRepositoryCommandsAndRestore(t *testing.T) {
	for _, arguments := range [][]string{
		{"repo", "init", "--target", "primary"},
		{"repo", "check", "--target", "primary"},
		{"repo", "unlock", "--target", "primary"},
		{"repo", "prune", "--target", "primary"},
	} {
		stdout, stderr, code := executeCommand(testEnvironment(&fakeRepository{}, nil), arguments...)
		if code != 0 || stderr != "" {
			t.Fatalf("%#v = %d/%q/%q", arguments, code, stdout, stderr)
		}
		var result map[string]any
		decodeSingleJSON(t, stdout, &result)
	}

	restore := func(_ context.Context, _ restoretool.Repository, request restoretool.Request) (restoretool.Result, error) {
		return restoretool.Result{
			Target: request.Target.Name, SnapshotID: strings.Repeat("a", 64),
			ProjectCommit: strings.Repeat("d", 40), ContentID: "sha256:" + strings.Repeat("b", 64),
			ArchiveID: "sha256:" + strings.Repeat("c", 64), Output: request.Output,
		}, nil
	}
	stdout, stderr, code := executeCommand(testEnvironment(&fakeRepository{}, nil, restore), "restore", "--target", "primary", "--output", "drill")
	if code != 0 || stderr != "" {
		t.Fatalf("restore = %d/%q/%q", code, stdout, stderr)
	}
	var result map[string]any
	decodeSingleJSON(t, stdout, &result)
	if result["target"] != "primary" || result["output"] != "/restore/drill" {
		t.Fatalf("restore result = %#v", result)
	}
}

func testEnvironment(repositoryClient repository, server *httptest.Server, restoreOverrides ...restoreFunc) commandEnvironment {
	if repositoryClient == nil {
		repositoryClient = &fakeRepository{}
	}
	environment := commandEnvironment{
		loadBackup: func(string) (*config.Backup, error) {
			return &config.Backup{
				Targets:   []config.Target{{Name: "primary", Repository: "s3:https://objects.example.test/bucket", Options: []config.Option{}}},
				Notifiers: []config.Notifier{},
			}, nil
		},
		loadCredentials: func(string, *config.Backup) (*config.RoleCredentials, error) {
			return &config.RoleCredentials{Targets: map[string]config.RoleTarget{
				"primary": {Environment: map[string]string{"AWS_ACCESS_KEY_ID": "test"}},
			}}, nil
		},
		validatePasswords: func(string, *config.Backup) error { return nil },
		validateDirectory: func(string) error { return nil },
		repository:        func(*config.Backup) (repository, error) { return repositoryClient, nil },
		restore: func(_ context.Context, _ restoretool.Repository, _ restoretool.Request) (restoretool.Result, error) {
			return restoretool.Result{}, errors.New("restore should not be called")
		},
		httpClient:      &http.Client{Timeout: time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		workerURL:       "http://127.0.0.1:1",
		credentialsPath: "/credentials/role.yaml",
		passwordRoot:    "/credentials/restic",
		resticBinary:    "restic",
		dataDir:         "/data",
		stagingDir:      "/staging",
		cacheDir:        "/cache",
		restoreRoot:     "/restore",
		now:             func() time.Time { return time.Date(2026, 8, 27, 12, 34, 56, 789_000_000, time.UTC) },
		ready:           func() bool { return true },
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
	err := Execute(command, arguments)
	WriteError(&stderr, err)
	return stdout.String(), stderr.String(), ExitCode(err)
}

func errorCode(t *testing.T, output string) string {
	t.Helper()
	var document errorDocument
	decodeSingleJSON(t, output, &document)
	if document.Error.Message == "" {
		t.Fatalf("error message is empty: %q", output)
	}
	return document.Error.Code
}

func decodeSingleJSON(t *testing.T, input string, destination any) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(input))
	if err := decoder.Decode(destination); err != nil {
		t.Fatalf("decode JSON %q: %v", input, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON contains a second value: %q", input)
	}
}

func setBuildIdentity(t *testing.T, commit, release, clean string) {
	t.Helper()
	oldCommit, oldRelease, oldClean := buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean
	buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = commit, release, clean
	t.Cleanup(func() { buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = oldCommit, oldRelease, oldClean })
}
