package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"maps"
	"math/big"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	productcli "github.com/hellodword/vaultwarden-less/internal/cli"
	productworker "github.com/hellodword/vaultwarden-less/internal/worker"
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
	lowerHex40Pattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	contentIDPattern  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type e2eHarness struct {
	t                 *testing.T
	ctx               context.Context
	repository        string
	localRoot         string
	project           string
	publishedHost     string
	proxyHostPort     int
	drillHostPort     int
	dockerConfig      string
	dockerPath        string
	dockerEnv         []string
	productionCompose string
	testCompose       string
	targets           []string
	publicHTTP        string
	publicTLS         string
	tlsCAFile         string
	tlsServer         *httptest.Server
	notifierCAFile    string
	notifierWebhook   string
	notifierServer    *httptest.Server

	backendMu sync.RWMutex
	backend   *url.URL

	notificationMu sync.Mutex
	notifications  []string
}

var (
	runBackupE2E      = flag.Bool("run-backup-e2e", false, "run the real Vaultwarden backup/restore lifecycle")
	runMultiTargetE2E = flag.Bool("run-multitarget-e2e", false, "run the real multi-target degradation lifecycle")
	proxyPort         = flag.Int("proxy-host-port", 0, "fixed proxy host port; zero asks Docker for an ephemeral port")
	drillPort         = flag.Int("drill-host-port", 0, "fixed drill host port; zero asks Docker for an ephemeral port")
	preloadedImages   = flag.Bool("preloaded-images", false, "use already loaded local test images")
)

func newE2EHarness(t *testing.T, ctx context.Context, targets ...string) *e2eHarness {
	t.Helper()
	if len(targets) == 0 {
		targets = []string{"primary"}
	}
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
	if _, err := exec.LookPath("bw"); err != nil {
		t.Fatalf("the Nix-provided official bw CLI is required: %v", err)
	}
	validateOptionalPort(t, "-proxy-host-port", *proxyPort)
	validateOptionalPort(t, "-drill-host-port", *drillPort)
	if *proxyPort != 0 && *proxyPort == *drillPort {
		t.Fatal("explicit proxy and drill ports must differ")
	}
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
		publishedHost: "127.0.0.1",
		proxyHostPort: *proxyPort,
		drillHostPort: *drillPort,
		dockerConfig:  dockerConfig,
		dockerPath:    dockerPath,
		dockerEnv:     isolatedDockerEnvironment(dockerConfig),
		targets:       slices.Clone(targets),
	}
	harness.productionCompose = filepath.Join(repository, "compose.yml")
	harness.testCompose = filepath.Join(repository, "tests", "e2e", "compose.override.yml")
	t.Cleanup(harness.cleanup)
	if output, err := harness.docker("info"); err != nil {
		t.Fatalf("Docker daemon is required: %v: %s", err, truncateDiagnostic(output, 2000))
	}

	harness.startTLSProxy()
	harness.startNotifierReceiver()
	harness.prepareDeployment()
	harness.validateComposeModel()
	if !*preloadedImages {
		harness.prepareImages()
	}
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

func (h *e2eHarness) prepareImages() {
	for _, attribute := range []string{"backup-image", "proxy-image", "vaultwarden-image", "moto-image"} {
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

func (h *e2eHarness) startNotifierReceiver() {
	h.t.Helper()
	certificatePEM, keyPEM := testServerCertificate(h.t, "e2e-host")
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		h.t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		h.t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		body, readError := io.ReadAll(io.LimitReader(request.Body, 64<<10+1))
		if readError != nil || len(body) > 64<<10 {
			http.Error(writer, "invalid body", http.StatusBadRequest)
			return
		}
		var payload struct {
			MessageType string `json:"msgtype"`
			Text        struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if decodeExactJSON(string(body), &payload) != nil || payload.MessageType != "text" || payload.Text.Content == "" {
			http.Error(writer, "invalid payload", http.StatusBadRequest)
			return
		}
		h.notificationMu.Lock()
		h.notifications = append(h.notifications, payload.Text.Content)
		h.notificationMu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, "{\"errcode\":0}\n")
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	h.notifierServer = server
	h.notifierCAFile = filepath.Join(h.localRoot, "notifier-ca.pem")
	writePrivateFile(h.t, h.notifierCAFile, string(certificatePEM))
	port := listener.Addr().(*net.TCPAddr).Port
	h.notifierWebhook = "https://e2e-host:" + strconv.Itoa(port) + "/wechat"
}

func testServerCertificate(t *testing.T, dnsName string) ([]byte, []byte) {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(2 * time.Hour),
		DNSNames:              []string{dnsName},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if certificatePEM == nil || keyPEM == nil {
		t.Fatal("encode notifier TLS certificate")
	}
	return certificatePEM, keyPEM
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

func (h *e2eHarness) prepareDeployment() {
	h.t.Helper()
	if os.Geteuid() == 0 || os.Getegid() == 0 {
		h.t.Fatal("E2E harness must run as an unprivileged identity")
	}
	for _, relative := range []string{
		"config", "credentials", "credentials/restic", "cache", "data", "restore", "staging", "state",
	} {
		path := filepath.Join(h.localRoot, relative)
		if err := os.MkdirAll(path, 0o700); err != nil {
			h.t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}

	deployment := readTrackedExample(h.t, filepath.Join(h.repository, ".env.example"))
	deployment = strings.Replace(deployment, "RUNTIME_UID=1000", "RUNTIME_UID="+strconv.Itoa(os.Geteuid()), 1)
	deployment = strings.Replace(deployment, "RUNTIME_GID=1000", "RUNTIME_GID="+strconv.Itoa(os.Getegid()), 1)
	deployment = strings.Replace(deployment, "LISTEN_PORT=8080", "LISTEN_PORT=18080", 1)
	deployment = strings.Replace(deployment, "DRILL_PORT=8081", "DRILL_PORT=18081", 1)
	writePrivateFile(h.t, filepath.Join(h.localRoot, ".env"), deployment)

	vaultwarden := readTrackedExample(h.t, filepath.Join(h.repository, "config", "vaultwarden.example.env"))
	vaultwarden = strings.Replace(vaultwarden, "DOMAIN=https://vaultwarden.example.test", "DOMAIN="+h.publicTLS, 1)
	vaultwarden = strings.Replace(vaultwarden, "SIGNUPS_ALLOWED=false", "SIGNUPS_ALLOWED=true", 1)
	vaultwarden = strings.Replace(vaultwarden, "INVITATIONS_ALLOWED=false", "INVITATIONS_ALLOWED=true", 1)
	writePrivateFile(h.t, filepath.Join(h.localRoot, "config", "vaultwarden.env"), vaultwarden)

	backupExample := readTrackedExample(h.t, filepath.Join(h.repository, "config", "backup.example.yaml"))
	writePrivateFile(h.t, filepath.Join(h.localRoot, "config", "backup.yaml"), renderBackupExample(h.t, backupExample, h.targets))
	for _, role := range []string{"backup", "maintenance"} {
		example := readTrackedExample(h.t, filepath.Join(h.repository, "credentials", role+".example.yaml"))
		writePrivateFile(h.t, filepath.Join(h.localRoot, "credentials", role+".yaml"), renderRoleExample(h.t, example, h.targets))
	}
	notifierExample := readTrackedExample(h.t, filepath.Join(h.repository, "credentials", "notifiers.example.yaml"))
	if !strings.Contains(notifierExample, "schema_version: 1") {
		h.t.Fatal("notifier example is not schema 1")
	}
	writePrivateFile(
		h.t,
		filepath.Join(h.localRoot, "credentials", "notifiers.yaml"),
		"schema_version: 1\nnotifiers:\n  e2e:\n    webhook_url: "+h.notifierWebhook+"\n",
	)
	passwordExample := strings.TrimSuffix(readTrackedExample(h.t, filepath.Join(h.repository, "credentials", "restic", "primary.example")), "\n")
	if passwordExample != "replace-me" {
		h.t.Fatal("restic password example changed unexpectedly")
	}
	for _, target := range h.targets {
		writePrivateFile(h.t, filepath.Join(h.localRoot, "credentials", "restic", target), "e2e-"+target+"-restic-password\n")
	}

	h.dockerEnv = append(
		h.dockerEnv,
		"E2E_PROXY_PORT="+composePublishedPort(h.proxyHostPort),
		"E2E_DRILL_PORT="+composePublishedPort(h.drillHostPort),
		"E2E_SOURCE_PORT=",
		"E2E_DOMAIN="+h.publicTLS,
		"E2E_DRILL_SOURCE="+filepath.Join(h.localRoot, "restore", "drill"),
		"E2E_NOTIFIER_CA="+h.notifierCAFile,
	)
}

func (h *e2eHarness) validateComposeModel() {
	h.t.Helper()
	output := h.compose("--profile", "drill", "config", "--format", "json")
	var model struct {
		Services map[string]struct {
			User        string            `json:"user"`
			Environment map[string]string `json:"environment"`
			CapAdd      []string          `json:"cap_add"`
			Ports       []struct {
				HostIP    string `json:"host_ip"`
				Target    int    `json:"target"`
				Published string `json:"published"`
			} `json:"ports"`
			Volumes []struct {
				Type     string `json:"type"`
				Source   string `json:"source"`
				Target   string `json:"target"`
				ReadOnly bool   `json:"read_only"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(output), &model); err != nil {
		h.t.Fatalf("decode Compose model: %v: %s", err, harnessDiagnostics(output))
	}
	for _, expected := range []struct {
		service   string
		target    int
		published int
	}{
		{"vaultwarden", 80, 0},
		{"proxy", 8080, h.proxyHostPort},
		{"drill-vaultwarden", 80, h.drillHostPort},
	} {
		ports := model.Services[expected.service].Ports
		if len(ports) != 1 || ports[0].HostIP != "127.0.0.1" || ports[0].Target != expected.target ||
			ports[0].Published != composePublishedPort(expected.published) {
			h.t.Fatalf("Compose model has invalid %s test port: %#v", expected.service, ports)
		}
	}
	deploymentMount := false
	for _, volume := range model.Services["backup"].Volumes {
		if volume.Type == "bind" && filepath.Clean(volume.Source) == filepath.Join(h.localRoot, ".env") &&
			volume.Target == "/deployment/.env" && volume.ReadOnly {
			deploymentMount = true
			break
		}
	}
	if !deploymentMount {
		h.t.Fatal("Compose model does not mount the deployment environment read-only for backup serve")
	}
	drillCopy := model.Services["drill-copy"]
	if drillCopy.User != "0:0" ||
		!slices.Equal(drillCopy.CapAdd, []string{"CHOWN", "DAC_OVERRIDE", "FOWNER"}) ||
		!maps.Equal(drillCopy.Environment, map[string]string{
			"RUNTIME_UID": strconv.Itoa(os.Geteuid()),
			"RUNTIME_GID": strconv.Itoa(os.Getegid()),
		}) {
		h.t.Fatalf("Compose model has invalid root-only drill copy identity: user=%q caps=%v environment=%v", drillCopy.User, drillCopy.CapAdd, drillCopy.Environment)
	}
}

func readTrackedExample(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("tracked example %s is not newline terminated", filepath.Base(path))
	}
	return string(data)
}

func renderBackupExample(t *testing.T, example string, targets []string) string {
	t.Helper()
	if !slices.Equal(targets, []string{"primary"}) && !slices.Equal(targets, []string{"primary", "secondary"}) {
		t.Fatalf("unsupported E2E targets: %v", targets)
	}
	result := strings.Replace(example, "s3:https://objects.example.test/bucket/vaultwarden", "s3:http://moto-primary:5000/vaultwarden-less-e2e-primary/repository", 1)
	result = strings.Replace(result, "      - name: s3.bucket-lookup\n        value: path", "      - name: s3.bucket-lookup\n        value: path\n      - name: s3.region\n        value: us-east-1", 1)
	result = strings.Replace(result, "notifiers:\n  - name: operations\n    type: wechat", "notifiers:\n  - name: e2e\n    type: wechat", 1)
	if len(targets) == 2 {
		result = strings.Replace(result, "upgrade_verification_targets:\n  - primary", "upgrade_verification_targets:\n  - primary\n  - secondary", 1)
		secondary := "  - name: secondary\n    repository: s3:http://moto-secondary:5000/vaultwarden-less-e2e-secondary/repository\n    options:\n      - name: s3.bucket-lookup\n        value: path\n      - name: s3.region\n        value: us-east-1\n"
		result = strings.Replace(result, "notifiers:\n  - name: e2e", secondary+"notifiers:\n  - name: e2e", 1)
	}
	if strings.Contains(result, "example.test") || strings.Contains(result, "replace-me") {
		t.Fatal("backup example customization is incomplete")
	}
	return result
}

func renderRoleExample(t *testing.T, example string, targets []string) string {
	t.Helper()
	if !strings.Contains(example, "schema_version: 1") || !strings.Contains(example, "primary:") {
		t.Fatal("role credential example changed unexpectedly")
	}
	result := strings.ReplaceAll(example, "replace-me", "test")
	if len(targets) == 2 {
		result += "  secondary:\n    environment:\n      AWS_ACCESS_KEY_ID: test\n      AWS_SECRET_ACCESS_KEY: test\n"
	}
	return result
}

func validateOptionalPort(t *testing.T, name string, port int) {
	t.Helper()
	if port != 0 && (port < 1024 || port > 65535) {
		t.Fatalf("%s must be zero or an unprivileged TCP port", name)
	}
}

func composePublishedPort(port int) string {
	if port == 0 {
		return ""
	}
	return strconv.Itoa(port)
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
		filepath.Join(h.localRoot, ".env"),
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

func (h *e2eHarness) startVaultwarden() {
	h.compose("up", "--detach", "vaultwarden")
	h.waitHealthy("vaultwarden", 3*time.Minute)
	h.setBackend(h.mappedEndpoint("vaultwarden", 80))
	h.waitHTTP(h.publicHTTP+"/alive", http.StatusOK, time.Minute)
}

func (h *e2eHarness) startProxy() {
	h.compose("up", "--detach", "proxy")
	h.waitHealthy("proxy", 3*time.Minute)
	h.setBackend(h.mappedEndpoint("proxy", 8080))
	h.waitHTTP(h.publicHTTP+"/.vaultwarden-less/healthz", http.StatusNoContent, 2*time.Minute)
}

func (h *e2eHarness) startBackup() {
	h.compose("up", "--detach", "backup")
	h.waitHealthy("backup", 2*time.Minute)
}

func (h *e2eHarness) mappedEndpoint(service string, containerPort int) string {
	container := strings.TrimSpace(h.compose("ps", "--quiet", service))
	if !lowerHex64Pattern.MatchString(container) {
		h.t.Fatalf("invalid container ID for %s: %q", service, container)
	}
	output, err := h.docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", container)
	if err != nil {
		h.t.Fatalf("inspect published port for %s: %v: %s", service, err, harnessDiagnostics(output))
	}
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := decodeExactJSON(output, &ports); err != nil {
		h.t.Fatalf("decode published ports for %s: %v: %s", service, err, harnessDiagnostics(output))
	}
	bindings := ports[strconv.Itoa(containerPort)+"/tcp"]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
		h.t.Fatalf("invalid loopback binding for %s: %s", service, harnessDiagnostics(output))
	}
	port, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil || port < 1 || port > 65535 {
		h.t.Fatalf("invalid published TCP port for %s: %q", service, bindings[0].HostPort)
	}
	return "http://" + net.JoinHostPort(h.publishedHost, strconv.Itoa(port))
}

func (h *e2eHarness) waitHealthy(service string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	var lastContainer string
	var lastState string
	for time.Now().Before(deadline) {
		container, err := h.tryCompose(h.ctx, "ps", "--quiet", service)
		if err == nil && strings.TrimSpace(container) != "" {
			lastContainer = strings.TrimSpace(container)
			output, inspectError := h.docker(
				"inspect",
				"--format",
				"{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}",
				lastContainer,
			)
			lastState = output
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
	logs := ""
	if lastContainer != "" {
		logs, _ = h.docker("logs", "--tail", "100", lastContainer)
	}
	h.t.Fatalf(
		"service %s did not become healthy: state=%s logs=%s",
		service,
		harnessDiagnostics(lastState),
		harnessDiagnostics(logs),
	)
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
		ID            string `json:"id"`
		Time          string `json:"time"`
		ProjectCommit string `json:"project_commit"`
		ContentID     string `json:"content_id"`
		ArchiveID     string `json:"archive_id"`
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
	h.repositoryCheckTarget("primary")
}

func (h *e2eHarness) repositoryCheckTarget(target string) {
	output := h.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"repo", "check", "--target", target,
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
		result.Target != target || !result.Success {
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
		result.SchemaVersion != productcli.ResultSchemaVersion ||
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
	if err := decodeExactJSON(output, &status); err != nil ||
		status.SchemaVersion != productworker.SchemaVersion || status.Targets == nil {
		h.t.Fatalf("invalid worker status: %v: %s", err, harnessDiagnostics(output))
	}
	return status
}

func (h *e2eHarness) requestBackup() {
	h.t.Helper()
	output := h.compose("exec", "--no-TTY", "backup", "/bin/vaultwarden-less-backup", "request-backup")
	var result struct {
		SchemaVersion int  `json:"schema_version"`
		Accepted      bool `json:"accepted"`
	}
	if err := decodeExactJSON(output, &result); err != nil ||
		result.SchemaVersion != productworker.SchemaVersion || !result.Accepted {
		h.t.Fatalf("invalid backup request result: %v: %s", err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) waitCheckAfter(previousFinished, result string, timeout time.Duration) productworker.Status {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := h.status()
		if status.LastCheck != nil && status.LastCheck.FinishedAt != previousFinished && status.LastCheck.Result == result {
			return status
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	h.t.Fatalf("worker did not complete a new %s check", result)
	return productworker.Status{}
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

func (h *e2eHarness) singleFileIn(relativeDirectory string) string {
	h.t.Helper()
	relativeDirectory = cleanTestRelative(h.t, relativeDirectory)
	directory := filepath.Join(h.localRoot, filepath.FromSlash(relativeDirectory))
	entries, err := os.ReadDir(directory)
	if err != nil {
		h.t.Fatalf("list file Send data: %v", err)
	}
	if len(entries) != 1 || !lowerHex64Pattern.MatchString(entries[0].Name()) {
		h.t.Fatalf("file Send directory contains unexpected entries: %#v", entries)
	}
	info, err := entries[0].Info()
	if err != nil || !info.Mode().IsRegular() {
		h.t.Fatalf("file Send entry is not a regular file: %v", err)
	}
	return relativeDirectory + "/" + entries[0].Name()
}

func (h *e2eHarness) holdDataFile(relative string) func() {
	h.t.Helper()
	relative = cleanTestRelative(h.t, relative)
	if !strings.HasPrefix(relative, "data/") {
		h.t.Fatalf("refusing to hold a non-data test path %q", relative)
	}
	source := filepath.Join(h.localRoot, filepath.FromSlash(relative))
	held := filepath.Join(h.localRoot, "data", ".e2e-held-"+strings.ReplaceAll(strings.TrimPrefix(relative, "data/"), "/", "-"))
	restored := false
	h.renameDataFile(source, held)
	restore := func() {
		if restored {
			return
		}
		h.renameDataFile(held, source)
		restored = true
	}
	h.t.Cleanup(restore)
	return restore
}

func (h *e2eHarness) renameDataFile(from, to string) {
	h.t.Helper()
	if err := os.Rename(from, to); err != nil {
		h.t.Fatalf("rename E2E data file: %v", err)
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
		result.SchemaVersion != productcli.ResultSchemaVersion ||
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
	volume := h.project + "_drill-data"
	_, _ = h.docker("volume", "rm", volume)
	h.setDockerEnvironment("E2E_DRILL_SOURCE", filepath.Join(h.localRoot, "restore", recoveryName))
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
			"upgrade",
			"--profile",
			"rollback",
			"down",
			"--volumes",
			"--remove-orphans",
		)
	}
	if h.tlsServer != nil {
		h.tlsServer.Close()
	}
	if h.notifierServer != nil {
		h.notifierServer.Close()
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

func (h *e2eHarness) setDockerEnvironment(name, value string) {
	prefix := name + "="
	for index, entry := range h.dockerEnv {
		if strings.HasPrefix(entry, prefix) {
			h.dockerEnv[index] = prefix + value
			return
		}
	}
	h.dockerEnv = append(h.dockerEnv, prefix+value)
}

func (h *e2eHarness) waitNotificationContains(timeout time.Duration, fragments ...string) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h.notificationMu.Lock()
		messages := slices.Clone(h.notifications)
		h.notificationMu.Unlock()
		for _, message := range messages {
			matched := true
			for _, fragment := range fragments {
				if !strings.Contains(message, fragment) {
					matched = false
					break
				}
			}
			if matched {
				return message
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	h.t.Fatalf("notification did not contain %q", fragments)
	return ""
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
	return append(os.Environ(), "DOCKER_CONFIG="+configDirectory)
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
