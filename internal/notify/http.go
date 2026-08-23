package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/securefile"
)

const maxResponseBytes = 64 << 10

type Sender interface {
	Name() string
	Send(context.Context, Event) error
}

type webhookSender struct {
	name    string
	url     string
	headers http.Header
	client  *http.Client
}

func newWebhookSender(cfg config.Notifier) (Sender, error) {
	headers := make(http.Header, len(cfg.HeadersFromFiles))
	for name, path := range cfg.HeadersFromFiles {
		value, err := securefile.Read(path)
		if err != nil || strings.ContainsAny(value, "\r\n") || len(value) > 8192 {
			return nil, fmt.Errorf("webhook secret header file is invalid")
		}
		headers.Set(name, value)
	}
	return &webhookSender{name: cfg.Name, url: cfg.URL, headers: headers, client: strictClient(cfg.Timeout.Duration)}, nil
}

func (s *webhookSender) Name() string { return s.name }

func (s *webhookSender) Send(ctx context.Context, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "vaultwarden-less/1")
	for name, values := range s.headers {
		request.Header[name] = append([]string(nil), values...)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if err := drainBounded(response.Body); err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", response.StatusCode)
	}
	return nil
}

type wechatSender struct {
	name   string
	url    string
	client *http.Client
}

func newWechatSender(cfg config.Notifier) (Sender, error) {
	rawURL, err := securefile.Read(cfg.URLFile)
	if err != nil {
		return nil, fmt.Errorf("WeChat URL file is invalid")
	}
	if len(rawURL) > 4096 || strings.ContainsAny(rawURL, "\r\n") {
		return nil, fmt.Errorf("WeChat URL file does not contain a bounded URL")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("WeChat URL file does not contain an HTTPS URL")
	}
	return &wechatSender{name: cfg.Name, url: rawURL, client: strictClient(cfg.Timeout.Duration)}, nil
}

func (s *wechatSender) Name() string { return s.name }

func (s *wechatSender) Send(ctx context.Context, event Event) error {
	payload := struct {
		MsgType string `json:"msgtype"`
		Text    struct {
			Content string `json:"content"`
		} `json:"text"`
	}{MsgType: "text"}
	payload.Text.Content = event.Message()
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil || len(responseBody) > maxResponseBytes {
		return fmt.Errorf("WeChat response exceeded safe bounds")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("WeChat returned status %d", response.StatusCode)
	}
	var result struct {
		ErrCode int `json:"errcode"`
	}
	if err := json.Unmarshal(responseBody, &result); err != nil || result.ErrCode != 0 {
		return fmt.Errorf("WeChat returned a failure result")
	}
	return nil
}

func strictClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func drainBounded(body io.Reader) error {
	read, err := io.Copy(io.Discard, io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if read > maxResponseBytes {
		return fmt.Errorf("notification response exceeded safe bounds")
	}
	return nil
}
