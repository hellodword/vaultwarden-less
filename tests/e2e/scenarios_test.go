package e2e_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/worker"
)

func TestNativeReleaseAcceptance(t *testing.T) {
	if !*runE2E {
		t.Skip("run with the offline release and its packaged test runner")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 24*time.Minute)
	defer cancel()
	h := newE2EHarness(t, ctx)
	started := time.Now()
	h.start()
	baseline := h.waitPass(started)
	email := "acceptance@example.com"
	expectRegistrationRejected(ctx, t, loopbackEndpoint(h.proxyPort), email, testPassword)
	h.overrideVaultwarden(map[string]string{"SIGNUPS_ALLOWED": "true"})
	registerAccount(ctx, t, loopbackEndpoint(h.proxyPort), email, testPassword)
	h.overrideVaultwarden(map[string]string{})
	client := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-source"), h.publicTLS, h.tlsCAFile, email)
	client.login(ctx)
	folder := client.createFolder(ctx, "Operations")
	id := client.createItem(ctx, "Recovery account", initialItemPassword, folder)
	saved := h.waitPass(time.Now())
	for i, s := range saved.Targets {
		if s.Snapshot == baseline.Targets[i].Snapshot {
			t.Fatal("personal content was not backed up")
		}
	}

	t.Log("new device login and sync trigger comparison without a new recovery point")
	fresh := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-device"), h.publicTLS, h.tlsCAFile, email)
	fresh.login(ctx)
	fresh.sync(ctx)
	observed := h.waitPass(time.Now())
	for i, s := range observed.Targets {
		if s.Snapshot != saved.Targets[i].Snapshot {
			t.Fatal("authentication created a snapshot")
		}
	}

	t.Log("one repository rejects credentials while its peer protects edited content")
	beforeOutage := len(h.snapshots("secondary"))
	passwordPath := filepath.Join(h.deploymentRoot, "credentials/restic/secondary")
	writePrivateFile(t, passwordPath, "synthetic incorrect password\n")
	item := client.getItem(ctx, id)
	item["favorite"] = true
	setItemPassword(t, item, "updated content")
	client.editItem(ctx, id, item)
	h.waitStatus(3*time.Minute, func(s worker.Status) bool {
		return len(s.Targets) == 2 && s.Targets[0].State == "protected" && s.Targets[0].Snapshot != saved.Targets[0].Snapshot && s.Targets[1].Error == "password_incorrect"
	})
	client.sync(ctx)
	item = client.getItem(ctx, id)
	setItemPassword(t, item, "latest content")
	client.editItem(ctx, id, item)
	edit := time.Now()
	h.waitStatus(3*time.Minute, func(s worker.Status) bool {
		return s.Targets[0].CheckedAt.After(edit) && s.Targets[0].State == "protected"
	})
	writePrivateFile(t, passwordPath, "e2e-secondary-restic-password\n")
	h.assertNotifications()
	latest := h.waitStatus(7*time.Minute, func(s worker.Status) bool {
		return s.Targets[1].State == "protected" && s.Targets[1].Snapshot != saved.Targets[1].Snapshot
	})
	before := len(h.snapshots("secondary"))
	if before != beforeOutage+1 {
		t.Fatal("secondary did not catch up in one recovery point")
	}
	restarted := time.Now()
	h.compose("restart", "backup")
	h.waitHealthy("backup", 3*time.Minute)
	cold := h.waitPass(restarted)
	if cold.Targets[1].Snapshot != latest.Targets[1].Snapshot || len(h.snapshots("secondary")) != before {
		t.Fatal("cold cache created a duplicate recovery point")
	}

	t.Log("recover and decrypt the same personal item independently from each repository")
	h.compose("stop", "proxy", "backup", "vaultwarden")
	for _, s := range cold.Targets {
		h.nativeRestic(s.Name, nil, "check", "--read-data")
		root := h.restore(s.Name, s.Snapshot, "restore-"+s.Name)
		h.startRestored(root, "restore-"+s.Name)
		recovered := newBitwardenCLI(t, filepath.Join(h.testRoot, "bw-"+s.Name), h.publicTLS, h.tlsCAFile, email)
		recovered.login(ctx)
		recovered.sync(ctx)
		item := recovered.getItem(ctx, id)
		if itemPassword(t, item) != "latest content" || item["favorite"] != true || item["folderId"] != folder || recovered.getFolder(ctx, folder)["name"] != "Operations" {
			t.Fatalf("%s did not recover the personal vault", s.Name)
		}
	}
}
