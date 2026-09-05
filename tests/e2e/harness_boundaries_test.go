package e2e_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestTLSProxyReportsExternalHTTPS(t *testing.T) {
	observed := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		observed <- request.Clone(request.Context())
		response.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)

	harness := &e2eHarness{
		t:        t,
		ctx:      context.Background(),
		testRoot: t.TempDir(),
	}
	harness.setBackend(backend.URL)
	harness.startTLSProxy()
	t.Cleanup(harness.tlsServer.Close)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, harness.publicTLS+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "vaultwarden.test"
	request.Header.Set("X-Forwarded-Proto", "http")
	request.Header.Set("X-Forwarded-Host", "untrusted.example")
	response, err := harness.tlsServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := response.Body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("TLS proxy status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	forwarded := <-observed
	if got := forwarded.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("X-Forwarded-Proto = %q, want https", got)
	}
	if got := forwarded.Header.Get("X-Forwarded-Host"); got != request.Host {
		t.Fatalf("X-Forwarded-Host = %q, want %q", got, request.Host)
	}
	if forwarded.Host != request.Host {
		t.Fatalf("forwarded Host = %q, want %q", forwarded.Host, request.Host)
	}
}

func TestGeneratedOverrideKeepsTmpfsOptionsTogether(t *testing.T) {
	testRoot := t.TempDir()
	harness := &e2eHarness{
		t:               t,
		testRoot:        testRoot,
		deploymentRoot:  filepath.Join(testRoot, "deployment"),
		overrideFile:    filepath.Join(testRoot, "compose.e2e.yml"),
		images:          e2eImageSet{productTag: "git-0123456789abcdef0123456789abcdef01234567"},
		vaultwardenPort: 18080,
		sourcePort:      18081,
		primaryPort:     18082,
		b2Port:          18083,
	}
	harness.writeOverride()

	document, err := os.ReadFile(harness.overrideFile)
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Services map[string]struct {
			Tmpfs []string `yaml:"tmpfs"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(document, &model); err != nil {
		t.Fatalf("decode generated Compose override: %v", err)
	}
	const expected = "/tmp:rw,noexec,nosuid,nodev,size=64m,mode=1777"
	for _, service := range []string{"moto-primary", "moto-b2"} {
		if got := model.Services[service].Tmpfs; len(got) != 1 || got[0] != expected {
			t.Fatalf("%s tmpfs = %#v, want [%q]", service, got, expected)
		}
	}
}
