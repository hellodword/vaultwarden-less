package notify

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

const apnsProductionEndpoint = "https://api.push.apple.com"

type barkSender struct {
	name        string
	endpoint    string
	deviceToken string
	keyID       string
	teamID      string
	topic       string
	sound       string
	key         *ecdsa.PrivateKey
	client      *http.Client
	mu          sync.Mutex
	jwt         string
	jwtExpiry   time.Time
}

func newBarkSender(cfg config.Notifier) (Sender, error) {
	deviceToken := cfg.DeviceToken
	if len(deviceToken) != 64 {
		return nil, fmt.Errorf("bark device token is invalid")
	}
	if _, err := hex.DecodeString(deviceToken); err != nil || strings.ToLower(deviceToken) != deviceToken {
		return nil, fmt.Errorf("bark device token is invalid")
	}
	block, remainder := pem.Decode([]byte(cfg.AuthKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 {
		return nil, fmt.Errorf("bark APNs key is not PEM")
	}
	if strings.TrimSpace(string(remainder)) != "" {
		return nil, fmt.Errorf("bark APNs key contains trailing data")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("bark APNs key is not PKCS#8")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("bark APNs key is not P-256")
	}
	sound := cfg.Sound
	if sound == "" {
		sound = "1107"
	}
	return &barkSender{
		name: cfg.Name, endpoint: apnsProductionEndpoint, deviceToken: deviceToken,
		keyID: cfg.KeyID, teamID: cfg.TeamID, topic: cfg.Topic, sound: sound, key: key,
		client: strictClient(config.NotifierTimeout),
	}, nil
}

func (s *barkSender) Name() string { return s.name }

func (s *barkSender) Send(ctx context.Context, event Event) error {
	token, err := s.token(time.Now())
	if err != nil {
		return err
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert":    map[string]string{"title": "vaultwarden-less", "body": event.Message()},
			"category": "myNotificationCategory", "mutable-content": 1,
			"sound": s.sound, "thread-id": "vaultwarden-less",
		},
		"badge": "1", "group": "vaultwarden-less", "isarchive": "1",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+"/3/device/"+s.deviceToken, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("apns-topic", s.topic)
	request.Header.Set("apns-push-type", "alert")
	request.Header.Set("authorization", "bearer "+token)
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(responseBody) > maxResponseBytes {
		return fmt.Errorf("APNs response exceeded safe bounds")
	}
	if response.ProtoMajor != 2 {
		return fmt.Errorf("APNs response did not use HTTP/2")
	}
	if response.StatusCode != http.StatusOK {
		var result struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(responseBody, &result)
		if result.Reason == "" || len(result.Reason) > 128 {
			result.Reason = "unknown"
		}
		return fmt.Errorf("APNs returned status %d reason %s", response.StatusCode, result.Reason)
	}
	return nil
}

func (s *barkSender) token(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jwt != "" && now.Before(s.jwtExpiry) {
		return s.jwt, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": s.keyID})
	claims, _ := json.Marshal(map[string]any{"iss": s.teamID, "iat": now.Unix()})
	unsigned := rawBase64(header) + "." + rawBase64(claims)
	digest := sha256.Sum256([]byte(unsigned))
	r, signatureS, err := ecdsa.Sign(rand.Reader, s.key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign APNs token: %w", err)
	}
	signature := append(padInteger(r, 32), padInteger(signatureS, 32)...)
	s.jwt = unsigned + "." + rawBase64(signature)
	s.jwtExpiry = now.Add(50 * time.Minute)
	return s.jwt, nil
}

func rawBase64(value []byte) string { return base64.RawURLEncoding.EncodeToString(value) }

func padInteger(value *big.Int, size int) []byte {
	raw := value.Bytes()
	result := make([]byte, size)
	copy(result[size-len(raw):], raw)
	return result
}

var _ crypto.Signer = (*ecdsa.PrivateKey)(nil)
