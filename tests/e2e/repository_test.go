package e2e_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
)

type nativeSnapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Parent   string    `json:"parent"`
	Summary  struct {
		DataAdded int64 `json:"data_added"`
	} `json:"summary"`
}

func (h *e2eHarness) nativeRestic(target string, mounts []string, arguments ...string) string {
	h.t.Helper()
	command := []string{"run", "--rm", "--pull", "never", "--network", h.project + "_default", "--user", h.identity(), "--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--env-file", filepath.Join(h.deploymentRoot, "credentials/maintenance", target+".env"),
		"--env", "RESTIC_CACHE_DIR=/tmp/restic", "--mount", "type=bind,source=" + filepath.Join(h.deploymentRoot, "credentials/restic") + ",target=/run/credentials/restic,readonly"}
	for _, mount := range mounts {
		command = append(command, "--mount", mount)
	}
	command = append(command, "--entrypoint", "/bin/restic", h.services["backup"].Image, "-o", "s3.bucket-lookup=path", "-o", "s3.region=us-east-1")
	return h.mustDocker(append(command, arguments...)...)
}
func (h *e2eHarness) snapshots(target string) []nativeSnapshot {
	h.t.Helper()
	output := h.nativeRestic(target, nil, "snapshots", "--host", "vaultwarden-less", "--json")
	var result []nativeSnapshot
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		h.t.Fatal("invalid native snapshots response")
	}
	for _, s := range result {
		if !lowerHex64Pattern.MatchString(s.ID) || s.Hostname != "vaultwarden-less" || (!slices.Contains(s.Tags, "complete") && !slices.Contains(s.Tags, "incomplete")) {
			h.t.Fatal("native snapshot identity or tag missing")
		}
	}
	slices.SortFunc(result, func(a, b nativeSnapshot) int { return a.Time.Compare(b.Time) })
	return result
}
func (h *e2eHarness) latest(target string) nativeSnapshot {
	h.t.Helper()
	all := h.snapshots(target)
	if len(all) == 0 {
		h.t.Fatal("no native snapshots")
	}
	return all[len(all)-1]
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
func settled(s productworker.Status) bool {
	if s.Checking || s.LastCheck.IsZero() {
		return false
	}
	for _, t := range s.Targets {
		if t.Uploading || t.State == "initializing" || (t.Waiting && t.Error == "") {
			return false
		}
	}
	return true
}
func (h *e2eHarness) check(full bool) productworker.Status {
	h.t.Helper()
	before := h.status().LastCheck
	args := []string{"exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "check"}
	if full {
		args = append(args, "--full")
	}
	output := h.compose(args...)
	var accepted map[string]any
	if json.Unmarshal([]byte(output), &accepted) != nil || accepted["accepted"] != true {
		h.t.Fatal("manual check not accepted")
	}
	return h.waitStatus(3*time.Minute, func(s productworker.Status) bool { return s.LastCheck.After(before) && settled(s) })
}
func (h *e2eHarness) waitNewRequestCheck(after time.Time) productworker.Status {
	return h.waitStatus(3*time.Minute, func(s productworker.Status) bool { return s.LastCheck.After(after) && settled(s) })
}
func (h *e2eHarness) assertUnchangedIDs(before productworker.Status, after productworker.Status) {
	h.t.Helper()
	if len(before.Targets) != len(after.Targets) {
		h.t.Fatal("target count changed")
	}
	for i := range before.Targets {
		if before.Targets[i].SuccessID != after.Targets[i].SuccessID {
			h.t.Fatalf("unchanged content created a snapshot for %s", after.Targets[i].Name)
		}
	}
}
func (h *e2eHarness) assertNewIDs(before productworker.Status, after productworker.Status) {
	h.t.Helper()
	for i := range before.Targets {
		if before.Targets[i].SuccessID == after.Targets[i].SuccessID || after.Targets[i].SuccessID == "" {
			h.t.Fatalf("changed content was not saved to %s", after.Targets[i].Name)
		}
	}
}
func (h *e2eHarness) restore(target, id, name string) string {
	h.t.Helper()
	path := filepath.Join(h.deploymentRoot, "restore", name)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		h.t.Fatal("restore target already exists")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		h.t.Fatal(err)
	}
	h.nativeRestic(target, []string{"type=bind,source=" + path + ",target=/restore"}, "restore", id, "--verify", "--target", "/restore")
	if _, err := os.Stat(filepath.Join(path, "db.sqlite3")); err != nil {
		h.t.Fatal("native restore did not produce a root database")
	}
	if result := h.sqliteReadOnly(path, "PRAGMA integrity_check;"); strings.TrimSpace(result) != "ok" {
		h.t.Fatal("restored SQLite integrity failed")
	}
	return path
}
func (h *e2eHarness) sqlite(root, sql string) string {
	h.t.Helper()
	return h.mustDocker("run", "--rm", "--pull", "never", "--user", h.identity(), "--read-only", "--mount", "type=bind,source="+root+",target=/data", "--entrypoint", "/bin/sqlite3", h.services["backup"].Image, "-batch", "-bail", "/data/db.sqlite3", sql)
}
func (h *e2eHarness) sqliteReadOnly(root, sql string) string {
	h.t.Helper()
	return h.mustDocker("run", "--rm", "--pull", "never", "--user", h.identity(), "--read-only", "--mount", "type=bind,source="+root+",target=/restore,readonly", "--entrypoint", "/bin/sqlite3", h.services["backup"].Image, "-batch", "-bail", "-readonly", "file:/restore/db.sqlite3?immutable=1", sql)
}
func (h *e2eHarness) startRestored(root, name string) {
	h.t.Helper()
	port := unusedLoopbackPort(h.t)
	container := h.project + "-" + name
	command := []string{"run", "--detach", "--pull", "never", "--name", container, "--label", "vaultwarden-less.e2e.project=" + h.project,
		"--user", h.identity(), "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777", "--mount", "type=bind,source=" + root + ",target=/data",
		"--publish", fmt.Sprintf("127.0.0.1:%d:80", port)}
	// Use the rendered product configuration before any test-only overrides.
	// Sends stay disabled here: existing Sends must still be readable on restore.
	for key, value := range h.services["vaultwarden"].Environment {
		command = append(command, "--env", key+"="+value)
	}
	command = append(command, h.services["vaultwarden"].Image)
	h.mustDocker(command...)
	h.t.Cleanup(func() { h.cleanupContainer(container) })
	h.setBackend(loopbackEndpoint(port))
	h.waitHTTP(h.publicTLS+"/alive", 200, 3*time.Minute)
}
