package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const maxResponseBytes = 64 << 10

type Sender interface {
	Name() string
	Send(context.Context, Event) error
}

type wechatSender struct {
	name   string
	url    string
	client *http.Client
}

func newWechatSender(cfg config.Notifier) (Sender, error) {
	parsed, err := url.Parse(cfg.WebhookURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("WeChat webhook URL is invalid")
	}
	return &wechatSender{name: cfg.Name, url: cfg.WebhookURL, client: strictClient(config.NotifierTimeout)}, nil
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
	defer func() { _ = response.Body.Close() }()
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
