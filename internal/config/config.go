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
	"github.com/hellodword/vaultwarden-less/internal/safefs"
	"github.com/hellodword/vaultwarden-less/internal/securefile"
	"github.com/hellodword/vaultwarden-less/internal/version"
)

const (
	MaxConfigBytes = 1 << 20

	DefaultBackupConfigPath = "/config/backup.yaml"
	RoleCredentialsPath     = "/run/credentials/role/credentials.yaml"
	ResticPasswordPath      = "/run/credentials/common/restic-password"

	ListenAddress = "0.0.0.0:9090"
	DataDir       = "/data"
	StateDir      = "/state"
	StagingDir    = "/staging"
	CacheDir      = "/cache"
	ResticBinary  = "/usr/local/bin/restic"
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

// Backup is the complete human-authored v1 configuration. Runtime paths,
// scheduler timing and event-server limits are fixed product behavior and are
// populated after decoding rather than exposed as YAML fields.
type Backup struct {
	SchemaVersion      int        `yaml:"schema_version"`
	TargetTimeout      Duration   `yaml:"target_timeout"`
	MaintenanceTimeout Duration   `yaml:"maintenance_timeout"`
	Targets            []Target   `yaml:"targets,omitempty"`
	Notifiers          []Notifier `yaml:"notifiers,omitempty"`

	ListenAddress      string   `yaml:"-"`
	DataDir            string   `yaml:"-"`
	StateDir           string   `yaml:"-"`
	StagingDir         string   `yaml:"-"`
	CacheDir           string   `yaml:"-"`
	ResticBinary       string   `yaml:"-"`
	Debounce           Duration `yaml:"-"`
	MaxWait            Duration `yaml:"-"`
	MinInterval        Duration `yaml:"-"`
	SampleInterval     Duration `yaml:"-"`
	MissingRetry       Duration `yaml:"-"`
	ShutdownTimeout    Duration `yaml:"-"`
	EventMaxBytes      int64    `yaml:"-"`
	EventRatePerSecond float64  `yaml:"-"`
	EventBurst         int      `yaml:"-"`
}

type Target struct {
	Name          string            `yaml:"name"`
	Repository    string            `yaml:"repository"`
	ResticOptions map[string]string `yaml:"restic_options,omitempty"`
}

func (t Target) Warnings() []string {
	if strings.HasPrefix(t.Repository, "s3:http://") {
		return []string{"plaintext_s3_endpoint"}
	}
	return []string{}
}

type Notifier struct {
	Name    string   `yaml:"name"`
	Type    string   `yaml:"type"`
	Timeout Duration `yaml:"timeout"`

	WebhookURL string `yaml:"webhook_url,omitempty"`

	DeviceToken string `yaml:"device_token,omitempty"`
	AuthKey     string `yaml:"auth_key,omitempty"`
	KeyID       string `yaml:"key_id,omitempty"`
	TeamID      string `yaml:"team_id,omitempty"`
	Topic       string `yaml:"topic,omitempty"`
	Sound       string `yaml:"sound,omitempty"`
}

// RoleCredentials is intentionally permissive about unknown targets and
// fields. Only the configured target and the keys required by its repository
// form are consumed.
type RoleCredentials struct {
	SchemaVersion int                          `yaml:"schema_version"`
	Targets       map[string]map[string]string `yaml:"targets"`
}

func LoadBackup(path string) (*Backup, error) {
	data, err := securefile.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read backup config: %w", err)
	}
	return DecodeBackup(data)
}

func DecodeBackup(data []byte) (*Backup, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("backup config exceeds %d bytes", MaxConfigBytes)
	}
	cfg := defaultBackup()
	if err := decodeOneYAML(data, &cfg, true); err != nil {
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
	if err := safefs.ValidatePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("validate role credentials directory: %w", err)
	}
	data, err := securefile.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read role credentials: %w", err)
	}
	var credentials RoleCredentials
	if err := decodeOneYAML(data, &credentials, false); err != nil {
		return nil, fmt.Errorf("decode role credentials: invalid YAML")
	}
	if credentials.SchemaVersion != version.SchemaVersion {
		return nil, fmt.Errorf("role credentials schema_version must be %d", version.SchemaVersion)
	}
	if credentials.Targets == nil {
		return nil, fmt.Errorf("role credentials targets must be a mapping")
	}
	for _, target := range cfg.Targets {
		if _, err := credentials.Environment(target); err != nil {
			return nil, fmt.Errorf("target %s credentials: %w", target.Name, err)
		}
	}
	return &credentials, nil
}

func decodeOneYAML(data []byte, destination any, strict bool) error {
	options := []yaml.DecodeOption{}
	if strict {
		options = append(options, yaml.DisallowUnknownField())
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data), options...)
	if err := decoder.Decode(destination); err != nil {
		if strict && strings.Contains(strings.ToLower(err.Error()), "unknown field") {
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

func defaultBackup() Backup {
	return Backup{
		ListenAddress:      ListenAddress,
		DataDir:            DataDir,
		StateDir:           StateDir,
		StagingDir:         StagingDir,
		CacheDir:           CacheDir,
		ResticBinary:       ResticBinary,
		Debounce:           Duration{Duration: 30 * time.Second},
		MaxWait:            Duration{Duration: 2 * time.Minute},
		MinInterval:        Duration{Duration: 5 * time.Minute},
		SampleInterval:     Duration{Duration: 15 * time.Minute},
		MissingRetry:       Duration{Duration: 30 * time.Second},
		ShutdownTimeout:    Duration{Duration: 2 * time.Minute},
		EventMaxBytes:      1024,
		EventRatePerSecond: 10,
		EventBurst:         20,
	}
}

func (c *Backup) Validate() error {
	if c.SchemaVersion != version.SchemaVersion {
		return fmt.Errorf("schema_version must be %d", version.SchemaVersion)
	}
	if c.TargetTimeout.Duration < time.Minute || c.TargetTimeout.Duration > 2*time.Hour {
		return fmt.Errorf("target_timeout must be between 1m and 2h")
	}
	if c.MaintenanceTimeout.Duration < time.Minute || c.MaintenanceTimeout.Duration > 24*time.Hour {
		return fmt.Errorf("maintenance_timeout must be between 1m and 24h")
	}
	if len(c.Targets) > 32 {
		return fmt.Errorf("targets must not contain more than 32 entries")
	}
	if c.Targets == nil {
		c.Targets = []Target{}
	}
	for index := 1; index < len(c.Targets); index++ {
		if c.Targets[index-1].Name >= c.Targets[index].Name {
			return fmt.Errorf("targets must be sorted by name")
		}
	}
	for index := range c.Targets {
		if err := c.Targets[index].validateFields(); err != nil {
			return fmt.Errorf("targets[%d]: %w", index, err)
		}
	}
	for index := range c.Targets {
		if err := c.Targets[index].validateOptions(); err != nil {
			return fmt.Errorf("targets[%d].restic_options: %w", index, err)
		}
	}
	if len(c.Notifiers) > 16 {
		return fmt.Errorf("notifiers must not contain more than 16 entries")
	}
	if c.Notifiers == nil {
		c.Notifiers = []Notifier{}
	}
	for index := 1; index < len(c.Notifiers); index++ {
		if c.Notifiers[index-1].Name >= c.Notifiers[index].Name {
			return fmt.Errorf("notifiers must be sorted by name")
		}
	}
	for index := range c.Notifiers {
		if err := c.Notifiers[index].validate(); err != nil {
			return fmt.Errorf("notifiers[%d]: %w", index, err)
		}
	}
	return nil
}

func (t Target) Backend() string {
	if strings.HasPrefix(t.Repository, "s3:") {
		return "s3"
	}
	return ""
}

func (t Target) Validate() error {
	if err := t.validateFields(); err != nil {
		return err
	}
	return t.validateOptions()
}

func (t Target) validateFields() error {
	if !namePattern.MatchString(t.Name) {
		return fmt.Errorf("invalid name %q", t.Name)
	}
	if !validRepository(t.Repository) {
		return fmt.Errorf("repository must be an s3:http[s]:// locator without embedded credentials")
	}
	return nil
}

func (t Target) validateOptions() error {
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
	if repository == "" || len(repository) > 4096 || strings.ContainsAny(repository, "\r\n\x00") {
		return false
	}
	if !strings.HasPrefix(repository, "s3:") {
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

func (c RoleCredentials) Environment(target Target) (map[string]string, error) {
	values, exists := c.Targets[target.Name]
	if !exists {
		return nil, fmt.Errorf("target entry is missing")
	}
	var names []string
	switch target.Backend() {
	case "s3":
		names = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}
	default:
		return nil, fmt.Errorf("repository backend is unsupported")
	}
	environment := make(map[string]string, len(names))
	for _, name := range names {
		value, exists := values[name]
		if !exists || value == "" || len(value) > 64<<10 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("required key %s is missing or invalid", name)
		}
		environment[name] = value
	}
	return environment, nil
}

func (n *Notifier) validate() error {
	if !namePattern.MatchString(n.Name) {
		return fmt.Errorf("invalid name %q", n.Name)
	}
	if n.Timeout.Duration < time.Second || n.Timeout.Duration > time.Minute {
		return fmt.Errorf("timeout must be between 1s and 1m")
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
