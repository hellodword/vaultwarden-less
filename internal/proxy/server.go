package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/routes"
)

const (
	HealthPath    = "/healthz"
	HealthAddress = "127.0.0.1:8081"
	ListenAddress = "0.0.0.0:8080"
	UpstreamURL   = "http://vaultwarden:80"
	EventsURL     = "http://backup:9090/events"

	ReadHeaderTimeout             = 10 * time.Second
	ReadTimeout                   = 30 * time.Minute
	WriteTimeout                  = 30 * time.Minute
	IdleTimeout                   = 2 * time.Minute
	UpstreamResponseHeaderTimeout = 5 * time.Minute
	EventTimeout                  = 2 * time.Second
	EventRetry                    = time.Second
	ShutdownTimeout               = 2 * time.Minute
)

type Server struct {
	upstream *url.URL
	rules    routes.Rules
	record   func()
	log      *slog.Logger
	proxy    *httputil.ReverseProxy
	http     *http.Server
	health   *http.Server
}

func NewServer(rules routes.Rules, record func(), log *slog.Logger) *Server {
	return newServer(mustURL(UpstreamURL), rules, record, log)
}

func newServer(upstream *url.URL, rules routes.Rules, record func(), log *slog.Logger) *Server {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       IdleTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: UpstreamResponseHeaderTimeout,
	}
	rp := &httputil.ReverseProxy{Transport: transport}
	s := &Server{upstream: upstream, rules: rules, record: record, log: log, proxy: rp}
	rp.Rewrite = s.rewrite
	rp.ErrorHandler = s.proxyError
	s.http = &http.Server{
		Addr:              ListenAddress,
		Handler:           s,
		ReadHeaderTimeout: ReadHeaderTimeout,
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    1 << 20,
	}
	s.health = &http.Server{Addr: HealthAddress, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != HealthPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	trigger := s.rules.Trigger(r.Method, r.URL.EscapedPath())
	if s.record != nil && trigger {
		s.record()
		defer s.record()
	}
	response := &responseStatus{ResponseWriter: w}
	completed := false
	defer func() {
		// Errors and interrupted responses remain conservative clues, including
		// handlers normally excluded as reads or temporary authentication work.
		if s.record != nil && !trigger && (!completed || response.code >= 400 || r.Context().Err() != nil) {
			s.record()
		}
	}()
	s.proxy.ServeHTTP(response, r)
	completed = true
}

type responseStatus struct {
	http.ResponseWriter
	code int
}

func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseStatus) WriteHeader(code int) {
	if code >= 200 && w.code == 0 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseStatus) Write(data []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(data)
}

func (s *Server) ListenAndServe() error {
	business, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	defer business.Close()
	health, err := net.Listen("tcp", s.health.Addr)
	if err != nil {
		return err
	}
	defer health.Close()
	done := make(chan error, 2)
	go func() { done <- s.http.Serve(business) }()
	go func() { done <- s.health.Serve(health) }()
	err = <-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	err := errors.Join(s.http.Shutdown(ctx), s.health.Shutdown(ctx))
	if err != nil {
		s.http.Close()
		s.health.Close()
	}
	return err
}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(s.upstream)
	pr.Out.Host = pr.In.Host
	copyForwardingHeaders(pr.Out.Header, pr.In.Header)
}

func (s *Server) proxyError(w http.ResponseWriter, _ *http.Request, _ error) {
	if s.log != nil {
		s.log.Error("upstream request failed", "error_code", "upstream_unavailable")
	}
	writeError(w, http.StatusBadGateway, "internal_error", "Upstream unavailable")
}

func mustURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func removeForwardingHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "forwarded" || lower == "x-real-ip" || strings.HasPrefix(lower, "x-forwarded-") {
			header.Del(name)
		}
	}
}

func copyForwardingHeaders(destination, source http.Header) {
	removeForwardingHeaders(destination)
	for name, values := range source {
		lower := strings.ToLower(name)
		if lower != "forwarded" && lower != "x-real-ip" && !strings.HasPrefix(lower, "x-forwarded-") {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\"error\":{\"code\":%s,\"message\":%s}}\n", strconv.Quote(code), strconv.Quote(message))
}
