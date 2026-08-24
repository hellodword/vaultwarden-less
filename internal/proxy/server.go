package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
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
	ShutdownTimeout               = 2 * time.Minute
)

type DirtyMarker interface {
	MarkDirty()
}

type Server struct {
	upstream *url.URL
	marker   DirtyMarker
	log      *slog.Logger
	proxy    *httputil.ReverseProxy
	http     *http.Server
}

func NewServer(marker DirtyMarker, log *slog.Logger) *Server {
	return newServer(mustURL(UpstreamURL), marker, log)
}

func newServer(upstream *url.URL, marker DirtyMarker, log *slog.Logger) *Server {
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
	s := &Server{upstream: upstream, marker: marker, log: log, proxy: rp}
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

func (s *Server) HTTPServer() *http.Server { return s.http }

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
	if !validHost(r.Host) {
		writeError(w, http.StatusBadRequest, "invalid_host", "Invalid Host header")
		return
	}
	if _, _, err := s.forwardedValues(r); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_forwarded_header", "Invalid forwarding header")
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
	clientIP, proto, _ := s.forwardedValues(pr.In)
	removeForwardingHeaders(pr.Out.Header)
	pr.SetURL(s.upstream)
	pr.Out.Host = pr.In.Host
	pr.Out.Header.Set("X-Real-IP", clientIP.String())
	pr.Out.Header.Set("X-Forwarded-For", clientIP.String())
	pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
	pr.Out.Header.Set("X-Forwarded-Proto", proto)
	pr.Out.Header.Set("Forwarded", formatForwarded(clientIP, proto, pr.In.Host))
}

func (s *Server) modifyResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 400 && isMutating(resp.Request.Method) {
		s.marker.MarkDirty()
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

func (s *Server) forwardedValues(r *http.Request) (netip.Addr, string, error) {
	peer, err := remoteIP(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, "", err
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	if !s.trusts(peer) {
		return peer, proto, nil
	}
	client := peer
	if len(r.Header.Values("X-Real-IP")) > 1 || len(r.Header.Values("X-Forwarded-Proto")) > 1 {
		return netip.Addr{}, "", fmt.Errorf("multiple forwarding header values")
	}
	if raw := r.Header.Get("X-Real-IP"); raw != "" {
		if strings.Contains(raw, ",") || strings.TrimSpace(raw) != raw {
			return netip.Addr{}, "", fmt.Errorf("invalid X-Real-IP")
		}
		parsed, err := netip.ParseAddr(raw)
		if err != nil || parsed.Zone() != "" {
			return netip.Addr{}, "", fmt.Errorf("invalid X-Real-IP")
		}
		client = parsed.Unmap()
	}
	if raw := r.Header.Get("X-Forwarded-Proto"); raw != "" {
		if raw != "http" && raw != "https" {
			return netip.Addr{}, "", fmt.Errorf("invalid X-Forwarded-Proto")
		}
		proto = raw
	}
	return client, proto, nil
}

func (s *Server) trusts(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsPrivate()
}

func mustURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func remoteIP(remote string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid peer address")
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("invalid peer address")
	}
	return addr.Unmap(), nil
}

func removeForwardingHeaders(header http.Header) {
	for name := range header {
		lower := strings.ToLower(name)
		if lower == "forwarded" || lower == "x-real-ip" || strings.HasPrefix(lower, "x-forwarded-") {
			header.Del(name)
		}
	}
}

func formatForwarded(ip netip.Addr, proto, host string) string {
	forValue := ip.String()
	if ip.Is6() {
		forValue = "[" + forValue + "]"
	}
	return "for=" + quoteForwarded(forValue) + ";proto=" + proto + ";host=" + quoteForwarded(host)
}

func quoteForwarded(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func isMutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	default:
		return true
	}
}

func validHost(hostport string) bool {
	if hostport == "" || len(hostport) > 255 || strings.ContainsAny(hostport, " \t\r\n/\\,") {
		return false
	}
	host := hostport
	bracketedIPv6 := false
	if strings.HasPrefix(hostport, "[") {
		closing := strings.IndexByte(hostport, ']')
		if closing < 0 {
			return false
		}
		bracketedIPv6 = true
		if closing == len(hostport)-1 {
			host = hostport[1:closing]
		} else if hostport[closing+1] == ':' {
			var port string
			var err error
			host, port, err = net.SplitHostPort(hostport)
			if err != nil || !validPort(port) {
				return false
			}
		} else {
			return false
		}
	} else if strings.Contains(hostport, ":") {
		if strings.Count(hostport, ":") != 1 {
			return false
		}
		var port string
		var err error
		host, port, err = net.SplitHostPort(hostport)
		if err != nil || !validPort(port) {
			return false
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return false
		}
		return addr.IsValid() && (bracketedIPv6 && addr.Is6() || !bracketedIPv6 && addr.Is4())
	}
	if len(host) > 253 || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if char != '-' && (char < '0' || char > '9') && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
				return false
			}
		}
	}
	return true
}

func validPort(port string) bool {
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\"error\":{\"code\":%s,\"message\":%s}}\n", strconv.Quote(code), strconv.Quote(message))
}
