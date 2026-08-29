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
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-e2e-backup-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, email, testPassword)
	client := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-backup"),
		harness.publicTLS,
		harness.tlsCAFile,
		email,
	)
	client.login(ctx)
	itemID := client.createItem(ctx, "Recovery Login", initialItemPassword, "")

	attachmentData := []byte("vaultwarden-less attachment\nwith a stable payload\n")
	if len(attachmentData) > 4<<10 {
		t.Fatal("attachment fixture exceeds the bounded scenario contract")
	}
	attachmentPath := privateTestPath(t, harness.localRoot, "attachment.txt", attachmentData)
	attachmentID := client.createAttachment(ctx, itemID, attachmentPath)

	sendData := bytes.Repeat([]byte("vaultwarden-less file Send payload\n"), 512)
	if len(sendData) > 32<<10 {
		t.Fatal("Send fixture exceeds the bounded scenario contract")
	}
	sendPath := privateTestPath(t, harness.localRoot, "send.bin", sendData)
	sendID := client.createFileSend(ctx, sendPath, "Recovery Send")

	harness.startBackup()
	firstSnapshots := harness.waitSnapshotCount(1, 4*time.Minute)
	firstSnapshot := firstSnapshots.Snapshots[0]
	if firstSnapshot.ID == "" || firstSnapshot.ContentID == "" {
		t.Fatalf("first snapshot lacks identity: %#v", firstSnapshot)
	}
	harness.startProxy()

	updated := client.getItem(ctx, itemID)
	setItemPassword(t, updated, newItemPassword)
	client.editItem(ctx, itemID, updated)
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
	client.expectSyncFailure(ctx)

	restored := newBitwardenCLI(
		t,
		filepath.Join(harness.localRoot, "bw-restored"),
		harness.publicTLS,
		harness.tlsCAFile,
		email,
	)
	restored.login(ctx)
	if itemPassword(t, restored.getItem(ctx, itemID)) != newItemPassword {
		t.Fatal("latest login generation did not survive restore")
	}
	restoredAttachment := filepath.Join(harness.localRoot, "restored-attachment.txt")
	restored.getAttachment(ctx, attachmentID, itemID, restoredAttachment)
	assertFileContent(t, restoredAttachment, attachmentData)
	restoredSend := filepath.Join(harness.localRoot, "restored-send.bin")
	restored.getFileSend(ctx, sendID, restoredSend)
	assertFileContent(t, restoredSend, sendData)
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
