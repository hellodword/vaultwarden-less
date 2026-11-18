package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/routes"
	"github.com/hellodword/vaultwarden-less/internal/testutil"
)

func testedRules(t *testing.T) routes.Rules {
	t.Helper()
	rules, err := routes.Load("../../rules/routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func TestWebSocketTunnelSurvivesProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		buffer.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		buffer.Flush()
		data := make([]byte, 3)
		if _, err := io.ReadFull(buffer, data); err != nil {
			t.Error(err)
			return
		}
		connection.Write(data)
	}))
	defer upstream.Close()
	marks := make(chan bool, 2)
	proxy := httptest.NewServer(newServer(mustURL(upstream.URL), testedRules(t), func() { marks <- true }, nil))
	defer proxy.Close()
	address, _ := url.Parse(proxy.URL)
	connection, err := net.DialTimeout("tcp", address.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(connection, "GET /unknown-websocket HTTP/1.1\r\nHost: example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101") {
		t.Fatalf("upgrade failed: %v", err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	io.WriteString(connection, "abc")
	data := make([]byte, 3)
	if _, err := io.ReadFull(reader, data); err != nil || string(data) != "abc" {
		t.Fatal("duplex upgrade traffic failed")
	}
	connection.Close()
	for _, want := range []bool{true, true} {
		select {
		case got := <-marks:
			if got != want {
				t.Fatal("upgrade trigger order")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("upgrade end trigger absent")
		}
	}
}

func TestStartEndForErrorsUnknownAndInterruptedRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/partial" {
			conn, buf, _ := w.(http.Hijacker).Hijack()
			buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 999\r\n\r\npartial")
			buf.Flush()
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	var mu sync.Mutex
	var signals int
	server := newServer(mustURL(upstream.URL), testedRules(t), func() { mu.Lock(); defer mu.Unlock(); signals++ }, nil)
	for _, path := range []string{"/unknown", "/partial", "/healthz"} {
		func() {
			defer func() {
				if value := recover(); value != nil && value != http.ErrAbortHandler {
					panic(value)
				}
			}()
			server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		}()
	}
	if signals != 6 {
		t.Fatalf("got %d write signals", signals)
	}
	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/sync", nil))
	if signals != 7 {
		t.Fatal("failed read request did not trigger conservative check")
	}
	upstream.Close()
	server.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/identity/connect/token", nil))
	if signals != 9 {
		t.Fatal("connection failure not triggered")
	}
}

func TestProxyDoesNotWaitForBackupAndPreservesStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	sender := NewEventSender(nil)
	server := httptest.NewServer(newServer(mustURL(upstream.URL), testedRules(t), sender.Record, nil))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/stream", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data := make([]byte, 6)
	if _, err := io.ReadFull(response.Body, data); err != nil || string(data) != "first\n" {
		t.Fatal("stream did not arrive before upstream completion")
	}
	cancel()
}

func TestEventFailuresRetainBoundedCounts(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var accepted bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		accepted = r.ContentLength == 0
		w.WriteHeader(204)
	}))
	defer endpoint.Close()
	clock := testutil.NewClock()
	sender := NewEventSender(nil)
	sender.endpoint = endpoint.URL
	sender.clock = clock
	for range 50 {
		sender.Record()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sender.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first submission absent")
		}
	}
	sender.Record()
	for {
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry absent")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !accepted {
		t.Fatal("failed event counts lost or raw paths retained")
	}
}

func TestShutdownDrainsWriteThenFlushesItsEndEvent(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	var accepted bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted = r.ContentLength == 0
		w.WriteHeader(http.StatusNoContent)
	}))
	defer peer.Close()
	sender := NewEventSender(nil)
	sender.endpoint = peer.URL
	server := newServer(mustURL(upstream.URL), testedRules(t), sender.Record, nil)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go server.http.Serve(listener)
	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Post("http://"+listener.Addr().String()+"/api/ciphers", "application/json", strings.NewReader("{}"))
		if err == nil {
			response.Body.Close()
		}
		requestDone <- err
	}()
	<-entered
	shutdownDone := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { shutdownDone <- server.Shutdown(ctx) }()
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if err := sender.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("shutdown signal was not delivered")
	}
}
