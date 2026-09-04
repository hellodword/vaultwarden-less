package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

type vaultFixture struct {
	folderID       string
	folderName     string
	itemID         string
	itemPassword   string
	attachmentID   string
	attachmentData []byte
	sendID         string
	sendData       []byte
}

func TestTargetlessDeploymentAndFullDataAdoption(t *testing.T) {
	if !*runE2E {
		t.Skip("pass -run-e2e to run the release-level Docker scenarios")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	h := newE2EHarness(t, ctx, []string{})
	h.validateConfiguration([]string{})
	assertEmptyDirectory(t, filepath.Join(h.deploymentRoot, "credentials", "restic"))

	// Build a realistic stopped external Vaultwarden tree through the official
	// client, then adopt the complete tree as an offline operator copy.
	h.startSource()
	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-targetless-%d@example.com", unique)
	registerAccount(ctx, t, loopbackEndpoint(h.sourcePort), email, testPassword)
	sourceClient := newBitwardenCLI(
		t, filepath.Join(h.testRoot, "bw-source"), h.publicTLS, h.tlsCAFile, email,
	)
	sourceClient.login(ctx)
	fixture := createVaultFixture(t, ctx, h.testRoot, sourceClient, "Targetless adoption")
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-source"), sourceClient, fixture)
	h.stopService("source-vaultwarden")

	sourceRoot := filepath.Join(h.testRoot, "source-data")
	assertCompleteVaultwardenTree(t, sourceRoot)
	sourceIdentity := privateTreeIdentity(t, sourceRoot)
	sourceRSA := privateFileDigest(t, filepath.Join(sourceRoot, "rsa_key.pem"))
	copyCompleteTreeIntoEmpty(t, sourceRoot, filepath.Join(h.deploymentRoot, "data"))
	if got := privateTreeIdentity(t, filepath.Join(h.deploymentRoot, "data")); got != sourceIdentity {
		t.Fatalf("adopted data tree identity = %q, want %q", got, sourceIdentity)
	}
	if got := privateFileDigest(t, filepath.Join(h.deploymentRoot, "data", "rsa_key.pem")); got != sourceRSA {
		t.Fatalf("adopted RSA digest = %q, want %q", got, sourceRSA)
	}
	if got := privateTreeIdentity(t, sourceRoot); got != sourceIdentity {
		t.Fatal("full data adoption modified the stopped source tree")
	}

	h.startProduction()
	assertTargetlessRuntime(t, h)
	assertNoRepositoryServices(t, h)
	adoptedClient := newBitwardenCLI(
		t, filepath.Join(h.testRoot, "bw-adopted"), h.publicTLS, h.tlsCAFile, email,
	)
	adoptedClient.login(ctx)
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-adopted"), adoptedClient, fixture)

	oldContainer := h.containerID("vaultwarden")
	setSignupsAllowed(t, filepath.Join(h.deploymentRoot, "config", "vaultwarden.env"), false)
	h.compose("up", "--detach", "--force-recreate", "vaultwarden")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	newContainer := h.containerID("vaultwarden")
	if newContainer == oldContainer {
		t.Fatal("Vaultwarden was not force-recreated after disabling signup")
	}
	if got := privateFileDigest(t, filepath.Join(h.deploymentRoot, "data", "rsa_key.pem")); got != sourceRSA {
		t.Fatalf("force recreation changed the adopted RSA digest: %q != %q", got, sourceRSA)
	}
	restartedClient := newBitwardenCLI(
		t, filepath.Join(h.testRoot, "bw-restarted"), h.publicTLS, h.tlsCAFile, email,
	)
	restartedClient.login(ctx)
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-restarted"), restartedClient, fixture)
	expectRegistrationRejected(
		ctx,
		t,
		loopbackEndpoint(h.proxyPort),
		fmt.Sprintf("vaultwarden-less-rejected-%d@example.com", unique),
		"Rejected registration password 2026!",
	)
	assertTargetlessStatusAndFilesystem(t, h)
}

func TestTwoTargetBackupRestore(t *testing.T) {
	if !*runE2E {
		t.Skip("pass -run-e2e to run the release-level Docker scenarios")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	targets := []string{"primary", "b2"}
	h := newE2EHarness(t, ctx, targets)
	h.validateConfiguration(targets)
	h.startRepositories()
	h.startVaultwarden()

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-two-target-%d@example.com", unique)
	registerAccount(ctx, t, loopbackEndpoint(h.vaultwardenPort), email, testPassword)
	client := newBitwardenCLI(
		t, filepath.Join(h.testRoot, "bw-two-target"), h.publicTLS, h.tlsCAFile, email,
	)
	client.login(ctx)
	fixture := createVaultFixture(t, ctx, h.testRoot, client, "Two target recovery")
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-two-target-source"), client, fixture)
	sourceRSA := privateFileDigest(t, filepath.Join(h.deploymentRoot, "data", "rsa_key.pem"))

	// The failed endpoint is a real network peer returning 403. Primary must
	// publish the point independently while b2 remains retryable.
	restoreB2 := rejectB2Endpoint(t, h)
	h.compose("up", "--detach", "backup")
	h.waitHealthy("backup", 3*time.Minute)
	partial := h.waitStatus(4*time.Minute, func(status productworker.Status) bool {
		primary, b2 := exactTwoTargets(t, status)
		return status.State == "partial" && status.LastCheck != nil && status.LastCheck.Result == "partial" &&
			status.LastCheck.ContentID != nil && status.LastCheck.ArchiveID != nil &&
			primary.State == "healthy" && primary.SnapshotID != nil && primary.ContentID != nil &&
			b2.State == "retrying" && b2.SnapshotID == nil && b2.ContentID == nil && b2.ErrorCode != nil
	})
	partialPrimary, _ := exactTwoTargets(t, partial)
	if partial.LastCheck == nil || partial.LastCheck.ContentID == nil || partialPrimary.ContentID == nil ||
		*partialPrimary.ContentID != *partial.LastCheck.ContentID {
		t.Fatalf("partial target identities are incomplete: %#v", partial)
	}
	logicalContent := *partial.LastCheck.ContentID
	primarySnapshotID := *partialPrimary.SnapshotID
	primarySnapshots := h.waitTargetSnapshotCount("primary", 1, 2*time.Minute)
	if primarySnapshots.Snapshots[0].ID != primarySnapshotID || primarySnapshots.Snapshots[0].ContentID != logicalContent {
		t.Fatalf("primary repository does not contain the partial recovery point: %#v", primarySnapshots)
	}

	// Bringing up the proxy delivers its startup event, but no Vaultwarden write
	// occurs between the failed attempt and the b2 catch-up.
	h.compose("up", "--detach", "proxy")
	h.waitHealthy("proxy", 3*time.Minute)
	h.setBackend(loopbackEndpoint(h.proxyPort))
	h.waitHTTP(h.publicTLS+"/.vaultwarden-less/healthz", 204, time.Minute)
	restoreB2()
	h.requestBackup()
	recovered := h.waitStatus(8*time.Minute, func(status productworker.Status) bool {
		primary, b2 := exactTwoTargets(t, status)
		return status.State == "idle" && status.LastCheck != nil && status.LastCheck.Result == "created" &&
			primary.State == "healthy" && b2.State == "healthy" &&
			primary.SnapshotID != nil && b2.SnapshotID != nil &&
			primary.ContentID != nil && b2.ContentID != nil &&
			*primary.ContentID == logicalContent && *b2.ContentID == logicalContent
	})
	recoveredPrimary, recoveredB2 := exactTwoTargets(t, recovered)
	if *recoveredPrimary.SnapshotID != primarySnapshotID {
		t.Fatalf("b2 catch-up replaced the primary snapshot: %#v", recoveredPrimary)
	}
	primarySnapshots = h.waitTargetSnapshotCount("primary", 1, 2*time.Minute)
	b2Snapshots := h.waitTargetSnapshotCount("b2", 1, 2*time.Minute)
	primarySnapshot, b2Snapshot := primarySnapshots.Snapshots[0], b2Snapshots.Snapshots[0]
	if primarySnapshot.ContentID != logicalContent || b2Snapshot.ContentID != logicalContent ||
		b2Snapshot.ID != *recoveredB2.SnapshotID || primarySnapshot.ID == b2Snapshot.ID {
		t.Fatalf("repositories did not converge independently: primary=%#v b2=%#v", primarySnapshot, b2Snapshot)
	}
	for _, target := range targets {
		h.repositoryCheck(target)
	}

	primaryRestore := h.restoreTarget("primary", primarySnapshot.ID, "primary-copy")
	b2Restore := h.restoreTarget("b2", b2Snapshot.ID, "b2-copy")
	assertRestoreMatchesSnapshot(t, primaryRestore, primarySnapshot)
	assertRestoreMatchesSnapshot(t, b2Restore, b2Snapshot)
	primaryRoot := filepath.Join(h.deploymentRoot, "restore", "primary-copy")
	b2Root := filepath.Join(h.deploymentRoot, "restore", "b2-copy")
	assertRecoveryExcludesRSA(t, primaryRoot)
	assertRecoveryExcludesRSA(t, b2Root)

	marker := filepath.Join(primaryRoot, "operator-marker")
	writePrivateFile(t, marker, "existing restore must survive\n")
	failedOutput, failedErr := h.tryRestoreTarget("primary", primarySnapshot.ID, "primary-copy")
	if failedErr == nil || exactCommandErrorCode(t, failedOutput) != "verification_failure" {
		t.Fatalf("second restore did not reject an existing output: %v: %s", failedErr, harnessDiagnostics(failedOutput))
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "existing restore must survive\n" {
		t.Fatalf("failed restore changed its existing output marker: %q/%v", data, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	assertNoRestoreTemporaryDirectories(t, filepath.Join(h.deploymentRoot, "restore"))

	h.stopService("proxy")
	h.stopService("backup")
	h.stopService("vaultwarden")
	h.startRestored("restored-primary", h.primaryPort)
	client.expectSyncFailure(ctx)
	primaryClient := newBitwardenCLI(
		t, filepath.Join(h.testRoot, "bw-restored-primary"), h.publicTLS, h.tlsCAFile, email,
	)
	primaryClient.login(ctx)
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-restored-primary"), primaryClient, fixture)
	primaryRSA := privateFileDigest(t, filepath.Join(primaryRoot, "rsa_key.pem"))
	if primaryRSA == sourceRSA {
		t.Fatal("primary restore retained the production RSA identity")
	}
	h.compose("--profile", "e2e-vaultwarden", "stop", "restored-primary")

	h.startRestored("restored-b2", h.b2Port)
	b2Client := newBitwardenCLI(
		t, filepath.Join(h.testRoot, "bw-restored-b2"), h.publicTLS, h.tlsCAFile, email,
	)
	b2Client.login(ctx)
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-restored-b2"), b2Client, fixture)
	b2RSA := privateFileDigest(t, filepath.Join(b2Root, "rsa_key.pem"))
	if b2RSA == sourceRSA || b2RSA == primaryRSA {
		t.Fatal("restored copies did not generate independent new RSA identities")
	}
}

func createVaultFixture(
	t *testing.T,
	ctx context.Context,
	root string,
	client *bitwardenCLI,
	label string,
) vaultFixture {
	t.Helper()
	fixture := vaultFixture{
		folderName:     label + " folder",
		itemPassword:   initialItemPassword,
		attachmentData: []byte("vaultwarden-less attachment for " + label + "\n"),
		sendData:       bytes.Repeat([]byte("vaultwarden-less file Send payload\n"), 64),
	}
	fixture.folderID = client.createFolder(ctx, fixture.folderName)
	fixture.itemID = client.createItem(ctx, label+" login", fixture.itemPassword, fixture.folderID)
	attachment := privateTestPath(t, root, fmt.Sprintf("attachment-%d.txt", time.Now().UnixNano()), fixture.attachmentData)
	fixture.attachmentID = client.createAttachment(ctx, fixture.itemID, attachment)
	send := privateTestPath(t, root, fmt.Sprintf("send-%d.bin", time.Now().UnixNano()), fixture.sendData)
	fixture.sendID = client.createFileSend(ctx, send, label+" Send")
	return fixture
}

func assertVaultFixture(
	t *testing.T,
	ctx context.Context,
	outputRoot string,
	client *bitwardenCLI,
	fixture vaultFixture,
) {
	t.Helper()
	client.sync(ctx)
	if folder := client.getFolder(ctx, fixture.folderID); stringValue(folder["name"]) != fixture.folderName {
		t.Fatalf("folder did not survive: %#v", folder)
	}
	item := client.getItem(ctx, fixture.itemID)
	if itemPassword(t, item) != fixture.itemPassword || stringValue(item["folderId"]) != fixture.folderID {
		t.Fatalf("item did not survive: %#v", item)
	}
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	attachment := filepath.Join(outputRoot, "attachment")
	client.getAttachment(ctx, fixture.attachmentID, fixture.itemID, attachment)
	assertFileContent(t, attachment, fixture.attachmentData)
	send := filepath.Join(outputRoot, "send")
	client.getFileSend(ctx, fixture.sendID, send)
	assertFileContent(t, send, fixture.sendData)
}

func assertTargetlessRuntime(t *testing.T, h *e2eHarness) {
	t.Helper()
	container := h.containerID("backup")
	logs, err := h.dockerLogs("--tail", "200", container)
	if err != nil {
		t.Fatalf("read targetless backup logs: %v: %s", err, harnessDiagnostics(logs))
	}
	if strings.Count(logs, `"warning_code":"no_backup_targets"`) != 1 ||
		!strings.Contains(logs, `"level":"WARN"`) ||
		!strings.Contains(logs, "Vaultwarden data is not being backed up") {
		t.Fatalf("targetless startup warning is missing or duplicated: %s", harnessDiagnostics(logs))
	}
	assertTargetlessStatusAndFilesystem(t, h)
	requestOutput, requestErr := h.tryCompose(
		h.ctx,
		"exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "request-backup",
	)
	if requestErr == nil || exactCommandErrorCode(t, requestOutput) != "not_ready" ||
		!strings.Contains(requestOutput, productworker.NoBackupTargetsMessage) {
		t.Fatalf("targetless request-backup = %v: %s", requestErr, harnessDiagnostics(requestOutput))
	}
}

func assertTargetlessStatusAndFilesystem(t *testing.T, h *e2eHarness) {
	t.Helper()
	status := h.status()
	if status.State != "idle" || status.LastCheck != nil || status.NextCheckAt != nil || status.Targets == nil || len(status.Targets) != 0 {
		t.Fatalf("targetless worker status = %#v", status)
	}
	point := filepath.Join(h.deploymentRoot, "staging", "point")
	if _, err := os.Lstat(point); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("targetless worker created a staging point: %v", err)
	}
}

func assertNoRepositoryServices(t *testing.T, h *e2eHarness) {
	t.Helper()
	for _, service := range []string{"moto-primary", "moto-b2"} {
		output, err := h.tryCompose(h.ctx, "--profile", "e2e-repositories", "ps", "--all", "--quiet", service)
		if err != nil || strings.TrimSpace(output) != "" {
			t.Fatalf("targetless deployment started %s: %v: %s", service, err, harnessDiagnostics(output))
		}
	}
}

func assertCompleteVaultwardenTree(t *testing.T, root string) {
	t.Helper()
	for _, relative := range []string{"db.sqlite3", "rsa_key.pem", "attachments", "sends"} {
		info, err := os.Lstat(filepath.Join(root, relative))
		if err != nil || info.Mode()&os.ModeSymlink != 0 ||
			(relative == "db.sqlite3" || relative == "rsa_key.pem") != info.Mode().IsRegular() ||
			(relative == "attachments" || relative == "sends") != info.IsDir() {
			t.Fatalf("external Vaultwarden tree lacks safe %s: %#v/%v", relative, info, err)
		}
	}
}

func copyCompleteTreeIntoEmpty(t *testing.T, source, destination string) {
	t.Helper()
	if err := privatefs.ValidatePrivateDirectory(source); err != nil {
		t.Fatalf("validate stopped source root: %v", err)
	}
	if err := privatefs.ValidatePrivateDirectory(destination); err != nil {
		t.Fatalf("validate empty destination root: %v", err)
	}
	assertEmptyDirectory(t, destination)
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || filepath.Clean(relative) != relative {
			return fmt.Errorf("invalid stopped source path")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("stopped source contains an unsafe owner or symlink")
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		if !info.Mode().IsRegular() || stat.Nlink != 1 {
			return fmt.Errorf("stopped source contains a special or multiply linked file")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			_ = input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		syncErr := output.Sync()
		closeOutputErr := output.Close()
		closeInputErr := input.Close()
		for _, candidate := range []error{copyErr, syncErr, closeOutputErr, closeInputErr} {
			if candidate != nil {
				return candidate
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
}

func privateTreeIdentity(t *testing.T, root string) string {
	t.Helper()
	digest := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("tree contains a symlink")
		}
		if info.IsDir() {
			_, err = fmt.Fprintf(digest, "D\x00%s\x00", relative)
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("tree contains a special file")
		}
		if _, err := fmt.Fprintf(digest, "F\x00%s\x00%d\x00", relative, info.Size()); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}

func privateFileDigest(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("private digest input is not a regular file: %#v/%v", info, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", digest[:])
}

func setSignupsAllowed(t *testing.T, path string, allowed bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	from, to := "SIGNUPS_ALLOWED=true\n", "SIGNUPS_ALLOWED=false\n"
	if allowed {
		from, to = to, from
	}
	if strings.Count(string(data), from) != 1 || strings.Contains(string(data), to) {
		t.Fatal("Vaultwarden signup setting has an unexpected shape")
	}
	updated := []byte(strings.Replace(string(data), from, to, 1))
	parent := filepath.Dir(path)
	file, err := os.CreateTemp(parent, ".vaultwarden.env-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	temporary := file.Name()
	open := true
	defer func() {
		if open {
			_ = file.Close()
		}
		_ = os.Remove(temporary)
	}()
	if err := file.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if written, err := file.Write(updated); err != nil || written != len(updated) {
		if err == nil {
			err = io.ErrShortWrite
		}
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		open = false
		t.Fatal(err)
	}
	open = false
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveryExcludesRSA(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"rsa_key", "rsa_key.pem", "rsa_key.pub.pem", "rsa_key.der"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("recovery point retained %s: %v", name, err)
		}
	}
}

func assertNoRestoreTemporaryDirectories(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".vaultwarden-less-restore-") {
			t.Fatalf("restore left temporary output %s", entry.Name())
		}
	}
}

func assertEmptyDirectory(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory %s is not empty: %#v/%v", filepath.Base(path), entries, err)
	}
}

func assertFileContent(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("file %s differs", filepath.Base(path))
	}
}
