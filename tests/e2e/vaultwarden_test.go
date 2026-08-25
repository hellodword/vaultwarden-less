//go:build systemtest

package e2e_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/tests/e2e/register"
)

const (
	vaultwardenImage = "vaultwarden/server:1.37.2@sha256:5d326778c22f063d093d6b0c9c766a28249561632266776f2c93132ab0ad3a80"
	motoImage        = "ghcr.io/getmoto/motoserver:5.2.2@sha256:0349ef6ecb9cd041293026accda0ea71bfa09925dffae3d76d120d15ecc76971"
	bitwardenCLIURL  = "https://github.com/bitwarden/clients/releases/download/cli-v2026.8.0/bw-oss-linux-2026.8.0.zip"
	bitwardenCLISHA  = "d8bbc213d3dbdb701709386af6ea3bd763482f922ae0b538fdc3fdb88b1b1704"
	cliPasswordEnv   = "BW_TEST_PASSWORD"
)

var (
	productImage  = flag.String("product-image", "vaultwarden-less-e2e:dev", "product image exercised by the E2E")
	proxyHostPort = flag.Int("proxy-host-port", 0, "first of two fixed proxy host ports; zero selects a process-scoped pair")
	publishedHost = flag.String("published-host", "127.0.0.1", "host through which this test reaches Docker-published ports")
)

func TestVaultwardenBackupRestoreE2E(t *testing.T) {
	options := validateE2EOptions(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	h := newDockerE2E(t, ctx, options)
	if _, err := h.tryDocker("info"); err != nil {
		t.Fatalf("Docker daemon is required for the real E2E: %v", err)
	}

	appNetwork := h.network("app", true)
	eventsNetwork := h.network("events", true)
	proxyIngressNetwork := h.network("proxy-ingress", false)
	backupNetwork := h.network("backup-outbound", false)
	maintenanceNetwork := h.network("maintenance-outbound", false)
	secondAppNetwork := h.network("app-restore", true)

	dataVolume := h.volume("data")
	stateVolume := h.volume("state")
	stagingVolume := h.volume("staging")
	cacheVolume := h.volume("cache")
	restoreCacheVolume := h.volume("restore-cache")
	restoreVolume := h.volume("restore")
	secondDataVolume := h.volume("second-data")
	configVolume := h.volume("config")
	commonCredentialsVolume := h.volume("common-credentials")
	roleCredentialsVolume := h.volume("role-credentials")
	for _, volume := range []string{dataVolume, stateVolume, stagingVolume, cacheVolume, restoreCacheVolume, restoreVolume, secondDataVolume, configVolume, commonCredentialsVolume, roleCredentialsVolume} {
		h.prepareVolume(volume)
	}

	configPath := filepath.Join(h.root, "backup.yaml")
	if err := os.WriteFile(configPath, []byte("schema_version: 1\n"+
		"target_timeout: 30m\n"+
		"maintenance_timeout: 12h\n"+
		"targets:\n"+
		"  - name: primary\n"+
		"    repository: s3:http://moto:5000/vaultwarden-less-e2e/repository\n"+
		"    restic_options:\n"+
		"      s3.bucket-lookup: path\n"+
		"      s3.region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commonPath := filepath.Join(h.root, "restic-password")
	if err := os.WriteFile(commonPath, []byte("e2e-restic-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rolePath := filepath.Join(h.root, "credentials.yaml")
	if err := os.WriteFile(rolePath, []byte("schema_version: 1\n"+
		"targets:\n"+
		"  primary:\n"+
		"    AWS_ACCESS_KEY_ID: test\n"+
		"    AWS_SECRET_ACCESS_KEY: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.seedFile(configVolume, configPath, "backup.yaml")
	h.seedFile(commonCredentialsVolume, commonPath, "restic-password")
	h.seedFile(roleCredentialsVolume, rolePath, "credentials.yaml")

	moto := h.container("moto")
	h.run("run", "--detach", "--name", moto, "--network", backupNetwork, "--network-alias", "moto", motoImage, "-H", "0.0.0.0")
	h.run("network", "connect", "--alias", "moto", maintenanceNetwork, moto)
	h.waitForMotoBucket(ctx, moto)
	h.initRepository(configVolume, commonCredentialsVolume, roleCredentialsVolume, cacheVolume, backupNetwork)

	proxyPort, proxyHost := h.proxyBinding(0)
	firstHTTPURL := h.proxyURL(proxyPort, proxyHost)
	firstURL := newTLSForwarder(t, firstHTTPURL)
	vaultwarden := h.container("vaultwarden")
	h.startVaultwarden(vaultwarden, dataVolume, appNetwork, firstURL)
	h.waitForHealthy(vaultwarden)

	backup := h.container("backup")
	h.startBackup(backup, dataVolume, stateVolume, stagingVolume, cacheVolume, configVolume, commonCredentialsVolume, roleCredentialsVolume, eventsNetwork, backupNetwork)
	h.waitForBackup(backup)
	proxy := h.container("proxy")
	h.startProxy(proxy, proxyIngressNetwork, appNetwork, eventsNetwork, proxyPort)
	h.waitForHTTP(ctx, firstHTTPURL+"/.vaultwarden-less/healthz", http.StatusNoContent)

	email := fmt.Sprintf("vaultwarden-less-e2e-%d@example.com", time.Now().UnixNano())
	password := "Correct Horse Battery Staple 2026!"
	registerAccount(ctx, t, firstHTTPURL, email, password)
	cliPath := downloadBitwardenCLI(ctx, t, filepath.Join(h.root, "cli"))
	cliOne := newBitwardenCLI(t, cliPath, filepath.Join(h.root, "cli-one"), firstURL, email, password)
	cliOne.login(ctx)

	itemName := "Vaultwarden-less E2E Login"
	oldPassword := "old-e2e-item-password"
	newPassword := "new-e2e-item-password"
	itemID := cliOne.createItem(ctx, itemName, oldPassword)
	attachmentData := []byte("vaultwarden-less attachment\nwith a stable payload\n")
	attachmentPath := filepath.Join(h.root, "attachment.txt")
	if err := os.WriteFile(attachmentPath, attachmentData, 0o600); err != nil {
		t.Fatal(err)
	}
	attachmentID := cliOne.createAttachment(ctx, itemID, attachmentPath)

	firstContentID := h.waitForCompleteContent(ctx, backup, "")
	updatedItem := cliOne.getItem(ctx, itemID)
	setItemPassword(t, updatedItem, oldPassword)
	if got := stringValue(updatedItem["name"]); got != itemName {
		t.Fatalf("CLI returned item name %q, want %q", got, itemName)
	}
	setItemPassword(t, updatedItem, newPassword)
	cliOne.editItem(ctx, itemID, updatedItem)
	secondContentID := h.waitForCompleteContent(ctx, backup, firstContentID)
	if secondContentID == firstContentID {
		t.Fatalf("item mutation did not produce a new complete content ID: %s", firstContentID)
	}

	restoreOutput := h.restore(ctx, restoreVolume, restoreCacheVolume, cacheVolume, configVolume, commonCredentialsVolume, roleCredentialsVolume, maintenanceNetwork)
	if restoreOutput.PublishState != "durable" || restoreOutput.Manifest.ContentID != secondContentID {
		t.Fatalf("restore result = %#v, want durable content %s", restoreOutput, secondContentID)
	}
	h.copyRestoredData(restoreVolume, secondDataVolume)

	secondProxyPort, secondProxyHost := h.proxyBinding(1)
	secondHTTPURL := h.proxyURL(secondProxyPort, secondProxyHost)
	secondURL := newTLSForwarder(t, secondHTTPURL)
	secondVaultwarden := h.container("vaultwarden-restore")
	h.startVaultwarden(secondVaultwarden, secondDataVolume, secondAppNetwork, secondURL)
	h.waitForHealthy(secondVaultwarden)
	secondProxy := h.container("proxy-restore")
	h.startProxy(secondProxy, proxyIngressNetwork, secondAppNetwork, "", secondProxyPort)
	h.waitForHTTP(ctx, secondHTTPURL+"/.vaultwarden-less/healthz", http.StatusNoContent)

	cliTwo := newBitwardenCLI(t, cliPath, filepath.Join(h.root, "cli-two"), secondURL, email, password)
	cliTwo.login(ctx)
	restoredItem := cliTwo.getItem(ctx, itemID)
	if stringValue(restoredItem["name"]) != itemName {
		t.Fatalf("restored item name = %q", stringValue(restoredItem["name"]))
	}
	login, ok := restoredItem["login"].(map[string]any)
	if !ok || stringValue(login["password"]) != newPassword {
		t.Fatalf("restored item password did not survive backup/restore: %#v", restoredItem["login"])
	}
	restoredAttachment := filepath.Join(h.root, "restored-attachment.txt")
	cliTwo.getAttachment(ctx, attachmentID, itemID, restoredAttachment)
	gotAttachment, err := os.ReadFile(restoredAttachment)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotAttachment, attachmentData) {
		t.Fatalf("restored attachment differs: got SHA-256 %s", sha256Hex(gotAttachment))
	}
}

type dockerE2E struct {
	t             *testing.T
	ctx           context.Context
	root          string
	image         string
	publishedHost string
	proxyHostPort int
	prefix        string
	helper        string
	containers    []string
	volumes       []string
	networks      []string
}

type e2eOptions struct {
	image         string
	publishedHost string
	proxyHostPort int
}

func validateE2EOptions(t *testing.T) e2eOptions {
	t.Helper()
	image := strings.TrimSpace(*productImage)
	if image == "" || image != *productImage {
		t.Fatalf("invalid -product-image %q", *productImage)
	}
	host := strings.TrimSpace(*publishedHost)
	if host == "" || host != *publishedHost || strings.ContainsAny(host, "/?#@") {
		t.Fatalf("invalid -published-host %q", *publishedHost)
	}
	port := *proxyHostPort
	if port != 0 && (port < 1024 || port > 65534) {
		t.Fatalf("invalid -proxy-host-port %d", port)
	}
	return e2eOptions{image: image, publishedHost: host, proxyHostPort: port}
}

func newDockerE2E(t *testing.T, ctx context.Context, options e2eOptions) *dockerE2E {
	h := &dockerE2E{
		t: t, ctx: ctx, root: t.TempDir(), image: options.image, publishedHost: options.publishedHost,
		proxyHostPort: options.proxyHostPort, prefix: fmt.Sprintf("vaultwarden-less-e2e-%d", os.Getpid()), helper: vaultwardenImage,
	}
	t.Cleanup(h.cleanup)
	return h
}

func (h *dockerE2E) cleanup() {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for index := len(h.containers) - 1; index >= 0; index-- {
		_, _ = h.tryDockerContext(cleanupContext, "rm", "--force", h.containers[index])
	}
	for index := len(h.volumes) - 1; index >= 0; index-- {
		_, _ = h.tryDockerContext(cleanupContext, "volume", "rm", h.volumes[index])
	}
	for index := len(h.networks) - 1; index >= 0; index-- {
		_, _ = h.tryDockerContext(cleanupContext, "network", "rm", h.networks[index])
	}
}

func (h *dockerE2E) container(label string) string {
	name := h.prefix + "-" + label
	h.containers = append(h.containers, name)
	return name
}

func (h *dockerE2E) volume(label string) string {
	name := h.prefix + "-" + label
	h.run("volume", "create", name)
	h.volumes = append(h.volumes, name)
	return name
}

func (h *dockerE2E) network(label string, internal bool) string {
	name := h.prefix + "-" + label
	args := []string{"network", "create"}
	if internal {
		args = append(args, "--internal")
	}
	if label == "proxy-ingress" {
		args = append(args, "--opt", "com.docker.network.bridge.enable_ip_masquerade=false")
	}
	args = append(args, name)
	h.run(args...)
	h.networks = append(h.networks, name)
	return name
}

func (h *dockerE2E) run(args ...string) string {
	output, err := h.tryDocker(args...)
	if err != nil {
		h.t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, truncate(output, 4000))
	}
	return output
}

func (h *dockerE2E) tryDocker(args ...string) (string, error) {
	return h.tryDockerContext(h.ctx, args...)
}

func (h *dockerE2E) tryDockerContext(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func (h *dockerE2E) prepareVolume(volume string) {
	h.run("run", "--rm", "--user", "0:0", "--entrypoint", "/bin/sh", "--mount", "type=volume,source="+volume+",target=/data,volume-nocopy", h.helper, "-c", "chown 65532:65532 /data && chmod 700 /data")
}

func (h *dockerE2E) seedFile(volume, localPath, name string) {
	seed := h.container("seed-" + strings.ReplaceAll(name, ".", "-"))
	command := "chown 65532:65532 /data && chmod 700 /data && chown 65532:65532 /data/" + name + " && chmod 600 /data/" + name
	h.run("create", "--name", seed, "--user", "0:0", "--entrypoint", "/bin/sh", "--mount", "type=volume,source="+volume+",target=/data,volume-nocopy", h.helper, "-c", command)
	h.run("cp", localPath, seed+":/data/"+name)
	h.run("start", "--attach", seed)
	h.run("rm", seed)
}

func (h *dockerE2E) waitForMotoBucket(ctx context.Context, moto string) {
	deadline := time.Now().Add(90 * time.Second)
	command := []string{"exec", moto, "python", "-c", "import boto3; boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1').create_bucket(Bucket='vaultwarden-less-e2e')"}
	for time.Now().Before(deadline) {
		if _, err := h.tryDocker(command...); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			h.t.Fatal(ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	h.t.Fatalf("Moto bucket did not become ready")
}

func (h *dockerE2E) startVaultwarden(name, dataVolume, network, domain string) {
	args := []string{
		"run", "--detach", "--name", name, "--user", "65532:65532", "--network", network, "--network-alias", "vaultwarden",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
		"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--health-cmd", "/healthcheck.sh", "--health-interval", "2s", "--health-timeout", "5s", "--health-retries", "60", "--health-start-period", "10s",
		"--env", "DOMAIN=" + domain, "--env", "ROCKET_ADDRESS=0.0.0.0", "--env", "ROCKET_PORT=80",
		"--env", "SIGNUPS_ALLOWED=true", "--env", "SIGNUPS_VERIFY=false", "--env", "WEB_VAULT_ENABLED=false", "--env", "DISABLE_ICON_DOWNLOAD=true", "--env", "LOG_LEVEL=warn",
		"--mount", "type=volume,source=" + dataVolume + ",target=/data,volume-nocopy", vaultwardenImage,
	}
	h.run(args...)
}

func (h *dockerE2E) waitForHealthy(name string) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		output, err := h.tryDocker("inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", name)
		if err == nil {
			fields := strings.Fields(output)
			if len(fields) >= 2 && fields[0] == "running" && fields[1] == "healthy" {
				return
			}
			if len(fields) > 0 && fields[0] == "exited" {
				logs, _ := h.tryDocker("logs", "--tail", "80", name)
				h.t.Fatalf("container %s exited: %s", name, truncate(logs, 6000))
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	logs, _ := h.tryDocker("logs", "--tail", "80", name)
	h.t.Fatalf("container %s did not become healthy: %s", name, truncate(logs, 6000))
}

func (h *dockerE2E) startBackup(name, data, state, staging, cache, config, common, role, events, outbound string) {
	args := []string{
		"run", "--detach", "--name", name, "--user", "65532:65532", "--network", events, "--network-alias", "backup",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--mount", "type=volume,source=" + data + ",target=/data,volume-nocopy,readonly",
		"--mount", "type=volume,source=" + state + ",target=/state,volume-nocopy",
		"--mount", "type=volume,source=" + staging + ",target=/staging,volume-nocopy",
		"--mount", "type=volume,source=" + cache + ",target=/cache,volume-nocopy",
		"--mount", "type=volume,source=" + config + ",target=/config,volume-nocopy,readonly",
		"--mount", "type=volume,source=" + common + ",target=/run/credentials/common,volume-nocopy,readonly",
		"--mount", "type=volume,source=" + role + ",target=/run/credentials/role,volume-nocopy,readonly",
		h.image, "serve", "--config", "/config/backup.yaml",
	}
	h.run(args...)
	h.run("network", "connect", "--alias", "backup", outbound, name)
}

func (h *dockerE2E) initRepository(config, common, role, cache, network string) {
	name := h.container("repo-init")
	h.run(
		"run", "--name", name, "--user", "65532:65532", "--network", network,
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
		"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--mount", "type=volume,source="+cache+",target=/cache,volume-nocopy",
		"--mount", "type=volume,source="+config+",target=/config,volume-nocopy,readonly",
		"--mount", "type=volume,source="+common+",target=/run/credentials/common,volume-nocopy,readonly",
		"--mount", "type=volume,source="+role+",target=/run/credentials/role,volume-nocopy,readonly",
		h.image, "repo", "init", "--target", "primary", "--config", "/config/backup.yaml",
	)
	h.run("rm", name)
}

func (h *dockerE2E) waitForBackup(name string) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.tryDocker("exec", name, "/usr/local/bin/vaultwarden-less-backup", "healthcheck"); err == nil {
			return
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	logs, _ := h.tryDocker("logs", "--tail", "100", name)
	h.t.Fatalf("backup did not become healthy: %s", truncate(logs, 6000))
}

func (h *dockerE2E) startProxy(name, ingressNetwork, appNetwork, eventsNetwork string, port int) {
	args := []string{
		"run", "--detach", "--name", name, "--user", "65532:65532", "--network", ingressNetwork, "--network-alias", "proxy",
		"--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--expose", "8080",
	}
	if port > 0 {
		args = append(args, "--publish", fmt.Sprintf("0.0.0.0:%d:8080", port))
	}
	args = append(args, "--entrypoint", "/usr/local/bin/vaultwarden-less-proxy", h.image)
	h.run(args...)
	h.run("network", "connect", "--alias", "proxy", appNetwork, name)
	if eventsNetwork != "" {
		h.run("network", "connect", "--alias", "proxy", eventsNetwork, name)
	}
}

func (h *dockerE2E) proxyBinding(index int) (int, string) {
	if h.proxyHostPort != 0 {
		return h.proxyHostPort + index, h.publishedHost
	}
	base := 40000 + os.Getpid()%10000
	return base + index, h.publishedHost
}

func (h *dockerE2E) proxyURL(port int, host string) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

func (h *dockerE2E) waitForHTTP(ctx context.Context, endpoint string, want int) {
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
				if response.StatusCode == want {
					return
				}
				lastErr = fmt.Errorf("status %d", response.StatusCode)
			} else {
				lastErr = requestErr
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			h.t.Fatal(ctx.Err())
		case <-time.After(1 * time.Second):
		}
	}
	h.t.Fatalf("endpoint %s did not return %d: %v", endpoint, want, lastErr)
}

type restoreResult struct {
	PublishState string `json:"publish_state"`
	Manifest     struct {
		ContentID string `json:"content_id"`
	} `json:"manifest"`
}

func (h *dockerE2E) restore(ctx context.Context, restoreVolume, restoreCache, cache, config, common, role, network string) restoreResult {
	name := h.container("restore")
	args := []string{
		"run", "--name", name, "--user", "65532:65532", "--network", network, "--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777",
		"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--mount", "type=volume,source=" + restoreVolume + ",target=/restore,volume-nocopy",
		"--mount", "type=volume,source=" + restoreCache + ",target=/cache,volume-nocopy",
		"--mount", "type=volume,source=" + config + ",target=/config,volume-nocopy,readonly",
		"--mount", "type=volume,source=" + common + ",target=/run/credentials/common,volume-nocopy,readonly",
		"--mount", "type=volume,source=" + role + ",target=/run/credentials/role,volume-nocopy,readonly",
		h.image, "restore", "--target", "primary", "--output", "/restore/prepared", "--config", "/config/backup.yaml",
	}
	output := h.run(args...)
	var result restoreResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		h.t.Fatalf("decode restore output: %v: %s", err, truncate(output, 4000))
	}
	return result
}

func (h *dockerE2E) copyRestoredData(restoreVolume, dataVolume string) {
	h.run("run", "--rm", "--user", "0:0", "--entrypoint", "/bin/sh", "--mount", "type=volume,source="+restoreVolume+",target=/source,volume-nocopy", "--mount", "type=volume,source="+dataVolume+",target=/data,volume-nocopy", h.helper, "-c", "cp -a /source/prepared/data/. /data/ && chown -R 65532:65532 /data && chmod 700 /data")
}

type snapshotReport struct {
	Snapshots []struct {
		ContentID string `json:"content_id"`
		Status    string `json:"status"`
	} `json:"snapshots"`
	Anomalies []struct {
		ErrorCode string `json:"error_code"`
	} `json:"anomalies"`
}

func (h *dockerE2E) waitForCompleteContent(ctx context.Context, backup, previous string) string {
	deadline := time.Now().Add(14 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		output, err := h.tryDocker("exec", backup, "/usr/local/bin/vaultwarden-less-backup", "repo", "snapshots", "--target", "primary", "--config", "/config/backup.yaml")
		if err == nil {
			var report snapshotReport
			if decodeErr := json.Unmarshal([]byte(output), &report); decodeErr == nil {
				if len(report.Anomalies) > 0 {
					h.t.Fatalf("snapshot anomalies during E2E: %#v", report.Anomalies)
				}
				for index := len(report.Snapshots) - 1; index >= 0; index-- {
					candidate := report.Snapshots[index]
					if candidate.Status == "complete" && candidate.ContentID != "" && candidate.ContentID != previous {
						return candidate.ContentID
					}
				}
				lastErr = errors.New("no new complete snapshot")
			} else {
				lastErr = decodeErr
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			h.t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	h.t.Fatalf("complete snapshot did not appear: %v", lastErr)
	return ""
}

func registerAccount(ctx context.Context, t *testing.T, server, email, password string) {
	post := func(path string, body []byte) (int, []byte) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return response.StatusCode, data
	}
	verification, err := json.Marshal(map[string]string{"email": email, "name": "Vaultwarden-less E2E"})
	if err != nil {
		t.Fatal(err)
	}
	status, body := post("/identity/accounts/register/send-verification-email", verification)
	if status != http.StatusOK {
		t.Fatalf("registration verification endpoint returned %d", status)
	}
	var token string
	if err := json.Unmarshal(body, &token); err != nil || token == "" {
		t.Fatalf("registration verification token was invalid: %v", err)
	}
	registration, err := register.BuildRegistration(email, password, "Vaultwarden-less E2E", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registration = registration.WithEmailVerificationToken(token)
	data, err := registration.JSON()
	if err != nil {
		t.Fatal(err)
	}
	status, body = post("/identity/accounts/register/finish", data)
	if status != http.StatusOK {
		t.Fatalf("registration finish returned %d: %s", status, truncate(string(body), 2000))
	}
}

type bitwardenCLI struct {
	t        *testing.T
	path     string
	appdata  string
	server   string
	email    string
	password string
	session  string
}

func newBitwardenCLI(t *testing.T, path, appdata, server, email, password string) *bitwardenCLI {
	return &bitwardenCLI{t: t, path: path, appdata: appdata, server: server, email: email, password: password}
}

func (c *bitwardenCLI) login(ctx context.Context) {
	c.run(ctx, nil, "config", "server", c.server)
	c.run(ctx, nil, "login", "--passwordenv", cliPasswordEnv, c.email)
	output := c.run(ctx, nil, "--raw", "unlock", "--passwordenv", cliPasswordEnv)
	c.session = strings.TrimSpace(string(output))
	if c.session == "" {
		c.t.Fatal("Bitwarden CLI returned an empty session")
	}
	c.run(ctx, nil, "sync", "--force")
}

func (c *bitwardenCLI) run(ctx context.Context, input []byte, args ...string) []byte {
	command := exec.CommandContext(ctx, c.path, append([]string{"--nointeraction"}, args...)...)
	// The official standalone CLI requires HTTPS. A short-lived local TLS
	// reverse proxy wraps the product's HTTP endpoint for this test. TLS
	// verification is disabled only for this disposable CLI process.
	command.Env = append(os.Environ(), "BITWARDENCLI_APPDATA_DIR="+c.appdata, cliPasswordEnv+"="+c.password, "NODE_TLS_REJECT_UNAUTHORIZED=0")
	if c.session != "" {
		command.Env = append(command.Env, "BW_SESSION="+c.session)
	}
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		diagnostics := string(output)
		if stderr.Len() > 0 {
			diagnostics += "\n" + stderr.String()
		}
		c.t.Fatalf("Bitwarden CLI %s failed: %v: %s", strings.Join(args, " "), err, sanitizeCLIOutput(diagnostics, c.password))
	}
	return output
}

func (c *bitwardenCLI) createItem(ctx context.Context, name, password string) string {
	item := map[string]any{
		"organizationId": nil, "folderId": nil, "type": 1, "reprompt": 0, "name": name, "notes": "stable E2E note", "favorite": false,
		"login":  map[string]any{"username": "vaultwarden-less@example.com", "password": password, "uris": []any{}},
		"fields": []any{}, "secureNote": nil, "card": nil,
	}
	return c.createItemJSON(ctx, item)
}

func (c *bitwardenCLI) createItemJSON(ctx context.Context, item map[string]any) string {
	data, err := json.Marshal(item)
	if err != nil {
		c.t.Fatal(err)
	}
	encoded := strings.TrimSpace(string(c.run(ctx, data, "encode")))
	created := c.run(ctx, nil, "--raw", "create", "item", encoded)
	var object map[string]any
	if err := json.Unmarshal(created, &object); err != nil {
		c.t.Fatalf("decode created item: %v", err)
	}
	id := stringValue(object["id"])
	if id == "" {
		c.t.Fatalf("created item has no ID: %#v", object)
	}
	return id
}

func (c *bitwardenCLI) createAttachment(ctx context.Context, itemID, path string) string {
	output := c.run(ctx, nil, "--raw", "create", "attachment", "--file", path, "--itemid", itemID)
	var object map[string]any
	if err := json.Unmarshal(output, &object); err != nil {
		c.t.Fatalf("decode created attachment: %v", err)
	}
	attachments, ok := object["attachments"].([]any)
	if !ok || len(attachments) == 0 {
		c.t.Fatalf("created item has no attachment list: %#v", object)
	}
	for _, value := range attachments {
		attachment, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if id := stringValue(attachment["id"]); id != "" {
			return id
		}
	}
	c.t.Fatalf("created attachment has no ID: %#v", object)
	return ""
}

func (c *bitwardenCLI) getItem(ctx context.Context, id string) map[string]any {
	output := c.run(ctx, nil, "--raw", "get", "item", id)
	var object map[string]any
	if err := json.Unmarshal(output, &object); err != nil {
		c.t.Fatalf("decode item: %v", err)
	}
	return object
}

func (c *bitwardenCLI) editItem(ctx context.Context, id string, item map[string]any) {
	data, err := json.Marshal(item)
	if err != nil {
		c.t.Fatal(err)
	}
	encoded := strings.TrimSpace(string(c.run(ctx, data, "encode")))
	c.run(ctx, nil, "--raw", "edit", "item", id, encoded)
}

func (c *bitwardenCLI) getAttachment(ctx context.Context, attachmentID, itemID, outputPath string) {
	c.run(ctx, nil, "--raw", "get", "attachment", attachmentID, "--itemid", itemID, "--output", outputPath)
}

func newTLSForwarder(t *testing.T, target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(parsed))
	t.Cleanup(server.Close)
	return server.URL
}

func setItemPassword(t *testing.T, item map[string]any, password string) {
	login, ok := item["login"].(map[string]any)
	if !ok {
		t.Fatalf("item has no login object: %#v", item)
	}
	login["password"] = password
}

func downloadBitwardenCLI(ctx context.Context, t *testing.T, directory string) string {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, bitwardenCLIURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := (&http.Client{Timeout: 5 * time.Minute}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Bitwarden CLI download returned %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 64<<20 {
		t.Fatal("Bitwarden CLI download exceeded its bound")
	}
	digest := sha256.Sum256(data)
	if got := hex.EncodeToString(digest[:]); got != bitwardenCLISHA {
		t.Fatalf("Bitwarden CLI SHA-256 = %s, want %s", got, bitwardenCLISHA)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 1 || archive.File[0].Name != "bw" {
		names := make([]string, 0, len(archive.File))
		for _, file := range archive.File {
			names = append(names, file.Name)
		}
		t.Fatalf("Bitwarden CLI archive members = %v, want exactly [bw]", names)
	}
	file := archive.File[0]
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "bw")
	output, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		_ = reader.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, reader)
	closeOutputErr := output.Close()
	closeReaderErr := reader.Close()
	if copyErr != nil || closeOutputErr != nil || closeReaderErr != nil {
		t.Fatalf("extract Bitwarden CLI: %v %v %v", copyErr, closeOutputErr, closeReaderErr)
	}
	return path
}

func stringValue(value any) string {
	valueString, _ := value.(string)
	return valueString
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func sanitizeCLIOutput(output, password string) string {
	output = strings.ReplaceAll(output, password, "<redacted>")
	return truncate(output, 4000)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
