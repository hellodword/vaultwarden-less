package e2e_test

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

var runMultiTargetFastE2E = flag.Bool("run-multitarget-fast-e2e", false, "run the fast real multi-target catch-up lifecycle")

type multiTargetFixture struct {
	t                 *testing.T
	ctx               context.Context
	harness           *e2eHarness
	email             string
	itemID            string
	client            *bitwardenCLI
	restoreSecondary  func()
	contentID         string
	secondaryStatus   productworker.TargetStatus
	primarySnapshotID string
}

func TestMultiTargetFastCatchUp(t *testing.T) {
	if !*runMultiTargetFastE2E {
		t.Skip("pass -run-multitarget-fast-e2e to run the fast real multi-target lifecycle")
	}
	fixture := newMultiTargetFixture(t, 25*time.Minute)
	primarySnapshots, secondarySnapshots := fixture.recoverSecondary()
	fixture.verifyBothRestores(primarySnapshots, secondarySnapshots)
}

func newMultiTargetFixture(t *testing.T, timeout time.Duration) *multiTargetFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	harness := newE2EHarness(t, ctx, "primary", "secondary")
	harness.startMotoAndRepository()
	harness.initializeDeployment()
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
	if primaryStatus.SnapshotID == nil {
		t.Fatalf("partial primary snapshot identity is missing: %#v", primaryStatus)
	}
	return &multiTargetFixture{
		t: t, ctx: ctx, harness: harness, email: email, itemID: itemID, client: client,
		restoreSecondary: restoreSecondary, contentID: contentID,
		secondaryStatus:   secondaryStatus,
		primarySnapshotID: *primaryStatus.SnapshotID,
	}
}

func (fixture *multiTargetFixture) recoverSecondary() (snapshotList, snapshotList) {
	fixture.t.Helper()
	// Restore only the failed endpoint and catch it up to the already-known
	// logical content without requiring another Vaultwarden mutation.
	fixture.restoreSecondary()
	fixture.harness.createMotoBucket("secondary")
	fixture.harness.initRepository("secondary")
	fixture.harness.requestBackup()
	recovered := waitMultiTargetStatus(fixture.t, fixture.harness, 8*time.Minute, func(status productworker.Status) bool {
		primary, secondary := exactTwoTargets(fixture.t, status)
		return status.State == "idle" && status.LastCheck != nil && status.LastCheck.Result == "created" &&
			primary.State == "healthy" && secondary.State == "healthy" &&
			primary.ContentID != nil && secondary.ContentID != nil &&
			*primary.ContentID == fixture.contentID && *secondary.ContentID == fixture.contentID &&
			secondary.SnapshotID != nil
	})
	recoveredPrimary, recoveredSecondary := exactTwoTargets(fixture.t, recovered)
	if recoveredPrimary.SnapshotID == nil || *recoveredPrimary.SnapshotID != fixture.primarySnapshotID {
		fixture.t.Fatalf("catch-up replaced the primary recovery point: %#v", recoveredPrimary)
	}
	fixture.harness.waitNotificationContains(30*time.Second, "synced=[primary]", "created=[secondary]", "failed=[]")

	primarySnapshots := fixture.harness.waitSnapshotCount(1, 2*time.Minute)
	secondarySnapshots := waitTargetSnapshotCount(fixture.t, fixture.harness, "secondary", 1, 2*time.Minute)
	primarySnapshot := primarySnapshots.Snapshots[0]
	secondarySnapshot := secondarySnapshots.Snapshots[0]
	if primarySnapshot.ContentID != fixture.contentID || secondarySnapshot.ContentID != fixture.contentID ||
		primarySnapshot.ID != *recoveredPrimary.SnapshotID || secondarySnapshot.ID != *recoveredSecondary.SnapshotID {
		fixture.t.Fatalf("repositories did not converge to one logical content: primary=%#v secondary=%#v", primarySnapshot, secondarySnapshot)
	}
	for _, target := range []string{"primary", "secondary"} {
		fixture.harness.repositoryCheckTarget(target)
	}
	return primarySnapshots, secondarySnapshots
}

func (fixture *multiTargetFixture) verifyBothRestores(primarySnapshots, secondarySnapshots snapshotList) {
	fixture.t.Helper()
	primarySnapshot := primarySnapshots.Snapshots[0]
	secondarySnapshot := secondarySnapshots.Snapshots[0]
	primaryRestore := fixture.harness.restoreTarget("primary", primarySnapshot.ID, "multi-primary")
	secondaryRestore := fixture.harness.restoreTarget("secondary", secondarySnapshot.ID, "multi-secondary")
	assertRestoreMatchesSnapshot(fixture.t, primaryRestore, primarySnapshot)
	assertRestoreMatchesSnapshot(fixture.t, secondaryRestore, secondarySnapshot)

	fixture.harness.stopProduction()
	fixture.harness.startDrill("multi-primary")
	fixture.client.expectSyncFailure(fixture.ctx)
	primaryClient := newBitwardenCLI(
		fixture.t,
		filepath.Join(fixture.harness.localRoot, "bw-multitarget-primary-restore"),
		fixture.harness.publicTLS,
		fixture.harness.tlsCAFile,
		fixture.email,
	)
	primaryClient.login(fixture.ctx)
	if itemPassword(fixture.t, primaryClient.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("primary target drill did not restore the source item")
	}
	fixture.harness.stopDrill()

	fixture.harness.startDrill("multi-secondary")
	secondaryClient := newBitwardenCLI(
		fixture.t,
		filepath.Join(fixture.harness.localRoot, "bw-multitarget-secondary-restore"),
		fixture.harness.publicTLS,
		fixture.harness.tlsCAFile,
		fixture.email,
	)
	secondaryClient.login(fixture.ctx)
	if itemPassword(fixture.t, secondaryClient.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("secondary target drill did not restore the source item")
	}
	fixture.harness.stopDrill()
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
		if _, err := harness.docker("container", "inspect", rejector); err == nil {
			t.Fatal("rejecting secondary endpoint survived exact cleanup")
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
