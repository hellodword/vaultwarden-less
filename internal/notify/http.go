package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const notifierTimeout = 10 * time.Second

type Sender interface {
	Name() string
	Send(context.Context, Event) error
}
type DeliveryError struct {
	Code      string
	Permanent bool
	After     time.Duration
}

func (e *DeliveryError) Error() string { return e.Code }

type webhookSender struct {
	cfg    config.Notifier
	client *http.Client
	now    func() time.Time
}

func (s *webhookSender) Name() string { return s.cfg.Name }

func strictClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = true
	return &http.Client{Transport: transport, Timeout: notifierTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func signature(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func boundedPayload(e Event, textLimit, payloadLimit int, encode func(string) any) ([]byte, error) {
	for limit := textLimit; limit >= 256; limit -= 64 {
		message := e.Message(limit)
		body, err := json.Marshal(encode(message))
		if err != nil {
			return nil, err
		}
		if len(message) <= textLimit && len(body) <= payloadLimit {
			return body, nil
		}
	}
	return nil, &DeliveryError{Code: "payload_too_large", Permanent: true}
}

func (s *webhookSender) Send(ctx context.Context, event Event) error {
	now := s.now()
	endpoint := s.cfg.WebhookURL
	textLimit, payloadLimit := 20000, 20000
	if s.cfg.Type == "wecom" {
		textLimit = 4096
		payloadLimit = 16000
	} else if s.cfg.Type == "dingtalk" {
		// Keep a conservative text budget separately from the encoded envelope.
		textLimit = 4000
	}
	if s.cfg.Type == "dingtalk" && s.cfg.Secret != "" {
		u, err := url.Parse(endpoint)
		if err != nil {
			return &DeliveryError{Code: "invalid_webhook", Permanent: true}
		}
		ts := strconv.FormatInt(now.UnixMilli(), 10)
		q := u.Query()
		q.Set("timestamp", ts)
		q.Set("sign", signature(s.cfg.Secret, ts+"\n"+s.cfg.Secret))
		u.RawQuery = q.Encode()
		endpoint = u.String()
	}
	body, err := boundedPayload(event, textLimit, payloadLimit, func(message string) any {
		if s.cfg.Type == "feishu" {
			value := map[string]any{"msg_type": "interactive", "card": map[string]any{
				"schema": "2.0",
				"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": "vaultwarden-less · " + event.Title()}},
				"body":   map[string]any{"elements": []any{map[string]string{"tag": "markdown", "content": message}}},
			}}
			if s.cfg.Secret != "" {
				ts := strconv.FormatInt(now.Unix(), 10)
				value["timestamp"] = ts
				value["sign"] = signature(ts+"\n"+s.cfg.Secret, "")
			}
			return value
		}
		if s.cfg.Type == "dingtalk" {
			return map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": "vaultwarden-less · " + event.Title(), "text": message}}
		}
		return map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": message}}
	})
	if err != nil {
		return err
	}
	response, data, err := post(ctx, s.client, endpoint, body, nil)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return &DeliveryError{Code: "webhook_authentication_failed", Permanent: true, After: retryAfter(response, now)}
		}
		return &DeliveryError{Code: "webhook_http_failure", After: retryAfter(response, now)}
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(data, &result) != nil {
		return &DeliveryError{Code: "webhook_invalid_result"}
	}
	key := "errcode"
	if s.cfg.Type == "feishu" {
		key = "code"
	}
	raw, ok := result[key]
	var number json.Number
	if !ok || len(raw) == 0 || bytes.Equal(raw, []byte("null")) || raw[0] == '"' || json.Unmarshal(raw, &number) != nil {
		return &DeliveryError{Code: "webhook_missing_success_code"}
	}
	// Compare the numeric mantissa exactly. Floating-point underflow must not
	// turn a nonzero server code such as 1e-9999 into a success.
	mantissa, _, _ := strings.Cut(strings.ToLower(number.String()), "e")
	if strings.Trim(mantissa, "-0.") != "" {
		return &DeliveryError{Code: "webhook_rejected"}
	}
	return nil
}

func post(ctx context.Context, client *http.Client, endpoint string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, &DeliveryError{Code: "notification_request_failed"}
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, nil, &DeliveryError{Code: "notification_network_failed"}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, nil, &DeliveryError{Code: "notification_response_failed"}
	}
	return response, data, nil
}

func retryAfter(response *http.Response, now time.Time) time.Duration {
	value := response.Header.Get("Retry-After")
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

func senderForConfig(cfg config.Notifier) (Sender, error) {
	if cfg.Error != "" {
		return nil, errors.New(cfg.Error)
	}
	switch cfg.Type {
	case "wecom", "feishu", "dingtalk":
		return &webhookSender{cfg: cfg, client: strictClient(), now: time.Now}, nil
	case "bark":
		if cfg.Key == nil {
			return nil, errors.New("invalid_auth_key")
		}
		return &barkSender{cfg: cfg, client: strictClient(), now: time.Now}, nil
	}
	return nil, errors.New("invalid_notifier_type")
}
