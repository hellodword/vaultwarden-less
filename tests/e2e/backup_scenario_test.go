package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

func TestBackupRestoreLifecycle(t *testing.T) {
	if !*runBackupE2E {
		t.Skip("pass -run-backup-e2e to run the real backup/restore lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	harness := newE2EHarness(t, ctx)
	harness.startMotoAndRepository()
	harness.initializeDeployment()
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	firstEmail := fmt.Sprintf("vaultwarden-less-e2e-backup-a-%d@example.com", unique)
	secondEmail := fmt.Sprintf("vaultwarden-less-e2e-backup-b-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, firstEmail, testPassword)
	_ = registerAccount(ctx, t, harness.publicHTTP, secondEmail, secondPassword)
	firstClient := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-backup-a"),
		harness.publicTLS,
		harness.tlsCAFile,
		firstEmail,
	)
	secondClient := newBitwardenCLIWithPassword(
		t,
		filepath.Join(harness.localRoot, "bw-backup-b"),
		harness.publicTLS,
		harness.tlsCAFile,
		secondEmail,
		secondPassword,
	)
	firstClient.login(ctx)
	secondClient.login(ctx)
	firstFolderID := firstClient.createFolder(ctx, "Recovery Folder A")
	secondFolderID := secondClient.createFolder(ctx, "Recovery Folder B")
	firstItemID := firstClient.createItem(ctx, "Recovery Login A", initialItemPassword, firstFolderID)
	secondItemID := secondClient.createItem(ctx, "Recovery Login B", secondItemPassword, secondFolderID)

	attachmentData := []byte("vaultwarden-less attachment\nwith a stable payload\n")
	if len(attachmentData) > 4<<10 {
		t.Fatal("attachment fixture exceeds the bounded scenario contract")
	}
	attachmentPath := privateTestPath(t, harness.localRoot, "attachment.txt", attachmentData)
	attachmentID := firstClient.createAttachment(ctx, firstItemID, attachmentPath)

	sendData := bytes.Repeat([]byte("vaultwarden-less file Send payload\n"), 512)
	if len(sendData) > 32<<10 {
		t.Fatal("Send fixture exceeds the bounded scenario contract")
	}
	sendPath := privateTestPath(t, harness.localRoot, "send.bin", sendData)
	sendID := firstClient.createFileSend(ctx, sendPath, "Recovery Send")

	limitedSendData := bytes.Repeat([]byte("vaultwarden-less exhausted Send payload\n"), 128)
	limitedSendPath := privateTestPath(t, harness.localRoot, "limited-send.bin", limitedSendData)
	limitedSendID := secondClient.createFileSendWithLimit(ctx, limitedSendPath, "Exhausted Recovery Send", 1)
	consumedLimitedSend := filepath.Join(harness.localRoot, "consumed-limited-send.bin")
	secondClient.getFileSend(ctx, limitedSendID, consumedLimitedSend)
	assertFileContent(t, consumedLimitedSend, limitedSendData)
	secondClient.expectFileSendExhausted(
		ctx,
		limitedSendID,
		filepath.Join(harness.localRoot, "unexpected-second-limited-send.bin"),
	)

	harness.startBackup()
	firstSnapshots := harness.waitSnapshotCount(1, 4*time.Minute)
	firstSnapshot := firstSnapshots.Snapshots[0]
	if firstSnapshot.ID == "" || firstSnapshot.ContentID == "" {
		t.Fatalf("first snapshot lacks identity: %#v", firstSnapshot)
	}
	harness.startProxy()

	updated := firstClient.getItem(ctx, firstItemID)
	setItemPassword(t, updated, newItemPassword)
	firstClient.editItem(ctx, firstItemID, updated)
	secondSnapshots := harness.waitSnapshotCount(2, 7*time.Minute)
	latestSnapshot := secondSnapshots.Snapshots[0]
	if latestSnapshot.ContentID == firstSnapshot.ContentID {
		t.Fatal("second login generation did not change recovery content")
	}
	harness.repositoryCheck()

	latest := harness.restore("", "drill")
	if latest.SnapshotID != latestSnapshot.ID || latest.ProjectCommit != latestSnapshot.ProjectCommit ||
		latest.ContentID != latestSnapshot.ContentID || latest.ArchiveID != latestSnapshot.ArchiveID {
		t.Fatalf("latest restore = %#v, want snapshot %s", latest, latestSnapshot.ID)
	}
	harness.stopProduction()
	harness.startDrill("drill")
	firstClient.expectSyncFailure(ctx)
	secondClient.expectSyncFailure(ctx)

	restoredFirst := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-restored-a"),
		harness.publicTLS,
		harness.tlsCAFile,
		firstEmail,
	)
	restoredSecond := newBitwardenCLIWithPassword(
		t,
		filepath.Join(harness.localRoot, "bw-restored-b"),
		harness.publicTLS,
		harness.tlsCAFile,
		secondEmail,
		secondPassword,
	)
	restoredFirst.login(ctx)
	restoredSecond.login(ctx)
	firstItem := restoredFirst.getItem(ctx, firstItemID)
	if itemPassword(t, firstItem) != newItemPassword || stringValue(firstItem["folderId"]) != firstFolderID {
		t.Fatal("latest login generation did not survive restore")
	}
	secondItem := restoredSecond.getItem(ctx, secondItemID)
	if itemPassword(t, secondItem) != secondItemPassword || stringValue(secondItem["folderId"]) != secondFolderID {
		t.Fatal("second user's independent folder item did not survive restore")
	}
	if stringValue(restoredFirst.getFolder(ctx, firstFolderID)["name"]) != "Recovery Folder A" ||
		stringValue(restoredSecond.getFolder(ctx, secondFolderID)["name"]) != "Recovery Folder B" {
		t.Fatal("independent user folders did not survive restore")
	}
	restoredAttachment := filepath.Join(harness.localRoot, "restored-attachment.txt")
	restoredFirst.getAttachment(ctx, attachmentID, firstItemID, restoredAttachment)
	assertFileContent(t, restoredAttachment, attachmentData)
	restoredSend := filepath.Join(harness.localRoot, "restored-send.bin")
	restoredFirst.getFileSend(ctx, sendID, restoredSend)
	assertFileContent(t, restoredSend, sendData)
	restoredSecond.expectFileSendExhausted(
		ctx,
		limitedSendID,
		filepath.Join(harness.localRoot, "unexpected-restored-limited-send.bin"),
	)
	harness.stopDrill()
}

func TestTargetlessDeploymentLifecycle(t *testing.T) {
	if !*runBackupE2E {
		t.Skip("pass -run-backup-e2e to run the targetless deployment lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	harness := newTargetlessE2EHarness(t, ctx)

	for _, role := range []string{"backup", "maintenance"} {
		output := harness.compose(
			"--profile", "maintenance", "run", "--rm", "--no-deps", "config-check-"+role,
		)
		var result struct {
			Valid   bool     `json:"valid"`
			Role    string   `json:"role"`
			Targets []string `json:"targets"`
		}
		if err := decodeExactJSON(output, &result); err != nil || !result.Valid || result.Role != role || result.Targets == nil || len(result.Targets) != 0 {
			t.Fatalf("targetless %s config validation = %#v/%v: %s", role, result, err, harnessDiagnostics(output))
		}
	}
	if entries, err := os.ReadDir(filepath.Join(harness.localRoot, "credentials", "restic")); err != nil || len(entries) != 0 {
		t.Fatalf("targetless Restic password directory = %#v/%v", entries, err)
	}
	harness.initializeDeployment()
	harness.startVaultwarden()
	harness.startBackup()
	harness.startProxy()
	for _, service := range []string{"moto-primary", "moto-secondary"} {
		output, err := harness.tryCompose(ctx, "ps", "--all", "--quiet", service)
		if err != nil || strings.TrimSpace(output) != "" {
			t.Fatalf("targetless lifecycle started %s: %v: %s", service, err, harnessDiagnostics(output))
		}
	}

	backupContainer := strings.TrimSpace(harness.compose("ps", "--quiet", "backup"))
	logs, err := harness.dockerLogs(backupContainer)
	if err != nil {
		t.Fatalf("read targetless backup logs: %v: %s", err, harnessDiagnostics(logs))
	}
	if strings.Count(logs, `"warning_code":"no_backup_targets"`) != 1 ||
		!strings.Contains(logs, `"level":"WARN"`) ||
		!strings.Contains(logs, "Vaultwarden data is not being backed up") {
		t.Fatalf("targetless startup warning is missing or duplicated: %s", harnessDiagnostics(logs))
	}
	status := harness.status()
	if status.State != "idle" || status.NextCheckAt != nil || status.LastCheck != nil || status.Targets == nil || len(status.Targets) != 0 {
		t.Fatalf("targetless worker status = %#v", status)
	}
	if _, err := os.Lstat(filepath.Join(harness.localRoot, "staging", "point")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("targetless worker created a staging point: %v", err)
	}
	requestOutput, requestErr := harness.tryCompose(
		ctx, "exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "request-backup",
	)
	if requestErr == nil || exactCommandErrorCode(t, requestOutput) != "not_ready" ||
		!strings.Contains(requestOutput, productworker.NoBackupTargetsMessage) {
		t.Fatalf("targetless request-backup = %v: %s", requestErr, harnessDiagnostics(requestOutput))
	}

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-e2e-targetless-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, email, testPassword)
	client := newBitwardenCLI(
		t, filepath.Join(harness.localRoot, "bw-targetless-initial"), harness.publicTLS, harness.tlsCAFile, email,
	)
	client.login(ctx)
	folderID := client.createFolder(ctx, "Targetless Persistence Folder")
	itemID := client.createItem(ctx, "Targetless Persistence Login", initialItemPassword, folderID)
	if itemPassword(t, client.getItem(ctx, itemID)) != initialItemPassword {
		t.Fatal("targetless proxy write was not readable before recreation")
	}

	setTargetlessSignups(t, filepath.Join(harness.localRoot, "config", "vaultwarden.env"), false)
	oldVaultwarden := strings.TrimSpace(harness.compose("ps", "--quiet", "vaultwarden"))
	harness.compose("up", "--detach", "--force-recreate", "vaultwarden")
	harness.waitHealthy("vaultwarden", 3*time.Minute)
	newVaultwarden := strings.TrimSpace(harness.compose("ps", "--quiet", "vaultwarden"))
	if oldVaultwarden == "" || newVaultwarden == "" || oldVaultwarden == newVaultwarden {
		t.Fatalf("Vaultwarden was not force-recreated: %q/%q", oldVaultwarden, newVaultwarden)
	}

	restarted := newBitwardenCLI(
		t, filepath.Join(harness.localRoot, "bw-targetless-restarted"), harness.publicTLS, harness.tlsCAFile, email,
	)
	restarted.login(ctx)
	item := restarted.getItem(ctx, itemID)
	if itemPassword(t, item) != initialItemPassword || stringValue(item["folderId"]) != folderID {
		t.Fatal("existing targetless user data did not survive Vaultwarden recreation")
	}
	expectRegistrationRejected(
		ctx, t, harness.publicHTTP,
		fmt.Sprintf("vaultwarden-less-e2e-targetless-rejected-%d@example.com", unique), secondPassword,
	)
	status = harness.status()
	if status.State != "idle" || status.NextCheckAt != nil || status.LastCheck != nil || len(status.Targets) != 0 {
		t.Fatalf("targetless worker changed state after proxy writes: %#v", status)
	}
	if _, err := os.Lstat(filepath.Join(harness.localRoot, "staging", "point")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("targetless proxy events created a staging point: %v", err)
	}
}

func setTargetlessSignups(t *testing.T, path string, allowed bool) {
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
		t.Fatalf("Vaultwarden signup setting has an unexpected shape")
	}
	data = []byte(strings.Replace(string(data), from, to, 1))
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
	if written, err := file.Write(data); err != nil || written != len(data) {
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

func assertFileContent(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("restored file %s differs", filepath.Base(path))
	}
}
