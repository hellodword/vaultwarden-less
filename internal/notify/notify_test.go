package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

func recoveryEvent(id string) Event {
	return NewRecoveryEvent(
		id,
		"2026-08-23T12:00:00Z",
		"sha256:"+strings.Repeat("a", 64),
		[]TargetStatus{
			{Name: "primary", State: "created"},
			{Name: "cold", State: "failed", ErrorCode: "command_failed"},
		},
	)
}

func TestEventContractAlwaysUsesTargetArraysAndContainsNoSecrets(t *testing.T) {
	t.Parallel()
	events := []Event{
		recoveryEvent("recovery"),
		NewSourceEvent("source", "source_degraded", "2026-08-23T12:00:00Z", "source_check_failed"),
		NewTargetEvent("target", "target_recovered", "2026-08-23T12:00:00Z", TargetStatus{Name: "primary", State: "healthy"}),
	}
	for _, event := range events {
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"targets":null`) {
			t.Fatalf("null targets in %s", data)
		}
		for _, forbidden := range []string{"repository", "password", "webhook", "private_key", "AWS_SECRET"} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("notification contains forbidden field %q: %s", forbidden, data)
			}
		}
	}
}

func TestRecoveryMessageListsEveryTargetOutcome(t *testing.T) {
	t.Parallel()
	event := NewRecoveryEvent(
		"recovery",
		"2026-08-23T12:00:00Z",
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

func TestManagerAttemptsEachNotificationAtMostThreeTimes(t *testing.T) {
	sender := &countingSender{name: "operations", failUntil: 99, reached: make(chan int64, 3)}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0, 0, 0})
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	manager.Enqueue(recoveryEvent("three-attempts"))
	for {
		select {
		case calls := <-sender.reached:
			if calls == 3 {
				cancel()
				manager.Wait()
				if sender.calls.Load() != 3 {
					t.Fatalf("calls = %d", sender.calls.Load())
				}
				return
			}
		case <-time.After(time.Second):
			cancel()
			manager.Wait()
			t.Fatal("notification attempts did not finish")
		}
	}
}

func TestManagerStopsRetryingAfterSuccess(t *testing.T) {
	sender := &countingSender{name: "operations", failUntil: 1, reached: make(chan int64, 3)}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0, 0, 0})
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	manager.Enqueue(recoveryEvent("eventual-success"))
	for {
		select {
		case calls := <-sender.reached:
			if calls == 2 {
				cancel()
				manager.Wait()
				if sender.calls.Load() != 2 {
					t.Fatalf("calls = %d", sender.calls.Load())
				}
				return
			}
		case <-time.After(time.Second):
			cancel()
			manager.Wait()
			t.Fatal("notification success did not finish")
		}
	}
}

func TestManagerQueueIsBoundedAndNonblocking(t *testing.T) {
	t.Parallel()
	sender := &countingSender{name: "operations"}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0})
	for index := 0; index < notifierQueueCapacity+20; index++ {
		manager.Enqueue(recoveryEvent("bounded-" + string(rune('a'+index%26))))
	}
	if queued := len(manager.workers[0].queue); queued != notifierQueueCapacity {
		t.Fatalf("queued = %d", queued)
	}
}

func TestManagerPreservesEmptyTargetArray(t *testing.T) {
	t.Parallel()
	sender := &countingSender{name: "operations"}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0})
	manager.Enqueue(NewSourceEvent("source", "source_degraded", "2026-08-23T12:00:00Z", "source_check_failed"))
	queued := <-manager.workers[0].queue
	if queued.Targets == nil || len(queued.Targets) != 0 {
		t.Fatalf("queued targets = %#v, want a non-nil empty array", queued.Targets)
	}
}

func TestCancellationDropsPendingRetries(t *testing.T) {
	sender := &countingSender{name: "operations", failUntil: 99, reached: make(chan int64, 1)}
	manager := newManagerWithSenders([]Sender{sender}, nil, []time.Duration{0, time.Hour, time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	manager.Start(ctx)
	manager.Enqueue(recoveryEvent("cancel"))
	select {
	case <-sender.reached:
		cancel()
		manager.Wait()
	case <-time.After(time.Second):
		cancel()
		manager.Wait()
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
	if err := sender.Send(context.Background(), recoveryEvent("bark-ok")); err != nil {
		t.Fatal(err)
	}
	http1 := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) }))
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
