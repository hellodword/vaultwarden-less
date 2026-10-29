package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

type vaultFixture struct {
	folderID, folderName, itemID, itemPassword, attachmentID, sendID string
	attachmentData, sendData                                         []byte
}

func TestNativeReleaseAcceptance(t *testing.T) {
	if !*runE2E {
		t.Skip("pass -run-e2e with the packaged runner for Docker acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel()
	h := newE2EHarness(t, ctx)
	t.Log("start three products, two independent object stores and notification peer")
	h.start()
	h.waitStatus(3*time.Minute, settled)
	dataRoot := filepath.Join(h.deploymentRoot, "data")
	email := "acceptance@example.com"
	t.Log("shipped profile rejects registration; enable it only for test enrollment")
	expectRegistrationRejected(ctx, t, loopbackEndpoint(h.proxyPort), email, testPassword)
	h.overrideVaultwarden(map[string]string{"SIGNUPS_ALLOWED": "true"})
	registerAccount(ctx, t, loopbackEndpoint(h.proxyPort), email, testPassword)
	h.overrideVaultwarden(map[string]string{})
	client := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-source"), h.publicTLS, h.tlsCAFile, email)
	client.login(ctx)
	t.Log("shipped profile rejects Send creation; enable Sends only for recovery coverage")
	client.expectSendRejected(ctx)
	h.overrideVaultwarden(map[string]string{"SENDS_ALLOWED": "true"})
	fixture := createVaultFixture(t, ctx, h.testRoot, client, "Native recovery")
	beforeRequests := time.Now()
	item := client.getItem(ctx, fixture.itemID)
	fixture.itemPassword = "edited-e2e-password"
	setItemPassword(t, item, fixture.itemPassword)
	client.editItem(ctx, fixture.itemID, item)
	assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-source"), client, fixture)
	h.waitNewRequestCheck(beforeRequests)

	t.Log("burst requests merge; unknown GET requests cause no upload")
	before := h.check(false)
	requestStart := time.Now()
	for i := 0; i < 8; i++ {
		response, err := h.tlsServer.Client().Get(h.publicTLS + fmt.Sprintf("/e2e-private-path-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown route status %d", response.StatusCode)
		}
	}
	immediate := h.status()
	if !immediate.LastCheck.Equal(before.LastCheck) {
		t.Fatal("request burst bypassed the quiet window")
	}
	after := h.waitNewRequestCheck(requestStart)
	h.assertUnchangedIDs(before, after)
	if after.LastCheck.Sub(requestStart) < 29*time.Second {
		t.Fatal("request quiet window too short")
	}
	before = after
	second := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-auth"), h.publicTLS, h.tlsCAFile, email)
	authStart := time.Now()
	second.login(ctx)
	after = h.waitNewRequestCheck(authStart)
	h.assertNewIDs(before, after)
	full := h.check(true)
	h.assertNewIDs(after, full)
	if full.Source != "complete" {
		t.Fatalf("native fixture captured as %s: %v", full.Source, full.Issues)
	}

	t.Log("unlimited Send consumption is ignored; limited consumption persists")
	sendURL := client.fileSendURL(ctx, fixture.sendID)
	receiver := newBitwardenSendReceiver(t, ctx, client.server, client.caFile)
	before = h.check(false)
	if _, _, err := receiver.tryReceive(ctx, sendURL, filepath.Join(h.testRoot, "unlimited-consumption"), false); err != nil {
		t.Fatal("unlimited Send receive failed")
	}
	after = h.check(false)
	h.assertUnchangedIDs(before, after)
	limitedID := client.createFileSendWithLimit(ctx, privateTestPath(t, h.testRoot, "limited-send", []byte("limited synthetic send")), "Limited Send", 2)
	limitedURL := client.fileSendURL(ctx, limitedID)
	before = h.check(false)
	if _, _, err := receiver.tryReceive(ctx, limitedURL, filepath.Join(h.testRoot, "limited-consumption"), false); err != nil {
		t.Fatal("limited Send receive failed")
	}
	after = h.check(false)
	h.assertNewIDs(before, after)
	full = after

	t.Log("192 MiB incompressible attachment is captured using disk workspace")
	large := make([]byte, 192<<20)
	random := rand.New(rand.NewSource(42))
	if _, err := random.Read(large); err != nil {
		t.Fatal(err)
	}
	largePath := privateTestPath(t, h.testRoot, "large-attachment", large)
	largeID := client.createAttachment(ctx, fixture.itemID, largePath)
	large = nil
	before = full
	full = h.check(false)
	h.assertNewIDs(before, full)
	if full.Source != "complete" {
		t.Fatal("large native capture failed")
	}
	t.Logf("large encrypted attachment: %s", largeID)

	t.Log("file-only changes include orphan files, RSA identity, links and empty directories")
	rsa := fileDigest(t, filepath.Join(dataRoot, "rsa_key.pem"))
	writePrivateFile(t, filepath.Join(dataRoot, "orphan.bin"), "orphan bytes\n")
	if err := os.Mkdir(filepath.Join(dataRoot, "unknown-empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("orphan.bin", filepath.Join(dataRoot, "unknown-link")); err != nil {
		t.Fatal(err)
	}
	before = full
	after = h.check(false)
	h.assertNewIDs(before, after)
	if after.Source != "complete" {
		t.Fatalf("file capture failed: %v", after.Issues)
	}

	t.Log("native incomplete snapshots retain references and all available data")
	attachment := filepath.Join(dataRoot, "attachments", fixture.itemID, fixture.attachmentID)
	encrypted, err := os.ReadFile(attachment)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(attachment); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(dataRoot, "unreadable.bin")
	writePrivateFile(t, unreadable, "temporarily unreadable")
	if err = os.Chmod(unreadable, 0000); err != nil {
		t.Fatal(err)
	}
	// Always restore readability so retained diagnostic artifacts can be inspected.
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0600) })
	before = after
	after = h.check(false)
	h.assertNewIDs(before, after)
	if after.Source != "incomplete" || len(after.Issues) == 0 {
		t.Fatal("missing and unreadable files did not mark incomplete")
	}
	incomplete := h.latest("primary")
	if !slices.Contains(incomplete.Tags, "incomplete") {
		t.Fatal("native incomplete tag missing")
	}
	restored := h.restore("primary", incomplete.ID, "incomplete")
	count := h.sqliteReadOnly(restored, "SELECT count(*) FROM attachments WHERE id='"+fixture.attachmentID+"';")
	if strings.TrimSpace(count) != "1" {
		t.Fatal("incomplete capture removed a database reference")
	}
	if _, err = os.Lstat(filepath.Join(restored, "attachments", fixture.itemID, fixture.attachmentID)); !os.IsNotExist(err) {
		t.Fatal("incomplete snapshot substituted an old attachment")
	}
	h.assertUnchangedIDs(after, h.check(false))
	if err = os.WriteFile(attachment, encrypted, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(unreadable, 0600); err != nil {
		t.Fatal(err)
	}
	before = after
	after = h.check(false)
	h.assertNewIDs(before, after)
	if after.Source != "complete" {
		t.Fatalf("repaired capture still incomplete: %v", after.Issues)
	}

	t.Log("unknown SQLite schema and values survive backup and native restore")
	h.sqlite(dataRoot, "CREATE TABLE acceptance_future (value BLOB, detail TEXT); INSERT INTO acceptance_future VALUES (x'0001ff', '{\"future\":{}}'), (NULL, 'duplicate'), (NULL, 'duplicate');")
	before = after
	after = h.check(false)
	h.assertNewIDs(before, after)

	t.Log("one destination fails independently and recovers with the newest waiting version")
	failedID := after.Targets[1].SuccessID
	secondaryContainer := h.containerID("moto-secondary")
	h.mustDocker("network", "disconnect", h.project+"_default", secondaryContainer)
	rejector := h.project + "-rejector"
	h.mustDocker("run", "--detach", "--pull", "never", "--name", rejector,
		"--network", h.project+"_default", "--network-alias", "moto-secondary",
		"--user", h.identity(), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--env", "VWLESS_E2E_REJECTOR=1", "--entrypoint", "/bin/vaultwarden-less-tests", *runnerImage)
	t.Cleanup(func() { h.cleanupContainer(rejector) })
	h.wait(time.Minute, "rejecting object-store endpoint ready", func() bool {
		_, err := h.docker("exec", h.containerID("moto-primary"), "/bin/python3", "-c", `import urllib.error,urllib.request
try:
    urllib.request.urlopen("http://moto-secondary:5000",timeout=2)
except urllib.error.HTTPError as error:
    raise SystemExit(0 if error.code==403 else 1)
raise SystemExit(1)
`)
		return err == nil
	})
	writePrivateFile(t, filepath.Join(dataRoot, "failure-window"), "first")
	h.check(false)
	failed := h.waitStatus(time.Minute, func(s productworker.Status) bool {
		return len(s.Targets) == 2 && s.Targets[1].Error != "" && s.Targets[0].Error == "" && !s.Targets[0].Waiting && !s.Targets[0].Uploading
	})
	if failed.Targets[1].SuccessID != failedID {
		t.Fatal("failed restic advanced its baseline")
	}
	writePrivateFile(t, filepath.Join(dataRoot, "failure-window"), "newest")
	secondFailure := h.check(false)
	if secondFailure.Targets[0].SuccessID == failed.Targets[0].SuccessID {
		t.Fatal("healthy destination stalled")
	}
	if !secondFailure.Targets[1].RetryAt.Equal(failed.Targets[1].RetryAt) {
		t.Fatal("new content reset destination backoff")
	}
	h.mustDocker("rm", "--force", rejector)
	h.mustDocker("network", "connect", "--alias", "moto-secondary", h.project+"_default", secondaryContainer)
	client.sync(ctx)
	h.check(true)
	h.waitStatus(time.Minute, func(s productworker.Status) bool {
		return settled(s) && s.Targets[1].Error == "" && s.Targets[1].SuccessID != failedID
	})

	t.Log("backup outage leaves client service usable; restart reuses native baseline")
	before = h.check(true)
	h.compose("stop", "backup")
	client.sync(ctx)
	writePrivateFile(t, filepath.Join(dataRoot, "during-outage"), "fresh source after restart")
	h.compose("start", "backup")
	h.waitHealthy("backup", time.Minute)
	after = h.waitStatus(3*time.Minute, settled)
	h.assertNewIDs(before, after)
	// Restart again without traffic to independently verify cache reuse.
	before = after
	counts := map[string]int{"primary": len(h.snapshots("primary")), "secondary": len(h.snapshots("secondary"))}
	h.compose("restart", "backup")
	h.waitHealthy("backup", time.Minute)
	after = h.waitStatus(3*time.Minute, settled)
	h.assertUnchangedIDs(before, after)
	for name, count := range counts {
		if len(h.snapshots(name)) != count {
			t.Fatalf("restart uploaded unchanged content to %s", name)
		}
	}

	t.Log("cache loss reconstructs the native comparison baseline without uploading")
	h.compose("stop", "backup")
	cache := filepath.Join(h.deploymentRoot, "cache")
	// Move only this harness's synthetic cache; retain it for investigation.
	if err = os.Rename(cache, filepath.Join(h.testRoot, "cache-before-loss")); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	h.compose("up", "--detach", "--force-recreate", "backup")
	h.waitHealthy("backup", time.Minute)
	after = h.waitStatus(3*time.Minute, settled)
	h.assertUnchangedIDs(before, after)
	for name, count := range counts {
		if got := len(h.snapshots(name)); got != count {
			t.Fatalf("cache loss duplicated snapshot for %s: %d", name, got)
		}
	}
	h.assertNotifications()

	t.Log("native snapshot deletion clears coverage and schedules a replacement")
	removed := h.status().Targets[0].SuccessID
	h.nativeRestic("primary", nil, "forget", removed)
	op := h.operation("verify", "--target", "primary")
	if op.State != "succeeded" {
		t.Fatal("structure check after snapshot deletion failed")
	}
	h.waitStatus(3*time.Minute, func(s productworker.Status) bool {
		return settled(s) && s.Targets[0].SuccessID != "" && s.Targets[0].SuccessID != removed
	})
	for _, name := range []string{"primary", "secondary"} {
		if op := h.operation("verify", "--target", name, "--read-data"); op.State != "succeeded" {
			t.Fatalf("full read failed: %s", name)
		}
	}
	for _, damage := range []string{"delete", "corrupt"} {
		t.Log("remote object " + damage + " is detected; upload cannot clear the verification fault")
		h.damageRemoteObject("primary", damage)
		if op := h.operation("verify", "--target", "primary", "--read-data"); op.State != "failed" {
			t.Fatal("damaged object passed full repository verification")
		}
		after = h.check(true)
		if after.Targets[0].ReadData.Error == "" || after.Targets[1].Error != "" {
			t.Fatal("upload cleared verification failure or stalled the other destination")
		}
		h.damageRemoteObject("primary", "restore")
		if op := h.operation("verify", "--target", "primary", "--read-data"); op.State != "succeeded" {
			t.Fatal("restored remote object did not pass full verification")
		}
		if h.status().Targets[0].ReadData.Error != "" {
			t.Fatal("successful full verification retained fault")
		}
	}

	t.Log("restore each destination into a new root and read it through official clients")
	h.compose("stop", "proxy", "backup", "vaultwarden")
	for _, name := range []string{"primary", "secondary"} {
		latest := h.latest(name)
		root := h.restore(name, latest.ID, name)
		if fileDigest(t, filepath.Join(root, "rsa_key.pem")) != rsa {
			t.Fatal("RSA identity was not preserved")
		}
		assertFileContent(t, filepath.Join(root, "orphan.bin"), []byte("orphan bytes\n"))
		assertFileContent(t, filepath.Join(root, "failure-window"), []byte("newest"))
		link, err := os.Readlink(filepath.Join(root, "unknown-link"))
		if err != nil || link != "orphan.bin" {
			t.Fatal("symbolic link was not restored")
		}
		info, err := os.Stat(filepath.Join(root, "unknown-empty"))
		if err != nil || !info.IsDir() {
			t.Fatal("empty directory was not restored")
		}
		if strings.TrimSpace(h.sqliteReadOnly(root, "SELECT count(*) FROM acceptance_future;")) != "3" {
			t.Fatal("unknown database records were not restored")
		}
		h.startRestored(root, "restored-"+name)
		recovered := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-restored-"+name), h.publicTLS, h.tlsCAFile, email)
		recovered.login(ctx)
		assertVaultFixture(t, ctx, filepath.Join(h.testRoot, "verify-"+name), recovered, fixture)
	}
}

func createVaultFixture(t *testing.T, ctx context.Context, root string, client *bitwardenCLI, label string) vaultFixture {
	t.Helper()
	f := vaultFixture{folderName: label + " folder", itemPassword: initialItemPassword, attachmentData: []byte("vaultwarden-less attachment for " + label + "\n"), sendData: bytes.Repeat([]byte("vaultwarden-less file Send payload\n"), 64)}
	f.folderID = client.createFolder(ctx, f.folderName)
	f.itemID = client.createItem(ctx, label+" login", f.itemPassword, f.folderID)
	f.attachmentID = client.createAttachment(ctx, f.itemID, privateTestPath(t, root, "attachment.txt", f.attachmentData))
	f.sendID = client.createFileSend(ctx, privateTestPath(t, root, "send.bin", f.sendData), label+" Send")
	return f
}
func assertVaultFixture(t *testing.T, ctx context.Context, root string, client *bitwardenCLI, f vaultFixture) {
	t.Helper()
	client.sync(ctx)
	if stringValue(client.getFolder(ctx, f.folderID)["name"]) != f.folderName {
		t.Fatal("folder was not recovered")
	}
	item := client.getItem(ctx, f.itemID)
	if itemPassword(t, item) != f.itemPassword || stringValue(item["folderId"]) != f.folderID {
		t.Fatal("edited item was not recovered")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	client.getAttachment(ctx, f.attachmentID, f.itemID, filepath.Join(root, "attachment"))
	assertFileContent(t, filepath.Join(root, "attachment"), f.attachmentData)
	client.getFileSend(ctx, f.sendID, filepath.Join(root, "send"))
	assertFileContent(t, filepath.Join(root, "send"), f.sendData)
}
func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file payload differs: %s", filepath.Base(path))
	}
}
func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}
