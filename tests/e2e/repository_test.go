package e2e_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

type recoverySnapshot struct {
	ID            string `json:"id"`
	Time          string `json:"time"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
}

type snapshotList struct {
	Target    string             `json:"target"`
	Limit     int                `json:"limit"`
	Truncated bool               `json:"truncated"`
	Snapshots []recoverySnapshot `json:"snapshots"`
}

type restoreOutput struct {
	Target        string `json:"target"`
	SnapshotID    string `json:"snapshot_id"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
	Output        string `json:"output"`
}

func (h *e2eHarness) startRepositories() {
	h.compose("--profile", "e2e-repositories", "up", "--detach", "moto-primary", "moto-b2")
	for _, target := range []string{"primary", "b2"} {
		h.createMotoBucket(target)
		h.initRepository(target)
	}
}

func (h *e2eHarness) createMotoBucket(target string) {
	h.t.Helper()
	service := "moto-" + target
	bucket := "vaultwarden-less-e2e-" + target
	const program = `import boto3,sys
boto3.client(
    "s3",
    endpoint_url="http://127.0.0.1:5000",
    aws_access_key_id="e2e",
    aws_secret_access_key="e2e",
    region_name="us-east-1",
).create_bucket(Bucket=sys.argv[1])
`
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		output, err := h.tryCompose(
			h.ctx,
			"--profile", "e2e-repositories", "exec", "--no-TTY", service,
			"/bin/python3", "-c", program, bucket,
		)
		last = output
		if err == nil {
			return
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	h.t.Fatalf("Moto target %s did not accept bucket creation: %s", target, harnessDiagnostics(last))
}

func (h *e2eHarness) initRepository(target string) {
	h.t.Helper()
	output := h.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "init", "--target", target,
	)
	var result struct {
		Operation string `json:"operation"`
		Target    string `json:"target"`
		Success   bool   `json:"success"`
	}
	if err := decodeExactJSON(output, &result); err != nil || result.Operation != "init" || result.Target != target || !result.Success {
		h.t.Fatalf("invalid repository init result for %s: %#v/%v: %s", target, result, err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) repositoryCheck(target string) {
	h.t.Helper()
	output := h.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "check", "--target", target,
	)
	var result struct {
		Operation string `json:"operation"`
		Target    string `json:"target"`
		Success   bool   `json:"success"`
	}
	if err := decodeExactJSON(output, &result); err != nil || result.Operation != "check" || result.Target != target || !result.Success {
		h.t.Fatalf("invalid repository check result for %s: %#v/%v: %s", target, result, err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) status() productworker.Status {
	h.t.Helper()
	output := h.compose("exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "status")
	var result productworker.Status
	if err := decodeExactJSON(output, &result); err != nil || result.Targets == nil {
		h.t.Fatalf("invalid worker status: %#v/%v: %s", result, err, harnessDiagnostics(output))
	}
	return result
}

func (h *e2eHarness) requestBackup() {
	h.t.Helper()
	output := h.compose("exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "request-backup")
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if err := decodeExactJSON(output, &result); err != nil || !result.Accepted {
		h.t.Fatalf("invalid backup request result: %#v/%v: %s", result, err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) targetSnapshots(target string) (snapshotList, error) {
	output, err := h.tryCompose(
		h.ctx,
		"exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup",
		"repo", "snapshots", "--target", target, "--limit", "100",
	)
	if err != nil {
		return snapshotList{}, fmt.Errorf("list %s snapshots: %w: %s", target, err, harnessDiagnostics(output))
	}
	var result snapshotList
	if err := decodeExactJSON(output, &result); err != nil || result.Target != target || result.Limit != 100 || result.Truncated || result.Snapshots == nil {
		return snapshotList{}, fmt.Errorf("invalid %s snapshot response", target)
	}
	seen := make(map[string]struct{}, len(result.Snapshots))
	var previous time.Time
	for index, snapshot := range result.Snapshots {
		when, parseErr := time.Parse(time.RFC3339Nano, snapshot.Time)
		_, duplicate := seen[snapshot.ID]
		if parseErr != nil || !lowerHex64Pattern.MatchString(snapshot.ID) ||
			!lowerHex40Pattern.MatchString(snapshot.ProjectCommit) ||
			!contentIDPattern.MatchString(snapshot.ContentID) ||
			!contentIDPattern.MatchString(snapshot.ArchiveID) || duplicate ||
			(index > 0 && when.After(previous)) {
			return snapshotList{}, fmt.Errorf("invalid %s snapshot identity or ordering", target)
		}
		seen[snapshot.ID] = struct{}{}
		previous = when
	}
	return result, nil
}

func (h *e2eHarness) waitTargetSnapshotCount(target string, count int, timeout time.Duration) snapshotList {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastError error
	for time.Now().Before(deadline) {
		result, err := h.targetSnapshots(target)
		lastError = err
		if err == nil && len(result.Snapshots) == count {
			return result
		}
		if err == nil && len(result.Snapshots) > count {
			h.t.Fatalf("%s snapshot count jumped to %d, want %d", target, len(result.Snapshots), count)
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	h.t.Fatalf("%s snapshot count did not become %d: %v", target, count, lastError)
	return snapshotList{}
}

func (h *e2eHarness) waitStatus(timeout time.Duration, accept func(productworker.Status) bool) productworker.Status {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	last := productworker.Status{}
	for time.Now().Before(deadline) {
		last = h.status()
		if accept(last) {
			return last
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	h.t.Fatalf("worker did not reach the required state; last status: %#v", last)
	return productworker.Status{}
}

func exactTwoTargets(t *testing.T, status productworker.Status) (productworker.TargetStatus, productworker.TargetStatus) {
	t.Helper()
	if len(status.Targets) != 2 || status.Targets[0].Name != "primary" || status.Targets[1].Name != "b2" {
		t.Fatalf("worker target order = %#v", status.Targets)
	}
	return status.Targets[0], status.Targets[1]
}

func (h *e2eHarness) restoreTarget(target, snapshotID, outputName string) restoreOutput {
	h.t.Helper()
	output, err := h.tryRestoreTarget(target, snapshotID, outputName)
	if err != nil {
		h.t.Fatalf("restore %s to %s: %v: %s", target, outputName, err, harnessDiagnostics(output))
	}
	var result restoreOutput
	if decodeErr := decodeExactJSON(output, &result); decodeErr != nil ||
		result.Target != target || result.Output != "/restore/"+outputName ||
		!lowerHex64Pattern.MatchString(result.SnapshotID) ||
		!lowerHex40Pattern.MatchString(result.ProjectCommit) ||
		!contentIDPattern.MatchString(result.ContentID) ||
		!contentIDPattern.MatchString(result.ArchiveID) {
		h.t.Fatalf("invalid restore output for %s: %#v/%v: %s", target, result, decodeErr, harnessDiagnostics(output))
	}
	return result
}

func (h *e2eHarness) tryRestoreTarget(target, snapshotID, outputName string) (string, error) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`).MatchString(outputName) {
		h.t.Fatalf("unsafe restore output name %q", outputName)
	}
	arguments := []string{
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"restore", "--target", target, "--output", outputName,
	}
	if snapshotID != "" {
		arguments = append(arguments, "--snapshot", snapshotID)
	}
	return h.tryCompose(h.ctx, arguments...)
}

func assertRestoreMatchesSnapshot(t *testing.T, restored restoreOutput, snapshot recoverySnapshot) {
	t.Helper()
	if restored.SnapshotID != snapshot.ID || restored.ProjectCommit != snapshot.ProjectCommit ||
		restored.ContentID != snapshot.ContentID || restored.ArchiveID != snapshot.ArchiveID {
		t.Fatalf("restore identity = %#v, want snapshot %#v", restored, snapshot)
	}
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

const verifyRejectingS3Endpoint = `import urllib.error,urllib.request
try:
    urllib.request.urlopen("http://moto-b2:5000/", timeout=2)
except urllib.error.HTTPError as error:
    raise SystemExit(0 if error.code == 403 else 1)
raise SystemExit(1)
`

func rejectB2Endpoint(t *testing.T, h *e2eHarness) func() {
	t.Helper()
	const service = "moto-b2"
	h.compose("--profile", "e2e-repositories", "stop", service)
	container := h.containerID(service)
	network := h.project + "_default"
	if output, err := h.docker("network", "disconnect", network, container); err != nil {
		t.Fatalf("disconnect stopped b2 endpoint: %v: %s", err, harnessDiagnostics(output))
	}

	rejector := h.project + "-b2-rejector"
	output, err := h.docker(
		"run", "--detach", "--pull", "never", "--name", rejector,
		"--label", "vaultwarden-less.e2e.project="+h.project,
		"--user", fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
		"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--network", network, "--network-alias", service,
		"--entrypoint", "/bin/python3", "vaultwarden-less-moto:local", "-c", rejectingS3Endpoint,
	)
	if err != nil || !lowerHex64Pattern.MatchString(strings.TrimSpace(output)) {
		t.Fatalf("start rejecting b2 endpoint: %v: %s", err, harnessDiagnostics(output))
	}
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), time.Minute)
		defer cancel()
		_, _ = h.dockerContext(cleanupContext, "container", "rm", "--force", rejector)
	})

	deadline := time.Now().Add(30 * time.Second)
	for {
		_, probeErr := h.docker(
			"run", "--rm", "--pull", "never",
			"--user", fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
			"--read-only", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
			"--network", network, "--entrypoint", "/bin/python3",
			"vaultwarden-less-moto:local", "-c", verifyRejectingS3Endpoint,
		)
		if probeErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rejecting b2 endpoint did not become reachable")
		}
		select {
		case <-h.ctx.Done():
			t.Fatal(h.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}

	return func() {
		t.Helper()
		if restored {
			t.Fatal("b2 endpoint was restored more than once")
		}
		if output, err := h.docker("container", "rm", "--force", rejector); err != nil {
			t.Fatalf("remove rejecting b2 endpoint: %v: %s", err, harnessDiagnostics(output))
		}
		if output, err := h.docker("network", "connect", "--alias", service, network, container); err != nil {
			t.Fatalf("reconnect b2 endpoint: %v: %s", err, harnessDiagnostics(output))
		}
		h.compose("--profile", "e2e-repositories", "start", service)
		h.createMotoBucket("b2")
		h.initRepository("b2")
		restored = true
	}
}
