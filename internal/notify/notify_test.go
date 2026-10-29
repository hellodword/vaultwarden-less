package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/routes"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"github.com/hellodword/vaultwarden-less/internal/testutil"
)

type responseTransport func(*http.Request) (*http.Response, error)

func (f responseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAPNsResponsePolicy(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	for _, test := range []struct {
		name      string
		status    int
		retry     string
		delay     time.Duration
		permanent bool
	}{
		{"success", 200, "", 0, false},
		{"rate limit", 429, "120", 2 * time.Minute, false},
		{"server failure", 500, "", 15 * time.Minute, false},
		{"server maintenance", 503, now.Add(20 * time.Minute).UTC().Format(http.TimeFormat), 20 * time.Minute, false},
		{"bad payload", 400, "", 0, true},
		{"bad authorization", 403, "", 0, true},
		{"expired device", 410, "", 0, true},
		{"oversized payload", 413, "", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: responseTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.status, ProtoMajor: 2, Header: http.Header{"Retry-After": []string{test.retry}}, Body: io.NopCloser(strings.NewReader(`{"reason":"synthetic"}`)), Request: r}, nil
			})}
			s := &barkSender{cfg: config.Notifier{Key: key, DeviceToken: "device"}, client: client, now: func() time.Time { return now }}
			err := s.Send(context.Background(), Event{Count: 1})
			if test.status == 200 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var failure *DeliveryError
			if !errors.As(err, &failure) || failure.After != test.delay || failure.Permanent != test.permanent {
				t.Fatalf("response policy: %v", err)
			}
		})
	}
}

func webhookMarkdown(t *testing.T, kind string, doc map[string]any) string {
	t.Helper()
	if kind == "feishu" {
		card, _ := doc["card"].(map[string]any)
		header, _ := card["header"].(map[string]any)
		title, _ := header["title"].(map[string]any)
		body, _ := card["body"].(map[string]any)
		elements, _ := body["elements"].([]any)
		if doc["msg_type"] != "interactive" || card["schema"] != "2.0" || title["tag"] != "plain_text" || title["content"] == "" || len(elements) != 1 {
			t.Error("invalid Feishu Markdown card")
			return ""
		}
		element, _ := elements[0].(map[string]any)
		if element["tag"] != "markdown" {
			t.Error("missing native Markdown element")
		}
		content, _ := element["content"].(string)
		return content
	}
	markdown, _ := doc["markdown"].(map[string]any)
	if doc["msgtype"] != "markdown" {
		t.Error("missing native Markdown message")
	}
	if kind == "dingtalk" {
		title, _ := markdown["title"].(string)
		if !strings.HasPrefix(title, "vaultwarden-less · ") {
			t.Error("missing DingTalk title")
		}
		content, _ := markdown["text"].(string)
		return content
	}
	content, _ := markdown["content"].(string)
	return content
}

func TestWebhookEncodedBodyLimitsAndHTTPRetry(t *testing.T) {
	event := Event{Checks: 1, Count: 1, Source: "incomplete", Routes: map[string]routes.Counts{}}
	for i := range 1000 {
		event.Routes[strconv.Itoa(i)+strings.Repeat("<界&", 20)] = routes.Counts{Started: 1, Finished: 1}
	}
	now := time.Unix(1800000000, 0)
	for _, kind := range []string{"wecom", "feishu", "dingtalk"} {
		t.Run(kind, func(t *testing.T) {
			s := &webhookSender{cfg: config.Notifier{Type: kind, WebhookURL: "https://example.test/hook"}, now: func() time.Time { return now }}
			s.client = &http.Client{Transport: responseTransport(func(r *http.Request) (*http.Response, error) {
				data, _ := io.ReadAll(r.Body)
				limit := 20000
				if kind == "wecom" {
					limit = 16000
				}
				if len(data) > limit || !strings.Contains(string(data), "Omitted details:") {
					t.Error("unbounded or unreported truncated payload")
				}
				var doc map[string]any
				if json.Unmarshal(data, &doc) != nil {
					t.Fatal("invalid body")
				}
				content := webhookMarkdown(t, kind, doc)
				if !utf8.ValidString(content) || !strings.Contains(content, "Source: Incomplete") || !strings.Contains(content, "Source checks: 1; effective changes: 0") {
					t.Error("truncation lost essential summary or UTF-8")
				}
				if kind == "wecom" && (len(content) > 4096 || len(content) <= 2048) {
					t.Error("WeCom did not use its Markdown byte budget")
				}
				if kind == "dingtalk" && len(content) > 4000 {
					t.Error("DingTalk text exceeds its summary budget")
				}
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"90"}}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
			})}
			var failure *DeliveryError
			if err := s.Send(context.Background(), event); !errors.As(err, &failure) || failure.After != 90*time.Second || failure.Permanent {
				t.Fatalf("webhook retry policy: %v", err)
			}
		})
	}
}

func TestWebhooksSignaturesAndExplicitSuccess(t *testing.T) {
	now := time.Unix(1800000000, 123000000)
	for _, kind := range []string{"wecom", "feishu", "dingtalk"} {
		t.Run(kind, func(t *testing.T) {
			var response string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if kind == "feishu" {
					if payload["timestamp"] != strconv.FormatInt(now.Unix(), 10) || payload["sign"] != signature(strconv.FormatInt(now.Unix(), 10)+"\nsecret", "") {
						t.Error("Feishu signature or payload invalid")
					}
				}
				if content := webhookMarkdown(t, kind, payload); !strings.Contains(content, "Source: Complete") {
					t.Error("shared English Markdown report missing")
				}
				if kind == "dingtalk" {
					timestamp := strconv.FormatInt(now.UnixMilli(), 10)
					if r.URL.Query().Get("timestamp") != timestamp || r.URL.Query().Get("sign") != signature("secret", timestamp+"\nsecret") {
						t.Error("DingTalk signature invalid")
					}
				}
				io.WriteString(w, response)
			}))
			defer server.Close()
			sender := &webhookSender{cfg: config.Notifier{Name: kind, Type: kind, WebhookURL: server.URL, Secret: "secret"}, client: server.Client(), now: func() time.Time { return now }}
			key := "errcode"
			if kind == "feishu" {
				key = "code"
			}
			for _, raw := range []string{"", `{}`, `null`, `{"` + key + `":null}`, `{"` + key + `":"0"}`, `{"` + key + `":1}`, `{"` + key + `":1e-9999}`, `invalid`} {
				response = raw
				if sender.Send(context.Background(), Event{Count: 1, Source: "complete"}) == nil {
					t.Errorf("accepted %q", raw)
				}
			}
			for _, zero := range []string{"0", "-0", "0.0", "0e20"} {
				response = `{"` + key + `":` + zero + `,"future":true}`
				if err := sender.Send(context.Background(), Event{Count: 1, Source: "complete"}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestBarkRealHTTP2JWTAndPayload(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"api.push.apple.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 || r.Host != "api.push.apple.com" || r.URL.Path != "/3/device/variable-token" {
			t.Error("APNs protocol or endpoint invalid")
		}
		if r.Header.Get("apns-topic") != "topic" || r.Header.Get("apns-push-type") != "alert" {
			t.Error("APNs headers missing")
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("authorization"), "bearer "), ".")
		if len(parts) != 3 {
			t.Error("invalid JWT")
			return
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err != nil || len(signature) != 64 || !ecdsa.Verify(&key.PublicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
			t.Error("JWT signature not standard ES256")
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claim map[string]any
		json.Unmarshal(claims, &claim)
		if claim["iss"] != "team" || claim["iat"] != float64(now.Unix()) {
			t.Error("JWT claims invalid")
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) > 4096 {
			t.Error("APNs payload exceeds limit")
		}
		var value map[string]any
		json.Unmarshal(body, &value)
		aps := value["aps"].(map[string]any)
		if aps["category"] != "myNotificationCategory" || aps["mutable-content"] != float64(1) || aps["sound"] != "" || aps["thread-id"] != "vaultwarden-less" || value["badge"] != "1" || value["group"] != "vaultwarden-less" || value["isarchive"] != "1" {
			t.Error("Bark extension fields missing")
		}
		markdown, _ := value["markdown"].(string)
		alert, _ := aps["alert"].(map[string]any)
		fallback, _ := alert["body"].(string)
		if alert["title"] != "vaultwarden-less · Vaultwarden backup — Daily" || !strings.HasPrefix(fallback, "Source: Complete.") || strings.Contains(fallback, "**") {
			t.Error("APNs plain fallback or result title missing")
		}
		if !utf8.ValidString(markdown) || !strings.Contains(markdown, "Source checks: 1; effective changes: 0") || !strings.Contains(markdown, "Omitted details:") || !strings.Contains(markdown, "Unknown GET: started 1, finished 1") {
			t.Error("Bark top-level Markdown missing or invalid")
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.Time = func() time.Time { return now }
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	// The test transport trusts only the local TLS server. Production has no
	// configurable APNs endpoint and always addresses the Apple hostname.
	sender := &barkSender{cfg: config.Notifier{Name: "bark", Key: key, KeyID: "key", TeamID: "team", Topic: "topic", DeviceToken: "variable-token", Sound: ""}, client: &http.Client{Transport: transport}, now: func() time.Time { return now }}
	event := Event{Checks: 1, Count: 4, Source: "complete", Targets: []TargetStatus{{Name: "primary", State: "unchanged"}}, Unknown: map[string]routes.Counts{"GET": {Started: 1, Finished: 1}}, Routes: map[string]routes.Counts{strings.Repeat("\"<&界", 5000): {Started: 1}}}
	for i := range 100 {
		event.Routes[strconv.Itoa(i)+strings.Repeat("<界&", 20)] = routes.Counts{Started: 1, Finished: 1}
	}
	if err := sender.Send(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	first, err := sender.token(now)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := sender.token(now.Add(49 * time.Minute))
	third, _ := sender.token(now.Add(50 * time.Minute))
	if first != second || first == third {
		t.Fatal("JWT cache lifetime incorrect")
	}
}

type fakeSender struct {
	mu      sync.Mutex
	name    string
	events  []Event
	failure error
}

func (s *fakeSender) Name() string { return s.name }
func (s *fakeSender) Send(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return s.failure
}
func (s *fakeSender) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.events) }

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestMergedRetriesIsolationAndRecovery(t *testing.T) {
	clock := testutil.NewClock()
	bad := &fakeSender{name: "bad", failure: &DeliveryError{Code: "unavailable"}}
	good := &fakeSender{name: "good"}
	m := &Manager{clock: clock}
	for _, s := range []*fakeSender{bad, good} {
		m.channels = append(m.channels, &channel{sender: s, wake: make(chan struct{}, 1), status: Status{Name: s.name}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	m.Enqueue(Event{Checks: 1, Source: "incomplete", Issues: map[string]int{"capture_timeout": 1}, Unknown: map[string]routes.Counts{"GET": {Started: 1}}})
	eventually(t, func() bool { return bad.count() == 1 && good.count() == 1 && !m.Status()[0].NextAttempt.IsZero() })
	for range 100 {
		m.Enqueue(Event{Checks: 1, Source: "complete"})
	}
	clock.Advance(10 * time.Second)
	eventually(t, func() bool { return good.count() == 2 })
	if bad.count() != 1 {
		t.Fatal("new checks reset failed channel backoff")
	}
	clock.Advance(50 * time.Second)
	eventually(t, func() bool { return bad.count() == 2 })
	clock.Advance(5 * time.Minute)
	eventually(t, func() bool { return bad.count() == 3 && m.Status()[0].Exhausted })
	clock.Advance(24 * time.Hour)
	if bad.count() != 3 {
		t.Fatal("exhausted notification retried without new event")
	}
	bad.mu.Lock()
	bad.failure = nil
	bad.mu.Unlock()
	m.Enqueue(Event{Checks: 1, Source: "complete", Unknown: map[string]routes.Counts{"GET": {Finished: 1}}})
	eventually(t, func() bool { return bad.count() == 4 && m.Status()[0].Pending == 0 })
	bad.mu.Lock()
	recovery := bad.events[3]
	bad.mu.Unlock()
	if recovery.Checks != 102 || recovery.Undelivered == 0 {
		t.Fatal("undelivered state was lost")
	}
	if recovery.Unknown["GET"] != (routes.Counts{Started: 1, Finished: 1}) || len(recovery.Issues) != 0 || recovery.Source != "complete" {
		t.Fatal("retry merged request phases or kept stale source diagnostics")
	}
	cancel()
	<-done
}

func TestReportTitlesAndTargetState(t *testing.T) {
	for _, kind := range []string{"Fault", "Recovery", "Reminder", "Daily"} {
		now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
		event := Event{Kind: kind, At: now, Timezone: "Asia/Shanghai", Source: "incomplete", Count: 1, Checks: 3, Changes: 2, Targets: []TargetStatus{{Name: "primary", State: "failed", Failed: true}}}
		message := event.Message(4096)
		for _, want := range []string{"Vaultwarden backup — " + kind, "2026-10-29 20:00:00 +08:00", "Source: Incomplete; retry pending", "Targets: 1 configured; 1 failed", "Verification:", "Source checks: 3; effective changes: 2"} {
			if !strings.Contains(message, want) {
				t.Fatalf("missing %q: %s", want, message)
			}
		}
	}
}

func TestReportMergeKeepsRequestPhasesAndLatestDiagnostics(t *testing.T) {
	event := Event{}
	event.Merge(Event{Checks: 1, Count: 1, Undelivered: 1, Source: "incomplete", Issues: map[string]int{"capture_timeout": 1}, Warnings: []string{"file_reference_diagnostics_unavailable"}, Routes: map[string]routes.Counts{"POST /api/ciphers": {Started: 1}}, Unknown: map[string]routes.Counts{"GET": {Started: 1}}, Targets: []TargetStatus{{Name: "primary", State: "failed"}}})
	event.Merge(Event{Checks: 1, Count: 2, Source: "complete", Routes: map[string]routes.Counts{"POST /api/ciphers": {Finished: 1}}, Unknown: map[string]routes.Counts{"GET": {Finished: 1}}, Targets: []TargetStatus{{Name: "primary", State: "uploaded"}}})
	message := event.Message(4096)
	for _, want := range []string{"Unknown GET: started 1, finished 1", "POST /api/ciphers: started 1, finished 1", "Previously undelivered reports: 1"} {
		if !strings.Contains(message, want) {
			t.Fatalf("missing %q in %s", want, message)
		}
	}
	if len(event.Issues) != 0 || len(event.Warnings) != 0 || event.Title() != "Vaultwarden backup — Daily" {
		t.Fatal("recovered source still reported old problems")
	}
	copy := event.clone()
	copy.Unknown["GET"] = routes.Counts{Started: math.MaxUint64, Finished: 7}
	copy.Merge(Event{Unknown: map[string]routes.Counts{"GET": {Started: 1, Finished: 2}}})
	if copy.Unknown["GET"] != (routes.Counts{Started: math.MaxUint64, Finished: 9}) || event.Unknown["GET"] != (routes.Counts{Started: 1, Finished: 1}) {
		t.Fatal("phase counter overflow or aliased delivery snapshot")
	}
}

func TestMarkdownEscapingAndWholeDetailOmission(t *testing.T) {
	event := Event{Checks: 1, Count: 2, Source: "complete", Targets: []TargetStatus{
		{Name: "- ~~primary**\n> [link](https://example.test) <at id=all>", State: "unchanged"},
		{Name: strings.Repeat("界", 5000), State: "failed"},
	}, Unknown: map[string]routes.Counts{"GET": {Started: 1, Finished: 1}}}
	message := event.Message(700)
	if !utf8.ValidString(message) || len(message) > 700 || !strings.Contains(message, "Omitted details:") || !strings.Contains(message, "Targets: 2 configured; 1 failed; 0 pending") {
		t.Fatalf("unsafe detail omission: %s", message)
	}
	if !strings.Contains(message, `\- \~\~primary\*\* &gt; \[link\]\(https://example\.test\) &lt;at id=all&gt;`) || strings.Contains(message, "\n> [link]") || strings.Contains(message, "<at") {
		t.Fatal("dynamic target name changed Markdown structure")
	}
	if !strings.Contains(message, "Unknown GET: started 1, finished 1") || strings.Contains(message, "界") {
		t.Fatal("oversized detail hid later small details or was partially included")
	}
}

func TestNotifierStatusOmitsUnsetTimes(t *testing.T) {
	status := Status{Name: "bark"}
	data, err := json.Marshal(status)
	if err != nil || strings.Contains(string(data), "last_success") || strings.Contains(string(data), "next_attempt") {
		t.Fatalf("status invented a timestamp: %s (%v)", data, err)
	}
	status.LastSuccess = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	status.NextAttempt = status.LastSuccess.Add(time.Minute)
	data, err = json.Marshal(status)
	var decoded Status
	if err != nil || json.Unmarshal(data, &decoded) != nil || !decoded.LastSuccess.Equal(status.LastSuccess) || !decoded.NextAttempt.Equal(status.NextAttempt) {
		t.Fatal("nonzero timestamps did not survive status JSON")
	}
}

func TestAPNsMinimumRetryAndPermanentFailure(t *testing.T) {
	clock := testutil.NewClock()
	s := &fakeSender{name: "bark", failure: &DeliveryError{Code: "apns_server_failure", After: 15 * time.Minute}}
	c := &channel{sender: s, wake: make(chan struct{}, 1), status: Status{Name: s.name}}
	m := &Manager{clock: clock, channels: []*channel{c}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	m.Enqueue(Event{Checks: 1})
	eventually(t, func() bool { return s.count() == 1 && m.Status()[0].Error != "" })
	clock.Advance(14 * time.Minute)
	m.Enqueue(Event{Checks: 1})
	if s.count() != 1 {
		t.Fatal("APNs 5xx retried early")
	}
	clock.Advance(time.Minute)
	eventually(t, func() bool { return s.count() == 2 })
	s.mu.Lock()
	s.failure = &DeliveryError{Code: "bad_device", Permanent: true}
	s.mu.Unlock()
	clock.Advance(15 * time.Minute)
	eventually(t, func() bool { return m.Status()[0].Permanent })
	clock.Advance(time.Hour)
	m.Enqueue(Event{Checks: 1})
	if s.count() != 3 {
		t.Fatal("invalid device retried")
	}
	cancel()
	<-done
}

func TestDurableBudgetAndRecoveryBeforeDelivery(t *testing.T) {
	clock := testutil.NewClock()
	store := state.Store{Directory: t.TempDir()}
	sender := &fakeSender{name: "persistent", failure: &DeliveryError{Code: "unavailable"}}
	makeManager := func() *Manager {
		m := &Manager{clock: clock, channels: []*channel{{sender: sender, wake: make(chan struct{}, 1), status: Status{Name: sender.name}, identity: "same"}}}
		if err := m.Restore(store); err != nil {
			t.Fatal(err)
		}
		return m
	}
	start := func(m *Manager) (context.CancelFunc, <-chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- m.Run(ctx) }()
		return cancel, done
	}
	m := makeManager()
	cancel, done := start(m)
	first := clock.Now()
	if err := m.Enqueue(Event{Kind: "Fault", At: first, Faults: map[string]Fault{"source": {Object: "Source", Code: "incomplete", First: first}}, Source: "incomplete"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return m.Status()[0].Error != "" })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m = makeManager()
	if m.Status()[0].Attempts != 1 {
		t.Fatal("restart reset budget")
	}
	cancel, done = start(m)
	clock.Advance(time.Minute)
	eventually(t, func() bool { return sender.count() == 2 && m.Status()[0].Attempts == 2 })
	clock.Advance(5 * time.Minute)
	eventually(t, func() bool { return m.Status()[0].Exhausted })
	cancel()
	<-done
	m = makeManager()
	if !m.Status()[0].Exhausted {
		t.Fatal("restart reset exhausted round")
	}
	cancel, done = start(m)
	clock.Advance(time.Hour)
	if sender.count() != 3 {
		t.Fatal("idle activity reactivated exhausted delivery")
	}
	sender.mu.Lock()
	sender.failure = nil
	sender.mu.Unlock()
	recovered := clock.Now()
	m.Enqueue(Event{Kind: "Recovery", At: recovered, Source: "complete", Faults: map[string]Fault{"source": {Object: "Source", Code: "incomplete", First: first, Recovered: recovered}}})
	eventually(t, func() bool { return m.Status()[0].Pending == 0 })
	sender.mu.Lock()
	last := sender.events[len(sender.events)-1]
	sender.mu.Unlock()
	if last.Faults["source"].First != first || last.Faults["source"].Recovered != recovered || !strings.Contains(last.Message(4096), "occurred then recovered") {
		t.Fatal("undelivered incident lost")
	}
	cancel()
	<-done
}
