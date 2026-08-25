//go:build systemtest

package restore_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	restoretool "github.com/hellodword/vaultwarden-less/internal/restore"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

const motoServerImage = "ghcr.io/getmoto/motoserver:5.2.2@sha256:0349ef6ecb9cd041293026accda0ea71bfa09925dffae3d76d120d15ecc76971"

var (
	motoHostPort      = flag.Int("moto-host-port", 0, "fixed host port for Moto; zero asks Docker for an ephemeral port")
	motoPublishedHost = flag.String("published-host", "127.0.0.1", "host through which this test reaches Docker-published ports")
)

// TestMotoResticRoundTrip is the full Restic/S3 contract test selected by the
// systemtest build tag. The test owns one exact Moto container and all data
// lives below testing.T's system temp directory.
func TestMotoResticRoundTrip(t *testing.T) {
	publishedHost := validatePublishedHost(t, *motoPublishedHost)
	if *motoHostPort != 0 && (*motoHostPort < 1024 || *motoHostPort > 65535) {
		t.Fatalf("invalid -moto-host-port %d", *motoHostPort)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("restic is required in the full test shell: %v", err)
	}
	if _, err := runDocker(ctx, "info"); err != nil {
		t.Fatalf("Docker daemon is required for the full test: %v", err)
	}

	containerName := fmt.Sprintf("vaultwarden-less-moto-%d", os.Getpid())
	publish := "0.0.0.0::5000"
	if *motoHostPort != 0 {
		publish = "0.0.0.0:" + strconv.Itoa(*motoHostPort) + ":5000"
	}
	if output, err := runDocker(ctx, "run", "--detach", "--name", containerName, "--publish", publish, motoServerImage, "-H", "0.0.0.0"); err != nil {
		t.Fatalf("start Moto: %v (%s)", err, output)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = runDocker(cleanupCtx, "rm", "--force", containerName)
	})

	port := waitForPublishedPort(ctx, t, containerName, publishedHost)
	bucketCommand := []string{"exec", containerName, "python", "-c", "import boto3; boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1').create_bucket(Bucket='vaultwarden-less-test')"}
	var bucketErr error
	for attempt := 0; attempt < 60; attempt++ {
		if _, bucketErr = runDocker(ctx, bucketCommand...); bucketErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if bucketErr != nil {
		t.Fatalf("create Moto bucket with its bundled boto3: %v", bucketErr)
	}

	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	stagingDir := filepath.Join(root, "staging")
	cacheDir := filepath.Join(root, "cache")
	for _, path := range []string{dataDir, stagingDir, cacheDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database := createSyntheticVaultwarden(t, dataDir)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	buildCtx, buildCancel := context.WithTimeout(ctx, 2*time.Minute)
	built, err := (snapshot.Builder{DataDir: dataDir, StagingDir: stagingDir}).Build(buildCtx, false)
	buildCancel()
	if err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(stagingDir, "current")
	if err := os.Rename(built.Path, currentPath); err != nil {
		t.Fatal(err)
	}

	credentialsDir := filepath.Join(root, "credentials")
	if err := os.Mkdir(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	passwordFile := writeSecret(t, credentialsDir, "restic-password", "moto-restic-password")
	target := config.Target{
		Name:          "moto",
		Repository:    "s3:http://" + net.JoinHostPort(publishedHost, strconv.Itoa(port)) + "/vaultwarden-less-test/repository",
		ResticOptions: map[string]string{"s3.bucket-lookup": "path", "s3.region": "us-east-1"},
	}
	runner := restic.Runner{
		Binary: resticBinary, CacheDir: cacheDir, StagingDir: stagingDir,
		PasswordFile: passwordFile, Credentials: &config.RoleCredentials{SchemaVersion: 1, Targets: map[string]map[string]string{
			"moto": {"AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test"},
		}},
	}
	if err := runner.VerifyVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(ctx, target); err != nil {
		t.Fatal(err)
	}
	complete, err := runner.Backup(ctx, target, built.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	report, err := runner.ListSnapshots(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Snapshots) != 1 || len(report.Anomalies) != 0 || report.Snapshots[0].ID != complete.SnapshotID {
		t.Fatalf("initial snapshot report = %#v", report)
	}
	if err := runner.Check(ctx, target, ""); err != nil {
		t.Fatal(err)
	}

	restoreRoot := filepath.Join(root, "restore")
	if err := os.Mkdir(restoreRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(restoreRoot, "prepared")
	restored, err := restoretool.Run(ctx, &config.Backup{DataDir: dataDir}, runner, restoretool.Request{Target: target, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	if restored.SnapshotID != complete.SnapshotID || restored.Manifest.ContentID != built.Manifest.ContentID {
		t.Fatalf("restore result = %#v", restored)
	}
	if _, err := snapshot.Verify(ctx, output, false); err != nil {
		t.Fatal(err)
	}
	if reused, err := restoretool.Run(ctx, &config.Backup{DataDir: dataDir}, runner, restoretool.Request{Target: target, Output: output}); err != nil {
		t.Fatal(err)
	} else if !reused.ReusedExistingOutput || reused.SnapshotID != complete.SnapshotID {
		t.Fatalf("idempotent restore result = %#v", reused)
	}

	missingPath := "data/attachments/11111111-1111-1111-1111-111111111111/" + strings.Repeat("a", 20)
	if err := os.Remove(filepath.Join(currentPath, filepath.FromSlash(missingPath))); err != nil {
		t.Fatal(err)
	}
	incompleteFiles := slices.DeleteFunc(append([]snapshot.File(nil), built.Manifest.Files...), func(file snapshot.File) bool { return file.Path == missingPath })
	incompleteManifest, err := snapshot.NewManifest(snapshot.Incomplete, built.Manifest.DatabaseMigration, incompleteFiles, []snapshot.Missing{{Path: missingPath, Reason: "not_found"}})
	if err != nil {
		t.Fatal(err)
	}
	writeManifest(t, currentPath, incompleteManifest)
	incomplete, err := runner.Backup(ctx, target, incompleteManifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Prune(ctx, target, false); err != nil {
		t.Fatal(err)
	}
	if incomplete.SnapshotID == complete.SnapshotID {
		t.Fatal("complete and incomplete snapshots reused the same ID")
	}

	badDir := filepath.Join(root, "bad")
	if err := os.Mkdir(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "manifest.json"), []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	badID, err := rawResticBackup(ctx, resticBinary, runner, target, badDir, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	report, err = runner.ListSnapshots(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Snapshots) != 2 || len(report.Anomalies) != 1 || report.Anomalies[0].SnapshotID != badID || report.Anomalies[0].ErrorCode != "snapshot_manifest_invalid" {
		t.Fatalf("report with invalid candidate = %#v", report)
	}
	if _, err := runner.FindSnapshot(ctx, target, badID); err == nil {
		t.Fatal("explicit invalid candidate was accepted")
	}
	if _, err := runner.Prune(ctx, target, false); err == nil {
		t.Fatal("prune ignored an invalid product candidate")
	} else {
		var operationErr *restic.OperationError
		if !errors.As(err, &operationErr) || operationErr.Code != "snapshot_anomaly_blocks_prune" {
			t.Fatalf("prune anomaly error = %v", err)
		}
	}
}

func waitForPublishedPort(ctx context.Context, t *testing.T, name, publishedHost string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		output, err := runDocker(ctx, "port", name, "5000/tcp")
		if err == nil {
			fields := strings.Fields(strings.TrimSpace(output))
			if len(fields) > 0 {
				address := fields[0]
				_, port, splitErr := net.SplitHostPort(address)
				if splitErr == nil {
					value, parseErr := strconv.Atoi(port)
					if parseErr == nil {
						dialAddress := net.JoinHostPort(publishedHost, port)
						connection, dialErr := net.DialTimeout("tcp", dialAddress, time.Second)
						if dialErr == nil {
							_ = connection.Close()
							return value
						}
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

func validatePublishedHost(t *testing.T, host string) string {
	t.Helper()
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/?#@") {
		t.Fatalf("invalid -published-host %q", host)
	}
	return host
}

func rawResticBackup(ctx context.Context, binary string, runner restic.Runner, target config.Target, directory, contentID string) (string, error) {
	cache := filepath.Join(runner.CacheDir, "moto")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return "", err
	}
	password, err := os.ReadFile(runner.PasswordFile)
	if err != nil {
		return "", err
	}
	args := []string{
		"-o", "s3.bucket-lookup=path", "-o", "s3.region=us-east-1",
		"backup", "--json", "--cache-dir", cache, "--host", "vaultwarden-less-v1",
		"--tag", "vaultwarden-less-v1", "--tag", "complete", "--tag", "content-id=" + contentID,
		"manifest.json",
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	command.Env = []string{
		"RESTIC_REPOSITORY=" + target.Repository,
		"RESTIC_PASSWORD=" + strings.TrimSpace(string(password)),
		"TMPDIR=/tmp",
		"AWS_ACCESS_KEY_ID=test",
		"AWS_SECRET_ACCESS_KEY=test",
	}
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("plant invalid snapshot: %w", err)
	}
	var summary struct {
		MessageType string `json:"message_type"`
		SnapshotID  string `json:"snapshot_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		if err := decoder.Decode(&summary); err != nil {
			if err == io.EOF {
				break
			}
			return "", err
		}
		if summary.MessageType == "summary" {
			return summary.SnapshotID, nil
		}
	}
	return "", fmt.Errorf("invalid planted snapshot summary")
}

func writeManifest(t *testing.T, root string, manifest snapshot.Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runDocker(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}
