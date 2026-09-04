package worker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/restic"
)

var runAcceleratedWorkerIntegration = flag.Bool(
	"run-accelerated-worker-integration",
	false,
	"run the fake-clock worker lifecycle against real Restic and notification adapters",
)

func TestWorkerAcceleratedTargetDegradationIntegration(t *testing.T) {
	if !*runAcceleratedWorkerIntegration {
		t.Skip("pass -run-accelerated-worker-integration to run the real adapter lifecycle")
	}
	resticBinary, err := exec.LookPath("restic")
	if err != nil {
		t.Fatalf("locate Restic: %v", err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	privateDirectory := func(name string) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cacheRoot := privateDirectory("cache")
	passwordRoot := privateDirectory("passwords")
	recoveryRoot := privateDirectory("recovery")
	repositoryRoot := privateDirectory("repositories")
	if err := os.WriteFile(filepath.Join(recoveryRoot, "db.sqlite3"), []byte("accelerated worker integration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"primary", "secondary"} {
		if err := os.WriteFile(filepath.Join(passwordRoot, target), []byte("integration-password\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	credentials := &config.RoleCredentials{Targets: map[string]config.RoleTarget{
		"primary":   {Environment: map[string]string{}},
		"secondary": {Environment: map[string]string{}},
	}}
	targets := []config.Target{
		{Name: "primary", Repository: filepath.Join(repositoryRoot, "primary"), Options: []config.Option{}},
		{Name: "secondary", Repository: filepath.Join(repositoryRoot, "secondary"), Options: []config.Option{}},
	}
	runner := restic.NewRunner(resticBinary, cacheRoot, passwordRoot, recoveryRoot, credentials, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runner.VerifyVersion(ctx); err != nil {
		t.Fatalf("verify Restic adapter: %v", err)
	}
	if err := runner.Init(ctx, targets[0]); err != nil {
		t.Fatalf("initialize primary Restic repository: %v", err)
	}

	messages := make(chan string, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read notification: %v", readErr)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.Unmarshal(body, &payload); err != nil || payload.MsgType != "text" || payload.Text.Content == "" {
			t.Errorf("invalid notification payload: %v/%s", err, body)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		messages <- payload.Text.Content
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"errcode":0}`)
	}))
	defer server.Close()
	certificatePath := filepath.Join(root, "notification-ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if certificate == nil {
		t.Fatal("encode notification test certificate")
	}
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", certificatePath)
	notifications, err := notify.NewManager([]config.Notifier{{
		Name: "integration", Type: "wechat", WebhookURL: server.URL,
	}}, discardLogger())
	if err != nil {
		t.Fatalf("initialize notification adapter: %v", err)
	}
	notificationContext, stopNotifications := context.WithCancel(ctx)
	var notificationWait sync.WaitGroup
	notificationWait.Go(func() { notifications.Run(notificationContext) })
	defer func() {
		stopNotifications()
		notificationWait.Wait()
	}()

	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(base)
	manifest := workerManifest(t)
	cfg := &config.Backup{Targets: targets}
	worker := newWithClock(cfg, (&fixedSource{manifest: manifest}).Build, runner.Ensure, notifications.Enqueue, discardLogger(), clock)
	runCheck := func(want string) {
		t.Helper()
		started := clock.Now()
		outcome := worker.check(ctx)
		if outcome.cancelled || outcome.result != want {
			t.Fatalf("accelerated check outcome = %#v, want %s", outcome, want)
		}
		worker.finishCheck(started, clock.Now(), outcome)
	}

	runCheck("partial")
	if message := waitWorkerNotification(t, messages); !strings.Contains(message, "created=[primary]") ||
		!strings.Contains(message, "failed=[secondary]") {
		t.Fatalf("initial adapter notification = %q", message)
	}
	clock.Advance(degradedAfter - time.Millisecond)
	runCheck("partial")
	assertNoWorkerNotification(t, messages)
	if status := worker.Status(); status.State != "partial" || status.Targets[1].State != "retrying" {
		t.Fatalf("status at 29:59.999 = %#v", status)
	}

	clock.Advance(time.Millisecond)
	runCheck("partial")
	if message := waitWorkerNotification(t, messages); !strings.Contains(message, "target secondary is degraded") {
		t.Fatalf("threshold adapter notification = %q", message)
	}
	if status := worker.Status(); status.State != "degraded" || status.Targets[1].State != "degraded" {
		t.Fatalf("status at 30:00.000 = %#v", status)
	}
	clock.Advance(failureRetry)
	runCheck("partial")
	assertNoWorkerNotification(t, messages)

	if err := runner.Init(ctx, targets[1]); err != nil {
		t.Fatalf("initialize recovered secondary Restic repository: %v", err)
	}
	clock.Advance(failureRetry)
	runCheck("created")
	if message := waitWorkerNotification(t, messages); !strings.Contains(message, "target secondary has recovered") {
		t.Fatalf("recovery adapter notification = %q", message)
	}
	if message := waitWorkerNotification(t, messages); !strings.Contains(message, "created=[secondary]") ||
		!strings.Contains(message, "synced=[primary]") {
		t.Fatalf("catch-up adapter notification = %q", message)
	}
	assertNoWorkerNotification(t, messages)
	status := worker.Status()
	if status.State != "idle" || status.Targets[0].State != "healthy" || status.Targets[1].State != "healthy" {
		t.Fatalf("recovered accelerated status = %#v", status)
	}
}

func waitWorkerNotification(t *testing.T, messages <-chan string) string {
	t.Helper()
	select {
	case message := <-messages:
		return message
	case <-time.After(5 * time.Second):
		t.Fatal("notification adapter did not deliver an event")
		return ""
	}
}

func assertNoWorkerNotification(t *testing.T, messages <-chan string) {
	t.Helper()
	select {
	case message := <-messages:
		t.Fatalf("unexpected repeated notification: %q", message)
	case <-time.After(150 * time.Millisecond):
	}
}
