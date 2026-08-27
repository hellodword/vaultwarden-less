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
)

const (
	HealthPath    = "/.vaultwarden-less/healthz"
	ListenAddress = "0.0.0.0:8080"
	UpstreamURL   = "http://vaultwarden:80"
	EventsURL     = "http://backup:9090/v1/events"

	MaxRequestBytes               = int64(600 << 20)
	ReadHeaderTimeout             = 10 * time.Second
	ReadTimeout                   = 30 * time.Minute
	WriteTimeout                  = 30 * time.Minute
	IdleTimeout                   = 2 * time.Minute
	UpstreamResponseHeaderTimeout = 5 * time.Minute
	EventTimeout                  = 2 * time.Second
	EventRetry                    = 30 * time.Second
	ShutdownTimeout               = 2 * time.Minute
)

type Server struct {
	upstream  *url.URL
	markDirty func()
	log       *slog.Logger
	proxy     *httputil.ReverseProxy
	http      *http.Server
}

func NewServer(markDirty func(), log *slog.Logger) *Server {
	return newServer(mustURL(UpstreamURL), markDirty, log)
}

func newServer(upstream *url.URL, markDirty func(), log *slog.Logger) *Server {
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
	s := &Server{upstream: upstream, markDirty: markDirty, log: log, proxy: rp}
	rp.Rewrite = s.rewrite
	rp.ModifyResponse = s.modifyResponse
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
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == HealthPath {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.ContentLength > MaxRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 600 MiB")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) ListenAndServe() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(s.upstream)
	pr.Out.Host = pr.In.Host
	copyForwardingHeaders(pr.Out.Header, pr.In.Header)
}

func (s *Server) modifyResponse(resp *http.Response) error {
	if s.markDirty != nil && shouldRequestBackup(resp.Request.Method, resp.Request.URL.Path, resp.StatusCode) {
		s.markDirty()
	}
	return nil
}

func (s *Server) proxyError(w http.ResponseWriter, _ *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Request body exceeds 600 MiB")
		return
	}
	s.log.Error("upstream request failed", "error_code", "upstream_unavailable")
	writeError(w, http.StatusBadGateway, "upstream_unavailable", "Upstream unavailable")
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
