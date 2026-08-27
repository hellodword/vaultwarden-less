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

func TestCLIExactCommandSetHelpAndDevelopmentVersion(t *testing.T) {
	setBuildIdentity(t, "", "development", "false")
	environment := testEnvironment(nil, nil)
	command := newCommand(io.Discard, io.Discard, environment)
	names := make([]string, 0, len(command.Commands()))
	for _, child := range command.Commands() {
		if child.Name() != "help" {
			names = append(names, child.Name())
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"config", "healthcheck", "repo", "request-backup", "restore", "rollback", "serve", "status", "upgrade", "version"}) {
		t.Fatalf("command set = %#v", names)
	}
	stdout, stderr, code := executeCommand(environment, "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "request-backup") || !strings.Contains(stdout, "upgrade") {
		t.Fatalf("help = %d/%q/%q", code, stdout, stderr)
	}
	stdout, stderr, code = executeCommand(environment, "version")
	if code != 0 || stderr != "" || stdout != "{\"schema_version\":1,\"project_commit\":null,\"release\":\"development\",\"clean\":false}\n" {
		t.Fatalf("version = %d/%q/%q", code, stdout, stderr)
	}
}

func TestCLICleanVersionGolden(t *testing.T) {
	commit := strings.Repeat("a", 40)
	setBuildIdentity(t, commit, "RELEASE.2026-08-27T12-34-56Z-gaaaaaaaaaaaa", "true")
	stdout, stderr, code := executeCommand(testEnvironment(nil, nil), "version")
	want := `{"schema_version":1,"project_commit":"` + commit + `","release":"RELEASE.2026-08-27T12-34-56Z-gaaaaaaaaaaaa","clean":true}` + "\n"
	if code != 0 || stderr != "" || stdout != want {
		t.Fatalf("clean version = %d/%q/%q", code, stdout, stderr)
	}
}

func TestCLIUsageRuntimeAndDirtyFailuresHaveOneErrorDocument(t *testing.T) {
	for _, arguments := range [][]string{
		nil,
		{"unknown"},
		{"serve", "extra"},
		{"config"},
		{"config", "validate"},
		{"repo"},
		{"repo", "snapshots"},
		{"repo", "check", "--target", "primary", "--read-data-subset", "1/12"},
		{"restore"},
		{"restore", "--target", "primary", "--output", "/restore/drill"},
	} {
		stdout, stderr, code := executeCommand(testEnvironment(nil, nil), arguments...)
		if code != 2 || stdout != "" {
			t.Errorf("arguments %#v = %d/%q/%q", arguments, code, stdout, stderr)
		}
		assertErrorJSON(t, stderr, codeUsageError)
	}

	repository := &fakeRepository{error: errors.New("repository and secret diagnostics must not escape")}
	stdout, stderr, code := executeCommand(testEnvironment(repository, nil), "repo", "snapshots", "--target", "primary")
	if code != 1 || stdout != "" || strings.Contains(stderr, "secret") {
		t.Fatalf("runtime failure = %d/%q/%q", code, stdout, stderr)
	}
	assertErrorJSON(t, stderr, codeRepositoryFailure)

	dirty := testEnvironment(repository, nil)
	dirty.ready = func() bool { return false }
	repository.calls = nil
	stdout, stderr, code = executeCommand(dirty, "repo", "init", "--target", "primary")
	if code != 1 || stdout != "" || len(repository.calls) != 0 {
		t.Fatalf("dirty command side effects = %d/%q/%q/%#v", code, stdout, stderr, repository.calls)
	}
	assertErrorJSON(t, stderr, codeNotReady)
}

func TestConfigValidateRoleGoldenAndFailureClassification(t *testing.T) {
	for _, role := range []string{"backup", "maintenance"} {
		stdout, stderr, code := executeCommand(testEnvironment(nil, nil), "config", "validate", "--role", role)
		want := `{"schema_version":1,"valid":true,"role":"` + role + `","targets":["primary"],"warnings":[]}` + "\n"
		if code != 0 || stderr != "" || stdout != want {
			t.Fatalf("config validate %s = %d/%q/%q", role, code, stdout, stderr)
		}
	}
	environment := testEnvironment(nil, nil)
	environment.validatePasswords = func(string, *config.Backup) error { return errors.New("sensitive password") }
	stdout, stderr, code := executeCommand(environment, "config", "validate", "--role", "backup")
	if code != 1 || stdout != "" || strings.Contains(stderr, "sensitive") {
		t.Fatalf("invalid credentials = %d/%q/%q", code, stdout, stderr)
	}
	assertErrorJSON(t, stderr, codeInvalidCredentials)
}

func TestWorkerClientCommandsHaveExactJSON(t *testing.T) {
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
			_, _ = writer.Write([]byte("{\"schema_version\":1,\"state\":\"idle\",\"started_at\":\"2026-08-27T12:00:00.000Z\",\"next_check_at\":null,\"last_check\":null,\"targets\":[]}"))
		case worker.HealthPath:
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	environment := testEnvironment(nil, server)
	for _, test := range []struct {
		command string
		want    string
	}{
		{"request-backup", "{\"schema_version\":1,\"accepted\":true}\n"},
		{"status", "{\"schema_version\":1,\"state\":\"idle\",\"started_at\":\"2026-08-27T12:00:00.000Z\",\"next_check_at\":null,\"last_check\":null,\"targets\":[]}\n"},
		{"healthcheck", "{\"schema_version\":1,\"healthy\":true}\n"},
	} {
		stdout, stderr, code := executeCommand(environment, test.command)
		if code != 0 || stderr != "" || stdout != test.want {
			t.Fatalf("%s = %d/%q/%q", test.command, code, stdout, stderr)
		}
	}
}

func TestRepositoryCommandsAndRestoreGolden(t *testing.T) {
	contentID := "sha256:" + strings.Repeat("b", 64)
	archiveID := "sha256:" + strings.Repeat("c", 64)
	commit := strings.Repeat("d", 40)
	snapshotID := strings.Repeat("a", 64)
	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"repo", "init", "--target", "primary"}, `{"schema_version":1,"operation":"init","target":"primary","success":true}` + "\n"},
		{[]string{"repo", "check", "--target", "primary"}, `{"schema_version":1,"operation":"check","target":"primary","success":true}` + "\n"},
		{[]string{"repo", "unlock", "--target", "primary"}, `{"schema_version":1,"operation":"unlock","target":"primary","success":true}` + "\n"},
		{[]string{"repo", "prune", "--target", "primary"}, `{"schema_version":1,"operation":"prune","target":"primary","applied":false,"groups":[{"host":"vaultwarden-less","paths":["/staging/point"],"keep_snapshot_ids":[],"remove_snapshot_ids":[]}]}` + "\n"},
	} {
		repository := &fakeRepository{prune: restic.PruneResult{Applied: false, Groups: []restic.PruneGroup{{Host: "vaultwarden-less", Paths: []string{"/staging/point"}, KeepSnapshotIDs: []string{}, RemoveSnapshotIDs: []string{}}}}}
		stdout, stderr, code := executeCommand(testEnvironment(repository, nil), test.arguments...)
		if code != 0 || stderr != "" || stdout != test.want {
			t.Fatalf("%#v = %d/%q/%q", test.arguments, code, stdout, stderr)
		}
	}
	repository := &fakeRepository{snapshots: []restic.Snapshot{{ID: snapshotID, Time: "2026-08-27T12:00:00.000Z", ProjectCommit: commit, ContentID: contentID, ArchiveID: archiveID}}}
	stdout, stderr, code := executeCommand(testEnvironment(repository, nil), "repo", "snapshots", "--target", "primary")
	wantSnapshots := `{"schema_version":1,"target":"primary","limit":100,"truncated":false,"snapshots":[{"id":"` + snapshotID + `","time":"2026-08-27T12:00:00.000Z","project_commit":"` + commit + `","content_id":"` + contentID + `","archive_id":"` + archiveID + `"}]}` + "\n"
	if code != 0 || stderr != "" || stdout != wantSnapshots {
		t.Fatalf("snapshots = %d/%q/%q", code, stdout, stderr)
	}

	restore := func(_ context.Context, _ restoretool.Repository, request restoretool.Request) (restoretool.Result, error) {
		return restoretool.Result{SchemaVersion: restoretool.ResultSchemaVersion, Target: request.Target.Name, SnapshotID: snapshotID, ContentID: contentID, Output: request.Output}, nil
	}
	stdout, stderr, code = executeCommand(testEnvironment(&fakeRepository{}, nil, restore), "restore", "--target", "primary", "--output", "drill")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"output":"/restore/drill"`) {
		t.Fatalf("restore = %d/%q/%q", code, stdout, stderr)
	}
}

func testEnvironment(repositoryClient repository, server *httptest.Server, restoreOverrides ...restoreFunc) commandEnvironment {
	if repositoryClient == nil {
		repositoryClient = &fakeRepository{}
	}
	environment := commandEnvironment{
		loadBackup: func(string) (*config.Backup, error) {
			return &config.Backup{
				SchemaVersion: config.SchemaVersion, UpgradeVerificationTargets: []string{"primary"},
				Targets: []config.Target{{Name: "primary", Repository: "s3:https://objects.example.test/bucket", Options: []config.Option{}}}, Notifiers: []config.Notifier{},
			}, nil
		},
		loadDeployment: func(string) (config.Deployment, error) { return config.Deployment{SchemaVersion: 1}, nil },
		loadCredentials: func(string, *config.Backup) (*config.RoleCredentials, error) {
			return &config.RoleCredentials{SchemaVersion: config.SchemaVersion, Targets: map[string]config.RoleTarget{"primary": {Environment: map[string]string{"AWS_ACCESS_KEY_ID": "test"}}}}, nil
		},
		loadNotifiers:     func(string, *config.Backup) ([]config.Notifier, error) { return []config.Notifier{}, nil },
		validatePasswords: func(string, *config.Backup) error { return nil },
		validateDirectory: func(string) error { return nil },
		repository:        func(*config.Backup) (repository, error) { return repositoryClient, nil },
		restore: func(_ context.Context, _ restoretool.Repository, _ restoretool.Request) (restoretool.Result, error) {
			return restoretool.Result{}, errors.New("restore should not be called")
		},
		httpClient: &http.Client{Timeout: time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		workerURL:  "http://127.0.0.1:1", deploymentPath: "/deployment/.env", credentialsPath: "/credentials/role.yaml",
		notifierPath: "/credentials/notifiers.yaml", passwordRoot: "/credentials/restic", resticBinary: "restic",
		dataDir: "/data", stagingDir: "/staging", cacheDir: "/cache", restoreRoot: "/restore", stateDir: "/state",
		ready: func() bool { return true },
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

func assertErrorJSON(t *testing.T, output, code string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	var document struct {
		SchemaVersion int `json:"schema_version"`
		Error         struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decoder.Decode(&document); err != nil || document.SchemaVersion != 1 || document.Error.Code != code || document.Error.Message == "" {
		t.Fatalf("error document = %#v/%v", document, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("error contains a second document: %q", output)
	}
}

func setBuildIdentity(t *testing.T, commit, release, clean string) {
	t.Helper()
	oldCommit, oldRelease, oldClean := buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean
	buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = commit, release, clean
	t.Cleanup(func() { buildinfo.ProjectCommit, buildinfo.Release, buildinfo.Clean = oldCommit, oldRelease, oldClean })
}
