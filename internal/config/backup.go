package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Backup is the complete human-authored configuration. Scheduling, retry,
// timeout, retention, filesystem, and HTTP behavior are product constants.
type Backup struct {
	Targets   []Target
	Notifiers []Notifier
}

type backupDocument struct {
	Targets   []Target           `yaml:"targets"`
	Notifiers []notifierDocument `yaml:"notifiers"`
}

type Target struct {
	Name       string   `yaml:"name"`
	Repository string   `yaml:"repository"`
	Options    []Option `yaml:"options"`
}

type Option struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type Notifier struct {
	Name string
	Type string

	WebhookURL string

	DeviceToken string
	AuthKey     string
	KeyID       string
	TeamID      string
	Topic       string
	Sound       string
}

type notifierDocument struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`

	WebhookURL *string `yaml:"webhook_url"`

	DeviceToken *string `yaml:"device_token"`
	AuthKey     *string `yaml:"auth_key"`
	KeyID       *string `yaml:"key_id"`
	TeamID      *string `yaml:"team_id"`
	Topic       *string `yaml:"topic"`
	Sound       *string `yaml:"sound"`
}

func LoadBackup(path string) (*Backup, error) {
	data, err := privatefs.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read backup config: %w", err)
	}
	return DecodeBackup(data)
}

func DecodeBackup(data []byte) (*Backup, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("backup config exceeds %d bytes", MaxConfigBytes)
	}
	var document backupDocument
	if err := decodeOneYAML(data, &document); err != nil {
		if errors.Is(err, errUnknownYAMLField) {
			return nil, fmt.Errorf("decode backup config: unknown field")
		}
		return nil, fmt.Errorf("decode backup config: invalid YAML")
	}
	if document.Targets == nil || len(document.Targets) > 32 {
		return nil, fmt.Errorf("validate backup config: targets must be an array containing 0 to 32 entries")
	}
	if document.Notifiers == nil || len(document.Notifiers) > 16 {
		return nil, fmt.Errorf("validate backup config: notifiers must be an array containing 0 to 16 entries")
	}

	result := &Backup{Targets: document.Targets, Notifiers: make([]Notifier, 0, len(document.Notifiers))}
	targetNames := make(map[string]struct{}, len(result.Targets))
	for index := range result.Targets {
		if err := result.Targets[index].Validate(); err != nil {
			return nil, fmt.Errorf("validate backup config: targets[%d]: %w", index, err)
		}
		if _, duplicate := targetNames[result.Targets[index].Name]; duplicate {
			return nil, fmt.Errorf("validate backup config: target names must be unique")
		}
		targetNames[result.Targets[index].Name] = struct{}{}
	}

	notifierNames := make(map[string]struct{}, len(document.Notifiers))
	for index := range document.Notifiers {
		notifier, err := document.Notifiers[index].resolve()
		if err != nil {
			return nil, fmt.Errorf("validate backup config: notifiers[%d]: %w", index, err)
		}
		if _, duplicate := notifierNames[notifier.Name]; duplicate {
			return nil, fmt.Errorf("validate backup config: notifier names must be unique")
		}
		notifierNames[notifier.Name] = struct{}{}
		result.Notifiers = append(result.Notifiers, notifier)
	}
	return result, nil
}

func (c Backup) Target(name string) (Target, bool) {
	for _, target := range c.Targets {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

func (t Target) Validate() error {
	if !namePattern.MatchString(t.Name) {
		return fmt.Errorf("invalid name %q", t.Name)
	}
	if len(t.Repository) < 1 || len(t.Repository) > 4096 || strings.ContainsAny(t.Repository, "\r\n\x00") {
		return fmt.Errorf("repository must be a nonempty string of at most 4096 bytes")
	}
	if t.Options == nil || len(t.Options) > 64 {
		return fmt.Errorf("options must be an array containing at most 64 entries")
	}
	optionNames := make(map[string]struct{}, len(t.Options))
	for index, option := range t.Options {
		if len(option.Name) < 1 || len(option.Name) > 256 || strings.ContainsAny(option.Name, "=\r\n\x00") {
			return fmt.Errorf("options[%d] has an invalid name", index)
		}
		if _, duplicate := optionNames[option.Name]; duplicate {
			return fmt.Errorf("option names must be unique")
		}
		optionNames[option.Name] = struct{}{}
		if len(option.Value) > 4096 || strings.ContainsAny(option.Value, "\r\n\x00") {
			return fmt.Errorf("options[%d] has an invalid value", index)
		}
	}
	return nil
}

func (document notifierDocument) resolve() (Notifier, error) {
	if !namePattern.MatchString(document.Name) {
		return Notifier{}, fmt.Errorf("invalid name %q", document.Name)
	}
	notifier := Notifier{Name: document.Name, Type: document.Type}
	switch document.Type {
	case "wechat":
		if document.WebhookURL == nil || document.DeviceToken != nil || document.AuthKey != nil || document.KeyID != nil ||
			document.TeamID != nil || document.Topic != nil || document.Sound != nil {
			return Notifier{}, fmt.Errorf("fields are invalid for WeChat")
		}
		if _, err := parseHTTPSURL(*document.WebhookURL); err != nil {
			return Notifier{}, fmt.Errorf("webhook_url must be an HTTPS URL without user info or fragment")
		}
		notifier.WebhookURL = *document.WebhookURL
	case "bark":
		if document.WebhookURL != nil || document.DeviceToken == nil || document.AuthKey == nil || document.KeyID == nil ||
			document.TeamID == nil || document.Topic == nil {
			return Notifier{}, fmt.Errorf("bark fields are incomplete or invalid")
		}
		notifier.DeviceToken = *document.DeviceToken
		notifier.AuthKey = *document.AuthKey
		notifier.KeyID = *document.KeyID
		notifier.TeamID = *document.TeamID
		notifier.Topic = *document.Topic
		if document.Sound != nil {
			notifier.Sound = *document.Sound
		}
		if notifier.Sound == "" {
			notifier.Sound = "1107"
		}
		if err := validateBark(&notifier); err != nil {
			return Notifier{}, err
		}
	default:
		return Notifier{}, fmt.Errorf("unsupported type %q", document.Type)
	}
	return notifier, nil
}

func validateBark(notifier *Notifier) error {
	if len(notifier.DeviceToken) != 64 || strings.ToLower(notifier.DeviceToken) != notifier.DeviceToken {
		return fmt.Errorf("device_token must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(notifier.DeviceToken); err != nil {
		return fmt.Errorf("device_token must be 64 lowercase hexadecimal characters")
	}
	if len(notifier.AuthKey) < 1 || len(notifier.AuthKey) > 16<<10 || strings.IndexByte(notifier.AuthKey, 0) >= 0 {
		return fmt.Errorf("auth_key is invalid")
	}
	block, rest := pem.Decode([]byte(notifier.AuthKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("auth_key must contain one PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		return fmt.Errorf("auth_key must be a P-256 PKCS#8 private key")
	}
	if _, err := key.ECDH(); err != nil {
		return fmt.Errorf("auth_key must be a P-256 PKCS#8 private key")
	}
	for _, field := range []struct {
		name  string
		value string
	}{{"key_id", notifier.KeyID}, {"team_id", notifier.TeamID}, {"topic", notifier.Topic}} {
		if len(field.value) < 2 || len(field.value) > 255 || strings.ContainsAny(field.value, "\r\n\x00") {
			return fmt.Errorf("%s is invalid", field.name)
		}
	}
	if len(notifier.Sound) > 255 || strings.ContainsAny(notifier.Sound, "\r\n\x00") {
		return fmt.Errorf("sound is invalid")
	}
	return nil
}

func parseHTTPSURL(raw string) (*url.URL, error) {
	if len(raw) < 1 || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("URL is invalid")
	}
	return parsed, nil
}
