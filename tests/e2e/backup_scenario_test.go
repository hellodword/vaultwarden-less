package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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
