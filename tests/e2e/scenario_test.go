//go:build systemtest

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

func TestVaultwardenBackupRestoreE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	harness := newE2EHarness(t, ctx)
	harness.startMotoAndRepository()
	harness.startVaultwardenAndProxy()

	unique := time.Now().UnixNano()
	firstEmail := fmt.Sprintf("vaultwarden-less-e2e-one-%d@example.com", unique)
	secondEmail := fmt.Sprintf("vaultwarden-less-e2e-two-%d@example.com", unique)
	registerAccount(ctx, t, harness.publicHTTP, firstEmail, testPassword)
	registerAccount(ctx, t, harness.publicHTTP, secondEmail, testPassword)
	firstClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-first"), harness.publicTLS, firstEmail, testPassword)
	secondClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-second"), harness.publicTLS, secondEmail, testPassword)
	firstClient.login(ctx)
	secondClient.login(ctx)

	folderName := "Vaultwarden-less Recovery Folder"
	folderID := firstClient.createFolder(ctx, folderName)
	itemName := "Vaultwarden-less E2E Login"
	itemID := firstClient.createItem(ctx, itemName, oldItemPassword, folderID)
	secondItemID := secondClient.createItem(ctx, "Second User Recovery Item", secondPassword, "")
	attachmentData := []byte("vaultwarden-less attachment\nwith a stable payload\n")
	attachmentPath := privateTestPath(t, harness.localRoot, "attachment.txt", attachmentData)
	attachmentID := firstClient.createAttachment(ctx, itemID, attachmentPath)
	sendData := bytes.Repeat([]byte("vaultwarden-less file Send payload\n"), 32768)
	sendPath := privateTestPath(t, harness.localRoot, "send.bin", sendData)
	sendID := firstClient.createFileSend(ctx, sendPath, "Vaultwarden-less Recovery Send")

	// Exercise the real two-stage persistence boundary. Both database rows now
	// exist, but the test-owned source payloads are briefly unavailable. The
	// worker must keep retrying without publishing any partial Restic snapshot.
	sendRelative := harness.singleFileIn("data/sends/" + sendID)
	restoreAttachment := harness.holdDataFile("data/attachments/" + itemID + "/" + attachmentID)
	restoreSend := harness.holdDataFile(sendRelative)
	harness.startBackup()
	harness.waitWorkerState("checking", 2*time.Minute)
	restoreAttachment()
	harness.assertSnapshotCountFor(0, 35*time.Second)
	restoreSend()

	// The startup demand and the proxy's retained pending signal still coalesce
	// into exactly one complete recovery point after the source becomes stable.
	firstSnapshots := harness.waitSnapshotCount(1, 4*time.Minute)
	firstSnapshot := firstSnapshots.Snapshots[0]
	if firstSnapshot.ID == "" || firstSnapshot.ContentID == "" {
		t.Fatalf("first snapshot lacks identity: %#v", firstSnapshot)
	}

	updated := firstClient.getItem(ctx, itemID)
	setItemPassword(t, updated, newItemPassword)
	firstClient.editItem(ctx, itemID, updated)
	secondSnapshots := harness.waitSnapshotCount(2, 7*time.Minute)
	secondSnapshot := secondSnapshots.Snapshots[0]
	if secondSnapshot.ContentID == firstSnapshot.ContentID {
		t.Fatal("item mutation did not change the recovery content ID")
	}

	// Restart intentionally erases all scheduler history. The startup check is
	// the product's recovery mechanism and must not create a duplicate snapshot
	// for identical content.
	harness.compose("restart", "backup")
	harness.waitHealthy("backup", 2*time.Minute)
	harness.waitUnchangedCheck("", 2*time.Minute)
	afterUnchanged, err := harness.snapshots()
	if err != nil || len(afterUnchanged.Snapshots) != 2 {
		t.Fatalf("unchanged startup check changed snapshot count: %#v/%v", afterUnchanged, err)
	}

	latest := harness.restore("", "drill")
	if latest.SnapshotID != secondSnapshot.ID || latest.ContentID != secondSnapshot.ContentID {
		t.Fatalf("latest restore = %#v, want snapshot %s", latest, secondSnapshot.ID)
	}
	harness.stopProduction()
	harness.startDrill(filepath.Join(harness.deployRoot, "restore", "drill"))
	firstClient.expectSyncFailure(ctx)

	restoredFirst := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-restored-first"), harness.publicTLS, firstEmail, testPassword)
	restoredFirst.login(ctx)
	restoredFolder := restoredFirst.getFolder(ctx, folderID)
	if stringValue(restoredFolder["name"]) != folderName {
		t.Fatalf("restored folder name = %q", stringValue(restoredFolder["name"]))
	}
	restoredItem := restoredFirst.getItem(ctx, itemID)
	if stringValue(restoredItem["name"]) != itemName || itemPassword(t, restoredItem) != newItemPassword {
		t.Fatalf("latest restored item is incorrect")
	}
	restoredAttachment := filepath.Join(harness.localRoot, "restored-attachment.txt")
	restoredFirst.getAttachment(ctx, attachmentID, itemID, restoredAttachment)
	assertFileContent(t, restoredAttachment, attachmentData)
	restoredSend := filepath.Join(harness.localRoot, "restored-send.bin")
	restoredFirst.getFileSend(ctx, sendID, restoredSend)
	assertFileContent(t, restoredSend, sendData)
	restoredSecond := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-restored-second"), harness.publicTLS, secondEmail, testPassword)
	restoredSecond.login(ctx)
	if itemPassword(t, restoredSecond.getItem(ctx, secondItemID)) != secondPassword {
		t.Fatal("second user's item did not survive restore")
	}

	harness.stopDrill()
	old := harness.restore(firstSnapshot.ID, "old")
	if old.SnapshotID != firstSnapshot.ID || old.ContentID != firstSnapshot.ContentID {
		t.Fatalf("explicit old restore = %#v", old)
	}
	harness.startDrill(filepath.Join(harness.deployRoot, "restore", "old"))
	oldClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-restored-old"), harness.publicTLS, firstEmail, testPassword)
	oldClient.login(ctx)
	if itemPassword(t, oldClient.getItem(ctx, itemID)) != oldItemPassword {
		t.Fatal("explicit old snapshot did not restore the old item content")
	}
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
