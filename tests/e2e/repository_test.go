package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

func (h *e2eHarness) startMotoAndRepository() {
	services := make([]string, 0, len(h.targets)+2)
	services = append(services, "up", "--detach")
	for _, target := range h.targets {
		services = append(services, "moto-"+target)
	}
	h.compose(services...)
	for _, target := range h.targets {
		h.createMotoBucket(target)
		h.initRepository(target)
	}
}

func (h *e2eHarness) initRepository(target string) {
	h.t.Helper()
	h.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "init", "--target", target, "--config", "/config/backup.yaml",
	)
}

func (h *e2eHarness) createMotoBucket(target string) {
	h.t.Helper()
	service := "moto-" + target
	bucket := "vaultwarden-less-e2e-" + target
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.tryCompose(
			h.ctx,
			"exec", "--no-TTY", service, "python", "-c",
			"import boto3,sys; boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1').create_bucket(Bucket=sys.argv[1])",
			bucket,
		)
		if err == nil {
			return
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	h.t.Fatalf("Moto target %s did not accept bucket creation", target)
}

type snapshotList struct {
	Target    string `json:"target"`
	Limit     int    `json:"limit"`
	Truncated bool   `json:"truncated"`
	Snapshots []struct {
		ID            string `json:"id"`
		Time          string `json:"time"`
		ProjectCommit string `json:"project_commit"`
		ContentID     string `json:"content_id"`
		ArchiveID     string `json:"archive_id"`
	} `json:"snapshots"`
}

func (h *e2eHarness) repositoryCheck() {
	h.repositoryCheckTarget("primary")
}

func (h *e2eHarness) repositoryCheckTarget(target string) {
	output := h.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "check", "--target", target,
		"--config", "/config/backup.yaml",
	)
	var result struct {
		Operation string `json:"operation"`
		Target    string `json:"target"`
		Success   bool   `json:"success"`
	}
	if decodeExactJSON(output, &result) != nil || result.Operation != "check" ||
		result.Target != target || !result.Success {
		h.t.Fatalf("invalid repository check result: %s", harnessDiagnostics(output))
	}
}

func (h *e2eHarness) snapshots() (snapshotList, error) {
	return h.targetSnapshots("primary")
}

func (h *e2eHarness) targetSnapshots(target string) (snapshotList, error) {
	output, err := h.tryCompose(
		h.ctx,
		"exec",
		"--no-TTY",
		"backup",
		"/bin/vaultwarden-less-backup",
		"repo",
		"snapshots",
		"--target",
		target,
		"--limit",
		"100",
		"--config",
		"/config/backup.yaml",
	)
	if err != nil {
		return snapshotList{}, err
	}
	var result snapshotList
	if err := decodeExactJSON(output, &result); err != nil ||
		result.Target != target || result.Limit != 100 ||
		result.Snapshots == nil || result.Truncated {
		return snapshotList{}, fmt.Errorf("invalid snapshots response")
	}
	seen := make(map[string]bool, len(result.Snapshots))
	var previous time.Time
	for index, item := range result.Snapshots {
		parsed, err := time.Parse(time.RFC3339, item.Time)
		if err != nil || !lowerHex64Pattern.MatchString(item.ID) ||
			!lowerHex40Pattern.MatchString(item.ProjectCommit) ||
			!contentIDPattern.MatchString(item.ContentID) ||
			!contentIDPattern.MatchString(item.ArchiveID) || seen[item.ID] ||
			(index > 0 && parsed.After(previous)) {
			return snapshotList{}, fmt.Errorf("invalid snapshot identity or ordering")
		}
		seen[item.ID] = true
		previous = parsed
	}
	return result, nil
}

func (h *e2eHarness) status() productworker.Status {
	h.t.Helper()
	output := h.compose("exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "status")
	var status productworker.Status
	if err := decodeExactJSON(output, &status); err != nil || status.Targets == nil {
		h.t.Fatalf("invalid worker status: %v: %s", err, harnessDiagnostics(output))
	}
	return status
}

func (h *e2eHarness) requestBackup() {
	h.t.Helper()
	output := h.compose("exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "request-backup")
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if err := decodeExactJSON(output, &result); err != nil || !result.Accepted {
		h.t.Fatalf("invalid backup request result: %v: %s", err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) waitSnapshotCount(count int, timeout time.Duration) snapshotList {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		result, err := h.snapshots()
		if err == nil && len(result.Snapshots) == count {
			return result
		}
		if err == nil && len(result.Snapshots) > count {
			h.t.Fatalf("snapshot count jumped to %d, want %d", len(result.Snapshots), count)
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
	h.t.Fatalf("snapshot count did not become %d", count)
	return snapshotList{}
}

type restoreOutput struct {
	Target        string `json:"target"`
	SnapshotID    string `json:"snapshot_id"`
	ProjectCommit string `json:"project_commit"`
	ContentID     string `json:"content_id"`
	ArchiveID     string `json:"archive_id"`
	Output        string `json:"output"`
}

func (h *e2eHarness) restore(snapshotID, outputName string) restoreOutput {
	return h.restoreTarget("primary", snapshotID, outputName)
}

func (h *e2eHarness) restoreTarget(target, snapshotID, outputName string) restoreOutput {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`).MatchString(outputName) {
		h.t.Fatalf("unsafe restore output name %q", outputName)
	}
	arguments := []string{
		"--profile",
		"restore",
		"run",
		"--rm",
		"--no-deps",
		"restore",
		"restore",
		"--target",
		target,
		"--output",
		outputName,
		"--config",
		"/config/backup.yaml",
	}
	if snapshotID != "" {
		arguments = append(arguments, "--snapshot", snapshotID)
	}
	output := h.compose(arguments...)
	var result restoreOutput
	if err := decodeExactJSON(output, &result); err != nil ||
		result.Target != target || result.Output != "/restore/"+outputName ||
		!lowerHex64Pattern.MatchString(result.SnapshotID) ||
		!lowerHex40Pattern.MatchString(result.ProjectCommit) ||
		!contentIDPattern.MatchString(result.ContentID) ||
		!contentIDPattern.MatchString(result.ArchiveID) {
		h.t.Fatalf("invalid restore output: %v: %s", err, harnessDiagnostics(output))
	}
	return result
}

func decodeExactJSON(value string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON documents")
		}
		return err
	}
	return nil
}

func TestDecodeExactJSONRejectsUnknownAndAdditionalDocuments(t *testing.T) {
	t.Parallel()
	type result struct {
		Value string `json:"value"`
	}
	var decoded result
	if err := decodeExactJSON(`{"value":"valid"}`, &decoded); err != nil || decoded.Value != "valid" {
		t.Fatalf("valid result = %#v/%v", decoded, err)
	}
	for name, value := range map[string]string{
		"unknown":    `{"value":"valid","extra":true}`,
		"additional": `{"value":"valid"} {"value":"valid"}`,
	} {
		withResult := result{}
		if err := decodeExactJSON(value, &withResult); err == nil {
			t.Fatalf("%s JSON was accepted", name)
		}
	}
}
