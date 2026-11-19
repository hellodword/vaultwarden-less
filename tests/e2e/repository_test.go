package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

type nativeSnapshot struct {
	ID    string
	Paths []string
}

func (h *e2eHarness) nativeRestic(target string, arguments ...string) string {
	h.t.Helper()
	command := []string{"run", "--rm", "--pull", "never", "--network", h.project + "_default", "--read-only",
		"--mount", "type=bind,source=" + filepath.Join(h.deploymentRoot, "maintenance-cache") + ",target=/cache", "--env", "TMPDIR=/cache", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--env-file", filepath.Join(h.deploymentRoot, "credentials/maintenance", target+".env"),
		"--env", "RESTIC_CACHE_DIR=/cache/restic"}
	command = append(command, "--entrypoint", "/bin/restic", h.services["backup"].Image, "-o", "s3.bucket-lookup=path", "-o", "s3.region=us-east-1")
	return h.mustDocker(append(command, arguments...)...)
}
func (h *e2eHarness) snapshots(target string) []nativeSnapshot {
	h.t.Helper()
	output := h.nativeRestic(target, "snapshots", "--json")
	var result []nativeSnapshot
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		h.t.Fatal("invalid native snapshots response")
	}
	for _, s := range result {
		if !lowerHex64Pattern.MatchString(s.ID) || !slices.Equal(s.Paths, []string{"/db.sqlite3"}) {
			h.t.Fatal("unexpected native snapshot")
		}
	}
	return result
}
func (h *e2eHarness) status() productworker.Status {
	h.t.Helper()
	output := h.compose("exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "status")
	var s productworker.Status
	if err := json.Unmarshal([]byte(output), &s); err != nil {
		h.t.Fatal("invalid status JSON")
	}
	writePrivateFile(h.t, filepath.Join(h.testRoot, "last-status.json"), output)
	return s
}
func (h *e2eHarness) waitStatus(timeout time.Duration, accept func(productworker.Status) bool) productworker.Status {
	h.t.Helper()
	var state productworker.Status
	h.wait(timeout, "backup status condition", func() bool { state = h.status(); return accept(state) })
	return state
}
func (h *e2eHarness) waitPass(since time.Time) productworker.Status {
	return h.waitStatus(3*time.Minute, func(s productworker.Status) bool {
		if s.Source != "ready" || !s.CheckedAt.After(since) || len(s.Targets) != 2 {
			return false
		}
		for _, t := range s.Targets {
			if t.State != "protected" || t.CheckedAt.Before(s.CheckedAt) {
				return false
			}
		}
		return true
	})
}
func (h *e2eHarness) restore(target, id, name string) string {
	h.t.Helper()
	path := filepath.Join(h.deploymentRoot, "restore", name)
	if err := os.Mkdir(path, 0700); err != nil {
		h.t.Fatal(err)
	}
	env, err := os.ReadFile(filepath.Join(h.deploymentRoot, "credentials/maintenance", target+".env"))
	if err != nil {
		h.t.Fatal(err)
	}
	writePrivateFile(h.t, filepath.Join(path, "primary.env"), string(env))
	// The recovery containers join the private network that hosts the test S3 endpoints.
	h.runExample(path, "recovery.md", "sqlite-restore", "替换为完整快照ID", id,
		"docker run ", "docker run --network "+h.project+"_default ",
		"--no-cache ", "--no-cache -o s3.bucket-lookup=path -o s3.region=us-east-1 ")
	return filepath.Join(path, "recovered")
}
func (h *e2eHarness) sqlite(root, sql string) string {
	h.t.Helper()
	return h.mustDocker("run", "--rm", "--pull", "never", "--read-only", "--mount", "type=bind,source="+root+",target=/data", "--entrypoint", "/bin/sqlite3", h.services["backup"].Image, "-batch", "-bail", "/data/db.sqlite3", sql)
}
func (h *e2eHarness) startRestored(root, name string) {
	h.t.Helper()
	port := unusedLoopbackPort(h.t)
	container := h.project + "-" + name
	command := []string{"run", "--detach", "--pull", "never", "--name", container, "--label", "vaultwarden-less.e2e.project=" + h.project,
		"--user", "1000:1000", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777", "--mount", "type=bind,source=" + root + ",target=/data",
		"--publish", fmt.Sprintf("127.0.0.1:%d:80", port)}
	// Use the rendered product configuration before any test-only overrides.
	for key, value := range h.services["vaultwarden"].Environment {
		command = append(command, "--env", key+"="+value)
	}
	command = append(command, h.services["vaultwarden"].Image)
	h.mustDocker(command...)
	h.t.Cleanup(func() { h.cleanupContainer(container) })
	h.setBackend(loopbackEndpoint(port))
	h.waitHTTP(h.publicTLS+"/alive", 200, 3*time.Minute)
}
