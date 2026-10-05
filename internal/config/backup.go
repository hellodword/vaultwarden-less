package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/docker/cli/pkg/kvfile"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

const (
	MaxConfigBytes          = 1 << 20
	DefaultBackupConfigPath = "/config/backup.yaml"
	WorkerURL               = "http://backup:9090"
	DataDir                 = "/data"
	CacheDir                = "/cache"
)

type Backup struct {
	Targets   []Target   `json:"targets"`
	Notifiers []Notifier `json:"notifiers"`
}

type Target struct {
	Name        string   `yaml:"name" json:"name"`
	EnvFile     string   `yaml:"env_file" json:"-"`
	Options     []string `yaml:"options" json:"-"`
	Environment []string `yaml:"-" json:"-"`
	Error       string   `yaml:"-" json:"error,omitempty"`
}

type Notifier struct {
	Name        string            `yaml:"name" json:"name"`
	Type        string            `yaml:"type" json:"type"`
	WebhookURL  string            `yaml:"webhook_url" json:"-"`
	Secret      string            `yaml:"secret" json:"-"`
	DeviceToken string            `yaml:"device_token" json:"-"`
	AuthKeyFile string            `yaml:"auth_key_file" json:"-"`
	KeyID       string            `yaml:"key_id" json:"-"`
	TeamID      string            `yaml:"team_id" json:"-"`
	Topic       string            `yaml:"topic" json:"-"`
	Sound       string            `yaml:"sound" json:"-"`
	Key         *ecdsa.PrivateKey `yaml:"-" json:"-"`
	Error       string            `yaml:"-" json:"error,omitempty"`
}

// DecodeBackup rejects unknown root keys, while each invalid item remains a
// disabled entry. An invalid destination never takes down the other workers.
func DecodeBackup(data []byte) (*Backup, error) {
	if len(data) > MaxConfigBytes {
		return nil, errors.New("configuration too large")
	}
	file, err := parser.ParseBytes(data, 0)
	if err != nil || len(file.Docs) != 1 || file.Docs[0].Body == nil {
		return nil, errors.New("invalid YAML document")
	}
	var doc struct {
		Targets   []any `yaml:"targets"`
		Notifiers []any `yaml:"notifiers"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(data), yaml.Strict()).Decode(&doc); err != nil {
		return nil, errors.New("invalid configuration document or unknown root key")
	}
	result := &Backup{Targets: []Target{}, Notifiers: []Notifier{}}
	for i, raw := range doc.Targets {
		target := Target{}
		encoded, err := yaml.Marshal(raw)
		if err != nil || decodeOneYAML(encoded, &target) != nil {
			target.Error = "invalid_target_config"
		}
		if strings.TrimSpace(target.Name) == "" {
			target.Name = fmt.Sprintf("targets[%d]", i)
			target.Error = "name_required"
		}
		if strings.TrimSpace(target.EnvFile) == "" {
			target.Error = "env_file_required"
		}
		for _, option := range target.Options {
			if option == "" || strings.ContainsRune(option, 0) {
				target.Error = "invalid_restic_option"
			}
		}
		result.Targets = append(result.Targets, target)
	}
	for i, raw := range doc.Notifiers {
		notifier := Notifier{Sound: "1107"}
		encoded, err := yaml.Marshal(raw)
		if err != nil || decodeOneYAML(encoded, &notifier) != nil {
			notifier.Error = "invalid_notifier_config"
		}
		if strings.TrimSpace(notifier.Name) == "" {
			notifier.Name = fmt.Sprintf("notifiers[%d]", i)
			notifier.Error = "name_required"
		}
		var allowed map[string]bool
		switch notifier.Type {
		case "wecom", "feishu", "dingtalk":
			allowed = map[string]bool{"name": true, "type": true, "webhook_url": true}
			if notifier.Type != "wecom" {
				allowed["secret"] = true
			}
			u, err := url.Parse(notifier.WebhookURL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
				notifier.Error = "invalid_webhook_url"
			}
		case "bark":
			allowed = map[string]bool{"name": true, "type": true, "device_token": true, "auth_key_file": true, "key_id": true, "team_id": true, "topic": true, "sound": true}
			for _, value := range []string{notifier.DeviceToken, notifier.AuthKeyFile, notifier.KeyID, notifier.TeamID, notifier.Topic} {
				if value == "" || strings.ContainsAny(value, "\r\n\x00") {
					notifier.Error = "invalid_bark_config"
				}
			}
			if strings.ContainsAny(notifier.DeviceToken, "/?#% ") {
				notifier.Error = "invalid_device_token"
			}
		default:
			notifier.Error = "unknown_notifier_type"
		}
		var keys map[string]any
		if yaml.Unmarshal(encoded, &keys) != nil {
			notifier.Error = "invalid_notifier_config"
		}
		for key := range keys {
			if !allowed[key] {
				notifier.Error = "invalid_notifier_field"
			}
		}
		result.Notifiers = append(result.Notifiers, notifier)
	}
	for i := range result.Targets {
		for j := 0; j < i; j++ {
			if result.Targets[i].Name == result.Targets[j].Name {
				result.Targets[i].Error = "duplicate_name"
				result.Targets[j].Error = "duplicate_name"
			}
		}
	}
	for i := range result.Notifiers {
		for j := 0; j < i; j++ {
			if result.Notifiers[i].Name == result.Notifiers[j].Name {
				result.Notifiers[i].Error = "duplicate_name"
				result.Notifiers[j].Error = "duplicate_name"
			}
		}
	}
	return result, nil
}

func readLocal(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("local file unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxConfigBytes {
		return nil, errors.New("invalid local file")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxConfigBytes+1))
	if err != nil || len(data) > MaxConfigBytes {
		return nil, errors.New("local file read failed")
	}
	return data, nil
}

func LoadBackup(path string) (*Backup, error) {
	data, err := readLocal(path)
	if err != nil {
		return nil, errors.New("configuration unavailable")
	}
	cfg, err := DecodeBackup(data)
	if err != nil {
		return nil, err
	}
	for i := range cfg.Targets {
		target := &cfg.Targets[i]
		if target.Error != "" {
			continue
		}
		data, err := readLocal(target.EnvFile)
		if err != nil {
			target.Error = "env_file_unavailable"
			continue
		}
		env, err := kvfile.ParseFromReader(bytes.NewReader(data), nil)
		if err != nil {
			target.Error = "invalid_env_file"
			continue
		}
		for _, entry := range env {
			if strings.ContainsRune(entry, 0) {
				target.Error = "invalid_env_file"
			}
		}
		target.Environment = env
	}
	for i := range cfg.Notifiers {
		notifier := &cfg.Notifiers[i]
		if notifier.Error != "" || notifier.Type != "bark" {
			continue
		}
		data, err := readLocal(notifier.AuthKeyFile)
		if err != nil {
			notifier.Error = "auth_key_unavailable"
			continue
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
			notifier.Error = "invalid_auth_key"
			continue
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		key, ok := parsed.(*ecdsa.PrivateKey)
		if err != nil || !ok || key.Curve != elliptic.P256() {
			notifier.Error = "invalid_auth_key"
			continue
		}
		notifier.Key = key
	}
	return cfg, nil
}

func (b *Backup) Valid() bool {
	for _, t := range b.Targets {
		if t.Error != "" {
			return false
		}
	}
	for _, n := range b.Notifiers {
		if n.Error != "" {
			return false
		}
	}
	return true
}
