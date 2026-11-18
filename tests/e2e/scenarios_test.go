package e2e_test

import (
	"bytes"
	"context"
	"os"
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
	// Each channel now has a durable failed attempt and a pending Fault.
	h.waitStatus(time.Minute, func(s worker.Status) bool { return len(s.NotificationErrors) == 4 })
	stateDir := filepath.Join(h.deploymentRoot, "state")
	pendingPath := filepath.Join(stateDir, "notifications.json")
	pending, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(stateDir, 0700); err != nil {
			t.Error(err)
		}
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
	h.waitNotificationError("state_write_failed")
	latest := h.waitStatus(7*time.Minute, func(s worker.Status) bool {
		return s.Targets[1].State == "protected" && s.Targets[1].Snapshot != saved.Targets[1].Snapshot
	})
	before := len(h.snapshots("secondary"))
	if before != beforeOutage+1 {
		t.Fatal("secondary did not catch up in one recovery point")
	}
	editWhilePaused := func(password string, previous worker.Status) worker.Status {
		t.Helper()
		client.sync(ctx)
		item := client.getItem(ctx, id)
		setItemPassword(t, item, password)
		client.editItem(ctx, id, item)
		updated := h.waitPass(time.Now())
		for i, target := range updated.Targets {
			if target.Snapshot == previous.Targets[i].Snapshot {
				t.Fatalf("%s missed edit while notifications were paused", target.Name)
			}
		}
		return updated
	}
	t.Log("both repositories keep edited passwords during notification state failures")
	latest = editWhilePaused("saved during write failure", latest)
	h.compose("stop", "backup")
	if err := os.Chmod(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(stateDir, "reports.json")
	reports, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, reportPath, "{")
	h.compose("start", "backup")
	h.waitHealthy("backup", 3*time.Minute)
	h.waitNotificationError("state_invalid")
	latest = editWhilePaused("latest content", latest)
	for path, want := range map[string][]byte{pendingPath: pending, reportPath: []byte("{")} {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatal("paused worker changed a notification record")
		}
	}

	t.Log("repair resumes persisted delivery and a cold comparison keeps both snapshot IDs")
	h.compose("stop", "backup")
	writePrivateFile(t, reportPath, string(reports))
	restarted := time.Now()
	h.compose("start", "backup")
	h.waitHealthy("backup", 3*time.Minute)
	cold := h.waitPass(restarted)
	for i, target := range cold.Targets {
		if target.Snapshot != latest.Targets[i].Snapshot {
			t.Fatal("cold cache created a duplicate recovery point")
		}
	}
	h.assertNotifications()

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

func (h *e2eHarness) waitNotificationError(code string) {
	h.t.Helper()
	h.waitStatus(2*time.Minute, func(s worker.Status) bool {
		if len(s.NotificationErrors) != 4 {
			return false
		}
		for _, value := range s.NotificationErrors {
			if value != code {
				return false
			}
		}
		return true
	})
}
