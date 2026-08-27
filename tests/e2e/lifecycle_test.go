package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
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
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	productcli "github.com/hellodword/vaultwarden-less/internal/cli"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
)

const (
	testPassword             = "Correct Horse Battery Staple 2026!"
	initialItemPassword      = "initial-e2e-item-password"
	newItemPassword          = "new-e2e-item-password"
	secondPassword           = "second-user-password"
	organizationItemPassword = "organization-item-password"
)

var (
	lowerHex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	contentIDPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type e2eHarness struct {
	t                 *testing.T
	ctx               context.Context
	repository        string
	localRoot         string
	project           string
	version           string
	publishedHost     string
	proxyHostPort     int
	drillHostPort     int
	dockerConfig      string
	dockerPath        string
	dockerEnv         []string
	productionCompose string
	testCompose       string
	publicHTTP        string
	publicTLS         string
	tlsCAFile         string
	tlsServer         *httptest.Server

	backendMu sync.RWMutex
	backend   *url.URL
}

var (
	runVaultwardenE2E = flag.Bool("run-vaultwarden-e2e", false, "run the real Vaultwarden lifecycle test")
	proxyPort         = flag.Int("proxy-host-port", 0, "fixed proxy host port; zero asks Docker for an ephemeral port")
	drillPort         = flag.Int("drill-host-port", 0, "fixed drill host port; zero asks Docker for an ephemeral port")
)

func newE2EHarness(t *testing.T, ctx context.Context) *e2eHarness {
	t.Helper()
	repository := repositoryRoot(t)
	localRoot := t.TempDir()
	dockerConfig := filepath.Join(localRoot, "docker-config")
	if err := os.Mkdir(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(
		t,
		filepath.Join(dockerConfig, "config.json"),
		"{\"auths\":{\"https://index.docker.io/v1/\":{}}}\n",
	)
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
	validateOptionalPort(t, "-proxy-host-port", *proxyPort)
	validateOptionalPort(t, "-drill-host-port", *drillPort)
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("the Nix-provided Docker CLI is required: %v", err)
	}
	harness := &e2eHarness{
		t:             t,
		ctx:           ctx,
		repository:    repository,
		localRoot:     localRoot,
		project:       fmt.Sprintf("vwless-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()),
		version:       version,
		publishedHost: "127.0.0.1",
		proxyHostPort: *proxyPort,
		drillHostPort: *drillPort,
		dockerConfig:  dockerConfig,
		dockerPath:    dockerPath,
		dockerEnv:     isolatedDockerEnvironment(dockerConfig),
	}
	harness.productionCompose = filepath.Join(localRoot, "compose.yml")
	harness.testCompose = filepath.Join(localRoot, "compose.e2e.yml")
	t.Cleanup(harness.cleanup)
	if output, err := harness.docker("info"); err != nil {
		t.Fatalf("Docker daemon is required: %v: %s", err, truncateDiagnostic(output, 2000))
	}

	harness.prepareImages()
	harness.startTLSProxy()
	harness.writeComposeFiles("prepared")
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
		if _, err := os.Stat(filepath.Join(current, "deploy", "compose.template.yml")); err == nil {
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
			h.t.Fatalf("build Nix image %s: %v: %s", attribute, err, harnessDiagnostics(output))
		}
		paths := strings.Fields(output)
		if len(paths) != 1 {
			h.t.Fatalf("Nix returned invalid output for %s: %q", attribute, output)
		}
		if loaded, err := h.docker("load", "--input", paths[0]); err != nil {
			h.t.Fatalf("load Nix image %s: %v: %s", attribute, err, harnessDiagnostics(loaded))
		}
	}
	official := upstream.VaultwardenImageRepository + ":" + upstream.VaultwardenVersion + "@" + upstream.VaultwardenImageDigest
	if output, err := h.docker("pull", official); err != nil {
		h.t.Fatalf("pull pinned Vaultwarden image: %v: %s", err, harnessDiagnostics(output))
	}
	if output, err := h.docker("tag", official, upstream.VaultwardenLocalImage); err != nil {
		h.t.Fatalf("tag pinned Vaultwarden image: %v: %s", err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) startTLSProxy() {
	reverseProxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			h.backendMu.RLock()
			backend := h.backend
			h.backendMu.RUnlock()
			if backend == nil {
				request.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:1"})
				return
			}
			request.SetURL(backend)
			request.Out.Host = request.In.Host
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

func (h *e2eHarness) writeComposeFiles(drillName string) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString(drillName) {
		h.t.Fatalf("unsafe drill restore name %q", drillName)
	}
	production := renderTestTemplate(
		h.t,
		filepath.Join(h.repository, "deploy", "compose.template.yml"),
		map[string]string{
			"@@USER_UID@@":           "10000",
			"@@USER_GID@@":           "10000",
			"@@IMAGE_BACKUP@@":       "vaultwarden-less-backup:" + h.version,
			"@@IMAGE_PROXY@@":        "vaultwarden-less-proxy:" + h.version,
			"@@IMAGE_VAULTWARDEN@@":  upstream.VaultwardenLocalImage,
			"@@PORT_PROXY@@":         "0",
			"@@PORT_DRILL@@":         "0",
			"@@RESTORE_NAME_DRILL@@": drillName,
		},
	)
	overlay := renderTestTemplate(
		h.t,
		filepath.Join(h.repository, "tests", "e2e", "compose.template.yml"),
		map[string]string{
			"@@IMAGE_VAULTWARDEN@@": upstream.VaultwardenLocalImage,
			"@@IMAGE_MOTO@@":        "vaultwarden-less-moto:" + upstream.MotoVersion,
			"@@E2E_DOMAIN@@":        h.publicTLS,
			"@@PORT_PROXY_E2E@@":    strconv.Itoa(h.proxyHostPort),
			"@@PORT_DRILL_E2E@@":    strconv.Itoa(h.drillHostPort),
		},
	)
	writePrivateFile(h.t, h.productionCompose, production)
	writePrivateFile(h.t, h.testCompose, overlay)
}

func renderTestTemplate(t *testing.T, filename string, replacements map[string]string) string {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	result := string(data)
	for token, value := range replacements {
		if !strings.Contains(result, token) {
			t.Fatalf("test Compose template is missing %s", token)
		}
		result = strings.ReplaceAll(result, token, value)
	}
	if unresolved := regexp.MustCompile(`@@[A-Z0-9_]+@@`).FindAllString(result, -1); len(unresolved) != 0 {
		t.Fatalf("unresolved test Compose tokens: %v", unresolved)
	}
	if strings.Contains(result, "${") {
		t.Fatal("test Compose must not use environment interpolation")
	}
	return result
}

func validateOptionalPort(t *testing.T, name string, port int) {
	t.Helper()
	if port != 0 && (port < 1024 || port > 65535) {
		t.Fatalf("%s must be zero or an unprivileged TCP port", name)
	}
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

func (h *e2eHarness) seedVolumes() {
	h.compose("--profile", "seed", "run", "--rm", "--no-deps", "seed")
}

func (h *e2eHarness) compose(arguments ...string) string {
	h.t.Helper()
	output, err := h.tryCompose(h.ctx, arguments...)
	if err != nil {
		h.t.Fatalf("docker compose %s: %v: %s", strings.Join(arguments, " "), err, harnessDiagnostics(output))
	}
	return output
}

func (h *e2eHarness) tryCompose(ctx context.Context, arguments ...string) (string, error) {
	base := []string{
		"compose",
		"--env-file",
		"/dev/null",
		"--project-directory",
		h.localRoot,
		"--project-name",
		h.project,
		"--file",
		h.productionCompose,
		"--file",
		h.testCompose,
	}
	base = append(base, arguments...)
	return h.dockerContext(ctx, base...)
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
			output, inspectError := h.docker(
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

type pruneOutput struct {
	SchemaVersion int    `json:"schema_version"`
	Operation     string `json:"operation"`
	Target        string `json:"target"`
	Applied       bool   `json:"applied"`
	Groups        []struct {
		Host              string   `json:"host"`
		Paths             []string `json:"paths"`
		KeepSnapshotIDs   []string `json:"keep_snapshot_ids"`
		RemoveSnapshotIDs []string `json:"remove_snapshot_ids"`
	} `json:"groups"`
}

func (h *e2eHarness) repositoryCheck() {
	output := h.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "check", "--target", "primary", "--read-data-subset", "1/12",
		"--config", "/config/backup.yaml",
	)
	var result struct {
		SchemaVersion int    `json:"schema_version"`
		Operation     string `json:"operation"`
		Target        string `json:"target"`
		Success       bool   `json:"success"`
	}
	if decodeExactJSON(output, &result) != nil ||
		result.SchemaVersion != productcli.ResultSchemaVersion || result.Operation != "check" ||
		result.Target != "primary" || !result.Success {
		h.t.Fatalf("invalid repository check result: %s", harnessDiagnostics(output))
	}
}

func (h *e2eHarness) prune(apply bool) pruneOutput {
	arguments := []string{
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "prune", "--target", "primary", "--config", "/config/backup.yaml",
	}
	if apply {
		arguments = append(arguments, "--apply")
	}
	output := h.compose(arguments...)
	var result pruneOutput
	if decodeExactJSON(output, &result) != nil ||
		result.SchemaVersion != productcli.ResultSchemaVersion || result.Operation != "prune" ||
		result.Target != "primary" || result.Applied != apply || result.Groups == nil {
		h.t.Fatalf("invalid repository prune result: %s", harnessDiagnostics(output))
	}
	return result
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
	if err := decodeExactJSON(output, &result); err != nil ||
		result.SchemaVersion != productcli.ResultSchemaVersion ||
		result.Target != "primary" || result.Limit != 100 ||
		result.Snapshots == nil || result.Truncated {
		return snapshotList{}, fmt.Errorf("invalid snapshots response")
	}
	seen := make(map[string]bool, len(result.Snapshots))
	var previous time.Time
	for index, item := range result.Snapshots {
		parsed, err := time.Parse(time.RFC3339, item.Time)
		if err != nil || !lowerHex64Pattern.MatchString(item.ID) ||
			!contentIDPattern.MatchString(item.ContentID) || seen[item.ID] ||
			(index > 0 && parsed.After(previous)) {
			return snapshotList{}, fmt.Errorf("invalid snapshot identity or ordering")
		}
		seen[item.ID] = true
		previous = parsed
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
	output, err := h.docker(
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
		h.t.Fatalf("list file Send data: %v: %s", err, harnessDiagnostics(output))
	}
	names := strings.Fields(output)
	if len(names) != 1 || !lowerHex64Pattern.MatchString(names[0]) {
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
	output, err := h.docker(
		"exec",
		h.vaultwardenContainer(),
		"mv",
		"--",
		from,
		to,
	)
	if err != nil {
		h.t.Fatalf("rename E2E data file: %v: %s", err, harnessDiagnostics(output))
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
	if err := decodeExactJSON(output, &result); err != nil ||
		result.SchemaVersion != productcli.ResultSchemaVersion ||
		result.Target != "primary" || result.Output != "/restore/"+outputName {
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
		SchemaVersion int `json:"schema_version"`
	}
	var decoded result
	if err := decodeExactJSON(`{"schema_version":2}`, &decoded); err != nil || decoded.SchemaVersion != 2 {
		t.Fatalf("valid result = %#v/%v", decoded, err)
	}
	for name, value := range map[string]string{
		"unknown":    `{"schema_version":2,"extra":true}`,
		"additional": `{"schema_version":2} {"schema_version":2}`,
	} {
		withResult := result{}
		if err := decodeExactJSON(value, &withResult); err == nil {
			t.Fatalf("%s JSON was accepted", name)
		}
	}
}

func (h *e2eHarness) stopProduction() {
	h.compose("stop", "proxy", "backup", "vaultwarden")
}

func (h *e2eHarness) startDrill(recoveryName string) {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`).MatchString(recoveryName) {
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
	volume := h.project + "_e2e-drill-data"
	_, _ = h.docker("volume", "rm", volume)
	h.writeComposeFiles(recoveryName)
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
	if h.productionCompose != "" {
		_, _ = h.tryCompose(
			cleanupContext,
			"--profile",
			"drill",
			"--profile",
			"restore",
			"--profile",
			"maintenance",
			"--profile",
			"seed",
			"down",
			"--volumes",
			"--remove-orphans",
		)
	}
	if h.tlsServer != nil {
		h.tlsServer.Close()
	}
	if h.project != "" {
		containers, _ := h.dockerContext(
			cleanupContext,
			"ps",
			"--all",
			"--quiet",
			"--filter",
			"label=com.docker.compose.project="+h.project,
		)
		networks, _ := h.dockerContext(
			cleanupContext,
			"network",
			"ls",
			"--quiet",
			"--filter",
			"label=com.docker.compose.project="+h.project,
		)
		volumes, _ := h.dockerContext(
			cleanupContext,
			"volume",
			"ls",
			"--quiet",
			"--filter",
			"label=com.docker.compose.project="+h.project,
		)
		if strings.TrimSpace(containers) != "" || strings.TrimSpace(networks) != "" || strings.TrimSpace(volumes) != "" {
			h.t.Errorf("Compose cleanup left test containers, networks, or volumes")
		}
	}
}

func (h *e2eHarness) docker(arguments ...string) (string, error) {
	return h.dockerContext(h.ctx, arguments...)
}

func (h *e2eHarness) dockerContext(ctx context.Context, arguments ...string) (string, error) {
	arguments = append([]string{"--config", h.dockerConfig}, arguments...)
	return runCommandWithEnvironment(
		ctx,
		h.repository,
		nil,
		h.dockerEnv,
		h.dockerPath,
		arguments...,
	)
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

func isolatedDockerEnvironment(configDirectory string) []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + environmentOrDefault("TMPDIR", "/tmp"),
		"DOCKER_CONFIG=" + configDirectory,
	}
	for _, name := range []string{"DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_API_VERSION"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

func environmentOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func harnessDiagnostics(value string) string {
	return redactDiagnostics(value,
		testPassword,
		initialItemPassword,
		newItemPassword,
		secondPassword,
		organizationItemPassword,
		"e2e-restic-password",
		"BW_SESSION",
		"AWS_SECRET_ACCESS_KEY",
	)
}
