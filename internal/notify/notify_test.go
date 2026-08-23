package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
)

func recoveryEvent(id string) Event {
	return NewRecoveryEvent(id, "2026-08-23T12:00:00.000Z", RecoveryPoint{ContentID: "sha256:" + strings.Repeat("a", 64), Status: snapshot.Complete}, nil)
}

func TestQueueBoundsSuccessesAndCoalescesTargetState(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenQueueStore(dir, []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		event := recoveryEvent("recovery-" + strings.Repeat("x", i%8) + string(rune('A'+i%26)))
		if err := store.EnqueueAll([]string{"ops"}, event, "success", event.ID); err != nil {
			t.Fatal(err)
		}
	}
	queue := store.state.Notifiers["ops"]
	if len(queue.Items) != 100 || queue.Items[len(queue.Items)-1].Event.MergedSuccesses != 1 || queue.Items[len(queue.Items)-1].Event.ID != "recovery-xxxxW" {
		t.Fatalf("queue length/merged = %d/%d", len(queue.Items), queue.Items[len(queue.Items)-1].Event.MergedSuccesses)
	}
	if err := store.Ack("ops", queue.Items[0].Event.ID); err != nil {
		t.Fatal(err)
	}
	event := recoveryEvent("new-success")
	if err := store.EnqueueAll([]string{"ops"}, event, "success", event.ID); err != nil {
		t.Fatal(err)
	}
	queue = store.state.Notifiers["ops"]
	if queue.Items[len(queue.Items)-2].Event.MergedSuccesses != 1 || queue.Items[len(queue.Items)-1].Event.MergedSuccesses != 0 {
		t.Fatalf("merged success was not retained for delivery: %#v", queue)
	}
	degraded := NewTargetEvent("degraded", "target_degraded", "2026-08-23T12:00:00.000Z", TargetStatus{Name: "primary", State: "degraded", ErrorCode: "backup_failed"})
	recovered := NewTargetEvent("recovered", "target_recovered", "2026-08-23T12:01:00.000Z", TargetStatus{Name: "primary", State: "healthy"})
	if err := store.EnqueueAll([]string{"ops"}, degraded, "state", "primary"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAll([]string{"ops"}, recovered, "state", "primary"); err != nil {
		t.Fatal(err)
	}
	queue = store.state.Notifiers["ops"]
	stateCount := 0
	for _, item := range queue.Items {
		if item.Kind == "state" {
			stateCount++
			if item.Event.Type != "target_recovered" {
				t.Fatalf("state event was not replaced: %#v", item)
			}
		}
	}
	if stateCount != 1 {
		t.Fatalf("state item count = %d", stateCount)
	}
	if queue.Items[0].Kind != "state" {
		t.Fatal("target state change was queued behind ordinary successes")
	}
	if _, err := OpenQueueStore(dir, []string{"ops"}); err != nil {
		t.Fatalf("persistent queue did not reopen: %v", err)
	}
}

func TestSuccessMergeCountIsIndependentPerNotifier(t *testing.T) {
	t.Parallel()
	store, err := OpenQueueStore(t.TempDir(), []string{"full", "room"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSuccessItems; i++ {
		event := recoveryEvent(fmt.Sprintf("queued-%d", i))
		if err := store.EnqueueAll([]string{"full"}, event, "success", event.ID); err != nil {
			t.Fatal(err)
		}
	}
	latest := recoveryEvent("latest")
	if err := store.EnqueueAll([]string{"full", "room"}, latest, "success", latest.ID); err != nil {
		t.Fatal(err)
	}
	full := store.state.Notifiers["full"].Items
	room := store.state.Notifiers["room"].Items
	if full[len(full)-1].Event.MergedSuccesses != 1 {
		t.Fatalf("full queue merged count = %d", full[len(full)-1].Event.MergedSuccesses)
	}
	if len(room) != 1 || room[0].Event.ID != latest.ID || room[0].Event.MergedSuccesses != 0 {
		t.Fatalf("room queue inherited another notifier's merge count: %#v", room)
	}
}

func TestIncompleteRecoveryUsesReplaceablePriorityState(t *testing.T) {
	t.Parallel()
	store, err := OpenQueueStore(t.TempDir(), []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxSuccessItems; i++ {
		event := recoveryEvent(fmt.Sprintf("success-%d", i))
		if err := store.EnqueueAll([]string{"ops"}, event, "success", event.ID); err != nil {
			t.Fatal(err)
		}
	}
	incomplete := NewRecoveryEvent("incomplete", "2026-08-23T12:01:00.000Z", RecoveryPoint{ContentID: "sha256:" + strings.Repeat("b", 64), Status: snapshot.Incomplete}, nil)
	if err := store.EnqueueAll([]string{"ops"}, incomplete, "state", "source-integrity"); err != nil {
		t.Fatal(err)
	}
	queue := store.state.Notifiers["ops"]
	if len(queue.Items) != maxSuccessItems+1 || queue.Items[0].Event.ID != "incomplete" {
		t.Fatalf("incomplete recovery was not retained with priority: %#v", queue.Items[:1])
	}
}

func TestCompleteRecoveryAtomicallySupersedesPendingIncompleteState(t *testing.T) {
	t.Parallel()
	store, err := OpenQueueStore(t.TempDir(), []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	incomplete := NewRecoveryEvent("incomplete", "2026-08-23T12:00:00.000Z", RecoveryPoint{ContentID: "sha256:" + strings.Repeat("b", 64), Status: snapshot.Incomplete}, nil)
	if err := store.EnqueueAll([]string{"ops"}, incomplete, "state", "source-integrity"); err != nil {
		t.Fatal(err)
	}
	complete := recoveryEvent("complete")
	if err := store.EnqueueSuccessReplacingState([]string{"ops"}, complete, "source-integrity"); err != nil {
		t.Fatal(err)
	}
	queue := store.state.Notifiers["ops"]
	if len(queue.Items) != 1 || queue.Items[0].Kind != "success" || queue.Items[0].Event.ID != "complete" {
		t.Fatalf("complete recovery did not supersede incomplete state: %#v", queue.Items)
	}
}

func TestNotificationStateReconcilesRemovedNotifiers(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := OpenQueueStore(dir, []string{"removed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueAll([]string{"removed"}, recoveryEvent("old"), "success", "old"); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenQueueStore(dir, []string{"current"})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := reopened.state.Notifiers["removed"]; exists {
		t.Fatal("removed notifier queue remained active")
	}
	if queue, exists := reopened.state.Notifiers["current"]; !exists || len(queue.Items) != 0 {
		t.Fatalf("current notifier queue was not initialized: %#v", reopened.state.Notifiers)
	}
}

func TestManagerDefersBrokenNotifierSecretsWithoutBlockingBackupSetup(t *testing.T) {
	t.Parallel()
	manager, err := NewManager(t.TempDir(), []config.Notifier{{
		Name: "automation", Type: "webhook", Timeout: config.Duration{Duration: time.Second},
		URL: "https://example.test/hook", HeadersFromFiles: map[string]string{"Authorization": "/missing/secret"},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("notification secret blocked manager setup: %v", err)
	}
	if err := manager.EnqueueSuccess(recoveryEvent("deferred-secret")); err != nil {
		t.Fatalf("notification event was not persisted: %v", err)
	}
	if err := manager.senders[0].Send(context.Background(), recoveryEvent("deferred-secret")); err == nil {
		t.Fatal("broken deferred notifier secret was accepted")
	}
}

func TestWebhookUsesVersionedJSONAndRefusesRedirects(t *testing.T) {
	var redirected atomic.Int64
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			var event Event
			if err := json.NewDecoder(r.Body).Decode(&event); err != nil || event.SchemaVersion != 1 {
				t.Errorf("invalid webhook payload: %#v, %v", event, err)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := origin.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	sender := &webhookSender{name: "ops", url: origin.URL + "/ok", headers: http.Header{}, client: client}
	if err := sender.Send(context.Background(), recoveryEvent("one")); err != nil {
		t.Fatal(err)
	}
	sender.url = origin.URL + "/redirect"
	if err := sender.Send(context.Background(), recoveryEvent("two")); err == nil {
		t.Fatal("redirect was accepted")
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect destination was contacted")
	}
}

func TestWechatChecksHTTPAndErrcode(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"msgtype":"text"`) {
			t.Errorf("unexpected WeChat payload: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/http-error":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"errcode":0}`)
		case "/api-error":
			_, _ = io.WriteString(w, `{"errcode":40001}`)
		default:
			_, _ = io.WriteString(w, `{"errcode":0}`)
		}
	}))
	defer server.Close()
	sender := &wechatSender{name: "wechat", url: server.URL, client: server.Client()}
	if err := sender.Send(context.Background(), recoveryEvent("wechat")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/http-error", "/api-error"} {
		sender.url = server.URL + path
		if err := sender.Send(context.Background(), recoveryEvent("wechat-error")); err == nil {
			t.Fatalf("WeChat failure at %s was accepted", path)
		}
	}
}

type gatedSender struct {
	name    string
	started chan<- string
	release <-chan struct{}
}

func (s *gatedSender) Name() string { return s.name }

func (s *gatedSender) Send(ctx context.Context, _ Event) error {
	s.started <- s.name
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestNotifierDeliveryRunsIndependentlyPerNotifier(t *testing.T) {
	t.Parallel()
	names := []string{"first", "second"}
	store, err := OpenQueueStore(t.TempDir(), names)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, len(names))
	release := make(chan struct{})
	manager := &Manager{
		store: store, names: names, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		senders: []Sender{
			&gatedSender{name: names[0], started: started, release: release},
			&gatedSender{name: names[1], started: started, release: release},
		},
	}
	if err := manager.EnqueueSuccess(recoveryEvent("parallel")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	seen := map[string]bool{}
	for range names {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			cancel()
			close(release)
			manager.Wait()
			t.Fatal("notifiers did not start independently")
		}
	}
	if len(seen) != len(names) {
		t.Fatalf("notifier starts = %#v", seen)
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, first := store.Peek(names[0]); !first {
			if _, second := store.Peek(names[1]); !second {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	manager.Wait()
	for _, name := range names {
		if _, pending := store.Peek(name); pending {
			t.Fatalf("notifier %s did not acknowledge delivery", name)
		}
	}
}

func TestBarkUsesHTTP2JWTAndChecksAPNsReason(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("APNs request did not use HTTP/2: %s", r.Proto)
		}
		if !strings.HasPrefix(r.Header.Get("authorization"), "bearer ") || r.Header.Get("apns-topic") != "me.fin.bark" {
			t.Errorf("missing APNs headers: %#v", r.Header)
		}
		if fail.Load() {
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"reason":"Unregistered"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "apns-key")
	tokenPath := filepath.Join(dir, "device-token")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("a", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	senderRaw, err := newBarkSender(config.Notifier{
		Name: "bark", Type: "bark", Timeout: config.Duration{Duration: time.Second}, Endpoint: server.URL,
		DeviceTokenFile: tokenPath, AuthKeyFile: keyPath, KeyID: "KEY123", TeamID: "TEAM123", Topic: "me.fin.bark",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := senderRaw.(*barkSender)
	sender.client = server.Client()
	sender.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if err := sender.Send(context.Background(), recoveryEvent("bark-ok")); err != nil {
		t.Fatal(err)
	}
	http1 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	sender.endpoint = http1.URL
	sender.client = http1.Client()
	if err := sender.Send(context.Background(), recoveryEvent("bark-http1")); err == nil || !strings.Contains(err.Error(), "HTTP/2") {
		t.Fatalf("APNs HTTP/1 response was accepted: %v", err)
	}
	http1.Close()
	sender.endpoint = server.URL
	sender.client = server.Client()
	sender.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	fail.Store(true)
	if err := sender.Send(context.Background(), recoveryEvent("bark-fail")); err == nil || !strings.Contains(err.Error(), "Unregistered") {
		t.Fatalf("APNs failure was not checked: %v", err)
	}
}
