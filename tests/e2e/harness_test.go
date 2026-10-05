package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
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
	"sync"
	"testing"
	"time"
)

const (
	testPassword        = "Correct Horse Battery Staple 2026!"
	initialItemPassword = "initial-e2e-item-password"
)

var (
	runE2E            = flag.Bool("run-e2e", false, "run Docker release acceptance")
	releaseDirectory  = flag.String("release-dir", "", "absolute deployment bundle directory")
	workDirectory     = flag.String("work-dir", "", "retained work directory shared with the Docker host at the same absolute path")
	dockerSocket      = flag.String("docker-socket", "/var/run/docker.sock", "local Docker socket shared with the host at the same absolute path")
	runnerImage       = flag.String("runner-image", "", "test runner image reference supplied by Nix")
	motoImage         = flag.String("moto-image", "", "Moto image reference supplied by Nix")
	motoArchive       = flag.String("moto-archive", "", "Moto image archive supplied by Nix")
	lowerHex64Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type e2eHarness struct {
	t                                                    *testing.T
	ctx                                                  context.Context
	testRoot, deploymentRoot, project, releaseRoot       string
	dockerPath, dockerConfig                             string
	dockerEnv                                            []string
	composeFile, overrideFile, environmentFile, imageTag string
	vaultwardenPort, proxyPort                           int
	publicTLS, tlsCAFile                                 string
	tlsServer                                            *httptest.Server
	backendMu                                            sync.RWMutex
	backend                                              *url.URL
	services                                             map[string]composeService
	vaultwardenOverrides                                 map[string]string
}

type composeService struct {
	Image       string            `json:"image"`
	Environment map[string]string `json:"environment"`
}

func newE2EHarness(t *testing.T, ctx context.Context) *e2eHarness {
	t.Helper()
	if os.Geteuid() == 0 || os.Getegid() == 0 {
		t.Fatal("acceptance requires a non-root runtime identity")
	}
	for _, path := range []string{*releaseDirectory, *workDirectory, *dockerSocket, *motoArchive} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			t.Fatal("release-dir, work-dir, docker-socket and moto-archive must be clean absolute paths")
		}
	}
	if *runnerImage == "" || *motoImage == "" {
		t.Fatal("runner-image and moto-image must be supplied by the packaged runner")
	}
	root, err := os.MkdirTemp(*workDirectory, "acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = exec.LookPath("bw"); err != nil {
		t.Fatal(err)
	}
	h := &e2eHarness{t: t, ctx: ctx, testRoot: root, deploymentRoot: filepath.Join(root, "deployment"), releaseRoot: *releaseDirectory,
		project: fmt.Sprintf("vwless-%d", time.Now().UnixNano()), dockerPath: dockerPath, dockerConfig: filepath.Join(root, "docker-config"),
		vaultwardenPort: unusedLoopbackPort(t), proxyPort: unusedLoopbackPort(t)}
	t.Logf("retained acceptance artifacts: %s", root)
	for _, relative := range []string{"config", "credentials/backup", "credentials/maintenance", "credentials/restic", "data", "cache", "restore", "maintenance-cache"} {
		if err := os.MkdirAll(filepath.Join(h.deploymentRoot, relative), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(h.dockerConfig, 0700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(h.dockerConfig, "config.json"), `{"auths":{}}`)
	h.dockerEnv = isolatedDockerEnvironment(h.dockerConfig)
	h.composeFile = filepath.Join(h.releaseRoot, "compose.yml")
	h.overrideFile = filepath.Join(root, "compose.e2e.json")
	h.environmentFile = filepath.Join(h.deploymentRoot, "compose.env")
	data, err := os.ReadFile(filepath.Join(h.releaseRoot, "templates", "compose.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VWLESS_IMAGE_TAG=") {
			h.imageTag = strings.TrimPrefix(line, "VWLESS_IMAGE_TAG=")
		}
	}
	if h.imageTag == "" {
		t.Fatal("bundle does not declare its product image tag")
	}
	t.Cleanup(h.cleanup)
	h.mustDocker("info", "--format", "{{.ServerVersion}}")
	h.mustDocker("load", "--input", *motoArchive)
	canary := filepath.Join(root, "bind-canary")
	writePrivateFile(t, canary, h.project)
	h.mustDocker("run", "--rm", "--pull", "never", "--user", h.identity(), "--mount", "type=bind,source="+canary+",target=/canary,readonly", "--entrypoint", "/bin/python3", *motoImage, "-c", `import pathlib,sys; assert pathlib.Path('/canary').read_text()==sys.argv[1]`, h.project)
	for _, name := range []string{"vaultwarden", "backup", "proxy"} {
		h.mustDocker("load", "--input", filepath.Join(h.releaseRoot, "images", name+".tar.gz"))
	}
	h.startTLSProxy()
	h.prepareDeployment()
	h.writeOverride()
	h.services, err = parseComposeServices(h.compose("config", "--format", "json"))
	if err != nil {
		t.Fatal("cannot parse rendered Compose services")
	}
	for _, name := range []string{"vaultwarden", "backup", "proxy"} {
		if h.services[name].Image == "" {
			t.Fatalf("Compose has no image for %s", name)
		}
	}
	return h
}

func parseComposeServices(data string) (map[string]composeService, error) {
	var rendered struct {
		Services map[string]composeService `json:"services"`
	}
	// Compose config doubles every dollar for subsequent Compose interpolation,
	// including in JSON. Undo that serialization escape before docker run --env.
	err := json.Unmarshal([]byte(strings.ReplaceAll(data, "$$", "$")), &rendered)
	return rendered.Services, err
}

func (h *e2eHarness) identity() string { return fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()) }
func (h *e2eHarness) prepareDeployment() {
	writePrivateFile(h.t, h.environmentFile, fmt.Sprintf("DEPLOYMENT_ROOT=%s\nVWLESS_IMAGE_TAG=%s\nRUNTIME_UID=%d\nRUNTIME_GID=%d\nLISTEN_PORT=%d\n", h.deploymentRoot, h.imageTag, os.Geteuid(), os.Getegid(), h.proxyPort))
	data, err := os.ReadFile(filepath.Join(h.releaseRoot, "templates/config/vaultwarden.env"))
	if err != nil {
		h.t.Fatal(err)
	}
	writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "config/vaultwarden.env"), string(data))
	configuration := "targets:\n"
	for _, name := range []string{"primary", "secondary"} {
		configuration += fmt.Sprintf("  - name: %s\n    env_file: /run/credentials/backup/%s.env\n    options: [s3.bucket-lookup=path, s3.region=us-east-1]\n", name, name)
		writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "credentials/restic", name), "e2e-"+name+"-restic-password\n")
		for _, role := range []string{"backup", "maintenance"} {
			writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "credentials", role, name+".env"), fmt.Sprintf("RESTIC_REPOSITORY=s3:http://moto-%s:5000/vaultwarden-less-%s/repository\nRESTIC_PASSWORD_FILE=/run/credentials/restic/%s\nAWS_ACCESS_KEY_ID=e2e-%s-%s\nAWS_SECRET_ACCESS_KEY=e2e-%s-secret\n", name, name, name, name, role, name))
		}
	}
	h.prepareNotificationStub()
	configuration += notificationConfiguration
	writePrivateFile(h.t, filepath.Join(h.deploymentRoot, "config/backup.yaml"), configuration)
}
func (h *e2eHarness) writeOverride() {
	mount := func(source, target string, ro bool) map[string]any {
		return map[string]any{"type": "bind", "source": source, "target": target, "read_only": ro, "bind": map[string]any{"create_host_path": false}}
	}
	vaultwarden := map[string]any{"ports": []string{fmt.Sprintf("127.0.0.1:%d:80", h.vaultwardenPort)}}
	if len(h.vaultwardenOverrides) > 0 {
		vaultwarden["environment"] = h.vaultwardenOverrides
	}
	services := map[string]any{
		"vaultwarden": vaultwarden,
		"backup":      map[string]any{"environment": map[string]string{"SSL_CERT_FILE": "/stub/ca.pem"}, "volumes": []any{mount(filepath.Join(h.testRoot, "stub"), "/stub", true)}},
		"notify-stub": map[string]any{"image": *runnerImage, "pull_policy": "never", "user": h.identity(), "read_only": true, "cap_drop": []string{"ALL"}, "security_opt": []string{"no-new-privileges:true"}, "entrypoint": []string{"/bin/vaultwarden-less-tests"}, "environment": map[string]string{"VWLESS_E2E_STUB": "/stub"}, "volumes": []any{mount(filepath.Join(h.testRoot, "stub"), "/stub", false)}, "networks": map[string]any{"default": map[string]any{"aliases": []string{"api.push.apple.com"}}}},
	}
	for _, name := range []string{"primary", "secondary"} {
		services["moto-"+name] = map[string]any{"image": *motoImage, "pull_policy": "never", "user": h.identity(), "read_only": true, "tmpfs": []string{"/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777"}, "cap_drop": []string{"ALL"}, "security_opt": []string{"no-new-privileges:true"}}
	}
	data, err := json.Marshal(map[string]any{"services": services})
	if err != nil {
		h.t.Fatal(err)
	}
	writePrivateFile(h.t, h.overrideFile, string(data))
}

func (h *e2eHarness) overrideVaultwarden(values map[string]string) {
	// Test-only provisioning and Send coverage must not modify the shipped profile.
	h.vaultwardenOverrides = values
	h.writeOverride()
	h.compose("up", "--detach", "--no-deps", "vaultwarden")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	h.waitHTTP(h.publicTLS+"/alive", http.StatusOK, time.Minute)
}
func (h *e2eHarness) start() {
	h.compose("up", "--detach", "moto-primary", "moto-secondary", "notify-stub", "vaultwarden")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	for _, name := range []string{"primary", "secondary"} {
		h.wait(90*time.Second, "Moto readiness", func() bool {
			_, err := h.docker("exec", h.containerID("moto-"+name), "/bin/python3", "-c", `import boto3; boto3.client('s3',endpoint_url='http://127.0.0.1:5000',aws_access_key_id='test',aws_secret_access_key='test',region_name='us-east-1').create_bucket(Bucket='vaultwarden-less-`+name+`')`)
			return err == nil
		})
		h.nativeRestic(name, nil, "init")
	}
	h.compose("run", "--rm", "--no-deps", "backup", "validate")
	h.compose("up", "--detach", "backup", "proxy")
	for _, name := range []string{"backup", "proxy"} {
		h.waitHealthy(name, 3*time.Minute)
	}
	h.setBackend(loopbackEndpoint(h.proxyPort))
	h.waitHTTP(h.publicTLS+"/alive", http.StatusOK, time.Minute)
	h.validateBoundaries()
}
func (h *e2eHarness) validateBoundaries() {
	for _, name := range []string{"vaultwarden", "backup", "proxy"} {
		var inspect []struct {
			Config struct {
				User string
				Env  []string
			}
			Mounts []struct {
				Source, Destination string
				RW                  bool
			}
		}
		output := h.mustDocker("inspect", h.containerID(name))
		if json.Unmarshal([]byte(output), &inspect) != nil || len(inspect) != 1 {
			h.t.Fatal("invalid Docker inspection")
		}
		item := inspect[0]
		if item.Config.User != h.identity() {
			h.t.Fatal("product runtime is not the test user")
		}
		if name == "vaultwarden" {
			values := map[string]string{}
			for _, entry := range item.Config.Env {
				key, value, _ := strings.Cut(entry, "=")
				values[key] = value
			}
			for key, want := range map[string]string{
				"WEB_VAULT_ENABLED": "false", "ADMIN_TOKEN": "", "DISABLE_ADMIN_TOKEN": "false",
				"SIGNUPS_ALLOWED": "false", "SIGNUPS_DOMAINS_WHITELIST": "", "INVITATIONS_ALLOWED": "false",
				"DISABLE_ICON_DOWNLOAD": "true", "HTTP_REQUEST_BLOCK_NON_GLOBAL_IPS": "true",
				"PASSWORD_HINTS_ALLOWED": "false", "SHOW_PASSWORD_HINT": "false",
				"SENDS_ALLOWED": "false", "EMERGENCY_ACCESS_ALLOWED": "false", "ORG_CREATION_USERS": "none",
			} {
				if got, exists := values[key]; !exists || got != want {
					h.t.Fatalf("shipped Vaultwarden profile: %s=%q, want %q", key, got, want)
				}
			}
		}
		if name == "backup" {
			found := false
			for _, m := range item.Mounts {
				if m.Destination == "/data" {
					found = true
					if m.RW {
						h.t.Fatal("backup source mount is writable")
					}
				}
				if strings.Contains(m.Source, "maintenance") {
					h.t.Fatal("backup received maintenance credentials")
				}
			}
			if !found {
				h.t.Fatal("backup source mount missing")
			}
		}
		if name == "proxy" {
			for _, m := range item.Mounts {
				if strings.Contains(m.Source, "credentials") || m.Destination == "/data" {
					h.t.Fatal("proxy received source data or credentials")
				}
			}
			for _, v := range item.Config.Env {
				if strings.HasPrefix(v, "RESTIC_") || strings.HasPrefix(v, "AWS_") {
					h.t.Fatal("proxy received repository environment")
				}
			}
		}
	}
}
func (h *e2eHarness) compose(arguments ...string) string {
	h.t.Helper()
	output, err := h.tryCompose(h.ctx, arguments...)
	if err != nil {
		h.t.Fatalf("compose %v: %v: %s", arguments, err, harnessDiagnostics(output))
	}
	return output
}
func (h *e2eHarness) tryCompose(ctx context.Context, arguments ...string) (string, error) {
	base := []string{"compose", "--env-file", h.environmentFile, "--project-directory", h.testRoot, "--project-name", h.project, "--file", h.composeFile, "--file", h.overrideFile}
	return h.dockerContext(ctx, append(base, arguments...)...)
}
func (h *e2eHarness) containerID(service string) string {
	h.t.Helper()
	id := strings.TrimSpace(h.compose("ps", "--all", "--quiet", service))
	if !lowerHex64Pattern.MatchString(id) {
		h.t.Fatalf("missing container for %s", service)
	}
	return id
}
func (h *e2eHarness) docker(arguments ...string) (string, error) {
	return h.dockerContext(h.ctx, arguments...)
}
func (h *e2eHarness) cleanupContainer(name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 30*time.Second)
	defer cancel()
	output, err := h.dockerContext(ctx, "rm", "--force", name)
	if err != nil && !strings.Contains(output, "No such container") {
		h.t.Errorf("cleanup container: %v: %s", err, harnessDiagnostics(output))
	}
}
func (h *e2eHarness) dockerContext(ctx context.Context, arguments ...string) (string, error) {
	return runCommand(ctx, h.testRoot, nil, h.dockerEnv, h.dockerPath, append([]string{"--config", h.dockerConfig, "--host", "unix://" + *dockerSocket}, arguments...)...)
}
func (h *e2eHarness) mustDocker(arguments ...string) string {
	h.t.Helper()
	output, err := h.docker(arguments...)
	if err != nil {
		h.t.Fatalf("docker operation failed: %v: %s", err, harnessDiagnostics(output))
	}
	return output
}
func (h *e2eHarness) wait(timeout time.Duration, description string, ready func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	h.t.Fatalf("timed out: %s", description)
}
func (h *e2eHarness) waitHealthy(service string, timeout time.Duration) {
	h.wait(timeout, service+" healthy", func() bool {
		output, err := h.docker("inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", h.containerID(service))
		return err == nil && strings.TrimSpace(output) == "running healthy"
	})
}
func (h *e2eHarness) waitHTTP(endpoint string, status int, timeout time.Duration) {
	client := h.tlsServer.Client()
	client.Timeout = 3 * time.Second
	h.wait(timeout, "HTTP readiness", func() bool {
		r, e := client.Get(endpoint)
		if e != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == status
	})
}
func (h *e2eHarness) cleanup() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(h.ctx), 3*time.Minute)
	defer cancel()
	if h.tlsServer != nil {
		h.tlsServer.Close()
	}
	if _, err := os.Stat(h.overrideFile); err == nil {
		for _, service := range []string{"vaultwarden", "proxy", "backup", "moto-primary", "moto-secondary", "notify-stub"} {
			output, _ := h.tryCompose(ctx, "logs", "--no-color", "--tail", "60", service)
			writePrivateFile(h.t, filepath.Join(h.testRoot, service+".log"), harnessDiagnostics(output))
		}
		if output, err := h.tryCompose(ctx, "down", "--remove-orphans"); err != nil {
			h.t.Errorf("cleanup: %v: %s", err, harnessDiagnostics(output))
		}
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
		if !strings.HasPrefix(entry, "DOCKER_") && !strings.HasPrefix(entry, "COMPOSE_") {
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

func harnessDiagnostics(value string) string {
	return redactDiagnostics(
		value,
		testPassword,
		initialItemPassword,
		"e2e-primary-secret",
		"e2e-secondary-secret",
		"e2e-primary-restic-password",
		"e2e-secondary-restic-password",
		"BW_SESSION",
	)
}
