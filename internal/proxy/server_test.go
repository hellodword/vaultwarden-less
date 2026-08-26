package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type marker struct{ count atomic.Int64 }

func (marker *marker) MarkDirty() { marker.count.Add(1) }

func proxyServer(t *testing.T, upstream string, dirty DirtyMarker) *Server {
	t.Helper()
	parsed, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	return newServer(parsed, dirty, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func TestTransparentProxyPreservesResponseAndForwardingHeaders(t *testing.T) {
	t.Parallel()
	received := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		clone := request.Clone(request.Context())
		clone.Header = request.Header.Clone()
		received <- clone
		writer.Header().Set("X-Upstream", "preserved")
		writer.WriteHeader(http.StatusMultiStatus)
		_, _ = writer.Write([]byte("exact upstream body"))
	}))
	defer upstream.Close()
	marked := &marker{}
	server := proxyServer(t, upstream.URL, marked)
	request := httptest.NewRequest(http.MethodPatch, "http://vault.example/api/ciphers/item?revision=7", strings.NewReader("request body"))
	request.Host = "vault.example"
	request.Header.Set("Forwarded", `for="203.0.113.7";proto=https;host="vault.example"`)
	request.Header.Set("X-Real-IP", "203.0.113.7")
	request.Header.Add("X-Forwarded-For", "203.0.113.7")
	request.Header.Add("X-Forwarded-For", "198.51.100.9")
	request.Header.Set("X-Forwarded-Host", "vault.example")
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Port", "443")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusMultiStatus || response.Body.String() != "exact upstream body" || response.Header().Get("X-Upstream") != "preserved" {
		t.Fatalf("proxied response = %d/%q/%q", response.Code, response.Body.String(), response.Header().Get("X-Upstream"))
	}
	forwarded := <-received
	if forwarded.Host != "vault.example" || forwarded.URL.RawQuery != "revision=7" {
		t.Fatalf("host/query = %q/%q", forwarded.Host, forwarded.URL.RawQuery)
	}
	for _, name := range []string{"Forwarded", "X-Real-IP", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port"} {
		if got, want := forwarded.Header.Values(name), request.Header.Values(name); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s = %#v, want %#v", name, got, want)
		}
	}
	if got := strings.Join(forwarded.Header.Values("X-Forwarded-For"), "|"); got != "203.0.113.7|198.51.100.9" {
		t.Fatalf("X-Forwarded-For = %q", got)
	}
	if marked.count.Load() != 1 {
		t.Fatalf("backup marks = %d", marked.count.Load())
	}
}

func TestProxyTriggerUsesPathNotQueryAndOnlyTwoXX(t *testing.T) {
	t.Parallel()
	var status atomic.Int64
	status.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(int(status.Load()))
	}))
	defer upstream.Close()
	marked := &marker{}
	server := proxyServer(t, upstream.URL, marked)
	for _, test := range []struct {
		method string
		path   string
		code   int
		want   int64
	}{
		{http.MethodPost, "/api/ciphers?next=/api/accounts/prelogin", 200, 1},
		{http.MethodPost, "/api/accounts/prelogin?write=true", 200, 1},
		{http.MethodDelete, "/api/ciphers/item", 299, 2},
		{http.MethodPost, "/api/ciphers", 300, 2},
		{http.MethodPost, "/api/ciphers", 500, 2},
		{http.MethodGet, "/api/ciphers", 200, 2},
	} {
		status.Store(int64(test.code))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(test.method, "http://vault.example"+test.path, nil))
		if marked.count.Load() != test.want {
			t.Fatalf("%s %s/%d marks = %d, want %d", test.method, test.path, test.code, marked.count.Load(), test.want)
		}
	}
}

func TestProxyBodyLimitAndHealthAreLocal(t *testing.T) {
	t.Parallel()
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server := proxyServer(t, upstream.URL, &marker{})

	request := httptest.NewRequest(http.MethodPost, "http://vault.example/api/ciphers", nil)
	request.ContentLength = MaxRequestBytes + 1
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || upstreamCalls.Load() != 0 {
		t.Fatalf("oversize response/calls = %d/%d", response.Code, upstreamCalls.Load())
	}

	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://vault.example"+HealthPath, nil))
	if response.Code != http.StatusNoContent || upstreamCalls.Load() != 0 {
		t.Fatalf("health response/calls = %d/%d", response.Code, upstreamCalls.Load())
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://vault.example"+HealthPath, nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("health method response = %d/%q", response.Code, response.Header().Get("Allow"))
	}
}

func TestTransparentStreamingResponse(t *testing.T) {
	firstWritten := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("first"))
		writer.(http.Flusher).Flush()
		close(firstWritten)
		<-release
		_, _ = writer.Write([]byte("second"))
	}))
	defer upstream.Close()
	proxyHTTP := httptest.NewServer(proxyServer(t, upstream.URL, &marker{}))
	defer proxyHTTP.Close()

	response, err := proxyHTTP.Client().Get(proxyHTTP.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	<-firstWritten
	first := make([]byte, 5)
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
		t.Fatalf("first streamed chunk = %q/%v", first, err)
	}
	close(release)
	remainder, err := io.ReadAll(response.Body)
	if err != nil || string(remainder) != "second" {
		t.Fatalf("remaining stream = %q/%v", remainder, err)
	}
}

func TestTransparentWebSocketUpgrade(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		connection, buffer, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = buffer.Flush()
		line, _ := buffer.ReadString('\n')
		_, _ = buffer.WriteString("echo:" + line)
		_ = buffer.Flush()
	}))
	defer upstream.Close()
	proxyHTTP := httptest.NewServer(proxyServer(t, upstream.URL, &marker{}))
	defer proxyHTTP.Close()
	parsed, _ := url.Parse(proxyHTTP.URL)
	connection, err := net.DialTimeout("tcp", parsed.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprintf(connection, "GET /notifications/hub HTTP/1.1\r\nHost: vault.example\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(connection)
	request := &http.Request{Method: http.MethodGet}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", response.StatusCode)
	}
	_, _ = connection.Write([]byte("ping\n"))
	line, err := reader.ReadString('\n')
	if err != nil || line != "echo:ping\n" {
		t.Fatalf("upgrade echo = %q/%v", line, err)
	}
}

func TestEventSenderRetriesStartupPendingUntilSuccess(t *testing.T) {
	t.Parallel()
	var count atomic.Int64
	succeeded := make(chan struct{})
	var once sync.Once
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := count.Add(1)
		body, _ := io.ReadAll(request.Body)
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" || string(body) != string(eventBody) {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if attempt < 3 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
		once.Do(func() { close(succeeded) })
	}))
	defer endpoint.Close()
	parsed, _ := url.Parse(endpoint.URL)
	sender := newEventSender(parsed, time.Second, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	sender.retry = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	sender.Run(ctx, &wait)
	select {
	case <-succeeded:
	case <-time.After(2 * time.Second):
		t.Fatal("pending startup signal was not delivered")
	}
	cancel()
	wait.Wait()
	if count.Load() != 3 {
		t.Fatalf("event attempts = %d, want 3", count.Load())
	}
}

func TestEventSenderRetainsSignalArrivingDuringSend(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	second := make(chan struct{})
	var count atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempt := count.Add(1)
		if attempt == 1 {
			close(started)
			<-release
		}
		writer.WriteHeader(http.StatusNoContent)
		if attempt == 2 {
			close(second)
		}
	}))
	defer endpoint.Close()
	parsed, _ := url.Parse(endpoint.URL)
	sender := newEventSender(parsed, time.Second, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	sender.Run(ctx, &wait)
	<-started
	for range 1000 {
		sender.MarkDirty()
	}
	close(release)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("signal arriving during delivery was lost")
	}
	cancel()
	wait.Wait()
	if count.Load() != 2 {
		t.Fatalf("coalesced event attempts = %d, want 2", count.Load())
	}
}

func TestEventFailureCannotChangeUpstreamResponse(t *testing.T) {
	t.Parallel()
	events := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer events.Close()
	eventsURL, _ := url.Parse(events.URL)
	sender := newEventSender(eventsURL, 50*time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	sender.retry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	sender.Run(ctx, &wait)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Exact", "yes")
		writer.WriteHeader(http.StatusAlreadyReported)
		_, _ = writer.Write([]byte("unchanged"))
	}))
	defer upstream.Close()
	response := httptest.NewRecorder()
	proxyServer(t, upstream.URL, sender).ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "http://vault.example/api/ciphers/item", nil))
	cancel()
	wait.Wait()
	if response.Code != http.StatusAlreadyReported || response.Header().Get("X-Exact") != "yes" || response.Body.String() != "unchanged" {
		t.Fatalf("event failure changed response = %d/%q/%q", response.Code, response.Header().Get("X-Exact"), response.Body.String())
	}
}
