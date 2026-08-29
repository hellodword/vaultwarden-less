package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupRestoreLifecycle(t *testing.T) {
	if !*runBackupE2E {
		t.Skip("pass -run-backup-e2e to run the real backup/restore lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	harness := newE2EHarness(t, ctx)
	harness.startMotoAndRepository()
	harness.initializeDeployment()
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	firstEmail := fmt.Sprintf("vaultwarden-less-e2e-one-%d@example.com", unique)
	secondEmail := fmt.Sprintf("vaultwarden-less-e2e-two-%d@example.com", unique)
	firstRegistration := registerAccount(ctx, t, harness.publicHTTP, firstEmail, testPassword)
	_ = registerAccount(ctx, t, harness.publicHTTP, secondEmail, testPassword)
	firstClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-first"), harness.publicTLS, harness.tlsCAFile, firstEmail)
	secondClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-second"), harness.publicTLS, harness.tlsCAFile, secondEmail)
	firstClient.login(ctx)
	secondClient.login(ctx)
	organizationName := "Vaultwarden-less Recovery Organization"
	initialCollectionName := "Default Recovery Collection"
	organizationID := createOrganization(
		ctx,
		t,
		harness.publicHTTP,
		firstEmail,
		organizationName,
		initialCollectionName,
		firstRegistration,
	)
	firstClient.sync(ctx)
	initialCollection := firstClient.collectionByName(ctx, organizationID, initialCollectionName)
	initialCollectionID := stringValue(initialCollection["id"])
	if initialCollectionID == "" {
		t.Fatal("initial organization collection has no ID")
	}
	if stringValue(firstClient.getOrganization(ctx, organizationID)["name"]) != organizationName {
		t.Fatal("created organization was not readable through the official CLI")
	}
	members := firstClient.organizationMembers(ctx, organizationID)
	if len(members) != 1 || !strings.EqualFold(stringValue(members[0]["email"]), firstEmail) {
		t.Fatalf("official CLI organization owner membership = %#v", members)
	}
	collectionName := "Audited Recovery Collection"
	collectionID := firstClient.createOrganizationCollection(ctx, organizationID, collectionName)
	organizationItemID := firstClient.createOrganizationItem(
		ctx,
		"Organization Recovery Item",
		organizationItemPassword,
		organizationID,
		collectionID,
	)
	if initialCollectionID == collectionID {
		t.Fatal("organization collection creation reused the initial collection")
	}

	folderName := "Vaultwarden-less Recovery Folder"
	folderID := firstClient.createFolder(ctx, folderName)
	itemName := "Vaultwarden-less E2E Login"
	itemID := firstClient.createItem(ctx, itemName, initialItemPassword, folderID)
	secondItemID := secondClient.createItem(ctx, "Second User Recovery Item", secondPassword, "")
	attachmentData := []byte("vaultwarden-less attachment\nwith a stable payload\n")
	attachmentPath := privateTestPath(t, harness.localRoot, "attachment.txt", attachmentData)
	attachmentID := firstClient.createAttachment(ctx, itemID, attachmentPath)
	sendData := bytes.Repeat([]byte("vaultwarden-less file Send payload\n"), 32768)
	sendPath := privateTestPath(t, harness.localRoot, "send.bin", sendData)
	sendID := firstClient.createFileSend(ctx, sendPath, "Vaultwarden-less Recovery Send")

	// Exercise the real two-stage persistence boundary. The database row exists,
	// but its test-owned Send payload is briefly unavailable. The
	// worker must keep retrying without publishing any partial Restic snapshot.
	sendRelative := harness.singleFileIn("data/sends/" + sendID)
	restoreSend := harness.holdDataFile(sendRelative)
	harness.startBackup()
	harness.waitWorkerState("checking", 2*time.Minute)
	harness.assertSnapshotCountFor(0, 35*time.Second)
	restoreSend()

	// The startup demand and the proxy's retained pending signal still coalesce
	// into exactly one complete recovery point after the source becomes stable.
	firstSnapshots := harness.waitSnapshotCount(1, 4*time.Minute)
	firstSnapshot := firstSnapshots.Snapshots[0]
	if firstSnapshot.ID == "" || firstSnapshot.ContentID == "" {
		t.Fatalf("first snapshot lacks identity: %#v", firstSnapshot)
	}
	harness.startProxy()

	// Keep the first Send unavailable while every second-state mutation occurs.
	// This closes the race in which the worker could publish an intermediate
	// item edit or unconsumed access-limited Send as an extra recovery point.
	restoreSecondWindow := harness.holdDataFile(sendRelative)
	updated := firstClient.getItem(ctx, itemID)
	setItemPassword(t, updated, newItemPassword)
	firstClient.editItem(ctx, itemID, updated)
	limitedData := []byte("access-limited Send payload\n")
	limitedPath := privateTestPath(t, harness.localRoot, "limited-send.bin", limitedData)
	limitedSendID := firstClient.createFileSendWithLimit(ctx, limitedPath, "Access Count Recovery Send", 1)
	limitedDownload := filepath.Join(harness.localRoot, "limited-send-download.bin")
	firstClient.getFileSend(ctx, limitedSendID, limitedDownload)
	assertFileContent(t, limitedDownload, limitedData)
	firstClient.waitFileSendAccessCount(ctx, limitedSendID, 1, 30*time.Second)
	firstClient.expectFileSendAccessFailure(ctx, limitedSendID)
	harness.assertSnapshotCountFor(1, 20*time.Second)
	restoreSecondWindow()
	secondSnapshots := harness.waitSnapshotCount(2, 7*time.Minute)
	latestSnapshot := secondSnapshots.Snapshots[0]
	if latestSnapshot.ContentID == firstSnapshot.ContentID {
		t.Fatal("second state did not change the recovery content ID")
	}

	// A new device login is session/token-only activity. It must traverse the
	// proxy event boundary and produce a fresh observed raw archive without
	// publishing a third logical recovery point.
	beforeSession := harness.status()
	if beforeSession.LastCheck == nil || beforeSession.LastCheck.ArchiveID == nil || beforeSession.LastCheck.ContentID == nil {
		t.Fatalf("second recovery check lacks identity: %#v", beforeSession.LastCheck)
	}
	sessionClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-session-only"), harness.publicTLS, harness.tlsCAFile, firstEmail)
	sessionClient.login(ctx)
	afterSession := harness.waitCheckAfter(beforeSession.LastCheck.FinishedAt, "unchanged", 7*time.Minute)
	if afterSession.LastCheck == nil || afterSession.LastCheck.ArchiveID == nil || afterSession.LastCheck.ContentID == nil {
		t.Fatalf("session-only check lacks identity: %#v", afterSession.LastCheck)
	}
	if *afterSession.LastCheck.ContentID != latestSnapshot.ContentID {
		t.Fatalf("session-only content ID = %q, want %q", *afterSession.LastCheck.ContentID, latestSnapshot.ContentID)
	}
	if *afterSession.LastCheck.ArchiveID == *beforeSession.LastCheck.ArchiveID {
		t.Fatal("session-only activity did not change the observed archive ID")
	}
	if snapshots := harness.waitSnapshotCount(2, 2*time.Minute); len(snapshots.Snapshots) != 2 {
		t.Fatalf("session-only activity changed snapshot count: %#v", snapshots)
	}

	harness.repositoryCheck()
	for _, apply := range []bool{false, true} {
		pruned := harness.prune(apply)
		kept := map[string]bool{}
		for _, group := range pruned.Groups {
			if len(group.RemoveSnapshotIDs) != 0 {
				t.Fatalf("two current recovery points were selected for removal: %#v", pruned)
			}
			for _, id := range group.KeepSnapshotIDs {
				kept[id] = true
			}
		}
		if len(kept) != 2 || !kept[firstSnapshot.ID] || !kept[latestSnapshot.ID] {
			t.Fatalf("prune did not report the exact retained set: %#v", pruned)
		}
		if snapshots := harness.waitSnapshotCount(2, 2*time.Minute); len(snapshots.Snapshots) != 2 ||
			snapshots.Snapshots[0].ID != latestSnapshot.ID ||
			snapshots.Snapshots[1].ID != firstSnapshot.ID {
			t.Fatalf("prune changed the exact two-snapshot set: %#v", snapshots)
		}
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
	if latest.SnapshotID != latestSnapshot.ID || latest.ProjectCommit != latestSnapshot.ProjectCommit ||
		latest.ContentID != latestSnapshot.ContentID || latest.ArchiveID != latestSnapshot.ArchiveID {
		t.Fatalf("latest restore = %#v, want snapshot %s", latest, latestSnapshot.ID)
	}
	harness.stopProduction()
	harness.startDrill("drill")
	firstClient.expectSyncFailure(ctx)

	restoredFirst := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-restored-first"), harness.publicTLS, harness.tlsCAFile, firstEmail)
	restoredFirst.login(ctx)
	if stringValue(restoredFirst.getOrganization(ctx, organizationID)["name"]) != organizationName {
		t.Fatal("organization did not survive restore")
	}
	restoredMembers := restoredFirst.organizationMembers(ctx, organizationID)
	if len(restoredMembers) != 1 ||
		!strings.EqualFold(stringValue(restoredMembers[0]["email"]), firstEmail) {
		t.Fatalf("restored organization owner membership = %#v", restoredMembers)
	}
	if stringValue(restoredFirst.collectionByName(ctx, organizationID, initialCollectionName)["id"]) != initialCollectionID {
		t.Fatal("initial organization collection did not survive restore")
	}
	if stringValue(restoredFirst.collectionByName(ctx, organizationID, collectionName)["id"]) != collectionID {
		t.Fatal("organization collection did not survive restore")
	}
	if itemPassword(t, restoredFirst.getItem(ctx, organizationItemID)) != organizationItemPassword {
		t.Fatal("organization item did not survive restore")
	}
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
	restoredFirst.expectFileSendAccessFailure(ctx, limitedSendID)
	restoredSecond := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-restored-second"), harness.publicTLS, harness.tlsCAFile, secondEmail)
	restoredSecond.login(ctx)
	if itemPassword(t, restoredSecond.getItem(ctx, secondItemID)) != secondPassword {
		t.Fatal("second user's item did not survive restore")
	}

	harness.stopDrill()
	earlier := harness.restore(firstSnapshot.ID, "earlier")
	if earlier.SnapshotID != firstSnapshot.ID || earlier.ProjectCommit != firstSnapshot.ProjectCommit ||
		earlier.ContentID != firstSnapshot.ContentID || earlier.ArchiveID != firstSnapshot.ArchiveID {
		t.Fatalf("explicit earlier restore = %#v", earlier)
	}
	harness.startDrill("earlier")
	earlierClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-restored-earlier"), harness.publicTLS, harness.tlsCAFile, firstEmail)
	earlierClient.login(ctx)
	if itemPassword(t, earlierClient.getItem(ctx, itemID)) != initialItemPassword {
		t.Fatal("explicit earlier snapshot did not restore the earlier item content")
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
