//go:build systemtest

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/upstream"
)

const (
	testPassword             = "Correct Horse Battery Staple 2026!"
	oldItemPassword          = "old-e2e-item-password"
	newItemPassword          = "new-e2e-item-password"
	secondPassword           = "second-user-password"
	organizationItemPassword = "organization-item-password"
)

var (
	sendFileNamePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostnamePattern     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

type e2eHarness struct {
	t              *testing.T
	ctx            context.Context
	repository     string
	localRoot      string
	project        string
	version        string
	envFile        string
	vaultEnv       string
	publishedHost  string
	publishAddress string
	proxyHostPort  int
	drillHostPort  int
	dockerConfig   string
	publicHTTP     string
	publicTLS      string
	tlsCAFile      string
	tlsServer      *httptest.Server

	backendMu sync.RWMutex
	backend   *url.URL
	volumes   []string
}

func newE2EHarness(t *testing.T, ctx context.Context) *e2eHarness {
	t.Helper()
	repository := repositoryRoot(t)
	localRoot := t.TempDir()
	dockerConfig := filepath.Join(localRoot, "docker-config")
	if err := os.Mkdir(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(dockerConfig, "config.json"), "{}\n")
	t.Setenv("DOCKER_CONFIG", dockerConfig)
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

	publishedHost := strings.TrimSpace(os.Getenv("VWLESS_E2E_PUBLISHED_HOST"))
	if publishedHost == "" {
		publishedHost = "127.0.0.1"
	}
	if net.ParseIP(publishedHost) == nil && !hostnamePattern.MatchString(publishedHost) {
		t.Fatal("VWLESS_E2E_PUBLISHED_HOST must be an IP address or hostname")
	}
	publishAddress := strings.TrimSpace(os.Getenv("VWLESS_E2E_PUBLISH_ADDRESS"))
	if publishAddress == "" {
		publishAddress = "127.0.0.1"
		if publishedHost != "127.0.0.1" && publishedHost != "localhost" {
			publishAddress = "0.0.0.0"
		}
	}
	if net.ParseIP(publishAddress) == nil {
		t.Fatal("VWLESS_E2E_PUBLISH_ADDRESS must be an IP address")
	}
	harness := &e2eHarness{
		t:              t,
		ctx:            ctx,
		repository:     repository,
		localRoot:      localRoot,
		project:        fmt.Sprintf("vwless-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()),
		version:        version,
		publishedHost:  publishedHost,
		publishAddress: publishAddress,
		proxyHostPort:  optionalHostPort(t, "VWLESS_E2E_PROXY_HOST_PORT"),
		drillHostPort:  optionalHostPort(t, "VWLESS_E2E_DRILL_HOST_PORT"),
		dockerConfig:   dockerConfig,
	}
	harness.envFile = filepath.Join(harness.localRoot, ".env")
	harness.vaultEnv = filepath.Join(harness.localRoot, "vaultwarden.env")
	t.Cleanup(harness.cleanup)

	harness.prepareImages()
	harness.startTLSProxy()
	harness.writeDeploymentFiles(".")
	harness.createVolumes()
	harness.seedVolumes()
	return harness
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "deploy", "compose.yml")); err == nil {
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

func (h *e2eHarness) prepareImages() {
	for _, attribute := range []string{"backup-image", "proxy-image", "moto-image"} {
		output, err := runCommand(
			h.ctx,
			h.repository,
			nil,
			"nix",
			"build",
			"--no-link",
			"--print-out-paths",
			".#"+attribute,
		)
		if err != nil {
			h.t.Fatalf("build Nix image %s: %v: %s", attribute, err, sanitize(output))
		}
		paths := strings.Fields(output)
		if len(paths) != 1 {
			h.t.Fatalf("Nix returned invalid output for %s: %q", attribute, output)
		}
		if loaded, err := runCommand(h.ctx, h.repository, nil, "docker", "load", "--input", paths[0]); err != nil {
			h.t.Fatalf("load Nix image %s: %v: %s", attribute, err, sanitize(loaded))
		}
	}
	official := upstream.VaultwardenImageRepository + ":" + upstream.VaultwardenVersion + "@" + upstream.VaultwardenImageDigest
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		h.t.Fatal(err)
	}
	pullEnvironment := isolatedDockerEnvironment(dockerPath, h.dockerConfig)
	if output, err := runCommandWithEnvironment(
		h.ctx,
		h.repository,
		nil,
		pullEnvironment,
		dockerPath,
		"--config",
		h.dockerConfig,
		"pull",
		official,
	); err != nil {
		h.t.Fatalf("pull pinned Vaultwarden image: %v: %s", err, sanitize(output))
	}
	if output, err := runCommand(h.ctx, h.repository, nil, "docker", "tag", official, upstream.VaultwardenLocalImage); err != nil {
		h.t.Fatalf("tag pinned Vaultwarden image: %v: %s", err, sanitize(output))
	}
}

func (h *e2eHarness) startTLSProxy() {
	reverseProxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			h.backendMu.RLock()
			backend := h.backend
			h.backendMu.RUnlock()
			if backend == nil {
				request.URL.Scheme = "http"
				request.URL.Host = "127.0.0.1:1"
				return
			}
			request.URL.Scheme = backend.Scheme
			request.URL.Host = backend.Host
		},
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	h.tlsServer = httptest.NewTLSServer(reverseProxy)
	h.publicTLS = h.tlsServer.URL
	certificate := h.tlsServer.Certificate()
	if certificate == nil {
		h.t.Fatal("E2E TLS server returned no certificate")
	}
	h.tlsCAFile = filepath.Join(h.localRoot, "tls-ca.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	if encoded == nil {
		h.t.Fatal("encode E2E TLS certificate")
	}
	writePrivateFile(h.t, h.tlsCAFile, string(encoded))
}

func (h *e2eHarness) setBackend(endpoint string) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		h.t.Fatalf("invalid E2E backend %q", endpoint)
	}
	h.backendMu.Lock()
	h.backend = parsed
	h.backendMu.Unlock()
	h.publicHTTP = endpoint
}

func (h *e2eHarness) writeDeploymentFiles(drillName string) {
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
	content := fmt.Sprintf(
		"DEPLOY_UID=65532\nDEPLOY_GID=65532\nDEPLOY_ROOT=/unused\n"+
			"PRODUCT_VERSION=%s\nBACKUP_IMAGE=vaultwarden-less-backup:%s\n"+
			"PROXY_IMAGE=vaultwarden-less-proxy:%s\nVAULTWARDEN_IMAGE=%s\n"+
			"MOTO_IMAGE=vaultwarden-less-moto:%s\nE2E_PROJECT=%s\nE2E_PUBLISH_ADDRESS=%s\n"+
			"E2E_PROXY_HOST_PORT=%d\nE2E_DRILL_HOST_PORT=%d\n"+
			"VAULTWARDEN_ENV_FILE=%s\nLISTEN_PORT=0\nDRILL_PORT=0\n"+
			"DRILL_DOMAIN=%s\nDRILL_RECOVERY_PATH=/unused\nDRILL_RECOVERY_NAME=%s\n",
		h.version,
		h.version,
		h.version,
		upstream.VaultwardenLocalImage,
		upstream.MotoVersion,
		h.project,
		h.publishAddress,
		h.proxyHostPort,
		h.drillHostPort,
		h.vaultEnv,
		h.publicTLS,
		drillName,
	)
	writePrivateFile(h.t, h.envFile, content)
}

func optionalHostPort(t *testing.T, name string) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	if strings.TrimSpace(raw) != raw {
		t.Fatalf("%s must not contain surrounding whitespace", name)
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1024 || port > 65535 {
		t.Fatalf("%s must be zero or an unprivileged TCP port", name)
	}
	return port
}

func writePrivateFile(t *testing.T, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *e2eHarness) createVolumes() {
	for _, suffix := range []string{
		"data",
		"staging",
		"cache",
		"restore",
		"config",
		"backup_credentials",
		"maintenance_credentials",
		"restic_credentials",
		"notifier_credentials",
		"drill_data",
	} {
		volume := h.project + "_" + suffix
		if output, err := runCommand(
			h.ctx,
			h.repository,
			nil,
			"docker",
			"volume",
			"create",
			"--label",
			"vaultwarden-less.e2e="+h.project,
			volume,
		); err != nil {
			h.t.Fatalf("create E2E volume %s: %v: %s", suffix, err, sanitize(output))
		}
		h.volumes = append(h.volumes, volume)
		h.initializeVolume(volume)
	}
}

func (h *e2eHarness) initializeVolume(volume string) {
	command := "chown 65532:65532 /target && chmod 0700 /target"
	if output, err := runCommand(
		h.ctx,
		h.repository,
		nil,
		"docker",
		"run",
		"--rm",
		"--user",
		"0:0",
		"--volume",
		volume+":/target",
		"--entrypoint",
		"/bin/sh",
		upstream.VaultwardenLocalImage,
		"-eu",
		"-c",
		command,
	); err != nil {
		h.t.Fatalf("initialize E2E volume: %v: %s", err, sanitize(output))
	}
}

func (h *e2eHarness) seedVolume(volumeSuffix, filename, content string) {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(filename) {
		h.t.Fatalf("unsafe volume seed filename %q", filename)
	}
	volume := h.project + "_" + volumeSuffix
	command := `umask 077; cat > "/target/$1"; chown 65532:65532 "/target/$1"; chmod 0600 "/target/$1"`
	if output, err := runCommand(
		h.ctx,
		h.repository,
		[]byte(content),
		"docker",
		"run",
		"--rm",
		"--interactive",
		"--user",
		"0:0",
		"--volume",
		volume+":/target",
		"--entrypoint",
		"/bin/sh",
		upstream.VaultwardenLocalImage,
		"-eu",
		"-c",
		command,
		"seed",
		filename,
	); err != nil {
		h.t.Fatalf("seed E2E volume %s: %v: %s", volumeSuffix, err, sanitize(output))
	}
}

func (h *e2eHarness) seedVolumes() {
	backupConfig := "schema_version: 2\n\n" +
		"targets:\n" +
		"  - name: primary\n" +
		"    repository: s3:http://moto:5000/vaultwarden-less-e2e/repository\n" +
		"    restic_options:\n" +
		"      s3.bucket-lookup: path\n" +
		"      s3.region: us-east-1\n\n" +
		"notifiers: []\n"
	credentials := "schema_version: 2\ntargets:\n  primary:\n" +
		"    AWS_ACCESS_KEY_ID: test\n    AWS_SECRET_ACCESS_KEY: test\n"
	h.seedVolume("config", "backup.yaml", backupConfig)
	h.seedVolume("backup_credentials", "credentials.yaml", credentials)
	h.seedVolume("maintenance_credentials", "credentials.yaml", credentials)
	h.seedVolume("restic_credentials", "primary", "e2e-restic-password\n")
	h.seedVolume(
		"notifier_credentials",
		"credentials.yaml",
		"schema_version: 2\nnotifiers: {}\n",
	)
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
		"compose",
		"--project-name",
		h.project,
		"--env-file",
		h.envFile,
		"--file",
		filepath.Join(h.repository, "deploy", "compose.yml"),
		"--file",
		filepath.Join(h.repository, "tests", "e2e", "compose.yml"),
	}
	base = append(base, arguments...)
	return runCommand(ctx, h.repository, nil, "docker", base...)
}

func (h *e2eHarness) startMotoAndRepository() {
	h.compose("up", "--detach", "moto")
	deadline := time.Now().Add(90 * time.Second)
	created := false
	for time.Now().Before(deadline) {
		_, err := h.tryCompose(
			h.ctx,
			"exec",
			"--no-TTY",
			"moto",
			"python",
			"-c",
			"import boto3; boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='test', aws_secret_access_key='test', region_name='us-east-1').create_bucket(Bucket='vaultwarden-less-e2e')",
		)
		if err == nil {
			created = true
			break
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !created {
		h.t.Fatal("Moto did not accept bucket creation")
	}
	h.compose(
		"--profile",
		"maintenance",
		"run",
		"--rm",
		"--no-deps",
		"maintenance",
		"repo",
		"init",
		"--target",
		"primary",
		"--config",
		"/config/backup.yaml",
	)
}

func (h *e2eHarness) startVaultwardenAndProxy() {
	h.compose("up", "--detach", "vaultwarden", "proxy")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	h.waitHealthy("proxy", 3*time.Minute)
	h.setBackend(h.mappedEndpoint("proxy", 8080))
	h.waitHTTP(h.publicHTTP+"/.vaultwarden-less/healthz", http.StatusNoContent, 2*time.Minute)
}

func (h *e2eHarness) startBackup() {
	h.compose("up", "--detach", "backup")
	h.waitHealthy("backup", 2*time.Minute)
}

func (h *e2eHarness) mappedEndpoint(service string, containerPort int) string {
	output := h.compose("port", service, strconv.Itoa(containerPort))
	lines := strings.Fields(output)
	if len(lines) != 1 {
		h.t.Fatalf("invalid published port for %s: %q", service, output)
	}
	_, rawPort, err := net.SplitHostPort(lines[0])
	if err != nil {
		h.t.Fatalf("invalid published port for %s: %q", service, output)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		h.t.Fatalf("invalid published TCP port for %s: %q", service, rawPort)
	}
	return "http://" + net.JoinHostPort(h.publishedHost, strconv.Itoa(port))
}

func (h *e2eHarness) waitHealthy(service string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		container, err := h.tryCompose(h.ctx, "ps", "--quiet", service)
		if err == nil && strings.TrimSpace(container) != "" {
			output, inspectError := runCommand(
				h.ctx,
				h.repository,
				nil,
				"docker",
				"inspect",
				"--format",
				"{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}",
				strings.TrimSpace(container),
			)
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
	client := &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
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
	Limit         int    `json:"limit"`
	Truncated     bool   `json:"truncated"`
	Snapshots     []struct {
		ID        string `json:"id"`
		Time      string `json:"time"`
		ContentID string `json:"content_id"`
	} `json:"snapshots"`
}

func (h *e2eHarness) snapshots() (snapshotList, error) {
	output, err := h.tryCompose(
		h.ctx,
		"exec",
		"--no-TTY",
		"backup",
		"/bin/vaultwarden-less-backup",
		"repo",
		"snapshots",
		"--target",
		"primary",
		"--limit",
		"100",
		"--config",
		"/config/backup.yaml",
	)
	if err != nil {
		return snapshotList{}, err
	}
	var result snapshotList
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Snapshots == nil || result.Truncated {
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
		output, err := h.tryCompose(
			h.ctx,
			"exec",
			"--no-TTY",
			"backup",
			"/bin/vaultwarden-less-backup",
			"status",
		)
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

func (h *e2eHarness) vaultwardenContainer() string {
	container := strings.TrimSpace(h.compose("ps", "--quiet", "vaultwarden"))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(container) {
		h.t.Fatalf("invalid Vaultwarden container ID %q", container)
	}
	return container
}

func (h *e2eHarness) singleFileIn(relativeDirectory string) string {
	h.t.Helper()
	relativeDirectory = cleanTestRelative(h.t, relativeDirectory)
	output, err := runCommand(
		h.ctx,
		h.repository,
		nil,
		"docker",
		"exec",
		h.vaultwardenContainer(),
		"find",
		"/"+relativeDirectory,
		"-mindepth",
		"1",
		"-maxdepth",
		"1",
		"-type",
		"f",
		"-printf",
		"%f\n",
	)
	if err != nil {
		h.t.Fatalf("list file Send data: %v: %s", err, sanitize(output))
	}
	names := strings.Fields(output)
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
	source := "/" + relative
	held := "/data/.e2e-held-" + strings.ReplaceAll(strings.TrimPrefix(relative, "data/"), "/", "-")
	restored := false
	h.renameContainerFile(source, held)
	restore := func() {
		if restored {
			return
		}
		h.renameContainerFile(held, source)
		restored = true
	}
	h.t.Cleanup(restore)
	return restore
}

func (h *e2eHarness) renameContainerFile(from, to string) {
	h.t.Helper()
	output, err := runCommand(
		h.ctx,
		h.repository,
		nil,
		"docker",
		"exec",
		h.vaultwardenContainer(),
		"mv",
		"--",
		from,
		to,
	)
	if err != nil {
		h.t.Fatalf("rename E2E data file: %v: %s", err, sanitize(output))
	}
}

func cleanTestRelative(t *testing.T, value string) string {
	t.Helper()
	clean := path.Clean(value)
	if value == "" || strings.HasPrefix(value, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		t.Fatalf("unsafe E2E relative path %q", value)
	}
	return value
}

func (h *e2eHarness) waitUnchangedCheck(previousFinished string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		output, err := h.tryCompose(
			h.ctx,
			"exec",
			"--no-TTY",
			"backup",
			"/bin/vaultwarden-less-backup",
			"status",
		)
		if err == nil {
			var status struct {
				LastCheck *struct {
					FinishedAt string `json:"finished_at"`
					Result     string `json:"result"`
				} `json:"last_check"`
			}
			if json.Unmarshal([]byte(output), &status) == nil &&
				status.LastCheck != nil &&
				status.LastCheck.FinishedAt != previousFinished &&
				status.LastCheck.Result == "unchanged" {
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
		"primary",
		"--output",
		"/restore/" + outputName,
		"--config",
		"/config/backup.yaml",
	}
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

func (h *e2eHarness) startDrill(recoveryName string) {
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`).MatchString(recoveryName) {
		h.t.Fatalf("unsafe drill recovery name %q", recoveryName)
	}
	_, _ = h.tryCompose(
		h.ctx,
		"--profile",
		"drill",
		"rm",
		"--force",
		"--stop",
		"drill-vaultwarden",
		"drill-copy",
	)
	volume := h.project + "_drill_data"
	_, _ = runCommand(h.ctx, h.repository, nil, "docker", "volume", "rm", volume)
	if output, err := runCommand(h.ctx, h.repository, nil, "docker", "volume", "create", "--label", "vaultwarden-less.e2e="+h.project, volume); err != nil {
		h.t.Fatalf("recreate drill volume: %v: %s", err, sanitize(output))
	}
	h.initializeVolume(volume)
	h.writeDeploymentFiles(recoveryName)
	h.compose("--profile", "drill", "up", "--detach", "drill-vaultwarden")
	h.waitHealthy("drill-vaultwarden", 3*time.Minute)
	h.setBackend(h.mappedEndpoint("drill-vaultwarden", 80))
	h.waitHTTP(h.publicHTTP+"/alive", http.StatusOK, time.Minute)
}

func (h *e2eHarness) stopDrill() {
	_, _ = h.tryCompose(
		h.ctx,
		"--profile",
		"drill",
		"rm",
		"--force",
		"--stop",
		"drill-vaultwarden",
		"drill-copy",
	)
}

func (h *e2eHarness) cleanup() {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 3*time.Minute)
	defer cancel()
	if h.envFile != "" {
		_, _ = h.tryCompose(
			cleanupContext,
			"--profile",
			"drill",
			"--profile",
			"restore",
			"--profile",
			"maintenance",
			"down",
			"--remove-orphans",
		)
	}
	if h.tlsServer != nil {
		h.tlsServer.Close()
	}
	for _, volume := range h.volumes {
		if output, err := runCommand(cleanupContext, h.repository, nil, "docker", "volume", "rm", volume); err != nil {
			h.t.Errorf("remove E2E volume %s: %v: %s", volume, err, sanitize(output))
		}
	}
	if h.project != "" {
		containers, _ := runCommand(
			cleanupContext,
			h.repository,
			nil,
			"docker",
			"ps",
			"--all",
			"--quiet",
			"--filter",
			"label=com.docker.compose.project="+h.project,
		)
		networks, _ := runCommand(
			cleanupContext,
			h.repository,
			nil,
			"docker",
			"network",
			"ls",
			"--quiet",
			"--filter",
			"label=com.docker.compose.project="+h.project,
		)
		if strings.TrimSpace(containers) != "" || strings.TrimSpace(networks) != "" {
			h.t.Errorf("Compose cleanup left test containers or networks")
		}
	}
}

func runCommand(ctx context.Context, directory string, input []byte, binary string, arguments ...string) (string, error) {
	return runCommandWithEnvironment(ctx, directory, input, os.Environ(), binary, arguments...)
}

func runCommandWithEnvironment(
	ctx context.Context,
	directory string,
	input []byte,
	environment []string,
	binary string,
	arguments ...string,
) (string, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = directory
	command.Env = environment
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

func isolatedDockerEnvironment(dockerPath, configDirectory string) []string {
	environment := make([]string, 0, len(os.Environ())+2)
	for _, value := range os.Environ() {
		name, _, ok := strings.Cut(value, "=")
		if ok && name != "PATH" && name != "DOCKER_CONFIG" {
			environment = append(environment, value)
		}
	}
	return append(
		environment,
		"PATH="+filepath.Dir(dockerPath),
		"DOCKER_CONFIG="+configDirectory,
	)
}

func sanitize(value string) string {
	for _, secret := range []string{
		testPassword,
		oldItemPassword,
		newItemPassword,
		secondPassword,
		organizationItemPassword,
		"e2e-restic-password",
		"BW_SESSION",
		"AWS_SECRET_ACCESS_KEY",
	} {
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
