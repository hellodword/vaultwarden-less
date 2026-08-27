package e2e_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

func TestMultiTargetDegradationAndCatchUp(t *testing.T) {
	if !*runMultiTargetE2E {
		t.Skip("pass -run-multitarget-e2e to run the real multi-target lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel()
	harness := newE2EHarness(t, ctx, "primary", "secondary")
	harness.startMotoAndRepository()
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-e2e-multitarget-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, email, testPassword)
	client := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-multitarget"), harness.publicTLS, harness.tlsCAFile, email)
	client.login(ctx)
	itemID := client.createItem(ctx, "Multi-target Recovery Item", initialItemPassword, "")

	// Make the secondary repository genuinely unavailable before the resident
	// worker observes its first source point. Primary must still publish it.
	restoreSecondary := rejectSecondaryEndpoint(t, harness)
	harness.startBackup()
	partial := waitMultiTargetStatus(t, harness, 4*time.Minute, func(status productworker.Status) bool {
		primary, secondary := exactTwoTargets(t, status)
		return status.State == "partial" && status.LastCheck != nil && status.LastCheck.Result == "partial" &&
			primary.State == "healthy" && primary.ContentID != nil && primary.SnapshotID != nil &&
			secondary.State == "retrying" && secondary.ErrorCode != nil && secondary.ContentID == nil
	})
	primaryStatus, secondaryStatus := exactTwoTargets(t, partial)
	harness.startProxy()
	if partial.LastCheck == nil || partial.LastCheck.ContentID == nil || partial.LastCheck.ArchiveID == nil ||
		primaryStatus.ContentID == nil || *primaryStatus.ContentID != *partial.LastCheck.ContentID {
		t.Fatalf("partial status identities are incomplete: %#v", partial)
	}
	contentID := *partial.LastCheck.ContentID
	harness.waitNotificationContains(30*time.Second, "created=[primary]", "synced=[]", "failed=[secondary]")
	primarySnapshots := harness.waitSnapshotCount(1, 2*time.Minute)
	if primarySnapshots.Snapshots[0].ContentID != contentID {
		t.Fatalf("primary snapshot content ID = %q, want %q", primarySnapshots.Snapshots[0].ContentID, contentID)
	}

	// The product contract fixes degradation at 30 minutes. Keep the real
	// endpoint down through normal five-minute retries; no clock override or
	// product test hook is permitted here.
	degraded := waitMultiTargetStatus(t, harness, 37*time.Minute, func(status productworker.Status) bool {
		primary, secondary := exactTwoTargets(t, status)
		return status.State == "degraded" && primary.State == "healthy" &&
			secondary.State == "degraded" && secondary.ErrorCode != nil
	})
	degradedPrimary, degradedSecondary := exactTwoTargets(t, degraded)
	if degradedPrimary.SnapshotID == nil || *degradedPrimary.SnapshotID != *primaryStatus.SnapshotID ||
		degradedSecondary.ErrorCode == nil || secondaryStatus.ErrorCode == nil ||
		*degradedSecondary.ErrorCode != *secondaryStatus.ErrorCode {
		t.Fatalf("degraded status changed the wrong target: %#v", degraded)
	}
	harness.waitNotificationContains(30*time.Second, "target secondary is degraded", *degradedSecondary.ErrorCode)

	// Restoring only the failed endpoint must catch it up to the already-known
	// logical content without requiring another Vaultwarden mutation.
	restoreSecondary()
	harness.createMotoBucket("secondary")
	harness.initRepository("secondary")
	harness.requestBackup()
	recovered := waitMultiTargetStatus(t, harness, 8*time.Minute, func(status productworker.Status) bool {
		primary, secondary := exactTwoTargets(t, status)
		return status.State == "idle" && status.LastCheck != nil && status.LastCheck.Result == "created" &&
			primary.State == "healthy" && secondary.State == "healthy" &&
			primary.ContentID != nil && secondary.ContentID != nil &&
			*primary.ContentID == contentID && *secondary.ContentID == contentID &&
			secondary.SnapshotID != nil
	})
	recoveredPrimary, recoveredSecondary := exactTwoTargets(t, recovered)
	if recoveredPrimary.SnapshotID == nil || *recoveredPrimary.SnapshotID != *primaryStatus.SnapshotID {
		t.Fatalf("catch-up replaced the primary recovery point: %#v", recoveredPrimary)
	}
	harness.waitNotificationContains(30*time.Second, "target secondary has recovered")
	harness.waitNotificationContains(30*time.Second, "synced=[primary]", "created=[secondary]", "failed=[]")

	primarySnapshots = harness.waitSnapshotCount(1, 2*time.Minute)
	secondarySnapshots := waitTargetSnapshotCount(t, harness, "secondary", 1, 2*time.Minute)
	primarySnapshot := primarySnapshots.Snapshots[0]
	secondarySnapshot := secondarySnapshots.Snapshots[0]
	if primarySnapshot.ContentID != contentID || secondarySnapshot.ContentID != contentID ||
		primarySnapshot.ID != *recoveredPrimary.SnapshotID || secondarySnapshot.ID != *recoveredSecondary.SnapshotID {
		t.Fatalf("repositories did not converge to one logical content: primary=%#v secondary=%#v", primarySnapshot, secondarySnapshot)
	}
	for _, target := range []string{"primary", "secondary"} {
		harness.repositoryCheckTarget(target)
	}

	primaryRestore := harness.restoreTarget("primary", primarySnapshot.ID, "multi-primary")
	secondaryRestore := harness.restoreTarget("secondary", secondarySnapshot.ID, "multi-secondary")
	assertRestoreMatchesSnapshot(t, primaryRestore, primarySnapshot)
	assertRestoreMatchesSnapshot(t, secondaryRestore, secondarySnapshot)

	harness.stopProduction()
	harness.startDrill("multi-primary")
	client.expectSyncFailure(ctx)
	primaryClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-multitarget-primary-restore"), harness.publicTLS, harness.tlsCAFile, email)
	primaryClient.login(ctx)
	if itemPassword(t, primaryClient.getItem(ctx, itemID)) != initialItemPassword {
		t.Fatal("primary target drill did not restore the source item")
	}
	harness.stopDrill()

	harness.startDrill("multi-secondary")
	secondaryClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-multitarget-secondary-restore"), harness.publicTLS, harness.tlsCAFile, email)
	secondaryClient.login(ctx)
	if itemPassword(t, secondaryClient.getItem(ctx, itemID)) != initialItemPassword {
		t.Fatal("secondary target drill did not restore the source item")
	}
	harness.stopDrill()
}

const rejectingS3Endpoint = `from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

class Handler(BaseHTTPRequestHandler):
    def reject(self):
        self.send_response(403)
        self.send_header("Content-Length", "0")
        self.end_headers()

    do_DELETE = reject
    do_GET = reject
    do_HEAD = reject
    do_POST = reject
    do_PUT = reject

    def log_message(self, format, *arguments):
        pass

ThreadingHTTPServer(("0.0.0.0", 5000), Handler).serve_forever()
`

const verifyRejectingS3Endpoint = `import urllib.error, urllib.request
try:
    urllib.request.urlopen("http://moto-secondary:5000/", timeout=2)
except urllib.error.HTTPError as error:
    raise SystemExit(0 if error.code == 403 else 1)
raise SystemExit(1)
`

func rejectSecondaryEndpoint(t *testing.T, harness *e2eHarness) func() {
	t.Helper()
	const service = "moto-secondary"
	harness.compose("stop", service)
	container := strings.TrimSpace(harness.compose("ps", "--all", "--quiet", service))
	if !lowerHex64Pattern.MatchString(container) {
		t.Fatalf("invalid stopped secondary container ID %q", container)
	}
	network := harness.project + "_backup-outbound"
	if output, err := harness.docker("network", "disconnect", network, container); err != nil {
		t.Fatalf("disconnect stopped secondary endpoint: %v: %s", err, harnessDiagnostics(output))
	}

	rejector := harness.project + "-secondary-rejector"
	output, err := harness.docker(
		"run", "--detach", "--pull", "never", "--name", rejector,
		"--label", "vaultwarden-less.e2e.project="+harness.project,
		"--user", "1000:1000", "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev",
		"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--network", network, "--network-alias", service,
		"--entrypoint", "/bin/python3", "vaultwarden-less-moto:local", "-c", rejectingS3Endpoint,
	)
	if err != nil || !lowerHex64Pattern.MatchString(strings.TrimSpace(output)) {
		t.Fatalf("start rejecting secondary endpoint: %v: %s", err, harnessDiagnostics(output))
	}
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(harness.ctx), time.Minute)
		defer cancel()
		_, _ = harness.dockerContext(cleanupContext, "rm", "--force", rejector)
	})

	deadline := time.Now().Add(30 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		_, probeErr := harness.docker(
			"run", "--rm", "--pull", "never", "--user", "1000:1000", "--read-only",
			"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL", "--network", network,
			"--entrypoint", "/bin/python3", "vaultwarden-less-moto:local", "-c", verifyRejectingS3Endpoint,
		)
		if probeErr == nil {
			ready = true
			break
		}
		select {
		case <-harness.ctx.Done():
			t.Fatal(harness.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatal("rejecting secondary endpoint did not become reachable")
	}

	return func() {
		t.Helper()
		if restored {
			t.Fatal("secondary endpoint was restored more than once")
		}
		if output, err := harness.docker("rm", "--force", rejector); err != nil {
			t.Fatalf("remove rejecting secondary endpoint: %v: %s", err, harnessDiagnostics(output))
		}
		if output, err := harness.docker("network", "connect", "--alias", service, network, container); err != nil {
			t.Fatalf("reconnect secondary endpoint: %v: %s", err, harnessDiagnostics(output))
		}
		harness.compose("start", service)
		restored = true
	}
}

func waitMultiTargetStatus(
	t *testing.T,
	harness *e2eHarness,
	timeout time.Duration,
	accept func(productworker.Status) bool,
) productworker.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := productworker.Status{}
	for time.Now().Before(deadline) {
		status := harness.status()
		last = status
		if accept(status) {
			return status
		}
		select {
		case <-harness.ctx.Done():
			t.Fatal(harness.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	t.Fatalf("worker did not reach the required multi-target state; last status: %#v", last)
	return productworker.Status{}
}

func exactTwoTargets(t *testing.T, status productworker.Status) (productworker.TargetStatus, productworker.TargetStatus) {
	t.Helper()
	if len(status.Targets) != 2 || status.Targets[0].Name != "primary" || status.Targets[1].Name != "secondary" {
		t.Fatalf("worker target order = %#v", status.Targets)
	}
	return status.Targets[0], status.Targets[1]
}

func waitTargetSnapshotCount(t *testing.T, harness *e2eHarness, target string, count int, timeout time.Duration) snapshotList {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := harness.targetSnapshots(target)
		if err == nil && len(result.Snapshots) == count {
			return result
		}
		if err == nil && len(result.Snapshots) > count {
			t.Fatalf("%s snapshot count jumped to %d, want %d", target, len(result.Snapshots), count)
		}
		select {
		case <-harness.ctx.Done():
			t.Fatal(harness.ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	t.Fatalf("%s snapshot count did not become %d", target, count)
	return snapshotList{}
}

func assertRestoreMatchesSnapshot(t *testing.T, restored restoreOutput, snapshot struct {
	ID            string `json:"id"`
	Time          string `json:"time"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
}) {
	t.Helper()
	if restored.SnapshotID != snapshot.ID || restored.ProjectCommit != snapshot.ProjectCommit ||
		restored.ContentID != snapshot.ContentID || restored.ArchiveID != snapshot.ArchiveID {
		t.Fatalf("restore identity = %#v, want snapshot %#v", restored, snapshot)
	}
}
