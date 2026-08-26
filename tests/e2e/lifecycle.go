//go:build systemtest

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testPassword    = "Correct Horse Battery Staple 2026!"
	oldItemPassword = "old-e2e-item-password"
	newItemPassword = "new-e2e-item-password"
	secondPassword  = "second-user-password"
)

var remoteRootPattern = regexp.MustCompile(`^/tmp/vaultwarden-less-e2e\.[A-Za-z0-9]+$`)
var sendFileNamePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type e2eHarness struct {
	t          *testing.T
	ctx        context.Context
	repository string
	localRoot  string
	deployRoot string
	project    string
	image      string
	version    string
	envFile    string
	vaultEnv   string
	uid        int
	gid        int
	proxyPort  int
	publicHTTP string
	publicTLS  string
	drillPath  string

	sshTarget  string
	sshPort    int
	tunnelEnd  context.CancelFunc
	tunnelDone <-chan error
	tlsServer  *httptest.Server
}

func newE2EHarness(t *testing.T, ctx context.Context) *e2eHarness {
	t.Helper()
	repository := repositoryRoot(t)
	versionData, err := os.ReadFile(filepath.Join(repository, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(versionData))
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(version) {
		t.Fatalf("invalid VERSION %q", version)
	}
	if _, err := exec.LookPath("bw"); err != nil {
		t.Fatalf("the Nix-provided official bw CLI is required: %v", err)
	}
	if output, err := runCommand(ctx, repository, nil, "docker", "info"); err != nil {
		t.Fatalf("Docker daemon is required: %v: %s", err, truncate(output, 2000))
	}

	harness := &e2eHarness{
		t: t, ctx: ctx, repository: repository, localRoot: t.TempDir(), version: version,
		project:   fmt.Sprintf("vwless-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()),
		sshTarget: strings.TrimSpace(os.Getenv("VWLESS_E2E_SSH_TARGET")),
	}
	harness.image = harness.project + ":" + version
	harness.envFile = filepath.Join(harness.localRoot, ".env")
	harness.vaultEnv = filepath.Join(harness.localRoot, ".env.vaultwarden")
	// Register cleanup before creating any remote resources or building the
	// image so setup failures receive the same exact, task-scoped cleanup as
	// scenario failures.
	t.Cleanup(harness.cleanup)
	if raw := os.Getenv("VWLESS_E2E_SSH_PORT"); raw != "" {
		port, parseError := strconv.Atoi(raw)
		if parseError != nil || port < 1 || port > 65535 {
			t.Fatal("VWLESS_E2E_SSH_PORT must be a valid TCP port")
		}
		harness.sshPort = port
	} else {
		harness.sshPort = 22
	}
	if harness.sshTarget == "" {
		harness.prepareLocalRoot()
	} else {
		harness.prepareRemoteRoot()
	}
	harness.proxyPort = harness.reservePort()
	if harness.sshTarget == "" {
		harness.publicHTTP = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(harness.proxyPort))
	} else {
		localPort := reserveLocalPort(t)
		harness.tunnelEnd, harness.tunnelDone = harness.startTunnel(localPort, harness.proxyPort)
		harness.publicHTTP = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	}
	parsed, err := url.Parse(harness.publicHTTP)
	if err != nil {
		t.Fatal(err)
	}
	reverseProxy := httputil.NewSingleHostReverseProxy(parsed)
	reverseProxy.Transport = &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
	}
	harness.tlsServer = httptest.NewTLSServer(reverseProxy)
	harness.publicTLS = harness.tlsServer.URL
	harness.drillPath = filepath.Join(harness.deployRoot, "restore", "drill")
	harness.writeDeploymentFiles()
	harness.buildImage()
	return harness
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "compose.yml")); err == nil {
			if _, err := os.Stat(filepath.Join(current, "VERSION")); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root not found")
		}
		current = parent
	}
}

func (h *e2eHarness) prepareLocalRoot() {
	h.deployRoot = filepath.Join(h.localRoot, "deployment")
	if err := os.Mkdir(h.deployRoot, 0o700); err != nil {
		h.t.Fatal(err)
	}
	h.uid, h.gid = os.Geteuid(), os.Getegid()
	for _, directory := range []string{"data", "staging", "cache", "restore", "config", "credentials", "credentials/backup", "credentials/maintenance", "credentials/common"} {
		if err := os.Mkdir(filepath.Join(h.deployRoot, directory), 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *e2eHarness) prepareRemoteRoot() {
	if strings.HasPrefix(h.sshTarget, "-") || strings.ContainsAny(h.sshTarget, "\r\n\x00") {
		h.t.Fatal("invalid VWLESS_E2E_SSH_TARGET")
	}
	output := h.ssh("mktemp -d /tmp/vaultwarden-less-e2e.XXXXXX")
	h.deployRoot = strings.TrimSpace(output)
	if !remoteRootPattern.MatchString(h.deployRoot) {
		h.t.Fatalf("remote temporary root has unexpected form %q", h.deployRoot)
	}
	h.uid = parsePositiveInt(h.t, strings.TrimSpace(h.ssh("id -u")), "remote UID")
	h.gid = parsePositiveInt(h.t, strings.TrimSpace(h.ssh("id -g")), "remote GID")
	directories := make([]string, 0, 6)
	for _, name := range []string{"data", "staging", "cache", "restore", "config", "credentials", "credentials/backup", "credentials/maintenance", "credentials/common"} {
		directories = append(directories, shellQuote(filepath.Join(h.deployRoot, name)))
	}
	h.ssh("chmod 0700 " + shellQuote(h.deployRoot) + " && mkdir -m 0700 " + strings.Join(directories, " "))
}

func (h *e2eHarness) writeDeploymentFiles() {
	backupConfig := "schema_version: 1\n\n" +
		"targets:\n" +
		"  - name: primary\n" +
		"    repository: s3:http://moto:5000/vaultwarden-less-e2e/repository\n" +
		"    restic_options:\n" +
		"      s3.bucket-lookup: path\n" +
		"      s3.region: us-east-1\n\n" +
		"notifiers: []\n"
	credentials := "schema_version: 1\ntargets:\n  primary:\n    AWS_ACCESS_KEY_ID: test\n    AWS_SECRET_ACCESS_KEY: test\n"
	h.seedPrivateFile("config/backup.yaml", backupConfig)
	h.seedPrivateFile("credentials/backup/credentials.yaml", credentials)
	h.seedPrivateFile("credentials/maintenance/credentials.yaml", credentials)
	h.seedPrivateFile("credentials/common/restic-password", "e2e-restic-password\n")
	vaultwardenEnvironment := strings.Join([]string{
		"DOMAIN=" + h.publicTLS,
		"ROCKET_ADDRESS=0.0.0.0",
		"ROCKET_PORT=80",
		"DATA_FOLDER=/data",
		"DATABASE_URL=sqlite:///data/db.sqlite3",
		"RSA_KEY_FILENAME=/data/rsa_key",
		"ATTACHMENTS_FOLDER=/data/attachments",
		"SENDS_FOLDER=/data/sends",
		"WEB_VAULT_ENABLED=false",
		"ENABLE_WEBSOCKET=false",
		"PUSH_ENABLED=false",
		"SIGNUPS_ALLOWED=true",
		"SIGNUPS_VERIFY=false",
		"INVITATIONS_ALLOWED=true",
		"DISABLE_ICON_DOWNLOAD=true",
		"LOG_LEVEL=warn",
	}, "\n") + "\n"
	writePrivateFile(h.t, h.vaultEnv, vaultwardenEnvironment)
	h.writeComposeEnvironment(h.drillPath)
}

func (h *e2eHarness) writeComposeEnvironment(drillPath string) {
	content := fmt.Sprintf(
		"DEPLOY_UID=%d\nDEPLOY_GID=%d\nDEPLOY_ROOT=%s\nPRODUCT_VERSION=%s\nPRODUCT_IMAGE=%s\nVAULTWARDEN_ENV_FILE=%s\nLISTEN_PORT=%d\nDRILL_PORT=%d\nDRILL_DOMAIN=%s\nDRILL_RECOVERY_PATH=%s\n",
		h.uid, h.gid, h.deployRoot, h.version, h.image, h.vaultEnv, h.proxyPort, h.proxyPort, h.publicTLS, drillPath,
	)
	writePrivateFile(h.t, h.envFile, content)
	h.drillPath = drillPath
}

func (h *e2eHarness) seedPrivateFile(relative, content string) {
	if h.sshTarget == "" {
		writePrivateFile(h.t, filepath.Join(h.deployRoot, filepath.FromSlash(relative)), content)
		return
	}
	local := filepath.Join(h.localRoot, "seed-"+strings.ReplaceAll(relative, "/", "-"))
	writePrivateFile(h.t, local, content)
	destination := filepath.Join(h.deployRoot, filepath.FromSlash(relative))
	h.scp(local, destination)
	h.ssh("chmod 0600 " + shellQuote(destination))
}

func writePrivateFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *e2eHarness) buildImage() {
	output, err := runCommand(h.ctx, h.repository, nil,
		"docker", "buildx", "build", "--load", "--provenance=false", "--sbom=false", "--platform", "linux/amd64",
		"--file", "docker/Dockerfile", "--build-arg", "PRODUCT_VERSION="+h.version,
		"--tag", h.image, ".",
	)
	if err != nil {
		h.t.Fatalf("build product image: %v: %s", err, truncate(output, 6000))
	}
}

func (h *e2eHarness) compose(arguments ...string) string {
	h.t.Helper()
	output, err := h.tryCompose(h.ctx, arguments...)
	if err != nil {
		h.t.Fatalf("docker compose %s: %v: %s", strings.Join(arguments, " "), err, sanitize(output))
	}
	return output
}

func (h *e2eHarness) tryCompose(ctx context.Context, arguments ...string) (string, error) {
	base := []string{
		"compose", "--project-name", h.project, "--env-file", h.envFile,
		"--file", filepath.Join(h.repository, "compose.yml"),
		"--file", filepath.Join(h.repository, "compose.e2e.yml"),
		"--file", filepath.Join(h.repository, "compose.drill.yml"),
	}
	base = append(base, arguments...)
	return runCommand(ctx, h.repository, nil, "docker", base...)
}

func (h *e2eHarness) startMotoAndRepository() {
	h.compose("up", "--detach", "moto")
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		_, err := h.tryCompose(h.ctx, "exec", "--no-TTY", "moto", "python", "-c", "import boto3; boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1').create_bucket(Bucket='vaultwarden-less-e2e')")
		if err == nil {
			break
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	h.compose("--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance", "repo", "init", "--target", "primary", "--config", "/config/backup.yaml")
}

func (h *e2eHarness) startVaultwardenAndProxy() {
	h.compose("up", "--detach", "vaultwarden")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	h.compose("up", "--detach", "proxy")
	h.waitHealthy("proxy", 3*time.Minute)
	h.waitHTTP(h.publicHTTP+"/.vaultwarden-less/healthz", http.StatusNoContent, 2*time.Minute)
}

func (h *e2eHarness) startBackup() {
	h.compose("up", "--detach", "backup")
	h.waitHealthy("backup", 2*time.Minute)
}

func (h *e2eHarness) waitHealthy(service string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		container, err := h.tryCompose(h.ctx, "ps", "--quiet", service)
		if err == nil && strings.TrimSpace(container) != "" {
			output, inspectError := runCommand(h.ctx, h.repository, nil, "docker", "inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", strings.TrimSpace(container))
			fields := strings.Fields(output)
			if inspectError == nil && len(fields) >= 2 && fields[0] == "running" && fields[1] == "healthy" {
				return
			}
			if inspectError == nil && len(fields) > 0 && fields[0] == "exited" {
				h.t.Fatalf("service %s exited", service)
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	h.t.Fatalf("service %s did not become healthy", service)
}

func (h *e2eHarness) waitHTTP(endpoint string, status int, timeout time.Duration) {
	h.t.Helper()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(h.ctx, http.MethodGet, endpoint, nil)
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
			_ = response.Body.Close()
			if response.StatusCode == status {
				return
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	h.t.Fatalf("endpoint %s did not return %d", endpoint, status)
}

type snapshotList struct {
	SchemaVersion int    `json:"schema_version"`
	Target        string `json:"target"`
	Snapshots     []struct {
		ID        string `json:"id"`
		Time      string `json:"time"`
		ContentID string `json:"content_id"`
	} `json:"snapshots"`
}

func (h *e2eHarness) snapshots() (snapshotList, error) {
	output, err := h.tryCompose(h.ctx, "exec", "--no-TTY", "backup", "/usr/local/bin/vaultwarden-less-backup", "repo", "snapshots", "--target", "primary", "--config", "/config/backup.yaml")
	if err != nil {
		return snapshotList{}, err
	}
	var result snapshotList
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Snapshots == nil {
		return snapshotList{}, fmt.Errorf("invalid snapshots response")
	}
	return result, nil
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

func (h *e2eHarness) assertSnapshotCountFor(count int, duration time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(duration)
	observed := false
	for time.Now().Before(deadline) {
		result, err := h.snapshots()
		if err == nil {
			observed = true
			if len(result.Snapshots) != count {
				h.t.Fatalf("snapshot count became %d during an inconsistent source window, want %d", len(result.Snapshots), count)
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	if !observed {
		h.t.Fatal("could not observe repository snapshots during inconsistent source window")
	}
}

func (h *e2eHarness) waitWorkerState(want string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := h.tryCompose(h.ctx, "exec", "--no-TTY", "backup", "/usr/local/bin/vaultwarden-less-backup", "status")
		if err == nil {
			var status struct {
				State string `json:"state"`
			}
			if json.Unmarshal([]byte(output), &status) == nil && status.State == want {
				return
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	h.t.Fatalf("worker state did not become %s", want)
}

func (h *e2eHarness) singleFileIn(relativeDirectory string) string {
	h.t.Helper()
	relativeDirectory = cleanTestRelative(h.t, relativeDirectory)
	var names []string
	if h.sshTarget == "" {
		entries, err := os.ReadDir(filepath.Join(h.deployRoot, filepath.FromSlash(relativeDirectory)))
		if err != nil {
			h.t.Fatal(err)
		}
		for _, entry := range entries {
			info, infoErr := entry.Info()
			if infoErr != nil {
				h.t.Fatal(infoErr)
			}
			if info.Mode().IsRegular() {
				names = append(names, entry.Name())
			}
		}
	} else {
		directory := filepath.Join(h.deployRoot, filepath.FromSlash(relativeDirectory))
		output := h.ssh("find " + shellQuote(directory) + " -mindepth 1 -maxdepth 1 -type f -printf '%f\\n'")
		for _, name := range strings.Split(strings.TrimSpace(output), "\n") {
			if name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) != 1 || !sendFileNamePattern.MatchString(names[0]) {
		h.t.Fatalf("file Send directory contains unexpected entries: %q", names)
	}
	return relativeDirectory + "/" + names[0]
}

func (h *e2eHarness) holdDataFile(relative string) func() {
	h.t.Helper()
	relative = cleanTestRelative(h.t, relative)
	if !strings.HasPrefix(relative, "data/") {
		h.t.Fatalf("refusing to hold a non-data test path %q", relative)
	}
	held := ".e2e-held-" + strings.ReplaceAll(relative, "/", "-")
	restored := false
	h.renameDeploymentFile(relative, held)
	restore := func() {
		if restored {
			return
		}
		h.renameDeploymentFile(held, relative)
		restored = true
	}
	h.t.Cleanup(restore)
	return restore
}

func (h *e2eHarness) renameDeploymentFile(fromRelative, toRelative string) {
	h.t.Helper()
	fromRelative = cleanTestRelative(h.t, fromRelative)
	toRelative = cleanTestRelative(h.t, toRelative)
	from := filepath.Join(h.deployRoot, filepath.FromSlash(fromRelative))
	to := filepath.Join(h.deployRoot, filepath.FromSlash(toRelative))
	if h.sshTarget == "" {
		if err := os.Rename(from, to); err != nil {
			h.t.Fatalf("rename test data file: %v", err)
		}
		return
	}
	h.ssh("mv -- " + shellQuote(from) + " " + shellQuote(to))
}

func cleanTestRelative(t *testing.T, value string) string {
	t.Helper()
	converted := filepath.FromSlash(value)
	clean := filepath.Clean(converted)
	if value == "" || filepath.IsAbs(converted) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != value {
		t.Fatalf("unsafe E2E relative path %q", value)
	}
	return value
}

func (h *e2eHarness) waitUnchangedCheck(previousFinished string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := h.tryCompose(h.ctx, "exec", "--no-TTY", "backup", "/usr/local/bin/vaultwarden-less-backup", "status")
		if err == nil {
			var status struct {
				LastCheck *struct {
					FinishedAt string `json:"finished_at"`
					Result     string `json:"result"`
				} `json:"last_check"`
			}
			if json.Unmarshal([]byte(output), &status) == nil && status.LastCheck != nil && status.LastCheck.FinishedAt != previousFinished && status.LastCheck.Result == "unchanged" {
				return
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	h.t.Fatal("worker did not complete an unchanged startup check")
}

type restoreOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Target        string `json:"target"`
	SnapshotID    string `json:"snapshot_id"`
	ContentID     string `json:"content_id"`
	Output        string `json:"output"`
}

func (h *e2eHarness) restore(snapshotID, outputName string) restoreOutput {
	arguments := []string{"--profile", "restore", "run", "--rm", "--no-deps", "restore", "restore", "--target", "primary", "--output", "/restore/" + outputName, "--config", "/config/backup.yaml"}
	if snapshotID != "" {
		arguments = append(arguments, "--snapshot", snapshotID)
	}
	output := h.compose(arguments...)
	var result restoreOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		h.t.Fatalf("decode restore output: %v: %s", err, sanitize(output))
	}
	return result
}

func (h *e2eHarness) stopProduction() {
	h.compose("stop", "proxy", "backup", "vaultwarden")
}

func (h *e2eHarness) startDrill(recoveryPath string) {
	h.writeComposeEnvironment(recoveryPath)
	h.compose("--profile", "drill", "up", "--detach", "drill-vaultwarden")
	h.waitHealthy("drill-vaultwarden", 3*time.Minute)
	h.waitHTTP(h.publicHTTP+"/alive", http.StatusOK, time.Minute)
}

func (h *e2eHarness) stopDrill() {
	h.compose("--profile", "drill", "stop", "drill-vaultwarden")
}

func (h *e2eHarness) reservePort() int {
	if h.sshTarget == "" {
		return reserveLocalPort(h.t)
	}
	code := "import socket; s=socket.socket(); s.bind(('127.0.0.1',0)); print(s.getsockname()[1]); s.close()"
	return parsePositiveInt(h.t, strings.TrimSpace(h.ssh("python3 -c "+shellQuote(code))), "remote port")
}

func reserveLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func (h *e2eHarness) startTunnel(localPort, remotePort int) (context.CancelFunc, <-chan error) {
	tunnelContext, cancel := context.WithCancel(h.ctx)
	arguments := h.sshArguments()
	arguments = append(arguments,
		"-N", "-o", "ExitOnForwardFailure=yes", "-o", "LogLevel=ERROR",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort), h.sshTarget,
	)
	binary := "ssh"
	if os.Getenv("SSHPASS") != "" {
		binary = "sshpass"
		arguments = append([]string{"-e", "ssh"}, arguments...)
	}
	command := exec.CommandContext(tunnelContext, binary, arguments...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		cancel()
		h.t.Fatalf("start SSH tunnel: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return cancel, done
}

func (h *e2eHarness) ssh(command string) string {
	h.t.Helper()
	arguments := append(h.sshArguments(), h.sshTarget, command)
	binary := "ssh"
	if os.Getenv("SSHPASS") != "" {
		binary = "sshpass"
		arguments = append([]string{"-e", "ssh"}, arguments...)
	}
	output, err := runCommand(h.ctx, h.repository, nil, binary, arguments...)
	if err != nil {
		h.t.Fatalf("remote SSH operation failed: %v: %s", err, sanitize(output))
	}
	return output
}

func (h *e2eHarness) scp(source, destination string) {
	h.t.Helper()
	arguments := []string{
		"-P", strconv.Itoa(h.sshPort), "-o", "BatchMode=no", "-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR", source, h.sshTarget + ":" + destination,
	}
	binary := "scp"
	if os.Getenv("SSHPASS") != "" {
		binary = "sshpass"
		arguments = append([]string{"-e", "scp"}, arguments...)
	}
	if output, err := runCommand(h.ctx, h.repository, nil, binary, arguments...); err != nil {
		h.t.Fatalf("copy test fixture to remote root: %v: %s", err, sanitize(output))
	}
}

func (h *e2eHarness) sshArguments() []string {
	return []string{
		"-p", strconv.Itoa(h.sshPort), "-o", "BatchMode=no", "-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR",
	}
}

func (h *e2eHarness) cleanup() {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 3*time.Minute)
	defer cancel()
	if h.envFile != "" {
		_, _ = h.tryCompose(cleanupContext, "--profile", "drill", "--profile", "restore", "--profile", "maintenance", "down", "--remove-orphans")
	}
	if h.tlsServer != nil {
		h.tlsServer.Close()
	}
	if h.tunnelEnd != nil {
		h.tunnelEnd()
		select {
		case <-h.tunnelDone:
		case <-time.After(5 * time.Second):
			h.t.Errorf("SSH tunnel did not stop during cleanup")
		}
	}
	if h.project != "" {
		containers, _ := runCommand(cleanupContext, h.repository, nil, "docker", "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+h.project)
		networks, _ := runCommand(cleanupContext, h.repository, nil, "docker", "network", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+h.project)
		volumes, _ := runCommand(cleanupContext, h.repository, nil, "docker", "volume", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+h.project)
		if strings.TrimSpace(containers) != "" || strings.TrimSpace(networks) != "" || strings.TrimSpace(volumes) != "" {
			h.t.Errorf("Compose cleanup left test containers, networks, or volumes")
		}
		_, _ = runCommand(cleanupContext, h.repository, nil, "docker", "image", "rm", h.image)
		if _, err := runCommand(cleanupContext, h.repository, nil, "docker", "image", "inspect", h.image); err == nil {
			h.t.Errorf("E2E product image remained after cleanup")
		}
	}
	if h.sshTarget != "" && remoteRootPattern.MatchString(h.deployRoot) {
		arguments := append(h.sshArguments(), h.sshTarget, "find "+shellQuote(h.deployRoot)+" -depth -delete && test ! -e "+shellQuote(h.deployRoot))
		binary := "ssh"
		if os.Getenv("SSHPASS") != "" {
			binary = "sshpass"
			arguments = append([]string{"-e", "ssh"}, arguments...)
		}
		if output, err := runCommand(cleanupContext, h.repository, nil, binary, arguments...); err != nil {
			h.t.Errorf("remote E2E root cleanup failed: %v: %s", err, sanitize(output))
		}
	}
}

func runCommand(ctx context.Context, directory string, input []byte, binary string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = directory
	command.Env = os.Environ()
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String() + "\n" + stderr.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func parsePositiveInt(t *testing.T, value, label string) int {
	t.Helper()
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		t.Fatalf("invalid %s %q", label, value)
	}
	return parsed
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func sanitize(value string) string {
	for _, secret := range []string{testPassword, oldItemPassword, newItemPassword, secondPassword, "e2e-restic-password", "BW_SESSION", "AWS_SECRET_ACCESS_KEY"} {
		value = strings.ReplaceAll(value, secret, "<redacted>")
	}
	return truncate(value, 6000)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}
