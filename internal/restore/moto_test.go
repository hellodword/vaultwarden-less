package restore_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
	_ "github.com/mattn/go-sqlite3"
)

var motoServerImage = "vaultwarden-less-moto:" + upstream.MotoVersion

var (
	runMotoIntegration = flag.Bool("run-moto-integration", false, "run the real Moto and Restic integration test")
	motoHostPort       = flag.Int("moto-host-port", 0, "fixed host port for Moto; zero asks Docker for an ephemeral port")
)

// TestMotoResticIntegration exercises the real Restic/S3 command boundary. All
// filesystem data is owned by testing.T and the exact Moto container is
// removed even when the test fails.
func TestMotoResticIntegration(t *testing.T) {
	if !*runMotoIntegration {
		t.Skip("pass -run-moto-integration to run the real external integration")
	}
	const publishedHost = "127.0.0.1"
	if *motoHostPort != 0 && (*motoHostPort < 1024 || *motoHostPort > 65535) {
		t.Fatalf("invalid -moto-host-port %d", *motoHostPort)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("restic is required in the integration shell: %v", err)
	}
	docker := newDockerClient(t)
	if _, err := docker.run(ctx, "info"); err != nil {
		t.Fatalf("Docker daemon is required for the integration test: %v", err)
	}
	loadMotoImage(ctx, t, docker)

	containerName := fmt.Sprintf("vaultwarden-less-moto-%d-%d", os.Getpid(), time.Now().UnixNano())
	publish := "127.0.0.1::5000"
	if *motoHostPort != 0 {
		publish = "127.0.0.1:" + strconv.Itoa(*motoHostPort) + ":5000"
	}
	if output, err := docker.run(ctx, "run", "--detach", "--name", containerName, "--publish", publish, motoServerImage, "-H", "0.0.0.0"); err != nil {
		t.Fatalf("start Moto: %v (%s)", err, output)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cleanupCancel()
		_, _ = docker.run(cleanupContext, "rm", "--force", containerName)
	})

	endpointPort := waitForPublishedPort(ctx, t, docker, containerName, publishedHost)
	createMotoBucket(ctx, t, docker, containerName)

	root := t.TempDir()
	dataDir := privateDirectory(t, root, "data")
	stagingDir := privateDirectory(t, root, "staging")
	cacheDir := privateDirectory(t, root, "cache")
	restoreRoot := privateDirectory(t, root, "restore")
	database := createSyntheticVaultwarden(t, dataDir)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	first, err := (snapshot.Builder{DataDir: dataDir, StagingDir: stagingDir}).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(stagingDir, "point")
	credentialsDir := privateDirectory(t, root, "credentials")
	passwordFile := writeSecret(t, credentialsDir, "moto", "moto-restic-password")
	target := config.Target{
		Name:          "moto",
		Repository:    "s3:http://" + net.JoinHostPort(publishedHost, strconv.Itoa(endpointPort)) + "/vaultwarden-less-test/repository",
		ResticOptions: map[string]string{"s3.bucket-lookup": "path", "s3.region": "us-east-1"},
	}
	credentials := &config.RoleCredentials{SchemaVersion: config.SchemaVersion, Targets: map[string]config.S3Credentials{
		"moto": {AccessKeyID: "test", SecretAccessKey: "test"},
	}}
	runner := restic.NewRunner(resticBinary, cacheDir, credentialsDir, recoveryPath, credentials, nil)
	if err := runner.VerifyVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(ctx, target); err != nil {
		t.Fatal(err)
	}

	if !t.Run("backup_deduplicate_check_unlock_and_restore", func(t *testing.T) {
		firstSync, err := runner.Ensure(ctx, target, first)
		if err != nil || !firstSync.Created {
			t.Fatalf("first sync = %#v/%v", firstSync, err)
		}
		unchanged, err := runner.Ensure(ctx, target, first)
		if err != nil || unchanged.Created || unchanged.SnapshotID != firstSync.SnapshotID {
			t.Fatalf("unchanged sync = %#v/%v", unchanged, err)
		}

		mutateSyntheticDatabase(t, filepath.Join(dataDir, "db.sqlite3"))
		second, err := (snapshot.Builder{DataDir: dataDir, StagingDir: stagingDir}).Build(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if second.ContentID == first.ContentID {
			t.Fatal("source mutation did not change content ID")
		}
		secondSync, err := runner.Ensure(ctx, target, second)
		if err != nil || !secondSync.Created || secondSync.SnapshotID == firstSync.SnapshotID {
			t.Fatalf("second sync = %#v/%v", secondSync, err)
		}
		listed, truncated, err := runner.Snapshots(ctx, target, 100)
		if err != nil || truncated || len(listed) != 2 {
			t.Fatalf("snapshots = %#v/%v", listed, err)
		}
		if err := runner.Check(ctx, target, ""); err != nil {
			t.Fatal(err)
		}
		if err := runner.Unlock(ctx, target); err != nil {
			t.Fatal(err)
		}

		latestOutput := filepath.Join(restoreRoot, "latest")
		latest, err := restoretool.Run(ctx, runner, restoretool.Request{Target: target, Output: latestOutput, RestoreRoot: restoreRoot})
		if err != nil || latest.SnapshotID != secondSync.SnapshotID || latest.ContentID != second.ContentID {
			t.Fatalf("latest restore = %#v/%v", latest, err)
		}
		if _, err := snapshot.Verify(ctx, latestOutput); err != nil {
			t.Fatal(err)
		}
		explicitOutput := filepath.Join(restoreRoot, "explicit-earlier")
		explicit, err := restoretool.Run(ctx, runner, restoretool.Request{Target: target, SnapshotID: firstSync.SnapshotID, Output: explicitOutput, RestoreRoot: restoreRoot})
		if err != nil || explicit.ContentID != first.ContentID {
			t.Fatalf("explicit restore = %#v/%v", explicit, err)
		}
		if _, err := restoretool.Run(ctx, runner, restoretool.Request{Target: target, SnapshotID: firstSync.SnapshotID, Output: explicitOutput, RestoreRoot: restoreRoot}); err == nil {
			t.Fatal("restore unexpectedly overwrote an existing output")
		}

		if err := os.RemoveAll(recoveryPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(recoveryPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(recoveryPath, "manifest.json"), []byte("{\"schema_version\":1}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		invalidContent := strings.Repeat("c", 64)
		invalidID, err := rawResticBackup(ctx, resticBinary, cacheDir, passwordFile, recoveryPath, target, invalidContent)
		if err != nil {
			t.Fatal(err)
		}
		invalidOutput := filepath.Join(restoreRoot, "invalid-latest")
		if _, err := restoretool.Run(ctx, runner, restoretool.Request{Target: target, Output: invalidOutput, RestoreRoot: restoreRoot}); err == nil {
			t.Fatal("invalid latest snapshot was silently skipped")
		}
		if _, err := os.Lstat(invalidOutput); !os.IsNotExist(err) {
			t.Fatalf("failed restore published output: %v", err)
		}
		earlierAgain := filepath.Join(restoreRoot, "explicit-after-invalid")
		if restored, err := restoretool.Run(ctx, runner, restoretool.Request{Target: target, SnapshotID: firstSync.SnapshotID, Output: earlierAgain, RestoreRoot: restoreRoot}); err != nil || restored.SnapshotID == invalidID {
			t.Fatalf("explicit earlier restore after invalid latest = %#v/%v", restored, err)
		}
	}) {
		return
	}

	t.Run("prune_dry_run_and_apply_exact_scope", func(t *testing.T) {
		baseTime := time.Now().UTC().AddDate(-10, 0, 0).Truncate(time.Hour)
		historical := make([]string, 0, 4)
		for index := 0; index < 4; index++ {
			id, err := rawResticBackupAt(
				ctx, resticBinary, cacheDir, passwordFile, recoveryPath, target,
				fmt.Sprintf("%064x", index+1), restic.SnapshotHost,
				baseTime.Add(time.Duration(index)*time.Second),
			)
			if err != nil {
				t.Fatal(err)
			}
			historical = append(historical, id)
		}
		outOfScopeID, err := rawResticBackupAt(
			ctx, resticBinary, cacheDir, passwordFile, recoveryPath, target,
			strings.Repeat("e", 64), "outside-vaultwarden-less",
			baseTime.Add(10*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}

		dryRun, err := runner.Prune(ctx, target, false)
		if err != nil || dryRun.Applied {
			t.Fatalf("dry-run prune = %#v/%v", dryRun, err)
		}
		dryRemove := pruneRemovalSet(t, dryRun)
		if len(dryRemove) != 2 {
			t.Fatalf("controlled retention remove set = %#v, want exactly two", dryRemove)
		}
		historicalSet := map[string]bool{}
		for _, id := range historical {
			historicalSet[id] = true
		}
		for id := range dryRemove {
			if !historicalSet[id] || id == outOfScopeID {
				t.Fatalf("dry-run selected an out-of-scope snapshot: %s", id)
			}
		}
		applied, err := runner.Prune(ctx, target, true)
		if err != nil || !applied.Applied {
			t.Fatalf("applied prune = %#v/%v", applied, err)
		}
		appliedRemove := pruneRemovalSet(t, applied)
		if !maps.Equal(appliedRemove, dryRemove) {
			t.Fatalf("applied remove set = %#v, dry-run = %#v", appliedRemove, dryRemove)
		}

		remaining, err := rawResticSnapshotIDs(ctx, resticBinary, cacheDir, passwordFile, target)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range historical {
			wantPresent := !dryRemove[id]
			if remaining[id] != wantPresent {
				t.Fatalf("historical snapshot %s has wrong post-prune presence", id)
			}
		}
		if !remaining[outOfScopeID] {
			t.Fatalf("prune removed out-of-scope snapshot %s", outOfScopeID)
		}
		if err := runner.Check(ctx, target, ""); err != nil {
			t.Fatal(err)
		}
	})
}

func privateDirectory(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func mutateSyntheticDatabase(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO users(
		uuid, created_at, updated_at, email, name, password_hash, salt,
		password_iterations, akey, security_stamp, equivalent_domains,
		excluded_globals
	) VALUES (
		'22222222-2222-2222-2222-222222222222',
		'2026-05-05', '2026-05-05', 'second@example.test', 'Second',
		X'00', X'00', 600000, 'key', 'stamp', '[]', '[]'
	)`); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
}

func createMotoBucket(ctx context.Context, t *testing.T, docker *dockerClient, containerName string) {
	t.Helper()
	command := []string{"exec", containerName, "python", "-c", "import boto3; boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1').create_bucket(Bucket='vaultwarden-less-test')"}
	var lastError error
	for attempt := 0; attempt < 60; attempt++ {
		if _, lastError = docker.run(ctx, command...); lastError == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("create Moto bucket: %v", lastError)
}

func waitForPublishedPort(ctx context.Context, t *testing.T, docker *dockerClient, name, publishedHost string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err := docker.run(ctx, "port", name, "5000/tcp")
		if err == nil {
			fields := strings.Fields(strings.TrimSpace(output))
			if len(fields) > 0 {
				_, port, splitError := net.SplitHostPort(fields[0])
				value, parseError := strconv.Atoi(port)
				if splitError == nil && parseError == nil {
					connection, dialError := net.DialTimeout("tcp", net.JoinHostPort(publishedHost, port), time.Second)
					if dialError == nil {
						_ = connection.Close()
						return value
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatal("Moto did not publish a ready port")
	return 0
}

func rawResticBackup(ctx context.Context, binary, cacheDir, passwordFile, recoveryPath string, target config.Target, contentHex string) (string, error) {
	return rawResticBackupAt(
		ctx, binary, cacheDir, passwordFile, recoveryPath, target,
		contentHex, restic.SnapshotHost, time.Time{},
	)
}

func rawResticBackupAt(
	ctx context.Context,
	binary, cacheDir, passwordFile, recoveryPath string,
	target config.Target,
	contentHex, host string,
	when time.Time,
) (string, error) {
	arguments := []string{
		"--repo", target.Repository,
		"--password-file", passwordFile,
		"--cache-dir", filepath.Join(cacheDir, target.Name),
		"--option", "s3.bucket-lookup=path",
		"--option", "s3.region=us-east-1",
		"backup", "--json",
		"--host", host,
		"--tag", restic.SnapshotTag,
		"--tag", "content-id=" + contentHex,
	}
	if !when.IsZero() {
		arguments = append(arguments, "--time", when.Format("2006-01-02 15:04:05"))
	}
	arguments = append(arguments, "--", recoveryPath)
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = testResticEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		diagnostics := strings.TrimSpace(string(output))
		if len(diagnostics) > 2000 {
			diagnostics = diagnostics[:2000] + "..."
		}
		return "", fmt.Errorf("plant raw snapshot: %w: %s", err, diagnostics)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		var summary struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &summary); err != nil {
			return "", err
		}
		if summary.MessageType == "summary" {
			return summary.SnapshotID, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", io.ErrUnexpectedEOF
}

func testResticEnvironment() []string {
	return []string{
		"TMPDIR=/tmp",
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
	}
}

func rawResticSnapshotIDs(
	ctx context.Context,
	binary, cacheDir, passwordFile string,
	target config.Target,
) (map[string]bool, error) {
	arguments := []string{
		"--repo", target.Repository,
		"--password-file", passwordFile,
		"--cache-dir", filepath.Join(cacheDir, target.Name),
		"--option", "s3.bucket-lookup=path",
		"--option", "s3.region=us-east-1",
		"snapshots", "--json",
	}
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Env = testResticEnvironment()
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("list all raw snapshots: %w", err)
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(snapshots))
	for _, item := range snapshots {
		if len(item.ID) != 64 {
			return nil, fmt.Errorf("raw snapshot has invalid ID")
		}
		ids[item.ID] = true
	}
	return ids, nil
}

func pruneRemovalSet(t *testing.T, result restic.PruneResult) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, group := range result.Groups {
		for _, id := range group.RemoveSnapshotIDs {
			if got[id] {
				t.Fatalf("prune reported duplicate removal %s", id)
			}
			got[id] = true
		}
	}
	return got
}

func loadMotoImage(ctx context.Context, t *testing.T, docker *dockerClient) {
	t.Helper()
	repository := filepath.Clean(filepath.Join("..", ".."))
	command := exec.CommandContext(
		ctx,
		"nix",
		"build",
		"--no-link",
		"--print-out-paths",
		".#moto-image",
	)
	command.Dir = repository
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	paths := strings.Fields(string(output))
	if err != nil || len(paths) != 1 {
		t.Fatalf("build local Moto image: %v: %s", err, diagnostics.String())
	}
	if output, err := docker.run(ctx, "load", "--input", paths[0]); err != nil {
		t.Fatalf("load local Moto image: %v: %s", err, output)
	}
}

type dockerClient struct {
	binary      string
	config      string
	environment []string
}

func newDockerClient(t *testing.T) *dockerClient {
	t.Helper()
	binary, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "docker-config")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(config, "config.json"),
		[]byte("{\"auths\":{\"https://index.docker.io/v1/\":{}}}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	environment := append(os.Environ(), "DOCKER_CONFIG="+config)
	return &dockerClient{binary: binary, config: config, environment: environment}
}

func (client *dockerClient) run(ctx context.Context, arguments ...string) (string, error) {
	arguments = append([]string{"--config", client.config}, arguments...)
	command := exec.CommandContext(ctx, client.binary, arguments...)
	command.Env = client.environment
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}
