package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

type marker struct{ count atomic.Int64 }

func (m *marker) MarkDirty() { m.count.Add(1) }

func proxyConfig(t *testing.T, upstream string) *config.Proxy {
	t.Helper()
	parsed, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	return &config.Proxy{
		ListenAddress: ":8080", Upstream: parsed, Trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		MaxRequestBytes:   600 << 20,
		ReadHeaderTimeout: config.Duration{Duration: 10 * time.Second}, ReadTimeout: config.Duration{Duration: 30 * time.Minute},
		WriteTimeout: config.Duration{Duration: 30 * time.Minute}, IdleTimeout: config.Duration{Duration: 2 * time.Minute},
		UpstreamResponseHeaderTimeout: config.Duration{Duration: 5 * time.Minute},
	}
}

func TestProxyRebuildsForwardingHeadersAndMarksMutation(t *testing.T) {
	t.Parallel()
	received := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	m := &marker{}
	server := NewServer(proxyConfig(t, upstream.URL), m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPatch, "http://vault.example/api/ciphers/1", strings.NewReader("x"))
	request.RemoteAddr = "127.0.0.1:12345"
	request.Host = "vault.example"
	request.Header.Set("X-Real-IP", "203.0.113.7")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-For", "198.51.100.4")
	request.Header.Set("Forwarded", "for=attacker")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	headers := <-received
	if headers.Get("X-Real-IP") != "203.0.113.7" || headers.Get("X-Forwarded-For") != "203.0.113.7" || headers.Get("X-Forwarded-Proto") != "https" || headers.Get("X-Forwarded-Host") != "vault.example" {
		t.Fatalf("forwarding headers were not rebuilt: %#v", headers)
	}
	if strings.Contains(headers.Get("Forwarded"), "attacker") || !strings.Contains(headers.Get("Forwarded"), "203.0.113.7") {
		t.Fatalf("Forwarded was not rebuilt: %q", headers.Get("Forwarded"))
	}
	if m.count.Load() != 1 {
		t.Fatalf("dirty count = %d", m.count.Load())
	}
}

func TestProxyOnlyMarksSuccessfulMutations(t *testing.T) {
	t.Parallel()
	var status atomic.Int64
	status.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(status.Load())) }))
	defer upstream.Close()
	m := &marker{}
	server := NewServer(proxyConfig(t, upstream.URL), m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, test := range []struct {
		method string
		code   int
		want   int64
	}{
		{http.MethodGet, 200, 0}, {http.MethodPost, 400, 0}, {http.MethodPost, 399, 1}, {http.MethodDelete, 200, 2},
	} {
		status.Store(int64(test.code))
		request := httptest.NewRequest(test.method, "http://vault.example/api/test", nil)
		request.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if got := m.count.Load(); got != test.want {
			t.Fatalf("%s/%d dirty count = %d, want %d", test.method, test.code, got, test.want)
		}
	}
}

func TestProxyRejectsInvalidTrustedHeaderAndOversizeBody(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()
	server := NewServer(proxyConfig(t, upstream.URL), &marker{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPost, "http://vault.example/api", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.Header.Add("X-Forwarded-Proto", "https")
	request.Header.Add("X-Forwarded-Proto", "http")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("multi-value proto status = %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "http://vault.example/api", nil)
	request.RemoteAddr = "127.0.0.1:12345"
	request.ContentLength = 600<<20 + 1
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d", response.Code)
	}
}

func TestEventSenderCoalescesSignals(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var count atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer endpoint.Close()
	parsed, _ := url.Parse(endpoint.URL)
	sender := NewEventSender(parsed, 5*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	sender.Run(ctx, &wg)
	sender.MarkDirty()
	<-started
	for range 1000 {
		sender.MarkDirty()
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for count.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	wg.Wait()
	if got := count.Load(); got < 1 || got > 2 {
		t.Fatalf("event requests = %d, want 1 or 2", got)
	}
}

func TestHostValidationHandlesIPv6AndRejectsAmbiguousForms(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"vault.example", "vault.example:8443", "192.0.2.1", "[2001:db8::1]", "[2001:db8::1]:8443"} {
		if !validHost(host) {
			t.Errorf("valid host rejected: %q", host)
		}
	}
	for _, host := range []string{"", "2001:db8::1", "[192.0.2.1]", "[2001:db8::1", "[2001:db8::1]extra", "vault.example:0", "vault.example:65536"} {
		if validHost(host) {
			t.Errorf("invalid host accepted: %q", host)
		}
	}
}
