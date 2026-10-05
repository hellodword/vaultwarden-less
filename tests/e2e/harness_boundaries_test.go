package e2e_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDockerEnvironmentIgnoresAmbientDaemonAndComposeSettings(t *testing.T) {
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CONFIG", "DOCKER_AUTH_CONFIG", "COMPOSE_FILE", "COMPOSE_ENV_FILES"} {
		t.Setenv(key, "private-unrelated-setting")
	}
	env := isolatedDockerEnvironment("/test/private docker config")
	configCount := 0
	for _, entry := range env {
		if entry == "DOCKER_CONFIG=/test/private docker config" {
			configCount++
		} else if strings.HasPrefix(entry, "DOCKER_") || strings.HasPrefix(entry, "COMPOSE_") {
			t.Fatal("Docker inherited an ambient daemon or Compose setting")
		}
	}
	if configCount != 1 {
		t.Fatal("Docker private config missing or duplicated")
	}
}

func TestComposeServicesPreserveResolvedImageAndLiteralEnvironment(t *testing.T) {
	services, err := parseComposeServices(`{"services":{"backup":{"image":"custom-backup:release"},"vaultwarden":{"image":"custom-vaultwarden:release","environment":{"ADMIN_TOKEN":"$$argon2id$$v=19$$synthetic","LITERAL":"$$HOME $$$$ 'quoted'","EMPTY":""}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if services["backup"].Image != "custom-backup:release" || services["vaultwarden"].Image != "custom-vaultwarden:release" {
		t.Fatal("product image references were not read from Compose")
	}
	env := services["vaultwarden"].Environment
	if env["ADMIN_TOKEN"] != "$argon2id$v=19$synthetic" || env["LITERAL"] != "$HOME $$ 'quoted'" || env["EMPTY"] != "" {
		t.Fatal("rendered environment was expanded or retained Compose dollar escaping")
	}
	if _, err := parseComposeServices("not JSON"); err == nil {
		t.Fatal("malformed Compose output accepted")
	}
}

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
