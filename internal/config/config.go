package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

const (
	SchemaVersion = 2

	MaxConfigBytes = 1 << 20

	DefaultBackupConfigPath = "/config/backup.yaml"
	RoleCredentialsPath     = "/run/credentials/role/credentials.yaml"
	ResticPasswordRoot      = "/run/credentials/restic"
	NotifierCredentialsPath = "/run/credentials/notifiers/credentials.yaml"

	ListenAddress     = "0.0.0.0:9090"
	WorkerURL         = "http://backup:9090"
	DataDir           = "/data"
	StagingDir        = "/staging"
	RecoveryPointPath = "/staging/point"
	CacheDir          = "/cache"
	RestoreRoot       = "/restore"
	ResticBinary      = "/bin/restic"

	QuietWindow           = 30 * time.Second
	MaximumQuietWindow    = 2 * time.Minute
	MinimumCheckInterval  = 5 * time.Minute
	FallbackCheckInterval = 12 * time.Hour
	ConsistencyRetry      = 30 * time.Second
	FailureRetry          = 5 * time.Minute
	DegradedAfter         = 30 * time.Minute
	EventRetry            = 30 * time.Second
	TargetTimeout         = 2 * time.Hour
	MaintenanceTimeout    = 24 * time.Hour
	NotifierTimeout       = 10 * time.Second
	ShutdownTimeout       = 2 * time.Minute
	EventMaxBytes         = 1024
)

var (
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	allowedS3Option = map[string]struct{}{
		"s3.bucket-lookup": {},
		"s3.region":        {},
	}
	regionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

var errUnknownYAMLField = errors.New("unknown YAML field")

// Backup is the complete human-authored schema v2 configuration. Scheduling,
// retry, timeout, retention, filesystem and HTTP behavior are product constants.
type Backup struct {
	SchemaVersion int        `yaml:"schema_version"`
	Targets       []Target   `yaml:"targets"`
	Notifiers     []Notifier `yaml:"notifiers,omitempty"`
}

type Target struct {
	Name          string            `yaml:"name"`
	Repository    string            `yaml:"repository"`
	ResticOptions map[string]string `yaml:"restic_options,omitempty"`
}

type Notifier struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`

	WebhookURL string `yaml:"-"`

	DeviceToken string `yaml:"-"`
	AuthKey     string `yaml:"-"`
	KeyID       string `yaml:"-"`
	TeamID      string `yaml:"-"`
	Topic       string `yaml:"-"`
	Sound       string `yaml:"-"`
}

type NotifierSecrets struct {
	WebhookURL string `yaml:"webhook_url,omitempty"`

	DeviceToken string `yaml:"device_token,omitempty"`
	AuthKey     string `yaml:"auth_key,omitempty"`
	KeyID       string `yaml:"key_id,omitempty"`
	TeamID      string `yaml:"team_id,omitempty"`
	Topic       string `yaml:"topic,omitempty"`
	Sound       string `yaml:"sound,omitempty"`
}

type NotifierCredentials struct {
	SchemaVersion int                        `yaml:"schema_version"`
	Notifiers     map[string]NotifierSecrets `yaml:"notifiers"`
}

type S3Credentials struct {
	AccessKeyID     string `yaml:"AWS_ACCESS_KEY_ID"`
	SecretAccessKey string `yaml:"AWS_SECRET_ACCESS_KEY"`
}

type RoleCredentials struct {
	SchemaVersion int                      `yaml:"schema_version"`
	Targets       map[string]S3Credentials `yaml:"targets"`
}

func LoadBackup(path string) (*Backup, error) {
	if err := privatefs.ValidatePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("validate backup config directory: %w", err)
	}
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
	var cfg Backup
	if err := decodeOneYAML(data, &cfg); err != nil {
		if errors.Is(err, errUnknownYAMLField) {
			return nil, fmt.Errorf("decode backup config: unknown field")
		}
		return nil, fmt.Errorf("decode backup config: invalid YAML")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate backup config: %w", err)
	}
	return &cfg, nil
}

func LoadRoleCredentials(path string, cfg *Backup) (*RoleCredentials, error) {
	if cfg == nil {
		return nil, fmt.Errorf("backup config is required")
	}
	if err := privatefs.ValidatePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("validate role credentials directory: %w", err)
	}
	data, err := privatefs.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read role credentials: %w", err)
	}
	var credentials RoleCredentials
	if err := decodeOneYAML(data, &credentials); err != nil {
		return nil, fmt.Errorf("decode role credentials: invalid YAML")
	}
	if err := credentials.Validate(cfg); err != nil {
		return nil, fmt.Errorf("validate role credentials: %w", err)
	}
	return &credentials, nil
}

func LoadNotifierCredentials(path string, cfg *Backup) ([]Notifier, error) {
	if cfg == nil {
		return nil, fmt.Errorf("backup config is required")
	}
	if len(cfg.Notifiers) == 0 {
		return []Notifier{}, nil
	}
	if err := privatefs.ValidatePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("validate notifier credentials directory: %w", err)
	}
	data, err := privatefs.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read notifier credentials: %w", err)
	}
	var credentials NotifierCredentials
	if err := decodeOneYAML(data, &credentials); err != nil {
		return nil, fmt.Errorf("decode notifier credentials: invalid YAML")
	}
	resolved, err := credentials.Resolve(cfg)
	if err != nil {
		return nil, fmt.Errorf("validate notifier credentials: %w", err)
	}
	return resolved, nil
}

func decodeOneYAML(data []byte, destination any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField())
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			return errUnknownYAMLField
		}
		return err
	}
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple YAML documents")
	}
	return err
}

func (c *Backup) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if len(c.Targets) == 0 {
		return fmt.Errorf("targets must contain at least one entry")
	}
	if len(c.Targets) > 32 {
		return fmt.Errorf("targets must not contain more than 32 entries")
	}
	targetNames := make(map[string]struct{}, len(c.Targets))
	for index := range c.Targets {
		if err := c.Targets[index].Validate(); err != nil {
			return fmt.Errorf("targets[%d]: %w", index, err)
		}
		if _, duplicate := targetNames[c.Targets[index].Name]; duplicate {
			return fmt.Errorf("target names must be unique")
		}
		targetNames[c.Targets[index].Name] = struct{}{}
		if c.Targets[index].ResticOptions == nil {
			c.Targets[index].ResticOptions = map[string]string{}
		}
	}
	if len(c.Notifiers) > 16 {
		return fmt.Errorf("notifiers must not contain more than 16 entries")
	}
	if c.Notifiers == nil {
		c.Notifiers = []Notifier{}
	}
	notifierNames := make(map[string]struct{}, len(c.Notifiers))
	for index := range c.Notifiers {
		if err := c.Notifiers[index].validateReference(); err != nil {
			return fmt.Errorf("notifiers[%d]: %w", index, err)
		}
		if _, duplicate := notifierNames[c.Notifiers[index].Name]; duplicate {
			return fmt.Errorf("notifier names must be unique")
		}
		notifierNames[c.Notifiers[index].Name] = struct{}{}
	}
	return nil
}

func (c Backup) Target(name string) (Target, bool) {
	for _, target := range c.Targets {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

func (t Target) Backend() string {
	if strings.HasPrefix(t.Repository, "s3:") {
		return "s3"
	}
	return ""
}

func (t Target) Warnings() []string {
	if strings.HasPrefix(t.Repository, "s3:http://") {
		return []string{"plaintext_s3_endpoint"}
	}
	return []string{}
}

func (t Target) Validate() error {
	if !namePattern.MatchString(t.Name) {
		return fmt.Errorf("invalid name %q", t.Name)
	}
	if !validRepository(t.Repository) {
		return fmt.Errorf("repository must be an s3:http[s]:// locator without embedded credentials")
	}
	optionNames := make([]string, 0, len(t.ResticOptions))
	for name := range t.ResticOptions {
		optionNames = append(optionNames, name)
	}
	sort.Strings(optionNames)
	for _, name := range optionNames {
		if _, allowed := allowedS3Option[name]; !allowed {
			return fmt.Errorf("unsupported Restic option %q", name)
		}
		value := t.ResticOptions[name]
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("restic option %q has an invalid value", name)
		}
		switch name {
		case "s3.bucket-lookup":
			if value != "auto" && value != "dns" && value != "path" {
				return fmt.Errorf("restic option %q has an invalid value", name)
			}
		case "s3.region":
			if !regionPattern.MatchString(value) {
				return fmt.Errorf("restic option %q has an invalid value", name)
			}
		}
	}
	return nil
}

func validRepository(repository string) bool {
	if repository == "" || len(repository) > 4096 || strings.ContainsAny(repository, "\r\n\x00") || !strings.HasPrefix(repository, "s3:") {
		return false
	}
	parsed, err := url.Parse(strings.TrimPrefix(repository, "s3:"))
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Opaque != "" {
		return false
	}
	path := parsed.Path
	if path == "" || !strings.HasPrefix(path, "/") {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "\r\n\x00") {
			return false
		}
	}
	return true
}

func (c RoleCredentials) Validate(cfg *Backup) error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if c.Targets == nil || len(c.Targets) != len(cfg.Targets) {
		return fmt.Errorf("target key set must exactly match backup config")
	}
	for _, target := range cfg.Targets {
		value, exists := c.Targets[target.Name]
		if !exists {
			return fmt.Errorf("target key set must exactly match backup config")
		}
		if err := value.validate(); err != nil {
			return fmt.Errorf("target %s: %w", target.Name, err)
		}
	}
	return nil
}

func (c RoleCredentials) Environment(target Target) (map[string]string, error) {
	values, exists := c.Targets[target.Name]
	if !exists {
		return nil, fmt.Errorf("target entry is missing")
	}
	if err := values.validate(); err != nil {
		return nil, err
	}
	return map[string]string{
		"AWS_ACCESS_KEY_ID":     values.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": values.SecretAccessKey,
	}, nil
}

func (c S3Credentials) validate() error {
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":     c.AccessKeyID,
		"AWS_SECRET_ACCESS_KEY": c.SecretAccessKey,
	} {
		if value == "" || len(value) > 64<<10 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("required key %s is missing or invalid", name)
		}
	}
	return nil
}

func (n Notifier) validateReference() error {
	if !namePattern.MatchString(n.Name) {
		return fmt.Errorf("invalid name %q", n.Name)
	}
	if n.Type != "wechat" && n.Type != "bark" {
		return fmt.Errorf("unsupported type %q", n.Type)
	}
	if n.WebhookURL != "" || n.DeviceToken != "" || n.AuthKey != "" || n.KeyID != "" || n.TeamID != "" || n.Topic != "" || n.Sound != "" {
		return fmt.Errorf("secret fields belong in notifier credentials")
	}
	return nil
}

func (n Notifier) validateResolved() error {
	if !namePattern.MatchString(n.Name) {
		return fmt.Errorf("invalid name %q", n.Name)
	}
	switch n.Type {
	case "wechat":
		if n.WebhookURL == "" || n.DeviceToken != "" || n.AuthKey != "" || n.KeyID != "" || n.TeamID != "" || n.Topic != "" || n.Sound != "" {
			return fmt.Errorf("fields are invalid for WeChat")
		}
		parsed, err := parseHTTPSURL(n.WebhookURL)
		if err != nil || parsed.Path == "" {
			return fmt.Errorf("webhook_url must be an HTTPS URL without user info or fragment")
		}
	case "bark":
		if n.WebhookURL != "" || n.DeviceToken == "" || n.AuthKey == "" || n.KeyID == "" || n.TeamID == "" || n.Topic == "" {
			return fmt.Errorf("bark fields are incomplete or invalid")
		}
		if len(n.DeviceToken) != 64 || strings.ToLower(n.DeviceToken) != n.DeviceToken {
			return fmt.Errorf("device_token must be 64 lowercase hexadecimal characters")
		}
		if _, err := hex.DecodeString(n.DeviceToken); err != nil {
			return fmt.Errorf("device_token must be 64 lowercase hexadecimal characters")
		}
		if len(n.AuthKey) > 16<<10 || strings.IndexByte(n.AuthKey, 0) >= 0 {
			return fmt.Errorf("auth_key is invalid")
		}
		for _, field := range []struct {
			name  string
			value string
		}{{"key_id", n.KeyID}, {"team_id", n.TeamID}, {"topic", n.Topic}} {
			if len(field.value) < 2 || len(field.value) > 255 || strings.ContainsAny(field.value, "\r\n\x00") {
				return fmt.Errorf("%s is invalid", field.name)
			}
		}
		if len(n.Sound) > 255 || strings.ContainsAny(n.Sound, "\r\n\x00") {
			return fmt.Errorf("sound is invalid")
		}
	default:
		return fmt.Errorf("unsupported type %q", n.Type)
	}
	return nil
}

func (c NotifierCredentials) Resolve(cfg *Backup) ([]Notifier, error) {
	if c.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if c.Notifiers == nil || len(c.Notifiers) != len(cfg.Notifiers) {
		return nil, fmt.Errorf("notifier key set must exactly match backup config")
	}
	resolved := make([]Notifier, 0, len(cfg.Notifiers))
	for _, reference := range cfg.Notifiers {
		secrets, exists := c.Notifiers[reference.Name]
		if !exists {
			return nil, fmt.Errorf("notifier key set must exactly match backup config")
		}
		notifier := reference
		notifier.WebhookURL = secrets.WebhookURL
		notifier.DeviceToken = secrets.DeviceToken
		notifier.AuthKey = secrets.AuthKey
		notifier.KeyID = secrets.KeyID
		notifier.TeamID = secrets.TeamID
		notifier.Topic = secrets.Topic
		notifier.Sound = secrets.Sound
		if err := notifier.validateResolved(); err != nil {
			return nil, fmt.Errorf("notifier %s: %w", reference.Name, err)
		}
		resolved = append(resolved, notifier)
	}
	return resolved, nil
}

func parseHTTPSURL(raw string) (*url.URL, error) {
	if len(raw) < 1 || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("URL is invalid")
	}
	return parsed, nil
}
