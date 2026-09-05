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
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

const (
	testPassword        = "Correct Horse Battery Staple 2026!"
	initialItemPassword = "initial-e2e-item-password"
)

var (
	runE2E                = flag.Bool("run-e2e", false, "run the two release-level Docker E2E scenarios")
	e2eImageDirectory     = flag.String("e2e-image-directory", "", "directory containing the four release-test image archives")
	e2eProductImageTag    = flag.String("e2e-product-image-tag", "", "complete git-<commit> tag for the three production images")
	lowerHex64Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lowerHex40Pattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	fullReleaseTagPattern = regexp.MustCompile(`^git-[0-9a-f]{40}$`)
	contentIDPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type e2eImageSet struct {
	directory  string
	productTag string
	archives   map[string]string
}

type e2eHarness struct {
	t               *testing.T
	ctx             context.Context
	repository      string
	testRoot        string
	deploymentRoot  string
	project         string
	dockerPath      string
	dockerConfig    string
	dockerEnv       []string
	composeFile     string
	overrideFile    string
	environmentFile string
	images          e2eImageSet

	vaultwardenPort int
	proxyPort       int
	sourcePort      int
	primaryPort     int
	b2Port          int

	publicTLS string
	tlsCAFile string
	tlsServer *httptest.Server

	backendMu sync.RWMutex
	backend   *url.URL
}

func newE2EHarness(t *testing.T, ctx context.Context, targets []string) *e2eHarness {
	t.Helper()
	if os.Geteuid() == 0 || os.Getegid() == 0 {
		t.Fatal("release E2E must run with a non-root runtime identity")
	}
	if _, err := exec.LookPath("bw"); err != nil {
		t.Fatalf("the Nix-provided official bw CLI is required: %v", err)
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("the Nix-provided Docker CLI is required: %v", err)
	}
	images, err := resolveE2EImages(*e2eImageDirectory, *e2eProductImageTag)
	if err != nil {
		t.Fatalf("invalid release E2E image inputs: %v", err)
	}

	testRoot := t.TempDir()
	if err := os.Chmod(testRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	deploymentRoot := filepath.Join(testRoot, "deployment")
	if err := os.Mkdir(deploymentRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := privatefs.ValidatePrivateDirectory(deploymentRoot); err != nil {
		t.Fatalf("validate E2E deployment root: %v", err)
	}
	dockerConfig := filepath.Join(testRoot, "docker-config")
	if err := os.Mkdir(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(dockerConfig, "config.json"), "{\"auths\":{}}\n")

	harness := &e2eHarness{
		t:               t,
		ctx:             ctx,
		repository:      repositoryRoot(t),
		testRoot:        testRoot,
		deploymentRoot:  deploymentRoot,
		project:         fmt.Sprintf("vwless-e2e-%d-%d", os.Getpid(), time.Now().UnixNano()),
		dockerPath:      dockerPath,
		dockerConfig:    dockerConfig,
		dockerEnv:       isolatedDockerEnvironment(dockerConfig),
		images:          images,
		vaultwardenPort: unusedLoopbackPort(t),
		proxyPort:       unusedLoopbackPort(t),
		sourcePort:      unusedLoopbackPort(t),
		primaryPort:     unusedLoopbackPort(t),
		b2Port:          unusedLoopbackPort(t),
	}
	harness.composeFile = filepath.Join(harness.repository, "compose.yml")
	harness.overrideFile = filepath.Join(testRoot, "compose.e2e.yml")
	harness.environmentFile = filepath.Join(deploymentRoot, "compose.env")
	t.Cleanup(harness.cleanup)

	if output, err := harness.docker("info"); err != nil {
		t.Fatalf("Docker daemon is required: %v: %s", err, harnessDiagnostics(output))
	}
	// Load only the test-tooling image needed to execute the canary. Prove the
	// filesystem boundary before loading any production image.
	harness.loadImages("moto")
	harness.requireSharedBindPath()
	harness.loadImages("backup", "proxy", "vaultwarden")
	harness.validateProductImages()
	harness.startTLSProxy()
	harness.prepareDeployment(targets)
	harness.writeOverride()
	harness.validateComposeModel()
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
			if _, err := os.Stat(filepath.Join(current, "flake.nix")); err == nil {
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

func resolveE2EImages(directory, productTag string) (e2eImageSet, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return e2eImageSet{}, fmt.Errorf("image directory must be a clean absolute path")
	}
	info, err := os.Stat(directory)
	if err != nil {
		return e2eImageSet{}, fmt.Errorf("stat image directory: %w", err)
	}
	if !info.IsDir() {
		return e2eImageSet{}, fmt.Errorf("image directory is not a directory")
	}
	if !fullReleaseTagPattern.MatchString(productTag) {
		return e2eImageSet{}, fmt.Errorf("product image tag must be git- followed by one complete lowercase commit")
	}
	images := e2eImageSet{
		directory:  directory,
		productTag: productTag,
		archives:   make(map[string]string, 4),
	}
	for _, name := range []string{"moto", "backup", "proxy", "vaultwarden"} {
		archive := filepath.Join(directory, name+".tar.gz")
		archiveInfo, statErr := os.Stat(archive)
		if statErr != nil {
			return e2eImageSet{}, fmt.Errorf("stat %s image archive: %w", name, statErr)
		}
		if !archiveInfo.Mode().IsRegular() {
			return e2eImageSet{}, fmt.Errorf("%s image archive is not a regular file", name)
		}
		images.archives[name] = archive
	}
	return images, nil
}

func (h *e2eHarness) loadImages(names ...string) {
	h.t.Helper()
	for _, name := range names {
		archive, exists := h.images.archives[name]
		if !exists {
			h.t.Fatalf("unknown E2E image %q", name)
		}
		if loaded, err := h.docker("image", "load", "--input", archive); err != nil {
			h.t.Fatalf("load %s image archive: %v: %s", name, err, harnessDiagnostics(loaded))
		}
	}
}

func (h *e2eHarness) validateProductImages() {
	h.t.Helper()
	commit := strings.TrimPrefix(h.images.productTag, "git-")
	for _, name := range []string{"backup", "proxy", "vaultwarden"} {
		reference := "vaultwarden-less-" + name + ":" + h.images.productTag
		format := `{{.Os}}/{{.Architecture}}|{{index .Config.Labels "org.opencontainers.image.version"}}|{{index .Config.Labels "org.opencontainers.image.revision"}}`
		output, err := h.docker("image", "inspect", "--format", format, reference)
		expected := "linux/amd64|" + h.images.productTag + "|" + commit
		if err != nil || strings.TrimSpace(output) != expected {
			h.t.Fatalf("inspect production image %s: got %q/%v, want %q", reference, harnessDiagnostics(output), err, expected)
		}
	}
}

func (h *e2eHarness) requireSharedBindPath() {
	h.t.Helper()
	token := h.project + "-shared-bind"
	canary := filepath.Join(h.testRoot, "bind-canary")
	writePrivateFile(h.t, canary, token)
	name := h.project + "-bind-canary"
	h.t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 30*time.Second)
		defer cancel()
		_, _ = h.dockerContext(cleanupContext, "container", "rm", "--force", name)
	})
	program := `import pathlib,sys; raise SystemExit(0 if pathlib.Path("/canary/token").read_text() == sys.argv[1] else 7)`
	output, err := h.docker(
		"run", "--rm", "--pull", "never", "--name", name,
		"--label", "vaultwarden-less.e2e.project="+h.project,
		"--user", fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
		"--read-only", "--security-opt", "no-new-privileges=true", "--cap-drop", "ALL",
		"--mount", "type=bind,source="+canary+",target=/canary/token,readonly",
		"--entrypoint", "/bin/python3", "vaultwarden-less-moto:local", "-c", program, token,
	)
	if err != nil {
		h.t.Fatalf(
			"Docker bind-path canary failed: the Docker daemon and Go test process must share the same filesystem path; formal E2E cannot use this daemon: %v: %s",
			err,
			harnessDiagnostics(output),
		)
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
				request.SetXForwarded()
				return
			}
			request.SetURL(backend)
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}
	h.tlsServer = httptest.NewTLSServer(reverseProxy)
	h.publicTLS = h.tlsServer.URL
	certificate := h.tlsServer.Certificate()
	if certificate == nil {
		h.t.Fatal("E2E TLS server returned no certificate")
	}
	h.tlsCAFile = filepath.Join(h.testRoot, "tls-ca.pem")
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
}

func (h *e2eHarness) prepareDeployment(targets []string) {
	for _, relative := range []string{"config", "credentials", "credentials/restic", "data", "staging", "cache", "restore"} {
		path := filepath.Join(h.deploymentRoot, relative)
		if err := os.MkdirAll(path, 0o700); err != nil {
			h.t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	for _, relative := range []string{"source-data"} {
		if err := os.Mkdir(filepath.Join(h.testRoot, relative), 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	writePrivateFile(h.t, h.environmentFile, fmt.Sprintf(
		"DEPLOYMENT_ROOT=%s\nVWLESS_IMAGE_TAG=%s\nRUNTIME_UID=%d\nRUNTIME_GID=%d\nLISTEN_PORT=%d\n",
		h.deploymentRoot, h.images.productTag, os.Geteuid(), os.Getegid(), h.proxyPort,
	))
	writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "config", "vaultwarden.env"), "SIGNUPS_ALLOWED=true\nINVITATIONS_ALLOWED=false\n")
	writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "config", "backup.yaml"), backupConfiguration(h.t, targets))
	credentials := roleCredentials(h.t, targets)
	writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "credentials", "backup.yaml"), credentials)
	writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "credentials", "maintenance.yaml"), credentials)
	for _, target := range targets {
		writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "credentials", "restic", target), "e2e-"+target+"-restic-password\n")
	}
}

func backupConfiguration(t *testing.T, targets []string) string {
	t.Helper()
	if len(targets) == 0 {
		return "targets: []\nnotifiers: []\n"
	}
	if !slices.Equal(targets, []string{"primary", "b2"}) {
		t.Fatalf("unsupported E2E target set: %v", targets)
	}
	return `targets:
  - name: primary
    repository: s3:http://moto-primary:5000/vaultwarden-less-e2e-primary/repository
    options:
      - name: s3.bucket-lookup
        value: path
      - name: s3.region
        value: us-east-1
  - name: b2
    repository: s3:http://moto-b2:5000/vaultwarden-less-e2e-b2/repository
    options:
      - name: s3.bucket-lookup
        value: path
      - name: s3.region
        value: us-east-1
notifiers: []
`
}

func roleCredentials(t *testing.T, targets []string) string {
	t.Helper()
	if len(targets) == 0 {
		return "targets: {}\n"
	}
	if !slices.Equal(targets, []string{"primary", "b2"}) {
		t.Fatalf("unsupported E2E target set: %v", targets)
	}
	return `targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: e2e-primary-access
      AWS_SECRET_ACCESS_KEY: e2e-primary-secret
  b2:
    environment:
      AWS_ACCESS_KEY_ID: e2e-b2-access
      AWS_SECRET_ACCESS_KEY: e2e-b2-secret
`
}

func (h *e2eHarness) writeOverride() {
	uid, gid := os.Geteuid(), os.Getegid()
	service := func(name, dataPath string, port int) string {
		return fmt.Sprintf(`  %s:
    profiles: [e2e-vaultwarden]
    image: vaultwarden-less-vaultwarden:%s
    pull_policy: never
    platform: linux/amd64
    user: "%d:%d"
    read_only: true
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777
    security_opt: [no-new-privileges:true]
    cap_drop: [ALL]
    environment:
      ROCKET_ADDRESS: "0.0.0.0"
      ROCKET_PORT: "80"
      DATA_FOLDER: /data
      DATABASE_URL: sqlite:///data/db.sqlite3
      RSA_KEY_FILENAME: /data/rsa_key
      ATTACHMENTS_FOLDER: /data/attachments
      SENDS_FOLDER: /data/sends
      WEB_VAULT_ENABLED: "false"
      DISABLE_ICON_DOWNLOAD: "true"
      ICON_CACHE_TTL: "0"
      SIGNUPS_ALLOWED: "false"
      INVITATIONS_ALLOWED: "false"
    ports:
      - "127.0.0.1:%d:80"
    volumes:
      - type: bind
        source: %s
        target: /data
        bind:
          create_host_path: false
    healthcheck:
      test: [CMD, /healthcheck.sh]
      interval: 2s
      timeout: 3s
      retries: 60
`, name, h.images.productTag, uid, gid, port, strconv.Quote(dataPath))
	}
	sourceService := service("source-vaultwarden", filepath.Join(h.testRoot, "source-data"), h.sourcePort)
	sourceService = strings.Replace(sourceService, `SIGNUPS_ALLOWED: "false"`, `SIGNUPS_ALLOWED: "true"`, 1)
	document := fmt.Sprintf(`services:
  vaultwarden:
    ports:
      - "127.0.0.1:%d:80"
  moto-primary:
    profiles: [e2e-repositories]
    image: vaultwarden-less-moto:local
    pull_policy: never
    platform: linux/amd64
    user: "%d:%d"
    read_only: true
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777
    security_opt: [no-new-privileges:true]
    cap_drop: [ALL]
  moto-b2:
    profiles: [e2e-repositories]
    image: vaultwarden-less-moto:local
    pull_policy: never
    platform: linux/amd64
    user: "%d:%d"
    read_only: true
    tmpfs:
      - /tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777
    security_opt: [no-new-privileges:true]
    cap_drop: [ALL]
%s%s%s
`, h.vaultwardenPort, uid, gid, uid, gid, sourceService,
		service("restored-primary", filepath.Join(h.deploymentRoot, "restore", "primary-copy"), h.primaryPort),
		service("restored-b2", filepath.Join(h.deploymentRoot, "restore", "b2-copy"), h.b2Port),
	)
	writePrivateFile(h.t, h.overrideFile, document)
}

func (h *e2eHarness) validateComposeModel() {
	output := h.compose(
		"--profile", "maintenance", "--profile", "e2e-repositories", "--profile", "e2e-vaultwarden",
		"config", "--format", "json",
	)
	var model struct {
		Services map[string]struct {
			User     string         `json:"user"`
			ReadOnly bool           `json:"read_only"`
			Networks map[string]any `json:"networks"`
			Ports    []struct {
				HostIP    string `json:"host_ip"`
				Target    int    `json:"target"`
				Published string `json:"published"`
			} `json:"ports"`
			Volumes []struct {
				Type string `json:"type"`
				Bind struct {
					CreateHostPath bool `json:"create_host_path"`
				} `json:"bind"`
			} `json:"volumes"`
		} `json:"services"`
		Networks map[string]struct {
			Internal bool `json:"internal"`
		} `json:"networks"`
	}
	if err := json.Unmarshal([]byte(output), &model); err != nil {
		h.t.Fatalf("decode Compose model: %v: %s", err, harnessDiagnostics(output))
	}
	for name, service := range model.Services {
		if len(service.Networks) != 1 {
			h.t.Fatalf("service %s does not use exactly one network: %#v", name, service.Networks)
		}
		if _, exists := service.Networks["default"]; !exists {
			h.t.Fatalf("service %s does not use the default network: %#v", name, service.Networks)
		}
	}
	for _, name := range []string{"vaultwarden", "backup", "proxy", "maintenance"} {
		service, exists := model.Services[name]
		if !exists || service.User != fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()) || !service.ReadOnly {
			h.t.Fatalf("production service %s lost its runtime boundary: %#v", name, service)
		}
		for _, volume := range service.Volumes {
			if volume.Type != "bind" || volume.Bind.CreateHostPath {
				h.t.Fatalf("production service %s has an unsafe mount: %#v", name, volume)
			}
		}
	}
	if len(model.Networks) != 1 {
		h.t.Fatalf("Compose model does not have exactly one network: %#v", model.Networks)
	}
	if network, exists := model.Networks["default"]; !exists || network.Internal {
		h.t.Fatalf("default network is missing or internal: %#v", model.Networks)
	}
	proxy := model.Services["proxy"]
	if len(proxy.Ports) != 1 || proxy.Ports[0].HostIP != "127.0.0.1" ||
		proxy.Ports[0].Target != 8080 || proxy.Ports[0].Published != strconv.Itoa(h.proxyPort) {
		h.t.Fatalf("proxy lost its exact loopback publication: %#v", proxy.Ports)
	}
}

func (h *e2eHarness) validateConfiguration(expectedTargets []string) {
	for _, test := range []struct {
		service string
		role    string
		profile []string
	}{
		{service: "backup", role: "backup"},
		{service: "maintenance", role: "maintenance", profile: []string{"--profile", "maintenance"}},
	} {
		arguments := append([]string(nil), test.profile...)
		arguments = append(arguments, "run", "--rm", "--no-deps", test.service, "config", "validate", "--role", test.role)
		output := h.compose(arguments...)
		var result struct {
			Valid   bool     `json:"valid"`
			Role    string   `json:"role"`
			Targets []string `json:"targets"`
		}
		if err := decodeExactJSON(output, &result); err != nil || !result.Valid || result.Role != test.role ||
			result.Targets == nil || !slices.Equal(result.Targets, expectedTargets) {
			h.t.Fatalf("%s config validation failed: %#v/%v: %s", test.role, result, err, harnessDiagnostics(output))
		}
	}
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
		"compose", "--env-file", h.environmentFile,
		"--project-directory", h.testRoot,
		"--project-name", h.project,
		"--file", h.composeFile,
		"--file", h.overrideFile,
	}
	return h.dockerContext(ctx, append(base, arguments...)...)
}

func (h *e2eHarness) startVaultwarden() {
	h.compose("up", "--detach", "vaultwarden")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	h.setBackend(loopbackEndpoint(h.vaultwardenPort))
	h.waitHTTP(h.publicTLS+"/alive", http.StatusOK, time.Minute)
}

func (h *e2eHarness) startProduction() {
	h.compose("up", "--detach", "vaultwarden", "backup", "proxy")
	for _, service := range []string{"vaultwarden", "backup", "proxy"} {
		h.waitHealthy(service, 3*time.Minute)
	}
	h.validateProxyBinding()
	h.setBackend(loopbackEndpoint(h.proxyPort))
	h.waitHTTP(h.publicTLS+"/.vaultwarden-less/healthz", http.StatusNoContent, time.Minute)
}

func (h *e2eHarness) validateProxyBinding() {
	h.t.Helper()
	address := strings.TrimSpace(h.compose("port", "proxy", "8080"))
	expected := net.JoinHostPort("127.0.0.1", strconv.Itoa(h.proxyPort))
	if address != expected {
		h.t.Fatalf("proxy published address = %q, want %q", address, expected)
	}
}

func (h *e2eHarness) startSource() {
	h.compose("--profile", "e2e-vaultwarden", "up", "--detach", "source-vaultwarden")
	h.waitHealthy("source-vaultwarden", 3*time.Minute)
	h.setBackend(loopbackEndpoint(h.sourcePort))
	h.waitHTTP(h.publicTLS+"/alive", http.StatusOK, time.Minute)
}

func (h *e2eHarness) startRestored(service string, port int) {
	h.compose("--profile", "e2e-vaultwarden", "up", "--detach", service)
	h.waitHealthy(service, 3*time.Minute)
	h.setBackend(loopbackEndpoint(port))
	h.waitHTTP(h.publicTLS+"/alive", http.StatusOK, time.Minute)
}

func (h *e2eHarness) stopService(service string) {
	h.compose("stop", service)
}

func (h *e2eHarness) containerID(service string) string {
	identifier := strings.TrimSpace(h.compose("ps", "--all", "--quiet", service))
	if !lowerHex64Pattern.MatchString(identifier) {
		h.t.Fatalf("invalid container ID for %s: %q", service, identifier)
	}
	return identifier
}

func (h *e2eHarness) waitHealthy(service string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		identifier, err := h.tryCompose(h.ctx, "ps", "--all", "--quiet", service)
		if err == nil && strings.TrimSpace(identifier) != "" {
			output, inspectErr := h.docker("inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", strings.TrimSpace(identifier))
			last = output
			fields := strings.Fields(output)
			if inspectErr == nil && len(fields) >= 2 && fields[0] == "running" && fields[1] == "healthy" {
				return
			}
			if inspectErr == nil && len(fields) > 0 && fields[0] == "exited" {
				break
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	logs, _ := h.dockerLogs("--tail", "100", h.containerID(service))
	h.t.Fatalf("service %s did not become healthy: state=%s logs=%s", service, harnessDiagnostics(last), harnessDiagnostics(logs))
}

func (h *e2eHarness) waitHTTP(endpoint string, status int, timeout time.Duration) {
	client := h.tlsServer.Client()
	client.Timeout = 3 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
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

func (h *e2eHarness) cleanup() {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 3*time.Minute)
	defer cancel()
	if h.overrideFile != "" {
		_, _ = h.tryCompose(
			cleanupContext,
			"--profile", "maintenance", "--profile", "e2e-repositories", "--profile", "e2e-vaultwarden",
			"down", "--remove-orphans",
		)
	}
	if h.tlsServer != nil {
		h.tlsServer.Close()
	}
	if h.project == "" {
		return
	}
	containers, _ := h.dockerContext(cleanupContext, "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+h.project)
	networks, _ := h.dockerContext(cleanupContext, "network", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+h.project)
	volumes, _ := h.dockerContext(cleanupContext, "volume", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+h.project)
	custom, _ := h.dockerContext(cleanupContext, "ps", "--all", "--quiet", "--filter", "label=vaultwarden-less.e2e.project="+h.project)
	if strings.TrimSpace(containers) != "" || strings.TrimSpace(networks) != "" || strings.TrimSpace(volumes) != "" || strings.TrimSpace(custom) != "" {
		h.t.Errorf("E2E cleanup left scenario containers, networks, or volumes")
	}
}

func (h *e2eHarness) docker(arguments ...string) (string, error) {
	return h.dockerContext(h.ctx, arguments...)
}

func (h *e2eHarness) dockerContext(ctx context.Context, arguments ...string) (string, error) {
	arguments = append([]string{"--config", h.dockerConfig}, arguments...)
	return runCommand(ctx, h.repository, nil, h.dockerEnv, h.dockerPath, arguments...)
}

func (h *e2eHarness) dockerLogs(arguments ...string) (string, error) {
	arguments = append([]string{"--config", h.dockerConfig, "logs"}, arguments...)
	return runCommandCombined(h.ctx, h.repository, h.dockerEnv, h.dockerPath, arguments...)
}

func runCommand(
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
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err != nil {
		return strings.TrimSpace(stdout.String() + "\n" + stderr.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func runCommandCombined(ctx context.Context, directory string, environment []string, binary string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = directory
	command.Env = environment
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func isolatedDockerEnvironment(configDirectory string) []string {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DOCKER_CONFIG=") {
			environment = append(environment, entry)
		}
	}
	return append(environment, "DOCKER_CONFIG="+configDirectory)
}

func unusedLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if port < 1024 || port > 65535 {
		t.Fatalf("kernel selected invalid E2E port %d", port)
	}
	return port
}

func loopbackEndpoint(port int) string {
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
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

func harnessDiagnostics(value string) string {
	return redactDiagnostics(
		value,
		testPassword,
		initialItemPassword,
		"e2e-primary-secret",
		"e2e-b2-secret",
		"e2e-primary-restic-password",
		"e2e-b2-restic-password",
		"BW_SESSION",
	)
}

func exactCommandErrorCode(t *testing.T, output string) string {
	t.Helper()
	type envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	codes := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var document envelope
		if decodeExactJSON(line, &document) == nil && document.Error.Code != "" && document.Error.Message != "" {
			codes = append(codes, document.Error.Code)
		}
	}
	if len(codes) != 1 {
		t.Fatalf("failure did not contain exactly one command error: %s", harnessDiagnostics(output))
	}
	return codes[0]
}
