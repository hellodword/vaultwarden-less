package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

func recoveryEvent() Event {
	return RecoveryPointCreated(
		"sha256:"+strings.Repeat("a", 64),
		[]TargetStatus{
			{Name: "primary", State: "created"},
			{Name: "cold", State: "failed", ErrorCode: "command_failed"},
		},
	)
}

func TestRecoveryMessageListsEveryTargetOutcome(t *testing.T) {
	t.Parallel()
	event := RecoveryPointCreated(
		"sha256:"+strings.Repeat("a", 64),
		[]TargetStatus{
			{Name: "primary", State: "created"},
			{Name: "archive", State: "synced"},
			{Name: "cold", State: "failed", ErrorCode: "command_failed"},
		},
	)
	message := event.Message()
	for _, expected := range []string{"created=[primary]", "synced=[archive]", "failed=[cold]"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("recovery message %q does not contain %q", message, expected)
		}
	}
}

func TestMalformedTargetEventHasSafeFallbackMessage(t *testing.T) {
	t.Parallel()
	for _, event := range []Event{
		{Type: "target_degraded"},
		{Type: "target_recovered", Targets: []TargetStatus{{}, {}}},
	} {
		if message := event.Message(); message != "Vaultwarden backup status changed" {
			t.Fatalf("malformed event message = %q", message)
		}
	}
}

func TestManagerEnqueueCopiesEventData(t *testing.T) {
	t.Parallel()
	manager := newManagerWithSenders([]Sender{&countingSender{name: "operations"}}, nil, []time.Duration{0})
	event := recoveryEvent()
	manager.Enqueue(event)
	event.Targets[0].Name = "mutated"
	queued := <-manager.workers[0].queue
	if queued.Targets[0].Name != "primary" {
		t.Fatalf("queued event changed through caller slice: %#v", queued)
	}
}

type countingSender struct {
	name      string
	failUntil int64
	calls     atomic.Int64
	reached   chan int64
}

func (sender *countingSender) Name() string { return sender.name }

func (sender *countingSender) Send(context.Context, Event) error {
	calls := sender.calls.Add(1)
	if sender.reached != nil {
		select {
		case sender.reached <- calls:
		default:
		}
	}
	if calls <= sender.failUntil {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func TestManagerRetryLimitAndSuccessStop(t *testing.T) {
	for _, test := range []struct {
		name      string
		failUntil int64
		wantCalls int64
	}{
		{name: "three-attempt limit", failUntil: 99, wantCalls: 3},
		{name: "stop after success", failUntil: 1, wantCalls: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			sender := &countingSender{name: "operations", failUntil: test.failUntil, reached: make(chan int64, 3)}
			manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0, 0, 0})
			ctx, cancel := context.WithCancel(context.Background())
			var wait sync.WaitGroup
			wait.Go(func() { manager.Run(ctx) })
			manager.Enqueue(recoveryEvent())
			for {
				select {
				case calls := <-sender.reached:
					if calls == test.wantCalls {
						cancel()
						wait.Wait()
						if sender.calls.Load() != test.wantCalls {
							t.Fatalf("calls = %d, want %d", sender.calls.Load(), test.wantCalls)
						}
						return
					}
				case <-time.After(time.Second):
					cancel()
					wait.Wait()
					t.Fatal("notification attempts did not finish")
				}
			}
		})
	}
}

func TestManagerQueueIsBoundedAndNonblocking(t *testing.T) {
	t.Parallel()
	sender := &countingSender{name: "operations"}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0})
	for range notifierQueueCapacity + 20 {
		manager.Enqueue(recoveryEvent())
	}
	if queued := len(manager.workers[0].queue); queued != notifierQueueCapacity {
		t.Fatalf("queued = %d", queued)
	}
}

func TestCancellationDropsPendingRetries(t *testing.T) {
	sender := &countingSender{name: "operations", failUntil: 99, reached: make(chan int64, 1)}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0, time.Hour, time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Go(func() { manager.Run(ctx) })
	manager.Enqueue(recoveryEvent())
	select {
	case <-sender.reached:
		cancel()
		wait.Wait()
	case <-time.After(time.Second):
		cancel()
		wait.Wait()
		t.Fatal("first attempt did not start")
	}
	if sender.calls.Load() != 1 {
		t.Fatalf("calls = %d", sender.calls.Load())
	}
}

func TestWechatChecksHTTPAndErrcode(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"msgtype":"text"`) {
			t.Errorf("unexpected WeChat payload: %s", body)
		}
		if strings.Contains(string(body), "synthetic-webhook-secret") {
			t.Errorf("WeChat payload exposed webhook material: %s", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/http-error":
			writer.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(writer, `{"errcode":0}`)
		case "/api-error":
			_, _ = io.WriteString(writer, `{"errcode":40001}`)
		default:
			_, _ = io.WriteString(writer, `{"errcode":0}`)
		}
	}))
	defer server.Close()
	sender := &wechatSender{name: "wechat", url: server.URL + "?key=synthetic-webhook-secret", client: server.Client()}
	if err := sender.Send(context.Background(), recoveryEvent()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/http-error", "/api-error"} {
		sender.url = server.URL + path
		if err := sender.Send(context.Background(), recoveryEvent()); err == nil {
			t.Fatalf("WeChat failure at %s was accepted", path)
		}
	}
}

func TestNotificationHTTPClientRefusesRedirects(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	response, err := strictClient(time.Second).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("redirect status/contacts = %d/%d", response.StatusCode, redirected.Load())
	}
}

func TestBarkUsesHTTP2JWTAndChecksAPNsReason(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.ProtoMajor != 2 {
			t.Errorf("APNs request did not use HTTP/2: %s", request.Proto)
		}
		if !strings.HasPrefix(request.Header.Get("authorization"), "bearer ") || request.Header.Get("apns-topic") != "me.fin.bark" {
			t.Errorf("missing APNs headers: %#v", request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		for _, forbidden := range []string{strings.Repeat("a", 64), "KEY123", "TEAM123", "PRIVATE KEY"} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("APNs payload exposed notifier material %q", forbidden)
			}
		}
		if fail.Load() {
			writer.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(writer, `{"reason":"Unregistered"}`)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	senderRaw, err := newBarkSender(config.Notifier{
		Name: "bark", Type: "bark",
		DeviceToken: strings.Repeat("a", 64), AuthKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})),
		KeyID: "KEY123", TeamID: "TEAM123", Topic: "me.fin.bark",
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := senderRaw.(*barkSender)
	sender.endpoint = server.URL
	sender.client = server.Client()
	sender.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	if err := sender.Send(context.Background(), recoveryEvent()); err != nil {
		t.Fatal(err)
	}
	http1 := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) }))
	sender.endpoint = http1.URL
	sender.client = http1.Client()
	if err := sender.Send(context.Background(), recoveryEvent()); err == nil || !strings.Contains(err.Error(), "HTTP/2") {
		t.Fatalf("APNs HTTP/1 response was accepted: %v", err)
	}
	http1.Close()
	sender.endpoint = server.URL
	sender.client = server.Client()
	sender.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	fail.Store(true)
	if err := sender.Send(context.Background(), recoveryEvent()); err == nil || !strings.Contains(err.Error(), "Unregistered") {
		t.Fatalf("APNs failure was not checked: %v", err)
	}
}
