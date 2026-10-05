package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const apnsProductionEndpoint = "https://api.push.apple.com"

type barkSender struct {
	cfg       config.Notifier
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	jwt       string
	jwtExpiry time.Time
}

func (s *barkSender) Name() string { return s.cfg.Name }
func (s *barkSender) Send(ctx context.Context, event Event) error {
	now := s.now()
	token, err := s.token(now)
	if err != nil {
		return err
	}
	body, err := boundedPayload(event, 3800, 4096, func(message string) any {
		return map[string]any{
			"aps":   map[string]any{"alert": map[string]string{"title": "vaultwarden-less · " + event.Title(), "body": event.PlainSummary()}, "category": "myNotificationCategory", "mutable-content": 1, "sound": s.cfg.Sound, "thread-id": "vaultwarden-less"},
			"badge": "1", "group": "vaultwarden-less", "isarchive": "1",
			"markdown": message,
		}
	})
	if err != nil {
		return err
	}
	response, _, err := post(ctx, s.client, apnsProductionEndpoint+"/3/device/"+s.cfg.DeviceToken, body, map[string]string{"apns-topic": s.cfg.Topic, "apns-push-type": "alert", "authorization": "bearer " + token})
	if err != nil {
		return err
	}
	if response.ProtoMajor != 2 {
		return &DeliveryError{Code: "apns_requires_http2"}
	}
	if response.StatusCode == http.StatusOK {
		return nil
	}
	after := retryAfter(response, now)
	if response.StatusCode >= 500 {
		return &DeliveryError{Code: "apns_server_failure", After: max(after, 15*time.Minute)}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return &DeliveryError{Code: "apns_rate_limited", After: max(after, time.Minute)}
	}
	return &DeliveryError{Code: "apns_configuration_or_payload_rejected", Permanent: true}
}

func (s *barkSender) token(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jwt != "" && now.Before(s.jwtExpiry) {
		return s.jwt, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.cfg.KeyID})
	claims, _ := json.Marshal(map[string]any{"iss": s.cfg.TeamID, "iat": now.Unix()})
	encode := base64.RawURLEncoding.EncodeToString
	unsigned := encode(header) + "." + encode(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, ss, err := ecdsa.Sign(rand.Reader, s.cfg.Key, digest[:])
	if err != nil {
		return "", &DeliveryError{Code: "apns_signing_failed", Permanent: true}
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	s.jwt = unsigned + "." + encode(signature)
	s.jwtExpiry = now.Add(50 * time.Minute)
	return s.jwt, nil
}
